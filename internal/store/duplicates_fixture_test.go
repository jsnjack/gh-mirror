package store

import (
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"gh-mirror/internal/embedding"
)

// duplicateFixturePath holds anonymized duplicate pairs from a real mirror.
const duplicateFixturePath = "testdata/duplicates.json.gz"

// duplicateFixture keeps ranking inputs without readable text: words are keyed
// hashes in shuffled order, and vectors share one discarded random rotation.
type duplicateFixture struct {
	Cases   []duplicateFixtureCase   `json:"cases"`
	Tickets []duplicateFixtureTicket `json:"tickets"`
	// Labels maps each distinct anonymized label list to its shared vector.
	Labels map[string]string `json:"labels"`
}
type duplicateFixtureCase struct {
	Name   string `json:"name"`
	Seed   int    `json:"seed"`
	Target int    `json:"target"`
}
type duplicateFixtureTicket struct {
	Number   int                       `json:"number"`
	Pull     bool                      `json:"pull_request,omitempty"`
	Closed   bool                      `json:"closed,omitempty"`
	Title    string                    `json:"title"`
	Labels   []string                  `json:"labels,omitempty"`
	Passages []duplicateFixturePassage `json:"passages"`
}

// duplicateFixturePassage is one indexed title, body chunk or label list.
type duplicateFixturePassage struct {
	Field  string `json:"field"`
	Text   string `json:"text,omitempty"`
	Vector string `json:"vector"`
}

// duplicateFixtureNeighbours bounds hard negatives taken from each engine per case.
const duplicateFixtureNeighbours = 20

// duplicateFixtureBackground adds random tickets so corpus statistics stay realistic.
const duplicateFixtureBackground = 600

// duplicateFixtureWords bounds each anonymized passage, and duplicateFixtureChunks
// bounds body passages per ticket; long bodies dominate fixture size.
const (
	duplicateFixtureWords  = 120
	duplicateFixtureChunks = 3
)

// TestWriteDuplicateFixture regenerates the fixture from a local mirror and a local
// judgment file. It never runs in normal test passes.
func TestWriteDuplicateFixture(t *testing.T) {
	source, casesPath := os.Getenv("GH_MIRROR_DUPLICATE_DB"), os.Getenv("GH_MIRROR_DUPLICATE_CASES")
	if source == "" || casesPath == "" {
		t.Skip("set GH_MIRROR_DUPLICATE_DB and GH_MIRROR_DUPLICATE_CASES to regenerate the duplicate fixture")
	}
	ctx := context.Background()
	raw, err := os.ReadFile(casesPath)
	if err != nil {
		t.Fatal(err)
	}
	var set EvaluationSet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	db, err := OpenWithVectorizer(source, true, embedding.Default)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	repo := ""
	keep := map[int]bool{}
	for _, c := range set.Cases {
		if c.Candidates == nil || len(c.Relevant) != 1 {
			t.Fatalf("case %s needs one candidate seed and one relevant ticket", c.Name)
		}
		if repo == "" {
			repo = c.Candidates.Repo
		}
		if c.Candidates.Repo != repo || c.Relevant[0].Repo != repo {
			t.Fatalf("case %s leaves repository %s", c.Name, repo)
		}
		keep[c.Candidates.Number], keep[c.Relevant[0].Number] = true, true
		for _, engine := range []string{"hybrid", "semantic", "lexical"} {
			out, err := db.FindCandidates(ctx, CandidateOptions{Repo: repo, Number: c.Candidates.Number, Engine: engine, Limit: duplicateFixtureNeighbours})
			if err != nil {
				t.Fatal(c.Name, engine, err)
			}
			for _, m := range out.Matches {
				keep[m.Number] = true
			}
		}
	}
	var all []int
	rows, err := db.db.QueryContext(ctx, "SELECT number FROM issues WHERE repo=? ORDER BY number", repo)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		all = append(all, n)
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		t.Fatal(err)
	}
	shuffle := mathrand.New(mathrand.NewPCG(randomUint64(t), randomUint64(t)))
	shuffle.Shuffle(len(all), func(a, b int) { all[a], all[b] = all[b], all[a] })
	for _, n := range all[:min(duplicateFixtureBackground, len(all))] {
		keep[n] = true
	}
	numbers := make([]int, 0, len(keep))
	for n := range keep {
		numbers = append(numbers, n)
	}
	shuffle.Shuffle(len(numbers), func(a, b int) { numbers[a], numbers[b] = numbers[b], numbers[a] })
	alias := map[int]int{}
	for n, number := range numbers {
		alias[number] = n + 1
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	rotation := randomRotation(shuffle, embedding.Dimension)
	fixture := duplicateFixture{Labels: map[string]string{}}
	for n, c := range set.Cases {
		fixture.Cases = append(fixture.Cases, duplicateFixtureCase{Name: anonymousCaseName(c.Name, n), Seed: alias[c.Candidates.Number], Target: alias[c.Relevant[0].Number]})
	}
	for _, number := range numbers {
		ticket, err := fixtureTicket(ctx, db.db, repo, number, key, rotation, shuffle)
		if err != nil {
			t.Fatal(number, err)
		}
		ticket.Number = alias[number]
		for n, p := range ticket.Passages {
			if p.Field == "labels" {
				fixture.Labels[strings.Join(ticket.Labels, "\n")] = p.Vector
				ticket.Passages = slices.Delete(ticket.Passages, n, n+1)
				break
			}
		}
		fixture.Tickets = append(fixture.Tickets, ticket)
	}
	sort.Slice(fixture.Tickets, func(a, b int) bool { return fixture.Tickets[a].Number < fixture.Tickets[b].Number })
	file, err := os.Create(duplicateFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	zw, _ := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err := json.NewEncoder(zw).Encode(fixture); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d cases and %d tickets", len(fixture.Cases), len(fixture.Tickets))
}

// anonymousCaseName keeps only names that were written for the fixture.
func anonymousCaseName(name string, n int) string {
	if strings.HasPrefix(name, "join_notifications") {
		return name
	}
	return fmt.Sprintf("duplicate_%02d", n+1)
}

func randomUint64(t *testing.T) uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return binary.LittleEndian.Uint64(b[:])
}

// randomRotation returns an orthogonal matrix from Gram-Schmidt over Gaussian rows.
func randomRotation(r *mathrand.Rand, dim int) [][]float64 {
	out := make([][]float64, dim)
	for i := range out {
		v := make([]float64, dim)
		for j := range v {
			v[j] = r.NormFloat64()
		}
		for _, u := range out[:i] {
			var d float64
			for j := range v {
				d += v[j] * u[j]
			}
			for j := range v {
				v[j] -= d * u[j]
			}
		}
		var norm float64
		for _, x := range v {
			norm += x * x
		}
		for j := range v {
			v[j] /= math.Sqrt(norm)
		}
		out[i] = v
	}
	return out
}

// anonymousText replaces every word except stopwords and single characters with a
// keyed hash. Shuffled order hides sentences; the bag of words keeps BM25 statistics.
func anonymousText(text string, key []byte, r *mathrand.Rand, shuffled bool) string {
	replace := func(word string) string {
		lower := strings.ToLower(word)
		if utf8.RuneCountInString(lower) < 2 || slices.Contains(candidateStopwords, lower) {
			return lower
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(lower))
		return "w" + strings.ToLower(base32.StdEncoding.EncodeToString(mac.Sum(nil))[:9])
	}
	if !shuffled {
		return words.ReplaceAllStringFunc(text, replace)
	}
	out := words.FindAllString(text, duplicateFixtureWords)
	for n, word := range out {
		out[n] = replace(word)
	}
	r.Shuffle(len(out), func(a, b int) { out[a], out[b] = out[b], out[a] })
	return strings.Join(out, " ")
}

func fixtureTicket(ctx context.Context, db *sql.DB, repo string, number int, key []byte, rotation [][]float64, r *mathrand.Rand) (duplicateFixtureTicket, error) {
	var out duplicateFixtureTicket
	var payload, kind, state string
	if err := db.QueryRowContext(ctx, "SELECT payload,kind,state FROM issues WHERE repo=? AND number=?", repo, number).Scan(&payload, &kind, &state); err != nil {
		return out, err
	}
	object, err := Object(json.RawMessage(payload))
	if err != nil {
		return out, err
	}
	out.Pull, out.Closed = kind == "pull_request", state == "closed"
	out.Title = anonymousText(Text(object, "title"), key, r, true)
	var labels []struct{ Name string }
	if raw := object["labels"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &labels); err != nil {
			return out, err
		}
	}
	for _, label := range labels {
		out.Labels = append(out.Labels, anonymousText(label.Name, key, r, false))
	}
	rows, err := db.QueryContext(ctx, `SELECT v.field,v.text,COALESCE(v.vector,e.vector) FROM documents d JOIN semantic_vectors v ON v.document_id=d.id LEFT JOIN embedding_cache e ON e.key=v.cache_key WHERE d.repo=? AND d.number=? AND v.field IN ('title','body','labels') ORDER BY d.id,v.chunk`, repo, number)
	if err != nil {
		return out, err
	}
	chunks := 0
	for rows.Next() {
		var field, text string
		var raw []byte
		if err := rows.Scan(&field, &text, &raw); err != nil {
			return out, finishRows(rows, err)
		}
		v := make([]float64, embedding.Dimension)
		for n := range v {
			v[n] = float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[n*4:])))
		}
		p := duplicateFixturePassage{Field: field, Vector: quantize(rotate(rotation, v))}
		if field == "body" {
			if chunks == duplicateFixtureChunks {
				continue
			}
			chunks++
			p.Text = anonymousText(text, key, r, true)
		}
		out.Passages = append(out.Passages, p)
	}
	return out, finishRows(rows, rows.Err())
}

func rotate(rotation [][]float64, v []float64) []float64 {
	out := make([]float64, len(v))
	for i, row := range rotation {
		for j, x := range v {
			out[i] += row[j] * x
		}
	}
	return out
}

// quantize stores a direction as signed bytes scaled to the largest coordinate.
// Four-bit coordinates moved borderline duplicates by several ranks.
func quantize(v []float64) string {
	var peak float64
	for _, x := range v {
		peak = math.Max(peak, math.Abs(x))
	}
	raw := make([]byte, len(v))
	for n, x := range v {
		raw[n] = byte(int8(math.Round(x / peak * 127)))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func dequantize(text string) ([]float32, error) {
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, err
	}
	if len(raw) != embedding.Dimension {
		return nil, fmt.Errorf("fixture vector has %d coordinates", len(raw))
	}
	out := make([]float32, len(raw))
	for n, b := range raw {
		out[n] = float32(int8(b))
	}
	return normalizeVector(out)
}
