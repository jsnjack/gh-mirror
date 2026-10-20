package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gh-mirror/internal/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestQueryAPI(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		code                     int
	}{
		{"context", "GET", "/v1/issues/o/r/1/context?purpose=proofread&max_characters=1000", "", 200},
		{"invalid context", "GET", "/v1/issues/o/r/1/context?comment_limit=100", "", 400},
		{"invalid context exclusion override", "GET", "/v1/issues/o/r/1/context?include_excluded=invalid", "", 400},
		{"invalid comment exclusion override", "GET", "/v1/issues/o/r/1/comments?include_excluded=invalid", "", 400},
		{"invalid get exclusion override", "GET", "/v1/issues/o/r/1?include_excluded=invalid", "", 400},
		{"all words", "GET", "/v1/search?q=client+socket&match=all&count=true&facets=true&limit=1", "", 200},
		{"multiple repositories", "GET", "/v1/issues?repo=o/r&repo=o/s&view=summary", "", 200},
		{"bad boolean", "GET", "/v1/search?q=socket&count=invalid", "", 400},
		{"bad date", "GET", "/v1/search?q=socket&updated_after=yesterday", "", 400},
		{"summary get", "GET", "/v1/issues/o/r/1?view=summary", "", 200},
		{"comments", "GET", "/v1/issues/o/r/1/comments?limit=1", "", 200},
		{"batch", "POST", "/v1/issues/batch", `{"tickets":[{"repo":"o/r","number":1},{"repo":"o/r","number":9}]}`, 200},
		{"batch unknown", "POST", "/v1/issues/batch", `{"tickets":[{"repo":"o/r","number":1}],"sql":"bad"}`, 400},
		{"trailing batch", "POST", "/v1/issues/batch", `{"tickets":[{"repo":"o/r","number":1}]} {}`, 400},
		{"catalog page", "GET", "/v1/catalog?kind=labels&scope=o/r&limit=1", "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := fixture(t)
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			svc.Handler().ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatal(w.Code, w.Body.String())
			}
			var out map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if tc.code == 400 {
				if string(out["code"]) != `"invalid_query"` {
					t.Fatal(w.Body.String())
				}
			}
			if tc.name == "all words" {
				if string(out["has_more"]) != "true" || string(out["total"]) != "2" {
					t.Fatal(w.Body.String())
				}
			}
			if tc.name == "summary get" && out["issue"] != nil {
				t.Fatal("summary exposed raw payload")
			}
		})
	}
}

func TestAPIErrorCategories(t *testing.T) {
	for _, name := range []string{"stale cursor", "internal failure"} {
		t.Run(name, func(t *testing.T) {
			svc := fixture(t)
			path := "/v1/search?q=socket"
			want := 500
			if name == "stale cursor" {
				first, err := svc.Store.Search(context.Background(), store.SearchOptions{Query: "socket", Limit: 1})
				if err != nil {
					t.Fatal(err)
				}
				if err := svc.Store.Update(context.Background(), func(w *store.Writer) error { return w.SetMetadata(context.Background(), "generation", "new") }); err != nil {
					t.Fatal(err)
				}
				path += "&limit=1&cursor=" + first.NextCursor
				want = 409
			} else {
				if err := svc.Store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			svc.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}
func TestNewMCPTools(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input any
	}{
		{"get_issue_context", store.ContextOptions{Repo: "o/r", Number: 1, Purpose: "classification"}},
		{"list_comments", store.CommentOptions{Repo: "o/r", Number: 1}},
		{"get_issues", store.BatchOptions{Tickets: []store.TicketID{{Repo: "o/r", Number: 1}}}},
		{"list_catalog", store.CatalogOptions{Kind: "labels", Scope: "o/r", Limit: 1}},
		{"search_issues", store.SearchOptions{Query: "socket", Match: "all", PageOptions: store.PageOptions{Count: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := fixture(t)
			svc.Tools = ToolsAll
			ctx := context.Background()
			a, b := mcp.NewInMemoryTransports()
			server, err := svc.MCP().Connect(ctx, a, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := server.Close(); err != nil {
					t.Error(err)
				}
			}()
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
			session, err := client.Connect(ctx, b, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			}()
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.input})
			if err != nil || result.IsError {
				t.Fatal(result, err)
			}
			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range tools.Tools {
				if tool.OutputSchema != nil {
					t.Fatal("tool advertises an output schema", tool.Name)
				}
				var schema struct {
					Properties map[string]struct {
						Description string `json:"description"`
					} `json:"properties"`
				}
				raw, err := json.Marshal(tool.InputSchema)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &schema); err != nil {
					t.Fatal(err)
				}
				for name, property := range schema.Properties {
					if property.Description == "" {
						t.Fatalf("%s input %s has no description", tool.Name, name)
					}
				}
			}
		})
	}
}

func connectMCP(t *testing.T, svc *Service) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	a, b := mcp.NewInMemoryTransports()
	server, err := svc.MCP().Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	return session
}

func TestMCPToolSets(t *testing.T) {
	core := []string{"find_duplicate_candidates", "get_issue", "get_issue_context", "get_issues", "get_sync_status", "list_comments", "search_issues"}
	all := append(slices.Clone(core), "get_catalog", "get_project", "list_catalog", "list_issues")
	slices.Sort(all)
	for _, tc := range []struct {
		tools string
		want  []string
	}{{"", core}, {ToolsCore, core}, {ToolsAll, all}} {
		t.Run("set "+tc.tools, func(t *testing.T) {
			svc := fixture(t)
			svc.Tools = tc.tools
			tools, err := connectMCP(t, svc).ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, tool := range tools.Tools {
				names = append(names, tool.Name)
			}
			slices.Sort(names)
			if !slices.Equal(names, tc.want) {
				t.Fatalf("got %v, want %v", names, tc.want)
			}
		})
	}
	t.Run("invalid set", func(t *testing.T) {
		if err := (&Service{Tools: "some"}).ValidateTools(); err == nil {
			t.Fatal("accepted unknown tool set")
		}
	})
}

func TestMCPCompactResults(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		args       map[string]any
		check      func(map[string]any) bool
	}{
		{"get_issue defaults to summary without comments", "get_issue", map[string]any{"repo": "o/r", "number": 1}, func(out map[string]any) bool {
			return out["issue"] == nil && len(out["comments"].([]any)) == 0 && out["summary"] != nil
		}},
		{"get_issue paging implies comment pages", "get_issue", map[string]any{"repo": "o/r", "number": 1, "limit": 1}, func(out map[string]any) bool {
			page, ok := out["comment_page"].(map[string]any)
			return ok && len(page["status"].(map[string]any)) == 2
		}},
		{"empty enum value keeps the default", "search_issues", map[string]any{"query": "socket", "engine": "", "view": ""}, func(out map[string]any) bool { return out["matches"] != nil }},
		{"search status is compact", "search_issues", map[string]any{"query": "socket"}, func(out map[string]any) bool {
			status := out["status"].(map[string]any)
			return len(status) == 2 && status["generation"] != nil && status["collected_at"] != nil
		}},
		{"sync status stays complete", "get_sync_status", map[string]any{}, func(out map[string]any) bool { return out["repositories"] != nil && out["coverage"] != nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := connectMCP(t, fixture(t)).CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil || result.IsError {
				t.Fatal(result, err)
			}
			raw, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			if !tc.check(out) {
				t.Fatal(string(raw))
			}
		})
	}
	t.Run("invalid enum rejected", func(t *testing.T) {
		result, err := connectMCP(t, fixture(t)).CallTool(context.Background(), &mcp.CallToolParams{Name: "search_issues", Arguments: map[string]any{"query": "socket", "engine": "fuzzy"}})
		if err != nil || !result.IsError {
			t.Fatal("accepted an unknown engine", result, err)
		}
	})
}

func TestRelatedRoute(t *testing.T) {
	svc := fixture(t)
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/issues/o/r/1/related?limit=5", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, path := range []string{"/v1/issues/o/r/0/related", "/v1/issues/o/r/1/related?limit=500", "/v1/issues/o/r/1/related?include_labels=maybe"} {
		w := httptest.NewRecorder()
		svc.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 400 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
}
