package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gh-mirror/internal/config"
)

func TestConfigCommands(t *testing.T) {
	for _, name := range []string{"compact", "effective", "explicit JSON", "migrate"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(`{"repositories":["o/code"],"repository_options":{"o/code":{"pull_requests":true,"pull_request_comments":true,"pull_request_review_comments":true}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			oldSettings, oldPath, oldFormat := settings, configPath, outputFormat
			var err error
			settings, err = config.Load(path, true)
			if err != nil {
				t.Fatal(err)
			}
			configPath, outputFormat = path, "auto"
			t.Cleanup(func() { settings, configPath, outputFormat = oldSettings, oldPath, oldFormat })
			args := []string{"config", "show"}
			if name == "migrate" {
				args[1] = "migrate"
			}
			command, _, err := root.Find(args)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			command.SetOut(&out)
			t.Cleanup(func() { command.SetOut(nil) })
			if name != "migrate" {
				flag := command.Flags().Lookup("effective")
				old := flag.Value.String()
				if err := flag.Value.Set(map[bool]string{true: "true", false: "false"}[name == "effective"]); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := flag.Value.Set(old); err != nil {
						t.Fatal(err)
					}
				})
			}
			if name == "explicit JSON" {
				outputFormat = "json"
			}
			if err := command.RunE(command, nil); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "compact":
				if !strings.Contains(out.String(), "preset = 'pull_requests'") || strings.Contains(out.String(), "issues =") {
					t.Fatal(out.String())
				}
			case "effective":
				if !strings.Contains(out.String(), "issues = false") || !strings.Contains(out.String(), "workers = 4") {
					t.Fatal(out.String())
				}
			case "explicit JSON":
				if !strings.Contains(out.String(), `"repository_options"`) {
					t.Fatal(out.String())
				}
			case "migrate":
				if !strings.Contains(out.String(), "original retained") {
					t.Fatal(out.String())
				}
				c, err := config.Load(filepath.Join(filepath.Dir(path), "config.toml"), true)
				if err != nil || c.Scope("o/code") != settings.Scope("o/code") {
					t.Fatal(c, err)
				}
			}
		})
	}
}
