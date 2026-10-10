package embedding

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const runtimeModel = "gh-mirror-minilm"

type runtimeLog struct {
	mu   sync.Mutex
	data []byte
}

func (l *runtimeLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.data = append(l.data, b...)
	if len(l.data) > 65536 {
		l.data = l.data[len(l.data)-65536:]
	}
	return len(b), nil
}
func (l *runtimeLog) text() string { l.mu.Lock(); defer l.mu.Unlock(); return string(l.data) }

type vulkanRuntime struct {
	url, key, device string
	client           *http.Client
	stop             context.CancelFunc
	done             chan struct{}
	once             sync.Once
}

func startVulkan(ctx context.Context, o Options) (*vulkanRuntime, error) {
	binaryPath := o.Runtime
	if binaryPath == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("locate Lemonade runtime: %w", err)
		}
		name := "llama-server"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binaryPath = filepath.Join(cache, "lemonade", "bin", "llamacpp", "vulkan", name)
	}
	if !filepath.IsAbs(binaryPath) {
		return nil, fmt.Errorf("lemonade runtime path must be absolute")
	}
	if _, err := os.Stat(binaryPath); err != nil {
		return nil, fmt.Errorf("lemonade Vulkan runtime unavailable; install with lemonade backends install llamacpp:vulkan: %w", err)
	}
	startup, finish := context.WithTimeout(ctx, 45*time.Second)
	defer finish()
	list := exec.CommandContext(startup, binaryPath, "--list-devices")
	list.Env = vulkanEnvironment()
	devices, err := list.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("enumerate Lemonade Vulkan devices: %w", err)
	}
	if !strings.Contains(string(devices), o.Device+":") {
		return nil, fmt.Errorf("lemonade Vulkan device %s is unavailable", o.Device)
	}
	model, err := GGUF()
	if err != nil {
		return nil, fmt.Errorf("prepare exact MiniLM GGUF: %w", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("reserve local Vulkan port: %w", err)
	}
	addr := l.Addr().(*net.TCPAddr)
	port := strconv.Itoa(addr.Port)
	if err := l.Close(); err != nil {
		return nil, fmt.Errorf("release local Vulkan port: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("create local inference key: %w", err)
	}
	child, cancel := context.WithCancel(ctx)
	tokens := strconv.Itoa(o.BatchSize * MaxTokens)
	args := []string{"--model", model, "--host", "127.0.0.1", "--port", port, "--embedding", "--pooling", "mean", "--embd-normalize", "2", "--gpu-layers", "99", "--device", o.Device, "--batch-size", tokens, "--ubatch-size", tokens, "--ctx-size", tokens, "--parallel", strconv.Itoa(o.BatchSize), "--threads", "1", "--offline", "--alias", runtimeModel, "--flash-attn", "off", "--log-verbosity", "4", "--api-key", hex.EncodeToString(key)}
	command := exec.CommandContext(child, binaryPath, args...)
	command.Env = vulkanEnvironment()
	logs := &runtimeLog{}
	command.Stdout, command.Stderr = logs, logs
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start Lemonade Vulkan runtime: %w", err)
	}
	r := &vulkanRuntime{url: "http://127.0.0.1:" + port, key: hex.EncodeToString(key), stop: cancel, done: make(chan struct{}), client: localClient()}
	go func() {
		if err := command.Wait(); err != nil && child.Err() == nil {
			slog.Debug("Lemonade Vulkan process exited", "error", err)
		}
		close(r.done)
	}()
	if o.notify != nil {
		o.notify(r.url)
		slog.Debug("Listening on " + r.url + " (local embedding runtime)")
	} else {
		slog.Warn("Listening on " + r.url + " (local embedding runtime)")
	}
	failure := func(err error) (*vulkanRuntime, error) { r.close(); return nil, err }
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-startup.Done():
			return failure(fmt.Errorf("initialize Lemonade Vulkan runtime: %w", startup.Err()))
		case <-r.done:
			return failure(fmt.Errorf("lemonade Vulkan runtime exited during model loading"))
		case <-ticker.C:
			request, err := http.NewRequestWithContext(startup, http.MethodGet, r.url+"/health", nil)
			if err != nil {
				return failure(fmt.Errorf("prepare runtime health request: %w", err))
			}
			request.Header.Set("Authorization", "Bearer "+r.key)
			response, err := r.client.Do(request)
			if err != nil {
				continue
			}
			ready := response.StatusCode == http.StatusOK
			if err := response.Body.Close(); err != nil {
				slog.Debug("close runtime health response", "error", err)
			}
			if !ready {
				continue
			}
			device, err := loadedVulkan(logs.text(), o.Device)
			if err != nil {
				return failure(err)
			}
			r.device = device
			if err := r.verify(startup); err != nil {
				return failure(err)
			}
			return r, nil
		}
	}
}

func vulkanEnvironment() []string {
	vars := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, "LLAMA_") && !strings.HasPrefix(name, "GGML_") {
			vars = append(vars, entry)
		}
	}
	return append(vars, "GGML_VK_DISABLE_F16=1")
}
func localClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: time.Minute}, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("local inference redirects are disabled")
	}}
}
func (r *vulkanRuntime) close() {
	r.once.Do(func() {
		if r.stop != nil {
			r.stop()
		}
		if r.done != nil {
			<-r.done
		}
		if r.client != nil {
			r.client.CloseIdleConnections()
		}
	})
}

func loadedVulkan(logs, device string) (string, error) {
	location := regexp.MustCompile(`using device (Vulkan[0-9]+) \((.*)\) \(`).FindStringSubmatch(logs)
	counts := regexp.MustCompile(`offloaded ([0-9]+)/([0-9]+) layers to GPU`).FindStringSubmatch(logs)
	if len(location) != 3 || location[1] != device || len(counts) != 3 || counts[1] != counts[2] || counts[1] == "0" {
		return "", fmt.Errorf("lemonade runtime did not confirm complete Vulkan layer offload on %s", device)
	}
	return location[1] + ": " + location[2], nil
}

func (r *vulkanRuntime) embed(ctx context.Context, ids [][]int64) ([][]float32, error) {
	raw, err := json.Marshal(struct {
		Model  string    `json:"model"`
		Input  [][]int64 `json:"input"`
		Format string    `json:"encoding_format"`
	}{runtimeModel, ids, "float"})
	if err != nil {
		return nil, fmt.Errorf("encode local embedding request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url+"/v1/embeddings", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("prepare local embedding request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.key)
	response, err := r.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("lemonade Vulkan inference: %w", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			slog.Debug("close embedding response", "error", err)
		}
	}()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lemonade Vulkan inference returned HTTP %d", response.StatusCode)
	}
	var out struct {
		Model string `json:"model"`
		Data  []struct {
			Index  int       `json:"index"`
			Vector []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode Vulkan vectors: %w", err)
	}
	if out.Model != runtimeModel || len(out.Data) != len(ids) {
		return nil, fmt.Errorf("vulkan response changed model identity or batch length")
	}
	vectors := make([][]float32, len(ids))
	for _, item := range out.Data {
		if item.Index < 0 || item.Index >= len(ids) || vectors[item.Index] != nil || len(item.Vector) != Dimension {
			return nil, fmt.Errorf("invalid Vulkan vector index or dimension")
		}
		var norm float64
		for _, v := range item.Vector {
			norm += float64(v) * float64(v)
		}
		if math.IsNaN(norm) || math.IsInf(norm, 0) || math.Abs(norm-1) > 1e-4 {
			return nil, fmt.Errorf("vulkan returned an invalid or unnormalized vector")
		}
		vectors[item.Index] = item.Vector
	}
	return vectors, nil
}

func (r *vulkanRuntime) verify(ctx context.Context) error {
	raw, err := references.ReadFile("testdata/minilm-golden.json")
	if err != nil {
		return fmt.Errorf("read compatibility references: %w", err)
	}
	var golden struct {
		Cases []struct {
			Text   string    `json:"text"`
			IDs    []int64   `json:"input_ids"`
			Vector []float32 `json:"embedding"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		return fmt.Errorf("decode compatibility references: %w", err)
	}
	ids := make([][]int64, len(golden.Cases))
	for n, c := range golden.Cases {
		current, err := Default.Tokens(c.Text)
		if err != nil {
			return err
		}
		if !equalIDs(current, c.IDs) {
			return fmt.Errorf("bundled tokenizer differs from reference case %d", n)
		}
		ids[n] = current
	}
	// A full-length case also validates slot context limits and mean pooling at 256 tokens.
	long := strings.Repeat("software ", MaxTokens-2)
	longIDs, err := Default.Tokens(long)
	if err != nil {
		return err
	}
	longVector, err := Default.Embed(ctx, long)
	if err != nil {
		return err
	}
	ids = append(ids, longIDs)
	for start := 0; start < len(ids); start += 8 {
		end := min(start+8, len(ids))
		vectors, err := r.embed(ctx, ids[start:end])
		if err != nil {
			return fmt.Errorf("validate Vulkan compatibility: %w", err)
		}
		for n, v := range vectors {
			ref := longVector
			if start+n < len(golden.Cases) {
				ref = golden.Cases[start+n].Vector
			}
			if err := compatibleVector(v, ref); err != nil {
				return fmt.Errorf("vulkan model/tokenizer/pooling compatibility case %d: %w", start+n, err)
			}
		}
	}
	return nil
}
func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for n := range a {
		if a[n] != b[n] {
			return false
		}
	}
	return true
}
func compatibleVector(v, ref []float32) error {
	if len(v) != len(ref) || len(v) != Dimension {
		return fmt.Errorf("incompatible vector dimension")
	}
	var dot, a, b, diff float64
	for n, x := range v {
		y := ref[n]
		dot += float64(x) * float64(y)
		a += float64(x) * float64(x)
		b += float64(y) * float64(y)
		diff = max(diff, math.Abs(float64(x)-float64(y)))
	}
	cosine := dot / math.Sqrt(a*b)
	if math.IsNaN(cosine) || cosine < 0.9999 || diff > 0.001 {
		return fmt.Errorf("cosine %.8f, maximum coordinate difference %.6f exceed compatibility tolerance", cosine, diff)
	}
	return nil
}
