package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gh-mirror/internal/commentpolicy"
)

func seedCommentPolicy(t *testing.T, db *Store) {
	t.Helper()
	seed(t, db)
	if err := db.Update(context.Background(), func(w *Writer) error {
		raw := issueFixture(1, "PR description: useful implementation details", "2026-01-03T00:00:00Z")
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return err
		}
		obj["pull_request"] = map[string]any{"url": "https://api.github.com/repos/o/r/pulls/1"}
		raw, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		if _, err := w.PutIssue(context.Background(), "o/r", raw); err != nil {
			return err
		}
		for _, c := range []struct {
			id                int
			login, kind, body string
			parent            int
			number            int
		}{
			{10, "Copilot", "Bot", "aibotuniquetoken", 0, 1},
			{11, "alice", "User", "aithreaduniquetoken", 10, 1},
			{12, "alice", "User", "humanuniquetoken", 0, 1},
			{13, "Copilot", "Bot", "aireplyuniquetoken", 12, 1},
			{14, "alice", "User", "humandiscussionuniquetoken", 0, 1},
			{15, "dependabot[bot]", "Bot", "dependencyuniquetoken", 0, 1},
			{16, "Copilot", "Bot", "issuebotuniquetoken", 0, 2},
		} {
			obj := map[string]any{"id": c.id, "body": c.body, "user": map[string]any{"login": c.login, "type": c.kind, "id": c.id + 1000}, "html_url": fmt.Sprintf("https://github.com/o/r/issues/%d#issuecomment-%d", c.number, c.id), "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-03T00:00:00Z"}
			if c.id < 14 {
				obj["pull_request_url"] = "https://api.github.com/repos/o/r/pulls/1"
			}
			if c.parent > 0 {
				obj["in_reply_to_id"] = c.parent
			}
			raw, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			if err := w.PutComment(context.Background(), "o/r", c.number, raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAppliedCommentPolicy(t *testing.T) {
	for _, name := range []string{"lexical exclusion", "context exclusion", "raw override", "issue comments retained", "actors", "invalid actor scope", "restore locally", "cursor invalidation", "atomic cancellation", "incremental sync writes", "candidate seed comments", "human AI attribution"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seedCommentPolicy(t, db)
			p := map[string]commentpolicy.Policy{"o/r": {ExcludeKnownAI: true, ExcludeAIThreads: true}}
			before, err := db.Comments(ctx, CommentOptions{Repo: "o/r", Number: 1, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Update(ctx, func(w *Writer) error { return w.ApplyCommentPolicies(ctx, p) }); err != nil {
				t.Fatal(err)
			}
			status, err := db.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if status.Generation != "testgeneration" || status.CommentFiltering == nil || status.CommentFiltering.Excluded != 3 {
				t.Fatal(status)
			}
			switch name {
			case "human AI attribution":
				if err := db.Update(ctx, func(w *Writer) error {
					raw := json.RawMessage(`{"id":17,"body":"declaredclaudeuniquetoken\n\n_🤖 Addressed by [Claude Code](https://claude.com/claude-code)_","user":{"login":"alice","type":"User"},"html_url":"https://github.com/o/r/pull/1#discussion_r17","pull_request_url":"https://api.github.com/repos/o/r/pulls/1","updated_at":"2026-01-04T00:00:00Z"}`)
					if err := w.PutComment(ctx, "o/r", 1, raw); err != nil {
						return err
					}
					return w.ApplyCommentPolicies(ctx, nil)
				}); err != nil {
					t.Fatal(err)
				}
				out, err := db.Search(ctx, SearchOptions{Query: "declaredclaudeuniquetoken", Engine: "lexical"})
				if err != nil || len(out.Matches) != 0 {
					t.Fatal(out, err)
				}
				human, err := db.Search(ctx, SearchOptions{Query: "humanuniquetoken", Engine: "lexical"})
				if err != nil || len(human.Matches) != 1 {
					t.Fatal("other comments by the same human disappeared", human, err)
				}
				actors, err := db.CommentActors(ctx, "o/r")
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, actor := range actors {
					if actor.Login == "alice" && actor.Provider == "claude" && actor.Comments == 1 && actor.Excluded == 1 {
						found = true
					}
				}
				if !found {
					t.Fatal(actors)
				}
			case "lexical exclusion", "issue comments retained":
				for _, tc := range []struct {
					query string
					want  bool
				}{{"aibotuniquetoken", false}, {"aithreaduniquetoken", false}, {"aireplyuniquetoken", false}, {"humanuniquetoken", true}, {"dependencyuniquetoken", true}, {"implementation", true}, {"issuebotuniquetoken", true}} {
					out, err := db.Search(ctx, SearchOptions{Query: tc.query, Engine: "lexical"})
					if err != nil || (len(out.Matches) > 0) != tc.want {
						t.Fatal(tc, out, err)
					}
				}
			case "context exclusion":
				out, err := db.Context(ctx, ContextOptions{Repo: "o/r", Number: 1, Query: "aibotuniquetoken"})
				if err != nil {
					t.Fatal(err)
				}
				if out.Coverage.CommentsTotal != 7 || out.Coverage.CommentsExcludedByPolicy != 3 || len(out.Comments) != 4 {
					t.Fatal(out.Coverage, out.Comments)
				}
				for _, c := range out.Comments {
					if strings.Contains(c.Body, "aibotuniquetoken") || strings.Contains(c.Body, "aithreaduniquetoken") {
						t.Fatal(c)
					}
				}
			case "raw override":
				for _, include := range []bool{false, true} {
					out, err := db.Comments(ctx, CommentOptions{Repo: "o/r", Number: 1, IncludeExcluded: include, View: "full"})
					want := 4
					if include {
						want = 7
					}
					if err != nil || len(out.Comments) != want || out.ExcludedByPolicy != 3 {
						t.Fatal(out, err)
					}
					outContext, err := db.Context(ctx, ContextOptions{Repo: "o/r", Number: 1, IncludeExcluded: include})
					if err != nil || len(outContext.Comments) != want {
						t.Fatal(outContext.Coverage, err)
					}
				}
				out, err := db.Read(ctx, ReadOptions{Repo: "o/r", Number: 1, Comments: "all"})
				if err != nil || len(out.Comments) != 7 {
					t.Fatal(out, err)
				}
				selected, err := db.Context(ctx, ContextOptions{Repo: "o/r", Number: 1, IncludeExcluded: true, CommentLimit: 1, Query: "aibotuniquetoken"})
				if err != nil || len(selected.Comments) != 1 || selected.Comments[0].Body != "aibotuniquetoken" {
					t.Fatal("query did not select retained excluded text", selected.Comments, err)
				}
			case "actors":
				actors, err := db.CommentActors(ctx, "o/r")
				if err != nil || len(actors) != 3 {
					t.Fatal(actors, err)
				}
				copilot, dependency := 0, 0
				for _, a := range actors {
					if a.Provider == "copilot" {
						copilot += a.Excluded
					}
					if a.Login == "dependabot[bot]" {
						dependency += a.Excluded
					}
				}
				if copilot != 2 || dependency != 0 {
					t.Fatal(actors)
				}
			case "invalid actor scope":
				if _, err := db.CommentActors(ctx, "invalid"); !errors.Is(err, ErrInvalidQuery) {
					t.Fatal(err)
				}
			case "candidate seed comments":
				if err := db.Update(ctx, func(w *Writer) error {
					_, err := w.PutIssue(ctx, "o/r", issueFixture(3, "aibotuniquetoken aithreaduniquetoken aireplyuniquetoken", "2026-01-03T00:00:00Z"))
					return err
				}); err != nil {
					t.Fatal(err)
				}
				out, err := db.FindCandidates(ctx, CandidateOptions{Repo: "o/r", Number: 1, Engine: "lexical", IncludeComments: true, CommentLimit: 20})
				if err != nil {
					t.Fatal(err)
				}
				for _, term := range out.Query.Terms {
					if term == "aibotuniquetoken" || term == "aithreaduniquetoken" || term == "aireplyuniquetoken" {
						t.Fatal("excluded comments seeded duplicate candidates", out.Query)
					}
				}
			case "restore locally":
				if err := db.Update(ctx, func(w *Writer) error { return w.ApplyCommentPolicies(ctx, map[string]commentpolicy.Policy{}) }); err != nil {
					t.Fatal(err)
				}
				out, err := db.Search(ctx, SearchOptions{Query: "aibotuniquetoken", Engine: "lexical"})
				if err != nil || len(out.Matches) != 1 {
					t.Fatal(out, err)
				}
			case "cursor invalidation":
				_, err := db.Comments(ctx, CommentOptions{Repo: "o/r", Number: 1, Limit: 1, Cursor: before.NextCursor})
				if !errors.Is(err, ErrCursorConflict) {
					t.Fatal(err)
				}
			case "atomic cancellation":
				child, cancel := context.WithCancel(ctx)
				cancel()
				if err := db.Update(child, func(w *Writer) error { return w.ApplyCommentPolicies(child, map[string]commentpolicy.Policy{}) }); err == nil {
					t.Fatal("cancelled write committed")
				}
				out, err := db.Search(ctx, SearchOptions{Query: "aibotuniquetoken", Engine: "lexical"})
				if err != nil || len(out.Matches) != 0 {
					t.Fatal(out, err)
				}
			case "incremental sync writes":
				if err := db.Update(ctx, func(w *Writer) error {
					raw := json.RawMessage(`{"id":10,"body":"editedaiuniquetoken","user":{"login":"Copilot","type":"Bot"},"html_url":"https://github.com/o/r/pull/1#discussion_r10","pull_request_url":"https://api.github.com/repos/o/r/pulls/1","updated_at":"2026-01-04T00:00:00Z"}`)
					if err := w.PutComment(ctx, "o/r", 1, raw); err != nil {
						return err
					}
					return w.ApplyCommentPolicies(ctx, nil)
				}); err != nil {
					t.Fatal(err)
				}
				out, err := db.Search(ctx, SearchOptions{Query: "editedaiuniquetoken", Engine: "lexical"})
				if err != nil || len(out.Matches) != 0 {
					t.Fatal(out, err)
				}
			}
		})
	}
}

func TestCommentPolicySemanticResume(t *testing.T) {
	for _, name := range []string{"exclude without inference", "restore retained comments", "snapshot policy", "nil preserves policy", "semantic evidence"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seedCommentPolicy(t, db)
			e := &fakeVectorizer{}
			if _, err := db.Index(ctx, e, IndexOptions{Workers: 2}); err != nil {
				t.Fatal(err)
			}
			calls := e.calls
			p := map[string]commentpolicy.Policy{"o/r": {ExcludeKnownAI: true, ExcludeAIThreads: true}}
			status, err := db.Index(ctx, e, IndexOptions{Workers: 2, CommentPolicies: p})
			if err != nil || e.calls != calls || status.PendingDocuments != 0 {
				t.Fatal(status, e.calls, calls, err)
			}
			switch name {
			case "restore retained comments":
				status, err = db.Index(ctx, e, IndexOptions{Workers: 2, CommentPolicies: map[string]commentpolicy.Policy{}})
				if err != nil || e.calls != calls+3 || status.PendingDocuments != 0 {
					t.Fatal(status, e.calls, calls, err)
				}
			case "semantic evidence":
				db.vectorizer = e
				out, err := db.Search(ctx, SearchOptions{Query: "review", Engine: "semantic", In: []string{"reviews"}})
				if err != nil || len(out.Matches) != 1 || len(out.Matches[0].Evidence) == 0 {
					t.Fatal(out, err)
				}
				for _, evidence := range out.Matches[0].Evidence {
					if !strings.Contains(evidence.Source, "#issuecomment-12") {
						t.Fatal(evidence)
					}
				}
			case "snapshot policy":
				path := filepath.Join(t.TempDir(), "snapshot.sqlite")
				if err := db.Export(ctx, path); err != nil {
					t.Fatal(err)
				}
				reader, err := Open(path, true)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := reader.Close(); err != nil {
						t.Error(err)
					}
				})
				out, err := reader.Context(ctx, ContextOptions{Repo: "o/r", Number: 1})
				if err != nil || out.Coverage.CommentsExcludedByPolicy != 3 || len(out.Comments) != 4 {
					t.Fatal(out.Coverage, err)
				}
			case "nil preserves policy":
				if _, err := db.Index(ctx, e, IndexOptions{Workers: 2}); err != nil {
					t.Fatal(err)
				}
				out, err := db.Search(ctx, SearchOptions{Query: "aibotuniquetoken", Engine: "lexical"})
				if err != nil || len(out.Matches) != 0 {
					t.Fatal(out, err)
				}
			}
		})
	}
}
