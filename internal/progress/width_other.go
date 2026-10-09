//go:build !unix

package progress

import "io"

func terminalWidth(io.Writer) int { return lineWidth }
