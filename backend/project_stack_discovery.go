package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type discoveredStack struct {
	Name         string   `json:"name"`
	Directory    string   `json:"directory"`
	Stack        string   `json:"stack"`
	Preset       string   `json:"preset,omitempty"`
	Evidence     string   `json:"evidence"`
	Target       string   `json:"target,omitempty"`
	BuildTarget  string   `json:"buildTarget,omitempty"`
	PreviewMode  string   `json:"previewMode"`
	Command      []string `json:"command,omitempty"`
	Available    bool     `json:"available"`
	NeedsInstall bool     `json:"needsInstall"`
}

// Discovery reads bounded project metadata. It never runs tools or saves settings.
func discoverProjectStacks(project string, defs []previewDefinition) ([]discoveredStack, bool, error) {
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		return nil, false, err
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	read := func(path string) []byte {
		info, err := root.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 256*1024 {
			return nil
		}
		f, err := root.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 256*1024 {
			return nil
		}
		data, _ := io.ReadAll(io.LimitReader(f, 256*1024))
		return data
	}
	exists := func(path string) bool { info, err := root.Stat(path); return err == nil && info.Mode().IsRegular() }
	known := map[string]previewDefinition{}
	for _, d := range defs {
		known[filepath.Clean(d.Directory)] = d
	}
	manifestNames := map[string]string{}
	var manifest struct {
		Targets map[string]struct {
			OutputDir string `json:"outputDir"`
			Stack     string `json:"stack"`
		} `json:"targets"`
	}
	_ = json.Unmarshal(read("glowbom.json"), &manifest)
	seeds := map[string]bool{".": true}
	for _, d := range defs {
		seeds[filepath.Clean(d.Directory)] = true
	}
	for id, t := range manifest.Targets {
		dir := t.OutputDir
		if dir == "" {
			dir = id
		}
		dir = filepath.Clean(dir)
		if !filepath.IsLocal(dir) {
			continue
		}
		seeds[dir] = true
		switch t.Stack {
		case "swiftui":
			manifestNames[dir] = "Apple"
		case "kotlin":
			manifestNames[dir] = "Android"
		case "nextjs":
			manifestNames[dir] = "Web"
		}
	}
	skipped := map[string]bool{"__MACOSX": true, "node_modules": true, "vendor": true, "build": true, "dist": true, "target": true, "assets": true, "history": true, "current_instructions": true, "Pods": true, "DerivedData": true}
	visited := map[string]bool{}
	canonical := map[string]bool{}
	found := []discoveredStack{}
	count := 0
	limited := false
	var scan func(string, int)
	scan = func(dir string, depth int) {
		if visited[dir] {
			return
		}
		visited[dir] = true
		if count >= 300 {
			limited = true
			return
		}
		count++
		full, err := filepath.EvalSymlinks(filepath.Join(project, dir))
		if err != nil || !isPathWithin(project, full) {
			return
		}
		if canonical[full] {
			return
		}
		canonical[full] = true
		f, err := root.Open(dir)
		if err != nil {
			return
		}
		entries, readErr := f.ReadDir(256)
		_ = f.Close()
		if readErr != nil && readErr != io.EOF {
			return
		}
		if len(entries) == 256 {
			limited = true
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		item := discoveredStack{Name: filepath.Base(dir), Directory: filepath.ToSlash(dir), PreviewMode: "none"}
		if dir == "." {
			item.Name = filepath.Base(project)
		}
		at := func(name string) string { return filepath.Join(dir, name) }
		var pkg struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		_ = json.Unmarshal(read(at("package.json")), &pkg)
		dep := func(name string) bool { return pkg.Dependencies[name] != "" || pkg.DevDependencies[name] != "" }
		switch {
		case exists(at("src-tauri/tauri.conf.json")) || exists(at("src-tauri/Tauri.toml")):
			item.Stack, item.Evidence = "Tauri", "src-tauri configuration"
			if dep("react") {
				item.Preset = "tauri-react"
			}
			item.PreviewMode = "auto"
		case dep("expo"):
			item.Stack, item.Preset, item.Evidence = "React Native + Expo", "expo", "package.json · expo"
		case dep("next"):
			item.Stack, item.Preset, item.Evidence = "Next.js", "nextjs", "package.json · next"
			item.PreviewMode = "auto"
		case dep("vite"):
			item.Stack, item.Evidence = "Vite", "package.json · vite"
			item.PreviewMode = "auto"
			if dep("react") {
				item.Stack, item.Preset = "React + Vite", "react-vite"
			} else if dep("vue") {
				item.Stack = "Vue + Vite"
			} else if dep("svelte") {
				item.Stack = "Svelte + Vite"
			}
		case exists(at("project.godot")):
			item.Stack, item.Evidence = "Godot", "project.godot"
		case strings.Contains(string(read(at("pubspec.yaml"))), "sdk: flutter"):
			item.Stack, item.Preset, item.Evidence = "Flutter", "flutter", "pubspec.yaml · Flutter SDK"
			if exists(at("web/index.html")) {
				item.PreviewMode = "command"
				item.Command = []string{"flutter", "run", "-d", "web-server", "--web-hostname", "{host}", "--web-port", "{port}"}
			}
		case exists(at("public/index.php")) || exists(at("index.php")):
			item.Stack, item.Preset, item.Evidence = "PHP", "php", "PHP entry point"
			item.PreviewMode = "command"
			item.Command = []string{"php", "-S", "{host}:{port}"}
			if exists(at("public/index.php")) {
				item.Command = append(item.Command, "-t", "public")
			}
		case exists(at("go.mod")):
			item.Stack, item.Evidence = "Go", "go.mod"
		case exists(at("pyproject.toml")) || exists(at("requirements.txt")):
			item.Stack, item.Evidence = "Python", "Python project metadata"
		case exists(at("build.gradle.kts")) || exists(at("build.gradle")):
			gradle := string(read(at("build.gradle.kts"))) + string(read(at("build.gradle"))) + string(read(at("app/build.gradle.kts"))) + string(read(at("app/build.gradle")))
			if strings.Contains(gradle, "com.android.") {
				item.Stack, item.Preset, item.Evidence = "Android", "kotlin", "Android Gradle configuration"
			}
		case exists(at("index.html")):
			item.Stack, item.Preset, item.Evidence = "HTML", "html", "index.html"
			item.PreviewMode = "auto"
		}
		if item.Stack == "" {
			for _, e := range entries {
				if e.IsDir() && ((strings.HasSuffix(e.Name(), ".xcodeproj") && exists(at(e.Name()+"/project.pbxproj"))) || (strings.HasSuffix(e.Name(), ".xcworkspace") && exists(at(e.Name()+"/contents.xcworkspacedata")))) {
					item.Stack, item.Preset, item.Evidence = "Apple / Xcode", "swiftui", e.Name()
					break
				}
			}
		}
		if label := manifestNames[dir]; label != "" {
			item.Name = label
		}
		if d, ok := known[dir]; ok {
			if item.Stack != "" || strings.HasPrefix(d.ID, "custom-") {
				item.Name, item.Target = d.Name, d.ID
				if item.Stack == "" {
					item.Stack, item.Evidence = "Custom stack", "Saved stack settings"
				}
				if strings.HasPrefix(d.ID, "custom-") {
					item.PreviewMode, item.Command = d.PreviewMode, d.Command
					if item.PreviewMode == "" {
						item.PreviewMode = "auto"
					}
				}
			}
		}
		if item.Stack != "" {
			if dir == "apple" && item.Preset == "swiftui" {
				item.BuildTarget = "apple"
			}
			if dir == "android" && item.Preset == "kotlin" {
				item.BuildTarget = "android"
			}
			if item.Target != "" {
				item.BuildTarget = item.Target
			}
			d := previewDefinition{Directory: dir, PreviewMode: item.PreviewMode, Command: item.Command}
			v := inspectPreviewDefinition(project, d)
			item.Available, item.NeedsInstall = v.Available, v.NeedsInstall
			found = append(found, item)
			// Child source folders and web exports are part of this app, not extra apps.
			if dir != "." {
				return
			}
		}
		if depth >= 3 {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() || strings.HasPrefix(name, ".") || skipped[name] || strings.HasSuffix(name, ".xcodeproj") || strings.HasSuffix(name, ".xcworkspace") {
				continue
			}
			scan(at(name), depth+1)
		}
	}
	seedList := []string{}
	for dir := range seeds {
		seedList = append(seedList, dir)
	}
	sort.Strings(seedList)
	// Explicit folders are inspected even when nested beyond the normal scan depth.
	scan(".", 0)
	for _, dir := range seedList {
		if dir != "." {
			scan(dir, 0)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Directory < found[j].Directory })
	return found, limited, nil
}
