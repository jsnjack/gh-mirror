package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// TestReadCacheFollowsCommits checks that cached status and vectors are dropped
// when this or another Store commits, and that cached and direct reads agree.
func TestReadCacheFollowsCommits(t *testing.T) {
	db := testStore(t)
	seedRetrievalCorpus(t, db, 60)
	db.EnableReadCache()
	ctx := context.Background()
	search := func() SearchResult {
		t.Helper()
		out, err := db.Search(ctx, SearchOptions{Engine: "hybrid", Query: "zebraword", Repo: "o/r", Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	numbers := func(out SearchResult) []int {
		got := []int{}
		for _, m := range out.Matches {
			got = append(got, m.Number)
		}
		return got
	}
	before := search()
	if again := search(); !slices.Equal(numbers(before), numbers(again)) || again.Status.Issues != before.Status.Issues {
		t.Fatalf("repeated cached search changed: %v then %v", numbers(before), numbers(again))
	}

	// Another process: a second Store on the same file adds a matching ticket.
	other, err := Open(db.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := other.Close(); err != nil {
			t.Error(err)
		}
	})
	add := func(s *Store, number int) {
		t.Helper()
		raw := json.RawMessage(fmt.Sprintf(`{"number":%d,"node_id":"N%d","title":"zebraword failure","body":"body network","state":"open","created_at":"2026-03-01T00:00:00Z","updated_at":"2026-03-02T00:00:00Z","html_url":"https://github.com/o/r/issues/%d","labels":[]}`, number, number, number))
		if err := s.Update(ctx, func(w *Writer) error { _, err := w.PutIssue(ctx, "o/r", raw); return err }); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Index(ctx, &hashVectorizer{}, IndexOptions{Workers: 1}); err != nil {
			t.Fatal(err)
		}
	}
	add(other, 901)
	after := search()
	if !slices.Contains(numbers(after), 901) || after.Status.Issues != before.Status.Issues+1 {
		t.Fatalf("cache missed another process's commit: issues %d → %d, matches %v", before.Status.Issues, after.Status.Issues, numbers(after))
	}

	// The same Store commits through another pooled connection.
	add(db, 902)
	latest := search()
	if !slices.Contains(numbers(latest), 902) || latest.Status.Issues != before.Status.Issues+2 {
		t.Fatalf("cache missed this Store's commit: issues %d, matches %v", latest.Status.Issues, numbers(latest))
	}

	setReadCache(db, false)
	direct := search()
	cached, err := json.Marshal(latest.Matches)
	if err != nil {
		t.Fatal(err)
	}
	uncached, err := json.Marshal(direct.Matches)
	if err != nil {
		t.Fatal(err)
	}
	if string(cached) != string(uncached) {
		t.Fatalf("cached and direct searches differ:\n%s\n%s", cached, uncached)
	}
}
