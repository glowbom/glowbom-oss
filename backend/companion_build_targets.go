package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type companionBuildTarget struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Directory string `json:"directory"`
	Available bool   `json:"available"`
}

func appendBuildTargets(ids []string) []string {
	if ids == nil {
		return nil
	}
	return append([]string{}, ids...)
}

func (s *companionSession) rememberBuildTargets(project companionProject, ids []string) {
	if ids == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buildTargets == nil {
		s.buildTargets = map[string][]string{}
	}
	s.buildTargets[project.ID] = appendBuildTargets(ids)
}

// A missing target can be created. An existing ancestor must stay in the shared
// project, including when a saved stack points through a symbolic link.
func companionBuildDirectoryAllowed(project, directory string) bool {
	if !filepath.IsLocal(directory) {
		return false
	}
	path := filepath.Join(project, directory)
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			info, statErr := os.Stat(resolved)
			return statErr == nil && info.IsDir() && isPathWithin(project, resolved)
		}
		if !errors.Is(err, os.ErrNotExist) || path == project {
			return false
		}
		if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		path = filepath.Dir(path)
	}
}

func companionValidateBuildTargets(project string, ids []string) error {
	if ids == nil {
		return nil
	}
	if _, err := prepareStackBuildInstructions(project, "", ids); err != nil {
		return err
	}
	defs, err := projectBuildDefinitions(project)
	if err != nil {
		return err
	}
	byID := map[string]previewDefinition{}
	for _, def := range defs {
		byID[def.ID] = def
	}
	for _, id := range ids {
		if !companionBuildDirectoryAllowed(project, byID[id].Directory) {
			return errors.New("Choose a stack folder inside the shared project.")
		}
	}
	return nil
}

func (s *companionSession) projectEditors(ctx context.Context, project companionProject) ([]stackEditor, error) {
	writer, err := s.projectTools(ctx, previewRequest{Path: project.path, ProjectRoot: true, Action: "tools"})
	if err != nil {
		return nil, err
	}
	var response struct {
		Editors       []stackEditor `json:"editors"`
		CanOpenFolder bool          `json:"canOpenFolder"`
	}
	if json.Unmarshal(writer.buffer, &response) != nil {
		return nil, errors.New("Desktop returned an invalid editor list.")
	}
	editors := []stackEditor{}
	if response.CanOpenFolder {
		editors = append(editors, stackEditor{ID: "folder", Name: stackFileManagerLabel()})
	}
	seen := map[string]bool{}
	for _, editor := range response.Editors {
		for _, supported := range stackEditors {
			if supported.ID == editor.ID && !seen[editor.ID] {
				editors = append(editors, supported.stackEditor)
				seen[editor.ID] = true
			}
		}
	}
	return editors, nil
}

func (s *companionSession) projectTools(ctx context.Context, request previewRequest) (*companionWriter, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	writer := &companionWriter{header: make(http.Header), job: &companionJob{}}
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return nil, errors.New("This connection expired. Pair again on Desktop.")
	}
	s.api.ServeHTTP(writer, s.internalRequest(ctx, http.MethodPost, "/preview", request))
	if writer.code >= 400 || ctx.Err() != nil || s.ctx.Err() != nil {
		return nil, errors.New("Desktop could not open its project tools. Check the project and installed applications on Desktop.")
	}
	return writer, nil
}

func (s *companionSession) buildTargetCatalog(w http.ResponseWriter, r *http.Request, project companionProject) {
	defs, err := projectBuildDefinitions(project.path)
	if err != nil {
		http.Error(w, "Desktop could not read this project's saved stacks. Check its stack settings on Desktop.", http.StatusBadRequest)
		return
	}
	targets := []companionBuildTarget{}
	inProject := []string{}
	allowed := map[string]bool{}
	for _, def := range defs {
		available := companionBuildDirectoryAllowed(project.path, def.Directory)
		targets = append(targets, companionBuildTarget{ID: def.ID, Name: companionPublicText(def.Name, 160), Directory: filepath.ToSlash(filepath.Clean(def.Directory)), Available: available})
		allowed[def.ID] = available
		if available && (strings.HasPrefix(def.ID, "custom-") || inspectPreviewDefinition(project.path, def).directory != "") {
			inProject = append(inProject, def.ID)
		}
	}
	s.mu.Lock()
	remembered := appendBuildTargets(s.buildTargets[project.ID])
	s.mu.Unlock()
	selected := []string{}
	seen := map[string]bool{}
	if remembered == nil {
		remembered = inProject
	}
	for _, id := range remembered {
		if allowed[id] && !seen[id] {
			selected = append(selected, id)
			seen[id] = true
		}
	}
	if len(selected) == 0 && allowed["prototype"] {
		selected = append(selected, "prototype")
	}
	editors, err := s.projectEditors(r.Context(), project)
	if err != nil {
		// Stack selection remains useful on a Desktop version without editor tools.
		editors = []stackEditor{}
	}
	writeJSON(w, map[string]any{"targets": targets, "selectedTargets": selected, "editors": editors})
}

func (s *companionSession) openProject(w http.ResponseWriter, r *http.Request, project companionProject) {
	var input struct {
		Editor string `json:"editor"`
		Target string `json:"target,omitempty"`
	}
	if !companionDecode(w, r, &input, 1024) {
		return
	}
	editors, err := s.projectEditors(r.Context(), project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	available := false
	for _, editor := range editors {
		available = available || editor.ID == input.Editor
	}
	if !available {
		http.Error(w, "Choose an installed Desktop editor.", http.StatusBadRequest)
		return
	}
	if input.Target != "" {
		if err := companionValidateBuildTargets(project.path, []string{input.Target}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	_, err = s.projectTools(r.Context(), previewRequest{Path: project.path, ProjectRoot: input.Target == "", Target: input.Target, Editor: input.Editor, Action: "open"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"success": true})
}
