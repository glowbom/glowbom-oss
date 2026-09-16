//go:build !unix

package main

import "os/exec"

// CommandContext cancels the CLI process on non-Unix hosts.
func configureCursorCancellation(cmd *exec.Cmd) {}
