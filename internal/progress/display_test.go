package progress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReporter(t *testing.T) {
	for _, name := range []string{"nil", "callback"} {
		t.Run(name, func(t *testing.T) {
			var reporter Reporter
			var received Event
			if name == "callback" {
				reporter = func(event Event) { received = event }
			}
			reporter.Send(Event{Requests: 7})
			if name == "callback" && received.Requests != 7 {
				t.Fatal("reporter lost the event", received)
			}
		})
	}
}

func TestDisplay(t *testing.T) {
	for _, name := range []string{"plain", "terminal", "cancelled", "failed", "output failure", "heartbeat", "terminal detection"} {
		t.Run(name, func(t *testing.T) {
			var output lockedBuffer
			writer := io.Writer(&output)
			if name == "output failure" {
				writer = failingWriter{}
			}
			var display *Display
			var err error
			if name == "terminal" {
				display, err = newDisplay(writer, true)
			} else {
				display, err = New(writer, true)
			}
			if name == "output failure" {
				if !errors.Is(err, io.ErrClosedPipe) {
					t.Fatal("progress output error lost", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "Opening database") {
				t.Fatal("no immediate startup status", output.String())
			}
			if name == "terminal detection" {
				f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				nullDisplay, err := New(f, true)
				if err != nil {
					t.Fatal(err)
				}
				if nullDisplay.interactive {
					t.Fatal("a character device was mistaken for a terminal")
				}
				if err := nullDisplay.Finish(nil); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			display.Report(Event{Phase: FetchingIssues, Scope: "owner/repo", Repository: 1, Repositories: 2})
			display.Report(Event{Page: 1, Records: 100})
			display.Report(Event{Page: 2, Records: 125})
			display.Report(Event{Requests: 2, Limit: 3000, Remaining: "4998", Cached: true})
			display.Report(Event{Phase: FetchingComments, Scope: "owner/repo"})
			display.Report(Event{Page: 1, Records: 3})
			display.Report(Event{Phase: FetchingIssues, Scope: "owner/second"})
			display.Report(Event{Page: 1, Records: 2})
			display.Report(Event{Phase: "Hydrating issue metadata", Total: 127})
			display.Report(Event{Advance: 50})
			display.Report(Event{Wait: 30 * time.Second})
			if name == "heartbeat" {
				display.mu.Lock()
				display.printed = time.Now().Add(-plainInterval)
				display.mu.Unlock()
				time.Sleep(2 * refreshInterval)
			}
			var result error
			switch name {
			case "cancelled":
				result = context.Canceled
			case "failed":
				result = errors.New("upstream failed")
			}
			if err := display.Finish(result); err != nil {
				t.Fatal(err)
			}
			text := output.String()
			for _, want := range []string{"127 issues, 3 comments", "2/3000 requests", "4998", "1 cached", "50/127", "retry wait"} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q in progress: %s", want, text)
				}
			}
			want := "Sync complete"
			switch name {
			case "cancelled":
				want = "Sync cancelled"
			case "failed":
				want = "Sync failed"
			}
			if !strings.Contains(text, want) || (strings.Contains(text, "\x1b[") != (name == "terminal")) {
				t.Fatal("wrong completion or terminal mode", text)
			}
			if name == "heartbeat" && strings.Count(text, "retry wait") < 2 {
				t.Fatal("status did not refresh during idle work", text)
			}
			if err := display.Finish(nil); err != nil {
				t.Fatal(err)
			}
			display.Report(Event{Phase: "late event"})
			if output.String() != text {
				t.Fatal("finished display kept writing")
			}
		})
	}
}

func TestParallelProgress(t *testing.T) {
	t.Run("interleaved pages and shared counters", func(t *testing.T) {
		var output lockedBuffer
		display, err := newDisplay(&output, false)
		if err != nil {
			t.Fatal(err)
		}
		display.Report(Event{Phase: "Fetching issues and comments", Scope: "o/r", Saved: 5})
		for _, event := range []Event{
			{Resource: FetchingIssues, Scope: "o/r", Page: 1, Records: 100},
			{Resource: FetchingComments, Scope: "o/r", Page: 1, Records: 100},
			{Resource: FetchingIssues, Scope: "o/r", Page: 2, Records: 125},
			{Resource: FetchingComments, Scope: "o/r", Page: 2, Records: 150},
			{Requests: 8, Limit: 20, Workers: 4, Active: 3, Resumed: 5},
			{Requests: 7, Limit: 20, Workers: 4, Active: 2, Resumed: 4},
		} {
			display.Report(event)
		}
		if err := display.Finish(nil); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"125 issues, 150 comments", "8/20 requests", "workers 2/4", "5 resumed"} {
			if !strings.Contains(output.String(), want) {
				t.Fatal("incorrect parallel counters", want, output.String())
			}
		}
	})
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
