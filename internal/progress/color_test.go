package progress

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

var escapes = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func TestColorMode(t *testing.T) {
	for _, tc := range []struct {
		value               string
		noColor             string
		interactive, styled bool
		fail                bool
	}{
		{value: "auto", interactive: true, styled: true},
		{value: "auto", noColor: "1", interactive: true},
		{value: "auto"},
		{value: "always", noColor: "1", styled: true},
		{value: "never", interactive: true},
		{value: "sometimes", fail: true},
	} {
		t.Run(tc.value+"/"+tc.noColor, func(t *testing.T) {
			t.Setenv("NO_COLOR", tc.noColor)
			mode, err := ParseColor(tc.value)
			if (err != nil) != tc.fail {
				t.Fatal("unexpected parse result", err)
			}
			if !tc.fail && mode.enabled(tc.interactive) != tc.styled {
				t.Fatal("wrong colour decision", tc)
			}
		})
	}
}

func TestColoredProgress(t *testing.T) {
	for _, name := range []string{"terminal", "plain", "failed"} {
		t.Run(name, func(t *testing.T) {
			var output lockedBuffer
			display, err := startDisplay(&output, name != "plain", "sync", true)
			if err != nil {
				t.Fatal(err)
			}
			display.Report(Event{Phase: "Hydrating issue metadata", Scope: "owner/repo", Total: 100, Advance: 40, Remaining: "400", QuotaLimit: "5000"})
			display.Report(Event{Wait: time.Minute})
			display.Report(Event{Changes: []RepositoryChanges{
				{Repo: "owner/repo", Issues: Tally{Added: 3, Updated: 41, Removed: 1}, Comments: Tally{Added: 88}},
				{Repo: "owner/quiet"},
			}})
			display.mu.Lock()
			display.columns = func() int { return 60 }
			display.mu.Unlock()
			var result error
			if name == "failed" {
				result = errors.New("upstream failed")
			}
			if err := display.Finish(result); err != nil {
				t.Fatal(err)
			}
			text := output.String()
			plain := escapes.ReplaceAllString(text, "")
			for _, want := range []string{"owner/repo: issues +3 ~41 -1 | comments +88", "owner/quiet: no changes"} {
				if !strings.Contains(plain, want) {
					t.Fatal("missing change summary", want, plain)
				}
			}
			switch name {
			case "terminal":
				for _, want := range []string{styleGreen + "+3" + styleReset, styleCyan + "~41" + styleReset, styleRed + "-1" + styleReset, styleBold + styleGreen + "✓ Sync complete", styleRed + " 400/5000", string(barFilled)} {
					if !strings.Contains(text, want) {
						t.Fatalf("missing style %q in %q", want, text)
					}
				}
				for _, line := range strings.Split(text, "\x1b[2K") {
					line = escapes.ReplaceAllString(strings.TrimSuffix(line, "\n"), "")
					if len([]rune(line)) >= 60 {
						t.Fatal("escape codes changed wrapping", line)
					}
				}
			case "plain":
				if strings.Contains(plain, string(barFilled)) || !strings.Contains(text, styleGreen) {
					t.Fatal("plain heartbeat lost its ASCII bar or completion colour", text)
				}
			case "failed":
				if !strings.Contains(text, styleBold+styleRed+"✗ Sync failed") {
					t.Fatal("failure was not marked", text)
				}
			}
		})
	}
}

func TestUncoloredProgress(t *testing.T) {
	var output lockedBuffer
	display, err := newDisplay(&output, true)
	if err != nil {
		t.Fatal(err)
	}
	display.Report(Event{Changes: []RepositoryChanges{{Repo: "owner/repo", PullRequests: Tally{Updated: 2}}}})
	if err := display.Finish(nil); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, styleReset) || !strings.Contains(text, "owner/repo: pull requests ~2") {
		t.Fatal("colour leaked or summary missing", text)
	}
}
