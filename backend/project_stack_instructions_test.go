package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stackBuildFixture(t *testing.T, project string, custom ...previewDefinition) {
	t.Helper()
	defs := append([]previewDefinition{{ID: "prototype"}, {ID: "web"}}, custom...)
	if err := savePreviewDefinitions(project, defs); err != nil {
		t.Fatal(err)
	}
}

func TestStackInstructionsRespectSelectionAndReloadSavedDescriptions(t *testing.T) {
	project := t.TempDir()
	d := previewDefinition{ID: "custom-react", Name: "React app", Directory: "apps/react", Description: "Use React, TypeScript, Vite, and plain CSS.", Preset: "react-vite"}
	other := previewDefinition{ID: "custom-php", Name: "PHP", Directory: "php", Description: "Use PHP and Twig."}
	stackBuildFixture(t, project, d, other)
	text, err := prepareStackBuildInstructions(project, "Add a login screen.", []string{d.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Add a login screen.", d.Description, "Output folder: apps/react", "If this target is missing, create", "Do not modify these unselected targets: prototype/, apple/, android/, web/, php/"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, other.Description) {
		t.Fatal("sent an unselected stack's instructions")
	}
	d.Description = "Use React and CSS Modules for subsequent changes."
	stackBuildFixture(t, project, d, other)
	updated, err := prepareStackBuildInstructions(project, "Change the login button.", []string{d.ID})
	if err != nil || !strings.Contains(updated, d.Description) || strings.Contains(updated, "plain CSS") {
		t.Fatalf("follow-up did not use saved edits: %v %s", err, updated)
	}
	for _, ids := range [][]string{{}, {"custom-missing"}, {"../escape"}} {
		if _, err := prepareStackBuildInstructions(project, "Build", ids); err == nil {
			t.Fatalf("accepted invalid selection %v", ids)
		}
	}
	previewFixture(t, project, ".glowbom/previews.json", "{")
	if got, err := prepareStackBuildInstructions(project, "Legacy request", nil); err != nil || got != "Legacy request" {
		t.Fatal("changed behavior for a client without buildTargets")
	}
}

func TestStackInstructionsDoNotExcludeContainingTarget(t *testing.T) {
	project := t.TempDir()
	for _, folder := range []string{"web/admin", "."} {
		d := previewDefinition{ID: "custom-app", Name: "App", Directory: folder, Description: "Use the existing Vite app."}
		stackBuildFixture(t, project, d)
		text, err := prepareStackBuildInstructions(project, "Update", []string{d.ID})
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "Do not modify these unselected targets:") && strings.Contains(line, "web/") {
				t.Fatalf("forbids the selected folder's ancestor: %s", line)
			}
		}
	}
}

func TestStackInstructionsReachAgentStagingAndHistory(t *testing.T) {
	project := t.TempDir()
	previewFixture(t, project, "glowbom.json", `{"name":"Test"}`)
	d := previewDefinition{ID: "custom-desktop", Name: "Desktop", Directory: "desktop", Description: "Use Tauri 2 and React. Keep native file calls behind browser fallbacks.", Preset: "tauri-react"}
	stackBuildFixture(t, project, d)
	bin := fakeCursor(t, `cat > received-prompt.txt
printf '%s\n' '{"type":"result","subtype":"success","result":"Reviewed target"}'
`)
	t.Setenv("GLOWBOM_CURSOR_BIN", bin)
	payload, _ := json.Marshal(OpenCodeAgentRequest{AgentDriver: "cursor", ProjectPath: project, Instructions: "Build the app", BuildTargets: []string{d.ID}, PersistCurrentInstructionsToHistory: true})
	w := httptest.NewRecorder()
	openCodeRefineHandler(w, httptest.NewRequest("POST", "/opencode/refine", strings.NewReader(string(payload))))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, name := range []string{"received-prompt.txt", "current_instructions/instructions.txt"} {
		content, err := os.ReadFile(filepath.Join(project, name))
		if err != nil || !strings.Contains(string(content), d.Description) || !strings.Contains(string(content), "Build the app") {
			t.Fatalf("missing saved stack context in %s: %v", name, err)
		}
	}
	found := false
	_ = filepath.WalkDir(filepath.Join(project, "history"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			content, _ := os.ReadFile(path)
			found = found || strings.Contains(string(content), d.Description)
		}
		return nil
	})
	if !found {
		t.Fatal("history lost the saved stack context")
	}
}
