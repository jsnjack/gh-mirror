package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gh-mirror/internal/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func fixture(t *testing.T) *Service {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	err = db.Update(context.Background(), func(w *store.Writer) error {
		for _, n := range []int{1, 2} {
			raw := json.RawMessage(fmt.Sprintf(`{"number":%d,"node_id":"I_%d","title":"network crash","body":"socket timeout","state":"closed","updated_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/%d","labels":[{"name":"client:Acme"}]}`, n, n, n))
			if _, err := w.PutIssue(context.Background(), "o/r", raw); err != nil {
				return fmt.Errorf("seed ticket: %w", err)
			}
		}
		for key, value := range map[string]string{"generation": "testgeneration", "collected_at": "2026-01-01T00:00:00Z", "repositories": `["o/r"]`} {
			if err := w.SetMetadata(context.Background(), key, value); err != nil {
				return fmt.Errorf("seed metadata: %w", err)
			}
		}
		for kind, scope := range map[string]string{"labels": "o/r", "projects": "o"} {
			if err := w.ReplaceCatalog(context.Background(), kind, scope, []json.RawMessage{json.RawMessage(`{"id":1,"number":1,"name":"sample"}`)}); err != nil {
				return fmt.Errorf("seed catalog: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Store: db, Version: "test"}
}
func TestRESTMCPParity(t *testing.T) {
	cases := []struct {
		name, path, tool string
		args             map[string]any
	}{{"search", "/v1/search?q=acme", "search_issues", map[string]any{"query": "acme"}}, {"get", "/v1/issues/o/r/1", "get_issue", map[string]any{"repo": "o/r", "number": 1}}, {"catalog", "/v1/catalog?kind=labels&scope=o/r", "get_catalog", map[string]any{"kind": "labels", "scope": "o/r"}}, {"project", "/v1/projects?owner=o&number=1", "get_project", map[string]any{"owner": "o", "number": 1}}, {"candidates", "/v1/candidates?repo=o/r&number=1", "find_duplicate_candidates", map[string]any{"repo": "o/r", "number": 1}}, {"status", "/v1/status", "get_sync_status", map[string]any{}}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := fixture(t)
			ctx := context.Background()
			a, b := mcp.NewInMemoryTransports()
			serverSession, err := svc.MCP().Connect(ctx, a, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := serverSession.Close(); err != nil {
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
			tools, err := session.ListTools(ctx, nil)
			if err != nil || len(tools.Tools) != 6 {
				t.Fatal("MCP tools", tools, err)
			}
			for _, tool := range tools.Tools {
				if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
					t.Fatal("missing read-only hint", tool)
				}
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil || result.IsError {
				t.Fatal("MCP call", result, err)
			}
			encoded, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var mcpOut any
			if err := json.Unmarshal(encoded, &mcpOut); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			response := httptest.NewRecorder()
			svc.Handler().ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			var restOut any
			if err := json.Unmarshal(response.Body.Bytes(), &restOut); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(mcpOut, restOut) {
				t.Fatalf("REST and MCP differ:\n%s\n%s", encoded, response.Body.String())
			}
			if tc.name == "search" && len(restOut.(map[string]any)["matches"].([]any)) != 2 {
				t.Fatal("REST and MCP omitted label-only matches", restOut)
			}
		})
	}
}
func TestHTTPTransportAndAccess(t *testing.T) {
	for _, name := range []string{"unauthenticated", "authenticated", "missing issue", "invalid limit", "snapshot traversal", "HTTP MCP"} {
		t.Run(name, func(t *testing.T) {
			svc := fixture(t)
			svc.Token = "secret"
			server := httptest.NewServer(svc.Handler())
			defer server.Close()
			if name == "HTTP MCP" {
				client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
				transport := &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{base: http.DefaultTransport}}}
				session, err := client.Connect(context.Background(), transport, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := session.Close(); err != nil {
						t.Error(err)
					}
				}()
				result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_sync_status", Arguments: map[string]any{}})
				if err != nil || result.IsError {
					t.Fatal(result, err)
				}
				return
			}
			path := "/v1/status"
			want := 200
			switch name {
			case "unauthenticated":
				want = 401
			case "missing issue":
				path = "/v1/issues/o/r/99"
				want = 404
			case "invalid limit":
				path = "/v1/search?q=network&limit=1000"
				want = 400
			case "snapshot traversal":
				path = "/snapshots/mirror-test.sqlite"
				want = 404
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if name != "unauthenticated" {
				req.Header.Set("Authorization", "Bearer secret")
			}
			response := httptest.NewRecorder()
			svc.Handler().ServeHTTP(response, req)
			if response.Code != want {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
}

type bearerTransport struct{ base http.RoundTripper }

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.Header.Set("Authorization", "Bearer secret")
	response, err := t.base.RoundTrip(copy)
	if err != nil {
		return nil, fmt.Errorf("test transport: %w", err)
	}
	return response, nil
}
func TestListener(t *testing.T) {
	for _, tc := range []struct {
		address, token string
		valid          bool
	}{{"127.0.0.1:8787", "", true}, {"[::1]:8787", "", true}, {"0.0.0.0:8787", "", false}, {":8787", "secret", true}, {"localhost:8787", "", false}, {"bad", "secret", false}} {
		t.Run(tc.address, func(t *testing.T) {
			if err := ValidateListen(tc.address, tc.token); (err == nil) != tc.valid {
				t.Fatal(err)
			}
		})
	}
	t.Run("address log", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		defer server.Close()
		if !strings.HasPrefix(Address(server.Listener), "Listening on ") {
			t.Fatal(Address(server.Listener))
		}
	})
}
