package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"gh-mirror/internal/store"
)

func (s *Service) readRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/issues/{owner}/{repo}/{number}/related", func(w http.ResponseWriter, r *http.Request) {
		repo, number, err := pathTicket(r)
		if err != nil {
			respond(w, nil, err)
			return
		}
		s.related(w, r, repo, number)
	})
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
