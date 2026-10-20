// Package service exposes the same local mirror operations through REST and MCP.
package service

import (
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
	// Tools selects the MCP tool set: ToolsCore (default) or ToolsAll.
	Tools string
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
		repo, number, err := pathTicket(r)
		if err != nil {
			respond(w, nil, err)
			return
		}
		q := r.URL.Query()
		limit, err := intParam(q, "limit", 0)
		if err != nil {
			respond(w, nil, err)
			return
		}
		includeExcluded, err := boolParam(q, "include_excluded")
		if err != nil {
			respond(w, nil, err)
			return
		}
		out, err := s.Store.Read(r.Context(), store.ReadOptions{Repo: repo, Number: number, View: q.Get("view"), Comments: q.Get("comments"), CommentKind: q.Get("comment_kind"), Limit: limit, Cursor: q.Get("cursor"), IncludeExcluded: includeExcluded})
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
func (s *Service) related(w http.ResponseWriter, r *http.Request, repo string, number int) {
	options, err := candidateParams(r.URL.Query(), repo, number)
	if err != nil {
		respond(w, nil, err)
		return
	}
	out, err := s.Store.FindCandidates(r.Context(), options)
	respond(w, out, err)
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

// Address formats the unconditional server startup message.
func Address(listener net.Listener) string {
	return "Listening on " + strings.TrimSpace(listener.Addr().String())
}
