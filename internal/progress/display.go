package progress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mattn/go-isatty"
)

const (
	refreshInterval = 200 * time.Millisecond
	plainInterval   = 5 * time.Second
	barWidth        = 20
	lineWidth       = 78
)

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Display renders terminal activity or periodic plain status lines on its writer.
type Display struct {
	mu                                  sync.Mutex
	writer                              io.Writer
	interactive                         bool
	started, printed, until             time.Time
	phaseStarted                        time.Time
	columns                             func() int
	phase, scope, remaining             string
	resource                            string
	received                            map[string]int
	page, records                       int
	done, total                         int
	issues, comments                    int
	repository, repositories            int
	requests, limit, cached             int
	active, workers                     int
	workerKind                          string
	resumed, saved                      int
	lines, frame                        int
	err                                 error
	stop, stopped                       chan struct{}
	finished                            bool
	operation                           string
	backend, device, fallback, listener string
	embeddingVectors, batchSize         int
	embeddingRate                       float64
}

// New starts immediate status reporting and animates only a terminal writer.
func New(writer io.Writer, animate bool) (*Display, error) {
	return NewOperation(writer, animate, "sync")
}

// NewOperation reports collection or standalone offline indexing activity.
func NewOperation(writer io.Writer, animate bool, operation string) (*Display, error) {
	interactive := false
	if fd, ok := writer.(interface{ Fd() uintptr }); ok && animate && os.Getenv("TERM") != "dumb" {
		interactive = isatty.IsTerminal(fd.Fd()) || isatty.IsCygwinTerminal(fd.Fd())
	}
	return startDisplay(writer, interactive, operation)
}

func newDisplay(writer io.Writer, interactive bool) (*Display, error) {
	return startDisplay(writer, interactive, "sync")
}
func startDisplay(writer io.Writer, interactive bool, operation string) (*Display, error) {
	now := time.Now()
	d := &Display{writer: writer, interactive: interactive, started: now, phaseStarted: now, columns: func() int { return terminalWidth(writer) }, phase: "Opening database", received: map[string]int{}, stop: make(chan struct{}), stopped: make(chan struct{})}
	d.operation = operation
	d.render(time.Now(), "")
	if d.err != nil {
		return nil, d.err
	}
	go func() {
		defer close(d.stopped)
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-d.stop:
				return
			case now := <-ticker.C:
				d.mu.Lock()
				if !d.finished && (d.interactive || now.Sub(d.printed) >= plainInterval) {
					d.render(now, "")
				}
				d.mu.Unlock()
			}
		}
	}()
	return d, nil
}

// Report updates counters and immediately renders phase changes.
func (d *Display) Report(event Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finished {
		return
	}
	changed := event.Phase != "" && (event.Phase != d.phase || event.Scope != d.scope)
	if event.Phase != "" {
		if changed {
			d.phaseStarted = time.Now()
			d.page, d.records, d.done, d.total = 0, 0, 0, 0
			d.resource = ""
		}
		d.phase, d.scope = clean(event.Phase), clean(event.Scope)
	}
	if event.Page > 0 {
		resource, previous := d.phase, d.records
		if event.Resource != "" {
			resource = event.Resource
			key := event.Scope + "/" + resource
			previous = d.received[key]
			d.received[key] = event.Records
			d.resource = clean(resource)
		}
		switch resource {
		case FetchingIssues:
			d.issues += event.Records - previous
		case FetchingComments, FetchingReviewComments:
			d.comments += event.Records - previous
		}
		d.page, d.records = event.Page, event.Records
	}
	if event.Total > 0 {
		d.total, d.done = event.Total, event.Completed
	}
	d.done += event.Advance
	if event.Repositories > 0 {
		d.repository, d.repositories = event.Repository, event.Repositories
	}
	if event.Requests > 0 {
		d.requests = max(d.requests, event.Requests)
		if event.Workers == 0 {
			d.until = time.Time{}
		}
	}
	if event.Workers > 0 {
		d.active, d.workers = event.Active, event.Workers
		d.until = event.WaitUntil
	}
	if event.WorkerKind != "" {
		d.workerKind = event.WorkerKind
	}
	if event.Backend != "" {
		changed = changed || event.Backend != d.backend || event.FallbackReason != d.fallback || event.Listener != d.listener
		d.backend, d.device, d.fallback, d.listener = clean(event.Backend), clean(event.Device), clean(event.FallbackReason), clean(event.Listener)
		d.embeddingVectors, d.embeddingRate, d.batchSize = event.EmbeddingVectors, event.EmbeddingRate, event.BatchSize
	}
	if (d.backend == "lemonade-vulkan" || d.backend == "starting Vulkan") && d.workerKind == CPUWorkers {
		d.workerKind = "Index"
	}
	d.resumed = max(d.resumed, event.Resumed)
	d.saved = max(d.saved, event.Saved)
	if event.Limit > 0 {
		d.limit = event.Limit
	}
	if event.Remaining != "" {
		d.remaining = clean(event.Remaining)
	}
	if event.Cached {
		d.cached++
	}
	if event.Wait > 0 {
		if event.WaitUntil.IsZero() {
			d.until = time.Now().Add(event.Wait)
		}
	}
	if changed || event.Wait > 0 || (d.total > 0 && d.done == d.total) || time.Since(d.printed) >= time.Second {
		d.render(time.Now(), "")
	}
}

// Finish stops refreshes, prints a completion status, and returns output errors.
func (d *Display) Finish(result error) error {
	d.mu.Lock()
	if d.finished {
		err := d.err
		d.mu.Unlock()
		return err
	}
	d.finished = true
	close(d.stop)
	d.mu.Unlock()
	<-d.stopped
	d.mu.Lock()
	defer d.mu.Unlock()
	status := "Sync complete"
	if errors.Is(result, context.Canceled) {
		status = "Sync cancelled; rerun sync to resume"
	} else if result != nil {
		status = "Sync failed"
	}
	if d.operation == "index" {
		status = "Index complete"
		if result != nil {
			status = "Index failed"
		}
		if errors.Is(result, context.Canceled) {
			status = "Index cancelled; rerun index to resume"
		}
	}
	d.render(time.Now(), status)
	return d.err
}

func (d *Display) render(now time.Time, status string) {
	if d.err != nil {
		return
	}
	elapsed := now.Sub(d.started).Round(time.Second)
	activity := d.phase
	if d.scope != "" {
		activity += " | " + d.scope
	}
	if d.page > 0 {
		if d.resource != "" {
			activity += " | " + d.resource
		}
		activity += fmt.Sprintf(" | page %d, %d records", d.page, d.records)
	}
	bounded := ""
	if d.total > 0 {
		done := min(d.done, d.total)
		filled := done * barWidth / d.total
		bounded = fmt.Sprintf("[%s%s] %d/%d (%d%%)", strings.Repeat("=", filled), strings.Repeat("-", barWidth-filled), done, d.total, done*100/d.total)
		if seconds := now.Sub(d.phaseStarted).Seconds(); seconds >= 1 && done > 0 {
			rate := float64(done) / seconds
			bounded += fmt.Sprintf(" | %.1f/s", rate)
			if status == "" && done < d.total && !now.Before(d.until) {
				eta := time.Duration(float64(d.total-done) / rate * float64(time.Second)).Round(time.Second)
				bounded += " | phase ETA " + eta.String()
			}
		}
	}
	wait := ""
	if now.Before(d.until) {
		wait = fmt.Sprintf("retry wait %s", max(time.Duration(0), d.until.Sub(now)).Round(time.Second))
	}
	counters := fmt.Sprintf("Received: %d issues, %d comments", d.issues, d.comments)
	if d.repositories > 0 {
		counters += fmt.Sprintf(" | repository %d/%d", d.repository, d.repositories)
	}
	requests := fmt.Sprintf("API: %d", d.requests)
	if d.limit > 0 {
		requests += fmt.Sprintf("/%d", d.limit)
	}
	requests += fmt.Sprintf(" requests | %d cached", d.cached)
	if d.workers > 0 {
		kind := d.workerKind
		if kind == "" {
			kind = GitHubWorkers
		}
		requests = fmt.Sprintf("%s workers %d/%d active | ", kind, d.active, d.workers) + requests
	}
	resume := ""
	if d.saved > 0 || d.resumed > 0 {
		resume = fmt.Sprintf("Resume: %d resumed responses", d.resumed)
	}
	if d.saved > 0 {
		resume += fmt.Sprintf(" | %d saved before this run", d.saved)
	}
	quota := "GitHub remaining: not reported yet"
	if d.remaining != "" {
		quota = "GitHub remaining: " + d.remaining
	}
	if d.operation == "index" {
		counters = ""
		kind := d.workerKind
		if kind == "" {
			kind = CPUWorkers
		}
		requests = fmt.Sprintf("%s workers %d/%d active", kind, d.active, d.workers)
		quota = ""
		resume = ""
	}
	embeddings, fallback, listener := "", "", ""
	if d.backend != "" {
		embeddings = fmt.Sprintf("Embeddings: %s | %s | %d vectors | %.1f vectors/s", d.backend, d.device, d.embeddingVectors, d.embeddingRate)
		if d.batchSize > 0 {
			embeddings += fmt.Sprintf(" | batch %d", d.batchSize)
		}
		if d.fallback != "" {
			fallback = "CPU fallback: " + d.fallback
		}
		if d.listener != "" {
			listener = "Listening on " + d.listener + " (local embedding runtime)"
		}
	}
	var text string
	if d.interactive {
		var b strings.Builder
		if d.lines > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", d.lines)
		}
		header := fmt.Sprintf("gh-mirror %s | elapsed %s", d.operation, elapsed)
		if status != "" {
			header = status + " | elapsed " + elapsed.String()
			activity = "Last phase: " + activity
		} else {
			activity = frames[d.frame%len(frames)] + " " + activity
			d.frame++
		}
		rows := []string{header, activity, bounded, wait, counters, requests, quota, resume, embeddings, fallback, listener}
		var lines []string
		// Leave the final column unused to avoid an untracked terminal wrap.
		width := max(1, d.columns()-1)
		for _, row := range rows {
			if row != "" {
				lines = append(lines, wrap(row, width)...)
			}
		}
		for index := range max(d.lines, len(lines)) {
			b.WriteString("\r\x1b[2K")
			if index < len(lines) {
				b.WriteString(lines[index])
			}
			b.WriteByte('\n')
		}
		if d.lines > len(lines) {
			fmt.Fprintf(&b, "\x1b[%dA", d.lines-len(lines))
		}
		text, d.lines = b.String(), len(lines)
	} else {
		for _, detail := range []string{bounded, wait} {
			if detail != "" {
				activity += " | " + detail
			}
		}
		if status != "" {
			activity = status + " | last phase: " + activity
		}
		text = fmt.Sprintf("gh-mirror | %s | %s | %s | elapsed %s | %s", activity, counters, requests, elapsed, quota)
		if d.operation == "index" {
			text = fmt.Sprintf("gh-mirror index | %s | elapsed %s | %s", activity, elapsed, requests)
		}
		if resume != "" {
			text += " | " + resume
		}
		for _, detail := range []string{embeddings, fallback, listener} {
			if detail != "" {
				text += " | " + detail
			}
		}
		text += "\n"
	}
	if _, err := io.WriteString(d.writer, text); err != nil {
		d.err = fmt.Errorf("write sync progress: %w", err)
	}
	d.printed = now
}

func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}

func wrap(line string, width int) []string {
	runes := []rune(line)
	var lines []string
	for len(runes) > width {
		at, skip := width, 0
		for i := width; i > 0; i-- {
			if i+2 < len(runes) && string(runes[i:i+3]) == " | " {
				at, skip = i, 3
				break
			}
		}
		if skip == 0 {
			for i := width; i > 0; i-- {
				if runes[i] == ' ' {
					at, skip = i, 1
					break
				}
			}
		}
		lines = append(lines, string(runes[:at]))
		runes = runes[at+skip:]
	}
	if len(runes) > 0 {
		lines = append(lines, string(runes))
	}
	return lines
}
