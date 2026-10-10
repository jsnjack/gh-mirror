package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"gh-mirror/internal/config"
	"gh-mirror/internal/embedding"
	"gh-mirror/internal/store"
)

func TestSearchDefaultAndExplicitEngine(t *testing.T) {
	for _, tc := range []struct {
		name, engine, want string
		indexed, explicit  bool
	}{
		{"default without vectors", "hybrid", "lexical", false, false},
		{"explicit hybrid without vectors", "hybrid", "", false, true},
		{"default with vectors", "hybrid", "hybrid", true, false},
		{"explicit lexical with vectors", "lexical", "lexical", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "mirror.sqlite")
			db, err := store.Open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := db.Update(ctx, func(w *store.Writer) error {
				_, err := w.PutIssue(ctx, "o/r", json.RawMessage(`{"number":1,"node_id":"cli_default","title":"Network timeout","body":"Reconnect the session","state":"closed","updated_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/1","labels":[]}`))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if tc.indexed {
				if _, err := db.Index(ctx, embedding.Default, store.IndexOptions{Workers: 1}); err != nil {
					t.Fatal(err)
				}
			}
			command, _, err := root.Find([]string{"search"})
			if err != nil {
				t.Fatal(err)
			}
			flag := command.Flags().Lookup("engine")
			if flag.DefValue != "hybrid" {
				t.Fatal("wrong advertised default", flag.DefValue)
			}
			oldValue, oldChanged, oldSettings := flag.Value.String(), flag.Changed, settings
			t.Cleanup(func() {
				settings = oldSettings
				if err := flag.Value.Set(oldValue); err != nil {
					t.Error(err)
				}
				flag.Changed = oldChanged
				command.SetOut(nil)
				command.SetErr(nil)
			})
			if err := flag.Value.Set(tc.engine); err != nil {
				t.Fatal(err)
			}
			flag.Changed = tc.explicit
			settings = config.Config{Database: path}
			var stdout bytes.Buffer
			command.SetOut(&stdout)
			command.SetContext(ctx)
			err = command.RunE(command, []string{"network"})
			if tc.want == "" {
				if !errors.Is(err, store.ErrSemanticUnavailable) {
					t.Fatal("explicit hybrid silently fell back", err)
				}
				return
			}
			var out store.SearchResult
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Query.Engine != tc.want {
				t.Fatal("incorrect command engine", out, err)
			}
		})
	}
}
