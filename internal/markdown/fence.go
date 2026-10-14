// Package markdown recognizes the Markdown block structure that derived evidence relies on.
package markdown

import "strings"

// Fence tracks fenced code blocks across consecutive lines of one document.
type Fence struct {
	mark  byte
	width int
}

// Open reports whether the lines read so far leave a fenced code block open.
func (f *Fence) Open() bool { return f.width > 0 }

// Line advances the tracker and reports whether line opens or closes a fenced code block.
// Leading indentation is accepted so fences nested in list items are recognized. A closing
// fence repeats the opening character at least as many times and carries no info string;
// a backtick opening fence cannot contain backticks after its delimiter, so a line such as
// "```npm ci``` fails" is inline code rather than a fence.
func (f *Fence) Line(line string) bool {
	trim := strings.TrimLeft(line, " \t")
	if len(trim) < 3 || trim[0] != '`' && trim[0] != '~' {
		return false
	}
	width := 0
	for width < len(trim) && trim[width] == trim[0] {
		width++
	}
	if width < 3 {
		return false
	}
	rest := trim[width:]
	if f.width == 0 {
		if trim[0] == '`' && strings.Contains(rest, "`") {
			return false
		}
		f.mark, f.width = trim[0], width
		return true
	}
	if trim[0] == f.mark && width >= f.width && strings.TrimSpace(rest) == "" {
		f.width = 0
		return true
	}
	return false
}
