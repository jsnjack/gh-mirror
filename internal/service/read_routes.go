package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"context"
	"gh-mirror/internal/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Service) readRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/issues/{owner}/{repo}/{number}/context", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			respond(w, nil, badInput("invalid ticket number"))
			return
		}
		comments, err := intParam(q, "comment_limit", 10)
		if err != nil {
			respond(w, nil, err)
			return
		}
		related, err := intParam(q, "related_limit", 5)
		if err != nil {
			respond(w, nil, err)
			return
		}
		characters, err := intParam(q, "max_characters", 12000)
		if err != nil {
			respond(w, nil, err)
			return
		}
		includeExcluded, err := boolParam(q, "include_excluded")
		if err != nil {
			respond(w, nil, err)
			return
		}
		out, err := s.Store.Context(r.Context(), store.ContextOptions{Repo: r.PathValue("owner") + "/" + r.PathValue("repo"), Number: number, Purpose: q.Get("purpose"), Query: q.Get("query"), CommentLimit: comments, RelatedLimit: related, MaxCharacters: characters, IncludeExcluded: includeExcluded})
		respond(w, out, err)
	})
	mux.HandleFunc("GET /v1/issues/{owner}/{repo}/{number}/comments", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			respond(w, nil, badInput("invalid ticket number"))
			return
		}
		limit, err := intParam(q, "limit", 30)
		if err != nil {
			respond(w, nil, err)
			return
		}
		includeExcluded, err := boolParam(q, "include_excluded")
		if err != nil {
			respond(w, nil, err)
			return
		}
		out, err := s.Store.Comments(r.Context(), store.CommentOptions{Repo: r.PathValue("owner") + "/" + r.PathValue("repo"), Number: number, Kind: q.Get("kind"), View: q.Get("view"), Order: q.Get("order"), Limit: limit, Cursor: q.Get("cursor"), IncludeExcluded: includeExcluded})
		respond(w, out, err)
	})
	mux.HandleFunc("POST /v1/issues/batch", func(w http.ResponseWriter, r *http.Request) {
		var options store.BatchOptions
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&options); err != nil {
			respond(w, nil, badInput("invalid batch JSON: %v", err))
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			respond(w, nil, badInput("batch requires one JSON object"))
			return
		}
		out, err := s.Store.Batch(r.Context(), options)
		respond(w, out, err)
	})
}
func (s *Service) readTools(server *mcp.Server) {
	mcp.AddTool(server, tool("get_issue_context", "Read bounded original ticket text, structured label attributes, section offsets, selected relevant/recent comments and one-hop relationships from one local generation. purpose=evaluate/classification/duplicates/proofread; missing sections are observations, references are not duplicate judgments. Explicit coverage and truncation report omitted material. Saved PR comment policies apply by default; include_excluded=true inspects retained excluded sources. Query text selects discussion evidence; no upstream requests or embeddings required.", store.ContextResult{}), func(ctx context.Context, _ *mcp.CallToolRequest, input store.ContextOptions) (*mcp.CallToolResult, store.ContextResult, error) {
		out, err := s.Store.Context(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, tool("list_comments", "Read a bounded page of discussion or inline review comments. Summary previews are limited to 1024 characters; full view preserves raw records. Saved PR comment policies apply; include_excluded=true returns excluded sources. Follow next_cursor with the same options.", store.CommentResult{}), func(ctx context.Context, _ *mcp.CallToolRequest, input store.CommentOptions) (*mcp.CallToolResult, store.CommentResult, error) {
		out, err := s.Store.Comments(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, tool("get_issues", "Read up to 100 selected tickets from one mirror generation, without comments. Defaults to compact summaries and explicitly reports missing identities.", store.BatchResult{}), func(ctx context.Context, _ *mcp.CallToolRequest, input store.BatchOptions) (*mcp.CallToolResult, store.BatchResult, error) {
		out, err := s.Store.Batch(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, tool("list_catalog", "Read a bounded catalog page; follow next_cursor with the same kind and scope. Available kinds: labels, milestones, issue_types, issue_fields, projects.", store.CatalogResult{}), func(ctx context.Context, _ *mcp.CallToolRequest, input store.CatalogOptions) (*mcp.CallToolResult, store.CatalogResult, error) {
		out, err := s.Store.CatalogPage(ctx, input)
		return nil, out, err
	})
}
