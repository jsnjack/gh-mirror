package cmd

import (
	"errors"
	"fmt"
	"time"

	"gh-mirror/internal/collect"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

// ensureFreshMode annotates commands that accept --ensure-fresh with how they apply it.
const ensureFreshMode = "gh-mirror/ensure-fresh"

const (
	// freshConfig commands also follow ensure_fresh from the configuration.
	freshConfig = "config"
	// freshFlag commands sync first only with an explicit --ensure-fresh.
	freshFlag = "flag"
	// freshSelf commands apply the threshold themselves instead of syncing first.
	freshSelf = "self"
)

// markEnsureFresh annotates mirror readers after every command is registered.
// Servers start without writing unless asked; sync skips a fresh mirror itself.
func markEnsureFresh() {
	modes := map[string]string{"status": freshConfig, "serve": freshFlag, "mcp": freshFlag, "sync": freshSelf}
	for _, command := range root.Commands() {
		mode := modes[command.Name()]
		if command.GroupID == groupQuery {
			mode = freshConfig
		}
		if mode == "" {
			continue
		}
		if command.Annotations == nil {
			command.Annotations = map[string]string{}
		}
		command.Annotations[ensureFreshMode] = mode
	}
}

// staleThreshold resolves --ensure-fresh, ensure_fresh and --stale-after; zero disables the check.
func staleThreshold(command *cobra.Command) (time.Duration, error) {
	mode := command.Annotations[ensureFreshMode]
	enabled := settings.EnsureFresh && mode == freshConfig
	if command.Flags().Changed("ensure-fresh") {
		enabled = ensureFresh
	}
	changed := command.Flags().Changed("stale-after")
	switch {
	case enabled && mode == "":
		return 0, fmt.Errorf("--ensure-fresh applies only to sync, queries, status, serve and mcp")
	case changed && !enabled:
		return 0, fmt.Errorf("--stale-after requires --ensure-fresh")
	case !enabled:
		return 0, nil
	case changed:
		if staleAfterFlag <= 0 {
			return 0, fmt.Errorf("--stale-after must be a positive duration")
		}
		return staleAfterFlag, nil
	}
	configured, err := time.ParseDuration(settings.StaleAfter)
	if err != nil {
		return 0, fmt.Errorf("parse stale_after: %w", err)
	}
	return configured, nil
}

// ensureFreshMirror syncs a stale mirror before a command reads it. Progress shows
// only on a terminal so stdout and agent-captured stderr stay clean. A failed sync
// keeps the previous collection, so the read continues with a warning; a mirror
// that was never collected fails.
func ensureFreshMirror(command *cobra.Command, staleAfter time.Duration) error {
	_, freshness, err := runSync(command, syncRun{index: true, quiet: !terminalWriter(command.ErrOrStderr()), cpuWorkers: settings.Workers, staleAfter: staleAfter})
	if err == nil {
		return nil
	}
	if !freshness.Collected || command.Context().Err() != nil {
		return fmt.Errorf("ensure fresh mirror: %w", err)
	}
	if _, writeErr := fmt.Fprintf(command.ErrOrStderr(), "gh-mirror: warning: %v; using the mirror collected %s ago\n", err, freshness.Age.Round(time.Second)); writeErr != nil {
		return fmt.Errorf("write sync warning: %w", writeErr)
	}
	return nil
}

// syncRun holds the settings shared by sync and --ensure-fresh.
type syncRun struct {
	full, restart, index, quiet bool
	cpuWorkers                  int
	// staleAfter, when positive, skips a fresh mirror or one another collector is updating.
	staleAfter time.Duration
}

// runSync collects and indexes, returning the command result and the
// freshness found before collecting. A conditional run returns syncSkip when it
// leaves the mirror unchanged.
func runSync(command *cobra.Command, run syncRun) (_ any, freshness collect.Freshness, runErr error) {
	ctx := command.Context()
	if run.staleAfter > 0 {
		var err error
		freshness, err = collect.CheckFreshness(ctx, settings.Database, run.staleAfter, time.Now())
		if err != nil {
			return nil, freshness, fmt.Errorf("check mirror freshness: %w", err)
		}
		if freshness.Skip != "" {
			return newSyncSkip(freshness, run.staleAfter), freshness, nil
		}
	}
	var report progress.Reporter
	var display *progress.Display
	// Finish cursor updates before stdout can write to the same terminal.
	finishDisplay := func() error {
		if display == nil {
			return nil
		}
		err := display.Finish(nil)
		display = nil
		return err
	}
	// A collector that took the lock after the freshness check makes this one skip.
	skip := func(reason string) (any, collect.Freshness, error) {
		if err := finishDisplay(); err != nil {
			return nil, freshness, err
		}
		freshness.Skip = reason
		return newSyncSkip(freshness, run.staleAfter), freshness, nil
	}
	if !run.quiet {
		var err error
		display, err = progress.New(command.ErrOrStderr())
		if err != nil {
			return nil, freshness, fmt.Errorf("start sync progress: %w", err)
		}
		report = display.Report
		defer func() {
			if display != nil {
				if err := display.Finish(runErr); err != nil {
					runErr = errors.Join(runErr, err)
				}
			}
		}()
	}
	conditional := run.staleAfter > 0
	db, err := open(false)
	if conditional && store.IsBusy(err) {
		return skip(collect.SkipRunning)
	}
	if err != nil {
		return nil, freshness, err
	}
	defer closeStore(ctx, db)
	result, err := collect.Sync(ctx, db, settings, collect.Options{Full: run.full, Restart: run.restart, StaleAfter: run.staleAfter, Progress: report})
	switch {
	case errors.Is(err, collect.ErrFresh):
		freshness.CollectedAt = result.Status.CollectedAt
		freshness.Age, freshness.Collected, err = result.Status.CollectionAge(time.Now())
		if err != nil {
			return nil, freshness, fmt.Errorf("read collection age: %w", err)
		}
		return skip(collect.SkipFresh)
	case conditional && store.IsBusy(err):
		return skip(collect.SkipRunning)
	case err != nil:
		return nil, freshness, fmt.Errorf("collect GitHub data: %w", err)
	}
	if run.index {
		setEmbeddingProgress(report)
		defer setEmbeddingProgress(nil)
		if _, err := db.Index(ctx, commandEncoder(), store.IndexOptions{Workers: run.cpuWorkers, Progress: report, LabelRules: settings.LabelRules, CommentPolicies: settings.CommentPolicies()}); err != nil {
			return nil, freshness, fmt.Errorf("index collected mirror: %w", err)
		}
		if result.Status, err = db.Status(ctx); err != nil {
			return nil, freshness, err
		}
	}
	if err := finishDisplay(); err != nil {
		return nil, freshness, err
	}
	return result, freshness, nil
}

// syncSkip reports a conditional sync that left the mirror unchanged.
type syncSkip struct {
	Skipped     bool   `json:"skipped"`
	Reason      string `json:"reason"`
	CollectedAt string `json:"collected_at,omitempty"`
	Age         string `json:"age,omitempty"`
	StaleAfter  string `json:"stale_after"`
}

func newSyncSkip(f collect.Freshness, staleAfter time.Duration) syncSkip {
	out := syncSkip{Skipped: true, Reason: f.Skip, CollectedAt: f.CollectedAt, StaleAfter: staleAfter.String()}
	if f.Collected {
		out.Age = f.Age.Round(time.Second).String()
	}
	return out
}

func (s syncSkip) text() string {
	reason := "the mirror is fresh"
	if s.Reason == collect.SkipRunning {
		reason = "another sync is running"
	}
	collected := "never collected"
	if s.Age != "" {
		collected = fmt.Sprintf("collected %s ago at %s", s.Age, cleanLine(s.CollectedAt))
	}
	return fmt.Sprintf("Sync skipped: %s | %s | stale after %s", reason, collected, s.StaleAfter)
}
