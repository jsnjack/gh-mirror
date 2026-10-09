package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
)

// ProjectResult includes project metadata, definitions, and complete item records.
type ProjectResult struct {
	Project json.RawMessage   `json:"project"`
	Fields  []json.RawMessage `json:"fields"`
	Items   []json.RawMessage `json:"items"`
	Status  Status            `json:"status"`
}

// Project reads an owner project and all its catalog data in one transaction.
func (s *Store) Project(ctx context.Context, owner string, number int) (ProjectResult, error) {
	out := ProjectResult{Fields: []json.RawMessage{}, Items: []json.RawMessage{}}
	err := s.view(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, "SELECT payload FROM catalog WHERE kind='projects' AND scope=? AND json_extract(payload,'$.number')=?", owner, number).Scan((*[]byte)(&out.Project))
		if err == sql.ErrNoRows {
			return fmt.Errorf("project %s/%d: %w", owner, number, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("read project: %w", err)
		}
		rows, err := tx.QueryContext(ctx, "SELECT kind,payload FROM catalog WHERE scope=? AND kind IN ('project_fields','project_items') ORDER BY kind,id", owner+"/"+strconv.Itoa(number))
		if err != nil {
			return fmt.Errorf("read project data: %w", err)
		}
		for rows.Next() {
			var kind string
			var raw json.RawMessage
			if err := rows.Scan(&kind, (*[]byte)(&raw)); err != nil {
				return finishRows(rows, err)
			}
			if kind == "project_fields" {
				out.Fields = append(out.Fields, raw)
			} else {
				out.Items = append(out.Items, raw)
			}
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return err
		}
		out.Status, err = status(ctx, tx)
		return err
	})
	return out, err
}
