//go:build unix

package progress

import (
	"context"
	"io"
	"log/slog"

	"gh-mirror/internal/diagnostics"

	"golang.org/x/sys/unix"
)

func terminalWidth(writer io.Writer) int {
	if fd, ok := writer.(interface{ Fd() uintptr }); ok {
		if size, err := unix.IoctlGetWinsize(int(fd.Fd()), unix.TIOCGWINSZ); err == nil && size.Col > 0 {
			return int(size.Col)
		} else if err != nil {
			slog.Log(context.Background(), diagnostics.TraceLevel, "read terminal width; use default width", "error", err)
		}
	}
	return lineWidth
}
