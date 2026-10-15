package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gh-mirror/internal/config"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

// editableListing serves numbered issue pages whose order follows the requested sort,
// or always follows update time when ignoreSort models a shifting listing.
type editableListing struct {
	mu         sync.Mutex
	t          *testing.T
	url        string
	ignoreSort bool
	updated    map[int]time.Time
	lookups    int
}

func (f *editableListing) issue(n int) map[string]any {
	return map[string]any{"number": n, "id": n, "node_id": fmt.Sprint("I_", n), "title": "t", "body": "b", "state": "open", "updated_at": f.updated[n].Format(time.RFC3339), "html_url": fmt.Sprintf("https://github.com/o/r/issues/%d", n), "labels": []any{}}
}

func (f *editableListing) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out any
	if number, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/")); err == nil {
		f.lookups++
		out = f.issue(number)
	} else if r.URL.Path == "/repos/o/r/issues" {
		var since time.Time
		if v := r.URL.Query().Get("since"); v != "" {
			since, err = time.Parse(time.RFC3339, v)
			if err != nil {
				f.t.Error(err)
			}
		}
		numbers := []int{}
		for n, at := range f.updated {
			if !at.Before(since) {
				numbers = append(numbers, n)
			}
		}
		byUpdate := f.ignoreSort || r.URL.Query().Get("sort") == "updated"
		sort.Slice(numbers, func(i, j int) bool {
			a, b := f.updated[numbers[i]], f.updated[numbers[j]]
			if byUpdate && !a.Equal(b) {
				return a.Before(b)
			}
			return numbers[i] < numbers[j]
		})
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, err = strconv.Atoi(p)
			if err != nil {
				f.t.Error(err)
			}
		}
		items := []any{}
		for i := (page - 1) * 100; i < min(page*100, len(numbers)); i++ {
			items = append(items, f.issue(numbers[i]))
		}
		if page*100 < len(numbers) {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, f.url, r.URL.Path, q.Encode()))
		}
		out = items
	} else {
		f.t.Error("unexpected fixture path", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		f.t.Error(err)
	}
}

func TestListingEditedBetweenPages(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ignoreSort bool
		lookups    int
	}{{"creation order keeps edited listings stable", false, 0}, {"skipped live issue is confirmed before reconciliation", true, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
			f := &editableListing{t: t, ignoreSort: tc.ignoreSort, updated: map[int]time.Time{}}
			for n := 1; n <= 150; n++ {
				f.updated[n] = base.Add(time.Duration(n) * time.Minute)
			}
			server := httptest.NewServer(f)
			defer server.Close()
			f.url = server.URL
			c, err := config.Load(filepath.Join(t.TempDir(), "missing.toml"), false)
			if err != nil {
				t.Fatal(err)
			}
			c.Repositories = []string{"o/r"}
			c.RepositoryOptions = map[string]config.Scope{"o/r": {Issues: true}}
			c.Database = filepath.Join(t.TempDir(), "mirror.sqlite")
			c.APIURL, c.GraphQLURL, c.TokenEnv, c.Workers = server.URL, server.URL+"/graphql", "GH_MIRROR_TEST_TOKEN", 1
			db, err := store.Open(c.Database, false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			}()
			ctx := context.Background()
			if _, err := Sync(ctx, db, c, Options{}); err != nil {
				t.Fatal(err)
			}
			interrupted, cancel := context.WithCancel(ctx)
			defer cancel()
			if _, err := Sync(interrupted, db, c, Options{Full: true, Progress: func(e progress.Event) {
				if e.Resource == progress.FetchingIssues && e.Page == 1 {
					cancel()
				}
			}}); err == nil {
				t.Fatal("interrupted sync committed")
			}
			f.mu.Lock()
			f.updated[5] = time.Now().UTC().Truncate(time.Second)
			f.lookups = 0
			f.mu.Unlock()
			result, err := Sync(ctx, db, c, Options{Full: true})
			if err != nil {
				t.Fatal(err)
			}
			if result.Resumed == 0 || result.Status.Issues != 150 || f.lookups != tc.lookups {
				t.Fatalf("resumed full sync lost a live issue: resumed=%d issues=%d lookups=%d", result.Resumed, result.Status.Issues, f.lookups)
			}
		})
	}
}
