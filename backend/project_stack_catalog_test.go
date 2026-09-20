package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStackCatalogPreservesLegacyAndNativeModes(t *testing.T) {
	project, _ := filepath.EvalSymlinks(t.TempDir())
	previewFixture(t, project, "cli/index.html", "Documentation, not an app")
	native := previewDefinition{ID: "custom-cli", Name: "CLI", Directory: "cli", PreviewMode: "none", PreviewNotes: "Run python3 main.py --help", Description: "Build a CLI"}
	legacy := previewDefinition{ID: "custom-legacy", Name: "Legacy", Directory: "cli", Command: []string{"python3", "serve.py", "{host}", "{port}"}}
	stackBuildFixture(t, project, native, legacy)
	defs, err := readPreviewDefinitions(project)
	if err != nil || defs[2].PreviewNotes != native.PreviewNotes {
		t.Fatalf("roundtrip: %v %+v", err, defs)
	}
	if v := inspectPreviewDefinition(project, defs[2]); v.Available || v.Kind != "" || v.PreviewMode != "none" {
		t.Fatalf("native became a browser preview: %+v", v)
	}
	if v := inspectPreviewDefinition(project, defs[3]); !v.Available || v.Kind != "custom" {
		t.Fatalf("legacy command lost: %+v", v)
	}
	m := newProjectPreviewManager()
	defer m.Close()
	if _, code := previewCall(t, m, previewRequest{Path: project, Target: native.ID, Action: "start"}); code != 400 {
		t.Fatalf("native browser start: %d", code)
	}
	prompt, err := prepareStackBuildInstructions(project, "Build", []string{native.ID})
	if err != nil || !strings.Contains(prompt, native.PreviewNotes) || !strings.Contains(prompt, "no browser preview") {
		t.Fatalf("lost native instructions: %v %s", err, prompt)
	}
}

func TestStackCatalogRejectsInvalidPreviewSettings(t *testing.T) {
	for _, d := range []previewDefinition{
		{PreviewMode: "unknown"}, {PreviewMode: "command"},
		{PreviewMode: "none", Command: []string{"python3", "{host}", "{port}"}},
		{PreviewNotes: strings.Repeat("x", 2001)},
	} {
		d.ID = "custom-test"
		d.Name = "Test"
		d.Directory = "."
		if validatePreviewDefinition(d) == nil {
			t.Fatalf("accepted %+v", d)
		}
	}
}

func TestStackTerminalRejectsOutsideFolders(t *testing.T) {
	project, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.Symlink(t.TempDir(), filepath.Join(project, "escape")); err != nil {
		t.Fatal(err)
	}
	stackBuildFixture(t, project, previewDefinition{ID: "custom-escape", Name: "Escape", Directory: "escape", PreviewMode: "none"})
	m := newProjectPreviewManager()
	defer m.Close()
	if _, code := previewCall(t, m, previewRequest{Path: project, Target: "custom-escape", Action: "terminal"}); code != 400 {
		t.Fatalf("outside folder accepted: %d", code)
	}
}

func TestStackTerminalUsesLiteralPathAndFilteredEnvironment(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "folder with spaces; $(echo nope)")
	t.Setenv("GLOWBOM_SERVER_TOKEN", "must-not-leak")
	cmd, ok := stackTerminalCommand(directory)
	if !ok {
		t.Skip("No supported terminal on this host")
	}
	found := false
	for _, arg := range cmd.Args {
		if arg == directory || arg == "--working-directory="+directory {
			found = true
		}
	}
	if !found {
		t.Fatalf("path was not a literal argument: %v", cmd.Args)
	}
	for _, env := range cmd.Env {
		if strings.Contains(env, "must-not-leak") {
			t.Fatal("terminal inherited backend token")
		}
	}
}
