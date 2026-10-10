package representation

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Reference records an explicit source reference; mentions do not imply duplicates.
type Reference struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Start  int    `json:"start,omitempty"`
	End    int    `json:"end,omitempty"`
}

var ticketReference = regexp.MustCompile(`https?://[^\s<>\[\]()]+/(?:issues|pull)/[0-9]+|[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#[0-9]+|#[0-9]+`)
var duplicatePrefix = regexp.MustCompile(`(?i)(?:duplicate\s+(?:of\s+)?|duplicates\s+)$`)
var fixPrefix = regexp.MustCompile(`(?i)(?:fix(?:es|ed)?|resolv(?:es|ed)|clos(?:es|ed))\s+$`)

// References extracts references outside fenced code with conservative typed provenance.
func References(repo, body, source string) []Reference {
	out := []Reference{}
	// Ignore code fences rather than interpreting issue-like log fragments as links.
	code := []Span{}
	offset, start := 0, 0
	var fence codeFence
	for _, line := range strings.SplitAfter(body, "\n") {
		wasOpen := fence.width > 0
		if fence.line(line) {
			if !wasOpen {
				start = offset
			} else if fence.width == 0 {
				code = append(code, Span{start, offset + len(line)})
			}
		}
		offset += len(line)
	}
	if fence.width > 0 {
		code = append(code, Span{start, len(body)})
	}
	for _, loc := range ticketReference.FindAllStringIndex(body, -1) {
		inside := false
		for _, span := range code {
			if loc[0] >= span.Start && loc[0] < span.End {
				inside = true
			}
		}
		if inside {
			continue
		}
		text, target, number := body[loc[0]:loc[1]], repo, 0
		if strings.HasPrefix(text, "http") {
			u, err := url.Parse(text)
			if err != nil {
				continue
			}
			origin, err := url.Parse(source)
			if err != nil || (u.Host != "github.com" && u.Host != origin.Host) {
				continue
			}
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) != 4 {
				continue
			}
			target = parts[0] + "/" + parts[1]
			parsed, err := strconv.Atoi(parts[3])
			if err != nil {
				continue
			}
			number = parsed
		} else {
			name, n, _ := strings.Cut(text, "#")
			if name != "" {
				target = name
			}
			parsed, err := strconv.Atoi(n)
			if err != nil {
				continue
			}
			number = parsed
		}
		if number < 1 {
			continue
		}
		kind := "mention"
		prefix := body[max(0, loc[0]-64):loc[0]]
		if duplicatePrefix.MatchString(prefix) {
			kind = "duplicate_reference"
		} else if fixPrefix.MatchString(prefix) {
			kind = "fix_reference"
		}
		out = append(out, Reference{Repo: target, Number: number, Kind: kind, Source: source, Start: loc[0], End: loc[1]})
	}
	return out
}
