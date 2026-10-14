package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fakeClock(c *Client) *[]time.Duration {
	clock := time.Unix(1_800_000_000, 0)
	waits := []time.Duration{}
	c.now = func() time.Time { return clock }
	c.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		clock = clock.Add(d)
		return nil
	}
	return &waits
}

func TestRateLimitRetries(t *testing.T) {
	reset := strconv.FormatInt(time.Unix(1_800_000_000, 0).Add(10*time.Minute).Unix(), 10)
	exhausted := map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}
	for _, tc := range []struct {
		name     string
		failures int
		status   int
		headers  map[string]string
		body     string
		graph    bool
		waits    []time.Duration
		fails    bool
	}{
		{"secondary 403 without Retry-After", 1, http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "4000"}, `{"message":"You have exceeded a secondary rate limit"}`, false, []time.Duration{time.Minute}, false},
		{"secondary limit backs off and gives up", 10, http.StatusTooManyRequests, nil, "", false, []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}, true},
		{"primary limit waits for reset", 1, http.StatusForbidden, exhausted, "", false, []time.Duration{10*time.Minute + time.Second}, false},
		{"plain 403 is not retried", 1, http.StatusForbidden, nil, `{"message":"Resource not accessible"}`, false, []time.Duration{}, true},
		{"GraphQL RATE_LIMITED", 1, http.StatusOK, exhausted, `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`, true, []time.Duration{10*time.Minute + time.Second}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body := `{"data":{"ok":true}}`
				if calls <= tc.failures {
					for key, value := range tc.headers {
						w.Header().Set(key, value)
					}
					w.WriteHeader(tc.status)
					body = tc.body
				}
				if _, err := w.Write([]byte(body)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client := New(server.URL, server.URL+"/graphql", "secret", 20, 1, nil)
			waits := fakeClock(client)
			var err error
			if tc.graph {
				var out map[string]any
				err = client.GraphQL(context.Background(), "query{ok}", nil, &out)
			} else {
				_, err = client.Get(context.Background(), "/items")
			}
			if (err != nil) != tc.fails {
				t.Fatal("unexpected outcome", err)
			}
			if !slices.Equal(*waits, tc.waits) {
				t.Fatal("unexpected retry waits", *waits)
			}
		})
	}
}

func TestGraphQLRateLimitMessage(t *testing.T) {
	t.Run("exhausted retries keep the upstream message", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation"}]}`)); err != nil {
				t.Error(err)
			}
		}))
		defer server.Close()
		client := New(server.URL, server.URL+"/graphql", "secret", 20, 1, nil)
		fakeClock(client)
		var out any
		err := client.GraphQL(context.Background(), "query{ok}", nil, &out)
		if err == nil || !strings.Contains(err.Error(), "API rate limit exceeded for installation") || strings.Contains(err.Error(), "secret") {
			t.Fatal("rate limit message lost or credential leaked", err)
		}
	})
}

func TestLookup(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone, http.StatusMovedPermanently} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/gone" {
					w.Header().Set("Location", "https://example.com/elsewhere")
					w.WriteHeader(code)
					return
				}
				if _, err := w.Write([]byte(`{"id":1}`)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client := New(server.URL, server.URL+"/graphql", "secret", 10, 1, nil)
			if _, found, err := client.Lookup(context.Background(), "/gone"); err != nil || found {
				t.Fatal("absent resource not reported", found, err)
			}
			if _, found, err := client.Lookup(context.Background(), "/present"); err != nil || !found {
				t.Fatal("absence made the client fatal", found, err)
			}
			_, err := client.Get(context.Background(), "/gone")
			var status *StatusError
			if !errors.As(err, &status) || status.Code != code {
				t.Fatal("Get must still fail for absent resources", err)
			}
		})
	}
}
