package service

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gh-mirror/internal/store"
)

func badInput(format string, args ...any) error {
	return fmt.Errorf("%w: %s", store.ErrInvalidQuery, fmt.Sprintf(format, args...))
}
func boolParam(q url.Values, key string) (bool, error) {
	v := q.Get(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, badInput("%s must be true or false", key)
	}
	return b, nil
}
func intParam(q url.Values, key string, defaultValue int) (int, error) {
	v := q.Get(key)
	if v == "" {
		return defaultValue, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, badInput("%s must be an integer", key)
	}
	return n, nil
}
func queryParams(q url.Values) (store.QueryFilters, store.PageOptions, error) {
	f := store.QueryFilters{Repositories: q["repositories"], LabelsAll: q["labels_all"], LabelsAny: q["labels_any"], ExcludeLabels: q["exclude_labels"], Author: q.Get("author"), Assignees: q["assignees"], Milestone: q.Get("milestone"), CreatedAfter: q.Get("created_after"), CreatedBefore: q.Get("created_before"), UpdatedAfter: q.Get("updated_after"), UpdatedBefore: q.Get("updated_before")}
	if len(q["repo"]) > 1 {
		f.Repositories = append(f.Repositories, q["repo"]...)
	}
	for _, v := range q["field"] {
		name, value, ok := strings.Cut(v, "=")
		if !ok {
			return f, store.PageOptions{}, badInput("field must be name=value")
		}
		f.FieldValues = append(f.FieldValues, store.FieldFilter{Name: name, Value: value})
	}
	p := store.PageOptions{Sort: q.Get("sort"), Order: q.Get("order"), View: q.Get("view"), MaxEnrichmentAge: q.Get("max_enrichment_age")}
	var err error
	p.Count, err = boolParam(q, "count")
	if err != nil {
		return f, p, err
	}
	p.Facets, err = boolParam(q, "facets")
	return f, p, err
}
func searchParams(q url.Values) (store.SearchOptions, error) {
	f, p, err := queryParams(q)
	if err != nil {
		return store.SearchOptions{}, err
	}
	o := store.SearchOptions{Engine: q.Get("engine"), Query: q.Get("q"), Repo: q.Get("repo"), State: q.Get("state"), Label: q.Get("label"), Type: q.Get("type"), Kind: q.Get("kind"), Project: q.Get("project"), QueryFilters: f, PageOptions: p, Match: q.Get("match"), In: q["in"], ExcludeWords: q["exclude_words"], Cursor: q.Get("cursor")}
	if len(q["repo"]) > 1 {
		o.Repo = ""
	}
	o.Limit, err = intParam(q, "limit", 30)
	if err != nil {
		return o, err
	}
	o.EvidenceLimit, err = intParam(q, "evidence_limit", 3)
	if err != nil {
		return o, err
	}
	o.Prefix, err = boolParam(q, "prefix")
	return o, err
}
func listParams(q url.Values) (store.ListOptions, error) {
	f, p, err := queryParams(q)
	if err != nil {
		return store.ListOptions{}, err
	}
	o := store.ListOptions{Repo: q.Get("repo"), State: q.Get("state"), Label: q.Get("label"), Type: q.Get("type"), Kind: q.Get("kind"), Project: q.Get("project"), QueryFilters: f, PageOptions: p, Cursor: q.Get("cursor")}
	if len(q["repo"]) > 1 {
		o.Repo = ""
	}
	o.Limit, err = intParam(q, "limit", 30)
	return o, err
}
