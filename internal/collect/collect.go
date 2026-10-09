// Package collect builds and incrementally refreshes a transactional GitHub mirror.
package collect

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gh-mirror/internal/config"
	"gh-mirror/internal/github"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

// Result reports committed state and the number of upstream HTTP attempts.
type Result struct {
	Status   store.Status `json:"status"`
	Requests int          `json:"requests"`
}

// Sync collects complete listings initially and overlapping deltas thereafter.
func Sync(ctx context.Context, db *store.Store, c config.Config, full bool, report progress.Reporter) (Result, error) {
	return syncAt(ctx, db, c, full, time.Now().UTC(), report)
}
func syncAt(ctx context.Context, db *store.Store, c config.Config, full bool, started time.Time, report progress.Reporter) (Result, error) {
	var result Result
	report.Send(progress.Event{Phase: "Checking checkpoints", Limit: c.MaxRequests})
	if err := c.Validate(); err != nil {
		return result, fmt.Errorf("validate collector configuration: %w", err)
	}
	if len(c.Repositories) == 0 {
		return result, fmt.Errorf("configure at least one repository before collection")
	}
	overlap, err := time.ParseDuration(c.Overlap)
	if err != nil {
		return result, fmt.Errorf("parse overlap: %w", err)
	}
	reconcile, err := time.ParseDuration(c.ReconcileInterval)
	if err != nil {
		return result, fmt.Errorf("parse reconciliation interval: %w", err)
	}
	enrichment, err := time.ParseDuration(c.EnrichmentInterval)
	if err != nil {
		return result, fmt.Errorf("parse enrichment interval: %w", err)
	}
	repos := append([]string{}, c.Repositories...)
	sort.Strings(repos)
	scope, err := json.Marshal(repos)
	if err != nil {
		return result, fmt.Errorf("encode scope: %w", err)
	}
	err = db.Update(ctx, func(w *store.Writer) error {
		old, err := w.Status(ctx)
		if err != nil {
			return fmt.Errorf("load collector checkpoints: %w", err)
		}
		prior, err := json.Marshal(old.Repositories)
		if err != nil {
			return fmt.Errorf("encode previous scope: %w", err)
		}
		scopeChanged := string(prior) != string(scope) || old.Upstream != strings.TrimRight(c.APIURL, "/")
		refresh := full || scopeChanged || due(old.EnrichedAt, started, enrichment)
		// Feature changes require rehydration even inside the normal refresh interval.
		for _, coverage := range old.Coverage {
			if (coverage.Fields == "disabled") != (!c.Fields) || (coverage.Projects == "disabled") != (!c.Projects) {
				refresh = true
			}
		}
		if err := w.ResetScope(ctx, repos, refresh, old.Upstream != strings.TrimRight(c.APIURL, "/")); err != nil {
			return fmt.Errorf("reset collection scope: %w", err)
		}
		client := github.New(c.APIURL, c.GraphQLURL, os.Getenv(c.TokenEnv), c.MaxRequests, c.Workers, w)
		client.Progress = report
		owners := map[string]bool{}
		ownerTypes := map[string]bool{}
		withFields, withoutFields := []store.IssueRef{}, []store.IssueRef{}
		for repository, repo := range repos {
			previous := store.Coverage{Repo: repo, Fields: "disabled", Projects: "disabled"}
			for _, coverage := range old.Coverage {
				if coverage.Repo == repo {
					previous = coverage
				}
			}
			inventory := full || scopeChanged || due(previous.ReconciledAt, started, reconcile)
			query := url.Values{"state": {"all"}, "per_page": {"100"}, "sort": {"updated"}, "direction": {"asc"}}
			commentQuery := url.Values{"per_page": {"100"}, "sort": {"updated"}, "direction": {"asc"}}
			if !inventory {
				checkpoint, err := time.Parse(time.RFC3339Nano, previous.CollectedAt)
				if err != nil {
					return fmt.Errorf("parse checkpoint: %w", err)
				}
				since := checkpoint.Add(-overlap).Format(time.RFC3339)
				query.Set("since", since)
				commentQuery.Set("since", since)
			}
			report.Send(progress.Event{Phase: progress.FetchingIssues, Scope: repo, Repository: repository + 1, Repositories: len(repos)})
			issues, err := client.List(ctx, "/repos/"+repo+"/issues?"+query.Encode())
			if err != nil {
				return fmt.Errorf("collect issues for %s: %w", repo, err)
			}
			seenIssues := map[int]bool{}
			changed := []store.IssueRef{}
			report.Send(progress.Event{Phase: "Indexing issues", Scope: repo, Total: len(issues)})
			for i, raw := range issues {
				ref, err := w.PutIssue(ctx, repo, raw)
				if err != nil {
					return fmt.Errorf("store issue for %s: %w", repo, err)
				}
				seenIssues[ref.Number] = true
				if ref.Changed {
					changed = append(changed, ref)
				}
				if (i+1)%100 == 0 || i+1 == len(issues) {
					report.Send(progress.Event{Completed: i + 1, Total: len(issues)})
				}
			}
			report.Send(progress.Event{Phase: progress.FetchingComments, Scope: repo})
			comments, err := client.List(ctx, "/repos/"+repo+"/issues/comments?"+commentQuery.Encode())
			if err != nil {
				return fmt.Errorf("collect comments for %s: %w", repo, err)
			}
			seenComments := map[string]bool{}
			report.Send(progress.Event{Phase: "Indexing comments", Scope: repo, Total: len(comments)})
			for i, raw := range comments {
				o, err := store.Object(raw)
				if err != nil {
					return fmt.Errorf("decode repository comment: %w", err)
				}
				issueURL, err := url.Parse(store.Text(o, "issue_url"))
				if err != nil {
					return fmt.Errorf("parse comment issue URL: %w", err)
				}
				marker := "/repos/" + repo + "/issues/"
				at := strings.LastIndex(strings.ToLower(issueURL.Path), strings.ToLower(marker))
				if at < 0 {
					return fmt.Errorf("comment refers outside repository %s", repo)
				}
				number, err := strconv.Atoi(issueURL.Path[at+len(marker):])
				if err != nil || number < 1 {
					return fmt.Errorf("invalid comment issue number")
				}
				if err := w.PutComment(ctx, repo, number, raw); err != nil {
					return fmt.Errorf("store repository comment: %w", err)
				}
				seenComments[store.Identity(o, "id")] = true
				if (i+1)%100 == 0 || i+1 == len(comments) {
					report.Send(progress.Event{Completed: i + 1, Total: len(comments)})
				}
			}
			if inventory {
				report.Send(progress.Event{Phase: "Reconciling inventories", Scope: repo})
				if err := w.Reconcile(ctx, repo, seenIssues, seenComments); err != nil {
					return fmt.Errorf("reconcile %s: %w", repo, err)
				}
				previous.ReconciledAt = started.Format(time.RFC3339Nano)
			}
			if refresh {
				owner := strings.Split(repo, "/")[0]
				organization, known := ownerTypes[owner]
				if !known {
					report.Send(progress.Event{Phase: "Reading repository owner", Scope: repo})
					raw, err := client.Get(ctx, "/repos/"+repo)
					if err != nil {
						return fmt.Errorf("read repository owner: %w", err)
					}
					object, err := store.Object(raw)
					if err != nil {
						return fmt.Errorf("decode repository metadata: %w", err)
					}
					ownerObject, err := store.Object(object["owner"])
					if err != nil {
						return fmt.Errorf("decode repository owner: %w", err)
					}
					organization = store.Text(ownerObject, "type") == "Organization"
					if !organization && store.Text(ownerObject, "type") != "User" {
						return fmt.Errorf("unsupported repository owner type")
					}
					ownerTypes[owner] = organization
				}
				for kind, path := range map[string]string{"labels": "/labels?per_page=100", "milestones": "/milestones?state=all&per_page=100"} {
					if err := catalog(ctx, client, w, kind, repo, "/repos/"+repo+path); err != nil {
						return err
					}
				}
				if !owners[owner] {
					if c.Fields && organization {
						for kind, path := range map[string]string{"issue_types": "/issue-types?per_page=100", "issue_fields": "/issue-fields?per_page=100"} {
							if err := catalog(ctx, client, w, kind, owner, "/orgs/"+owner+path); err != nil {
								return err
							}
						}
					}
					if c.Projects {
						if err := projects(ctx, client, w, owner, organization); err != nil {
							return fmt.Errorf("collect owner projects: %w", err)
						}
					}
					owners[owner] = true
				}
				refs, err := w.IssueRefs(ctx, repo)
				if err != nil {
					return fmt.Errorf("read hydration inventory: %w", err)
				}
				if c.Fields && organization {
					withFields = append(withFields, refs...)
				} else {
					withoutFields = append(withoutFields, refs...)
				}
				previous.Fields = "disabled"
				if c.Fields {
					previous.Fields = "complete"
					if !organization {
						previous.Fields = "not_applicable"
					}
				}
				previous.Projects = "disabled"
				if c.Projects {
					previous.Projects = "complete"
				}
			}
			if !refresh && len(changed) > 0 {
				if c.Fields && previous.Fields == "complete" {
					withFields = append(withFields, changed...)
				} else {
					withoutFields = append(withoutFields, changed...)
				}
			}
			previous.CollectedAt = started.Format(time.RFC3339Nano)
			if err := w.SetCoverage(ctx, previous); err != nil {
				return fmt.Errorf("checkpoint %s: %w", repo, err)
			}
			slog.DebugContext(ctx, "collected repository", "repo", repo, "issues", len(issues), "comments", len(comments), "full", inventory, "enrichment", refresh)
		}
		if total := len(withFields) + len(withoutFields); total > 0 {
			report.Send(progress.Event{Phase: "Hydrating issue metadata", Total: total})
		}
		for _, group := range []struct {
			refs   []store.IssueRef
			fields bool
		}{{withFields, true}, {withoutFields, false}} {
			if err := hydrate(ctx, client, w, group.refs, group.fields, c.Projects); err != nil {
				return fmt.Errorf("hydrate ticket batches: %w", err)
			}
		}
		report.Send(progress.Event{Phase: "Committing mirror"})
		if refresh {
			if err := w.SetMetadata(ctx, "enriched_at", started.Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
		for key, value := range map[string]string{"upstream": strings.TrimRight(c.APIURL, "/"), "repositories": string(scope), "generation": rand.Text(), "collected_at": started.Format(time.RFC3339Nano)} {
			if err := w.SetMetadata(ctx, key, value); err != nil {
				return err
			}
		}
		result.Requests = client.Requests()
		result.Status, err = w.Status(ctx)
		if err != nil {
			return fmt.Errorf("read collected mirror status: %w", err)
		}
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("synchronize GitHub mirror: %w", err)
	}
	return result, nil
}
func due(timestamp string, now time.Time, interval time.Duration) bool {
	if timestamp == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339Nano, timestamp)
	return err != nil || now.Sub(last) >= interval
}
func catalog(ctx context.Context, c *github.Client, w *store.Writer, kind, scope, path string) error {
	c.Progress.Send(progress.Event{Phase: "Refreshing " + strings.ReplaceAll(kind, "_", " "), Scope: scope})
	items, err := c.List(ctx, path)
	if err != nil {
		return fmt.Errorf("collect %s catalog: %w", kind, err)
	}
	if err := w.ReplaceCatalog(ctx, kind, scope, items); err != nil {
		return fmt.Errorf("replace %s catalog: %w", kind, err)
	}
	return nil
}
func projects(ctx context.Context, c *github.Client, w *store.Writer, owner string, organization bool) error {
	c.Progress.Send(progress.Event{Phase: "Listing projects", Scope: owner})
	prefix := "/users/"
	if organization {
		prefix = "/orgs/"
	}
	base := prefix + owner + "/projectsV2"
	items, err := c.List(ctx, base+"?per_page=100")
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	if err := w.ReplaceCatalog(ctx, "projects", owner, items); err != nil {
		return fmt.Errorf("store project catalog: %w", err)
	}
	for _, raw := range items {
		o, err := store.Object(raw)
		if err != nil {
			return fmt.Errorf("decode project: %w", err)
		}
		number := store.Identity(o, "number")
		if n, err := strconv.Atoi(number); err != nil || n < 1 {
			return fmt.Errorf("invalid project number")
		}
		scope := owner + "/" + number
		c.Progress.Send(progress.Event{Phase: "Fetching project fields", Scope: scope})
		fields, err := c.List(ctx, base+"/"+number+"/fields?per_page=100")
		if err != nil {
			return fmt.Errorf("list project fields: %w", err)
		}
		if err := w.ReplaceCatalog(ctx, "project_fields", scope, fields); err != nil {
			return fmt.Errorf("store project fields: %w", err)
		}
		ids := []string{}
		for _, field := range fields {
			object, err := store.Object(field)
			if err != nil {
				return fmt.Errorf("decode project field: %w", err)
			}
			id := store.Identity(object, "id")
			if id == "" {
				return fmt.Errorf("project field is missing id")
			}
			ids = append(ids, id)
		}
		query := url.Values{"per_page": {"100"}}
		if len(ids) > 0 {
			query.Set("fields", strings.Join(ids, ","))
		}
		all, err := projectItems(ctx, c, base+"/"+number, store.Text(o, "node_id"), query)
		if err != nil {
			return fmt.Errorf("collect project items: %w", err)
		}
		if err := w.ReplaceCatalog(ctx, "project_items", scope, all); err != nil {
			return fmt.Errorf("store project items: %w", err)
		}
	}
	return nil
}
