package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testProvider(t *testing.T, handler http.HandlerFunc, batch int) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	p, err := New(context.Background(), Options{Backend: "lemonade-vulkan", BatchSize: batch, Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	p.start = func(context.Context, Options) (*vulkanRuntime, error) {
		return &vulkanRuntime{url: server.URL, client: localClient(), device: "Vulkan0: fixture"}, nil
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func writeVectors(t *testing.T, w http.ResponseWriter, model string, vs [][]float32) {
	t.Helper()
	data := make([]map[string]any, len(vs))
	for n, v := range vs {
		data[n] = map[string]any{"index": n, "embedding": v}
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"model": model, "data": data}); err != nil {
		t.Error(err)
	}
}

func TestProviderBatching(t *testing.T) {
	ctx := context.Background()
	texts := []string{"hello world", "café résumé naïve"}
	expected := make([][]float32, len(texts))
	ids := make([][]int64, len(texts))
	for n, text := range texts {
		var err error
		expected[n], err = Default.Embed(ctx, text)
		if err != nil {
			t.Fatal(err)
		}
		ids[n], err = Default.Tokens(text)
		if err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	batches := []int{}
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input [][]int64 `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		batches = append(batches, len(request.Input))
		mu.Unlock()
		vs := make([][]float32, len(request.Input))
		for n, in := range request.Input {
			matched := false
			for j, id := range ids {
				if equalIDs(in, id) {
					vs[n] = expected[j]
					matched = true
					break
				}
			}
			if !matched {
				t.Error("token IDs changed")
			}
		}
		writeVectors(t, w, runtimeModel, vs)
	}, 8)
	input := make([]string, 35)
	for n := range input {
		input[n] = texts[n%2]
	}
	vs, err := p.EmbedBatch(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	for n, v := range vs {
		if err := compatibleVector(v, expected[n%2]); err != nil {
			t.Fatal("batch ordering changed", n, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 5 || batches[0] != 8 || batches[4] != 3 {
		t.Fatal("batch bounds or tail flush", batches)
	}
	s := p.Status()
	if s.Backend != "lemonade-vulkan" || !s.Verified || s.Vectors != 35 || s.VectorsPerSecond <= 0 || p.ID() != Fingerprint || p.BatchSize() != 8 {
		t.Fatal("actual runtime status", s)
	}
	chunks, err := p.Chunks(strings.Repeat("software ", 400))
	if err != nil || len(chunks) < 2 {
		t.Fatal("shared chunking", chunks, err)
	}
}

func TestProviderSameModelFallback(t *testing.T) {
	for _, failure := range []string{"HTTP", "model", "dimension", "normalization", "indices"} {
		t.Run(failure, func(t *testing.T) {
			p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if failure == "HTTP" {
					w.WriteHeader(503)
					return
				}
				model := runtimeModel
				if failure == "model" {
					model = "another-model"
				}
				v := make([]float32, Dimension)
				v[0] = 1
				if failure == "dimension" {
					v = v[:1]
				}
				if failure == "normalization" {
					v[0] = 2
				}
				if failure == "indices" {
					if err := json.NewEncoder(w).Encode(map[string]any{"model": model, "data": []map[string]any{{"index": 1, "embedding": v}}}); err != nil {
						t.Error(err)
					}
					return
				}
				writeVectors(t, w, model, [][]float32{v})
			}, 8)
			v, err := p.Embed(context.Background(), "hello world")
			if err != nil {
				t.Fatal(err)
			}
			expected, err := Default.Embed(context.Background(), "hello world")
			if err != nil {
				t.Fatal(err)
			}
			if err := compatibleVector(v, expected); err != nil {
				t.Fatal("fallback changed embedding", err)
			}
			s := p.Status()
			if s.Backend != "cpu" || s.FallbackReason == "" || p.ID() != Fingerprint || p.BatchSize() != 1 || s.Vectors != 1 {
				t.Fatal("fallback was not explicit", s)
			}
		})
	}
}

func TestProviderStartupAndCancellation(t *testing.T) {
	for _, backend := range []string{"cpu", "lemonade-vulkan"} {
		t.Run(backend, func(t *testing.T) {
			p, err := New(context.Background(), Options{Backend: backend, Runtime: filepath.Join(t.TempDir(), "missing")})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := p.Close(); err != nil {
					t.Error(err)
				}
			}()
			var reports []State
			p.SetReporter(func(s State) { reports = append(reports, s) })
			if _, err := p.Embed(context.Background(), "hello world"); err != nil {
				t.Fatal(err)
			}
			if s := p.Status(); s.Backend != "cpu" || (backend != "cpu" && s.FallbackReason == "") || s.Vectors != 1 {
				t.Fatal(s)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := p.Embed(ctx, "hello world"); !errors.Is(err, context.Canceled) {
				t.Fatal("ignored cancellation", err)
			}
			p.SetReporter(nil)
			if len(reports) == 0 {
				t.Fatal("missing status updates")
			}
		})
	}
}

func TestProviderCloseReleasesQueuedCalls(t *testing.T) {
	entered := make(chan struct{})
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var request any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		close(entered)
		<-r.Context().Done()
	}, 8)
	done := make(chan error, 1)
	go func() { _, err := p.Embed(context.Background(), "hello world"); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request never started")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed provider succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queue did not stop")
	}
}

func TestEmbeddingOptions(t *testing.T) {
	for _, o := range []Options{{Backend: "remote"}, {BatchSize: 33}, {Workers: 17}, {Device: "CPU"}, {Device: "Vulkan../"}} {
		t.Run(fmt.Sprint(o), func(t *testing.T) {
			if _, err := New(context.Background(), o); err == nil {
				t.Fatal("invalid option accepted")
			}
		})
	}
}
