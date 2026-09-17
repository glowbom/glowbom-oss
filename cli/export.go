package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
)

const exportUsage = `Usage: glowbom export [--output NEW_DIRECTORY]

Download your saved generation and the current starter as one complete project.
Sign in with glowbom login and finish saving your project in Glowbom first.
Generated HTML, SwiftUI, Kotlin, and Next.js files go into their platform locations.
Your original exports, prompt, and any saved drawing or icon are also preserved.

Without --output, creates a unique glowbom-export-* folder in the current directory.
The output's parent must already exist. Existing files and folders are never replaced.
This does not generate missing code, run downloaded code, install dependencies,
or synchronize future changes. Use glowbom pull for only the saved generation,
or glowbom template for only the clean starter.
`

func parseExportOptions(args []string) (pullOptions, error) {
	opts, err := parsePullOptions(args)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		return opts, errors.New(strings.ReplaceAll(err.Error(), "pull only accepts", "export only accepts"))
	}
	return opts, err
}

func (c *accountClient) exportProject(ctx context.Context, opts pullOptions) error {
	if ctx.Err() != nil {
		return errors.New("project export canceled")
	}
	credentials, err := c.store.Load()
	if err != nil {
		return err
	}
	if !credentials.valid() {
		return errors.New("you are not signed in; run glowbom login first")
	}
	output, err := prepareDirectoryOutput(opts.Output, "glowbom-export")
	if err != nil {
		return err
	}
	defer output.Close()
	saved, err := c.downloadSavedProject(ctx, credentials)
	if err != nil {
		return err
	}
	// Reject an invalid saved bundle before requesting the public starter.
	if _, err := validateProjectArchive(ctx, saved); err != nil {
		return err
	}
	fmt.Fprintln(c.out, "Downloading starter and assembling project...")
	starter, err := downloadTemplate(ctx, c.http)
	if err != nil {
		return err
	}
	entries, err := assembleProject(ctx, saved, starter)
	if err != nil {
		return err
	}
	path, count, err := output.extractEntries(ctx, entries)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Exported project: %s\nSaved %d files with your generated code in its platform locations.\n", path, count)
	return nil
}

func runExportCommand(args []string) int {
	opts, err := parseExportOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(exportUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Glowbom:", err)
		fmt.Fprint(os.Stderr, exportUsage)
		return 2
	}
	c, err := newAccountClient()
	if err == nil {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		err = c.exportProject(ctx, opts)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Glowbom:", strings.ReplaceAll(err.Error(), "glowbom pull", "glowbom export"))
		return 1
	}
	return 0
}
