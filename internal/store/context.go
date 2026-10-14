package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"gh-mirror/internal/representation"
)

// ContextOptions selects a bounded, source-preserving ticket investigation view.
type ContextOptions struct {
	IncludeExcluded bool   `json:"include_excluded,omitempty"`
	Repo            string `json:"repo"`
	Number          int    `json:"number"`
	Purpose         string `json:"purpose,omitempty"`
	Query           string `json:"query,omitempty"`
	CommentLimit    int    `json:"comment_limit,omitempty"`
	RelatedLimit    int    `json:"related_limit,omitempty"`
	MaxCharacters   int    `json:"max_characters,omitempty"`
}

// SourceText is an exact original prefix with byte offsets and explicit omissions.
type SourceText struct {
	Text          string `json:"text"`
	Source        string `json:"source"`
	Start         int    `json:"start"`
	End           int    `json:"end"`
	OriginalBytes int    `json:"original_bytes"`
	Truncated     bool   `json:"truncated"`
}

// RelatedTicket associates a locally available summary with a source reference.
type RelatedTicket struct {
	Reference   representation.Reference `json:"reference"`
	Summary     *TicketSummary           `json:"summary,omitempty"`
	Description *SourceText              `json:"description,omitempty"`
	Missing     bool                     `json:"missing"`
}

// ContextCoverage explains bounded selection and which source families were examined.
type ContextCoverage struct {
	CommentsExcludedByPolicy int    `json:"comments_excluded_by_policy"`
	SectionsOmitted          int    `json:"sections_omitted"`
	LabelDefinitionsTotal    int    `json:"label_definitions_total"`
	LabelDefinitionsOmitted  int    `json:"label_definitions_omitted"`
	LabelRulesOmitted        bool   `json:"label_rules_omitted"`
	CommentsTotal            int    `json:"comments_total"`
	CommentsIncluded         int    `json:"comments_included"`
	CommentsOmitted          int    `json:"comments_omitted"`
	RelatedTotal             int    `json:"related_total"`
	RelatedOmitted           int    `json:"related_omitted"`
	ReferencesOmitted        int    `json:"references_omitted"`
	ReferenceSources         string `json:"reference_sources"`
	Selection                string `json:"selection"`
	TextCharacters           int    `json:"text_characters"`
	CharacterBudget          int    `json:"character_budget"`
}

// ContextResult combines classification attributes, original prose and discussion evidence.
type ContextResult struct {
	LabelRules       []representation.LabelRule `json:"label_rules"`
	Ticket           TicketID                   `json:"ticket"`
	Kind             string                     `json:"kind"`
	Purpose          string                     `json:"purpose"`
	Summary          *TicketSummary             `json:"summary"`
	Description      SourceText                 `json:"description"`
	Sections         []representation.Section   `json:"sections"`
	MissingSections  []string                   `json:"missing_sections"`
	LabelDefinitions []json.RawMessage          `json:"label_definitions"`
	Fields           json.RawMessage            `json:"fields,omitempty"`
	Comments         []Comment                  `json:"comments"`
	Relationships    []representation.Reference `json:"relationships"`
	Related          []RelatedTicket            `json:"related"`
	Coverage         ContextCoverage            `json:"coverage"`
	Warnings         []Warning                  `json:"warnings"`
	Status           Status                     `json:"status"`
}

// Context reads one consistent local generation without fetching or requiring embeddings.
func (s *Store) Context(ctx context.Context, o ContextOptions) (ContextResult, error) {
	var out ContextResult
	if err := validTicket(o.Repo, o.Number); err != nil {
		return out, err
	}
	if o.Purpose == "" {
		o.Purpose = "evaluate"
	}
	if !slices.Contains([]string{"evaluate", "classification", "duplicates", "proofread"}, o.Purpose) {
		return out, invalid("purpose must be evaluate, classification, duplicates or proofread")
	}
	if o.CommentLimit == 0 {
		o.CommentLimit = 10
	}
	if o.RelatedLimit == 0 {
		o.RelatedLimit = 5
	}
	if o.MaxCharacters == 0 {
		o.MaxCharacters = 12000
	}
	if o.CommentLimit < 0 || o.CommentLimit > 50 || o.RelatedLimit < 0 || o.RelatedLimit > 20 || o.MaxCharacters < 1000 || o.MaxCharacters > 100000 || len(o.Query) > 16384 {
		return out, invalid("context limits: comments 1–50, related 1–20, characters 1000–100000, query 16384 bytes")
	}
	err := s.view(ctx, func(tx *sql.Tx) error { var err error; out, err = ticketContext(ctx, tx, o); return err })
	return out, err
}

func sourceText(text, source string, remaining *int, limit int) SourceText {
	runes := []rune(text)
	count := min(len(runes), *remaining, limit)
	selected := string(runes[:count])
	*remaining -= count
	return SourceText{Text: selected, Source: source, End: len(selected), OriginalBytes: len(text), Truncated: count < len(runes)}
}

func ticketContext(ctx context.Context, q querier, o ContextOptions) (ContextResult, error) {
	out := ContextResult{Ticket: TicketID{o.Repo, o.Number}, Purpose: o.Purpose, Comments: []Comment{}, Related: []RelatedTicket{}, Relationships: []representation.Reference{}, LabelDefinitions: []json.RawMessage{}, MissingSections: []string{}, Warnings: []Warning{}}
	t, err := readTicket(ctx, q, o.Repo, o.Number, "full")
	if err != nil {
		return out, err
	}
	out.Summary, out.Kind = t.Summary, t.Kind
	rules, err := labelRules(ctx, q)
	if err != nil {
		return out, err
	}
	out.LabelRules = []representation.LabelRule{}
	var payload struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(t.Payload, &payload); err != nil {
		return out, fmt.Errorf("decode context description: %w", err)
	}
	remaining := o.MaxCharacters
	bodyBudget := remaining
	if o.Purpose != "proofread" {
		bodyBudget = max(1000, remaining*2/3)
	}
	out.Description = sourceText(payload.Body, t.Summary.URL, &remaining, bodyBudget)
	out.Sections = representation.Sections(payload.Body)
	for _, kind := range []string{"description", "reproduction", "expected", "actual", "environment", "acceptance"} {
		found := false
		for _, section := range out.Sections {
			if section.Kind == kind && !section.Empty {
				found = true
			}
		}
		if !found {
			out.MissingSections = append(out.MissingSections, kind)
		}
	}
	if len(out.Sections) > 100 {
		out.Coverage.SectionsOmitted = len(out.Sections) - 100
		out.Sections = out.Sections[:100]
	}
	// Missing sections are observations; acceptance criteria are not required for every ticket.
	native, err := nativeRelationships(t.Extra, t.Summary.URL)
	if err != nil {
		return out, err
	}
	out.Relationships = append(out.Relationships, native...)
	out.Relationships = append(out.Relationships, representation.References(o.Repo, payload.Body, t.Summary.URL)...)
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM comments WHERE repo=? AND number=?", o.Repo, o.Number).Scan(&out.Coverage.CommentsTotal); err != nil {
		return out, fmt.Errorf("count context comments: %w", err)
	}
	out.Coverage.CommentsExcludedByPolicy, err = excludedCommentCount(ctx, q, o.Repo, o.Number, "all")
	if err != nil {
		return out, err
	}
	// Policy exclusions are reported separately, so the total counts only selectable comments.
	if !o.IncludeExcluded {
		out.Coverage.CommentsTotal -= out.Coverage.CommentsExcludedByPolicy
	}
	if !o.IncludeExcluded && out.Coverage.CommentsExcludedByPolicy > 0 {
		out.Warnings = append(out.Warnings, commentFilterWarning(o.Repo, out.Coverage.CommentsExcludedByPolicy))
	}
	comments, err := contextComments(ctx, q, o)
	if err != nil {
		return out, err
	}
	for _, c := range comments {
		if remaining == 0 {
			break
		}
		out.Relationships = append(out.Relationships, representation.References(o.Repo, c.Body, c.URL)...)
		text := sourceText(c.Body, c.URL, &remaining, 2048)
		c.Body, c.Truncated, c.Payload = text.Text, text.Truncated, nil
		out.Comments = append(out.Comments, c)
	}
	out.Coverage.ReferenceSources = "original description, native relationships and selected comments; unselected comments are not inspected"
	if len(out.Relationships) > 100 {
		out.Coverage.ReferencesOmitted = len(out.Relationships) - 100
		out.Relationships = out.Relationships[:100]
	}
	seen := map[TicketID]bool{out.Ticket: true}
	for _, ref := range out.Relationships {
		id := TicketID{ref.Repo, ref.Number}
		if seen[id] {
			continue
		}
		seen[id] = true
		out.Coverage.RelatedTotal++
		if len(out.Related) >= o.RelatedLimit {
			continue
		}
		related := RelatedTicket{Reference: ref}
		ticket, err := readTicket(ctx, q, ref.Repo, ref.Number, "full")
		if errors.Is(err, ErrNotFound) {
			related.Missing = true
		} else if err != nil {
			return out, err
		} else {
			related.Summary = ticket.Summary
			var raw struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(ticket.Payload, &raw); err != nil {
				return out, fmt.Errorf("decode related description: %w", err)
			}
			text := sourceText(raw.Body, ticket.Summary.URL, &remaining, 512)
			related.Description = &text
		}
		out.Related = append(out.Related, related)
	}
	labelsJSON, err := json.Marshal(t.Summary.Labels)
	if err != nil {
		return out, fmt.Errorf("encode context labels: %w", err)
	}
	allLabels := o.Purpose == "classification"
	rulesJSON, err := json.Marshal(rules[o.Repo])
	if err != nil {
		return out, fmt.Errorf("measure label rules: %w", err)
	}
	if len(rules[o.Repo]) > 0 {
		if len(rules[o.Repo]) <= 100 && utf8.RuneCount(rulesJSON) <= remaining {
			out.LabelRules = append(out.LabelRules, rules[o.Repo]...)
			remaining -= utf8.RuneCount(rulesJSON)
		} else {
			out.Coverage.LabelRulesOmitted = true
			out.Warnings = append(out.Warnings, Warning{Code: "context_label_rules_omitted", Repo: o.Repo, Resource: "labels", Message: "Configured label rules exceed the context budget; inspect the repository label_rules configuration."})
		}
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM catalog WHERE kind='labels' AND scope=? AND (? OR json_extract(payload,'$.name') IN (SELECT value FROM json_each(?)))`, o.Repo, allLabels, string(labelsJSON)).Scan(&out.Coverage.LabelDefinitionsTotal); err != nil {
		return out, fmt.Errorf("count classification labels: %w", err)
	}
	rows, err := q.QueryContext(ctx, `SELECT json_object('name',json_extract(payload,'$.name'),'description',json_extract(payload,'$.description')) FROM catalog WHERE kind='labels' AND scope=? AND (? OR json_extract(payload,'$.name') IN (SELECT value FROM json_each(?))) ORDER BY CASE WHEN json_extract(payload,'$.name') IN (SELECT value FROM json_each(?)) THEN 0 ELSE 1 END,id LIMIT 100`, o.Repo, allLabels, string(labelsJSON), string(labelsJSON))
	if err != nil {
		return out, fmt.Errorf("read context label definitions: %w", err)
	}
	labelBudget := min(remaining, o.MaxCharacters/3)
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan((*[]byte)(&raw)); err != nil {
			return out, finishRows(rows, err)
		}
		if utf8.RuneCount(raw) <= labelBudget {
			out.LabelDefinitions = append(out.LabelDefinitions, raw)
			remaining -= utf8.RuneCount(raw)
			labelBudget -= utf8.RuneCount(raw)
		}
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return out, err
	}
	out.Coverage.LabelDefinitionsOmitted = out.Coverage.LabelDefinitionsTotal - len(out.LabelDefinitions)
	if out.Coverage.LabelDefinitionsOmitted > 0 {
		out.Warnings = append(out.Warnings, Warning{Code: "context_label_definition_omitted", Repo: o.Repo, Resource: "labels", Message: fmt.Sprintf("%d label definitions omitted by context limits; use gh-mirror catalog --kind labels --scope %s to inspect the full catalog.", out.Coverage.LabelDefinitionsOmitted, o.Repo)})
	}
	// Custom fields are opaque source data; omit the complete resource when it exceeds the remaining budget.
	if utf8.RuneCount(t.Fields) <= remaining {
		out.Fields = t.Fields
		remaining -= utf8.RuneCount(t.Fields)
	} else {
		out.Warnings = append(out.Warnings, Warning{Code: "context_fields_omitted", Repo: o.Repo, Resource: "fields", Message: "Custom fields exceed the remaining character budget; read the full ticket to inspect them."})
	}
	out.Coverage.CommentsIncluded = len(out.Comments)
	out.Coverage.CommentsOmitted = out.Coverage.CommentsTotal - len(out.Comments)
	out.Coverage.RelatedOmitted = out.Coverage.RelatedTotal - len(out.Related)
	out.Coverage.TextCharacters = o.MaxCharacters - remaining
	out.Coverage.CharacterBudget = o.MaxCharacters
	out.Coverage.Selection = "query-matching comments followed by recent comments; bounded one-hop relationships"
	out.Status, err = status(ctx, q)
	if err != nil {
		return out, err
	}
	resources := []string{"comments"}
	if out.Kind == "pull_request" {
		resources = append(resources, "reviews")
	}
	out.Warnings = append(out.Warnings, queryWarnings(out.Status, filters{Repo: o.Repo, Kind: out.Kind}, PageOptions{}, resources)...)
	return out, nil
}

func nativeRelationships(raw json.RawMessage, source string) ([]representation.Reference, error) {
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(raw, &extra); err != nil {
		return nil, fmt.Errorf("decode native relationships: %w", err)
	}
	out := []representation.Reference{}
	for _, pair := range []struct{ key, kind string }{{"parent", "parent"}, {"subIssues", "child"}, {"blockedBy", "blocked_by"}, {"blocking", "blocks"}} {
		var list []json.RawMessage
		if pair.key == "parent" {
			if len(extra[pair.key]) > 0 {
				list = []json.RawMessage{extra[pair.key]}
			}
		} else if len(extra[pair.key]) > 0 {
			if err := json.Unmarshal(extra[pair.key], &list); err != nil {
				return nil, fmt.Errorf("decode native %s: %w", pair.key, err)
			}
		}
		for _, value := range list {
			var node struct {
				Number     int
				URL        string
				Repository struct{ NameWithOwner string }
			}
			if err := json.Unmarshal(value, &node); err != nil {
				return nil, fmt.Errorf("decode native %s node: %w", pair.key, err)
			}
			if node.Number < 1 {
				continue
			}
			if node.Repository.NameWithOwner != "" {
				out = append(out, representation.Reference{Repo: node.Repository.NameWithOwner, Number: node.Number, Kind: pair.kind, Source: source})
			}
		}
	}
	return out, nil
}

func contextComments(ctx context.Context, q querier, o ContextOptions) ([]Comment, error) {
	selected := []Comment{}
	terms := queryTerms(o.Query, true)
	if len(terms) > 32 {
		terms = terms[:32]
	}
	ids := []string{}
	if len(terms) > 0 {
		var rows *sql.Rows
		var err error
		if o.IncludeExcluded {
			conditions := make([]string, len(terms))
			args := []any{o.Repo, o.Number}
			for i, term := range terms {
				conditions[i] = "instr(lower(body),lower(?))>0"
				args = append(args, term)
			}
			args = append(args, max(1, o.CommentLimit/2))
			rows, err = q.QueryContext(ctx, `SELECT 'comment:'||id FROM comments WHERE repo=? AND number=? AND (`+strings.Join(conditions, " OR ")+`) ORDER BY COALESCE(json_extract(payload,'$.created_at'),'') DESC,id DESC LIMIT ?`, args...)
		} else {
			rows, err = q.QueryContext(ctx, `SELECT d.id FROM documents_fts JOIN documents d ON d.rowid=documents_fts.rowid WHERE documents_fts MATCH ? AND d.repo=? AND d.number=? AND d.id LIKE 'comment:%' ORDER BY bm25(documents_fts,5,1),d.id LIMIT ?`, strings.Join(quoteTerms(terms, false), " OR "), o.Repo, o.Number, max(1, o.CommentLimit/2))
		}
		if err != nil {
			return nil, fmt.Errorf("select relevant context comments: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, finishRows(rows, err)
			}
			ids = append(ids, strings.TrimPrefix(id, "comment:"))
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return nil, err
		}
	}
	where := "repo=? AND number=?"
	if !o.IncludeExcluded {
		where += " AND " + visibleComment("comments")
	}
	rows, err := q.QueryContext(ctx, `SELECT id FROM comments WHERE `+where+` ORDER BY COALESCE(json_extract(payload,'$.created_at'),'') DESC,id DESC LIMIT ?`, o.Repo, o.Number, o.CommentLimit)
	if err != nil {
		return nil, fmt.Errorf("select recent context comments: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, finishRows(rows, err)
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return nil, err
	}
	for _, id := range ids[:min(len(ids), o.CommentLimit)] {
		var c Comment
		var body string
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(CAST(json_extract(payload,'$.id') AS TEXT),''),body,url,COALESCE(json_extract(payload,'$.user.login'),''),COALESCE(json_extract(payload,'$.created_at'),''),updated_at,COALESCE(json_extract(payload,'$.path'),''),COALESCE(json_extract(payload,'$.line'),json_extract(payload,'$.original_line'),0) FROM comments WHERE id=?`, id).Scan(&c.ID, &body, &c.URL, &c.Author, &c.CreatedAt, &c.UpdatedAt, &c.Path, &c.Line); err != nil {
			return nil, fmt.Errorf("read selected context comment: %w", err)
		}
		c.Body, c.Kind = body, "discussion"
		if strings.HasPrefix(id, "review:") {
			c.Kind = "review"
		}
		selected = append(selected, c)
	}
	return selected, nil
}
