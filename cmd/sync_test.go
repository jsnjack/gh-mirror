package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gh-mirror/internal/checkpoint"
	"gh-mirror/internal/store"

	"github.com/pelletier/go-toml/v2"
)

// conditionalSync runs sync against a local fixture and counts its upstream requests.
type conditionalSync struct {
	t        *testing.T
	config   string
	database string
	requests atomic.Int64
}

func newConditionalSync(t *testing.T, settings map[string]any) *conditionalSync {
	t.Helper()
	s := &conditionalSync{t: t}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		body := `[]`
		if r.URL.Path == "/repos/owner/repo" {
			body = `{"owner":{"type":"User"}}`
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	s.database = filepath.Join(dir, "state.sqlite")
	values := map[string]any{
		"repos": map[string]any{"owner/repo": map[string]any{}}, "database": s.database,
		"snapshot_dir": filepath.Join(dir, "snapshots"), "api_url": server.URL,
		"graphql_url": server.URL + "/graphql", "fields": false, "projects": false,
		"token_env": "GH_MIRROR_TEST_TOKEN",
	}
	for key, value := range settings {
		values[key] = value
	}
	raw, err := toml.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	s.config = filepath.Join(dir, "config.toml")
	if err := os.WriteFile(s.config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command, _, err := root.Find([]string{"sync"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for name, value := range map[string]string{"if-stale": "false", "stale-after": "0s", "index": "true", "publish": "true", "quiet": "false"} {
			flag := command.Flags().Lookup(name)
			if err := flag.Value.Set(value); err != nil {
				t.Error(err)
			}
			flag.Changed = false
		}
	})
	return s
}

// run executes sync and returns its JSON result and the upstream requests it made.
func (s *conditionalSync) run(args ...string) (map[string]any, int64, error) {
	s.t.Helper()
	before := s.requests.Load()
	var stdout, stderr strings.Builder
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"--config", s.config, "--format", "json", "sync", "--quiet", "--publish=false", "--index=false"}, args...))
	command, _, err := root.Find([]string{"sync"})
	if err != nil {
		s.t.Fatal(err)
	}
	// Cobra keeps a context set by an earlier test on the subcommand.
	command.SetContext(context.Background())
	err = root.ExecuteContext(context.Background())
	requests := s.requests.Load() - before
	if err != nil {
		return nil, requests, err
	}
	var out map[string]any
	if decodeErr := json.Unmarshal([]byte(stdout.String()), &out); decodeErr != nil {
		s.t.Fatalf("sync output is not JSON: %v: %q", decodeErr, stdout.String())
	}
	return out, requests, nil
}

func TestConditionalSyncCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings map[string]any
		collect  bool
		session  bool
		lock     bool
		args     []string
		reason   string
		fails    string
	}{
		{name: "missing database collects", args: []string{"--if-stale"}},
		{name: "fresh mirror skips without requests", collect: true, args: []string{"--if-stale"}, reason: "fresh"},
		{name: "flag makes the mirror stale", collect: true, args: []string{"--if-stale", "--stale-after", "1ns"}},
		{name: "configured stale_after makes the mirror stale", settings: map[string]any{"stale_after": "1ns"}, collect: true, args: []string{"--if-stale"}},
		{name: "flag overrides configured stale_after", settings: map[string]any{"stale_after": "1ns"}, collect: true, args: []string{"--if-stale", "--stale-after", "1h"}, reason: "fresh"},
		{name: "running collector skips", collect: true, session: true, lock: true, args: []string{"--if-stale", "--stale-after", "1ns"}, reason: "running"},
		{name: "interrupted session resumes", collect: true, session: true, args: []string{"--if-stale", "--stale-after", "1ns"}},
		{name: "lock taken after the check reports running", collect: true, lock: true, args: []string{"--if-stale", "--stale-after", "1ns"}, reason: "running"},
		{name: "stale-after requires if-stale", args: []string{"--stale-after", "1h"}, fails: "--stale-after requires --if-stale"},
		{name: "zero stale-after", args: []string{"--if-stale", "--stale-after", "0s"}, fails: "positive duration"},
		{name: "negative stale-after", args: []string{"--if-stale", "--stale-after", "-1m"}, fails: "positive duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newConditionalSync(t, tc.settings)
			if tc.collect {
				if _, _, err := s.run(); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			if tc.session {
				pending, err := checkpoint.Open(ctx, s.database+".sync.sqlite", "interrupted", time.Now(), false)
				if err != nil {
					t.Fatal(err)
				}
				if err := pending.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.lock {
				db, err := store.Open(s.database, false)
				if err != nil {
					t.Fatal(err)
				}
				held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() {
					done <- db.Update(ctx, func(*store.Writer) error {
						close(held)
						<-release
						return nil
					})
				}()
				<-held
				defer func() {
					close(release)
					if err := <-done; err != nil {
						t.Error(err)
					}
					if err := db.Close(); err != nil {
						t.Error(err)
					}
				}()
			}
			info, statErr := os.Stat(s.database)
			out, requests, err := s.run(tc.args...)
			if tc.fails != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fails) || requests != 0 {
					t.Fatalf("got %v after %d requests; want %q", err, requests, tc.fails)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.reason == "" {
				if out["skipped"] != nil || out["status"] == nil || requests == 0 {
					t.Fatalf("stale mirror did not collect: %d requests, %v", requests, out)
				}
				return
			}
			if out["skipped"] != true || out["reason"] != tc.reason || requests != 0 {
				t.Fatalf("got %v after %d requests; want %s skip", out, requests, tc.reason)
			}
			if out["collected_at"] == "" || out["age"] == "" || out["stale_after"] == "" {
				t.Fatalf("skip omits collection age: %v", out)
			}
			if tc.reason == "fresh" {
				after, err := os.Stat(s.database)
				if statErr != nil || err != nil || !after.ModTime().Equal(info.ModTime()) || after.Size() != info.Size() {
					t.Fatal("fresh skip changed the mirror", statErr, err)
				}
			}
		})
	}
}

func TestSyncSkipText(t *testing.T) {
	for _, tc := range []struct {
		name string
		skip syncSkip
		want string
	}{
		{name: "fresh", skip: syncSkip{Skipped: true, Reason: "fresh", CollectedAt: "2026-10-15T10:00:00Z", Age: "23m0s", StaleAfter: "1h0m0s"}, want: "Sync skipped: the mirror is fresh | collected 23m0s ago at 2026-10-15T10:00:00Z | stale after 1h0m0s\n"},
		{name: "running before first collection", skip: syncSkip{Skipped: true, Reason: "running", StaleAfter: "1h0m0s"}, want: "Sync skipped: another sync is running | never collected | stale after 1h0m0s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := humanOutput(tc.skip)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}
