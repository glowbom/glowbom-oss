//go:build !unix

package main

import "os/exec"

func configurePreviewProcess(cmd *exec.Cmd) {}

func stopPreviewProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
