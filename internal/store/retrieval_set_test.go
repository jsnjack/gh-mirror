package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"gh-mirror/internal/commentpolicy"
	"gh-mirror/internal/embedding"
)

// hashVectorizer gives each passage a distinct, repeatable unit vector.
type hashVectorizer struct{ fakeVectorizer }

func (e *hashVectorizer) Embed(ctx context.Context, text string) ([]float32, error) {
	v := make([]float32, embedding.Dimension)
	for i := 0; i < 8; i++ {
		h := fnv.New32a()
		if _, err := fmt.Fprintf(h, "%d:%s", i, text); err != nil {
			return nil, err
		}
		x := h.Sum32()
		v[int(x)%embedding.Dimension] += float32(x%1000+1) / 1000
	}
	return normalizeVector(v)
}

func seedRetrievalCorpus(t *testing.T, db *Store, tickets int, extra ...json.RawMessage) {
	t.Helper()
	ctx := context.Background()
	if err := db.Update(ctx, func(w *Writer) error {
		for n := 1; n <= tickets; n++ {
			title := fmt.Sprintf("network crash %d", n%7)
			if n%23 == 0 {
				title = "zebraword failure"
			}
			raw := json.RawMessage(fmt.Sprintf(`{"number":%d,"node_id":"N%d","title":%q,"body":"body %d network","state":"open","created_at":"2026-01-%02dT00:00:00Z","updated_at":"2026-02-%02dT00:00:00Z","html_url":"https://github.com/o/r/issues/%d","labels":[]}`, n, n, title, n%5, n%28+1, n%27+1, n))
			if _, err := w.PutIssue(ctx, "o/r", raw); err != nil {
				return err
			}
		}
		for _, raw := range extra {
			if _, err := w.PutIssue(ctx, "o/r", raw); err != nil {
				return err
			}
		}
		for k, v := range map[string]string{"generation": "g", "repositories": `["o/r"]`} {
			if err := w.SetMetadata(ctx, k, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	encoder := &hashVectorizer{}
	if _, err := db.Index(ctx, encoder, IndexOptions{Workers: 2}); err != nil {
		t.Fatal(err)
	}
	db.vectorizer = encoder
}

func walkSearch(t *testing.T, db *Store, o SearchOptions) ([]Match, int) {
	t.Helper()
	o.Count = true
	seen := map[TicketID]bool{}
	all := []Match{}
	total := -1
	for {
		page, err := db.Search(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if total >= 0 && *page.Total != total {
			t.Fatalf("total changed between pages: %d then %d", total, *page.Total)
		}
		total = *page.Total
		for _, m := range page.Matches {
			id := TicketID{m.Repo, m.Number}
			if seen[id] {
				t.Fatalf("duplicate %v", id)
			}
			seen[id] = true
			all = append(all, m)
		}
		if !page.HasMore {
			return all, total
		}
		o.Cursor = page.NextCursor
	}
}

func TestHybridFusesLexicalOnlyMatches(t *testing.T) {
	checklist := json.RawMessage(`{"number":100,"node_id":"X100","title":"unrelated","body":"## Checklist\n- [ ] zebraword confirmed","state":"open","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-02-01T00:00:00Z","html_url":"https://github.com/o/r/issues/100","labels":[]}`)
	db := testStore(t)
	seedRetrievalCorpus(t, db, 10, checklist)
	for _, in := range [][]string{nil, {"body"}} {
		t.Run(fmt.Sprint(in), func(t *testing.T) {
			matches, total := walkSearch(t, db, SearchOptions{Engine: "hybrid", Query: "zebraword", In: in})
			found := false
			for _, m := range matches {
				if m.Number == 100 {
					found = true
					// The title still has a vector; only the body scope leaves the ticket without one.
					if m.LexicalScore == nil || len(m.Evidence) == 0 || (m.SemanticScore == nil) != (len(in) > 0) {
						t.Fatalf("lexical-only match %+v", m)
					}
				}
			}
			if !found || total != len(matches) {
				t.Fatalf("total %d matches %v", total, matches)
			}
		})
	}
}

func TestVectorSearchRetrievedSet(t *testing.T) {
	db := testStore(t)
	tickets := semanticCandidateLimit + 30
	seedRetrievalCorpus(t, db, tickets)
	lexical, lexicalTotal := walkSearch(t, db, SearchOptions{Engine: "lexical", Query: "zebraword", Limit: 7})
	if lexicalTotal != tickets/23 || len(lexical) != lexicalTotal {
		t.Fatal(lexicalTotal, len(lexical))
	}
	for _, tc := range []struct {
		engine   string
		min, max int
	}{{"semantic", semanticCandidateLimit, semanticCandidateLimit}, {"hybrid", semanticCandidateLimit, semanticCandidateLimit + lexicalTotal}} {
		for _, sort := range []string{"relevance", "number", "updated", "created"} {
			for _, order := range []string{"asc", "desc"} {
				t.Run(tc.engine+" "+sort+" "+order, func(t *testing.T) {
					matches, total := walkSearch(t, db, SearchOptions{Engine: tc.engine, Query: "zebraword", Limit: 60, PageOptions: PageOptions{Sort: sort, Order: order}})
					if total < tc.min || total > tc.max || len(matches) != total {
						t.Fatalf("total %d returned %d; want %d–%d", total, len(matches), tc.min, tc.max)
					}
					if tc.engine == "hybrid" {
						for _, want := range lexical {
							if !slices.ContainsFunc(matches, func(m Match) bool { return m.Number == want.Number }) {
								t.Fatalf("literal match %d missing", want.Number)
							}
						}
					}
				})
			}
		}
	}
	t.Run("facets follow retrieved set", func(t *testing.T) {
		out, err := db.Search(context.Background(), SearchOptions{Engine: "semantic", Query: "zebraword", PageOptions: PageOptions{Facets: true}})
		if err != nil || out.Facets == nil {
			t.Fatal(out.Facets, err)
		}
	})
}

func TestRelevanceOrderIsBestFirst(t *testing.T) {
	db := testStore(t)
	seedRetrievalCorpus(t, db, 40)
	for _, engine := range []string{"lexical", "semantic", "hybrid"} {
		for _, order := range []string{"", "desc", "asc"} {
			t.Run(engine+" "+order, func(t *testing.T) {
				out, err := db.Search(context.Background(), SearchOptions{Engine: engine, Query: "network crash 3", Limit: 100, PageOptions: PageOptions{Order: order}})
				if err != nil || len(out.Matches) < 2 {
					t.Fatal(out, err)
				}
				if out.Ranking.Direction != "descending" || strings.Contains(out.Ranking.Description, "Lower scores") {
					t.Fatal(out.Ranking)
				}
				for n := 1; n < len(out.Matches); n++ {
					a, b := out.Matches[n-1].Score, out.Matches[n].Score
					if order == "asc" && a > b || order != "asc" && a < b {
						t.Fatalf("order %q: score %v before %v", order, a, b)
					}
				}
			})
		}
	}
	t.Run("lexical cursor continues best first", func(t *testing.T) {
		matches, total := walkSearch(t, db, SearchOptions{Engine: "lexical", Query: "network crash 3", Limit: 3})
		if len(matches) != total {
			t.Fatal(len(matches), total)
		}
		for n := 1; n < len(matches); n++ {
			if matches[n-1].Score < matches[n].Score {
				t.Fatal(matches[n-1].Score, matches[n].Score)
			}
		}
	})
}

func TestNormalizeVectorRejectsNonFinite(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    []float32
	}{{"nan", []float32{float32(math.NaN()), 1}}, {"inf", []float32{float32(math.Inf(1)), 1}}, {"empty", []float32{0, 0}}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := normalizeVector(tc.v); err == nil {
				t.Fatal("accepted", tc.v)
			}
		})
	}
}

func TestCatalogKinds(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	seed(t, db)
	for _, kind := range []string{"label", "bogus"} {
		t.Run("page "+kind, func(t *testing.T) {
			_, err := db.CatalogPage(ctx, CatalogOptions{Kind: kind, Scope: "o/r"})
			if !errors.Is(err, ErrInvalidQuery) || !strings.Contains(err.Error(), "labels, milestones") {
				t.Fatal(err)
			}
		})
		t.Run("legacy "+kind, func(t *testing.T) {
			if _, err := db.Catalog(ctx, kind, "o/r"); !errors.Is(err, ErrInvalidQuery) {
				t.Fatal(err)
			}
		})
	}
	t.Run("exported list", func(t *testing.T) {
		kinds := CatalogKinds()
		kinds[0] = "changed"
		if !slices.Equal(CatalogKinds(), []string{"labels", "milestones", "issue_types", "issue_fields", "projects"}) {
			t.Fatal(CatalogKinds())
		}
	})
	t.Run("sorted warnings", func(t *testing.T) {
		if err := db.Update(ctx, func(w *Writer) error {
			return w.SetMetadata(ctx, "repository_options", `{"o/z":{"projects":false},"o/a":{"projects":false},"o/m":{"projects":false},"p/b":{"projects":false}}`)
		}); err != nil {
			t.Fatal(err)
		}
		for range 20 {
			out, err := db.CatalogPage(ctx, CatalogOptions{Kind: "projects", Scope: "o"})
			if err != nil {
				t.Fatal(err)
			}
			repos := []string{}
			for _, w := range out.Warnings {
				repos = append(repos, w.Repo)
			}
			if !slices.Equal(repos, []string{"o/a", "o/m", "o/z"}) {
				t.Fatal(repos)
			}
		}
	})
}

func TestCommentKindCoverage(t *testing.T) {
	ctx := context.Background()
	t.Run("policy counts follow kind", func(t *testing.T) {
		db := testStore(t)
		seedCommentPolicy(t, db)
		p := map[string]commentpolicy.Policy{"o/r": {ExcludeKnownAI: true, ExcludeAIThreads: true}}
		if err := db.Update(ctx, func(w *Writer) error { return w.ApplyCommentPolicies(ctx, p) }); err != nil {
			t.Fatal(err)
		}
		for kind, want := range map[string]int{"all": 3, "review": 3, "discussion": 0} {
			out, err := db.Comments(ctx, CommentOptions{Repo: "o/r", Number: 1, Kind: kind})
			if err != nil || out.ExcludedByPolicy != want {
				t.Fatal(kind, out.ExcludedByPolicy, err)
			}
			warned := slices.ContainsFunc(out.Warnings, func(w Warning) bool { return w.Code == "comments_excluded" })
			if warned != (want > 0) {
				t.Fatal(kind, out.Warnings)
			}
		}
		search, err := db.Search(ctx, SearchOptions{Engine: "lexical", Query: "humandiscussionuniquetoken", In: []string{"comments"}})
		if err != nil || slices.ContainsFunc(search.Warnings, func(w Warning) bool { return w.Code == "comments_excluded" }) {
			t.Fatal(search.Warnings, err)
		}
	})
	t.Run("reviews warning only for pull requests", func(t *testing.T) {
		db := testStore(t)
		seed(t, db)
		if err := db.Update(ctx, func(w *Writer) error {
			raw := json.RawMessage(`{"number":3,"node_id":"PR3","title":"pr","body":"","state":"open","updated_at":"2026-01-02T00:00:00Z","html_url":"https://github.com/o/r/pull/3","pull_request":{"url":"https://api.github.com/repos/o/r/pulls/3"},"labels":[]}`)
			if _, err := w.PutIssue(ctx, "o/r", raw); err != nil {
				return err
			}
			return w.SetMetadata(ctx, "repository_options", `{"o/r":{"issues":true,"pull_requests":true,"issue_comments":true,"pull_request_comments":true,"pull_request_review_comments":false}}`)
		}); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			number int
			kind   string
			want   bool
		}{{1, "all", false}, {1, "discussion", false}, {1, "review", true}, {3, "all", true}} {
			out, err := db.Comments(ctx, CommentOptions{Repo: "o/r", Number: tc.number, Kind: tc.kind})
			if err != nil {
				t.Fatal(err)
			}
			warned := slices.ContainsFunc(out.Warnings, func(w Warning) bool { return w.Code == "resource_disabled" && w.Resource == "reviews" })
			if warned != tc.want {
				t.Fatal(tc, out.Warnings)
			}
		}
	})
}

func TestDateBounds(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value  string
		before bool
		want   time.Time
		op     string
	}{
		{"2026-01-02T03:04:05Z", false, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), ">="},
		{"2026-01-02T03:04:05+02:00", true, time.Date(2026, 1, 2, 1, 4, 5, 0, time.UTC), "<="},
		{"2026-01-02", false, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), ">="},
		{"2026-01-02", true, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), "<"},
		{"12h", false, now.Add(-12 * time.Hour), ">="},
		{"7d", true, now.AddDate(0, 0, -7), "<="},
		{"2w", false, now.AddDate(0, 0, -14), ">="},
	} {
		t.Run(fmt.Sprint(tc.value, tc.before), func(t *testing.T) {
			got, op, err := dateBound(tc.value, tc.before, now)
			if err != nil || !got.Equal(tc.want) || op != tc.op {
				t.Fatal(got, op, err)
			}
		})
	}
	for _, value := range []string{"yesterday", "0d", "7", "7m", "2026-13-01", "-7d"} {
		t.Run("reject "+value, func(t *testing.T) {
			_, _, err := dateBound(value, false, now)
			if !errors.Is(err, ErrInvalidQuery) || !strings.Contains(err.Error(), "YYYY-MM-DD") {
				t.Fatal(err)
			}
		})
	}
	t.Run("whole day before bound", func(t *testing.T) {
		db := testStore(t)
		seed(t, db)
		for _, tc := range []struct {
			f    QueryFilters
			want int
		}{
			{QueryFilters{UpdatedBefore: "2026-01-02"}, 2},
			{QueryFilters{UpdatedBefore: "2026-01-01"}, 0},
			{QueryFilters{UpdatedAfter: "2026-01-02", UpdatedBefore: "2026-01-02"}, 2},
			{QueryFilters{UpdatedAfter: "2026-01-03"}, 0},
		} {
			out, err := db.List(context.Background(), ListOptions{QueryFilters: tc.f})
			if err != nil || len(out.Issues) != tc.want {
				t.Fatal(tc.f, len(out.Issues), err)
			}
		}
		if _, err := db.List(context.Background(), ListOptions{QueryFilters: QueryFilters{UpdatedAfter: "2026-01-03", UpdatedBefore: "2026-01-02"}}); !errors.Is(err, ErrInvalidQuery) {
			t.Fatal(err)
		}
	})
}
