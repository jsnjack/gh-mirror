package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gh-mirror/internal/embedding"

	"modernc.org/sqlite"
)

// The driver registers connection hooks globally, so probes are selected by database path.
var (
	commitHookOnce sync.Once
	commitProbes   sync.Map
)

// commitProbe counts write commits and delays each one like a slow durable disk sync.
type commitProbe struct {
	delay   time.Duration
	commits atomic.Int64
}

func probedStore(tb testing.TB, delay time.Duration) (*Store, *commitProbe) {
	tb.Helper()
	commitHookOnce.Do(func() {
		sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, dsn string) error {
			u, err := url.Parse(dsn)
			if err != nil {
				return fmt.Errorf("parse probed DSN: %w", err)
			}
			value, ok := commitProbes.Load(u.Path)
			if !ok {
				return nil
			}
			probe := value.(*commitProbe)
			hooks, ok := conn.(sqlite.HookRegisterer)
			if !ok {
				return fmt.Errorf("SQLite connection %T has no commit hook", conn)
			}
			hooks.RegisterCommitHook(func() int32 {
				probe.commits.Add(1)
				time.Sleep(probe.delay)
				return 0
			})
			return nil
		})
	})
	path := filepath.Join(tb.TempDir(), "mirror.sqlite")
	probe := &commitProbe{delay: delay}
	commitProbes.Store(path, probe)
	db, err := Open(path, false)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := db.Close(); err != nil {
			tb.Error(err)
		}
		commitProbes.Delete(path)
	})
	return db, probe
}

// putDistinctIssues stores label-free issues whose titles and bodies all need their own inference.
func putDistinctIssues(tb testing.TB, db *Store, count int, body func(int) string) {
	tb.Helper()
	ctx := context.Background()
	if err := db.Update(ctx, func(w *Writer) error {
		for n := 1; n <= count; n++ {
			raw := json.RawMessage(fmt.Sprintf(`{"number":%d,"node_id":"commit_%d","title":"title %03d","body":%q,"state":"open","updated_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/%d","labels":[]}`, n, n, n, body(n), n))
			if _, err := w.PutIssue(ctx, "o/r", raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		tb.Fatal(err)
	}
}

// Indexing once committed about six times per two-passage document, one disk sync
// each. Sharing commits brought that near one; keeping more documents waiting than
// CPU workers brings it below 0.5, so the limit fails if either regresses.
func TestIndexSharesCommits(t *testing.T) {
	const documents = 48
	const maxCommitsPerDocument = 0.75
	db, probe := probedStore(t, 5*time.Millisecond)
	putDistinctIssues(t, db, documents, func(n int) string { return fmt.Sprintf("body %03d", n) })
	encoder := &fakeVectorizer{}
	before := probe.commits.Load()
	status, err := db.Index(context.Background(), encoder, IndexOptions{Workers: 8})
	if err != nil || status.PendingDocuments != 0 || status.IndexedDocuments != documents || encoder.calls != 2*documents {
		t.Fatal("indexing did not complete every passage", status, encoder.calls, err)
	}
	commits := probe.commits.Load() - before
	perDocument := float64(commits) / documents
	t.Logf("%d commits for %d documents (%.2f each)", commits, documents, perDocument)
	if perDocument > maxCommitsPerDocument {
		t.Fatalf("indexing made %d commits for %d documents (%.2f each); the limit is %.2f", commits, documents, perDocument, maxCommitsPerDocument)
	}
}

// BenchmarkIndexSlowDisk indexes comment-length passages with the bundled encoder
// while each commit waits 6 ms, the sync latency inferred from indexing progress on an md RAID host.
func BenchmarkIndexSlowDisk(b *testing.B) {
	const documents = 64
	ctx := context.Background()
	if _, err := embedding.Default.Embed(ctx, "warm up"); err != nil {
		b.Fatal(err)
	}
	for _, workers := range []int{4, 8, 16} {
		b.Run(fmt.Sprintf("workers_%d", workers), func(b *testing.B) {
			var elapsed time.Duration
			var commits int64
			for b.Loop() {
				b.StopTimer()
				db, probe := probedStore(b, 6*time.Millisecond)
				putDistinctIssues(b, db, documents, func(n int) string {
					return strings.Repeat(fmt.Sprintf("Customer %d reports that the session freezes after sharing a screen. ", n), 3)
				})
				before := probe.commits.Load()
				started := time.Now()
				b.StartTimer()
				if _, err := db.Index(ctx, embedding.Default, IndexOptions{Workers: workers}); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				elapsed += time.Since(started)
				commits += probe.commits.Load() - before
				b.StartTimer()
			}
			b.ReportMetric(float64(documents*b.N)/elapsed.Seconds(), "docs/s")
			b.ReportMetric(float64(commits)/float64(documents*b.N), "commits/doc")
		})
	}
}
