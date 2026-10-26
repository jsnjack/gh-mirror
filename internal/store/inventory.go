package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ObserveKind retains only the identity and kind needed to filter repository comment streams.
func (w *Writer) ObserveKind(ctx context.Context, repo string, number int, kind string) error {
	if _, err := w.tx.ExecContext(ctx, `INSERT INTO issue_inventory(repo,number,kind) VALUES(?,?,?) ON CONFLICT(repo,number) DO UPDATE SET kind=excluded.kind`, repo, number, kind); err != nil {
		return fmt.Errorf("record ticket kind: %w", err)
	}
	return nil
}

// KnownKind consults selected tickets and the minimal inventory before fetching a missing comment parent.
func (w *Writer) KnownKind(ctx context.Context, repo string, number int) (string, error) {
	var kind string
	err := w.tx.QueryRowContext(ctx, `SELECT kind FROM issues WHERE repo=? AND number=? UNION ALL SELECT kind FROM issue_inventory WHERE repo=? AND number=? LIMIT 1`, repo, number, repo, number).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read comment parent kind: %w", err)
	}
	return kind, nil
}

// ResetInventory clears minimal kind observations before a complete repository inventory.
func (w *Writer) ResetInventory(ctx context.Context, repo string) error {
	if _, err := w.tx.ExecContext(ctx, "DELETE FROM issue_inventory WHERE repo=?", repo); err != nil {
		return fmt.Errorf("reset kind inventory: %w", err)
	}
	return nil
}

// ClearExtras removes disabled GraphQL observations before a repository's next hydration.
func (w *Writer) ClearExtras(ctx context.Context, repo string) error {
	if _, err := w.tx.ExecContext(ctx, "UPDATE issues SET fields='[]',extra='{}' WHERE repo=?", repo); err != nil {
		return fmt.Errorf("clear excluded metadata: %w", err)
	}
	return nil
}

// DeleteCatalog removes an excluded catalog from a successful collection transaction.
func (w *Writer) DeleteCatalog(ctx context.Context, kind, scope string) error {
	if _, err := w.tx.ExecContext(ctx, "DELETE FROM catalog WHERE kind=? AND scope=?", kind, scope); err != nil {
		return fmt.Errorf("remove excluded catalog: %w", err)
	}
	return nil
}

// PruneListing removes cached pages of a complete listing's paths that it no longer
// requested: an older sort order, stale cursors or pages past a shrunken tail.
func (w *Writer) PruneListing(ctx context.Context, pages []string) error {
	keep := make(map[string]bool, len(pages))
	paths := map[string]bool{}
	for _, page := range pages {
		keep[page] = true
		path, _, _ := strings.Cut(page, "?")
		paths[path] = true
	}
	stale := []string{}
	for path := range paths {
		// '@' follows '?', so this range holds exactly the URLs of path with a query.
		rows, err := w.tx.QueryContext(ctx, "SELECT url FROM responses WHERE url>=? AND url<?", path+"?", path+"@")
		if err != nil {
			return fmt.Errorf("read cached listing pages: %w", err)
		}
		for rows.Next() {
			var url string
			if err := rows.Scan(&url); err != nil {
				return finishRows(rows, fmt.Errorf("scan cached listing page: %w", err))
			}
			if !keep[url] {
				stale = append(stale, url)
			}
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return err
		}
	}
	for _, url := range stale {
		if _, err := w.tx.ExecContext(ctx, "DELETE FROM responses WHERE url=?", url); err != nil {
			return fmt.Errorf("remove stale listing page: %w", err)
		}
	}
	return nil
}

// DeleteCachedPrefix prunes reusable responses for one excluded API resource.
func (w *Writer) DeleteCachedPrefix(ctx context.Context, prefix string) error {
	if _, err := w.tx.ExecContext(ctx, "DELETE FROM responses WHERE substr(url,1,length(?))=?", prefix, prefix); err != nil {
		return fmt.Errorf("remove excluded resource cache: %w", err)
	}
	return nil
}
