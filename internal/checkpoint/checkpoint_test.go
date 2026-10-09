package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckpoint(t *testing.T) {
	for _, name := range []string{"resume", "restart", "different session", "cancelled save", "cleanup ownership"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "nested", "pending.sqlite")
			started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			s, err := Open(ctx, path, "first", started, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, found, err := s.Load(ctx, "page"); err != nil || found {
				t.Fatal("new session reused data", found, err)
			}
			saveCtx, cancel := context.WithCancel(ctx)
			if name == "cancelled save" {
				cancel()
			}
			if err := s.Save(saveCtx, "page", []byte("completed response")); err != nil {
				t.Fatal(err)
			}
			cancel()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("checkpoint permissions", info, err)
			}
			signature := "first"
			if name == "different session" {
				signature = "second"
			}
			s, err = Open(ctx, path, signature, started.Add(time.Hour), name == "restart")
			if err != nil {
				t.Fatal(err)
			}
			body, found, loadErr := s.Load(ctx, "page")
			resumed := name != "restart" && name != "different session"
			if loadErr != nil || found != resumed || s.Resuming != resumed {
				t.Fatal("wrong recovery", found, s.Resuming, loadErr)
			}
			if resumed && (string(body) != "completed response" || s.Saved != 1 || !s.Started.Equal(started)) {
				t.Fatal("lost response or original start", string(body), s)
			}
			if !resumed && !s.Started.Equal(started.Add(time.Hour)) {
				t.Fatal("fresh session retained old time", s.Started)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if name == "cleanup ownership" {
				if err := Discard(ctx, path, "different"); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("deleted another collector's work", err)
				}
			}
			for range 2 {
				if err := Discard(ctx, path, signature); err != nil {
					t.Fatal(err)
				}
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("checkpoint cleanup", suffix, err)
				}
			}
		})
	}
}
