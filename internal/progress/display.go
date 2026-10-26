package progress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	// plainInterval spaces status lines; phase changes and events print sooner.
	plainInterval = 5 * time.Second
	barWidth      = 20
	noChanges     = "no changes"
)

// Display writes plain status lines on its writer: on phase changes, at most once a
// second while events arrive, and every plainInterval otherwise.
type Display struct {
	mu                                  sync.Mutex
	writer                              io.Writer
	started, printed, until             time.Time
	phaseStarted                        time.Time
	phase, scope, remaining             string
	resource, pageScope                 string
	received                            map[string]int
	page, records                       int
	done, total                         int
	issues, comments                    int
	phaseIssues, phaseComments          int
	repository, repositories            int
	requests, limit, cached             int
	active, workers                     int
	workerKind                          string
	resumed, saved                      int
	err                                 error
	stop, stopped                       chan struct{}
	finished                            bool
	operation                           string
	backend, device, fallback, listener string
	embeddingVectors, batchSize         int
	embeddingRate                       float64
	quotaLimit                          string
	changes                             []RepositoryChanges
	// Terminal checklist state; see live.go.
	live, color  bool
	frame        int
	pending      []string
	group        string
	groupStarted time.Time
	noted        string
}

// New starts status reporting for a sync.
func New(writer io.Writer) (*Display, error) {
	return NewOperation(writer, "sync")
}

// NewOperation reports collection or standalone offline indexing activity.
func NewOperation(writer io.Writer, operation string) (*Display, error) {
	return startDisplay(writer, operation, false, false)
}

func startDisplay(writer io.Writer, operation string, live, color bool) (*Display, error) {
	now := time.Now()
	d := &Display{writer: writer, started: now, phaseStarted: now, phase: "Opening database", received: map[string]int{}, stop: make(chan struct{}), stopped: make(chan struct{}), operation: operation, live: live, color: color}
	d.render(time.Now(), "")
	if d.err != nil {
		return nil, d.err
	}
	interval := time.Second
	if live {
		interval = liveInterval
	}
	go func() {
		defer close(d.stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-d.stop:
				return
			case now := <-ticker.C:
				d.mu.Lock()
				if !d.finished && (d.live || now.Sub(d.printed) >= plainInterval) {
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
	changed := event.Phase != "" && (clean(event.Phase) != d.phase || clean(event.Scope) != d.scope)
	if event.Phase != "" {
		if changed {
			if d.live {
				d.commitPhase(event, time.Now())
			}
			d.phaseStarted = time.Now()
			d.page, d.records, d.done, d.total = 0, 0, 0, 0
			d.resource, d.pageScope = "", ""
			d.phaseIssues, d.phaseComments = d.issues, d.comments
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
			d.resource, d.pageScope = clean(resource), clean(event.Scope)
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
		d.remaining, d.quotaLimit = clean(event.Remaining), clean(event.QuotaLimit)
	}
	if event.Changes != nil {
		d.changes = event.Changes
	}
	if event.Cached {
		d.cached++
	}
	if event.Wait > 0 {
		if event.WaitUntil.IsZero() {
			d.until = time.Now().Add(event.Wait)
		}
	}
	if d.live {
		if d.fallback != "" && d.fallback != d.noted {
			d.noted = d.fallback
			d.pending = append(d.pending, d.paint(ansiYellow, "  ! ")+"CPU fallback: "+d.fallback)
		}
		if len(d.pending) > 0 {
			d.render(time.Now(), "")
		}
		return
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
	if d.live {
		d.finishLive(result)
		return d.err
	}
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
	if d.live {
		d.renderLive(now, status)
		return
	}
	if d.err != nil {
		return
	}
	elapsed := now.Sub(d.started).Round(time.Second)
	activity := d.phase
	if d.scope != "" {
		activity += " | " + d.scope
	}
	if d.page > 0 {
		if d.pageScope != "" && d.pageScope != d.scope {
			activity += " | " + d.pageScope
		}
		if d.resource != "" {
			activity += " | " + d.resource
		}
		activity += fmt.Sprintf(" | page %d, %d records", d.page, d.records)
	}
	bounded := ""
	if d.total > 0 {
		done := min(d.done, d.total)
		filled := done * barWidth / d.total
		bar := "[" + strings.Repeat("=", filled) + strings.Repeat("-", barWidth-filled) + "]"
		bounded = fmt.Sprintf("%s %d/%d (%d%%)", bar, done, d.total, done*100/d.total)
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
		requests += fmt.Sprintf(" | %s workers %d/%d active", kind, d.active, d.workers)
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
		if d.quotaLimit != "" {
			quota += "/" + d.quotaLimit
		}
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
	changes := d.changeRows(status)
	for _, detail := range []string{bounded, wait} {
		if detail != "" {
			activity += " | " + detail
		}
	}
	if status != "" {
		activity = status + " | last phase: " + activity
	}
	text := fmt.Sprintf("gh-mirror | %s | %s | %s | elapsed %s | %s", activity, counters, requests, elapsed, quota)
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
	for _, change := range changes {
		text += " | changed " + change
	}
	text += "\n"
	if _, err := io.WriteString(d.writer, text); err != nil {
		d.err = fmt.Errorf("write sync progress: %w", err)
	}
	d.printed = now
}

// changeRows summarises committed changes per repository once the sync finishes.
func (d *Display) changeRows(status string) []string {
	if status == "" || d.changes == nil {
		return nil
	}
	rows := make([]string, 0, len(d.changes))
	for _, c := range d.changes {
		rows = append(rows, clean(c.Repo)+": "+repositoryChanges(c, " | "))
	}
	return rows
}

// repositoryChanges lists one repository's non-zero tallies joined by separator.
func repositoryChanges(c RepositoryChanges, separator string) string {
	var parts []string
	for _, kind := range []struct {
		name  string
		tally Tally
	}{{"issues", c.Issues}, {"pull requests", c.PullRequests}, {"comments", c.Comments}} {
		if part := tallyText(kind.tally); part != "" {
			parts = append(parts, kind.name+" "+part)
		}
	}
	if len(parts) == 0 {
		return noChanges
	}
	return strings.Join(parts, separator)
}

// tallyText renders non-zero counts as +added ~updated -removed.
func tallyText(t Tally) string {
	var parts []string
	for _, count := range []struct {
		sign string
		n    int
	}{{"+", t.Added}, {"~", t.Updated}, {"-", t.Removed}} {
		if count.n > 0 {
			parts = append(parts, count.sign+strconv.Itoa(count.n))
		}
	}
	return strings.Join(parts, " ")
}

func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}
