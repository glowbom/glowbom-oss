package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func companionCleanupFixture(t *testing.T) (*os.Root, string, os.FileInfo, []starterCreatedPath) {
	t.Helper()
	parentPath := filepath.Join(t.TempDir(), "parent")
	if err := os.Mkdir(parentPath, 0700); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Close() })
	name := "Phone-project"
	var original os.FileInfo
	var created []starterCreatedPath
	files := []starterProjectFile{
		{name: "AGENTS.md", data: []byte("project instructions"), mode: 0644},
		{name: filepath.Join("apple", "App.swift"), data: []byte("starter SwiftUI"), mode: 0644},
	}
	err = installStarterProject(context.Background(), parent, name, files, func(info os.FileInfo, inventory []starterCreatedPath) {
		original, created = info, inventory
	})
	if err != nil || original == nil || len(created) < 3 {
		t.Fatal("missing original installer inventory", err, original, created)
	}
	return parent, name, original, created
}

func TestCompanionStarterCleanupRemovesOnlyUnchangedCreatedStarter(t *testing.T) {
	parent, name, original, created := companionCleanupFixture(t)
	if !cleanupCompanionStarter(parent, name, original, created) {
		t.Fatal("unchanged starter was not cleaned up")
	}
	if _, err := parent.Lstat(name); !os.IsNotExist(err) {
		t.Fatal("unchanged starter folder remains", err)
	}
}

func TestCompanionStarterCleanupPreservesOwnerEditsAndUntrackedPrototype(t *testing.T) {
	parent, name, original, created := companionCleanupFixture(t)
	root := filepath.Join(parent.Name(), name)
	edited := []byte("owner SwiftUI")
	if err := os.WriteFile(filepath.Join(root, "apple", "App.swift"), edited, 0644); err != nil {
		t.Fatal(err)
	}
	prototype := []byte("<!doctype html><html>saved phone prototype</html>")
	if err := os.WriteFile(filepath.Join(root, "prototype", "index.html"), prototype, 0600); err != nil {
		t.Fatal(err)
	}
	if cleanupCompanionStarter(parent, name, original, created) {
		t.Fatal("cleanup removed a folder with local edits and generated files")
	}
	for path, expected := range map[string]string{"apple/App.swift": string(edited), "prototype/index.html": string(prototype)} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || string(data) != expected {
			t.Fatal("owner or untracked generated data changed", path, string(data), err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatal("unchanged owned file was not cleaned up", err)
	}
}

func TestCompanionStarterCleanupPreservesSameSizeContentEdits(t *testing.T) {
	parent, name, original, created := companionCleanupFixture(t)
	path := filepath.Join(parent.Name(), name, "apple", "App.swift")
	info, _ := os.Stat(path)
	edited := []byte("changed SwiftUI")
	if int64(len(edited)) != info.Size() {
		t.Fatal("fixture must keep the original size")
	}
	if err := os.WriteFile(path, edited, 0644); err != nil {
		t.Fatal(err)
	}
	// Keep the recorded timestamp to ensure the content hash protects this edit.
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if cleanupCompanionStarter(parent, name, original, created) {
		t.Fatal("same-size content edit was removed")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(edited) {
		t.Fatal("same-size edit changed", string(data), err)
	}
}

func TestCompanionStarterCleanupPreservesRewrittenOrPermissionEditedFile(t *testing.T) {
	for _, edit := range []string{"timestamp", "permissions"} {
		t.Run(edit, func(t *testing.T) {
			parent, name, original, created := companionCleanupFixture(t)
			path := filepath.Join(parent.Name(), name, "AGENTS.md")
			if edit == "timestamp" {
				stamp := time.Now().Add(time.Minute)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			if cleanupCompanionStarter(parent, name, original, created) {
				t.Fatal("owner changed file was removed")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("owner changed file disappeared", err)
			}
		})
	}
}

func TestCompanionStarterCleanupNeverRemovesReplacementFolder(t *testing.T) {
	parent, name, original, created := companionCleanupFixture(t)
	old := filepath.Join(parent.Name(), name)
	moved := filepath.Join(parent.Name(), "kept-original")
	if err := os.Rename(old, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(old, original.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "AGENTS.md"), []byte("project instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	if cleanupCompanionStarter(parent, name, original, created) {
		t.Fatal("replacement folder was removed")
	}
	for _, path := range []string{filepath.Join(old, "AGENTS.md"), filepath.Join(moved, "AGENTS.md")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("replacement or moved original was modified", path, err)
		}
	}
}

func TestCompanionStarterCleanupNeverRemovesReplacementFileWithSameBytes(t *testing.T) {
	parent, name, original, created := companionCleanupFixture(t)
	path := filepath.Join(parent.Name(), name, "AGENTS.md")
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("project instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	if cleanupCompanionStarter(parent, name, original, created) {
		t.Fatal("replacement file was removed")
	}
	for _, path := range []string{path, path + ".original"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("replacement or untracked original was removed", path, err)
		}
	}
}

func TestCompanionStarterCleanupRejectsMovedOrReplacedParent(t *testing.T) {
	parent, name, original, created := companionCleanupFixture(t)
	path := parent.Name()
	moved := path + "-moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if cleanupCompanionStarter(parent, name, original, created) {
		t.Fatal("cleanup accepted a replaced parent")
	}
	if _, err := os.Stat(filepath.Join(moved, name, "AGENTS.md")); err != nil {
		t.Fatal("cleanup modified the moved parent", err)
	}
}

func TestCompanionStarterCleanupRejectsSymlinkFolderReplacement(t *testing.T) {
	parent, name, original, created := companionCleanupFixture(t)
	path := filepath.Join(parent.Name(), name)
	moved := path + "-original"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, path); err != nil {
		t.Fatal(err)
	}
	if cleanupCompanionStarter(parent, name, original, created) {
		t.Fatal("cleanup accepted a symlink replacement")
	}
	if _, err := os.Stat(filepath.Join(moved, "AGENTS.md")); err != nil {
		t.Fatal("cleanup followed the replacement symlink", err)
	}
}
