package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gh-mirror/internal/store"
)

func source(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err = db.Update(context.Background(), func(w *store.Writer) error {
		if _, err := w.PutIssue(context.Background(), "o/r", json.RawMessage(fmt.Sprintf(`{"number":1,"node_id":"I_1","title":"snapshotword","body":"body","state":"closed","updated_at":%q,"html_url":"https://github.com/o/r/issues/1"}`, now))); err != nil {
			return fmt.Errorf("seed issue: %w", err)
		}
		for key, value := range map[string]string{"generation": "generationone", "collected_at": now, "enriched_at": now, "repositories": `["o/r"]`} {
			if err := w.SetMetadata(context.Background(), key, value); err != nil {
				return fmt.Errorf("seed metadata: %w", err)
			}
		}
		return w.SetCoverage(context.Background(), store.Coverage{Repo: "o/r", CollectedAt: now, ReconciledAt: now, Fields: "complete", Projects: "complete"})
	})
	if err != nil {
		t.Fatal(err)
	}
	return db, filepath.Join(dir, "snapshots")
}
func writeManifest(t *testing.T, path string, m Manifest) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestSnapshots(t *testing.T) {
	for _, name := range []string{"local isolated", "readonly publication", "HTTP authenticated", "checksum failure", "scope failure", "stale failure", "schema failure", "unsafe filename", "embedded mismatch", "publication failure", "sidecar rejection"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, dir := source(t)
			m, err := Publish(ctx, db, dir)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "latest.json")
			dest := filepath.Join(t.TempDir(), "private.sqlite")
			if err := os.WriteFile(dest, []byte("previous destination"), 0600); err != nil {
				t.Fatal(err)
			}
			o := Options{Source: path, Destination: dest, Repositories: []string{"o/r"}, MaxAge: time.Hour}
			shouldFail := false
			switch name {
			case "local isolated":
				if err := db.Update(ctx, func(w *store.Writer) error { return w.SetMetadata(ctx, "generation", "generationtwo") }); err != nil {
					t.Fatal(err)
				}
			case "readonly publication":
				reader, err := store.Open(db.Path, true)
				if err != nil {
					t.Fatal(err)
				}
				m, err = Publish(ctx, reader, dir)
				if closeErr := reader.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "HTTP authenticated":
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer secret" {
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					file := r.URL.Path[len("/snapshots/"):]
					if file == "latest" {
						file = "latest.json"
					}
					http.ServeFile(w, r, filepath.Join(dir, file))
				}))
				defer server.Close()
				o.Source = server.URL + "/snapshots/latest"
				o.Token = "secret"
			case "checksum failure":
				if err := os.WriteFile(filepath.Join(dir, m.Filename), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
				shouldFail = true
			case "scope failure":
				o.Repositories = []string{"different/repo"}
				shouldFail = true
			case "stale failure":
				m.CollectedAt = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
				writeManifest(t, path, m)
				shouldFail = true
			case "schema failure":
				m.SchemaVersion = 999
				writeManifest(t, path, m)
				shouldFail = true
			case "unsafe filename":
				m.Filename = "../state.sqlite"
				writeManifest(t, path, m)
				shouldFail = true
			case "embedded mismatch":
				m.Generation = "wrong"
				writeManifest(t, path, m)
				shouldFail = true
			case "publication failure":
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Update(ctx, func(w *store.Writer) error { return w.SetMetadata(ctx, "generation", "") }); err != nil {
					t.Fatal(err)
				}
				if _, err := Publish(ctx, db, dir); err == nil {
					t.Fatal("published incomplete generation")
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(before) != string(after) {
					t.Fatal("failed publication replaced latest pointer")
				}
			case "sidecar rejection":
				if err := os.WriteFile(dest+"-wal", []byte("live"), 0600); err != nil {
					t.Fatal(err)
				}
				shouldFail = true
			}
			got, err := Acquire(ctx, o)
			if shouldFail {
				if err == nil {
					t.Fatal("acquired invalid snapshot")
				}
				previous, readErr := os.ReadFile(dest)
				if readErr != nil || string(previous) != "previous destination" {
					t.Fatal("failed acquisition replaced prior destination", readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Generation != m.Generation || got.SHA256 != m.SHA256 {
				t.Fatal("acquired different generation", got)
			}
			reader, err := store.Open(dest, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			}()
			out, err := reader.Search(ctx, store.SearchOptions{Query: "snapshotword"})
			if err != nil || len(out.Matches) != 1 || out.Status.Generation != "generationone" {
				t.Fatal("snapshot changed with source", out, err)
			}
			if info, err := os.Stat(dest); err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("destination permissions", err)
			}
		})
	}
}
