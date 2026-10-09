// Package service exposes the same local mirror operations through REST and MCP.
package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gh-mirror/internal/config"
	"gh-mirror/internal/snapshot"
	"gh-mirror/internal/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Service reads a live mirror or an acquired immutable snapshot.
type Service struct {
	Store       *store.Store
	SnapshotDir string
	Token       string
	Version     string
}

// ValidateListen requires an authentication token for any non-loopback listener.
func ValidateListen(address, token string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parse listening address: %w", err)
	}
	ip := net.ParseIP(host)
	if (ip == nil || !ip.IsLoopback()) && token == "" {
		return fmt.Errorf("non-loopback listener requires an API token")
	}
	return nil
}

// Handler creates authenticated REST, snapshot, and Streamable HTTP MCP routes.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	s.readRoutes(mux)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		status, err := s.Store.Status(r.Context())
		if err == nil && status.Generation == "" {
			err = fmt.Errorf("mirror has not been collected")
		}
		respond(w, status, err)
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.Store.Status(r.Context())
		respond(w, out, err)
	})
	mux.HandleFunc("GET /v1/search", func(w http.ResponseWriter, r *http.Request) {
		options, err := searchParams(r.URL.Query())
		if err != nil {
			respond(w, nil, err)
			return
		}
		out, err := s.Store.Search(r.Context(), options)
		respond(w, out, err)
	})
	mux.HandleFunc("GET /v1/issues", func(w http.ResponseWriter, r *http.Request) {
		options, err := listParams(r.URL.Query())
		if err != nil {
			respond(w, nil, err)
			return
		}
		out, err := s.Store.List(r.Context(), options)
		respond(w, out, err)
	})
	mux.HandleFunc("GET /v1/issues/{owner}/{repo}/{number}", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("owner") + "/" + r.PathValue("repo")
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil || number < 1 || !config.ValidRepository(repo) {
			respond(w, nil, badInput("invalid issue identity"))
			return
		}
		q := r.URL.Query()
		limit, err := intParam(q, "limit", 0)
		if err != nil {
			respond(w, nil, err)
			return
		}
		out, err := s.Store.Read(r.Context(), store.ReadOptions{Repo: repo, Number: number, View: q.Get("view"), Comments: q.Get("comments"), CommentKind: q.Get("comment_kind"), Limit: limit, Cursor: q.Get("cursor")})
		respond(w, out, err)
	})
	mux.HandleFunc("GET /v1/catalog", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var out store.CatalogResult
		var err error
		if q.Has("limit") || q.Has("cursor") {
			limit, e := intParam(q, "limit", 30)
			if e != nil {
				respond(w, nil, e)
				return
			}
			out, err = s.Store.CatalogPage(r.Context(), store.CatalogOptions{Kind: q.Get("kind"), Scope: q.Get("scope"), Limit: limit, Cursor: q.Get("cursor")})
		} else {
			out, err = s.Store.Catalog(r.Context(), q.Get("kind"), q.Get("scope"))
		}
		respond(w, out, err)
	})
	mux.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.URL.Query().Get("number"))
		if err != nil || number < 1 {
			respond(w, nil, badInput("positive project number required"))
			return
		}
		out, err := s.Store.Project(r.Context(), r.URL.Query().Get("owner"), number)
		respond(w, out, err)
	})
	mux.HandleFunc("GET /v1/candidates", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.URL.Query().Get("number"))
		if err != nil || number < 1 {
			respond(w, nil, badInput("positive issue number required"))
			return
		}
		limit, err := parseLimit(r)
		if err != nil {
			respond(w, nil, err)
			return
		}
		q := r.URL.Query()
		filters, page, err := queryParams(q)
		if err != nil {
			respond(w, nil, err)
			return
		}
		comments, err := boolParam(q, "include_comments")
		if err != nil {
			respond(w, nil, err)
			return
		}
		var labels *bool
		if q.Has("include_labels") {
			value, e := boolParam(q, "include_labels")
			if e != nil {
				respond(w, nil, e)
				return
			}
			labels = &value
		}
		commentLimit, err := intParam(q, "comment_limit", 20)
		if err != nil {
			respond(w, nil, err)
			return
		}
		out, err := s.Store.FindCandidates(r.Context(), store.CandidateOptions{Engine: q.Get("engine"), Repo: q.Get("repo"), Number: number, QueryFilters: filters, PageOptions: page, Limit: limit, Cursor: q.Get("cursor"), IncludeComments: comments, IncludeLabels: labels, CommentLimit: commentLimit})
		respond(w, out, err)
	})
	mux.HandleFunc("GET /snapshots/latest", func(w http.ResponseWriter, r *http.Request) {
		if s.SnapshotDir == "" {
			http.NotFound(w, r)
			return
		}
		m, err := snapshot.ReadManifest(filepath.Join(s.SnapshotDir, "latest.json"))
		respond(w, m, err)
	})
	mux.HandleFunc("GET /snapshots/{filename}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("filename")
		if s.SnapshotDir == "" || !snapshot.ValidFilename(name) {
			http.NotFound(w, r)
			return
		}
		info, err := os.Lstat(filepath.Join(s.SnapshotDir, name))
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		http.ServeFile(w, r, filepath.Join(s.SnapshotDir, name))
	})
	server := s.MCP()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if s.Token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if len(r.URL.RawQuery) > 32768 {
			http.Error(w, "query too large", http.StatusRequestURITooLong)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		mux.ServeHTTP(w, r)
	})
}
func parseLimit(r *http.Request) (int, error) {
	if r.URL.Query().Get("limit") == "" {
		return 30, nil
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 1 || limit > 100 {
		return 0, badInput("limit must be between 1 and 100")
	}
	return limit, nil
}
func respond(w http.ResponseWriter, out any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		code, category := http.StatusInternalServerError, "internal_error"
		if errors.Is(err, store.ErrInvalidQuery) {
			code, category = http.StatusBadRequest, "invalid_query"
		}
		if errors.Is(err, store.ErrSemanticUnavailable) {
			code, category = http.StatusServiceUnavailable, "semantic_index_unavailable"
		}
		if errors.Is(err, store.ErrCursorConflict) {
			code, category = http.StatusConflict, "stale_cursor"
		}
		if errors.Is(err, store.ErrNotFound) || os.IsNotExist(err) {
			code, category = http.StatusNotFound, "not_found"
		}
		w.WriteHeader(code)
		out = map[string]string{"error": err.Error(), "code": category}
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		slog.Warn("write API response", "error", err)
	}
}

type catalogInput struct {
	Kind  string `json:"kind"`
	Scope string `json:"scope"`
}
type projectInput struct {
	Owner  string `json:"owner"`
	Number int    `json:"number"`
}

func tool(name, description string, result any) *mcp.Tool {
	destructive, openWorld := false, false
	// Describe stable envelopes while leaving preserved upstream JSON unconstrained.
	return &mcp.Tool{Name: name, Description: description, OutputSchema: outputSchema(result), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &destructive, OpenWorldHint: &openWorld}}
}

// MCP creates local read tools; tool calls never fetch upstream data.
func (s *Service) MCP() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "gh-mirror", Version: s.Version}, nil)
	mcp.AddTool(server, tool("list_issues", "List local tickets with repository, state, label, type, kind and project owner/number filters. Follow next_cursor with the same filters; cursors pin a mirror generation.", store.ListResult{}), func(ctx context.Context, _ *mcp.CallToolRequest, input store.ListOptions) (*mcp.CallToolResult, store.ListResult, error) {
		out, err := s.Store.List(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, tool("search_issues", "Search local tickets using lexical, semantic or hybrid engine. Lexical supports any/all/phrase and prefix. Semantic uses bundled offline MiniLM; hybrid fuses lexical and semantic rankings. Read ranking for score meaning. Semantic engines require a complete compatible local index. Follow next_cursor with identical options.", store.SearchResult{}), func(ctx context.Context, _ *mcp.CallToolRequest, input store.SearchOptions) (*mcp.CallToolResult, store.SearchResult, error) {
		out, err := s.Store.Search(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, tool("get_issue", "Read a local ticket. Use view=summary for compact metadata, comments=none to omit comments, or comments=page with limit/cursor/comment_kind for bounded comments. Legacy full reads include all comments.", store.Issue{}), func(ctx context.Context, _ *mcp.CallToolRequest, input store.ReadOptions) (*mcp.CallToolResult, store.Issue, error) {
		out, err := s.Store.Read(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, tool("get_catalog", "Read labels, milestones, issue_types, issue_fields or projects for a scope.", store.CatalogResult{}), func(ctx context.Context, _ *mcp.CallToolRequest, input catalogInput) (*mcp.CallToolResult, store.CatalogResult, error) {
		out, err := s.Store.Catalog(ctx, input.Kind, input.Scope)
		return nil, out, err
	})
	mcp.AddTool(server, tool("get_project", "Read a local owner's project catalog record and collection status.", store.ProjectResult{}), func(ctx context.Context, _ *mcp.CallToolRequest, input projectInput) (*mcp.CallToolResult, store.ProjectResult, error) {
		out, err := s.Store.Project(ctx, input.Owner, input.Number)
		return nil, out, err
	})
	mcp.AddTool(server, tool("find_duplicate_candidates", "Find related local tickets using lexical, semantic or hybrid engine and optional labels/comments. Defaults to the seed repository; repositories can widen scope. Include closed history. Scores are retrieval scores, not duplicate probabilities; follow next_cursor with identical options.", store.SearchResult{}), func(ctx context.Context, _ *mcp.CallToolRequest, input store.CandidateOptions) (*mcp.CallToolResult, store.SearchResult, error) {
		out, err := s.Store.FindCandidates(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, tool("get_sync_status", "Read generation, scope, collection timestamps and coverage.", store.Status{}), func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, store.Status, error) {
		out, err := s.Store.Status(ctx)
		return nil, out, err
	})
	s.readTools(server)
	return server
}

// RunMCP serves local tools over the process's stdio transport.
func (s *Service) RunMCP(ctx context.Context) error {
	if err := s.MCP().Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("serve MCP stdio: %w", err)
	}
	return nil
}

// Address formats the unconditional server startup message.
func Address(listener net.Listener) string {
	return "Listening on " + strings.TrimSpace(listener.Addr().String())
}
