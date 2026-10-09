package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"gh-mirror/internal/github"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

func projectInventory(ctx context.Context, c *github.Client, id string) ([]json.RawMessage, error) {
	if id == "" {
		return nil, fmt.Errorf("project is missing its GraphQL node identity")
	}
	const query = `query($id:ID!,$cursor:String){node(id:$id){... on ProjectV2{items(first:100,after:$cursor,archivedStates:[ARCHIVED,NOT_ARCHIVED]){nodes{id fullDatabaseId isArchived} pageInfo{hasNextPage endCursor}}}}}`
	out := []json.RawMessage{}
	seen := map[string]bool{}
	var cursor *string
	for {
		var data struct {
			Node map[string]json.RawMessage `json:"node"`
		}
		if err := c.GraphQL(ctx, query, map[string]any{"id": id, "cursor": cursor}, &data); err != nil {
			return nil, fmt.Errorf("inventory archived and active project items: %w", err)
		}
		var page connection
		if err := json.Unmarshal(data.Node["items"], &page); err != nil {
			return nil, fmt.Errorf("decode project item inventory: %w", err)
		}
		if page.PageInfo == nil || page.Nodes == nil {
			return nil, fmt.Errorf("incomplete project item inventory")
		}
		for _, raw := range page.Nodes {
			object, err := store.Object(raw)
			if err != nil {
				return nil, fmt.Errorf("decode project inventory node: %w", err)
			}
			if store.Text(object, "id") == "" || store.Identity(object, "fullDatabaseId") == "" {
				return nil, fmt.Errorf("missing project item identity")
			}
		}
		out = append(out, page.Nodes...)
		if !page.PageInfo.HasNextPage {
			return out, nil
		}
		next := page.PageInfo.EndCursor
		if next == "" || seen[next] {
			return nil, fmt.Errorf("empty or repeated project inventory cursor")
		}
		seen[next] = true
		cursor = &next
	}
}
func projectItems(ctx context.Context, c *github.Client, base, id string, query url.Values) ([]json.RawMessage, error) {
	c.Progress.Send(progress.Event{Phase: "Inventorying project items", Scope: base})
	inventory, err := projectInventory(ctx, c, id)
	if err != nil {
		return nil, fmt.Errorf("collect complete project inventory: %w", err)
	}
	c.Progress.Send(progress.Event{Phase: "Fetching project items", Scope: base})
	items, err := c.List(ctx, base+"/items?"+query.Encode())
	if err != nil {
		return nil, fmt.Errorf("list projected project values: %w", err)
	}
	seen := map[string]bool{}
	for _, raw := range items {
		object, err := store.Object(raw)
		if err != nil {
			return nil, fmt.Errorf("decode projected project item: %w", err)
		}
		nodeID := store.Text(object, "node_id")
		if nodeID == "" {
			return nil, fmt.Errorf("project item is missing a node identity")
		}
		seen[nodeID] = true
	}
	for _, raw := range inventory {
		object, err := store.Object(raw)
		if err != nil {
			return nil, fmt.Errorf("decode inventory identity: %w", err)
		}
		nodeID := store.Text(object, "id")
		if seen[nodeID] {
			continue
		}
		itemID := store.Identity(object, "fullDatabaseId")
		params := url.Values{}
		if fields := query.Get("fields"); fields != "" {
			params.Set("fields", fields)
		}
		// Some REST implementations omit archived items from their bulk listing.
		item, err := c.Get(ctx, base+"/items/"+url.PathEscape(itemID)+"?"+params.Encode())
		if err != nil {
			return nil, fmt.Errorf("fetch project item missing from bulk listing: %w", err)
		}
		record, err := store.Object(item)
		if err != nil {
			return nil, fmt.Errorf("decode missing project item: %w", err)
		}
		if store.Text(record, "node_id") != nodeID || store.Identity(record, "id") != itemID {
			return nil, fmt.Errorf("project inventory identity mismatch")
		}
		items = append(items, item)
		seen[nodeID] = true
	}
	return items, nil
}
