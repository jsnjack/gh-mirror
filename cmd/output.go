package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"gh-mirror/internal/store"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

func resolveOutputFormat(format string, terminal bool) (string, error) {
	switch format {
	case "auto":
		if terminal {
			return "text", nil
		}
		return "json", nil
	case "json", "text":
		return format, nil
	default:
		return "", fmt.Errorf("--format must be auto, text or json")
	}
}

func output(command *cobra.Command, value any) error {
	w := command.OutOrStdout()
	terminal := false
	if file, ok := w.(interface{ Fd() uintptr }); ok {
		terminal = isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd())
	}
	format, err := resolveOutputFormat(outputFormat, terminal)
	if err != nil {
		return err
	}
	if format == "json" {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(value); err != nil {
			return fmt.Errorf("write command output: %w", err)
		}
		return nil
	}
	text, err := humanOutput(value)
	if err != nil {
		return fmt.Errorf("format command output: %w", err)
	}
	if _, err := io.WriteString(w, text); err != nil {
		return fmt.Errorf("write command output: %w", err)
	}
	return nil
}

func humanOutput(value any) (string, error) {
	lines := []string{}
	switch v := value.(type) {
	case store.SearchResult:
		engine := v.Query.Engine
		if engine == "" {
			engine = "lexical"
		}
		lines = append(lines, fmt.Sprintf("%d results | %s search", len(v.Matches), engine), cleanLine(v.Ranking.Description))
		for i, m := range v.Matches {
			lines = append(lines, "", fmt.Sprintf("%d. %s#%d  %s", i+1, cleanLine(m.Repo), m.Number, cleanLine(m.Title)), fmt.Sprintf("   %s | %s | score %.6g", cleanLine(m.State), cleanLine(m.Kind), m.Score), "   "+cleanLine(m.URL))
			if m.Summary != nil && len(m.Summary.Labels) > 0 {
				lines = append(lines, "   Labels: "+cleanLine(strings.Join(m.Summary.Labels, ", ")))
			}
			for _, evidence := range m.Evidence {
				lines = append(lines, "   "+cleanLine(evidence.Field)+": "+preview(evidence.Snippet, 280))
				if evidence.Source != m.URL {
					lines = append(lines, "   Source: "+cleanLine(evidence.Source))
				}
			}
			if len(m.Evidence) == 0 && m.Snippet != "" {
				lines = append(lines, "   "+preview(m.Snippet, 280))
			}
			if len(m.Payload) > 0 {
				full, err := humanFields(map[string]any{"issue": m.Payload, "fields": m.Fields, "extra": m.Extra})
				if err != nil {
					return "", err
				}
				lines = append(lines, full)
			}
		}
		lines = append(lines, resultFooter(v.Total, v.HasMore, v.NextCursor, v.Warnings, v.Facets)...)
	case store.ListResult:
		lines = append(lines, fmt.Sprintf("Tickets: %d", len(v.Issues)))
		for _, ticket := range v.Issues {
			lines = append(lines, ticketLines(ticket)...)
			if len(ticket.Payload) > 0 {
				full, err := humanFields(ticket)
				if err != nil {
					return "", err
				}
				lines = append(lines, full)
			}
		}
		lines = append(lines, resultFooter(v.Total, v.HasMore, v.NextCursor, v.Warnings, v.Facets)...)
	case store.EvaluationReport:
		lines = append(lines, fmt.Sprintf("Retrieval evaluation | %d cases", len(v.Cases)), fmt.Sprintf("Mean P@10: %.1f%% | Mean R@20: %.1f%%", v.MeanPrecisionAt10*100, v.MeanRecallAt20*100), fmt.Sprintf("Latency: p50 %.1f ms | p95 %.1f ms", v.P50MS, v.P95MS), "", fmt.Sprintf("%-28s %-10s %7s %7s %9s %8s", "Case", "Kind", "P@10", "R@20", "Time (ms)", "Returned"))
		for _, c := range v.Cases {
			lines = append(lines, fmt.Sprintf("%-28s %-10s %6.1f%% %6.1f%% %9.1f %8d", preview(c.Name, 28), cleanLine(c.Kind), c.PrecisionAt10*100, c.RecallAt20*100, c.ElapsedMS, c.Returned))
		}
		if v.SearchPrecisionAt10 != nil {
			lines = append(lines, fmt.Sprintf("Search P@10: %.1f%%", *v.SearchPrecisionAt10*100))
		}
		if v.CandidateRecallAt20 != nil {
			lines = append(lines, fmt.Sprintf("Candidate R@20: %.1f%%", *v.CandidateRecallAt20*100))
		}
		lines = append(lines, "", "Metrics use supplied judgments; unjudged results are not confirmed negatives.")
	case store.Status:
		lines = append(lines, fmt.Sprintf("Mirror: %d tickets | %d comments | %d repositories", v.Issues, v.Comments, len(v.Repositories)), "Repositories: "+cleanLine(strings.Join(v.Repositories, ", ")), "Collected: "+cleanLine(v.CollectedAt), "Enriched: "+cleanLine(v.EnrichedAt), fmt.Sprintf("Schema: %d | Collection: %d", v.SchemaVersion, v.CollectionVersion))
		if v.Semantic != nil {
			lines = append(lines, semanticLines(*v.Semantic)...)
		}
		for _, c := range v.Coverage {
			lines = append(lines, fmt.Sprintf("%s | fields: %s | projects: %s | reconciled: %s", cleanLine(c.Repo), cleanLine(c.Fields), cleanLine(c.Projects), cleanLine(c.ReconciledAt)))
		}
	case store.SemanticStatus:
		lines = semanticLines(v)
	default:
		return humanFields(value)
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func semanticLines(s store.SemanticStatus) []string {
	return []string{fmt.Sprintf("Semantic index: %d/%d documents | %d passages | %d pending", s.IndexedDocuments, s.Documents, s.Chunks, s.PendingDocuments), fmt.Sprintf("Model: %s | %d dimensions | compatible: %t", cleanLine(s.Model), s.Dimension, s.Compatible), "Indexed: " + cleanLine(s.IndexedAt)}
}

func ticketLines(t store.Ticket) []string {
	lines := []string{"", fmt.Sprintf("%s#%d | %s", cleanLine(t.Repo), t.Number, cleanLine(t.Kind))}
	if t.Summary != nil {
		lines = append(lines, cleanLine(t.Summary.Title), cleanLine(t.Summary.State)+" | "+cleanLine(t.Summary.URL))
		if len(t.Summary.Labels) > 0 {
			lines = append(lines, "Labels: "+cleanLine(strings.Join(t.Summary.Labels, ", ")))
		}
	}
	return lines
}

func resultFooter(total *int, more bool, cursor string, warnings []store.Warning, facets *store.FacetResult) []string {
	lines := []string{}
	if total != nil {
		lines = append(lines, fmt.Sprintf("\nTotal matching tickets: %d", *total))
	}
	if more {
		lines = append(lines, "\nMore results available. Continue with --cursor "+cleanLine(cursor))
	}
	for _, warning := range warnings {
		lines = append(lines, "Warning: "+cleanLine(warning.Message))
	}
	if facets != nil {
		for _, group := range []struct {
			name  string
			items []store.Bucket
		}{{"Labels", facets.Labels}, {"Types", facets.Types}, {"Projects", facets.Projects}} {
			if len(group.items) == 0 {
				continue
			}
			lines = append(lines, "\n"+group.name+":")
			for _, item := range group.items {
				lines = append(lines, fmt.Sprintf("  %s: %d", cleanLine(item.Value), item.Count))
			}
		}
		if facets.Truncated {
			lines = append(lines, "Facet values are truncated.")
		}
	}
	return lines
}

func cleanLine(text string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)), " ")
}

func preview(text string, limit int) string {
	runes := []rune(cleanLine(text))
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return string(runes)
}

func humanFields(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode readable fields: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		return "", fmt.Errorf("decode readable fields: %w", err)
	}
	return strings.Join(fieldLines(tree, ""), "\n") + "\n", nil
}

func fieldLines(value any, indent string) []string {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		lines := []string{}
		for _, key := range keys {
			name := strings.ReplaceAll(cleanLine(key), "_", " ")
			switch field := v[key].(type) {
			case map[string]any, []any:
				lines = append(lines, indent+name+":")
				lines = append(lines, fieldLines(field, indent+"  ")...)
			case string:
				if strings.Contains(field, "\n") {
					lines = append(lines, indent+name+":")
					for _, line := range strings.Split(field, "\n") {
						lines = append(lines, indent+"  "+cleanLine(line))
					}
				} else {
					lines = append(lines, indent+name+": "+cleanLine(field))
				}
			default:
				lines = append(lines, indent+name+": "+fieldText(field))
			}
		}
		return lines
	case []any:
		if len(v) == 0 {
			return []string{indent + "(none)"}
		}
		lines := []string{}
		for _, item := range v {
			itemLines := fieldLines(item, indent+"  ")
			if len(itemLines) > 0 {
				itemLines[0] = indent + "- " + strings.TrimPrefix(itemLines[0], indent+"  ")
			}
			lines = append(lines, itemLines...)
		}
		return lines
	default:
		return []string{indent + fieldText(v)}
	}
}

func fieldText(value any) string {
	if value == nil {
		return "(none)"
	}
	return cleanLine(fmt.Sprint(value))
}
