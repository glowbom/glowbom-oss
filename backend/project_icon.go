package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const projectIconMaxBytes = 12 << 20

var projectIconSaveMu sync.Mutex
var projectIconAuthFileCandidates = xAIAuthFileCandidates

type projectIconSource struct {
	ID               string `json:"id"`
	Label            string `json:"label"`
	Model            string `json:"model"`
	AuthType         string `json:"authType"`
	Available        bool   `json:"available"`
	AvailabilityCode string `json:"availabilityCode,omitempty"`
}

type projectIconRequest struct {
	Path           string `json:"path"`
	Prompt         string `json:"prompt"`
	SourceID       string `json:"sourceId,omitempty"`
	APIKey         string `json:"apiKey,omitempty"`
	ImageSource    string `json:"imageSource,omitempty"`
	OpenAIKey      string `json:"openaiKey,omitempty"`
	GeminiKey      string `json:"geminiKey,omitempty"`
	XaiKey         string `json:"xaiKey,omitempty"`
	ReferenceImage string `json:"referenceImage,omitempty"`
}

// Existing callers keep their default behavior; interactive icon requests can cancel I/O.
func imageRequestContext(contexts []context.Context) context.Context {
	if len(contexts) > 0 && contexts[0] != nil {
		return contexts[0]
	}
	return context.Background()
}

func projectIconAPIKey(sourceID string) string {
	var envNames, providers []string
	switch sourceID {
	case "openai-api":
		envNames, providers = []string{"OPENAI_API_KEY"}, []string{"openai"}
	case "gemini-api":
		envNames, providers = []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, []string{"google", "gemini"}
	case "xai-api":
		envNames, providers = []string{"XAI_API_KEY"}, []string{"xai"}
	}
	for _, name := range envNames {
		if key := strings.TrimSpace(os.Getenv(name)); key != "" {
			return key
		}
	}
	for _, path := range projectIconAuthFileCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var stored map[string]json.RawMessage
		if json.Unmarshal(data, &stored) != nil {
			continue
		}
		for _, provider := range providers {
			var credential struct {
				Type string `json:"type"`
				Key  string `json:"key"`
			}
			if json.Unmarshal(stored[provider], &credential) != nil {
				continue
			}
			if credential.Type == "api" && strings.TrimSpace(credential.Key) != "" {
				return strings.TrimSpace(credential.Key)
			}
		}
	}
	return ""
}

func projectIconSubscription() (xAICredential, bool) {
	if !grokSubscriptionMediaEnabled() {
		return xAICredential{}, false
	}
	for _, path := range projectIconAuthFileCandidates() {
		credential, ok, err := readXAIStoredCredential(path)
		if err != nil || !ok || credential.Kind != "subscription" {
			continue
		}
		if credential.Expires > 0 && credential.Expires <= time.Now().Add(2*time.Minute).UnixMilli() && credential.Refresh == "" {
			continue
		}
		return credential, true
	}
	return xAICredential{}, false
}

func projectIconSources(contexts ...context.Context) []projectIconSource {
	_, connected := projectIconSubscription()
	chatGPTConnected, chatGPTCode := codexImageSourceAvailability(imageRequestContext(contexts))
	sources := []projectIconSource{
		{ID: "openai-subscription", Label: "ChatGPT", Model: openAIImageModelID, AuthType: "subscription", Available: chatGPTConnected, AvailabilityCode: chatGPTCode},
		{ID: "glowbom-api", Label: "Glowbom account", Model: "Flux", AuthType: "account", AvailabilityCode: "backend_auth_required"},
		{ID: "openai-api", Label: "OpenAI", Model: openAIImageModelID, AuthType: "api-key", Available: projectIconAPIKey("openai-api") != ""},
		{ID: "gemini-api", Label: "Google Gemini", Model: nanoBanana2ModelID, AuthType: "api-key", Available: projectIconAPIKey("gemini-api") != ""},
		{ID: "xai-api", Label: "xAI", Model: xAIImageModel, AuthType: "api-key", Available: projectIconAPIKey("xai-api") != ""},
	}
	if grokSubscriptionMediaEnabled() {
		sources = append([]projectIconSource{{ID: "xai-subscription", Label: "Grok Imagine", Model: xAIImageModel, AuthType: "subscription", Available: connected}}, sources...)
	}
	sort.SliceStable(sources, func(i, j int) bool { return sources[i].Available && !sources[j].Available })
	return sources
}

func openCodeIconSourcesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sources := projectIconSources(r.Context())
	if bridge := glowbomImageAccount; bridge != nil && bridge.token != "" && hasValidGlowbomServerToken(r, bridge.token) {
		for i := range sources {
			if sources[i].ID == "glowbom-api" {
				sources[i].Available, sources[i].AvailabilityCode = glowbomImagesAvailability(r.Context())
			}
		}
		sort.SliceStable(sources, func(i, j int) bool { return sources[i].Available && !sources[j].Available })
	}
	recommended := ""
	for _, source := range sources {
		if source.Available {
			recommended = source.ID
			break
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"sources": sources, "recommendedSource": recommended})
}

func resolveProjectIconSource(req projectIconRequest, contexts ...context.Context) (string, string, error) {
	id := strings.TrimSpace(req.SourceID)
	if id == "" {
		source := strings.ToLower(req.ImageSource)
		switch {
		case isOpenAIImageSource(source):
			id = "openai-api"
		case strings.Contains(source, "nano banana"):
			id = "gemini-api"
		case strings.Contains(source, "grok"):
			id = "xai-api"
		case strings.TrimSpace(req.OpenAIKey) != "":
			id = "openai-api"
		case strings.TrimSpace(req.GeminiKey) != "":
			id = "gemini-api"
		case strings.TrimSpace(req.XaiKey) != "":
			id = "xai-api"
		default:
			for _, source := range projectIconSources(contexts...) {
				if source.Available {
					id = source.ID
					break
				}
			}
		}
	}
	key := strings.TrimSpace(req.APIKey)
	switch id {
	case "glowbom-api":
		if glowbomImageAccount == nil {
			return "", "", glowbomImageError(errAccountCLIMissing)
		}
		return id, "", nil
	case "xai-subscription":
		if err := requireGrokSubscriptionMedia(); err != nil {
			return "", "", err
		}
		if _, ok := projectIconSubscription(); !ok {
			return "", "", errors.New("Connect your xAI subscription in OpenCode, or choose an API key source.")
		}
		return id, "", nil
	case "openai-subscription":
		if ok, code := codexImageSourceAvailability(imageRequestContext(contexts)); !ok {
			if code == "codex_transport" {
				return "", "", codexImageTransportError()
			}
			if strings.HasPrefix(code, "codex_") {
				return "", "", &codexAppServerImageFailure{Code: strings.TrimPrefix(code, "codex_")}
			}
			return "", "", errors.New("Connect your ChatGPT subscription in OpenCode or Codex, or choose an API key source.")
		}
		return id, "", nil
	case "openai-api":
		if key == "" {
			key = strings.TrimSpace(req.OpenAIKey)
		}
	case "gemini-api":
		if key == "" {
			key = strings.TrimSpace(req.GeminiKey)
		}
	case "xai-api":
		if key == "" {
			key = strings.TrimSpace(req.XaiKey)
		}
	default:
		return "", "", errors.New("Choose an available image source or enter an API key.")
	}
	if key == "" {
		key = projectIconAPIKey(id)
	}
	if key == "" {
		return "", "", errors.New("Enter an API key for the selected image source.")
	}
	return id, key, nil
}

func generateProjectIcon(ctx context.Context, sourceID, key, prompt, reference string) (string, string, error) {
	switch sourceID {
	case "openai-subscription":
		value, err := callCodexImageGeneration(ctx, prompt, reference, "1:1")
		return value, codexImageSourceLabel, err
	case "glowbom-api":
		value, err := callGlowbomImageGeneration(ctx, prompt, reference)
		return value, glowbomImageSourceLabel, err
	case "openai-api":
		if reference != "" {
			value, err := callOpenAIImageGenerationWithReference(prompt, reference, "1:1", "png", key, ctx)
			return value, openAIImageSourceLabel, err
		}
		value, err := callOpenAIImageGeneration(prompt, "1:1", "png", key, ctx)
		return value, openAIImageSourceLabel, err
	case "gemini-api":
		if reference != "" {
			value, err := callGeminiImageGenerationWithReference(prompt, reference, "1:1", "png", key, ctx)
			return value, "Glowbom Images (Nano Banana 2)", err
		}
		value, err := callGeminiImageGeneration(prompt, "1:1", "png", key, ctx)
		return value, "Glowbom Images (Nano Banana 2)", err
	case "xai-api", "xai-subscription":
		generate := func(bearer string) (string, error) {
			if reference != "" {
				return callGrokImageGenerationWithReference(prompt, "data:image/png;base64,"+reference, bearer, "1:1", ctx)
			}
			return callGrokImageGeneration(prompt, bearer, "1:1", ctx)
		}
		if sourceID == "xai-api" {
			value, err := generate(key)
			return value, xAIImageSourceLabel, err
		}
		credential, err := resolveProjectIconSubscription(ctx, false)
		if err != nil {
			return "", xAIImageSourceLabel, err
		}
		value, err := generate(credential.Bearer)
		if isXAIUnauthorized(err) {
			if refreshed, refreshErr := resolveProjectIconSubscription(ctx, true); refreshErr == nil {
				value, err = generate(refreshed.Bearer)
			}
		}
		return value, xAIImageSourceLabel, err
	}
	return "", "", errors.New("Unsupported image source")
}

func resolveProjectIconSubscription(ctx context.Context, forceRefresh bool) (xAICredential, error) {
	if err := requireGrokSubscriptionMedia(); err != nil {
		return xAICredential{}, err
	}
	xAIAuthMu.Lock()
	defer xAIAuthMu.Unlock()
	credential, ok := projectIconSubscription()
	if !ok {
		return xAICredential{}, errors.New("Reconnect xAI in OpenCode.")
	}
	expiresSoon := credential.Expires > 0 && credential.Expires <= time.Now().Add(2*time.Minute).UnixMilli()
	if !forceRefresh && !expiresSoon {
		return credential, nil
	}
	if credential.Refresh == "" {
		return xAICredential{}, errors.New("Reconnect xAI in OpenCode.")
	}
	refreshed, err := refreshXAICredential(credential, xAIOAuthTokenURL, http.DefaultClient, ctx)
	if err != nil {
		return xAICredential{}, err
	}
	if refreshed.Kind != "subscription" {
		return xAICredential{}, errors.New("Reconnect the selected xAI subscription in OpenCode.")
	}
	return refreshed, nil
}

func openProjectIconRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("Choose an absolute project folder path.")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.New("Could not open the project folder.")
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, errors.New("Could not open the project folder.")
	}
	return root, nil
}

func readProjectIcon(root *os.Root) ([]byte, error) {
	info, err := root.Lstat("icon.png")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > projectIconMaxBytes {
		return nil, errors.New("The project icon must be an image file smaller than 12 MB.")
	}
	file, err := root.Open("icon.png")
	if err != nil {
		return nil, errors.New("Could not open the project icon.")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, projectIconMaxBytes+1))
	if err != nil || len(data) > projectIconMaxBytes {
		return nil, errors.New("Could not read the project icon.")
	}
	return data, nil
}

func normalizeProjectIcon(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > projectIconMaxBytes {
		return nil, errors.New("Use an image smaller than 12 MB.")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 {
		return nil, errors.New("The image source did not return a supported PNG or JPEG icon.")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("The image source returned an incomplete icon.")
	}
	var output bytes.Buffer
	if err := png.Encode(&output, decoded); err != nil {
		return nil, errors.New("Could not prepare the icon.")
	}
	if output.Len() > projectIconMaxBytes {
		return nil, errors.New("The generated icon is too large.")
	}
	return output.Bytes(), nil
}

// References stay in memory and are normalized before reaching the chosen provider.
func normalizeProjectIconReference(value string) (string, error) {
	payload := strings.TrimSpace(value)
	if strings.HasPrefix(payload, "data:") {
		header, data, ok := strings.Cut(payload, ",")
		if !ok || (header != "data:image/png;base64" && header != "data:image/jpeg;base64") {
			return "", errors.New("Use a PNG or JPEG reference photo.")
		}
		payload = data
	}
	if payload == "" || len(payload) > base64.StdEncoding.EncodedLen(projectIconMaxBytes) {
		return "", errors.New("Use a reference photo smaller than 12 MB.")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(data) == 0 || len(data) > projectIconMaxBytes {
		return "", errors.New("Use a valid PNG or JPEG reference photo smaller than 12 MB.")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 {
		return "", errors.New("The reference photo could not be opened. Choose a PNG or JPEG image.")
	}
	if config.Width > 4096 || config.Height > 4096 {
		return "", errors.New("Resize the reference photo to 4096 pixels or less on each side.")
	}
	normalized, err := normalizeProjectIcon(data)
	if err != nil {
		return "", errors.New("The reference photo could not be prepared. Choose a complete PNG or JPEG image smaller than 12 MB.")
	}
	return base64.StdEncoding.EncodeToString(normalized), nil
}

func saveProjectIcon(root *os.Root, previous, generated []byte, contexts ...context.Context) error {
	projectIconSaveMu.Lock()
	defer projectIconSaveMu.Unlock()
	current, err := readProjectIcon(root)
	if err != nil {
		return err
	}
	if sha256.Sum256(previous) != sha256.Sum256(current) {
		return errors.New("The project icon changed while generating. Try again to replace the new icon.")
	}
	// The temporary file and rename preserve the previous icon on a failed write.
	temp, err := os.CreateTemp(root.Name(), ".icon-*.png")
	if err != nil {
		return errors.New("Could not save the project icon.")
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(generated); err != nil {
		temp.Close()
		return errors.New("Could not save the project icon.")
	}
	if err := temp.Chmod(0644); err != nil {
		temp.Close()
		return errors.New("Could not save the project icon.")
	}
	if err := temp.Close(); err != nil {
		return errors.New("Could not save the project icon.")
	}
	rootInfo, rootErr := root.Stat(".")
	pathInfo, pathErr := os.Stat(root.Name())
	if rootErr != nil || pathErr != nil || !os.SameFile(rootInfo, pathInfo) {
		return errors.New("The project folder moved while generating. Reopen it and try again.")
	}
	if imageRequestContext(contexts).Err() != nil {
		return errors.New("Icon generation stopped. Your previous icon was kept.")
	}
	if err := os.Rename(temp.Name(), filepath.Join(root.Name(), "icon.png")); err != nil {
		return errors.New("Could not save the project icon.")
	}
	return nil
}

func openCodeGenerateIconHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req projectIconRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 18<<20)).Decode(&req); err != nil {
		http.Error(w, "Invalid icon request", http.StatusBadRequest)
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" || len(req.Prompt) > 4000 {
		http.Error(w, "Enter an icon description up to 4,000 characters.", http.StatusBadRequest)
		return
	}
	root, err := openProjectIconRoot(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer root.Close()
	previous, err := readProjectIcon(root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sourceID, key, err := resolveProjectIconSource(req, r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if sourceID == "glowbom-api" && !authorizeGlowbomImage(w, r) {
		return
	}
	reference := ""
	if req.ReferenceImage != "" {
		reference, err = normalizeProjectIconReference(req.ReferenceImage)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	timeout := 3 * time.Minute
	if sourceID == "openai-subscription" {
		timeout = codexImageTimeout
	}
	if sourceID == "glowbom-api" {
		if err := validateGlowbomImageReference(reference); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		timeout = glowbomImageTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	prompt := fmt.Sprintf("Generate a square app icon (1024x1024) for: %s. The icon should be simple, modern, and look great at small sizes. No text on the icon.", req.Prompt)
	if reference != "" {
		prompt += " Use the supplied photo as the visual reference for the person or object. Keep the subject recognizable while simplifying it into an app icon."
	}
	dataURI, sourceService, err := generateProjectIcon(ctx, sourceID, key, prompt, reference)
	if err != nil {
		// Provider errors can contain keys, request URLs, and personal data.
		message := "The image source could not generate an icon. Check its connection or API key and try again."
		if sourceID == "xai-subscription" {
			message = "Grok could not generate an icon. Check that your connected subscription includes image generation, or choose another source."
		}
		if sourceID == "openai-subscription" {
			message = "ChatGPT could not generate an icon. Check your subscription connection and image limits, or choose another source."
		}
		var codexFailure *codexImageFailure
		if errors.As(err, &codexFailure) {
			message = codexFailure.Error()
		}
		var appServerFailure *codexAppServerImageFailure
		if errors.As(err, &appServerFailure) {
			message = appServerFailure.Error()
		}
		var glowbomFailure *glowbomImageFailure
		if errors.As(err, &glowbomFailure) {
			message = glowbomFailure.Error()
		}
		if ctx.Err() != nil && sourceID != "glowbom-api" {
			message = "Icon generation stopped or timed out. Your previous icon was kept."
		}
		writeJSON(w, map[string]any{"success": false, "error": message})
		return
	}
	data, _, err := decodeBase64Payload(dataURI, "image/png")
	if err == nil {
		data, err = normalizeProjectIcon(data)
	}
	if err == nil {
		err = validateGeneratedImageAspect(data, "1:1")
	}
	if err != nil {
		var aspectFailure *imageAspectFailure
		if errors.As(err, &aspectFailure) {
			writeJSON(w, map[string]any{"success": false, "error": aspectFailure.Error() + " Your previous icon was kept."})
			return
		}
		writeJSON(w, map[string]any{"success": false, "error": "The image source returned an invalid icon. Your previous icon was kept."})
		return
	}
	if ctx.Err() != nil {
		writeJSON(w, map[string]any{"success": false, "error": "Icon generation stopped. Your previous icon was kept."})
		return
	}
	if err := saveProjectIcon(root, previous, data, ctx); err != nil {
		writeJSON(w, map[string]any{"success": false, "error": err.Error()})
		return
	}
	iconImage := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	result := map[string]any{"success": true, "iconPath": filepath.Join(root.Name(), "icon.png"), "sourceService": sourceService, "image": iconImage}
	size, _, _ := image.DecodeConfig(bytes.NewReader(data))
	asset, err := saveProjectIconStudioAsset(root, data, studioSaveOptions{
		NewGeneration: true, Prompt: "App icon: " + req.Prompt, DataURI: iconImage, MediaType: "image", Source: sourceService,
		AspectRatio: "1:1", Dimensions: &studioDimensions{Width: size.Width, Height: size.Height},
	})
	if err != nil {
		result["warning"] = "Your project icon was saved, but it could not be added to Studio. Check that Glowbom can write to its Studio folder."
	} else {
		result["studioAssetId"] = asset.ID
	}
	writeJSON(w, result)
}

func saveProjectIconStudioAsset(root *os.Root, data []byte, options studioSaveOptions) (studioImageRecord, error) {
	// The icon endpoint also supports an ordinary folder before project setup.
	// Keep those generations in Studio until a Glowbom project can be linked.
	if _, err := root.Stat("glowbom.json"); os.IsNotExist(err) {
		return saveStudioAsset(options)
	}
	return linkStudioProjectImage(root.Name(), "icon.png", data, options)
}

func openCodeProjectIconHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root, err := openProjectIconRoot(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer root.Close()
	data, err := readProjectIcon(root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(data) == 0 {
		writeJSON(w, map[string]any{"success": true, "exists": false})
		return
	}
	data, err = normalizeProjectIcon(data)
	if err != nil {
		writeJSON(w, map[string]any{"success": true, "exists": false})
		return
	}
	writeJSON(w, map[string]any{"success": true, "exists": true, "image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)})
}
