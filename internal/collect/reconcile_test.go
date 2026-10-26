package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gh-mirror/internal/config"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

func pullRequest(repo string, n int, updated string) map[string]any {
	return map[string]any{"number": n, "id": n, "node_id": fmt.Sprintf("PR_%s_%d", repo, n), "title": "t", "body": "b", "state": "open", "updated_at": updated, "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, n), "labels": []any{}, "pull_request": map[string]any{"url": "https://api.github.test/repos/" + repo + "/pulls/" + fmt.Sprint(n)}}
}

func pullRequestOnly(c *config.Config, repos ...string) {
	c.Repositories = repos
	c.RepositoryOptions = map[string]config.Scope{}
	for _, repo := range repos {
		c.RepositoryOptions[repo] = config.Scope{PullRequests: true, PullRequestComments: true}
	}
}

func TestRepositoriesListConcurrently(t *testing.T) {
	db, c, f, started := setup(t, 1)
	// Each repository's first issue page waits for the other's, so a serial collector
	// times out here instead of overlapping the two cursor-paged listings.
	var arrivals sync.WaitGroup
	arrivals.Add(2)
	var overlapped atomic.Int32
	ids := map[string]int{"o/a": 1, "o/b": 2}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := []any{}
		switch r.URL.Path {
		case "/repos/o/a/issues", "/repos/o/b/issues":
			repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/issues")
			arrivals.Done()
			both := make(chan struct{})
			go func() { arrivals.Wait(); close(both) }()
			select {
			case <-both:
				overlapped.Add(1)
			case <-time.After(2 * time.Second):
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/repositories/%d/issues?after=c1&page=2>; rel="next"`, f.url, ids[repo]))
			out = append(out, pullRequest(repo, 1, f.timestamp))
		case "/repositories/1/issues":
			out = append(out, pullRequest("o/a", 2, f.timestamp))
		case "/repositories/2/issues":
			out = append(out, pullRequest("o/b", 2, f.timestamp))
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c.APIURL, c.GraphQLURL, f.url = server.URL, server.URL+"/graphql", server.URL
	pullRequestOnly(&c, "o/a", "o/b")
	result, err := syncAt(context.Background(), db, c, Options{}, started)
	if err != nil || result.Status.Issues != 4 {
		t.Fatal("concurrent listings lost tickets", result.Status, err)
	}
	if overlapped.Load() != 2 {
		t.Fatal("repositories were listed one after another", overlapped.Load())
	}
}

func TestReconciliationPrunesListingCache(t *testing.T) {
	ctx := context.Background()
	db, c, f, started := setup(t, 1)
	var issuePages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/a/issues" {
			issuePages.Add(1)
		}
		w.Header().Set("ETag", `"v1"`)
		if r.URL.Query().Get("since") == "" && r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		out := []any{}
		if r.URL.Path == "/repos/o/a/issues" && r.URL.Query().Get("since") == "" {
			out = append(out, pullRequest("o/a", 1, f.timestamp))
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c.APIURL, c.GraphQLURL, f.url = server.URL, server.URL+"/graphql", server.URL
	pullRequestOnly(&c, "o/a")
	if _, err := syncAt(ctx, db, c, Options{}, started); err != nil {
		t.Fatal(err)
	}
	api := server.URL
	stale := []string{api + "/repos/o/a/issues?direction=asc&per_page=100&sort=updated&state=all", api + "/repos/o/a/issues/comments?per_page=100&sort=updated"}
	unrelated := []string{api + "/repos/o/a/issues/5", api + "/repos/o/ab/issues?state=all", api + "/repos/o/a/issues/comments/9"}
	current := []string{api + "/repos/o/a/issues?direction=asc&per_page=100&sort=created&state=all", api + "/repos/o/a/issues/comments?direction=asc&per_page=100&sort=created"}
	if err := db.Update(ctx, func(w *store.Writer) error {
		for _, url := range append(stale, unrelated...) {
			if err := w.Cache(ctx, url, "old", []byte(`{"body":[]}`)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cached := func(want map[string]bool) {
		t.Helper()
		if err := db.Update(ctx, func(w *store.Writer) error {
			for url, present := range want {
				etag, _, err := w.Cached(ctx, url)
				if err != nil {
					return err
				}
				if (etag != "") != present {
					return fmt.Errorf("cached %s = %v, want %v", url, etag != "", present)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	all := func(urls []string, present bool, want map[string]bool) map[string]bool {
		for _, url := range urls {
			want[url] = present
		}
		return want
	}
	if _, err := syncAt(ctx, db, c, Options{}, started.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Delta listings are partial, so they never prune.
	cached(all(stale, true, all(unrelated, true, all(current, true, map[string]bool{}))))

	interrupted, cancel := context.WithCancel(ctx)
	_, err := syncAt(interrupted, db, c, Options{Full: true, Progress: func(event progress.Event) {
		if event.Resource == progress.FetchingIssues && event.Page == 1 {
			cancel()
		}
	}}, started.Add(20*time.Minute))
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatal("full sync did not stop", err)
	}
	before := issuePages.Load()
	result, err := syncAt(ctx, db, c, Options{Full: true}, started.Add(30*time.Minute))
	if err != nil || result.Status.Issues != 1 || result.Resumed < 1 {
		t.Fatal("full sync did not resume", result, err)
	}
	if issuePages.Load() != before {
		t.Fatal("resume refetched a page GitHub had confirmed unchanged", issuePages.Load()-before)
	}
	cached(all(stale, false, all(unrelated, true, all(current, true, map[string]bool{}))))
}
