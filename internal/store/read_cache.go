package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
)

// readCache keeps results that depend only on the committed database for
// long-running readers: `mcp` and `serve` turn it on with EnableReadCache. A
// one-shot command would only pay to load it. Every entry belongs to one
// value of SQLite's data_version on a dedicated connection; that value changes
// whenever another connection or process commits, which drops every entry.
type readCache struct {
	mu      sync.Mutex
	conn    *sql.Conn
	enabled bool
	token   int64
	status  []byte
	pending map[string]int
	vectors *vectorRows
}

// readScope ties one read transaction to the data_version read before it began.
// The transaction's snapshot is therefore at least as new as the token.
type readScope struct {
	store *Store
	token int64
}

// readScopes maps an open read transaction to its scope. Query helpers receive
// only the transaction, so they look their scope up here.
var readScopes sync.Map

func scopeOf(q querier) (readScope, bool) {
	tx, ok := q.(*sql.Tx)
	if !ok {
		return readScope{}, false
	}
	value, ok := readScopes.Load(tx)
	if !ok {
		return readScope{}, false
	}
	return value.(readScope), true
}

// changeToken returns the current data_version, or false when caching is off.
func (s *Store) changeToken(ctx context.Context) (int64, bool) {
	c := &s.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return 0, false
	}
	if c.conn == nil {
		conn, err := s.db.Conn(context.WithoutCancel(ctx))
		if err != nil {
			return 0, false
		}
		c.conn = conn
	}
	var version int64
	if err := c.conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
		c.closeConn()
		return 0, false
	}
	if version != c.token {
		c.token, c.status, c.pending, c.vectors = version, nil, nil, nil
	}
	return version, true
}

func (c *readCache) closeConn() {
	if c.conn != nil {
		_ = c.conn.Close() // a failed close leaves nothing cached to protect
		c.conn = nil
	}
	c.token, c.status, c.pending, c.vectors = 0, nil, nil, nil
}

// EnableReadCache keeps status, coverage counts and semantic vectors between
// reads until the database changes. Use it in processes that serve many queries.
func (s *Store) EnableReadCache() {
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	s.cache.enabled = true
}

// close releases the dedicated connection and turns caching off.
func (c *readCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeConn()
	c.enabled = false
}

// cachedStatus returns a copy of the status computed for this scope's token.
func (r readScope) cachedStatus() (Status, bool) {
	c := &r.store.cache
	c.mu.Lock()
	raw := c.status
	current := c.token == r.token
	c.mu.Unlock()
	if !current || raw == nil {
		return Status{}, false
	}
	var out Status
	if err := json.Unmarshal(raw, &out); err != nil {
		return Status{}, false
	}
	return out, true
}

func (r readScope) saveStatus(s Status) {
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	c := &r.store.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == r.token {
		c.status = raw
	}
}

// pendingCount caches the semantic coverage check per filter for this token.
func (r readScope) pendingCount(ctx context.Context, q querier, query string, args []any) (int, error) {
	key := fmt.Sprintf("%s\x00%#v", query, args)
	c := &r.store.cache
	c.mu.Lock()
	if c.token == r.token && c.pending != nil {
		if n, ok := c.pending[key]; ok {
			c.mu.Unlock()
			return n, nil
		}
	}
	c.mu.Unlock()
	var n int
	if err := q.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == r.token {
		if c.pending == nil {
			c.pending = map[string]int{}
		}
		c.pending[key] = n
	}
	return n, nil
}

// vectorRow is one indexed passage with its stored vector.
type vectorRow struct {
	id       TicketID
	evidence scoredEvidence
	raw      []byte
}

// vectorRows holds every complete passage for one embedding model.
type vectorRows struct {
	model string
	rows  []vectorRow
}

// vectors loads all complete passages once per token and model. The load reads
// the same rows as the uncached scan without its ticket filters; callers apply
// the filters per search. Rows keep the order of a repository-filtered scan
// (documents_issue, then chunk), because seed neighbours sum float32 vectors
// and the sum depends on order.
func (r readScope) vectors(ctx context.Context, q querier, model string) (*vectorRows, error) {
	c := &r.store.cache
	c.mu.Lock()
	if c.token == r.token && c.vectors != nil && c.vectors.model == model {
		v := c.vectors
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	rows, err := q.QueryContext(ctx, `SELECT d.repo,d.number,d.id,d.source,v.field,v.text,v.start,v.end,v.cache_key,COALESCE(v.vector,cache.vector) FROM semantic_vectors v JOIN semantic_documents s ON s.document_id=v.document_id JOIN documents d ON d.id=v.document_id LEFT JOIN embedding_cache cache ON cache.key=v.cache_key WHERE s.complete=1 AND s.model=? ORDER BY d.repo,d.number,d.rowid,v.chunk`, model)
	if err != nil {
		return nil, fmt.Errorf("load semantic vectors: %w", err)
	}
	out := &vectorRows{model: model}
	shared := map[string][]byte{}
	for rows.Next() {
		var row vectorRow
		var key sql.NullString
		if err := rows.Scan(&row.id.Repo, &row.id.Number, &row.evidence.document, &row.evidence.source, &row.evidence.field, &row.evidence.text, &row.evidence.start, &row.evidence.end, &key, &row.raw); err != nil {
			return nil, finishRows(rows, err)
		}
		if key.Valid && key.String != "" {
			if same, ok := shared[key.String]; ok && string(same) == string(row.raw) {
				row.raw = same
			} else {
				shared[key.String] = row.raw
			}
		}
		out.rows = append(out.rows, row)
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == r.token {
		c.vectors = out
	}
	return out, nil
}

// scanVectors passes each cached passage that the search filters allow to
// consume, with the ticket's current updated_at. The filters are the same SQL
// the uncached scan joins on issues, evaluated once per search.
func (r readScope) scanVectors(ctx context.Context, q querier, model string, in []string, where string, args []any, consume func(TicketID, scoredEvidence, []byte, string) error) error {
	all, err := r.vectors(ctx, q, model)
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT i.repo,i.number,i.updated_at FROM issues i WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("select semantic tickets: %w", err)
	}
	allowed := map[TicketID]string{}
	for rows.Next() {
		var id TicketID
		var updated string
		if err := rows.Scan(&id.Repo, &id.Number, &updated); err != nil {
			return finishRows(rows, err)
		}
		allowed[id] = updated
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return err
	}
	fields := map[string]bool{}
	for _, f := range in {
		fields[sourceScopes[f].field] = true
	}
	for _, row := range all.rows {
		if len(fields) > 0 && !fields[row.evidence.field] {
			continue
		}
		updated, ok := allowed[row.id]
		if !ok {
			continue
		}
		if err := consume(row.id, row.evidence, row.raw, updated); err != nil {
			return err
		}
	}
	return nil
}
