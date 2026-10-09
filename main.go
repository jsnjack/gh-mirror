package main

import (
	"context"
	"fmt"
	"gh-mirror/internal/diagnostics"
	"log/slog"
	"os"

	"gh-mirror/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			slog.Log(context.Background(), diagnostics.TraceLevel, "write command error", "error", writeErr)
		}
		os.Exit(1)
	}
}
