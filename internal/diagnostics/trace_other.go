//go:build !unix

package diagnostics

import "os"

const noFollow = 0

func singleLink(os.FileInfo) bool { return true }
