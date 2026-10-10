// Package representation derives ticket evidence without changing upstream records.
package representation

import (
	"fmt"
	"strings"
)

// Version identifies local extraction and embedding inputs independently of collection.
const Version = "1"

// LabelRule assigns a role to an exact label or a namespace prefix.
type LabelRule struct {
	Label       string   `json:"label,omitempty"`
	Prefix      string   `json:"prefix,omitempty"`
	Role        string   `json:"role"`
	Value       string   `json:"value,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
	Description string   `json:"description,omitempty"`
}

// Attribute preserves the label and its derived role, value and interpretation.
type Attribute struct {
	Label       string   `json:"label"`
	Role        string   `json:"role"`
	Value       string   `json:"value"`
	Aliases     []string `json:"aliases,omitempty"`
	Description string   `json:"description,omitempty"`
}

// ValidateRules rejects ambiguous selectors and missing roles.
func ValidateRules(rules map[string][]LabelRule) error {
	for repo, list := range rules {
		seen := map[string]bool{}
		for _, r := range list {
			if (r.Label == "") == (r.Prefix == "") || strings.TrimSpace(r.Role) == "" {
				return fmt.Errorf("label rules for %s require one label or prefix and a role", repo)
			}
			key := r.Label + "\x00" + r.Prefix
			if seen[key] {
				return fmt.Errorf("duplicate label rule for %s", repo)
			}
			seen[key] = true
		}
	}
	return nil
}

// Attributes maps every attached label, keeping multiple values and unknown labels.
func Attributes(labels []string, rules []LabelRule) []Attribute {
	out := make([]Attribute, 0, len(labels))
	for _, label := range labels {
		prefix, value, found := strings.Cut(label, ":")
		a := Attribute{Label: label, Role: "label", Value: label}
		if found && strings.TrimSpace(value) != "" {
			a.Role, a.Value = strings.ToLower(prefix), strings.TrimSpace(value)
			switch a.Role {
			case "comp", "component":
				a.Role = "component"
			case "imp", "priority", "importance":
				a.Role = "importance"
			case "eff":
				a.Role = "effort"
			}
		}
		// Exact rules take precedence over prefixes; longest prefixes win.
		best := -1
		for _, r := range rules {
			weight := len(r.Prefix)
			matches := r.Prefix != "" && strings.HasPrefix(label, r.Prefix)
			if r.Label != "" {
				matches, weight = label == r.Label, len(label)+1
			}
			if !matches || weight <= best {
				continue
			}
			best = weight
			a.Role, a.Value, a.Aliases, a.Description = r.Role, r.Value, r.Aliases, r.Description
			if a.Value == "" {
				a.Value = strings.TrimSpace(strings.TrimPrefix(label, r.Prefix))
			}
		}
		out = append(out, a)
	}
	return out
}
