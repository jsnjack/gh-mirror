package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"gh-mirror/internal/representation"
)

const ticketRankingVersion = "2"

const ticketFusionWeight = 3

// RetrievalSignals explains whole-ticket evidence used by hybrid retrieval.
type RetrievalSignals struct {
	MatchedTerms  []string `json:"matched_terms"`
	MatchedFields []string `json:"matched_fields"`
	EntityMatches []string `json:"entity_matches"`
	LexicalRank   int      `json:"lexical_rank,omitempty"`
	SemanticRank  int      `json:"semantic_rank"`
	TicketRank    int      `json:"ticket_rank,omitempty"`
}

type termEvidence struct {
	terms    map[string]float64
	fields   map[string]bool
	entities []string
	score    float64
}

func fieldWeight(field string) float64 {
	switch field {
	case "title":
		return 2
	case "labels":
		return 1.5
	case "body", "ticket":
		return 1
	default:
		return 0.6
	}
}

func hasPhrase(query, phrase string) bool {
	parts := queryTerms(phrase, false)
	return len(parts) > 0 && strings.Contains(query, " "+strings.Join(parts, " ")+" ")
}

func coverageDocuments(term string, in []string) (string, []any) {
	clauses := []string{}
	args := []any{}
	if len(in) == 0 {
		clauses = append(clauses, `SELECT d.repo,d.number,CASE WHEN d.id LIKE 'labels:%' THEN 'labels' WHEN d.id LIKE 'comment:review:%' THEN 'review_comment' WHEN d.id LIKE 'comment:%' THEN 'comment' ELSE 'ticket' END field FROM documents_fts JOIN documents d ON d.rowid=documents_fts.rowid WHERE documents_fts MATCH ?`)
		args = append(args, term)
		in = []string{"title"}
	}
	for _, scope := range in {
		expression, condition, field := term, "1=1", scope
		switch scope {
		case "title", "body":
			expression = scope + ":(" + term + ")"
			condition = "d.id LIKE 'issue:%'"
		case "labels":
			expression = "title:(" + term + ")"
			condition = "d.id LIKE 'labels:%'"
		case "comments":
			condition = "d.id LIKE 'comment:%' AND d.id NOT LIKE 'comment:review:%'"
			field = "comment"
		case "reviews":
			condition = "d.id LIKE 'comment:review:%'"
			field = "review_comment"
		}
		clauses = append(clauses, "SELECT d.repo,d.number,'"+field+"' field FROM documents_fts JOIN documents d ON d.rowid=documents_fts.rowid WHERE documents_fts MATCH ? AND "+condition)
		args = append(args, expression)
	}
	return strings.Join(clauses, " UNION ALL "), args
}

func ticketRanks(ctx context.Context, q querier, o SearchOptions, hits map[TicketID]*semanticHit, where string, args []any) ([]*semanticHit, []string, error) {
	terms := []string{}
	for _, term := range queryTerms(o.Query, true) {
		if !slices.Contains(candidateStopwords, term) {
			terms = append(terms, term)
		}
	}
	if len(terms) > 32 {
		terms = terms[:32]
	}
	evidence := map[TicketID]*termEvidence{}
	entry := func(id TicketID) *termEvidence {
		if evidence[id] == nil {
			evidence[id] = &termEvidence{terms: map[string]float64{}, fields: map[string]bool{}}
		}
		return evidence[id]
	}
	in := o.In
	df := map[string]int{}
	for _, term := range terms {
		docs, docArgs := coverageDocuments(quoteTerms([]string{term}, false)[0], in)
		rows, err := q.QueryContext(ctx, `WITH term_hits AS (`+docs+`) SELECT DISTINCT h.repo,h.number,h.field FROM term_hits h JOIN issues i ON i.repo=h.repo AND i.number=h.number WHERE `+where, append(docArgs, args...)...)
		if err != nil {
			return nil, nil, fmt.Errorf("collect ticket term evidence: %w", err)
		}
		seen := map[TicketID]bool{}
		for rows.Next() {
			var id TicketID
			var field string
			if err := rows.Scan(&id.Repo, &id.Number, &field); err != nil {
				return nil, nil, finishRows(rows, err)
			}
			if hits[id] == nil {
				continue
			}
			seen[id] = true
			item := entry(id)
			item.terms[term] = math.Max(item.terms[term], fieldWeight(field))
			item.fields[field] = true
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return nil, nil, err
		}
		df[term] = len(seen)
	}
	entities := []string{}
	if len(in) == 0 || slices.Contains(in, "labels") {
		rules, err := labelRules(ctx, q)
		if err != nil {
			return nil, nil, err
		}
		rows, err := q.QueryContext(ctx, `SELECT i.repo,i.number,COALESCE(json_extract(i.payload,'$.labels'),'[]') FROM issues i WHERE `+where, args...)
		if err != nil {
			return nil, nil, fmt.Errorf("read ticket entity attributes: %w", err)
		}
		query := " " + strings.Join(queryTerms(o.Query, false), " ") + " "
		for rows.Next() {
			var id TicketID
			var raw string
			if err := rows.Scan(&id.Repo, &id.Number, &raw); err != nil {
				return nil, nil, finishRows(rows, err)
			}
			if hits[id] == nil {
				continue
			}
			var labels []struct{ Name string }
			if err := json.Unmarshal([]byte(raw), &labels); err != nil {
				return nil, nil, finishRows(rows, fmt.Errorf("decode ticket entity labels: %w", err))
			}
			names := make([]string, 0, len(labels))
			for _, label := range labels {
				names = append(names, label.Name)
			}
			for _, a := range representation.Attributes(names, rules[id.Repo]) {
				if a.Role != "client" && a.Role != "partner" {
					continue
				}
				matched := hasPhrase(query, a.Value)
				for _, alias := range a.Aliases {
					matched = matched || hasPhrase(query, alias)
				}
				if matched {
					item := entry(id)
					if !slices.Contains(item.entities, a.Label) {
						item.entities = append(item.entities, a.Label)
					}
					item.fields["labels"] = true
					if !slices.Contains(entities, a.Value) {
						entities = append(entities, a.Value)
					}
				}
			}
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return nil, nil, err
		}
	}
	var denominator float64
	for _, term := range terms {
		denominator += math.Log(1 + float64(len(hits)+1)/float64(df[term]+1))
	}
	ranked := []*semanticHit{}
	for id, item := range evidence {
		signal := &RetrievalSignals{MatchedTerms: []string{}, MatchedFields: []string{}, EntityMatches: item.entities}
		if signal.EntityMatches == nil {
			signal.EntityMatches = []string{}
		}
		var matched, weighted float64
		for _, term := range terms {
			if weight := item.terms[term]; weight > 0 {
				signal.MatchedTerms = append(signal.MatchedTerms, term)
				rarity := math.Log(1 + float64(len(hits)+1)/float64(df[term]+1))
				matched += rarity
				weighted += rarity * weight
			}
		}
		for field := range item.fields {
			signal.MatchedFields = append(signal.MatchedFields, field)
		}
		sort.Strings(signal.MatchedFields)
		sort.Strings(signal.EntityMatches)
		if denominator > 0 {
			item.score = matched/denominator + 0.25*weighted/denominator
		}
		if len(item.entities) > 0 {
			bonus := 1.5
			entityTerms := []string{}
			for _, label := range item.entities {
				_, value, _ := strings.Cut(label, ":")
				entityTerms = append(entityTerms, queryTerms(value, true)...)
			}
			topic, covered := 0, 0
			for _, term := range terms {
				if slices.Contains(entityTerms, term) {
					continue
				}
				topic++
				if item.terms[term] > 0 {
					covered++
				}
			}
			if topic > 0 {
				bonus *= float64(covered) / float64(topic)
			}
			item.score += bonus
		}
		hits[id].signals = signal
		ranked = append(ranked, hits[id])
	}
	sort.Slice(ranked, func(a, b int) bool {
		x, y := evidence[ranked[a].id].score, evidence[ranked[b].id].score
		if x != y {
			return x > y
		}
		return semanticLess(ranked[a], ranked[b], PageOptions{Sort: "relevance", Order: "desc"})
	})
	sort.Strings(entities)
	return ranked, entities, nil
}

func hybridEvidence(ctx context.Context, q querier, o SearchOptions, m *Match) error {
	terms := queryTerms(o.Query, true)
	if len(terms) == 0 {
		return nil
	}
	docs, args := scopedDocuments(strings.Join(quoteTerms(terms, false), " OR "), o.In, true, true, "d.repo=? AND d.number=?", []any{m.Repo, m.Number})
	rows, err := q.QueryContext(ctx, `SELECT id,field,source,excerpt FROM (`+docs+`) ORDER BY CASE WHEN field='labels' THEN 0 WHEN field='title' THEN 1 ELSE 2 END,score,id LIMIT ?`, append(args, o.EvidenceLimit)...)
	if err != nil {
		return fmt.Errorf("read hybrid literal evidence: %w", err)
	}
	literal := []Evidence{}
	for rows.Next() {
		var ev Evidence
		var id string
		if err := rows.Scan(&id, &ev.Field, &ev.Source, &ev.Snippet); err != nil {
			return finishRows(rows, err)
		}
		if strings.HasPrefix(id, "comment:") {
			enriched, err := semanticEvidence(ctx, q, scoredEvidence{document: id, field: ev.Field, source: ev.Source, text: ev.Snippet})
			if err != nil {
				return finishRows(rows, err)
			}
			enriched.SemanticScore = nil
			ev = enriched
		}
		ev.Truncated = ev.Truncated || strings.Contains(ev.Snippet, " … ")
		literal = append(literal, ev)
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return err
	}
	// Prefer complete semantic source metadata where the same passage also matches literally.
	combined := append([]Evidence{}, m.Evidence...)
	for _, ev := range literal {
		found := false
		for _, existing := range combined {
			if existing.Field == ev.Field && existing.Source == ev.Source {
				found = true
			}
		}
		if !found {
			combined = append(combined, ev)
		}
	}
	sort.SliceStable(combined, func(a, b int) bool {
		weight := func(ev Evidence) float64 {
			w := fieldWeight(ev.Field)
			if ev.Field == "labels" && m.Signals != nil && len(m.Signals.EntityMatches) > 0 {
				w += 2
			}
			return w
		}
		return weight(combined[a]) > weight(combined[b])
	})
	m.Evidence = combined[:min(len(combined), o.EvidenceLimit)]
	return nil
}
