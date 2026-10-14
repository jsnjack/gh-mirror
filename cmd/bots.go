package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func addBots() {
	var repo string
	command := &cobra.Command{Use: "bots", GroupID: groupInspect, Short: "Show PR automation accounts and policy exclusions", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(c.Context(), db)
		actors, err := db.CommentActors(c.Context(), repo)
		if err != nil {
			return fmt.Errorf("inspect comment actors: %w", err)
		}
		return output(c, actors)
	}}
	command.Flags().StringVar(&repo, "repo", "", "Filter repository owner/name")
	root.AddCommand(command)
}
