package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSyncStatus(t *testing.T) {
	for _, name := range []string{"default", "quiet", "failure", "cancellation"} {
		t.Run(name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/owner/repo/issues" {
					close(entered)
					<-release
					if name == "failure" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
				}
				body := `[]`
				if r.URL.Path == "/repos/owner/repo" {
					body = `{"owner":{"type":"User"}}`
				}
				if _, err := w.Write([]byte(body)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			// Unblock before server cleanup even when an assertion stops the test.
			defer unblock()
			dir := t.TempDir()
			configuration, err := json.Marshal(map[string]any{
				"repositories": []string{"owner/repo"}, "database": filepath.Join(dir, "state.sqlite"),
				"snapshot_dir": filepath.Join(dir, "snapshots"), "api_url": server.URL,
				"graphql_url": server.URL + "/graphql", "fields": false, "projects": false,
				"token_env": "GH_MIRROR_TEST_TOKEN",
			})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, configuration, 0600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr statusBuffer
			stdout.beforeWrite = func() {
				if name == "default" && !strings.Contains(stderr.String(), "Sync complete") {
					t.Error("stdout wrote before terminal progress finished")
				}
			}
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			args := []string{"--config", path, "sync", "--publish=false", "--quiet=false"}
			if name == "default" {
				args[len(args)-2] = "--publish=true"
			}
			if name == "quiet" {
				args[len(args)-1] = "--quiet"
			}
			root.SetArgs(args)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			command, _, err := root.Find([]string{"sync"})
			if err != nil {
				t.Fatal(err)
			}
			command.SetContext(ctx)
			completed := make(chan error, 1)
			go func() { completed <- root.ExecuteContext(ctx) }()
			select {
			case <-entered:
			case err := <-completed:
				t.Fatalf("sync exited before its first request: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("sync never started")
			}
			if stdout.String() != "" {
				t.Fatal("status polluted JSON stdout", stdout.String())
			}
			if name == "quiet" {
				if stderr.String() != "" {
					t.Fatal("quiet sync printed progress", stderr.String())
				}
			} else if !strings.Contains(stderr.String(), "Fetching issues") {
				t.Fatal("sync was silent while waiting for its first HTTP response", stderr.String())
			}
			if name == "cancellation" {
				cancel()
			}
			unblock()
			select {
			case err = <-completed:
			case <-time.After(5 * time.Second):
				t.Fatal("sync did not finish")
			}
			switch name {
			case "failure":
				if err == nil || !strings.Contains(stderr.String(), "Sync failed") || stdout.String() != "" {
					t.Fatal("failed sync reported success", err, stderr.String(), stdout.String())
				}
			case "cancellation":
				if !errors.Is(err, context.Canceled) || !strings.Contains(stderr.String(), "Sync cancelled") || stdout.String() != "" {
					t.Fatal("cancelled sync reported success", err, stderr.String(), stdout.String())
				}
			default:
				if err != nil || !json.Valid([]byte(stdout.String())) {
					t.Fatal("sync did not return JSON", err, stdout.String())
				}
				if name == "default" && !strings.Contains(stderr.String(), "Sync complete") {
					t.Fatal("missing completion status", stderr.String())
				}
				if name == "default" && !strings.Contains(stderr.String(), "Publishing snapshot") {
					t.Fatal("snapshot publication was silent", stderr.String())
				}
			}
			if strings.Contains(stderr.String(), "\x1b[") {
				t.Fatal("nonterminal output contained cursor controls", stderr.String())
			}
		})
	}
}

type statusBuffer struct {
	mu          sync.Mutex
	b           bytes.Buffer
	beforeWrite func()
}

func (b *statusBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.beforeWrite != nil {
		b.beforeWrite()
	}
	return b.b.Write(data)
}

func (b *statusBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
