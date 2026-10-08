package main

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

func validBookModelID(id string) bool {
	if id == "" {
		return true
	}
	provider, model, ok := strings.Cut(id, "/")
	return ok && provider != "" && model != "" && len(id) <= 200 && !strings.ContainsAny(id, " \t\r\n\x00")
}

func bookModelEligible(id string) bool {
	if !validBookModelID(id) || id == "" || isBuildOnlyCLIModel(id) {
		return false
	}
	provider, model, _ := strings.Cut(id, "/")
	if provider == appleIntelligenceProvider || model == localMiMoModel {
		return false
	}
	return provider != "opencode" || (model != "big-pickle" && !strings.HasSuffix(model, "-free"))
}

// Book uses text completions without tools. Build and image-input support are
// independent capabilities, so neither is required for these vector drawings.
func bookOpenCodeModels(ctx context.Context, service *chatService) ([]chatModel, error) {
	if err := prepareBookModel(ctx, service.prepare); err != nil {
		return nil, err
	}
	var catalog struct {
		Connected []string `json:"connected"`
		All       []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Models map[string]struct {
				Name     string `json:"name"`
				Status   string `json:"status"`
				Variants map[string]struct {
					Disabled bool `json:"disabled"`
				} `json:"variants"`
				Modalities struct {
					Output []string `json:"output"`
				} `json:"modalities"`
			} `json:"models"`
		} `json:"all"`
	}
	if err := service.json(ctx, http.MethodGet, "/provider", nil, &catalog); err != nil {
		return nil, err
	}
	connected := map[string]bool{}
	for _, id := range catalog.Connected {
		connected[id] = true
	}
	models := []chatModel{}
	for _, provider := range catalog.All {
		if !connected[provider.ID] {
			continue
		}
		for id, model := range provider.Models {
			fullID := provider.ID + "/" + id
			if !bookModelEligible(fullID) || model.Status == "deprecated" || (len(model.Modalities.Output) > 0 && !containsTextModality(model.Modalities.Output)) {
				continue
			}
			name := model.Name
			if name == "" {
				name = id
			}
			low, hasLow := model.Variants["low"]
			models = append(models, chatModel{ID: fullID, Name: name, Provider: provider.Name, LowEffort: hasLow && !low.Disabled})
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func bookConnectedModels(ctx context.Context, service *chatService) ([]chatModel, error) {
	type result struct {
		models []chatModel
		err    error
	}
	results := make(chan result, 2)
	go func() { models, err := bookOpenCodeModels(ctx, service); results <- result{models, err} }()
	go func() { models, err := codexLoadChatModels(ctx); results <- result{models, err} }()
	models := []chatModel{}
	var firstErr error
collect:
	for i := 0; i < 2; i++ {
		select {
		case value := <-results:
			if firstErr == nil {
				firstErr = value.err
			}
			for _, model := range value.models {
				if bookModelEligible(model.ID) {
					models = append(models, model)
				}
			}
		case <-ctx.Done():
			// A slow runtime must not hide models already returned by another.
			// Explicit cancellation still stops the request.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && len(models) > 0 {
				break collect
			}
			return nil, ctx.Err()
		}
	}
	if len(models) == 0 && firstErr != nil {
		return nil, firstErr
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func resolveBookModel(ctx context.Context, service *chatService, buildModel, override string, preferences bookWritingPreferences) (chatModel, error) {
	selected := override
	if selected == "" {
		selected = preferences.Model
	}
	if selected != "" {
		return bookVisualModel(ctx, service, selected)
	}
	for _, candidate := range []string{buildModel, preferences.FallbackModel} {
		if !bookModelEligible(candidate) {
			continue
		}
		model, err := bookVisualModel(ctx, service, candidate)
		if err == nil {
			return model, nil
		}
		if ctx.Err() != nil {
			return chatModel{}, bookGenerationError(ctx, ctx.Err(), candidate, "connecting to the Book model")
		}
	}
	return chatModel{}, errors.New("Choose a connected Project Book model in Settings or for this entry. Automatic needs a supported build or Chat model.")
}

func projectBookModelsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.Query().Get("path")
	if path != "" {
		var err error
		path, err = chatProjectRoot(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	models, err := bookConnectedModels(ctx, newChatService(path, ""))
	if err != nil {
		http.Error(w, chatFailureMessage(err, ""), bookGenerationHTTPStatus(err))
		return
	}
	writeJSON(w, map[string]any{"models": models})
}
