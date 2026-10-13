package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"gh-mirror/internal/commentpolicy"
	"gh-mirror/internal/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCommentPolicyParity(t *testing.T) {
	for _, name := range []string{"comments filtered", "comments override", "context filtered", "context override", "get page override"} {
		t.Run(name, func(t *testing.T) {
			svc := fixture(t)
			ctx := context.Background()
			if err := svc.Store.Update(ctx, func(w *store.Writer) error {
				if _, err := w.PutIssue(ctx, "o/r", json.RawMessage(`{"number":1,"node_id":"I_1","title":"socket implementation","body":"PR description","state":"open","pull_request":{},"updated_at":"2026-01-02T00:00:00Z","html_url":"https://github.com/o/r/pull/1"}`)); err != nil {
					return err
				}
				if err := w.PutComment(ctx, "o/r", 1, json.RawMessage(`{"id":1,"body":"AI review text","user":{"login":"Copilot","type":"Bot"},"html_url":"https://github.com/o/r/pull/1#discussion_r1","pull_request_url":"https://api.github.com/repos/o/r/pulls/1","updated_at":"2026-01-02T00:00:00Z"}`)); err != nil {
					return err
				}
				return w.ApplyCommentPolicies(ctx, map[string]commentpolicy.Policy{"o/r": {ExcludeKnownAI: true, ExcludeAIThreads: true}})
			}); err != nil {
				t.Fatal(err)
			}
			tool, path := "list_comments", "/v1/issues/o/r/1/comments"
			include := name == "comments override" || name == "context override" || name == "get page override"
			args := map[string]any{"repo": "o/r", "number": 1, "include_excluded": include}
			if name == "context filtered" || name == "context override" {
				tool, path = "get_issue_context", "/v1/issues/o/r/1/context"
			}
			if name == "get page override" {
				tool, path = "get_issue", "/v1/issues/o/r/1?comments=page"
				args["comments"] = "page"
			}
			separator := "?"
			if name == "get page override" {
				separator = "&"
			}
			if include {
				path += separator + "include_excluded=true"
			}
			w := httptest.NewRecorder()
			svc.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var rest map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &rest); err != nil {
				t.Fatal(err)
			}
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
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
			session, err := client.Connect(ctx, b, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
			if err != nil || result.IsError {
				t.Fatal(result, err)
			}
			raw, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var fromMCP map[string]any
			if err := json.Unmarshal(raw, &fromMCP); err != nil {
				t.Fatal(err)
			}
			want := 0
			if include {
				want = 1
			}
			for _, out := range []map[string]any{rest, fromMCP} {
				if name == "get page override" {
					out = out["comment_page"].(map[string]any)
				}
				if len(out["comments"].([]any)) != want {
					t.Fatal(out)
				}
			}
		})
	}
}
