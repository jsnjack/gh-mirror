package collect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"gh-mirror/internal/checkpoint"
	"gh-mirror/internal/diagnostics"
	"gh-mirror/internal/store"
)

// Reasons a conditional sync leaves the mirror unchanged.
const (
	SkipFresh   = "fresh"
	SkipRunning = "running"
)

// ErrFresh stops a conditional sync that finds a fresh mirror after taking the collector lock.
var ErrFresh = errors.New("mirror is fresh")

// Freshness describes the committed mirror before a conditional sync.
type Freshness struct {
	// Skip is SkipFresh, SkipRunning, or empty when collection should run.
	Skip        string
	CollectedAt string
	Age         time.Duration
	Collected   bool
}

func checkpointPath(database string) string { return database + ".sync.sqlite" }

// CheckFreshness reads the mirror without writing or waiting. A missing, unreadable
// or uncollected mirror is stale. Another process collects when it holds the
// mirror's write lock while a checkpoint session exists; a session without that
// lock is interrupted work that the next sync resumes.
func CheckFreshness(ctx context.Context, database string, staleAfter time.Duration, now time.Time) (Freshness, error) {
	var out Freshness
	if _, err := os.Stat(database); errors.Is(err, os.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return out, fmt.Errorf("inspect mirror: %w", err)
	}
	db, err := store.Open(database, true)
	if err != nil {
		slog.DebugContext(ctx, "treat unreadable mirror as stale", "error", err)
		return out, nil
	}
	status, err := db.Status(ctx)
	if closeErr := db.Close(); closeErr != nil {
		slog.Log(ctx, diagnostics.TraceLevel, "close freshness check", "error", closeErr)
	}
	if err != nil {
		slog.DebugContext(ctx, "treat unreadable mirror as stale", "error", err)
		return out, nil
	}
	out.CollectedAt = status.CollectedAt
	out.Age, out.Collected, err = status.CollectionAge(now)
	if err != nil {
		slog.DebugContext(ctx, "treat unreadable collection time as stale", "error", err)
	}
	if out.Collected && out.Age < staleAfter {
		out.Skip = SkipFresh
		return out, nil
	}
	busy, err := store.WriterBusy(ctx, database)
	if err != nil {
		return out, err
	}
	if !busy {
		return out, nil
	}
	if _, running, err := checkpoint.Inspect(ctx, checkpointPath(database)); err != nil {
		return out, fmt.Errorf("inspect running sync: %w", err)
	} else if running {
		out.Skip = SkipRunning
	}
	return out, nil
}
