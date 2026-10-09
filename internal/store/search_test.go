package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSearchModes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options SearchOptions
		want    int
		fail    bool
	}{
		{"any", SearchOptions{Query: "bug uniqueword"}, 2, false},
		{"all across documents", SearchOptions{Query: "bug uniqueword", Match: "all"}, 1, false},
		{"phrase", SearchOptions{Query: "socket hang", Match: "phrase"}, 1, false},
		{"reversed phrase", SearchOptions{Query: "hang socket", Match: "phrase"}, 0, false},
		{"repeated phrase", SearchOptions{Query: "socket socket", Match: "phrase"}, 0, false},
		{"prefix", SearchOptions{Query: "uniquew", Prefix: true}, 1, false},
		{"exclude across documents", SearchOptions{Query: "network", ExcludeWords: []string{"uniqueword"}}, 1, false},
		{"labels", SearchOptions{Query: "bug", In: []string{"labels"}}, 2, false},
		{"title scope", SearchOptions{Query: "uniqueword", In: []string{"title"}}, 0, false},
		{"title and comments", SearchOptions{Query: "crash uniqueword", Match: "all", In: []string{"title", "comments"}}, 1, false},
		{"bad mode", SearchOptions{Query: "x", Match: "magic"}, 0, true},
		{"bad scope", SearchOptions{Query: "x", In: []string{"sql"}}, 0, true},
		{"long query", SearchOptions{Query: strings.Repeat("word ", 33) + "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty twentyone twentytwo twentythree twentyfour twentyfive twentysix twentyseven twentyeight twentynine thirty thirtyone thirtytwo"}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testStore(t)
			seed(t, db)
			out, err := db.Search(context.Background(), tc.options)
			if tc.fail {
				if !errors.Is(err, ErrInvalidQuery) {
					t.Fatal(out, err)
				}
				return
			}
			if err != nil || len(out.Matches) != tc.want {
				t.Fatal(out, err)
			}
			for _, m := range out.Matches {
				if m.Summary == nil || len(m.Evidence) == 0 || m.Payload != nil {
					t.Fatal("missing compact evidence", m)
				}
			}
		})
	}
}

func TestSearchPages(t *testing.T) {
	for _, sort := range []string{"relevance", "number", "updated", "created"} {
		for _, order := range []string{"asc", "desc"} {
			t.Run(sort+order, func(t *testing.T) {
				db := testStore(t)
				seed(t, db)
				o := SearchOptions{Query: "network", Limit: 1, PageOptions: PageOptions{Sort: sort, Order: order, Count: true, Facets: true}}
				ctx := context.Background()
				a, err := db.Search(ctx, o)
				if err != nil || len(a.Matches) != 1 || !a.HasMore || a.NextCursor == "" || a.Total == nil || *a.Total != 2 || len(a.Facets.Labels) != 1 {
					t.Fatal(a, err)
				}
				o.Cursor = a.NextCursor
				b, err := db.Search(ctx, o)
				if err != nil || len(b.Matches) != 1 || b.HasMore || b.Matches[0].Number == a.Matches[0].Number {
					t.Fatal(b, err)
				}
				o.Label = "new"
				if _, err := db.Search(ctx, o); !errors.Is(err, ErrInvalidQuery) {
					t.Fatal(err)
				}
				o.Label = ""
				if err := db.Update(ctx, func(w *Writer) error { return w.SetMetadata(ctx, "generation", "new") }); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Search(ctx, o); !errors.Is(err, ErrCursorConflict) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestCompoundFilters(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    QueryFilters
		want int
	}{
		{"repositories", QueryFilters{Repositories: []string{"o/r", "o/s"}}, 2},
		{"all labels", QueryFilters{LabelsAll: []string{"bug", "client:Acme"}}, 1},
		{"any labels", QueryFilters{LabelsAny: []string{"missing", "client:Acme"}}, 1},
		{"exclude labels", QueryFilters{ExcludeLabels: []string{"client:Acme"}}, 1},
		{"author", QueryFilters{Author: "someone"}, 1},
		{"assignee", QueryFilters{Assignees: []string{"b"}}, 2},
		{"milestone", QueryFilters{Milestone: "Launch"}, 1},
		{"updated bounds", QueryFilters{UpdatedAfter: "2026-01-03T00:00:00Z"}, 1},
		{"field scalar", QueryFilters{FieldValues: []FieldFilter{{"Cost", "42"}}}, 1},
		{"field option", QueryFilters{FieldValues: []FieldFilter{{"Tier", "Gold"}}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testStore(t)
			seed(t, db)
			ctx := context.Background()
			if err := db.Update(ctx, func(w *Writer) error {
				raw := issueFixture(1, "network failure", "2026-01-03T00:00:00Z")
				var o map[string]any
				if err := json.Unmarshal(raw, &o); err != nil {
					return err
				}
				o["labels"] = []any{map[string]any{"name": "bug"}, map[string]any{"name": "client:Acme"}}
				o["user"] = map[string]any{"login": "someone"}
				o["milestone"] = map[string]any{"title": "Launch", "number": 1}
				raw, err := json.Marshal(o)
				if err != nil {
					return err
				}
				ref, err := w.PutIssue(ctx, "o/r", raw)
				if err != nil {
					return err
				}
				return w.PutExtra(ctx, ref, json.RawMessage(`[{"field":{"name":"Cost"},"value":42},{"field":{"name":"Tier"},"options":[{"name":"Gold"}]}]`), json.RawMessage(`{}`))
			}); err != nil {
				t.Fatal(err)
			}
			listed, err := db.List(ctx, ListOptions{QueryFilters: tc.f})
			if err != nil || len(listed.Issues) != tc.want {
				t.Fatal(listed, err)
			}
			found, err := db.Search(ctx, SearchOptions{Query: "network", QueryFilters: tc.f})
			if err != nil || len(found.Matches) != tc.want {
				t.Fatal(found, err)
			}
		})
	}
}
