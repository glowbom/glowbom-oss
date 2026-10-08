package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const mediaApprovalTimeout = 10 * time.Minute

type OpenCodeMediaApprovalItem struct {
	ID                string   `json:"id"`
	Placeholder       string   `json:"placeholder,omitempty"`
	MediaType         string   `json:"mediaType"`
	Prompt            string   `json:"prompt"`
	Provider          string   `json:"provider"`
	SourceID          string   `json:"sourceId"`
	Excluded          bool     `json:"excluded,omitempty"`
	ReferenceImages   []string `json:"referenceImages,omitempty"`
	ReferenceOrigin   string   `json:"referenceOrigin,omitempty"`
	AspectRatio       string   `json:"aspectRatio,omitempty"`
	FromKey           string   `json:"fromKey,omitempty"`
	AudioType         string   `json:"audioType,omitempty"`
	VoiceID           string   `json:"voiceID,omitempty"`
	ModelID           string   `json:"modelID,omitempty"`
	DurationSeconds   float64  `json:"durationSeconds,omitempty"`
	Resolution        string   `json:"resolution,omitempty"`
	Quality           string   `json:"quality,omitempty"`
	PromptInfluence   *float64 `json:"promptInfluence,omitempty"`
	Loop              bool     `json:"loop,omitempty"`
	ForceInstrumental bool     `json:"forceInstrumental,omitempty"`
	UsagePrompt       string   `json:"usagePrompt,omitempty"`
}

type OpenCodeMediaApproval struct {
	ID      string                      `json:"id"`
	Title   string                      `json:"title"`
	Message string                      `json:"message"`
	Items   []OpenCodeMediaApprovalItem `json:"items"`
}

type openCodeMediaApprovalResponse struct {
	ApprovalID                  string                      `json:"approvalID"`
	Response                    string                      `json:"response"`
	ProjectPath                 string                      `json:"projectPath,omitempty"`
	Items                       []OpenCodeMediaApprovalItem `json:"items,omitempty"`
	ImageAPIKeys                map[string]string           `json:"imageApiKeys,omitempty"`
	ImageUseSavedKey            bool                        `json:"imageUseSavedKey,omitempty"`
	VideoAPIKeys                map[string]string           `json:"videoApiKeys,omitempty"`
	VideoUseSavedKey            bool                        `json:"videoUseSavedKey,omitempty"`
	glowbomAuthorized           bool
	imageSavedKeyAuthorized     bool
	imageSubscriptionAuthorized bool
	videoSavedKeyAuthorized     bool
	videoSubscriptionAuthorized bool
}

type pendingOpenCodeMediaApproval struct {
	projectPath string
	response    chan string
	plan        *OpenCodeMediaApproval
	selection   openCodeMediaApprovalResponse
	answered    bool
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
	imageSource := strings.TrimSpace(req.ImageSource)
	if imageSource == "" {
		imageSource = openAIImageSourceLabel
	}
	reference, mimeType, err := resolveReferenceImage(req, projectPath, studioAssets)
	if err != nil {
		return nil, fmt.Errorf("reference image unavailable: %s", sanitizeProviderError(err))
	}
	var references []string
	if reference != "" {
		normalized, err := normalizeProjectIconReference("data:" + mimeType + ";base64," + reference)
		if err != nil {
			return nil, err
		}
		references = []string{"data:image/png;base64," + normalized}
	}
	for _, placeholder := range extractImagePlaceholders(string(content)) {
		if len(references) == 0 {
			if _, reusable := findStudioAssetByPrompt(studioAssets, "image", placeholder.Prompt, imageSource); reusable {
				continue
			}
		}
		itemReferences := references
		referenceOrigin := ""
		if len(itemReferences) == 0 {
			previous, err := resolvePreviousPrototypeImageReference(req.previousImageReferences, string(content), placeholder.Token, postPassImageSourceID(req))
			if err != nil {
				return nil, err
			}
			if previous != "" {
				itemReferences = []string{previous}
				referenceOrigin = "previous-image"
			}
		}
		items = append(items, OpenCodeMediaApprovalItem{
			ID: mediaApprovalItemID("image", placeholder.Token), Placeholder: placeholder.Token,
			MediaType: "image", Prompt: placeholder.Prompt, Provider: imageSource,
			SourceID: postPassImageSourceID(req), ReferenceImages: itemReferences, ReferenceOrigin: referenceOrigin,
		})
	}
	for _, placeholder := range extractVideoPlaceholders(string(content)) {
		sourceID := placeholder.sourceID
		if sourceID == "" {
			sourceID = "veo-api"
		}
		items = append(items, OpenCodeMediaApprovalItem{
			ID: mediaApprovalItemID("video", placeholder.token), Placeholder: placeholder.token,
			MediaType: "video", Prompt: placeholder.prompt, Provider: "Video", SourceID: sourceID,
			FromKey: placeholder.fromKey, AspectRatio: placeholder.aspectRatio,
			ModelID: placeholder.modelID, DurationSeconds: float64(placeholder.durationSeconds), Resolution: placeholder.resolution,
		})
	}
	for _, placeholder := range extractAudioPlaceholders(string(content)) {
		if _, reusable := findReusablePostPassAudio(studioAssets, placeholder); reusable {
			continue
		}
		items = append(items, OpenCodeMediaApprovalItem{
			ID: mediaApprovalItemID("audio", placeholder.token), Placeholder: placeholder.token,
			MediaType: "audio", Prompt: placeholder.prompt, Provider: "ElevenLabs", SourceID: "elevenlabs-api",
			AudioType: placeholder.audioType, VoiceID: placeholder.voiceID, ModelID: placeholder.modelID,
			DurationSeconds: placeholder.durationSeconds, PromptInfluence: placeholder.promptInfluence,
			Loop: placeholder.loop, ForceInstrumental: placeholder.forceInstrumental,
		})
	}

	if len(items) == 0 {
		return nil, nil
	}

	return &OpenCodeMediaApproval{
		Title:   "Generate new media assets?",
		Message: "This agent run requested new media that may use provider credits. Existing reusable assets do not require approval.",
		Items:   normalizeMediaApprovalItems(items),
	}, nil
}

func registerOpenCodeMediaApproval(projectPath string, plans ...*OpenCodeMediaApproval) (string, <-chan string) {
	id := randomUUIDString()
	pending := &pendingOpenCodeMediaApproval{
		projectPath: cleanMediaApprovalProjectPath(projectPath),
		response:    make(chan string, 1),
	}

	if len(plans) > 0 {
		pending.plan = plans[0]
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

func waitForOpenCodeMediaApproval(ctx context.Context, id string, response <-chan string) (openCodeMediaApprovalResponse, error) {
	openCodeMediaApprovalState.mu.Lock()
	pending := openCodeMediaApprovalState.pending[id]
	openCodeMediaApprovalState.mu.Unlock()
	defer removeOpenCodeMediaApproval(id)

	select {
	case value := <-response:
		if pending != nil {
			return pending.selection, nil
		}
		return openCodeMediaApprovalResponse{Response: value}, nil
	case <-ctx.Done():
		return openCodeMediaApprovalResponse{Response: "skip"}, ctx.Err()
	case <-time.After(mediaApprovalTimeout):
		return openCodeMediaApprovalResponse{Response: "skip"}, fmt.Errorf("media approval timed out")
	}
}

func openCodeMediaApprovalRespondHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req openCodeMediaApprovalResponse
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 36*1024*1024)).Decode(&req); err != nil {
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
	if !ok || pending.answered {
		openCodeMediaApprovalState.mu.Unlock()
		http.Error(w, "media approval is no longer pending", http.StatusNotFound)
		return
	}

	if requestedPath := cleanMediaApprovalProjectPath(req.ProjectPath); requestedPath != "" && requestedPath != pending.projectPath {
		openCodeMediaApprovalState.mu.Unlock()
		http.Error(w, "project path does not match pending approval", http.StatusBadRequest)
		return
	}
	if req.Response == "generate" {
		if req.Items == nil && pending.plan != nil {
			req.Items = pending.plan.Items
		}
		req.Items = normalizeMediaApprovalItems(req.Items)
		if err := preparePreviousImageApprovalReferences(req.Items); err != nil {
			openCodeMediaApprovalState.mu.Unlock()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateMediaApprovalSelection(pending, req.Items); err != nil {
			openCodeMediaApprovalState.mu.Unlock()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if postPassUsesGlowbom(OpenCodeMediaPostPassRequest{Items: req.Items}) {
			if !authorizeGlowbomImage(w, r) {
				openCodeMediaApprovalState.mu.Unlock()
				return
			}
			req.glowbomAuthorized = true
		}
		if err := validateMediaAPIKeys(req.ImageAPIKeys); err != nil {
			openCodeMediaApprovalState.mu.Unlock()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateMediaAPIKeys(req.VideoAPIKeys); err != nil {
			openCodeMediaApprovalState.mu.Unlock()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.VideoUseSavedKey {
			if !authorizeVoiceKey(w, r) {
				openCodeMediaApprovalState.mu.Unlock()
				return
			}
			req.videoSavedKeyAuthorized = true
		}
		if req.ImageUseSavedKey || mediaApprovalUsesImageSubscription(req.Items) {
			if !authorizeVoiceKey(w, r) {
				openCodeMediaApprovalState.mu.Unlock()
				return
			}
			req.imageSavedKeyAuthorized = req.ImageUseSavedKey
			req.imageSubscriptionAuthorized = mediaApprovalUsesImageSubscription(req.Items)
		}
		if mediaApprovalUsesVideoSubscription(req.Items) {
			if !authorizeVoiceKey(w, r) {
				openCodeMediaApprovalState.mu.Unlock()
				return
			}
			req.videoSubscriptionAuthorized = true
		}
	}
	pending.selection = req
	pending.answered = true
	openCodeMediaApprovalState.mu.Unlock()

	select {
	case pending.response <- req.Response:
		writeJSON(w, map[string]interface{}{"ok": true})
	default:
		http.Error(w, "media approval already answered", http.StatusConflict)
	}
}

func mediaApprovalUsesVideoSubscription(items []OpenCodeMediaApprovalItem) bool {
	for _, item := range items {
		if !item.Excluded && item.MediaType == "video" && item.SourceID == "xai-subscription" {
			return true
		}
	}
	return false
}

func mediaApprovalUsesImageSubscription(items []OpenCodeMediaApprovalItem) bool {
	for _, item := range items {
		if !item.Excluded && item.MediaType == "image" && (item.SourceID == "xai-subscription" || item.SourceID == "openai-subscription") {
			return true
		}
	}
	return false
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

func mediaApprovalItemID(mediaType, token string) string {
	hash := sha256.Sum256([]byte(mediaType + "|" + token))
	return fmt.Sprintf("%s-%x", mediaType, hash[:12])
}

func normalizeMediaApprovalItems(items []OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem {
	if items == nil {
		return nil
	}
	normalized := append([]OpenCodeMediaApprovalItem{}, items...)
	for index := range normalized {
		item := &normalized[index]
		if item.MediaType == "image" && mediaApprovalHasImageOptions(*item) {
			options, err := normalizeStudioImageOptions(studioImageOptions{SourceID: item.SourceID, ModelID: item.ModelID, AspectRatio: item.AspectRatio, Resolution: item.Resolution, Quality: item.Quality})
			if err == nil {
				item.SourceID, item.ModelID, item.AspectRatio = options.SourceID, options.ModelID, options.AspectRatio
				item.Resolution, item.Quality = options.Resolution, options.Quality
			}
		}
		if item.MediaType == "audio" && item.AudioType == "music" && item.DurationSeconds == 0 {
			item.DurationSeconds = defaultElevenMusicDuration
		}
		if item.MediaType == "video" && item.SourceID != "" {
			options, err := normalizeStudioVideoOptions(studioVideoOptions{SourceID: item.SourceID, ModelID: item.ModelID, DurationSeconds: int(item.DurationSeconds), Resolution: item.Resolution, AspectRatio: item.AspectRatio})
			if err == nil && (item.DurationSeconds == 0 || item.DurationSeconds == float64(int(item.DurationSeconds))) {
				item.SourceID, item.ModelID = options.SourceID, options.ModelID
				item.DurationSeconds, item.Resolution, item.AspectRatio = float64(options.DurationSeconds), options.Resolution, options.AspectRatio
			}
		}
	}
	return normalized
}

func validateMediaApprovalSelection(pending *pendingOpenCodeMediaApproval, items []OpenCodeMediaApprovalItem) error {
	if len(items) > 100 {
		return fmt.Errorf("Use at most 100 assets per run.")
	}
	originals := make(map[string]OpenCodeMediaApprovalItem)
	if pending.plan != nil {
		for _, item := range pending.plan.Items {
			originals[item.ID] = item
		}
	}
	var content string
	if pending.plan != nil {
		path, err := safeProjectPath(pending.projectPath, "prototype", "index.html")
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("Could not recheck the pending prototype.")
		}
		content = string(data)
	}
	seen := make(map[string]bool)
	totalReferenceBytes := 0
	for _, item := range items {
		if item.ID == "" || len(item.ID) > 100 || seen[item.ID] {
			return fmt.Errorf("Each asset needs a unique valid ID.")
		}
		seen[item.ID] = true
		original, existing := originals[item.ID]
		if existing {
			if item.MediaType != original.MediaType || item.Placeholder != original.Placeholder {
				return fmt.Errorf("The original asset type and placeholder cannot change.")
			}
			if !strings.Contains(content, original.Placeholder) {
				return fmt.Errorf("An asset placeholder changed. Run the agent again to review the current assets.")
			}
		} else if !strings.HasPrefix(item.ID, "added-") || item.Placeholder != "" || (!item.Excluded && strings.TrimSpace(item.UsagePrompt) == "") {
			return fmt.Errorf("New assets need an added- ID and instructions for where to use them.")
		}
		if err := validateMediaApprovalItem(item); err != nil {
			return err
		}
		for _, reference := range item.ReferenceImages {
			totalReferenceBytes += len(reference)
		}
		if totalReferenceBytes > 32*1024*1024 {
			return fmt.Errorf("Use fewer or smaller reference photos.")
		}
	}
	return validateMediaApprovalVideoReferences(originals, items)
}

func validateMediaApprovalItem(item OpenCodeMediaApprovalItem) error {
	if item.Excluded {
		if item.MediaType != "image" && item.MediaType != "video" && item.MediaType != "audio" {
			return fmt.Errorf("Choose an image, video, or audio asset.")
		}
		return nil
	}
	if item.ReferenceOrigin != "" && item.ReferenceOrigin != "previous-image" {
		return fmt.Errorf("Choose a supported image reference origin.")
	}
	if strings.TrimSpace(item.Prompt) == "" || len(item.Prompt) > 10000 || len(item.UsagePrompt) > 10000 {
		return fmt.Errorf("Use a nonempty asset prompt up to 10000 characters.")
	}
	if len(item.ReferenceImages) > 1 {
		return fmt.Errorf("Use one reference photo per image asset.")
	}
	if item.MediaType != "image" && len(item.ReferenceImages) > 0 {
		return fmt.Errorf("Reference photos apply only to image assets.")
	}
	if math.IsNaN(item.DurationSeconds) || math.IsInf(item.DurationSeconds, 0) || item.DurationSeconds < 0 {
		return fmt.Errorf("Use a valid duration.")
	}
	if item.MediaType != "audio" && (item.AudioType != "" || item.VoiceID != "" || item.PromptInfluence != nil || item.Loop || item.ForceInstrumental) {
		return fmt.Errorf("Audio parameters apply only to audio assets.")
	}
	if item.MediaType != "audio" && item.MediaType != "video" && item.DurationSeconds != 0 {
		return fmt.Errorf("Duration applies only to audio or video assets.")
	}
	if item.MediaType != "video" && item.MediaType != "image" && item.Resolution != "" {
		return fmt.Errorf("Resolution applies only to image or video assets.")
	}
	if item.MediaType != "image" && item.Quality != "" {
		return fmt.Errorf("Quality applies only to image assets.")
	}
	if item.MediaType != "video" && item.FromKey != "" {
		return fmt.Errorf("Start images apply only to video assets.")
	}
	switch item.MediaType {
	case "image":
		if item.SourceID == "glowbom-api" && item.AspectRatio != "" && !mediaApprovalHasImageOptions(item) {
			return fmt.Errorf("Glowbom chooses the image size. Clear the aspect ratio for this source.")
		}
		switch item.SourceID {
		case "openai-subscription", "glowbom-api", "openai-api", "gemini-api", "xai-api", "xai-subscription":
		default:
			return fmt.Errorf("Choose a supported image source.")
		}
		if mediaApprovalHasImageOptions(item) {
			if _, err := normalizeStudioImageOptions(studioImageOptions{SourceID: item.SourceID, ModelID: item.ModelID, AspectRatio: item.AspectRatio, Resolution: item.Resolution, Quality: item.Quality}); err != nil {
				return err
			}
		} else if item.AspectRatio != "" && item.AspectRatio != "1:1" && item.AspectRatio != "16:9" && item.AspectRatio != "9:16" && item.AspectRatio != "4:3" && item.AspectRatio != "3:4" {
			return fmt.Errorf("Choose a supported image aspect ratio.")
		}
		for _, reference := range item.ReferenceImages {
			if !strings.HasPrefix(reference, "data:image/") {
				return fmt.Errorf("Upload a PNG or JPEG reference photo.")
			}
			normalized, err := normalizeProjectIconReference(reference)
			if err != nil {
				return err
			}
			if item.SourceID == "glowbom-api" {
				if err := validateGlowbomImageReference(normalized); err != nil {
					return err
				}
			}
		}
	case "video":
		if item.SourceID != "veo-api" && item.SourceID != "xai-api" && item.SourceID != "xai-subscription" {
			return fmt.Errorf("Choose a supported video source.")
		}
		if item.DurationSeconds != float64(int(item.DurationSeconds)) {
			return fmt.Errorf("Choose a whole number of seconds for video.")
		}
		if _, err := normalizeStudioVideoOptions(studioVideoOptions{SourceID: item.SourceID, ModelID: item.ModelID, DurationSeconds: int(item.DurationSeconds), Resolution: item.Resolution, AspectRatio: item.AspectRatio}); err != nil {
			return err
		}
		if strings.TrimSpace(item.FromKey) == "" || len(item.FromKey) > 10000 {
			return fmt.Errorf("Choose a start image for the video.")
		}
	case "audio":
		if item.SourceID != "elevenlabs-api" {
			return fmt.Errorf("Choose ElevenLabs for audio assets.")
		}
		if item.AspectRatio != "" {
			return fmt.Errorf("Aspect ratios do not apply to audio assets.")
		}
		if len(item.VoiceID) > 200 || len(item.ModelID) > 200 {
			return fmt.Errorf("Use a valid voice or model ID.")
		}
		switch item.AudioType {
		case "voice":
			if item.DurationSeconds != 0 || item.PromptInfluence != nil || item.Loop || item.ForceInstrumental {
				return fmt.Errorf("Duration, influence, looping and instrumental options do not apply to voice.")
			}
		case "sound":
			if item.DurationSeconds != 0 && (item.DurationSeconds < minElevenSoundDuration || item.DurationSeconds > maxElevenSoundDuration) {
				return fmt.Errorf("Sound effects support durations from 0.5 to 30 seconds.")
			}
			if item.VoiceID != "" || item.ForceInstrumental {
				return fmt.Errorf("Voice and instrumental options do not apply to sound effects.")
			}
		case "music":
			if item.DurationSeconds < minElevenMusicDuration || item.DurationSeconds > maxElevenMusicDuration {
				return fmt.Errorf("Music supports durations from 3 to 600 seconds.")
			}
			if item.VoiceID != "" || item.PromptInfluence != nil || item.Loop {
				return fmt.Errorf("Voice, influence and looping options do not apply to music.")
			}
		default:
			return fmt.Errorf("Choose voice, sound, or music for audio assets.")
		}
		if item.PromptInfluence != nil && (math.IsNaN(*item.PromptInfluence) || math.IsInf(*item.PromptInfluence, 0) || *item.PromptInfluence < 0 || *item.PromptInfluence > 1) {
			return fmt.Errorf("Use prompt influence from 0 to 1.")
		}
		if _, err := postPassElevenLabsAudioRequest(postPassAudioPlaceholder{prompt: item.Prompt, audioType: item.AudioType, voiceID: item.VoiceID, modelID: item.ModelID, durationSeconds: item.DurationSeconds, promptInfluence: item.PromptInfluence, loop: item.Loop, forceInstrumental: item.ForceInstrumental}, "", ""); err != nil {
			return err
		}
	default:
		return fmt.Errorf("Choose an image, video, or audio asset.")
	}
	return nil
}

func mediaApprovalHasImageOptions(item OpenCodeMediaApprovalItem) bool {
	return item.ModelID != "" || item.Resolution != "" || item.Quality != ""
}

func validateMediaAPIKeys(keys map[string]string) error {
	for source, key := range keys {
		if source != "openai-api" && source != "gemini-api" && source != "xai-api" {
			return fmt.Errorf("Unsupported image API key source.")
		}
		if len(key) > 16384 {
			return fmt.Errorf("Use a valid image API key.")
		}
	}
	return nil
}

func validateMediaApprovalVideoReferences(originals map[string]OpenCodeMediaApprovalItem, items []OpenCodeMediaApprovalItem) error {
	available := map[string]bool{}
	selected := map[string]OpenCodeMediaApprovalItem{}
	blocked := map[string]bool{}
	for _, item := range items {
		selected[item.ID] = item
		if item.MediaType != "image" || item.Excluded {
			continue
		}
		for _, key := range []string{item.ID, item.Prompt, item.Placeholder, resourceSafeName(item.Prompt)} {
			available[normalizedLookupKey(key)] = true
		}
		if original, ok := originals[item.ID]; ok {
			available[normalizedLookupKey(original.Prompt)] = true
			available[normalizedLookupKey(resourceSafeName(original.Prompt))] = true
		}
	}
	for _, original := range originals {
		if original.MediaType != "image" {
			continue
		}
		selection, ok := selected[original.ID]
		if ok && !selection.Excluded {
			continue
		}
		for _, key := range []string{original.ID, original.Prompt, original.Placeholder, resourceSafeName(original.Prompt)} {
			blocked[normalizedLookupKey(key)] = true
		}
	}
	var studioAssets []studioAssetRecord
	for _, item := range items {
		if item.MediaType != "video" || item.Excluded {
			continue
		}
		key := normalizedLookupKey(item.FromKey)
		if available[key] {
			continue
		}
		if blocked[key] {
			return fmt.Errorf("The video's start image is excluded. Include it or choose another image.")
		}
		if studioAssets == nil {
			studioAssets, _ = loadStudioAssets()
		}
		asset, ok := findStudioAssetByKey(studioAssets, "image", item.FromKey, "")
		if !ok {
			return fmt.Errorf("The video's start image must match an included image prompt, asset ID, or Studio image.")
		}
		data, _, err := decodeBase64Payload(asset.DataBase64, "image/png")
		if err != nil {
			return fmt.Errorf("The video's Studio start image could not be opened.")
		}
		if _, err := normalizeProjectIcon(data); err != nil {
			return fmt.Errorf("The video's Studio start image could not be opened.")
		}
	}
	return nil
}
