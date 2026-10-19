// Package cmd wires Cobra commands to collection, queries, snapshots, and servers.
package cmd

import (
	"context"
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
var debug, trace, ensureFresh bool
var staleAfterFlag time.Duration
var outputFormat = "auto"
var logCloser io.Closer
var embeddingProvider *embedding.Provider
var embeddingBackend, embeddingRuntime, embeddingDevice string
var embeddingBatchSize int
var root = &cobra.Command{Use: "gh-mirror", Short: "Mirror GitHub tickets into a portable, searchable SQLite database", SilenceUsage: true, SilenceErrors: true}

// Command groups order the help output by task.
const (
	groupQuery    = "query"
	groupCollect  = "collect"
	groupServe    = "serve"
	groupInspect  = "inspect"
	groupAdvanced = "advanced"
)

// configOptional marks commands that report setup errors instead of failing before running.
const configOptional = "gh-mirror/config-optional"

// setupErr keeps the configuration or encoder error for commands marked configOptional.
var setupErr error

func init() {
	root.Version = Version
	root.SetVersionTemplate("{{.Version}}\n")
	root.Flags().Bool("version", false, "Print the version and exit")
	root.PersistentFlags().Bool("help", false, "Show command help")
	root.AddGroup(&cobra.Group{ID: groupQuery, Title: "Query the local mirror:"}, &cobra.Group{ID: groupCollect, Title: "Collect and publish:"}, &cobra.Group{ID: groupServe, Title: "Serve agents and clients:"}, &cobra.Group{ID: groupInspect, Title: "Inspect configuration and state:"}, &cobra.Group{ID: groupAdvanced, Title: "Advanced:"})
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "", "TOML configuration file (default: XDG gh-mirror/config.toml)")
	root.PersistentFlags().StringVar(&database, "db", "", "Override the database path")
	root.PersistentFlags().StringVar(&outputFormat, "format", "auto", "Output format: auto (text in terminals, JSON when piped), text or json")
	root.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "Verbose diagnostics on stderr")
	root.PersistentFlags().BoolVar(&trace, "trace", false, "Detailed diagnostics in the temporary gh-mirror-<uid>.log")
	root.PersistentFlags().BoolVar(&ensureFresh, "ensure-fresh", false, "Sync first when the mirror is older than stale_after (default: ensure_fresh for queries and status; false skips)")
	root.PersistentFlags().DurationVar(&staleAfterFlag, "stale-after", 0, "Override stale_after for --ensure-fresh (default: configuration, 1h)")
	root.PersistentPreRunE = func(command *cobra.Command, _ []string) error {
		if completionCommand(command) {
			return nil
		}
		if _, err := resolveOutputFormat(outputFormat, false); err != nil {
			return err
		}
		logger, closer, err := diagnostics.Setup(debug, trace, command.ErrOrStderr())
		if err != nil {
			return fmt.Errorf("set up diagnostics: %w", err)
		}
		logCloser = closer
		slog.SetDefault(logger)
		setupErr = setup(command)
		if setupErr != nil && command.Annotations[configOptional] == "" {
			return setupErr
		}
		if setupErr != nil || command.Annotations[ensureFreshMode] == freshSelf {
			return nil
		}
		staleAfter, err := staleThreshold(command)
		if err != nil || staleAfter == 0 {
			return err
		}
		return ensureFreshMirror(command, staleAfter)
	}
	addConfig()
	addBots()
	addCollection()
	addIndex()
	addQueries()
	addSnapshots()
	addServers()
	addDoctor()
	addCompletions()
	markEnsureFresh()
}

func setup(command *cobra.Command) error {
	path := configPath
	if path == "" {
		var err error
		path, err = config.DefaultPath()
		if err != nil {
			return fmt.Errorf("resolve settings: %w", err)
		}
	}
	var err error
	settings, err = config.Load(path, configPath != "")
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if database != "" {
		settings.Database = database
	}
	if command.Flags().Changed("embedding-backend") {
		settings.Embedding.Backend = embeddingBackend
	}
	if command.Flags().Changed("embedding-runtime") {
		settings.Embedding.Runtime = embeddingRuntime
	}
	if command.Flags().Changed("embedding-device") {
		settings.Embedding.Device = embeddingDevice
	}
	if command.Flags().Changed("embedding-batch-size") {
		if embeddingBatchSize < 1 {
			return fmt.Errorf("embedding-batch-size must be between 1 and 32")
		}
		settings.Embedding.BatchSize = embeddingBatchSize
	}
	if err := settings.Validate(); err != nil {
		return fmt.Errorf("validate settings: %w", err)
	}
	if embeddingProvider != nil {
		if err := embeddingProvider.Close(); err != nil {
			return err
		}
	}
	opts := settings.Embedding
	opts.Workers = settings.Workers
	embeddingProvider, err = embedding.New(command.Context(), opts)
	if err != nil {
		return fmt.Errorf("start embedding provider: %w", err)
	}
	return nil
}

// Execute runs commands with interrupt cancellation and closes trace diagnostics.
func Execute() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	defer func() {
		if embeddingProvider != nil {
			if err := embeddingProvider.Close(); err != nil {
				slog.Debug("close embedding provider", "error", err)
			}
			embeddingProvider = nil
		}
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
func open(readOnly bool) (*store.Store, error) {
	db, err := store.OpenWithVectorizer(settings.Database, readOnly, commandEncoder())
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
	var run syncRun
	var colorFlag string
	var workers, indexWorkers int
	command := &cobra.Command{Use: "sync", GroupID: groupCollect, Short: "Collect configured repositories from GitHub, then index and publish", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		var err error
		if run.staleAfter, err = staleThreshold(command); err != nil {
			return err
		}
		if run.color, err = progress.ParseColor(colorFlag); err != nil {
			return err
		}
		if command.Flags().Changed("workers") {
			settings.Workers = workers
			if err := settings.Validate(); err != nil {
				return fmt.Errorf("validate sync settings: %w", err)
			}
		}
		if run.cpuWorkers, err = inferenceWorkers(command, "index-workers", indexWorkers); err != nil {
			return err
		}
		if err := configureIndexEncoder(command.Context(), run.cpuWorkers); err != nil {
			return err
		}
		out, _, err := runSync(command, run)
		if err != nil {
			return err
		}
		return output(command, out)
	}}
	command.Flags().IntVar(&workers, "workers", config.DefaultWorkers, "Maximum parallel GitHub requests (1–16; overrides configuration)")
	command.Flags().BoolVar(&run.index, "index", true, "Incrementally index bundled MiniLM vectors after collection")
	command.Flags().IntVar(&indexWorkers, "index-workers", 0, "Parallel CPU workers (1–16; defaults to --workers/configuration)")
	command.Flags().BoolVar(&run.restart, "restart", false, "Discard pending fetches and start a new sync")
	command.Flags().BoolVar(&run.full, "full", false, "Force complete inventories and enrichment")
	command.Flags().BoolVar(&run.publish, "publish", true, "Publish a standalone snapshot after collection")
	command.Flags().BoolVar(&run.quiet, "quiet", false, "Suppress sync progress; retain results and errors")
	addColorFlag(command, &colorFlag)
	addEmbeddingFlags(command)
	root.AddCommand(command)
}
func addQueries() {
	root.AddCommand(&cobra.Command{Use: "scope", GroupID: groupInspect, Short: "Show resolved collection switches per repository", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		return output(command, settings.Scopes())
	}})
	var search store.SearchOptions
	command := &cobra.Command{Use: "search WORDS", GroupID: groupQuery, Short: "Search tickets and comments by words or meaning", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		search.Query = args[0]
		options := search
		if !command.Flags().Changed("engine") {
			options.Engine = ""
		}
		out, err := db.Search(command.Context(), options)
		if err != nil {
			return fmt.Errorf("search local mirror: %w", err)
		}
		return output(command, out)
	}}
	command.Flags().StringVar(&search.Repo, "repo", "", "Filter repository owner/name")
	command.Flags().StringVar(&search.Engine, "engine", store.DefaultSearchEngine, "Search using lexical, semantic or hybrid retrieval (default falls back to lexical with a warning)")
	command.Flags().StringVar(&search.State, "state", "", "Filter open or closed")
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
	addEmbeddingFlags(command)
	root.AddCommand(command)
	addList()
	addReads()
	addRelated()
	addCatalog()
	addStatus()
	addEvaluate()
}
func addSnapshots() {
	root.AddCommand(&cobra.Command{Use: "snapshot", GroupID: groupCollect, Short: "Publish an immutable snapshot of the last successful collection", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
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
	command := &cobra.Command{Use: "acquire", GroupID: groupCollect, Short: "Verify and install a published snapshot for a CI job", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
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
	var listen, serveTools, mcpTools string
	command := &cobra.Command{Use: "serve", GroupID: groupServe, Short: "Serve REST, MCP over HTTP and snapshots", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
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
		svc := &service.Service{Store: db, SnapshotDir: settings.SnapshotDir, Token: token, Version: Version, Tools: serveTools}
		if err := svc.ValidateTools(); err != nil {
			return err
		}
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
	command.Flags().StringVar(&serveTools, "mcp-tools", service.ToolsCore, "MCP tool set: core or all")
	addEmbeddingFlags(command)
	root.AddCommand(command)
	mcpCommand := &cobra.Command{Use: "mcp", GroupID: groupServe, Short: "Serve read tools to agents over MCP stdio", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		svc := &service.Service{Version: Version, Tools: mcpTools}
		if err := svc.ValidateTools(); err != nil {
			return err
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(command.Context(), db)
		svc.Store = db
		return svc.RunMCP(command.Context())
	}}
	mcpCommand.Flags().StringVar(&mcpTools, "tools", service.ToolsCore, "Tool set: core (search and ticket reads) or all (adds listing, catalogs and projects)")
	addEmbeddingFlags(mcpCommand)
	root.AddCommand(mcpCommand)
}
