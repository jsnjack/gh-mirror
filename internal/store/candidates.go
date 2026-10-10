package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// CandidateOptions chooses a seed and local retrieval scope; closed history remains included.
type CandidateOptions struct {
	Engine string `json:"engine,omitempty"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	QueryFilters
	PageOptions
	Limit           int    `json:"limit,omitempty"`
	Cursor          string `json:"cursor,omitempty"`
	IncludeLabels   *bool  `json:"include_labels,omitempty"`
	IncludeComments bool   `json:"include_comments,omitempty"`
	CommentLimit    int    `json:"comment_limit,omitempty"`
}

var candidateStopwords = strings.Fields("a an the and or of to in on for at by as is are was were be been being it its this that these those with from have has had not no yes we you your our they their then than when where what how can could should would will do does did please issue description expected actual behavior browser details steps reproduce environment")

func seedTerms(title, body string, labels []string, comments []string) map[string]float64 {
	weights := map[string]float64{}
	add := func(text string, weight float64) {
		seen := map[string]bool{}
		for _, word := range words.FindAllString(strings.ToLower(text), -1) {
			if utf8.RuneCountInString(word) < 2 || slices.Contains(candidateStopwords, word) || seen[word] {
				continue
			}
			seen[word] = true
			weights[word] += weight
			if len(seen) == 1024 {
				break
			}
		}
	}
	add(title, 4)
	add(body, 1)
	for _, label := range labels {
		add(label, 1.5)
	}
	for _, comment := range comments {
		add(comment, 0.5)
	}
	return weights
}

// Candidates preserves the original same-repository command with improved term selection.
func (s *Store) Candidates(ctx context.Context, repo string, number, limit int) (SearchResult, error) {
	return s.FindCandidates(ctx, CandidateOptions{Repo: repo, Number: number, Limit: limit})
}

// FindCandidates selects distinctive local seed terms, retaining technical acronyms and identifiers.
func (s *Store) FindCandidates(ctx context.Context, o CandidateOptions) (SearchResult, error) {
	var out SearchResult
	if err := validTicket(o.Repo, o.Number); err != nil {
		return out, err
	}
	if _, err := pageLimit(o.Limit); err != nil {
		return out, err
	}
	if (o.Engine == "semantic" || o.Engine == "hybrid") && o.Order == "" && (o.Sort == "" || o.Sort == "relevance") {
		o.Order = "desc"
	}
	if err := o.normalize(true); err != nil {
		return out, err
	}
	if o.Engine != "" && o.Engine != "lexical" && o.Engine != "semantic" && o.Engine != "hybrid" {
		return out, invalid("engine must be lexical, semantic or hybrid")
	}
	if o.CommentLimit == 0 {
		o.CommentLimit = 20
	}
	if o.CommentLimit < 1 || o.CommentLimit > 100 {
		return out, invalid("comment_limit must be between 1 and 100")
	}
	err := s.view(ctx, func(tx *sql.Tx) error {
		seed, err := readTicket(ctx, tx, o.Repo, o.Number, "full")
		if err != nil {
			return err
		}
		object, err := Object(seed.Payload)
		if err != nil {
			return err
		}
		labels := []string{}
		if o.IncludeLabels == nil || *o.IncludeLabels {
			labels = seed.Summary.Labels
		}
		comments := []string{}
		commentIDs := []string{}
		if o.IncludeComments {
			rows, err := tx.QueryContext(ctx, "SELECT id,body FROM comments WHERE repo=? AND number=? ORDER BY COALESCE(json_extract(payload,'$.created_at'),'') DESC,id DESC LIMIT ?", o.Repo, o.Number, o.CommentLimit)
			if err != nil {
				return fmt.Errorf("read candidate seed comments: %w", err)
			}
			for rows.Next() {
				var id, body string
				if err := rows.Scan(&id, &body); err != nil {
					return finishRows(rows, err)
				}
				comments = append(comments, body)
				commentIDs = append(commentIDs, "comment:"+id)
			}
			if err := finishRows(rows, rows.Err()); err != nil {
				return err
			}
		}
		weights := seedTerms(Text(object, "title"), Text(object, "body"), labels, comments)
		preliminary := []string{}
		for word := range weights {
			preliminary = append(preliminary, word)
		}
		sort.Slice(preliminary, func(a, b int) bool {
			if weights[preliminary[a]] == weights[preliminary[b]] {
				return preliminary[a] < preliminary[b]
			}
			return weights[preliminary[a]] > weights[preliminary[b]]
		})
		if len(preliminary) > 256 {
			preliminary = preliminary[:256]
		}
		targets := o.QueryFilters
		if len(targets.Repositories) == 0 {
			targets.Repositories = []string{o.Repo}
		}
		if o.Engine == "semantic" || o.Engine == "hybrid" {
			terms := preliminary[:min(32, len(preliminary))]
			query := strings.Join(terms, " ")
			if query == "" {
				query = Text(object, "title")
			}
			out, err = search(ctx, tx, SearchOptions{Engine: o.Engine, Query: query, QueryFilters: targets, PageOptions: o.PageOptions, Limit: o.Limit, Cursor: o.Cursor, Exclude: o.Number, ExcludeRepo: o.Repo, seed: &vectorSeed{TicketID{Repo: o.Repo, Number: o.Number}, o.IncludeLabels == nil || *o.IncludeLabels, commentIDs}}, s.vectorizer)
			return err
		}
		where, filterArgs, err := (filters{QueryFilters: targets}).sql()
		if err != nil {
			return err
		}
		var total int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM issues i WHERE "+where, filterArgs...).Scan(&total); err != nil {
			return fmt.Errorf("count candidate corpus: %w", err)
		}
		type term struct {
			word   string
			weight float64
		}
		chosen := []term{}
		for _, word := range preliminary {
			hits, args := scopedDocuments(`"`+word+`"`, nil, false, false, "", nil)
			args = append(args, filterArgs...)
			args = append(args, o.Repo, o.Number)
			var frequency int
			sql := `WITH hits AS (` + hits + `) SELECT count(*) FROM (SELECT DISTINCT h.repo,h.number FROM hits h JOIN issues i ON i.repo=h.repo AND i.number=h.number WHERE ` + where + ` AND NOT(i.repo=? AND i.number=?))`
			if err := tx.QueryRowContext(ctx, sql, args...).Scan(&frequency); err != nil {
				return fmt.Errorf("measure candidate term: %w", err)
			}
			if frequency == 0 {
				continue
			}
			chosen = append(chosen, term{word, weights[word] * math.Log(1+float64(total+1)/float64(frequency+1))})
		}
		sort.Slice(chosen, func(a, b int) bool {
			if chosen[a].weight == chosen[b].weight {
				return chosen[a].word < chosen[b].word
			}
			return chosen[a].weight > chosen[b].weight
		})
		if len(chosen) > 24 {
			chosen = chosen[:24]
		}
		selected := []string{}
		for _, term := range chosen {
			selected = append(selected, term.word)
		}
		if len(selected) == 0 {
			out = SearchResult{Matches: []Match{}, Warnings: []Warning{}, Ranking: Ranking{"sqlite_fts5_bm25", "ascending", "No seed terms occur in other tickets within the selected scope."}, Query: QueryInfo{Terms: []string{}, Match: "any", In: []string{"title", "body", "labels", "comments", "reviews"}}}
			out.Status, err = status(ctx, tx)
			if err != nil {
				return err
			}
			if o.Cursor != "" {
				return fmt.Errorf("%w; restart candidate retrieval", ErrCursorConflict)
			}
			if o.Facets {
				out.Facets = &FacetResult{Labels: []Bucket{}, Types: []Bucket{}, Projects: []Bucket{}}
			}
			if o.Count {
				zero := 0
				out.Total = &zero
			}
			out.Warnings = queryWarnings(out.Status, filters{QueryFilters: targets}, o.PageOptions, nil)
			return nil
		}
		out, err = search(ctx, tx, SearchOptions{Query: strings.Join(selected, " "), QueryFilters: targets, PageOptions: o.PageOptions, Limit: o.Limit, Cursor: o.Cursor, Exclude: o.Number, ExcludeRepo: o.Repo})
		return err
	})
	return out, err
}
