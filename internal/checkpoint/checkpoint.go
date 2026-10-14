// Package checkpoint durably retains completed fetches for an interrupted sync.
package checkpoint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gh-mirror/internal/diagnostics"

	_ "modernc.org/sqlite"
)

const schema = `CREATE TABLE IF NOT EXISTS session(id INTEGER PRIMARY KEY CHECK(id=1), signature TEXT NOT NULL, started TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS responses(key TEXT PRIMARY KEY, body BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS failures(id INTEGER PRIMARY KEY CHECK(id=1), attempts INTEGER NOT NULL, error TEXT NOT NULL, failed_at TEXT NOT NULL);`

// maxErrorBytes bounds the recorded failure text shown by status.
const maxErrorBytes = 2048

// Store keeps one compatible sync session outside the published mirror database.
type Store struct {
	db       *sql.DB
	Started  time.Time
	Resuming bool
	Saved    int
}

// Pending describes an uncommitted sync session for status reporting.
type Pending struct {
	Started   time.Time `json:"started"`
	Saved     int       `json:"saved_responses"`
	Failures  int       `json:"failures"`
	LastError string    `json:"last_error,omitempty"`
	FailedAt  time.Time `json:"failed_at,omitzero"`
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
	if _, err := db.ExecContext(ctx, schema); err != nil {
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
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			openErr = errors.Join(openErr, fmt.Errorf("rollback checkpoint reset: %w", err))
		}
	}()
	if _, err := tx.ExecContext(ctx, "DELETE FROM responses; DELETE FROM session; DELETE FROM failures;"); err != nil {
		return nil, fmt.Errorf("clear checkpoint responses: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO session(id,signature,started) VALUES(1,?,?)", signature, started.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("reset checkpoint session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("reset checkpoint session: %w", err)
	}
	return s, nil
}

// RecordFailure counts a failed attempt so status can report a sync that keeps failing.
func (s *Store) RecordFailure(ctx context.Context, cause error, at time.Time) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	text := cause.Error()
	if len(text) > maxErrorBytes {
		text = strings.ToValidUTF8(text[:maxErrorBytes], "") + "…"
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO failures(id,attempts,error,failed_at) VALUES(1,1,?,?) ON CONFLICT(id) DO UPDATE SET attempts=attempts+1,error=excluded.error,failed_at=excluded.failed_at", text, at.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record failed sync attempt: %w", err)
	}
	return nil
}

// Inspect reads a pending session without creating or changing the checkpoint file.
func Inspect(ctx context.Context, path string) (Pending, bool, error) {
	var out Pending
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return out, false, nil
	} else if err != nil {
		return out, false, fmt.Errorf("inspect pending sync: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return out, false, fmt.Errorf("resolve checkpoint path: %w", err)
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(2000)"}}.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return out, false, fmt.Errorf("open pending sync: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Log(ctx, diagnostics.TraceLevel, "close inspected checkpoint", "error", err)
		}
	}()
	var started string
	if err := db.QueryRowContext(ctx, "SELECT started FROM session WHERE id=1").Scan(&started); err != nil {
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "no such table") {
			return out, false, nil
		}
		return out, false, fmt.Errorf("read pending sync: %w", err)
	}
	if out.Started, err = time.Parse(time.RFC3339Nano, started); err != nil {
		return out, false, fmt.Errorf("parse pending sync start: %w", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM responses").Scan(&out.Saved); err != nil {
		return out, false, fmt.Errorf("count pending responses: %w", err)
	}
	var failedAt string
	err = db.QueryRowContext(ctx, "SELECT attempts,error,failed_at FROM failures WHERE id=1").Scan(&out.Failures, &out.LastError, &failedAt)
	if errors.Is(err, sql.ErrNoRows) || (err != nil && strings.Contains(err.Error(), "no such table")) {
		return out, true, nil
	}
	if err != nil {
		return out, false, fmt.Errorf("read pending sync failures: %w", err)
	}
	if out.FailedAt, err = time.Parse(time.RFC3339Nano, failedAt); err != nil {
		return out, false, fmt.Errorf("parse pending sync failure time: %w", err)
	}
	return out, true, nil
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

// Delete removes an invalid saved response while retaining other completed work.
func (s *Store) Delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM responses WHERE key=?", key); err != nil {
		return fmt.Errorf("delete invalid saved fetch: %w", err)
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
