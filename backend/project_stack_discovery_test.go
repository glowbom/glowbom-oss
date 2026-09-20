package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverExistingApps(t *testing.T) {
	root := t.TempDir()
	for path, body := range map[string]string{
		"prototype/index.html": "hello", "web/package.json": `{"dependencies":{"next":"16","react":"19"}}`,
		"apple/Custom.xcodeproj/project.pbxproj": "project", "android/app/build.gradle.kts": `plugins { id("com.android.application") }`, "android/build.gradle.kts": "plugins {}",
		"apps/site/index.html": "independent HTML", "apps/game/project.godot": "config_version=5", "php/public/index.php": "<?php echo 'ok';",
		"__MACOSX/apple/Fake.xcodeproj/project.pbxproj": "ignore",
		"node_modules/fake/index.html":                  "ignore", "apps/site/assets/index.html": "ignore",
	} {
		previewFixture(t, root, path, body)
	}
	defs, err := readPreviewDefinitions(root)
	if err != nil {
		t.Fatal(err)
	}
	apps, limited, err := discoverProjectStacks(root, defs)
	if err != nil || limited {
		t.Fatalf("%v limited %v", err, limited)
	}
	if len(apps) != 7 {
		t.Fatalf("wanted seven apps, got %#v", apps)
	}
	byDir := map[string]discoveredStack{}
	for _, app := range apps {
		byDir[app.Directory] = app
	}
	if byDir["prototype"].Target != "prototype" || byDir["web"].Target != "web" || byDir["apple"].BuildTarget != "apple" || byDir["android"].BuildTarget != "android" {
		t.Fatalf("lost builtins: %#v", apps)
	}
	if byDir["apps/site"].Target != "" || byDir["apps/site"].Stack != "HTML" {
		t.Fatal("independent HTML must not become prototype")
	}
	if byDir["apps/game"].Preset != "" || byDir["apps/game"].PreviewMode != "none" {
		t.Fatal("must not guess game dimension or export support")
	}
	if got := byDir["php"]; len(got.Command) != 5 || got.Command[4] != "public" {
		t.Fatalf("bad PHP command: %#v", got)
	}
}

func TestDiscoverPreservesSettingsAndBounds(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	previewFixture(t, root, "php/index.php", "<?php")
	previewFixture(t, outside, "index.html", "outside")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	defs := []previewDefinition{{ID: "custom-saved", Name: "My PHP", Directory: "php", PreviewMode: "none", Command: nil}, {ID: "custom-escape", Directory: "escape"}}
	apps, _, err := discoverProjectStacks(root, defs)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].Target != "custom-saved" || apps[0].Name != "My PHP" || apps[0].PreviewMode != "none" || apps[0].Command != nil {
		t.Fatalf("unexpected discovery: %#v", apps)
	}
	if _, err := os.Stat(filepath.Join(root, ".glowbom")); !os.IsNotExist(err) {
		t.Fatal("discovery must not save settings")
	}
}
