package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gh-mirror/internal/github"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

const pageInfo = `pageInfo { hasNextPage endCursor }`
const related = `id number url repository { nameWithOwner }`
const membership = `id isArchived project { id number title url }`
const fieldValues = `__typename
 ... on IssueFieldValueCommon { field { ... on Node { id } ... on IssueFieldCommon { name dataType } } }
 ... on IssueFieldTextValue { id textValue:value }
 ... on IssueFieldDateValue { id dateValue:value }
 ... on IssueFieldNumberValue { id numberValue:value }
 ... on IssueFieldSingleSelectValue { id singleValue:value optionId name color description }
 ... on IssueFieldMultiSelectValue { id multiValue:value options { id name } }`

type connection struct {
	Nodes    []json.RawMessage `json:"nodes"`
	PageInfo *struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

func selection(name, cursor string) string {
	args := "first:100"
	if cursor != "" {
		args += ",after:$cursor"
	}
	nodes := related
	switch name {
	case "issueFieldValues":
		nodes = fieldValues
	case "projectItems":
		nodes = membership
		args += ",includeArchived:true"
	}
	return name + "(" + args + "){nodes{" + nodes + "} " + pageInfo + "}"
}

type hydratedIssue struct {
	ref           store.IssueRef
	values, extra json.RawMessage
}

func hydrate(ctx context.Context, c *github.Client, w *store.Writer, refs []store.IssueRef, fields, projects, relationships bool, workers int) error {
	return parallelFetch(ctx, workers, (len(refs)+49)/50,
		func(ctx context.Context, index int) ([]hydratedIssue, error) {
			start := index * 50
			return fetchBatch(ctx, c, refs[start:min(start+50, len(refs))], fields, projects, relationships)
		},
		func(batch []hydratedIssue) error {
			for _, issue := range batch {
				if err := w.PutExtra(ctx, issue.ref, issue.values, issue.extra); err != nil {
					return fmt.Errorf("store hydrated issue: %w", err)
				}
			}
			c.Report(progress.Event{Advance: len(batch)})
			return nil
		})
}

func fetchBatch(ctx context.Context, c *github.Client, batch []store.IssueRef, fields, projects, relationships bool) ([]hydratedIssue, error) {
	out := make([]hydratedIssue, 0, len(batch))
	ids := []string{}
	for _, ref := range batch {
		ids = append(ids, ref.NodeID)
	}
	names := []string{}
	if relationships {
		names = append(names, "subIssues", "blockedBy", "blocking")
	}
	if fields {
		names = append(names, "issueFieldValues")
	}
	issueSelection := ""
	if relationships {
		issueSelection = `parent { ` + related + ` } `
	}
	for _, name := range names {
		issueSelection += selection(name, "") + " "
	}
	projectSelection := ""
	if projects {
		projectSelection = selection("projectItems", "")
	}
	query := `query($ids:[ID!]!){nodes(ids:$ids){id __typename ... on Issue{` + issueSelection + projectSelection + `} ... on PullRequest{` + projectSelection + `}}}`
	// GraphQL forbids empty selection sets when project collection is disabled.
	if !projects {
		query = strings.ReplaceAll(query, "... on PullRequest{}", "")
		query = strings.ReplaceAll(query, "... on Issue{}", "")
	}
	var data struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if err := c.GraphQLValidated(ctx, query, map[string]any{"ids": ids}, &data, func(raw json.RawMessage) error {
		return validateBatch(raw, batch, names, projects, relationships)
	}); err != nil {
		return nil, fmt.Errorf("batch issue metadata: %w", err)
	}
	if len(data.Nodes) != len(batch) {
		return nil, fmt.Errorf("GraphQL issue inventory count mismatch")
	}
	for i, raw := range data.Nodes {
		object, err := store.Object(raw)
		if err != nil {
			return nil, fmt.Errorf("inaccessible GraphQL node %s: %w", batch[i].NodeID, err)
		}
		if store.Text(object, "id") != batch[i].NodeID {
			return nil, fmt.Errorf("GraphQL issue node identity mismatch")
		}
		expected := "Issue"
		if batch[i].Kind == "pull_request" {
			expected = "PullRequest"
		}
		if store.Text(object, "__typename") != expected {
			return nil, fmt.Errorf("GraphQL node changed resource kind")
		}
		nodeNames := []string{}
		if expected == "Issue" {
			nodeNames = append(nodeNames, names...)
			if _, ok := object["parent"]; relationships && !ok {
				return nil, fmt.Errorf("GraphQL issue is missing parent observation")
			}
		}
		if projects {
			nodeNames = append(nodeNames, "projectItems")
		}
		values := json.RawMessage(`[]`)
		for _, name := range nodeNames {
			items, err := connectionItems(ctx, c, batch[i], name, object[name])
			if err != nil {
				return nil, fmt.Errorf("paginate %s on %s#%d: %w", name, batch[i].Repo, batch[i].Number, err)
			}
			encoded, err := json.Marshal(items)
			if err != nil {
				return nil, fmt.Errorf("encode connection: %w", err)
			}
			if name == "issueFieldValues" {
				items, err = normalizeValues(items)
				if err != nil {
					return nil, fmt.Errorf("normalize native field values: %w", err)
				}
				encoded, err = json.Marshal(items)
				if err != nil {
					return nil, fmt.Errorf("encode native field values: %w", err)
				}
				values = encoded
				delete(object, name)
			} else {
				object[name] = encoded
			}
		}
		extra, err := json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("encode issue observations: %w", err)
		}
		out = append(out, hydratedIssue{batch[i], values, extra})
	}

	return out, nil
}

func normalizeValues(items []json.RawMessage) ([]json.RawMessage, error) {
	for i, raw := range items {
		object, err := store.Object(raw)
		if err != nil {
			return nil, fmt.Errorf("decode native value: %w", err)
		}
		// Distinct aliases avoid GraphQL conflicts between String and Float values.
		for _, key := range []string{"textValue", "dateValue", "numberValue", "singleValue", "multiValue"} {
			if value, ok := object[key]; ok {
				object["value"] = value
				delete(object, key)
			}
		}
		items[i], err = json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("encode native value: %w", err)
		}
	}
	return items, nil
}

func validateConnection(raw json.RawMessage) (connection, error) {
	var page connection
	if err := json.Unmarshal(raw, &page); err != nil {
		return page, fmt.Errorf("decode connection: %w", err)
	}
	if page.PageInfo == nil || page.Nodes == nil {
		return page, fmt.Errorf("missing GraphQL connection data")
	}
	if page.PageInfo.HasNextPage && page.PageInfo.EndCursor == "" {
		return page, fmt.Errorf("empty GraphQL cursor")
	}
	for _, node := range page.Nodes {
		if _, err := store.Object(node); err != nil {
			return page, fmt.Errorf("inaccessible connection node: %w", err)
		}
	}
	return page, nil
}

func validateBatch(raw json.RawMessage, batch []store.IssueRef, names []string, projects, relationships bool) error {
	var data struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode batch: %w", err)
	}
	if len(data.Nodes) != len(batch) {
		return fmt.Errorf("GraphQL issue inventory count mismatch")
	}
	for i, raw := range data.Nodes {
		object, err := store.Object(raw)
		if err != nil {
			return fmt.Errorf("inaccessible GraphQL node %s: %w", batch[i].NodeID, err)
		}
		expected := "Issue"
		if batch[i].Kind == "pull_request" {
			expected = "PullRequest"
		}
		if store.Text(object, "id") != batch[i].NodeID || store.Text(object, "__typename") != expected {
			return fmt.Errorf("GraphQL issue identity or resource kind mismatch")
		}
		connections := []string{}
		if expected == "Issue" {
			parent, ok := object["parent"]
			if relationships && !ok {
				return fmt.Errorf("GraphQL issue is missing parent observation")
			}
			if relationships && string(parent) != "null" {
				if _, err := store.Object(parent); err != nil {
					return fmt.Errorf("invalid parent observation: %w", err)
				}
			}
			connections = append(connections, names...)
		}
		if projects {
			connections = append(connections, "projectItems")
		}
		for _, name := range connections {
			if _, err := validateConnection(object[name]); err != nil {
				return fmt.Errorf("validate %s on %s: %w", name, batch[i].NodeID, err)
			}
		}
	}
	return nil
}
func connectionItems(ctx context.Context, c *github.Client, ref store.IssueRef, name string, raw json.RawMessage) ([]json.RawMessage, error) {
	out := []json.RawMessage{}
	seen := map[string]bool{}
	for {
		page, err := validateConnection(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Nodes...)
		if !page.PageInfo.HasNextPage {
			return out, nil
		}
		cursor := page.PageInfo.EndCursor
		if cursor == "" || seen[cursor] {
			return nil, fmt.Errorf("empty or repeated GraphQL cursor")
		}
		seen[cursor] = true
		typ := "Issue"
		if ref.Kind == "pull_request" {
			typ = "PullRequest"
		}
		query := `query($id:ID!,$cursor:String!){node(id:$id){... on ` + typ + `{` + selection(name, cursor) + `}}}`
		var data struct {
			Node map[string]json.RawMessage `json:"node"`
		}
		if err := c.GraphQLValidated(ctx, query, map[string]any{"id": ref.NodeID, "cursor": cursor}, &data, func(raw json.RawMessage) error {
			var response struct {
				Node map[string]json.RawMessage `json:"node"`
			}
			if err := json.Unmarshal(raw, &response); err != nil {
				return fmt.Errorf("decode connection response: %w", err)
			}
			page, err := validateConnection(response.Node[name])
			if err != nil {
				return err
			}
			if page.PageInfo.HasNextPage && seen[page.PageInfo.EndCursor] {
				return fmt.Errorf("repeated GraphQL cursor")
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("fetch connection page: %w", err)
		}
		raw = data.Node[name]
	}
}
