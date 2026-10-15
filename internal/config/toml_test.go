package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"github.com/pelletier/go-toml/v2"
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
		{"legacy repository options", "[repository_options.\"o/r\"]\nissues = true", true},
		{"legacy PR overrides", "[pr_comment_overrides.\"o/r\"]\nexclude_known_ai = false", true},
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

func TestExampleConfiguration(t *testing.T) {
	t.Run("full example loads and lists every setting", func(t *testing.T) {
		path := filepath.Join("..", "..", "examples", "config.toml")
		c, err := Load(path, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		effective, err := c.TOML(true)
		if err != nil {
			t.Fatal(err)
		}
		var all map[string]any
		if err := toml.Unmarshal(effective, &all); err != nil {
			t.Fatal(err)
		}
		keys := []string{"preset", "runtime", "device", "batch_size", "role", "label", "prefix", "value", "aliases", "description"}
		for _, table := range []map[string]any{all, all["pr_comments"].(map[string]any), all["repos"].(map[string]any)["owner/repository"].(map[string]any)} {
			for key, value := range table {
				if _, nested := value.(map[string]any); !nested {
					keys = append(keys, key)
				}
			}
		}
		// Commented keys count: their defaults depend on the host.
		for _, key := range keys {
			if !regexp.MustCompile(`(?m)^#? ?` + key + ` = `).Match(raw) {
				t.Errorf("example omits %q", key)
			}
		}
	})
}

func TestUnconfiguredCommentPolicy(t *testing.T) {
	t.Run("standalone indexing preserves saved policies", func(t *testing.T) {
		c, err := Load(filepath.Join(t.TempDir(), "config.toml"), false)
		if err != nil {
			t.Fatal(err)
		}
		if c.CommentPolicies() != nil {
			t.Fatal("unconfigured indexing would replace saved policies")
		}
	})
}
