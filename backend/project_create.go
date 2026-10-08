package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func sketchProjectDestination(parent, name string) (string, error) {
	if name == "" || name != strings.TrimSpace(name) || strings.HasPrefix(name, ".") || len(name) > 100 || strings.ContainsAny(name, "/\\:\x00\r\n\t") {
		return "", errors.New("Choose a project name without slashes, dots at the start, or control characters.")
	}
	for _, ch := range name {
		if ch < 32 || ch == 127 {
			return "", errors.New("Choose a project name without control characters.")
		}
	}
	if !filepath.IsAbs(parent) {
		return "", errors.New("Choose an absolute parent folder path.")
	}
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", errors.New("The parent folder could not be opened.")
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return "", errors.New("Choose an existing parent folder.")
	}
	return filepath.Join(parent, name), nil
}

func checkSketchProject(parent, name string) (map[string]interface{}, error) {
	root, err := sketchProjectDestination(parent, name)
	if err != nil {
		return nil, err
	}
	exists := func(path string) (bool, error) {
		_, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, errors.New("Could not check the project folder. Check its permissions.")
		}
		return true, nil
	}
	taken, err := exists(root)
	if err != nil {
		return nil, err
	}
	result := map[string]interface{}{"success": true, "path": root, "exists": taken}
	if taken {
		for i := 2; i <= 100; i++ {
			suffix := fmt.Sprintf(" %d", i)
			base := []rune(name)
			for len(string(base))+len(suffix) > 100 {
				base = base[:len(base)-1]
			}
			candidate := strings.TrimSpace(string(base)) + suffix
			used, err := exists(filepath.Join(filepath.Dir(root), candidate))
			if err != nil {
				return nil, err
			}
			if !used {
				result["suggestedName"] = candidate
				break
			}
		}
	}
	return result, nil
}

// Create only a new child directory. Never initialize over an existing project.
func createSketchProject(parent, name string) (string, error) {
	root, err := sketchProjectDestination(parent, name)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(root, 0755); err != nil {
		if os.IsExist(err) {
			return "", errors.New("That folder already exists. Choose another name or open the existing project.")
		}
		return "", errors.New("Could not create the project folder. Check its permissions.")
	}
	paths := GetProjectPaths(root)
	if err := os.MkdirAll(paths.Assets, 0755); err != nil {
		return "", err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	project := &GlowbomProject{Name: name, Version: "1.0.0", Targets: map[string]Target{}, CreatedAt: now, UpdatedAt: now}
	if err := SaveProject(paths.Manifest, project); err != nil {
		return "", err
	}
	return root, nil
}

func openCodeCreateProjectHandler(w http.ResponseWriter, r *http.Request) {
	createStarterProjectHandler(runAccountCLI)(w, r)
}

func createStarterProjectHandler(run accountCLIRunner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ParentPath string `json:"parentPath"`
			Name       string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			http.Error(w, "Invalid project request", http.StatusBadRequest)
			return
		}
		root, err := createStarterProject(r.Context(), req.ParentPath, req.Name, run)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "path": root})
	}
}

// Inspect a destination without creating or changing any files.
func openCodeCheckProjectHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ParentPath string `json:"parentPath"`
		Name       string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "Invalid project request", http.StatusBadRequest)
		return
	}
	result, err := checkSketchProject(req.ParentPath, req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
