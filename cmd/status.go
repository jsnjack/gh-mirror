package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gh-mirror/internal/checkpoint"
	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

// statusReport adds local file details to the mirror status.
type statusReport struct {
	store.Status
	DBPath      string       `json:"db_path"`
	PendingSync *pendingSync `json:"pending_sync,omitempty"`
}

// pendingSync describes an interrupted or running sync that left resumable work.
type pendingSync struct {
	Path      string `json:"path"`
	UpdatedAt string `json:"updated_at"`
	// Session details are absent when the resume file cannot be read yet.
	*checkpoint.Pending
}

// databasePath resolves the configured database to an absolute path.
func databasePath() (string, error) {
	path, err := filepath.Abs(settings.Database)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	return path, nil
}

// findPendingSync reports the resume file a sync removes only after it commits,
// with its saved work and repeated failures when the file is readable.
func findPendingSync(ctx context.Context, dbPath string) (*pendingSync, error) {
	path := dbPath + ".sync.sqlite"
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect pending sync: %w", err)
	}
	out := &pendingSync{Path: path, UpdatedAt: info.ModTime().UTC().Format(time.RFC3339)}
	if pending, ok, err := checkpoint.Inspect(ctx, path); err == nil && ok {
		out.Pending = &pending
	}
	return out, nil
}

func addStatus() {
	var check bool
	var maxAge time.Duration
	command := &cobra.Command{Use: "status", GroupID: groupInspect, Short: "Show database path, collection times, counts and coverage", Long: "Show database path, collection times, counts and coverage.\nWith --check, run health checks instead and exit 1 when one fails.", Args: cobra.NoArgs, Annotations: map[string]string{configOptional: "true"}, RunE: func(command *cobra.Command, _ []string) error {
		if check {
			report := runDoctor(command.Context(), maxAge)
			if err := output(command, report); err != nil {
				return err
			}
			if !report.OK {
				return errDoctorFailed
			}
			return nil
		}
		if setupErr != nil {
			return setupErr
		}
		path, err := databasePath()
		if err != nil {
			return err
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		out := statusReport{DBPath: path}
		out.Status, err = db.Status(command.Context())
		if err != nil {
			return fmt.Errorf("read local status: %w", err)
		}
		out.PendingSync, err = findPendingSync(command.Context(), path)
		if err != nil {
			return err
		}
		return output(command, out)
	}}
	command.Flags().BoolVar(&check, "check", false, "Check configuration, token, freshness, index and encoder; exit 1 on a failed check")
	command.Flags().DurationVar(&maxAge, "max-age", 24*time.Hour, "With --check, fail when the last collection is older than this")
	root.AddCommand(command)
}
