package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
)

// searchGoldenPath records full search results over the anonymized duplicate
// fixture. Performance work must leave them byte-identical; regenerate with
// -update-search-golden only for an intended ranking or output change.
const searchGoldenPath = "testdata/search_golden.json.gz"

var updateSearchGolden = flag.Bool("update-search-golden", false, "rewrite "+searchGoldenPath)

// searchGoldenStep picks every Nth fixture ticket title as a text query.
const searchGoldenStep = 27

type searchGoldenCase struct {
	Name   string          `json:"name"`
	Result json.RawMessage `json:"result"`
}

// TestSearchGolden replays text searches with every engine and seed-based
// candidate searches, and fails on any change to matches, scores, evidence,
// snippets, warnings or status apart from volatile timestamps.
func TestSearchGolden(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("search golden indexes the full fixture; make duplicates runs it without -race")
	}
	t.Parallel()
	ctx := context.Background()
	var want []searchGoldenCase
	if *updateSearchGolden {
		db, fixture := loadDuplicateFixtureWith(t, true)
		setReadCache(db, true)
		want = searchGoldenCases(t, ctx, db, fixture)
		writeSearchGolden(t, want)
		t.Logf("wrote %d cases to %s", len(want), searchGoldenPath)
	} else {
		want = readSearchGolden(t)
	}
	// Both read paths must give the golden results: the cache used by
	// long-running readers and the direct SQL scan.
	for _, cached := range []bool{true, false} {
		mode := fmt.Sprintf("cache=%v", cached)
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			db, fixture := loadDuplicateFixtureWith(t, true)
			setReadCache(db, cached)
			compareSearchGolden(t, mode, want, searchGoldenCases(t, ctx, db, fixture))
		})
	}
}

func setReadCache(db *Store, enabled bool) {
	db.cache.mu.Lock()
	defer db.cache.mu.Unlock()
	db.cache.closeConn()
	db.cache.enabled = enabled
}

func searchGoldenCases(t *testing.T, ctx context.Context, db *Store, fixture duplicateFixture) []searchGoldenCase {
	t.Helper()
	type query struct {
		name string
		run  func() (SearchResult, error)
	}
	queries := []query{}
	search := func(name string, o SearchOptions) {
		queries = append(queries, query{name, func() (SearchResult, error) { return db.Search(ctx, o) }})
	}
	for n := 0; n < len(fixture.Tickets); n += searchGoldenStep {
		ticket := fixture.Tickets[n]
		if words := len(strings.Fields(ticket.Title)); words == 0 || words > 20 {
			continue // lexical search accepts at most 32 terms; keep queries realistic
		}
		for _, engine := range []string{"lexical", "semantic", "hybrid"} {
			for _, limit := range []int{10, 20} {
				search(fmt.Sprintf("search %s limit=%d #%d", engine, limit, ticket.Number), SearchOptions{Engine: engine, Query: ticket.Title, Repo: "o/r", Limit: limit})
			}
		}
		search(fmt.Sprintf("search hybrid open evidence=1 #%d", ticket.Number), SearchOptions{Engine: "hybrid", Query: ticket.Title, Repo: "o/r", Limit: 10, EvidenceLimit: 1, State: "open"})
	}
	for _, c := range fixture.Cases {
		o := CandidateOptions{Repo: "o/r", Number: c.Seed, Engine: "hybrid", Limit: 20}
		queries = append(queries, query{"candidates " + c.Name, func() (SearchResult, error) { return db.FindCandidates(ctx, o) }})
	}
	got := make([]searchGoldenCase, len(queries))
	errs := make([]error, len(queries))
	forEachConcurrently(len(queries), func(n int) {
		result, err := queries[n].run()
		if err == nil {
			got[n].Result, err = stableResult(result)
		}
		got[n].Name, errs[n] = queries[n].name, err
	})
	for n, err := range errs {
		if err != nil {
			t.Fatalf("%s: %v", queries[n].name, err)
		}
	}
	return got
}

func compareSearchGolden(t *testing.T, mode string, want, got []searchGoldenCase) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: golden has %d cases, run produced %d", mode, len(want), len(got))
	}
	failed := 0
	for n := range got {
		if want[n].Name != got[n].Name {
			t.Fatalf("%s: case %d is %q, golden has %q", mode, n, got[n].Name, want[n].Name)
		}
		if !bytes.Equal(want[n].Result, got[n].Result) {
			failed++
			if failed <= 5 {
				t.Errorf("%s: %s changed:\n%s", mode, got[n].Name, firstDifference(want[n].Result, got[n].Result))
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%s: %d of %d search results differ from %s", mode, failed, len(got), searchGoldenPath)
	}
}

// stableResult serializes a result without what differs between index runs:
// timestamps and the random index generation, which the page cursor embeds.
func stableResult(result SearchResult) (json.RawMessage, error) {
	if result.NextCursor != "" {
		result.NextCursor = "present"
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if object, ok := value.(map[string]any); ok {
		dropVolatile(object["status"])
	}
	return json.Marshal(value)
}

func dropVolatile(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if strings.HasSuffix(key, "_at") || key == "generation" {
				delete(v, key)
				continue
			}
			dropVolatile(child)
		}
	case []any:
		for _, child := range v {
			dropVolatile(child)
		}
	}
}

func firstDifference(want, got []byte) string {
	n := 0
	for n < len(want) && n < len(got) && want[n] == got[n] {
		n++
	}
	from := max(n-120, 0)
	return fmt.Sprintf("want …%s…\n got …%s…", want[from:min(n+120, len(want))], got[from:min(n+120, len(got))])
}

func writeSearchGolden(t *testing.T, cases []searchGoldenCase) {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	zw.ModTime = zw.ModTime.UTC() // zero time: deterministic output
	encoder := json.NewEncoder(zw)
	for _, c := range cases {
		if err := encoder.Encode(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(searchGoldenPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readSearchGolden(t *testing.T) []searchGoldenCase {
	t.Helper()
	file, err := os.Open(searchGoldenPath)
	if err != nil {
		t.Fatalf("%v; create it with go test -run '^TestSearchGolden$' -update-search-golden ./internal/store", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	zr, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	cases := []searchGoldenCase{}
	decoder := json.NewDecoder(zr)
	for decoder.More() {
		var c searchGoldenCase
		if err := decoder.Decode(&c); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, c)
	}
	return cases
}
