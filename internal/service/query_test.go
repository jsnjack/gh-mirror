package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
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
		{"list_comments", store.CommentOptions{Repo: "o/r", Number: 1}},
		{"get_issues", store.BatchOptions{Tickets: []store.TicketID{{Repo: "o/r", Number: 1}}}},
		{"list_catalog", store.CatalogOptions{Kind: "labels", Scope: "o/r", Limit: 1}},
		{"search_issues", store.SearchOptions{Query: "socket", Match: "all", PageOptions: store.PageOptions{Count: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := fixture(t)
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
				schema, err := json.Marshal(tool.OutputSchema)
				if err != nil || !strings.Contains(string(schema), "$defs") || !strings.Contains(string(schema), "Status") {
					t.Fatal(string(schema), err)
				}
			}
		})
	}
}
