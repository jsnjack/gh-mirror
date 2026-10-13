package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gh-mirror/internal/representation"
	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

func TestOutputFormat(t *testing.T) {
	for _, tc := range []struct {
		name, format, want string
		terminal           bool
	}{
		{"automatic terminal", "auto", "text", true},
		{"automatic pipe", "auto", "json", false},
		{"explicit terminal JSON", "json", "json", true},
		{"explicit piped text", "text", "text", false},
		{"invalid", "yaml", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOutputFormat(tc.format, tc.terminal)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatal(got, err)
			}
		})
	}
}

func TestHumanContext(t *testing.T) {
	for _, name := range []string{"visible evidence", "invalid definition", "absent taxonomy"} {
		t.Run(name, func(t *testing.T) {
			v := store.ContextResult{
				Ticket: store.TicketID{Repo: "o/r", Number: 1}, Kind: "issue", Purpose: "classification",
				Summary:     &store.TicketSummary{Title: "Connection crash", State: "closed", Labels: []string{"customer:Example"}, Attributes: []representation.Attribute{{Label: "customer:Example", Role: "client", Value: "Example", Aliases: []string{"Example Inc"}, Description: "Affected customer"}}},
				Description: store.SourceText{Text: "Résumé\n```sh\nrun --flag\n```"}, MissingSections: []string{"expected"},
				Related:          []store.RelatedTicket{{Reference: representation.Reference{Repo: "o/r", Number: 2, Kind: "parent"}, Summary: &store.TicketSummary{Title: "Customer project", Labels: []string{"client:Example"}}, Description: &store.SourceText{Text: "Parent requirements", Truncated: true}}},
				LabelRules:       []representation.LabelRule{{Prefix: "customer:", Role: "client", Description: "Customer namespace", Aliases: []string{"Example alias"}}},
				LabelDefinitions: []json.RawMessage{json.RawMessage(`{"name":"customer:Example","description":"Reported by this customer\u001b"}`), json.RawMessage(`{"name":"type:bug","description":null}`)},
				Fields:           json.RawMessage(`{"priority":"high"}`), Coverage: store.ContextCoverage{LabelDefinitionsTotal: 4, LabelDefinitionsOmitted: 2, TextCharacters: 300, CharacterBudget: 12000},
				Warnings: []store.Warning{{Code: "context_label_definition_omitted", Message: "2 label definitions omitted"}},
			}
			if name == "invalid definition" {
				v.LabelDefinitions[0] = json.RawMessage(`broken`)
			}
			if name == "absent taxonomy" {
				v.Coverage.LabelDefinitionsTotal = 0
				v.LabelDefinitions = nil
			}
			text, err := humanOutput(v)
			if name == "invalid definition" {
				if err == nil || !strings.Contains(err.Error(), "decode context label definition") {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"client = Example [customer:Example]", "Affected customer", "aliases: Example Inc", "Résumé\n```sh\nrun --flag\n```", "(observations): expected", "parent: o/r#2", "Parent requirements", "[Related description truncated]", "customer:* → client", "Customer namespace", "Example alias", "priority: high", "context text: 300/12000"} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q in %s", want, text)
				}
			}
			if strings.Contains(text, "\x1b") || strings.Count(text, "Warning: 2 label definitions omitted") != 1 {
				t.Fatal(text)
			}
			if name == "visible evidence" {
				for _, want := range []string{"Label definitions: 2/4", "* customer:Example — Reported by this customer", "  type:bug"} {
					if !strings.Contains(text, want) {
						t.Fatalf("missing %q in %s", want, text)
					}
				}
			} else if strings.Contains(text, "Label definitions:") {
				t.Fatal("empty taxonomy section", text)
			}
		})
	}
}

func TestHumanOutput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  []string
	}{
		{"search", store.SearchResult{Query: store.QueryInfo{Engine: "hybrid"}, Ranking: store.Ranking{Description: "Higher scores rank first."}, Matches: []store.Match{{Repo: "demo/support", Number: 123, Title: "Unicode rendering 日本語\x1b", State: "open", Kind: "issue", URL: "https://github.com/demo/support/issues/123", Score: 0.0321, Summary: &store.TicketSummary{Labels: []string{"client:Acme"}}, Evidence: []store.Evidence{{Field: "comment", Snippet: "Styles disappear after login", Source: "https://github.com/demo/support/issues/123#issuecomment-1"}}}}, HasMore: true, NextCursor: "opaque", Warnings: []store.Warning{{Message: "Comments are stale"}}}, []string{"hybrid search", "demo/support#123", "日本語", "client:Acme", "comment: Styles disappear", "score 0.0321", "--cursor opaque", "Comments are stale"}},
		{"evaluation", store.EvaluationReport{Cases: []store.EvaluationScore{{Name: "Known duplicates", Kind: "candidates", PrecisionAt10: 0.1, RecallAt20: 1, ElapsedMS: 12.5, Returned: 20}}, MeanPrecisionAt10: 0.1, MeanRecallAt20: 1, P50MS: 12.5, P95MS: 20.5}, []string{"Retrieval evaluation", "Known duplicates", "P@10: 10.0%", "R@20: 100.0%", "p50 12.5 ms", "unjudged results are not confirmed negatives"}},
		{"status", store.Status{Issues: 100, Comments: 250, Repositories: []string{"demo/support"}, Semantic: &store.SemanticStatus{Documents: 300, IndexedDocuments: 290, PendingDocuments: 10, Chunks: 400, Compatible: true}}, []string{"100 tickets", "250 comments", "290/300 documents", "10 pending", "compatible: true"}},
		{"attributed actor", []store.CommentActor{{Repo: "demo/support", Login: "alice", Type: "User", AccountID: "123", Provider: "claude", Detection: "attribution", Comments: 2, Excluded: 2}}, []string{"alice (User)", "claude", "detection: attribution", "2 comments", "2 excluded"}},
		{"list", store.ListResult{Issues: []store.Ticket{{Repo: "demo/support", Number: 123, Kind: "issue", Summary: &store.TicketSummary{Title: "Connection crash", State: "open", Labels: []string{"client:Acme"}}}}}, []string{"Tickets: 1", "demo/support#123", "Connection crash", "client:Acme"}},
		{"structured fields", map[string]any{"id": json.Number("9007199254740993"), "body": "First paragraph\n\nSecond paragraph", "items": []string{"Acme", "日本語"}}, []string{"id: 9007199254740993", "body:\n  First paragraph\n  \n  Second paragraph", "日本語"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := humanOutput(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q in %s", want, text)
				}
			}
			if strings.Contains(text, "\x1b") {
				t.Fatal("terminal escape retained", text)
			}
		})
	}
}

type failedOutput struct{ err error }

func (w failedOutput) Write([]byte) (int, error) { return 0, w.err }

func TestOutputWrites(t *testing.T) {
	old := outputFormat
	t.Cleanup(func() { outputFormat = old })
	for _, format := range []string{"auto", "json", "text"} {
		t.Run(format, func(t *testing.T) {
			outputFormat = format
			var buffer bytes.Buffer
			command := &cobra.Command{}
			command.SetOut(&buffer)
			value := map[string]any{"id": json.Number("9007199254740993")}
			if err := output(command, value); err != nil {
				t.Fatal(err)
			}
			if format == "text" {
				if !strings.Contains(buffer.String(), "id: 9007199254740993") {
					t.Fatal(buffer.String())
				}
			} else if !json.Valid(buffer.Bytes()) || !strings.Contains(buffer.String(), "9007199254740993") {
				t.Fatal("piped JSON contract changed", buffer.String())
			}
			failure := errors.New("fixture broken pipe")
			command.SetOut(failedOutput{failure})
			if err := output(command, value); !errors.Is(err, failure) {
				t.Fatal("output failure lost", err)
			}
		})
	}
}
