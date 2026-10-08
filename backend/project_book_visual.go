package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const bookVisualPrompt = `Create a brief Project Book story and draw the single most important visible change from this completed software run. Return only JSON with exactly "story", "description", and "annotations". The story is {"title":"reader-facing headline","body":"Markdown story"}. ` + bookStoryInstructions + ` Draw a simple hand-sketched wireframe of an actual screen or interaction when supported. For a nonvisual change, draw one small feature schematic. Do not invent behavior. Use about 6 to 18 marks with generous white space. Draw one clear composition, not a detailed screenshot. Use no more than four text marks, each a short label of at most 24 characters. Do not copy paragraphs or small print from the source. Use normalized coordinates from 0 to 1 on a 1000 by 750 canvas. Use black ink (#111111). Marks may be {"kind":"stroke","points":[{"x":0.1,"y":0.1},{"x":0.2,"y":0.1}],"erase":false,"color":"#111111","width":0.002}, {"kind":"shape","shape":"rectangle","start":{"x":0.1,"y":0.1},"end":{"x":0.9,"y":0.8},"color":"#111111","width":0.002}, or {"kind":"text","text":"short label","x":0.12,"y":0.15,"width":0.3,"fontSize":0.022,"color":"#111111"}. Do not return SVG, HTML, executable content, or an image URL.`

func completedSessionModel(ctx context.Context, projectPath, sessionID string) (string, error) {
	service := newChatService(projectPath, "")
	var messages []struct {
		Info struct {
			Role       string `json:"role"`
			ProviderID string `json:"providerID"`
			ModelID    string `json:"modelID"`
			Model      struct {
				ProviderID string `json:"providerID"`
				ModelID    string `json:"modelID"`
			} `json:"model"`
		} `json:"info"`
	}
	if err := service.json(ctx, http.MethodGet, "/session/"+url.PathEscape(sessionID)+"/message", nil, &messages); err != nil {
		return "", err
	}
	for i := len(messages) - 1; i >= 0; i-- {
		info := messages[i].Info
		if info.Role != "assistant" {
			continue
		}
		provider, model := info.ProviderID, info.ModelID
		if provider == "" || model == "" {
			provider, model = info.Model.ProviderID, info.Model.ModelID
		}
		if provider != "" && model != "" {
			return provider + "/" + model, nil
		}
	}
	return "", errors.New("the completed session did not report its model")
}

func bookVisualInput(root *os.Root, entry bookEntry) string {
	type source struct {
		Request      string            `json:"request"`
		Result       string            `json:"recordedResult"`
		Kind         string            `json:"kind"`
		Status       string            `json:"status"`
		ChangedFiles []string          `json:"changedFiles,omitempty"`
		Files        map[string]string `json:"savedFiles,omitempty"`
	}
	input := source{Request: bookClip(entry.Request, 2400), Kind: entry.Kind, Status: entry.Status, ChangedFiles: entry.ChangedFiles, Files: map[string]string{}}
	if len(input.ChangedFiles) > 20 {
		input.ChangedFiles = input.ChangedFiles[:20]
	}
	if entry.Kind == "prototype" {
		base := strings.TrimSuffix(entry.Source, "/request.json")
		if data, err := bookRead(root, base+"/result.html", 2<<20); err == nil {
			input.Files["prototype/index.html"] = bookClip(string(data), 18<<10)
		}
		input.Result = "A browser prototype was saved. Its exact HTML is included in savedFiles."
	} else {
		var record agentHistoryEntryRecord
		if data, err := bookRead(root, entry.Source, 1<<20); err == nil && json.Unmarshal(data, &record) == nil {
			input.Result = bookClip(record.OutputSummary, 2800)
		}
		for _, path := range entry.ChangedFiles {
			if len(input.Files) >= 4 {
				break
			}
			path = filepath.ToSlash(path)
			if !filepath.IsLocal(path) || strings.HasPrefix(path, "project-book/") || strings.HasPrefix(path, "history/") || strings.HasPrefix(path, ".glowbom/") || strings.Contains(path, `\`) {
				continue
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".html", ".htm", ".tsx", ".jsx", ".swift", ".dart", ".kt", ".xml", ".css", ".js", ".ts":
			default:
				continue
			}
			if data, err := bookRead(root, path, 2<<20); err == nil {
				input.Files[path] = bookClip(string(data), 8000)
			}
		}
	}
	data, _ := json.Marshal(input)
	return string(data)
}

func parseBookVisual(text, model string) (*bookVisual, error) {
	text = bookModelJSON(text)
	if len(text) > 64<<10 {
		return nil, errors.New("The model returned a sketch that is too large.")
	}
	var response struct {
		Description string            `json:"description"`
		Annotations []json.RawMessage `json:"annotations"`
		Story       json.RawMessage   `json:"story"`
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&response) != nil || decoder.Decode(new(any)) != io.EOF || strings.TrimSpace(response.Description) == "" || len(response.Description) > 600 || len(response.Annotations) < 2 || len(response.Annotations) > 200 {
		return nil, errors.New("The model did not return a usable vector sketch.")
	}
	visual := &bookVisual{Description: strings.TrimSpace(response.Description), Model: model, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Document: sketchDocument{Version: 1, Width: 1000, Height: 750, Annotations: response.Annotations}}
	if err := validateSketchDocument(&visual.Document); err != nil {
		return nil, errors.New("The model returned drawing marks outside the sketch canvas.")
	}
	drawn := false
	for _, mark := range response.Annotations {
		var kind struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(mark, &kind) == nil && (kind.Kind == "stroke" || kind.Kind == "shape") {
			drawn = true
			break
		}
	}
	if !drawn {
		return nil, errors.New("The model returned labels without a drawing. Try again.")
	}
	return visual, nil
}

func bookVisualModel(ctx context.Context, service *chatService, preferred string) (chatModel, error) {
	if !bookModelEligible(preferred) {
		return chatModel{}, errors.New("Choose a supported Project Book model. Build-only agents and models cannot write its story or drawing.")
	}
	var models []chatModel
	var err error
	if strings.HasPrefix(preferred, "codex/") {
		models, err = codexLoadChatModels(ctx)
	} else {
		models, err = bookOpenCodeModels(ctx, service)
	}
	if err != nil {
		return chatModel{}, bookGenerationError(ctx, err, preferred, "finding the connected model")
	}
	for _, model := range models {
		if model.ID == preferred && bookModelEligible(model.ID) {
			return model, nil
		}
	}
	return chatModel{}, errors.New("The selected Project Book model is not connected. Reconnect it or choose another Book model.")
}

// Server setup predates cancellable Book requests. Bound the wait here so a
// cancelled reader never remains stuck behind another run's setup lock.
func prepareBookModel(ctx context.Context, prepare func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	setup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- prepare() }()
	select {
	case <-setup.Done():
		return setup.Err()
	case err := <-done:
		if setup.Err() != nil {
			return setup.Err()
		}
		return err
	}
}

func bookCompletionOptions(model chatModel) chatCompletionOptions {
	options := chatCompletionOptions{}
	if strings.HasPrefix(model.ID, "codex/") {
		// Book prose and vector marks use a short, separate turn. Prefer the
		// model's advertised low effort instead of its heavier build default.
		for _, effort := range model.ReasoningEfforts {
			if effort == "low" {
				options.ReasoningEffort = effort
				break
			}
		}
		return options
	}
	if model.LowEffort {
		options.Variant = "low"
	}
	return options
}

func bookGenerationError(ctx context.Context, err error, model, action string) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("The result is saved, but %s took too long. Try again. (%w)", action, context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("The result is saved. Book generation was stopped. (%w)", context.Canceled)
	}
	return errors.New("The result is saved, but " + action + " failed. " + chatFailureMessage(err, model))
}

func bookGenerationHTTPStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	if errors.Is(err, context.Canceled) {
		return http.StatusRequestTimeout
	}
	return http.StatusBadGateway
}

// A result sketch is added to the latest Book entry revision. The model never
// receives tools or permission to change the project.
func drawProjectBookVisual(ctx context.Context, projectPath, source, runID, preferredModel string, retryParts ...string) (projectBook, bool, error) {
	return drawProjectBookVisualWithFallback(ctx, projectPath, source, runID, preferredModel, nil, retryParts...)
}

func drawProjectBookVisualWithFallback(ctx context.Context, projectPath, source, runID, preferredModel string, fallbackModel *string, retryParts ...string) (projectBook, bool, error) {
	var book projectBook
	projectBookMu.Lock()
	root, name, err := openProjectBookProject(projectPath)
	if err != nil {
		projectBookMu.Unlock()
		return book, false, err
	}
	book, exists, err := readProjectBook(root, name)
	if err == nil && !exists {
		err = initializeProjectBook(root)
	}
	if err == nil {
		err = syncProjectBook(root, &book)
	}
	var target bookEntry
	if err == nil {
		for _, entry := range book.Entries {
			if (runID != "" && entry.RunID == runID || source != "" && entry.Source == source) && entry.Status == "completed" {
				target = entry
				break
			}
		}
	}
	input := ""
	parts := "both"
	if len(retryParts) > 0 {
		parts = retryParts[0]
	}
	storyEligible := parts != "drawing" && bookAutoStoryEligible(target)
	needsVisual := parts != "text" && target.Sketch.Visual == nil
	retryStory := len(retryParts) > 0 && storyEligible
	if target.ID != "" && (needsVisual || retryStory) {
		input = bookVisualInput(root, target)
	}
	root.Close()
	projectBookMu.Unlock()
	if err != nil || target.ID == "" || (!needsVisual && !retryStory) {
		return book, false, err
	}
	preferences := readBookWritingPreferences()
	if fallbackModel != nil {
		preferences.FallbackModel = *fallbackModel
	}
	service := newChatService(projectPath, "")
	model, err := resolveBookModel(ctx, service, target.Model, preferredModel, preferences)
	if err != nil {
		return book, false, err
	}
	story, visual, generationErr := generateBookContent(ctx, func(ctx context.Context, prompt string) (string, error) {
		text, err := service.complete(ctx, model.ID, prompt, []map[string]any{{"type": "text", "text": "Saved result evidence as JSON:\n" + input}}, nil, bookCompletionOptions(model))
		if err != nil {
			return "", bookGenerationError(ctx, err, model.ID, "completing its story and drawing")
		}
		return text, nil
	}, preferences, model.ID, storyEligible, needsVisual)
	if generationErr != nil && ((needsVisual && visual == nil) || (storyEligible && story == nil)) {
		if ctx.Err() != nil {
			generationErr = bookGenerationError(ctx, generationErr, model.ID, "completing its story and drawing")
		}
		if visual == nil && story == nil {
			return book, false, generationErr
		}
	}

	projectBookMu.Lock()
	defer projectBookMu.Unlock()
	root, name, err = openProjectBookProject(projectPath)
	if err != nil {
		return book, false, err
	}
	defer root.Close()
	book, _, err = readProjectBook(root, name)
	if err != nil {
		return book, false, err
	}
	for i := range book.Entries {
		if book.Entries[i].ID != target.ID {
			continue
		}
		entry := book.Entries[i]
		changed := false
		if needsVisual && visual != nil && entry.Sketch.Visual == nil && entry.ContentVersion == target.ContentVersion {
			visual.SourceVersion = target.ContentVersion
			entry.Sketch.Visual = visual
			changed = true
		}
		if story != nil && storyEligible && entry.ContentVersion == target.ContentVersion && bookAutoStoryEligible(entry) {
			entry.Title, entry.Body = story.Title, story.Body
			setBookStoryGeneration(&entry, model.ID, target.ContentVersion)
			entry.AutoStoryHash = ""
			changed = true
		}
		if !changed {
			return book, false, generationErr
		}
		entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := saveBookEntry(root, entry); err != nil {
			return book, false, err
		}
		entry.ContentVersion = bookEntryContentVersion(entry)
		book.Entries[i] = entry
		orderProjectBook(&book)
		setBookStoryEligibility(&book)
		return book, true, generationErr
	}
	return book, false, errors.New("The completed run is no longer in this Project Book.")
}

func projectBookVisualHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ProjectPath   string  `json:"projectPath"`
		EntryID       string  `json:"entryId"`
		Parts         string  `json:"parts"`
		Model         string  `json:"model"`
		FallbackModel *string `json:"fallbackModel"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || !projectBookID.MatchString(req.EntryID) || !validBookModelID(req.Model) || (req.FallbackModel != nil && !validBookModelID(*req.FallbackModel)) || (req.Parts != "" && req.Parts != "both" && req.Parts != "drawing" && req.Parts != "text") {
		http.Error(w, "Choose a Project Book entry to draw.", http.StatusBadRequest)
		return
	}
	projectBookMu.Lock()
	root, name, err := openProjectBookProject(req.ProjectPath)
	if err == nil {
		var book projectBook
		book, _, err = readProjectBook(root, name)
		if err == nil {
			for _, entry := range book.Entries {
				if entry.ID == req.EntryID {
					req.EntryID = entry.Source
					break
				}
			}
		}
		root.Close()
	}
	projectBookMu.Unlock()
	if err != nil || projectBookID.MatchString(req.EntryID) {
		http.Error(w, "That Project Book entry was not found.", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), bookGenerationTimeout)
	defer cancel()
	if req.Parts == "" {
		req.Parts = "both"
	}
	book, changed, err := drawProjectBookVisualWithFallback(ctx, req.ProjectPath, req.EntryID, "", req.Model, req.FallbackModel, req.Parts)
	if err != nil && changed {
		book.Warnings = append(book.Warnings, err.Error())
		writeJSON(w, book)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), bookGenerationHTTPStatus(err))
		return
	}
	writeJSON(w, book)
}
