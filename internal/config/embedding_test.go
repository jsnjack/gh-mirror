package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddingConfiguration(t *testing.T) {
	for _, c := range []struct {
		name, body string
		valid      bool
	}{{"Vulkan", "[embedding]\nbackend = 'lemonade-vulkan'\nbatch_size = 16\ndevice = 'Vulkan0'", true}, {"CPU", "[embedding]\nbackend = 'cpu'", true}, {"remote", "[embedding]\nbackend = 'remote'", false}, {"large batch", "[embedding]\nbatch_size = 64", false}} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(c.body), 0600); err != nil {
				t.Fatal(err)
			}
			settings, err := Load(path, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := settings.Validate(); (err == nil) != c.valid {
				t.Fatal("embedding validation", err)
			}
		})
	}
}
