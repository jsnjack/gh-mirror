package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gh-mirror/internal/embedding"
)

// SearchOptions selects literal text, ticket predicates, evidence and a result page.
type SearchOptions struct {
	Engine  string `json:"engine,omitempty"`
	Query   string `json:"query"`
	Repo    string `json:"repo,omitempty"`
	State   string `json:"state,omitempty"`
	Label   string `json:"label,omitempty"`
	Type    string `json:"type,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Project string `json:"project,omitempty"`
	QueryFilters
	PageOptions
	Match         string   `json:"match,omitempty"`
	Prefix        bool     `json:"prefix,omitempty"`
	In            []string `json:"in,omitempty"`
	ExcludeWords  []string `json:"exclude_words,omitempty"`
	Limit         int      `json:"limit,omitempty"`
	Cursor        string   `json:"cursor,omitempty"`
	EvidenceLimit int      `json:"evidence_limit,omitempty"`
	Exclude       int      `json:"-"`
	ExcludeRepo   string   `json:"-"`
	seed          *vectorSeed
}

// Match identifies a ranked ticket with bounded evidence and optional full records.
type Match struct {
	Repo          string          `json:"repo"`
	Number        int             `json:"number"`
	Title         string          `json:"title"`
	State         string          `json:"state"`
	Kind          string          `json:"kind"`
	URL           string          `json:"url"`
	Source        string          `json:"source"`
	Snippet       string          `json:"snippet"`
	Score         float64         `json:"score"`
	LexicalScore  *float64        `json:"lexical_score,omitempty"`
	SemanticScore *float64        `json:"semantic_score,omitempty"`
	UpdatedAt     string          `json:"updated_at"`
	Summary       *TicketSummary  `json:"summary"`
	Evidence      []Evidence      `json:"evidence"`
	Payload       json.RawMessage `json:"issue,omitempty"`
	Fields        json.RawMessage `json:"fields,omitempty"`
	Extra         json.RawMessage `json:"extra,omitempty"`
	createdAt     string
}

// Ranking describes how retrieval scores should be interpreted.
type Ranking struct {
	Algorithm   string `json:"algorithm"`
	Direction   string `json:"direction"`
	Description string `json:"description"`
}

// QueryInfo records the effective literal terms and matching mode.
type QueryInfo struct {
	Terms  []string `json:"terms"`
	Match  string   `json:"match"`
	Prefix bool     `json:"prefix"`
	In     []string `json:"in"`
	Engine string   `json:"engine,omitempty"`
}

// SearchResult returns one ranked page and the generation used for all its observations.
type SearchResult struct {
	Matches    []Match      `json:"matches"`
	NextCursor string       `json:"next_cursor"`
	HasMore    bool         `json:"has_more"`
	Total      *int         `json:"total,omitempty"`
	Facets     *FacetResult `json:"facets,omitempty"`
	Ranking    Ranking      `json:"ranking"`
	Query      QueryInfo    `json:"query"`
	Warnings   []Warning    `json:"warnings"`
	Status     Status       `json:"status"`
}

var words = regexp.MustCompile(`[\p{L}\p{N}_]+`)

func queryTerms(query string, unique bool) []string {
	out := []string{}
	for _, w := range words.FindAllString(strings.ToLower(query), -1) {
		if !unique || !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}
func quoteTerms(terms []string, prefix bool) []string {
	out := make([]string, len(terms))
	for n, w := range terms {
		out[n] = `"` + w + `"`
		if prefix {
			out[n] += "*"
		}
	}
	return out
}
func (o SearchOptions) filters() filters {
	return filters{o.Repo, o.State, o.Label, o.Type, o.Kind, o.Project, o.QueryFilters}
}

func scopedDocuments(term string, in []string, evidence, excerpts bool, selection string, selectionArgs []any) (string, []any) {
	selected := []string{}
	args := []any{}
	scopes := in
	if len(scopes) == 0 {
		scopes = []string{"all"}
	}
	for _, scope := range scopes {
		expression := term
		condition := "1=1"
		field := `CASE WHEN highlight(documents_fts,0,'[',']')<>d.title THEN 'title' ELSE 'body' END`
		column := "CASE WHEN highlight(documents_fts,0,'[',']')<>d.title THEN 0 ELSE 1 END"
		switch scope {
		case "title":
			expression = "title:(" + term + ")"
			condition = "d.id LIKE 'issue:%'"
			field = "'title'"
			column = "0"
		case "body":
			expression = "body:(" + term + ")"
			condition = "d.id LIKE 'issue:%'"
			field = "'body'"
			column = "1"
		case "labels":
			expression = "title:(" + term + ")"
			condition = "d.id LIKE 'labels:%'"
			field = "'labels'"
			column = "0"
		case "comments":
			condition = "d.id LIKE 'comment:%' AND d.id NOT LIKE 'comment:review:%'"
			field = "'comment'"
			column = "1"
		case "reviews":
			condition = "d.id LIKE 'comment:review:%'"
			field = "'review_comment'"
			column = "1"
		}
		if !excerpts && scope == "all" {
			field = "'ticket'"
		}
		projection := "d.repo,d.number"
		if evidence {
			projection = "d.repo,d.number,d.id,d.source,bm25(documents_fts,5,1) score,CASE WHEN d.id LIKE 'labels:%' THEN 'labels' WHEN d.id LIKE 'comment:review:%' THEN 'review_comment' WHEN d.id LIKE 'comment:%' THEN 'comment' ELSE " + field + " END field"
			if excerpts {
				projection += ",snippet(documents_fts," + column + ",'[',']',' … ',32) excerpt"
			} else {
				projection += ",'' excerpt"
			}
		}
		if selection != "" {
			condition += " AND (" + selection + ")"
		}
		selected = append(selected, "SELECT "+projection+" FROM documents_fts JOIN documents d ON d.rowid=documents_fts.rowid WHERE documents_fts MATCH ? AND "+condition)
		args = append(args, expression)
		args = append(args, selectionArgs...)
	}
	return strings.Join(selected, " UNION ALL "), args
}
func search(ctx context.Context, q querier, o SearchOptions) (SearchResult, error) {
	if o.Engine == "semantic" || o.Engine == "hybrid" {
		return vectorSearch(ctx, q, o, embedding.Default)
	}
	if o.Engine != "" && o.Engine != "lexical" {
		return SearchResult{}, invalid("engine must be lexical, semantic or hybrid")
	}
	out := SearchResult{Matches: []Match{}, Warnings: []Warning{}, Ranking: Ranking{"sqlite_fts5_bm25", "ascending", "Lower scores rank first; text relevance within this query, not duplicate probability."}}
	if len(o.Query) > 16384 {
		return out, invalid("query exceeds 16384 bytes")
	}
	if o.Match == "" {
		o.Match = "any"
	}
	if !slices.Contains([]string{"any", "all", "phrase"}, o.Match) {
		return out, invalid("match must be any, all or phrase")
	}
	terms := queryTerms(o.Query, o.Match != "phrase")
	if len(terms) == 0 {
		return out, invalid("query must contain searchable words")
	}
	if len(terms) > 32 {
		return out, invalid("query exceeds 32 terms; shorten it explicitly")
	}
	if len(o.ExcludeWords) > 32 {
		return out, invalid("at most 32 excluded words allowed")
	}
	for _, scope := range o.In {
		if !slices.Contains([]string{"title", "body", "labels", "comments", "reviews"}, scope) {
			return out, invalid("in must select title, body, labels, comments or reviews")
		}
	}
	o.In = slices.Compact(slices.Sorted(slices.Values(o.In)))
	limit, err := pageLimit(o.Limit)
	if err != nil {
		return out, err
	}
	o.Limit = limit
	if err := o.normalize(true); err != nil {
		return out, err
	}
	if o.EvidenceLimit == 0 {
		o.EvidenceLimit = 3
	}
	if o.EvidenceLimit < 1 || o.EvidenceLimit > 10 {
		return out, invalid("evidence_limit must be between 1 and 10")
	}
	filters, args, err := o.filters().sql()
	if err != nil {
		return out, err
	}
	expressions := quoteTerms(terms, o.Prefix)
	term := strings.Join(expressions, " OR ")
	if o.Match == "phrase" {
		term = `"` + strings.Join(terms, " ") + `"`
		if o.Prefix {
			term += "*"
		}
	}
	docs, docArgs := scopedDocuments(term, o.In, true, false, "", nil)
	ctes := []string{"hits AS MATERIALIZED (" + docs + ")"}
	baseArgs := append([]any{}, docArgs...)
	if o.Match == "all" {
		for n, expression := range expressions {
			sql, a := scopedDocuments(expression, o.In, false, false, "", nil)
			name := fmt.Sprintf("required%d", n)
			ctes = append(ctes, name+" AS ("+sql+")")
			baseArgs = append(baseArgs, a...)
			filters += " AND EXISTS(SELECT 1 FROM " + name + " t WHERE t.repo=i.repo AND t.number=i.number)"
		}
	}
	for n, excluded := range o.ExcludeWords {
		ws := queryTerms(excluded, true)
		if len(ws) == 0 || len(ws) > 32 {
			return out, invalid("excluded text must contain 1–32 words")
		}
		sql, a := scopedDocuments(strings.Join(quoteTerms(ws, false), " OR "), o.In, false, false, "", nil)
		name := fmt.Sprintf("excluded%d", n)
		ctes = append(ctes, name+" AS ("+sql+")")
		baseArgs = append(baseArgs, a...)
		filters += " AND NOT EXISTS(SELECT 1 FROM " + name + " t WHERE t.repo=i.repo AND t.number=i.number)"
	}
	if o.Exclude > 0 {
		filters += " AND NOT(i.repo=? AND i.number=?)"
		repo := o.ExcludeRepo
		if repo == "" {
			repo = o.Repo
		}
		args = append(args, repo, o.Exclude)
	}
	baseArgs = append(baseArgs, args...)
	ctes = append(ctes, `ranked AS (SELECT h.*,row_number() OVER(PARTITION BY h.repo,h.number ORDER BY h.score,h.source,h.id,h.field) ordinal FROM hits h JOIN issues i ON i.repo=h.repo AND i.number=h.number WHERE `+filters+`)`)
	ctes = append(ctes, `tickets AS (SELECT r.repo,r.number,r.source,r.excerpt,r.score,i.title,i.state,i.kind,i.url,i.updated_at,COALESCE(json_extract(i.payload,'$.created_at'),'') created_at,i.payload,i.fields,i.extra FROM ranked r JOIN issues i ON i.repo=r.repo AND i.number=r.number WHERE r.ordinal=1)`)
	cte := "WITH " + strings.Join(ctes, ",")
	fingerprint := o
	fingerprint.Limit = 0
	fingerprint.Cursor = ""
	sig, err := signature(struct {
		Options     SearchOptions
		Exclude     int
		ExcludeRepo string
	}{fingerprint, o.Exclude, o.ExcludeRepo})
	if err != nil {
		return out, err
	}
	out.Status, err = status(ctx, q)
	if err != nil {
		return out, err
	}
	cursor, err := decodeCursor(o.Cursor, "search", sig, out.Status.Generation)
	if err != nil {
		return out, err
	}
	if o.Cursor != "" && (cursor.Repo == "" || cursor.Number < 1) {
		return out, invalid("cursor has no ticket identity")
	}
	where, cursorArgs := continuation(o.PageOptions, cursor)
	pageArgs := append(append([]any{}, baseArgs...), cursorArgs...)
	pageArgs = append(pageArgs, limit+1)
	rows, err := q.QueryContext(ctx, cte+" SELECT repo,number,title,state,kind,url,source,excerpt,score,updated_at,created_at,payload,fields,extra FROM tickets WHERE "+where+" ORDER BY "+orderSQL(o.PageOptions)+" LIMIT ?", pageArgs...)
	if err != nil {
		return out, fmt.Errorf("search index: %w", err)
	}
	for rows.Next() {
		var m Match
		var payload, fields, extra json.RawMessage
		if err := rows.Scan(&m.Repo, &m.Number, &m.Title, &m.State, &m.Kind, &m.URL, &m.Source, &m.Snippet, &m.Score, &m.UpdatedAt, &m.createdAt, (*[]byte)(&payload), (*[]byte)(&fields), (*[]byte)(&extra)); err != nil {
			return out, finishRows(rows, err)
		}
		m.Summary, err = summarize(payload, extra)
		if err != nil {
			return out, finishRows(rows, err)
		}
		m.Evidence = []Evidence{}
		if o.View == "full" {
			m.Payload, m.Fields, m.Extra = payload, fields, extra
		}
		out.Matches = append(out.Matches, m)
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return out, err
	}
	out.HasMore = len(out.Matches) > limit
	if out.HasMore {
		out.Matches = out.Matches[:limit]
		last := out.Matches[limit-1]
		key := last.UpdatedAt
		if o.Sort == "created" {
			key = last.createdAt
		}
		out.NextCursor, err = encodeCursor(pageCursor{Generation: out.Status.Generation, Signature: sig, Kind: "search", Repo: last.Repo, Number: last.Number, Key: key, Score: last.Score})
		if err != nil {
			return out, err
		}
	}
	if o.Count {
		var total int
		if err := q.QueryRowContext(ctx, cte+" SELECT count(*) FROM tickets", baseArgs...).Scan(&total); err != nil {
			return out, fmt.Errorf("count search matches: %w", err)
		}
		out.Total = &total
	}
	if o.Facets {
		out.Facets, err = facets(ctx, q, cte, "SELECT repo,number FROM tickets", baseArgs)
		if err != nil {
			return out, err
		}
	}
	if len(out.Matches) > 0 {
		selected := []string{}
		selectionArgs := []any{}
		indices := map[string]int{}
		for n, m := range out.Matches {
			selected = append(selected, "(d.repo=? AND d.number=?)")
			selectionArgs = append(selectionArgs, m.Repo, m.Number)
			indices[fmt.Sprintf("%s#%d", m.Repo, m.Number)] = n
		}
		boundedDocs, boundedArgs := scopedDocuments(term, o.In, true, true, strings.Join(selected, " OR "), selectionArgs)
		evidenceCTEs := append([]string{}, ctes...)
		evidenceCTEs[0] = "hits AS MATERIALIZED (" + boundedDocs + ")"
		evidenceCTE := "WITH " + strings.Join(evidenceCTEs, ",")
		evidenceArgs := append(boundedArgs, baseArgs[len(docArgs):]...)
		evidenceArgs = append(evidenceArgs, o.EvidenceLimit)
		sql := evidenceCTE + `,dedup AS (SELECT r.*,row_number() OVER(PARTITION BY r.repo,r.number,r.id ORDER BY r.score,r.field) doc_ordinal FROM ranked r), evidence AS (SELECT *,row_number() OVER(PARTITION BY repo,number ORDER BY score,source,id) evidence_ordinal FROM dedup WHERE doc_ordinal=1) SELECT e.repo,e.number,e.field,e.source,e.excerpt,COALESCE(CAST(json_extract(c.payload,'$.id') AS TEXT),''),COALESCE(json_extract(c.payload,'$.created_at'),''),COALESCE(json_extract(c.payload,'$.path'),''),COALESCE(json_extract(c.payload,'$.line'),json_extract(c.payload,'$.original_line'),0),substr(COALESCE(json_extract(c.payload,'$.diff_hunk'),''),1,1024) FROM evidence e LEFT JOIN comments c ON c.id=substr(e.id,9) WHERE e.evidence_ordinal<=? ORDER BY e.repo,e.number,e.evidence_ordinal`
		rows, err := q.QueryContext(ctx, sql, evidenceArgs...)
		if err != nil {
			return out, fmt.Errorf("read match evidence: %w", err)
		}
		for rows.Next() {
			var repo string
			var number int
			var e Evidence
			if err := rows.Scan(&repo, &number, &e.Field, &e.Source, &e.Snippet, &e.CommentID, &e.CreatedAt, &e.Path, &e.Line, &e.DiffHunk); err != nil {
				return out, finishRows(rows, err)
			}
			n := indices[fmt.Sprintf("%s#%d", repo, number)]
			out.Matches[n].Evidence = append(out.Matches[n].Evidence, e)
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return out, err
		}
		for n := range out.Matches {
			if len(out.Matches[n].Evidence) > 0 {
				out.Matches[n].Snippet = out.Matches[n].Evidence[0].Snippet
			}
		}
	}
	out.Query = QueryInfo{Terms: terms, Match: o.Match, Prefix: o.Prefix, In: o.In, Engine: "lexical"}
	if out.Query.In == nil {
		out.Query.In = []string{"title", "body", "labels", "comments", "reviews"}
	}
	out.Warnings = queryWarnings(out.Status, o.filters(), o.PageOptions, o.In)
	return out, nil
}

// Search retrieves a consistent local page without exposing SQL or raw FTS syntax.
func (s *Store) Search(ctx context.Context, o SearchOptions) (SearchResult, error) {
	var out SearchResult
	err := s.view(ctx, func(tx *sql.Tx) error { var err error; out, err = search(ctx, tx, o); return err })
	return out, err
}
