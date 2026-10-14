package cmd

import (
	"context"
	"gh-mirror/internal/embedding"
	"gh-mirror/internal/progress"
	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

// addEmbeddingFlags exposes encoder overrides only on commands that embed text.
func addEmbeddingFlags(command *cobra.Command) {
	flags := command.Flags()
	flags.StringVar(&embeddingBackend, "embedding-backend", "", "Embedding backend: cpu or lemonade-vulkan (defaults to configuration)")
	flags.StringVar(&embeddingRuntime, "embedding-runtime", "", "Absolute path to Lemonade's installed Vulkan llama-server")
	flags.StringVar(&embeddingDevice, "embedding-device", "", "Vulkan device, such as Vulkan0 (defaults to configuration)")
	flags.IntVar(&embeddingBatchSize, "embedding-batch-size", 0, "Maximum Vulkan batch size (1–32; default 8)")
}

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
