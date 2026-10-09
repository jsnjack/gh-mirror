package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Coverage describes independently collected resource families for a repository.
type Coverage struct {
	Repo         string `json:"repo"`
	CollectedAt  string `json:"collected_at"`
	ReconciledAt string `json:"reconciled_at"`
	Fields       string `json:"fields"`
	Projects     string `json:"projects"`
}

// Status identifies the committed generation and collection scope.
type Status struct {
	Upstream          string     `json:"upstream"`
	EnrichedAt        string     `json:"enriched_at"`
	SchemaVersion     int        `json:"schema_version"`
	CollectionVersion int        `json:"collection_version"`
	Generation        string     `json:"generation"`
	CollectedAt       string     `json:"collected_at"`
	Repositories      []string   `json:"repositories"`
	Coverage          []Coverage `json:"coverage"`
	Issues            int        `json:"issues"`
	Comments          int        `json:"comments"`
}
type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func status(ctx context.Context, q querier) (Status, error) {
	s := Status{SchemaVersion: SchemaVersion, CollectionVersion: LegacyCollectionVersion, Repositories: []string{}, Coverage: []Coverage{}}
	rows, err := q.QueryContext(ctx, "SELECT key,value FROM metadata")
	if err != nil {
		return s, fmt.Errorf("read generation: %w", err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return s, finishRows(rows, err)
		}
		switch k {
		case "collection_version":
			version, err := strconv.Atoi(v)
			if err != nil {
				return s, finishRows(rows, fmt.Errorf("parse collection version: %w", err))
			}
			s.CollectionVersion = version
		case "upstream":
			s.Upstream = v
		case "enriched_at":
			s.EnrichedAt = v
		case "generation":
			s.Generation = v
		case "collected_at":
			s.CollectedAt = v
		case "repositories":
			if err := json.Unmarshal([]byte(v), &s.Repositories); err != nil {
				return s, finishRows(rows, err)
			}
		}
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return s, err
	}
	rows, err = q.QueryContext(ctx, "SELECT repo,collected_at,reconciled_at,fields,projects FROM sync_status ORDER BY repo")
	if err != nil {
		return s, fmt.Errorf("read coverage: %w", err)
	}
	for rows.Next() {
		var c Coverage
		if err := rows.Scan(&c.Repo, &c.CollectedAt, &c.ReconciledAt, &c.Fields, &c.Projects); err != nil {
			return s, finishRows(rows, err)
		}
		s.Coverage = append(s.Coverage, c)
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return s, err
	}
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM issues").Scan(&s.Issues); err != nil {
		return s, fmt.Errorf("count issues: %w", err)
	}
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM comments").Scan(&s.Comments); err != nil {
		return s, fmt.Errorf("count comments: %w", err)
	}
	return s, nil
}
func (s *Store) view(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin mirror read: %w", err)
	}
	defer rollback(tx)
	if err := f(tx); err != nil {
		return fmt.Errorf("query mirror: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finish mirror read: %w", err)
	}
	return nil
}

// Status reads a consistent generation, scope, coverage, and record counts.
func (s *Store) Status(ctx context.Context) (Status, error) {
	var out Status
	err := s.view(ctx, func(tx *sql.Tx) error { var err error; out, err = status(ctx, tx); return err })
	return out, err
}

// Status reads collection checkpoints within the writer transaction.
func (w *Writer) Status(ctx context.Context) (Status, error) { return status(ctx, w.tx) }

// SetCoverage records successful collection checkpoints.
func (w *Writer) SetCoverage(ctx context.Context, c Coverage) error {
	if _, err := w.tx.ExecContext(ctx, `INSERT INTO sync_status(repo,collected_at,reconciled_at,fields,projects) VALUES(?,?,?,?,?) ON CONFLICT(repo) DO UPDATE SET collected_at=excluded.collected_at,reconciled_at=excluded.reconciled_at,fields=excluded.fields,projects=excluded.projects`, c.Repo, c.CollectedAt, c.ReconciledAt, c.Fields, c.Projects); err != nil {
		return fmt.Errorf("set collection coverage: %w", err)
	}
	return nil
}

// ResetScope removes excluded resources and resets data when the upstream changes.
func (w *Writer) ResetScope(ctx context.Context, repos []string, resetCatalog, resetData bool) error {
	old, err := w.Status(ctx)
	if err != nil {
		return fmt.Errorf("read old scope: %w", err)
	}
	if resetData {
		for _, query := range []string{"DELETE FROM issues", "DELETE FROM sync_status"} {
			if _, err := w.tx.ExecContext(ctx, query); err != nil {
				return fmt.Errorf("reset upstream records: %w", err)
			}
		}
	}
	if resetData || !slices.Equal(old.Repositories, repos) {
		if _, err := w.tx.ExecContext(ctx, "DELETE FROM responses"); err != nil {
			return fmt.Errorf("reset upstream response cache: %w", err)
		}
	}
	for _, r := range old.Repositories {
		found := false
		for _, current := range repos {
			if r == current {
				found = true
			}
		}
		if !found {
			for _, query := range []string{"DELETE FROM issues WHERE repo=?", "DELETE FROM sync_status WHERE repo=?"} {
				if _, err := w.tx.ExecContext(ctx, query, r); err != nil {
					return fmt.Errorf("remove old scope: %w", err)
				}
			}
		}
	}
	if resetCatalog {
		if _, err := w.tx.ExecContext(ctx, "DELETE FROM catalog"); err != nil {
			return fmt.Errorf("reset catalogs: %w", err)
		}
	} else if _, err := w.tx.ExecContext(ctx, "DELETE FROM catalog WHERE kind IN ('project_fields','project_items')"); err != nil {
		return fmt.Errorf("remove obsolete project catalogs: %w", err)
	}
	return nil
}

// Issue contains complete raw upstream records plus independent GraphQL observations.
type Issue struct {
	Repo     string            `json:"repo"`
	Number   int               `json:"number"`
	Kind     string            `json:"kind"`
	Payload  json.RawMessage   `json:"issue"`
	Comments []json.RawMessage `json:"comments"`
	Fields   json.RawMessage   `json:"fields"`
	Extra    json.RawMessage   `json:"extra"`
	Status   Status            `json:"status"`
}

func get(ctx context.Context, q querier, repo string, number int) (Issue, error) {
	out := Issue{Repo: repo, Number: number, Comments: []json.RawMessage{}}
	err := q.QueryRowContext(ctx, "SELECT kind,payload,fields,extra FROM issues WHERE repo=? AND number=?", repo, number).Scan(&out.Kind, (*[]byte)(&out.Payload), (*[]byte)(&out.Fields), (*[]byte)(&out.Extra))
	if errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("issue %s#%d: %w", repo, number, ErrNotFound)
	}
	if err != nil {
		return out, fmt.Errorf("read issue: %w", err)
	}
	rows, err := q.QueryContext(ctx, "SELECT payload FROM comments WHERE repo=? AND number=? ORDER BY json_extract(payload,'$.created_at'),id", repo, number)
	if err != nil {
		return out, fmt.Errorf("read issue comments: %w", err)
	}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan((*[]byte)(&raw)); err != nil {
			return out, finishRows(rows, err)
		}
		out.Comments = append(out.Comments, raw)
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return out, err
	}
	out.Status, err = status(ctx, q)
	return out, err
}

// Get returns a ticket and all comments from the same database generation.
func (s *Store) Get(ctx context.Context, repo string, number int) (Issue, error) {
	var out Issue
	err := s.view(ctx, func(tx *sql.Tx) error { var err error; out, err = get(ctx, tx, repo, number); return err })
	return out, err
}

// SearchOptions specifies literal query words and issue filters.
type SearchOptions struct {
	Query   string `json:"query"`
	Repo    string `json:"repo,omitempty"`
	State   string `json:"state,omitempty"`
	Label   string `json:"label,omitempty"`
	Type    string `json:"type,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Exclude int    `json:"-"`
}

// Match identifies an issue and the best matching issue or comment document.
type Match struct {
	Repo      string  `json:"repo"`
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	State     string  `json:"state"`
	Kind      string  `json:"kind"`
	URL       string  `json:"url"`
	Source    string  `json:"source"`
	Snippet   string  `json:"snippet"`
	Score     float64 `json:"score"`
	UpdatedAt string  `json:"updated_at"`
}

// SearchResult includes ranked local matches and their collection generation.
type SearchResult struct {
	Matches []Match `json:"matches"`
	Status  Status  `json:"status"`
}

var words = regexp.MustCompile(`[\p{L}\p{N}_]+`)

func literal(query string) string {
	parts := []string{}
	seen := map[string]bool{}
	for _, word := range words.FindAllString(strings.ToLower(query), -1) {
		if !seen[word] {
			seen[word] = true
			parts = append(parts, `"`+word+`"`)
			if len(parts) == 32 {
				break
			}
		}
	}
	return strings.Join(parts, " OR ")
}
func search(ctx context.Context, q querier, o SearchOptions) (SearchResult, error) {
	out := SearchResult{Matches: []Match{}}
	if len(o.Query) > 16384 {
		return out, fmt.Errorf("query exceeds 16384 bytes")
	}
	term := literal(o.Query)
	if term == "" {
		return out, fmt.Errorf("query must contain searchable words")
	}
	if o.Limit == 0 {
		o.Limit = 30
	}
	if o.Limit < 1 || o.Limit > 100 {
		return out, fmt.Errorf("limit must be between 1 and 100")
	}
	if o.State != "" && o.State != "open" && o.State != "closed" {
		return out, fmt.Errorf("state must be open or closed")
	}
	rows, err := q.QueryContext(ctx, `WITH ranked AS MATERIALIZED (
 SELECT i.repo,i.number,i.title,i.state,i.kind,i.url,d.source,snippet(documents_fts,-1,'[',']',' … ',32) AS excerpt,bm25(documents_fts,5,1) AS score,i.updated_at
 FROM documents_fts JOIN documents d ON d.rowid=documents_fts.rowid JOIN issues i ON i.repo=d.repo AND i.number=d.number
 WHERE documents_fts MATCH ? AND (?='' OR i.repo=?) AND (?='' OR i.state=?)
 AND (?='' OR EXISTS(SELECT 1 FROM json_each(i.payload,'$.labels') l WHERE json_extract(l.value,'$.name')=?))
 AND (?='' OR json_extract(i.payload,'$.type.name')=?) AND NOT(i.repo=? AND i.number=?)
 ), grouped AS (SELECT *,row_number() OVER(PARTITION BY repo,number ORDER BY score,source) AS ordinal FROM ranked)
 SELECT repo,number,title,state,kind,url,source,excerpt,score,updated_at FROM grouped WHERE ordinal=1 ORDER BY score,repo,number LIMIT ?`, term, o.Repo, o.Repo, o.State, o.State, o.Label, o.Label, o.Type, o.Type, o.Repo, o.Exclude, o.Limit)
	if err != nil {
		return out, fmt.Errorf("search index: %w", err)
	}
	for rows.Next() {
		var m Match
		if err := rows.Scan(&m.Repo, &m.Number, &m.Title, &m.State, &m.Kind, &m.URL, &m.Source, &m.Snippet, &m.Score, &m.UpdatedAt); err != nil {
			return out, finishRows(rows, err)
		}
		out.Matches = append(out.Matches, m)
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return out, err
	}
	out.Status, err = status(ctx, q)
	return out, err
}

// Search retrieves literal terms locally without exposing SQL or FTS syntax.
func (s *Store) Search(ctx context.Context, o SearchOptions) (SearchResult, error) {
	var out SearchResult
	err := s.view(ctx, func(tx *sql.Tx) error { var err error; out, err = search(ctx, tx, o); return err })
	return out, err
}

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

// CatalogResult preserves complete catalog records and generation coverage.
type CatalogResult struct {
	Items  []json.RawMessage `json:"items"`
	Status Status            `json:"status"`
}

// Catalog retrieves a complete named catalog for a repository, owner, or project scope.
func (s *Store) Catalog(ctx context.Context, kind, scope string) (CatalogResult, error) {
	out := CatalogResult{Items: []json.RawMessage{}}
	err := s.view(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT payload FROM catalog WHERE kind=? AND scope=? ORDER BY id", kind, scope)
		if err != nil {
			return fmt.Errorf("read catalog: %w", err)
		}
		for rows.Next() {
			var raw json.RawMessage
			if err := rows.Scan((*[]byte)(&raw)); err != nil {
				return finishRows(rows, err)
			}
			out.Items = append(out.Items, raw)
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return err
		}
		out.Status, err = status(ctx, tx)
		return err
	})
	return out, err
}

// Export creates a standalone SQLite snapshot from one consistent read snapshot.
func (s *Store) Export(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("export SQLite snapshot: %w", err)
	}
	return nil
}

// Validate checks database integrity before publication or installation.
func (s *Store) Validate(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("check database integrity: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("database integrity: %s", result)
	}
	return nil
}
