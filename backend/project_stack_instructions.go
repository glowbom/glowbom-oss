package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Resolve saved stack descriptions on every run, including resumed sessions.
// Both OpenCode and Cursor receive this text before staging and history capture.
func prepareStackBuildInstructions(project, instructions string, selectedIDs []string) (string, error) {
	if selectedIDs == nil {
		return instructions, nil
	}
	if len(selectedIDs) == 0 || len(selectedIDs) > 24 {
		return "", errors.New("Select at least one build target.")
	}
	defs, err := readPreviewDefinitions(project)
	if err != nil {
		return "", err
	}
	targets := []previewDefinition{
		{ID: "prototype", Name: "Prototype", Directory: "prototype"},
		{ID: "apple", Name: "Apple", Directory: "apple"},
		{ID: "android", Name: "Android", Directory: "android"},
		{ID: "web", Name: "Web", Directory: "web"},
	}
	targets = append(targets, defs[2:]...)
	byID := make(map[string]previewDefinition, len(targets))
	for _, target := range targets {
		byID[target.ID] = target
	}
	chosen := make(map[string]bool, len(selectedIDs))
	var selected []previewDefinition
	var labels []string
	for _, id := range selectedIDs {
		target, ok := byID[id]
		if !ok {
			return "", errors.New("A selected stack is no longer available. Reload the project and choose its build targets again.")
		}
		if !chosen[id] {
			selected = append(selected, target)
			labels = append(labels, fmt.Sprintf("%s (%s/)", target.Name, filepath.ToSlash(filepath.Clean(target.Directory))))
			chosen[id] = true
		}
	}
	lines := []string{strings.TrimSpace(instructions), "", "[Selected build targets]", strings.Join(labels, ", ")}
	if len(chosen) < len(targets) {
		lines = append(lines, "IMPORTANT: Only edit code in the selected target folders. Other folders may be read as reference.")
		var excluded []string
		for _, other := range targets {
			if chosen[other.ID] {
				continue
			}
			overlap := false
			for _, target := range selected {
				if stackFoldersOverlap(target.Directory, other.Directory) {
					overlap = true
					break
				}
			}
			if !overlap {
				excluded = append(excluded, filepath.ToSlash(filepath.Clean(other.Directory))+"/")
			}
		}
		if len(excluded) > 0 {
			lines = append(lines, "Do not modify these unselected targets: "+strings.Join(excluded, ", ")+".")
		}
	}
	for _, target := range selected {
		if !strings.HasPrefix(target.ID, "custom-") {
			continue
		}
		lines = append(lines, "", fmt.Sprintf("[Saved stack: %s]", target.Name), "Output folder: "+filepath.ToSlash(filepath.Clean(target.Directory)))
		if target.Description != "" {
			lines = append(lines, "Stack description:", target.Description)
		}
		lines = append(lines,
			"If this target is missing, create a working implementation using the existing prototype, project intent, and this run's request as reference. If it exists, refine it in place.",
			"Existing apps do not need the standard prototype or platform folders. If no prototype exists, use the app's README, current behavior, and this run's request. Do not create unselected platform folders.",
			"Preserve existing project and export formats. Read applicable AGENTS.md instructions and the target's README and package configuration; keep its package manager and working dependencies unless a change is requested.",
			"Keep application logic separate from presentation where practical. Verify the target with its own tests and build commands, and report checks that could not run.",
			"These saved choices describe the stack; the current user request determines the change to make. Do not edit another target merely to make it match.")
		if len(target.Command) > 0 {
			lines = append(lines, fmt.Sprintf("Preview launch arguments: %q", target.Command), "Glowbom substitutes {host} with 127.0.0.1 and {port} with a free port. Do not hard-code the preview port.")
		}
		if target.PreviewMode == "none" {
			lines = append(lines, "This target has no browser preview. Document native or terminal run commands; do not create a web server solely for preview.")
		}
		if target.PreviewNotes != "" {
			lines = append(lines, "Saved setup and preview notes:", target.PreviewNotes)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}

func stackFoldersOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == "." || b == "." || a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}
