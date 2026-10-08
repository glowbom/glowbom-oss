package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClaudeCodeCLIAvailableOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", path)
	if claudeCodeCLIAvailable() {
		t.Fatal("missing configured executable accepted")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if !claudeCodeCLIAvailable() {
		t.Fatal("configured executable not found")
	}
}

func TestClaudeCodeCLIAvailableNativeInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", "")
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(home, ".local", "bin", name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if !claudeCodeCLIAvailable() {
		t.Fatal("native Claude Code installation was not found outside PATH")
	}
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", filepath.Join(home, "missing-override"))
	if claudeCodeCLIAvailable() {
		t.Fatal("missing explicit override must not fall back to a different installation")
	}
}

func TestReportCodingAgents(t *testing.T) {
	for _, test := range []struct {
		name     string
		env      string
		openCode bool
		wanted   bool
	}{
		{name: "none"},
		{name: "OpenCode", openCode: true, wanted: true},
		{name: "Cursor", env: "GLOWBOM_CURSOR_BIN", wanted: true},
		{name: "Claude Code", env: "GLOWBOM_CLAUDE_CODE_BIN", wanted: true},
		{name: "Codex", env: "GLOWBOM_CODEX_BIN", wanted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, env := range []string{"GLOWBOM_CURSOR_BIN", "GLOWBOM_CLAUDE_CODE_BIN", "GLOWBOM_CODEX_BIN"} {
				t.Setenv(env, filepath.Join(dir, "missing"))
			}
			if test.env != "" {
				path := filepath.Join(dir, "agent")
				if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv(test.env, path)
			}
			var output bytes.Buffer
			if got := reportCodingAgents(&output, test.openCode); got != test.wanted {
				t.Fatalf("available = %v, want %v; output = %s", got, test.wanted, output.String())
			}
			if strings.Contains(output.String(), "[MISSING]") == test.wanted {
				t.Fatalf("incorrect missing-agent status: %s", output.String())
			}
			if test.env != "" && !strings.Contains(output.String(), test.name) {
				t.Fatalf("installed agent was not reported: %s", output.String())
			}
		})
	}
}
