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
	"slices"
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
	// Changes lists every configured repository, including those with nothing changed.
	Changes map[string]store.Changes `json:"changes"`
}

// Options controls full collection, pending work, and activity reporting.
type Options struct {
	Full    bool
	Restart bool
	// StaleAfter, when positive, makes Sync return ErrFresh without collecting if
	// the committed collection is younger; the check runs under the collector lock.
	StaleAfter time.Duration
	Progress   progress.Reporter
}

// Sync collects complete listings initially and overlapping deltas thereafter.
func Sync(ctx context.Context, db *store.Store, c config.Config, options Options) (Result, error) {
	return syncAt(ctx, db, c, options, time.Now().UTC())
}
func syncAt(ctx context.Context, db *store.Store, c config.Config, options Options, started time.Time) (Result, error) {
	return syncVersion(ctx, db, c, options, started, store.CollectionVersion)
}

// syncVersion receives the current time as started; a resumed session replaces it with
// the original watermark while interval checks keep using the current time.
func syncVersion(ctx context.Context, db *store.Store, c config.Config, options Options, started time.Time, version int) (Result, error) {
	full, report := options.Full, options.Progress
	now := started
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
	token := os.Getenv(c.TokenEnv)
	identity := credentialIdentity(c, token)
	pendingPath := checkpointPath(db.Path)
	err = db.Update(ctx, func(w *store.Writer) error {
		old, err := w.Status(ctx)
		if err != nil {
			return fmt.Errorf("load collector checkpoints: %w", err)
		}
		if options.StaleAfter > 0 {
			if age, collected, err := old.CollectionAge(now); err == nil && collected && age < options.StaleAfter {
				result.Status = old
				return ErrFresh
			}
		}
		incompatible := old.Generation != "" && old.CollectionVersion != version
		priorIdentity, err := w.Metadata(ctx, "credential_identity")
		if err != nil {
			return fmt.Errorf("read credential scope: %w", err)
		}
		credentialsChanged := old.Generation != "" && priorIdentity != identity
		if credentialsChanged {
			full = true
			report.Send(progress.Event{Phase: "Credential scope changed or unknown; full sync required"})
		}
		if incompatible {
			full = true
			report.Send(progress.Event{Phase: "Collection version changed; full sync required", Scope: fmt.Sprintf("v%d -> v%d", old.CollectionVersion, version)})
			slog.DebugContext(ctx, "rebuild incompatible collection", "previous", old.CollectionVersion, "current", version)
		}
		signature, err = sessionSignature(c, repos, old.Generation, full, token, version)
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
		refresh := full || scopeChanged || due(old.EnrichedAt, now, enrichment)
		scopes := c.Scopes()
		priorScopes, err := previousScopes(ctx, w, c, old)
		if err != nil {
			return fmt.Errorf("read prior repository scope: %w", err)
		}
		scopesJSON, err := json.Marshal(scopes)
		if err != nil {
			return fmt.Errorf("encode repository options: %w", err)
		}
		if err := w.ResetScope(ctx, repos, refresh, incompatible || credentialsChanged || old.Upstream != strings.TrimRight(c.APIURL, "/")); err != nil {
			return fmt.Errorf("reset collection scope: %w", err)
		}
		if err := w.PruneResponseCache(ctx, true, true); err != nil {
			return fmt.Errorf("remove excluded cached resources: %w", err)
		}
		if err := pruneScope(ctx, w, c); err != nil {
			return fmt.Errorf("prune repository scope: %w", err)
		}
		client := github.New(c.APIURL, c.GraphQLURL, token, c.MaxRequests, c.Workers, w)
		client.Progress = report
		client.Checkpoint = pending
		owners := map[string]bool{}
		ownerTypes := map[string]bool{}
		groups := map[hydrationScope][]store.IssueRef{}
		coverages := map[string]store.Coverage{}
		unreconciled := map[string]bool{}
		for repository, repo := range repos {
			scope := scopes[repo]
			priorScope, knownScope := priorScopes[repo]
			changedScope := knownScope && priorScope != scope
			changedInventory := knownScope && (priorScope.Issues != scope.Issues || priorScope.PullRequests != scope.PullRequests || priorScope.IssueComments != scope.IssueComments || priorScope.PullRequestComments != scope.PullRequestComments || priorScope.PullRequestReviewComments != scope.PullRequestReviewComments)
			repoRefresh := refresh || changedScope
			projectCoverage := "disabled"
			if scope.Projects {
				projectCoverage = "memberships"
			}
			if changedScope {
				if err := w.ClearExtras(ctx, repo); err != nil {
					return err
				}
			}
			previous := store.Coverage{Repo: repo, Fields: "disabled", Projects: "disabled"}
			for _, coverage := range old.Coverage {
				if coverage.Repo == repo {
					previous = coverage
				}
			}
			inventory := full || scopeChanged || changedInventory || due(previous.ReconciledAt, now, reconcile)
			// Creation order stays fixed when records are edited, so numbered pages do not shift
			// during or between page fetches; since still filters by update time.
			query := url.Values{"state": {"all"}, "per_page": {"100"}, "sort": {"created"}, "direction": {"asc"}}
			commentQuery := url.Values{"per_page": {"100"}, "sort": {"created"}, "direction": {"asc"}}
			if !inventory {
				checkpoint, err := time.Parse(time.RFC3339Nano, previous.CollectedAt)
				if err != nil {
					return fmt.Errorf("parse checkpoint: %w", err)
				}
				since := checkpoint.Add(-overlap).Format(time.RFC3339)
				query.Set("since", since)
				commentQuery.Set("since", since)
			}
			if inventory {
				if err := w.ResetInventory(ctx, repo); err != nil {
					return err
				}
			}
			client.Report(progress.Event{Phase: "Fetching issues and comments", Scope: repo, Repository: repository + 1, Repositories: len(repos)})
			type listing struct {
				index   int
				records []json.RawMessage
			}
			lists := [3][]json.RawMessage{}
			paths := []string{"/repos/" + repo + "/issues?" + query.Encode(), "/repos/" + repo + "/issues/comments?" + commentQuery.Encode(), "/repos/" + repo + "/pulls/comments?" + commentQuery.Encode()}
			resources := []string{progress.FetchingIssues, progress.FetchingComments, progress.FetchingReviewComments}
			err := parallelFetch(ctx, c.Workers, len(paths), func(ctx context.Context, index int) (listing, error) {
				if (index == 1 && !scope.IssueComments && !scope.PullRequestComments) || (index == 2 && !scope.PullRequestReviewComments) {
					return listing{index: index, records: []json.RawMessage{}}, nil
				}
				items, err := client.ListWithProgress(ctx, paths[index], progress.Event{Resource: resources[index], Scope: repo})
				if err != nil {
					return listing{}, fmt.Errorf("collect %s for %s: %w", resources[index], repo, err)
				}
				return listing{index, items}, nil
			}, func(result listing) error { lists[result.index] = result.records; return nil })
			if err != nil {
				return err
			}
			issues, comments := lists[0], append(lists[1], lists[2]...)
			seenIssues := map[int]bool{}
			changed := []store.IssueRef{}
			client.Report(progress.Event{Phase: "Indexing issues", Scope: repo, Total: len(issues)})
			for i, raw := range issues {
				ref, err := retainIssue(ctx, w, repo, raw, scope)
				if err != nil {
					return fmt.Errorf("store issue for %s: %w", repo, err)
				}
				if !scope.Includes(ref.Kind) {
					continue
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
				isReview := store.Text(o, "pull_request_url") != ""
				parentURL := store.Text(o, "issue_url")
				if isReview {
					parentURL = store.Text(o, "pull_request_url")
				}
				issueURL, err := url.Parse(parentURL)
				if err != nil {
					return fmt.Errorf("parse comment issue URL: %w", err)
				}
				marker := "/repos/" + repo + "/issues/"
				if isReview {
					marker = "/repos/" + repo + "/pulls/"
				}
				at := strings.LastIndex(strings.ToLower(issueURL.Path), strings.ToLower(marker))
				if at < 0 {
					return fmt.Errorf("comment refers outside repository %s", repo)
				}
				number, err := strconv.Atoi(issueURL.Path[at+len(marker):])
				if err != nil || number < 1 {
					return fmt.Errorf("invalid comment issue number")
				}
				kind, err := w.KnownKind(ctx, repo, number)
				if err != nil {
					return err
				}
				if kind != "" && !includeComment(scope, kind, isReview) {
					continue
				}
				if !knownIssues[number] {
					client.Report(progress.Event{Phase: "Recovering comment parent", Scope: fmt.Sprintf("%s#%d", repo, number)})
					ref, err := recoverIssue(ctx, client, w, repo, number, scope)
					if err != nil {
						return fmt.Errorf("recover parent of comment %s: %w", store.Identity(o, "id"), err)
					}
					if !includeComment(scope, ref.Kind, isReview) {
						continue
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
				commentID := store.Identity(o, "id")
				if isReview {
					commentID = "review:" + commentID
				}
				seenComments[commentID] = true
				if (i+1)%100 == 0 || i+1 == len(comments) {
					client.Report(progress.Event{Completed: i + 1, Total: len(comments)})
				}
			}
			if inventory {
				confirmed, err := confirmMissing(ctx, client, w, repo, scope, c.Workers, seenIssues)
				if err != nil {
					return err
				}
				changed = append(changed, confirmed...)
				client.Report(progress.Event{Phase: "Reconciling inventories", Scope: repo})
				if err := w.Reconcile(ctx, repo, seenIssues, seenComments); err != nil {
					return fmt.Errorf("reconcile %s: %w", repo, err)
				}
				previous.ReconciledAt = started.Format(time.RFC3339Nano)
			}
			if repoRefresh {
				owner := strings.Split(repo, "/")[0]
				organization, known := ownerTypes[owner]
				if !known && (scope.Fields || scope.IssueTypes || scope.Projects) {
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
					if (kind == "labels" && !scope.Labels) || (kind == "milestones" && !scope.Milestones) {
						continue
					}
					var prior store.CatalogResult
					if kind == "labels" && !inventory {
						// The committed mirror still holds the previous catalog; this transaction already reset it.
						if prior, err = db.Catalog(ctx, kind, repo); err != nil {
							return fmt.Errorf("read previous label catalog: %w", err)
						}
					}
					items, err := catalog(ctx, client, w, kind, repo, "/repos/"+repo+path)
					if err != nil {
						return err
					}
					if kind == "labels" && !inventory {
						relabeled, absent, err := refreshRelabeled(ctx, db, client, w, repo, scope, c.Workers, prior.Items, items)
						if err != nil {
							return err
						}
						changed = append(changed, relabeled...)
						unreconciled[repo] = unreconciled[repo] || absent
					}
				}
				for kind, enabled := range map[string]bool{"issue_types": scope.IssueTypes, "issue_fields": scope.Fields, "projects": scope.Projects} {
					key := owner + ":" + kind
					if !enabled || owners[key] || (!organization && kind != "projects") {
						continue
					}
					if kind == "projects" {
						if err := projects(ctx, client, w, owner, organization); err != nil {
							return err
						}
					} else {
						resource := strings.ReplaceAll(kind, "_", "-")
						if _, err := catalog(ctx, client, w, kind, owner, "/orgs/"+owner+"/"+resource+"?per_page=100"); err != nil {
							return err
						}
					}
					owners[key] = true
				}
				refs, err := w.IssueRefs(ctx, repo)
				if err != nil {
					return fmt.Errorf("read hydration inventory: %w", err)
				}
				key := hydrationScope{scope.Fields && organization, scope.Projects, scope.Relationships}
				if key.fields || key.projects || key.relationships {
					groups[key] = append(groups[key], refs...)
				}
				previous.Fields = "disabled"
				if scope.Fields {
					previous.Fields = "complete"
					if !organization {
						previous.Fields = "not_applicable"
					}
				}
			}
			if !repoRefresh && len(changed) > 0 {
				key := hydrationScope{scope.Fields && previous.Fields == "complete", scope.Projects, scope.Relationships}
				if key.fields || key.projects || key.relationships {
					groups[key] = append(groups[key], changed...)
				}
			}
			previous.Projects = projectCoverage
			previous.CollectedAt = started.Format(time.RFC3339Nano)
			coverages[repo] = previous
			slog.DebugContext(ctx, "collected repository", "repo", repo, "issues", len(issues), "comments", len(comments), "full", inventory, "enrichment", repoRefresh)
		}
		total := 0
		for _, refs := range groups {
			total += len(refs)
		}
		if total > 0 {
			client.Report(progress.Event{Phase: "Hydrating issue metadata", Total: total})
		}
		for _, fields := range []bool{true, false} {
			for _, projects := range []bool{true, false} {
				for _, relationships := range []bool{true, false} {
					key := hydrationScope{fields, projects, relationships}
					missing, err := hydrate(ctx, client, w, groups[key], fields, projects, relationships, c.Workers)
					if err != nil {
						return fmt.Errorf("hydrate ticket batches: %w", err)
					}
					for _, ref := range missing {
						unreconciled[ref.Repo] = true
					}
				}
			}
		}
		for _, repo := range repos {
			coverage := coverages[repo]
			if unreconciled[repo] {
				// The next sync lists the repository completely and removes confirmed absences.
				coverage.ReconciledAt = ""
				slog.DebugContext(ctx, "schedule reconciliation for removed tickets", "repo", repo)
			}
			if err := w.SetCoverage(ctx, coverage); err != nil {
				return fmt.Errorf("checkpoint %s: %w", repo, err)
			}
		}
		client.Report(progress.Event{Phase: "Applying PR comment policy"})
		if err := w.ApplyCommentPolicies(ctx, c.CommentPolicies()); err != nil {
			return fmt.Errorf("apply PR comment policy: %w", err)
		}
		client.Report(progress.Event{Phase: "Committing mirror"})
		if refresh {
			if err := w.SetMetadata(ctx, "enriched_at", started.Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
		for key, value := range map[string]string{"upstream": strings.TrimRight(c.APIURL, "/"), "repositories": string(scope), "generation": rand.Text(), "collected_at": started.Format(time.RFC3339Nano), "collection_version": strconv.Itoa(version), "credential_identity": identity, "repository_options": string(scopesJSON)} {
			if err := w.SetMetadata(ctx, key, value); err != nil {
				return err
			}
		}
		result.Requests, result.Resumed = client.Requests(), client.Resumed()
		changed := w.Changes()
		result.Changes = make(map[string]store.Changes, len(repos))
		for _, repo := range repos {
			result.Changes[repo] = changed[repo]
		}
		result.Status, err = w.Status(ctx)
		if err != nil {
			return fmt.Errorf("read collected mirror status: %w", err)
		}
		return nil
	})
	if pending != nil {
		if err != nil && ctx.Err() == nil {
			if recordErr := pending.RecordFailure(ctx, err, now); recordErr != nil {
				slog.Log(ctx, diagnostics.TraceLevel, "record failed sync attempt", "error", recordErr)
			}
		}
		if closeErr := pending.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}
	if err != nil {
		return result, fmt.Errorf("synchronize GitHub mirror: %w", err)
	}
	report.Send(progress.Event{Changes: committedChanges(repos, result.Changes)})
	// Cleanup takes the collector lock again: another process may already own a newer session.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := db.Update(cleanupCtx, func(*store.Writer) error { return checkpoint.Discard(cleanupCtx, pendingPath, signature) }); err != nil {
		slog.Log(ctx, diagnostics.TraceLevel, "retain completed sync checkpoint", "error", err)
	}
	return result, nil
}

func recoverIssue(ctx context.Context, c *github.Client, w *store.Writer, repo string, number int, scope config.Scope) (store.IssueRef, error) {
	raw, found, err := fetchIssue(ctx, c, repo, number)
	if err != nil {
		return store.IssueRef{}, err
	}
	if !found {
		return store.IssueRef{}, fmt.Errorf("missing issue %s#%d is inaccessible", repo, number)
	}
	ref, err := retainIssue(ctx, w, repo, raw, scope)
	if err != nil {
		return ref, fmt.Errorf("store recovered issue %s#%d: %w", repo, number, err)
	}
	return ref, nil
}

func fetchIssue(ctx context.Context, c *github.Client, repo string, number int) (json.RawMessage, bool, error) {
	raw, found, err := c.Lookup(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, number))
	if err != nil {
		return nil, false, fmt.Errorf("fetch issue %s#%d: %w", repo, number, err)
	}
	if !found {
		return nil, false, nil
	}
	object, err := store.Object(raw)
	if err != nil {
		return nil, false, fmt.Errorf("decode issue %s#%d: %w", repo, number, err)
	}
	if store.Identity(object, "number") != strconv.Itoa(number) {
		return nil, false, fmt.Errorf("issue %s#%d returned a different issue number", repo, number)
	}
	return raw, true, nil
}

// refetchIssues retains live issues and reports whether any were confirmed absent.
func refetchIssues(ctx context.Context, c *github.Client, w *store.Writer, repo string, scope config.Scope, workers int, numbers []int, seen map[int]bool) ([]store.IssueRef, bool, error) {
	changed := []store.IssueRef{}
	absent := false
	err := parallelFetch(ctx, workers, len(numbers), func(ctx context.Context, index int) (json.RawMessage, error) {
		raw, _, err := fetchIssue(ctx, c, repo, numbers[index])
		return raw, err
	}, func(raw json.RawMessage) error {
		if raw == nil {
			absent = true
			return nil
		}
		ref, err := retainIssue(ctx, w, repo, raw, scope)
		if err != nil {
			return fmt.Errorf("store refetched issue for %s: %w", repo, err)
		}
		if !scope.Includes(ref.Kind) {
			return nil
		}
		if seen != nil {
			seen[ref.Number] = true
		}
		if ref.Changed {
			changed = append(changed, ref)
		}
		return nil
	})
	return changed, absent, err
}

// confirmMissing keeps stored issues that a complete listing skipped but GitHub still serves.
func confirmMissing(ctx context.Context, c *github.Client, w *store.Writer, repo string, scope config.Scope, workers int, seen map[int]bool) ([]store.IssueRef, error) {
	refs, err := w.IssueRefs(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("read reconciliation inventory: %w", err)
	}
	missing := []int{}
	for _, ref := range refs {
		if !seen[ref.Number] && scope.Includes(ref.Kind) {
			missing = append(missing, ref.Number)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	c.Report(progress.Event{Phase: "Confirming missing issues", Scope: repo, Total: len(missing)})
	changed, _, err := refetchIssues(ctx, c, w, repo, scope, workers, missing, seen)
	if err != nil {
		return nil, fmt.Errorf("confirm issues missing from %s listing: %w", repo, err)
	}
	return changed, nil
}

// refreshRelabeled refetches issues carrying renamed or deleted labels, which GitHub
// changes without advancing issue updated_at.
func refreshRelabeled(ctx context.Context, db *store.Store, c *github.Client, w *store.Writer, repo string, scope config.Scope, workers int, prior, current []json.RawMessage) ([]store.IssueRef, bool, error) {
	names := map[string]string{}
	for _, raw := range current {
		object, err := store.Object(raw)
		if err != nil {
			return nil, false, fmt.Errorf("decode label catalog: %w", err)
		}
		names[store.Identity(object, "id")] = store.Text(object, "name")
	}
	stale := []string{}
	for _, raw := range prior {
		object, err := store.Object(raw)
		if err != nil {
			return nil, false, fmt.Errorf("decode previous label catalog: %w", err)
		}
		if name := store.Text(object, "name"); name != "" && names[store.Identity(object, "id")] != name {
			stale = append(stale, name)
		}
	}
	if len(stale) == 0 {
		return nil, false, nil
	}
	numbers := []int{}
	// Listing filters accept at most 100 label names per request.
	for start := 0; start < len(stale); start += 100 {
		cursor := ""
		for {
			page, err := db.List(ctx, store.ListOptions{Repo: repo, QueryFilters: store.QueryFilters{LabelsAny: stale[start:min(start+100, len(stale))]}, Limit: 100, Cursor: cursor})
			if err != nil {
				return nil, false, fmt.Errorf("find issues with changed labels: %w", err)
			}
			for _, ticket := range page.Issues {
				numbers = append(numbers, ticket.Number)
			}
			if !page.HasMore {
				break
			}
			cursor = page.NextCursor
		}
	}
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	c.Report(progress.Event{Phase: "Refreshing relabeled issues", Scope: repo, Total: len(numbers)})
	changed, absent, err := refetchIssues(ctx, c, w, repo, scope, workers, numbers, nil)
	if err != nil {
		return nil, false, fmt.Errorf("refresh issues with changed labels in %s: %w", repo, err)
	}
	return changed, absent, nil
}

func due(timestamp string, now time.Time, interval time.Duration) bool {
	if timestamp == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339Nano, timestamp)
	return err != nil || now.Sub(last) >= interval
}
func catalog(ctx context.Context, c *github.Client, w *store.Writer, kind, scope, path string) ([]json.RawMessage, error) {
	c.Report(progress.Event{Phase: "Refreshing " + strings.ReplaceAll(kind, "_", " "), Scope: scope})
	items, err := c.List(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("collect %s catalog: %w", kind, err)
	}
	if err := w.ReplaceCatalog(ctx, kind, scope, items); err != nil {
		return nil, fmt.Errorf("replace %s catalog: %w", kind, err)
	}
	return items, nil
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
	return nil
}

// committedChanges orders tallies by repository for the completion summary.
func committedChanges(repos []string, changes map[string]store.Changes) []progress.RepositoryChanges {
	tally := func(c store.ChangeCount) progress.Tally {
		return progress.Tally{Added: c.Added, Updated: c.Updated, Removed: c.Removed}
	}
	out := make([]progress.RepositoryChanges, 0, len(repos))
	for _, repo := range repos {
		c := changes[repo]
		out = append(out, progress.RepositoryChanges{Repo: repo, Issues: tally(c.Issues), PullRequests: tally(c.PullRequests), Comments: tally(c.Comments)})
	}
	return out
}
