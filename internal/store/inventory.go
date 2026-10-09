package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// DeleteCachedPrefix prunes reusable responses for one excluded API resource.
func (w *Writer) DeleteCachedPrefix(ctx context.Context, prefix string) error {
	if _, err := w.tx.ExecContext(ctx, "DELETE FROM responses WHERE substr(url,1,length(?))=?", prefix, prefix); err != nil {
		return fmt.Errorf("remove excluded resource cache: %w", err)
	}
	return nil
}
