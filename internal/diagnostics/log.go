// Package diagnostics configures stderr diagnostics and private trace logs.
package diagnostics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// TraceLevel records collection and transport metadata in the trace file.
const TraceLevel slog.Level = -8

type fanout struct{ handlers []slog.Handler }

func (h fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}
func (h fanout) Handle(ctx context.Context, record slog.Record) error {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, record.Level) {
			if err := handler.Handle(ctx, record); err != nil {
				return fmt.Errorf("write diagnostic: %w", err)
			}
		}
	}
	return nil
}
func (h fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := fanout{}
	for _, handler := range h.handlers {
		next.handlers = append(next.handlers, handler.WithAttrs(attrs))
	}
	return next
}
func (h fanout) WithGroup(name string) slog.Handler {
	next := fanout{}
	for _, handler := range h.handlers {
		next.handlers = append(next.handlers, handler.WithGroup(name))
	}
	return next
}

// Setup creates a logger and returns the trace file closer when enabled.
func Setup(debug, trace bool, stderr io.Writer) (*slog.Logger, io.Closer, error) {
	level := slog.LevelWarn
	if debug {
		level = slog.LevelDebug
	}
	h := fanout{handlers: []slog.Handler{slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})}}
	if !trace {
		return slog.New(h), nil, nil
	}
	f, err := openTrace(TracePath())
	if err != nil {
		return nil, nil, err
	}
	h.handlers = append(h.handlers, slog.NewTextHandler(f, &slog.HandlerOptions{Level: TraceLevel}))
	return slog.New(h), f, nil
}

// TracePath returns the per-user trace log location in the temporary directory.
func TracePath() string {
	name := "gh-mirror.log"
	if uid := os.Getuid(); uid >= 0 {
		name = fmt.Sprintf("gh-mirror-%d.log", uid)
	}
	return filepath.Join(os.TempDir(), name)
}

// openTrace refuses links planted in the shared temporary directory and truncates only
// a regular single-link file this user owns.
func openTrace(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|noFollow, 0600)
	if err != nil {
		return nil, fmt.Errorf("open trace log: %w", err)
	}
	if err := checkTrace(f); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w; close: %v", err, closeErr)
		}
		return nil, err
	}
	return f, nil
}

func checkTrace(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect trace log: %w", err)
	}
	if !info.Mode().IsRegular() || !singleLink(info) {
		return fmt.Errorf("trace log %s is not a private regular file", f.Name())
	}
	// Chmod fails unless this user owns the file.
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("set trace permissions: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate trace log: %w", err)
	}
	return nil
}
