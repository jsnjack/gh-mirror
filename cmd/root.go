// Package cmd wires Cobra commands to collection, queries, snapshots, and servers.
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gh-mirror/internal/collect"
	"gh-mirror/internal/config"
	"gh-mirror/internal/diagnostics"
	"gh-mirror/internal/embedding"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/service"
	"gh-mirror/internal/snapshot"
	"gh-mirror/internal/store"

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
	addCollection()
	addIndex()
	addQueries()
	addSnapshots()
	addServers()
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
func output(command *cobra.Command, value any) error {
	encoder := json.NewEncoder(command.OutOrStdout())
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write command output: %w", err)
	}
	return nil
}
func open(readOnly bool) (*store.Store, error) {
	db, err := store.Open(settings.Database, readOnly)
	if err != nil {
		return nil, fmt.Errorf("open configured mirror: %w", err)
	}
	return db, nil
}
func closeStore(ctx context.Context, db *store.Store) {
	if err := db.Close(); err != nil {
		slog.Log(ctx, diagnostics.TraceLevel, "close mirror", "error", err)
	}
}
func addCollection() {
	var full, publish, quiet, restart, index bool
	var workers, indexWorkers int
	command := &cobra.Command{Use: "sync", Short: "Bootstrap or incrementally collect the configured repositories", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) (runErr error) {
		if command.Flags().Changed("workers") {
			settings.Workers = workers
			if err := settings.Validate(); err != nil {
				return fmt.Errorf("validate sync settings: %w", err)
			}
		}
		cpuWorkers, err := inferenceWorkers(command, "index-workers", indexWorkers)
		if err != nil {
			return err
		}
		var report progress.Reporter
		var display *progress.Display
		if !quiet {
			var err error
			display, err = progress.New(command.ErrOrStderr(), !debug)
			if err != nil {
				return fmt.Errorf("start sync progress: %w", err)
			}
			report = display.Report
			defer func() {
				if display != nil {
					if err := display.Finish(runErr); err != nil {
						runErr = errors.Join(runErr, err)
					}
				}
			}()
		}
		db, err := open(false)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		result, err := collect.Sync(command.Context(), db, settings, collect.Options{Full: full, Restart: restart, Progress: report})
		if err != nil {
			return fmt.Errorf("collect GitHub data: %w", err)
		}
		if index {
			if _, err := db.Index(command.Context(), embedding.Default, store.IndexOptions{Workers: cpuWorkers, Progress: report}); err != nil {
				return fmt.Errorf("index collected mirror: %w", err)
			}
			result.Status, err = db.Status(command.Context())
			if err != nil {
				return err
			}
		}
		var manifest *snapshot.Manifest
		if publish {
			report.Send(progress.Event{Phase: "Publishing snapshot"})
			m, err := snapshot.Publish(command.Context(), db, settings.SnapshotDir)
			if err != nil {
				return fmt.Errorf("publish collected mirror: %w", err)
			}
			manifest = &m
		}
		// Finish cursor updates before stdout can write to the same terminal.
		if display != nil {
			err := display.Finish(nil)
			display = nil
			if err != nil {
				return err
			}
		}
		return output(command, struct {
			collect.Result
			Snapshot *snapshot.Manifest `json:"snapshot,omitempty"`
		}{Result: result, Snapshot: manifest})
	}}
	command.Flags().IntVar(&workers, "workers", config.DefaultWorkers, "Maximum parallel GitHub requests (1–16; overrides configuration)")
	command.Flags().BoolVar(&index, "index", true, "Incrementally index bundled MiniLM vectors after collection")
	command.Flags().IntVar(&indexWorkers, "index-workers", 0, "Parallel CPU workers (1–16; defaults to --workers/configuration)")
	command.Flags().BoolVar(&restart, "restart", false, "Discard pending fetches and start a new sync")
	command.Flags().BoolVar(&full, "full", false, "Force complete inventories and enrichment")
	command.Flags().BoolVar(&publish, "publish", true, "Publish a standalone snapshot after collection")
	command.Flags().BoolVar(&quiet, "quiet", false, "Suppress sync progress; retain JSON output and errors")
	root.AddCommand(command)
}
func addQueries() {
	root.AddCommand(&cobra.Command{Use: "scope", Short: "Show resolved true/false collection options per repository", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		return output(command, settings.Scopes())
	}})
	var search store.SearchOptions
	command := &cobra.Command{Use: "search WORDS", Short: "Search local issues and comments", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		search.Query = args[0]
		out, err := db.Search(command.Context(), search)
		if err != nil {
			return fmt.Errorf("search local mirror: %w", err)
		}
		return output(command, out)
	}}
	command.Flags().StringVar(&search.Repo, "repo", "", "Filter repository owner/name")
	command.Flags().StringVar(&search.Engine, "engine", "lexical", "Search using lexical, semantic or hybrid retrieval")
	command.Flags().StringVar(&search.State, "state", "", "Filter open or closed")
	command.Flags().StringVar(&search.Label, "label", "", "Filter label name")
	command.Flags().StringVar(&search.Type, "type", "", "Filter native issue type name")
	command.Flags().StringVar(&search.Kind, "kind", "", "Filter issue or pull_request")
	command.Flags().StringVar(&search.Project, "project", "", "Filter project membership owner/number, including archived memberships")
	command.Flags().IntVar(&search.Limit, "limit", 30, "Maximum results (1–100)")
	queryFlags(command, &search.QueryFilters, &search.PageOptions)
	command.Flags().StringVar(&search.Match, "match", "any", "Match any words, all words across the ticket, or a phrase")
	command.Flags().BoolVar(&search.Prefix, "prefix", false, "Match word prefixes (phrase: final word only)")
	command.Flags().StringArrayVar(&search.In, "in", nil, "Search title, body, labels, comments or reviews; repeat")
	command.Flags().StringArrayVar(&search.ExcludeWords, "exclude-word", nil, "Exclude tickets containing these literal words; repeat")
	command.Flags().StringVar(&search.Cursor, "cursor", "", "Continue next_cursor with the same query")
	command.Flags().IntVar(&search.EvidenceLimit, "evidence-limit", 3, "Maximum supporting sources per ticket (1–10)")

	root.AddCommand(command)
	addList()
	addReads()
	addEvaluate()
	for _, kind := range []string{"get", "candidates"} {
		var repo string
		var number, limit int
		var read store.ReadOptions
		var candidates store.CandidateOptions
		var includeLabels bool
		command := &cobra.Command{Use: kind, Short: "Read local ticket data or retrieve duplicate candidates", Args: cobra.NoArgs}
		command.Flags().StringVar(&repo, "repo", "", "Repository owner/name")
		command.Flags().IntVar(&number, "number", 0, "Issue number")
		command.Flags().IntVar(&limit, "limit", 30, "Maximum candidate results (1–100)")
		if kind == "candidates" {
			command.Flags().StringVar(&candidates.Engine, "engine", "lexical", "Retrieve candidates using lexical, semantic or hybrid search")
			command.Flags().StringArrayVar(&candidates.Repositories, "repositories", nil, "Search these repositories; repeat (default seed repo)")
			command.Flags().BoolVar(&includeLabels, "include-labels", true, "Use attached labels in seed terms")
			command.Flags().BoolVar(&candidates.IncludeComments, "include-comments", false, "Use recent seed comments")
			command.Flags().IntVar(&candidates.CommentLimit, "comment-limit", 20, "Recent seed comment limit (1–100)")
			command.Flags().StringVar(&candidates.Cursor, "cursor", "", "Continue next_cursor")
			command.Flags().BoolVar(&candidates.Count, "count", false, "Count all candidates")
			command.Flags().BoolVar(&candidates.Facets, "facets", false, "Count candidate labels, types and projects")
			command.Flags().StringVar(&candidates.View, "view", "summary", "Return summary or full raw records")
		}
		if kind == "get" {
			command.Flags().StringVar(&read.View, "view", "full", "Return summary or full raw ticket")
			command.Flags().StringVar(&read.Comments, "comments", "", "Include none, page or legacy all comments")
			command.Flags().StringVar(&read.Cursor, "cursor", "", "Continue a comment page")
			command.Flags().StringVar(&read.CommentKind, "comment-kind", "", "Filter discussion or review comments in page mode")
		}
		command.RunE = func(command *cobra.Command, _ []string) error {
			if !config.ValidRepository(repo) || number < 1 {
				return fmt.Errorf("valid --repo and positive --number are required")
			}
			db, err := open(true)
			if err != nil {
				return err
			}
			defer closeStore(command.Context(), db)
			var out any
			if kind == "get" {
				read.Repo, read.Number = repo, number
				if command.Flags().Changed("limit") {
					read.Limit = limit
				}
				out, err = db.Read(command.Context(), read)
			} else {
				candidates.Repo, candidates.Number, candidates.Limit = repo, number, limit
				candidates.IncludeLabels = &includeLabels
				out, err = db.FindCandidates(command.Context(), candidates)
			}
			if err != nil {
				return fmt.Errorf("read local ticket: %w", err)
			}
			return output(command, out)
		}
		root.AddCommand(command)
	}
	var kind, scope string
	var catalogLimit int
	var catalogCursor string
	catalog := &cobra.Command{Use: "catalog", Short: "Read labels, milestones, issue fields/types or project catalogs", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		if kind == "" || scope == "" {
			return fmt.Errorf("--kind and --scope are required")
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		var out store.CatalogResult
		if command.Flags().Changed("limit") || catalogCursor != "" {
			out, err = db.CatalogPage(command.Context(), store.CatalogOptions{Kind: kind, Scope: scope, Limit: catalogLimit, Cursor: catalogCursor})
		} else {
			out, err = db.Catalog(command.Context(), kind, scope)
		}
		if err != nil {
			return fmt.Errorf("read local catalog: %w", err)
		}
		return output(command, out)
	}}
	catalog.Flags().StringVar(&kind, "kind", "", "Catalog name")
	catalog.Flags().StringVar(&scope, "scope", "", "Repository, owner or owner/project-number")
	catalog.Flags().IntVar(&catalogLimit, "limit", 30, "Enable pagination with this page size (1–100)")
	catalog.Flags().StringVar(&catalogCursor, "cursor", "", "Continue next_cursor")
	root.AddCommand(catalog)
	root.AddCommand(&cobra.Command{Use: "status", Short: "Read generation, scope and coverage", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		out, err := db.Status(command.Context())
		if err != nil {
			return fmt.Errorf("read local status: %w", err)
		}
		return output(command, out)
	}})
}
func addSnapshots() {
	root.AddCommand(&cobra.Command{Use: "snapshot", Short: "Publish an immutable snapshot of the last successful collection", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		out, err := snapshot.Publish(command.Context(), db, settings.SnapshotDir)
		if err != nil {
			return fmt.Errorf("publish snapshot: %w", err)
		}
		return output(command, out)
	}})
	var source, dest string
	var repos []string
	var maxAge, maxEnrichmentAge time.Duration
	command := &cobra.Command{Use: "acquire", Short: "Verify and install one private snapshot for a CI job", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		if len(repos) == 0 {
			repos = settings.Repositories
		}
		if source == "" {
			source = settings.SnapshotDir + string(os.PathSeparator) + "latest.json"
		}
		out, err := snapshot.Acquire(command.Context(), snapshot.Options{Source: source, Destination: dest, Repositories: repos, MaxAge: maxAge, MaxEnrichmentAge: maxEnrichmentAge, Token: os.Getenv(settings.APITokenEnv)})
		if err != nil {
			return fmt.Errorf("acquire CI snapshot: %w", err)
		}
		return output(command, out)
	}}
	command.Flags().StringVar(&source, "source", "", "Local latest.json or HTTPS /snapshots/latest URL")
	command.Flags().StringVar(&dest, "dest", "", "Private destination database (required)")
	command.Flags().StringSliceVar(&repos, "repo", nil, "Exact required repository scope")
	command.Flags().DurationVar(&maxAge, "max-age", 2*time.Hour, "Maximum collection age")
	command.Flags().DurationVar(&maxEnrichmentAge, "max-enrichment-age", 0, "Maximum metadata enrichment age (0 disables the check)")
	root.AddCommand(command)
}
func addServers() {
	var listen string
	command := &cobra.Command{Use: "serve", Short: "Serve local REST, MCP HTTP and immutable snapshots", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		address := listen
		if address == "" {
			address = settings.Listen
		}
		token := os.Getenv(settings.APITokenEnv)
		if err := service.ValidateListen(address, token); err != nil {
			return fmt.Errorf("validate HTTP listener: %w", err)
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		svc := &service.Service{Store: db, SnapshotDir: settings.SnapshotDir, Token: token, Version: Version}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		defer func() {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				slog.Log(command.Context(), diagnostics.TraceLevel, "close listener", "error", err)
			}
		}()
		if _, err := fmt.Fprintln(command.ErrOrStderr(), service.Address(listener)); err != nil {
			return fmt.Errorf("write listening address: %w", err)
		}
		slog.Log(command.Context(), diagnostics.TraceLevel, service.Address(listener))
		server := &http.Server{Handler: svc.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 5 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32768}
		done := make(chan struct{})
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			select {
			case <-command.Context().Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := server.Shutdown(shutdownCtx); err != nil {
					slog.Warn("shut down HTTP server", "error", err)
					if err := server.Close(); err != nil {
						slog.Log(context.Background(), diagnostics.TraceLevel, "close HTTP server", "error", err)
					}
				}
			case <-done:
			}
		}()
		err = server.Serve(listener)
		close(done)
		<-stopped
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil
	}}
	command.Flags().StringVar(&listen, "listen", "", "Override listening address")
	root.AddCommand(command)
	root.AddCommand(&cobra.Command{Use: "mcp", Short: "Serve local read tools over MCP stdio", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		svc := &service.Service{Store: db, Version: Version}
		return svc.RunMCP(command.Context())
	}})
}
