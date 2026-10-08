package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeImportedChatManifest(t *testing.T, project string, exports []importedChatExport) {
	t.Helper()
	filename := filepath.Join(project, "glowbom.json")
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["exports"] = exports
	manifest["updatedAt"] = "2026-09-24T08:00:00Z"
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeImportedChatCode(t *testing.T, project, name, code string) {
	t.Helper()
	filename := filepath.Join(project, name)
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestChatResultReadsAccountTranslationsWithoutClaimingFreshness(t *testing.T) {
	project := translationProject(t)
	exports := []importedChatExport{
		{Format: "html", Path: "exports/index.html"},
		{Format: "htmlProcessed", Path: "exports/index.processed.html"},
		{Format: "swiftUI", DisplayName: "SwiftUI", Path: "exports/AiExtensions.swift"},
		{Format: "kotlin", DisplayName: "Kotlin", Path: "exports/AiExtensions.kt"},
		{Format: "nextjs", DisplayName: "Next.js", Path: "exports/AiExtensions.tsx"},
		{Format: "godot", StackID: "godot", DisplayName: "Godot", Path: "exports/godot.gd"},
		{Format: "reactNative", Path: "exports/reactnative.tsx"},
		{Format: "custom", StackID: "custom-1234ABCD", DisplayName: "My Elm stack", Path: "exports/custom-1234ABCD.txt"},
	}
	writeImportedChatManifest(t, project, exports)
	for _, export := range exports {
		writeImportedChatCode(t, project, export.Path, "saved "+export.Format+" source")
	}
	result := readTestChatResult(t, project)
	if result.HTML != translationSource || len(result.Translations) != 6 {
		t.Fatalf("did not restore imported translations: %+v", result)
	}
	wantIDs := []string{"swiftui", "kotlin", "nextjs", "godot", "reactnative", "custom-1234abcd"}
	for i, item := range result.Translations {
		if item.ID != wantIDs[i] || item.Source != "imported" || item.SourceHash != "" || item.Outdated || item.Model != "" || item.CreatedAt != "2026-09-24T08:00:00Z" {
			t.Fatalf("incorrect imported identity or source metadata: %+v", item)
		}
	}
	if result.Translations[5].Name != "My Elm stack" || result.Translations[5].Description != "" {
		t.Fatal("lost the custom stack name or invented its missing instructions")
	}
	if _, err := os.Stat(filepath.Join(project, ".glowbom", "translations")); !os.IsNotExist(err) {
		t.Fatal("opening imported code wrote translation records")
	}
	if err := saveChatPrototype(project, "<!doctype html><html><body>Changed</body></html>", translationSource, nil, "test/model", nil); err != nil {
		t.Fatal(err)
	}
	for _, item := range readTestChatResult(t, project).Translations {
		if item.SourceHash != "" || item.Outdated {
			t.Fatal("inferred an imported source revision from the current prototype")
		}
	}
}

func TestChatResultLocalTranslationSupersedesImport(t *testing.T) {
	project := translationProject(t)
	export := importedChatExport{Format: "swiftUI", Path: "exports/swiftui.swift"}
	writeImportedChatManifest(t, project, []importedChatExport{export})
	writeImportedChatCode(t, project, export.Path, "old imported source")
	stack := readTestChatResult(t, project).Translations[0].chatStack
	snapshot, err := chatTranslationSnapshot(project, stack.ID)
	if err != nil || snapshot != "" {
		t.Fatalf("unexpected local snapshot for imported result: %q %v", snapshot, err)
	}
	if _, err := saveChatTranslation(project, stack, "new local source", translationSource, snapshot, "test/model"); err != nil {
		t.Fatal(err)
	}
	result := readTestChatResult(t, project)
	if len(result.Translations) != 1 || result.Translations[0].Code != "new local source" || result.Translations[0].Source != "" || result.Translations[0].Outdated {
		t.Fatalf("import replaced newer local translation: %+v", result.Translations)
	}
	data, err := os.ReadFile(filepath.Join(project, export.Path))
	if err != nil || string(data) != "old imported source" {
		t.Fatal("regeneration changed the original account export")
	}
	if err := os.Remove(filepath.Join(project, export.Path)); err != nil {
		t.Fatal(err)
	}
	if got := readTestChatResult(t, project); len(got.Translations) != 1 || got.Translations[0].Code != "new local source" {
		t.Fatal("a missing old export hid the local translation")
	}
}

func TestChatResultSkipsAbsentAndEmptyImports(t *testing.T) {
	project := translationProject(t)
	writeImportedChatManifest(t, project, []importedChatExport{
		{Format: "swiftUI", Path: "exports/missing.swift"},
		{Format: "kotlin", Path: "exports/empty.kt"},
		{Format: "nextjs", Path: "exports/nextjs.tsx"},
		{Format: "nextjs", Path: "exports/duplicate.tsx"},
	})
	writeImportedChatCode(t, project, "exports/empty.kt", " \n")
	writeImportedChatCode(t, project, "exports/nextjs.tsx", "saved source")
	writeImportedChatCode(t, project, "exports/duplicate.tsx", "duplicate")
	writeImportedChatCode(t, project, "apple/Custom/AiExtensions.swift", "starter source is not a saved translation")
	result := readTestChatResult(t, project)
	if len(result.Translations) != 1 || result.Translations[0].Code != "saved source" {
		t.Fatalf("mistook an absent, duplicate, or starter file for a translation: %+v", result.Translations)
	}
}

func TestChatResultImportedFilesAreBoundedAndContained(t *testing.T) {
	for _, kind := range []string{"traversal", "absolute", "symlink", "oversized", "invalid utf8", "directory"} {
		t.Run(kind, func(t *testing.T) {
			project := translationProject(t)
			outside := filepath.Join(t.TempDir(), "private.swift")
			if err := os.WriteFile(outside, []byte("private source"), 0600); err != nil {
				t.Fatal(err)
			}
			name := "exports/source.swift"
			writeImportedChatCode(t, project, name, "source")
			switch kind {
			case "traversal":
				name = "../private.swift"
			case "absolute":
				name = outside
			case "symlink":
				if err := os.Remove(filepath.Join(project, name)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(project, name)); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				writeImportedChatCode(t, project, name, strings.Repeat("x", maxChatResultBytes+1))
			case "invalid utf8":
				writeImportedChatCode(t, project, name, "\xff")
			case "directory":
				name = "exports"
			}
			writeImportedChatManifest(t, project, []importedChatExport{{Format: "swiftUI", Path: name}})
			w := httptest.NewRecorder()
			(&chatService{}).resultHandler(w, httptest.NewRequest("GET", "/chat/result?path="+url.QueryEscape(project), nil))
			if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "private source") {
				t.Fatalf("accepted unsafe saved export: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestChatTranslationCountIncludesImportedStacks(t *testing.T) {
	project := translationProject(t)
	exports := make([]importedChatExport, maxChatTranslations)
	for i := range exports {
		exports[i] = importedChatExport{Format: "custom", StackID: fmt.Sprintf("custom-%d", i), Path: fmt.Sprintf("exports/custom-%d.txt", i)}
		writeImportedChatCode(t, project, exports[i].Path, "saved source")
	}
	writeImportedChatManifest(t, project, exports)
	if _, err := chatTranslationSnapshot(project, "new-stack"); err == nil {
		t.Fatal("started another translation beyond the combined stack limit")
	}
	stack := chatStack{ID: "new-stack", Name: "New", Description: "SwiftUI"}
	if _, err := saveChatTranslation(project, stack, "new code", translationSource, "", "test/model"); err == nil {
		t.Fatal("saved another translation beyond the combined stack limit")
	}
	stack.ID = "custom-0"
	snapshot, err := chatTranslationSnapshot(project, stack.ID)
	if err != nil {
		t.Fatalf("could not regenerate an existing imported stack at the limit: %v", err)
	}
	if _, err := saveChatTranslation(project, stack, "regenerated source", translationSource, snapshot, "test/model"); err != nil {
		t.Fatalf("could not save regenerated imported stack at the limit: %v", err)
	}
	result := readTestChatResult(t, project)
	if len(result.Translations) != maxChatTranslations || result.Translations[0].Code != "regenerated source" {
		t.Fatal("saving at the limit made the result unreadable")
	}
	if _, err := chatTranslationSnapshot(project, "new-stack"); err == nil {
		t.Fatal("mixed local and imported stacks did not count toward the limit")
	}
}

func TestChatTranslationSizeIncludesImportedCode(t *testing.T) {
	project := translationProject(t)
	exports := make([]importedChatExport, 8)
	for i := range exports {
		exports[i] = importedChatExport{Format: "custom", StackID: fmt.Sprintf("custom-%d", i), Path: fmt.Sprintf("exports/custom-%d.txt", i)}
		writeImportedChatCode(t, project, exports[i].Path, strings.Repeat("x", maxChatResultBytes))
	}
	writeImportedChatManifest(t, project, exports)
	stack := chatStack{ID: "swiftui", Name: "SwiftUI", Description: "SwiftUI"}
	if _, err := saveChatTranslation(project, stack, "new code", translationSource, "", "test/model"); err == nil {
		t.Fatal("saved code beyond the combined translation size limit")
	}
	if result := readTestChatResult(t, project); len(result.Translations) != len(exports) {
		t.Fatal("a rejected save hid the previous imported translations")
	}
}
