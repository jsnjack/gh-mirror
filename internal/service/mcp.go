package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"gh-mirror/internal/store"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP tool sets selected by Service.Tools.
const (
	ToolsCore = "core"
	ToolsAll  = "all"
)

// coreTools are the search and ticket-read tools agents need for triage.
var coreTools = []string{"search_issues", "find_duplicate_candidates", "get_issue_context", "get_issues", "get_issue", "list_comments", "get_sync_status"}

// ValidateTools rejects an unknown MCP tool set before serving.
func (s *Service) ValidateTools() error {
	if s.Tools != "" && s.Tools != ToolsCore && s.Tools != ToolsAll {
		return fmt.Errorf("MCP tool set must be %s or %s", ToolsCore, ToolsAll)
	}
	return nil
}

// param documents one MCP input property.
type param struct {
	description string
	enum        []any
}

var (
	views       = []any{"summary", "full"}
	orders      = []any{"asc", "desc"}
	engines     = []any{"hybrid", "lexical", "semantic"}
	searchSorts = []any{"relevance", "number", "updated", "created"}
)

// params describes input properties shared by several tools; tools override meanings that differ.
var params = map[string]param{
	"repo":               {description: "Repository owner/name"},
	"number":             {description: "Ticket number"},
	"query":              {description: "Words to search for"},
	"engine":             {description: "Retrieval engine; omit for hybrid with a warned lexical fallback", enum: engines},
	"state":              {description: "Ticket state", enum: []any{"open", "closed"}},
	"label":              {description: "Exact label name; same as one labels_all entry"},
	"type":               {description: "Native issue type name"},
	"kind":               {description: "Ticket kind", enum: []any{"issue", "pull_request"}},
	"project":            {description: "Project membership as owner/number, including archived memberships"},
	"repositories":       {description: "Repositories owner/name to include"},
	"labels_all":         {description: "Exact labels that must all be attached"},
	"labels_any":         {description: "Exact labels of which at least one must be attached"},
	"exclude_labels":     {description: "Exact labels that must not be attached"},
	"author":             {description: "Ticket author login"},
	"assignees":          {description: "Assignee logins; any match"},
	"milestone":          {description: "Milestone title or number"},
	"created_after":      {description: "Inclusive lower creation bound: RFC3339, YYYY-MM-DD or age such as 12h, 7d, 2w"},
	"created_before":     {description: "Inclusive upper creation bound: RFC3339, YYYY-MM-DD or age such as 12h, 7d, 2w"},
	"updated_after":      {description: "Inclusive lower update bound: RFC3339, YYYY-MM-DD or age such as 12h, 7d, 2w"},
	"updated_before":     {description: "Inclusive upper update bound: RFC3339, YYYY-MM-DD or age such as 12h, 7d, 2w"},
	"field_values":       {description: "Collected custom field name/value pairs that must all match"},
	"sort":               {description: "Sort key; relevance applies to searches only", enum: searchSorts},
	"order":              {description: "Sort direction", enum: orders},
	"view":               {description: "summary returns compact metadata; full adds raw upstream records", enum: views},
	"count":              {description: "Count all matches before pagination"},
	"facets":             {description: "Count matching labels, types and projects"},
	"max_enrichment_age": {description: "Warn when requested metadata is older than this Go duration (default 24h)"},
	"match":              {description: "any word, all words across the ticket, or a phrase", enum: []any{"any", "all", "phrase"}},
	"prefix":             {description: "Match word prefixes (phrase: final word only)"},
	"in":                 {description: "Fields to search; default all", enum: []any{"title", "body", "labels", "comments", "reviews"}},
	"exclude_words":      {description: "Exclude tickets containing any of these literal words"},
	"limit":              {description: "Page size (1–100, default 30)"},
	"cursor":             {description: "next_cursor from the previous page; repeat the other options unchanged"},
	"evidence_limit":     {description: "Maximum supporting sources per ticket (1–10, default 3)"},
	"include_labels":     {description: "Use the seed ticket's labels in seed terms (default true)"},
	"include_comments":   {description: "Use recent seed comments in seed terms"},
	"comment_limit":      {description: "Maximum seed comments (1–100, default 20)"},
	"include_excluded":   {description: "Include comments excluded by the saved PR comment policy"},
}

// inputSchema infers a tool's input schema and adds descriptions and enums to every top-level property.
func inputSchema[T any](overrides map[string]param) *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("infer MCP input schema: %v", err))
	}
	for name, property := range schema.Properties {
		p, ok := overrides[name]
		if !ok {
			p, ok = params[name]
		}
		if !ok {
			panic(fmt.Sprintf("MCP input %q has no description", name))
		}
		property.Description = p.description
		if p.enum == nil {
			continue
		}
		if property.Items != nil {
			property.Items.Enum = p.enum
		} else {
			// Earlier clients sent "" for an omitted choice; the store treats it as the default.
			property.Enum = append(slices.Clone(p.enum), "")
		}
	}
	return schema
}

// compactResult keeps only the generation and collection time of status blocks; agents call get_sync_status for the rest.
func compactResult(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode tool result: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decode tool result: %w", err)
	}
	if err := compactStatus(fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func compactStatus(fields map[string]json.RawMessage) error {
	if raw, ok := fields["status"]; ok {
		var status struct {
			Generation  string `json:"generation"`
			CollectedAt string `json:"collected_at"`
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			return fmt.Errorf("decode result status: %w", err)
		}
		compact, err := json.Marshal(status)
		if err != nil {
			return fmt.Errorf("encode result status: %w", err)
		}
		fields["status"] = compact
	}
	if raw, ok := fields["comment_page"]; ok && string(raw) != "null" {
		var page map[string]json.RawMessage
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("decode comment page: %w", err)
		}
		if err := compactStatus(page); err != nil {
			return err
		}
		compact, err := json.Marshal(page)
		if err != nil {
			return fmt.Errorf("encode comment page: %w", err)
		}
		fields["comment_page"] = compact
	}
	return nil
}

// addTool registers a read-only tool whose result omits an output schema and carries a compact status.
func addTool[In, Out any](s *Service, server *mcp.Server, name, description string, overrides map[string]param, read func(context.Context, In) (Out, error)) {
	if s.Tools != ToolsAll && !slices.Contains(coreTools, name) {
		return
	}
	destructive, openWorld := false, false
	tool := &mcp.Tool{Name: name, Description: description, InputSchema: inputSchema[In](overrides), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &destructive, OpenWorldHint: &openWorld}}
	mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, input In) (*mcp.CallToolResult, any, error) {
		out, err := read(ctx, input)
		if err != nil {
			return nil, nil, err
		}
		if name == "get_sync_status" {
			return nil, out, nil
		}
		compact, err := compactResult(out)
		return nil, compact, err
	})
}

type catalogInput struct {
	Kind  string `json:"kind"`
	Scope string `json:"scope"`
}
type projectInput struct {
	Owner  string `json:"owner"`
	Number int    `json:"number"`
}

var catalogParams = map[string]param{
	"kind":   {description: "Catalog name", enum: []any{"labels", "milestones", "issue_types", "issue_fields", "projects"}},
	"scope":  {description: "Repository owner/name for labels and milestones; owner for issue_types, issue_fields and projects"},
	"limit":  {description: "Page size (1–100, default 30)"},
	"cursor": {description: "next_cursor from the previous page with the same kind and scope"},
}

// MCP creates local read tools; tool calls never fetch upstream data.
func (s *Service) MCP() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "gh-mirror", Version: s.Version}, nil)
	addTool(s, server, "search_issues", "Retrieve relevant local ticket context for investigation and evaluation: behavior, decisions, workarounds, fixes and related history. Searches titles, bodies, labels, discussions and reviews; returns compact summaries and source-linked evidence. Defaults to hybrid; missing/incomplete/incompatible indexes fall back to lexical with a warning. All/phrase/prefix select lexical unless an engine is explicit. Explicit semantic/hybrid require a compatible complete index. Inspect ranking and warnings; scores are not relevance probabilities. Use get_issues/get_issue/list_comments for details. Follow next_cursor with identical options.", map[string]param{"query": {description: "Words to search for (required)"}}, s.Store.Search)
	addTool(s, server, "find_duplicate_candidates", "Retrieve related history from a seed ticket for evaluation or duplicate investigation. Defaults to hybrid with a warned lexical fallback when the index is unavailable; explicit semantic/hybrid require a complete compatible index. Defaults to the seed repository; repositories can widen scope. Labels are included by default; bounded recent seed comments are opt-in. Includes closed history. Inspect summaries and source evidence; scores do not establish duplicate probability. Follow next_cursor with identical options.", map[string]param{"repo": {description: "Seed ticket repository owner/name"}, "number": {description: "Seed ticket number"}, "repositories": {description: "Repositories to search instead of the seed repository"}}, s.Store.FindCandidates)
	addTool(s, server, "get_issue_context", "Read bounded original ticket text, structured label attributes, section offsets, selected relevant/recent comments and one-hop relationships from one local generation. purpose=evaluate/classification/duplicates/proofread; missing sections are observations, references are not duplicate judgments. Explicit coverage and truncation report omitted material. Saved PR comment policies apply by default; include_excluded=true inspects retained excluded sources. Query text selects discussion evidence; no upstream requests or embeddings required.", map[string]param{
		"purpose":        {description: "Selects which evidence to prioritise", enum: []any{"evaluate", "classification", "duplicates", "proofread"}},
		"query":          {description: "Words that select matching discussion alongside recent comments"},
		"comment_limit":  {description: "Maximum selected comments (1–50, default 10)"},
		"related_limit":  {description: "Maximum expanded related tickets (1–20, default 5)"},
		"max_characters": {description: "Shared source-text budget (1000–100000 characters, default 12000)"},
	}, s.Store.Context)
	addTool(s, server, "get_issues", "Read up to 100 selected tickets from one mirror generation, without comments. Defaults to compact summaries and explicitly reports missing identities.", map[string]param{"tickets": {description: "1–100 ticket identities as {repo, number}"}}, s.Store.Batch)
	addTool(s, server, "get_issue", "Read one local ticket. Defaults to view=summary without comments. Use comments=page with limit/cursor/comment_kind for bounded comments, view=full for the raw upstream payload, or comments=all for every comment.", map[string]param{
		"comments":     {description: "none (default), page for a bounded page, or all for every comment", enum: []any{"none", "page", "all"}},
		"comment_kind": {description: "Comment kind for comments=page", enum: []any{"discussion", "review"}},
		"limit":        {description: "Comment page size for comments=page (1–100, default 30)"},
		"cursor":       {description: "next_cursor from comment_page for comments=page"},
	}, s.readIssue)
	addTool(s, server, "list_comments", "Read a bounded page of discussion or inline review comments. Summary previews are limited to 1024 characters; full view preserves raw records. Saved PR comment policies apply; include_excluded=true returns excluded sources. Follow next_cursor with the same options.", map[string]param{
		"kind":  {description: "Comment kind", enum: []any{"all", "discussion", "review"}},
		"order": {description: "Creation time order (default asc)", enum: orders},
	}, s.Store.Comments)
	addTool(s, server, "get_sync_status", "Read generation, scope, collection timestamps and coverage.", nil, func(ctx context.Context, _ struct{}) (store.Status, error) { return s.Store.Status(ctx) })
	addTool(s, server, "list_issues", "List local tickets with repository, state, label, type, kind and project owner/number filters. Follow next_cursor with the same filters; cursors pin a mirror generation.", map[string]param{"sort": {description: "Sort key (default number)", enum: []any{"number", "updated", "created"}}}, s.Store.List)
	addTool(s, server, "get_catalog", "Read a complete labels, milestones, issue_types, issue_fields or projects catalog for a scope.", catalogParams, func(ctx context.Context, input catalogInput) (store.CatalogResult, error) {
		return s.Store.Catalog(ctx, input.Kind, input.Scope)
	})
	addTool(s, server, "list_catalog", "Read a bounded catalog page; follow next_cursor with the same kind and scope.", catalogParams, s.Store.CatalogPage)
	addTool(s, server, "get_project", "Read a local owner's project catalog record and collection status.", map[string]param{"owner": {description: "Project owner login"}, "number": {description: "Project number"}}, func(ctx context.Context, input projectInput) (store.ProjectResult, error) {
		return s.Store.Project(ctx, input.Owner, input.Number)
	})
	return server
}

// readIssue applies the agent defaults: summary view and no comments unless paging options are given.
func (s *Service) readIssue(ctx context.Context, input store.ReadOptions) (store.Issue, error) {
	if input.View == "" {
		input.View = "summary"
	}
	if input.Comments == "" {
		input.Comments = "none"
		if input.Limit != 0 || input.Cursor != "" || input.CommentKind != "" {
			input.Comments = "page"
		}
	}
	return s.Store.Read(ctx, input)
}

// RunMCP serves local tools over the process's stdio transport.
func (s *Service) RunMCP(ctx context.Context) error {
	if err := s.MCP().Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("serve MCP stdio: %w", err)
	}
	return nil
}
