package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"gh-mirror/internal/embedding"
	"gh-mirror/internal/representation"
)

func embeddingKey(model, text string) []byte {
	key := sha256.Sum256([]byte(model + "\x00" + representation.Version + "\x00" + text))
	return key[:]
}

type cachedCall struct {
	done   chan struct{}
	vector []float32
	err    error
	key    []byte
}

type cachingVectorizer struct {
	Vectorizer
	store    *Store
	mu       sync.Mutex
	inflight map[string]*cachedCall
	reused   atomic.Int64
}

func (e *cachingVectorizer) BatchSize() int {
	if batch, ok := e.Vectorizer.(BatchVectorizer); ok {
		return batch.BatchSize()
	}
	return 1
}

func (e *cachingVectorizer) Embed(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (e *cachingVectorizer) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	calls := make([]*cachedCall, len(texts))
	owned := []*cachedCall{}
	inputs := []string{}
	e.mu.Lock()
	for n, text := range texts {
		key := embeddingKey(e.ID(), text)
		call := e.inflight[string(key)]
		if call == nil {
			call = &cachedCall{done: make(chan struct{}), key: key}
			e.inflight[string(key)] = call
			owned = append(owned, call)
			inputs = append(inputs, text)
		} else {
			e.reused.Add(1)
		}
		calls[n] = call
	}
	e.mu.Unlock()
	if len(owned) > 0 {
		e.compute(ctx, owned, inputs)
		e.mu.Lock()
		for _, call := range owned {
			close(call.done)
			delete(e.inflight, string(call.key))
		}
		e.mu.Unlock()
	}
	out := make([][]float32, len(calls))
	for n, call := range calls {
		select {
		case <-call.done:
		default:
			select {
			case <-call.done:
			case <-ctx.Done():
				return nil, fmt.Errorf("wait for shared embedding: %w", ctx.Err())
			}
		}
		if call.err != nil {
			return nil, call.err
		}
		out[n] = call.vector
	}
	return out, nil
}

func (e *cachingVectorizer) compute(ctx context.Context, calls []*cachedCall, texts []string) {
	missing := []*cachedCall{}
	inputs := []string{}
	for n, call := range calls {
		var raw []byte
		err := e.store.db.QueryRowContext(ctx, "SELECT vector FROM embedding_cache WHERE key=?", call.key).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			missing = append(missing, call)
			inputs = append(inputs, texts[n])
			continue
		}
		if err != nil {
			call.err = fmt.Errorf("read shared embedding: %w", err)
			continue
		}
		call.vector, call.err = decodeVector(raw)
		e.reused.Add(1)
	}
	if len(missing) == 0 {
		return
	}
	var vectors [][]float32
	var err error
	if batch, ok := e.Vectorizer.(BatchVectorizer); ok && len(inputs) > 1 {
		vectors, err = batch.EmbedBatch(ctx, inputs)
	} else {
		for _, input := range inputs {
			v, encodeErr := e.Vectorizer.Embed(ctx, input)
			if encodeErr != nil {
				err = encodeErr
				break
			}
			vectors = append(vectors, v)
		}
	}
	if err == nil && len(vectors) != len(missing) {
		err = fmt.Errorf("encoder returned %d shared vectors for %d inputs", len(vectors), len(missing))
	}
	if err != nil {
		for _, call := range missing {
			call.err = fmt.Errorf("compute shared embedding: %w", err)
		}
		return
	}
	raw := make([][]byte, len(vectors))
	for n, v := range vectors {
		var encodeErr error
		raw[n], encodeErr = encodeVector(v)
		if encodeErr != nil {
			err = encodeErr
			break
		}
	}
	if err == nil {
		checkpoint, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = e.store.Update(checkpoint, func(w *Writer) error {
			for n, call := range missing {
				if _, err := w.tx.ExecContext(checkpoint, `INSERT INTO embedding_cache(key,vector) VALUES(?,?) ON CONFLICT(key) DO NOTHING`, call.key, raw[n]); err != nil {
					return fmt.Errorf("checkpoint shared embedding: %w", err)
				}
			}
			return nil
		})
		cancel()
	}
	for n, call := range missing {
		call.vector = vectors[n]
		call.err = err
	}
}

func decodeVector(raw []byte) ([]float32, error) {
	if len(raw) != embedding.Dimension*4 {
		return nil, fmt.Errorf("shared vector length %d", len(raw))
	}
	out := make([]float32, embedding.Dimension)
	for n := range out {
		out[n] = math.Float32frombits(binary.LittleEndian.Uint32(raw[n*4:]))
	}
	if _, err := encodeVector(out); err != nil {
		return nil, fmt.Errorf("validate shared vector: %w", err)
	}
	return out, nil
}
