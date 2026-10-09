package collect

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gh-mirror/internal/checkpoint"
	"gh-mirror/internal/config"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

func TestCollectionCompatibility(t *testing.T) {
	for _, name := range []string{"compatible", "upgrade", "downgrade", "incompatible pending work", "cancelled upgrade", "failed upgrade", "budget exhausted"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, c, f, started := setup(t, 2)
			c.Workers = 1
			initialVersion, target := store.CollectionVersion, store.CollectionVersion+1
			if name == "compatible" {
				target = initialVersion
			}
			if name == "downgrade" {
				initialVersion, target = target, initialVersion
			}
			initial, err := syncVersion(ctx, db, c, Options{}, started, initialVersion)
			if err != nil {
				t.Fatal(err)
			}
			upgradeAt := started.Add(10 * time.Minute)
			f.stage, f.timestamp = "delete", upgradeAt.Format(time.RFC3339)
			if name == "compatible" {
				f.stage = "delta"
			}
			query := url.Values{"state": {"all"}, "per_page": {"100"}, "sort": {"updated"}, "direction": {"asc"}}
			listing := c.APIURL + "/repos/o/r/issues?" + query.Encode()
			if err := db.Update(ctx, func(w *store.Writer) error {
				return w.Cache(ctx, listing, "obsolete-etag", []byte(`{"body":[],"link":""}`))
			}); err != nil {
				t.Fatal(err)
			}
			if name == "incompatible pending work" {
				signature, err := sessionSignature(c, c.Repositories, initial.Status.Generation, true, os.Getenv(c.TokenEnv), initialVersion)
				if err != nil {
					t.Fatal(err)
				}
				pending, err := checkpoint.Open(ctx, db.Path+".sync.sqlite", signature, started.Add(time.Minute), false)
				if err != nil {
					t.Fatal(err)
				}
				if err := pending.Save(ctx, "GET:"+listing, []byte(`{"body":[],"link":""}`)); err != nil {
					t.Fatal(err)
				}
				if err := pending.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var phases []string
			options := Options{Progress: func(event progress.Event) { phases = append(phases, event.Phase) }}
			interrupted := name == "cancelled upgrade" || name == "failed upgrade" || name == "budget exhausted"
			saved := 0
			if interrupted {
				stopCtx, cancel := context.WithCancel(ctx)
				options.Progress = func(event progress.Event) {
					if name == "cancelled upgrade" && event.Resource == progress.FetchingIssues && event.Page == 1 {
						cancel()
					}
				}
				f.failGraph = name == "failed upgrade"
				if name == "budget exhausted" {
					c.MaxRequests = 1
				}
				_, err := syncVersion(stopCtx, db, c, options, upgradeAt, target)
				cancel()
				if err == nil || (name == "cancelled upgrade" && !errors.Is(err, context.Canceled)) {
					t.Fatal("upgrade did not stop", err)
				}
				after, err := db.Status(ctx)
				if err != nil || after.Generation != initial.Status.Generation || after.CollectionVersion != initialVersion || after.Issues != 2 || after.Comments != 1 {
					t.Fatal("partial upgrade replaced the published mirror", after, err)
				}
				signature, err := sessionSignature(c, c.Repositories, initial.Status.Generation, true, os.Getenv(c.TokenEnv), target)
				if err != nil {
					t.Fatal(err)
				}
				pending, err := checkpoint.Open(ctx, db.Path+".sync.sqlite", signature, upgradeAt.Add(time.Minute), false)
				if err != nil {
					t.Fatal(err)
				}
				saved = pending.Saved
				if saved < 1 || !pending.Started.Equal(upgradeAt) {
					t.Fatal("upgrade lost completed fetches", saved, pending.Started)
				}
				if err := pending.Close(); err != nil {
					t.Fatal(err)
				}
				f.failGraph, c.MaxRequests = false, 3000
				options.Progress = func(event progress.Event) { phases = append(phases, event.Phase) }
			}
			invokedAt := upgradeAt
			if interrupted {
				invokedAt = upgradeAt.Add(5 * time.Minute)
			}
			result, err := syncVersion(ctx, db, c, options, invokedAt, target)
			if err != nil {
				t.Fatal(err)
			}
			requests, issues, comments := 10-saved, 1, 0
			if name == "compatible" {
				requests, issues, comments = 2, 2, 1
			}
			if result.Requests != requests || result.Resumed != saved || result.Status.CollectionVersion != target || result.Status.Issues != issues || result.Status.Comments != comments || result.Status.CollectedAt != upgradeAt.Format(time.RFC3339Nano) {
				t.Fatal("incorrect rebuild or recovery", result, saved)
			}
			changed := strings.Contains(strings.Join(phases, "\n"), "Collection version changed")
			if changed != (name != "compatible") {
				t.Fatal("upgrade reason missing or compatible sync rebuilt", phases)
			}
			if name == "compatible" {
				if result.Status.EnrichedAt != initial.Status.EnrichedAt {
					t.Fatal("compatible change forced enrichment", result.Status)
				}
			} else {
				labels, err := db.Catalog(ctx, "labels", "o/r")
				if err != nil || len(labels.Items) != 1 || !strings.Contains(string(labels.Items[0]), "replacement") || result.Status.EnrichedAt != upgradeAt.Format(time.RFC3339Nano) {
					t.Fatal("upgrade skipped catalog enrichment", labels, err)
				}
				if err := db.Update(ctx, func(w *store.Writer) error {
					etag, _, err := w.Cached(ctx, listing)
					if err != nil || etag != "" {
						t.Fatal("obsolete conditional response survived the upgrade", etag, err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(db.Path + ".sync.sqlite"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed upgrade retained its checkpoint", err)
			}
			f.stage = "delta"
			next, err := syncVersion(ctx, db, c, Options{}, invokedAt.Add(time.Minute), target)
			if err != nil || next.Requests != 2 || next.Resumed != 0 || next.Status.CollectionVersion != target {
				t.Fatal("completed upgrade did not return to incremental updates", next, err)
			}
		})
	}
}

func TestSessionCollectionVersion(t *testing.T) {
	for _, name := range []string{"legacy fingerprint", "incompatible version"} {
		t.Run(name, func(t *testing.T) {
			c := config.Config{APIURL: "https://api.github.com", GraphQLURL: "https://api.github.com/graphql", Fields: true, Projects: true, Overlap: "5m", EnrichmentInterval: "1h", ReconcileInterval: "24h"}
			signature, err := sessionSignature(c, []string{"o/r"}, "generation", false, "test-token", store.LegacyCollectionVersion)
			if err != nil {
				t.Fatal(err)
			}
			if name == "legacy fingerprint" {
				if signature != "60805497f84adf9da02f302ec837ab7c33d4d1b76774cca597f176a15ecfc0ee" {
					t.Fatal("compatible checkpoints were invalidated", signature)
				}
			} else {
				upgraded, err := sessionSignature(c, []string{"o/r"}, "generation", false, "test-token", store.LegacyCollectionVersion+1)
				if err != nil || upgraded == signature {
					t.Fatal("incompatible checkpoint stayed reusable", upgraded, err)
				}
			}
		})
	}
}
