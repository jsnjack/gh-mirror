// Package commentpolicy selects locally retained comments for ticket retrieval.
package commentpolicy

import (
	"fmt"
	"strings"
	"unicode"
)

// RegistryVersion identifies the bundled AI reviewer account registry.
const RegistryVersion = "1"

// Policy controls PR comment retrieval while leaving source records intact.
type Policy struct {
	ExcludeKnownAI   bool     `json:"exclude_known_ai,omitempty"`
	ExcludeAIThreads bool     `json:"exclude_ai_threads,omitempty"`
	ExcludeAllBots   bool     `json:"exclude_all_bots,omitempty"`
	ExcludeAuthors   []string `json:"exclude_authors,omitempty"`
	IncludeAuthors   []string `json:"include_authors,omitempty"`
}

// Override distinguishes an inherited switch from an explicit false value.
type Override struct {
	ExcludeKnownAI   *bool     `json:"exclude_known_ai,omitempty"`
	ExcludeAIThreads *bool     `json:"exclude_ai_threads,omitempty"`
	ExcludeAllBots   *bool     `json:"exclude_all_bots,omitempty"`
	ExcludeAuthors   *[]string `json:"exclude_authors,omitempty"`
	IncludeAuthors   *[]string `json:"include_authors,omitempty"`
}

// Resolve applies repository overrides, replacing explicitly supplied author lists.
func (p Policy) Resolve(o Override) Policy {
	if o.ExcludeKnownAI != nil {
		p.ExcludeKnownAI = *o.ExcludeKnownAI
	}
	if o.ExcludeAIThreads != nil {
		p.ExcludeAIThreads = *o.ExcludeAIThreads
	}
	if o.ExcludeAllBots != nil {
		p.ExcludeAllBots = *o.ExcludeAllBots
	}
	if o.ExcludeAuthors != nil {
		p.ExcludeAuthors = *o.ExcludeAuthors
	}
	if o.IncludeAuthors != nil {
		p.IncludeAuthors = *o.IncludeAuthors
	}
	return p
}

// Validate rejects empty names, wildcard guesses and ambiguous duplicate entries.
func (p Policy) Validate() error {
	for _, list := range [][]string{p.ExcludeAuthors, p.IncludeAuthors} {
		seen := map[string]bool{}
		for _, author := range list {
			name := strings.ToLower(author)
			if author == "" || strings.ContainsAny(author, "*/\\") || strings.IndexFunc(author, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 || seen[name] {
				return fmt.Errorf("comment policy requires distinct exact author logins: %q", author)
			}
			seen[name] = true
		}
	}
	return nil
}

// Author preserves GitHub account identity independently of comment prose.
type Author struct {
	Login string
	Type  string
	ID    string
	Host  string
}

// KnownAI recognizes bundled reviewer identities; account IDs are GitHub-host scoped.
func KnownAI(a Author) string {
	if !strings.EqualFold(a.Type, "Bot") {
		return ""
	}
	if strings.EqualFold(a.Host, "github.com") && (a.ID == "175728472" || a.ID == "198982749") {
		return "copilot"
	}
	name := strings.TrimSuffix(strings.ToLower(a.Login), "[bot]")
	switch name {
	case "copilot", "copilot-pull-request-reviewer":
		return "copilot"
	case "coderabbitai":
		return "coderabbit"
	case "gemini-code-assist":
		return "gemini"
	default:
		return ""
	}
}

func contains(list []string, name string) bool {
	for _, v := range list {
		if strings.EqualFold(v, name) {
			return true
		}
	}
	return false
}

// Decision explains an excluded comment or inherited thread exclusion.
type Decision struct {
	Reason   string `json:"reason"`
	Provider string `json:"provider,omitempty"`
	Root     string `json:"root,omitempty"`
}

func (p Policy) excluded(a Author) Decision {
	if contains(p.IncludeAuthors, a.Login) {
		return Decision{}
	}
	provider := KnownAI(a)
	if contains(p.ExcludeAuthors, a.Login) {
		return Decision{Reason: "excluded_author", Provider: provider}
	}
	if p.ExcludeKnownAI && provider != "" {
		return Decision{Reason: "known_ai", Provider: provider}
	}
	if p.ExcludeAllBots && strings.EqualFold(a.Type, "Bot") {
		return Decision{Reason: "bot", Provider: provider}
	}
	return Decision{}
}

// Comment identifies an inline parent within the same repository and ticket.
type Comment struct {
	ID     string
	Repo   string
	Number int
	Parent string
	Author Author
}

// Select resolves same-ticket reply chains and explicit author exceptions.
func Select(comments []Comment, policies map[string]Policy) (map[string]Decision, error) {
	byID := make(map[string]Comment, len(comments))
	for _, c := range comments {
		byID[c.ID] = c
	}
	roots := map[string]string{}
	var root func(string, map[string]bool) (string, error)
	root = func(id string, path map[string]bool) (string, error) {
		if cached, ok := roots[id]; ok {
			return cached, nil
		}
		if path[id] {
			return "", fmt.Errorf("cycle in review reply chain at comment %s", id)
		}
		path[id] = true
		c := byID[id]
		parent, found := byID[c.Parent]
		resolved := id
		if found && parent.Repo == c.Repo && parent.Number == c.Number {
			var err error
			resolved, err = root(parent.ID, path)
			if err != nil {
				return "", err
			}
		}
		delete(path, id)
		roots[id] = resolved
		return resolved, nil
	}
	out := map[string]Decision{}
	for _, c := range comments {
		p := policies[c.Repo]
		if contains(p.IncludeAuthors, c.Author.Login) {
			continue
		}
		d := p.excluded(c.Author)
		if p.ExcludeAIThreads && strings.HasPrefix(c.ID, "review:") {
			id, err := root(c.ID, map[string]bool{})
			if err != nil {
				return nil, err
			}
			if decision := p.excluded(byID[id].Author); decision.Reason != "" {
				d = Decision{Reason: "excluded_thread", Provider: decision.Provider, Root: id}
			}
		}
		if d.Reason != "" {
			out[c.ID] = d
		}
	}
	return out, nil
}
