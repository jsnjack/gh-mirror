package cmd

import (
	"fmt"

	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

func addList() {
	var options store.ListOptions
	command := &cobra.Command{Use: "list", Short: "List local tickets with filters and pagination", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		out, err := db.List(command.Context(), options)
		if err != nil {
			return fmt.Errorf("list local mirror: %w", err)
		}
		return output(command, out)
	}}
	command.Flags().StringVar(&options.Repo, "repo", "", "Filter repository owner/name")
	command.Flags().StringVar(&options.State, "state", "", "Filter open or closed")
	command.Flags().StringVar(&options.Label, "label", "", "Filter exact label name")
	command.Flags().StringVar(&options.Type, "type", "", "Filter native issue type name")
	command.Flags().StringVar(&options.Kind, "kind", "", "Filter issue or pull_request")
	command.Flags().StringVar(&options.Project, "project", "", "Filter project membership owner/number, including archived memberships")
	command.Flags().IntVar(&options.Limit, "limit", 30, "Page size (1–100)")
	command.Flags().StringVar(&options.Cursor, "cursor", "", "Continue next_cursor with the same filters")
	root.AddCommand(command)
}
