package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"gh-mirror/internal/config"
	"gh-mirror/internal/store"
)

func TestOfflineIndexCommands(t *testing.T) {
	for _, name := range []string{"model", "index"} {
		t.Run(name, func(t *testing.T) {
			command, _, err := root.Find([]string{name})
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			command.SetOut(&stdout)
			command.SetErr(&stderr)
			command.SetContext(context.Background())
			t.Cleanup(func() { command.SetOut(nil); command.SetErr(nil) })
			if name == "index" {
				path := filepath.Join(t.TempDir(), "fixture.sqlite")
				db, err := store.Open(path, false)
				if err != nil {
					t.Fatal(err)
				}
				err = db.Update(context.Background(), func(w *store.Writer) error {
					_, err := w.PutIssue(context.Background(), "demo/support", json.RawMessage(`{"number":1,"node_id":"command_fixture","title":"Download stops before completion","body":"The transfer freezes","state":"open","updated_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/demo/support/issues/1","labels":[]}`))
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				old := settings
				settings = config.Config{Database: path}
				t.Cleanup(func() { settings = old })
			}
			if err := command.RunE(command, nil); err != nil {
				t.Fatal(err)
			}
			var result map[string]json.RawMessage
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal("stdout is not JSON", err)
			}
			if name == "model" {
				if result["fingerprint"] == nil || result["dimension"] == nil {
					t.Fatal("missing model identity", stdout.String())
				}
			}
			if name == "index" {
				if string(result["pending_documents"]) != "0" || !strings.Contains(stderr.String(), "Index complete") || strings.Contains(stderr.String(), "API:") {
					t.Fatal("missing completed offline index progress", stdout.String(), stderr.String())
				}
			}
		})
	}
}
