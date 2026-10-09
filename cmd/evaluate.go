package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"gh-mirror/internal/diagnostics"
	"gh-mirror/internal/store"

	"github.com/spf13/cobra"
)

func addEvaluate() {
	var path string
	command := &cobra.Command{Use: "evaluate", Short: "Measure local retrieval against a JSON judgment set", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if path == "" {
			return fmt.Errorf("--cases is required")
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open evaluation cases: %w", err)
		}
		defer func() {
			if err := file.Close(); err != nil {
				slog.Log(c.Context(), diagnostics.TraceLevel, "close evaluation input", "error", err)
			}
		}()
		decoder := json.NewDecoder(io.LimitReader(file, 8<<20))
		decoder.DisallowUnknownFields()
		var set store.EvaluationSet
		if err := decoder.Decode(&set); err != nil {
			return fmt.Errorf("decode evaluation cases: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return fmt.Errorf("evaluation requires one JSON object")
		}
		db, err := open(true)
		if err != nil {
			return err
		}
		defer closeStore(c.Context(), db)
		out, err := db.Evaluate(c.Context(), set)
		if err != nil {
			return fmt.Errorf("evaluate retrieval: %w", err)
		}
		return output(c, out)
	}}
	command.Flags().StringVar(&path, "cases", "", "Path to local JSON searches/candidates and relevance judgments")
	root.AddCommand(command)
}
