package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gh-mirror/internal/config"
)

// ReadOptions controls a ticket projection and whether comments are included.
type ReadOptions struct {
	Repo        string `json:"repo"`
	Number      int    `json:"number"`
	View        string `json:"view,omitempty"`
	Comments    string `json:"comments,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
	CommentKind string `json:"comment_kind,omitempty"`
}

// CommentOptions selects one ticket's discussion or inline review comments.
type CommentOptions struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Kind   string `json:"kind,omitempty"`
	View   string `json:"view,omitempty"`
	Order  string `json:"order,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// Comment contains source metadata, a bounded preview and optional upstream JSON.
type Comment struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	URL       string          `json:"url"`
	Body      string          `json:"body"`
	Truncated bool            `json:"truncated"`
	Author    string          `json:"author,omitempty"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	Path      string          `json:"path,omitempty"`
	Line      int             `json:"line,omitempty"`
	Payload   json.RawMessage `json:"comment,omitempty"`
}

// CommentResult provides a generation-bound page and resource coverage warnings.
type CommentResult struct {
	Comments   []Comment `json:"comments"`
	NextCursor string    `json:"next_cursor"`
	HasMore    bool      `json:"has_more"`
	Warnings   []Warning `json:"warnings"`
	Status     Status    `json:"status"`
}

// TicketID identifies a ticket in a batch read.
type TicketID struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// BatchOptions selects up to 100 ticket identities and a projection without comments.
type BatchOptions struct {
	Tickets []TicketID `json:"tickets"`
	View    string     `json:"view,omitempty"`
}

// BatchResult includes found records and explicit missing identities from one generation.
type BatchResult struct {
	Issues  []Ticket   `json:"issues"`
	Missing []TicketID `json:"missing"`
	Status  Status     `json:"status"`
}

// CatalogOptions selects a bounded catalog page.
type CatalogOptions struct {
	Kind   string `json:"kind"`
	Scope  string `json:"scope"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

func validTicket(repo string, number int) error {
	if !config.ValidRepository(repo) || number < 1 {
		return invalid("valid repository and positive ticket number required")
	}
	return nil
}
func readTicket(ctx context.Context, q querier, repo string, number int, view string) (Ticket, error) {
	t := Ticket{Repo: repo, Number: number}
	var raw, fields, extra json.RawMessage
	err := q.QueryRowContext(ctx, "SELECT kind,payload,fields,extra FROM issues WHERE repo=? AND number=?", repo, number).Scan(&t.Kind, (*[]byte)(&raw), (*[]byte)(&fields), (*[]byte)(&extra))
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("ticket %s#%d: %w", repo, number, ErrNotFound)
	}
	if err != nil {
		return t, fmt.Errorf("read ticket: %w", err)
	}
	t.Summary, err = summarize(raw, extra)
	if err != nil {
		return t, err
	}
	if view == "full" {
		t.Payload, t.Fields, t.Extra = raw, fields, extra
	}
	return t, nil
}

// Read returns a ticket with optional none, page or legacy all comments.
func (s *Store) Read(ctx context.Context, o ReadOptions) (Issue, error) {
	out := Issue{Repo: o.Repo, Number: o.Number, Comments: []json.RawMessage{}}
	if err := validTicket(o.Repo, o.Number); err != nil {
		return out, err
	}
	if o.View == "" {
		o.View = "full"
	}
	if o.View != "full" && o.View != "summary" {
		return out, invalid("view must be summary or full")
	}
	if o.Comments == "" {
		if o.View == "summary" {
			o.Comments = "none"
		} else {
			o.Comments = "all"
		}
	}
	if o.Comments != "all" && o.Comments != "none" && o.Comments != "page" {
		return out, invalid("comments must be all, none or page")
	}
	if o.Comments != "page" && (o.Cursor != "" || o.Limit != 0 || o.CommentKind != "") {
		return out, invalid("comment paging options require comments=page")
	}
	err := s.view(ctx, func(tx *sql.Tx) error {
		t, err := readTicket(ctx, tx, o.Repo, o.Number, o.View)
		if err != nil {
			return err
		}
		out.Kind, out.Payload, out.Fields, out.Extra, out.Summary = t.Kind, t.Payload, t.Fields, t.Extra, t.Summary
		if o.Comments == "all" {
			legacy, err := get(ctx, tx, o.Repo, o.Number)
			if err != nil {
				return err
			}
			out.Comments = legacy.Comments
			out.Status = legacy.Status
			return nil
		}
		out.Status, err = status(ctx, tx)
		if err != nil {
			return err
		}
		if o.Comments == "page" {
			page, err := comments(ctx, tx, CommentOptions{Repo: o.Repo, Number: o.Number, Kind: o.CommentKind, Limit: o.Limit, Cursor: o.Cursor, View: o.View})
			if err != nil {
				return err
			}
			out.CommentPage = &page
		}
		return nil
	})
	return out, err
}

func comments(ctx context.Context, q querier, o CommentOptions) (CommentResult, error) {
	out := CommentResult{Comments: []Comment{}, Warnings: []Warning{}}
	if err := validTicket(o.Repo, o.Number); err != nil {
		return out, err
	}
	if o.Kind == "" {
		o.Kind = "all"
	}
	if o.Kind != "all" && o.Kind != "discussion" && o.Kind != "review" {
		return out, invalid("comment kind must be all, discussion or review")
	}
	if o.View == "" {
		o.View = "summary"
	}
	if o.View != "summary" && o.View != "full" {
		return out, invalid("view must be summary or full")
	}
	if o.Order == "" {
		o.Order = "asc"
	}
	if o.Order != "asc" && o.Order != "desc" {
		return out, invalid("order must be asc or desc")
	}
	limit, err := pageLimit(o.Limit)
	if err != nil {
		return out, err
	}
	var exists int
	if err := q.QueryRowContext(ctx, "SELECT 1 FROM issues WHERE repo=? AND number=?", o.Repo, o.Number).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("comment parent: %w", ErrNotFound)
	} else if err != nil {
		return out, fmt.Errorf("read comment parent: %w", err)
	}
	fingerprint := o
	fingerprint.Cursor = ""
	fingerprint.Limit = 0
	sig, err := signature(fingerprint)
	if err != nil {
		return out, err
	}
	out.Status, err = status(ctx, q)
	if err != nil {
		return out, err
	}
	cursor, err := decodeCursor(o.Cursor, "comments", sig, out.Status.Generation)
	if err != nil {
		return out, err
	}
	if o.Cursor != "" && cursor.ID == "" {
		return out, invalid("cursor has no comment identity")
	}
	where := "repo=? AND number=?"
	args := []any{o.Repo, o.Number}
	if o.Kind == "discussion" {
		where += " AND id NOT LIKE 'review:%'"
	}
	if o.Kind == "review" {
		where += " AND id LIKE 'review:%'"
	}
	op := ">"
	if o.Order == "desc" {
		op = "<"
	}
	if cursor.ID != "" {
		where += " AND (COALESCE(json_extract(payload,'$.created_at'),'')" + op + "? OR (COALESCE(json_extract(payload,'$.created_at'),'')=? AND id" + op + "?))"
		args = append(args, cursor.Key, cursor.Key, cursor.ID)
	}
	args = append(args, limit+1)
	rows, err := q.QueryContext(ctx, "SELECT id,payload FROM comments WHERE "+where+" ORDER BY COALESCE(json_extract(payload,'$.created_at'),'') "+o.Order+",id "+o.Order+" LIMIT ?", args...)
	if err != nil {
		return out, fmt.Errorf("list comments: %w", err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		var raw json.RawMessage
		if err := rows.Scan(&id, (*[]byte)(&raw)); err != nil {
			return out, finishRows(rows, err)
		}
		var obj struct {
			ID           json.Number             `json:"id"`
			Body         string                  `json:"body"`
			URL          string                  `json:"html_url"`
			CreatedAt    string                  `json:"created_at"`
			UpdatedAt    string                  `json:"updated_at"`
			User         *struct{ Login string } `json:"user"`
			Path         string                  `json:"path"`
			Line         int                     `json:"line"`
			OriginalLine int                     `json:"original_line"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return out, finishRows(rows, err)
		}
		c := Comment{ID: string(obj.ID), Kind: "discussion", URL: obj.URL, Body: obj.Body, CreatedAt: obj.CreatedAt, UpdatedAt: obj.UpdatedAt, Path: obj.Path, Line: obj.Line}
		if c.Line == 0 {
			c.Line = obj.OriginalLine
		}
		if strings.HasPrefix(id, "review:") {
			c.Kind = "review"
		}
		if obj.User != nil {
			c.Author = obj.User.Login
		}
		if o.View == "summary" {
			r := []rune(c.Body)
			if len(r) > 1024 {
				c.Body = string(r[:1024])
				c.Truncated = true
			}
		} else {
			c.Payload = raw
		}
		out.Comments = append(out.Comments, c)
		ids = append(ids, id)
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return out, err
	}
	out.HasMore = len(out.Comments) > limit
	if out.HasMore {
		out.Comments = out.Comments[:limit]
		last := out.Comments[limit-1]
		out.NextCursor, err = encodeCursor(pageCursor{Generation: out.Status.Generation, Signature: sig, Kind: "comments", ID: ids[limit-1], Key: last.CreatedAt})
		if err != nil {
			return out, err
		}
	}
	in := []string{"comments", "reviews"}
	if o.Kind == "discussion" {
		in = []string{"comments"}
	}
	if o.Kind == "review" {
		in = []string{"reviews"}
	}
	out.Warnings = queryWarnings(out.Status, filters{Repo: o.Repo}, PageOptions{}, in)
	return out, nil
}

// Comments returns one bounded local comment page without fetching upstream.
func (s *Store) Comments(ctx context.Context, o CommentOptions) (CommentResult, error) {
	var out CommentResult
	err := s.view(ctx, func(tx *sql.Tx) error { var err error; out, err = comments(ctx, tx, o); return err })
	return out, err
}

// Batch reads up to 100 tickets from one generation, omitting comments and reporting missing records.
func (s *Store) Batch(ctx context.Context, o BatchOptions) (BatchResult, error) {
	out := BatchResult{Issues: []Ticket{}, Missing: []TicketID{}}
	if len(o.Tickets) < 1 || len(o.Tickets) > 100 {
		return out, invalid("batch requires 1–100 tickets")
	}
	if o.View == "" {
		o.View = "summary"
	}
	if o.View != "summary" && o.View != "full" {
		return out, invalid("view must be summary or full")
	}
	for _, id := range o.Tickets {
		if err := validTicket(id.Repo, id.Number); err != nil {
			return out, err
		}
	}
	err := s.view(ctx, func(tx *sql.Tx) error {
		seen := map[TicketID]bool{}
		for _, id := range o.Tickets {
			if seen[id] {
				continue
			}
			seen[id] = true
			t, err := readTicket(ctx, tx, id.Repo, id.Number, o.View)
			if errors.Is(err, ErrNotFound) {
				out.Missing = append(out.Missing, id)
				continue
			}
			if err != nil {
				return err
			}
			out.Issues = append(out.Issues, t)
		}
		var err error
		out.Status, err = status(ctx, tx)
		return err
	})
	return out, err
}

// CatalogPage enumerates a catalog by stable identity with bounded results.
func (s *Store) CatalogPage(ctx context.Context, o CatalogOptions) (CatalogResult, error) {
	out := CatalogResult{Items: []json.RawMessage{}, Warnings: []Warning{}}
	if o.Kind == "" || o.Scope == "" {
		return out, invalid("catalog kind and scope are required")
	}
	switch o.Kind {
	case "labels", "milestones", "issue_types", "issue_fields", "projects":
	default:
		return out, invalid("unknown catalog kind")
	}
	limit, err := pageLimit(o.Limit)
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
		cursor, err := decodeCursor(o.Cursor, "catalog", sig, out.Status.Generation)
		if err != nil {
			return err
		}
		if o.Cursor != "" && cursor.ID == "" {
			return invalid("cursor has no catalog identity")
		}
		rows, err := tx.QueryContext(ctx, "SELECT id,payload FROM catalog WHERE kind=? AND scope=? AND (?='' OR id>?) ORDER BY id LIMIT ?", o.Kind, o.Scope, cursor.ID, cursor.ID, limit+1)
		if err != nil {
			return fmt.Errorf("read catalog page: %w", err)
		}
		ids := []string{}
		for rows.Next() {
			var id string
			var raw json.RawMessage
			if err := rows.Scan(&id, (*[]byte)(&raw)); err != nil {
				return finishRows(rows, err)
			}
			ids = append(ids, id)
			out.Items = append(out.Items, raw)
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return err
		}
		out.HasMore = len(out.Items) > limit
		if out.HasMore {
			out.Items = out.Items[:limit]
			out.NextCursor, err = encodeCursor(pageCursor{Generation: out.Status.Generation, Signature: sig, Kind: "catalog", ID: ids[limit-1]})
			if err != nil {
				return err
			}
		}
		resource := o.Kind
		if resource == "issue_fields" {
			resource = "fields"
		}
		for repo, raw := range out.Status.RepositoryOptions {
			applies := repo == o.Scope
			if resource == "projects" || resource == "issue_types" || resource == "fields" {
				applies = strings.HasPrefix(repo, o.Scope+"/")
			}
			if !applies {
				continue
			}
			var options map[string]bool
			if err := json.Unmarshal(raw, &options); err != nil {
				return fmt.Errorf("decode catalog coverage: %w", err)
			}
			if !options[resource] {
				out.Warnings = append(out.Warnings, Warning{"resource_disabled", repo, resource, "Requested catalog is disabled for this repository."})
			}
		}
		return nil
	})
	return out, err
}
