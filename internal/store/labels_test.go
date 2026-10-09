package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func labeledIssue(t *testing.T, name, updated string) json.RawMessage {
	t.Helper()
	object, err := Object(issueFixture(1, "network failure", updated))
	if err != nil {
		t.Fatal(err)
	}
	labels := []map[string]string{}
	if name != "" {
		labels = append(labels, map[string]string{"name": name})
	}
	object["labels"], err = json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLabelSearch(t *testing.T) {
	for _, name := range []string{"label only", "accented name", "label filter", "rename", "remove", "stale update", "transfer", "deletion", "unused label", "backfill", "backfill rollback", "snapshot"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seed(t, db)
			updated := "2026-01-03T00:00:00Z"
			put := func(label, timestamp, repo string) {
				t.Helper()
				if err := db.Update(ctx, func(w *Writer) error {
					_, err := w.PutIssue(ctx, repo, labeledIssue(t, label, timestamp))
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			put("client:Acme-Café", updated, "o/r")
			options := SearchOptions{Query: "acme", Repo: "o/r"}
			want := 1
			switch name {
			case "accented name":
				options.Query = "café"
			case "label filter":
				options.Label, options.State, options.Type = "client:Acme-Café", "closed", "Bug"
			case "rename":
				put("client:Beta", updated, "o/r")
				options.Query = "beta"
				old, err := db.Search(ctx, SearchOptions{Query: "acme"})
				if err != nil || len(old.Matches) != 0 {
					t.Fatal("renamed label remained searchable", old, err)
				}
			case "remove":
				put("", updated, "o/r")
				want = 0
			case "stale update":
				put("client:Beta", "2026-01-02T00:00:00Z", "o/r")
			case "transfer":
				put("client:Acme-Café", updated, "o/s")
				old, err := db.Search(ctx, options)
				if err != nil || len(old.Matches) != 0 {
					t.Fatal("transfer left old label documents", old, err)
				}
				options.Repo = "o/s"
			case "deletion":
				if err := db.Update(ctx, func(w *Writer) error {
					return w.Reconcile(ctx, "o/r", map[int]bool{2: true}, map[string]bool{})
				}); err != nil {
					t.Fatal(err)
				}
				want = 0
			case "unused label":
				if err := db.Update(ctx, func(w *Writer) error {
					return w.ReplaceCatalog(ctx, "labels", "o/r", []json.RawMessage{json.RawMessage(`{"id":42,"name":"UnusedClient"}`)})
				}); err != nil {
					t.Fatal(err)
				}
				options.Query, want = "UnusedClient", 0
			case "backfill", "backfill rollback":
				if err := db.Update(ctx, func(w *Writer) error {
					if _, err := w.tx.ExecContext(ctx, "DELETE FROM documents WHERE id LIKE 'labels:%'; DELETE FROM metadata WHERE key='label_index_version'"); err != nil {
						return fmt.Errorf("simulate legacy label index: %w", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if name == "backfill rollback" {
					err := db.Update(ctx, func(w *Writer) error {
						if err := w.EnsureLabelIndex(ctx); err != nil {
							return err
						}
						return fmt.Errorf("collection interrupted")
					})
					if err == nil {
						t.Fatal("failed backfill committed")
					}
					out, err := db.Search(ctx, options)
					if err != nil || len(out.Matches) != 0 {
						t.Fatal("partial label index became visible", out, err)
					}
				}
				if err := db.Update(ctx, func(w *Writer) error { return w.EnsureLabelIndex(ctx) }); err != nil {
					t.Fatal(err)
				}
			case "snapshot":
				path := filepath.Join(t.TempDir(), "mirror.sqlite")
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
				db = reader
			}
			out, err := db.Search(ctx, options)
			if err != nil || len(out.Matches) != want {
				t.Fatal("incorrect label search", out, err)
			}
			if want > 0 && (out.Matches[0].Number != 1 || out.Matches[0].Title != "Connection crash" || !strings.Contains(strings.ToLower(out.Matches[0].Snippet), options.Query)) {
				t.Fatal("label match lost the ticket title or evidence", out)
			}
		})
	}
}
