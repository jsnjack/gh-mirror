package store

import (
	"context"
	"fmt"
	"time"
)

// checkpointTimeout bounds one shared commit, including work saved after cancellation.
const checkpointTimeout = 5 * time.Second

// indexWriter commits concurrent indexing writes together, so workers share one
// FULL-sync commit instead of waiting for a disk sync each.
type indexWriter struct {
	store    *Store
	ctx      context.Context
	requests chan *indexWrite
	stopped  chan struct{}
}

type indexWrite struct {
	ctx   context.Context
	apply func(context.Context, *Writer) error
	done  chan error
}

// startIndexWriter runs one committer until close; callers must stop writing before closing it.
func (s *Store) startIndexWriter(ctx context.Context) *indexWriter {
	w := &indexWriter{store: s, ctx: context.WithoutCancel(ctx), requests: make(chan *indexWrite), stopped: make(chan struct{})}
	go w.run()
	return w
}

// update applies one write in the next shared transaction and returns its own
// result. Writes whose context ends before they are applied are skipped; apply
// must run its statements with the context it receives.
func (w *indexWriter) update(ctx context.Context, apply func(context.Context, *Writer) error) error {
	write := &indexWrite{ctx: ctx, apply: apply, done: make(chan error, 1)}
	select {
	case w.requests <- write:
	case <-ctx.Done():
		return fmt.Errorf("queue index write: %w", ctx.Err())
	}
	return <-write.done
}

func (w *indexWriter) close() {
	close(w.requests)
	<-w.stopped
}

func (w *indexWriter) run() {
	defer close(w.stopped)
	for first := range w.requests {
		batch := []*indexWrite{first}
	collect:
		for {
			select {
			case next, ok := <-w.requests:
				if !ok {
					break collect
				}
				batch = append(batch, next)
			default:
				break collect
			}
		}
		w.commit(batch)
	}
}

// commit isolates each write in a savepoint so one failed write does not discard the others.
func (w *indexWriter) commit(batch []*indexWrite) {
	// Statements share one context: interrupting a write statement would roll back the whole transaction.
	ctx, cancel := context.WithTimeout(w.ctx, checkpointTimeout)
	defer cancel()
	results := make([]error, len(batch))
	err := w.store.Update(ctx, func(tx *Writer) error {
		for n, write := range batch {
			if err := write.ctx.Err(); err != nil {
				results[n] = fmt.Errorf("skip index write: %w", err)
				continue
			}
			if _, err := tx.tx.ExecContext(ctx, "SAVEPOINT index_write"); err != nil {
				return fmt.Errorf("begin index write: %w", err)
			}
			if err := write.apply(ctx, tx); err != nil {
				results[n] = err
				if _, err := tx.tx.ExecContext(ctx, "ROLLBACK TO index_write"); err != nil {
					return fmt.Errorf("undo failed index write: %w", err)
				}
			}
			if _, err := tx.tx.ExecContext(ctx, "RELEASE index_write"); err != nil {
				return fmt.Errorf("end index write: %w", err)
			}
		}
		return nil
	})
	for n, write := range batch {
		if results[n] == nil {
			results[n] = err
		}
		write.done <- results[n]
	}
}
