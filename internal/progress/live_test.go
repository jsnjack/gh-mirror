package progress

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// syncEvents mimics two repositories followed by hydration.
func syncEvents(d *Display) {
	d.Report(Event{Phase: "Checking checkpoints", Limit: 3000})
	for i, repo := range []string{"owner/alpha", "owner/beta"} {
		d.Report(Event{Phase: "Fetching issues and comments", Scope: repo, Repository: i + 1, Repositories: 2})
		d.Report(Event{Resource: FetchingIssues, Scope: repo, Page: 1, Records: 120})
		d.Report(Event{Resource: FetchingComments, Scope: repo, Page: 2, Records: 1500})
		d.Report(Event{Phase: "Indexing issues", Scope: repo, Total: 120, Advance: 120})
		d.Report(Event{Phase: "Recovering comment parent", Scope: repo + "#7"})
		d.Report(Event{Phase: "Reconciling inventories", Scope: repo})
	}
	d.Report(Event{Phase: "Hydrating issue metadata", Total: 240, Advance: 240, Requests: 12, Remaining: "4812", QuotaLimit: "5000", Workers: 8, Active: 8})
	d.Report(Event{Changes: []RepositoryChanges{{Repo: "owner/alpha", Issues: Tally{Added: 3, Updated: 2}}, {Repo: "owner/beta"}}})
}

func TestTerminalChecklist(t *testing.T) {
	var output lockedBuffer
	d, err := NewTerminal(&output, "sync", false)
	if err != nil {
		t.Fatal(err)
	}
	syncEvents(d)
	if err := d.Finish(nil); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"✓ Opening database", "✓ Checking checkpoints", "✓ owner/alpha", "120 issues · 1,500 comments", "✓ Hydrating issue metadata", "240/240", "Synced in", "12 requests", "owner/alpha   issues +3 ~2", "owner/beta    no changes"} {
		if !strings.Contains(text, want) {
			t.Fatalf("checklist missing %q:\n%s", want, text)
		}
	}
	// Sub-phases of one repository collapse into its single checklist line.
	if strings.Count(text, "✓ owner/alpha") != 1 || strings.Contains(text, "✓ Indexing issues") || strings.Contains(text, "✓ Recovering") {
		t.Fatalf("repository phases were not grouped:\n%s", text)
	}
	if strings.Contains(text, "\x1b[3") || strings.Contains(text, "\x1b[2m") {
		t.Fatalf("colour without color mode:\n%q", text)
	}
}

func TestTerminalColour(t *testing.T) {
	var output lockedBuffer
	d, err := NewTerminal(&output, "sync", true)
	if err != nil {
		t.Fatal(err)
	}
	syncEvents(d)
	if err := d.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), ansiGreen+"  ✓ "+ansiReset) {
		t.Fatalf("finished phases are not green:\n%q", output.String())
	}
}

func TestTerminalOutcome(t *testing.T) {
	for _, tc := range []struct {
		result      error
		mark, final string
	}{
		{errors.New("boom"), "✗ Hydrating issue metadata", "Sync failed after"},
		{context.Canceled, "■ Hydrating issue metadata", "Sync cancelled after"},
	} {
		var output lockedBuffer
		d, err := NewTerminal(&output, "sync", false)
		if err != nil {
			t.Fatal(err)
		}
		d.Report(Event{Phase: "Hydrating issue metadata", Total: 10, Advance: 4})
		if err := d.Finish(tc.result); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), tc.mark) || !strings.Contains(output.String(), tc.final) {
			t.Fatalf("outcome %v not shown:\n%s", tc.result, output.String())
		}
	}
}

func TestTerminalFrameRedraw(t *testing.T) {
	var output lockedBuffer
	d, err := NewTerminal(&output, "sync", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.Finish(nil); err != nil {
			t.Error(err)
		}
	}()
	d.Report(Event{Phase: "Hydrating issue metadata", Total: 100, Advance: 30, Wait: time.Minute})
	d.mu.Lock()
	frame := d.frame
	before := len(output.String())
	d.render(time.Now(), "")
	redraw := output.String()[before:]
	d.mu.Unlock()
	if frame != 3 || !strings.HasPrefix(redraw, "\x1b[3A\r\x1b[J") || strings.Count(redraw, "\n") != 3 {
		t.Fatalf("frame of %d rows redrew as %q", frame, redraw)
	}
	if !strings.Contains(redraw, "30/100") || !strings.Contains(redraw, "retry wait") {
		t.Fatalf("frame hid progress: %q", redraw)
	}
}

func TestTerminalLogsPrintAboveFrame(t *testing.T) {
	var output lockedBuffer
	d, err := NewTerminal(&output, "index", false)
	if err != nil {
		t.Fatal(err)
	}
	d.Report(Event{Phase: "Indexing semantic documents", Total: 10, Workers: 4, WorkerKind: CPUWorkers})
	if _, err := d.Write([]byte("level=WARN msg=\"embedding CPU fallback\"\n")); err != nil {
		t.Fatal(err)
	}
	d.Report(Event{Backend: "cpu", Device: "cpu", FallbackReason: "vulkan failed", EmbeddingVectors: 50})
	if err := d.Finish(nil); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	logAt := strings.Index(text, "level=WARN")
	if logAt < 0 || !strings.HasPrefix(text[logAt-len("\r\x1b[J"):], "\r\x1b[J") {
		t.Fatalf("log line was not printed after clearing the frame:\n%q", text)
	}
	for _, want := range []string{"! CPU fallback: vulkan failed", "Indexed in", "50 vectors"} {
		if !strings.Contains(text, want) {
			t.Fatalf("index output missing %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "CPU fallback: vulkan failed") != 1 {
		t.Fatalf("fallback repeated:\n%s", text)
	}
}

func TestFit(t *testing.T) {
	if got := fit("surfly/cobro · Fetching", 10); got != "surfly/co…" {
		t.Fatal(got)
	}
	if got := pad("ab", 4); got != "ab  " {
		t.Fatalf("%q", got)
	}
	if got := count(1234567); got != "1,234,567" {
		t.Fatal(got)
	}
}

// TestTerminalDemo plays a short simulated sync on stderr for visual review:
// GH_MIRROR_PROGRESS_DEMO=1 script -qc 'go test ./internal/progress -run TestTerminalDemo -v'
func TestTerminalDemo(t *testing.T) {
	if os.Getenv("GH_MIRROR_PROGRESS_DEMO") == "" {
		t.Skip("set GH_MIRROR_PROGRESS_DEMO to watch the terminal checklist")
	}
	d, err := NewTerminal(os.Stderr, "sync", os.Getenv("NO_COLOR") == "")
	if err != nil {
		t.Fatal(err)
	}
	step := func(e Event) { d.Report(e); time.Sleep(150 * time.Millisecond) }
	step(Event{Phase: "Checking checkpoints", Limit: 3000})
	for i, repo := range []string{"surfly/cobro", "surfly/it"} {
		step(Event{Phase: "Fetching issues and comments", Scope: repo, Repository: i + 1, Repositories: 2})
		for page := 1; page <= 6; page++ {
			step(Event{Resource: FetchingComments, Scope: repo, Page: page, Records: page * 100, Requests: page + 10*i, Remaining: "4900", QuotaLimit: "5000", Workers: 8, Active: 3})
		}
		step(Event{Phase: "Indexing issues", Scope: repo, Total: 300})
		for range 6 {
			step(Event{Advance: 50})
		}
	}
	step(Event{Phase: "Hydrating issue metadata", Total: 500})
	for range 10 {
		step(Event{Advance: 50, Requests: 40, Workers: 8, Active: 8})
	}
	step(Event{Changes: []RepositoryChanges{{Repo: "surfly/cobro", Issues: Tally{Added: 12, Updated: 40}, Comments: Tally{Added: 188, Updated: 3}}, {Repo: "surfly/it"}}})
	if err := d.Finish(nil); err != nil {
		t.Fatal(err)
	}
}
