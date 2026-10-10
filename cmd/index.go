package cmd

import (
	"errors"
	"fmt"

	"gh-mirror/internal/config"
	"gh-mirror/internal/embedding"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

func addIndex() {
	var workers int
	var quiet bool
	command := &cobra.Command{Use: "index", Short: "Build or resume bundled offline semantic vectors without GitHub requests", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) (runErr error) {
		cpuWorkers, err := inferenceWorkers(command, "workers", workers)
		if err != nil {
			return err
		}
		if err := configureIndexEncoder(command.Context(), cpuWorkers); err != nil {
			return err
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
		setEmbeddingProgress(report)
		defer setEmbeddingProgress(nil)
		result, err := db.Index(command.Context(), commandEncoder(), store.IndexOptions{Workers: cpuWorkers, Progress: report})
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
	command.Flags().IntVar(&workers, "workers", 0, "Parallel CPU inference workers (1–16; defaults to configuration)")
	command.Flags().BoolVar(&quiet, "quiet", false, "Suppress indexing progress")
	root.AddCommand(command)
	var check bool
	modelCommand := &cobra.Command{Use: "model", Short: "Describe the bundled offline encoder and its compatibility identity", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		var runtime *embedding.State
		if check {
			if embeddingProvider == nil {
				var err error
				embeddingProvider, err = embedding.New(command.Context(), settings.Embedding)
				if err != nil {
					return err
				}
			}
			if _, err := embeddingProvider.Embed(command.Context(), "hello world"); err != nil {
				return err
			}
			s := embeddingProvider.Status()
			runtime = &s
		}
		return output(command, struct {
			Model       string           `json:"model"`
			Revision    string           `json:"revision"`
			Fingerprint string           `json:"fingerprint"`
			Dimension   int              `json:"dimension"`
			MaxTokens   int              `json:"max_tokens"`
			License     string           `json:"license"`
			Runtime     *embedding.State `json:"runtime,omitempty"`
		}{embedding.Model, embedding.Revision, embedding.Fingerprint, embedding.Dimension, embedding.MaxTokens, "Apache-2.0", runtime})
	}}
	modelCommand.Flags().BoolVar(&check, "check", false, "Verify the configured backend, model compatibility, and CPU fallback offline")
	root.AddCommand(modelCommand)
}

func inferenceWorkers(command *cobra.Command, flag string, value int) (int, error) {
	if !command.Flags().Changed(flag) {
		value = settings.Workers
		if value == 0 {
			value = config.DefaultWorkers
		}
	}
	if value < 1 || value > config.MaxWorkers {
		return 0, fmt.Errorf("index workers must be between 1 and %d", config.MaxWorkers)
	}
	return value, nil
}
