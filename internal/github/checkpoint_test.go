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

func TestNotModifiedCheckpoint(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("ETag", "v1")
		if r.Header.Get("If-None-Match") == "v1" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if _, err := w.Write([]byte(`[{"id":1}]`)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	cache := &memoryCache{}
	if _, err := New(server.URL, server.URL+"/graphql", "", 1, 4, cache).Get(ctx, "/items"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pending.sqlite")
	run := func(cache *memoryCache) *Client {
		t.Helper()
		pending, err := checkpoint.Open(ctx, path, "test-session", time.Now(), false)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := pending.Close(); err != nil {
				t.Error(err)
			}
		}()
		client := New(server.URL, server.URL+"/graphql", "", 4, 4, cache)
		client.Checkpoint = pending
		body, err := client.Get(ctx, "/items")
		if err != nil || string(body) != `[{"id":1}]` {
			t.Fatal("conditional response changed", string(body), err)
		}
		return client
	}
	writes := cache.writes
	run(cache)
	if cache.writes != writes || calls.Load() != 2 {
		t.Fatal("304 rewrote the unchanged cache entry", cache.writes-writes, calls.Load())
	}
	pending, err := checkpoint.Open(ctx, path, "test-session", time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	saved, found, err := pending.Load(ctx, "GET:"+server.URL+"/items")
	if closeErr := pending.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	var reference cachedResponse
	if err != nil || !found || json.Unmarshal(saved, &reference) != nil || !reference.NotModified || reference.ETag != "v1" || len(reference.Body) != 0 {
		t.Fatal("checkpoint copied the cached body instead of its ETag", string(saved), found, err)
	}
	if client := run(cache); client.Requests() != 0 || client.Resumed() != 1 || calls.Load() != 2 {
		t.Fatal("resume did not read the confirmed body from the cache", client.Requests(), client.Resumed(), calls.Load())
	}
	// A cache that no longer holds the confirmed version refetches instead of trusting the reference.
	changed := &memoryCache{}
	if err := changed.Cache(ctx, server.URL+"/items", "v0", []byte(`{"body":[{"id":0}],"link":"","etag":"v0"}`)); err != nil {
		t.Fatal(err)
	}
	if client := run(changed); client.Requests() != 1 || calls.Load() != 3 {
		t.Fatal("stale reference was not refetched", client.Requests(), calls.Load())
	}
}
