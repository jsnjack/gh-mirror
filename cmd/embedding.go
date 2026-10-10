package cmd

import (
	"context"
	"gh-mirror/internal/embedding"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"
)

func configureIndexEncoder(ctx context.Context, workers int) error {
	if embeddingProvider == nil {
		return nil
	}
	if err := embeddingProvider.Close(); err != nil {
		return err
	}
	opts := settings.Embedding
	opts.Workers = workers
	var err error
	embeddingProvider, err = embedding.New(ctx, opts)
	return err
}

func commandEncoder() store.Vectorizer {
	if embeddingProvider != nil {
		return embeddingProvider
	}
	return embedding.Default
}

func setEmbeddingProgress(report progress.Reporter) {
	if embeddingProvider == nil {
		return
	}
	if report == nil {
		embeddingProvider.SetReporter(nil)
		return
	}
	embeddingProvider.SetReporter(func(s embedding.State) {
		kind := progress.CPUWorkers
		if s.Backend != "cpu" {
			kind = "Index"
		}
		report.Send(progress.Event{Backend: s.Backend, Device: s.Device, FallbackReason: s.FallbackReason, EmbeddingVectors: s.Vectors, EmbeddingRate: s.VectorsPerSecond, BatchSize: s.LastBatch, WorkerKind: kind, Listener: s.Listener})
	})
}
