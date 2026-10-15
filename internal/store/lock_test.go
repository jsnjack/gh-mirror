package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestWriterBusy(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "idle mirror"
		if held {
			name = "held write lock"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, err := Open(filepath.Join(t.TempDir(), "mirror.sqlite"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			}()
			if held {
				locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() {
					done <- db.Update(ctx, func(*Writer) error {
						close(locked)
						<-release
						return nil
					})
				}()
				<-locked
				defer func() {
					close(release)
					if err := <-done; err != nil {
						t.Error(err)
					}
				}()
			}
			begin := time.Now()
			busy, err := WriterBusy(ctx, db.Path)
			if err != nil || busy != held {
				t.Fatalf("busy %v, %v; want %v", busy, err, held)
			}
			if elapsed := time.Since(begin); elapsed > time.Second {
				t.Fatalf("lock probe waited %s", elapsed)
			}
			if !held {
				return
			}
			other, err := Open(db.Path, false)
			if err == nil {
				if closeErr := other.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}
			if !IsBusy(err) {
				t.Fatalf("second writer error is not busy: %v", err)
			}
			if IsBusy(errors.New("database is locked")) {
				t.Fatal("plain error classified as busy")
			}
		})
	}
}

func TestCollectionAge(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name, collectedAt string
		age               time.Duration
		collected, fails  bool
	}{
		{name: "never collected"},
		{name: "collected", collectedAt: now.Add(-23 * time.Minute).Format(time.RFC3339Nano), age: 23 * time.Minute, collected: true},
		{name: "unreadable", collectedAt: "yesterday", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			age, collected, err := Status{CollectedAt: tc.collectedAt}.CollectionAge(now)
			if (err != nil) != tc.fails || age != tc.age || collected != tc.collected {
				t.Fatalf("got %s, %v, %v", age, collected, err)
			}
		})
	}
}
