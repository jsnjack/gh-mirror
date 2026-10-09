package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gh-mirror/internal/config"
	"gh-mirror/internal/store"
)

type hydrationScope struct{ fields, projects, relationships bool }

func previousScopes(ctx context.Context, w *store.Writer, c config.Config, old store.Status) (map[string]config.Scope, error) {
	raw, err := w.Metadata(ctx, "repository_options")
	if err != nil {
		return nil, err
	}
	scopes := map[string]config.Scope{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
			return nil, fmt.Errorf("decode previous repository options: %w", err)
		}
		return scopes, nil
	}
	for _, coverage := range old.Coverage {
		defaults := c.DefaultScope()
		defaults.Fields, defaults.IssueTypes = coverage.Fields != "disabled", coverage.Fields != "disabled"
		defaults.Projects = coverage.Projects != "disabled"
		scopes[coverage.Repo] = defaults
	}
	return scopes, nil
}

func pruneScope(ctx context.Context, w *store.Writer, c config.Config) error {
	owners := map[string]map[string]bool{}
	for _, repo := range c.Repositories {
		scope := c.Scope(repo)
		for kind, enabled := range map[string]bool{"labels": scope.Labels, "milestones": scope.Milestones} {
			if !enabled {
				if err := w.DeleteCatalog(ctx, kind, repo); err != nil {
					return err
				}
				if err := w.DeleteCachedPrefix(ctx, strings.TrimRight(c.APIURL, "/")+"/repos/"+repo+"/"+kind); err != nil {
					return err
				}
			}
		}
		owner := strings.Split(repo, "/")[0]
		if owners[owner] == nil {
			owners[owner] = map[string]bool{}
		}
		for kind, enabled := range map[string]bool{"issue_types": scope.IssueTypes, "issue_fields": scope.Fields, "projects": scope.Projects} {
			owners[owner][kind] = owners[owner][kind] || enabled
		}
	}
	for owner, kinds := range owners {
		for kind, enabled := range kinds {
			if enabled {
				continue
			}
			if err := w.DeleteCatalog(ctx, kind, owner); err != nil {
				return err
			}
			resource := strings.ReplaceAll(kind, "_", "-")
			if kind == "projects" {
				resource = "projectsV2"
			}
			for _, prefix := range []string{"/orgs/", "/users/"} {
				if err := w.DeleteCachedPrefix(ctx, strings.TrimRight(c.APIURL, "/")+prefix+owner+"/"+resource); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func retainIssue(ctx context.Context, w *store.Writer, repo string, raw json.RawMessage, scope config.Scope) (store.IssueRef, error) {
	object, err := store.Object(raw)
	if err != nil {
		return store.IssueRef{}, err
	}
	kind := "issue"
	if _, ok := object["pull_request"]; ok {
		kind = "pull_request"
	}
	var number int
	if err := json.Unmarshal(object["number"], &number); err != nil || number < 1 {
		return store.IssueRef{}, fmt.Errorf("invalid issue inventory number")
	}
	if err := w.ObserveKind(ctx, repo, number, kind); err != nil {
		return store.IssueRef{}, err
	}
	if !scope.Includes(kind) {
		return store.IssueRef{Repo: repo, Number: number, Kind: kind}, nil
	}
	return w.PutIssue(ctx, repo, raw)
}

func includeComment(scope config.Scope, kind string, review bool) bool {
	if review {
		return kind == "pull_request" && scope.PullRequestReviewComments
	}
	return scope.Comments(kind)
}
