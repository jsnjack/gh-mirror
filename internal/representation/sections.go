package representation

import (
	"regexp"
	"strings"

	"gh-mirror/internal/markdown"
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

// Sections recognizes headings outside fenced code without rewriting source text.
func Sections(body string) []Section {
	out := []Section{}
	current := Section{Kind: "description", Start: 0}
	offset := 0
	var fence markdown.Fence
	for _, line := range strings.SplitAfter(body, "\n") {
		if !fence.Line(line) && !fence.Open() {
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
