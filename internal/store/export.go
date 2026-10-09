package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// Export creates a standalone snapshot without collector caches or credential identity.
func (s *Store) Export(ctx context.Context, path string) (exportErr error) {
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("export SQLite snapshot: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("protect export: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve exported database: %w", err)
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("mode", "rw")
	q.Add("_pragma", "journal_mode(DELETE)")
	q.Add("_pragma", "secure_delete(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fmt.Errorf("open export for cache removal: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err := db.Close(); err != nil {
			exportErr = errors.Join(exportErr, fmt.Errorf("close sanitized export: %w", err))
		}
	}()
	if _, err := db.ExecContext(ctx, "DELETE FROM responses; DELETE FROM metadata WHERE key='credential_identity'"); err != nil {
		return fmt.Errorf("remove collector data from export: %w", err)
	}
	var semantic int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='semantic_documents'").Scan(&semantic); err != nil {
		return fmt.Errorf("inspect exported vector index: %w", err)
	}
	if semantic != 0 {
		if _, err := db.ExecContext(ctx, "DELETE FROM semantic_vectors WHERE document_id IN (SELECT document_id FROM semantic_documents WHERE complete=0); DELETE FROM semantic_documents WHERE complete=0"); err != nil {
			return fmt.Errorf("remove incomplete and obsolete passages from export: %w", err)
		}
	}
	var inventory int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='issue_inventory'").Scan(&inventory); err != nil {
		return fmt.Errorf("inspect exported inventory: %w", err)
	}
	if inventory != 0 {
		if _, err := db.ExecContext(ctx, "DELETE FROM issue_inventory"); err != nil {
			return fmt.Errorf("remove private kind inventory: %w", err)
		}
	}
	// Rebuild the exported file so deleted payloads cannot remain in free pages.
	if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("compact sanitized export: %w", err)
	}
	return nil
}

// PruneResponseCache removes responses for disabled or obsolete collection resources.
func (w *Writer) PruneResponseCache(ctx context.Context, fields, projects bool) error {
	if _, err := w.tx.ExecContext(ctx, `DELETE FROM responses WHERE
 (?=0 AND url LIKE '%/issue-fields%') OR
 (?=0 AND url LIKE '%/projectsV2%') OR
 url LIKE '%/projectsV2/%/items%' OR url LIKE '%/projectsV2/%/fields%'`, fields, projects); err != nil {
		return fmt.Errorf("prune excluded response cache: %w", err)
	}
	return nil
}
