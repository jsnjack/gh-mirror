package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func TestWholeTicketHybridRanking(t *testing.T) {
	for _, name := range []string{"split evidence", "cross-client retained", "field scopes", "aliases", "cursor version"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			if err := db.Update(ctx, func(w *Writer) error {
				for _, v := range []struct {
					number             int
					title, body, label string
				}{{1, "Session continuation failure", "Browser cookies fail", "client:Other"}, {2, "Cookie transfer", "session continuation fails after transfer", "customer:Acme"}, {3, "Billing invoice", "Monthly statement", "customer:Acme"}, {4, "Session continuation failure", "Browser cookies fail too", "client:Different"}} {
					raw := json.RawMessage(fmt.Sprintf(`{"number":%d,"node_id":"R_%d","title":%q,"body":%q,"state":"open","updated_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/o/r/issues/%d","labels":[{"name":%q}]}`, v.number, v.number, v.title, v.body, v.number, v.label))
					if _, err := w.PutIssue(ctx, "o/r", raw); err != nil {
						return err
					}
				}
				return w.SetMetadata(ctx, "label_rules", `{"o/r":[{"prefix":"customer:","role":"client","aliases":["ExampleCo"]}]}`)
			}); err != nil {
				t.Fatal(err)
			}
			e := &fakeVectorizer{}
			if _, err := db.Index(ctx, e, IndexOptions{Workers: 1}); err != nil {
				t.Fatal(err)
			}
			db.vectorizer = e
			o := SearchOptions{Query: "acme session continuation issue", Limit: 4, Engine: "hybrid"}
			if name == "field scopes" {
				o.In = []string{"body"}
			}
			if name == "aliases" {
				o.Query = "ExampleCo session continuation"
			}
			if name == "cursor version" {
				o.Limit = 1
			}
			out, err := db.Search(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if name == "field scopes" {
				if len(out.Query.Entities) != 0 {
					t.Fatal(out.Query)
				}
				return
			}
			if out.Matches[0].Number != 2 {
				t.Fatal("combined client and topic evidence lost", out.Matches)
			}
			if name == "split evidence" || name == "aliases" {
				m := out.Matches[0]
				if len(m.Signals.EntityMatches) != 1 || m.Signals.TicketRank != 1 || !slices.Contains(m.Signals.MatchedFields, "ticket") {
					t.Fatal(m.Signals)
				}
			}
			if name == "cross-client retained" {
				found := false
				for _, m := range out.Matches {
					if m.Number == 4 {
						found = true
					}
				}
				if !found {
					t.Fatal("client became hard filter")
				}
			}
			if name == "cursor version" {
				o.Cursor = out.NextCursor
				next, err := db.Search(ctx, o)
				if err != nil || len(next.Matches) != 1 || next.Matches[0].Number == 2 {
					t.Fatal(next, err)
				}
			}
		})
	}
}
