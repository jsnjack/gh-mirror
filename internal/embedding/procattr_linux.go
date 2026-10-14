//go:build linux

package embedding

import "syscall"

// childAttributes stops the inference runtime when gh-mirror exits without closing it.
func childAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
