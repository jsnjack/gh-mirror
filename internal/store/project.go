package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// ProjectResult includes a project catalog record and collection status.
type ProjectResult struct {
	Project json.RawMessage `json:"project"`
	Status  Status          `json:"status"`
}

// Project reads an owner project and collection status in one transaction.
func (s *Store) Project(ctx context.Context, owner string, number int) (ProjectResult, error) {
	var out ProjectResult
	err := s.view(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, "SELECT payload FROM catalog WHERE kind='projects' AND scope=? AND json_extract(payload,'$.number')=?", owner, number).Scan((*[]byte)(&out.Project))
		if err == sql.ErrNoRows {
			return fmt.Errorf("project %s/%d: %w", owner, number, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("read project: %w", err)
		}
		out.Status, err = status(ctx, tx)
		return err
	})
	return out, err
}
