package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

func addReads() {
	addContext()
	var comments store.CommentOptions
	command := &cobra.Command{Use: "comments", Short: "Read a bounded page of ticket comments", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(c.Context(), db)
		out, err := db.Comments(c.Context(), comments)
		if err != nil {
			return fmt.Errorf("read comments: %w", err)
		}
		return output(c, out)
	}}
	command.Flags().StringVar(&comments.Repo, "repo", "", "Repository owner/name")
	command.Flags().BoolVar(&comments.IncludeExcluded, "include-excluded", false, "Include comments excluded by the saved PR retrieval policy")
	command.Flags().IntVar(&comments.Number, "number", 0, "Ticket number")
	command.Flags().StringVar(&comments.Kind, "kind", "all", "Select all, discussion or review comments")
	command.Flags().StringVar(&comments.View, "view", "summary", "Return summary previews or full raw records")
	command.Flags().StringVar(&comments.Order, "order", "asc", "Order by creation time asc or desc")
	command.Flags().IntVar(&comments.Limit, "limit", 30, "Page size (1–100)")
	command.Flags().StringVar(&comments.Cursor, "cursor", "", "Continue next_cursor")
	root.AddCommand(command)
	var view string
	batch := &cobra.Command{Use: "batch owner/repository#number ...", Short: "Read selected tickets without comments", Args: cobra.RangeArgs(1, 100), RunE: func(c *cobra.Command, args []string) error {
		options := store.BatchOptions{View: view}
		for _, raw := range args {
			repo, n, ok := strings.Cut(raw, "#")
			if !ok {
				return fmt.Errorf("ticket must be owner/repository#number")
			}
			number, err := strconv.Atoi(n)
			if err != nil {
				return fmt.Errorf("parse ticket number: %w", err)
			}
			options.Tickets = append(options.Tickets, store.TicketID{Repo: repo, Number: number})
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(c.Context(), db)
		out, err := db.Batch(c.Context(), options)
		if err != nil {
			return fmt.Errorf("read ticket batch: %w", err)
		}
		return output(c, out)
	}}
	batch.Flags().StringVar(&view, "view", "summary", "Return compact summaries or full raw ticket records")
	root.AddCommand(batch)
}

func addContext() {
	var options store.ContextOptions
	command := &cobra.Command{Use: "context", Short: "Read bounded ticket evidence for classification, duplicates or proofreading", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(c.Context(), db)
		out, err := db.Context(c.Context(), options)
		if err != nil {
			return fmt.Errorf("read ticket context: %w", err)
		}
		return output(c, out)
	}}
	command.Flags().StringVar(&options.Repo, "repo", "", "Repository owner/name")
	command.Flags().BoolVar(&options.IncludeExcluded, "include-excluded", false, "Include comments excluded by the saved PR retrieval policy")
	command.Flags().IntVar(&options.Number, "number", 0, "Ticket number")
	command.Flags().StringVar(&options.Purpose, "purpose", "evaluate", "Select evaluate, classification, duplicates or proofread")
	command.Flags().StringVar(&options.Query, "query", "", "Select matching discussion alongside recent comments")
	command.Flags().IntVar(&options.CommentLimit, "comment-limit", 10, "Maximum selected comments (1–50)")
	command.Flags().IntVar(&options.RelatedLimit, "related-limit", 5, "Maximum expanded related tickets (1–20)")
	command.Flags().IntVar(&options.MaxCharacters, "max-characters", 12000, "Shared source-text budget (1000–100000 characters)")
	root.AddCommand(command)
}
