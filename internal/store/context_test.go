package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestContext(t *testing.T) {
	for _, name := range []string{"exact original", "budget and coverage", "relationships", "classification metadata", "classification catalog", "invalid options", "missing ticket"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seed(t, db)
			body := "### Description\nRésumé fails; duplicate of #2; mention #9.\n### Expected behavior\n_No response_\n### Steps to reproduce\n```sh\nrun --flag\n```\n" + strings.Repeat("α", 1200)
			if err := db.Update(ctx, func(w *Writer) error {
				raw := issueFixture(1, body, "2026-01-03T00:00:00Z")
				var obj map[string]any
				if err := json.Unmarshal(raw, &obj); err != nil {
					return err
				}
				obj["labels"] = []map[string]string{{"name": "customer:Acme"}, {"name": "client:Other"}}
				obj["state_reason"] = "duplicate"
				obj["pull_request"] = map[string]string{"merged_at": "2026-01-03T00:00:00Z"}
				encoded, err := json.Marshal(obj)
				if err != nil {
					return err
				}
				if _, err := w.PutIssue(ctx, "o/r", encoded); err != nil {
					return err
				}
				if err := w.SetMetadata(ctx, "label_rules", `{"o/r":[{"prefix":"customer:","role":"client","description":"Customer affected"}]}`); err != nil {
					return err
				}
				return w.PutExtra(ctx, IssueRef{Repo: "o/r", Number: 1}, json.RawMessage(`[]`), json.RawMessage(`{"parent":{"number":2,"repository":{"nameWithOwner":"o/r"}},"subIssues":[],"blocking":[],"blockedBy":[]}`))
			}); err != nil {
				t.Fatal(err)
			}
			o := ContextOptions{Repo: "o/r", Number: 1, Query: "uniqueword"}
			if name == "budget and coverage" {
				o.MaxCharacters = 1000
				o.Purpose = "proofread"
			}
			if name == "classification catalog" {
				o.Purpose = "classification"
				if err := db.Update(ctx, func(w *Writer) error {
					return w.ReplaceCatalog(ctx, "labels", "o/r", []json.RawMessage{json.RawMessage(`{"id":1,"name":"unused:label","description":"Available classification"}`)})
				}); err != nil {
					t.Fatal(err)
				}
			}
			if name == "invalid options" {
				o.Purpose = "invented"
			}
			if name == "missing ticket" {
				o.Number = 99
			}
			out, err := db.Context(ctx, o)
			if name == "invalid options" {
				if !errors.Is(err, ErrInvalidQuery) {
					t.Fatal(err)
				}
				return
			}
			if name == "missing ticket" {
				if !errors.Is(err, ErrNotFound) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "classification catalog":
				if len(out.LabelDefinitions) != 1 || out.Coverage.LabelDefinitionsTotal != 1 || len(out.LabelRules) != 1 {
					t.Fatal(out.LabelDefinitions, out.LabelRules, out.Coverage)
				}
			case "exact original":
				if out.Description.Text != body || out.Description.End != len(body) || !strings.Contains(out.Comments[0].Body, "uniqueword") {
					t.Fatal(out.Description, out.Comments)
				}
			case "budget and coverage":
				if !out.Description.Truncated || out.Coverage.TextCharacters > 1000 || out.Coverage.CommentsOmitted != 1 || out.Description.End != len(out.Description.Text) {
					t.Fatal(out.Coverage, out.Description)
				}
			case "relationships":
				if len(out.Related) != 2 || len(out.Relationships) != 3 || out.Relationships[0].Kind != "parent" || !out.Related[1].Missing {
					t.Fatal(out.Relationships, out.Related)
				}
			case "classification metadata":
				if out.Summary.StateReason != "duplicate" || out.Summary.MergedAt == "" || out.Summary.Attributes[0].Role != "client" || out.Summary.Attributes[0].Description == "" || len(out.MissingSections) == 0 {
					t.Fatal(out.Summary, out.MissingSections)
				}
			}
		})
	}
}
