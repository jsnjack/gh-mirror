package cmd

import (
	"fmt"
	"slices"
	"strings"

	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

// catalogKinds lists the catalog names the store accepts.
var catalogKinds = store.CatalogKinds()

// ownerCatalogs are collected once per repository owner rather than per repository.
var ownerCatalogs = []string{"issue_types", "issue_fields", "projects"}

// catalogScope defaults to the default repository, or its owner for owner-level catalogs.
func catalogScope(kind string) string {
	repo := settings.DefaultRepository()
	if repo == "" || !slices.Contains(ownerCatalogs, kind) {
		return repo
	}
	owner, _, _ := strings.Cut(repo, "/")
	return owner
}

func addCatalog() {
	var cursor string
	var limit int
	command := &cobra.Command{Use: "catalog KIND [SCOPE]", GroupID: groupQuery, Short: "Read a repository or owner catalog such as labels or milestones", Long: "Read a catalog. KIND is one of " + strings.Join(catalogKinds, ", ") + ".\nSCOPE is a repository for labels and milestones, or an owner for issue_types, issue_fields and projects.\nIt defaults to the default repository or its owner.", Args: cobra.MaximumNArgs(2), ValidArgsFunction: completeCatalog, RunE: func(c *cobra.Command, args []string) error {
		var k, s string
		if len(args) > 0 {
			k = args[0]
		}
		if len(args) > 1 {
			s = args[1]
		}
		if !slices.Contains(catalogKinds, k) {
			return fmt.Errorf("catalog KIND must be one of %s", strings.Join(catalogKinds, ", "))
		}
		if s == "" {
			s = catalogScope(k)
		}
		if s == "" {
			return fmt.Errorf("catalog SCOPE is required when no default repository is configured")
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(c.Context(), db)
		var out store.CatalogResult
		if c.Flags().Changed("limit") || cursor != "" {
			out, err = db.CatalogPage(c.Context(), store.CatalogOptions{Kind: k, Scope: s, Limit: limit, Cursor: cursor})
		} else {
			out, err = db.Catalog(c.Context(), k, s)
		}
		if err != nil {
			return fmt.Errorf("read local catalog: %w", err)
		}
		return output(c, out)
	}}
	command.Flags().IntVar(&limit, "limit", 30, "Return pages of this size (1–100); without --limit or --cursor the whole catalog is returned")
	command.Flags().StringVar(&cursor, "cursor", "", "Continue next_cursor")
	root.AddCommand(command)
}
