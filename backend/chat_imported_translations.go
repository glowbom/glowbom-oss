package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type importedChatExport struct {
	Format      string `json:"format"`
	StackID     string `json:"stackID"`
	DisplayName string `json:"displayName"`
	Path        string `json:"path"`
}

// Account downloads keep their original exports and manifest alongside the
// runnable project. Read those as a fallback without modifying either format.
func appendImportedChatTranslations(root *os.Root, local []chatTranslation) ([]chatTranslation, error) {
	data, err := readImportedChatFile(root, "glowbom.json", 64<<10)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Exports    []importedChatExport `json:"exports"`
		ExportedAt string               `json:"exportedAt"`
		UpdatedAt  string               `json:"updatedAt"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return nil, errors.New("Could not read the project's saved translations.")
	}
	if len(manifest.Exports) > 512 {
		return nil, errors.New("The project lists too many saved exports.")
	}
	createdAt := ""
	for _, value := range []string{manifest.UpdatedAt, manifest.ExportedAt} {
		if _, err := time.Parse(time.RFC3339Nano, value); err == nil {
			createdAt = value
			break
		}
	}
	seen := make(map[string]bool, len(local))
	total := 0
	for _, item := range local {
		seen[item.ID] = true
		total += len(item.Code)
	}
	if total > 16<<20 || len(local) > maxChatTranslations {
		return nil, errors.New("Saved translations are too large to open.")
	}
	for _, export := range manifest.Exports {
		stack, ok := importedChatStack(export)
		if !ok || seen[stack.ID] {
			continue
		}
		code, err := readImportedChatFile(root, export.Path, maxChatResultBytes)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(code) || strings.ContainsRune(string(code), '\x00') {
			return nil, errors.New("A saved translation is not valid source text.")
		}
		if strings.TrimSpace(string(code)) == "" {
			continue
		}
		total += len(code)
		if total > 16<<20 || len(local) >= maxChatTranslations {
			return nil, errors.New("Saved translations are too large to open.")
		}
		// The account format has no prototype revision for each translation.
		// An empty hash records that uncertainty instead of asserting freshness.
		local = append(local, chatTranslation{
			chatStack: stack,
			Code:      string(code),
			CreatedAt: createdAt,
			Source:    "imported",
		})
		seen[stack.ID] = true
	}
	return local, nil
}

func importedChatStack(export importedChatExport) (chatStack, bool) {
	format := strings.ToLower(strings.TrimSpace(export.Format))
	if format == "" || format == "html" || format == "htmlprocessed" {
		return chatStack{}, false
	}
	id := strings.ToLower(strings.TrimSpace(export.StackID))
	if id == "" {
		id = format
	}
	name := strings.TrimSpace(export.DisplayName)
	descriptions := map[string]chatStack{
		"swiftui":     {Name: "SwiftUI", Description: "SwiftUI for iOS and macOS using native views and Swift."},
		"kotlin":      {Name: "Kotlin", Description: "Kotlin with Jetpack Compose for Android."},
		"nextjs":      {Name: "Next.js", Description: "Next.js with TypeScript, React, and the App Router."},
		"godot":       {Name: "Godot", Description: "Godot 4 with GDScript, scenes, and native controls."},
		"flutter":     {Name: "Flutter", Description: "Flutter with Dart and Material widgets."},
		"reactnative": {Name: "React Native", Description: "React Native with TypeScript."},
		"flask":       {Name: "Flask", Description: "Python with Flask, HTML templates, and CSS."},
		"php":         {Name: "PHP", Description: "PHP with responsive HTML and CSS."},
	}
	stack := descriptions[id]
	stack.ID = id
	if name != "" {
		stack.Name = name
	} else if stack.Name == "" {
		stack.Name = id
	}
	// Custom stack instructions are not part of account exports. Keep those
	// empty so the UI can ask for them before regenerating the saved code.
	validation := stack
	if validation.Description == "" {
		validation.Description = "Imported stack"
	}
	return stack, validateChatStack(validation) == nil
}

func readImportedChatFile(root *os.Root, name string, limit int64) ([]byte, error) {
	if !filepath.IsLocal(name) || strings.Contains(name, "\\") || filepath.Clean(name) != name {
		return nil, errors.New("A saved translation has an invalid project path.")
	}
	info, err := root.Stat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("A saved translation is invalid or too large.")
	}
	file, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("Could not read a saved translation inside the project folder.")
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("A saved translation is invalid or too large.")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("A saved translation is invalid or too large.")
	}
	return data, nil
}
