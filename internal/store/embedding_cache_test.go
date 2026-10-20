package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedEmbeddings(t *testing.T) {
	for _, name := range []string{"one inference per input", "unchanged edit", "model change", "export prune", "representation change"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seed(t, db)
			e := &fakeVectorizer{}
			initial, err := db.Index(ctx, e, IndexOptions{Workers: 8})
			if err != nil {
				t.Fatal(err)
			}
			if initial.SharedVectors >= initial.Chunks || e.calls != initial.SharedVectors {
				t.Fatal("identical inputs recomputed or duplicated", e.calls, initial)
			}
			var inline int
			if err := db.db.QueryRow("SELECT count(*) FROM semantic_vectors WHERE vector IS NOT NULL").Scan(&inline); err != nil || inline != 0 {
				t.Fatal("new vectors are not shared", inline, err)
			}
			before := e.calls
			switch name {
			case "unchanged edit":
				if err := db.Update(ctx, func(w *Writer) error {
					_, err := w.PutIssue(ctx, "o/r", issueFixture(2, "network failure", "2026-01-03T00:00:00Z"))
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "model change":
				e.identity = "changed-model"
			case "representation change":
				if err := db.Update(ctx, func(w *Writer) error { return w.SetMetadata(ctx, "representation_version", "obsolete") }); err != nil {
					t.Fatal(err)
				}
			case "export prune":
				if err := db.Update(ctx, func(w *Writer) error {
					_, err := w.tx.ExecContext(ctx, "INSERT INTO embedding_cache(key,vector) SELECT ?,vector FROM embedding_cache LIMIT 1", embeddingKey(e.ID(), "unreferenced input"))
					return err
				}); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "export.sqlite")
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
				status, err := reader.Status(ctx)
				if err != nil || status.Semantic.SharedVectors != initial.SharedVectors {
					t.Fatal("orphan cache exported", status, err)
				}
				return
			}
			out, err := db.Index(ctx, e, IndexOptions{Workers: 8})
			if err != nil {
				t.Fatal(err)
			}
			if name == "model change" {
				if e.calls-before != initial.SharedVectors {
					t.Fatal("model change reused old embeddings", e.calls-before, initial)
				}
			} else if e.calls != before {
				t.Fatal("unchanged inputs recomputed", e.calls-before)
			}
			if out.PendingDocuments != 0 {
				t.Fatal(out)
			}
		})
	}
}

func TestDerivedSectionPassageOffsets(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	body := "### Description\nRésumé failure\n### Expected behavior\n_No response_\n### Before requesting the review, make sure you've:\nboilerplate marker\n### Steps to reproduce\n```\nexact code\n```\n"
	if err := db.Update(ctx, func(w *Writer) error {
		_, err := w.PutIssue(ctx, "o/r", issueFixture(1, body, "2026-01-01T00:00:00Z"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Index(ctx, &fakeVectorizer{}, IndexOptions{Workers: 1}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.db.Query("SELECT text,start,end FROM semantic_vectors WHERE field='body'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var text string
		var start, end int
		if err := rows.Scan(&text, &start, &end); err != nil {
			t.Fatal(finishRows(rows, err))
		}
		if body[start:end] != text || strings.Contains(text, "boilerplate") || strings.Contains(text, "No response") {
			t.Fatal(text, start, end)
		}
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		t.Fatal(err)
	}
	raw, err := db.Read(ctx, ReadOptions{Repo: "o/r", Number: 1, Comments: "none"})
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(raw.Payload, &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Body != body {
		t.Fatal("source body changed")
	}
}
