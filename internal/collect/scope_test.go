package collect

import (
	"context"
	"encoding/json"
	"errors"
	"gh-mirror/internal/progress"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gh-mirror/internal/config"
	"gh-mirror/internal/store"
)

func TestRepositoryScopes(t *testing.T) {
	for _, name := range []string{"PR and comments", "PR all comments", "PR review only", "review toggle", "issues and comments", "no comments", "explicit all compatible", "narrow scope", "expand scope"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, c, f, started := setup(t, 2)
			f.count = 2
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/o/r/issues" {
					items := []any{}
					if r.URL.Query().Get("since") == "" {
						issue, pr := f.issue(1), f.issue(2)
						pr["pull_request"] = map[string]any{"url": "https://api.github.test/repos/o/r/pulls/2"}
						items = []any{issue, pr}
					}
					if err := json.NewEncoder(w).Encode(items); err != nil {
						t.Error(err)
					}
					return
				}
				if r.URL.Path == "/repos/o/r/issues/comments" {
					comments := []any{}
					for n := 1; n <= 2; n++ {
						comments = append(comments, map[string]any{"id": n, "body": "scopedcomment", "updated_at": f.timestamp, "html_url": "https://github.com/o/r/issues/1#comment", "issue_url": f.url + "/repos/o/r/issues/" + string(rune('0'+n))})
					}
					if err := json.NewEncoder(w).Encode(comments); err != nil {
						t.Error(err)
					}
					return
				}
				if r.URL.Path == "/repos/o/r/pulls/comments" {
					if err := json.NewEncoder(w).Encode([]any{map[string]any{"id": 2, "body": "inlineword", "updated_at": f.timestamp, "html_url": "https://github.com/o/r/pull/2#discussion_r2", "pull_request_url": f.url + "/repos/o/r/pulls/2", "diff_hunk": "@@ code @@"}}); err != nil {
						t.Error(err)
					}
					return
				}
				if r.URL.Path == "/graphql" {
					var input struct {
						Variables struct {
							IDs []string `json:"ids"`
						}
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					nodes := []any{}
					for _, id := range input.Variables.IDs {
						kind := "Issue"
						if id == "I_2" {
							kind = "PullRequest"
						}
						nodes = append(nodes, map[string]any{"id": id, "__typename": kind, "parent": nil, "subIssues": emptyConnection([]any{}, false, ""), "blockedBy": emptyConnection([]any{}, false, ""), "blocking": emptyConnection([]any{}, false, ""), "issueFieldValues": emptyConnection([]any{}, false, ""), "projectItems": emptyConnection([]any{projectMembership(false)}, false, "")})
					}
					if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": nodes}}); err != nil {
						t.Error(err)
					}
					return
				}
				f.ServeHTTP(w, r)
			}))
			defer server.Close()
			c.APIURL, c.GraphQLURL, f.url = server.URL, server.URL+"/graphql", server.URL
			prOnly := config.Scope{PullRequests: true, PullRequestComments: true}
			selected := prOnly
			switch name {
			case "PR all comments", "review toggle":
				selected.PullRequestReviewComments = true
			case "PR review only":
				selected.PullRequestComments = false
				selected.PullRequestReviewComments = true
			case "issues and comments":
				selected = config.Scope{Issues: true, IssueComments: true}
			case "no comments":
				selected = config.Scope{PullRequests: true}
			case "explicit all compatible", "narrow scope":
				selected = c.DefaultScope()
			}
			c.RepositoryOptions = map[string]config.Scope{"o/r": selected}
			initial, err := syncAt(ctx, db, c, Options{}, started)
			if err != nil {
				t.Fatal(err)
			}
			wantIssues, wantComments, requests := 1, 1, 2
			if name == "no comments" {
				wantComments, requests = 0, 1
			}
			if name == "PR all comments" || name == "review toggle" {
				wantComments, requests = 2, 3
			}
			if name == "explicit all compatible" || name == "narrow scope" {
				wantIssues, wantComments, requests = 2, 2, 9
			}
			if initial.Status.Issues != wantIssues || initial.Status.Comments != wantComments || initial.Requests != requests {
				t.Fatal("wrong scoped bootstrap", initial)
			}
			when := started.Add(10 * time.Minute)
			if name == "review toggle" {
				selected.PullRequestReviewComments = false
				c.RepositoryOptions["o/r"] = selected
				wantComments, requests = 1, 2
			}
			if name == "narrow scope" {
				c.RepositoryOptions["o/r"] = prOnly
				wantIssues, wantComments, requests = 1, 1, 2
			}
			if name == "expand scope" {
				c.RepositoryOptions["o/r"] = c.DefaultScope()
				wantIssues, wantComments, requests = 2, 2, 9
			}
			delta, err := syncAt(ctx, db, c, Options{}, when)
			if err != nil {
				t.Fatal(err)
			}
			if name != "narrow scope" && name != "expand scope" {
				requests = 2
				if name == "PR all comments" {
					requests = 3
				}
				if name == "no comments" {
					requests = 1
				}
			}
			if delta.Status.Issues != wantIssues || delta.Status.Comments != wantComments || delta.Requests != requests {
				t.Fatal("scope change or delta wasted requests", delta)
			}
			if name == "narrow scope" {
				issue, err := db.Get(ctx, "o/r", 2)
				if err != nil || string(issue.Fields) != "[]" || string(issue.Extra) != "{}" {
					t.Fatal("excluded metadata retained", issue, err)
				}
				for _, kind := range []string{"labels", "milestones", "issue_types", "issue_fields", "projects"} {
					scope := "o"
					if kind == "labels" || kind == "milestones" {
						scope = "o/r"
					}
					catalog, err := db.Catalog(ctx, kind, scope)
					if err != nil || len(catalog.Items) != 0 {
						t.Fatal("excluded catalog retained", kind, catalog, err)
					}
				}
				found, err := db.Search(ctx, store.SearchOptions{Query: "scopedcomment"})
				if err != nil || len(found.Matches) != 1 || found.Matches[0].Kind != "pull_request" {
					t.Fatal("excluded comments remained searchable", found, err)
				}
			}
			raw, err := json.Marshal(delta.Status)
			if err != nil || !strings.Contains(string(raw), "repository_options") {
				t.Fatal("status omitted scope", err)
			}
		})
	}
}

func TestRepositoryScopeResume(t *testing.T) {
	t.Run("narrowing keeps previous mirror and saved inventory", func(t *testing.T) {
		ctx := context.Background()
		db, c, _, started := setup(t, 2)
		c.Workers = 1
		initial, err := syncAt(ctx, db, c, Options{}, started)
		if err != nil {
			t.Fatal(err)
		}
		c.RepositoryOptions = map[string]config.Scope{"o/r": {Issues: true}}
		when := started.Add(10 * time.Minute)
		stopCtx, cancel := context.WithCancel(ctx)
		_, err = syncAt(stopCtx, db, c, Options{Progress: func(e progress.Event) {
			if e.Resource == progress.FetchingIssues && e.Page == 1 {
				cancel()
			}
		}}, when)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatal("scope change did not cancel", err)
		}
		previous, err := db.Status(ctx)
		if err != nil || previous.Generation != initial.Status.Generation || previous.Comments != initial.Status.Comments {
			t.Fatal("failed scope change replaced previous data", previous, err)
		}
		result, err := syncAt(ctx, db, c, Options{}, when.Add(time.Minute))
		if err != nil || result.Requests != 0 || result.Resumed != 1 || result.Status.Comments != 0 || result.Status.Issues != 2 || result.Status.CollectedAt != when.Format(time.RFC3339Nano) {
			t.Fatal("scope change lost saved progress", result, err)
		}
	})
}

func TestMixedRepositoryScopes(t *testing.T) {
	t.Run("full repository and PR-only repository share one collector", func(t *testing.T) {
		ctx := context.Background()
		db, c, f, started := setup(t, 2)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/repos/o/s/") {
				out := []any{}
				if strings.HasSuffix(r.URL.Path, "/issues") && r.URL.Query().Get("since") == "" {
					issue, pr := f.issue(3), f.issue(4)
					pr["pull_request"] = map[string]any{"url": "https://api.github.test/repos/o/s/pulls/4"}
					out = []any{issue, pr}
				}
				if strings.HasSuffix(r.URL.Path, "/comments") {
					for n := 3; n <= 4; n++ {
						out = append(out, map[string]any{"id": n, "body": "mixedcomment", "updated_at": f.timestamp, "html_url": "https://github.com/o/s/issues/4#comment", "issue_url": f.url + "/repos/o/s/issues/" + strconv.Itoa(n)})
					}
				}
				if err := json.NewEncoder(w).Encode(out); err != nil {
					t.Error(err)
				}
				return
			}
			f.ServeHTTP(w, r)
		}))
		defer server.Close()
		c.APIURL, c.GraphQLURL, f.url = server.URL, server.URL+"/graphql", server.URL
		c.Repositories = []string{"o/r", "o/s"}
		c.RepositoryOptions = map[string]config.Scope{"o/r": c.DefaultScope(), "o/s": {PullRequests: true, PullRequestComments: true}}
		initial, err := syncAt(ctx, db, c, Options{}, started)
		if err != nil || initial.Status.Issues != 3 || initial.Status.Comments != 2 || initial.Requests != 12 {
			t.Fatal("mixed scopes collected wrong resources", initial, err)
		}
		f.stage = "delta"
		delta, err := syncAt(ctx, db, c, Options{}, started.Add(10*time.Minute))
		if err != nil || delta.Requests != 4 || delta.Status.Issues != 3 || delta.Status.Comments != 2 {
			t.Fatal("mixed scopes forced extra requests", delta, err)
		}
		pr, err := db.Get(ctx, "o/s", 4)
		if err != nil || len(pr.Comments) != 1 || string(pr.Fields) != "[]" || string(pr.Extra) != "{}" {
			t.Fatal("PR-only repository collected full metadata", pr, err)
		}
	})
}
