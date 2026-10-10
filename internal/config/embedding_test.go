package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddingConfiguration(t *testing.T) {
	for _, c := range []struct {
		name, json string
		valid      bool
	}{{"Vulkan", `{"embedding":{"backend":"lemonade-vulkan","batch_size":16,"device":"Vulkan0"}}`, true}, {"CPU", `{"embedding":{"backend":"cpu"}}`, true}, {"remote", `{"embedding":{"backend":"remote"}}`, false}, {"large batch", `{"embedding":{"batch_size":64}}`, false}} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(c.json), 0600); err != nil {
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
