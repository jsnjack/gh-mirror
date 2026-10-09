package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// ListOptions selects tickets, projection, ordering and a generation-bound page.
type ListOptions struct {
	Repo    string `json:"repo,omitempty"`
	State   string `json:"state,omitempty"`
	Label   string `json:"label,omitempty"`
	Type    string `json:"type,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Project string `json:"project,omitempty"`
	QueryFilters
	PageOptions
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

func (o ListOptions) filters() filters {
	return filters{o.Repo, o.State, o.Label, o.Type, o.Kind, o.Project, o.QueryFilters}
}

// Ticket contains common metadata and optionally stored raw records; comments are separate.
type Ticket struct {
	Repo    string          `json:"repo"`
	Number  int             `json:"number"`
	Kind    string          `json:"kind"`
	Summary *TicketSummary  `json:"summary"`
	Payload json.RawMessage `json:"issue,omitempty"`
	Fields  json.RawMessage `json:"fields,omitempty"`
	Extra   json.RawMessage `json:"extra,omitempty"`
}

// ListResult returns a consistent page with optional counts and facets.
type ListResult struct {
	Issues     []Ticket     `json:"issues"`
	NextCursor string       `json:"next_cursor"`
	HasMore    bool         `json:"has_more"`
	Total      *int         `json:"total,omitempty"`
	Facets     *FacetResult `json:"facets,omitempty"`
	Warnings   []Warning    `json:"warnings"`
	Status     Status       `json:"status"`
}

// List enumerates matching tickets with deterministic ordering and generation-bound cursors.
func (s *Store) List(ctx context.Context, o ListOptions) (ListResult, error) {
	out := ListResult{Issues: []Ticket{}, Warnings: []Warning{}}
	limit, err := pageLimit(o.Limit)
	if err != nil {
		return out, err
	}
	o.Limit = limit
	if err := o.normalize(false); err != nil {
		return out, err
	}
	where, args, err := o.filters().sql()
	if err != nil {
		return out, err
	}
	fingerprint := o
	fingerprint.Limit = 0
	fingerprint.Cursor = ""
	sig, err := signature(fingerprint)
	if err != nil {
		return out, err
	}
	err = s.view(ctx, func(tx *sql.Tx) error {
		var err error
		out.Status, err = status(ctx, tx)
		if err != nil {
			return err
		}
		cursor, err := decodeCursor(o.Cursor, "list", sig, out.Status.Generation)
		if err != nil {
			return err
		}
		if o.Cursor != "" && (cursor.Repo == "" || cursor.Number < 1) {
			return invalid("cursor has no ticket identity")
		}
		cte := `WITH tickets AS (SELECT i.*,COALESCE(json_extract(i.payload,'$.created_at'),'') created_at FROM issues i WHERE ` + where + `)`
		continuation, moreArgs := continuation(o.PageOptions, cursor)
		pageArgs := append(append([]any{}, args...), moreArgs...)
		pageArgs = append(pageArgs, limit+1)
		rows, err := tx.QueryContext(ctx, cte+" SELECT repo,number,kind,payload,fields,extra,updated_at,created_at FROM tickets WHERE "+continuation+" ORDER BY "+orderSQL(o.PageOptions)+" LIMIT ?", pageArgs...)
		if err != nil {
			return fmt.Errorf("list tickets: %w", err)
		}
		keys := []string{}
		for rows.Next() {
			var t Ticket
			var raw, fields, extra json.RawMessage
			var updated, created string
			if err := rows.Scan(&t.Repo, &t.Number, &t.Kind, (*[]byte)(&raw), (*[]byte)(&fields), (*[]byte)(&extra), &updated, &created); err != nil {
				return finishRows(rows, err)
			}
			t.Summary, err = summarize(raw, extra)
			if err != nil {
				return finishRows(rows, err)
			}
			if o.View == "full" {
				t.Payload, t.Fields, t.Extra = raw, fields, extra
			}
			out.Issues = append(out.Issues, t)
			key := updated
			if o.Sort == "created" {
				key = created
			}
			keys = append(keys, key)
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return err
		}
		out.HasMore = len(out.Issues) > limit
		if out.HasMore {
			out.Issues = out.Issues[:limit]
			last := out.Issues[limit-1]
			out.NextCursor, err = encodeCursor(pageCursor{Generation: out.Status.Generation, Signature: sig, Kind: "list", Repo: last.Repo, Number: last.Number, Key: keys[limit-1]})
			if err != nil {
				return err
			}
		}
		if o.Count {
			var count int
			if err := tx.QueryRowContext(ctx, cte+" SELECT count(*) FROM tickets", args...).Scan(&count); err != nil {
				return fmt.Errorf("count listing: %w", err)
			}
			out.Total = &count
		}
		if o.Facets {
			out.Facets, err = facets(ctx, tx, cte, "SELECT repo,number FROM tickets", args)
			if err != nil {
				return err
			}
		}
		out.Warnings = queryWarnings(out.Status, o.filters(), o.PageOptions, nil)
		return nil
	})
	return out, err
}
