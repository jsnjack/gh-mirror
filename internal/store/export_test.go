package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExportSanitization(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "writer"
		if readOnly {
			name = "reader"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seed(t, db)
			marker := "ExcludedCollectorPayload"
			identity := "SyntheticCredentialIdentity"
			if err := db.Update(ctx, func(w *Writer) error {
				if err := w.Cache(ctx, "https://api.github.test/orgs/o/issue-fields", "etag", []byte(marker)); err != nil {
					return err
				}
				return w.SetMetadata(ctx, "credential_identity", identity)
			}); err != nil {
				t.Fatal(err)
			}
			source := db
			if readOnly {
				var err error
				source, err = Open(db.Path, true)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := source.Close(); err != nil {
						t.Error(err)
					}
				}()
			}
			path := filepath.Join(t.TempDir(), "export.sqlite")
			if err := source.Export(ctx, path); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil || bytes.Contains(raw, []byte(marker)) || bytes.Contains(raw, []byte(identity)) {
				t.Fatal("export retains private collector data", err)
			}
			exported, err := Open(path, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := exported.Close(); err != nil {
					t.Error(err)
				}
			}()
			for _, table := range []string{"responses", "metadata WHERE key='credential_identity'"} {
				var count int
				if err := exported.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
					t.Fatal("exported collector data", table, count, err)
				}
			}
			out, err := exported.Search(ctx, SearchOptions{Query: "network"})
			if err != nil || len(out.Matches) == 0 {
				t.Fatal("export lost search data", out, err)
			}
			if err := db.Update(ctx, func(w *Writer) error {
				_, body, err := w.Cached(ctx, "https://api.github.test/orgs/o/issue-fields")
				if err != nil {
					return err
				}
				if string(body) != marker {
					t.Fatal("export changed source cache")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
					t.Fatal("export is not standalone", suffix, err)
				}
			}
		})
	}
}

func TestPruneResponseCache(t *testing.T) {
	for _, tc := range []struct {
		name             string
		fields, projects bool
		want             int
	}{
		{"enabled", true, true, 3}, {"fields disabled", false, true, 2}, {"projects disabled", true, false, 2}, {"both disabled", false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			if err := db.Update(ctx, func(w *Writer) error {
				for _, path := range []string{"/repos/o/r/issues", "/orgs/o/issue-fields", "/orgs/o/projectsV2", "/orgs/o/projectsV2/1/items", "/orgs/o/projectsV2/1/fields"} {
					if err := w.Cache(ctx, "https://api.github.test"+path, "etag", []byte(`[]`)); err != nil {
						return err
					}
				}
				return w.PruneResponseCache(ctx, tc.fields, tc.projects)
			}); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.db.QueryRowContext(ctx, "SELECT count(*) FROM responses").Scan(&count); err != nil || count != tc.want {
				t.Fatal("excluded cache retained", count, err)
			}
		})
	}
}
