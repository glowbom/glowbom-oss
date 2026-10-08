package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"net/http"
	"strings"
	"time"
)

const (
	// xAI renders a short clip in a few minutes and can fail while storing it.
	studioVideoPollTimeout = 8 * time.Minute
)

func studioVideosHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	assets, err := listStudioAssets("video", studioVideoLimit)
	if err != nil {
		http.Error(w, "Could not load Studio videos.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"videos": assets})
}

func studioVideoContentHandler(w http.ResponseWriter, r *http.Request) {
	writeStudioAssetContent(w, r, "video", "video/mp4")
}

func studioVideoGenerateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		GenerationID    string `json:"generationId,omitempty"`
		Prompt          string `json:"prompt"`
		SourceID        string `json:"sourceId"`
		ModelID         string `json:"modelId"`
		Resolution      string `json:"resolution"`
		AspectRatio     string `json:"aspectRatio"`
		DurationSeconds int    `json:"durationSeconds"`
		ReferenceID     string `json:"referenceId"`
		ReferenceImage  string `json:"referenceImage"`
		APIKey          string `json:"apiKey"`
		UseSavedKey     bool   `json:"useSavedKey"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 12<<20)).Decode(&request) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.Prompt == "" || len(request.Prompt) > 4000 {
		http.Error(w, "Enter a video prompt up to 4,000 characters.", http.StatusBadRequest)
		return
	}
	if request.UseSavedKey && !authorizeVoiceKey(w, r) {
		return
	}
	legacySource := request.SourceID == ""
	// Preserve the implicit source used by earlier clients; new clients select one explicitly.
	if request.SourceID == "" {
		if grokSubscriptionMediaEnabled() {
			if _, connected := findStudioVideoSubscription(); connected {
				request.SourceID = "xai-subscription"
			}

		}
	}
	options, err := normalizeStudioVideoOptions(studioVideoOptions{SourceID: request.SourceID, ModelID: request.ModelID, Resolution: request.Resolution, AspectRatio: request.AspectRatio, DurationSeconds: request.DurationSeconds})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if options.SourceID == "xai-subscription" && !authorizeStudioGeneration(w, r, http.MethodPost) {
		return
	}
	key := ""
	progress, ok := beginStudioProgress(w, r, request.GenerationID, "video")
	if !ok {
		return
	}
	defer progress.finish()
	ctx := progress.context(r.Context())
	if err := progress.configure(request.Prompt, options.AspectRatio, request.ReferenceID, options.SourceID, options.DurationSeconds); err != nil {
		http.Error(w, "Could not save this request. No video generation was started.", http.StatusInternalServerError)
		return
	}
	if err := progress.store.edit(progress.id, func(entry *studioProgressEntry) {
		entry.state.ModelID = options.ModelID
		entry.state.Resolution = options.Resolution
		entry.state.EstimatedCostUSD = estimateStudioVideoCost(options, request.ReferenceID != "" || request.ReferenceImage != "")
		entry.videoKey = key
	}); err != nil {
		http.Error(w, "Could not save the video settings.", http.StatusInternalServerError)
		return
	}
	reference, sourceAssetID, err := persistStudioReference(request.ReferenceID, request.ReferenceImage, options.SourceID)
	if err != nil {
		progress.failed(ctx, "Could not load the starting still. Your prompt is saved.")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := progress.reference(sourceAssetID, nil); err != nil {
		http.Error(w, "Could not save the starting still. No generation was started.", http.StatusInternalServerError)
		return
	}
	var images []VeoImageInput
	if reference != "" {
		images = []VeoImageInput{{Data: reference, MimeType: "image/jpeg"}}
	}
	key, err = resolveStudioVideoKey(ctx, options.SourceID, request.APIKey, request.UseSavedKey)
	if err != nil {
		if legacySource && !grokSubscriptionMediaEnabled() {
			if _, connected := findStudioVideoSubscription(); connected {
				progress.failed(ctx, "Choose a video API source or Grok subscription.")
				writeStudioProviderError(w, errGrokSubscriptionMediaDisabled, "Connect a video API key.")
				return
			}
		}
		progress.failed(ctx, "Add the selected provider API key to generate this video.")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = progress.store.edit(progress.id, func(entry *studioProgressEntry) { entry.videoKey = key })
	progress.stage("generating")
	videoURL, err := startTrackedStudioSelectedVideo(ctx, progress, options, key, request.Prompt, images)
	if err != nil {
		progress.failed(ctx, "Could not finish this video. Your request is saved; check it again before generating another clip.")
		writeStudioProviderError(w, err, "Could not finish this video. Check the saved request before generating another clip.")
		return
	}
	state := studioProgressState{Prompt: request.Prompt, AspectRatio: options.AspectRatio, DurationSeconds: options.DurationSeconds, ReferenceID: sourceAssetID, SourceID: options.SourceID, ModelID: options.ModelID, Resolution: options.Resolution}
	asset, err := downloadAndSaveStudioSelectedVideo(ctx, progress, videoURL, state, key)
	if err != nil {
		progress.failed(ctx, "Your video was created, but could not be saved here. Retry downloading this request.")
		writeStudioProviderError(w, err, "Your video was created, but could not be saved here. Retry downloading this request.")
		return
	}
	writeJSON(w, map[string]any{"video": asset})
}

func persistStudioReference(id, inline string, sources ...string) (string, string, error) {
	dataURI, err := resolveStudioReferenceImage(id, inline)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(id) != "" {
		data, _, err := decodeBase64Payload(dataURI, "image/png")
		if err != nil {
			return "", "", errors.New("The saved image could not be opened as a reference.")
		}
		source := ""
		if len(sources) > 0 {
			source = sources[0]
		}
		data, err = normalizeSavedImageReference(data, source)
		if err != nil {
			return "", "", err
		}
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data), strings.TrimSpace(id), nil
	}
	if dataURI == "" {
		return "", "", nil
	}
	record, err := saveStudioAsset(studioSaveOptions{
		Prompt: "Uploaded reference", DataURI: dataURI, MediaType: "image",
		Source: "Studio upload", AssetType: "reference", SourceType: "uploaded",
	})
	if err != nil {
		return dataURI, "", nil
	}
	return dataURI, record.ID, nil
}

// Resize a bounded derivative for generation while retaining the saved original.
func normalizeSavedImageReference(data []byte, source string) ([]byte, error) {
	if len(data) == 0 || len(data) > studioGeneratedImageMaxBytes {
		return nil, errors.New("Choose a saved image smaller than 24 MB as the reference.")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 || config.Width > 6144 || config.Height > 6144 || int64(config.Width)*int64(config.Height) > 20_000_000 {
		return nil, errors.New("Choose a saved PNG or JPEG image within the supported dimensions.")
	}
	return normalizePreviousPrototypeImage(data, source)
}

func createStudioFirstFrame(prompt, aspectRatio string, contexts ...context.Context) (string, *studioImageSummary, error) {
	ctx, cancel := context.WithTimeout(imageRequestContext(contexts), 3*time.Minute)
	defer cancel()
	dataURI, err := withXAIBearerContext(ctx, func(bearer string) (string, error) {
		return callGrokImageGeneration(prompt, bearer, aspectRatio, ctx)
	})
	if err != nil {
		return "", nil, err
	}
	record, err := saveStudioAsset(studioSaveOptions{
		Prompt: prompt, DataURI: dataURI, MediaType: "image", Source: xAIImageSourceLabel, AspectRatio: aspectRatio,
	})
	if err != nil {
		// The frame still works for this video even when Studio cannot keep it.
		fmt.Printf("[GROK VIDEO] Could not save the generated first frame: %v\n", err)
		return dataURI, nil, nil
	}
	summary := summarizeStudioRecord(record)
	return dataURI, &summary, nil
}

// studioVideoFailure carries a message that is safe to show in Studio.
type studioVideoFailure struct {
	message   string
	retryable bool
	terminal  bool
}

func (e *studioVideoFailure) Error() string { return e.message }

func startTrackedStudioSelectedVideo(ctx context.Context, progress studioProgress, options studioVideoOptions, key, prompt string, images []VeoImageInput) (string, error) {
	start, err := generateStudioSelectedVideo(ctx, options, key, prompt, images)
	if err != nil {
		return "", err
	}
	if err := progress.operation(start.OperationID); err != nil {
		return "", errors.New("Could not save the video request. Check Studio before starting another clip.")
	}
	poll, err := waitForStudioSelectedVideo(ctx, options, key, start.OperationID)
	if err != nil {
		var failure *studioVideoFailure
		if errors.As(err, &failure) && failure.terminal {
			_ = progress.store.edit(progress.id, func(entry *studioProgressEntry) { entry.state.CanResume = false })
		}
		return "", err
	}
	return checkpointStudioVideoOutput(progress, poll)
}

func checkpointStudioVideoOutput(progress studioProgress, poll *VeoPollResponse) (string, error) {
	videoURL := strings.TrimSpace(poll.VideoURL)
	if videoURL == "" && poll.VideoAsset != nil {
		videoURL = strings.TrimSpace(poll.VideoAsset.URI)
	}
	if videoURL == "" {
		return "", &studioVideoFailure{message: "The provider finished without a video file."}
	}
	if err := progress.output(videoURL); err != nil {
		return "", errors.New("Could not save the completed video request. Retry downloading from Studio.")
	}
	if poll.DurationSeconds > 0 {
		if err := progress.store.edit(progress.id, func(entry *studioProgressEntry) { entry.state.ActualDurationSeconds = poll.DurationSeconds }); err != nil {
			return "", errors.New("Could not save the video duration.")
		}
	}
	return videoURL, nil
}

func waitForStudioSelectedVideo(parent context.Context, options studioVideoOptions, key, operationID string) (*VeoPollResponse, error) {
	ctx, cancel := context.WithTimeout(parent, studioVideoPollTimeout)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, studioVideoWaitError(err)
		}
		poll, err := pollStudioSelectedVideo(ctx, options, key, operationID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, studioVideoWaitError(ctx.Err())
			}
			return nil, err
		}
		if poll.Done {
			if poll.Status == "failed" {
				return nil, studioVideoPollFailure(poll.Error)
			}
			return poll, nil
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, studioVideoWaitError(ctx.Err())
		case <-timer.C:
		}
	}
}

func studioVideoWaitError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &studioVideoFailure{message: "The provider is still rendering this clip. Your request is saved; check it again to continue."}
	}
	return err
}

func studioVideoOptionsFromState(state studioProgressState) (studioVideoOptions, error) {
	options := studioVideoOptions{SourceID: state.SourceID, ModelID: state.ModelID, Resolution: state.Resolution, AspectRatio: state.AspectRatio, DurationSeconds: state.DurationSeconds}
	if options.SourceID == "xai" {
		if studioVideoEnvironmentKey("xai-api") == "" {
			if _, connected := findStudioVideoSubscription(); connected {
				options.SourceID = "xai-subscription"
			}
		}
		if options.ModelID == "" {
			options.ModelID = "grok-imagine-video"
		}
		if options.Resolution == "" {
			options.Resolution = "720p"
		}
	}
	return normalizeStudioVideoOptions(options)
}

func downloadAndSaveStudioSelectedVideo(ctx context.Context, progress studioProgress, videoURL string, state studioProgressState, key string) (studioImageSummary, error) {
	if err := ctx.Err(); err != nil {
		return studioImageSummary{}, err
	}
	options, err := studioVideoOptionsFromState(state)
	if err != nil {
		return studioImageSummary{}, err
	}
	progress.stage("downloading")
	dataURI, err := downloadStudioSelectedVideo(ctx, options, key, videoURL)
	if err != nil {
		return studioImageSummary{}, err
	}
	if err := ctx.Err(); err != nil {
		return studioImageSummary{}, err
	}
	progress.stage("saving")
	if progress.id != "" {
		state.ActualDurationSeconds = progress.store.read(progress.id).ActualDurationSeconds
	}
	record, err := saveStudioAsset(studioSaveOptions{Prompt: state.Prompt, DataURI: dataURI, MediaType: "video", Source: studioVideoSourceLabel(options), SourceID: options.SourceID, Model: options.ModelID, Resolution: options.Resolution, RequestedDurationSeconds: float64(options.DurationSeconds), GenerationID: progress.id, AspectRatio: options.AspectRatio, Duration: state.ActualDurationSeconds, SourceAssetID: state.ReferenceID})
	if err != nil {
		return studioImageSummary{}, err
	}
	asset := summarizeStudioRecord(record)
	_ = progress.completed(asset)
	return asset, nil
}
func studioVideoSourceLabel(options studioVideoOptions) string {
	if options.SourceID == "veo-api" {
		return "Google Veo"
	}
	if options.SourceID == "xai-subscription" {
		return "Grok subscription"
	}
	return xAIVideoSourceLabel
}
func resumeStudioVideo(ctx context.Context, progress studioProgress, entry studioProgressEntry) (studioProgressState, error) {
	options, err := studioVideoOptionsFromState(entry.state)
	if err != nil {
		return studioProgressState{}, err
	}
	key, err := resolveStudioVideoKey(ctx, options.SourceID, entry.videoKey, false)
	if err != nil {
		return studioProgressState{}, err
	}
	videoURL := entry.videoURL
	if entry.operationID != "" {
		poll, pollErr := waitForStudioSelectedVideo(ctx, options, key, entry.operationID)
		if pollErr == nil {
			videoURL, pollErr = checkpointStudioVideoOutput(progress, poll)
		}
		if pollErr != nil && (videoURL == "" || ctx.Err() != nil) {
			return studioProgressState{}, pollErr
		}
	}
	if videoURL == "" {
		return studioProgressState{}, errors.New("The saved request has no video file yet.")
	}
	asset, err := downloadAndSaveStudioSelectedVideo(ctx, progress, videoURL, entry.state, key)
	if err != nil {
		return studioProgressState{}, err
	}
	entry.state.Stage, entry.state.Asset = "complete", &asset
	return entry.state, nil
}

func studioVideoPollFailure(message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return &studioVideoFailure{message: "The provider could not finish this video. Your prompt is saved.", terminal: true}
	}
	lowered := strings.ToLower(message)
	retryable := strings.Contains(lowered, "please retry") ||
		strings.Contains(lowered, "temporarily") ||
		strings.Contains(lowered, "service_unavailable")
	// URLs in provider messages can contain signed download credentials.
	message = sanitizeProviderError(errors.New(studioProviderURL.ReplaceAllString(message, "[private URL]")))
	return &studioVideoFailure{message: "Video provider: " + message, retryable: retryable, terminal: true}
}
