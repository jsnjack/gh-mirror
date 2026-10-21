package cmd

import (
	"io"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"gh-mirror/internal/progress"
)

// logSink carries stderr diagnostics. Live progress takes it over so log lines print
// above the redrawn frame instead of inside it.
type logSink struct {
	mu     sync.Mutex
	stderr io.Writer
	live   io.Writer
}

var progressLogs = &logSink{}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	w := s.stderr
	if s.live != nil {
		w = s.live
	}
	s.mu.Unlock()
	return w.Write(p)
}

func (s *logSink) reset(stderr io.Writer) {
	s.mu.Lock()
	s.stderr, s.live = stderr, nil
	s.mu.Unlock()
}

func (s *logSink) route(live io.Writer) {
	s.mu.Lock()
	s.live = live
	s.mu.Unlock()
}

// startProgress draws a live checklist on terminals and prints plain status lines
// when stderr is piped, in debug mode, or on a dumb terminal.
func startProgress(command *cobra.Command, operation string) (*progress.Display, error) {
	w := command.ErrOrStderr()
	if debug || !terminalWriter(w) || os.Getenv("TERM") == "dumb" {
		return progress.NewOperation(w, operation)
	}
	display, err := progress.NewTerminal(w, operation, os.Getenv("NO_COLOR") == "")
	if err == nil {
		progressLogs.route(display)
	}
	return display, err
}
