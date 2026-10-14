package cmd

import (
	"fmt"

	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

func addReads() {
	root.AddCommand(showCommand("show", ""), showCommand("batch", "use show"), getCommand())
	addComments()
	addContext()
}

// ticketDetails renders show results as readable text; JSON output keeps the store result.
type ticketDetails struct {
	store.BatchResult
	fullBody bool
}

func showCommand(name, deprecated string) *cobra.Command {
	var view string
	command := &cobra.Command{Use: name + " REF...", GroupID: groupQuery, Short: "Show up to 100 tickets without comments", Long: "Show up to 100 tickets without comments. Use comments REF for discussion.\n\n" + refHelp, Deprecated: deprecated, Args: cobra.RangeArgs(1, 100), ValidArgsFunction: completeTicketRefs, RunE: func(c *cobra.Command, args []string) error {
		options := store.BatchOptions{View: view}
		for _, raw := range args {
			id, err := parseTicketRef(raw, settings.DefaultRepository())
			if err != nil {
				return err
			}
			options.Tickets = append(options.Tickets, id)
		}
		text, err := textOutput(c)
		if err != nil {
			return err
		}
		if text {
			options.View = "full"
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(c.Context(), db)
		out, err := db.Batch(c.Context(), options)
		if err != nil {
			return fmt.Errorf("read tickets: %w", err)
		}
		if text {
			return output(c, ticketDetails{BatchResult: out, fullBody: view == "full"})
		}
		return output(c, out)
	}}
	command.Flags().StringVar(&view, "view", "summary", "Return summary metadata or full raw records (text shows the whole body)")
	if deprecated != "" {
		command.GroupID = ""
	}
	return command
}

func getCommand() *cobra.Command {
	var repo string
	var number, limit int
	var read store.ReadOptions
	command := &cobra.Command{Use: "get [REF]", Short: "Read one ticket with legacy comment options", Deprecated: "use show REF, or comments REF for comment pages", Args: cobra.MaximumNArgs(1), RunE: func(c *cobra.Command, args []string) error {
		id, err := resolveTicket(args, repo, number)
		if err != nil {
			return err
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(c.Context(), db)
		options := read
		options.Repo, options.Number = id.Repo, id.Number
		if c.Flags().Changed("limit") {
			options.Limit = limit
		}
		out, err := db.Read(c.Context(), options)
		if err != nil {
			return fmt.Errorf("read local ticket: %w", err)
		}
		return output(c, out)
	}}
	command.Flags().StringVar(&repo, "repo", "", "Repository owner/name")
	command.Flags().IntVar(&number, "number", 0, "Issue number")
	command.Flags().IntVar(&limit, "limit", 30, "Comment page size with --comments page (1–100)")
	command.Flags().BoolVar(&read.IncludeExcluded, "include-excluded", false, "Include policy-excluded comments in page mode (legacy all retains every source)")
	command.Flags().StringVar(&read.View, "view", "full", "Return summary or full raw ticket")
	command.Flags().StringVar(&read.Comments, "comments", "", "Include none, page or legacy all comments")
	command.Flags().StringVar(&read.Cursor, "cursor", "", "Continue a comment page")
	command.Flags().StringVar(&read.CommentKind, "comment-kind", "", "Filter discussion or review comments in page mode")
	return command
}

// ticketFlags keeps the legacy --repo/--number identity flags out of help.
func ticketFlags(command *cobra.Command, repo *string, number *int) {
	command.Flags().StringVar(repo, "repo", "", "Repository owner/name (legacy; prefer REF)")
	command.Flags().IntVar(number, "number", 0, "Ticket number (legacy; prefer REF)")
	for _, name := range []string{"repo", "number"} {
		if err := command.Flags().MarkHidden(name); err != nil {
			panic(err)
		}
	}
}

func addComments() {
	var repo string
	var number int
	var comments store.CommentOptions
	command := &cobra.Command{Use: "comments REF", GroupID: groupQuery, Short: "Page through a ticket's discussion and review comments", Long: "Page through a ticket's discussion and review comments.\n\n" + refHelp, Args: cobra.MaximumNArgs(1), ValidArgsFunction: completeTicketRefs, RunE: func(c *cobra.Command, args []string) error {
		id, err := resolveTicket(args, repo, number)
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
	ticketFlags(command, &repo, &number)
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
	var repo string
	var number int
	var options store.ContextOptions
	command := &cobra.Command{Use: "context REF", GroupID: groupQuery, Short: "Read bounded ticket evidence for an agent or reviewer", Long: "Read bounded ticket text, labels, selected comments and one-hop relationships for evaluation, classification, duplicate checks or proofreading.\n\n" + refHelp, Args: cobra.MaximumNArgs(1), ValidArgsFunction: completeTicketRefs, RunE: func(c *cobra.Command, args []string) error {
		id, err := resolveTicket(args, repo, number)
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
	ticketFlags(command, &repo, &number)
	command.Flags().BoolVar(&options.IncludeExcluded, "include-excluded", false, "Include comments excluded by the saved PR retrieval policy")
	command.Flags().StringVar(&options.Purpose, "purpose", "evaluate", "Select evaluate, classification, duplicates or proofread")
	command.Flags().StringVar(&options.Query, "query", "", "Select matching discussion alongside recent comments")
	command.Flags().IntVar(&options.CommentLimit, "comment-limit", 10, "Maximum selected comments (1–50)")
	command.Flags().IntVar(&options.RelatedLimit, "related-limit", 5, "Maximum expanded related tickets (1–20)")
	command.Flags().IntVar(&options.MaxCharacters, "max-characters", 12000, "Shared source-text budget (1000–100000 characters)")
	root.AddCommand(command)
}

func addRelated() {
	root.AddCommand(relatedCommand("related", ""), relatedCommand("candidates", "use related REF"))
}

func relatedCommand(name, deprecated string) *cobra.Command {
	var repo string
	var number, limit int
	var includeLabels bool
	var candidates store.CandidateOptions
	command := &cobra.Command{Use: name + " REF", GroupID: groupQuery, Short: "Find related tickets and duplicate candidates for a ticket", Long: "Find related tickets and duplicate candidates using the seed ticket's title, body, labels and optionally recent comments. Scores rank results; they do not prove duplicates.\n\n" + refHelp, Deprecated: deprecated, Args: cobra.MaximumNArgs(1), ValidArgsFunction: completeTicketRefs, RunE: func(c *cobra.Command, args []string) error {
		id, err := resolveTicket(args, repo, number)
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
	ticketFlags(command, &repo, &number)
	command.Flags().IntVar(&limit, "limit", 30, "Maximum results (1–100)")
	command.Flags().StringVar(&candidates.Engine, "engine", store.DefaultSearchEngine, "Use lexical, semantic or hybrid retrieval (default falls back to lexical with a warning)")
	command.Flags().BoolVar(&includeLabels, "include-labels", true, "Use attached labels in seed terms")
	command.Flags().BoolVar(&candidates.IncludeComments, "include-comments", false, "Use recent seed comments")
	command.Flags().IntVar(&candidates.CommentLimit, "comment-limit", 20, "Recent seed comment limit (1–100)")
	command.Flags().StringVar(&candidates.Cursor, "cursor", "", "Continue next_cursor with the same options")
	queryFlags(command, &candidates.QueryFilters, &candidates.PageOptions)
	addEmbeddingFlags(command)
	if deprecated != "" {
		command.GroupID = ""
	}
	return command
}
