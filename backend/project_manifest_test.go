package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectRenameAndSettingsPreserveImportedTranslations(t *testing.T) {
	project := translationProject(t)
	export := importedChatExport{Format: "swiftUI", Path: "exports/AiExtensions.swift"}
	writeImportedChatManifest(t, project, []importedChatExport{export})
	writeImportedChatCode(t, project, export.Path, "original imported source")
	manifestPath := filepath.Join(project, "glowbom.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]json.RawMessage{
		"syncFormatVersion": json.RawMessage(`"1.0"`),
		"promptPath":        json.RawMessage(`"inputs/prompt.txt"`),
		"prototypePath":     json.RawMessage(`"prototype/index.html"`),
		"customMetadata":    json.RawMessage(`{"counter":9007199254740993,"setting":null}`),
	}
	for key, value := range metadata {
		manifest[key] = value
	}
	manifest["bundleID"] = json.RawMessage(`"com.example.old"`)
	manifest["displayName"] = json.RawMessage(`"Old name"`)
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, settings := range []bool{false, true} {
		body := map[string]string{"path": project, "name": "Renamed"}
		if settings {
			body = map[string]string{"path": project, "bundleID": "", "displayName": "", "version": "2.0"}
		}
		data, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/project", bytes.NewReader(data))
		if settings {
			openCodeUpdateProjectSettingsHandler(w, r)
		} else {
			openCodeRenameProjectHandler(w, r)
		}
		var response struct {
			Success bool `json:"success"`
		}
		if json.Unmarshal(w.Body.Bytes(), &response) != nil || !response.Success {
			t.Fatalf("project update failed: %s", w.Body.String())
		}
		result := readTestChatResult(t, project)
		if len(result.Translations) != 1 || result.Translations[0].Code != "original imported source" {
			t.Fatal("project metadata update hid the imported translation")
		}
	}
	data, err = os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = nil
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for key, expected := range metadata {
		var compact bytes.Buffer
		if err := json.Compact(&compact, manifest[key]); err != nil || !bytes.Equal(compact.Bytes(), expected) {
			t.Fatalf("lost or changed manifest metadata %s: %s", key, manifest[key])
		}
	}
	if _, exists := manifest["bundleID"]; exists {
		t.Fatal("cleared bundle ID survived the manifest merge")
	}
	if _, exists := manifest["displayName"]; exists {
		t.Fatal("cleared display name survived the manifest merge")
	}
	if string(manifest["name"]) != `"Renamed"` || string(manifest["version"]) != `"2.0"` {
		t.Fatal("known project fields were not updated")
	}
	data, err = os.ReadFile(filepath.Join(project, export.Path))
	if err != nil || string(data) != "original imported source" {
		t.Fatal("project metadata update changed an original export")
	}
}

func TestSaveProjectDoesNotOverwriteInvalidExistingManifest(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "glowbom.json")
	for _, content := range []string{"{incomplete", "null"} {
		if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := SaveProject(filename, &GlowbomProject{Name: "New"}); err == nil {
			t.Fatal("replaced a manifest whose existing fields could not be read")
		}
		data, err := os.ReadFile(filename)
		if err != nil || string(data) != content {
			t.Fatal("failed merge changed the existing manifest")
		}
	}
}
