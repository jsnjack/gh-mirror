package store

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"gh-mirror/internal/embedding"
)

// Minimum duplicate recovery on the anonymized fixture. Raise these when ranking
// improves; a change that lowers them needs new evidence, not a lower floor.
// The fixture keeps about a third of the source corpus, so the named pair ranks a
// few places lower here than in the full mirror (7 and 9 there).
const (
	duplicateMinMRR     = 0.48
	duplicateMinRecall  = 50
	duplicatePairWithin = 15
)

// fixtureVectorizer returns stored fixture vectors; body passages are separated by
// blank lines so chunking reproduces the source passages.
type fixtureVectorizer struct {
	fakeVectorizer
	vectors map[string][]float32
}

func (e *fixtureVectorizer) Chunks(text string) ([]embedding.Chunk, error) {
	out := []embedding.Chunk{}
	start := 0
	for _, part := range strings.SplitAfter(text, "\n\n") {
		if piece := strings.TrimSuffix(part, "\n\n"); piece != "" {
			out = append(out, embedding.Chunk{Text: piece, Start: start, End: start + len(piece)})
		}
		start += len(part)
	}
	return out, nil
}
func (e *fixtureVectorizer) Embed(_ context.Context, text string) ([]float32, error) {
	v, ok := e.vectors[text]
	if !ok {
		return nil, fmt.Errorf("fixture has no vector for %.40q", text)
	}
	return v, nil
}

func loadDuplicateFixture(t *testing.T) (*Store, duplicateFixture) {
	t.Helper()
	file, err := os.Open(duplicateFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	zr, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	var fixture duplicateFixture
	if err := json.NewDecoder(zr).Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	encoder := &fixtureVectorizer{vectors: map[string][]float32{}}
	for text, encoded := range fixture.Labels {
		v, err := dequantize(encoded)
		if err != nil {
			t.Fatal(err)
		}
		encoder.vectors[text] = v
	}
	db := testStore(t)
	ctx := context.Background()
	if err := db.Update(ctx, func(w *Writer) error {
		for _, ticket := range fixture.Tickets {
			body := []string{}
			for _, p := range ticket.Passages {
				v, err := dequantize(p.Vector)
				if err != nil {
					return err
				}
				switch p.Field {
				case "title":
					encoder.vectors[ticket.Title] = v
				default:
					encoder.vectors[p.Text] = v
					body = append(body, p.Text)
				}
			}
			labels := []map[string]string{}
			for _, label := range ticket.Labels {
				labels = append(labels, map[string]string{"name": label})
			}
			object := map[string]any{"number": ticket.Number, "node_id": fmt.Sprintf("D%d", ticket.Number), "title": ticket.Title, "body": strings.Join(body, "\n\n"), "labels": labels, "state": "open", "updated_at": "2026-01-01T00:00:00Z", "html_url": fmt.Sprintf("https://github.com/o/r/issues/%d", ticket.Number)}
			if ticket.Closed {
				object["state"] = "closed"
			}
			if ticket.Pull {
				object["pull_request"] = map[string]string{"url": fmt.Sprintf("https://api.github.com/repos/o/r/pulls/%d", ticket.Number)}
			}
			raw, err := json.Marshal(object)
			if err != nil {
				return err
			}
			if _, err := w.PutIssue(ctx, "o/r", raw); err != nil {
				return err
			}
		}
		for k, v := range map[string]string{"generation": "duplicates", "repositories": `["o/r"]`} {
			if err := w.SetMetadata(ctx, k, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Index(ctx, encoder, IndexOptions{Workers: 4}); err != nil {
		t.Fatal(err)
	}
	db.vectorizer = encoder
	return db, fixture
}

// TestDuplicateRegression replays anonymized real duplicate pairs through the
// default candidate engine and fails when recovery drops below recorded floors.
func TestDuplicateRegression(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("duplicate regression indexes the full fixture; make duplicates runs it without -race")
	}
	db, fixture := loadDuplicateFixture(t)
	var reciprocal float64
	found, judged := 0, 0
	for _, c := range fixture.Cases {
		out, err := db.FindCandidates(context.Background(), CandidateOptions{Repo: "o/r", Number: c.Seed, Engine: "hybrid", Limit: 20})
		if err != nil {
			t.Fatal(c.Name, err)
		}
		rank := 0
		for n, m := range out.Matches {
			if m.Number == c.Target {
				rank = n + 1
				break
			}
		}
		if strings.HasPrefix(c.Name, "join_notifications") {
			if rank == 0 || rank > duplicatePairWithin {
				t.Errorf("%s: duplicate rank %d; want top %d", c.Name, rank, duplicatePairWithin)
			}
			t.Logf("%s: duplicate rank %d", c.Name, rank)
			continue
		}
		judged++
		if rank > 0 {
			reciprocal += 1 / float64(rank)
		}
		if rank > 0 && rank <= 10 {
			found++
		}
	}
	mrr := reciprocal / float64(judged)
	t.Logf("duplicate MRR@20 %.3f, recall@10 %d/%d", mrr, found, judged)
	if mrr < duplicateMinMRR || found < duplicateMinRecall {
		t.Fatalf("duplicate recovery fell to MRR@20 %.3f, recall@10 %d/%d; floors are %.3f and %d", mrr, found, judged, duplicateMinMRR, duplicateMinRecall)
	}
}
