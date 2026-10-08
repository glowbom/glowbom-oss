package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStudioProjectMetadataCacheRefreshesChangedRecords(t *testing.T) {
	studio := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	directory := filepath.Join(studio, "Projects")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "record.json")
	write := func(destination, name string) {
		t.Helper()
		data, _ := json.Marshal(map[string]string{"id": "AAAAAAAA-1111-2222-3333-444444444444", "projectName": name, "timestamp": "2026-01-01T00:00:00Z", "largeIgnoredHistory": "Only registry fields belong in the cache."})
		if err := os.WriteFile(destination, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(name string) {
		t.Helper()
		projects, err := listRegisteredStudioProjects()
		if err != nil || len(projects) != 1 || projects[0].Name != name {
			t.Fatal("registered metadata was stale or missing", err)
		}
	}
	write(path, "Alpha")
	check("Alpha")
	check("Alpha")
	previous, _ := os.Stat(path)
	write(path, "Bravo")
	changed := previous.ModTime().Add(time.Second)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	check("Bravo")
	previous, _ = os.Stat(path)
	replacement := filepath.Join(directory, "replacement")
	write(replacement, "Delta")
	if err := os.Chtimes(replacement, previous.ModTime(), previous.ModTime()); err != nil {
		t.Fatal(err)
	}
	replaced, _ := os.Stat(replacement)
	if previous.Size() != replaced.Size() || !previous.ModTime().Equal(replaced.ModTime()) || os.SameFile(previous, replaced) {
		t.Fatal("atomic replacement fixture does not preserve size and modification time")
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	check("Delta")
	previous, _ = os.Stat(path)
	write(path, "A longer project name")
	if err := os.Chtimes(path, previous.ModTime(), previous.ModTime()); err != nil {
		t.Fatal(err)
	}
	check("A longer project name")
}

func TestStudioProjectMetadataCachePrunesDeletedLinkedAndOtherRoots(t *testing.T) {
	studio := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	root := studioTestProject(t, "Registered project")
	project, err := registerStudioProject(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(studio, "DesktopProjects", project.ID+".json")
	if _, err := listRegisteredStudioProjects(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "record.json")
	if err := os.Rename(path, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	checkEmpty := func() {
		t.Helper()
		projects, err := listRegisteredStudioProjects()
		if err != nil || len(projects) != 0 {
			t.Fatal("deleted, linked or previous-root record stayed visible", err)
		}
		studioProjectMu.Lock()
		count := len(studioProjectMetadataCache.entries)
		studioProjectMu.Unlock()
		if count != 0 {
			t.Fatal("invalid registry entry remained cached")
		}
	}
	checkEmpty()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := listRegisteredStudioProjects(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	checkEmpty()
	if _, err := registerStudioProject(root); err != nil {
		t.Fatal(err)
	}
	if _, err := listRegisteredStudioProjects(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	checkEmpty()
}

func TestStudioProjectMetadataCacheRevalidatesProjectFolderAndName(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	root := studioTestProject(t, "First name")
	if _, err := registerStudioProject(root); err != nil {
		t.Fatal(err)
	}
	check := func(name string, available bool) {
		t.Helper()
		projects, err := listRegisteredStudioProjects()
		if err != nil || len(projects) != 1 || projects[0].Name != name || projects[0].Available != available {
			t.Fatal("cached registry record hid a live project change", err)
		}
	}
	check("First name", true)
	manifest := GetProjectPaths(root).Manifest
	project, err := LoadProject(manifest)
	if err != nil {
		t.Fatal(err)
	}
	project.Name = "Updated name"
	if err := SaveProject(manifest, project); err != nil {
		t.Fatal(err)
	}
	check("Updated name", true)
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	check("First name", false)
	if err := os.Rename(moved, root); err != nil {
		t.Fatal(err)
	}
	check("Updated name", true)
	if err := os.WriteFile(filepath.Join(root, ".glowbom", "studio.json"), []byte(`{"projectID":"BBBBBBBB-1111-2222-3333-444444444444"}`), 0600); err != nil {
		t.Fatal(err)
	}
	check("First name", false)
}

func TestStudioProjectMetadataCachePreservesOptionalFieldCompatibility(t *testing.T) {
	studio := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	directory := filepath.Join(studio, "Projects")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "record.json")
	data := []byte(`{"id":"AAAAAAAA-1111-2222-3333-444444444444","projectName":{"legacy":"name"},"exportedProjectPath":123,"timestamp":42,"history":[{"text":"ignored"}]}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		projects, err := listRegisteredStudioProjects()
		if err != nil || len(projects) != 1 || projects[0].Name != "Untitled project" || projects[0].Timestamp != "" || projects[0].Path != "" || projects[0].Available {
			t.Fatal("wrong-typed optional fields changed registry compatibility", err)
		}
	}
}
