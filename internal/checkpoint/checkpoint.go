// Package checkpoint durably retains completed fetches for an interrupted sync.
package checkpoint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store keeps one compatible sync session outside the published mirror database.
type Store struct {
	db       *sql.DB
	Started  time.Time
	Resuming bool
	Saved    int
}

func database(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create checkpoint directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open checkpoint file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close checkpoint placeholder: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, fmt.Errorf("protect checkpoint file: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve checkpoint path: %w", err)
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(2000)"} {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open checkpoint SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Open resumes a matching session or replaces incompatible work while the collector owns its lock.
func Open(ctx context.Context, path, signature string, started time.Time, restart bool) (_ *Store, openErr error) {
	db, err := database(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if openErr != nil {
			if err := db.Close(); err != nil {
				openErr = errors.Join(openErr, fmt.Errorf("close failed checkpoint: %w", err))
			}
		}
	}()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS session(id INTEGER PRIMARY KEY CHECK(id=1), signature TEXT NOT NULL, started TEXT NOT NULL); CREATE TABLE IF NOT EXISTS responses(key TEXT PRIMARY KEY, body BLOB NOT NULL);`); err != nil {
		return nil, fmt.Errorf("initialize checkpoint: %w", err)
	}
	var prior, timestamp string
	err = db.QueryRowContext(ctx, "SELECT signature,started FROM session WHERE id=1").Scan(&prior, &timestamp)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read checkpoint session: %w", err)
	}
	s := &Store{db: db, Started: started}
	if prior == signature && !restart {
		s.Started, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return nil, fmt.Errorf("parse checkpoint start time: %w", err)
		}
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM responses").Scan(&s.Saved); err != nil {
			return nil, fmt.Errorf("count saved fetches: %w", err)
		}
		s.Resuming = s.Saved > 0
		return s, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin checkpoint reset: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM responses; DELETE FROM session;"); err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO session(id,signature,started) VALUES(1,?,?)", signature, started.Format(time.RFC3339Nano))
		if err == nil {
			err = tx.Commit()
		}
		if err == nil {
			return s, nil
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback checkpoint reset: %w", rollbackErr))
		}
		return nil, fmt.Errorf("reset checkpoint session: %w", err)
	} else {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback checkpoint reset: %w", rollbackErr))
		}
		return nil, fmt.Errorf("clear checkpoint responses: %w", err)
	}
}

// Load returns a completed fetch from the active session.
func (s *Store) Load(ctx context.Context, key string) ([]byte, bool, error) {
	var body []byte
	if err := s.db.QueryRowContext(ctx, "SELECT body FROM responses WHERE key=?", key).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("load saved fetch: %w", err)
	}
	return body, true, nil
}

// Save commits a completed response even when cancellation arrives just after reception.
func (s *Store) Save(ctx context.Context, key string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, "INSERT INTO responses(key,body) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET body=excluded.body", key, body); err != nil {
		return fmt.Errorf("save completed fetch: %w", err)
	}
	return nil
}

// Close releases the checkpoint database without discarding unfinished work.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close checkpoint database: %w", err)
	}
	return nil
}

// Discard removes only the named session while the caller owns the collector lock.
func Discard(ctx context.Context, path, signature string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect completed checkpoint: %w", err)
	}
	db, err := database(path)
	if err != nil {
		return err
	}
	var current string
	readErr := db.QueryRowContext(ctx, "SELECT signature FROM session WHERE id=1").Scan(&current)
	closeErr := db.Close()
	if readErr != nil {
		return fmt.Errorf("read completed checkpoint identity: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close completed checkpoint: %w", closeErr)
	}
	if current != signature {
		return nil
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove completed checkpoint: %w", err)
		}
	}
	return nil
}
