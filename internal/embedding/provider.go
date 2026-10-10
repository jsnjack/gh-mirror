package embedding

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Options selects an optional installed runtime; CPU remains the standalone default.
type Options struct {
	Backend   string `json:"backend,omitempty"`
	Runtime   string `json:"runtime,omitempty"`
	Device    string `json:"device,omitempty"`
	BatchSize int    `json:"batch_size,omitempty"`
	Workers   int    `json:"-"`
	notify    func(string)
}

// Validate rejects invalid backend and batching settings before starting inference.
func (o Options) Validate() error {
	if o.Runtime != "" && !filepath.IsAbs(o.Runtime) {
		return fmt.Errorf("embedding runtime must be an absolute path")
	}
	if o.Backend != "" && o.Backend != "cpu" && o.Backend != "lemonade-vulkan" {
		return fmt.Errorf("embedding backend must be cpu or lemonade-vulkan")
	}
	if o.BatchSize < 0 || o.BatchSize > 32 {
		return fmt.Errorf("embedding batch_size must be between 1 and 32, or omitted")
	}
	if o.Workers < 0 || o.Workers > 16 {
		return fmt.Errorf("embedding CPU workers must be between 1 and 16, or omitted")
	}
	if o.Device != "" {
		if !strings.HasPrefix(o.Device, "Vulkan") || len(o.Device) == 6 {
			return fmt.Errorf("embedding device must be Vulkan followed by its device number")
		}
		for _, r := range o.Device[6:] {
			if r < '0' || r > '9' {
				return fmt.Errorf("embedding device must be Vulkan followed by its device number")
			}
		}
	}
	return nil
}

// State describes the effective runtime and completed inference in this process.
type State struct {
	Backend          string  `json:"backend"`
	Device           string  `json:"device"`
	FallbackReason   string  `json:"fallback_reason,omitempty"`
	Verified         bool    `json:"verified"`
	BatchSize        int     `json:"batch_size"`
	LastBatch        int     `json:"last_batch,omitempty"`
	Vectors          int     `json:"vectors"`
	VectorsPerSecond float64 `json:"vectors_per_second"`
	Listener         string  `json:"listener,omitempty"`
}

type encodeRequest struct {
	ctx   context.Context
	ids   []int64
	text  string
	reply chan encodeReply
}
type encodeReply struct {
	vector []float32
	err    error
}

// Provider preserves the bundled model contract while optionally batching on Vulkan.
type Provider struct {
	once       sync.Once
	opts       Options
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	state      State
	started    time.Time
	baseline   int
	report     func(State)
	reportMu   sync.Mutex
	gpu        *vulkanRuntime
	jobs       chan encodeRequest
	stopped    chan struct{}
	start      func(context.Context, Options) (*vulkanRuntime, error)
	cpuPermits chan struct{}
}

// New constructs a lazy provider without opening a service or contacting a network.
func New(ctx context.Context, o Options) (*Provider, error) {
	if err := o.Validate(); err != nil {
		return nil, fmt.Errorf("configure embeddings: %w", err)
	}
	if o.BatchSize == 0 {
		o.BatchSize = 8
	}
	if o.Workers == 0 {
		o.Workers = 4
	}
	if o.Device == "" {
		o.Device = "Vulkan0"
	}
	child, cancel := context.WithCancel(ctx)
	return &Provider{opts: o, ctx: child, cancel: cancel, state: State{Backend: "cpu", Device: "CPU (pure Go / " + runtime.GOARCH + ")", Verified: true, BatchSize: 1}, start: startVulkan, cpuPermits: make(chan struct{}, o.Workers)}, nil
}

// SetReporter attaches serialized runtime updates without exposing input text.
func (p *Provider) SetReporter(report func(State)) { p.mu.Lock(); p.report = report; p.mu.Unlock() }

// Status returns the actual backend and process-local throughput.
func (p *Provider) Status() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.state
	if !p.started.IsZero() {
		s.VectorsPerSecond = float64(s.Vectors-p.baseline) / time.Since(p.started).Seconds()
	}
	return s
}
func (p *Provider) notify() {
	p.reportMu.Lock()
	defer p.reportMu.Unlock()
	p.mu.Lock()
	r := p.report
	p.mu.Unlock()
	if r != nil {
		r(p.Status())
	}
}
func (p *Provider) initialize() {
	p.once.Do(func() {
		if p.opts.Backend == "" || p.opts.Backend == "cpu" || p.ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		p.state.Backend = "starting Vulkan"
		p.state.Device = p.opts.Device + " (checking)"
		p.state.Verified = false
		p.mu.Unlock()
		p.notify()
		opts := p.opts
		p.mu.Lock()
		reporting := p.report != nil
		p.mu.Unlock()
		if reporting {
			opts.notify = func(address string) { p.mu.Lock(); p.state.Listener = address; p.mu.Unlock(); p.notify() }
		}
		gpu, err := p.start(p.ctx, opts)
		if err != nil {
			if p.ctx.Err() == nil {
				p.fallback(err)
			}
			return
		}
		p.gpu = gpu
		p.mu.Lock()
		p.state.Backend = "lemonade-vulkan"
		p.state.Device = gpu.device
		p.state.Verified = true
		p.state.BatchSize = p.opts.BatchSize
		p.mu.Unlock()
		p.jobs = make(chan encodeRequest, p.opts.BatchSize*2)
		p.stopped = make(chan struct{})
		go p.pump()
		p.notify()
	})
}
func (p *Provider) fallback(err error) {
	p.mu.Lock()
	p.state.Backend = "cpu"
	p.state.Device = "CPU (pure Go / " + runtime.GOARCH + ")"
	p.state.Verified = true
	p.state.FallbackReason = err.Error()
	p.state.BatchSize = 1
	p.started = time.Now()
	p.baseline = p.state.Vectors
	p.mu.Unlock()
	p.mu.Lock()
	reporting := p.report != nil
	p.state.Listener = ""
	p.mu.Unlock()
	if reporting {
		slog.Debug("embedding CPU fallback", "reason", err)
	} else {
		slog.Warn("embedding CPU fallback", "reason", err)
	}
	p.notify()
}

// ID returns the same fingerprint on CPU and verified Vulkan.
func (p *Provider) ID() string { return Fingerprint }

// Dim returns the bundled model's vector dimension.
func (p *Provider) Dim() int { return Dimension }

// BatchSize returns the effective inference batch bound after runtime validation.
func (p *Provider) BatchSize() int {
	p.initialize()
	if p.Status().Backend == "lemonade-vulkan" {
		return p.opts.BatchSize
	}
	return 1
}

// Chunks uses the bundled tokenizer and complete passage splitting on every backend.
func (p *Provider) Chunks(text string) ([]Chunk, error) { return Default.Chunks(text) }

// Embed encodes a passage, coalescing concurrent Vulkan calls into bounded batches.
func (p *Provider) Embed(ctx context.Context, text string) ([]float32, error) {
	v, err := p.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

// EmbedBatch encodes bounded passages in order, with automatic same-model CPU fallback.
func (p *Provider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("encode batch: %w", err)
	}
	ids := make([][]int64, len(texts))
	for n, text := range texts {
		var err error
		ids[n], err = Default.Tokens(text)
		if err != nil {
			return nil, err
		}
		if len(ids[n]) > MaxTokens {
			return nil, fmt.Errorf("passage has %d tokens; maximum is %d", len(ids[n]), MaxTokens)
		}
	}
	p.initialize()
	if err := p.ctx.Err(); err != nil {
		return nil, fmt.Errorf("embedding provider stopped: %w", err)
	}
	p.mu.Lock()
	if p.started.IsZero() {
		p.started = time.Now()
	}
	cpu := p.state.Backend == "cpu"
	p.mu.Unlock()
	if cpu {
		return p.cpuBatch(ctx, texts)
	}
	// Limit outstanding requests even when a caller supplies a large batch.
	out := make([][]float32, len(texts))
	for start := 0; start < len(texts); start += p.opts.BatchSize {
		end := min(start+p.opts.BatchSize, len(texts))
		replies := make([]chan encodeReply, end-start)
		for n := start; n < end; n++ {
			replies[n-start] = make(chan encodeReply, 1)
			select {
			case p.jobs <- encodeRequest{ctx, ids[n], texts[n], replies[n-start]}:
			case <-ctx.Done():
				return nil, fmt.Errorf("queue embedding: %w", ctx.Err())
			case <-p.ctx.Done():
				return nil, fmt.Errorf("embedding provider stopped: %w", p.ctx.Err())
			}
		}
		for n, reply := range replies {
			select {
			case r := <-reply:
				if r.err != nil {
					return nil, r.err
				}
				out[start+n] = r.vector
			case <-ctx.Done():
				return nil, fmt.Errorf("wait for embedding: %w", ctx.Err())
			case <-p.ctx.Done():
				return nil, fmt.Errorf("embedding provider stopped: %w", p.ctx.Err())
			}
		}
	}
	return out, nil
}

func (p *Provider) completed(count, batch int) {
	p.mu.Lock()
	p.state.Vectors += count
	p.state.LastBatch = batch
	p.mu.Unlock()
	p.notify()
}
func (p *Provider) cpuBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	jobs := make(chan int, len(texts))
	for n := range texts {
		jobs <- n
	}
	close(jobs)
	var group sync.WaitGroup
	var mu sync.Mutex
	var failure error
	for range min(p.opts.Workers, len(texts)) {
		group.Go(func() {
			for n := range jobs {
				select {
				case p.cpuPermits <- struct{}{}:
				case <-ctx.Done():
					mu.Lock()
					if failure == nil {
						failure = fmt.Errorf("wait for CPU inference: %w", ctx.Err())
					}
					mu.Unlock()
					return
				case <-p.ctx.Done():
					mu.Lock()
					if failure == nil {
						failure = fmt.Errorf("CPU provider stopped: %w", p.ctx.Err())
					}
					mu.Unlock()
					return
				}
				v, err := Default.Embed(ctx, texts[n])
				<-p.cpuPermits
				mu.Lock()
				if err != nil && failure == nil {
					failure = err
				}
				out[n] = v
				mu.Unlock()
				if err == nil {
					p.completed(1, 1)
				}
			}
		})
	}
	group.Wait()
	return out, failure
}
func (p *Provider) pump() {
	defer close(p.stopped)
	for {
		var first encodeRequest
		select {
		case first = <-p.jobs:
		case <-p.ctx.Done():
			return
		}
		batch := []encodeRequest{first}
		timer := time.NewTimer(2 * time.Millisecond)
	collect:
		for len(batch) < p.opts.BatchSize {
			select {
			case r := <-p.jobs:
				batch = append(batch, r)
			case <-timer.C:
				break collect
			case <-p.ctx.Done():
				timer.Stop()
				return
			}
		}
		timer.Stop()
		active := batch[:0]
		for _, r := range batch {
			if err := r.ctx.Err(); err != nil {
				r.reply <- encodeReply{err: fmt.Errorf("cancelled embedding: %w", err)}
			} else {
				active = append(active, r)
			}
		}
		if len(active) == 0 {
			continue
		}
		ids := make([][]int64, len(active))
		texts := make([]string, len(active))
		for n, r := range active {
			ids[n] = r.ids
			texts[n] = r.text
		}
		var vectors [][]float32
		var err error
		if p.Status().Backend == "lemonade-vulkan" {
			vectors, err = p.gpu.embed(p.ctx, ids)
			if err != nil && p.ctx.Err() == nil {
				p.gpu.close()
				p.fallback(err)
			} else if err == nil {
				p.completed(len(vectors), len(vectors))
			}
		}
		if p.Status().Backend == "cpu" {
			vectors, err = p.cpuBatch(p.ctx, texts)
		}
		for n, r := range active {
			reply := encodeReply{err: err}
			if err == nil {
				reply.vector = vectors[n]
			}
			r.reply <- reply
		}
	}
}

// Close stops the dedicated runtime and joins the bounded batch queue.
func (p *Provider) Close() error {
	p.cancel()
	p.initialize()
	if p.stopped != nil {
		<-p.stopped
	}
	if p.gpu != nil {
		p.gpu.close()
	}
	return nil
}
