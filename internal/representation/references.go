package representation

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"gh-mirror/internal/markdown"
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

// URLs are matched whole so fragments and path segments inside them are never read as
// shorthand references.
var ticketReference = regexp.MustCompile(`https?://[^\s<>\[\]()]+|` + owner + `/[A-Za-z0-9_.-]{1,100}#[0-9]+|#[0-9]+`)
var ticketURL = regexp.MustCompile(`^https?://[^/\s]+/(` + owner + `)/([A-Za-z0-9_.-]{1,100})/(?:issues|pull)/([0-9]+)(?:[^A-Za-z0-9_]|$)`)

const owner = `[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?`

var duplicatePrefix = regexp.MustCompile(`(?i)(?:duplicate\s+(?:of\s+)?|duplicates\s+)$`)
var fixPrefix = regexp.MustCompile(`(?i)(?:fix(?:es|ed)?|resolv(?:es|ed)|clos(?:es|ed))\s+$`)

// References extracts references outside fenced code with conservative typed provenance.
// Shorthand references must not follow a word character, slash, colon or dot, which rejects
// version strings such as 1.2/3.4#5. URLs count only as GitHub issue or pull request links.
func References(repo, body, source string) []Reference {
	out := []Reference{}
	// Ignore code fences rather than interpreting issue-like log fragments as links.
	code := []Span{}
	offset, start := 0, 0
	var fence markdown.Fence
	for _, line := range strings.SplitAfter(body, "\n") {
		wasOpen := fence.Open()
		if fence.Line(line) {
			if !wasOpen {
				start = offset
			} else if !fence.Open() {
				code = append(code, Span{start, offset + len(line)})
			}
		}
		offset += len(line)
	}
	if fence.Open() {
		code = append(code, Span{start, len(body)})
	}
	origin, err := url.Parse(source)
	if err != nil {
		origin = &url.URL{}
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
		text, target, digits := body[loc[0]:loc[1]], repo, ""
		if strings.HasPrefix(text, "http") {
			match := ticketURL.FindStringSubmatchIndex(text)
			if match == nil {
				continue
			}
			u, err := url.Parse(text[:match[7]])
			if err != nil || (u.Host != "github.com" && u.Host != origin.Host) {
				continue
			}
			target = text[match[2]:match[3]] + "/" + text[match[4]:match[5]]
			digits = text[match[6]:match[7]]
			loc[1] = loc[0] + match[7]
		} else {
			if loc[0] > 0 && shorthandBoundary(body[loc[0]-1]) {
				continue
			}
			name, n, _ := strings.Cut(text, "#")
			if name != "" {
				target = name
			}
			digits = n
		}
		number, err := strconv.Atoi(digits)
		if err != nil || number < 1 {
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

func shorthandBoundary(b byte) bool {
	return b == '/' || b == ':' || b == '.' || b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}
