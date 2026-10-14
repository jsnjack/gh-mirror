package collect

import (
	"context"
	"strings"
	"testing"
	"time"

	"gh-mirror/internal/checkpoint"
)

func TestRemovedTicketRecovery(t *testing.T) {
	t.Run("deleted issue schedules reconciliation instead of failing", func(t *testing.T) {
		ctx := context.Background()
		db, c, f, started := setup(t, 3)
		if _, err := syncAt(ctx, db, c, Options{}, started); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.gone, f.stage = map[int]bool{1: true}, "delta"
		f.mu.Unlock()
		enriched, err := syncAt(ctx, db, c, Options{}, started.Add(2*time.Hour))
		if err != nil {
			t.Fatal("deleted upstream issue blocked enrichment", err)
		}
		if enriched.Status.Issues != 3 || enriched.Status.Coverage[0].ReconciledAt != "" {
			t.Fatal("deleted issue did not schedule reconciliation", enriched.Status)
		}
		f.mu.Lock()
		f.stage = ""
		f.mu.Unlock()
		reconciled, err := syncAt(ctx, db, c, Options{}, started.Add(3*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if reconciled.Status.Issues != 2 || reconciled.Status.Coverage[0].ReconciledAt == "" {
			t.Fatal("reconciliation kept the deleted issue", reconciled.Status)
		}
	})
	t.Run("failed session reconciles when the interval elapses", func(t *testing.T) {
		ctx := context.Background()
		db, c, f, started := setup(t, 2)
		initial, err := syncAt(ctx, db, c, Options{}, started)
		if err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.stage = "fail"
		f.mu.Unlock()
		if _, err := syncAt(ctx, db, c, Options{}, started.Add(2*time.Hour)); err == nil {
			t.Fatal("failing upstream committed")
		}
		pending, found, err := checkpoint.Inspect(ctx, db.Path+".sync.sqlite")
		if err != nil || !found || pending.Failures != 1 || !strings.Contains(pending.LastError, "HTTP 403") || !pending.Started.Equal(started.Add(2*time.Hour)) {
			t.Fatal("failed attempt not recorded for status", pending, found, err)
		}
		f.mu.Lock()
		f.stage = ""
		f.mu.Unlock()
		result, err := syncAt(ctx, db, c, Options{}, started.Add(30*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if result.Status.Coverage[0].ReconciledAt == initial.Status.Coverage[0].ReconciledAt || result.Status.CollectedAt != started.Add(2*time.Hour).Format(time.RFC3339Nano) {
			t.Fatal("resumed session measured intervals from its stale start", result.Status)
		}
	})
}
