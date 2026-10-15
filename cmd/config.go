package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func addConfig() {
	command := &cobra.Command{Use: "config", GroupID: groupInspect, Short: "Show the configuration file"}
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
	root.AddCommand(command)
}
