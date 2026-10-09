package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func BenchmarkLocalQuery(b *testing.B) {
	db, err := Open(b.TempDir()+"/mirror.sqlite", false)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			b.Error(err)
		}
	}()
	ctx := context.Background()
	if err := db.Update(ctx, func(w *Writer) error {
		for n := 1; n <= 2000; n++ {
			if _, err := w.PutIssue(ctx, "demo/support", issueFixture(n, "socket timeout "+strings.Repeat("ordinary context ", 20), "2026-01-01T00:00:00Z")); err != nil {
				return err
			}
			for c := 0; c < 4; c++ {
				raw, err := json.Marshal(map[string]any{"id": n*10 + c, "body": "socket timeout discussion context", "updated_at": "2026-01-01T00:00:00Z", "html_url": "https://github.com/demo/support/issues/comment"})
				if err != nil {
					return err
				}
				if err := w.PutComment(ctx, "demo/support", n, raw); err != nil {
					return err
				}
			}
		}
		return w.SetMetadata(ctx, "generation", "benchmark")
	}); err != nil {
		b.Fatal(err)
	}
	for _, name := range []string{"broad search", "selective search", "listing", "candidate retrieval"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				switch name {
				case "broad search":
					_, err = db.Search(ctx, SearchOptions{Query: "socket timeout", Limit: 20})
				case "selective search":
					_, err = db.Search(ctx, SearchOptions{Query: "missing unique term", Match: "phrase", Limit: 20})
				case "listing":
					_, err = db.List(ctx, ListOptions{PageOptions: PageOptions{View: "summary"}, Limit: 20})
				case "candidate retrieval":
					_, err = db.FindCandidates(ctx, CandidateOptions{Repo: "demo/support", Number: 1, Limit: 20})
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
