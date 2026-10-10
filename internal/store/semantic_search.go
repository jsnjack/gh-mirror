package store

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

const fusionConstant = 60

type semanticHit struct {
	id               TicketID
	score            float64
	lexical          *float64
	evidence         []scoredEvidence
	created, updated string
}
type scoredEvidence struct {
	document, field, text, source string
	score                         float64
	start, end                    int
}

type vectorSeed struct {
	Ticket   TicketID
	Labels   bool
	Comments []string
}

func vectorSearch(ctx context.Context, q querier, o SearchOptions, e Vectorizer) (SearchResult, error) {
	out := SearchResult{Matches: []Match{}, Warnings: []Warning{}}
	if len(o.Query) > 16384 || strings.TrimSpace(o.Query) == "" {
		return out, invalid("semantic query must contain text and fit 16384 bytes")
	}
	if o.Match != "" && o.Match != "any" || o.Prefix {
		return out, invalid("match=all/phrase and prefix apply only to lexical search")
	}
	for _, field := range o.In {
		if !slices.Contains([]string{"title", "body", "labels", "comments", "reviews"}, field) {
			return out, invalid("invalid search field %q", field)
		}
	}
	o.In = slices.Compact(slices.Sorted(slices.Values(o.In)))
	if o.Order == "" && (o.Sort == "" || o.Sort == "relevance") {
		o.Order = "desc"
	}
	if err := o.normalize(true); err != nil {
		return out, err
	}
	limit, err := pageLimit(o.Limit)
	if err != nil {
		return out, err
	}
	o.Limit = limit
	if o.EvidenceLimit == 0 {
		o.EvidenceLimit = 3
	}
	if o.EvidenceLimit < 1 || o.EvidenceLimit > 10 {
		return out, invalid("evidence_limit must be between 1 and 10")
	}
	where, args, err := o.filters().sql()
	if err != nil {
		return out, err
	}
	if o.Exclude > 0 {
		where += " AND NOT(i.repo=? AND i.number=?)"
		repo := o.ExcludeRepo
		if repo == "" {
			repo = o.Repo
		}
		args = append(args, repo, o.Exclude)
	}
	if len(o.ExcludeWords) > 32 {
		return out, invalid("at most 32 excluded words allowed")
	}
	for _, text := range o.ExcludeWords {
		terms := queryTerms(text, true)
		if len(terms) == 0 || len(terms) > 32 {
			return out, invalid("excluded text must contain 1–32 words")
		}
		docs, a := scopedDocuments(strings.Join(quoteTerms(terms, false), " OR "), o.In, false, false, "", nil)
		where += " AND NOT EXISTS(SELECT 1 FROM (" + docs + ") x WHERE x.repo=i.repo AND x.number=i.number)"
		args = append(args, a...)
	}
	out.Status, err = status(ctx, q)
	if err != nil {
		return out, err
	}
	if out.Status.SchemaVersion < 2 || out.Status.Semantic == nil || out.Status.Semantic.Fingerprint != e.ID() {
		return out, fmt.Errorf("%w; run gh-mirror index with this binary", ErrSemanticUnavailable)
	}
	// Coverage follows ticket predicates; field selection can exclude pending comments or bodies.
	fieldWhere, fieldArgs := semanticFields(o.In)
	pendingSQL := `SELECT count(*) FROM documents d JOIN issues i ON i.repo=d.repo AND i.number=d.number LEFT JOIN semantic_documents s ON s.document_id=d.id WHERE (` + where + `) AND (s.document_id IS NULL OR s.complete=0 OR s.model<>?)`
	pendingArgs := append(append([]any{}, args...), e.ID())
	if len(o.In) > 0 {
		conditions := []string{}
		for _, f := range o.In {
			switch f {
			case "title", "body":
				conditions = append(conditions, "d.id LIKE 'issue:%'")
			case "labels":
				conditions = append(conditions, "d.id LIKE 'labels:%'")
			case "comments":
				conditions = append(conditions, "(d.id LIKE 'comment:%' AND d.id NOT LIKE 'comment:review:%')")
			case "reviews":
				conditions = append(conditions, "d.id LIKE 'comment:review:%'")
			}
		}
		pendingSQL += " AND (" + strings.Join(conditions, " OR ") + ")"
	}
	var pending int
	if err := q.QueryRowContext(ctx, pendingSQL, pendingArgs...).Scan(&pending); err != nil {
		return out, fmt.Errorf("check semantic coverage: %w", err)
	}
	if pending > 0 {
		return out, fmt.Errorf("%w: %d selected documents need indexing; run gh-mirror index", ErrSemanticUnavailable, pending)
	}
	queryVector, err := queryEmbedding(ctx, q, o, e)
	if err != nil {
		return out, err
	}
	rows, err := q.QueryContext(ctx, `SELECT d.repo,d.number,d.id,d.source,v.field,v.text,v.start,v.end,v.vector,i.updated_at,COALESCE(json_extract(i.payload,'$.created_at'),'') FROM semantic_vectors v JOIN semantic_documents s ON s.document_id=v.document_id JOIN documents d ON d.id=v.document_id JOIN issues i ON i.repo=d.repo AND i.number=d.number WHERE s.complete=1 AND s.model=? AND (`+fieldWhere+`) AND (`+where+`)`, append(append([]any{e.ID()}, fieldArgs...), args...)...)
	if err != nil {
		return out, fmt.Errorf("scan semantic vectors: %w", err)
	}
	hits := map[TicketID]*semanticHit{}
	for rows.Next() {
		var id TicketID
		var evidence scoredEvidence
		var raw []byte
		var updated, created string
		if err := rows.Scan(&id.Repo, &id.Number, &evidence.document, &evidence.source, &evidence.field, &evidence.text, &evidence.start, &evidence.end, &raw, &updated, &created); err != nil {
			return out, finishRows(rows, err)
		}
		score, err := vectorDot(queryVector, raw)
		if err != nil {
			return out, finishRows(rows, err)
		}
		evidence.score = score
		hit := hits[id]
		if hit == nil {
			hit = &semanticHit{id: id, score: score, created: created, updated: updated}
			hits[id] = hit
		}
		if score > hit.score {
			hit.score = score
		}
		hit.evidence = append(hit.evidence, evidence)
		sort.Slice(hit.evidence, func(a, b int) bool {
			if hit.evidence[a].score == hit.evidence[b].score {
				if hit.evidence[a].document != hit.evidence[b].document {
					return hit.evidence[a].document < hit.evidence[b].document
				}
				if hit.evidence[a].field != hit.evidence[b].field {
					return hit.evidence[a].field < hit.evidence[b].field
				}
				return hit.evidence[a].start < hit.evidence[b].start
			}
			return hit.evidence[a].score > hit.evidence[b].score
		})
		seen := map[string]bool{}
		distinct := hit.evidence[:0]
		for _, ev := range hit.evidence {
			if !seen[ev.document] {
				seen[ev.document] = true
				distinct = append(distinct, ev)
			}
		}
		hit.evidence = distinct
		if len(hit.evidence) > o.EvidenceLimit {
			hit.evidence = hit.evidence[:o.EvidenceLimit]
		}
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return out, err
	}
	ranked := make([]*semanticHit, 0, len(hits))
	for _, h := range hits {
		ranked = append(ranked, h)
	}
	sort.Slice(ranked, func(a, b int) bool {
		return semanticLess(ranked[a], ranked[b], PageOptions{Sort: "relevance", Order: "desc"})
	})
	semanticScores := map[TicketID]float64{}
	for n, h := range ranked {
		semanticScores[h.id] = h.score
		if o.Engine == "hybrid" {
			h.score = 1 / float64(fusionConstant+n+1)
		}
	}
	if o.Engine == "hybrid" {
		lexical, err := lexicalRanks(ctx, q, o, where, args)
		if err != nil {
			return out, err
		}
		for n, item := range lexical {
			if hit := hits[item.id]; hit != nil {
				value := item.score
				hit.lexical = &value
				hit.score += 1 / float64(fusionConstant+n+1)
			}
		}
	}
	sort.Slice(ranked, func(a, b int) bool { return semanticLess(ranked[a], ranked[b], o.PageOptions) })
	fp := o
	fp.Limit = 0
	fp.Cursor = ""
	sig, err := signature(struct {
		Options SearchOptions
		Exclude int
		Repo    string
		Seed    *vectorSeed
	}{fp, o.Exclude, o.ExcludeRepo, o.seed})
	if err != nil {
		return out, err
	}
	queryBytes, err := encodeVector(queryVector)
	if err != nil {
		return out, fmt.Errorf("fingerprint query vector: %w", err)
	}
	generation := out.Status.Generation + ":" + out.Status.Semantic.Generation + ":" + e.ID() + ":" + hash(queryBytes)
	cursor, err := decodeCursor(o.Cursor, "vector_search", sig, generation)
	if err != nil {
		return out, err
	}
	if o.Cursor != "" && (cursor.Repo == "" || cursor.Number < 1) {
		return out, invalid("cursor has no ticket identity")
	}
	if o.Count {
		total := len(ranked)
		out.Total = &total
	}
	if o.Facets {
		selected := `SELECT DISTINCT d.repo,d.number FROM semantic_vectors v JOIN semantic_documents s ON s.document_id=v.document_id JOIN documents d ON d.id=v.document_id JOIN issues i ON i.repo=d.repo AND i.number=d.number WHERE s.complete=1 AND s.model=? AND (` + fieldWhere + `) AND (` + where + `)`
		out.Facets, err = facets(ctx, q, "WITH vector_matches AS ("+selected+")", `SELECT repo,number FROM vector_matches`, append(append([]any{e.ID()}, fieldArgs...), args...))
		if err != nil {
			return out, err
		}
	}
	start := 0
	if o.Cursor != "" {
		found := false
		for n, h := range ranked {
			if h.id.Repo == cursor.Repo && h.id.Number == cursor.Number && h.score == cursor.Score {
				start = n + 1
				found = true
				break
			}
		}
		if !found {
			return out, invalid("cursor ticket is absent from these results")
		}
	}
	end := min(start+limit, len(ranked))
	out.HasMore = end < len(ranked)
	for _, h := range ranked[start:end] {
		ticket, err := readTicket(ctx, q, h.id.Repo, h.id.Number, "full")
		if err != nil {
			return out, err
		}
		m := Match{Repo: h.id.Repo, Number: h.id.Number, Title: ticket.Summary.Title, State: ticket.Summary.State, Kind: ticket.Kind, URL: ticket.Summary.URL, UpdatedAt: h.updated, Summary: ticket.Summary, Score: h.score, LexicalScore: h.lexical, Evidence: []Evidence{}, createdAt: h.created}
		score := semanticScores[h.id]
		m.SemanticScore = &score
		for _, hit := range h.evidence {
			ev, err := semanticEvidence(ctx, q, hit)
			if err != nil {
				return out, err
			}
			m.Evidence = append(m.Evidence, ev)
		}
		if len(m.Evidence) > 0 {
			m.Source = m.Evidence[0].Source
			m.Snippet = m.Evidence[0].Snippet
		}
		if o.View == "full" {
			m.Payload, m.Fields, m.Extra = ticket.Payload, ticket.Fields, ticket.Extra
		}
		out.Matches = append(out.Matches, m)
	}
	if out.HasMore {
		last := out.Matches[len(out.Matches)-1]
		out.NextCursor, err = encodeCursor(pageCursor{Generation: generation, Signature: sig, Kind: "vector_search", Repo: last.Repo, Number: last.Number, Score: last.Score})
		if err != nil {
			return out, err
		}
	}
	out.Ranking = Ranking{"cosine_similarity", "descending", "Higher scores rank first. Similarity is not duplicate probability."}
	if o.Engine == "hybrid" {
		out.Ranking = Ranking{"reciprocal_rank_fusion", "descending", "Sum of 1/(60+rank) across independent lexical and semantic rankings; higher is better."}
	}
	out.Query = QueryInfo{Terms: queryTerms(o.Query, true), Match: "any", In: o.In, Engine: o.Engine}
	if out.Query.In == nil {
		out.Query.In = []string{"title", "body", "labels", "comments", "reviews"}
	}
	out.Warnings = queryWarnings(out.Status, o.filters(), o.PageOptions, o.In)
	return out, nil
}
func semanticFields(in []string) (string, []any) {
	if len(in) == 0 {
		return "1=1", nil
	}
	holders := []string{}
	args := []any{}
	for _, f := range in {
		switch f {
		case "comments":
			f = "comment"
		case "reviews":
			f = "review_comment"
		}
		holders = append(holders, "?")
		args = append(args, f)
	}
	return "v.field IN (" + strings.Join(holders, ",") + ")", args
}
func vectorDot(query []float32, raw []byte) (float64, error) {
	if len(raw) != len(query)*4 {
		return 0, fmt.Errorf("stored vector has %d bytes; expected %d", len(raw), len(query)*4)
	}
	var sum float64
	for n, x := range query {
		v := math.Float32frombits(binary.LittleEndian.Uint32(raw[n*4:]))
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return 0, fmt.Errorf("stored vector contains non-finite coordinates")
		}
		sum += float64(x) * float64(v)
	}
	return math.Max(-1, math.Min(1, sum)), nil
}
func semanticLess(a, b *semanticHit, p PageOptions) bool {
	if p.Sort == "number" {
		if a.id.Repo != b.id.Repo {
			if p.Order == "desc" {
				return a.id.Repo > b.id.Repo
			}
			return a.id.Repo < b.id.Repo
		}
		if p.Order == "desc" {
			return a.id.Number > b.id.Number
		}
		return a.id.Number < b.id.Number
	}
	if p.Sort == "relevance" && a.score != b.score {
		if p.Order == "asc" {
			return a.score < b.score
		}
		return a.score > b.score
	}
	if p.Sort == "updated" || p.Sort == "created" {
		x, y := a.updated, b.updated
		if p.Sort == "created" {
			x, y = a.created, b.created
		}
		if x != y {
			if p.Order == "asc" {
				return x < y
			}
			return x > y
		}
	}
	if a.id.Repo != b.id.Repo {
		return a.id.Repo < b.id.Repo
	}
	return a.id.Number < b.id.Number
}
func lexicalRanks(ctx context.Context, q querier, o SearchOptions, where string, filterArgs []any) ([]semanticHit, error) {
	terms := queryTerms(o.Query, true)
	if len(terms) > 512 {
		return nil, invalid("hybrid query exceeds 512 literal terms; shorten it explicitly")
	}
	if len(terms) == 0 {
		return []semanticHit{}, nil
	}
	docs, args := scopedDocuments(strings.Join(quoteTerms(terms, false), " OR "), o.In, true, false, "", nil)
	rows, err := q.QueryContext(ctx, `WITH hits AS MATERIALIZED (`+docs+`) SELECT h.repo,h.number,min(h.score) score FROM hits h JOIN issues i ON i.repo=h.repo AND i.number=h.number WHERE `+where+` GROUP BY h.repo,h.number ORDER BY score,h.repo,h.number`, append(args, filterArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("rank hybrid lexical matches: %w", err)
	}
	out := []semanticHit{}
	for rows.Next() {
		var h semanticHit
		if err := rows.Scan(&h.id.Repo, &h.id.Number, &h.score); err != nil {
			return nil, finishRows(rows, err)
		}
		out = append(out, h)
	}
	return out, finishRows(rows, rows.Err())
}
func semanticEvidence(ctx context.Context, q querier, h scoredEvidence) (Evidence, error) {
	text := []rune(h.text)
	if len(text) > 1024 {
		text = append(text[:1024], '…')
	}
	out := Evidence{Field: h.field, Source: h.source, Snippet: string(text), Start: h.start, End: h.end, SemanticScore: &h.score}
	if !strings.HasPrefix(h.document, "comment:") {
		return out, nil
	}
	err := q.QueryRowContext(ctx, `SELECT COALESCE(CAST(json_extract(payload,'$.id') AS TEXT),''),COALESCE(json_extract(payload,'$.created_at'),''),COALESCE(json_extract(payload,'$.path'),''),COALESCE(json_extract(payload,'$.line'),json_extract(payload,'$.original_line'),0),substr(COALESCE(json_extract(payload,'$.diff_hunk'),''),1,1024) FROM comments WHERE id=?`, strings.TrimPrefix(h.document, "comment:")).Scan(&out.CommentID, &out.CreatedAt, &out.Path, &out.Line, &out.DiffHunk)
	if err != nil {
		return out, fmt.Errorf("read semantic evidence: %w", err)
	}
	return out, nil
}

func queryEmbedding(ctx context.Context, q querier, o SearchOptions, e Vectorizer) ([]float32, error) {
	if o.seed != nil {
		return seedEmbedding(ctx, q, o.seed, e)
	}
	chunks, err := e.Chunks(o.Query)
	if err != nil {
		return nil, err
	}
	if len(chunks) > 16 {
		return nil, invalid("semantic query exceeds 16 passages; shorten it explicitly")
	}
	v := make([]float32, e.Dim())
	parts := make([][]float32, len(chunks))
	if batcher, ok := e.(BatchVectorizer); ok {
		texts := make([]string, len(chunks))
		for n, c := range chunks {
			texts[n] = c.Text
		}
		parts, err = batcher.EmbedBatch(ctx, texts)
		if err != nil {
			return nil, err
		}
		if len(parts) != len(chunks) {
			return nil, fmt.Errorf("query encoder returned wrong batch length")
		}
	} else {
		for n, chunk := range chunks {
			parts[n], err = e.Embed(ctx, chunk.Text)
			if err != nil {
				return nil, err
			}
		}
	}
	for _, part := range parts {
		if len(part) != e.Dim() {
			return nil, fmt.Errorf("query encoder returned wrong vector dimension")
		}
		for n, x := range part {
			v[n] += x
		}
	}
	return normalizeVector(v)
}
func normalizeVector(v []float32) ([]float32, error) {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return nil, invalid("query produces an empty embedding")
	}
	for n := range v {
		v[n] /= float32(math.Sqrt(norm))
	}
	return v, nil
}
func seedEmbedding(ctx context.Context, q querier, seed *vectorSeed, e Vectorizer) ([]float32, error) {
	docs := []string{fmt.Sprintf("issue:%s:%d", seed.Ticket.Repo, seed.Ticket.Number)}
	if seed.Labels {
		docs = append(docs, fmt.Sprintf("labels:%s:%d", seed.Ticket.Repo, seed.Ticket.Number))
	}
	docs = append(docs, seed.Comments...)
	holders := make([]string, len(docs))
	args := make([]any, len(docs))
	for n, id := range docs {
		holders[n] = "?"
		args[n] = id
	}
	rows, err := q.QueryContext(ctx, `SELECT d.id,COALESCE(s.complete,0),COALESCE(s.model,''),COALESCE(v.field,''),v.vector FROM documents d LEFT JOIN semantic_documents s ON s.document_id=d.id LEFT JOIN semantic_vectors v ON v.document_id=d.id WHERE d.id IN (`+strings.Join(holders, ",")+`) ORDER BY d.id,v.chunk`, args...)
	if err != nil {
		return nil, fmt.Errorf("read seed vectors: %w", err)
	}
	sums := map[string][]float32{}
	counts := map[string]int{}
	for rows.Next() {
		var id, model, field string
		var complete int
		var raw []byte
		if err := rows.Scan(&id, &complete, &model, &field, &raw); err != nil {
			return nil, finishRows(rows, err)
		}
		if complete != 1 || model != e.ID() {
			return nil, finishRows(rows, fmt.Errorf("%w: seed document %s needs indexing", ErrSemanticUnavailable, id))
		}
		if len(raw) == 0 {
			continue
		}
		if len(raw) != e.Dim()*4 {
			return nil, finishRows(rows, fmt.Errorf("seed vector dimension mismatch"))
		}
		if sums[field] == nil {
			sums[field] = make([]float32, e.Dim())
		}
		for n := range sums[field] {
			x := math.Float32frombits(binary.LittleEndian.Uint32(raw[n*4:]))
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return nil, finishRows(rows, fmt.Errorf("seed vector contains non-finite coordinates"))
			}
			sums[field][n] += x
		}
		counts[field]++
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return nil, err
	}
	out := make([]float32, e.Dim())
	for _, field := range []string{"title", "body", "labels", "comment", "review_comment"} {
		v := sums[field]
		if len(v) == 0 {
			continue
		}
		weight := float32(1)
		switch field {
		case "title":
			weight = 4
		case "labels":
			weight = 1.5
		case "comment", "review_comment":
			weight = 0.5
		}
		for n, x := range v {
			out[n] += x * weight / float32(counts[field])
		}
	}
	return normalizeVector(out)
}
