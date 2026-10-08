package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexExecutableSelection(t *testing.T) {
	directory := t.TempDir()
	cli, bundle := filepath.Join(directory, "cli"), filepath.Join(directory, "bundle")
	for _, path := range []string{cli, bundle} {
		if err := os.WriteFile(path, []byte("fixture"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, cliVersion, bundleVersion, override, expected string }{
		{"newer bundle", "0.145.0", "0.159.2", "", bundle},
		{"newer CLI", "0.160.0", "0.159.2", "", cli},
		{"explicit older CLI", "0.145.0", "0.159.2", cli, cli},
		{"all versions unknown", "", "", "", cli},
		{"known bundle", "", "0.159.2", "", bundle},
		{"unknown bundle", "0.145.0", "", "", cli},
		{"matching versions preserve CLI", "0.159.2", "0.159.2", "", cli},
		{"release beats prerelease", "0.159.2-alpha.1", "0.159.2", "", bundle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected, err := selectCodexExecutable(tc.override, []string{cli, bundle}, func(path string) string {
				if tc.override != "" {
					t.Fatal("explicit override triggered version discovery")
				}
				if path == cli {
					return tc.cliVersion
				}
				return tc.bundleVersion
			})
			if err != nil || selected != tc.expected {
				t.Fatalf("selected %q, err=%v, want %q", selected, err, tc.expected)
			}
		})
	}
	if _, err := selectCodexExecutable(filepath.Join(directory, "missing"), []string{cli}, func(string) string { return "0.159.2" }); err == nil {
		t.Fatal("missing explicit runtime silently fell back")
	}
	t.Setenv("GLOWBOM_CODEX_BIN", cli)
	if selected, err := codexExecutable(); err != nil || selected != cli {
		t.Fatalf("environment override ignored: %q, %v", selected, err)
	}
}

func TestCodexExecutableMacFindsNewerRuntimeBeyondFirstPATH(t *testing.T) {
	directory := t.TempDir()
	first, second := filepath.Join(directory, "old", "codex"), filepath.Join(directory, "new", "codex")
	for _, path := range []string{first, second} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	searchPath := strings.Join([]string{filepath.Dir(first), "relative-bin", "", filepath.Dir(second)}, string(os.PathListSeparator))
	candidates := macCodexExecutableCandidates(first, searchPath, directory)
	selected, err := selectCodexExecutable("", candidates, func(path string) string {
		if path == first {
			return "0.145.0"
		}
		if path == second {
			return "0.160.0"
		}
		return ""
	})
	if err != nil || selected != second {
		t.Fatalf("newer CLI was hidden by first PATH entry: %q, %v", selected, err)
	}
	want := map[string]bool{
		"/opt/homebrew/bin/codex": false, "/usr/local/bin/codex": false,
		filepath.Join(directory, ".local", "bin", "codex"):                                                            false,
		filepath.Join(directory, ".npm-global", "bin", "codex"):                                                       false,
		filepath.Join(directory, "Applications", "ChatGPT.app", "Contents", "Resources", "codex-cli", "bin", "codex"): false,
	}
	for _, candidate := range candidates {
		if !filepath.IsAbs(candidate) {
			t.Fatalf("candidate came from relative or empty PATH entry: %q", candidate)
		}
		if _, expected := want[candidate]; expected {
			want[candidate] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("missing installed-runtime location %q", path)
		}
	}
}

func TestCodexExecutableVersionHelperProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-1] != "codex-version-helper" {
		return
	}
	for _, name := range []string{"GLOWBOM_SERVER_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"} {
		if os.Getenv(name) != "" {
			os.Exit(4)
		}
	}
	fmt.Println("codex-cli 0.159.2")
	os.Exit(0)
}

func TestCodexExecutableVersionCacheAndEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(path, []byte("first"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"GLOWBOM_SERVER_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"} {
		t.Setenv(name, "must-not-reach-version-probe")
	}
	previous := codexVersionCommand
	calls := 0
	codexVersionCommand = func(ctx context.Context, binary string, args ...string) *exec.Cmd {
		calls++
		if binary != path || len(args) != 1 || args[0] != "--version" {
			t.Fatal("unexpected runtime probe")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("version probe has no deadline")
		}
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCodexExecutableVersionHelperProcess$", "--", "codex-version-helper")
	}
	t.Cleanup(func() { codexVersionCommand = previous })
	for range 2 {
		if version := codexExecutableVersion(path); version != "0.159.2" {
			t.Fatalf("invalid version %q", version)
		}
	}
	if calls != 1 {
		t.Fatalf("cached lookup spawned %d probes", calls)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0700); err != nil {
		t.Fatal(err)
	}
	if version := codexExecutableVersion(path); version != "0.159.2" || calls != 2 {
		t.Fatalf("replacement runtime not rechecked: version=%q calls=%d", version, calls)
	}
}

func TestCodexExecutableVersionOutputBound(t *testing.T) {
	output := &codexVersionOutput{}
	data := []byte(strings.Repeat("x", 8192))
	if n, err := output.Write(data); err != nil || n != len(data) || len(output.data) != 4096 {
		t.Fatal("version output was not bounded")
	}
}
