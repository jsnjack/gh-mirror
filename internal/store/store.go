// Package store owns the SQLite mirror, transactional updates, and local retrieval.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"gh-mirror/internal/diagnostics"

	_ "modernc.org/sqlite"
)

// SchemaVersion identifies compatible mirror databases.
const SchemaVersion = 1

// ErrNotFound indicates that the requested local resource is absent.
var ErrNotFound = errors.New("local resource not found")

// Store maintains a SQLite connection pool and its filesystem location.
type Store struct {
	db   *sql.DB
	Path string
}

// Open opens a writer database or an existing read-only database.
func Open(path string, readOnly bool) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database: %w", err)
	}
	if readOnly {
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("open existing mirror: %w", err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
		f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("create database: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close database placeholder: %w", err)
		}
		if err := os.Chmod(abs, 0600); err != nil {
			return nil, fmt.Errorf("protect database: %w", err)
		}
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(2000)")
	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Set("_txlock", "immediate")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db, Path: abs}
	if err := s.initialize(readOnly); err != nil {
		s.closeOnError()
		return nil, err
	}
	return s, nil
}

func (s *Store) closeOnError() {
	if err := s.db.Close(); err != nil {
		slog.Log(context.Background(), diagnostics.TraceLevel, "close failed database", "error", err)
	}
}

func (s *Store) initialize(readOnly bool) error {
	if !readOnly {
		var exists int
		if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='metadata'").Scan(&exists); err != nil {
			return fmt.Errorf("inspect schema: %w", err)
		}
		if exists > 0 {
			var version string
			if err := s.db.QueryRow("SELECT value FROM metadata WHERE key='schema_version'").Scan(&version); err != nil {
				return fmt.Errorf("inspect existing schema: %w", err)
			}
			if version != strconv.Itoa(SchemaVersion) {
				return fmt.Errorf("unsupported database schema %q", version)
			}
		}
		if _, err := s.db.Exec(schema); err != nil {
			return fmt.Errorf("initialize schema: %w", err)
		}
		if _, err := s.db.Exec("INSERT INTO metadata(key,value) VALUES('schema_version',?) ON CONFLICT DO NOTHING", strconv.Itoa(SchemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
	}
	var version string
	if err := s.db.QueryRow("SELECT value FROM metadata WHERE key='schema_version'").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version != strconv.Itoa(SchemaVersion) {
		return fmt.Errorf("unsupported database schema %q", version)
	}
	return nil
}

// Close releases database connections.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close mirror: %w", err)
	}
	return nil
}

// Writer performs collection changes within one immediate transaction.
type Writer struct{ tx *sql.Tx }

// Update commits all changes together or rolls them back when collection fails.
func (s *Store) Update(ctx context.Context, update func(*Writer) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lock collector transaction: %w", err)
	}
	defer rollback(tx)
	if err := update(&Writer{tx: tx}); err != nil {
		return fmt.Errorf("collect mirror transaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mirror: %w", err)
	}
	return nil
}

func rollback(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		slog.Log(context.Background(), diagnostics.TraceLevel, "rollback mirror", "error", err)
	}
}

// Object decodes an upstream JSON object without rounding integer identities.
func Object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("expected upstream JSON object")
	}
	return object, nil
}

// Text returns a string property from an upstream object.
func Text(object map[string]json.RawMessage, key string) string {
	var text string
	if err := json.Unmarshal(object[key], &text); err == nil {
		return text
	}
	return ""
}

// Identity returns a numeric or string stable upstream identifier.
func Identity(object map[string]json.RawMessage, key string) string {
	if text := Text(object, key); text != "" {
		return text
	}
	var number json.Number
	if err := json.Unmarshal(object[key], &number); err == nil {
		return number.String()
	}
	return ""
}

// IssueRef identifies a stored issue for GraphQL hydration and reconciliation.
type IssueRef struct {
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	NodeID  string `json:"node_id"`
	Kind    string `json:"kind"`
	Changed bool   `json:"-"`
}

// PutIssue preserves raw issue metadata and updates its search document.
func (w *Writer) PutIssue(ctx context.Context, repo string, raw json.RawMessage) (IssueRef, error) {
	o, err := Object(raw)
	if err != nil {
		return IssueRef{}, fmt.Errorf("decode issue: %w", err)
	}
	number, err := strconv.Atoi(Identity(o, "number"))
	ref := IssueRef{Repo: repo, Number: number, NodeID: Text(o, "node_id"), Kind: "issue"}
	if _, ok := o["pull_request"]; ok {
		ref.Kind = "pull_request"
	}
	if err != nil || number < 1 || ref.NodeID == "" || Text(o, "updated_at") == "" || Text(o, "html_url") == "" {
		return ref, fmt.Errorf("incomplete upstream issue in %s", repo)
	}
	rows, err := w.tx.QueryContext(ctx, `SELECT c.payload FROM comments c JOIN issues i ON i.repo=c.repo AND i.number=c.number WHERE i.node_id=? AND (i.repo<>? OR i.number<>?)`, ref.NodeID, repo, number)
	if err != nil {
		return ref, fmt.Errorf("read transferred comments: %w", err)
	}
	transferred := []json.RawMessage{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan((*[]byte)(&raw)); err != nil {
			return ref, finishRows(rows, err)
		}
		transferred = append(transferred, raw)
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return ref, fmt.Errorf("read transferred comment inventory: %w", err)
	}
	if _, err := w.tx.ExecContext(ctx, "DELETE FROM issues WHERE node_id=? AND (repo<>? OR number<>?)", ref.NodeID, repo, number); err != nil {
		return ref, fmt.Errorf("relocate transferred issue: %w", err)
	}
	result, err := w.tx.ExecContext(ctx, `INSERT INTO issues(repo,number,node_id,kind,title,body,state,updated_at,url,payload) VALUES(?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(repo,number) DO UPDATE SET node_id=excluded.node_id,kind=excluded.kind,title=excluded.title,body=excluded.body,state=excluded.state,updated_at=excluded.updated_at,url=excluded.url,payload=excluded.payload
 WHERE excluded.updated_at>issues.updated_at OR (excluded.updated_at=issues.updated_at AND excluded.payload<>issues.payload)`, repo, number, ref.NodeID, ref.Kind, Text(o, "title"), Text(o, "body"), Text(o, "state"), Text(o, "updated_at"), Text(o, "html_url"), string(raw))
	if err != nil {
		return ref, fmt.Errorf("upsert %s#%d: %w", repo, number, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ref, fmt.Errorf("read issue update count: %w", err)
	}
	ref.Changed = n > 0
	if n > 0 {
		if err := w.document(ctx, "issue:"+repo+":"+strconv.Itoa(number), repo, number, Text(o, "html_url"), Text(o, "title"), Text(o, "body")); err != nil {
			return ref, err
		}
	}
	for _, raw := range transferred {
		if err := w.PutComment(ctx, repo, number, raw); err != nil {
			return ref, fmt.Errorf("restore transferred comment: %w", err)
		}
	}
	return ref, nil
}

func (w *Writer) document(ctx context.Context, id, repo string, number int, source, title, body string) error {
	if _, err := w.tx.ExecContext(ctx, `INSERT INTO documents(id,repo,number,source,title,body) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET repo=excluded.repo,number=excluded.number,source=excluded.source,title=excluded.title,body=excluded.body`, id, repo, number, source, title, body); err != nil {
		return fmt.Errorf("index document: %w", err)
	}
	return nil
}

// PutComment stores a complete comment and indexes its body.
func (w *Writer) PutComment(ctx context.Context, repo string, number int, raw json.RawMessage) error {
	o, err := Object(raw)
	if err != nil {
		return fmt.Errorf("decode comment: %w", err)
	}
	id := Identity(o, "id")
	if id == "" || Text(o, "updated_at") == "" || Text(o, "html_url") == "" {
		return fmt.Errorf("incomplete upstream comment in %s#%d", repo, number)
	}
	result, err := w.tx.ExecContext(ctx, `INSERT INTO comments(id,repo,number,body,updated_at,url,payload) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET repo=excluded.repo,number=excluded.number,body=excluded.body,updated_at=excluded.updated_at,url=excluded.url,payload=excluded.payload WHERE excluded.updated_at>comments.updated_at OR (excluded.updated_at=comments.updated_at AND (excluded.payload<>comments.payload OR excluded.repo<>comments.repo OR excluded.number<>comments.number))`, id, repo, number, Text(o, "body"), Text(o, "updated_at"), Text(o, "html_url"), string(raw))
	if err != nil {
		return fmt.Errorf("upsert comment %s: %w", id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read comment update count: %w", err)
	}
	if n > 0 {
		return w.document(ctx, "comment:"+id, repo, number, Text(o, "html_url"), "", Text(o, "body"))
	}
	return nil
}

// PutExtra replaces independently collected issue field values and relationships.
func (w *Writer) PutExtra(ctx context.Context, ref IssueRef, fields, extra json.RawMessage) error {
	if _, err := w.tx.ExecContext(ctx, "UPDATE issues SET fields=?,extra=? WHERE repo=? AND number=?", string(fields), string(extra), ref.Repo, ref.Number); err != nil {
		return fmt.Errorf("store issue fields and relationships: %w", err)
	}
	return nil
}

// ReplaceCatalog replaces a complete catalog only after upstream pagination succeeds.
func (w *Writer) ReplaceCatalog(ctx context.Context, kind, scope string, items []json.RawMessage) error {
	if _, err := w.tx.ExecContext(ctx, "DELETE FROM catalog WHERE kind=? AND scope=?", kind, scope); err != nil {
		return fmt.Errorf("clear %s catalog: %w", kind, err)
	}
	for _, raw := range items {
		o, err := Object(raw)
		if err != nil {
			return fmt.Errorf("decode %s catalog: %w", kind, err)
		}
		id := Identity(o, "id")
		if id == "" {
			id = Identity(o, "number")
		}
		if id == "" {
			return fmt.Errorf("missing %s catalog identity", kind)
		}
		if _, err := w.tx.ExecContext(ctx, "INSERT INTO catalog(kind,scope,id,payload) VALUES(?,?,?,?) ON CONFLICT(kind,scope,id) DO UPDATE SET payload=excluded.payload", kind, scope, id, string(raw)); err != nil {
			return fmt.Errorf("insert %s catalog: %w", kind, err)
		}
	}
	return nil
}

// Reconcile removes records absent from a complete upstream inventory.
func (w *Writer) Reconcile(ctx context.Context, repo string, issues map[int]bool, comments map[string]bool) error {
	rows, err := w.tx.QueryContext(ctx, "SELECT number FROM issues WHERE repo=?", repo)
	if err != nil {
		return fmt.Errorf("list reconciliation issues: %w", err)
	}
	var removed []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return finishRows(rows, fmt.Errorf("scan reconciliation issue: %w", err))
		}
		if !issues[n] {
			removed = append(removed, n)
		}
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return err
	}
	for _, n := range removed {
		if _, err := w.tx.ExecContext(ctx, "DELETE FROM issues WHERE repo=? AND number=?", repo, n); err != nil {
			return fmt.Errorf("remove absent issue: %w", err)
		}
	}
	rows, err = w.tx.QueryContext(ctx, "SELECT id FROM comments WHERE repo=?", repo)
	if err != nil {
		return fmt.Errorf("list reconciliation comments: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return finishRows(rows, fmt.Errorf("scan reconciliation comment: %w", err))
		}
		if !comments[id] {
			ids = append(ids, id)
		}
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := w.tx.ExecContext(ctx, "DELETE FROM comments WHERE id=?", id); err != nil {
			return fmt.Errorf("remove absent comment: %w", err)
		}
	}
	return nil
}

func finishRows(rows *sql.Rows, err error) error {
	closeErr := rows.Close()
	if err != nil {
		return fmt.Errorf("read database rows: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close database rows: %w", closeErr)
	}
	return nil
}

// IssueRefs returns the current issue inventory inside the collection transaction.
func (w *Writer) IssueRefs(ctx context.Context, repo string) ([]IssueRef, error) {
	rows, err := w.tx.QueryContext(ctx, "SELECT repo,number,node_id,kind FROM issues WHERE repo=? ORDER BY number", repo)
	if err != nil {
		return nil, fmt.Errorf("list issue inventory: %w", err)
	}
	refs := []IssueRef{}
	for rows.Next() {
		var r IssueRef
		if err := rows.Scan(&r.Repo, &r.Number, &r.NodeID, &r.Kind); err != nil {
			return nil, finishRows(rows, fmt.Errorf("scan issue inventory: %w", err))
		}
		refs = append(refs, r)
	}
	return refs, finishRows(rows, rows.Err())
}

// SetMetadata persists generation and scope in the published database.
func (w *Writer) SetMetadata(ctx context.Context, key, value string) error {
	if _, err := w.tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
		return fmt.Errorf("set %s metadata: %w", key, err)
	}
	return nil
}

// Cached retrieves an exact conditional HTTP response cached in the transaction.
func (w *Writer) Cached(ctx context.Context, key string) (string, []byte, error) {
	var etag string
	var body []byte
	err := w.tx.QueryRowContext(ctx, "SELECT etag,body FROM responses WHERE url=?", key).Scan(&etag, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("read HTTP cache: %w", err)
	}
	return etag, body, nil
}

// Cache saves successful conditional HTTP response data.
func (w *Writer) Cache(ctx context.Context, key, etag string, body []byte) error {
	if _, err := w.tx.ExecContext(ctx, "INSERT INTO responses(url,etag,body) VALUES(?,?,?) ON CONFLICT(url) DO UPDATE SET etag=excluded.etag,body=excluded.body", key, etag, body); err != nil {
		return fmt.Errorf("save HTTP cache: %w", err)
	}
	return nil
}
