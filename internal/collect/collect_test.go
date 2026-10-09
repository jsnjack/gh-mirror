package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gh-mirror/internal/config"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

type fixture struct {
	mu                    sync.Mutex
	t                     *testing.T
	url                   string
	stage                 string
	count                 int
	requests              int
	queries               int
	failGraph             bool
	repeatCursor          bool
	missingNode           bool
	timestamp             string
	omitArchived          bool
	projectCursorOverflow bool
	repeatProjectCursor   bool
}

func (f *fixture) issue(n int) map[string]any {
	return map[string]any{"number": n, "id": n, "node_id": fmt.Sprint("I_", n), "title": "network crash", "body": "socket reset", "state": "closed", "updated_at": f.timestamp, "html_url": fmt.Sprintf("https://github.com/o/r/issues/%d", n), "labels": []any{map[string]any{"name": "bug"}}, "assignees": []any{map[string]any{"login": "a"}, map[string]any{"login": "b"}}, "type": map[string]any{"name": "Bug"}}
}
func emptyConnection(nodes []any, next bool, cursor string) map[string]any {
	return map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}}
}
func (f *fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Method != http.MethodGet && (r.Method != http.MethodPost || r.URL.Path != "/graphql") {
		f.t.Error("unexpected upstream method", r.Method)
	}
	var out any = []any{}
	switch r.URL.Path {
	case "/repos/o/r":
		out = map[string]any{"owner": map[string]any{"type": "Organization"}}
	case "/repos/o/r/issues":
		if f.stage == "delta" {
			if r.URL.Query().Get("since") == "" {
				f.t.Error("delta omitted issue checkpoint")
			}
			break
		}
		count := f.count
		if f.stage == "delete" {
			count = 1
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			var err error
			page, err = strconv.Atoi(p)
			if err != nil {
				f.t.Error(err)
			}
		}
		items := []any{}
		for n := (page-1)*100 + 1; n <= min(page*100, count); n++ {
			items = append(items, f.issue(n))
		}
		out = items
		if page*100 < count {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, f.url, r.URL.Path, q.Encode()))
		}
		if r.URL.Query().Get("state") != "all" || r.URL.Query().Get("per_page") != "100" {
			f.t.Error("listing failed to include closed history at maximum page size")
		}
	case "/repos/o/r/issues/comments":
		if f.stage == "fail" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if f.stage == "delete" {
			break
		}
		body := "initialcomment"
		if f.stage == "delta" {
			body = "editedcomment"
			if r.URL.Query().Get("since") == "" {
				f.t.Error("delta omitted independent comment checkpoint")
			}
		}
		out = []any{map[string]any{"id": 123, "body": body, "updated_at": f.timestamp, "created_at": f.timestamp, "html_url": "https://github.com/o/r/issues/1#issuecomment-123", "issue_url": f.url + "/repos/o/r/issues/1"}}
	case "/repos/o/r/labels":
		out = []any{map[string]any{"id": 1, "name": "unused"}}
		if f.stage == "delete" {
			out = []any{map[string]any{"id": 2, "name": "replacement"}}
		}
	case "/repos/o/r/milestones":
		out = []any{map[string]any{"id": 7, "number": 1, "state": "closed"}}
	case "/orgs/o/issue-types":
		out = []any{map[string]any{"id": 11, "name": "Bug"}}
	case "/orgs/o/issue-fields":
		out = []any{map[string]any{"id": 12, "name": "Priority", "options": []any{map[string]any{"id": 13, "name": "P1"}}}}
	case "/orgs/o/projectsV2":
		out = []any{map[string]any{"id": 20, "node_id": "P_1", "number": 1, "title": "Roadmap"}}
	case "/orgs/o/projectsV2/1/fields":
		out = []any{map[string]any{"id": 21, "name": "Status"}, map[string]any{"id": 22, "name": "Estimate"}}
	case "/orgs/o/projectsV2/1/items", "/orgs/o/projectsV2/1/items/32":
		if r.URL.Query().Get("fields") != "21,22" {
			f.t.Error("project omitted field values")
		}
		if r.URL.Query().Get("q") != "" {
			f.t.Error("project inventory used an undocumented archive filter")
		}
		projectItem := func(id int) map[string]any {
			var archived any
			if id == 32 {
				archived = f.timestamp
			}
			return map[string]any{"id": id, "node_id": fmt.Sprintf("PI_%d", id), "archived_at": archived, "content_type": "DraftIssue", "content": map[string]any{"body": "draft body"}, "fields": []any{map[string]any{"id": 21, "value": "Done"}, map[string]any{"id": 22, "value": 8}}}
		}
		if strings.HasSuffix(r.URL.Path, "/32") {
			out = projectItem(32)
		} else {
			items := []any{projectItem(31)}
			if !f.omitArchived {
				items = append(items, projectItem(32))
			}
			out = items
		}

	case "/graphql":
		f.queries++
		var input struct {
			Query     string `json:"query"`
			Variables struct {
				IDs    []string `json:"ids"`
				ID     string   `json:"id"`
				Cursor string   `json:"cursor"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			f.t.Error(err)
		}
		if f.failGraph {
			out = map[string]any{"data": map[string]any{"nodes": []any{}}, "errors": []any{map[string]any{"message": "unsupported"}}}
			break
		}
		if input.Variables.ID == "P_1" {
			if !strings.Contains(input.Query, "archivedStates:[ARCHIVED,NOT_ARCHIVED]") {
				f.t.Error("project inventory omitted explicit archived states")
			}
			items := []any{map[string]any{"id": "PI_31", "fullDatabaseId": "31", "isArchived": false}, map[string]any{"id": "PI_32", "fullDatabaseId": "32", "isArchived": true}}
			next := false
			cursor := ""
			if f.projectCursorOverflow {
				if input.Variables.Cursor == "" {
					items = items[:1]
					next = true
					cursor = "project-next"
				} else {
					items = items[1:]
					if f.repeatProjectCursor {
						next = true
						cursor = input.Variables.Cursor
					}
				}
			}
			out = map[string]any{"data": map[string]any{"node": map[string]any{"items": emptyConnection(items, next, cursor)}}}
			break
		}
		if input.Variables.ID != "" {
			cursor := ""
			next := false
			if f.repeatCursor {
				cursor = input.Variables.Cursor
				next = true
			}
			out = map[string]any{"data": map[string]any{"node": map[string]any{"issueFieldValues": emptyConnection([]any{map[string]any{"id": "fv2", "value": 8}}, next, cursor)}}}
			break
		}
		nodes := []any{}
		for _, id := range input.Variables.IDs {
			if f.missingNode && id == "I_1" {
				nodes = append(nodes, nil)
				continue
			}
			nodes = append(nodes, map[string]any{"id": id, "__typename": "Issue", "parent": nil, "subIssues": emptyConnection([]any{}, false, ""), "blockedBy": emptyConnection([]any{}, false, ""), "blocking": emptyConnection([]any{}, false, ""), "issueFieldValues": emptyConnection([]any{map[string]any{"id": "fv1", "value": "P1"}}, id == "I_1", "next"), "projectItems": emptyConnection([]any{map[string]any{"id": "item1", "isArchived": true, "project": map[string]any{"id": "P_1", "number": 1}}}, false, "")})
		}
		out = map[string]any{"data": map[string]any{"nodes": nodes}}
	default:
		f.t.Error("unexpected fixture path", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		f.t.Error(err)
	}
}
func setup(t *testing.T, count int) (*store.Store, config.Config, *fixture, time.Time) {
	t.Helper()
	started := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	f := &fixture{t: t, count: count, timestamp: started.Format(time.RFC3339)}
	server := httptest.NewServer(f)
	f.url = server.URL
	t.Cleanup(server.Close)
	c, err := config.Load(filepath.Join(t.TempDir(), "missing.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	c.Repositories = []string{"o/r"}
	c.Database = filepath.Join(t.TempDir(), "mirror.sqlite")
	c.APIURL = server.URL
	c.GraphQLURL = server.URL + "/graphql"
	c.TokenEnv = "GH_MIRROR_TEST_TOKEN"
	db, err := store.Open(c.Database, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db, c, f, started
}
func TestCollection(t *testing.T) {
	for _, name := range []string{"bootstrap complete", "bootstrap batching", "incremental two requests", "deletion inventories", "HTTP rollback", "GraphQL rollback", "cursor rollback", "missing node rollback", "budget rollback", "changed issue batching", "upstream change resets records", "archived REST fallback", "project inventory overflow", "project cursor rollback"} {
		t.Run(name, func(t *testing.T) {
			count := 2
			if name == "bootstrap batching" {
				count = 251
			}
			db, c, f, started := setup(t, count)
			ctx := context.Background()
			initial, err := syncAt(ctx, db, c, Options{}, started)
			if err != nil {
				t.Fatal(err)
			}
			if initial.Status.Issues != count || initial.Status.Comments != 1 {
				t.Fatalf("incomplete bootstrap: %+v", initial)
			}
			switch name {
			case "archived REST fallback", "project inventory overflow", "project cursor rollback":
				if name == "archived REST fallback" {
					f.omitArchived = true
				} else {
					f.projectCursorOverflow = true
					f.repeatProjectCursor = name == "project cursor rollback"
				}
				out, err := syncAt(ctx, db, c, Options{Full: true}, started.Add(time.Hour))
				if name == "project cursor rollback" {
					if err == nil {
						t.Fatal("repeated project cursor committed")
					}
					after, readErr := db.Status(ctx)
					if readErr != nil || after.Generation != initial.Status.Generation {
						t.Fatal("project cursor failure advanced generation", after, readErr)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if out.Requests != 14 {
					t.Fatal("project recovery/overflow requested unexpected data", out.Requests)
				}
				project, err := db.Project(ctx, "o", 1)
				if err != nil || len(project.Items) != 2 {
					t.Fatal("archived inventory lost items", project, err)
				}
			case "upstream change resets records":
				newServer := httptest.NewServer(f)
				defer newServer.Close()
				c.APIURL = newServer.URL
				c.GraphQLURL = newServer.URL + "/graphql"
				f.url = newServer.URL
				f.count = 1
				f.timestamp = started.Add(-time.Hour).Format(time.RFC3339)
				out, err := syncAt(ctx, db, c, Options{}, started.Add(10*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				if out.Status.Issues != 1 || out.Status.Upstream != newServer.URL {
					t.Fatal("upstream change retained old records", out)
				}
			case "bootstrap complete":
				issue, err := db.Get(ctx, "o/r", 1)
				if err != nil {
					t.Fatal(err)
				}
				var values []any
				if err := json.Unmarshal(issue.Fields, &values); err != nil {
					t.Fatal(err)
				}
				if len(values) != 2 || !strings.Contains(string(issue.Payload), `"login":"b"`) || len(issue.Comments) != 1 {
					t.Fatal("incomplete issue hydration", issue)
				}
				for _, kind := range []string{"labels", "milestones", "issue_types", "issue_fields"} {
					scope := "o"
					if kind == "labels" || kind == "milestones" {
						scope = "o/r"
					}
					out, err := db.Catalog(ctx, kind, scope)
					if err != nil || len(out.Items) != 1 {
						t.Fatal(kind, out, err)
					}
				}
				project, err := db.Project(ctx, "o", 1)
				if err != nil || len(project.Items) != 2 || len(project.Fields) != 2 {
					t.Fatal("archived/draft items missing", project, err)
				}
			case "bootstrap batching":
				if f.queries != 8 || initial.Requests != 20 {
					t.Fatalf("expected 6 ticket batches + 1 overflow + 1 project inventory, 20 total requests, got %d and %d", f.queries, initial.Requests)
				}
			case "incremental two requests":
				f.stage = "delta"
				f.timestamp = started.Add(10 * time.Minute).Format(time.RFC3339)
				out, err := syncAt(ctx, db, c, Options{}, started.Add(10*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				if out.Requests != 2 {
					t.Fatalf("incremental requested %d, want 2", out.Requests)
				}
				matches, err := db.Search(ctx, store.SearchOptions{Query: "editedcomment"})
				if err != nil || len(matches.Matches) != 1 {
					t.Fatal("comment edit not independently collected", matches, err)
				}
			case "deletion inventories":
				f.stage = "delete"
				out, err := syncAt(ctx, db, c, Options{Full: true}, started.Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if out.Status.Issues != 1 || out.Status.Comments != 0 {
					t.Fatal("inventory did not prune", out)
				}
				labels, err := db.Catalog(ctx, "labels", "o/r")
				if err != nil || len(labels.Items) != 1 || !strings.Contains(string(labels.Items[0]), "replacement") {
					t.Fatal("label catalog retained removed labels", labels, err)
				}
			case "HTTP rollback", "GraphQL rollback", "cursor rollback", "missing node rollback", "budget rollback":
				switch name {
				case "HTTP rollback":
					f.stage = "fail"
				case "GraphQL rollback":
					f.failGraph = true
				case "cursor rollback":
					f.repeatCursor = true
				case "missing node rollback":
					f.missingNode = true
				case "budget rollback":
					c.MaxRequests = 1
				}
				if _, err := syncAt(ctx, db, c, Options{Full: true}, started.Add(time.Hour)); err == nil {
					t.Fatal("failed collection unexpectedly committed")
				}
				after, err := db.Status(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if after.Generation != initial.Status.Generation || after.CollectedAt != initial.Status.CollectedAt || after.Issues != initial.Status.Issues {
					t.Fatal("failure advanced state", after)
				}
			case "changed issue batching":
				out, err := syncAt(ctx, db, c, Options{}, started.Add(10*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				if out.Requests != 2 {
					t.Fatal("unchanged overlapping issue data triggered hydration", out.Requests)
				}
				f.timestamp = started.Add(20 * time.Minute).Format(time.RFC3339)
				out, err = syncAt(ctx, db, c, Options{}, started.Add(20*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				if out.Requests != 4 {
					t.Fatal("changed issues not batched with field overflow", out.Requests)
				}
			}
		})
	}
}

func TestSharedOwnerBatches(t *testing.T) {
	t.Run("two repositories share catalogs and hydration", func(t *testing.T) {
		db, c, f, started := setup(t, 2)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/o/s/issues" {
				f.mu.Lock()
				f.requests++
				f.mu.Unlock()
				out := []any{}
				for _, n := range []int{3, 4} {
					issue := f.issue(n)
					issue["html_url"] = fmt.Sprintf("https://github.com/o/s/issues/%d", n)
					out = append(out, issue)
				}
				if err := json.NewEncoder(w).Encode(out); err != nil {
					t.Error(err)
				}
				return
			}
			if r.URL.Path == "/repos/o/s/issues/comments" {
				f.mu.Lock()
				f.requests++
				f.mu.Unlock()
				if err := json.NewEncoder(w).Encode([]any{}); err != nil {
					t.Error(err)
				}
				return
			}
			if strings.HasPrefix(r.URL.Path, "/repos/o/s/") {
				copy := r.Clone(r.Context())
				u := *r.URL
				copy.URL = &u
				copy.URL.Path = strings.Replace(copy.URL.Path, "/repos/o/s/", "/repos/o/r/", 1)
				f.ServeHTTP(w, copy)
				return
			}
			f.ServeHTTP(w, r)
		}))
		defer server.Close()
		f.url = server.URL
		c.APIURL = server.URL
		c.GraphQLURL = server.URL + "/graphql"
		c.Repositories = []string{"o/r", "o/s"}
		out, err := syncAt(context.Background(), db, c, Options{}, started)
		if err != nil {
			t.Fatal(err)
		}
		if out.Requests != 17 || f.queries != 3 || out.Status.Issues != 4 {
			t.Fatalf("shared owner issued extra requests: %+v, GraphQL=%d", out, f.queries)
		}
	})
}

func TestNativeValues(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{{"text", `{"textValue":"hello"}`}, {"date", `{"dateValue":"2026-01-01"}`}, {"number", `{"numberValue":3.5}`}, {"single", `{"singleValue":"P1","optionId":"OPTION_1"}`}, {"multi", `{"multiValue":"P1,P2","options":[{"id":"a"},{"id":"b"}]}`}} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := normalizeValues([]json.RawMessage{json.RawMessage(tc.raw)})
			if err != nil {
				t.Fatal(err)
			}
			object, err := store.Object(out[0])
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := object["value"]; !ok {
				t.Fatal("lost typed field value", string(out[0]))
			}
			if strings.Contains(string(out[0]), "Value") {
				t.Fatal("query alias leaked", string(out[0]))
			}
		})
	}
}

func TestSyncProgress(t *testing.T) {
	t.Run("pages and metadata batches retain the request budget", func(t *testing.T) {
		db, c, _, _ := setup(t, 251)
		var events []progress.Event
		result, err := Sync(context.Background(), db, c, Options{Progress: func(event progress.Event) {
			events = append(events, event)
		}})
		if err != nil {
			t.Fatal(err)
		}
		phase := ""
		issuePages, issues, hydrated, requests := 0, 0, 0, 0
		for _, event := range events {
			if event.Phase != "" {
				phase = event.Phase
			}
			if event.Resource == progress.FetchingIssues && event.Page > 0 {
				issuePages, issues = event.Page, event.Records
			}
			hydrated += event.Advance
			requests = max(requests, event.Requests)
		}
		if issuePages != 3 || issues != 251 || hydrated != 251 || requests != 20 || result.Requests != 20 {
			t.Fatalf("inaccurate progress or extra upstream requests: pages=%d issues=%d hydrated=%d requests=%d result=%d", issuePages, issues, hydrated, requests, result.Requests)
		}
		if phase != "Committing mirror" || result.Status.Generation == "" {
			t.Fatal("collection did not reach the commit phase", phase, result)
		}
	})
}
