package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gh-mirror/internal/progress"
)

func TestSharedWorkers(t *testing.T) {
	for _, name := range []string{"concurrency", "budget", "queued cancellation"} {
		t.Run(name, func(t *testing.T) {
			var calls, active, peak atomic.Int32
			entered := make(chan struct{}, 16)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				n := active.Add(1)
				defer active.Add(-1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				if _, err := w.Write([]byte(`[]`)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			defer unblock()
			budget, workers, count := 20, 3, 10
			if name == "budget" {
				budget = 2
			}
			if name == "queued cancellation" {
				workers = 1
			}
			client := New(server.URL, server.URL+"/graphql", "", budget, workers, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			results := make(chan error, count)
			for index := range count {
				go func() { _, err := client.Get(ctx, fmt.Sprint("/items/", index)); results <- err }()
			}
			for range min(budget, workers) {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("workers did not run concurrently")
				}
			}
			if name == "queued cancellation" {
				cancel()
			}
			unblock()
			errorsSeen := 0
			for range count {
				select {
				case err := <-results:
					if err != nil {
						errorsSeen++
					}
				case <-time.After(5 * time.Second):
					t.Fatal("workers did not stop")
				}
			}
			switch name {
			case "concurrency":
				if calls.Load() != int32(count) || peak.Load() != int32(workers) || errorsSeen != 0 {
					t.Fatal("wrong concurrency", calls.Load(), peak.Load(), errorsSeen)
				}
			case "budget":
				if calls.Load() != int32(budget) || client.Requests() != budget || errorsSeen != count-budget {
					t.Fatal("budget overshot", calls.Load(), client.Requests(), errorsSeen)
				}
			case "queued cancellation":
				if client.Requests() != 1 || calls.Load() != 1 || errorsSeen < count-1 {
					t.Fatal("cancelled queue spent requests", calls.Load(), client.Requests(), errorsSeen)
				}
			}
		})
	}
}

func TestCoordinatedWait(t *testing.T) {
	for _, name := range []string{"secondary", "primary", "primary success"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if name == "secondary" {
					w.Header().Set("Retry-After", "30")
					w.WriteHeader(429)
					return
				}
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(20*time.Second).Unix()))
				if name == "primary" {
					w.WriteHeader(403)
					return
				}
				if _, err := w.Write([]byte(`[]`)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client := New(server.URL, server.URL+"/graphql", "", 10, 3, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waiting := make(chan struct{})
			var once sync.Once
			client.Progress = func(event progress.Event) {
				if event.Wait > 0 {
					once.Do(func() { close(waiting) })
				}
			}
			done := make(chan error, 2)
			go func() { _, err := client.Get(ctx, "/first"); done <- err }()
			select {
			case <-waiting:
			case <-time.After(5 * time.Second):
				t.Fatal("rate limit did not pause workers")
			}
			go func() { _, err := client.Get(ctx, "/second"); done <- err }()
			if name == "primary success" {
				if err := <-done; err != nil {
					t.Fatal("discarded final allowed response", err)
				}
			}
			select {
			case err := <-done:
				t.Fatal("request escaped shared pause", err)
			case <-time.After(30 * time.Millisecond):
			}
			cancel()
			remaining := 2
			if name == "primary success" {
				remaining = 1
			}
			for range remaining {
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatal("wait ignored cancellation", err)
				}
			}
			if calls.Load() != 1 || client.Requests() != 1 {
				t.Fatal("paused workers sent requests", calls.Load(), client.Requests())
			}
		})
	}
}
