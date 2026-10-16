package store

import (
	"context"
	"fmt"
)

// ChangeCount tallies records a collection transaction inserted, changed or removed.
type ChangeCount struct {
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Removed int `json:"removed"`
}

// Changes tallies one repository's ticket and comment writes in a collection transaction.
type Changes struct {
	Issues       ChangeCount `json:"issues"`
	PullRequests ChangeCount `json:"pull_requests"`
	Comments     ChangeCount `json:"comments"`
}

func (c *Changes) ticket(kind string) *ChangeCount {
	if kind == "pull_request" {
		return &c.PullRequests
	}
	return &c.Issues
}

// Changes returns tallies for repositories this transaction changed so far.
func (w *Writer) Changes() map[string]Changes {
	out := make(map[string]Changes, len(w.changes))
	for repo, changes := range w.changes {
		out[repo] = *changes
	}
	return out
}

func (w *Writer) changed(repo string) *Changes {
	if w.changes == nil {
		w.changes = map[string]*Changes{}
	}
	if w.changes[repo] == nil {
		w.changes[repo] = &Changes{}
	}
	return w.changes[repo]
}

// countRemoval tallies tickets matched by where, and the comments that cascade with them,
// before the caller deletes those tickets.
func (w *Writer) countRemoval(ctx context.Context, where string, args ...any) error {
	rows, err := w.tx.QueryContext(ctx, "SELECT repo,kind,COUNT(*) FROM issues WHERE "+where+" GROUP BY repo,kind", args...)
	if err != nil {
		return fmt.Errorf("count removed tickets: %w", err)
	}
	for rows.Next() {
		var repo, kind string
		var n int
		if err := rows.Scan(&repo, &kind, &n); err != nil {
			return finishRows(rows, err)
		}
		w.changed(repo).ticket(kind).Removed += n
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return err
	}
	rows, err = w.tx.QueryContext(ctx, "SELECT repo,COUNT(*) FROM comments WHERE (repo,number) IN (SELECT repo,number FROM issues WHERE "+where+") GROUP BY repo", args...)
	if err != nil {
		return fmt.Errorf("count removed comments: %w", err)
	}
	for rows.Next() {
		var repo string
		var n int
		if err := rows.Scan(&repo, &n); err != nil {
			return finishRows(rows, err)
		}
		w.changed(repo).Comments.Removed += n
	}
	return finishRows(rows, rows.Err())
}

func (w *Writer) exists(ctx context.Context, query string, args ...any) (bool, error) {
	var found int
	err := w.tx.QueryRowContext(ctx, "SELECT EXISTS("+query+")", args...).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("check stored record: %w", err)
	}
	return found == 1, nil
}
