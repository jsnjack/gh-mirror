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

func TestSchemaTwoVectorMigration(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	seed(t, db)
	e := &fakeVectorizer{}
	if _, err := db.Index(ctx, e, IndexOptions{Workers: 1}); err != nil {
		t.Fatal(err)
	}
	before := e.calls
	if _, err := db.db.Exec(`DROP INDEX semantic_vectors_cache;
ALTER TABLE semantic_vectors RENAME TO new_vectors;
CREATE TABLE semantic_vectors(document_id TEXT NOT NULL REFERENCES semantic_documents(document_id) ON DELETE CASCADE,chunk INTEGER NOT NULL,field TEXT NOT NULL,start INTEGER NOT NULL,end INTEGER NOT NULL,text TEXT NOT NULL,vector BLOB NOT NULL CHECK(length(vector)=1536),PRIMARY KEY(document_id,chunk));
INSERT INTO semantic_vectors SELECT document_id,chunk,field,start,end,text,COALESCE(v.vector,c.vector) FROM new_vectors v LEFT JOIN embedding_cache c ON c.key=v.cache_key;
DROP TABLE new_vectors; DROP TABLE embedding_cache;
UPDATE metadata SET value='2' WHERE key='schema_version'; DELETE FROM metadata WHERE key='representation_version';`); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenWithVectorizer(db.Path, true, e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Search(ctx, SearchOptions{Query: "network", Engine: "hybrid"}); err != nil {
		t.Fatal("schema-2 reader failed", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
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
	if out, err := writer.Index(ctx, e, IndexOptions{Workers: 1}); err != nil || out.PendingDocuments != 0 || e.calls != before+1 { // One query embedding above; migration reuses every unchanged passage.
		t.Fatal("migration refetched or recomputed unchanged passages", out, e.calls-before, err)
	}
	status, err := writer.Status(ctx)
	if err != nil || status.SchemaVersion != 3 || status.CollectionVersion != CollectionVersion || status.Generation != "testgeneration" {
		t.Fatal(status, err)
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

func TestCacheLayoutMigration(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	seed(t, db)
	e := &fakeVectorizer{}
	initial, err := db.Index(ctx, e, IndexOptions{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	before := e.calls
	if _, err := db.db.Exec(`DROP INDEX semantic_vectors_cache; ALTER TABLE semantic_vectors RENAME TO old_vectors; ALTER TABLE embedding_cache RENAME TO old_cache;
CREATE TABLE embedding_cache(key BLOB PRIMARY KEY,vector BLOB NOT NULL) WITHOUT ROWID;
INSERT INTO embedding_cache SELECT key,vector FROM old_cache;
CREATE TABLE semantic_vectors(document_id TEXT NOT NULL REFERENCES semantic_documents(document_id) ON DELETE CASCADE,chunk INTEGER NOT NULL,field TEXT NOT NULL,start INTEGER NOT NULL,end INTEGER NOT NULL,text TEXT NOT NULL,vector BLOB,cache_key BLOB REFERENCES embedding_cache(key),PRIMARY KEY(document_id,chunk));
INSERT INTO semantic_vectors SELECT document_id,chunk,field,start,end,text,vector,cache_key FROM old_vectors;
DROP TABLE old_vectors; DROP TABLE old_cache;`); err != nil {
		t.Fatal(err)
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
	out, err := writer.Index(ctx, e, IndexOptions{Workers: 1})
	if err != nil || e.calls != before || out.Chunks != initial.Chunks || out.SharedVectors != initial.SharedVectors || out.Generation != initial.Generation {
		t.Fatal("layout migration changed embeddings", out, e.calls-before, err)
	}
	var definition string
	if err := writer.db.QueryRow("SELECT sql FROM sqlite_master WHERE name='embedding_cache'").Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(definition), "WITHOUT ROWID") {
		t.Fatal(definition)
	}
}
