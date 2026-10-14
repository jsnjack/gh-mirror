//go:build unix

package diagnostics

import (
	"os"
	"syscall"
)

const noFollow = syscall.O_NOFOLLOW

func singleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink == 1
}
