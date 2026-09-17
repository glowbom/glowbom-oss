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
  glowbom login [--no-browser] [--device-auth]
                              Connect your optional Glowbom account
  glowbom account [--refresh]   Show your hosted account allowance
  glowbom logout               Remove this computer's account credentials
  glowbom generate-image [options] "prompt"
                              Generate and save an image, with optional references
  glowbom pull [--output NEW_DIRECTORY]
                              Download your saved project into a new local folder
  glowbom template [--output NEW_DIRECTORY]
                              Download a clean starter project without signing in
  glowbom export [--output NEW_DIRECTORY]
                              Combine your saved generation and starter into one project
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
	case "login", "account", "logout":
		return runAccountCommand(args[0], args[1:])
	case "generate-image":
		return runImageCommand(args[1:])
	case "pull":
		return runPullCommand(args[1:])
	case "template":
		return runTemplateCommand(args[1:])
	case "export":
		return runExportCommand(args[1:])
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
