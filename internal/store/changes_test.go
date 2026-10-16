package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestWriterChanges(t *testing.T) {
	pull := json.RawMessage(`{"number":3,"node_id":"PR_3","title":"Fix","body":"","state":"open","updated_at":"2026-01-02T00:00:00Z","html_url":"https://github.com/o/r/pull/3","pull_request":{}}`)
	for _, name := range []string{"unchanged", "added", "updated", "reconciled", "transferred", "scope removed"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seed(t, db)
			var got map[string]Changes
			err := db.Update(ctx, func(w *Writer) error {
				var err error
				switch name {
				case "unchanged":
					_, err = w.PutIssue(ctx, "o/r", issueFixture(1, "network failure", "2026-01-02T00:00:00Z"))
					if err == nil {
						err = w.PutComment(ctx, "o/r", 1, commentFixture("uniqueword socket hang", "2026-01-02T00:00:00Z"))
					}
				case "added":
					if _, err = w.PutIssue(ctx, "o/r", issueFixture(4, "new", "2026-01-03T00:00:00Z")); err == nil {
						_, err = w.PutIssue(ctx, "o/r", pull)
					}
				case "updated":
					_, err = w.PutIssue(ctx, "o/r", issueFixture(1, "edited", "2026-01-03T00:00:00Z"))
					if err == nil {
						err = w.PutComment(ctx, "o/r", 1, commentFixture("edited", "2026-01-03T00:00:00Z"))
					}
				case "reconciled":
					err = w.Reconcile(ctx, "o/r", map[int]bool{2: true}, map[string]bool{})
				case "transferred":
					_, err = w.PutIssue(ctx, "o/s", issueFixture(1, "network failure", "2026-01-03T00:00:00Z"))
				case "scope removed":
					err = w.ResetScope(ctx, []string{"o/s"}, false, false)
				}
				got = w.Changes()
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]Changes{}
			switch name {
			case "added":
				want["o/r"] = Changes{Issues: ChangeCount{Added: 1}, PullRequests: ChangeCount{Added: 1}}
			case "updated":
				want["o/r"] = Changes{Issues: ChangeCount{Updated: 1}, Comments: ChangeCount{Updated: 1}}
			case "reconciled":
				want["o/r"] = Changes{Issues: ChangeCount{Removed: 1}, Comments: ChangeCount{Removed: 1}}
			case "transferred":
				want["o/r"] = Changes{Issues: ChangeCount{Removed: 1}, Comments: ChangeCount{Removed: 1}}
				want["o/s"] = Changes{Issues: ChangeCount{Added: 1}, Comments: ChangeCount{Added: 1}}
			case "scope removed":
				want["o/r"] = Changes{Issues: ChangeCount{Removed: 2}, Comments: ChangeCount{Removed: 1}}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("changes = %+v, want %+v", got, want)
			}
		})
	}
}
