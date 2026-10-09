package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ListOptions selects a page of tickets without requiring a text query.
type ListOptions struct {
	Repo    string `json:"repo,omitempty"`
	State   string `json:"state,omitempty"`
	Label   string `json:"label,omitempty"`
	Type    string `json:"type,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Project string `json:"project,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
}

// Ticket contains stored issue payload and metadata; Get retrieves its comments.
type Ticket struct {
	Repo    string          `json:"repo"`
	Number  int             `json:"number"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"issue"`
	Fields  json.RawMessage `json:"fields"`
	Extra   json.RawMessage `json:"extra"`
}

// ListResult returns one ordered page, a continuation cursor, and its mirror generation.
type ListResult struct {
	Issues     []Ticket `json:"issues"`
	NextCursor string   `json:"next_cursor"`
	Status     Status   `json:"status"`
}

type issueCursor struct {
	Generation string `json:"generation"`
	Filters    string `json:"filters"`
	Repo       string `json:"repo"`
	Number     int    `json:"number"`
}

var projectPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[1-9][0-9]*$`)

const issueFilters = `(?='' OR i.repo=?) AND (?='' OR i.state=?)
 AND (?='' OR EXISTS(SELECT 1 FROM json_each(i.payload,'$.labels') l WHERE json_extract(l.value,'$.name')=?))
 AND (?='' OR json_extract(i.payload,'$.type.name')=?) AND (?='' OR i.kind=?)
 AND (?='' OR EXISTS(SELECT 1 FROM json_each(i.extra,'$.projectItems') p
 WHERE substr(json_extract(p.value,'$.project.url'),-length(?))=?
 OR substr(json_extract(p.value,'$.project.url'),-length(?))=?))`

func filterArguments(repo, state, label, issueType, kind, project string) ([]any, error) {
	if state != "" && state != "open" && state != "closed" {
		return nil, fmt.Errorf("state must be open or closed")
	}
	if kind != "" && kind != "issue" && kind != "pull_request" {
		return nil, fmt.Errorf("kind must be issue or pull_request")
	}
	org, user := "", ""
	if project != "" {
		if !projectPattern.MatchString(project) {
			return nil, fmt.Errorf("project must be owner/number")
		}
		parts := strings.Split(project, "/")
		number, err := strconv.Atoi(parts[1])
		if err != nil || number < 1 {
			return nil, fmt.Errorf("invalid project number")
		}
		suffix := parts[0] + "/projects/" + strconv.Itoa(number)
		org, user = "/orgs/"+suffix, "/users/"+suffix
	}
	return []any{repo, repo, state, state, label, label, issueType, issueType, kind, kind, project, org, org, user, user}, nil
}

// List enumerates tickets by repository and number using generation-bound keyset pagination.
func (s *Store) List(ctx context.Context, o ListOptions) (ListResult, error) {
	out := ListResult{Issues: []Ticket{}}
	if o.Limit == 0 {
		o.Limit = 30
	}
	if o.Limit < 1 || o.Limit > 100 {
		return out, fmt.Errorf("limit must be between 1 and 100")
	}
	args, err := filterArguments(o.Repo, o.State, o.Label, o.Type, o.Kind, o.Project)
	if err != nil {
		return out, err
	}
	filters, err := json.Marshal([]string{o.Repo, o.State, o.Label, o.Type, o.Kind, o.Project})
	if err != nil {
		return out, fmt.Errorf("encode page filters: %w", err)
	}
	signature := fmt.Sprintf("%x", sha256.Sum256(filters))
	var cursor issueCursor
	if o.Cursor != "" {
		if len(o.Cursor) > 2048 {
			return out, fmt.Errorf("cursor exceeds 2048 bytes")
		}
		raw, err := base64.RawURLEncoding.DecodeString(o.Cursor)
		if err != nil {
			return out, fmt.Errorf("decode cursor: %w", err)
		}
		if err := json.Unmarshal(raw, &cursor); err != nil {
			return out, fmt.Errorf("decode cursor identity: %w", err)
		}
		if cursor.Filters != signature || cursor.Repo == "" || cursor.Number < 1 {
			return out, fmt.Errorf("cursor does not match filters or ticket identity")
		}
	}
	err = s.view(ctx, func(tx *sql.Tx) error {
		var err error
		out.Status, err = status(ctx, tx)
		if err != nil {
			return fmt.Errorf("read listing generation: %w", err)
		}
		if o.Cursor != "" && cursor.Generation != out.Status.Generation {
			return fmt.Errorf("cursor belongs to a different mirror generation; restart listing")
		}
		args = append(args, cursor.Repo, cursor.Repo, cursor.Repo, cursor.Number, o.Limit+1)
		rows, err := tx.QueryContext(ctx, `SELECT i.repo,i.number,i.kind,i.payload,i.fields,i.extra FROM issues i WHERE `+issueFilters+`
   AND (?='' OR i.repo>? OR (i.repo=? AND i.number>?)) ORDER BY i.repo,i.number LIMIT ?`, args...)
		if err != nil {
			return fmt.Errorf("list local tickets: %w", err)
		}
		for rows.Next() {
			var ticket Ticket
			if err := rows.Scan(&ticket.Repo, &ticket.Number, &ticket.Kind, (*[]byte)(&ticket.Payload), (*[]byte)(&ticket.Fields), (*[]byte)(&ticket.Extra)); err != nil {
				return finishRows(rows, fmt.Errorf("read ticket page: %w", err))
			}
			out.Issues = append(out.Issues, ticket)
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return fmt.Errorf("finish ticket page: %w", err)
		}
		if len(out.Issues) > o.Limit {
			out.Issues = out.Issues[:o.Limit]
			last := out.Issues[len(out.Issues)-1]
			raw, err := json.Marshal(issueCursor{out.Status.Generation, signature, last.Repo, last.Number})
			if err != nil {
				return fmt.Errorf("encode continuation: %w", err)
			}
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		}
		return nil
	})
	return out, err
}
