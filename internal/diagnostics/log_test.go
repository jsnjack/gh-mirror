package diagnostics

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogging(t *testing.T) {
	for _, tc := range []struct {
		name         string
		debug, trace bool
	}{{"default", false, false}, {"debug", true, false}, {"trace", false, true}, {"both", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			var stderr bytes.Buffer
			logger, closer, err := Setup(tc.debug, tc.trace, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			logger = logger.With("component", "fixture").WithGroup("request")
			logger.Debug("debug message")
			logger.Warn("warn message")
			logger.Log(context.Background(), TraceLevel, "trace message")
			if closer != nil {
				if err := closer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if !strings.Contains(stderr.String(), "warn message") || strings.Contains(stderr.String(), "debug message") != tc.debug || strings.Contains(stderr.String(), "trace message") {
				t.Fatal(stderr.String())
			}
			path := filepath.Join(dir, "gh-mirror.log")
			if tc.trace {
				body, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(body), "trace message") || !strings.Contains(string(body), "debug message") {
					t.Fatal(string(body), err)
				}
				logger, closer, err = Setup(false, true, &stderr)
				if err != nil {
					t.Fatal(err)
				}
				logger.Warn("new run")
				if err := closer.Close(); err != nil {
					t.Fatal(err)
				}
				body, err = os.ReadFile(path)
				if err != nil || strings.Contains(string(body), "trace message") {
					t.Fatal("trace not truncated", err)
				}
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("trace permissions", err)
				}
			} else if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("trace created without flag", err)
			}
		})
	}
}
