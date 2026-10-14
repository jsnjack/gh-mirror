package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gh-mirror/internal/checkpoint"
	"gh-mirror/internal/config"
	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

func TestParseTicketRef(t *testing.T) {
	for _, tc := range []struct {
		name, raw, defaultRepo string
		want                   store.TicketID
		fails                  bool
	}{
		{name: "owner/repo#N", raw: "o/r#12", want: store.TicketID{Repo: "o/r", Number: 12}},
		{name: "issue URL", raw: "https://github.com/o/r/issues/12", want: store.TicketID{Repo: "o/r", Number: 12}},
		{name: "pull URL with suffix", raw: "https://github.com/o/r/pull/7/files#diff", want: store.TicketID{Repo: "o/r", Number: 7}},
		{name: "enterprise URL", raw: "https://git.example/o/r/issues/3", want: store.TicketID{Repo: "o/r", Number: 3}},
		{name: "hash with default", raw: "#5", defaultRepo: "o/r", want: store.TicketID{Repo: "o/r", Number: 5}},
		{name: "number with default", raw: "5", defaultRepo: "o/r", want: store.TicketID{Repo: "o/r", Number: 5}},
		{name: "number without default", raw: "5", fails: true},
		{name: "zero", raw: "o/r#0", fails: true},
		{name: "not a number", raw: "o/r#x", fails: true},
		{name: "invalid repository", raw: "../r#1", fails: true},
		{name: "URL without ticket", raw: "https://github.com/o/r", fails: true},
		{name: "URL with other resource", raw: "https://github.com/o/r/commit/1", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTicketRef(tc.raw, tc.defaultRepo)
			if tc.fails {
				if err == nil {
					t.Fatalf("accepted %q as %v", tc.raw, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestResolveTicket(t *testing.T) {
	old := settings
	t.Cleanup(func() { settings = old })
	for _, tc := range []struct {
		name   string
		config config.Config
		args   []string
		repo   string
		number int
		want   store.TicketID
		fails  bool
	}{
		{name: "positional", args: []string{"o/r#1"}, want: store.TicketID{Repo: "o/r", Number: 1}},
		{name: "legacy flags", repo: "o/r", number: 2, want: store.TicketID{Repo: "o/r", Number: 2}},
		{name: "number flag with default_repo", config: config.Config{DefaultRepo: "o/d", Repositories: []string{"o/a", "o/b"}}, number: 3, want: store.TicketID{Repo: "o/d", Number: 3}},
		{name: "single configured repository", config: config.Config{Repositories: []string{"o/a"}}, args: []string{"#4"}, want: store.TicketID{Repo: "o/a", Number: 4}},
		{name: "several repositories without default", config: config.Config{Repositories: []string{"o/a", "o/b"}}, args: []string{"4"}, fails: true},
		{name: "both forms", args: []string{"o/r#1"}, repo: "o/r", fails: true},
		{name: "missing", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings = tc.config
			got, err := resolveTicket(tc.args, tc.repo, tc.number)
			if tc.fails {
				if err == nil {
					t.Fatalf("accepted %v", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestCommandGroups(t *testing.T) {
	want := map[string][]string{
		groupQuery:    {"catalog", "comments", "context", "list", "related", "search", "show"},
		groupCollect:  {"acquire", "index", "snapshot", "sync"},
		groupServe:    {"mcp", "serve"},
		groupInspect:  {"bots", "config", "doctor", "model", "scope", "status"},
		groupAdvanced: {"evaluate"},
	}
	got := map[string][]string{}
	shorts := map[string]string{}
	for _, command := range root.Commands() {
		if !command.IsAvailableCommand() || command.Name() == "help" || command.Name() == "completion" {
			continue
		}
		got[command.GroupID] = append(got[command.GroupID], command.Name())
		if previous, ok := shorts[command.Short]; ok {
			t.Errorf("%s and %s share help %q", previous, command.Name(), command.Short)
		}
		shorts[command.Short] = command.Name()
	}
	for group, names := range want {
		slices.Sort(got[group])
		if !slices.Equal(got[group], names) {
			t.Errorf("group %s: got %v, want %v", group, got[group], names)
		}
	}
	if len(got[""]) != 0 {
		t.Errorf("ungrouped commands %v", got[""])
	}
}

func TestDeprecatedAliases(t *testing.T) {
	for _, tc := range []struct{ alias, replacement string }{{"get", "show"}, {"batch", "show"}, {"candidates", "related"}} {
		t.Run(tc.alias, func(t *testing.T) {
			command, _, err := root.Find([]string{tc.alias})
			if err != nil || command.Name() != tc.alias {
				t.Fatal("alias missing", err)
			}
			if command.IsAvailableCommand() || !strings.Contains(command.Deprecated, tc.replacement) {
				t.Fatalf("alias is listed or lacks a pointer to %s: %q", tc.replacement, command.Deprecated)
			}
		})
	}
	t.Run("candidates keeps its flags", func(t *testing.T) {
		command, _, err := root.Find([]string{"candidates"})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"repo", "number", "limit", "engine", "repositories", "include-labels", "include-comments", "comment-limit", "cursor", "count", "facets", "view"} {
			if command.Flags().Lookup(name) == nil {
				t.Errorf("missing --%s", name)
			}
		}
	})
	t.Run("label maps to labels-all", func(t *testing.T) {
		command, _, err := root.Find([]string{"list"})
		if err != nil {
			t.Fatal(err)
		}
		flag := command.Flags().Lookup("label")
		if flag == nil || !flag.Hidden {
			t.Fatal("--label is missing or listed in help")
		}
	})
	t.Run("get limit describes comment paging", func(t *testing.T) {
		command, _, err := root.Find([]string{"get"})
		if err != nil {
			t.Fatal(err)
		}
		if usage := command.Flags().Lookup("limit").Usage; !strings.Contains(usage, "Comment page") {
			t.Fatal(usage)
		}
	})
}

func TestEmbeddingFlagsOnlyOnEncoders(t *testing.T) {
	encoders := []string{"search", "related", "candidates", "index", "sync", "model", "serve", "mcp", "evaluate", "doctor"}
	for _, command := range root.Commands() {
		has := command.Flags().Lookup("embedding-backend") != nil
		if has != slices.Contains(encoders, command.Name()) {
			t.Errorf("%s: embedding flags present %t", command.Name(), has)
		}
		if command.InheritedFlags().Lookup("embedding-backend") != nil {
			t.Errorf("%s inherits embedding flags", command.Name())
		}
	}
}

// seedMirror creates a mirror with one ticket collected at the given time.
func seedMirror(t *testing.T, collected time.Time) string {
	t.Helper()
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
		if _, err := w.PutIssue(ctx, "o/r", json.RawMessage(`{"number":1,"node_id":"I_1","title":"Socket timeout","body":"Reconnect fails","state":"open","comments":2,"updated_at":"2026-01-01T00:00:00Z","created_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/1","labels":[{"name":"bug"}],"assignees":[{"login":"dev"}],"milestone":{"title":"v1"}}`)); err != nil {
			return err
		}
		for key, value := range map[string]string{"generation": "g1", "collected_at": collected.UTC().Format(time.RFC3339), "repositories": `["o/r"]`} {
			if err := w.SetMetadata(ctx, key, value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCommand(t *testing.T, name string, args []string, format string) (string, error) {
	t.Helper()
	command, _, err := root.Find([]string{name})
	if err != nil {
		t.Fatal(err)
	}
	oldFormat := outputFormat
	outputFormat = format
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetContext(context.Background())
	t.Cleanup(func() {
		outputFormat = oldFormat
		command.SetOut(nil)
	})
	err = command.RunE(command, args)
	return stdout.String(), err
}

func TestStatusReportsDatabasePath(t *testing.T) {
	old := settings
	t.Cleanup(func() { settings = old })
	path := seedMirror(t, time.Now())
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "pending sync"}[pending], func(t *testing.T) {
			settings = config.Config{Database: path}
			if pending {
				if err := os.WriteFile(path+".sync.sqlite", nil, 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Remove(path + ".sync.sqlite"); err != nil {
						t.Error(err)
					}
				})
			}
			out, err := runCommand(t, "status", nil, "json")
			if err != nil {
				t.Fatal(err)
			}
			var report struct {
				DBPath      string          `json:"db_path"`
				Generation  string          `json:"generation"`
				PendingSync json.RawMessage `json:"pending_sync"`
			}
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatal(err)
			}
			if report.DBPath != path || !filepath.IsAbs(report.DBPath) || report.Generation != "g1" || (report.PendingSync != nil) != pending {
				t.Fatal(out)
			}
			text, err := runCommand(t, "status", nil, "text")
			if err != nil || !strings.Contains(text, "Database: "+path) || strings.Contains(text, "Pending sync") != pending {
				t.Fatal(text, err)
			}
		})
	}
}

func TestShowText(t *testing.T) {
	old := settings
	t.Cleanup(func() { settings = old })
	settings = config.Config{Database: seedMirror(t, time.Now()), Repositories: []string{"o/r"}}
	out, err := runCommand(t, "show", []string{"#1", "o/r#9"}, "text")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"o/r#1  Socket timeout", "issue | open", "Labels: bug", "Assignees: dev", "Milestone: v1", "https://github.com/o/r/issues/1", "Comments: 2", "Reconnect fails", "Not in mirror: o/r#9"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "avatar") || strings.Contains(out, "node_id") {
		t.Fatal("text view printed raw fields", out)
	}
	raw, err := runCommand(t, "show", []string{"o/r#1"}, "json")
	if err != nil {
		t.Fatal(err)
	}
	var result store.BatchResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil || len(result.Issues) != 1 || result.Issues[0].Payload != nil {
		t.Fatal("JSON summary should omit raw records", raw, err)
	}
}

func TestDoctor(t *testing.T) {
	oldSettings, oldSetup := settings, setupErr
	t.Cleanup(func() {
		settings, setupErr = oldSettings, oldSetup
		if embeddingProvider != nil {
			if err := embeddingProvider.Close(); err != nil {
				t.Error(err)
			}
			embeddingProvider = nil
		}
	})
	fresh, stale := seedMirror(t, time.Now()), seedMirror(t, time.Now().Add(-72*time.Hour))
	for _, tc := range []struct {
		name     string
		database string
		setup    error
		failing  string
	}{
		{name: "fresh mirror", database: fresh},
		{name: "stale mirror", database: stale, failing: "collection age"},
		{name: "missing database", database: filepath.Join(t.TempDir(), "absent.sqlite"), failing: "database"},
		{name: "invalid configuration", setup: errors.New("load settings: bad"), failing: "configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, setupErr = config.Config{Database: tc.database, TokenEnv: "GH_MIRROR_TEST_TOKEN", Repositories: []string{"o/r"}}, tc.setup
			t.Setenv("GH_MIRROR_TEST_TOKEN", "")
			out, err := runCommand(t, "doctor", nil, "json")
			var report doctorReport
			if decodeErr := json.Unmarshal([]byte(out), &report); decodeErr != nil {
				t.Fatal(out, decodeErr)
			}
			failed := []string{}
			for _, check := range report.Checks {
				if check.Status == checkFail {
					failed = append(failed, check.Name)
				}
				if strings.Contains(check.Detail, "secret") {
					t.Fatal("printed a credential")
				}
			}
			if tc.failing == "" {
				if err != nil || !report.OK || len(failed) != 0 {
					t.Fatal(out, err)
				}
				return
			}
			if !errors.Is(err, errDoctorFailed) || report.OK || !slices.Contains(failed, tc.failing) {
				t.Fatal(out, err)
			}
		})
	}
	t.Run("text marks", func(t *testing.T) {
		settings, setupErr = config.Config{Database: fresh, TokenEnv: "GH_MIRROR_TEST_TOKEN"}, nil
		t.Setenv("GH_MIRROR_TEST_TOKEN", "secret")
		out, err := runCommand(t, "doctor", nil, "text")
		if err != nil || !strings.Contains(out, "✓ github token: GH_MIRROR_TEST_TOKEN is set") || strings.Contains(out, "secret") {
			t.Fatal(out, err)
		}
	})
}

func TestCatalogArguments(t *testing.T) {
	old := settings
	t.Cleanup(func() { settings = old })
	settings = config.Config{Database: seedMirror(t, time.Now()), Repositories: []string{"o/r"}}
	if _, err := runCommand(t, "catalog", []string{"widgets"}, "json"); err == nil || !strings.Contains(err.Error(), "labels, milestones") {
		t.Fatal("invalid kind error does not list kinds", err)
	}
	for kind, scope := range map[string]string{"labels": "o/r", "projects": "o", "issue_types": "o"} {
		if got := catalogScope(kind); got != scope {
			t.Errorf("%s scope %q, want %q", kind, got, scope)
		}
	}
	if _, err := runCommand(t, "catalog", []string{"labels"}, "json"); err != nil {
		t.Fatal(err)
	}
	command, _, err := root.Find([]string{"catalog"})
	if err != nil {
		t.Fatal(err)
	}
	kinds, _ := command.ValidArgsFunction(command, nil, "")
	if !slices.Equal(kinds, catalogKinds) {
		t.Fatal(kinds)
	}
}

func TestCompletionRegistered(t *testing.T) {
	for _, tc := range []struct{ command, flag string }{{"search", "state"}, {"search", "engine"}, {"search", "labels-any"}, {"list", "sort"}, {"related", "repositories"}, {"context", "purpose"}, {"comments", "kind"}, {"show", "view"}, {"mcp", "tools"}} {
		command, _, err := root.Find([]string{tc.command})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := command.GetFlagCompletionFunc(tc.flag); !ok {
			t.Errorf("%s --%s has no completion", tc.command, tc.flag)
		}
	}
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "absent.toml"), "completion", "bash"})
	err := Execute()
	root.SetOut(nil)
	root.SetArgs(nil)
	if err != nil || !strings.Contains(stdout.String(), "gh-mirror") {
		t.Fatalf("bash completion failed without configuration: %v", err)
	}
	if !completionCommand(&cobra.Command{Use: cobra.ShellCompRequestCmd}) {
		t.Fatal("completion requests run full setup")
	}
}

func TestStatusReportsSyncFailures(t *testing.T) {
	old := settings
	t.Cleanup(func() { settings = old })
	path := seedMirror(t, time.Now())
	settings = config.Config{Database: path}
	ctx := context.Background()
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	session, err := checkpoint.Open(ctx, path+".sync.sqlite", "sig", started, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RecordFailure(ctx, errors.New("node vanished"), started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	text, err := runCommand(t, "status", nil, "text")
	if err != nil || !strings.Contains(text, "Sync failures: 1 since 2026-01-02T03:04:05Z | last: node vanished") {
		t.Fatal(text, err)
	}
	doctor, _ := runCommand(t, "doctor", nil, "text")
	if !strings.Contains(doctor, "✗ pending sync") {
		t.Fatal(doctor)
	}
}
