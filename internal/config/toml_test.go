package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTOMLConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		invalid    bool
	}{
		{"presets", "workers = 8\n[repos.\"o/issues\"]\npreset = 'all'\npull_request_review_comments = false\n[repos.\"o/code\"]\npreset = 'pull_requests'", false},
		{"overrides", "[repos.\"o/r\"]\npreset = 'issues'\nprojects = false\nissue_comments = false", false},
		{"legacy global defaults", "fields = false\nprojects = false\n[repos.\"o/r\"]", false},
		{"embedding and rules", "[repos.\"o/r\"]\npreset = 'all'\n[embedding]\nbackend = 'cpu'\n[[label_rules.\"o/r\"]]\nprefix = 'customer:'\nrole = 'client'\naliases = ['日本語', 'Acme']", false},
		{"unknown key", "typo = true", true},
		{"unknown switch", "[repos.\"o/r\"]\nisssues = true", true},
		{"unknown preset", "[repos.\"o/r\"]\npreset = 'fast'", true},
		{"invalid boolean", "[repos.\"o/r\"]\nissues = 'true'", true},
		{"mixed repositories", "repositories = ['o/r']\n[repos.\"o/r\"]", true},
		{"duplicate key", "workers = 8\nworkers = 4", true},
		{"PR policy", "[pr_comments]\nexclude_known_ai = true\nexclude_ai_threads = true\nexclude_authors = ['custom-reviewer']\n[repos.\"o/r\"]\npreset = 'pull_requests'\n[repos.\"o/r\".pr_comments]\nexclude_known_ai = false\nexclude_authors = []", false},
		{"unknown PR policy", "[pr_comments]\nexclude_robots = true", true},
		{"wildcard authors", "[pr_comments]\nexclude_authors = ['*bot*']", true},
		{"invalid repository", "[repos.\"../r\"]\npreset = 'all'", true},
		{"empty collection", "[repos.\"o/r\"]\nissues = false\npull_requests = false", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path, true)
			if err == nil {
				err = c.Validate()
			}
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid TOML accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "presets":
				if c.Workers != 8 || c.Scope("o/issues").PullRequestReviewComments || c.Scope("o/code").Issues || !c.Scope("o/code").PullRequestReviewComments || c.Scope("o/code").Labels {
					t.Fatal(c.Scopes(), c.Workers)
				}
			case "overrides":
				if c.Scope("o/r").IssueComments || c.Scope("o/r").Projects || c.Scope("o/r").PullRequests {
					t.Fatal(c.Scopes())
				}
			case "legacy global defaults":
				if c.Scope("o/r") != c.DefaultScope() || c.Scope("o/r").Fields || c.Scope("o/r").Projects {
					t.Fatal(c.Scopes())
				}
			case "embedding and rules":
				if c.Embedding.Backend != "cpu" || len(c.LabelRules["o/r"][0].Aliases) != 2 {
					t.Fatal(c)
				}
			case "PR policy":
				policy := c.CommentPolicies()["o/r"]
				if !c.PRComments.ExcludeKnownAI || policy.ExcludeKnownAI || !policy.ExcludeAIThreads || len(policy.ExcludeAuthors) != 0 {
					t.Fatal(c.CommentPolicies())
				}
			}
			for _, effective := range []bool{false, true} {
				raw, err := c.TOML(effective)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				loaded, err := Load(path, true)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(c.Scopes(), loaded.Scopes()) || !reflect.DeepEqual(c.LabelRules, loaded.LabelRules) || !reflect.DeepEqual(c.Embedding, loaded.Embedding) || c.Workers != loaded.Workers {
					t.Fatal("TOML changed settings", string(raw))
				}
				beforePolicies, err := object(c.CommentPolicies())
				if err != nil {
					t.Fatal(err)
				}
				afterPolicies, err := object(loaded.CommentPolicies())
				if err != nil || !reflect.DeepEqual(beforePolicies, afterPolicies) {
					t.Fatal("TOML changed comment policy", string(raw), err)
				}
			}
		})
	}
}

func TestConfigMigration(t *testing.T) {
	for _, name := range []string{"scope preserved", "existing destination", "default fallback", "TOML preferred"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			dir = filepath.Join(dir, "gh-mirror")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			source, target := filepath.Join(dir, "config.json"), filepath.Join(dir, "config.toml")
			body := `{"repositories":["o/r","o/code"],"repository_options":{"o/r":{"issues":true,"issue_comments":true},"o/code":{"pull_requests":true,"pull_request_comments":true,"pull_request_review_comments":true}}}`
			if err := os.WriteFile(source, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if name == "existing destination" {
				if err := os.WriteFile(target, []byte("workers = 8"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := Migrate(source, target); err == nil {
					t.Fatal("overwrote destination")
				}
				raw, err := os.ReadFile(target)
				if err != nil || string(raw) != "workers = 8" {
					t.Fatal(string(raw), err)
				}
				return
			}
			if name == "default fallback" {
				path, err := DefaultPath()
				if err != nil || path != source {
					t.Fatal(path, err)
				}
				return
			}
			if err := Migrate(source, target); err != nil {
				t.Fatal(err)
			}
			before, err := Load(source, true)
			if err != nil {
				t.Fatal(err)
			}
			after, err := Load(target, true)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Scopes(), after.Scopes()) {
				t.Fatal(before.Scopes(), after.Scopes())
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal(info, err)
			}
			path, err := DefaultPath()
			if err != nil || path != target {
				t.Fatal(path, err)
			}
			raw, err := os.ReadFile(source)
			if err != nil || !strings.Contains(string(raw), "repository_options") {
				t.Fatal("source changed", err)
			}
		})
	}
}
