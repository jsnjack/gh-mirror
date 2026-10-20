package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

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
