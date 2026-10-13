package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"gh-mirror/internal/commentpolicy"
)

func TestRetrievalEvaluation(t *testing.T) {
	raw, err := os.ReadFile("testdata/retrieval.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Tickets []struct {
			Repo        string
			Number      int
			Title, Body string
			Labels      []string
		}
		Comments []struct {
			Number int
			Body   string
		}
		Evaluation EvaluationSet
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	db := testStore(t)
	ctx := context.Background()
	if err := db.Update(ctx, func(w *Writer) error {
		for _, ticket := range fixture.Tickets {
			labels := []map[string]string{}
			for _, label := range ticket.Labels {
				labels = append(labels, map[string]string{"name": label})
			}
			raw, err := json.Marshal(map[string]any{"number": ticket.Number, "node_id": fmt.Sprintf("fixture_%d", ticket.Number), "title": ticket.Title, "body": ticket.Body, "labels": labels, "state": "closed", "updated_at": "2026-01-01T00:00:00Z", "html_url": fmt.Sprintf("https://github.com/%s/issues/%d", ticket.Repo, ticket.Number)})
			if err != nil {
				return err
			}
			if _, err := w.PutIssue(ctx, ticket.Repo, raw); err != nil {
				return err
			}
		}
		for n, c := range fixture.Comments {
			raw, err := json.Marshal(map[string]any{"id": n + 1, "body": c.Body, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z", "html_url": "https://github.com/demo/support/issues/comment"})
			if err != nil {
				return err
			}
			if err := w.PutComment(ctx, "demo/support", c.Number, raw); err != nil {
				return err
			}
		}
		if err := w.SetMetadata(ctx, "generation", "evaluation-fixture"); err != nil {
			return err
		}
		return w.SetMetadata(ctx, "repositories", `["demo/support","demo/code"]`)
	}); err != nil {
		t.Fatal(err)
	}
	report, err := db.Evaluate(ctx, fixture.Evaluation)
	if err != nil {
		t.Fatal(err)
	}
	if report.Generation != "evaluation-fixture" || len(report.Cases) != 6 || report.CandidateRecallAt20 == nil || *report.CandidateRecallAt20 != 1 || report.MeanRecallAt20 != 1 {
		t.Fatal(report)
	}
	for _, score := range report.Cases {
		if score.RecallAt20 != 1 || score.ElapsedMS <= 0 {
			t.Fatal(score)
		}
	}
	t.Logf("synthetic fixture: search P@10 %.3f, candidate Recall@20 %.3f; sparse judgments limit maximum P@10", *report.SearchPrecisionAt10, *report.CandidateRecallAt20)
}

type changingEvaluationEncoder struct {
	*fakeVectorizer
	once   sync.Once
	change func(context.Context) error
}

func (e *changingEvaluationEncoder) Embed(ctx context.Context, text string) ([]float32, error) {
	var changeErr error
	e.once.Do(func() { changeErr = e.change(ctx) })
	if changeErr != nil {
		return nil, fmt.Errorf("change evaluation fixture: %w", changeErr)
	}
	return e.fakeVectorizer.Embed(ctx, text)
}

func TestEvaluationGenerationChanges(t *testing.T) {
	for _, name := range []string{"comment policy", "vectors", "collection", "unchanged"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testStore(t)
			seed(t, db)
			e := &fakeVectorizer{}
			if _, err := db.Index(ctx, e, IndexOptions{Workers: 1, CommentPolicies: map[string]commentpolicy.Policy{}}); err != nil {
				t.Fatal(err)
			}
			db.vectorizer = &changingEvaluationEncoder{fakeVectorizer: e, change: func(ctx context.Context) error {
				return db.Update(ctx, func(w *Writer) error {
					switch name {
					case "comment policy":
						return w.ApplyCommentPolicies(ctx, map[string]commentpolicy.Policy{"o/r": {ExcludeKnownAI: true}})
					case "vectors":
						return w.SetMetadata(ctx, "semantic_generation", "changed-vectors")
					case "collection":
						return w.SetMetadata(ctx, "generation", "changed-collection")
					default:
						return nil
					}
				})
			}}
			out, err := db.Evaluate(ctx, EvaluationSet{Cases: []EvaluationCase{{Name: "generation consistency", Search: &SearchOptions{Query: "network", Engine: "semantic"}, Relevant: []TicketID{{Repo: "o/r", Number: 1}}}}})
			if name == "unchanged" {
				if err != nil || out.RetrievalGeneration == "" || out.Generation != "testgeneration" {
					t.Fatal(out, err)
				}
			} else if !errors.Is(err, ErrCursorConflict) {
				t.Fatal(out, err)
			}
		})
	}
}
func TestEvaluationMetrics(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	seed(t, db)
	set := EvaluationSet{Cases: []EvaluationCase{{Name: "known recall", Search: &SearchOptions{Query: "network"}, Relevant: []TicketID{{"o/r", 1}, {"o/r", 2}, {"o/r", 99}}}}}
	report, err := db.Evaluate(ctx, set)
	if err != nil || report.MeanPrecisionAt10 != 0.2 || report.MeanRecallAt20 != 2.0/3 || report.SearchPrecisionAt10 == nil || report.CandidateRecallAt20 != nil {
		t.Fatal(report, err)
	}
	for _, invalid := range []EvaluationSet{{}, {Cases: []EvaluationCase{{Name: "missing judgments", Search: &SearchOptions{Query: "network"}}}}, {Cases: []EvaluationCase{{Name: "ambiguous", Search: &SearchOptions{Query: "network"}, Candidates: &CandidateOptions{Repo: "o/r", Number: 1}, Relevant: []TicketID{{"o/r", 1}}}}}} {
		if _, err := db.Evaluate(ctx, invalid); err == nil {
			t.Fatal("accepted invalid evaluation")
		}
	}
}
