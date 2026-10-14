//go:build linux

package embedding

import (
	"syscall"
	"testing"
)

func TestChildAttributes(t *testing.T) {
	t.Run("runtime receives SIGKILL when gh-mirror exits", func(t *testing.T) {
		if attrs := childAttributes(); attrs == nil || attrs.Pdeathsig != syscall.SIGKILL {
			t.Fatal("inference runtime can outlive gh-mirror", attrs)
		}
	})
}
