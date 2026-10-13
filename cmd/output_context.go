package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gh-mirror/internal/store"
)

func humanContext(v store.ContextResult) (string, error) {
	lines := []string{fmt.Sprintf("%s#%d  %s", cleanLine(v.Ticket.Repo), v.Ticket.Number, cleanLine(v.Summary.Title)), fmt.Sprintf("%s | %s | %s", cleanLine(v.Summary.State), cleanLine(v.Kind), cleanLine(v.Purpose)), cleanLine(v.Summary.URL), "Labels: " + cleanLine(strings.Join(v.Summary.Labels, ", "))}
	if len(v.Summary.Attributes) > 0 {
		lines = append(lines, "", "Label interpretation:")
		for _, a := range v.Summary.Attributes {
			line := fmt.Sprintf("  %s = %s [%s]", cleanLine(a.Role), cleanLine(a.Value), cleanLine(a.Label))
			if a.Description != "" {
				line += " — " + cleanLine(a.Description)
			}
			if len(a.Aliases) > 0 {
				line += " | aliases: " + cleanLine(strings.Join(a.Aliases, ", "))
			}
			lines = append(lines, line)
		}
	}
	lines = append(lines, "", "Description:", cleanText(v.Description.Text))
	if v.Description.Truncated {
		lines = append(lines, "[Description truncated; use get --comments none for the original record.]")
	}
	if len(v.MissingSections) > 0 {
		lines = append(lines, "Section headings absent or empty (observations): "+cleanLine(strings.Join(v.MissingSections, ", ")))
	}
	for _, c := range v.Comments {
		lines = append(lines, "", fmt.Sprintf("%s comment %s | %s | %s", cleanLine(c.Kind), cleanLine(c.ID), cleanLine(c.Author), cleanLine(c.CreatedAt)), cleanLine(c.URL), cleanText(c.Body))
		if c.Truncated {
			lines = append(lines, "[Comment truncated]")
		}
	}
	for _, r := range v.Related {
		title := "[Not collected]"
		if r.Summary != nil {
			title = r.Summary.Title
		}
		lines = append(lines, "", fmt.Sprintf("%s: %s#%d  %s", cleanLine(r.Reference.Kind), cleanLine(r.Reference.Repo), r.Reference.Number, cleanLine(title)))
		if r.Summary != nil {
			lines = append(lines, cleanLine(r.Summary.URL), "Labels: "+cleanLine(strings.Join(r.Summary.Labels, ", ")))
		}
		if r.Description != nil {
			lines = append(lines, cleanText(r.Description.Text))
			if r.Description.Truncated {
				lines = append(lines, "[Related description truncated]")
			}
		}
	}
	if len(v.LabelRules) > 0 {
		lines = append(lines, "", "Configured label rules:")
		for _, r := range v.LabelRules {
			selector := r.Label
			if r.Prefix != "" {
				selector = r.Prefix + "*"
			}
			line := "  " + cleanLine(selector) + " → " + cleanLine(r.Role)
			if r.Value != "" {
				line += " = " + cleanLine(r.Value)
			}
			if r.Description != "" {
				line += " — " + cleanLine(r.Description)
			}
			if len(r.Aliases) > 0 {
				line += " | aliases: " + cleanLine(strings.Join(r.Aliases, ", "))
			}
			lines = append(lines, line)
		}
	}
	if v.Coverage.LabelDefinitionsTotal > 0 {
		lines = append(lines, "", fmt.Sprintf("Label definitions: %d/%d (attached labels first; * = attached)", len(v.LabelDefinitions), v.Coverage.LabelDefinitionsTotal))
		attached := map[string]bool{}
		for _, label := range v.Summary.Labels {
			attached[label] = true
		}
		for _, raw := range v.LabelDefinitions {
			var label struct{ Name, Description string }
			if err := json.Unmarshal(raw, &label); err != nil {
				return "", fmt.Errorf("decode context label definition: %w", err)
			}
			marker := "  "
			if attached[label.Name] {
				marker = "* "
			}
			line := marker + cleanLine(label.Name)
			if label.Description != "" {
				line += " — " + cleanLine(label.Description)
			}
			lines = append(lines, line)
		}
	}
	fields := bytes.TrimSpace(v.Fields)
	if len(fields) > 0 && !bytes.Equal(fields, []byte("[]")) && !bytes.Equal(fields, []byte("{}")) && !bytes.Equal(fields, []byte("null")) {
		text, err := humanFields(map[string]any{"fields": v.Fields})
		if err != nil {
			return "", fmt.Errorf("format context fields: %w", err)
		}
		lines = append(lines, "", text)
	}
	lines = append(lines, "", fmt.Sprintf("Comments: %d/%d | excluded by policy: %d | related omitted: %d | context text: %d/%d characters", v.Coverage.CommentsIncluded, v.Coverage.CommentsTotal, v.Coverage.CommentsExcludedByPolicy, v.Coverage.RelatedOmitted, v.Coverage.TextCharacters, v.Coverage.CharacterBudget))
	for _, warning := range v.Warnings {
		lines = append(lines, "Warning: "+cleanLine(warning.Message))
	}
	return strings.Join(lines, "\n") + "\n", nil
}
