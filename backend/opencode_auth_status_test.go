package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAICredentialTypeFromAuthFile(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "oauth", payload: `{"openai":{"type":"oauth","access":"secret"}}`, want: "oauth"},
		{name: "api", payload: `{"openai":{"type":"api","key":"secret"}}`, want: "api"},
		{name: "missing OpenAI", payload: `{"opencode":{"type":"api","key":"secret"}}`, want: "none"},
		{name: "unknown type", payload: `{"openai":{"type":"other"}}`, want: "unknown"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(path, []byte(test.payload), 0600); err != nil {
				t.Fatalf("write auth fixture: %v", err)
			}

			if got := openAICredentialTypeFromAuthFile(path); got != test.want {
				t.Fatalf("openAICredentialTypeFromAuthFile() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOpenAICredentialTypeFromAuthFileReturnsNoneWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-auth.json")
	if got := openAICredentialTypeFromAuthFile(path); got != "none" {
		t.Fatalf("openAICredentialTypeFromAuthFile() = %q, want none", got)
	}
}

func TestUserOpenCodeRuntimePathsUsesStandardDefaults(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	paths, err := userOpenCodeRuntimePaths()
	if err != nil {
		t.Fatalf("userOpenCodeRuntimePaths() error = %v", err)
	}

	if want := filepath.Join(homeDir, ".local", "share"); paths.DataHome != want {
		t.Fatalf("DataHome = %q, want %q", paths.DataHome, want)
	}
	if want := filepath.Join(homeDir, ".local", "state"); paths.StateHome != want {
		t.Fatalf("StateHome = %q, want %q", paths.StateHome, want)
	}
	if want := filepath.Join(homeDir, ".local", "share", "opencode", "auth.json"); paths.AuthFile != want {
		t.Fatalf("AuthFile = %q, want %q", paths.AuthFile, want)
	}
	if want := filepath.Join(homeDir, ".config"); paths.ConfigHome != want {
		t.Fatalf("ConfigHome = %q, want %q", paths.ConfigHome, want)
	}
}

func TestUserOpenCodeRuntimePathsHonorsXDGDirectories(t *testing.T) {
	homeDir := t.TempDir()
	dataHome := filepath.Join(t.TempDir(), "data")
	stateHome := filepath.Join(t.TempDir(), "state")
	configHome := filepath.Join(t.TempDir(), "config")
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	paths, err := userOpenCodeRuntimePaths()
	if err != nil {
		t.Fatalf("userOpenCodeRuntimePaths() error = %v", err)
	}

	if paths.DataHome != dataHome {
		t.Fatalf("DataHome = %q, want %q", paths.DataHome, dataHome)
	}
	if paths.StateHome != stateHome {
		t.Fatalf("StateHome = %q, want %q", paths.StateHome, stateHome)
	}
	if paths.ConfigHome != configHome {
		t.Fatalf("ConfigHome = %q, want %q", paths.ConfigHome, configHome)
	}
	if want := filepath.Join(dataHome, "opencode", "auth.json"); paths.AuthFile != want {
		t.Fatalf("AuthFile = %q, want %q", paths.AuthFile, want)
	}
}

func TestAuthStatusRuntimePathsUsesUserStoreForOpenCodeConfig(t *testing.T) {
	glowbomPaths := openCodeRuntimePaths{AuthFile: "glowbom/auth.json"}
	userPaths := openCodeRuntimePaths{AuthFile: "user/auth.json"}

	tests := []struct {
		mode string
		want string
	}{
		{mode: "opencode-config", want: userPaths.AuthFile},
		{mode: "unknown", want: userPaths.AuthFile},
		{mode: "api-key", want: glowbomPaths.AuthFile},
		{mode: "codex-jwt", want: glowbomPaths.AuthFile},
	}

	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			got := authStatusRuntimePaths(test.mode, glowbomPaths, userPaths)
			if got.AuthFile != test.want {
				t.Fatalf("AuthFile = %q, want %q", got.AuthFile, test.want)
			}
		})
	}
}
