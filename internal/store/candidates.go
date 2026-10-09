package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Candidates retrieves related tickets including closed history, excluding the seed.
func (s *Store) Candidates(ctx context.Context, repo string, number, limit int) (SearchResult, error) {
	var out SearchResult
	err := s.view(ctx, func(tx *sql.Tx) error {
		issue, err := get(ctx, tx, repo, number)
		if err != nil {
			return fmt.Errorf("load candidate seed: %w", err)
		}
		o, err := Object(issue.Payload)
		if err != nil {
			return fmt.Errorf("decode candidate seed: %w", err)
		}
		counts := map[string]int{}
		for _, word := range words.FindAllString(strings.ToLower(Text(o, "title")+" "+Text(o, "body")), -1) {
			if len(word) > 3 && !strings.Contains(" the and this that with from have been issue please when then does into ", " "+word+" ") {
				counts[word]++
			}
		}
		terms := []string{}
		for word := range counts {
			terms = append(terms, word)
		}
		sort.Slice(terms, func(i, j int) bool {
			if counts[terms[i]] == counts[terms[j]] {
				return terms[i] < terms[j]
			}
			return counts[terms[i]] > counts[terms[j]]
		})
		if len(terms) > 24 {
			terms = terms[:24]
		}
		query := strings.Join(terms, " ")
		if query == "" {
			query = Text(o, "title")
		}
		out, err = search(ctx, tx, SearchOptions{Query: query, Repo: repo, Limit: limit, Exclude: number})
		return err
	})
	return out, err
}
