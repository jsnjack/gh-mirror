package store

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"
)

// EvaluationCase pairs a search or candidate request with independently judged relevant tickets.
type EvaluationCase struct {
	Name       string            `json:"name"`
	Search     *SearchOptions    `json:"search,omitempty"`
	Candidates *CandidateOptions `json:"candidates,omitempty"`
	Relevant   []TicketID        `json:"relevant"`
}

// EvaluationSet holds locally supplied judgments; it never fetches or modifies tickets.
type EvaluationSet struct {
	Cases []EvaluationCase `json:"cases"`
}

// EvaluationScore reports retrieval precision, recall and elapsed time for one case.
type EvaluationScore struct {
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	Returned      int     `json:"returned"`
	PrecisionAt10 float64 `json:"precision_at_10"`
	RecallAt20    float64 `json:"recall_at_20"`
	ElapsedMS     float64 `json:"elapsed_ms"`
}

// EvaluationReport summarizes one-generation evaluation without returning ticket content.
type EvaluationReport struct {
	Generation          string            `json:"generation"`
	Cases               []EvaluationScore `json:"cases"`
	MeanPrecisionAt10   float64           `json:"mean_precision_at_10"`
	MeanRecallAt20      float64           `json:"mean_recall_at_20"`
	SearchPrecisionAt10 *float64          `json:"search_precision_at_10,omitempty"`
	CandidateRecallAt20 *float64          `json:"candidate_recall_at_20,omitempty"`
	P50MS               float64           `json:"p50_ms"`
	P95MS               float64           `json:"p95_ms"`
}

// Evaluate measures local retrieval against explicit judgments, failing if the generation changes.
func (s *Store) Evaluate(ctx context.Context, set EvaluationSet) (EvaluationReport, error) {
	out := EvaluationReport{Cases: []EvaluationScore{}}
	if len(set.Cases) < 1 || len(set.Cases) > 1000 {
		return out, invalid("evaluation requires 1–1000 cases")
	}
	for _, c := range set.Cases {
		if c.Name == "" || (c.Search == nil) == (c.Candidates == nil) || len(c.Relevant) == 0 {
			return out, invalid("each evaluation case requires a name, exactly one request and relevant tickets")
		}
		for _, id := range c.Relevant {
			if err := validTicket(id.Repo, id.Number); err != nil {
				return out, err
			}
		}
	}
	initial, err := s.Status(ctx)
	if err != nil {
		return out, err
	}
	out.Generation = initial.Generation
	times := []float64{}
	var searchTotal, candidateTotal float64
	var searches, candidates int
	for _, c := range set.Cases {
		started := time.Now()
		var result SearchResult
		var err error
		kind := "search"
		if c.Search != nil {
			o := *c.Search
			o.Limit = 20
			o.Cursor = ""
			o.Count = false
			o.Facets = false
			o.View = "summary"
			o.EvidenceLimit = 1
			result, err = s.Search(ctx, o)
		} else {
			o := *c.Candidates
			o.Limit = 20
			o.Cursor = ""
			o.Count = false
			o.Facets = false
			o.View = "summary"
			result, err = s.FindCandidates(ctx, o)
			kind = "candidates"
		}
		if err != nil {
			return out, fmt.Errorf("evaluate case %q: %w", c.Name, err)
		}
		if result.Status.Generation != out.Generation {
			return out, fmt.Errorf("%w during evaluation; use an immutable snapshot", ErrCursorConflict)
		}
		relevant := map[TicketID]bool{}
		for _, id := range c.Relevant {
			relevant[id] = true
		}
		hits10, hits20 := 0, 0
		seen := map[TicketID]bool{}
		for n, m := range result.Matches {
			id := TicketID{m.Repo, m.Number}
			if !relevant[id] || seen[id] {
				continue
			}
			seen[id] = true
			if n < 10 {
				hits10++
			}
			if n < 20 {
				hits20++
			}
		}
		score := EvaluationScore{Name: c.Name, Kind: kind, Returned: len(result.Matches), PrecisionAt10: float64(hits10) / 10, RecallAt20: float64(hits20) / float64(len(relevant)), ElapsedMS: float64(time.Since(started)) / float64(time.Millisecond)}
		out.Cases = append(out.Cases, score)
		times = append(times, score.ElapsedMS)
		out.MeanPrecisionAt10 += score.PrecisionAt10
		out.MeanRecallAt20 += score.RecallAt20
		if kind == "search" {
			searchTotal += score.PrecisionAt10
			searches++
		} else {
			candidateTotal += score.RecallAt20
			candidates++
		}
	}
	final, err := s.Status(ctx)
	if err != nil {
		return out, err
	}
	if final.Generation != out.Generation {
		return out, fmt.Errorf("%w during evaluation; use an immutable snapshot", ErrCursorConflict)
	}
	out.MeanPrecisionAt10 /= float64(len(set.Cases))
	out.MeanRecallAt20 /= float64(len(set.Cases))
	slices.Sort(times)
	out.P50MS = times[int(math.Ceil(float64(len(times))*0.5))-1]
	out.P95MS = times[int(math.Ceil(float64(len(times))*0.95))-1]
	if searches > 0 {
		v := searchTotal / float64(searches)
		out.SearchPrecisionAt10 = &v
	}
	if candidates > 0 {
		v := candidateTotal / float64(candidates)
		out.CandidateRecallAt20 = &v
	}
	return out, nil
}
