package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguration(t *testing.T) {
	for _, name := range []string{"XDG defaults", "explicit missing", "valid", "unknown key", "malformed", "JSON file", "legacy repositories", "invalid repository", "duplicate repository", "unsafe API", "invalid duration", "invalid workers zero", "invalid workers high", "workers override", "stale_after override", "invalid stale_after zero", "invalid stale_after text"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
			path, err := DefaultPath()
			if err != nil {
				t.Fatal(err)
			}
			if path != filepath.Join(dir, "config", "gh-mirror", "config.toml") {
				t.Fatal(path)
			}
			file := filepath.Join(dir, "settings.toml")
			body := `[repos."o/r"]`
			switch name {
			case "unknown key":
				body = `typo = true`
			case "malformed":
				body = `workers =`
			case "JSON file":
				file = filepath.Join(dir, "settings.json")
				body = `{"repositories":["o/r"]}`
			case "legacy repositories":
				body = `repositories = ["o/r"]`
			case "stale_after override":
				body = "stale_after = \"15m\"\n" + body
			case "invalid stale_after zero":
				body = "stale_after = \"0s\"\n" + body
			case "invalid stale_after text":
				body = "stale_after = \"soon\"\n" + body
			}
			if name != "XDG defaults" && name != "explicit missing" {
				if err := os.WriteFile(file, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			c, err := Load(file, name != "XDG defaults")
			switch name {
			case "explicit missing", "unknown key", "malformed", "JSON file", "legacy repositories":
				if err == nil {
					t.Fatal("invalid configuration accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "invalid repository":
				c.Repositories = []string{"../.."}
			case "duplicate repository":
				c.Repositories = []string{"o/r", "O/R"}
			case "unsafe API":
				c.APIURL = "http://example.com"
			case "invalid workers zero":
				c.Workers = 0
			case "invalid workers high":
				c.Workers = MaxWorkers + 1
			case "workers override":
				c.Workers = 8
			case "invalid duration":
				c.EnrichmentInterval = "0s"
			}
			err = c.Validate()
			invalid := strings.HasPrefix(name, "invalid") || name == "duplicate repository" || name == "unsafe API"
			if (err != nil) != invalid {
				t.Fatal("validation", err)
			}
			if want := map[string]string{"XDG defaults": "1h", "valid": "1h", "stale_after override": "15m"}[name]; want != "" && c.StaleAfter != want {
				t.Fatal("stale_after", c.StaleAfter, "want", want)
			}
			if name == "XDG defaults" && c.Workers != DefaultWorkers {
				t.Fatal("missing worker default", c.Workers)
			}
			if name == "XDG defaults" && !strings.HasPrefix(c.Database, filepath.Join(dir, "data")) {
				t.Fatal(c.Database)
			}
		})
	}
}
func TestValidRepository(t *testing.T) {
	for _, tc := range []struct {
		repo  string
		valid bool
	}{{"owner/repo", true}, {"owner/repo-name.ext", true}, {"owner", false}, {"../repo", false}, {"owner/repo/name", false}, {"owner/repo?token=x", false}} {
		t.Run(tc.repo, func(t *testing.T) {
			if got := ValidRepository(tc.repo); got != tc.valid {
				t.Fatal(got)
			}
		})
	}
}

func TestDefaultRepository(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  Config
		want    string
		invalid bool
	}{
		{name: "explicit", config: Config{DefaultRepo: "o/b", Repositories: []string{"o/a"}}, want: "o/b"},
		{name: "single repository", config: Config{Repositories: []string{"o/a"}}, want: "o/a"},
		{name: "several repositories", config: Config{Repositories: []string{"o/a", "o/b"}}},
		{name: "invalid", config: Config{DefaultRepo: "../x"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.config.DefaultRepository(); got != tc.want && !tc.invalid {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			base, err := defaults()
			if err != nil {
				t.Fatal(err)
			}
			base.DefaultRepo, base.Repositories = tc.config.DefaultRepo, tc.config.Repositories
			if err := base.Validate(); (err != nil) != tc.invalid {
				t.Fatalf("validation error %v, invalid %t", err, tc.invalid)
			}
		})
	}
}
