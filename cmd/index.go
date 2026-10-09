package cmd

import (
	"errors"
	"fmt"

	"gh-mirror/internal/embedding"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

func addIndex() {
	var workers int
	var quiet bool
	command := &cobra.Command{Use: "index", Short: "Build or resume bundled offline semantic vectors without GitHub requests", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) (runErr error) {
		if workers < 1 || workers > 16 {
			return fmt.Errorf("index workers must be between 1 and 16")
		}
		var display *progress.Display
		var report progress.Reporter
		if !quiet {
			var err error
			display, err = progress.NewOperation(command.ErrOrStderr(), !debug, "index")
			if err != nil {
				return err
			}
			report = display.Report
			defer func() {
				if display != nil {
					runErr = errors.Join(runErr, display.Finish(runErr))
				}
			}()
		}
		db, err := open(false)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		result, err := db.Index(command.Context(), embedding.Default, store.IndexOptions{Workers: workers, Progress: report})
		if err != nil {
			return fmt.Errorf("index local mirror: %w", err)
		}
		if display != nil {
			err := display.Finish(nil)
			display = nil
			if err != nil {
				return err
			}
		}
		return output(command, result)
	}}
	command.Flags().IntVar(&workers, "workers", 4, "Parallel CPU inference workers (1–16)")
	command.Flags().BoolVar(&quiet, "quiet", false, "Suppress indexing progress")
	root.AddCommand(command)
	root.AddCommand(&cobra.Command{Use: "model", Short: "Describe the bundled offline encoder and its compatibility identity", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		return output(command, struct {
			Model       string `json:"model"`
			Revision    string `json:"revision"`
			Fingerprint string `json:"fingerprint"`
			Dimension   int    `json:"dimension"`
			MaxTokens   int    `json:"max_tokens"`
			License     string `json:"license"`
		}{embedding.Model, embedding.Revision, embedding.Fingerprint, embedding.Dimension, embedding.MaxTokens, "Apache-2.0"})
	}})
}
