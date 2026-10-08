package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/glowbom/glowbom-oss/cli/opencodecompat"
)

type depCheck struct {
	name     string
	required bool
	fixHint  string
}

func runDoctor() int {
	checks := []depCheck{
		{name: "go", required: true, fixHint: "Install: https://go.dev/dl/"},
		{name: "bun", required: true, fixHint: "Install: https://bun.sh/"},
	}

	issues := 0
	for _, c := range checks {
		path, err := exec.LookPath(c.name)
		if err != nil {
			tag := "missing"
			if c.required {
				tag = "MISSING"
				issues++
			}
			fmt.Printf("  [%s] %s\n", tag, c.name)
			fmt.Printf("          %s\n", c.fixHint)
		} else {
			ver := commandVersion(c.name)
			if ver != "" {
				fmt.Printf("  [ok]    %s %s (%s)\n", c.name, ver, path)
			} else {
				fmt.Printf("  [ok]    %s (%s)\n", c.name, path)
			}
		}
	}

	if runtime.GOOS == "linux" && !reportLinuxFolderPicker(os.Stdout, exec.LookPath) {
		issues++
	}

	agentRuntime, runtimeErr := opencodecompat.Resolve(context.Background())
	if runtimeErr == nil {
		selection, _ := opencodecompat.Selection()
		fmt.Printf("  [ok]    OpenCode %s, %s adapter (%s)\n", agentRuntime.Version, agentRuntime.Protocol, agentRuntime.Executable)
		fmt.Printf("          Version preference: %s\n", selection)
	} else {
		fmt.Printf("  [missing] OpenCode: %s\n", runtimeErr)
	}
	if !reportCodingAgents(os.Stdout, runtimeErr == nil) {
		issues++
	}

	if root, err := findGlowbomRoot(); err != nil {
		issues++
		fmt.Println("  [MISSING] Glowbom OSS checkout")
		fmt.Println("            Could not find sibling backend/ and web/ directories.")
		fmt.Println("            Clone https://github.com/glowbom/glowbom-oss and run `glowbom start` from the repo root.")
	} else {
		fmt.Printf("  [ok]    Glowbom OSS checkout (%s)\n", root)
	}

	fmt.Println()
	if issues > 0 {
		fmt.Printf("%d required dependency(ies) missing. Fix the above and retry.\n", issues)
		return 1
	}
	fmt.Println("All checks passed.")
	return 0
}

func reportCodingAgents(output io.Writer, openCodeAvailable bool) bool {
	available := openCodeAvailable
	for _, agent := range []struct {
		installed bool
		message   string
	}{
		{cursorCLIAvailable(), "Cursor CLI found. Run cursor-agent login and choose Cursor in Settings."},
		{claudeCodeCLIAvailable(), "Claude Code found. Run claude to sign in and choose Claude Code in Build."},
		{codexCLIAvailable(), "Codex found. Run codex login and choose Codex in Build."},
	} {
		if agent.installed {
			available = true
			fmt.Fprintf(output, "  [ok]    %s\n", agent.message)
		}
	}
	if !available {
		fmt.Fprintln(output, "  [MISSING] Coding agent: install OpenCode, Cursor CLI, Claude Code, or Codex.")
	}
	return available
}

func cursorCLIAvailable() bool {
	if configured := strings.TrimSpace(os.Getenv("GLOWBOM_CURSOR_BIN")); configured != "" {
		_, err := exec.LookPath(configured)
		return err == nil
	}
	if _, err := exec.LookPath("cursor-agent"); err == nil {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = exec.LookPath(filepath.Join(home, ".local", "bin", "cursor-agent"))
	return err == nil
}

func claudeCodeCLIAvailable() bool {
	if configured := strings.TrimSpace(os.Getenv("GLOWBOM_CLAUDE_CODE_BIN")); configured != "" {
		_, err := exec.LookPath(configured)
		return err == nil
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := exec.LookPath(filepath.Join(home, ".local", "bin", name)); err == nil {
			return true
		}
	}
	_, err := exec.LookPath(name)
	return err == nil
}

func codexCLIAvailable() bool {
	if configured := strings.TrimSpace(os.Getenv("GLOWBOM_CODEX_BIN")); configured != "" {
		_, err := exec.LookPath(configured)
		return err == nil
	}
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if _, err := exec.LookPath(name); err == nil {
		return true
	}
	candidates := []string{
		filepath.Join("/opt/homebrew/bin", name),
		filepath.Join("/usr/local/bin", name),
	}
	applications := []string{"/Applications"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin", name),
			filepath.Join(home, ".npm-global", "bin", name))
		applications = append(applications, filepath.Join(home, "Applications"))
	}
	if runtime.GOOS == "darwin" {
		for _, directory := range applications {
			for _, bundle := range []string{"ChatGPT.app", "Codex.app"} {
				resources := filepath.Join(directory, bundle, "Contents", "Resources")
				candidates = append(candidates, filepath.Join(resources, "codex-cli", "bin", name), filepath.Join(resources, name))
			}
		}
	}
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate); err == nil {
			return true
		}
	}
	return false
}

func commandVersion(name string) string {
	out, err := exec.Command(name, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
