package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gh-mirror/internal/store"
)

func TestSnapshotCompatibility(t *testing.T) {
	for _, name := range []string{"legacy manifest", "future manifest", "future embedded", "old enrichment allowed", "old enrichment rejected", "missing enrichment", "future enrichment", "negative enrichment age"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, dir := source(t)
			if name == "future embedded" {
				if err := db.Update(ctx, func(w *store.Writer) error {
					return w.SetMetadata(ctx, "collection_version", strconv.Itoa(store.CollectionVersion+1))
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := Publish(ctx, db, dir); err == nil {
					t.Fatal("published incompatible collection")
				}
				return
			}
			old := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
			if name == "old enrichment allowed" || name == "old enrichment rejected" {
				if err := db.Update(ctx, func(w *store.Writer) error { return w.SetMetadata(ctx, "enriched_at", old) }); err != nil {
					t.Fatal(err)
				}
			}
			manifest, err := Publish(ctx, db, dir)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(dir, "latest.json")
			dest := filepath.Join(t.TempDir(), "private.sqlite")
			if err := os.WriteFile(dest, []byte("previous"), 0600); err != nil {
				t.Fatal(err)
			}
			options := Options{Source: source, Destination: dest, Repositories: []string{"o/r"}, MaxAge: time.Hour}
			fail := false
			switch name {
			case "legacy manifest":
				manifest.CollectionVersion = 0
			case "future manifest":
				manifest.CollectionVersion = store.CollectionVersion + 1
				fail = true
			case "old enrichment rejected":
				options.MaxEnrichmentAge = time.Hour
				fail = true
			case "missing enrichment":
				manifest.EnrichedAt = ""
				options.MaxEnrichmentAge = time.Hour
				fail = true
			case "future enrichment":
				manifest.EnrichedAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
				options.MaxEnrichmentAge = time.Hour
				fail = true
			case "negative enrichment age":
				options.MaxEnrichmentAge = -time.Hour
				fail = true
			}
			writeManifest(t, source, manifest)
			result, err := Acquire(ctx, options)
			if fail {
				if err == nil {
					t.Fatal("accepted incompatible or stale snapshot", result)
				}
				body, err := os.ReadFile(dest)
				if err != nil || string(body) != "previous" {
					t.Fatal("failed acquisition replaced previous copy", err)
				}
				return
			}
			if err != nil || result.CollectionVersion != store.CollectionVersion {
				t.Fatal("compatible snapshot rejected", result, err)
			}
		})
	}
}
