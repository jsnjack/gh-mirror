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
	mu                       sync.Mutex
	writer                   io.Writer
	interactive              bool
	started, printed, until  time.Time
	phase, scope, remaining  string
	page, records            int
	done, total              int
	issues, comments         int
	repository, repositories int
	requests, limit, cached  int
	lines, frame             int
	err                      error
	stop, stopped            chan struct{}
	finished                 bool
}

// New starts immediate status reporting and animates only a terminal writer.
func New(writer io.Writer, animate bool) (*Display, error) {
	interactive := false
	if fd, ok := writer.(interface{ Fd() uintptr }); ok && animate && os.Getenv("TERM") != "dumb" {
		interactive = isatty.IsTerminal(fd.Fd()) || isatty.IsCygwinTerminal(fd.Fd())
	}
	return newDisplay(writer, interactive)
}

func newDisplay(writer io.Writer, interactive bool) (*Display, error) {
	d := &Display{writer: writer, interactive: interactive, started: time.Now(), phase: "Opening database", stop: make(chan struct{}), stopped: make(chan struct{})}
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
			d.page, d.records, d.done, d.total = 0, 0, 0, 0
		}
		d.phase, d.scope = clean(event.Phase), clean(event.Scope)
	}
	if event.Page > 0 {
		switch d.phase {
		case FetchingIssues:
			d.issues += event.Records - d.records
		case FetchingComments:
			d.comments += event.Records - d.records
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
		d.requests = event.Requests
		d.until = time.Time{}
	}
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
		d.until = time.Now().Add(event.Wait)
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
		status = "Sync cancelled"
	} else if result != nil {
		status = "Sync failed"
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
		activity += fmt.Sprintf(" | page %d, %d records", d.page, d.records)
	}
	if d.total > 0 {
		filled := min(d.done, d.total) * barWidth / d.total
		activity += fmt.Sprintf(" | [%s%s] %d/%d", strings.Repeat("=", filled), strings.Repeat("-", barWidth-filled), d.done, d.total)
	}
	if !d.until.IsZero() {
		activity += fmt.Sprintf(" | retry wait %s", max(time.Duration(0), d.until.Sub(now)).Round(time.Second))
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
	if d.remaining != "" {
		requests += " | GitHub remaining: " + d.remaining
	}
	var text string
	if d.interactive {
		var b strings.Builder
		if d.lines > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", d.lines)
		}
		header := fmt.Sprintf("gh-mirror sync | elapsed %s", elapsed)
		if status != "" {
			header = status + " | elapsed " + elapsed.String()
			activity = "Last phase: " + activity
		} else {
			activity = frames[d.frame%len(frames)] + " " + activity
			d.frame++
		}
		for _, line := range []string{header, activity, counters, requests} {
			b.WriteString("\r\x1b[2K" + fit(line) + "\n")
		}
		text, d.lines = b.String(), 4
	} else {
		if status != "" {
			activity = status + " | last phase: " + activity
		}
		text = fmt.Sprintf("gh-mirror | %s | %s | %s | elapsed %s\n", activity, counters, requests, elapsed)
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

func fit(line string) string {
	runes := []rune(line)
	if len(runes) > lineWidth {
		return string(runes[:lineWidth-3]) + "..."
	}
	return line
}
