// Package collect builds and incrementally refreshes a transactional GitHub mirror.
package collect

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gh-mirror/internal/checkpoint"
	"gh-mirror/internal/config"
	"gh-mirror/internal/diagnostics"
	"gh-mirror/internal/github"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

// Result reports committed state and the number of upstream HTTP attempts.
type Result struct {
	Status   store.Status `json:"status"`
	Requests int          `json:"requests"`
	Resumed  int          `json:"resumed_responses"`
}

// Options controls full collection, pending work, and activity reporting.
type Options struct {
	Full     bool
	Restart  bool
	Progress progress.Reporter
}

// Sync collects complete listings initially and overlapping deltas thereafter.
func Sync(ctx context.Context, db *store.Store, c config.Config, options Options) (Result, error) {
	return syncAt(ctx, db, c, options, time.Now().UTC())
}
func syncAt(ctx context.Context, db *store.Store, c config.Config, options Options, started time.Time) (Result, error) {
	full, report := options.Full, options.Progress
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
	var pending *checkpoint.Store
	var signature string
	pendingPath := db.Path + ".sync.sqlite"
	err = db.Update(ctx, func(w *store.Writer) error {
		old, err := w.Status(ctx)
		if err != nil {
			return fmt.Errorf("load collector checkpoints: %w", err)
		}
		signature, err = sessionSignature(c, repos, old.Generation, full, os.Getenv(c.TokenEnv))
		if err != nil {
			return err
		}
		pending, err = checkpoint.Open(ctx, pendingPath, signature, started, options.Restart)
		if err != nil {
			return fmt.Errorf("open pending sync: %w", err)
		}
		started = pending.Started
		if pending.Resuming {
			report.Send(progress.Event{Phase: "Resuming interrupted sync", Saved: pending.Saved})
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
		client.Checkpoint = pending
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
			client.Report(progress.Event{Phase: "Fetching issues and comments", Scope: repo, Repository: repository + 1, Repositories: len(repos)})
			type listing struct {
				index   int
				records []json.RawMessage
			}
			lists := [2][]json.RawMessage{}
			paths := []string{"/repos/" + repo + "/issues?" + query.Encode(), "/repos/" + repo + "/issues/comments?" + commentQuery.Encode()}
			resources := []string{progress.FetchingIssues, progress.FetchingComments}
			err := parallelFetch(ctx, c.Workers, len(paths), func(ctx context.Context, index int) (listing, error) {
				items, err := client.ListWithProgress(ctx, paths[index], progress.Event{Resource: resources[index], Scope: repo})
				if err != nil {
					return listing{}, fmt.Errorf("collect %s for %s: %w", resources[index], repo, err)
				}
				return listing{index, items}, nil
			}, func(result listing) error { lists[result.index] = result.records; return nil })
			if err != nil {
				return err
			}
			issues, comments := lists[0], lists[1]
			seenIssues := map[int]bool{}
			changed := []store.IssueRef{}
			client.Report(progress.Event{Phase: "Indexing issues", Scope: repo, Total: len(issues)})
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
					client.Report(progress.Event{Completed: i + 1, Total: len(issues)})
				}
			}
			knownIssues := seenIssues
			if !inventory {
				knownIssues = map[int]bool{}
				refs, err := w.IssueRefs(ctx, repo)
				if err != nil {
					return fmt.Errorf("read comment parent inventory: %w", err)
				}
				for _, ref := range refs {
					knownIssues[ref.Number] = true
				}
			}
			seenComments := map[string]bool{}
			client.Report(progress.Event{Phase: "Indexing comments", Scope: repo, Total: len(comments)})
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
				if !knownIssues[number] {
					client.Report(progress.Event{Phase: "Recovering comment parent", Scope: fmt.Sprintf("%s#%d", repo, number)})
					ref, err := recoverIssue(ctx, client, w, repo, number)
					if err != nil {
						return fmt.Errorf("recover parent of comment %s: %w", store.Identity(o, "id"), err)
					}
					knownIssues[number], seenIssues[number] = true, true
					if ref.Changed {
						changed = append(changed, ref)
					}
					client.Report(progress.Event{Phase: "Indexing comments", Scope: repo, Total: len(comments), Completed: i})
				}
				if err := w.PutComment(ctx, repo, number, raw); err != nil {
					return fmt.Errorf("store repository comment: %w", err)
				}
				seenComments[store.Identity(o, "id")] = true
				if (i+1)%100 == 0 || i+1 == len(comments) {
					client.Report(progress.Event{Completed: i + 1, Total: len(comments)})
				}
			}
			if inventory {
				client.Report(progress.Event{Phase: "Reconciling inventories", Scope: repo})
				if err := w.Reconcile(ctx, repo, seenIssues, seenComments); err != nil {
					return fmt.Errorf("reconcile %s: %w", repo, err)
				}
				previous.ReconciledAt = started.Format(time.RFC3339Nano)
			}
			if refresh {
				owner := strings.Split(repo, "/")[0]
				organization, known := ownerTypes[owner]
				if !known {
					client.Report(progress.Event{Phase: "Reading repository owner", Scope: repo})
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
			client.Report(progress.Event{Phase: "Hydrating issue metadata", Total: total})
		}
		for _, group := range []struct {
			refs   []store.IssueRef
			fields bool
		}{{withFields, true}, {withoutFields, false}} {
			if err := hydrate(ctx, client, w, group.refs, group.fields, c.Projects, c.Workers); err != nil {
				return fmt.Errorf("hydrate ticket batches: %w", err)
			}
		}
		client.Report(progress.Event{Phase: "Committing mirror"})
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
		result.Requests, result.Resumed = client.Requests(), client.Resumed()
		result.Status, err = w.Status(ctx)
		if err != nil {
			return fmt.Errorf("read collected mirror status: %w", err)
		}
		return nil
	})
	if pending != nil {
		if closeErr := pending.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}
	if err != nil {
		return result, fmt.Errorf("synchronize GitHub mirror: %w", err)
	}
	// Cleanup takes the collector lock again: another process may already own a newer session.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := db.Update(cleanupCtx, func(*store.Writer) error { return checkpoint.Discard(cleanupCtx, pendingPath, signature) }); err != nil {
		slog.Log(ctx, diagnostics.TraceLevel, "retain completed sync checkpoint", "error", err)
	}
	return result, nil
}

func recoverIssue(ctx context.Context, c *github.Client, w *store.Writer, repo string, number int) (store.IssueRef, error) {
	raw, err := c.Get(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, number))
	if err != nil {
		return store.IssueRef{}, fmt.Errorf("fetch missing issue %s#%d: %w", repo, number, err)
	}
	object, err := store.Object(raw)
	if err != nil {
		return store.IssueRef{}, fmt.Errorf("decode missing issue: %w", err)
	}
	if store.Identity(object, "number") != strconv.Itoa(number) {
		return store.IssueRef{}, fmt.Errorf("missing issue %s#%d returned a different issue number", repo, number)
	}
	ref, err := w.PutIssue(ctx, repo, raw)
	if err != nil {
		return ref, fmt.Errorf("store recovered issue %s#%d: %w", repo, number, err)
	}
	return ref, nil
}

func due(timestamp string, now time.Time, interval time.Duration) bool {
	if timestamp == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339Nano, timestamp)
	return err != nil || now.Sub(last) >= interval
}
func catalog(ctx context.Context, c *github.Client, w *store.Writer, kind, scope, path string) error {
	c.Report(progress.Event{Phase: "Refreshing " + strings.ReplaceAll(kind, "_", " "), Scope: scope})
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
	c.Report(progress.Event{Phase: "Listing projects", Scope: owner})
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
		c.Report(progress.Event{Phase: "Fetching project fields", Scope: scope})
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
