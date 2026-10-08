//go:build darwin || linux

package main

import (
	"os"
	"syscall"
)

// The native launcher creates a dedicated process group. Never signal a shared group.
func stopDesktopProcessGroup() {
	if syscall.Getpgrp() == os.Getpid() {
		_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
	}
}
