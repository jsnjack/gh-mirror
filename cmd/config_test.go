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
	for _, name := range []string{"compact", "effective", "explicit JSON"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("[repos.\"o/code\"]\npreset = 'pull_requests'"), 0600); err != nil {
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
			command, _, err := root.Find([]string{"config", "show"})
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			command.SetOut(&out)
			t.Cleanup(func() { command.SetOut(nil) })
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
			}
		})
	}
}
