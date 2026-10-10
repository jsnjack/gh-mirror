package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

type driftingVectorizer struct {
	fakeVectorizer
	drift float32
}

func (e *driftingVectorizer) Embed(ctx context.Context, text string) ([]float32, error) {
	v, err := e.fakeVectorizer.Embed(ctx, text)
	if err != nil {
		return nil, err
	}
	v[0] = float32(math.Sqrt(1 - float64(e.drift)*float64(e.drift)))
	v[1] = e.drift
	return v, nil
}

func TestSemanticCursorBindsQueryVector(t *testing.T) {
	db := testStore(t)
	seed(t, db)
	e := &driftingVectorizer{}
	db.vectorizer = e
	ctx := context.Background()
	if _, err := db.Index(ctx, e, IndexOptions{Workers: 1}); err != nil {
		t.Fatal(err)
	}
	options := SearchOptions{Engine: "semantic", Query: "query", Limit: 1}
	page, err := db.Search(ctx, options)
	if err != nil || page.NextCursor == "" {
		t.Fatal("missing first page", page, err)
	}
	options.Cursor = page.NextCursor
	if _, err := db.Search(ctx, options); err != nil {
		t.Fatal("stable query cursor rejected", err)
	}
	e.drift = 0.01
	if _, err := db.Search(ctx, options); !errors.Is(err, ErrCursorConflict) {
		t.Fatal("backend numerical change did not return stale cursor", err)
	}
}

type batchFixture struct {
	fakeVectorizer
	batches []int
	cancel  context.CancelFunc
}

func (e *batchFixture) BatchSize() int { return 4 }
func (e *batchFixture) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.batches = append(e.batches, len(texts))
	e.mu.Unlock()
	out := make([][]float32, len(texts))
	for n, text := range texts {
		var err error
		out[n], err = e.Embed(ctx, text)
		if err != nil {
			return nil, err
		}
	}
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	return out, nil
}

func TestBatchIndexCheckpoints(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "incremental", true: "cancelled batch"}[cancelled], func(t *testing.T) {
			db := testStore(t)
			ctx := context.Background()
			body := ""
			for n := range 6 {
				body += strings.Repeat(string('a'+rune(n)), 20)
			}
			err := db.Update(ctx, func(w *Writer) error {
				_, err := w.PutIssue(ctx, "o/r", json.RawMessage(`{"number":1,"node_id":"batch","title":"","body":"`+body+`","state":"open","updated_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/1","labels":[]}`))
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			e := &batchFixture{}
			run, stop := context.WithCancel(ctx)
			defer stop()
			if cancelled {
				e.cancel = stop
			}
			_, err = db.Index(run, e, IndexOptions{Workers: 1})
			if cancelled && !errors.Is(err, context.Canceled) || !cancelled && err != nil {
				t.Fatal(err)
			}
			var saved int
			if err := db.db.QueryRow("SELECT count(*) FROM semantic_vectors").Scan(&saved); err != nil {
				t.Fatal(err)
			}
			want := 6
			if cancelled {
				want = 4
			}
			if saved != want {
				t.Fatal("completed batch vectors lost", saved, want)
			}
			before := e.calls
			status, err := db.Index(ctx, e, IndexOptions{Workers: 1})
			if err != nil || status.PendingDocuments != 0 || e.calls-before != 6-saved {
				t.Fatal("resume recomputed saved vectors", e.calls-before, status, err)
			}
			if len(e.batches) == 0 || e.batches[0] != 4 {
				t.Fatal("batching absent", e.batches)
			}
			before = e.calls
			if _, err := db.Index(ctx, e, IndexOptions{Workers: 8}); err != nil || e.calls != before {
				t.Fatal("unchanged indexing invoked inference", err, e.calls, before)
			}
		})
	}
}

func TestOpenWithVectorizer(t *testing.T) {
	ctx := context.Background()
	e := &batchFixture{}
	db, err := OpenWithVectorizer(filepath.Join(t.TempDir(), "mirror.sqlite"), false, e)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	seed(t, db)
	if _, err := db.Index(ctx, e, IndexOptions{Workers: 1}); err != nil {
		t.Fatal(err)
	}
	before := e.calls
	out, err := db.Search(ctx, SearchOptions{Engine: "semantic", Query: strings.Repeat("query text ", 5)})
	if err != nil || len(out.Matches) == 0 || e.calls == before || e.batches[len(e.batches)-1] != 3 {
		t.Fatal("query did not use selected batch encoder", out, err, e.calls, e.batches)
	}
}
