package collect

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gh-mirror/internal/checkpoint"
	"gh-mirror/internal/store"
)

func TestConditionalSync(t *testing.T) {
	for _, tc := range []struct {
		name       string
		collect    bool
		staleAfter time.Duration
		fresh      bool
	}{
		{name: "fresh mirror skips under the collector lock", collect: true, staleAfter: time.Hour, fresh: true},
		{name: "stale mirror collects", collect: true, staleAfter: time.Second},
		{name: "uncollected mirror collects", staleAfter: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, c, f, started := setup(t, 2)
			ctx := context.Background()
			if tc.collect {
				if _, err := syncAt(ctx, db, c, Options{}, started); err != nil {
					t.Fatal(err)
				}
			}
			f.mu.Lock()
			f.requests = 0
			f.mu.Unlock()
			if tc.fresh {
				// A fresh mirror skips before the token check, so readers need no token.
				t.Setenv(c.TokenEnv, "")
			}
			out, err := Sync(ctx, db, c, Options{StaleAfter: tc.staleAfter})
			f.mu.Lock()
			requests := f.requests
			f.mu.Unlock()
			if tc.fresh {
				if !errors.Is(err, ErrFresh) || requests != 0 || out.Status.CollectedAt != started.Format(time.RFC3339Nano) {
					t.Fatalf("fresh mirror collected: %v, %d requests, %+v", err, requests, out.Status)
				}
				return
			}
			if err != nil || requests == 0 {
				t.Fatalf("stale mirror skipped: %v, %d requests", err, requests)
			}
		})
	}
}

func TestCheckFreshness(t *testing.T) {
	for _, tc := range []struct {
		name             string
		missing, collect bool
		session, lock    bool
		staleAfter       time.Duration
		want             string
	}{
		{name: "missing database is stale", missing: true, staleAfter: time.Hour},
		{name: "uncollected mirror is stale", staleAfter: time.Hour},
		{name: "recent collection is fresh", collect: true, staleAfter: time.Hour, want: SkipFresh},
		{name: "old collection is stale", collect: true, staleAfter: time.Second},
		{name: "collector holding the lock with a session is running", collect: true, session: true, lock: true, staleAfter: time.Second, want: SkipRunning},
		{name: "interrupted session without a collector is stale", collect: true, session: true, staleAfter: time.Second},
		{name: "local writer without a session is stale", collect: true, lock: true, staleAfter: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, c, _, started := setup(t, 2)
			ctx := context.Background()
			if tc.collect {
				if _, err := syncAt(ctx, db, c, Options{}, started); err != nil {
					t.Fatal(err)
				}
			}
			if tc.session {
				pending, err := checkpoint.Open(ctx, checkpointPath(c.Database), "interrupted", started, false)
				if err != nil {
					t.Fatal(err)
				}
				if err := pending.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.lock {
				held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() {
					done <- db.Update(ctx, func(*store.Writer) error {
						close(held)
						<-release
						return nil
					})
				}()
				<-held
				defer func() {
					close(release)
					if err := <-done; err != nil {
						t.Error(err)
					}
				}()
			}
			path := c.Database
			if tc.missing {
				path = filepath.Join(t.TempDir(), "absent.sqlite")
			}
			begin := time.Now()
			got, err := CheckFreshness(ctx, path, tc.staleAfter, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if got.Skip != tc.want {
				t.Fatalf("skip %q, want %q: %+v", got.Skip, tc.want, got)
			}
			if elapsed := time.Since(begin); elapsed > time.Second {
				t.Fatalf("freshness check waited %s", elapsed)
			}
			if tc.collect != got.Collected || (tc.collect && got.CollectedAt != started.Format(time.RFC3339Nano)) {
				t.Fatalf("collection time not reported: %+v", got)
			}
		})
	}
}
