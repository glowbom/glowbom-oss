package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const starterFixtureManifest = `{"name":"Glowbom Project","displayName":"Glowby","version":"1.0","createdAt":"old","updatedAt":"old","future":9007199254740993,"targets":{"ios":{"outputDir":"apple","stack":"swiftui","hasPreset":true,"status":"ready"},"android":{"outputDir":"android","stack":"kotlin"},"web":{"outputDir":"web","stack":"nextjs"}}}`

func writeStarterFixture(t *testing.T, root string) {
	t.Helper()
	for name, content := range map[string]string{
		"glowbom.json":                   starterFixtureManifest,
		"AGENTS.md":                      "Build project instructions",
		"apple/Custom/ContentView.swift": "SwiftUI starter",
		"android/app/build.gradle.kts":   "Android starter",
		"android/gradlew":                "#!/bin/sh\nexit 99\n",
		"web/package.json":               `{"name":"starter"}`,
		"prototype/index.html":           "Build Anything",
		"prototype/assets/demo.png":      "demo image",
		"prototype/assets.json":          "demo metadata",
		"icon.png":                       "demo icon",
		"android/local.properties":       "local SDK path",
		"android/.idea/workspace.xml":    "local IDE settings",
		"apple/Custom.xcodeproj/xcuserdata/local.xcuserdatad/settings": "local Xcode settings",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0644)
		if name == "android/gradlew" {
			mode = 0755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func starterFixtureRunner(t *testing.T, after func(string) error) accountCLIRunner {
	t.Helper()
	return func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) != 3 || args[0] != "template" || args[1] != "--output" || !filepath.IsAbs(args[2]) || filepath.Base(args[2]) != "starter" {
			t.Fatalf("unexpected CLI arguments: %v", args)
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 2*time.Minute {
			t.Fatal("template command must have a bounded deadline")
		}
		if info, err := os.Stat(filepath.Dir(args[2])); err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("starter staging is not private")
		}
		if _, err := os.Lstat(args[2]); !os.IsNotExist(err) {
			t.Fatal("CLI output path already exists", err)
		}
		writeStarterFixture(t, args[2])
		if after != nil {
			if err := after(args[2]); err != nil {
				return nil, err
			}
		}
		return []byte("untrusted output including ignored paths and tokens"), nil
	}
}

func TestCreateStarterProjectPreservesBuildsAndUsesRequestedName(t *testing.T) {
	parent := t.TempDir()
	parent, _ = filepath.EvalSymlinks(parent)
	stage := ""
	root, err := createStarterProject(context.Background(), parent, "Garden notes", starterFixtureRunner(t, func(path string) error {
		stage = filepath.Dir(path)
		return nil
	}))
	if err != nil || root != filepath.Join(parent, "Garden notes") {
		t.Fatal(root, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "glowbom.json"))
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal([]byte(starterFixtureManifest), &before)
	if json.Unmarshal(data, &after) != nil {
		t.Fatal("invalid manifest")
	}
	for _, key := range []string{"targets", "future", "version"} {
		// Compare raw JSON without converting large numbers through float64.
		compact := func(raw json.RawMessage) string {
			var out bytes.Buffer
			_ = json.Compact(&out, raw)
			return out.String()
		}
		if compact(after[key]) != compact(before[key]) {
			t.Fatalf("starter metadata changed: %s", key)
		}
	}
	if string(after["name"]) != `"Garden notes"` || string(after["displayName"]) != `"Garden notes"` || string(after["createdAt"]) == `"old"` || string(after["createdAt"]) != string(after["updatedAt"]) {
		t.Fatal(string(data))
	}
	for _, name := range []string{"apple/Custom/ContentView.swift", "android/app/build.gradle.kts", "web/package.json", "AGENTS.md", "prototype/assets"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal("missing starter path", name, err)
		}
	}
	for _, name := range []string{"prototype/index.html", "prototype/assets.json", "prototype/assets/demo.png", "icon.png", "android/local.properties", "android/.idea", "apple/Custom.xcodeproj/xcuserdata"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatal("kept sample or machine-local content", name, err)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "android/gradlew")); err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("Gradle executable was not preserved")
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("private staging was not removed", err)
	}
}

func TestCreateStarterProjectRejectsExistingAndRacingDestinations(t *testing.T) {
	for _, timing := range []string{"before", "during"} {
		for _, kind := range []string{"directory", "file", "symlink"} {
			t.Run(timing+"-"+kind, func(t *testing.T) {
				parent := t.TempDir()
				destination := filepath.Join(parent, "Garden")
				makeConflict := func() {
					var err error
					switch kind {
					case "directory":
						err = os.Mkdir(destination, 0700)
					case "file":
						err = os.WriteFile(destination, []byte("keep"), 0600)
					case "symlink":
						err = os.Symlink(filepath.Join(parent, "missing"), destination)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if timing == "before" {
					makeConflict()
				}
				called := false
				runner := starterFixtureRunner(t, func(string) error { called = true; makeConflict(); return nil })
				_, err := createStarterProject(context.Background(), parent, "Garden", runner)
				if !errors.Is(err, errStarterDestination) || called != (timing == "during") {
					t.Fatal("wrong collision behavior", called, err)
				}
				info, err := os.Lstat(destination)
				if err != nil || (kind == "directory" && !info.IsDir()) || (kind == "symlink" && info.Mode()&os.ModeSymlink == 0) {
					t.Fatal("existing destination changed")
				}
				if kind == "file" {
					data, _ := os.ReadFile(destination)
					if string(data) != "keep" {
						t.Fatal("existing file changed")
					}
				}
			})
		}
	}
}

func TestCreateStarterProjectFailureAndCancellationLeaveNoDestination(t *testing.T) {
	for _, failure := range []string{"canceled-before", "canceled-during", "missing-cli", "download-failure"} {
		t.Run(failure, func(t *testing.T) {
			parent := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "canceled-before" {
				cancel()
			}
			calls, stage := 0, ""
			runner := func(ctx context.Context, args ...string) ([]byte, error) {
				calls++
				stage = filepath.Dir(args[2])
				if failure == "canceled-during" {
					writeStarterFixture(t, args[2])
					cancel()
					return nil, ctx.Err()
				}
				if failure == "missing-cli" {
					return nil, errAccountCLIMissing
				}
				return []byte("secret-provider-output"), errors.New("secret-provider-error")
			}
			_, err := createStarterProject(ctx, parent, "Garden", runner)
			if err == nil || strings.Contains(err.Error(), "secret-provider") || calls > 1 || (failure == "canceled-before" && calls != 0) {
				t.Fatal("unexpected failure", calls, err)
			}
			children, _ := os.ReadDir(parent)
			if len(children) != 0 {
				t.Fatal("failed download left a destination")
			}
			if stage != "" {
				if _, err := os.Stat(stage); !os.IsNotExist(err) {
					t.Fatal("failed download left staging")
				}
			}
		})
	}
}

func TestCreateStarterProjectRejectsInvalidStagedContent(t *testing.T) {
	for _, invalid := range []string{"manifest-null", "missing-target", "missing-directory", "symlink", "missing-instructions", "empty-instructions", "instructions-directory"} {
		t.Run(invalid, func(t *testing.T) {
			parent := t.TempDir()
			runner := starterFixtureRunner(t, func(path string) error {
				switch invalid {
				case "manifest-null":
					return os.WriteFile(filepath.Join(path, "glowbom.json"), []byte("null"), 0600)
				case "missing-target":
					return os.WriteFile(filepath.Join(path, "glowbom.json"), []byte(`{"targets":{}}`), 0600)
				case "missing-directory":
					return os.RemoveAll(filepath.Join(path, "web"))
				case "symlink":
					return os.Symlink(parent, filepath.Join(path, "unexpected"))
				case "missing-instructions":
					return os.Remove(filepath.Join(path, "AGENTS.md"))
				case "empty-instructions":
					return os.WriteFile(filepath.Join(path, "AGENTS.md"), []byte(" \n\t"), 0600)
				case "instructions-directory":
					if err := os.Remove(filepath.Join(path, "AGENTS.md")); err != nil {
						return err
					}
					return os.Mkdir(filepath.Join(path, "AGENTS.md"), 0700)
				}
				return nil
			})
			if _, err := createStarterProject(context.Background(), parent, "Garden", runner); err == nil {
				t.Fatal("accepted invalid starter")
			}
			children, _ := os.ReadDir(parent)
			if len(children) != 0 {
				t.Fatal("invalid starter created destination")
			}
		})
	}
}

func TestLoadStarterProjectReturnsValidatedFilesAndRemovesPrivateStage(t *testing.T) {
	stage := ""
	files, err := loadStarterProject(context.Background(), "Phone garden", starterFixtureRunner(t, func(path string) error {
		stage = filepath.Dir(path)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("completed loading left its private staging directory", err)
	}
	byName := map[string]starterProjectFile{}
	for _, file := range files {
		byName[filepath.ToSlash(file.name)] = file
	}
	for _, name := range []string{"glowbom.json", "AGENTS.md", "apple/Custom/ContentView.swift", "android/app/build.gradle.kts", "android/gradlew", "web/package.json"} {
		if len(byName[name].data) == 0 {
			t.Fatal("missing validated starter file", name)
		}
	}
	for _, name := range []string{"prototype/index.html", "prototype/assets/demo.png", "icon.png", "android/local.properties"} {
		if _, present := byName[name]; present {
			t.Fatal("kept template sample or machine-local content", name)
		}
	}
	if byName["android/gradlew"].mode != 0755 {
		t.Fatal("lost Gradle wrapper executable permission")
	}
	var fields map[string]json.RawMessage
	var targets map[string]map[string]json.RawMessage
	if json.Unmarshal(byName["glowbom.json"].data, &fields) != nil || json.Unmarshal(fields["targets"], &targets) != nil || string(fields["name"]) != `"Phone garden"` || string(fields["future"]) != "9007199254740993" || string(targets["ios"]["hasPreset"]) != "true" || string(targets["ios"]["status"]) != `"ready"` {
		t.Fatal("lost template identity or future metadata", string(byName["glowbom.json"].data))
	}
}

func TestLoadStarterProjectPreservesCancellationAndSafeErrors(t *testing.T) {
	for _, failure := range []string{"canceled-before", "canceled-during", "deadline-before", "missing-cli", "download-failure"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "canceled-before" {
				cancel()
			}
			if failure == "deadline-before" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer deadlineCancel()
			}
			calls, stage := 0, ""
			files, err := loadStarterProject(ctx, "Phone garden", func(ctx context.Context, args ...string) ([]byte, error) {
				calls++
				stage = filepath.Dir(args[2])
				if failure == "canceled-during" {
					writeStarterFixture(t, args[2])
					cancel()
					return nil, ctx.Err()
				}
				if failure == "missing-cli" {
					return nil, errAccountCLIMissing
				}
				return []byte("secret-provider-output"), errors.New("secret-provider-error")
			})
			expected := errStarterProject.Error()
			switch failure {
			case "canceled-before", "canceled-during":
				expected = "Project creation was canceled."
			case "deadline-before":
				expected = "The starter download took too long. Check your connection and try again."
			case "missing-cli":
				expected = "Install or update the Glowbom CLI to create a project from the starter."
			}
			if err == nil || err.Error() != expected || files != nil || calls > 1 || ((failure == "canceled-before" || failure == "deadline-before") && calls != 0) {
				t.Fatal("loading changed its safe failure contract", calls, files, err)
			}
			if stage != "" {
				if _, err := os.Stat(stage); !os.IsNotExist(err) {
					t.Fatal("failed loading left private staging", err)
				}
			}
		})
	}
}

func TestLoadStarterProjectKeepsShorterCallerDeadline(t *testing.T) {
	deadline := time.Now().Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	run := starterFixtureRunner(t, nil)
	_, err := loadStarterProject(ctx, "Phone garden", func(inner context.Context, args ...string) ([]byte, error) {
		actual, ok := inner.Deadline()
		if !ok || !actual.Equal(deadline) {
			t.Fatal("starter extended the caller's deadline", actual)
		}
		return run(inner, args...)
	})
	if err != nil || ctx.Err() != nil {
		t.Fatal("loader failed or canceled its caller", err, ctx.Err())
	}
}

type starterObservingContext struct {
	context.Context
	observe func() error
}

func (ctx starterObservingContext) Err() error { return ctx.observe() }

func TestInstallStarterProjectCancellationPreservesConcurrentEdits(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean-rollback", true: "preserve-local-work"}[edit], func(t *testing.T) {
			parentPath := t.TempDir()
			parent, err := os.OpenRoot(parentPath)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			first := filepath.Join(parentPath, "Garden", "first.txt")
			ctx := starterObservingContext{Context: context.Background(), observe: func() error {
				if _, err := os.Stat(first); err == nil {
					if edit {
						if err := os.WriteFile(first, []byte("mine"), 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(filepath.Dir(first), "keep.txt"), []byte("new local file"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return context.Canceled
				}
				return nil
			}}
			err = installStarterProject(ctx, parent, "Garden", []starterProjectFile{{name: "first.txt", data: []byte("base"), mode: 0644}, {name: "second.txt", data: []byte("second"), mode: 0644}})
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if !edit {
				if _, err := os.Stat(filepath.Dir(first)); !os.IsNotExist(err) {
					t.Fatal("canceled install left output")
				}
			} else {
				data, err := os.ReadFile(first)
				if err != nil || string(data) != "mine" {
					t.Fatal("in-place edit was removed")
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(first), "keep.txt")); err != nil {
					t.Fatal("new local file was removed")
				}
			}
		})
	}
}

func TestInstallStarterProjectDetectsMovedParentBeforeCommit(t *testing.T) {
	base := t.TempDir()
	parentPath := filepath.Join(base, "parent")
	if err := os.Mkdir(parentPath, 0700); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	moved := filepath.Join(base, "moved")
	replaced := false
	ctx := starterObservingContext{Context: context.Background(), observe: func() error {
		if !replaced {
			if _, err := os.Stat(filepath.Join(parentPath, "Garden", "first.txt")); err == nil {
				if err := os.Rename(parentPath, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(parentPath, 0700); err != nil {
					t.Fatal(err)
				}
				replaced = true
			}
		}
		return nil
	}}
	err = installStarterProject(ctx, parent, "Garden", []starterProjectFile{{name: "first.txt", data: []byte("starter"), mode: 0644}})
	if err == nil || !replaced {
		t.Fatal("reported success for a stale destination", err)
	}
	for _, path := range []string{parentPath, moved} {
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			t.Fatal("left files after parent changed", path, entries, err)
		}
	}
}
