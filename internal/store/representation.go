package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"gh-mirror/internal/representation"
)

func labelRules(ctx context.Context, q querier) (map[string][]representation.LabelRule, error) {
	out := map[string][]representation.LabelRule{}
	var raw string
	err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='label_rules'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read label rules: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode label rules: %w", err)
	}
	return out, nil
}

func applyLabelRules(ctx context.Context, q querier, repo string, summary *TicketSummary) error {
	rules, err := labelRules(ctx, q)
	if err != nil {
		return err
	}
	summary.Attributes = representation.Attributes(summary.Labels, rules[repo])
	return nil
}
