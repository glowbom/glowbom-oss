package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCreateSketchProject(t *testing.T) {
	parent := t.TempDir()
	root, err := createSketchProject(parent, "My sketch")
	if err != nil {
		t.Fatal(err)
	}
	project, err := LoadProject(GetProjectPaths(root).Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if project.Name != "My sketch" || len(project.Targets) != 0 {
		t.Fatalf("unexpected project: %+v", project)
	}
	if _, err := os.Stat(GetProjectPaths(root).Assets); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareStackBuildInstructions(root, "Build the sketch", []string{"prototype"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(GetProjectPaths(root).Manifest)
	if _, err := createSketchProject(parent, "My sketch"); err == nil {
		t.Fatal("must reject existing folder")
	}
	after, _ := os.ReadFile(GetProjectPaths(root).Manifest)
	if !bytes.Equal(before, after) {
		t.Fatal("existing manifest changed")
	}
}

func TestCheckSketchProjectDoesNotCreateAndSuggestsAvailableName(t *testing.T) {
	parent := t.TempDir()
	result, err := checkSketchProject(parent, "Garden")
	if err != nil || result["exists"] != false {
		t.Fatal(result, err)
	}
	if _, err := os.Stat(result["path"].(string)); !os.IsNotExist(err) {
		t.Fatal("preflight created a folder", err)
	}
	if err := os.Mkdir(filepath.Join(parent, "Garden"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "Garden 2"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parent, "missing"), filepath.Join(parent, "Garden 3")); err != nil {
		t.Fatal(err)
	}
	result, err = checkSketchProject(parent, "Garden")
	if err != nil || result["exists"] != true || result["suggestedName"] != "Garden 4" {
		t.Fatal(result, err)
	}
	if _, err := checkSketchProject(parent, "../outside"); err == nil {
		t.Fatal("unsafe name accepted")
	}
	if _, err := checkSketchProject(filepath.Join(parent, "missing"), "Garden"); err == nil {
		t.Fatal("missing parent accepted")
	}
}

func TestCheckSketchProjectBoundsUnicodeSuggestion(t *testing.T) {
	parent := t.TempDir()
	name := strings.Repeat("花", 33)
	if err := os.Mkdir(filepath.Join(parent, name), 0755); err != nil {
		t.Fatal(err)
	}
	result, err := checkSketchProject(parent, name)
	if err != nil {
		t.Fatal(err)
	}
	suggestion := result["suggestedName"].(string)
	if len(suggestion) > 100 || !utf8.ValidString(suggestion) || !strings.HasSuffix(suggestion, " 2") {
		t.Fatal(suggestion)
	}
	if _, err := createSketchProject(parent, suggestion); err != nil {
		t.Fatal(err)
	}
}

func TestCheckSketchProjectEndpointIsReadOnly(t *testing.T) {
	parent := t.TempDir()
	body, _ := json.Marshal(map[string]interface{}{"parentPath": parent, "name": "Garden"})
	w := httptest.NewRecorder()
	openCodeCheckProjectHandler(w, httptest.NewRequest(http.MethodPost, "/opencode/project/check", bytes.NewReader(body)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"exists":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("preflight wrote files", entries, err)
	}
}

func TestCreateSketchProjectRejectsUnsafePaths(t *testing.T) {
	parent := t.TempDir()
	for _, name := range []string{"", ".", "..", "../outside", "a/b", `a\b`, ".hidden", "trailing ", "a\nname", "a\x00name"} {
		if _, err := createSketchProject(parent, name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if _, err := createSketchProject("relative", "app"); err == nil {
		t.Fatal("accepted relative parent")
	}
	if _, err := createSketchProject(filepath.Join(parent, "missing"), "app"); err == nil {
		t.Fatal("accepted missing parent")
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(parent, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := createSketchProject(parent, "linked"); err == nil {
		t.Fatal("overwrote symlink")
	}
	entries, _ := os.ReadDir(target)
	if len(entries) != 0 {
		t.Fatal("wrote through an existing symlink")
	}
}

func TestCreateSketchProjectEndpoint(t *testing.T) {
	parent := t.TempDir()
	body, _ := json.Marshal(map[string]string{"parentPath": parent, "name": "Test"})
	w := httptest.NewRecorder()
	createStarterProjectHandler(starterFixtureRunner(t, nil))(w, httptest.NewRequest(http.MethodPost, "/opencode/project/create", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Success bool
		Path    string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.Success || result.Path == "" {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	openCodeCreateProjectHandler(w, httptest.NewRequest(http.MethodGet, "/opencode/project/create", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w.Code)
	}
}
