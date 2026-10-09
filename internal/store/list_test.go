package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestList(t *testing.T) {
	for _, name := range []string{"all", "pagination", "filters", "no matches", "archived project", "user project", "wrong owner", "project search", "wrong kind", "invalid limit", "invalid state", "invalid kind", "invalid project", "invalid cursor", "filter mismatch", "generation mismatch"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seed(t, db)
			if err := db.Update(ctx, func(w *Writer) error {
				if _, err := w.PutIssue(ctx, "o/s", issueFixture(3, "network failure", "2026-01-02T00:00:00Z")); err != nil {
					return err
				}
				return w.PutExtra(ctx, IssueRef{Repo: "o/r", Number: 1}, json.RawMessage(`[]`), json.RawMessage(`{"projectItems":[{"isArchived":true,"project":{"id":"P_1","number":1,"url":"https://github.enterprise.test/orgs/o/projects/1"}},{"project":{"id":"P_2","number":2,"url":"https://github.com/users/u/projects/2"}}]}`))
			}); err != nil {
				t.Fatal(err)
			}
			options := ListOptions{}
			want, fail := 3, false
			switch name {
			case "pagination", "filter mismatch", "generation mismatch":
				first, err := db.List(ctx, ListOptions{Limit: 1})
				if err != nil || len(first.Issues) != 1 || first.Issues[0].Number != 1 || first.NextCursor == "" {
					t.Fatal("missing first page", first, err)
				}
				options.Cursor, options.Limit = first.NextCursor, 1
				want = 1
				if name == "filter mismatch" {
					options.Label, fail = "bug", true
				}
				if name == "generation mismatch" {
					if err := db.Update(ctx, func(w *Writer) error { return w.SetMetadata(ctx, "generation", "changed") }); err != nil {
						t.Fatal(err)
					}
					fail = true
				}
			case "filters":
				options.Repo, options.State, options.Label, options.Type, options.Kind = "o/r", "closed", "bug", "Bug", "issue"
				want = 2
			case "no matches":
				options.Label, want = "unused", 0
			case "archived project", "project search":
				options.Project, want = "o/1", 1
			case "user project":
				options.Project, want = "u/2", 1
			case "wrong owner":
				options.Project, want = "other/1", 0
			case "wrong kind":
				options.Kind, want = "pull_request", 0
			case "invalid limit":
				options.Limit, fail = 101, true
			case "invalid state":
				options.State, fail = "invalid", true
			case "invalid kind":
				options.Kind, fail = "invalid", true
			case "invalid project":
				options.Project, fail = "o/1 OR true", true
			case "invalid cursor":
				options.Cursor, fail = "bad", true
			}
			out, err := db.List(ctx, options)
			if fail {
				if err == nil {
					t.Fatal("accepted invalid listing", out)
				}
				return
			}
			if err != nil || len(out.Issues) != want {
				t.Fatal("incorrect listing", out, err)
			}
			if out.Status.Generation != "testgeneration" {
				t.Fatal("listing lost generation", out)
			}
			if name == "pagination" {
				if out.Issues[0].Number != 2 || out.NextCursor == "" {
					t.Fatal("skipped or repeated ticket", out)
				}
				options.Cursor = out.NextCursor
				last, err := db.List(ctx, options)
				if err != nil || len(last.Issues) != 1 || last.Issues[0].Repo != "o/s" || last.Issues[0].Number != 3 || last.NextCursor != "" {
					t.Fatal("incorrect last page", last, err)
				}
			}
			if name == "project search" {
				found, err := db.Search(ctx, SearchOptions{Query: "network", Project: options.Project})
				if err != nil || len(found.Matches) != 1 || found.Matches[0].Number != 1 {
					t.Fatal("search ignored project membership", found, err)
				}
			}
			if want > 0 && (!json.Valid(out.Issues[0].Payload) || !json.Valid(out.Issues[0].Extra)) {
				t.Fatal("listing omitted raw records", out)
			}
		})
	}
}
