package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguration(t *testing.T) {
	for _, name := range []string{"XDG defaults", "explicit missing", "valid", "unknown key", "null", "two objects", "invalid repository", "duplicate repository", "unsafe API", "invalid duration", "invalid workers zero", "invalid workers high", "workers override"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
			path, err := DefaultPath()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(path, filepath.Join(dir, "config")) {
				t.Fatal(path)
			}
			file := filepath.Join(dir, "settings.json")
			body := `{"repositories":["o/r"]}`
			switch name {
			case "unknown key":
				body = `{"typo":true}`
			case "null":
				body = `null`
			case "two objects":
				body = `{} {}`
			}
			if name != "XDG defaults" && name != "explicit missing" {
				if err := os.WriteFile(file, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			c, err := Load(file, name != "XDG defaults")
			switch name {
			case "explicit missing", "unknown key", "null", "two objects":
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
