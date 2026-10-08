package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

type companionBuildModel struct {
	Available   bool   `json:"available"`
	ProjectID   string `json:"projectId"`
	Model       string `json:"model"`
	Revision    uint64 `json:"revision"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
	Source      string `json:"source,omitempty"`
	ProjectPath string `json:"projectPath,omitempty"`
}

// Return only the public model fields from connected text-and-tool providers.
// Provider configuration, keys and account status never cross this connection.
func (s *companionSession) models(ctx context.Context) ([]chatModel, error) {
	models, err := s.buildModelOptions(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	selected := []string{}
	for _, choice := range s.buildModels {
		selected = append(selected, choice.Model)
	}
	s.mu.Unlock()
	return boundedCompanionModels(models, selected...), nil
}

func (s *companionSession) buildModelOptions(ctx context.Context) ([]chatModel, error) {
	connected, err := s.readModelCatalog(ctx, "Desktop could not load its connected build models. Check Desktop and try again.")
	if err != nil {
		return nil, err
	}
	models := []chatModel{}
	seen := map[string]bool{}
	for _, model := range connected {
		cursor := strings.HasPrefix(model.ID, "cursor/")
		claudeCode := strings.HasPrefix(model.ID, "claude-code/")
		if (!model.Build && !cursor) || !validCompanionModelID(model.ID) || (cursor && cursorCLIModel(model.ID) == "") || (claudeCode && claudeCodeCLIModel(model.ID) == "") || seen[model.ID] {
			continue
		}
		model.Build = true
		model.Name = companionPublicText(model.Name, 160)
		model.Provider = companionPublicText(model.Provider, 160)
		models = append(models, model)
		seen[model.ID] = true
	}
	return models, nil
}

func (s *companionSession) readModelCatalog(parent context.Context, failure string) ([]chatModel, error) {
	ctx, stop := s.chatContext(parent)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	writer := &companionWriter{header: make(http.Header), job: &companionJob{}}
	s.api.ServeHTTP(writer, s.internalRequest(ctx, http.MethodGet, "/chat/models", nil))
	// The local catalog can return a valid partial list when a runtime times out.
	// Explicit cancellation and pairing revocation still invalidate every result.
	if writer.code < 200 || writer.code >= 300 || errors.Is(ctx.Err(), context.Canceled) || s.ctx.Err() != nil || !s.now().Before(s.expires) {
		return nil, errors.New(failure)
	}
	var response struct {
		Models []chatModel `json:"models"`
	}
	if json.Unmarshal(writer.buffer, &response) != nil {
		return nil, errors.New("Desktop returned an invalid model list.")
	}
	return response.Models, nil
}

// Share the bounded catalog across connected sources instead of dropping late
// runtimes behind a large provider catalog. IDs remain stable and sorted.
func boundedCompanionModels(models []chatModel, preferred ...string) []chatModel {
	const limit = 500
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	if len(models) <= limit {
		return models
	}
	selected := make([]chatModel, 0, limit)
	wanted, included := map[string]bool{}, map[string]bool{}
	for _, id := range preferred {
		wanted[id] = true
	}
	for _, model := range models {
		if wanted[model.ID] && len(selected) < limit {
			selected = append(selected, model)
			included[model.ID] = true
		}
	}
	sources := map[string][]chatModel{}
	order := []string{}
	for _, model := range models {
		if included[model.ID] {
			continue
		}
		source, _, _ := strings.Cut(model.ID, "/")
		if _, exists := sources[source]; !exists {
			order = append(order, source)
		}
		sources[source] = append(sources[source], model)
	}
	for index := 0; len(selected) < limit; index++ {
		for _, source := range order {
			if options := sources[source]; index < len(options) {
				selected = append(selected, options[index])
				if len(selected) == limit {
					break
				}
			}
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	return selected
}

func validCompanionModelID(value string) bool {
	provider, model, ok := strings.Cut(value, "/")
	return ok && provider != "" && model != "" && len(value) <= 160 && !strings.ContainsAny(value, "\r\n\x00") && strings.TrimSpace(value) == value
}

func (s *companionSession) validateModel(ctx context.Context, model string) error {
	if model == "" {
		return nil
	}
	if !validCompanionModelID(model) {
		return errors.New("Choose a connected Desktop build model.")
	}
	models, err := s.buildModelOptions(ctx)
	if err != nil {
		return err
	}
	for _, option := range models {
		if option.ID == model {
			return nil
		}
	}
	return errors.New("This model is no longer available for building. Choose a connected Desktop build model.")
}

func (s *companionSession) modelChoice(project companionProject, local bool) companionBuildModel {
	s.mu.Lock()
	choice := s.buildModels[project.ID]
	s.mu.Unlock()
	choice.Available, choice.ProjectID = true, project.ID
	if local {
		choice.ProjectPath = project.path
	} else {
		choice.ProjectPath = ""
	}
	return choice
}

func (s *companionSession) setModel(project companionProject, model, source string) companionBuildModel {
	return s.setModelIfRevision(project, model, source, nil)
}

func (s *companionSession) setModelIfRevision(project companionProject, model, source string, expectedRevision *uint64) companionBuildModel {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buildModels == nil {
		s.buildModels = map[string]companionBuildModel{}
	}
	previous, exists := s.buildModels[project.ID]
	if expectedRevision != nil && previous.Revision != *expectedRevision {
		return previous
	}
	if exists && previous.Model == model {
		return previous
	}
	s.modelRevision++
	choice := companionBuildModel{Available: true, ProjectID: project.ID, Model: model, Revision: s.modelRevision, UpdatedAt: s.now().UTC().Format(time.RFC3339), Source: source}
	s.buildModels[project.ID] = choice
	return choice
}

func (s *companionSession) buildModelList(local bool) []companionBuildModel {
	s.mu.Lock()
	defer s.mu.Unlock()
	choices := []companionBuildModel{}
	for id, project := range s.projects {
		choice := s.buildModels[id]
		choice.Available, choice.ProjectID = true, id
		if local {
			choice.ProjectPath = project.path
		}
		choices = append(choices, choice)
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].ProjectID < choices[j].ProjectID })
	return choices
}

func (s *companionSession) buildModel(w http.ResponseWriter, r *http.Request, project companionProject, local bool) {
	if r.Method == http.MethodGet {
		writeJSON(w, s.modelChoice(project, local))
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Model       string `json:"model"`
		ProjectPath string `json:"projectPath,omitempty"`
	}
	if !companionDecode(w, r, &request, 1024) {
		return
	}
	if !local && request.ProjectPath != "" {
		http.Error(w, "Use a shared project.", http.StatusBadRequest)
		return
	}
	if err := s.validateModel(r.Context(), request.Model); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	source := "phone"
	if local {
		source = "desktop"
	}
	s.setModel(project, request.Model, source)
	writeJSON(w, s.modelChoice(project, local))
}

func (m *companionManager) buildModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.Query().Get("projectPath")
	if r.Method == http.MethodPut {
		// Decode once here, then pass only the model to the shared validation path.
		var request struct {
			ProjectPath      string  `json:"projectPath"`
			Model            string  `json:"model"`
			ExpectedRevision *uint64 `json:"expectedRevision,omitempty"`
		}
		if !companionDecode(w, r, &request, 2048) {
			return
		}
		path = request.ProjectPath
		m.mu.Lock()
		session := m.session
		m.mu.Unlock()
		if session == nil || session.ctx.Err() != nil || !m.now().Before(session.expires) {
			writeJSON(w, companionBuildModel{})
			return
		}
		project, ok := session.project(companionProjectID(path))
		if !ok {
			writeJSON(w, companionBuildModel{})
			return
		}
		if err := session.validateModel(r.Context(), request.Model); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		session.setModelIfRevision(project, request.Model, "desktop", request.ExpectedRevision)
		writeJSON(w, session.modelChoice(project, true))
		return
	}
	m.mu.Lock()
	session := m.session
	m.mu.Unlock()
	if session == nil || session.ctx.Err() != nil || !m.now().Before(session.expires) {
		writeJSON(w, companionBuildModel{})
		return
	}
	project, ok := session.project(companionProjectID(path))
	if !ok {
		writeJSON(w, companionBuildModel{})
		return
	}
	writeJSON(w, session.modelChoice(project, true))
}
