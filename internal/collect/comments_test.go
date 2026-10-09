package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

func TestCommentParents(t *testing.T) {
	for _, name := range []string{"bootstrap omission", "full reconciliation omission", "incremental new parent", "incremental existing parent", "resume after recovery", "inaccessible parent", "wrong parent identity"} {
		t.Run(name, func(t *testing.T) {
			count := 2
			if name == "full reconciliation omission" {
				count = 3
			}
			db, c, f, started := setup(t, count)
			ctx := context.Background()
			var enabled atomic.Bool
			var recovered atomic.Int32
			parent := 3
			if name == "incremental existing parent" {
				parent = 2
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if enabled.Load() && (r.URL.Path == "/repos/o/r/issues/comments" || r.URL.Path == fmt.Sprintf("/repos/o/r/issues/%d", parent)) {
					f.mu.Lock()
					f.requests++
					f.mu.Unlock()
					var out any
					if r.URL.Path == "/repos/o/r/issues/comments" {
						items := []any{}
						for _, ref := range []struct{ id, number int }{{123, 1}, {456, parent}, {457, parent}} {
							items = append(items, map[string]any{"id": ref.id, "body": "recoveredcommentword", "updated_at": f.timestamp, "html_url": fmt.Sprintf("https://github.com/o/r/issues/%d#issuecomment-%d", ref.number, ref.id), "issue_url": fmt.Sprintf("%s/repos/o/r/issues/%d", f.url, ref.number)})
						}
						out = items
					} else {
						recovered.Add(1)
						if name == "inaccessible parent" {
							w.WriteHeader(404)
							return
						}
						number := parent
						if name == "wrong parent identity" {
							number++
						}
						out = f.issue(number)
					}
					if err := json.NewEncoder(w).Encode(out); err != nil {
						t.Error(err)
					}
					return
				}
				f.ServeHTTP(w, r)
			}))
			defer server.Close()
			f.url = server.URL
			c.APIURL, c.GraphQLURL = server.URL, server.URL+"/graphql"
			var old store.Status
			incremental := strings.HasPrefix(name, "incremental")
			if incremental || name == "full reconciliation omission" {
				initial, err := syncAt(ctx, db, c, Options{}, started)
				if err != nil {
					t.Fatal(err)
				}
				old = initial.Status
				started = started.Add(10 * time.Minute)
				f.timestamp = started.Format(time.RFC3339)
				if incremental {
					f.stage = "delta"
				} else {
					f.count = 2
				}
			}
			enabled.Store(true)
			options := Options{Full: name == "full reconciliation omission"}
			if name == "resume after recovery" {
				interrupted, cancel := context.WithCancel(ctx)
				options.Progress = func(event progress.Event) {
					if event.Phase == "Reading repository owner" {
						cancel()
					}
				}
				_, err := syncAt(interrupted, db, c, options, started)
				cancel()
				if !errors.Is(err, context.Canceled) || recovered.Load() != 1 {
					t.Fatal("failed to interrupt after saving the recovered parent", err, recovered.Load())
				}
				after, err := db.Status(ctx)
				if err != nil || after.Generation != old.Generation {
					t.Fatal("partial recovery became visible", after, err)
				}
				options.Progress = nil
			}
			result, err := syncAt(ctx, db, c, options, started)
			if name == "inaccessible parent" || name == "wrong parent identity" {
				if err == nil || !strings.Contains(err.Error(), "recover parent of comment 456") {
					t.Fatal("unrecoverable parent was accepted", err)
				}
				after, readErr := db.Status(ctx)
				if readErr != nil || after.Generation != old.Generation || after.Issues != old.Issues {
					t.Fatal("failed recovery advanced the mirror", after, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantIssues, wantFetches := 3, int32(1)
			if name == "incremental existing parent" {
				wantIssues, wantFetches = 2, 0
			}
			if result.Status.Issues != wantIssues || result.Status.Comments != 3 || recovered.Load() != wantFetches {
				t.Fatal("missing parent or duplicate recovery requests", result, recovered.Load())
			}
			issue, err := db.Get(ctx, "o/r", parent)
			if err != nil || len(issue.Comments) != 2 {
				t.Fatal("recovery dropped comments", issue, err)
			}
			if wantFetches > 0 && !strings.Contains(string(issue.Fields), "P1") {
				t.Fatal("recovered issue was not hydrated", string(issue.Fields))
			}
			found, err := db.Search(ctx, store.SearchOptions{Query: "recoveredcommentword", Repo: "o/r"})
			if err != nil || len(found.Matches) != 2 {
				t.Fatal("recovered comments were not searchable", found, err)
			}
			if name == "incremental existing parent" && result.Requests != 2 {
				t.Fatal("known parents required extra requests", result.Requests)
			}
			if name == "incremental new parent" && result.Requests != 4 {
				t.Fatal("recovery was not batched for hydration", result.Requests)
			}
			if name == "resume after recovery" && result.Resumed != 3 {
				t.Fatal("saved pages or recovered parent were refetched", result.Resumed)
			}
		})
	}
}
