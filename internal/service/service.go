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
		out, err := s.Store.Get(r.Context(), repo, number)
		respond(w, out, err)
	})
	mux.HandleFunc("GET /v1/catalog", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.Store.Catalog(r.Context(), r.URL.Query().Get("kind"), r.URL.Query().Get("scope"))
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
		out, err := s.Store.Candidates(r.Context(), r.URL.Query().Get("repo"), number, limit)
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

type issueInput struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Limit  int    `json:"limit,omitempty"`
}
type catalogInput struct {
	Kind  string `json:"kind"`
	Scope string `json:"scope"`
}
type projectInput struct {
	Owner  string `json:"owner"`
	Number int    `json:"number"`
}

func tool(name, description string) *mcp.Tool {
	destructive, openWorld := false, false
	// Raw upstream JSON has variable shapes; the SDK infers RawMessage as a byte array.
	return &mcp.Tool{Name: name, Description: description, OutputSchema: map[string]any{"type": "object"}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &destructive, OpenWorldHint: &openWorld}}
}

// MCP creates local read tools; tool calls never fetch upstream data.
func (s *Service) MCP() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "gh-mirror", Version: s.Version}, nil)
	mcp.AddTool(server, tool("list_issues", "List local tickets with repository, state, label, type, kind and project owner/number filters. Follow next_cursor with the same filters; cursors pin a mirror generation."), func(ctx context.Context, _ *mcp.CallToolRequest, input store.ListOptions) (*mcp.CallToolResult, store.ListResult, error) {
		out, err := s.Store.List(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, tool("search_issues", "Search local issue titles, bodies, label names and all conversation comments using literal words."), func(ctx context.Context, _ *mcp.CallToolRequest, input store.SearchOptions) (*mcp.CallToolResult, store.SearchResult, error) {
		out, err := s.Store.Search(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, tool("get_issue", "Read a complete local issue, comments, fields and collection coverage."), func(ctx context.Context, _ *mcp.CallToolRequest, input issueInput) (*mcp.CallToolResult, store.Issue, error) {
		out, err := s.Store.Get(ctx, input.Repo, input.Number)
		return nil, out, err
	})
	mcp.AddTool(server, tool("get_catalog", "Read labels, milestones, issue_types, issue_fields or projects for a scope."), func(ctx context.Context, _ *mcp.CallToolRequest, input catalogInput) (*mcp.CallToolResult, store.CatalogResult, error) {
		out, err := s.Store.Catalog(ctx, input.Kind, input.Scope)
		return nil, out, err
	})
	mcp.AddTool(server, tool("get_project", "Read a local owner's project catalog record and collection status."), func(ctx context.Context, _ *mcp.CallToolRequest, input projectInput) (*mcp.CallToolResult, store.ProjectResult, error) {
		out, err := s.Store.Project(ctx, input.Owner, input.Number)
		return nil, out, err
	})
	mcp.AddTool(server, tool("find_duplicate_candidates", "Rank related local issues including closed history; scores are retrieval scores, not duplicate probabilities."), func(ctx context.Context, _ *mcp.CallToolRequest, input issueInput) (*mcp.CallToolResult, store.SearchResult, error) {
		out, err := s.Store.Candidates(ctx, input.Repo, input.Number, input.Limit)
		return nil, out, err
	})
	mcp.AddTool(server, tool("get_sync_status", "Read generation, scope, collection timestamps and coverage."), func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, store.Status, error) {
		out, err := s.Store.Status(ctx)
		return nil, out, err
	})
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
