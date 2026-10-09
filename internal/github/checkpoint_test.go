package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gh-mirror/internal/checkpoint"
)

func TestSavedResponses(t *testing.T) {
	for _, name := range []string{"REST conditional", "GraphQL", "GraphQL partial errors"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body := `[]`
				switch name {
				case "REST conditional":
					if r.Header.Get("If-None-Match") == "etag" {
						w.WriteHeader(304)
						return
					}
					w.Header().Set("ETag", "etag")
				case "GraphQL":
					body = `{"data":{"nodes":[]}}`
				case "GraphQL partial errors":
					body = `{"data":{"nodes":[]},"errors":[{"message":"permission denied"}]}`
				}
				if _, err := w.Write([]byte(body)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "pending.sqlite")
			pending, err := checkpoint.Open(ctx, path, "test-session", time.Now(), false)
			if err != nil {
				t.Fatal(err)
			}
			fetch := func(client *Client) error {
				if name == "REST conditional" {
					_, err := client.Get(ctx, "/items")
					return err
				}
				var out map[string]json.RawMessage
				return client.GraphQL(ctx, "query($ids:[ID!]!){nodes(ids:$ids){id}}", map[string]any{"ids": []string{"I_1"}}, &out)
			}
			client := New(server.URL, server.URL+"/graphql", "", 1, 4, &memoryCache{})
			client.Checkpoint = pending
			err = fetch(client)
			if (err != nil) != (name == "GraphQL partial errors") {
				t.Fatal(err)
			}
			if err := pending.Close(); err != nil {
				t.Fatal(err)
			}
			pending, err = checkpoint.Open(ctx, path, "test-session", time.Now(), false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := pending.Close(); err != nil {
					t.Error(err)
				}
			}()
			cache := &memoryCache{}
			client = New(server.URL, server.URL+"/graphql", "", 1, 4, cache)
			client.Checkpoint = pending
			err = fetch(client)
			if name == "GraphQL partial errors" {
				if err == nil || pending.Saved != 0 || client.Resumed() != 0 || calls.Load() != 2 {
					t.Fatal("partial GraphQL response was saved", err, pending.Saved, calls.Load())
				}
				return
			}
			if err != nil || client.Requests() != 0 || client.Resumed() != 1 || calls.Load() != 1 {
				t.Fatal("saved response spent an API request", err, client.Requests(), client.Resumed(), calls.Load())
			}
			if name == "REST conditional" {
				client = New(server.URL, server.URL+"/graphql", "", 1, 4, cache)
				if err := fetch(client); err != nil {
					t.Fatal("resume lost conditional response", err)
				}
				if calls.Load() != 2 || client.Requests() != 1 {
					t.Fatal("conditional request not counted", calls.Load(), client.Requests())
				}
			}
		})
	}
}
