package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func exportTestSavedEntries() []archiveTestEntry {
	return []archiveTestEntry{
		{name: "glowbom.json", data: []byte(`{
  "syncFormatVersion":"1.0", "name":"Garden journal", "displayName":"My garden",
  "promptPath":"inputs/prompt.txt", "initialDrawingPath":"inputs/sketch.png",
  "iconPath":"inputs/icon.png", "updatedAt":"2026-09-17T10:00:00Z",
  "targets":{"untrusted":"saved target"}, "prototypeAssetsManifestPath":"old-assets.json",
  "customMetadata":{"keep":true,"largeNumber":9007199254740993},
  "exports":[
    {"format":"html","path":"exports/index.html"},
    {"format":"htmlProcessed","path":"exports/index.processed.html"},
    {"format":"swiftUI","path":"exports/AiExtensions.swift"},
    {"format":"kotlin","path":"exports/AiExtensions.kt"},
    {"format":"nextjs","path":"exports/AiExtensions.tsx"}
  ]
}`)},
		{name: "inputs/prompt.txt", data: []byte("Build my garden journal.\n")},
		{name: "inputs/sketch.png", data: []byte{0xff, 0x01, 0x02}},
		{name: "inputs/icon.png", data: []byte{0x89, 'P', 'N', 'G', 0xff}},
		{name: "exports/index.html", data: []byte("<html>Original HTML with placeholders</html>")},
		{name: "exports/index.processed.html", data: []byte("<html>Processed preview with embedded images</html>")},
		{name: "exports/AiExtensions.swift", data: []byte("struct Garden {\n  static let enabled: Bool = false\n}")},
		{name: "exports/AiExtensions.kt", data: []byte("class Garden {\n  val enabled: Boolean = false\n}")},
		{name: "exports/AiExtensions.tsx", data: []byte("export const enabled: boolean = false;\nexport default function Garden() {}")},
	}
}

func exportTestStarterEntries() []archiveTestEntry {
	return []archiveTestEntry{
		{name: "glowbom.json", data: []byte(`{
  "name":"Starter", "displayName":"Starter name", "bundleID":"app.glowbom.starter",
  "createdAt":"old creation", "updatedAt":"old update", "exportedAt":"old export",
  "prototypeAssetsManifestPath":"prototype/assets.json",
  "targets":{"ios":{"outputDir":"apple"},"android":{"outputDir":"android"},"web":{"outputDir":"web"}}
}`)},
		{name: "AGENTS.md", data: []byte("Starter project instructions")},
		{name: "prototype/index.html", data: []byte("<html>Starter prototype</html>")},
		{name: "prototype/assets.json", data: []byte(`{"assets":["sample.png"]}`)},
		{name: "prototype/assets/sample.png", data: []byte("starter image")},
		{name: "apple/Custom/AiExtensions.swift", data: []byte("import SwiftUI\nstruct Starter {}")},
		{name: "android/app/src/main/java/com/glowbom/custom/AiExtensions.kt", data: []byte("package com.glowbom.custom\nclass Starter")},
		{name: "web/src/app/components/AiExtensions.tsx", data: []byte("\"use client\";\nexport default function Starter() {}")},
		{name: "android/gradlew", data: []byte("#!/bin/sh\necho starter\n"), mode: 0755},
		{name: "icon.png", data: []byte("starter icon")},
		{name: "web/src/app/icon.png", data: []byte("starter web icon")},
		{name: "web/public/icon.png", data: []byte("starter public icon")},
	}
}

func exportTestManifest(t *testing.T, entries []archiveTestEntry) map[string]any {
	t.Helper()
	var manifest map[string]any
	if err := json.Unmarshal(entries[0].data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func exportTestSetManifest(t *testing.T, entries []archiveTestEntry, manifest map[string]any) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	entries[0].data = data
}

func TestAssembleProjectInstallsSourcesAndPreservesSavedFiles(t *testing.T) {
	saved := exportTestSavedEntries()
	starter := exportTestStarterEntries()
	assembled, err := assembleProject(context.Background(), archiveTestZIP(t, saved), archiveTestZIP(t, starter))
	if err != nil {
		t.Fatal(err)
	}
	files := exportEntryMap(assembled)
	for _, entry := range saved[1:] {
		if !bytes.Equal(files[entry.name].data, entry.data) {
			t.Fatalf("saved source changed or missing: %s", entry.name)
		}
	}
	wantSources := map[string]string{
		"prototype/index.html":                                         "<html>Processed preview with embedded images</html>",
		"apple/Custom/AiExtensions.swift":                              "import SwiftUI\n\nstruct Garden {\n  static let enabled: Bool = true\n}",
		"android/app/src/main/java/com/glowbom/custom/AiExtensions.kt": "package com.glowbom.custom\n\nclass Garden {\n  val enabled: Boolean = true\n}",
		"web/src/app/components/AiExtensions.tsx":                      "\"use client\";\n\nexport const enabled: boolean = true;\nexport default function Garden() {}",
	}
	for name, want := range wantSources {
		if string(files[name].data) != want {
			t.Fatalf("wrong source installed at %s: %q", name, files[name].data)
		}
	}
	for _, name := range []string{"icon.png", "web/src/app/icon.png", "web/public/icon.png"} {
		if !bytes.Equal(files[name].data, files["inputs/icon.png"].data) {
			t.Fatalf("saved icon not installed at %s", name)
		}
	}
	for _, name := range []string{"prototype/assets.json", "prototype/assets/sample.png"} {
		if _, exists := files[name]; exists {
			t.Fatalf("starter demonstration asset retained: %s", name)
		}
	}
	if files["android/gradlew"].mode != 0755 {
		t.Fatal("Gradle launcher lost its executable mode")
	}
	manifest, err := readExportManifest(files)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]any{"name": "Garden journal", "displayName": "My garden", "bundleID": "app.glowbom.starter", "prototypePath": "prototype/index.html", "agentsPath": "AGENTS.md", "updatedAt": "2026-09-17T10:00:00Z"} {
		if manifest[name] != want {
			t.Fatalf("manifest %s = %v, want %v", name, manifest[name], want)
		}
	}
	for _, name := range []string{"createdAt", "exportedAt", "prototypeAssetsManifestPath"} {
		if _, exists := manifest[name]; exists {
			t.Fatalf("retained starter metadata %s", name)
		}
	}
	metadata := manifest["customMetadata"].(map[string]any)
	if metadata["largeNumber"] != json.Number("9007199254740993") {
		t.Fatal("unknown numeric metadata lost precision")
	}
	targets := manifest["targets"].(map[string]any)
	if len(targets) != 3 || targets["ios"] == nil || targets["untrusted"] != nil {
		t.Fatal("saved metadata replaced starter targets")
	}
	names := make([]string, 0, len(assembled))
	for _, entry := range assembled {
		names = append(names, entry.name)
	}
	if !sort.StringsAreSorted(names) {
		t.Fatal("assembled files are not deterministic")
	}
	again, err := assembleProject(context.Background(), archiveTestZIP(t, saved), archiveTestZIP(t, starter))
	if err != nil || !bytes.Equal(exportEntryMap(again)["glowbom.json"].data, files["glowbom.json"].data) {
		t.Fatal("repeated assembly changed the manifest")
	}
}

func TestAssembleProjectRawHTMLAndMissingOptionalExports(t *testing.T) {
	saved := exportTestSavedEntries()[:1]
	saved = append(saved,
		archiveTestEntry{name: "inputs/prompt.txt", data: []byte("prompt")},
		archiveTestEntry{name: "exports/index.html", data: []byte("<html>Raw preview</html>")},
		archiveTestEntry{name: "exports/processed.html", data: []byte(" \n\t ")},
	)
	manifest := map[string]any{"syncFormatVersion": "1.0", "promptPath": "inputs/prompt.txt", "exports": []map[string]any{{"format": "html", "path": "exports/index.html"}, {"format": "htmlProcessed", "path": "exports/processed.html"}}}
	exportTestSetManifest(t, saved, manifest)
	starter := exportTestStarterEntries()
	assembled, err := assembleProject(context.Background(), archiveTestZIP(t, saved), archiveTestZIP(t, starter))
	if err != nil {
		t.Fatal(err)
	}
	files := exportEntryMap(assembled)
	if string(files["prototype/index.html"].data) != "<html>Raw preview</html>" {
		t.Fatal("empty processed HTML did not fall back to raw HTML")
	}
	for _, entry := range starter {
		if strings.HasSuffix(entry.name, ".swift") || strings.HasSuffix(entry.name, ".kt") || strings.HasSuffix(entry.name, ".tsx") || strings.HasSuffix(entry.name, "icon.png") {
			if !bytes.Equal(files[entry.name].data, entry.data) {
				t.Fatalf("missing optional export replaced starter file %s", entry.name)
			}
		}
	}
	resultManifest, _ := readExportManifest(files)
	if resultManifest["name"] != "Glowbom" || resultManifest["displayName"] != "Glowbom" {
		t.Fatal("missing names did not use Glowbom fallback")
	}
}

func TestAssembleProjectNameFallbacksAndSavedDates(t *testing.T) {
	for _, test := range []struct{ name, display, wantName, wantDisplay string }{
		{"  Saved name  ", "", "Saved name", "Saved name"},
		{"", "  Display name  ", "Display name", "Display name"},
		{"Saved", "Display", "Saved", "Display"},
		{" ", " \n ", "Glowbom", "Glowbom"},
	} {
		t.Run(test.wantName+test.wantDisplay, func(t *testing.T) {
			saved := exportTestSavedEntries()
			manifest := exportTestManifest(t, saved)
			manifest["name"] = test.name
			manifest["displayName"] = test.display
			manifest["createdAt"] = "saved creation"
			manifest["exportedAt"] = nil
			exportTestSetManifest(t, saved, manifest)
			entries, err := assembleProject(context.Background(), archiveTestZIP(t, saved), archiveTestZIP(t, exportTestStarterEntries()))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := readExportManifest(exportEntryMap(entries))
			if got["name"] != test.wantName || got["displayName"] != test.wantDisplay || got["createdAt"] != "saved creation" {
				t.Fatalf("incorrect merged metadata: %v", got)
			}
			if value, exists := got["exportedAt"]; !exists || value != nil {
				t.Fatal("explicit saved date field was not preserved")
			}
		})
	}
}

func TestPrepareProjectExtensionPreservesHeadersAndOnlyEnablesTheScreen(t *testing.T) {
	for _, test := range []struct{ format, source, want string }{
		{"swiftUI", "import SwiftUI\nstatic let enabled = false\nlet unrelated = false", "import SwiftUI\nstatic let enabled = true\nlet unrelated = false"},
		{"kotlin", "package com.example\nconst val enabled = false\nvar other = false", "package com.example\nconst val enabled = true\nvar other = false"},
		{"nextjs", "'use client';\nlet enabled = false;\nconst other = false;", "'use client';\nlet enabled = true;\nconst other = false;"},
		{"swiftUI", "import SwiftUI\nstatic let enabled = true", "import SwiftUI\nstatic let enabled = true"},
		{"nextjs", "\"use client\";\n// const enabled = false\nconst enabled = falseValue;", "\"use client\";\n// const enabled = false\nconst enabled = falseValue;"},
	} {
		t.Run(test.format+test.source, func(t *testing.T) {
			if got := prepareProjectExtension(test.source, test.format); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestAssembleProjectRejectsMalformedExportsAndText(t *testing.T) {
	for _, format := range []any{nil, 1, false, "", " \n "} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			saved := exportTestSavedEntries()
			manifest := exportTestManifest(t, saved)
			manifest["exports"].([]any)[0].(map[string]any)["format"] = format
			exportTestSetManifest(t, saved, manifest)
			if _, err := assembleProject(context.Background(), archiveTestZIP(t, saved), archiveTestZIP(t, exportTestStarterEntries())); err == nil {
				t.Fatal("accepted invalid export format")
			}
		})
	}
	for _, name := range []string{"glowbom.json", "exports/index.html", "exports/index.processed.html", "exports/AiExtensions.swift", "exports/AiExtensions.kt", "exports/AiExtensions.tsx"} {
		t.Run(name, func(t *testing.T) {
			saved := exportTestSavedEntries()
			for index := range saved {
				if saved[index].name == name {
					saved[index].data = append(saved[index].data, 0xff)
				}
			}
			if _, err := assembleProject(context.Background(), archiveTestZIP(t, saved), archiveTestZIP(t, exportTestStarterEntries())); err == nil {
				t.Fatal("accepted invalid UTF-8")
			}
		})
	}
	saved := exportTestSavedEntries()
	for index := range saved {
		if saved[index].name == "exports/index.html" || saved[index].name == "exports/index.processed.html" {
			saved[index].data = []byte("  ")
		}
	}
	if _, err := assembleProject(context.Background(), archiveTestZIP(t, saved), archiveTestZIP(t, exportTestStarterEntries())); err == nil {
		t.Fatal("accepted a missing HTML prototype")
	}
}

func TestAssembleProjectRejectsIncompleteStarter(t *testing.T) {
	for _, missing := range []string{"AGENTS.md", "prototype/index.html", "apple/Custom/AiExtensions.swift", "android/app/src/main/java/com/glowbom/custom/AiExtensions.kt", "web/src/app/components/AiExtensions.tsx", "targets", "glowbom.json"} {
		t.Run(missing, func(t *testing.T) {
			starter := exportTestStarterEntries()
			if missing == "targets" {
				manifest := exportTestManifest(t, starter)
				delete(manifest, "targets")
				exportTestSetManifest(t, starter, manifest)
			} else {
				for index := range starter {
					if starter[index].name == missing {
						starter = append(starter[:index], starter[index+1:]...)
						break
					}
				}
			}
			if _, err := assembleProject(context.Background(), archiveTestZIP(t, exportTestSavedEntries()), archiveTestZIP(t, starter)); err == nil {
				t.Fatal("accepted incomplete starter")
			}
		})
	}
}

func TestAssembleProjectRejectsCrossArchivePathCollisions(t *testing.T) {
	for _, conflict := range []string{"inputs/prompt.txt", "INPUTS/prompt.txt", "Inputs/other.txt", "inputs", "inputs/prompt.txt/extra", "exports/AiExtensions.swift"} {
		t.Run(conflict, func(t *testing.T) {
			starter := append(exportTestStarterEntries(), archiveTestEntry{name: conflict, data: []byte("starter collision")})
			if _, err := assembleProject(context.Background(), archiveTestZIP(t, exportTestSavedEntries()), archiveTestZIP(t, starter)); err == nil {
				t.Fatal("accepted a cross-archive path collision")
			}
		})
	}
	// The icon creates paths that did not exist in this otherwise valid starter.
	starter := exportTestStarterEntries()
	filtered := starter[:0]
	for _, entry := range starter {
		if entry.name != "web/public/icon.png" {
			filtered = append(filtered, entry)
		}
	}
	filtered = append(filtered, archiveTestEntry{name: "web/public", data: []byte("a file, not a directory")})
	if _, err := assembleProject(context.Background(), archiveTestZIP(t, exportTestSavedEntries()), archiveTestZIP(t, filtered)); err == nil {
		t.Fatal("accepted a generated icon path collision")
	}
}

func TestAssembleProjectPreservesRootSavedIcon(t *testing.T) {
	saved := exportTestSavedEntries()
	manifest := exportTestManifest(t, saved)
	manifest["iconPath"] = "icon.png"
	exportTestSetManifest(t, saved, manifest)
	for index := range saved {
		if saved[index].name == "inputs/icon.png" {
			saved[index].name = "icon.png"
		}
	}
	entries, err := assembleProject(context.Background(), archiveTestZIP(t, saved), archiveTestZIP(t, exportTestStarterEntries()))
	if err != nil {
		t.Fatal(err)
	}
	files := exportEntryMap(entries)
	if !bytes.Equal(files["icon.png"].data, files["web/public/icon.png"].data) {
		t.Fatal("root saved icon was not installed")
	}
}

func TestAssembleProjectRejectsBadZIPAndCancellation(t *testing.T) {
	saved := archiveTestZIP(t, exportTestSavedEntries())
	starter := archiveTestZIP(t, exportTestStarterEntries())
	for _, archives := range [][2][]byte{{[]byte("bad"), starter}, {saved, []byte("bad")}} {
		if _, err := assembleProject(context.Background(), archives[0], archives[1]); err == nil {
			t.Fatal("accepted malformed ZIP")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := assembleProject(ctx, saved, starter); !errors.Is(err, context.Canceled) {
		t.Fatalf("did not preserve cancellation: %v", err)
	}
}

func TestAssembleProjectBoundsDuplicatedSourceAndIconBytes(t *testing.T) {
	manifest := map[string]any{
		"syncFormatVersion": "1.0", "promptPath": "prompt.txt", "iconPath": "exports/shared.txt",
		"exports": []map[string]string{
			{"format": "html", "path": "exports/shared.txt"},
			{"format": "swiftUI", "path": "exports/shared.txt"},
			{"format": "kotlin", "path": "exports/shared.txt"},
			{"format": "nextjs", "path": "exports/shared.txt"},
		},
	}
	saved := []archiveTestEntry{
		{name: "glowbom.json"},
		{name: "prompt.txt", data: []byte("prompt")},
		{name: "exports/shared.txt", data: bytes.Repeat([]byte("x"), 8*1024*1024), method: zip.Deflate},
	}
	exportTestSetManifest(t, saved, manifest)
	if _, err := assembleProject(context.Background(), archiveTestZIP(t, saved), archiveTestZIP(t, exportTestStarterEntries())); err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("did not bound the final assembled project: %v", err)
	}
}
