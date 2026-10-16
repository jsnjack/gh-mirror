package progress

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ColorMode selects ANSI styling for progress output.
type ColorMode int

const (
	// ColorAuto styles animated terminal output unless NO_COLOR is set.
	ColorAuto ColorMode = iota
	// ColorAlways styles terminal frames and plain status lines.
	ColorAlways
	// ColorNever writes no styling.
	ColorNever
)

// ParseColor reads a --color flag value.
func ParseColor(value string) (ColorMode, error) {
	switch value {
	case "auto":
		return ColorAuto, nil
	case "always":
		return ColorAlways, nil
	case "never":
		return ColorNever, nil
	}
	return ColorAuto, fmt.Errorf("--color must be auto, always or never, got %q", value)
}

func (m ColorMode) enabled(interactive bool) bool {
	switch m {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	}
	return interactive && os.Getenv("NO_COLOR") == ""
}

const (
	styleReset  = "\x1b[0m"
	styleBold   = "\x1b[1m"
	styleDim    = "\x1b[2m"
	styleRed    = "\x1b[31m"
	styleGreen  = "\x1b[32m"
	styleYellow = "\x1b[33m"
	styleCyan   = "\x1b[36m"
)

// tone names how a row is styled. Styling runs after wrapping, so escape codes never
// count toward the terminal width.
type tone int

const (
	tonePlain tone = iota
	toneHeader
	toneSuccess
	toneFailure
	toneCancelled
	toneActivity
	toneBar
	toneLabel
	toneLow
	toneCritical
	toneWarn
	toneMuted
	toneChanges
)

type row struct {
	text string
	tone tone
}

func paint(style, text string) string {
	if text == "" {
		return text
	}
	return style + text + styleReset
}

// styleLine colours one wrapped line of a row; first marks the line holding its label.
func styleLine(t tone, line string, first bool) string {
	switch t {
	case toneHeader:
		return paint(styleBold, line)
	case toneSuccess:
		return paint(styleBold+styleGreen, line)
	case toneFailure:
		return paint(styleBold+styleRed, line)
	case toneCancelled:
		return paint(styleBold+styleYellow, line)
	case toneWarn:
		return paint(styleYellow, line)
	case toneMuted:
		return paint(styleDim, line)
	case toneActivity:
		spinner, rest, ok := strings.Cut(line, " ")
		if !first || !ok {
			return line
		}
		phase, details, more := strings.Cut(rest, " | ")
		out := paint(styleCyan, spinner) + " " + paint(styleBold, phase)
		if more {
			out += " | " + details
		}
		return out
	case toneBar:
		return paintBar(line)
	case toneLabel, toneLow, toneCritical, toneChanges:
		label, value := "", line
		if first {
			if l, v, ok := strings.Cut(line, ": "); ok {
				label, value = l+":", " "+v
			}
		}
		switch t {
		case toneLow:
			value = paint(styleYellow, value)
		case toneCritical:
			value = paint(styleRed, value)
		case toneChanges:
			value = paintTallies(value)
		}
		if t == toneChanges {
			return paint(styleBold, label) + value
		}
		return paint(styleDim, label) + value
	}
	return line
}

// paintBar colours runs of filled and empty bar cells.
func paintBar(line string) string {
	var b strings.Builder
	runes := []rune(line)
	for i := 0; i < len(runes); {
		j := i
		for j < len(runes) && runes[j] == runes[i] {
			j++
		}
		run := string(runes[i:j])
		switch runes[i] {
		case barFilled:
			run = paint(styleGreen, run)
		case barEmpty:
			run = paint(styleDim, run)
		}
		b.WriteString(run)
		i = j
	}
	return b.String()
}

// paintTallies colours +added, ~updated and -removed counts.
func paintTallies(value string) string {
	if strings.TrimSpace(value) == noChanges {
		return paint(styleDim, value)
	}
	words := strings.Split(value, " ")
	for i, word := range words {
		if len(word) < 2 {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimRight(word[1:], ",")); err != nil {
			continue
		}
		switch word[0] {
		case '+':
			words[i] = paint(styleGreen, word)
		case '~':
			words[i] = paint(styleCyan, word)
		case '-':
			words[i] = paint(styleRed, word)
		}
	}
	return strings.Join(words, " ")
}

// quotaTone colours remaining quota below a quarter and a tenth of the limit.
func quotaTone(remaining, limit string) tone {
	left, err := strconv.Atoi(remaining)
	if err != nil {
		return toneLabel
	}
	total, err := strconv.Atoi(limit)
	if err != nil || total <= 0 {
		return toneLabel
	}
	switch {
	case left*10 < total:
		return toneCritical
	case left*4 < total:
		return toneLow
	}
	return toneLabel
}
