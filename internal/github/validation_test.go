package github

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"gh-mirror/internal/checkpoint"
)

func TestGraphQLValidation(t *testing.T) {
	for _, name := range []string{"fresh invalid", "legacy invalid", "legacy corrupt"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			valid := name != "fresh invalid"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				node := "null"
				if valid {
					node = `{"id":"I_1"}`
				}
				if _, err := fmt.Fprintf(w, `{"data":{"node":%s}}`, node); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			pending, err := checkpoint.Open(ctx, filepath.Join(t.TempDir(), "pending.sqlite"), "test", time.Now(), false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := pending.Close(); err != nil {
					t.Error(err)
				}
			}()
			if err := pending.Save(ctx, "unrelated", []byte(`[]`)); err != nil {
				t.Fatal(err)
			}
			query := "query{node{id}}"
			payload, err := json.Marshal(map[string]any{"query": query, "variables": nil})
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("GraphQL:%x", sha256.Sum256(append([]byte(server.URL+"\x00"), payload...)))
			if name != "fresh invalid" {
				body := []byte(`{"node":null}`)
				if name == "legacy corrupt" {
					body = []byte(`{broken`)
				}
				if err := pending.Save(ctx, key, body); err != nil {
					t.Fatal(err)
				}
			}
			fetch := func() (*Client, error) {
				client := New(server.URL, server.URL, "", 1, 1, nil)
				client.Checkpoint = pending
				var out map[string]json.RawMessage
				err := client.GraphQLValidated(ctx, query, nil, &out, func(raw json.RawMessage) error {
					var data struct{ Node *struct{ ID string } }
					if err := json.Unmarshal(raw, &data); err != nil {
						return fmt.Errorf("decode node: %w", err)
					}
					if data.Node == nil || data.Node.ID != "I_1" {
						return fmt.Errorf("missing issue node")
					}
					return nil
				})
				return client, err
			}
			client, err := fetch()
			if name == "fresh invalid" {
				if err == nil {
					t.Fatal("accepted invalid observation")
				}
				if _, found, err := pending.Load(ctx, key); err != nil || found {
					t.Fatal("saved invalid observation", found, err)
				}
				valid = true
				client, err = fetch()
			}
			if err != nil || client.Requests() != 1 {
				t.Fatal("did not refetch invalid observation", client.Requests(), err)
			}
			if _, found, err := pending.Load(ctx, "unrelated"); err != nil || !found {
				t.Fatal("lost unrelated saved work", found, err)
			}
			client, err = fetch()
			if err != nil || client.Requests() != 0 {
				t.Fatal("failed to reuse valid observation", client.Requests(), err)
			}
		})
	}
}
