package main

import (
	"fmt"
	"os"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const usage = `glowbom - terminal-first local AI coding agent for Glowbom OSS

Usage:
  glowbom start [project-path] [--show-local-auth]
                                Start Glowbom OSS from this checkout and open the browser
  glowbom doctor                Check environment dependencies
  glowbom version               Print version info
  glowbom help                  Show this help

Examples:
  glowbom start                   Start Glowbom OSS from the current checkout
  glowbom start --show-local-auth Start Glowbom OSS and print local dev auth credentials
  glowbom start /path/to/project  Start Glowbom OSS and print a project path hint

Compatibility:
  glowbom code [project-path]     Deprecated alias for glowbom start
`

func main() {
	args := os.Args[1:]
	os.Exit(run(args))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 0
	}

	switch args[0] {
	case "start":
		return runStart(args[1:])
	case "code":
		fmt.Fprintln(os.Stderr, "warning: `glowbom code` is deprecated; use `glowbom start`")
		return runStart(args[1:])
	case "doctor":
		return runDoctor()
	case "version":
		return runVersion()
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", args[0])
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}
