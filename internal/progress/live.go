package progress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Terminal rendering: finished phases become fixed checklist lines and the current
// phase redraws in place below them. Only frame lines are fitted to the terminal
// width, so the cursor moves up exactly as many rows as the frame printed.

const (
	liveInterval = 100 * time.Millisecond
	labelWidth   = 32
	detailWidth  = 30
	defaultWidth = 100
)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

var spinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// NewTerminal reports activity as a redrawn checklist for an interactive terminal.
func NewTerminal(writer io.Writer, operation string, color bool) (*Display, error) {
	return startDisplay(writer, operation, true, color)
}

// Write prints diagnostics above the live frame so log lines and redraws do not mix.
func (d *Display) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.live || d.finished {
		return d.writer.Write(p)
	}
	d.pending = append(d.pending, strings.TrimRight(string(p), "\n"))
	d.renderLive(time.Now(), "")
	if d.err != nil {
		return 0, d.err
	}
	return len(p), nil
}

// commitPhase turns the finished phase into a checklist line, unless next continues
// the current repository group.
func (d *Display) commitPhase(next Event, now time.Time) {
	scope := clean(next.Scope)
	if d.group != "" && next.Repositories == 0 && (scope == d.group || strings.HasPrefix(scope, d.group+"#")) {
		return
	}
	d.pending = append(d.pending, d.checklistLine(now, "✓", ansiGreen))
	d.group = ""
	if next.Repositories > 0 {
		d.group, d.groupStarted = scope, now
	}
}

// checklistLine describes the current phase or repository group as finished.
func (d *Display) checklistLine(now time.Time, mark, code string) string {
	label, detail, started := d.phase, "", d.phaseStarted
	if d.scope != "" {
		label += " · " + d.scope
	}
	switch {
	case d.group != "":
		label, started = d.group, d.groupStarted
		// Listings may finish before the repository's group starts, so count by scope.
		detail = received(d.received[d.group+"/"+FetchingIssues], d.received[d.group+"/"+FetchingComments]+d.received[d.group+"/"+FetchingReviewComments])
	case strings.HasSuffix(d.phase, "required"):
		return d.paint(ansiYellow, "  ! ") + label
	case d.total > 0:
		detail = count(min(d.done, d.total)) + "/" + count(d.total)
	case d.issues > d.phaseIssues || d.comments > d.phaseComments:
		detail = received(d.issues-d.phaseIssues, d.comments-d.phaseComments)
	}
	if d.backend != "" && d.total > 0 && d.workerKind != GitHubWorkers {
		detail += " · " + count(d.embeddingVectors) + " vectors"
	}
	return d.paint(code, "  "+mark+" ") + pad(label, labelWidth) + "  " + pad(detail, detailWidth) + "  " + d.paint(ansiDim, duration(now.Sub(started)))
}

func (d *Display) renderLive(now time.Time, status string) {
	if d.err != nil {
		return
	}
	var b strings.Builder
	if d.frame > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", d.frame)
	}
	b.WriteString("\r\x1b[J")
	for _, line := range d.pending {
		b.WriteString(line + "\n")
	}
	d.pending = nil
	lines := d.summaryLines(now, status)
	if status == "" {
		lines = d.frameLines(now)
	}
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	d.frame = 0
	if status == "" {
		d.frame = len(lines)
	}
	if _, err := io.WriteString(d.writer, b.String()); err != nil {
		d.err = fmt.Errorf("write sync progress: %w", err)
	}
	d.printed = now
}

// frameLines draws the running phase: its own progress, then shared counters.
func (d *Display) frameLines(now time.Time) []string {
	width := terminalWidth(d.writer) - 1
	label := d.phase
	if d.scope != "" && d.scope != d.group {
		label += " · " + d.scope
	}
	if d.group != "" {
		label = d.group
		if d.repositories > 1 {
			label += fmt.Sprintf(" (%d/%d)", d.repository, d.repositories)
		}
		label += " · " + d.phase
	}
	var metrics []string
	bar := ""
	if d.total > 0 {
		done := min(d.done, d.total)
		filled := done * barWidth / d.total
		bar = strings.Repeat("━", filled) + strings.Repeat("─", barWidth-filled)
		metrics = append(metrics, bar, fmt.Sprintf("%3d%%", done*100/d.total), count(done)+"/"+count(d.total))
		if seconds := now.Sub(d.phaseStarted).Seconds(); seconds >= 1 && done > 0 {
			rate := float64(done) / seconds
			metrics = append(metrics, fmt.Sprintf("%.1f/s", rate))
			if done < d.total && !now.Before(d.until) {
				metrics = append(metrics, "ETA "+duration(time.Duration(float64(d.total-done)/rate*float64(time.Second))))
			}
		}
	} else if d.page > 0 {
		resource := strings.ToLower(strings.TrimPrefix(d.resource, "Fetching "))
		if d.pageScope != "" && d.pageScope != d.scope && d.pageScope != d.group {
			resource = d.pageScope + " " + resource
		}
		metrics = append(metrics, strings.TrimSpace(fmt.Sprintf("%s page %d · %s records", resource, d.page, count(d.records))))
	}
	tail := strings.Join(metrics, "  ")
	frame := spinner[int(now.Sub(d.started)/liveInterval)%len(spinner)]
	head := d.fitHead(string(frame), label, tail, bar, width)

	var stats []string
	// Received records are left out here; each repository's checklist line shows them.
	if d.operation != "index" {
		if d.remaining != "" {
			quota := "GitHub " + number(d.remaining)
			if d.quotaLimit != "" {
				quota += "/" + number(d.quotaLimit)
			}
			stats = append(stats, quota+" left")
		}
		requests := count(d.requests)
		if d.limit > 0 {
			requests += "/" + count(d.limit)
		}
		stats = append(stats, requests+" requests", count(d.cached)+" cached")
	}
	if d.workers > 0 {
		kind := d.workerKind
		if kind == "" {
			kind = GitHubWorkers
			if d.operation == "index" {
				kind = CPUWorkers
			}
		}
		stats = append(stats, fmt.Sprintf("%s workers %d/%d", kind, d.active, d.workers))
	}
	stats = append(stats, duration(now.Sub(d.started)))
	lines := []string{head, d.paint(ansiDim, fit("    "+strings.Join(stats, " · "), width))}
	if now.Before(d.until) {
		wait := max(time.Duration(0), d.until.Sub(now)).Round(time.Second)
		lines = append(lines, d.paint(ansiYellow, fit("    retry wait "+wait.String(), width)))
	}
	if d.backend != "" {
		embeddings := fmt.Sprintf("    embeddings %s · %s · %s vectors · %.1f/s", d.backend, d.device, count(d.embeddingVectors), d.embeddingRate)
		if d.batchSize > 0 {
			embeddings += fmt.Sprintf(" · batch %d", d.batchSize)
		}
		lines = append(lines, d.paint(ansiDim, fit(embeddings, width)))
	}
	if d.operation != "index" && (d.saved > 0 || d.resumed > 0) {
		lines = append(lines, d.paint(ansiDim, fit(fmt.Sprintf("    resumed %s responses · %s saved before this run", count(d.resumed), count(d.saved)), width)))
	}
	return lines
}

// fitHead shortens the label first so the progress metrics stay visible.
func (d *Display) fitHead(frame, label, tail, bar string, width int) string {
	fixed := 4 + utf8.RuneCountInString(tail)
	if tail != "" {
		fixed += 2
	}
	if room := width - fixed; room < utf8.RuneCountInString(label) {
		if room < 12 {
			return fit("  "+frame+" "+label+"  "+tail, width)
		}
		label = fit(label, room)
	}
	if tail == "" {
		return d.paint(ansiCyan, "  "+frame) + " " + label
	}
	if bar != "" && d.color {
		filled := strings.Count(bar, "━")
		tail = strings.Replace(tail, bar, d.paint(ansiCyan, strings.Repeat("━", filled))+d.paint(ansiDim, strings.Repeat("─", barWidth-filled)), 1)
	}
	return d.paint(ansiCyan, "  "+frame) + " " + pad(label, max(labelWidth, utf8.RuneCountInString(label))) + "  " + tail
}

// finishLive closes the checklist and prints the outcome with committed changes.
func (d *Display) finishLive(result error) {
	now := time.Now()
	mark, code, status := "✓", ansiGreen, "done"
	if errors.Is(result, context.Canceled) {
		mark, code, status = "■", ansiYellow, "cancelled"
	} else if result != nil {
		mark, code, status = "✗", ansiRed, "failed"
	}
	d.pending = append(d.pending, d.checklistLine(now, mark, code))
	d.renderLive(now, status)
}

func (d *Display) summaryLines(now time.Time, status string) []string {
	elapsed := duration(now.Sub(d.started))
	name, verb := "Sync", "Synced"
	if d.operation == "index" {
		name, verb = "Index", "Indexed"
	}
	head := d.paint(ansiBold+ansiGreen, verb+" in "+elapsed)
	switch status {
	case "cancelled":
		head = d.paint(ansiYellow, fmt.Sprintf("%s cancelled after %s; rerun %s to resume", name, elapsed, d.operation))
	case "failed":
		head = d.paint(ansiRed, fmt.Sprintf("%s failed after %s", name, elapsed))
	}
	if d.operation != "index" {
		head += d.paint(ansiDim, fmt.Sprintf(" · %s requests · %s cached", count(d.requests), count(d.cached)))
	}
	lines := []string{"", "  " + head}
	repoWidth := 0
	for _, c := range d.changes {
		repoWidth = max(repoWidth, utf8.RuneCountInString(clean(c.Repo)))
	}
	for _, c := range d.changes {
		lines = append(lines, "  "+pad(clean(c.Repo), repoWidth)+"   "+repositoryChanges(c, "   "))
	}
	return lines
}

func (d *Display) paint(code, s string) string {
	if !d.color || s == "" {
		return s
	}
	return code + s + ansiReset
}

// pad fits s to exactly n columns.
func pad(s string, n int) string {
	s = fit(s, n)
	return s + strings.Repeat(" ", n-utf8.RuneCountInString(s))
}

// fit cuts s short with an ellipsis so it never wraps past n columns.
func fit(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}

// received names the non-zero record counts one repository listed.
func received(issues, comments int) string {
	var parts []string
	if issues > 0 {
		parts = append(parts, count(issues)+" issues")
	}
	if comments > 0 {
		parts = append(parts, count(comments)+" comments")
	}
	if len(parts) == 0 {
		return "no updates"
	}
	return strings.Join(parts, " · ")
}

func duration(d time.Duration) string {
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return d.Round(time.Second).String()
	}
}

// count groups thousands: 4812 → 4,812.
func count(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + count(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// number groups thousands in a numeric header value and keeps other text as is.
func number(s string) string {
	if n, err := strconv.Atoi(s); err == nil {
		return count(n)
	}
	return s
}
