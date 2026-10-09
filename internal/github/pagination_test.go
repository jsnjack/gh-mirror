package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gh-mirror/internal/checkpoint"
	"gh-mirror/internal/progress"
)

func TestNumberedPagination(t *testing.T) {
	for _, name := range []string{"parallel", "one worker", "no last", "cursor", "relative links", "conditional pages", "growing last", "foreign last", "broken chain", "budget"} {
		t.Run(name, func(t *testing.T) {
			var active, peak, calls atomic.Int32
			var base string
			barrier := make(chan struct{})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				page := 1
				if raw := r.URL.Query().Get("page"); raw != "" {
					var err error
					page, err = strconv.Atoi(raw)
					if err != nil {
						t.Error(err)
					}
				}
				if raw := r.URL.Query().Get("after"); raw != "" {
					var err error
					page, err = strconv.Atoi(raw)
					if err != nil {
						t.Error(err)
					}
				}
				if name == "conditional pages" {
					if r.Header.Get("If-None-Match") != "" {
						w.WriteHeader(http.StatusNotModified)
						return
					}
					w.Header().Set("ETag", fmt.Sprintf("page-%d", page))
				}
				if name == "parallel" && page >= 2 && page <= 5 {
					if n == 4 {
						close(barrier)
					}
					select {
					case <-barrier:
					case <-ctx.Done():
						return
					}
				}
				total := 7
				if name == "growing last" && page > 1 {
					total = 9
				}
				if page < total {
					link := fmt.Sprintf(`<%s/repositories/123/issues?sort=updated&per_page=100&page=%d>; rel="next"`, base, page+1)
					switch name {
					case "cursor":
						link = fmt.Sprintf(`<%s/repositories/123/issues?after=%d>; rel="next"`, base, page+1)
					case "no last":
					case "foreign last":
						link += `, <https://example.com/items?page=7>; rel="last"`
					default:
						link += fmt.Sprintf(`, <%s/repositories/123/issues?sort=updated&per_page=100&page=%d>; rel="last"`, base, total)
					}
					if name == "relative links" && page > 1 {
						link = fmt.Sprintf(`<?sort=updated&per_page=100&page=%d>; rel="next", <?sort=updated&per_page=100&page=%d>; rel="last"`, page+1, total)
					}
					if name == "broken chain" && page == 3 {
						link = ""
					}
					w.Header().Set("Link", link)
				}
				if err := json.NewEncoder(w).Encode([]map[string]int{{"id": page}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			base = server.URL
			workers, budget := 4, 20
			if name == "one worker" {
				workers = 1
			}
			if name == "budget" {
				budget = 3
			}
			client := New(base, base+"/graphql", "", budget, workers, &memoryCache{})
			completed, cached := 0, 0
			client.Progress = func(event progress.Event) {
				if event.Cached {
					cached++
				}
				if event.Page > 0 {
					if event.Page != completed+1 || event.Records != event.Page {
						t.Error("non-monotonic page progress", completed, event)
					}
					completed = event.Page
				}
			}
			out, err := client.List(ctx, "/repos/o/r/issues?sort=updated&per_page=100")
			if name == "foreign last" || name == "broken chain" || name == "budget" {
				if err == nil || out != nil {
					t.Fatal("invalid or incomplete inventory returned", out, err)
				}
				if name == "budget" && client.Requests() != budget {
					t.Fatal("request budget exceeded", client.Requests())
				}
				if name == "foreign last" && calls.Load() != 1 {
					t.Fatal("untrusted last link followed", calls.Load())
				}
				return
			}
			total := 7
			if name == "growing last" {
				total = 9
			}
			if err != nil || len(out) != total || int(calls.Load()) != total || completed != total {
				t.Fatal("wrong page count or extra requests", len(out), calls.Load(), completed, err)
			}
			for index, raw := range out {
				var item struct{ ID int }
				if err := json.Unmarshal(raw, &item); err != nil || item.ID != index+1 {
					t.Fatal("page order changed", index, item, err)
				}
			}
			if name == "parallel" && peak.Load() != 4 {
				t.Fatal("listing did not use four workers", peak.Load())
			}
			if peak.Load() > int32(workers) {
				t.Fatal("global worker limit exceeded", peak.Load())
			}
			if (name == "one worker" || name == "cursor" || name == "no last") && peak.Load() != 1 {
				t.Fatal("dependent pagination was parallelized", peak.Load())
			}
			if name == "conditional pages" {
				completed = 0
				out, err = client.List(ctx, "/repos/o/r/issues?sort=updated&per_page=100")
				if err != nil || len(out) != total || completed != total || cached != total || int(calls.Load()) != total*2 {
					t.Fatal("conditional responses lost parallel page links", len(out), completed, cached, calls.Load(), err)
				}
			}
		})
	}
}

func BenchmarkParallelPagination(b *testing.B) {
	const total = 40
	for _, workers := range []int{1, 4} {
		b.Run(fmt.Sprintf("workers_%d", workers), func(b *testing.B) {
			var base string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(10 * time.Millisecond)
				page := 1
				if raw := r.URL.Query().Get("page"); raw != "" {
					var err error
					page, err = strconv.Atoi(raw)
					if err != nil {
						b.Error(err)
					}
				}
				if page < total {
					w.Header().Set("Link", fmt.Sprintf(`<%s/items?page=%d>; rel="next", <%s/items?page=%d>; rel="last"`, base, page+1, base, total))
				}
				if _, err := w.Write([]byte(`[]`)); err != nil {
					b.Error(err)
				}
			}))
			defer server.Close()
			base = server.URL
			b.ResetTimer()
			for range b.N {
				client := New(base, base+"/graphql", "", total, workers, nil)
				if _, err := client.List(context.Background(), "/items"); err != nil {
					b.Fatal(err)
				}
				if client.Requests() != total {
					b.Fatal("unexpected request count", client.Requests())
				}
			}
			b.ReportMetric(total, "requests/op")
		})
	}
}

func TestConditionalPaginationGrowth(t *testing.T) {
	var growing atomic.Bool
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, total := 1, 3
		if growing.Load() {
			total = 5
		}
		if raw := r.URL.Query().Get("page"); raw != "" {
			var err error
			page, err = strconv.Atoi(raw)
			if err != nil {
				t.Error(err)
			}
		}
		if page < total {
			w.Header().Set("Link", fmt.Sprintf(`<%s/items?page=%d>; rel="next", <%s/items?page=%d>; rel="last"`, base, page+1, base, total))
		}
		w.Header().Set("ETag", fmt.Sprintf("page-%d", page))
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if err := json.NewEncoder(w).Encode([]map[string]int{{"id": page}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	base = server.URL
	cache := &memoryCache{}
	client := New(base, base+"/graphql", "", 8, 4, cache)
	if out, err := client.List(context.Background(), "/items"); err != nil || len(out) != 3 {
		t.Fatal(out, err)
	}
	growing.Store(true)
	if out, err := client.List(context.Background(), "/items"); err != nil || len(out) != 5 || client.Requests() != 8 {
		t.Fatal("304 page links hid new tail pages", len(out), client.Requests(), err)
	}
	var response cachedResponse
	if err := json.Unmarshal(cache.entries[base+"/items"].body, &response); err != nil || !strings.Contains(response.Link, "page=5") {
		t.Fatal("updated conditional links were not persisted", response.Link, err)
	}
}

func TestParallelPaginationResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var resuming atomic.Bool
	var mu sync.Mutex
	calls := map[int]int{}
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			var err error
			page, err = strconv.Atoi(raw)
			if err != nil {
				t.Error(err)
			}
		}
		mu.Lock()
		calls[page]++
		mu.Unlock()
		if !resuming.Load() && (page == 2 || page > 5) {
			<-r.Context().Done()
			return
		}
		if page < 7 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repositories/123/issues?sort=updated&per_page=100&page=%d>; rel="next", <%s/repositories/123/issues?sort=updated&per_page=100&page=7>; rel="last"`, base, page+1, base))
		}
		if err := json.NewEncoder(w).Encode([]map[string]int{{"id": page}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	base = server.URL
	path := filepath.Join(t.TempDir(), "pending.sqlite")
	pending, err := checkpoint.Open(ctx, path, "same-session", time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	client := New(base, base+"/graphql", "", 20, 4, nil)
	client.Checkpoint = pending
	client.Progress = func(event progress.Event) {
		if event.Page == 4 {
			cancel()
		}
	}
	_, err = client.List(ctx, "/repos/o/r/issues?sort=updated&per_page=100")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("sync did not stop on cancellation", err)
	}
	if err := pending.Close(); err != nil {
		t.Fatal(err)
	}
	resuming.Store(true)
	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resumeCancel()
	pending, err = checkpoint.Open(resumeCtx, path, "same-session", time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pending.Close(); err != nil {
			t.Error(err)
		}
	}()
	client = New(base, base+"/graphql", "", 3, 4, nil)
	client.Checkpoint = pending
	out, err := client.List(resumeCtx, "/repos/o/r/issues?sort=updated&per_page=100")
	if err != nil || len(out) != 7 || client.Requests() != 3 || client.Resumed() != 4 {
		t.Fatal("completed pages fetched again", len(out), client.Requests(), client.Resumed(), err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, page := range []int{1, 3, 4, 5} {
		if calls[page] != 1 {
			t.Fatal("out-of-order checkpoint lost", page, calls)
		}
	}
}
