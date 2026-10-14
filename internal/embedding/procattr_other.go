//go:build !linux

package embedding

import "syscall"

func childAttributes() *syscall.SysProcAttr { return nil }
