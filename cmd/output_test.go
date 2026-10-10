package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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

func TestHumanOutput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  []string
	}{
		{"search", store.SearchResult{Query: store.QueryInfo{Engine: "hybrid"}, Ranking: store.Ranking{Description: "Higher scores rank first."}, Matches: []store.Match{{Repo: "demo/support", Number: 123, Title: "Unicode rendering 日本語\x1b", State: "open", Kind: "issue", URL: "https://github.com/demo/support/issues/123", Score: 0.0321, Summary: &store.TicketSummary{Labels: []string{"client:Acme"}}, Evidence: []store.Evidence{{Field: "comment", Snippet: "Styles disappear after login", Source: "https://github.com/demo/support/issues/123#issuecomment-1"}}}}, HasMore: true, NextCursor: "opaque", Warnings: []store.Warning{{Message: "Comments are stale"}}}, []string{"hybrid search", "demo/support#123", "日本語", "client:Acme", "comment: Styles disappear", "score 0.0321", "--cursor opaque", "Comments are stale"}},
		{"evaluation", store.EvaluationReport{Cases: []store.EvaluationScore{{Name: "Known duplicates", Kind: "candidates", PrecisionAt10: 0.1, RecallAt20: 1, ElapsedMS: 12.5, Returned: 20}}, MeanPrecisionAt10: 0.1, MeanRecallAt20: 1, P50MS: 12.5, P95MS: 20.5}, []string{"Retrieval evaluation", "Known duplicates", "P@10: 10.0%", "R@20: 100.0%", "p50 12.5 ms", "unjudged results are not confirmed negatives"}},
		{"status", store.Status{Issues: 100, Comments: 250, Repositories: []string{"demo/support"}, Semantic: &store.SemanticStatus{Documents: 300, IndexedDocuments: 290, PendingDocuments: 10, Chunks: 400, Compatible: true}}, []string{"100 tickets", "250 comments", "290/300 documents", "10 pending", "compatible: true"}},
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
