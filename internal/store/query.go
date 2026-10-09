package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
	RepositoryOptions map[string]json.RawMessage `json:"repository_options"`
	Upstream          string                     `json:"upstream"`
	EnrichedAt        string                     `json:"enriched_at"`
	SchemaVersion     int                        `json:"schema_version"`
	CollectionVersion int                        `json:"collection_version"`
	Generation        string                     `json:"generation"`
	CollectedAt       string                     `json:"collected_at"`
	Repositories      []string                   `json:"repositories"`
	Coverage          []Coverage                 `json:"coverage"`
	Issues            int                        `json:"issues"`
	Comments          int                        `json:"comments"`
	Semantic          *SemanticStatus            `json:"semantic,omitempty"`
}
type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func status(ctx context.Context, q querier) (Status, error) {
	s := Status{RepositoryOptions: map[string]json.RawMessage{}, SchemaVersion: SchemaVersion, CollectionVersion: LegacyCollectionVersion, Repositories: []string{}, Coverage: []Coverage{}}
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
		case "schema_version":
			version, err := strconv.Atoi(v)
			if err != nil {
				return s, finishRows(rows, fmt.Errorf("parse schema version: %w", err))
			}
			s.SchemaVersion = version
		case "repository_options":
			if err := json.Unmarshal([]byte(v), &s.RepositoryOptions); err != nil {
				return s, finishRows(rows, fmt.Errorf("decode repository options: %w", err))
			}
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
	if s.SchemaVersion >= 2 {
		s.Semantic, err = semanticStatus(ctx, q)
		if err != nil {
			return s, err
		}
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
		for _, query := range []string{"DELETE FROM issues", "DELETE FROM sync_status", "DELETE FROM issue_inventory"} {
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
			for _, query := range []string{"DELETE FROM issues WHERE repo=?", "DELETE FROM sync_status WHERE repo=?", "DELETE FROM issue_inventory WHERE repo=?"} {
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
	Summary     *TicketSummary    `json:"summary,omitempty"`
	CommentPage *CommentResult    `json:"comment_page,omitempty"`
	Repo        string            `json:"repo"`
	Number      int               `json:"number"`
	Kind        string            `json:"kind"`
	Payload     json.RawMessage   `json:"issue,omitempty"`
	Comments    []json.RawMessage `json:"comments"`
	Fields      json.RawMessage   `json:"fields,omitempty"`
	Extra       json.RawMessage   `json:"extra,omitempty"`
	Status      Status            `json:"status"`
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

// CatalogResult preserves complete catalog records and generation coverage.
type CatalogResult struct {
	Warnings   []Warning         `json:"warnings"`
	NextCursor string            `json:"next_cursor"`
	HasMore    bool              `json:"has_more"`
	Items      []json.RawMessage `json:"items"`
	Status     Status            `json:"status"`
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
