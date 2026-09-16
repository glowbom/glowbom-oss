package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCursorCLIAvailableOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor-agent")
	t.Setenv("GLOWBOM_CURSOR_BIN", path)
	if cursorCLIAvailable() {
		t.Fatal("missing executable accepted")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if !cursorCLIAvailable() {
		t.Fatal("configured executable not found")
	}
}
