package store

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gh-mirror/internal/config"
)

// FieldFilter matches a collected native issue field by name and scalar or option value.
type FieldFilter struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// QueryFilters adds composable predicates shared by search and listing.
type QueryFilters struct {
	Repositories  []string      `json:"repositories,omitempty"`
	LabelsAll     []string      `json:"labels_all,omitempty"`
	LabelsAny     []string      `json:"labels_any,omitempty"`
	ExcludeLabels []string      `json:"exclude_labels,omitempty"`
	Author        string        `json:"author,omitempty"`
	Assignees     []string      `json:"assignees,omitempty"`
	Milestone     string        `json:"milestone,omitempty"`
	CreatedAfter  string        `json:"created_after,omitempty"`
	CreatedBefore string        `json:"created_before,omitempty"`
	UpdatedAfter  string        `json:"updated_after,omitempty"`
	UpdatedBefore string        `json:"updated_before,omitempty"`
	FieldValues   []FieldFilter `json:"field_values,omitempty"`
}

// PageOptions controls ordering, projection and optional aggregate work.
type PageOptions struct {
	Sort             string `json:"sort,omitempty"`
	Order            string `json:"order,omitempty"`
	View             string `json:"view,omitempty"`
	Count            bool   `json:"count,omitempty"`
	Facets           bool   `json:"facets,omitempty"`
	MaxEnrichmentAge string `json:"max_enrichment_age,omitempty"`
}

type filters struct {
	Repo, State, Label, Type, Kind, Project string
	QueryFilters
}

var projectPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[1-9][0-9]*$`)

func (f filters) sql() (string, []any, error) {
	clauses := []string{"1=1"}
	args := []any{}
	add := func(s string, values ...any) { clauses = append(clauses, s); args = append(args, values...) }
	if f.Repo != "" && !config.ValidRepository(f.Repo) {
		return "", nil, invalid("invalid repository")
	}
	if f.State != "" && f.State != "open" && f.State != "closed" {
		return "", nil, invalid("state must be open or closed")
	}
	if f.Kind != "" && f.Kind != "issue" && f.Kind != "pull_request" {
		return "", nil, invalid("kind must be issue or pull_request")
	}
	for _, v := range []struct{ column, value string }{{"i.repo", f.Repo}, {"i.state", f.State}, {"i.kind", f.Kind}, {"json_extract(i.payload,'$.type.name')", f.Type}, {"json_extract(i.payload,'$.user.login')", f.Author}} {
		if v.value != "" {
			add(v.column+"=?", v.value)
		}
	}
	if len(f.Repositories) > 100 {
		return "", nil, invalid("at most 100 repositories allowed")
	}
	if len(f.Repositories) > 0 {
		marks := []string{}
		values := []any{}
		for _, repo := range f.Repositories {
			if !config.ValidRepository(repo) {
				return "", nil, invalid("invalid repository")
			}
			marks = append(marks, "?")
			values = append(values, repo)
		}
		add("i.repo IN ("+strings.Join(marks, ",")+")", values...)
	}
	labels := append([]string{}, f.LabelsAll...)
	if f.Label != "" {
		labels = append(labels, f.Label)
	}
	if len(labels)+len(f.LabelsAny)+len(f.ExcludeLabels)+len(f.Assignees)+len(f.FieldValues) > 100 {
		return "", nil, invalid("at most 100 compound predicates allowed")
	}
	for _, label := range labels {
		add("EXISTS(SELECT 1 FROM json_each(i.payload,'$.labels') l WHERE json_extract(l.value,'$.name')=?)", label)
	}
	if len(f.LabelsAny) > 0 {
		choices := []string{}
		values := []any{}
		for _, label := range f.LabelsAny {
			choices = append(choices, "json_extract(l.value,'$.name')=?")
			values = append(values, label)
		}
		add("EXISTS(SELECT 1 FROM json_each(i.payload,'$.labels') l WHERE "+strings.Join(choices, " OR ")+")", values...)
	}
	for _, label := range f.ExcludeLabels {
		add("NOT EXISTS(SELECT 1 FROM json_each(i.payload,'$.labels') l WHERE json_extract(l.value,'$.name')=?)", label)
	}
	if len(f.Assignees) > 0 {
		choices := []string{}
		values := []any{}
		for _, login := range f.Assignees {
			choices = append(choices, "json_extract(a.value,'$.login')=?")
			values = append(values, login)
		}
		add("EXISTS(SELECT 1 FROM json_each(i.payload,'$.assignees') a WHERE "+strings.Join(choices, " OR ")+")", values...)
	}
	if f.Milestone != "" {
		add("(json_extract(i.payload,'$.milestone.title')=? OR CAST(json_extract(i.payload,'$.milestone.number') AS TEXT)=?)", f.Milestone, f.Milestone)
	}
	now := time.Now()
	for _, bounds := range []struct{ after, before, column string }{{f.CreatedAfter, f.CreatedBefore, "json_extract(i.payload,'$.created_at')"}, {f.UpdatedAfter, f.UpdatedBefore, "i.updated_at"}} {
		var low, high time.Time
		var exclusive bool
		for _, v := range []struct {
			value  string
			before bool
		}{{bounds.after, false}, {bounds.before, true}} {
			if v.value == "" {
				continue
			}
			t, op, err := dateBound(v.value, v.before, now)
			if err != nil {
				return "", nil, err
			}
			if v.before {
				high, exclusive = t, op == "<"
			} else {
				low = t
			}
			add("julianday("+bounds.column+")"+op+"julianday(?)", t.UTC().Format(time.RFC3339Nano))
		}
		if !low.IsZero() && !high.IsZero() && (low.After(high) || exclusive && low.Equal(high)) {
			return "", nil, invalid("after must not exceed before")
		}
	}
	for _, field := range f.FieldValues {
		if field.Name == "" {
			return "", nil, invalid("field name is required")
		}
		add(`EXISTS(SELECT 1 FROM json_each(i.fields) f WHERE json_extract(f.value,'$.field.name')=? AND (CAST(json_extract(f.value,'$.value') AS TEXT)=? OR json_extract(f.value,'$.name')=? OR EXISTS(SELECT 1 FROM json_each(f.value,'$.options') o WHERE json_extract(o.value,'$.name')=?)))`, field.Name, field.Value, field.Value, field.Value)
	}
	if f.Project != "" {
		if !projectPattern.MatchString(f.Project) {
			return "", nil, invalid("project must be owner/number")
		}
		owner, number, _ := strings.Cut(f.Project, "/")
		suffix := owner + "/projects/" + number
		org, user := "/orgs/"+suffix, "/users/"+suffix
		add(`EXISTS(SELECT 1 FROM json_each(i.extra,'$.projectItems') p WHERE substr(json_extract(p.value,'$.project.url'),-length(?))=? OR substr(json_extract(p.value,'$.project.url'),-length(?))=?)`, org, org, user, user)
	}
	return strings.Join(clauses, " AND "), args, nil
}

var relativeDate = regexp.MustCompile(`^([1-9][0-9]{0,5})([hdw])$`)

// dateBound resolves an inclusive RFC3339, calendar-day or relative bound. A before bound
// on a plain date covers that whole UTC day, so it compares strictly with the next midnight.
func dateBound(value string, before bool, now time.Time) (time.Time, string, error) {
	op := ">="
	if before {
		op = "<="
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, op, nil
	}
	if t, err := time.Parse(time.DateOnly, value); err == nil {
		if before {
			return t.AddDate(0, 0, 1), "<", nil
		}
		return t, op, nil
	}
	if m := relativeDate.FindStringSubmatch(value); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return time.Time{}, "", invalid("invalid relative date %q", value)
		}
		unit := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		return now.Add(-time.Duration(n) * unit), op, nil
	}
	return time.Time{}, "", invalid("date bounds must be RFC3339 timestamps, YYYY-MM-DD dates or relative ages such as 12h, 7d or 2w")
}

func (p *PageOptions) normalize(search bool) error {
	if p.Sort == "" {
		if search {
			p.Sort = "relevance"
		} else {
			p.Sort = "number"
		}
	}
	if p.Sort != "number" && p.Sort != "updated" && p.Sort != "created" && (!search || p.Sort != "relevance") {
		return invalid("sort must be number, updated, created or search relevance")
	}
	if p.Order == "" {
		if p.Sort == "updated" || p.Sort == "created" || p.Sort == "relevance" {
			p.Order = "desc"
		} else {
			p.Order = "asc"
		}
	}
	if p.Order != "asc" && p.Order != "desc" {
		return invalid("order must be asc or desc")
	}
	if p.View == "" {
		if search {
			p.View = "summary"
		} else {
			p.View = "full"
		}
	}
	if p.View != "summary" && p.View != "full" {
		return invalid("view must be summary or full")
	}
	if p.MaxEnrichmentAge != "" {
		d, err := time.ParseDuration(p.MaxEnrichmentAge)
		if err != nil || d <= 0 {
			return invalid("max_enrichment_age must be a positive duration")
		}
	}
	return nil
}

func signature(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode query signature: %w", err)
	}
	return hash(raw), nil
}
