package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// TicketSummary provides common ticket metadata without a full upstream payload.
type TicketSummary struct {
	Title     string   `json:"title"`
	State     string   `json:"state"`
	URL       string   `json:"url"`
	Type      string   `json:"type,omitempty"`
	Author    string   `json:"author,omitempty"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
	Projects  []string `json:"projects"`
	Milestone string   `json:"milestone,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
	UpdatedAt string   `json:"updated_at"`
}

// Evidence identifies the field and source supporting a search match.
type Evidence struct {
	Field         string   `json:"field"`
	Source        string   `json:"source"`
	Snippet       string   `json:"snippet"`
	CommentID     string   `json:"comment_id,omitempty"`
	CreatedAt     string   `json:"created_at,omitempty"`
	Path          string   `json:"path,omitempty"`
	Line          int      `json:"line,omitempty"`
	DiffHunk      string   `json:"diff_hunk,omitempty"`
	Start         int      `json:"start,omitempty"`
	End           int      `json:"end,omitempty"`
	SemanticScore *float64 `json:"semantic_score,omitempty"`
}

// Warning describes requested resources that are unavailable or stale locally.
type Warning struct {
	Code     string `json:"code"`
	Repo     string `json:"repo"`
	Resource string `json:"resource"`
	Message  string `json:"message"`
}

// Bucket counts matching tickets sharing a facet value.
type Bucket struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// FacetResult contains bounded counts over all matching tickets, before pagination.
type FacetResult struct {
	Labels    []Bucket `json:"labels"`
	Types     []Bucket `json:"types"`
	Projects  []Bucket `json:"projects"`
	Truncated bool     `json:"truncated"`
}

func summarize(raw, extra json.RawMessage) (*TicketSummary, error) {
	var o struct {
		Title     string                   `json:"title"`
		State     string                   `json:"state"`
		URL       string                   `json:"html_url"`
		Type      *struct{ Name string }   `json:"type"`
		User      *struct{ Login string }  `json:"user"`
		Labels    []struct{ Name string }  `json:"labels"`
		Assignees []struct{ Login string } `json:"assignees"`
		Milestone *struct{ Title string }  `json:"milestone"`
		CreatedAt string                   `json:"created_at"`
		UpdatedAt string                   `json:"updated_at"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("decode ticket summary: %w", err)
	}
	s := &TicketSummary{Title: o.Title, State: o.State, URL: o.URL, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt, Labels: []string{}, Assignees: []string{}, Projects: []string{}}
	if o.Type != nil {
		s.Type = o.Type.Name
	}
	if o.User != nil {
		s.Author = o.User.Login
	}
	if o.Milestone != nil {
		s.Milestone = o.Milestone.Title
	}
	for _, v := range o.Labels {
		s.Labels = append(s.Labels, v.Name)
	}
	for _, v := range o.Assignees {
		s.Assignees = append(s.Assignees, v.Login)
	}
	var e struct {
		ProjectItems []struct{ Project struct{ URL string } }
	}
	if err := json.Unmarshal(extra, &e); err != nil {
		return nil, fmt.Errorf("decode project summary: %w", err)
	}
	for _, v := range e.ProjectItems {
		if name := projectName(v.Project.URL); name != "" && !slices.Contains(s.Projects, name) {
			s.Projects = append(s.Projects, name)
		}
	}
	return s, nil
}
func projectName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(p) == 4 && (p[0] == "orgs" || p[0] == "users") && p[2] == "projects" {
		return p[1] + "/" + p[3]
	}
	return ""
}

func queryWarnings(s Status, f filters, p PageOptions, in []string) []Warning {
	out := []Warning{}
	requested := append([]string{}, f.Repositories...)
	if f.Repo != "" {
		requested = append(requested, f.Repo)
	}
	for _, repo := range requested {
		if !slices.Contains(s.Repositories, repo) {
			out = append(out, Warning{"repository_not_collected", repo, "repository", "Requested repository is not part of this mirror."})
		}
	}
	age := 24 * time.Hour
	if p.MaxEnrichmentAge != "" {
		if d, err := time.ParseDuration(p.MaxEnrichmentAge); err == nil {
			age = d
		}
	}
	resources := []string{}
	if f.Project != "" {
		resources = append(resources, "projects")
	}
	if len(f.FieldValues) > 0 {
		resources = append(resources, "fields")
	}
	if slices.Contains(in, "comments") {
		resources = append(resources, "comments")
	}
	if slices.Contains(in, "reviews") {
		resources = append(resources, "reviews")
	}
	for _, repo := range s.Repositories {
		if f.Repo != "" && repo != f.Repo || len(f.Repositories) > 0 && !slices.Contains(f.Repositories, repo) {
			continue
		}
		var options map[string]bool
		if raw, ok := s.RepositoryOptions[repo]; ok {
			if err := json.Unmarshal(raw, &options); err != nil {
				out = append(out, Warning{"coverage_unknown", repo, "scope", "Stored resource coverage cannot be decoded."})
				continue
			}
		}
		for _, resource := range resources {
			enabled := resource != "reviews"
			if options != nil {
				switch resource {
				case "comments":
					enabled = options["issue_comments"] || options["pull_request_comments"]
					if f.Kind == "issue" {
						enabled = options["issue_comments"]
					}
					if f.Kind == "pull_request" {
						enabled = options["pull_request_comments"]
					}
				case "reviews":
					enabled = options["pull_request_review_comments"]
				default:
					enabled = options[resource]
				}
			}
			var enriched string
			for _, c := range s.Coverage {
				if c.Repo == repo {
					enriched = c.ReconciledAt
					if resource == "fields" && c.Fields == "disabled" || resource == "projects" && c.Projects == "disabled" {
						enabled = false
					}
				}
			}
			if !enabled {
				out = append(out, Warning{"resource_disabled", repo, resource, "Requested resource is disabled in this mirror."})
				continue
			}
			if resource == "fields" || resource == "projects" {
				t, err := time.Parse(time.RFC3339, enriched)
				if err != nil || time.Since(t) > age {
					out = append(out, Warning{"enrichment_stale", repo, resource, "Requested metadata has no recent enrichment checkpoint."})
				}
			}
		}
	}
	if f.Kind != "" {
		for _, repo := range s.Repositories {
			if f.Repo != "" && repo != f.Repo || len(f.Repositories) > 0 && !slices.Contains(f.Repositories, repo) {
				continue
			}
			var options map[string]bool
			if raw, ok := s.RepositoryOptions[repo]; ok {
				if err := json.Unmarshal(raw, &options); err == nil {
					key := "issues"
					if f.Kind == "pull_request" {
						key = "pull_requests"
					}
					if !options[key] {
						out = append(out, Warning{"resource_disabled", repo, key, "Requested ticket kind is disabled in this mirror."})
					}
				}
			}
		}
	}
	return out
}

func facets(ctx context.Context, q querier, cte, selected string, args []any) (*FacetResult, error) {
	out := &FacetResult{Labels: []Bucket{}, Types: []Bucket{}, Projects: []Bucket{}}
	definitions := []struct {
		query string
		dest  *[]Bucket
	}{
		{`SELECT value,count(*) FROM (SELECT DISTINCT i.repo,i.number,json_extract(l.value,'$.name') value FROM selected s JOIN issues i USING(repo,number),json_each(i.payload,'$.labels') l) WHERE value IS NOT NULL GROUP BY value ORDER BY count(*) DESC,value LIMIT 101`, &out.Labels},
		{`SELECT json_extract(i.payload,'$.type.name') value,count(*) FROM selected s JOIN issues i USING(repo,number) WHERE value IS NOT NULL GROUP BY value ORDER BY count(*) DESC,value LIMIT 101`, &out.Types},
		{`SELECT value,count(*) FROM (SELECT DISTINCT i.repo,i.number,CASE WHEN instr(json_extract(p.value,'$.project.url'),'/orgs/')>0 THEN replace(substr(json_extract(p.value,'$.project.url'),instr(json_extract(p.value,'$.project.url'),'/orgs/')+6),'/projects/','/') WHEN instr(json_extract(p.value,'$.project.url'),'/users/')>0 THEN replace(substr(json_extract(p.value,'$.project.url'),instr(json_extract(p.value,'$.project.url'),'/users/')+7),'/projects/','/') END value FROM selected s JOIN issues i USING(repo,number),json_each(i.extra,'$.projectItems') p) WHERE value IS NOT NULL GROUP BY value ORDER BY count(*) DESC,value LIMIT 101`, &out.Projects},
	}
	for _, d := range definitions {
		rows, err := q.QueryContext(ctx, cte+",selected AS ("+selected+") "+d.query, args...)
		if err != nil {
			return nil, fmt.Errorf("query facets: %w", err)
		}
		for rows.Next() {
			var b Bucket
			if err := rows.Scan(&b.Value, &b.Count); err != nil {
				return nil, finishRows(rows, err)
			}
			*d.dest = append(*d.dest, b)
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return nil, err
		}
		if len(*d.dest) > 100 {
			*d.dest = (*d.dest)[:100]
			out.Truncated = true
		}
	}
	return out, nil
}
