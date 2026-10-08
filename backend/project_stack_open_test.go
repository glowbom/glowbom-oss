package main

import (
	"net/http"
	"path/filepath"
	"testing"
)

func TestStackOpenRejectsUnknownEditorAndUnavailableTarget(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	previewFixture(t, root, "prototype/index.html", "hello")
	manager := newProjectPreviewManager()
	for _, request := range []previewRequest{
		{Path: root, Target: "prototype", Action: "open", Editor: "sh"},
		{Path: root, Target: "prototype", Action: "open", Editor: "../cursor"},
		{Path: root, Target: "web", Action: "tools"},
		{Path: root, Target: "web", Action: "open", Editor: "folder"},
	} {
		_, status := previewCall(t, manager, request)
		if status != http.StatusBadRequest {
			t.Fatalf("unexpected status %d for %#v", status, request)
		}
	}
}

func TestInstalledStackEditorsUseKnownIDs(t *testing.T) {
	for _, editor := range installedStackEditors() {
		command, ok := stackEditorCommand(editor.ID)
		if !ok || command.Executable == "" {
			t.Fatalf("unlaunchable editor %#v", editor)
		}
	}
	if _, ok := stackEditorCommand("/bin/sh"); ok {
		t.Fatal("arbitrary executables must not be accepted")
	}
}

func TestDiscoveredNativeFolderTools(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	previewFixture(t, root, "apple/Custom.xcodeproj/project.pbxproj", "project")
	previewFixture(t, root, "android/build.gradle.kts", "plugins {}")
	previewFixture(t, root, "android/app/build.gradle.kts", `plugins { id("com.android.application") }`)
	defs, err := readPreviewDefinitions(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"apple", "android"} {
		found := discoveredPreviewDefinition(root, defs, id)
		if found == nil || found.Directory != id {
			t.Fatalf("missing %s folder: %#v", id, found)
		}
	}
	manager := newProjectPreviewManager()
	defer manager.Close()
	for _, id := range []string{"apple", "android"} {
		_, status := previewCall(t, manager, previewRequest{Path: root, Target: id, Action: "tools"})
		if status != http.StatusOK {
			t.Fatalf("%s tools status %d", id, status)
		}
	}
}

func TestProjectRootToolsAndActionScope(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := newProjectPreviewManager()
	defer manager.Close()
	_, status := previewCall(t, manager, previewRequest{Path: root, ProjectRoot: true, Action: "tools"})
	if status != http.StatusOK {
		t.Fatalf("root tools failed: %d", status)
	}
	for _, request := range []previewRequest{
		{Path: root, ProjectRoot: true, Action: "start"},
		{Path: root, ProjectRoot: true, Action: "open", Editor: "/bin/sh"},
	} {
		_, status := previewCall(t, manager, request)
		if status != http.StatusBadRequest {
			t.Fatalf("unsafe root action accepted: %d", status)
		}
	}
}
