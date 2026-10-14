//go:build unix

package diagnostics

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTraceLinks(t *testing.T) {
	for _, name := range []string{"symlink", "hardlink"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			if !strings.HasSuffix(TracePath(), "gh-mirror-"+strconv.Itoa(os.Getuid())+".log") {
				t.Fatal("trace path is not per user", TracePath())
			}
			victim := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(victim, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			link := os.Symlink
			if name == "hardlink" {
				link = os.Link
			}
			if err := link(victim, TracePath()); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Setup(false, true, &bytes.Buffer{}); err == nil {
				t.Fatal("trace log followed a planted link")
			}
			if body, err := os.ReadFile(victim); err != nil || string(body) != "keep" {
				t.Fatal("link target was modified", string(body), err)
			}
		})
	}
}
