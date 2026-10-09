// Package cmd wires Cobra commands to collection, queries, snapshots, and servers.
package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"gh-mirror/internal/config"
	"gh-mirror/internal/diagnostics"

	"github.com/spf13/cobra"
)

// Version is set at build time via ldflags.
var Version = "dev"
var settings config.Config
var configPath, database string
var debug, trace bool
var logCloser io.Closer
var root = &cobra.Command{Use: "gh-mirror", Short: "Mirror GitHub tickets into a portable, searchable SQLite database", SilenceUsage: true, SilenceErrors: true}

func init() {
	root.Version = Version
	root.SetVersionTemplate("{{.Version}}\n")
	root.Flags().Bool("version", false, "Print the version and exit")
	root.PersistentFlags().Bool("help", false, "Show command help")
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "", "Configuration file (default: XDG gh-mirror/config.json)")
	root.PersistentFlags().StringVar(&database, "db", "", "Override the database path")
	root.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "Verbose diagnostics on stderr")
	root.PersistentFlags().BoolVar(&trace, "trace", false, "Detailed diagnostics in the temporary gh-mirror.log")
	root.PersistentPreRunE = func(command *cobra.Command, _ []string) error {
		logger, closer, err := diagnostics.Setup(debug, trace, command.ErrOrStderr())
		if err != nil {
			return fmt.Errorf("set up diagnostics: %w", err)
		}
		logCloser = closer
		slog.SetDefault(logger)
		path := configPath
		if path == "" {
			path, err = config.DefaultPath()
			if err != nil {
				return fmt.Errorf("resolve settings: %w", err)
			}
		}
		settings, err = config.Load(path, configPath != "")
		if err != nil {
			return fmt.Errorf("load settings: %w", err)
		}
		if database != "" {
			settings.Database = database
		}
		if err := settings.Validate(); err != nil {
			return fmt.Errorf("validate settings: %w", err)
		}
		return nil
	}
}

// Execute runs commands with interrupt cancellation and closes trace diagnostics.
func Execute() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	defer func() {
		if logCloser != nil {
			if err := logCloser.Close(); err != nil {
				slog.Warn("close trace log", "error", err)
			}
		}
	}()
	if err := root.ExecuteContext(ctx); err != nil {
		return fmt.Errorf("gh-mirror: %w", err)
	}
	return nil
}
