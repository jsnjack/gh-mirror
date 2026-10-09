package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type memoryCache struct {
	entries map[string]struct {
		etag string
		body []byte
	}
}

func (c *memoryCache) Cached(_ context.Context, key string) (string, []byte, error) {
	entry := c.entries[key]
	return entry.etag, entry.body, nil
}
func (c *memoryCache) Cache(_ context.Context, key, etag string, body []byte) error {
	if c.entries == nil {
		c.entries = map[string]struct {
			etag string
			body []byte
		}{}
	}
	c.entries[key] = struct {
		etag string
		body []byte
	}{etag, body}
	return nil
}
func TestClient(t *testing.T) {
	for _, name := range []string{"pagination", "canonical repository links", "conditional next links", "foreign links", "cyclic links", "redirect", "budget", "GraphQL partial errors", "GraphQL null", "GraphQL mutation", "rate wait bounded", "cancellation", "delta cache omitted"} {
		t.Run(name, func(t *testing.T) {
			cache := &memoryCache{}
			calls := 0
			var base string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("missing credentials")
				}
				switch name {
				case "canonical repository links":
					if r.URL.Path == "/items" {
						w.Header().Set("Link", fmt.Sprintf(`<%s/repositories/123/issues?page=2>; rel="next"`, base))
					}
				case "delta cache omitted":
					w.Header().Set("ETag", "etag")
				case "pagination":
					if r.URL.Query().Get("page") == "" {
						w.Header().Set("Link", fmt.Sprintf(`<%s/items?page=2>; rel="next"`, base))
					}
				case "conditional next links":
					if r.Header.Get("If-None-Match") == "etag" {
						w.WriteHeader(304)
						return
					}
					w.Header().Set("ETag", "etag")
					if r.URL.Query().Get("page") == "" {
						w.Header().Set("Link", fmt.Sprintf(`<%s/items?page=2>; rel="next"`, base))
					}
				case "foreign links":
					w.Header().Set("Link", `<https://example.com/items>; rel="next"`)
				case "cyclic links":
					w.Header().Set("Link", fmt.Sprintf(`<%s/items>; rel="next"`, base))
				case "redirect":
					w.Header().Set("Location", "https://example.com")
					w.WriteHeader(302)
					return
				case "GraphQL partial errors":
					if _, err := w.Write([]byte(`{"data":{"nodes":[]},"errors":[{"message":"secret body"}]}`)); err != nil {
						t.Error(err)
					}
					return
				case "GraphQL null":
					if _, err := w.Write([]byte(`{"data":null}`)); err != nil {
						t.Error(err)
					}
					return
				case "rate wait bounded":
					w.Header().Set("Retry-After", "3600")
					w.WriteHeader(429)
					return
				}
				if _, err := w.Write([]byte(`[{"id":1}]`)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			base = server.URL
			client := New(base, base+"/graphql", "secret", 10, cache)
			switch name {
			case "delta cache omitted":
				for range 2 {
					if _, err := client.Get(context.Background(), "/items?since=2026-01-01T00:00:00Z"); err != nil {
						t.Fatal(err)
					}
				}
				if len(cache.entries) != 0 || client.Requests() != 2 {
					t.Fatal("delta response cache grows without reuse")
				}
			case "GraphQL partial errors", "GraphQL null", "GraphQL mutation":
				query := "query{viewer{login}}"
				if name == "GraphQL mutation" {
					query = "mutation{deleteIssue{id}}"
				}
				var out any
				if err := client.GraphQL(context.Background(), query, nil, &out); err == nil || strings.Contains(err.Error(), "secret body") {
					t.Fatal("GraphQL failure ignored or payload leaked", err)
				}
				if name == "GraphQL mutation" && calls != 0 {
					t.Fatal("mutation sent upstream")
				}
			case "conditional next links":
				if _, err := client.List(context.Background(), "/items"); err != nil {
					t.Fatal(err)
				}
				out, err := client.List(context.Background(), "/items")
				if err != nil || len(out) != 2 || calls != 4 {
					t.Fatal("conditional response lost its next link", out, err, calls)
				}
			default:
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if name == "budget" {
					client.remaining = 0
				}
				if name == "cancellation" {
					cancel()
				}
				out, err := client.List(ctx, "/items")
				if name == "pagination" || name == "canonical repository links" {
					if err != nil || len(out) != 2 || client.Requests() != 2 {
						t.Fatal(out, err, client.Requests())
					}
				} else if err == nil {
					t.Fatal("invalid listing accepted", name)
				}
			}
		})
	}
}
