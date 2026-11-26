package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestTransportRetries(t *testing.T) {
	// Each break ends one request the way a reset connection or a stream GitHub
	// cancels does: before any response, or partway through the body.
	breaks := map[string]func(http.ResponseWriter){
		"connection closed before the response": func(w http.ResponseWriter) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		},
		"body cut short": func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "100")
			if _, err := w.Write([]byte(`{"partial":`)); err != nil {
				t.Error(err)
			}
		},
	}
	for name, cut := range breaks {
		for _, tc := range []struct {
			failures int
			graph    bool
			waits    []time.Duration
			fails    bool
		}{
			{failures: 1, waits: []time.Duration{time.Second}},
			{failures: 2, graph: true, waits: []time.Duration{time.Second, 2 * time.Second}},
			{failures: 10, waits: []time.Duration{time.Second, 2 * time.Second}, fails: true},
		} {
			t.Run(fmt.Sprintf("%s/%d failures/graph %t", name, tc.failures, tc.graph), func(t *testing.T) {
				// Closing a connection gives the race detector no ordering, so count atomically.
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if int(calls.Add(1)) <= tc.failures {
						cut(w)
						return
					}
					if _, err := w.Write([]byte(`{"data":{"ok":true}}`)); err != nil {
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
				if tc.fails {
					if err == nil || !strings.Contains(err.Error(), "after 3 attempts") || calls.Load() != 3 {
						t.Fatalf("got %v after %d calls; want failure after 3 attempts", err, calls.Load())
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(*waits, tc.waits) || client.Requests() != int(calls.Load()) {
					t.Fatalf("waits %v and %d requests for %d calls", *waits, client.Requests(), calls.Load())
				}
			})
		}
	}
}

func TestTransportErrorsStopOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := New(server.URL, server.URL+"/graphql", "secret", 20, 1, nil)
	waits := fakeClock(client)
	if _, err := client.Get(ctx, "/items"); !errors.Is(err, context.Canceled) || calls.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("cancelled request retried: %v after %d calls and waits %v", err, calls.Load(), *waits)
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
