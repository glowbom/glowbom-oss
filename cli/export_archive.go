package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

type savedProjectExport struct {
	format string
	path   string
}

const maxAssembledProjectBytes = 64 * 1024 * 1024

var exportDestinations = []struct {
	format string
	path   string
}{
	{"swiftUI", "apple/Custom/AiExtensions.swift"},
	{"kotlin", "android/app/src/main/java/com/glowbom/custom/AiExtensions.kt"},
	{"nextjs", "web/src/app/components/AiExtensions.tsx"},
}

// assembleProject combines validated source exports and a starter in memory.
// It leaves the saved source files intact and installs copies into the starter.
func assembleProject(ctx context.Context, savedZip, starterZip []byte) ([]projectArchiveEntry, error) {
	savedEntries, err := validateProjectArchive(ctx, savedZip)
	if err != nil {
		return nil, err
	}
	starterEntries, err := validateTemplateArchive(ctx, starterZip)
	if err != nil {
		return nil, err
	}
	saved := exportEntryMap(savedEntries)
	starter := exportEntryMap(starterEntries)
	savedManifest, err := readExportManifest(saved)
	if err != nil {
		return nil, err
	}
	starterManifest, err := readExportManifest(starter)
	if err != nil {
		return nil, err
	}
	exports, err := readSavedProjectExports(savedManifest, saved)
	if err != nil {
		return nil, err
	}
	targets, targetsOK := starterManifest["targets"].(map[string]any)
	_, hasAgents := starter["AGENTS.md"]
	_, hasPrototype := starter["prototype/index.html"]
	if !targetsOK || !hasAgents || !hasPrototype {
		return nil, errors.New("the project starter is incomplete")
	}

	files := make(map[string]projectArchiveEntry, len(starter)+len(saved))
	for name, entry := range starter {
		if strings.HasPrefix(name, "prototype/assets/") || name == "prototype/assets.json" {
			continue
		}
		files[name] = entry
	}
	for name, entry := range saved {
		if name == "glowbom.json" {
			continue
		}
		if _, exists := files[name]; exists && name != "icon.png" {
			return nil, errors.New("the saved project conflicts with the starter")
		}
		files[name] = entry
	}

	sourceFor := func(format string) string {
		for _, export := range exports {
			if export.format == format {
				text := string(saved[export.path].data)
				if strings.TrimSpace(text) != "" {
					return text
				}
			}
		}
		return ""
	}
	prototype := sourceFor("htmlProcessed")
	if prototype == "" {
		prototype = sourceFor("html")
	}
	if prototype == "" {
		return nil, errors.New("the saved project has no HTML prototype")
	}
	files["prototype/index.html"] = exportGeneratedFile("prototype/index.html", []byte(prototype))
	for _, destination := range exportDestinations {
		source := sourceFor(destination.format)
		if source == "" {
			continue
		}
		if _, exists := starter[destination.path]; !exists {
			return nil, errors.New("the project starter is incomplete")
		}
		files[destination.path] = exportGeneratedFile(destination.path, []byte(prepareProjectExtension(source, destination.format)))
	}
	if iconPath, ok := savedManifest["iconPath"].(string); ok && iconPath != "" {
		icon := saved[iconPath]
		for _, name := range []string{"icon.png", "web/src/app/icon.png", "web/public/icon.png"} {
			if existing, exists := saved[name]; exists && !bytes.Equal(existing.data, icon.data) {
				return nil, errors.New("the saved project conflicts with the starter icon paths")
			}
			files[name] = exportGeneratedFile(name, icon.data)
		}
	}

	displayName := exportNonempty(savedManifest["displayName"])
	if displayName == "" {
		displayName = exportNonempty(savedManifest["name"])
	}
	if displayName == "" {
		displayName = "Glowbom"
	}
	manifest := make(map[string]any, len(starterManifest)+len(savedManifest))
	for key, value := range starterManifest {
		manifest[key] = value
	}
	for key, value := range savedManifest {
		manifest[key] = value
	}
	name := exportNonempty(savedManifest["name"])
	if name == "" {
		name = displayName
	}
	manifest["name"] = name
	manifest["displayName"] = displayName
	manifest["targets"] = targets
	manifest["prototypePath"] = "prototype/index.html"
	manifest["agentsPath"] = "AGENTS.md"
	for _, key := range []string{"createdAt", "updatedAt", "exportedAt"} {
		if _, exists := savedManifest[key]; !exists {
			delete(manifest, key)
		}
	}
	delete(manifest, "prototypeAssetsManifestPath")
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, errors.New("the project manifest could not be prepared")
	}
	files["glowbom.json"] = exportGeneratedFile("glowbom.json", append(manifestData, '\n'))
	if err := validateExportPaths(files); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]projectArchiveEntry, 0, len(names))
	total := 0
	for _, name := range names {
		entry := files[name]
		total += len(entry.data)
		if total > maxAssembledProjectBytes {
			return nil, errors.New("the assembled project exceeds the 64 MiB size limit")
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func exportEntryMap(entries []projectArchiveEntry) map[string]projectArchiveEntry {
	files := make(map[string]projectArchiveEntry, len(entries))
	for _, entry := range entries {
		files[entry.name] = entry
	}
	return files
}

func exportGeneratedFile(name string, data []byte) projectArchiveEntry {
	return projectArchiveEntry{name: name, data: data, mode: 0644}
}

func readExportManifest(files map[string]projectArchiveEntry) (map[string]any, error) {
	entry, exists := files["glowbom.json"]
	if !exists || len(entry.data) > maxProjectManifestBytes || !utf8.Valid(entry.data) {
		return nil, errors.New("the project manifest is missing, invalid, or too large")
	}
	var manifest map[string]any
	decoder := json.NewDecoder(bytes.NewReader(entry.data))
	decoder.UseNumber()
	if err := decoder.Decode(&manifest); err != nil || manifest == nil {
		return nil, errors.New("the project manifest is invalid")
	}
	return manifest, nil
}

func readSavedProjectExports(manifest map[string]any, files map[string]projectArchiveEntry) ([]savedProjectExport, error) {
	invalid := errors.New("the saved project export format is invalid")
	values, ok := manifest["exports"].([]any)
	if !ok || len(values) == 0 || len(values) > maxProjectFiles {
		return nil, invalid
	}
	exports := make([]savedProjectExport, 0, len(values))
	for _, value := range values {
		export, ok := value.(map[string]any)
		if !ok {
			return nil, invalid
		}
		format, formatOK := export["format"].(string)
		path, pathOK := export["path"].(string)
		file, exists := files[path]
		if !formatOK || strings.TrimSpace(format) == "" || !pathOK || !exists {
			return nil, invalid
		}
		switch format {
		case "html", "htmlProcessed", "swiftUI", "kotlin", "nextjs":
			if !utf8.Valid(file.data) {
				return nil, errors.New("a saved project export is not valid UTF-8 text")
			}
		}
		exports = append(exports, savedProjectExport{format: format, path: path})
	}
	return exports, nil
}

func exportNonempty(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func validateExportPaths(files map[string]projectArchiveEntry) error {
	spellings := make(map[string]string)
	kinds := make(map[string]bool)
	for name := range files {
		if !validProjectArchivePath(name) {
			return errors.New("the assembled project contains an unsafe path")
		}
		parts := strings.Split(name, "/")
		for index := 1; index <= len(parts); index++ {
			prefix := strings.Join(parts[:index], "/")
			folded := strings.ToLower(prefix)
			isFile := index == len(parts)
			if previous, exists := spellings[folded]; exists && (previous != prefix || kinds[folded] || isFile) {
				return errors.New("the saved project and starter contain conflicting paths")
			}
			spellings[folded] = prefix
			kinds[folded] = isFile
		}
	}
	return nil
}

var exportSwiftImport = regexp.MustCompile(`(?m)^\s*import\s+SwiftUI\b`)
var exportKotlinPackage = regexp.MustCompile(`(?m)^\s*package\s+[\w.]+`)
var exportClientDirective = regexp.MustCompile("(?m)^[ \\t]*[\"']use client[\"']")
var exportSwiftEnabled = regexp.MustCompile(`(?m)^([ \t]*static[ \t]+let[ \t]+enabled(?:[ \t]*:[ \t]*Bool)?[ \t]*=[ \t]*)false\b`)
var exportKotlinEnabled = regexp.MustCompile(`(?m)^([ \t]*(?:const[ \t]+)?(?:val|var)[ \t]+enabled(?:[ \t]*:[ \t]*Boolean)?[ \t]*=[ \t]*)false\b`)
var exportTypeScriptEnabled = regexp.MustCompile(`(?m)^([ \t]*(?:export[ \t]+)?(?:const|let|var)[ \t]+enabled(?:[ \t]*:[ \t]*boolean)?[ \t]*=[ \t]*)false\b`)

func prepareProjectExtension(source, format string) string {
	var enabled *regexp.Regexp
	switch format {
	case "swiftUI":
		if !exportSwiftImport.MatchString(source) {
			source = "import SwiftUI\n\n" + source
		}
		enabled = exportSwiftEnabled
	case "kotlin":
		if !exportKotlinPackage.MatchString(source) {
			source = "package com.glowbom.custom\n\n" + source
		}
		enabled = exportKotlinEnabled
	default:
		if !exportClientDirective.MatchString(source) {
			source = "\"use client\";\n\n" + source
		}
		enabled = exportTypeScriptEnabled
	}
	return enabled.ReplaceAllString(source, "${1}true")
}
