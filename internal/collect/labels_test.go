package collect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gh-mirror/internal/store"
)

func TestLabelCatalogChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		catalog []any
		labels  []any
	}{
		{"renamed label", []any{map[string]any{"id": 5, "name": "defect"}}, []any{map[string]any{"id": 5, "name": "defect"}}},
		{"deleted label", []any{}, []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, c, f, started := setup(t, 2)
			var mu sync.Mutex
			catalog, labels := []any{map[string]any{"id": 5, "name": "bug"}}, []any{map[string]any{"id": 5, "name": "bug"}}
			refetched := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				var out any
				if r.URL.Path == "/repos/o/r/labels" {
					out = catalog
				} else if number, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/")); err == nil {
					refetched++
					issue := f.issue(number)
					issue["labels"] = labels
					out = issue
				} else {
					f.ServeHTTP(w, r)
					return
				}
				if err := json.NewEncoder(w).Encode(out); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c.APIURL, c.GraphQLURL, f.url = server.URL, server.URL+"/graphql", server.URL
			if _, err := syncAt(ctx, db, c, Options{}, started); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			catalog, labels = tc.catalog, tc.labels
			mu.Unlock()
			f.stage = "delta"
			if _, err := syncAt(ctx, db, c, Options{}, started.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			stale, err := db.List(ctx, store.ListOptions{Label: "bug"})
			if err != nil || len(stale.Issues) != 0 || refetched != 2 {
				t.Fatal("issues kept a stale label after the catalog changed", stale.Issues, refetched, err)
			}
			if tc.name == "renamed label" {
				current, err := db.List(ctx, store.ListOptions{Label: "defect"})
				if err != nil || len(current.Issues) != 2 {
					t.Fatal("renamed label missing from issues", current.Issues, err)
				}
			}
		})
	}
}
