package collect

import (
	"context"
	"testing"
	"time"
)

func TestInvalidObservationRecovery(t *testing.T) {
	for _, name := range []string{"missing node", "repeated field cursor", "repeated membership cursor"} {
		t.Run(name, func(t *testing.T) {
			db, c, f, started := setup(t, 2)
			c.Workers = 1
			switch name {
			case "missing node":
				f.missingNode = true
			case "repeated field cursor":
				f.repeatCursor = true
			case "repeated membership cursor":
				f.membershipOverflow, f.repeatMembershipCursor = true, true
			}
			ctx := context.Background()
			if _, err := syncAt(ctx, db, c, Options{}, started); err == nil {
				t.Fatal("accepted invalid metadata")
			}
			before, err := db.Status(ctx)
			if err != nil || before.Generation != "" {
				t.Fatal("failed sync became visible", before, err)
			}
			f.missingNode, f.repeatCursor, f.repeatMembershipCursor = false, false, false
			result, err := syncAt(ctx, db, c, Options{}, started.Add(time.Minute))
			if err != nil || result.Resumed == 0 || result.Status.Issues != 2 || result.Status.CollectedAt != started.Format(time.RFC3339Nano) {
				t.Fatal("invalid response trapped resume or lost progress", result, err)
			}
		})
	}
}
