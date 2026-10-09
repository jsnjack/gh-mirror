package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"
)

// Disable SQLite's own waiting so fixtures expose competing writers immediately.
func disableSQLiteBusyWait(t *testing.T, db *Store) {
	t.Helper()
	poolSize := db.db.Stats().MaxOpenConnections
	db.db.SetMaxIdleConns(poolSize)
	var connections []*sql.Conn
	defer func() {
		for _, conn := range connections {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	for range poolSize {
		conn, err := db.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		if _, err := conn.ExecContext(context.Background(), "PRAGMA busy_timeout=0"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpdateQueue(t *testing.T) {
	for _, name := range []string{"commit", "rollback", "canceled waiter"} {
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			disableSQLiteBusyWait(t, db)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			entered := make(chan struct{})
			release := make(chan struct{})
			held := make(chan error, 1)
			failure := errors.New("fixture rollback")
			go func() {
				held <- db.Update(ctx, func(w *Writer) error {
					if err := w.SetMetadata(ctx, "held", "uncommitted"); err != nil {
						return err
					}
					close(entered)
					<-release
					if name == "rollback" {
						return failure
					}
					return nil
				})
			}()
			defer func() {
				close(release)
				if err := <-held; name == "rollback" {
					if !errors.Is(err, failure) {
						t.Error("held transaction did not roll back", err)
					}
				} else if err != nil {
					t.Error("held transaction failed", err)
				}
			}()
			select {
			case <-entered:
			case err := <-held:
				// Return the result for the deferred join.
				held <- err
				t.Fatal("writer did not acquire transaction", err)
			case <-ctx.Done():
				t.Fatal("writer did not start", ctx.Err())
			}
			if name == "canceled waiter" {
				waiting, stop := context.WithTimeout(ctx, 50*time.Millisecond)
				defer stop()
				err := db.Update(waiting, func(*Writer) error {
					t.Error("canceled writer callback ran")
					return nil
				})
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("queued writer did not wait for cancellation", err)
				}
				return
			}
			const writers = 16
			started := make(chan struct{}, writers)
			done := make(chan error, writers)
			for range writers {
				go func() {
					started <- struct{}{}
					done <- db.Update(ctx, func(w *Writer) error {
						value, err := w.Metadata(ctx, "queued")
						if err != nil {
							return err
						}
						count := 0
						if value != "" {
							count, err = strconv.Atoi(value)
							if err != nil {
								return fmt.Errorf("parse fixture counter: %w", err)
							}
						}
						return w.SetMetadata(ctx, "queued", strconv.Itoa(count+1))
					})
				}()
			}
			for range writers {
				<-started
			}
			if _, err := db.Status(ctx); err != nil {
				t.Error("queued writers blocked a reader", err)
			}
			select {
			case err := <-done:
				done <- err
				t.Error("queued writer returned before transaction released", err)
			case <-time.After(50 * time.Millisecond):
			}
			// Release before joining the queued commits; the defer joins the holder.
			release <- struct{}{}
			for range writers {
				if err := <-done; err != nil {
					t.Error("queued write failed", err)
				}
			}
			var count string
			if err := db.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='queued'").Scan(&count); err != nil || count != strconv.Itoa(writers) {
				t.Error("queued commits were lost", count, err)
			}
		})
	}
}
