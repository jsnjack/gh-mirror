package cmd

import (
	"fmt"
	"path/filepath"

	"gh-mirror/internal/config"

	"github.com/spf13/cobra"
)

func addConfig() {
	command := &cobra.Command{Use: "config", GroupID: groupInspect, Short: "Show or migrate the configuration file"}
	var effective bool
	show := &cobra.Command{Use: "show", Short: "Print TOML settings, with optional resolved defaults", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if outputFormat == "json" {
			return output(c, settings)
		}
		raw, err := settings.TOML(effective)
		if err != nil {
			return err
		}
		if _, err := c.OutOrStdout().Write(raw); err != nil {
			return fmt.Errorf("write configuration: %w", err)
		}
		return nil
	}}
	show.Flags().BoolVar(&effective, "effective", false, "Show defaults and every resolved repository switch")
	command.AddCommand(show)
	var destination string
	migrate := &cobra.Command{Use: "migrate", Short: "Convert current settings to TOML while preserving collection scope", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		source := configPath
		if source == "" {
			var err error
			source, err = config.DefaultPath()
			if err != nil {
				return err
			}
		}
		target := destination
		if target == "" {
			target = filepath.Join(filepath.Dir(source), "config.toml")
		}
		if err := config.Migrate(source, target); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.OutOrStdout(), "Configuration written to %s; original retained at %s.\n", target, source); err != nil {
			return fmt.Errorf("write migration result: %w", err)
		}
		return nil
	}}
	migrate.Flags().StringVar(&destination, "to", "", "Destination TOML file (default: config.toml beside source; must not exist)")
	command.AddCommand(migrate)
	root.AddCommand(command)
}
