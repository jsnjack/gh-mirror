package representation

import (
	"regexp"
	"strings"
)

// Section points to exact UTF-8 byte ranges in the original Markdown body.
type Section struct {
	Kind        string `json:"kind"`
	Heading     string `json:"heading,omitempty"`
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Empty       bool   `json:"empty"`
	Boilerplate bool   `json:"boilerplate"`
}

var headingLine = regexp.MustCompile(`^ {0,3}#{1,6}\s+(.+?)\s*#*\s*$`)

type codeFence struct {
	mark  byte
	width int
}

func (f *codeFence) line(line string) bool {
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
	if f.width == 0 {
		f.mark, f.width = trim[0], width
		return true
	}
	if trim[0] == f.mark && width >= f.width && strings.TrimSpace(trim[width:]) == "" {
		f.width = 0
		return true
	}
	return false
}

// Sections recognizes headings outside fenced code without rewriting source text.
func Sections(body string) []Section {
	out := []Section{}
	current := Section{Kind: "description", Start: 0}
	offset := 0
	var fence codeFence
	for _, line := range strings.SplitAfter(body, "\n") {
		if !fence.line(line) && fence.width == 0 {
			if match := headingLine.FindStringSubmatch(strings.TrimRight(line, "\r\n")); match != nil {
				current.End = offset
				if current.Start < current.End || current.Heading != "" {
					out = append(out, current)
				}
				current = Section{Kind: sectionKind(match[1]), Heading: match[1], Start: offset + len(line)}
			}
		}
		offset += len(line)
	}
	current.End = len(body)
	if current.Start < current.End || current.Heading != "" {
		out = append(out, current)
	}
	for i := range out {
		s := &out[i]
		text := strings.TrimSpace(body[s.Start:s.End])
		s.Empty = text == "" || strings.EqualFold(text, "_No response_") || strings.EqualFold(text, "No response")
		s.Boilerplate = s.Kind == "checklist" || s.Kind == "template"
	}
	return out
}

func sectionKind(heading string) string {
	h := strings.ToLower(strings.TrimSpace(heading))
	switch {
	case strings.Contains(h, "before requesting"), strings.Contains(h, "pr is ready"), strings.Contains(h, "checklist"):
		return "checklist"
	case strings.Contains(h, "reported via"):
		return "template"
	case strings.Contains(h, "acceptance"):
		return "acceptance"
	case strings.Contains(h, "expected"):
		return "expected"
	case strings.Contains(h, "actual"), strings.Contains(h, "observed"):
		return "actual"
	case strings.Contains(h, "repro"), strings.Contains(h, "steps"):
		return "reproduction"
	case strings.Contains(h, "environment"), strings.Contains(h, "browser"), strings.Contains(h, "os details"):
		return "environment"
	case strings.Contains(h, "description"):
		return "description"
	default:
		return "other"
	}
}

// Span identifies an original source range that is useful for derived retrieval.
type Span struct{ Start, End int }

// RetrievalSpans excludes empty sections and known template checklists, preserving code.
func RetrievalSpans(body string) []Span {
	out := []Span{}
	for _, s := range Sections(body) {
		if !s.Empty && !s.Boilerplate {
			out = append(out, Span{Start: s.Start, End: s.End})
		}
	}
	return out
}
