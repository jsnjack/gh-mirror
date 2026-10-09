package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const labelIndexVersion = "1"

func (w *Writer) indexLabels(ctx context.Context, repo string, number int, object map[string]json.RawMessage) error {
	var labels []struct{ Name string }
	if raw := object["labels"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &labels); err != nil {
			return fmt.Errorf("decode issue labels: %w", err)
		}
	}
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		if label.Name == "" {
			return fmt.Errorf("issue label has no name")
		}
		names = append(names, label.Name)
	}
	id := "labels:" + repo + ":" + strconv.Itoa(number)
	if len(names) == 0 {
		if _, err := w.tx.ExecContext(ctx, "DELETE FROM documents WHERE id=?", id); err != nil {
			return fmt.Errorf("remove issue label index: %w", err)
		}
		return nil
	}
	return w.document(ctx, id, repo, number, Text(object, "html_url"), strings.Join(names, "\n"), "")
}

// EnsureLabelIndex backfills label search documents from stored payloads without upstream requests.
func (w *Writer) EnsureLabelIndex(ctx context.Context) error {
	var version string
	err := w.tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='label_index_version'").Scan(&version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read label index version: %w", err)
	}
	if version == labelIndexVersion {
		return nil
	}
	rows, err := w.tx.QueryContext(ctx, "SELECT repo,number,payload FROM issues")
	if err != nil {
		return fmt.Errorf("read issues for label indexing: %w", err)
	}
	for rows.Next() {
		var repo string
		var number int
		var raw json.RawMessage
		if err := rows.Scan(&repo, &number, (*[]byte)(&raw)); err != nil {
			return finishRows(rows, fmt.Errorf("read label index source: %w", err))
		}
		object, err := Object(raw)
		if err != nil {
			return finishRows(rows, fmt.Errorf("decode label index source: %w", err))
		}
		if err := w.indexLabels(ctx, repo, number, object); err != nil {
			return finishRows(rows, fmt.Errorf("index labels for %s#%d: %w", repo, number, err))
		}
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return fmt.Errorf("finish label index sources: %w", err)
	}
	return w.SetMetadata(ctx, "label_index_version", labelIndexVersion)
}
