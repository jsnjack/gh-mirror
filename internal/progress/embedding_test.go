package progress

import (
	"bytes"
	"strings"
	"testing"
)

func TestEmbeddingRuntimeProgress(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "terminal"}[interactive], func(t *testing.T) {
			var output bytes.Buffer
			d, err := startDisplay(&output, interactive, "index")
			if err != nil {
				t.Fatal(err)
			}
			d.Report(Event{Phase: "Indexing semantic documents", Workers: 8, Active: 8, WorkerKind: CPUWorkers, Total: 100})
			d.Report(Event{Backend: "lemonade-vulkan", Device: "Vulkan0: Radeon 890M", EmbeddingVectors: 32, EmbeddingRate: 123.4, BatchSize: 16, Listener: "http://127.0.0.1:12345"})
			d.Report(Event{Workers: 8, Active: 4, WorkerKind: CPUWorkers})
			if err := d.Finish(nil); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"lemonade-vulkan", "Vulkan0: Radeon 890M", "32 vectors", "123.4 vectors/s", "batch 16", "Index workers 4/8 active", "Listening on http://127.0.0.1:12345"} {
				if !strings.Contains(output.String(), want) {
					t.Fatal("missing runtime detail", want, output.String())
				}
			}
		})
	}
	var output bytes.Buffer
	d, err := startDisplay(&output, false, "index")
	if err != nil {
		t.Fatal(err)
	}
	d.Report(Event{Backend: "cpu", Device: "CPU (pure Go)", FallbackReason: "runtime unavailable\x1b[31m", WorkerKind: CPUWorkers, Workers: 8})
	if err := d.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "CPU fallback: runtime unavailable") || strings.Contains(output.String(), "\x1b") {
		t.Fatal("fallback missing or unsafe", output.String())
	}
}
