package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		{name: "opencode", required: false, fixHint: "Install: bun install -g opencode-ai (or use Cursor CLI)"},
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

	if _, err := exec.LookPath("opencode"); err != nil {
		if !cursorCLIAvailable() {
			issues++
			fmt.Println("  [MISSING] Coding agent: install OpenCode or Cursor CLI (https://cursor.com/docs/cli/installation)")
		} else {
			fmt.Println("  [ok]    Cursor CLI found. Run cursor-agent login and choose Cursor in Settings.")
		}
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

func commandVersion(name string) string {
	out, err := exec.Command(name, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
