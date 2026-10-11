package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestContextLabelBudget(t *testing.T) {
	for _, name := range []string{"compact catalog", "bounded taxonomy", "exhausted budget"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seed(t, db)
			count := 1
			if name != "compact catalog" {
				count = 120
			}
			catalog := make([]json.RawMessage, count)
			for i := range catalog {
				label := fmt.Sprintf("category:%03d", i)
				if i == count-1 {
					label = "bug"
				}
				raw, err := json.Marshal(map[string]any{"id": i + 1, "name": label, "description": strings.Repeat("é", 100), "url": strings.Repeat("x", 8000)})
				if err != nil {
					t.Fatal(err)
				}
				catalog[i] = raw
			}
			if err := db.Update(ctx, func(w *Writer) error {
				if err := w.SetMetadata(ctx, "label_rules", `{"o/r":[{"label":"bug","role":"type","description":"Reported defect"}]}`); err != nil {
					return err
				}
				return w.ReplaceCatalog(ctx, "labels", "o/r", catalog)
			}); err != nil {
				t.Fatal(err)
			}
			o := ContextOptions{Repo: "o/r", Number: 1, Purpose: "classification"}
			if name == "exhausted budget" {
				o.MaxCharacters = 1000
				if err := db.Update(ctx, func(w *Writer) error {
					_, err := w.PutIssue(ctx, "o/r", issueFixture(1, strings.Repeat("body ", 1000), "2026-01-03T00:00:00Z"))
					if err != nil {
						return err
					}
					return w.PutComment(ctx, "o/r", 1, commentFixture(strings.Repeat("comment ", 1000), "2026-01-03T00:00:00Z"))
				}); err != nil {
					t.Fatal(err)
				}
			}
			out, err := db.Context(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := db.Catalog(ctx, "labels", "o/r")
			if err != nil {
				t.Fatal(err)
			}
			if len(stored.Items) != count || !strings.Contains(string(stored.Items[0]), strings.Repeat("x", 8000)) {
				t.Fatal("source catalog metadata was changed")
			}
			if out.Coverage.TextCharacters > out.Coverage.CharacterBudget || out.Coverage.LabelDefinitionsTotal != count || out.Coverage.LabelDefinitionsOmitted != count-len(out.LabelDefinitions) {
				t.Fatal(out.Coverage)
			}
			labelCharacters, warnings := 0, 0
			for _, raw := range out.LabelDefinitions {
				labelCharacters += utf8.RuneCount(raw)
				if strings.Contains(string(raw), `"url"`) || strings.Contains(string(raw), `"id"`) {
					t.Fatal("unneeded GitHub metadata consumes context", string(raw))
				}
			}
			if labelCharacters > out.Coverage.CharacterBudget/3 {
				t.Fatal("taxonomy exceeds its share", labelCharacters)
			}
			for _, warning := range out.Warnings {
				if warning.Code == "context_label_definition_omitted" {
					warnings++
					if !strings.Contains(warning.Message, fmt.Sprintf("%d label definitions", out.Coverage.LabelDefinitionsOmitted)) || !strings.Contains(warning.Message, "catalog --kind labels --scope o/r") {
						t.Fatal(warning)
					}
				}
			}
			switch name {
			case "compact catalog":
				if len(out.LabelDefinitions) != 1 || warnings != 0 || !strings.Contains(string(out.LabelDefinitions[0]), strings.Repeat("é", 100)) {
					t.Fatal(out.LabelDefinitions, out.Coverage, warnings)
				}
			case "bounded taxonomy":
				if warnings != 1 || len(out.LabelRules) != 1 || len(out.LabelDefinitions) == 0 || !strings.Contains(string(out.LabelDefinitions[0]), `"name":"bug"`) {
					t.Fatal(out.Coverage, warnings, out.LabelRules, out.LabelDefinitions)
				}
			case "exhausted budget":
				if warnings != 1 || len(out.LabelDefinitions) != 0 || !out.Coverage.LabelRulesOmitted {
					t.Fatal(out.Coverage, warnings)
				}
			}
		})
	}
}
