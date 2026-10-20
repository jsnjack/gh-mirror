package cmd

import (
	"fmt"

	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

func addReads() {
	addComments()
	addContext()
}

func addComments() {
	var comments store.CommentOptions
	command := &cobra.Command{Use: "comments REF", GroupID: groupQuery, Short: "Page through a ticket's discussion and review comments", Long: "Page through a ticket's discussion and review comments.\n\n" + refHelp, Args: cobra.MaximumNArgs(1), ValidArgsFunction: completeTicketRefs, RunE: func(c *cobra.Command, args []string) error {
		id, err := resolveTicket(args)
		if err != nil {
			return err
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(c.Context(), db)
		options := comments
		options.Repo, options.Number = id.Repo, id.Number
		out, err := db.Comments(c.Context(), options)
		if err != nil {
			return fmt.Errorf("read comments: %w", err)
		}
		return output(c, out)
	}}
	command.Flags().BoolVar(&comments.IncludeExcluded, "include-excluded", false, "Include comments excluded by the saved PR retrieval policy")
	command.Flags().StringVar(&comments.Kind, "kind", "all", "Select all, discussion or review comments")
	command.Flags().StringVar(&comments.View, "view", "summary", "Return summary previews or full raw records")
	command.Flags().StringVar(&comments.Order, "order", "asc", "Order by creation time asc or desc")
	command.Flags().IntVar(&comments.Limit, "limit", 30, "Page size (1–100)")
	command.Flags().StringVar(&comments.Cursor, "cursor", "", "Continue next_cursor")
	registerCompletion(command, "kind", "all", "discussion", "review")
	root.AddCommand(command)
}

func addContext() {
	var options store.ContextOptions
	command := &cobra.Command{Use: "context REF", GroupID: groupQuery, Short: "Read bounded ticket evidence for an agent or reviewer", Long: "Read bounded ticket text, labels, selected comments and one-hop relationships for evaluation, classification, duplicate checks or proofreading.\n\n" + refHelp, Args: cobra.MaximumNArgs(1), ValidArgsFunction: completeTicketRefs, RunE: func(c *cobra.Command, args []string) error {
		id, err := resolveTicket(args)
		if err != nil {
			return err
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(c.Context(), db)
		o := options
		o.Repo, o.Number = id.Repo, id.Number
		out, err := db.Context(c.Context(), o)
		if err != nil {
			return fmt.Errorf("read ticket context: %w", err)
		}
		return output(c, out)
	}}
	command.Flags().BoolVar(&options.IncludeExcluded, "include-excluded", false, "Include comments excluded by the saved PR retrieval policy")
	command.Flags().StringVar(&options.Purpose, "purpose", "evaluate", "Select evaluate, classification, duplicates or proofread")
	command.Flags().StringVar(&options.Query, "query", "", "Select matching discussion alongside recent comments")
	command.Flags().IntVar(&options.CommentLimit, "comment-limit", 10, "Maximum selected comments (1–50)")
	command.Flags().IntVar(&options.RelatedLimit, "related-limit", 5, "Maximum expanded related tickets (1–20)")
	command.Flags().IntVar(&options.MaxCharacters, "max-characters", 12000, "Shared source-text budget (1000–100000 characters)")
	root.AddCommand(command)
}

func addRelated() {
	root.AddCommand(relatedCommand())
}

func relatedCommand() *cobra.Command {
	var limit int
	var includeLabels bool
	var candidates store.CandidateOptions
	command := &cobra.Command{Use: "related REF", GroupID: groupQuery, Short: "Find related tickets and duplicate candidates for a ticket", Long: "Find related tickets and duplicate candidates using the seed ticket's title, body, labels and optionally recent comments. Scores rank results; they do not prove duplicates.\n\n" + refHelp, Args: cobra.MaximumNArgs(1), ValidArgsFunction: completeTicketRefs, RunE: func(c *cobra.Command, args []string) error {
		id, err := resolveTicket(args)
		if err != nil {
			return err
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(c.Context(), db)
		options := candidates
		options.Repo, options.Number, options.Limit = id.Repo, id.Number, limit
		options.IncludeLabels = &includeLabels
		if !c.Flags().Changed("engine") {
			options.Engine = ""
		}
		out, err := db.FindCandidates(c.Context(), options)
		if err != nil {
			return fmt.Errorf("find related tickets: %w", err)
		}
		return output(c, out)
	}}
	command.Flags().IntVar(&limit, "limit", 30, "Maximum results (1–100)")
	command.Flags().StringVar(&candidates.Engine, "engine", store.DefaultSearchEngine, "Use lexical, semantic or hybrid retrieval (default falls back to lexical with a warning)")
	command.Flags().BoolVar(&includeLabels, "include-labels", true, "Use attached labels in seed terms")
	command.Flags().BoolVar(&candidates.IncludeComments, "include-comments", false, "Use recent seed comments")
	command.Flags().IntVar(&candidates.CommentLimit, "comment-limit", 20, "Recent seed comment limit (1–100)")
	command.Flags().StringVar(&candidates.Cursor, "cursor", "", "Continue next_cursor with the same options")
	queryFlags(command, &candidates.QueryFilters, &candidates.PageOptions)
	addEmbeddingFlags(command)
	return command
}
