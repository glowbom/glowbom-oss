package main

import (
	"fmt"
	"os/exec"
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
		{name: "opencode", required: true, fixHint: "Install: bun install -g opencode-ai (or see https://opencode.ai)"},
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

func commandVersion(name string) string {
	out, err := exec.Command(name, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
