package collect

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"gh-mirror/internal/store"
)

func TestLegacyLabelBackfill(t *testing.T) {
	t.Run("index unchanged tickets without additional requests", func(t *testing.T) {
		ctx := context.Background()
		db, c, f, started := setup(t, 2)
		initial, err := syncAt(ctx, db, c, Options{}, started)
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := sql.Open("sqlite", db.Path)
		if err != nil {
			t.Fatal(err)
		}
		_, editErr := legacy.ExecContext(ctx, "DELETE FROM documents WHERE id LIKE 'labels:%'; DELETE FROM metadata WHERE key='label_index_version'")
		closeErr := legacy.Close()
		if editErr != nil || closeErr != nil {
			t.Fatal("simulate old search index", editErr, closeErr)
		}
		f.stage = "delta"
		result, err := syncAt(ctx, db, c, Options{}, started.Add(10*time.Minute))
		if err != nil || result.Requests != 2 || result.Status.EnrichedAt != initial.Status.EnrichedAt || result.Status.CollectionVersion != initial.Status.CollectionVersion {
			t.Fatal("label backfill forced upstream collection", result, err)
		}
		found, err := db.Search(ctx, store.SearchOptions{Query: "bug"})
		if err != nil || len(found.Matches) != 2 {
			t.Fatal("unchanged tickets were not reindexed", found, err)
		}
	})
}
