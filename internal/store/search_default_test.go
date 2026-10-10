package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gh-mirror/internal/embedding"
)

func hasWarning(result SearchResult, code string) bool {
	for _, warning := range result.Warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func TestDefaultSearchEngine(t *testing.T) {
	for _, tc := range []struct {
		name, engine, match, want, warning                 string
		indexed, prefix, stale, incompatible, queryFailure bool
		wantErr                                            error
	}{
		{name: "complete index", indexed: true, want: "hybrid"},
		{name: "missing index", want: "lexical", warning: "search_engine_fallback"},
		{name: "incomplete index", indexed: true, stale: true, want: "lexical", warning: "search_engine_fallback"},
		{name: "incompatible model", indexed: true, incompatible: true, want: "lexical", warning: "search_engine_fallback"},
		{name: "explicit lexical", indexed: true, engine: "lexical", want: "lexical"},
		{name: "explicit hybrid", engine: "hybrid", wantErr: ErrSemanticUnavailable},
		{name: "explicit semantic", engine: "semantic", wantErr: ErrSemanticUnavailable},
		{name: "phrase selects lexical", indexed: true, match: "phrase", want: "lexical", warning: "search_engine_selected"},
		{name: "all selects lexical", indexed: true, match: "all", want: "lexical", warning: "search_engine_selected"},
		{name: "prefix selects lexical", indexed: true, prefix: true, want: "lexical", warning: "search_engine_selected"},
		{name: "explicit hybrid preserves validation", indexed: true, engine: "hybrid", match: "phrase", wantErr: ErrInvalidQuery},
		{name: "invalid matching mode", match: "wrong", wantErr: ErrInvalidQuery},
		{name: "inference failure is returned", indexed: true, queryFailure: true, wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			if err := db.Update(ctx, func(w *Writer) error {
				for n := 1; n <= 2; n++ {
					if _, err := w.PutIssue(ctx, "o/r", issueFixture(n, "network socket guidance", "2026-01-01T00:00:00Z")); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			encoder := &fakeVectorizer{}
			db.vectorizer = encoder
			if tc.indexed {
				if _, err := db.Index(ctx, encoder, IndexOptions{Workers: 1}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.stale {
				if err := db.Update(ctx, func(w *Writer) error {
					_, err := w.PutIssue(ctx, "o/r", issueFixture(2, "new network socket guidance", "2026-01-02T00:00:00Z"))
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.incompatible {
				encoder.identity = "different-model"
			}
			if tc.queryFailure {
				encoder.failAt = encoder.calls + 1
			}
			opts := SearchOptions{Engine: tc.engine, Query: "network socket", Match: tc.match, Prefix: tc.prefix, Limit: 1, PageOptions: PageOptions{Count: true}}
			out, err := db.Search(ctx, opts)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatal("unexpected search error", err)
				}
				return
			}
			if err != nil || out.Query.Engine != tc.want || len(out.Matches) != 1 || out.Total == nil || *out.Total != 2 || !out.HasMore {
				t.Fatal("incorrect effective search", out, err)
			}
			if tc.warning != "" && !hasWarning(out, tc.warning) {
				t.Fatal("missing engine explanation", out.Warnings)
			}
			if tc.warning == "" && (hasWarning(out, "search_engine_fallback") || hasWarning(out, "search_engine_selected")) {
				t.Fatal("unexpected engine warning", out.Warnings)
			}
			opts.Cursor = out.NextCursor
			next, err := db.Search(ctx, opts)
			if err != nil || len(next.Matches) != 1 || next.Matches[0].Number == out.Matches[0].Number || next.HasMore {
				t.Fatal("incorrect automatic continuation", next, err)
			}
		})
	}
}

func TestDefaultCandidateFallback(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	if err := db.Update(ctx, func(w *Writer) error {
		for n := 1; n <= 3; n++ {
			if _, err := w.PutIssue(ctx, "o/r", issueFixture(n, "network socket guidance", "2026-01-01T00:00:00Z")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	encoder := &fakeVectorizer{}
	db.vectorizer = encoder
	for _, indexed := range []bool{false, true} {
		t.Run(map[bool]string{false: "fallback", true: "hybrid"}[indexed], func(t *testing.T) {
			if indexed {
				if _, err := db.Index(ctx, encoder, IndexOptions{Workers: 1}); err != nil {
					t.Fatal(err)
				}
			}
			opts := CandidateOptions{Repo: "o/r", Number: 1, Limit: 1}
			out, err := db.FindCandidates(ctx, opts)
			want := "lexical"
			if indexed {
				want = "hybrid"
			}
			if err != nil || out.Query.Engine != want || !out.HasMore || len(out.Matches) != 1 || out.Matches[0].Number == 1 || hasWarning(out, "search_engine_fallback") == indexed {
				t.Fatal("incorrect automatic candidate engine", out, err)
			}
			opts.Cursor = out.NextCursor
			next, err := db.FindCandidates(ctx, opts)
			if err != nil || len(next.Matches) != 1 || next.Matches[0].Number == out.Matches[0].Number || next.HasMore {
				t.Fatal("candidate continuation failed", next, err)
			}
			if !indexed {
				opts.Cursor = ""
				opts.Engine = "lexical"
				lexical, err := db.FindCandidates(ctx, opts)
				if err != nil || !reflect.DeepEqual(lexical.Matches, out.Matches) || !reflect.DeepEqual(lexical.Query, out.Query) {
					t.Fatal("fallback changed lexical term selection or ordering", lexical, err)
				}
			}
		})
	}
}

func TestDefaultSearchCursorEngineChange(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(map[bool]string{false: "index completed", true: "index becomes incomplete"}[indexed], func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			if err := db.Update(ctx, func(w *Writer) error {
				for n := 1; n <= 2; n++ {
					if _, err := w.PutIssue(ctx, "o/r", issueFixture(n, "network guidance", "2026-01-01T00:00:00Z")); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			encoder := &fakeVectorizer{}
			db.vectorizer = encoder
			if indexed {
				if _, err := db.Index(ctx, encoder, IndexOptions{Workers: 1}); err != nil {
					t.Fatal(err)
				}
			}
			out, err := db.Search(ctx, SearchOptions{Query: "network", Limit: 1})
			if err != nil || out.NextCursor == "" {
				t.Fatal(out, err)
			}
			if indexed {
				if err := db.Update(ctx, func(w *Writer) error {
					_, err := w.PutIssue(ctx, "o/r", issueFixture(2, "changed network guidance", "2026-01-02T00:00:00Z"))
					return err
				}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := db.Index(ctx, encoder, IndexOptions{Workers: 1}); err != nil {
				t.Fatal(err)
			}
			_, err = db.Search(ctx, SearchOptions{Query: "network", Limit: 1, Cursor: out.NextCursor})
			if !errors.Is(err, ErrCursorConflict) || !strings.Contains(err.Error(), "engine changed") {
				t.Fatal("ranking changed during continuation", err)
			}
		})
	}
}

func TestDefaultSearchAgentContext(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	if err := db.Update(ctx, func(w *Writer) error {
		for _, raw := range []json.RawMessage{
			json.RawMessage(`{"number":1,"node_id":"context_1","title":"Browser download problem","body":"File transfers fail after the session reconnects.","state":"closed","updated_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/1","labels":[{"name":"client:Acme"}],"assignees":[{"login":"maintainer"}]}`),
			json.RawMessage(`{"number":2,"node_id":"context_2","title":"Invoice totals differ","body":"Tax is calculated incorrectly on monthly invoices.","state":"open","updated_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/2","labels":[]}`),
		} {
			if _, err := w.PutIssue(ctx, "o/r", raw); err != nil {
				return err
			}
		}
		if err := w.PutComment(ctx, "o/r", 1, commentFixture("Workaround for downloads after reconnecting: refresh the browser. This restores file transfers until the reconnect fix is released.", "2026-01-02T00:00:00Z")); err != nil {
			return err
		}
		return w.PutComment(ctx, "o/r", 1, json.RawMessage(`{"id":5,"body":"Fix: retry when the session token is missing.","html_url":"https://github.com/o/r/pull/1#discussion_r5","pull_request_url":"https://api.github.com/repos/o/r/pulls/1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","path":"session.go","line":42,"diff_hunk":"@@ reconnect @@"}`))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Index(ctx, embedding.Default, IndexOptions{Workers: 2}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query, field, contains string
		in                           []string
	}{
		{"workaround in closed history", "workaround for downloads after reconnecting", "comment", "refresh the browser", nil},
		{"client label", "Acme", "labels", "client:Acme", nil},
		{"review fix", "retry missing session token", "review_comment", "session token", []string{"reviews"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := db.Search(ctx, SearchOptions{Query: tc.query, In: tc.in, Limit: 1})
			if err != nil || out.Query.Engine != "hybrid" || len(out.Matches) != 1 || out.Matches[0].Number != 1 {
				t.Fatal("context retrieval failed", out, err)
			}
			match := out.Matches[0]
			if match.Summary == nil || match.State != "closed" || !reflect.DeepEqual(match.Summary.Labels, []string{"client:Acme"}) || !reflect.DeepEqual(match.Summary.Assignees, []string{"maintainer"}) || len(match.Payload) != 0 {
				t.Fatal("missing compact evaluation metadata", match)
			}
			found := false
			for _, evidence := range match.Evidence {
				if evidence.Field != tc.field || !strings.Contains(evidence.Snippet, tc.contains) {
					continue
				}
				found = true
				if evidence.Source == "" || evidence.SemanticScore == nil {
					t.Fatal("missing source attribution", evidence)
				}
				if tc.field == "comment" && (evidence.CommentID == "" || evidence.CreatedAt == "") {
					t.Fatal("missing discussion identity", evidence)
				}
				if tc.field == "review_comment" && (evidence.Path != "session.go" || evidence.Line != 42 || evidence.DiffHunk == "") {
					t.Fatal("missing review context", evidence)
				}
			}
			if !found {
				t.Fatal("missing supporting passage", match.Evidence)
			}
		})
	}
}
