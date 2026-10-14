package cmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"gh-mirror/internal/config"
	"gh-mirror/internal/diagnostics"
	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// flagValues completes fixed flag values wherever a command defines the flag.
var flagValues = map[string][]string{
	"state":             {"open", "closed"},
	"kind":              {"issue", "pull_request"},
	"engine":            {"hybrid", "lexical", "semantic"},
	"view":              {"summary", "full"},
	"purpose":           {"evaluate", "classification", "duplicates", "proofread"},
	"sort":              {"relevance", "number", "updated", "created"},
	"order":             {"asc", "desc"},
	"match":             {"any", "all", "phrase"},
	"in":                {"title", "body", "labels", "comments", "reviews"},
	"comments":          {"none", "page", "all"},
	"comment-kind":      {"discussion", "review"},
	"format":            {"auto", "text", "json"},
	"embedding-backend": {"cpu", "lemonade-vulkan"},
	"tools":             {"core", "all"},
	"mcp-tools":         {"core", "all"},
}

var repoFlags = []string{"repo", "repositories"}
var labelFlags = []string{"labels-all", "labels-any", "exclude-label", "label"}

func completionCommand(command *cobra.Command) bool {
	for c := command; c != nil; c = c.Parent() {
		if c.Name() == cobra.ShellCompRequestCmd || c.Name() == cobra.ShellCompNoDescRequestCmd || c.Name() == "completion" {
			return true
		}
	}
	return false
}

func registerCompletion(command *cobra.Command, flag string, values ...string) {
	if err := command.RegisterFlagCompletionFunc(flag, cobra.FixedCompletions(values, cobra.ShellCompDirectiveNoFileComp)); err != nil {
		panic(err)
	}
}

// addCompletions registers value completion for every command after all flags exist.
func addCompletions() {
	registerCompletion(root, "format", flagValues["format"]...)
	var walk func(*cobra.Command)
	walk = func(command *cobra.Command) {
		command.Flags().VisitAll(func(flag *pflag.Flag) {
			if _, ok := command.GetFlagCompletionFunc(flag.Name); ok {
				return
			}
			switch {
			case slices.Contains(repoFlags, flag.Name):
				if err := command.RegisterFlagCompletionFunc(flag.Name, completeRepositories); err != nil {
					panic(err)
				}
			case slices.Contains(labelFlags, flag.Name):
				if err := command.RegisterFlagCompletionFunc(flag.Name, completeLabels); err != nil {
					panic(err)
				}
			case flagValues[flag.Name] != nil:
				registerCompletion(command, flag.Name, flagValues[flag.Name]...)
			}
		})
		for _, child := range command.Commands() {
			walk(child)
		}
	}
	for _, command := range root.Commands() {
		walk(command)
	}
}

// completionSettings loads configuration without failing completion when it is absent or invalid.
func completionSettings() config.Config {
	path := configPath
	if path == "" {
		var err error
		if path, err = config.DefaultPath(); err != nil {
			return config.Config{}
		}
	}
	c, err := config.Load(path, configPath != "")
	if err != nil {
		return config.Config{}
	}
	if database != "" {
		c.Database = database
	}
	return c
}

func completeRepositories(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return completionSettings().Repositories, cobra.ShellCompDirectiveNoFileComp
}

func completeTicketRefs(_ *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	out := []cobra.Completion{}
	for _, repo := range completionSettings().Repositories {
		out = append(out, repo+"#")
	}
	return out, cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveNoFileComp
}

func completeCatalog(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return catalogKinds, cobra.ShellCompDirectiveNoFileComp
	case 1:
		out := []cobra.Completion{}
		for _, repo := range completionSettings().Repositories {
			if slices.Contains(ownerCatalogs, args[0]) {
				repo, _, _ = strings.Cut(repo, "/")
			}
			if !slices.Contains(out, repo) {
				out = append(out, repo)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// completeLabels reads label names from the local catalog; a missing database yields no suggestions.
func completeLabels(command *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	c := completionSettings()
	repos := c.Repositories
	if flag := command.Flags().Lookup("repo"); flag != nil && flag.Value.String() != "" && config.ValidRepository(flag.Value.String()) {
		repos = []string{flag.Value.String()}
	}
	if c.Database == "" || len(repos) == 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	db, err := store.Open(c.Database, true)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx := command.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	defer closeStore(ctx, db)
	out := []cobra.Completion{}
	for _, repo := range repos {
		catalog, err := db.Catalog(ctx, "labels", repo)
		if err != nil {
			slog.Log(ctx, diagnostics.TraceLevel, "complete labels", "repo", repo, "error", err)
			continue
		}
		for _, raw := range catalog.Items {
			var label struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &label) == nil && label.Name != "" && !slices.Contains(out, label.Name) {
				out = append(out, label.Name)
			}
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
