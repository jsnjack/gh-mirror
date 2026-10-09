package collect

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gh-mirror/internal/checkpoint"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

func TestResumeSync(t *testing.T) {
	for _, name := range []string{"REST page", "metadata batch", "restart", "token change", "scope change", "budget exhausted", "incremental comment"} {
		t.Run(name, func(t *testing.T) {
			db, c, f, started := setup(t, 251)
			ctx := context.Background()
			c.Workers = 1
			var old store.Status
			if name == "incremental comment" {
				initial, err := syncAt(ctx, db, c, Options{}, started)
				if err != nil {
					t.Fatal(err)
				}
				old = initial.Status
				started = started.Add(10 * time.Minute)
				f.stage, f.timestamp = "delta", started.Format(time.RFC3339)
			}
			interrupted, cancel := context.WithCancel(ctx)
			options := Options{Progress: func(event progress.Event) {
				if name == "metadata batch" {
					if event.Advance > 0 {
						cancel()
					}
				} else if event.Resource == progress.FetchingIssues && event.Page == 1 {
					cancel()
				}
			}}
			if name == "budget exhausted" {
				c.MaxRequests = 1
				options.Progress = nil
			}
			_, err := syncAt(interrupted, db, c, options, started)
			cancel()
			if err == nil || (name != "budget exhausted" && !errors.Is(err, context.Canceled)) {
				t.Fatal("sync did not stop", err)
			}
			after, err := db.Status(ctx)
			if err != nil || after.Generation != old.Generation || after.Issues != old.Issues {
				t.Fatal("partial data became visible", after, err)
			}
			signature, err := sessionSignature(c, c.Repositories, old.Generation, false, os.Getenv(c.TokenEnv))
			if err != nil {
				t.Fatal(err)
			}
			pending, err := checkpoint.Open(ctx, db.Path+".sync.sqlite", signature, started.Add(time.Hour), false)
			if err != nil {
				t.Fatal(err)
			}
			saved := pending.Saved
			if saved < 1 || !pending.Started.Equal(started) {
				t.Fatal("interruption lost completed fetches", saved, pending.Started)
			}
			if name == "metadata batch" && saved < 15 {
				t.Fatal("metadata batch was not saved", saved)
			}
			if err := pending.Close(); err != nil {
				t.Fatal(err)
			}
			// Reopen both databases: recovery must survive a process boundary.
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(c.Database, false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			}()
			c.Workers, c.MaxRequests = 4, 3000
			options = Options{}
			switch name {
			case "restart":
				options.Restart = true
			case "token change":
				t.Setenv(c.TokenEnv, "different-test-credential")
			case "scope change":
				c.Fields = false
			}
			completedAt := started.Add(20 * time.Minute)
			result, err := syncAt(ctx, db, c, options, completedAt)
			if err != nil {
				t.Fatal(err)
			}
			fresh := name == "restart" || name == "token change" || name == "scope change"
			if fresh {
				if result.Resumed != 0 || result.Status.CollectedAt != completedAt.Format(time.RFC3339Nano) {
					t.Fatal("incompatible work was reused", result)
				}
			} else {
				total := 20
				if name == "incremental comment" {
					total = 2
				}
				if result.Resumed != saved || result.Requests != total-saved || result.Status.CollectedAt != started.Format(time.RFC3339Nano) {
					t.Fatal("recovery refetched saved work or advanced the watermark", saved, result)
				}
			}
			if result.Status.Issues != 251 || result.Status.Comments != 1 || result.Status.Generation == old.Generation {
				t.Fatal("incomplete recovery", result)
			}
			issue, err := db.Get(ctx, "o/r", 1)
			if err != nil || len(issue.Comments) != 1 {
				t.Fatal("recovery lost comments", issue, err)
			}
			if name == "incremental comment" && !strings.Contains(string(issue.Comments[0]), "editedcomment") {
				t.Fatal("lost incremental comment edit", issue.Comments)
			}
			if _, err := os.Stat(db.Path + ".sync.sqlite"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed checkpoint retained", err)
			}
			f.stage = "delta"
			next, err := syncAt(ctx, db, c, Options{}, completedAt.Add(time.Minute))
			if err != nil || next.Resumed != 0 || next.Requests != 2 {
				t.Fatal("next delta reused completed session", next, err)
			}
		})
	}
}
