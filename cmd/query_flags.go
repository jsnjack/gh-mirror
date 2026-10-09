package cmd

import (
	"fmt"
	"gh-mirror/internal/store"
	"strings"

	"github.com/spf13/cobra"
)

func queryFlags(command *cobra.Command, f *store.QueryFilters, p *store.PageOptions) {
	var fields []string
	flags := command.Flags()
	flags.StringArrayVar(&fields, "field", nil, "Require collected field name=value; repeat")
	run := command.RunE
	command.RunE = func(command *cobra.Command, args []string) error {
		f.FieldValues = nil
		for _, raw := range fields {
			name, value, ok := strings.Cut(raw, "=")
			if !ok || name == "" {
				return fmt.Errorf("--field requires name=value")
			}
			f.FieldValues = append(f.FieldValues, store.FieldFilter{Name: name, Value: value})
		}
		return run(command, args)
	}
	flags.StringArrayVar(&f.Repositories, "repositories", nil, "Include repository owner/name; repeat for several")
	flags.StringArrayVar(&f.LabelsAll, "labels-all", nil, "Require each exact label; repeat")
	flags.StringArrayVar(&f.LabelsAny, "labels-any", nil, "Require any exact label; repeat")
	flags.StringArrayVar(&f.ExcludeLabels, "exclude-label", nil, "Exclude an exact label; repeat")
	flags.StringVar(&f.Author, "author", "", "Filter ticket author")
	flags.StringArrayVar(&f.Assignees, "assignee", nil, "Filter any assignee; repeat")
	flags.StringVar(&f.Milestone, "milestone", "", "Filter milestone title or number")
	flags.StringVar(&f.CreatedAfter, "created-after", "", "Inclusive RFC3339 lower creation bound")
	flags.StringVar(&f.CreatedBefore, "created-before", "", "Inclusive RFC3339 upper creation bound")
	flags.StringVar(&f.UpdatedAfter, "updated-after", "", "Inclusive RFC3339 lower update bound")
	flags.StringVar(&f.UpdatedBefore, "updated-before", "", "Inclusive RFC3339 upper update bound")
	flags.StringVar(&p.Sort, "sort", "", "Sort by relevance (search), number, updated or created")
	flags.StringVar(&p.Order, "order", "", "Sort direction asc or desc")
	flags.StringVar(&p.View, "view", "", "Return summary or full raw records")
	flags.BoolVar(&p.Count, "count", false, "Count all matches before pagination")
	flags.BoolVar(&p.Facets, "facets", false, "Count matching labels, types and projects")
	flags.StringVar(&p.MaxEnrichmentAge, "max-enrichment-age", "", "Warn when requested metadata exceeds this age (default 24h)")
}
