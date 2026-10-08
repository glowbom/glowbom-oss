package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func postProjectRename(t *testing.T, path, name string, expected *string) bool {
	t.Helper()
	body := map[string]any{"path": path, "name": name}
	if expected != nil {
		body["expectedName"] = *expected
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Error(err)
		return false
	}
	response := httptest.NewRecorder()
	openCodeRenameProjectHandler(response, httptest.NewRequest("POST", "/opencode/project/rename", bytes.NewReader(data)))
	var result struct {
		Success bool `json:"success"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Errorf("invalid rename response: %d %s", response.Code, response.Body.String())
	}
	return result.Success
}

func TestProjectAutomaticRenamePreservesManualName(t *testing.T) {
	root, err := createSketchProject(t.TempDir(), "Suggested name")
	if err != nil {
		t.Fatal(err)
	}
	initial := "Suggested name"
	if !postProjectRename(t, root, "Prototype title", &initial) {
		t.Fatal("matching automatic rename failed")
	}
	if !postProjectRename(t, root, "My chosen name", nil) {
		t.Fatal("manual rename failed")
	}
	before, err := os.ReadFile(filepath.Join(root, "glowbom.json"))
	if err != nil {
		t.Fatal(err)
	}
	stale := "Prototype title"
	if postProjectRename(t, root, "Another generated title", &stale) {
		t.Fatal("stale automatic rename replaced manual name")
	}
	after, err := os.ReadFile(filepath.Join(root, "glowbom.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected automatic rename changed manifest", err)
	}
	project, err := LoadProject(filepath.Join(root, "glowbom.json"))
	if err != nil || project.Name != "My chosen name" {
		t.Fatal(project, err)
	}
}

func TestProjectConcurrentManualRenameWinsOverAutomaticName(t *testing.T) {
	for iteration := 0; iteration < 12; iteration++ {
		root, err := createSketchProject(t.TempDir(), "Suggested name")
		if err != nil {
			t.Fatal(err)
		}
		initial := "Suggested name"
		var group sync.WaitGroup
		start := make(chan struct{})
		group.Add(2)
		go func() { defer group.Done(); <-start; postProjectRename(t, root, "Prototype title", &initial) }()
		go func() {
			defer group.Done()
			<-start
			if !postProjectRename(t, root, "My chosen name", nil) {
				t.Error("manual rename failed")
			}
		}()
		close(start)
		group.Wait()
		project, err := LoadProject(filepath.Join(root, "glowbom.json"))
		if err != nil || project.Name != "My chosen name" {
			t.Fatal("automatic rename overwrote owner name", project, err)
		}
	}
}
