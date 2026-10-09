package checkpoint

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckpointCrash(t *testing.T) {
	const helperEnv = "GH_MIRROR_CRASH_TEST_PATH"
	ctx := context.Background()
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if path := os.Getenv(helperEnv); path != "" {
		s, err := Open(ctx, path, "crash-session", started, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, "page", []byte("durable response")); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(os.Stdout, "saved"); err != nil {
			t.Fatal(err)
		}
		// The parent kills this process while SQLite is still open with WAL sidecars.
		time.Sleep(time.Hour)
		return
	}
	t.Run("kill before closing SQLite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pending.sqlite")
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(executable, "-test.run=^TestCheckpointCrash$")
		command.Env = append(os.Environ(), helperEnv+"="+path)
		output, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if command.ProcessState == nil {
				if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Error(err)
				}
				var exited *exec.ExitError
				if err := command.Wait(); err != nil && !errors.As(err, &exited) {
					t.Error(err)
				}
			}
		})
		ready := make(chan string, 1)
		go func() {
			line, err := bufio.NewReader(output).ReadString('\n')
			if err != nil {
				ready <- err.Error()
				return
			}
			ready <- line
		}()
		select {
		case line := <-ready:
			if line != "saved\n" {
				t.Fatal("child did not persist its response", line)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("child checkpoint timed out")
		}
		if err := command.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		var exited *exec.ExitError
		if err := command.Wait(); !errors.As(err, &exited) {
			t.Fatal("child was not killed", err)
		}
		s, err := Open(ctx, path, "crash-session", started.Add(time.Hour), false)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
		body, found, err := s.Load(ctx, "page")
		if err != nil || !found || string(body) != "durable response" || s.Saved != 1 || !s.Started.Equal(started) {
			t.Fatal("abrupt termination lost progress", string(body), found, s.Saved, s.Started, err)
		}
	})
}
