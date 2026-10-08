package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

func codexExecutable() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("GLOWBOM_CODEX_BIN")); configured != "" {
		return exec.LookPath(configured)
	}
	cli, cliErr := codingAgentExecutable(codingAgentSpec{
		id: "codex", binary: "codex", homes: []string{filepath.Join(".local", "bin"), filepath.Join(".npm-global", "bin")},
	})
	if runtime.GOOS != "darwin" {
		return cli, cliErr
	}
	home, _ := os.UserHomeDir()
	return selectCodexExecutable("", macCodexExecutableCandidates(cli, os.Getenv("PATH"), home), codexExecutableVersion)
}

func macCodexExecutableCandidates(cli, searchPath, home string) []string {
	candidates := []string{cli}
	for _, directory := range filepath.SplitList(searchPath) {
		if filepath.IsAbs(directory) {
			candidates = append(candidates, filepath.Join(directory, "codex"))
		}
	}
	for _, directory := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		candidates = append(candidates, filepath.Join(directory, "codex"))
	}
	applications := []string{"/Applications"}
	if filepath.IsAbs(home) {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "codex"), filepath.Join(home, ".npm-global", "bin", "codex"))
		applications = append(applications, filepath.Join(home, "Applications"))
	}
	for _, directory := range applications {
		for _, bundle := range []string{"ChatGPT.app", "Codex.app"} {
			resources := filepath.Join(directory, bundle, "Contents", "Resources")
			candidates = append(candidates, filepath.Join(resources, "codex-cli", "bin", "codex"), filepath.Join(resources, "codex"))
		}
	}
	return candidates
}

// Prefer the newest installed runtime without changing either app's packages.
func selectCodexExecutable(override string, candidates []string, version func(string) string) (string, error) {
	if override = strings.TrimSpace(override); override != "" {
		return exec.LookPath(override)
	}
	selected, selectedVersion := "", ""
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		path, err := exec.LookPath(candidate)
		if err != nil || seen[path] {
			continue
		}
		seen[path] = true
		current := version(path)
		if selected == "" || codexVersionNewer(current, selectedVersion) {
			selected, selectedVersion = path, current
		}
	}
	if selected == "" {
		return "", exec.ErrNotFound
	}
	return selected, nil
}

var codexVersionPattern = regexp.MustCompile(`(?m)^codex-cli ([0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?)(?:\s|$)`)

func codexVersionNewer(candidate, current string) bool {
	if candidate == "" {
		return false
	}
	if current == "" {
		return true
	}
	left, leftPrerelease, _ := strings.Cut(candidate, "-")
	right, rightPrerelease, _ := strings.Cut(current, "-")
	leftParts, rightParts := strings.Split(left, "."), strings.Split(right, ".")
	if len(leftParts) != 3 || len(rightParts) != 3 {
		return false
	}
	for index := range leftParts {
		a, _ := strconv.Atoi(leftParts[index])
		b, _ := strconv.Atoi(rightParts[index])
		if a != b {
			return a > b
		}
	}
	return leftPrerelease == "" && rightPrerelease != ""
}

type codexVersionCacheEntry struct {
	size, modified int64
	mode           os.FileMode
	version        string
}

var codexVersionCache = struct {
	sync.Mutex
	entries map[string]codexVersionCacheEntry
}{entries: map[string]codexVersionCacheEntry{}}

var codexVersionCommand = exec.CommandContext

func codexExecutableVersion(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	key := codexVersionCacheEntry{size: info.Size(), modified: info.ModTime().UnixNano(), mode: info.Mode()}
	codexVersionCache.Lock()
	defer codexVersionCache.Unlock()
	if saved, ok := codexVersionCache.entries[path]; ok && saved.size == key.size && saved.modified == key.modified && saved.mode == key.mode {
		return saved.version
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := codexVersionCommand(ctx, path, "--version")
	cmd.Env = codexRuntimeEnvironment(os.Environ())
	cmd.Stderr = io.Discard
	output := &codexVersionOutput{}
	cmd.Stdout = output
	cmd.WaitDelay = 250 * time.Millisecond
	configureCursorCancellation(cmd)
	if cmd.Run() == nil {
		if match := codexVersionPattern.FindSubmatch(output.data); len(match) == 2 {
			key.version = string(match[1])
		}
	}
	codexVersionCache.entries[path] = key
	return key.version
}

type codexVersionOutput struct{ data []byte }

func (output *codexVersionOutput) Write(data []byte) (int, error) {
	if remaining := 4096 - len(output.data); remaining > 0 {
		output.data = append(output.data, data[:min(remaining, len(data))]...)
	}
	return len(data), nil
}
