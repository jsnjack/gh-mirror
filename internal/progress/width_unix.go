//go:build unix

package progress

import (
	"io"

	"golang.org/x/sys/unix"
)

// terminalWidth reads the terminal's column count, or defaultWidth when unknown.
func terminalWidth(writer io.Writer) int {
	if fd, ok := writer.(interface{ Fd() uintptr }); ok {
		if size, err := unix.IoctlGetWinsize(int(fd.Fd()), unix.TIOCGWINSZ); err == nil && size.Col > 0 {
			return int(size.Col)
		}
	}
	return defaultWidth
}
