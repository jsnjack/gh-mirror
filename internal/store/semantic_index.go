package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"gh-mirror/internal/embedding"
	"gh-mirror/internal/progress"
)

// ErrSemanticUnavailable indicates that compatible complete vectors are required.
var ErrSemanticUnavailable = errors.New("semantic index unavailable")

// Vectorizer defines deterministic offline passage encoding and chunking.
type Vectorizer interface {
	ID() string
	Dim() int
	Chunks(string) ([]embedding.Chunk, error)
	Embed(context.Context, string) ([]float32, error)
}

// SemanticStatus reports model identity and durable indexing coverage.
type SemanticStatus struct {
	Model            string `json:"model"`
	Fingerprint      string `json:"fingerprint"`
	Dimension        int    `json:"dimension"`
	Generation       string `json:"generation"`
	Documents        int    `json:"documents"`
	IndexedDocuments int    `json:"indexed_documents"`
	PendingDocuments int    `json:"pending_documents"`
	Chunks           int    `json:"chunks"`
	IndexedAt        string `json:"indexed_at,omitempty"`
	Compatible       bool   `json:"compatible"`
}

func semanticStatus(ctx context.Context, q querier) (*SemanticStatus, error) {
	out := &SemanticStatus{Model: embedding.Model}
	rows, err := q.QueryContext(ctx, "SELECT key,value FROM metadata WHERE key IN ('semantic_model','semantic_generation','semantic_indexed_at')")
	if err != nil {
		return nil, fmt.Errorf("read semantic metadata: %w", err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, finishRows(rows, err)
		}
		switch k {
		case "semantic_model":
			out.Fingerprint = v
		case "semantic_generation":
			out.Generation = v
		case "semantic_indexed_at":
			out.IndexedAt = v
		}
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return nil, err
	}
	out.Compatible = out.Fingerprint == embedding.Fingerprint
	if out.Fingerprint != "" {
		out.Dimension = embedding.Dimension
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN s.complete=1 AND s.model=? THEN 1 ELSE 0 END),0) FROM documents d LEFT JOIN semantic_documents s ON s.document_id=d.id`, out.Fingerprint).Scan(&out.Documents, &out.IndexedDocuments); err != nil {
		return nil, fmt.Errorf("count semantic documents: %w", err)
	}
	out.PendingDocuments = out.Documents - out.IndexedDocuments
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM semantic_vectors").Scan(&out.Chunks); err != nil {
		return nil, fmt.Errorf("count semantic chunks: %w", err)
	}
	return out, nil
}

// IndexOptions bounds CPU concurrency and reports local indexing progress.
type IndexOptions struct {
	Workers  int
	Progress progress.Reporter
}

type indexDocument struct{ id, title, body string }
type passage struct {
	embedding.Chunk
	field string
}

func passages(e Vectorizer, d indexDocument) ([]passage, error) {
	out := []passage{}
	for _, part := range []struct{ field, text string }{{"title", d.title}, {"body", d.body}} {
		chunks, err := e.Chunks(part.text)
		if err != nil {
			return nil, err
		}
		field := part.field
		switch {
		case strings.HasPrefix(d.id, "labels:"):
			field = "labels"
		case strings.HasPrefix(d.id, "comment:review:"):
			field = "review_comment"
		case strings.HasPrefix(d.id, "comment:"):
			field = "comment"
		}
		for _, chunk := range chunks {
			out = append(out, passage{chunk, field})
		}
	}
	return out, nil
}
func documentHash(d indexDocument) string { return hash([]byte(d.title + "\x00" + d.body)) }

// Index durably indexes only new or changed documents, retaining completed chunks on interruption.
func (s *Store) Index(ctx context.Context, e Vectorizer, o IndexOptions) (SemanticStatus, error) {
	if o.Workers == 0 {
		o.Workers = 4
	}
	if o.Workers < 1 || o.Workers > 16 {
		return SemanticStatus{}, invalid("index workers must be between 1 and 16")
	}
	if e.Dim() != embedding.Dimension {
		return SemanticStatus{}, fmt.Errorf("index dimension %d; expected %d", e.Dim(), embedding.Dimension)
	}
	err := s.Update(ctx, func(w *Writer) error {
		old, err := w.Metadata(ctx, "semantic_model")
		if err != nil {
			return err
		}
		if old != e.ID() {
			if _, err := w.tx.ExecContext(ctx, "DELETE FROM semantic_documents"); err != nil {
				return fmt.Errorf("reset incompatible vectors: %w", err)
			}
			if err := w.SetMetadata(ctx, "semantic_model", e.ID()); err != nil {
				return err
			}
			return advanceSemantic(ctx, w)
		}
		return nil
	})
	if err != nil {
		return SemanticStatus{}, err
	}
	initial, err := semanticStatus(ctx, s.db)
	if err != nil {
		return SemanticStatus{}, err
	}
	completed, total := 0, initial.PendingDocuments
	o.Progress.Send(progress.Event{Phase: "Indexing semantic documents", Total: total, Workers: o.Workers, WorkerKind: progress.CPUWorkers})
	for {
		if err := ctx.Err(); err != nil {
			return SemanticStatus{}, fmt.Errorf("index interrupted; completed chunks retained: %w", err)
		}
		processed, err := s.indexPass(ctx, e, o, &completed, total)
		if err != nil {
			return SemanticStatus{}, err
		}
		if processed == 0 {
			break
		}
	}
	if err := s.Update(ctx, func(w *Writer) error {
		return w.SetMetadata(ctx, "semantic_indexed_at", time.Now().UTC().Format(time.RFC3339Nano))
	}); err != nil {
		return SemanticStatus{}, err
	}
	out, err := semanticStatus(ctx, s.db)
	if err != nil {
		return SemanticStatus{}, err
	}
	return *out, nil
}

func (s *Store) pendingDocuments(ctx context.Context, after, model string) ([]indexDocument, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id,d.title,d.body FROM documents d LEFT JOIN semantic_documents v ON v.document_id=d.id WHERE d.id>? AND (v.document_id IS NULL OR v.complete=0 OR v.model<>?) ORDER BY d.id LIMIT 32`, after, model)
	if err != nil {
		return nil, fmt.Errorf("find pending documents: %w", err)
	}
	documents := []indexDocument{}
	for rows.Next() {
		var d indexDocument
		if err := rows.Scan(&d.id, &d.title, &d.body); err != nil {
			return nil, finishRows(rows, err)
		}
		documents = append(documents, d)
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return nil, err
	}
	return documents, nil
}

func (s *Store) indexPass(ctx context.Context, e Vectorizer, o IndexOptions, completed *int, total int) (int, error) {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan indexDocument, o.Workers)
	done := make(chan error, o.Workers+1)
	var group sync.WaitGroup
	var progressMu sync.Mutex
	active, submitted := 0, 0
	report := func(event progress.Event, delta int) {
		progressMu.Lock()
		defer progressMu.Unlock()
		active += delta
		event.Workers, event.Active, event.WorkerKind = o.Workers, active, progress.CPUWorkers
		o.Progress.Send(event)
	}
	group.Go(func() {
		defer close(jobs)
		after := ""
		for {
			documents, err := s.pendingDocuments(child, after, e.ID())
			if err != nil {
				done <- err
				return
			}
			if len(documents) == 0 {
				return
			}
			after = documents[len(documents)-1].id
			for _, d := range documents {
				select {
				case jobs <- d:
					submitted++
				case <-child.Done():
					done <- fmt.Errorf("queue index document: %w", child.Err())
					return
				}
			}
		}
	})
	for range o.Workers {
		group.Go(func() {
			for d := range jobs {
				if child.Err() != nil {
					return
				}
				report(progress.Event{}, 1)
				chunks, err := passages(e, d)
				if err == nil {
					err = s.indexDocument(child, e, d, chunks, IndexOptions{Workers: 1})
				}
				report(progress.Event{}, -1)
				done <- err
				if err != nil {
					return
				}
			}
		})
	}
	go func() { group.Wait(); close(done) }()
	var failure error
	for err := range done {
		if err != nil {
			if failure == nil {
				failure = err
				cancel()
			}
			continue
		}
		*completed++
		report(progress.Event{Phase: "Indexing semantic documents", Completed: *completed, Total: total}, 0)
	}
	if failure != nil {
		return submitted, fmt.Errorf("index documents; completed chunks retained: %w", failure)
	}
	if err := ctx.Err(); err != nil {
		return submitted, fmt.Errorf("index interrupted: %w", err)
	}
	return submitted, nil
}

var errDocumentChanged = errors.New("document changed during indexing; rerun index")

func checkDocument(ctx context.Context, w *Writer, d indexDocument) error {
	var current indexDocument
	err := w.tx.QueryRowContext(ctx, "SELECT title,body FROM documents WHERE id=?", d.id).Scan(&current.title, &current.body)
	if errors.Is(err, sql.ErrNoRows) {
		return errDocumentChanged
	}
	if err != nil {
		return fmt.Errorf("check index source: %w", err)
	}
	if documentHash(current) != documentHash(d) {
		return errDocumentChanged
	}
	return nil
}
func (s *Store) indexDocument(ctx context.Context, e Vectorizer, d indexDocument, chunks []passage, o IndexOptions) error {
	if err := s.Update(ctx, func(w *Writer) error {
		if err := checkModel(ctx, w, e.ID()); err != nil {
			return err
		}
		if err := checkDocument(ctx, w, d); err != nil {
			return err
		}
		cached := map[string][]byte{}
		rows, err := w.tx.QueryContext(ctx, `SELECT v.field,v.text,v.vector FROM semantic_vectors v JOIN semantic_documents s ON s.document_id=v.document_id WHERE v.document_id=? AND s.model=?`, d.id, e.ID())
		if err != nil {
			return fmt.Errorf("read reusable chunk vectors: %w", err)
		}
		for rows.Next() {
			var field, text string
			var raw []byte
			if err := rows.Scan(&field, &text, &raw); err != nil {
				return finishRows(rows, err)
			}
			cached[field+"\x00"+text] = raw
		}
		if err := finishRows(rows, rows.Err()); err != nil {
			return err
		}
		_, err = w.tx.ExecContext(ctx, `INSERT INTO semantic_documents(document_id,model,content_hash,chunks) VALUES(?,?,?,?) ON CONFLICT(document_id) DO UPDATE SET model=excluded.model,content_hash=excluded.content_hash,chunks=excluded.chunks,complete=0,indexed_at=''`, d.id, e.ID(), documentHash(d), len(chunks))
		if err != nil {
			return fmt.Errorf("stage semantic document: %w", err)
		}
		if _, err := w.tx.ExecContext(ctx, "DELETE FROM semantic_vectors WHERE document_id=?", d.id); err != nil {
			return fmt.Errorf("replace stale chunks: %w", err)
		}
		for n, c := range chunks {
			raw, ok := cached[c.field+"\x00"+c.Text]
			if !ok {
				continue
			}
			if _, err := w.tx.ExecContext(ctx, `INSERT INTO semantic_vectors(document_id,chunk,field,start,end,text,vector) VALUES(?,?,?,?,?,?,?)`, d.id, n, c.field, c.Start, c.End, c.Text, raw); err != nil {
				return fmt.Errorf("reuse unchanged passage: %w", err)
			}
		}
		if len(cached) > 0 {
			return advanceSemantic(ctx, w)
		}
		return nil
	}); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT chunk FROM semantic_vectors WHERE document_id=?", d.id)
	if err != nil {
		return fmt.Errorf("read completed chunks: %w", err)
	}
	saved := map[int]bool{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return finishRows(rows, err)
		}
		saved[n] = true
	}
	if err := finishRows(rows, rows.Err()); err != nil {
		return err
	}
	jobs := make(chan int, len(chunks))
	for n := range chunks {
		if !saved[n] {
			jobs <- n
		}
	}
	close(jobs)
	type result struct {
		n      int
		vector []float32
		err    error
	}
	done := make(chan result, o.Workers)
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	var group sync.WaitGroup
	for range min(o.Workers, len(jobs)) {
		group.Go(func() {
			for n := range jobs {
				if child.Err() != nil {
					return
				}
				vector, err := e.Embed(child, chunks[n].Text)
				select {
				case done <- result{n, vector, err}:
				case <-child.Done():
					return
				}
				if err != nil {
					return
				}
			}
		})
	}
	go func() { group.Wait(); close(done) }()
	var failure error
	for r := range done {
		if failure != nil {
			continue
		}
		if r.err != nil {
			failure = fmt.Errorf("encode document %s chunk %d: %w", d.id, r.n, r.err)
			cancel()
			continue
		}
		raw, err := encodeVector(r.vector)
		if err != nil {
			failure = err
			cancel()
			continue
		}
		// Completed inference survives cancellation while its small checkpoint finishes.
		checkpoint, finish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		failure = s.Update(checkpoint, func(w *Writer) error {
			if err := checkModel(checkpoint, w, e.ID()); err != nil {
				return err
			}
			if err := checkDocument(checkpoint, w, d); err != nil {
				return err
			}
			c := chunks[r.n]
			if _, err := w.tx.ExecContext(checkpoint, `INSERT INTO semantic_vectors(document_id,chunk,field,start,end,text,vector) VALUES(?,?,?,?,?,?,?) ON CONFLICT(document_id,chunk) DO NOTHING`, d.id, r.n, c.field, c.Start, c.End, c.Text, raw); err != nil {
				return fmt.Errorf("checkpoint vector: %w", err)
			}
			return advanceSemantic(checkpoint, w)
		})
		finish()
		if failure != nil {
			cancel()
		}
	}
	if failure != nil {
		return failure
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("index interrupted; chunks retained: %w", err)
	}
	return s.Update(ctx, func(w *Writer) error {
		if err := checkModel(ctx, w, e.ID()); err != nil {
			return err
		}
		if err := checkDocument(ctx, w, d); err != nil {
			return err
		}
		if _, err := w.tx.ExecContext(ctx, "UPDATE semantic_documents SET complete=1,indexed_at=? WHERE document_id=? AND chunks=(SELECT count(*) FROM semantic_vectors WHERE document_id=?)", time.Now().UTC().Format(time.RFC3339Nano), d.id, d.id); err != nil {
			return fmt.Errorf("complete semantic document: %w", err)
		}
		return advanceSemantic(ctx, w)
	})
}
func checkModel(ctx context.Context, w *Writer, expected string) error {
	current, err := w.Metadata(ctx, "semantic_model")
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("semantic model changed during indexing; rerun index")
	}
	return nil
}
func advanceSemantic(ctx context.Context, w *Writer) error {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return fmt.Errorf("create index generation: %w", err)
	}
	return w.SetMetadata(ctx, "semantic_generation", fmt.Sprintf("%x", id))
}
func encodeVector(v []float32) ([]byte, error) {
	if len(v) != embedding.Dimension {
		return nil, fmt.Errorf("vector dimension %d; expected %d", len(v), embedding.Dimension)
	}
	raw := make([]byte, len(v)*4)
	var norm float64
	for n, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, fmt.Errorf("non-finite embedding coordinate")
		}
		norm += float64(x) * float64(x)
		binary.LittleEndian.PutUint32(raw[n*4:], math.Float32bits(x))
	}
	if math.Abs(norm-1) > 0.001 {
		return nil, fmt.Errorf("embedding is not normalized (norm squared %.6f)", norm)
	}
	return raw, nil
}
