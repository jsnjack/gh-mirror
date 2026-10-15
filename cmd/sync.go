package cmd

import (
	"fmt"
	"time"

	"gh-mirror/internal/collect"

	"github.com/spf13/cobra"
)

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

// syncStaleAfter resolves the --if-stale threshold; zero disables the check.
func syncStaleAfter(command *cobra.Command, ifStale bool, flag time.Duration) (time.Duration, error) {
	changed := command.Flags().Changed("stale-after")
	if changed && !ifStale {
		return 0, fmt.Errorf("--stale-after requires --if-stale")
	}
	if !ifStale {
		return 0, nil
	}
	if changed {
		if flag <= 0 {
			return 0, fmt.Errorf("--stale-after must be a positive duration")
		}
		return flag, nil
	}
	configured, err := time.ParseDuration(settings.StaleAfter)
	if err != nil {
		return 0, fmt.Errorf("parse stale_after: %w", err)
	}
	return configured, nil
}
