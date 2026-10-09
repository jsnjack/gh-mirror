package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gh-mirror/internal/embedding"
	"gh-mirror/internal/progress"
)

type fakeVectorizer struct {
	mu       sync.Mutex
	calls    int
	failAt   int
	identity string
}

type queuedVectorizer struct {
	fakeVectorizer
	continued chan struct{}
	once      sync.Once
}

func (e *queuedVectorizer) Embed(ctx context.Context, text string) ([]float32, error) {
	if text == "slow" {
		select {
		case <-e.continued:
		case <-ctx.Done():
			return nil, fmt.Errorf("blocked fixture document: %w", ctx.Err())
		}
	}
	if text == "after-first-page" {
		e.once.Do(func() { close(e.continued) })
	}
	return e.fakeVectorizer.Embed(ctx, text)
}

func TestIndexStreamsBeyondDocumentPage(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.Update(ctx, func(w *Writer) error {
		for n := 1; n <= 40; n++ {
			number, title := n, "fast"
			if n == 1 {
				title = "slow"
			}
			if n == 40 {
				number, title = 999, "after-first-page"
			}
			raw := json.RawMessage(fmt.Sprintf(`{"number":%d,"node_id":"queue_%d","title":%q,"body":"","state":"open","updated_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/%d","labels":[]}`, number, number, title, number))
			if _, err := w.PutIssue(ctx, "o/r", raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	encoder := &queuedVectorizer{continued: make(chan struct{})}
	completed := 0
	status, err := db.Index(ctx, encoder, IndexOptions{Workers: 8, Progress: func(event progress.Event) {
		if event.Workers != 8 || event.Active < 0 || event.Active > 8 || event.WorkerKind != progress.CPUWorkers {
			t.Error("incorrect CPU worker reporting", event)
		}
		if event.Completed > 0 {
			if event.Completed != completed+1 {
				t.Error("non-monotonic document progress", completed, event)
			}
			completed = event.Completed
		}
	}})
	if err != nil || status.PendingDocuments != 0 || completed != 40 {
		t.Fatal("later documents waited for an unfinished page", completed, status, err)
	}
}

func (e *fakeVectorizer) ID() string {
	if e.identity != "" {
		return e.identity
	}
	return embedding.Fingerprint
}
func (e *fakeVectorizer) Dim() int { return embedding.Dimension }
func (e *fakeVectorizer) Chunks(text string) ([]embedding.Chunk, error) {
	out := []embedding.Chunk{}
	for start := 0; start < len(text); start += 20 {
		end := min(start+20, len(text))
		out = append(out, embedding.Chunk{Text: text[start:end], Start: start, End: end})
	}
	return out, nil
}
func (e *fakeVectorizer) Embed(ctx context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.failAt > 0 && e.calls == e.failAt {
		return nil, fmt.Errorf("interrupted fixture: %w", context.Canceled)
	}
	v := make([]float32, embedding.Dimension)
	v[0] = 1
	return v, nil
}
func TestSemanticDurability(t *testing.T) {
	for _, name := range []string{"resume", "unchanged", "metadata update", "body update", "labels update", "comment update", "delete", "model contract", "export"} {
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			seed(t, db)
			ctx := context.Background()
			encoder := &fakeVectorizer{}
			if name == "resume" {
				encoder.failAt = 2
			}
			_, err := db.Index(ctx, encoder, IndexOptions{Workers: 1})
			if name == "resume" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("interruption missing", err)
				}
				var saved int
				if err := db.db.QueryRow("SELECT count(*) FROM semantic_vectors").Scan(&saved); err != nil || saved != 1 {
					t.Fatal("completed chunk lost", saved, err)
				}
				encoder.failAt = 0
			}
			if err != nil && name != "resume" {
				t.Fatal(err)
			}
			before := encoder.calls
			switch name {
			case "metadata update", "body update", "labels update":
				raw := issueFixture(1, "network failure", "2026-01-03T00:00:00Z")
				if name == "body update" {
					raw = issueFixture(1, "different complete body", "2026-01-03T00:00:00Z")
				}
				if name == "labels update" {
					raw = json.RawMessage(strings.ReplaceAll(string(raw), `"bug"`, `"customer"`))
				}
				if err := db.Update(ctx, func(w *Writer) error { _, err := w.PutIssue(ctx, "o/r", raw); return err }); err != nil {
					t.Fatal(err)
				}
			case "comment update":
				if err := db.Update(ctx, func(w *Writer) error {
					return w.PutComment(ctx, "o/r", 1, commentFixture("Changed comment body", "2026-01-03T00:00:00Z"))
				}); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if _, err := db.db.Exec("DELETE FROM issues WHERE number=1"); err != nil {
					t.Fatal(err)
				}
			case "model contract":
				encoder.identity = "new-contract"
			}
			status, err := db.Index(ctx, encoder, IndexOptions{Workers: 1})
			if err != nil {
				t.Fatal(err)
			}
			if status.PendingDocuments != 0 || status.IndexedDocuments != status.Documents {
				t.Fatal("incomplete index", status)
			}
			if (name == "unchanged" || name == "metadata update" || name == "delete" || name == "export") && encoder.calls != before {
				t.Fatal("unchanged text was re-encoded", name, encoder.calls, before)
			}
			if (name == "body update" || name == "labels update" || name == "comment update" || name == "model contract") && encoder.calls == before {
				t.Fatal("changed text was not re-encoded")
			}
			if name == "export" {
				path := filepath.Join(t.TempDir(), "snapshot.sqlite")
				if err := db.Export(ctx, path); err != nil {
					t.Fatal(err)
				}
				reader, err := Open(path, true)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := reader.Close(); err != nil {
						t.Error(err)
					}
				}()
				got, err := reader.Status(ctx)
				if err != nil || got.Semantic.Chunks != status.Chunks {
					t.Fatal("vectors missing from snapshot", got, err)
				}
			}
		})
	}
}
func TestSemanticRetrieval(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	if err := db.Update(ctx, func(w *Writer) error {
		texts := []string{"Download freezes permanently", "Meeting reminder email is missing", "Cannot save an attachment from the browser", "Invoice totals do not match"}
		for n, title := range texts {
			raw := json.RawMessage(fmt.Sprintf(`{"number":%d,"node_id":"semantic_%d","title":%q,"body":"","state":"open","updated_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/%d","labels":[]}`, n+1, n+1, title, n+1))
			if _, err := w.PutIssue(ctx, "o/r", raw); err != nil {
				return err
			}
		}
		return w.SetMetadata(ctx, "generation", "semantic-fixture")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Search(ctx, SearchOptions{Engine: "semantic", Query: "transferring a file gets stuck"}); !errors.Is(err, ErrSemanticUnavailable) {
		t.Fatal("unindexed search silently succeeded", err)
	}
	if _, err := db.Index(ctx, embedding.Default, IndexOptions{Workers: 4}); err != nil {
		t.Fatal(err)
	}
	for _, engine := range []string{"semantic", "hybrid"} {
		t.Run(engine, func(t *testing.T) {
			opts := SearchOptions{Engine: engine, Query: "transferring a file gets stuck", Limit: 1, PageOptions: PageOptions{Count: true, Facets: true}}
			result, err := db.Search(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			if result.Matches[0].Number != 1 {
				t.Fatalf("paraphrase rank: %+v", result.Matches)
			}
			if !result.HasMore || result.Total == nil || *result.Total != 4 || len(result.Matches[0].Evidence) == 0 {
				t.Fatal("missing result metadata", result)
			}
			opts.Cursor = result.NextCursor
			next, err := db.Search(ctx, opts)
			if err != nil || next.Matches[0].Number == 1 {
				t.Fatal("invalid continuation", next, err)
			}
			candidates, err := db.FindCandidates(ctx, CandidateOptions{Engine: engine, Repo: "o/r", Number: 1, Limit: 2})
			if err != nil || len(candidates.Matches) == 0 || candidates.Matches[0].Number == 1 {
				t.Fatal("candidate engine failed", candidates, err)
			}
			if _, err := db.db.Exec("UPDATE documents SET body=? WHERE id='issue:o/r:4'", "Changed new source content "+engine); err != nil {
				t.Fatal(err)
			}
			opts.Cursor = ""
			if _, err := db.Search(ctx, opts); !errors.Is(err, ErrSemanticUnavailable) {
				t.Fatal("stale index was silently queried", err)
			}
			if _, err := db.Index(ctx, embedding.Default, IndexOptions{Workers: 4}); err != nil {
				t.Fatal(err)
			}
			opts.Cursor = result.NextCursor
			if _, err := db.Search(ctx, opts); !errors.Is(err, ErrCursorConflict) {
				t.Fatal("index cursor accepted after index change", err)
			}
		})
	}
}
func TestLegacySemanticSchema(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	seed(t, db)
	if _, err := db.db.Exec("DROP TRIGGER semantic_insert; DROP TRIGGER semantic_delete; DROP TRIGGER semantic_invalidate; DROP TABLE semantic_vectors; DROP TABLE semantic_documents; UPDATE metadata SET value='1' WHERE key='schema_version'"); err != nil {
		t.Fatal(err)
	}
	reader, err := Open(db.Path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	status, err := reader.Status(ctx)
	if err != nil || status.SchemaVersion != 1 {
		t.Fatal("legacy reader incompatibility", status, err)
	}
	if _, err := reader.Search(ctx, SearchOptions{Query: "network"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Search(ctx, SearchOptions{Engine: "hybrid", Query: "network"}); !errors.Is(err, ErrSemanticUnavailable) {
		t.Fatal("legacy semantic should need local index", err)
	}
	writer, err := Open(db.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := writer.Index(ctx, &fakeVectorizer{}, IndexOptions{Workers: 2}); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkSemanticIndex(b *testing.B) {
	for _, workers := range []int{1, 4, 8, 16} {
		b.Run(fmt.Sprintf("workers_%d", workers), func(b *testing.B) {
			ctx := context.Background()
			if _, err := embedding.Default.Embed(ctx, "warm up"); err != nil {
				b.Fatal(err)
			}
			for range b.N {
				b.StopTimer()
				db, err := Open(filepath.Join(b.TempDir(), "index.sqlite"), false)
				if err != nil {
					b.Fatal(err)
				}
				err = db.Update(ctx, func(w *Writer) error {
					for n := range 100 {
						raw := json.RawMessage(fmt.Sprintf(`{"number":%d,"node_id":"benchmark_%d","title":"A technical bug in the database connection","body":%q,"state":"open","updated_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/demo/support/issues/%d","labels":[]}`, n+1, n+1, strings.Repeat("software ", 64), n+1))
						if _, err := w.PutIssue(ctx, "demo/support", raw); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				status, err := db.Index(ctx, embedding.Default, IndexOptions{Workers: workers})
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(status.Chunks), "chunks/op")
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}

func TestIncrementalPassageReuse(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	seed(t, db)
	encoder := &fakeVectorizer{}
	if _, err := db.Index(ctx, encoder, IndexOptions{Workers: 1}); err != nil {
		t.Fatal(err)
	}
	before := encoder.calls
	if err := db.Update(ctx, func(w *Writer) error {
		_, err := w.PutIssue(ctx, "o/r", issueFixture(1, "new body", "2026-01-04T00:00:00Z"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Index(ctx, encoder, IndexOptions{Workers: 1}); err != nil {
		t.Fatal(err)
	}
	if encoder.calls-before != 1 {
		t.Fatal("unchanged title/labels/comments re-encoded", encoder.calls-before)
	}
}
func TestObsoleteSemanticExport(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	seed(t, db)
	encoder := &fakeVectorizer{}
	if err := db.Update(ctx, func(w *Writer) error {
		_, err := w.PutIssue(ctx, "o/r", issueFixture(1, "obsolete-private-text-marker", "2026-01-04T00:00:00Z"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Index(ctx, encoder, IndexOptions{Workers: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(ctx, func(w *Writer) error {
		_, err := w.PutIssue(ctx, "o/r", issueFixture(1, "replacement public text", "2026-01-05T00:00:00Z"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "export.sqlite")
	if err := db.Export(ctx, path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("obsolete-private-text-marker")) {
		t.Fatal("obsolete passages leaked into export")
	}
	var chunks int
	if err := db.db.QueryRow("SELECT count(*) FROM semantic_vectors WHERE document_id='issue:o/r:1'").Scan(&chunks); err != nil || chunks == 0 {
		t.Fatal("export destroyed local reusable checkpoints", chunks, err)
	}
}
func TestSemanticSourceTail(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	seed(t, db)
	text := strings.Repeat("Routine unrelated meeting agenda. ", 300) + "The download never completes because transferring attachments freezes."
	raw := commentFixture(text, "2026-01-04T00:00:00Z")
	if err := db.Update(ctx, func(w *Writer) error { return w.PutComment(ctx, "o/r", 1, raw) }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Index(ctx, embedding.Default, IndexOptions{Workers: 4}); err != nil {
		t.Fatal(err)
	}
	result, err := db.Search(ctx, SearchOptions{Engine: "semantic", Query: "a file transfer hangs permanently", In: []string{"comments"}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Number != 1 || !strings.Contains(result.Matches[0].Snippet, "download never completes") {
		t.Fatal("late comment passage lost", result)
	}
	ev := result.Matches[0].Evidence[0]
	if ev.CommentID != "9007199254740993" || ev.Start == 0 || ev.End <= ev.Start || ev.SemanticScore == nil {
		t.Fatal("missing comment provenance", ev)
	}
}
