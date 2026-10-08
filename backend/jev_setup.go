package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

var jevInstallMu sync.Mutex

// Keep an existing user tool, including a JavaScript or singular-directory install.
func existingJevTool(configDir string) (bool, error) {
	for _, directory := range []string{"tools", "tool"} {
		for _, name := range []string{"jev.ts", "jev.js"} {
			info, err := os.Stat(filepath.Join(configDir, directory, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return false, err
			}
			if !info.Mode().IsRegular() {
				return false, errors.New("the existing Jev tool is not a file")
			}
			return true, nil
		}
	}
	return false, nil
}

func bundledJevSource() ([]byte, error) {
	paths := []string{filepath.Join("..", "extras", "jev", "jev.ts")}
	if resources := os.Getenv("GLOWBOM_RESOURCE_DIR"); filepath.IsAbs(resources) {
		paths = append([]string{filepath.Join(resources, "jev.ts")}, paths...)
	}
	// Source checkouts may launch a development backend from a different directory.
	if _, source, _, ok := runtime.Caller(0); ok && filepath.IsAbs(source) {
		paths = append(paths, filepath.Join(filepath.Dir(source), "..", "extras", "jev", "jev.ts"))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil {
			if len(data) == 0 {
				return nil, errors.New("the bundled Jev tool is empty")
			}
			return data, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("could not read the bundled Jev tool: %w", err)
		}
	}
	return nil, errors.New("the bundled Jev tool is unavailable")
}

func ensureJevInstalled() error {
	paths, err := userOpenCodeRuntimePaths()
	if err != nil {
		return err
	}
	return installJevTool(filepath.Join(paths.ConfigHome, "opencode"), bundledJevSource)
}

func installJevTool(configDir string, source func() ([]byte, error)) error {
	jevInstallMu.Lock()
	defer jevInstallMu.Unlock()
	if exists, err := existingJevTool(configDir); exists || err != nil {
		return err
	}
	data, err := source()
	if err != nil {
		return err
	}
	directory := filepath.Join(configDir, "tools")
	if err := os.MkdirAll(directory, 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".glowbom-jev-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err = file.Chmod(0644); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// A hard link publishes the complete file atomically and cannot replace a file.
	err = os.Link(temporary, filepath.Join(directory, "jev.ts"))
	if errors.Is(err, os.ErrExist) {
		if exists, checkErr := existingJevTool(configDir); exists || checkErr != nil {
			return checkErr
		}
	}
	return err
}

func loadedJevTool(ctx context.Context, s *chatService) (bool, error) {
	var ids []string
	if err := s.json(ctx, http.MethodGet, "/experimental/tool/ids", nil, &ids); err != nil {
		return false, err
	}
	for _, id := range ids {
		if id == "jev" {
			return true, nil
		}
	}
	return false, nil
}

func prepareJevTool(ctx context.Context, s *chatService) bool {
	return prepareJevToolWithInstaller(ctx, s, ensureJevInstalled)
}

func prepareJevToolWithInstaller(ctx context.Context, s *chatService, install func() error) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	loaded, err := loadedJevTool(ctx, s)
	if loaded || err != nil {
		return loaded
	}
	if err := install(); err != nil {
		return false
	}
	var statuses map[string]struct {
		Type string `json:"type"`
	}
	if err := s.json(ctx, http.MethodGet, "/session/status", nil, &statuses); err != nil || statuses == nil {
		return false
	}
	for _, status := range statuses {
		if status.Type != "idle" {
			return false
		}
	}
	// Refresh only this idle project before its next session starts.
	if err := s.json(ctx, http.MethodPost, "/instance/dispose", nil, nil); err != nil {
		return false
	}
	loaded, err = loadedJevTool(ctx, s)
	return loaded && err == nil
}
