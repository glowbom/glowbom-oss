package main

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxGeneratedImageBytes = 25 * 1024 * 1024  // 25 MB
	maxGeneratedVideoBytes = 120 * 1024 * 1024 // 120 MB
	maxGeneratedAudioBytes = 40 * 1024 * 1024  // 40 MB
)

var (
	glowbyImagePlaceholderRegex = regexp.MustCompile(`(?:glowby|glowbom)images?:([^"'<>]+)`)
	glowbyVideoPlaceholderRegex = regexp.MustCompile(`glowbyvideo:([^"'<>]+)`)
	glowbyAudioPlaceholderRegex = regexp.MustCompile(`glowbyaudio:([^"'<>]+)`)
	sensitiveValueRegex         = regexp.MustCompile(`(?i)(api[_-]?key|authorization|bearer)\s*[:=]\s*[^,\s]+`)
)

type OpenCodeMediaPostPassRequest struct {
	ProjectPath          string   `json:"projectPath"`
	ImageSource          string   `json:"imageSource,omitempty"`
	OpenAIKey            string   `json:"openaiKey,omitempty"`
	GeminiKey            string   `json:"geminiKey,omitempty"`
	XaiKey               string   `json:"xaiKey,omitempty"`
	VeoGeminiKey         string   `json:"veoGeminiKey,omitempty"`
	ElevenLabsKey        string   `json:"elevenLabsKey,omitempty"`
	ElevenLabsVoiceID    string   `json:"elevenLabsVoiceID,omitempty"`
	ElevenLabsVoiceModel string   `json:"elevenLabsVoiceModel,omitempty"`
	ReferenceImagePath   string   `json:"referenceImagePath,omitempty"`
	ReferenceAssetID     string   `json:"referenceAssetID,omitempty"`
	ScanTargets          []string `json:"scanTargets,omitempty"`
}

type OpenCodeMediaPostPassResponse struct {
	PrototypeChanged     bool                   `json:"prototypeChanged"`
	GeneratedAssets      []OpenCodeMediaAsset   `json:"generatedAssets"`
	ReusedStudioAssets   []OpenCodeMediaAsset   `json:"reusedStudioAssets"`
	PlatformCopies       []OpenCodePlatformCopy `json:"platformCopies"`
	Warnings             []string               `json:"warnings"`
	PlatformAssetsSynced bool                   `json:"platformAssetsSynced"`
}

type OpenCodeMediaAsset struct {
	Prompt        string `json:"prompt"`
	Placeholder   string `json:"placeholder,omitempty"`
	MediaType     string `json:"mediaType"`
	Filename      string `json:"filename"`
	RelativePath  string `json:"relativePath"`
	SourceService string `json:"sourceService,omitempty"`
	StudioAssetID string `json:"studioAssetId,omitempty"`
}

type OpenCodePlatformCopy struct {
	Platform    string `json:"platform"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

type postPassImageRef struct {
	asset    OpenCodeMediaAsset
	bytes    []byte
	mimeType string
}

type postPassVideoPlaceholder struct {
	token       string
	prompt      string
	fromKey     string
	aspectRatio string
}

type postPassAudioPlaceholder struct {
	token             string
	prompt            string
	audioType         string
	voiceID           string
	modelID           string
	durationSeconds   float64
	promptInfluence   *float64
	loop              bool
	forceInstrumental bool
}

type studioAssetRecord struct {
	ID            string `json:"id"`
	MediaType     string `json:"mediaType"`
	DataBase64    string `json:"dataBase64"`
	Prompt        string `json:"prompt"`
	SourceService string `json:"sourceService"`
}

type prototypeAssetsManifest struct {
	Version    string                        `json:"version"`
	ExportedAt string                        `json:"exportedAt"`
	Assets     []prototypeAssetsManifestItem `json:"assets"`
}

type prototypeAssetsManifestItem struct {
	Filename      string         `json:"filename"`
	Prompt        string         `json:"prompt"`
	Dimensions    map[string]int `json:"dimensions,omitempty"`
	SourceService string         `json:"sourceService,omitempty"`
	MediaType     string         `json:"mediaType,omitempty"`
}

type platformAssetsMap struct {
	GeneratedAt string                 `json:"generatedAt"`
	Assets      []platformAssetsMapRow `json:"assets"`
}

type platformAssetsMapRow struct {
	Source       string                       `json:"source"`
	MediaType    string                       `json:"mediaType"`
	Destinations []platformAssetsMapTargetRow `json:"destinations"`
}

type platformAssetsMapTargetRow struct {
	Platform string `json:"platform"`
	Path     string `json:"path"`
}

func openCodeMediaPostPassHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenCodeMediaPostPassRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.ProjectPath) == "" {
		http.Error(w, "projectPath is required", http.StatusBadRequest)
		return
	}

	log.Printf("[OPENCODE][MEDIA] post-pass requested: project=%s, scanTargets=%d", req.ProjectPath, len(req.ScanTargets))

	resp, err := runOpenCodeMediaPostPass(r.Context(), req)
	if err != nil {
		http.Error(w, sanitizeProviderError(err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, resp)
}

func runOpenCodeMediaPostPass(ctx context.Context, req OpenCodeMediaPostPassRequest) (*OpenCodeMediaPostPassResponse, error) {
	resp := &OpenCodeMediaPostPassResponse{
		GeneratedAssets:    []OpenCodeMediaAsset{},
		ReusedStudioAssets: []OpenCodeMediaAsset{},
		PlatformCopies:     []OpenCodePlatformCopy{},
		Warnings:           []string{},
	}

	projectPath := strings.TrimSpace(req.ProjectPath)
	if projectPath == "" {
		return nil, fmt.Errorf("projectPath is required")
	}

	projectAbs, err := filepath.Abs(projectPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve project path: %w", err)
	}
	projectPath = projectAbs

	if req.ImageSource == "" {
		req.ImageSource = openAIImageSourceLabel
	}
	if req.VeoGeminiKey == "" {
		req.VeoGeminiKey = req.GeminiKey
	}
	if strings.TrimSpace(req.ElevenLabsVoiceModel) == "" {
		req.ElevenLabsVoiceModel = defaultElevenVoiceModel
	}
	if len(req.ScanTargets) == 0 {
		req.ScanTargets = []string{"prototype/index.html"}
	}

	if _, err := os.Stat(projectPath); err != nil {
		return nil, fmt.Errorf("project path not found: %w", err)
	}

	studioAssets, studioErr := loadStudioAssets()
	if studioErr != nil {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("Studio assets unavailable: %s", sanitizeProviderError(studioErr)))
	}

	referenceImageBase64, _, refErr := resolveReferenceImage(req, projectPath, studioAssets)
	if refErr != nil {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("Reference image unavailable: %s", sanitizeProviderError(refErr)))
	}

	imageRefsByPrompt := make(map[string]postPassImageRef)
	imageRefsByKey := make(map[string]postPassImageRef)
	videoCache := make(map[string]OpenCodeMediaAsset)
	audioCache := make(map[string]OpenCodeMediaAsset)

	if strings.TrimSpace(req.ElevenLabsKey) != "" && strings.TrimSpace(req.ElevenLabsVoiceID) == "" {
		voices, voiceErr := fetchElevenLabsVoices(strings.TrimSpace(req.ElevenLabsKey))
		if voiceErr != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("Could not preload ElevenLabs default voice: %s", sanitizeProviderError(voiceErr)))
		} else if len(voices) > 0 {
			req.ElevenLabsVoiceID = strings.TrimSpace(voices[0].VoiceID)
			log.Printf("[OPENCODE][MEDIA][AUDIO] using account default voice_id=%s for post-pass", req.ElevenLabsVoiceID)
		}
	}

	for _, rawTarget := range req.ScanTargets {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		targetPath, err := resolveScanTargetPath(projectPath, rawTarget)
		if err != nil {
			resp.Warnings = append(resp.Warnings, err.Error())
			continue
		}

		data, err := os.ReadFile(targetPath)
		if err != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("Failed to read %s: %s", rawTarget, sanitizeProviderError(err)))
			continue
		}

		original := string(data)
		updated := original

		imagePlaceholders := extractImagePlaceholders(updated)
		for _, placeholder := range imagePlaceholders {
			prompt := strings.TrimSpace(placeholder.Prompt)
			if prompt == "" {
				continue
			}

			ref, ok := imageRefsByPrompt[normalizedLookupKey(prompt)]
			if !ok {
				ref, err = materializeImagePlaceholder(req, projectPath, prompt, placeholder.Token, referenceImageBase64, studioAssets, resp)
				if err != nil {
					resp.Warnings = append(resp.Warnings, err.Error())
					continue
				}

				imageRefsByPrompt[normalizedLookupKey(prompt)] = ref
				registerImageRefKeys(imageRefsByKey, ref)
			}

			updated = strings.ReplaceAll(updated, placeholder.Token, "assets/"+ref.asset.Filename)
		}

		videoPlaceholders := extractVideoPlaceholders(updated)
		for _, placeholder := range videoPlaceholders {
			cacheKey := normalizedLookupKey(fmt.Sprintf("%s|%s|%s", placeholder.prompt, placeholder.fromKey, placeholder.aspectRatio))
			if existing, ok := videoCache[cacheKey]; ok {
				updated = strings.ReplaceAll(updated, placeholder.token, "assets/"+existing.Filename)
				continue
			}

			videoAsset, err := materializeVideoPlaceholder(ctx, req, projectPath, placeholder, imageRefsByKey, studioAssets, resp)
			if err != nil {
				resp.Warnings = append(resp.Warnings, err.Error())
				continue
			}

			videoCache[cacheKey] = videoAsset
			updated = strings.ReplaceAll(updated, placeholder.token, "assets/"+videoAsset.Filename)
		}

		audioPlaceholders := extractAudioPlaceholders(updated)
		for _, placeholder := range audioPlaceholders {
			cacheKey := normalizedLookupKey(fmt.Sprintf(
				"%s|%s|%s|%s|%s|%.3f|%t|%t",
				placeholder.prompt,
				placeholder.audioType,
				placeholder.voiceID,
				placeholder.modelID,
				floatPointerKey(placeholder.promptInfluence),
				placeholder.durationSeconds,
				placeholder.loop,
				placeholder.forceInstrumental,
			))
			if existing, ok := audioCache[cacheKey]; ok {
				updated = strings.ReplaceAll(updated, placeholder.token, "assets/"+existing.Filename)
				continue
			}

			audioAsset, err := materializeAudioPlaceholder(req, projectPath, placeholder, studioAssets, resp)
			if err != nil {
				resp.Warnings = append(resp.Warnings, err.Error())
				continue
			}

			audioCache[cacheKey] = audioAsset
			updated = strings.ReplaceAll(updated, placeholder.token, "assets/"+audioAsset.Filename)
		}

		if updated != original {
			if err := os.WriteFile(targetPath, []byte(updated), 0644); err != nil {
				resp.Warnings = append(resp.Warnings, fmt.Sprintf("Failed to update %s: %s", rawTarget, sanitizeProviderError(err)))
			} else {
				resp.PrototypeChanged = true
			}
		}
	}

	allAssets := make([]OpenCodeMediaAsset, 0, len(resp.GeneratedAssets)+len(resp.ReusedStudioAssets))
	allAssets = append(allAssets, resp.GeneratedAssets...)
	allAssets = append(allAssets, resp.ReusedStudioAssets...)

	if len(allAssets) > 0 {
		if err := writePrototypeAssetsManifest(projectPath, allAssets, req.ImageSource); err != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("Failed to update assets manifest: %s", sanitizeProviderError(err)))
		}
	}

	copies, synced, syncWarnings, err := syncAssetsToPlatforms(projectPath)
	if err != nil {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("Platform sync failed: %s", sanitizeProviderError(err)))
	} else {
		resp.PlatformCopies = copies
		resp.PlatformAssetsSynced = synced
		resp.Warnings = append(resp.Warnings, syncWarnings...)
		if synced {
			resp.PrototypeChanged = true
		}
	}

	if err := writePlatformAssetsMap(projectPath, resp.PlatformCopies); err != nil {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("Failed to write platform assets map: %s", sanitizeProviderError(err)))
	}

	resp.Warnings = dedupeWarnings(resp.Warnings)
	return resp, nil
}

func materializeImagePlaceholder(
	req OpenCodeMediaPostPassRequest,
	projectPath string,
	prompt string,
	placeholderToken string,
	referenceImageBase64 string,
	studioAssets []studioAssetRecord,
	resp *OpenCodeMediaPostPassResponse,
) (postPassImageRef, error) {
	if strings.TrimSpace(referenceImageBase64) == "" {
		if studioAsset, ok := findStudioAssetByPrompt(studioAssets, "image", prompt, req.ImageSource); ok {
			bytes, mimeType, err := decodeBase64Payload(studioAsset.DataBase64, "image/png")
			if err == nil {
				ext := imageExtensionForMimeType(mimeType)
				filename := deterministicAssetFilename("img", prompt, ext)
				relativePath, err := writePrototypeAsset(projectPath, filename, bytes, map[string]struct{}{
					".png":  {},
					".jpg":  {},
					".jpeg": {},
					".webp": {},
				}, maxGeneratedImageBytes)
				if err == nil {
					asset := OpenCodeMediaAsset{
						Prompt:        prompt,
						Placeholder:   placeholderToken,
						MediaType:     "image",
						Filename:      filename,
						RelativePath:  relativePath,
						SourceService: studioAsset.SourceService,
						StudioAssetID: studioAsset.ID,
					}
					resp.ReusedStudioAssets = append(resp.ReusedStudioAssets, asset)
					return postPassImageRef{asset: asset, bytes: bytes, mimeType: mimeType}, nil
				}
			}
		}
	}

	dataURI, sourceService, err := generateImageForPostPass(req, prompt, referenceImageBase64)
	if err != nil {
		return postPassImageRef{}, fmt.Errorf("image generation failed for %q: %s", prompt, sanitizeProviderError(err))
	}

	bytes, mimeType, err := decodeBase64Payload(dataURI, "image/png")
	if err != nil {
		return postPassImageRef{}, fmt.Errorf("generated image decode failed for %q: %s", prompt, sanitizeProviderError(err))
	}

	ext := imageExtensionForMimeType(mimeType)
	filename := deterministicAssetFilename("img", prompt, ext)
	relativePath, err := writePrototypeAsset(projectPath, filename, bytes, map[string]struct{}{
		".png":  {},
		".jpg":  {},
		".jpeg": {},
		".webp": {},
	}, maxGeneratedImageBytes)
	if err != nil {
		return postPassImageRef{}, fmt.Errorf("failed to write generated image for %q: %s", prompt, sanitizeProviderError(err))
	}

	asset := OpenCodeMediaAsset{
		Prompt:        prompt,
		Placeholder:   placeholderToken,
		MediaType:     "image",
		Filename:      filename,
		RelativePath:  relativePath,
		SourceService: sourceService,
	}
	resp.GeneratedAssets = append(resp.GeneratedAssets, asset)
	return postPassImageRef{asset: asset, bytes: bytes, mimeType: mimeType}, nil
}

func materializeVideoPlaceholder(
	ctx context.Context,
	req OpenCodeMediaPostPassRequest,
	projectPath string,
	placeholder postPassVideoPlaceholder,
	imageRefsByKey map[string]postPassImageRef,
	studioAssets []studioAssetRecord,
	resp *OpenCodeMediaPostPassResponse,
) (OpenCodeMediaAsset, error) {
	if req.VeoGeminiKey == "" {
		return OpenCodeMediaAsset{}, fmt.Errorf("missing veoGeminiKey for glowbyvideo:%s", placeholder.prompt)
	}

	fromRef, err := resolveVideoStartFrame(placeholder.fromKey, imageRefsByKey, studioAssets, req.ImageSource)
	if err != nil {
		return OpenCodeMediaAsset{}, fmt.Errorf("video source resolution failed for %q: %s", placeholder.prompt, sanitizeProviderError(err))
	}

	videoBytes, err := generateVeoVideoFromImage(ctx, placeholder.prompt, placeholder.aspectRatio, req.VeoGeminiKey, fromRef.bytes, fromRef.mimeType)
	if err != nil {
		return OpenCodeMediaAsset{}, fmt.Errorf("video generation failed for %q: %s", placeholder.prompt, sanitizeProviderError(err))
	}

	filename := deterministicAssetFilename("video", placeholder.prompt, ".mp4")
	relativePath, err := writePrototypeAsset(projectPath, filename, videoBytes, map[string]struct{}{
		".mp4": {},
	}, maxGeneratedVideoBytes)
	if err != nil {
		return OpenCodeMediaAsset{}, fmt.Errorf("failed to write generated video for %q: %s", placeholder.prompt, sanitizeProviderError(err))
	}

	asset := OpenCodeMediaAsset{
		Prompt:        placeholder.prompt,
		Placeholder:   placeholder.token,
		MediaType:     "video",
		Filename:      filename,
		RelativePath:  relativePath,
		SourceService: "Veo",
	}
	resp.GeneratedAssets = append(resp.GeneratedAssets, asset)
	return asset, nil
}

func materializeAudioPlaceholder(
	req OpenCodeMediaPostPassRequest,
	projectPath string,
	placeholder postPassAudioPlaceholder,
	studioAssets []studioAssetRecord,
	resp *OpenCodeMediaPostPassResponse,
) (OpenCodeMediaAsset, error) {
	if strings.TrimSpace(placeholder.prompt) == "" {
		return OpenCodeMediaAsset{}, fmt.Errorf("audio placeholder prompt is empty")
	}

	if studioAsset, ok := findStudioAssetByPrompt(studioAssets, "audio", placeholder.prompt, "ElevenLabs"); ok {
		bytes, mimeType, err := decodeBase64Payload(studioAsset.DataBase64, "audio/mpeg")
		if err == nil {
			ext := audioExtensionForMimeType(mimeType)
			filename := deterministicAssetFilename("audio", placeholder.prompt, ext)
			relativePath, err := writePrototypeAsset(projectPath, filename, bytes, map[string]struct{}{
				".mp3":  {},
				".wav":  {},
				".ogg":  {},
				".flac": {},
				".m4a":  {},
				".aac":  {},
			}, maxGeneratedAudioBytes)
			if err == nil {
				asset := OpenCodeMediaAsset{
					Prompt:        placeholder.prompt,
					Placeholder:   placeholder.token,
					MediaType:     "audio",
					Filename:      filename,
					RelativePath:  relativePath,
					SourceService: studioAsset.SourceService,
					StudioAssetID: studioAsset.ID,
				}
				resp.ReusedStudioAssets = append(resp.ReusedStudioAssets, asset)
				return asset, nil
			}
		}
	}

	audioBytes, mimeType, sourceService, err := generateElevenLabsAudioForPostPass(
		placeholder,
		strings.TrimSpace(req.ElevenLabsKey),
		strings.TrimSpace(req.ElevenLabsVoiceID),
		strings.TrimSpace(req.ElevenLabsVoiceModel),
	)
	if err != nil {
		return OpenCodeMediaAsset{}, fmt.Errorf("audio generation failed for %q: %s", placeholder.prompt, sanitizeProviderError(err))
	}

	ext := audioExtensionForMimeType(mimeType)
	filename := deterministicAssetFilename("audio", placeholder.prompt, ext)
	relativePath, err := writePrototypeAsset(projectPath, filename, audioBytes, map[string]struct{}{
		".mp3":  {},
		".wav":  {},
		".ogg":  {},
		".flac": {},
		".m4a":  {},
		".aac":  {},
	}, maxGeneratedAudioBytes)
	if err != nil {
		return OpenCodeMediaAsset{}, fmt.Errorf("failed to write generated audio for %q: %s", placeholder.prompt, sanitizeProviderError(err))
	}

	asset := OpenCodeMediaAsset{
		Prompt:        placeholder.prompt,
		Placeholder:   placeholder.token,
		MediaType:     "audio",
		Filename:      filename,
		RelativePath:  relativePath,
		SourceService: sourceService,
	}
	resp.GeneratedAssets = append(resp.GeneratedAssets, asset)
	return asset, nil
}

func generateElevenLabsAudioForPostPass(
	placeholder postPassAudioPlaceholder,
	apiKey string,
	defaultVoiceID string,
	defaultVoiceModel string,
) ([]byte, string, string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, "", "", fmt.Errorf("missing elevenLabsKey for glowbyaudio:%s", placeholder.prompt)
	}

	audioType := normalizePostPassAudioType(placeholder.audioType, placeholder.prompt)
	voiceID := strings.TrimSpace(placeholder.voiceID)
	if voiceID == "" {
		voiceID = strings.TrimSpace(defaultVoiceID)
	}
	voiceModel := normalizeElevenLabsVoiceModel(placeholder.modelID)
	if strings.TrimSpace(voiceModel) == "" {
		voiceModel = normalizeElevenLabsVoiceModel(defaultVoiceModel)
	}
	if strings.TrimSpace(voiceModel) == "" {
		voiceModel = defaultElevenVoiceModel
	}
	modelID := strings.TrimSpace(placeholder.modelID)
	if strings.EqualFold(modelID, "standard") || strings.EqualFold(modelID, "default") {
		modelID = ""
	}

	req := ElevenLabsAudioRequest{
		Prompt:            placeholder.prompt,
		AudioType:         audioType,
		VoiceID:           voiceID,
		DurationSeconds:   placeholder.durationSeconds,
		PromptInfluence:   placeholder.promptInfluence,
		Loop:              placeholder.loop,
		ForceInstrumental: placeholder.forceInstrumental,
	}

	switch audioType {
	case "voice":
		req.VoiceModel = voiceModel
	case "sound":
		req.SoundModel = modelID
	case "music":
		req.MusicModel = modelID
	}

	var (
		audioBytes []byte
		mimeType   string
		err        error
	)

	switch audioType {
	case "voice":
		log.Printf("[OPENCODE][MEDIA][AUDIO] generating voice prompt=%q voice_id=%s model=%s", req.Prompt, req.VoiceID, req.VoiceModel)
		audioBytes, mimeType, err = callElevenLabsVoice(req, apiKey)
	case "sound":
		log.Printf("[OPENCODE][MEDIA][AUDIO] generating sound prompt=%q model=%s", req.Prompt, req.SoundModel)
		audioBytes, mimeType, err = callElevenLabsSound(req, apiKey)
	case "music":
		log.Printf("[OPENCODE][MEDIA][AUDIO] generating music prompt=%q model=%s", req.Prompt, req.MusicModel)
		audioBytes, mimeType, err = callElevenLabsMusic(req, apiKey)
	default:
		return nil, "", "", fmt.Errorf("unsupported audio type: %s", audioType)
	}
	if err != nil {
		log.Printf("[OPENCODE][MEDIA][AUDIO] generation failed type=%s err=%s", audioType, sanitizeProviderError(err))
		return nil, "", "", err
	}
	log.Printf("[OPENCODE][MEDIA][AUDIO] generation succeeded type=%s bytes=%d mime=%s", audioType, len(audioBytes), mimeType)

	sourceService := "ElevenLabs"
	switch audioType {
	case "voice":
		sourceService = "ElevenLabs (Voice)"
	case "sound":
		sourceService = "ElevenLabs (Sound FX)"
	case "music":
		sourceService = "ElevenLabs (Music)"
	}

	return audioBytes, mimeType, sourceService, nil
}

func generateImageForPostPass(req OpenCodeMediaPostPassRequest, prompt, referenceImageBase64 string) (string, string, error) {
	source := strings.TrimSpace(req.ImageSource)
	if source == "" {
		source = openAIImageSourceLabel
	}
	lowerSource := strings.ToLower(source)
	useReference := strings.TrimSpace(referenceImageBase64) != ""

	switch {
	case isOpenAIImageSource(source):
		if req.OpenAIKey == "" {
			return "", source, fmt.Errorf("openai key is required for %s", source)
		}
		if useReference {
			dataURI, err := callOpenAIImageGenerationWithReference(prompt, referenceImageBase64, "", "", req.OpenAIKey)
			return dataURI, source, err
		}
		dataURI, err := callOpenAIImageGeneration(prompt, "", "", req.OpenAIKey)
		return dataURI, source, err
	case strings.Contains(lowerSource, "nano banana"):
		if req.GeminiKey == "" {
			return "", source, fmt.Errorf("gemini key is required for %s", source)
		}
		if useReference {
			dataURI, err := callGeminiImageGenerationWithReference(prompt, referenceImageBase64, "", "", req.GeminiKey)
			return dataURI, source, err
		}
		dataURI, err := callGeminiImageGeneration(prompt, "", "", req.GeminiKey)
		return dataURI, source, err
	case strings.Contains(lowerSource, "grok imagine image"), strings.Contains(lowerSource, "grok-imagine-image"), strings.Contains(lowerSource, "grok 2 image gen"):
		if req.XaiKey == "" {
			return "", source, fmt.Errorf("xai key is required for %s", source)
		}
		if useReference {
			if dataURI, err := callGrokImageGenerationWithReference(prompt, referenceImageBase64, req.XaiKey, ""); err == nil {
				return dataURI, source, nil
			}
		}
		dataURI, err := callGrokImageGeneration(prompt, req.XaiKey, "")
		return dataURI, source, err
	default:
		if req.OpenAIKey != "" {
			if useReference {
				dataURI, err := callOpenAIImageGenerationWithReference(prompt, referenceImageBase64, "", "", req.OpenAIKey)
				return dataURI, openAIImageSourceLabel, err
			}
			dataURI, err := callOpenAIImageGeneration(prompt, "", "", req.OpenAIKey)
			return dataURI, openAIImageSourceLabel, err
		}
		if req.GeminiKey != "" {
			if useReference {
				dataURI, err := callGeminiImageGenerationWithReference(prompt, referenceImageBase64, "", "", req.GeminiKey)
				return dataURI, "Glowbom Images (Nano Banana 2)", err
			}
			dataURI, err := callGeminiImageGeneration(prompt, "", "", req.GeminiKey)
			return dataURI, "Glowbom Images (Nano Banana 2)", err
		}
		if req.XaiKey != "" {
			dataURI, err := callGrokImageGeneration(prompt, req.XaiKey, "")
			return dataURI, xAIImageSourceLabel, err
		}
		return "", source, fmt.Errorf("no image provider key available")
	}
}

func generateVeoVideoFromImage(ctx context.Context, prompt, aspectRatio, geminiKey string, imageBytes []byte, mimeType string) ([]byte, error) {
	if geminiKey == "" {
		return nil, fmt.Errorf("gemini key is required for Veo generation")
	}
	if len(imageBytes) == 0 {
		return nil, fmt.Errorf("image bytes are required for Veo generation")
	}

	if aspectRatio == "" {
		aspectRatio = "16:9"
	}

	request := VeoGenerationRequest{
		Prompt: prompt,
		Images: []VeoImageInput{
			{
				Data:     base64.StdEncoding.EncodeToString(imageBytes),
				MimeType: mimeType,
			},
		},
		AspectRatio:  aspectRatio,
		UseKeyframes: false,
		GeminiKey:    geminiKey,
	}

	startResp, err := startVeoVideoGeneration(request)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(10 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("video generation timed out")
		}

		pollResp, err := pollVeoOperation(startResp.OperationID, geminiKey)
		if err != nil {
			return nil, err
		}

		if pollResp.Error != "" {
			return nil, fmt.Errorf("%s", pollResp.Error)
		}

		if pollResp.Done {
			if pollResp.Status != "completed" || pollResp.VideoURL == "" {
				return nil, fmt.Errorf("video generation finished without output")
			}
			return downloadVeoVideoBinary(pollResp.VideoURL, geminiKey)
		}

		time.Sleep(8 * time.Second)
	}
}

func downloadVeoVideoBinary(videoURL, apiKey string) ([]byte, error) {
	parsed, err := neturl.Parse(videoURL)
	if err != nil {
		return nil, fmt.Errorf("invalid video url: %w", err)
	}

	query := parsed.Query()
	if query.Get("key") == "" {
		query.Set("key", apiKey)
	}
	parsed.RawQuery = query.Encode()

	req, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("video download failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxGeneratedVideoBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxGeneratedVideoBytes {
		return nil, fmt.Errorf("video exceeds maximum allowed size")
	}
	return data, nil
}

func writePrototypeAssetsManifest(projectPath string, assets []OpenCodeMediaAsset, defaultImageSource string) error {
	manifestPath, err := safeProjectPath(projectPath, "prototype", "assets.json")
	if err != nil {
		return err
	}

	manifest := prototypeAssetsManifest{
		Version:    "1.0",
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Assets:     []prototypeAssetsManifestItem{},
	}

	if existingData, err := os.ReadFile(manifestPath); err == nil {
		_ = json.Unmarshal(existingData, &manifest)
		if manifest.Version == "" {
			manifest.Version = "1.0"
		}
		if manifest.Assets == nil {
			manifest.Assets = []prototypeAssetsManifestItem{}
		}
	}

	byFilename := make(map[string]prototypeAssetsManifestItem)
	for _, item := range manifest.Assets {
		byFilename[item.Filename] = item
	}

	for _, asset := range assets {
		sourceService := asset.SourceService
		if sourceService == "" {
			if asset.MediaType == "video" {
				sourceService = "Veo"
			} else if asset.MediaType == "audio" {
				sourceService = "ElevenLabs"
			} else {
				sourceService = defaultImageSource
			}
		}

		byFilename[asset.Filename] = prototypeAssetsManifestItem{
			Filename:      asset.Filename,
			Prompt:        asset.Prompt,
			SourceService: sourceService,
			MediaType:     asset.MediaType,
		}
	}

	manifest.Assets = make([]prototypeAssetsManifestItem, 0, len(byFilename))
	for _, item := range byFilename {
		manifest.Assets = append(manifest.Assets, item)
	}
	sort.Slice(manifest.Assets, func(i, j int) bool {
		return manifest.Assets[i].Filename < manifest.Assets[j].Filename
	})

	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(manifestPath, payload, 0644)
}

func writePlatformAssetsMap(projectPath string, copies []OpenCodePlatformCopy) error {
	mapPath, err := safeProjectPath(projectPath, "prototype", "platform_assets_map.json")
	if err != nil {
		return err
	}

	rowsBySource := make(map[string]*platformAssetsMapRow)
	for _, copy := range copies {
		row, ok := rowsBySource[copy.Source]
		if !ok {
			row = &platformAssetsMapRow{
				Source:       copy.Source,
				MediaType:    classifyMediaTypeByExt(filepath.Ext(copy.Source)),
				Destinations: []platformAssetsMapTargetRow{},
			}
			rowsBySource[copy.Source] = row
		}
		row.Destinations = append(row.Destinations, platformAssetsMapTargetRow{
			Platform: copy.Platform,
			Path:     copy.Destination,
		})
	}

	rows := make([]platformAssetsMapRow, 0, len(rowsBySource))
	for _, row := range rowsBySource {
		sort.Slice(row.Destinations, func(i, j int) bool {
			if row.Destinations[i].Platform == row.Destinations[j].Platform {
				return row.Destinations[i].Path < row.Destinations[j].Path
			}
			return row.Destinations[i].Platform < row.Destinations[j].Platform
		})
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Source < rows[j].Source
	})

	payload, err := json.MarshalIndent(platformAssetsMap{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Assets:      rows,
	}, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(mapPath, payload, 0644)
}

func syncAssetsToPlatforms(projectPath string) ([]OpenCodePlatformCopy, bool, []string, error) {
	assetsDir, err := safeProjectPath(projectPath, "prototype", "assets")
	if err != nil {
		return nil, false, nil, err
	}
	if _, err := os.Stat(assetsDir); err != nil {
		if os.IsNotExist(err) {
			return []OpenCodePlatformCopy{}, false, []string{}, nil
		}
		return nil, false, nil, err
	}

	type platformTarget struct {
		name      string
		imagePath string
		videoPath string
		audioPath string
		enabled   bool
		special   string
	}

	appleRoot, _ := safeProjectPath(projectPath, "apple", "Custom", "Assets.xcassets")
	androidRoot, _ := safeProjectPath(projectPath, "android", "app", "src", "main", "res")
	webRoot, _ := safeProjectPath(projectPath, "web", "public")
	godotRoot, _ := safeProjectPath(projectPath, "godot", "assets")

	targets := []platformTarget{
		{
			name:    "apple",
			enabled: directoryExists(appleRoot),
			special: "apple",
		},
		{
			name:      "android",
			imagePath: filepath.Join(androidRoot, "drawable"),
			videoPath: filepath.Join(androidRoot, "raw"),
			audioPath: filepath.Join(androidRoot, "raw"),
			enabled:   directoryExists(androidRoot),
		},
		{
			name:      "web",
			imagePath: filepath.Join(webRoot, "assets", "images"),
			videoPath: filepath.Join(webRoot, "assets", "videos"),
			audioPath: filepath.Join(webRoot, "assets", "audio"),
			enabled:   directoryExists(webRoot),
		},
		{
			name:      "godot",
			imagePath: filepath.Join(godotRoot, "sprites"),
			videoPath: filepath.Join(godotRoot, "videos"),
			audioPath: filepath.Join(godotRoot, "audio"),
			enabled:   directoryExists(godotRoot),
		},
	}

	synced := false
	warnings := []string{}
	copies := []OpenCodePlatformCopy{}

	err = filepath.WalkDir(assetsDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		mediaType := classifyMediaTypeByExt(ext)
		if mediaType == "" {
			return nil
		}

		relSource, err := filepath.Rel(projectPath, path)
		if err != nil {
			return err
		}
		relSource = filepath.ToSlash(relSource)

		for _, target := range targets {
			if !target.enabled {
				continue
			}

			var destinationPath string
			if target.special == "apple" {
				destinationPath, err = copyToAppleAssetCatalog(path, appleRoot, mediaType)
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("Apple sync failed for %s: %s", relSource, sanitizeProviderError(err)))
					continue
				}
			} else {
				baseName := resourceSafeName(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
				var platformDir string
				if mediaType == "video" {
					platformDir = target.videoPath
				} else if mediaType == "audio" {
					platformDir = target.audioPath
				} else {
					platformDir = target.imagePath
				}

				if err := os.MkdirAll(platformDir, 0755); err != nil {
					warnings = append(warnings, fmt.Sprintf("%s sync failed for %s: %s", strings.Title(target.name), relSource, sanitizeProviderError(err)))
					continue
				}

				var fileName string
				if mediaType == "video" {
					fileName = baseName + ".mp4"
				} else if mediaType == "audio" {
					fileName = baseName + ext
				} else {
					fileName = baseName + ext
				}

				destinationPath = filepath.Join(platformDir, fileName)
				if err := copyFile(path, destinationPath, maxAssetBytesForMediaType(mediaType)); err != nil {
					warnings = append(warnings, fmt.Sprintf("%s sync failed for %s: %s", strings.Title(target.name), relSource, sanitizeProviderError(err)))
					continue
				}
			}

			relDest, relErr := filepath.Rel(projectPath, destinationPath)
			if relErr != nil {
				warnings = append(warnings, fmt.Sprintf("sync path error for %s: %s", relSource, sanitizeProviderError(relErr)))
				continue
			}

			synced = true
			copies = append(copies, OpenCodePlatformCopy{
				Platform:    target.name,
				Source:      relSource,
				Destination: filepath.ToSlash(relDest),
			})
		}

		return nil
	})

	if err != nil {
		return nil, synced, warnings, err
	}

	sort.Slice(copies, func(i, j int) bool {
		if copies[i].Platform == copies[j].Platform {
			if copies[i].Source == copies[j].Source {
				return copies[i].Destination < copies[j].Destination
			}
			return copies[i].Source < copies[j].Source
		}
		return copies[i].Platform < copies[j].Platform
	})

	return copies, synced, dedupeWarnings(warnings), nil
}

func copyToAppleAssetCatalog(sourcePath, assetCatalogRoot, mediaType string) (string, error) {
	ext := strings.ToLower(filepath.Ext(sourcePath))
	baseName := strings.TrimSuffix(filepath.Base(sourcePath), ext)
	setName := resourceSafeName(baseName)

	var setDir string
	var outputName string
	var contents map[string]interface{}

	if mediaType == "video" || mediaType == "audio" {
		setDir = filepath.Join(assetCatalogRoot, setName+".dataset")
		if mediaType == "video" {
			outputName = setName + ".mp4"
		} else {
			outputName = setName + ext
		}
		contents = map[string]interface{}{
			"data": []map[string]string{
				{
					"idiom":    "universal",
					"filename": outputName,
				},
			},
			"info": map[string]interface{}{
				"author":  "xcode",
				"version": 1,
			},
		}
	} else {
		setDir = filepath.Join(assetCatalogRoot, setName+".imageset")
		outputName = setName + ext
		contents = map[string]interface{}{
			"images": []map[string]string{
				{
					"idiom":    "universal",
					"filename": outputName,
				},
			},
			"info": map[string]interface{}{
				"author":  "xcode",
				"version": 1,
			},
		}
	}

	if err := os.MkdirAll(setDir, 0755); err != nil {
		return "", err
	}

	targetPath := filepath.Join(setDir, outputName)
	if err := copyFile(sourcePath, targetPath, maxAssetBytesForMediaType(mediaType)); err != nil {
		return "", err
	}

	contentsPath := filepath.Join(setDir, "Contents.json")
	payload, err := json.MarshalIndent(contents, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(contentsPath, payload, 0644); err != nil {
		return "", err
	}

	return targetPath, nil
}

func materializeFromStudioAsset(asset studioAssetRecord, mediaType string, prompt string, placeholder string, projectPath string, sourceService string, maxBytes int, allowed map[string]struct{}) (OpenCodeMediaAsset, []byte, string, error) {
	bytes, mimeType, err := decodeBase64Payload(asset.DataBase64, "application/octet-stream")
	if err != nil {
		return OpenCodeMediaAsset{}, nil, "", err
	}

	ext := ".bin"
	if mediaType == "image" {
		ext = imageExtensionForMimeType(mimeType)
	} else if mediaType == "video" {
		ext = ".mp4"
	} else if mediaType == "audio" {
		ext = audioExtensionForMimeType(mimeType)
	}

	filename := deterministicAssetFilename(mediaType, prompt, ext)
	relativePath, err := writePrototypeAsset(projectPath, filename, bytes, allowed, maxBytes)
	if err != nil {
		return OpenCodeMediaAsset{}, nil, "", err
	}

	return OpenCodeMediaAsset{
		Prompt:        prompt,
		Placeholder:   placeholder,
		MediaType:     mediaType,
		Filename:      filename,
		RelativePath:  relativePath,
		SourceService: sourceService,
		StudioAssetID: asset.ID,
	}, bytes, mimeType, nil
}

func resolveVideoStartFrame(fromKey string, imageRefsByKey map[string]postPassImageRef, studioAssets []studioAssetRecord, imageSource string) (postPassImageRef, error) {
	normalized := normalizedLookupKey(fromKey)
	if ref, ok := imageRefsByKey[normalized]; ok {
		return ref, nil
	}

	if studioAsset, ok := findStudioAssetByKey(studioAssets, "image", fromKey, imageSource); ok {
		bytes, mimeType, err := decodeBase64Payload(studioAsset.DataBase64, "image/png")
		if err != nil {
			return postPassImageRef{}, err
		}

		filename := deterministicAssetFilename("img", studioAsset.Prompt, imageExtensionForMimeType(mimeType))
		return postPassImageRef{
			asset: OpenCodeMediaAsset{
				Prompt:        studioAsset.Prompt,
				MediaType:     "image",
				Filename:      filename,
				RelativePath:  filepath.ToSlash(filepath.Join("prototype", "assets", filename)),
				SourceService: studioAsset.SourceService,
				StudioAssetID: studioAsset.ID,
			},
			bytes:    bytes,
			mimeType: mimeType,
		}, nil
	}

	return postPassImageRef{}, fmt.Errorf("from:%s does not match any image generated in this pass or Studio image", fromKey)
}

func registerImageRefKeys(index map[string]postPassImageRef, ref postPassImageRef) {
	index[normalizedLookupKey(ref.asset.Prompt)] = ref
	index[normalizedLookupKey(strings.TrimSuffix(ref.asset.Filename, filepath.Ext(ref.asset.Filename)))] = ref
	index[normalizedLookupKey(resourceSafeName(ref.asset.Prompt))] = ref
}

type imagePlaceholder struct {
	Token  string
	Prompt string
}

func extractImagePlaceholders(content string) []imagePlaceholder {
	matches := glowbyImagePlaceholderRegex.FindAllStringSubmatch(content, -1)
	result := []imagePlaceholder{}
	seen := make(map[string]struct{})

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		token := strings.TrimSpace(match[0])
		prompt := strings.TrimSpace(match[1])
		if token == "" || prompt == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		result = append(result, imagePlaceholder{Token: token, Prompt: prompt})
	}

	return result
}

func extractVideoPlaceholders(content string) []postPassVideoPlaceholder {
	matches := glowbyVideoPlaceholderRegex.FindAllStringSubmatch(content, -1)
	result := []postPassVideoPlaceholder{}
	seen := make(map[string]struct{})

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		token := strings.TrimSpace(match[0])
		payload := strings.TrimSpace(match[1])
		if token == "" || payload == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}

		placeholder := parseGlowbyVideoPayload(token, payload)
		if placeholder.prompt == "" {
			continue
		}
		result = append(result, placeholder)
	}

	return result
}

func parseGlowbyVideoPayload(token, payload string) postPassVideoPlaceholder {
	parts := strings.Split(payload, "|")
	placeholder := postPassVideoPlaceholder{
		token:       token,
		prompt:      strings.TrimSpace(parts[0]),
		aspectRatio: "16:9",
	}

	for _, part := range parts[1:] {
		part = strings.TrimSpace(part)
		lowerPart := strings.ToLower(part)
		if strings.HasPrefix(lowerPart, "from:") {
			placeholder.fromKey = strings.TrimSpace(part[len("from:"):])
			continue
		}
		if strings.HasPrefix(lowerPart, "aspect:") {
			placeholder.aspectRatio = normalizeAspectRatio(strings.TrimSpace(part[len("aspect:"):]))
		}
	}

	return placeholder
}

func extractAudioPlaceholders(content string) []postPassAudioPlaceholder {
	matches := glowbyAudioPlaceholderRegex.FindAllStringSubmatch(content, -1)
	result := []postPassAudioPlaceholder{}
	seen := make(map[string]struct{})

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		token := strings.TrimSpace(match[0])
		payload := strings.TrimSpace(match[1])
		if token == "" || payload == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}

		placeholder := parseGlowbyAudioPayload(token, payload)
		if placeholder.prompt == "" {
			continue
		}
		result = append(result, placeholder)
	}

	return result
}

func parseGlowbyAudioPayload(token, payload string) postPassAudioPlaceholder {
	parts := strings.Split(payload, "|")
	placeholder := postPassAudioPlaceholder{
		token:     token,
		prompt:    strings.TrimSpace(parts[0]),
		audioType: inferAudioTypeFromPrompt(strings.TrimSpace(parts[0])),
	}

	for _, part := range parts[1:] {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		lowerPart := strings.ToLower(part)
		switch {
		case strings.HasPrefix(lowerPart, "type:"):
			placeholder.audioType = normalizePostPassAudioType(strings.TrimSpace(part[len("type:"):]), placeholder.prompt)
		case strings.HasPrefix(lowerPart, "voice:"):
			placeholder.voiceID = strings.TrimSpace(part[len("voice:"):])
		case strings.HasPrefix(lowerPart, "voiceid:"):
			placeholder.voiceID = strings.TrimSpace(part[len("voiceid:"):])
		case strings.HasPrefix(lowerPart, "voice_id:"):
			placeholder.voiceID = strings.TrimSpace(part[len("voice_id:"):])
		case strings.HasPrefix(lowerPart, "model:"):
			placeholder.modelID = strings.TrimSpace(part[len("model:"):])
		case strings.HasPrefix(lowerPart, "modelid:"):
			placeholder.modelID = strings.TrimSpace(part[len("modelid:"):])
		case strings.HasPrefix(lowerPart, "model_id:"):
			placeholder.modelID = strings.TrimSpace(part[len("model_id:"):])
		case strings.HasPrefix(lowerPart, "duration:"):
			if seconds, err := strconv.ParseFloat(strings.TrimSpace(part[len("duration:"):]), 64); err == nil && seconds > 0 {
				placeholder.durationSeconds = seconds
			}
		case strings.HasPrefix(lowerPart, "durationseconds:"):
			if seconds, err := strconv.ParseFloat(strings.TrimSpace(part[len("durationseconds:"):]), 64); err == nil && seconds > 0 {
				placeholder.durationSeconds = seconds
			}
		case strings.HasPrefix(lowerPart, "promptinfluence:"):
			if influence, err := strconv.ParseFloat(strings.TrimSpace(part[len("promptinfluence:"):]), 64); err == nil {
				placeholder.promptInfluence = &influence
			}
		case strings.HasPrefix(lowerPart, "influence:"):
			if influence, err := strconv.ParseFloat(strings.TrimSpace(part[len("influence:"):]), 64); err == nil {
				placeholder.promptInfluence = &influence
			}
		case strings.HasPrefix(lowerPart, "loop:"):
			placeholder.loop = parseBoolOption(strings.TrimSpace(part[len("loop:"):]))
		case strings.HasPrefix(lowerPart, "instrumental:"):
			placeholder.forceInstrumental = parseBoolOption(strings.TrimSpace(part[len("instrumental:"):]))
		case strings.HasPrefix(lowerPart, "forceinstrumental:"):
			placeholder.forceInstrumental = parseBoolOption(strings.TrimSpace(part[len("forceinstrumental:"):]))
		}
	}

	placeholder.audioType = normalizePostPassAudioType(placeholder.audioType, placeholder.prompt)
	return placeholder
}

func normalizePostPassAudioType(audioType string, prompt string) string {
	normalized := normalizeAudioType(audioType)
	if normalized == "" {
		return inferAudioTypeFromPrompt(prompt)
	}
	switch normalized {
	case "voice", "sound", "music":
		return normalized
	default:
		return inferAudioTypeFromPrompt(prompt)
	}
}

func inferAudioTypeFromPrompt(prompt string) string {
	lower := strings.ToLower(strings.TrimSpace(prompt))
	if lower == "" {
		return "voice"
	}

	voiceHints := []string{"voice", "voiceover", "narration", "narrator", "spoken", "dialogue", "read aloud", "tts"}
	for _, hint := range voiceHints {
		if strings.Contains(lower, hint) {
			return "voice"
		}
	}

	musicHints := []string{"music", "song", "soundtrack", "background track", "melody", "instrumental", "beat"}
	for _, hint := range musicHints {
		if strings.Contains(lower, hint) {
			return "music"
		}
	}

	soundHints := []string{
		"sound effect",
		"sfx",
		"fx",
		"effect",
		"ambient",
		"foley",
		"explosion",
		"whoosh",
		"footstep",
		"door slam",
		"meow",
		"bark",
		"woof",
		"chirp",
		"scream",
		"thunder",
		"rain",
		"wind",
		"gunshot",
		"impact",
	}
	for _, hint := range soundHints {
		if strings.Contains(lower, hint) {
			return "sound"
		}
	}

	return "voice"
}

func parseBoolOption(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func floatPointerKey(value *float64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%.3f", *value)
}

func normalizeAspectRatio(value string) string {
	switch strings.TrimSpace(value) {
	case "16:9", "9:16", "1:1", "4:3", "3:4":
		return value
	default:
		return "16:9"
	}
}

func resolveReferenceImage(req OpenCodeMediaPostPassRequest, projectPath string, studioAssets []studioAssetRecord) (string, string, error) {
	if strings.TrimSpace(req.ReferenceImagePath) != "" {
		refPath := strings.TrimSpace(req.ReferenceImagePath)
		var absPath string
		var err error
		if filepath.IsAbs(refPath) {
			absPath = refPath
		} else {
			absPath, err = safeProjectPath(projectPath, refPath)
			if err != nil {
				return "", "", err
			}
		}

		data, err := os.ReadFile(absPath)
		if err != nil {
			return "", "", err
		}
		if len(data) > maxGeneratedImageBytes {
			return "", "", fmt.Errorf("reference image is too large")
		}

		mimeType := detectMimeType(data, "image/png")
		return base64.StdEncoding.EncodeToString(data), mimeType, nil
	}

	if strings.TrimSpace(req.ReferenceAssetID) != "" {
		assetID := normalizedLookupKey(req.ReferenceAssetID)
		for _, asset := range studioAssets {
			if normalizedLookupKey(asset.ID) != assetID {
				continue
			}
			if asset.DataBase64 == "" {
				return "", "", fmt.Errorf("reference asset has no image data")
			}
			bytes, mimeType, err := decodeBase64Payload(asset.DataBase64, "image/png")
			if err != nil {
				return "", "", err
			}
			return base64.StdEncoding.EncodeToString(bytes), mimeType, nil
		}
		return "", "", fmt.Errorf("reference asset not found in Studio")
	}

	return "", "", nil
}

func loadStudioAssets() ([]studioAssetRecord, error) {
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}

	assetsDir := filepath.Join(userConfigDir, "Glowbom", "Studio", "Assets")
	entries, err := os.ReadDir(assetsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []studioAssetRecord{}, nil
		}
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() > entries[j].Name()
	})

	records := []studioAssetRecord{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}

		path := filepath.Join(assetsDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var record studioAssetRecord
		if err := json.Unmarshal(data, &record); err != nil {
			continue
		}

		if strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.Prompt) == "" {
			continue
		}
		records = append(records, record)
	}

	return records, nil
}

func findStudioAssetByPrompt(assets []studioAssetRecord, mediaType, prompt, preferredSource string) (studioAssetRecord, bool) {
	normalizedPrompt := normalizedLookupKey(prompt)
	normalizedSource := normalizedLookupKey(preferredSource)

	var fallback studioAssetRecord
	foundFallback := false

	for _, asset := range assets {
		if normalizedLookupKey(asset.MediaType) != normalizedLookupKey(mediaType) {
			continue
		}
		if normalizedLookupKey(asset.Prompt) != normalizedPrompt {
			continue
		}
		if asset.DataBase64 == "" {
			continue
		}
		if normalizedSource != "" && normalizedLookupKey(asset.SourceService) == normalizedSource {
			return asset, true
		}
		if !foundFallback {
			fallback = asset
			foundFallback = true
		}
	}

	return fallback, foundFallback
}

func findStudioAssetByKey(assets []studioAssetRecord, mediaType, key, preferredSource string) (studioAssetRecord, bool) {
	normalizedKey := normalizedLookupKey(key)
	normalizedSource := normalizedLookupKey(preferredSource)

	var fallback studioAssetRecord
	foundFallback := false

	for _, asset := range assets {
		if normalizedLookupKey(asset.MediaType) != normalizedLookupKey(mediaType) {
			continue
		}
		if asset.DataBase64 == "" {
			continue
		}

		promptKey := normalizedLookupKey(asset.Prompt)
		derivedKey := normalizedLookupKey(resourceSafeName(asset.Prompt))
		idKey := normalizedLookupKey(asset.ID)

		if normalizedKey != promptKey && normalizedKey != derivedKey && normalizedKey != idKey {
			continue
		}

		if normalizedSource != "" && normalizedLookupKey(asset.SourceService) == normalizedSource {
			return asset, true
		}
		if !foundFallback {
			fallback = asset
			foundFallback = true
		}
	}

	return fallback, foundFallback
}

func resolveScanTargetPath(projectPath, rawTarget string) (string, error) {
	target := strings.TrimSpace(rawTarget)
	if target == "" {
		target = "prototype/index.html"
	}

	if filepath.IsAbs(target) {
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return "", err
		}
		if !isPathWithin(projectPath, absTarget) {
			return "", fmt.Errorf("scan target escapes project root: %s", rawTarget)
		}
		return absTarget, nil
	}

	return safeProjectPath(projectPath, target)
}

func writePrototypeAsset(projectPath, filename string, data []byte, allowedExtensions map[string]struct{}, maxSize int) (string, error) {
	assetsDir, err := safeProjectPath(projectPath, "prototype", "assets")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(assetsDir, 0755); err != nil {
		return "", err
	}

	targetPath, err := safeProjectPath(projectPath, "prototype", "assets", filename)
	if err != nil {
		return "", err
	}

	ext := strings.ToLower(filepath.Ext(targetPath))
	if _, ok := allowedExtensions[ext]; !ok {
		return "", fmt.Errorf("extension %s is not allowed", ext)
	}
	if len(data) > maxSize {
		return "", fmt.Errorf("asset exceeds maximum size")
	}

	if err := os.WriteFile(targetPath, data, 0644); err != nil {
		return "", err
	}

	rel, err := filepath.Rel(projectPath, targetPath)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func copyFile(sourcePath, destinationPath string, maxSize int64) error {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return err
	}
	if info.Size() > maxSize {
		return fmt.Errorf("source file exceeds max size")
	}

	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	if int64(len(data)) > maxSize {
		return fmt.Errorf("source file exceeds max size")
	}

	return os.WriteFile(destinationPath, data, 0644)
}

func safeProjectPath(projectPath string, pathSegments ...string) (string, error) {
	baseAbs, err := filepath.Abs(projectPath)
	if err != nil {
		return "", err
	}

	all := append([]string{baseAbs}, pathSegments...)
	target := filepath.Join(all...)
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}

	if !isPathWithin(baseAbs, targetAbs) {
		return "", fmt.Errorf("path escapes project root: %s", filepath.Join(pathSegments...))
	}
	return targetAbs, nil
}

func isPathWithin(basePath, targetPath string) bool {
	rel, err := filepath.Rel(basePath, targetPath)
	if err != nil {
		return false
	}
	rel = filepath.Clean(rel)
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != "..")
}

func decodeBase64Payload(value string, fallbackMime string) ([]byte, string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, "", fmt.Errorf("empty payload")
	}

	mimeType := fallbackMime
	base64Payload := trimmed

	if strings.HasPrefix(trimmed, "data:") {
		parts := strings.SplitN(trimmed, ",", 2)
		if len(parts) != 2 {
			return nil, "", fmt.Errorf("invalid data URI")
		}
		meta := strings.TrimPrefix(parts[0], "data:")
		if idx := strings.Index(meta, ";"); idx >= 0 {
			meta = meta[:idx]
		}
		if strings.TrimSpace(meta) != "" {
			mimeType = meta
		}
		base64Payload = parts[1]
	}

	data, err := base64.StdEncoding.DecodeString(base64Payload)
	if err != nil {
		data, err = base64.StdEncoding.DecodeString(strings.TrimSpace(base64Payload))
		if err != nil {
			return nil, "", err
		}
	}

	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = detectMimeType(data, fallbackMime)
	}

	return data, mimeType, nil
}

func detectMimeType(data []byte, fallback string) string {
	if len(data) == 0 {
		return fallback
	}
	detected := http.DetectContentType(data)
	if detected == "" {
		return fallback
	}
	if strings.HasPrefix(detected, "application/octet-stream") {
		return fallback
	}
	return detected
}

func imageExtensionForMimeType(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func audioExtensionForMimeType(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "audio/wav", "audio/wave", "audio/x-wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	case "audio/flac":
		return ".flac"
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "audio/aac", "audio/x-aac":
		return ".aac"
	default:
		return ".mp3"
	}
}

func classifyMediaTypeByExt(ext string) string {
	switch strings.ToLower(strings.TrimSpace(ext)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg":
		return "image"
	case ".mp4", ".webm", ".mov":
		return "video"
	case ".mp3", ".wav", ".ogg", ".flac", ".m4a", ".aac":
		return "audio"
	default:
		return ""
	}
}

func maxAssetBytesForMediaType(mediaType string) int64 {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image":
		return maxGeneratedImageBytes
	case "audio":
		return maxGeneratedAudioBytes
	default:
		return maxGeneratedVideoBytes
	}
}

func deterministicAssetFilename(prefix, prompt, ext string) string {
	cleanPrefix := resourceSafeName(prefix)
	if cleanPrefix == "" {
		cleanPrefix = "asset"
	}
	cleanPrompt := resourceSafeName(prompt)
	if cleanPrompt == "" {
		cleanPrompt = "item"
	}
	if len(cleanPrompt) > 36 {
		cleanPrompt = cleanPrompt[:36]
	}

	hash := sha1.Sum([]byte(strings.ToLower(strings.TrimSpace(prompt))))
	shortHash := fmt.Sprintf("%x", hash[:4])
	return fmt.Sprintf("%s_%s_%s%s", cleanPrefix, cleanPrompt, shortHash, ext)
}

func resourceSafeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "asset"
	}

	builder := strings.Builder{}
	lastUnderscore := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			builder.WriteRune('_')
			lastUnderscore = true
		}
	}

	result := strings.Trim(builder.String(), "_")
	if result == "" {
		result = "asset"
	}
	if result[0] >= '0' && result[0] <= '9' {
		result = "asset_" + result
	}
	return result
}

func normalizedLookupKey(value string) string {
	return strings.TrimSpace(strings.ToLower(value))
}

func sanitizeProviderError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "unknown error"
	}
	msg = sensitiveValueRegex.ReplaceAllString(msg, "$1:[redacted]")
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.ReplaceAll(msg, "\r", " ")
	msg = strings.TrimSpace(msg)
	if len(msg) > 220 {
		msg = msg[:220] + "..."
	}
	return msg
}

func dedupeWarnings(warnings []string) []string {
	seen := make(map[string]struct{})
	result := []string{}
	for _, warning := range warnings {
		clean := strings.TrimSpace(warning)
		if clean == "" {
			continue
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		result = append(result, clean)
	}
	return result
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
