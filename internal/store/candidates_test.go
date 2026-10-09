package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCandidateTerms(t *testing.T) {
	terms := seedTerms("CSS SSO API 403 panic", "browser browser browser description expected actual", []string{"client:Acme"}, nil)
	for _, term := range []string{"css", "sso", "api", "403", "panic", "acme"} {
		if terms[term] <= 0 {
			t.Fatal("lost meaningful term", term)
		}
	}
	if terms["browser"] != 0 || terms["description"] != 0 {
		t.Fatal("retained boilerplate", terms)
	}
}
func TestCandidateScopeAndComments(t *testing.T) {
	for _, name := range []string{"acronym", "cross repository", "comments", "labels", "exclude labels", "pagination", "empty validation"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seedRaw := json.RawMessage(`{"number":1,"node_id":"seed","title":"CSS regression","body":"browser browser browser description expected actual","labels":[{"name":"client:Acme"}],"state":"open","updated_at":"2026-01-02T00:00:00Z","html_url":"https://github.com/o/r/issues/1"}`)
			if err := db.Update(ctx, func(w *Writer) error {
				if _, err := w.PutIssue(ctx, "o/r", seedRaw); err != nil {
					return err
				}
				for _, row := range []struct {
					repo string
					raw  json.RawMessage
				}{{"o/r", json.RawMessage(`{"number":2,"node_id":"css","title":"CSS rendering","body":"","state":"closed","updated_at":"2026-01-02T00:00:00Z","html_url":"https://github.com/o/r/issues/2"}`)}, {"o/s", json.RawMessage(`{"number":3,"node_id":"cross","title":"CSS rendering","body":"","state":"open","updated_at":"2026-01-02T00:00:00Z","html_url":"https://github.com/o/s/issues/3"}`)}, {"o/r", json.RawMessage(`{"number":4,"node_id":"comment","title":"Other topic","body":"uniquesignal","state":"open","updated_at":"2026-01-02T00:00:00Z","html_url":"https://github.com/o/r/issues/4"}`)}, {"o/r", json.RawMessage(`{"number":5,"node_id":"label","title":"Acme customer","body":"","state":"open","updated_at":"2026-01-02T00:00:00Z","html_url":"https://github.com/o/r/issues/5"}`)}} {
					if _, err := w.PutIssue(ctx, row.repo, row.raw); err != nil {
						return err
					}
				}
				if err := w.PutComment(ctx, "o/r", 1, commentFixture("uniquesignal", "2026-01-02T00:00:00Z")); err != nil {
					return err
				}
				return w.SetMetadata(ctx, "generation", "candidate-test")
			}); err != nil {
				t.Fatal(err)
			}
			o := CandidateOptions{Repo: "o/r", Number: 1}
			want := 2
			switch name {
			case "cross repository":
				o.Repositories = []string{"o/r", "o/s"}
				want = 3
			case "comments":
				o.IncludeComments = true
				want = 3
			case "exclude labels":
				off := false
				o.IncludeLabels = &off
				want = 1
			case "pagination":
				o.Limit = 1
			case "empty validation":
				o.Repositories = []string{"o/empty"}
				o.View = "wrong"
				if _, err := db.FindCandidates(ctx, o); err == nil {
					t.Fatal("invalid empty request accepted")
				}
				return
			}
			out, err := db.FindCandidates(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if name == "pagination" {
				if !out.HasMore || len(out.Matches) != 1 {
					t.Fatal(out)
				}
				o.Cursor = out.NextCursor
				last, err := db.FindCandidates(ctx, o)
				if err != nil || len(last.Matches) != 1 || last.Matches[0].Number == out.Matches[0].Number || last.HasMore {
					t.Fatal(last, err)
				}
				return
			}
			if len(out.Matches) != want {
				t.Fatal(out)
			}
			if !strings.Contains(strings.Join(out.Query.Terms, " "), "css") {
				t.Fatal("lost acronym", out.Query)
			}
			for _, m := range out.Matches {
				if m.Repo == "o/r" && m.Number == 1 {
					t.Fatal("seed leaked", out)
				}
			}
		})
	}
}
