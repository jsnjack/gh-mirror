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
	"github.com/spf13/pflag"
)

// conditionalSync runs sync against a local fixture and counts its upstream requests.
type conditionalSync struct {
	t        *testing.T
	config   string
	database string
	requests atomic.Int64
	// fail makes the fixture reject every request.
	fail atomic.Bool
}

func newConditionalSync(t *testing.T, settings map[string]any) *conditionalSync {
	t.Helper()
	s := &conditionalSync{t: t}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if s.fail.Load() {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
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
	t.Setenv("GH_MIRROR_TEST_TOKEN", "test-token")
	if err := os.WriteFile(s.config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command, _, err := root.Find([]string{"sync"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for flags, values := range map[*pflag.FlagSet]map[string]string{
			root.PersistentFlags(): {"ensure-fresh": "false", "stale-after": "0s"},
			command.Flags():        {"index": "true", "quiet": "false"},
		} {
			for name, value := range values {
				flag := flags.Lookup(name)
				if err := flag.Value.Set(value); err != nil {
					t.Error(err)
				}
				flag.Changed = false
			}
		}
	})
	return s
}

// run executes sync and returns its JSON result and the upstream requests it made.
func (s *conditionalSync) run(args ...string) (map[string]any, int64, error) {
	s.t.Helper()
	stdout, _, requests, err := s.exec(append([]string{"sync", "--quiet", "--index=false"}, args...)...)
	if err != nil {
		return nil, requests, err
	}
	var out map[string]any
	if decodeErr := json.Unmarshal([]byte(stdout), &out); decodeErr != nil {
		s.t.Fatalf("sync output is not JSON: %v: %q", decodeErr, stdout)
	}
	return out, requests, nil
}

// exec runs any command with JSON output and returns stdout, stderr and the upstream requests it made.
func (s *conditionalSync) exec(args ...string) (string, string, int64, error) {
	s.t.Helper()
	before := s.requests.Load()
	var stdout, stderr strings.Builder
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"--config", s.config, "--format", "json"}, args...))
	command, _, err := root.Find(args[:1])
	if err != nil {
		s.t.Fatal(err)
	}
	// Cobra keeps a context set by an earlier test on the subcommand.
	command.SetContext(context.Background())
	err = root.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), s.requests.Load() - before, err
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
		{name: "missing database collects", args: []string{"--ensure-fresh"}},
		{name: "fresh mirror skips without requests", collect: true, args: []string{"--ensure-fresh"}, reason: "fresh"},
		{name: "flag makes the mirror stale", collect: true, args: []string{"--ensure-fresh", "--stale-after", "1ns"}},
		{name: "configured stale_after makes the mirror stale", settings: map[string]any{"stale_after": "1ns"}, collect: true, args: []string{"--ensure-fresh"}},
		{name: "flag overrides configured stale_after", settings: map[string]any{"stale_after": "1ns"}, collect: true, args: []string{"--ensure-fresh", "--stale-after", "1h"}, reason: "fresh"},
		{name: "configured ensure_fresh leaves plain sync unconditional", settings: map[string]any{"ensure_fresh": true}, collect: true},
		{name: "running collector skips", collect: true, session: true, lock: true, args: []string{"--ensure-fresh", "--stale-after", "1ns"}, reason: "running"},
		{name: "interrupted session resumes", collect: true, session: true, args: []string{"--ensure-fresh", "--stale-after", "1ns"}},
		{name: "lock taken after the check reports running", collect: true, lock: true, args: []string{"--ensure-fresh", "--stale-after", "1ns"}, reason: "running"},
		{name: "stale-after requires ensure-fresh", args: []string{"--stale-after", "1h"}, fails: "--stale-after requires --ensure-fresh"},
		{name: "zero stale-after", args: []string{"--ensure-fresh", "--stale-after", "0s"}, fails: "positive duration"},
		{name: "negative stale-after", args: []string{"--ensure-fresh", "--stale-after", "-1m"}, fails: "positive duration"},
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

func TestEnsureFreshBeforeReads(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings map[string]any
		collect  bool
		fail     bool
		args     []string
		syncs    bool
		warns    bool
		fails    string
	}{
		{name: "flag syncs a missing mirror", args: []string{"--ensure-fresh", "status"}, syncs: true},
		{name: "flag leaves a fresh mirror alone", collect: true, args: []string{"--ensure-fresh", "status"}},
		{name: "flag syncs a stale mirror", collect: true, args: []string{"--ensure-fresh", "--stale-after", "1ns", "status"}, syncs: true},
		{name: "configuration syncs queries", settings: map[string]any{"ensure_fresh": true, "stale_after": "1ns"}, collect: true, args: []string{"list"}, syncs: true},
		{name: "false flag skips configured sync", settings: map[string]any{"ensure_fresh": true, "stale_after": "1ns"}, collect: true, args: []string{"--ensure-fresh=false", "status"}},
		{name: "configuration leaves unrelated commands alone", settings: map[string]any{"ensure_fresh": true, "stale_after": "1ns"}, collect: true, args: []string{"scope"}},
		{name: "failed sync warns and reads the old mirror", collect: true, fail: true, args: []string{"--ensure-fresh", "--stale-after", "1ns", "status"}, warns: true},
		{name: "failed first sync fails", fail: true, args: []string{"--ensure-fresh", "status"}, fails: "ensure fresh mirror"},
		{name: "unsupported command rejects the flag", args: []string{"--ensure-fresh", "scope"}, fails: "--ensure-fresh applies only to"},
		{name: "stale-after requires ensure-fresh", args: []string{"--stale-after", "1h", "status"}, fails: "--stale-after requires --ensure-fresh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newConditionalSync(t, tc.settings)
			if tc.collect {
				if _, _, err := s.run(); err != nil {
					t.Fatal(err)
				}
			}
			s.fail.Store(tc.fail)
			stdout, stderr, requests, err := s.exec(tc.args...)
			if tc.fails != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fails) {
					t.Fatalf("got %v; want %q", err, tc.fails)
				}
				return
			}
			if err != nil {
				t.Fatal(err, stderr)
			}
			if !json.Valid([]byte(stdout)) {
				t.Fatalf("stdout is not one JSON result: %q", stdout)
			}
			if synced := requests > 0 && !tc.fail; synced != tc.syncs {
				t.Fatalf("synced = %v after %d requests; want %v", synced, requests, tc.syncs)
			}
			if warned := strings.Contains(stderr, "warning:"); warned != tc.warns {
				t.Fatalf("stderr %q; want warning %v", stderr, tc.warns)
			}
		})
	}
	t.Run("servers ignore the configuration", func(t *testing.T) {
		previous := settings
		settings.EnsureFresh, settings.StaleAfter = true, "1h"
		t.Cleanup(func() { settings = previous })
		for name, want := range map[string]bool{"serve": false, "mcp": false, "search": true, "status": true, "sync": false} {
			command, _, err := root.Find([]string{name})
			if err != nil {
				t.Fatal(err)
			}
			staleAfter, err := staleThreshold(command)
			if err != nil || (staleAfter > 0) != want {
				t.Errorf("%s: threshold %v, %v; want enabled %v", name, staleAfter, err, want)
			}
		}
	})
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
