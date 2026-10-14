package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFailureRecord(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t.Run("missing checkpoint is not created", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pending.sqlite")
		if _, found, err := Inspect(ctx, path); err != nil || found {
			t.Fatal("reported absent session", found, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("inspection created a checkpoint", err)
		}
	})
	t.Run("failures accumulate until the session resets", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pending.sqlite")
		s, err := Open(ctx, path, "first", started, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, "page", []byte("body")); err != nil {
			t.Fatal(err)
		}
		for n, message := range []string{"first failure", strings.Repeat("x", 3*maxErrorBytes)} {
			if err := s.RecordFailure(ctx, errors.New(message), started.Add(time.Duration(n+1)*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		pending, found, err := Inspect(ctx, path)
		if err != nil || !found {
			t.Fatal("pending session hidden", found, err)
		}
		if pending.Failures != 2 || !pending.Started.Equal(started) || pending.Saved != 1 || !pending.FailedAt.Equal(started.Add(2*time.Hour)) || len(pending.LastError) > maxErrorBytes+len("…") {
			t.Fatal("incorrect failure record", pending)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(ctx, path, "second", started.Add(3*time.Hour), false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		pending, found, err = Inspect(ctx, path)
		if err != nil || !found || pending.Failures != 0 || pending.LastError != "" || pending.Saved != 0 {
			t.Fatal("new session kept stale failures", pending, found, err)
		}
	})
}
