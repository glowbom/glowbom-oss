package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const mediaApprovalTimeout = 10 * time.Minute

type OpenCodeMediaApprovalItem struct {
	MediaType string `json:"mediaType"`
	Prompt    string `json:"prompt"`
	Provider  string `json:"provider"`
	AudioType string `json:"audioType,omitempty"`
}

type OpenCodeMediaApproval struct {
	ID      string                      `json:"id"`
	Title   string                      `json:"title"`
	Message string                      `json:"message"`
	Items   []OpenCodeMediaApprovalItem `json:"items"`
}

type openCodeMediaApprovalResponse struct {
	ApprovalID  string `json:"approvalID"`
	Response    string `json:"response"`
	ProjectPath string `json:"projectPath,omitempty"`
}

type pendingOpenCodeMediaApproval struct {
	projectPath string
	response    chan string
}

var openCodeMediaApprovalState = struct {
	mu      sync.Mutex
	pending map[string]*pendingOpenCodeMediaApproval
}{
	pending: make(map[string]*pendingOpenCodeMediaApproval),
}

func normalizeMediaGenerationPolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "ask":
		return "ask"
	case "skip":
		return "skip"
	case "", "auto":
		return "auto"
	default:
		return "ask"
	}
}

func buildOpenCodeMediaApproval(req OpenCodeMediaPostPassRequest) (*OpenCodeMediaApproval, error) {
	projectPath := strings.TrimSpace(req.ProjectPath)
	if projectPath == "" {
		return nil, fmt.Errorf("projectPath is required")
	}

	indexPath, err := safeProjectPath(projectPath, "prototype", "index.html")
	if err != nil {
		return nil, err
	}
	content, err := osReadFile(indexPath)
	if err != nil {
		return nil, err
	}

	studioAssets, studioErr := loadStudioAssets()
	if studioErr != nil {
		studioAssets = []studioAssetRecord{}
	}

	items := []OpenCodeMediaApprovalItem{}
	seen := make(map[string]struct{})
	hasReferenceImage := strings.TrimSpace(req.ReferenceImagePath) != "" || strings.TrimSpace(req.ReferenceAssetID) != ""
	imageSource := strings.TrimSpace(req.ImageSource)
	if imageSource == "" {
		imageSource = openAIImageSourceLabel
	}

	for _, placeholder := range extractImagePlaceholders(string(content)) {
		prompt := strings.TrimSpace(placeholder.Prompt)
		if prompt == "" {
			continue
		}
		if !hasReferenceImage {
			if _, reusable := findStudioAssetByPrompt(studioAssets, "image", prompt, imageSource); reusable {
				continue
			}
		}

		key := "image|" + normalizedLookupKey(prompt)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, OpenCodeMediaApprovalItem{
			MediaType: "image",
			Prompt:    prompt,
			Provider:  imageSource,
		})
	}

	for _, placeholder := range extractVideoPlaceholders(string(content)) {
		key := normalizedLookupKey(fmt.Sprintf("video|%s|%s|%s", placeholder.prompt, placeholder.fromKey, placeholder.aspectRatio))
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, OpenCodeMediaApprovalItem{
			MediaType: "video",
			Prompt:    placeholder.prompt,
			Provider:  "Veo",
		})
	}

	for _, placeholder := range extractAudioPlaceholders(string(content)) {
		if _, reusable := findStudioAssetByPrompt(studioAssets, "audio", placeholder.prompt, "ElevenLabs"); reusable {
			continue
		}

		key := normalizedLookupKey(fmt.Sprintf(
			"audio|%s|%s|%s|%s|%s|%.3f|%t|%t",
			placeholder.prompt,
			placeholder.audioType,
			placeholder.voiceID,
			placeholder.modelID,
			floatPointerKey(placeholder.promptInfluence),
			placeholder.durationSeconds,
			placeholder.loop,
			placeholder.forceInstrumental,
		))
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, OpenCodeMediaApprovalItem{
			MediaType: "audio",
			AudioType: placeholder.audioType,
			Prompt:    placeholder.prompt,
			Provider:  "ElevenLabs",
		})
	}

	if len(items) == 0 {
		return nil, nil
	}

	return &OpenCodeMediaApproval{
		Title:   "Generate new media assets?",
		Message: "This agent run requested new media that may use provider credits. Existing reusable assets do not require approval.",
		Items:   items,
	}, nil
}

func registerOpenCodeMediaApproval(projectPath string) (string, <-chan string) {
	id := randomUUIDString()
	pending := &pendingOpenCodeMediaApproval{
		projectPath: cleanMediaApprovalProjectPath(projectPath),
		response:    make(chan string, 1),
	}

	openCodeMediaApprovalState.mu.Lock()
	openCodeMediaApprovalState.pending[id] = pending
	openCodeMediaApprovalState.mu.Unlock()

	return id, pending.response
}

func removeOpenCodeMediaApproval(id string) {
	openCodeMediaApprovalState.mu.Lock()
	delete(openCodeMediaApprovalState.pending, id)
	openCodeMediaApprovalState.mu.Unlock()
}

func waitForOpenCodeMediaApproval(ctx context.Context, id string, response <-chan string) (string, error) {
	defer removeOpenCodeMediaApproval(id)

	select {
	case value := <-response:
		return value, nil
	case <-ctx.Done():
		return "skip", ctx.Err()
	case <-time.After(mediaApprovalTimeout):
		return "skip", fmt.Errorf("media approval timed out")
	}
}

func openCodeMediaApprovalRespondHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req openCodeMediaApprovalResponse
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	req.ApprovalID = strings.TrimSpace(req.ApprovalID)
	req.Response = strings.ToLower(strings.TrimSpace(req.Response))
	if req.ApprovalID == "" {
		http.Error(w, "approvalID is required", http.StatusBadRequest)
		return
	}
	if req.Response != "generate" && req.Response != "skip" {
		http.Error(w, "response must be generate or skip", http.StatusBadRequest)
		return
	}

	openCodeMediaApprovalState.mu.Lock()
	pending, ok := openCodeMediaApprovalState.pending[req.ApprovalID]
	if !ok {
		openCodeMediaApprovalState.mu.Unlock()
		http.Error(w, "media approval is no longer pending", http.StatusNotFound)
		return
	}

	if requestedPath := cleanMediaApprovalProjectPath(req.ProjectPath); requestedPath != "" && requestedPath != pending.projectPath {
		openCodeMediaApprovalState.mu.Unlock()
		http.Error(w, "project path does not match pending approval", http.StatusBadRequest)
		return
	}
	delete(openCodeMediaApprovalState.pending, req.ApprovalID)
	openCodeMediaApprovalState.mu.Unlock()

	select {
	case pending.response <- req.Response:
		writeJSON(w, map[string]interface{}{"ok": true})
	default:
		http.Error(w, "media approval already answered", http.StatusConflict)
	}
}

func cleanMediaApprovalProjectPath(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return filepath.Clean(trimmed)
	}
	return filepath.Clean(abs)
}

// osReadFile is a package variable so approval planning can be tested without
// reaching external media providers. Production uses the standard filesystem.
var osReadFile = func(path string) ([]byte, error) {
	return os.ReadFile(path)
}
