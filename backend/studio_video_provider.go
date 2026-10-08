package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type studioVideoOptions struct {
	SourceID        string `json:"sourceId"`
	ModelID         string `json:"modelId"`
	DurationSeconds int    `json:"durationSeconds"`
	Resolution      string `json:"resolution"`
	AspectRatio     string `json:"aspectRatio"`
}

type studioVideoModelCapability struct {
	SourceID               string             `json:"sourceId"`
	InputImageUSD          float64            `json:"inputImageUSD,omitempty"`
	DurationsByResolution  map[string][]int   `json:"durationsByResolution,omitempty"`
	PricingURL             string             `json:"pricingURL,omitempty"`
	PricingAsOf            string             `json:"pricingAsOf,omitempty"`
	ID                     string             `json:"id"`
	Name                   string             `json:"name"`
	Durations              []int              `json:"durations,omitempty"`
	MinDurationSeconds     int                `json:"minDurationSeconds"`
	MaxDurationSeconds     int                `json:"maxDurationSeconds"`
	Resolutions            []string           `json:"resolutions"`
	AspectRatios           []string           `json:"aspectRatios"`
	DefaultDurationSeconds int                `json:"defaultDurationSeconds"`
	DefaultResolution      string             `json:"defaultResolution"`
	PricesPerSecondUSD     map[string]float64 `json:"pricesPerSecondUSD"`
	ImageInputPriceUSD     float64            `json:"imageInputPriceUSD,omitempty"`
	SupportsTextToVideo    bool               `json:"supportsTextToVideo"`
}

type studioVideoSourceCapability struct {
	ID             string                       `json:"id"`
	Name           string                       `json:"name"`
	Provider       string                       `json:"provider"`
	Experimental   bool                         `json:"experimental,omitempty"`
	Connected      bool                         `json:"connected"`
	RequiresAPIKey bool                         `json:"requiresApiKey"`
	Notice         string                       `json:"notice,omitempty"`
	Models         []studioVideoModelCapability `json:"models"`
}

type studioVideoCatalogue struct {
	Sources []studioVideoSourceCapability `json:"sources"`
}

func studioVideoCapabilities() studioVideoCatalogue {
	grokRatios := []string{"16:9", "9:16", "1:1", "4:3", "3:4", "3:2", "2:3"}
	latest := studioVideoModelCapability{ID: "grok-imagine-video-1.5", Name: "Grok Imagine Video 1.5", MinDurationSeconds: 1, MaxDurationSeconds: 15, Resolutions: []string{"480p", "720p", "1080p"}, AspectRatios: grokRatios, DefaultDurationSeconds: 5, DefaultResolution: "480p", PricesPerSecondUSD: map[string]float64{"480p": .08, "720p": .14, "1080p": .25}, ImageInputPriceUSD: .01, SupportsTextToVideo: true}
	older := studioVideoModelCapability{ID: "grok-imagine-video", Name: "Grok Imagine Video", MinDurationSeconds: 1, MaxDurationSeconds: 15, Resolutions: []string{"480p", "720p"}, AspectRatios: grokRatios, DefaultDurationSeconds: 5, DefaultResolution: "480p", PricesPerSecondUSD: map[string]float64{"480p": .05, "720p": .07}, ImageInputPriceUSD: .002, SupportsTextToVideo: true}
	veo := func(id, name string, low, high float64) studioVideoModelCapability {
		return studioVideoModelCapability{SourceID: "veo-api", PricingURL: "https://ai.google.dev/gemini-api/docs/pricing", PricingAsOf: "2026-10-01", DurationsByResolution: map[string][]int{"720p": {4, 6, 8}, "1080p": {8}}, ID: id, Name: name, Durations: []int{4, 6, 8}, MinDurationSeconds: 4, MaxDurationSeconds: 8, Resolutions: []string{"720p", "1080p"}, AspectRatios: []string{"16:9", "9:16"}, DefaultDurationSeconds: 4, DefaultResolution: "720p", PricesPerSecondUSD: map[string]float64{"720p": low, "1080p": high}, SupportsTextToVideo: true}
	}
	latest.SourceID = "xai-api"
	latest.InputImageUSD = latest.ImageInputPriceUSD
	older.SourceID = "xai-api"
	older.InputImageUSD = older.ImageInputPriceUSD
	latest.PricingURL = "https://docs.x.ai/developers/models/grok-imagine-video-1.5"
	latest.PricingAsOf = "2026-10-01"
	older.PricingURL = "https://docs.x.ai/developers/models/grok-imagine-video"
	older.PricingAsOf = "2026-10-01"
	subscriptionModel := func(model studioVideoModelCapability) studioVideoModelCapability {
		model.SourceID = "xai-subscription"
		model.InputImageUSD = 0
		model.PricesPerSecondUSD = map[string]float64{}
		model.ImageInputPriceUSD = 0
		model.PricingURL = "https://grok.com"
		model.PricingAsOf = ""
		return model
	}
	_, subscriptionConnected := findStudioVideoSubscription()
	return studioVideoCatalogue{Sources: []studioVideoSourceCapability{
		{ID: "xai-api", Name: "SpaceXAI API", Provider: "SpaceXAI", RequiresAPIKey: true, Connected: studioVideoEnvironmentKey("xai-api") != "", Models: []studioVideoModelCapability{latest, older}},
		{ID: "veo-api", Name: "Google API", Provider: "Google", RequiresAPIKey: true, Connected: studioVideoEnvironmentKey("veo-api") != "", Models: []studioVideoModelCapability{veo("veo-3.1-lite-generate-preview", "Veo 3.1 Lite", .05, .08), veo("veo-3.1-fast-generate-preview", "Veo 3.1 Fast", .10, .12), veo("veo-3.1-generate-preview", "Veo 3.1 Standard", .40, .40)}},
		{ID: "xai-subscription", Name: "Grok subscription", Provider: "SpaceXAI", Connected: subscriptionConnected, Notice: "Video access and limits depend on your Grok account. Account allowance or purchased credits may apply.", Models: []studioVideoModelCapability{subscriptionModel(older), subscriptionModel(latest)}},
	}}
}

func normalizeStudioVideoOptions(options studioVideoOptions) (studioVideoOptions, error) {
	options.SourceID = strings.TrimSpace(options.SourceID)
	options.ModelID = strings.TrimSpace(options.ModelID)
	options.Resolution = strings.ToLower(strings.TrimSpace(options.Resolution))
	options.AspectRatio = strings.TrimSpace(options.AspectRatio)
	if options.SourceID == "" {
		options.SourceID = "xai-api"
	}
	// Earlier Studio checkpoints identified the xAI provider without its credential source.
	if options.SourceID == "xai" {
		options.SourceID = "xai-api"
		if options.ModelID == "" {
			options.ModelID = "grok-imagine-video"
		}
		if options.Resolution == "" {
			options.Resolution = "720p"
		}
	}
	for _, source := range studioVideoCapabilities().Sources {
		if source.ID != options.SourceID {
			continue
		}
		if options.ModelID == "" {
			options.ModelID = source.Models[0].ID
		}
		for _, model := range source.Models {
			if model.ID != options.ModelID {
				continue
			}
			if options.DurationSeconds == 0 {
				options.DurationSeconds = model.DefaultDurationSeconds
			}
			if options.Resolution == "" {
				options.Resolution = model.DefaultResolution
			}
			if options.AspectRatio == "" {
				options.AspectRatio = "16:9"
			}
			if options.DurationSeconds < model.MinDurationSeconds || options.DurationSeconds > model.MaxDurationSeconds {
				return options, fmt.Errorf("Choose a video length between %d and %d seconds.", model.MinDurationSeconds, model.MaxDurationSeconds)
			}
			if len(model.Durations) > 0 {
				valid := false
				for _, duration := range model.Durations {
					valid = valid || duration == options.DurationSeconds
				}
				if !valid {
					return options, errors.New("Veo video length must be 4, 6, or 8 seconds.")
				}
			}
			if !studioVideoStringIn(options.Resolution, model.Resolutions) {
				return options, errors.New("Unsupported resolution for this video model.")
			}
			if !studioVideoStringIn(options.AspectRatio, model.AspectRatios) {
				return options, errors.New("Unsupported aspect ratio for this video model.")
			}
			if options.SourceID == "veo-api" && options.Resolution == "1080p" && options.DurationSeconds != 8 {
				return options, errors.New("Veo 1080p video requires an 8-second length.")
			}
			return options, nil
		}
		return options, errors.New("Unsupported model for this video source.")
	}
	return options, errors.New("Unsupported video source.")
}
func studioVideoStringIn(value string, values []string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
func estimateStudioVideoCost(options studioVideoOptions, hasImage bool) float64 {
	if options.SourceID == "xai-subscription" {
		return 0
	}
	for _, s := range studioVideoCapabilities().Sources {
		if s.ID == options.SourceID {
			for _, m := range s.Models {
				if m.ID == options.ModelID {
					cost := float64(options.DurationSeconds) * m.PricesPerSecondUSD[options.Resolution]
					if hasImage {
						cost += m.ImageInputPriceUSD
					}
					return cost
				}
			}
		}
	}
	return 0
}

func findStudioVideoSubscription() (xAICredential, bool) {
	for _, path := range xAIMediaAuthFileCandidates() {
		credential, ok, err := readXAIStoredCredentialWithSubscription(path, true)
		if err == nil && ok && credential.Kind == "subscription" {
			return credential, true
		}
	}
	return xAICredential{}, false
}
func resolveStudioVideoSubscription(ctx context.Context, forceRefresh ...bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	xAIAuthMu.Lock()
	defer xAIAuthMu.Unlock()
	credential, ok := findStudioVideoSubscription()
	if !ok {
		return "", errors.New("Connect Grok in OpenCode to generate subscription video.")
	}
	expiresSoon := credential.Expires > 0 && credential.Expires <= time.Now().Add(2*time.Minute).UnixMilli()
	if expiresSoon || (len(forceRefresh) > 0 && forceRefresh[0]) {
		if credential.Refresh == "" {
			return "", errors.New("Reconnect Grok in OpenCode to refresh the subscription.")
		}
		refreshed, err := refreshXAICredentialForVideo(credential, xAIOAuthTokenURL, http.DefaultClient, ctx)
		if err != nil {
			return "", err
		}
		credential = refreshed
	}
	return credential.Bearer, nil
}

func generateStudioSelectedVideo(ctx context.Context, options studioVideoOptions, key, prompt string, images []VeoImageInput) (*VeoGenerationResponse, error) {
	options, err := normalizeStudioVideoOptions(options)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(prompt) == "" || len(prompt) > 4000 {
		return nil, errors.New("Enter a video prompt up to 4,000 characters.")
	}
	if len(images) > 1 {
		return nil, errors.New("Studio video supports one starting image.")
	}
	normalizedImages := make([]VeoImageInput, 0, len(images))
	for _, image := range images {
		if strings.HasPrefix(image.Data, "data:") {
			parts := strings.SplitN(image.Data, ",", 2)
			if len(parts) != 2 {
				return nil, errors.New("Invalid video starting image.")
			}
			image.MimeType = strings.TrimSuffix(strings.TrimPrefix(parts[0], "data:"), ";base64")
			image.Data = parts[1]
		}
		normalizedImages = append(normalizedImages, image)
	}
	request := VeoGenerationRequest{Prompt: prompt, Images: normalizedImages, ModelID: options.ModelID, DurationSeconds: options.DurationSeconds, Resolution: options.Resolution, AspectRatio: options.AspectRatio}
	if options.SourceID == "veo-api" {
		request.GeminiKey = key
		return startVeoVideoGeneration(request, ctx)
	}
	if options.SourceID == "xai-subscription" {
		if strings.TrimSpace(key) != "" {
			return nil, errors.New("Subscription video uses the connected Grok account.")
		}
		key, err = resolveStudioVideoSubscription(ctx)
		if err != nil {
			return nil, err
		}
	}
	request.XaiKey = key
	result, err := startGrokImagineVideoGeneration(request, ctx)
	return result, studioSelectedVideoError(err, "xAI")
}
func pollStudioSelectedVideo(ctx context.Context, options studioVideoOptions, key, operationID string) (*VeoPollResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.SourceID == "veo-api" {
		return pollVeoOperation(operationID, key, ctx)
	}
	if options.SourceID == "xai-subscription" {
		var err error
		key, err = resolveStudioVideoSubscription(ctx)
		if err != nil {
			return nil, err
		}
	}
	result, err := pollGrokImagineVideoOperation(operationID, key, ctx)
	// Refreshing a rejected credential can repeat a GET without submitting another render.
	if err != nil && options.SourceID == "xai-subscription" && isXAIUnauthorized(err) {
		if refreshed, refreshErr := resolveStudioVideoSubscription(ctx, true); refreshErr == nil {
			result, err = pollGrokImagineVideoOperation(operationID, refreshed, ctx)
		}
	}
	if result != nil && result.Error != "" {
		result.Error = "The provider could not finish this video. Check its moderation or account limits."
	}
	return result, studioSelectedVideoError(err, "xAI")
}
func downloadStudioSelectedVideo(ctx context.Context, options studioVideoOptions, key, videoURL string) (string, error) {
	var body []byte
	var mimeType string
	var err error
	if options.SourceID == "veo-api" {
		body, mimeType, err = downloadStudioVeoVideo(ctx, videoURL, key)
	} else {
		if options.SourceID == "xai-subscription" {
			key, err = resolveStudioVideoSubscription(ctx)
			if err != nil {
				return "", err
			}
		}
		body, mimeType, err = downloadXAIMedia(videoURL, key, "video/mp4", ctx)
	}
	if err != nil {
		return "", studioSelectedVideoError(err, "Video provider")
	}
	return fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(body)), nil
}
func studioSelectedVideoError(err error, provider string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var apiErr *xAIAPIError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("%s video request failed (HTTP %d). Check account permissions and available credits.", provider, apiErr.Status)
	}
	return fmt.Errorf("%s video request could not finish. Check the connection and account permissions.", provider)
}

func downloadStudioVeoVideo(ctx context.Context, videoURL, key string) ([]byte, string, error) {
	parsed, err := url.Parse(videoURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return nil, "", errors.New("Invalid provider video URL.")
	}
	ctx, cancel := context.WithTimeout(ctx, xAIMediaDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, videoURL, nil)
	if err != nil {
		return nil, "", errors.New("Invalid provider video URL.")
	}
	// API keys accompany only Google's documented video download host.
	if parsed.Hostname() == "generativelanguage.googleapis.com" {
		req.Header.Set("x-goog-api-key", key)
	}
	client := *http.DefaultClient
	client.CheckRedirect = func(next *http.Request, previous []*http.Request) error {
		if len(previous) >= 10 {
			return errors.New("Too many video redirects.")
		}
		if next.URL.Scheme != "https" || next.URL.Hostname() != parsed.Hostname() {
			next.Header.Del("x-goog-api-key")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		if e, ok := err.(*url.Error); ok {
			err = e.Err
		}
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, xAIMediaDownloadLimit+1))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("Video download failed (HTTP %d).", resp.StatusCode)
	}
	if len(body) == 0 || len(body) > xAIMediaDownloadLimit {
		return nil, "", errors.New("Video download is empty or exceeds 80 MB.")
	}
	contentType := strings.Split(resp.Header.Get("Content-Type"), ";")[0]
	if !strings.HasPrefix(contentType, "video/") && contentType != "application/octet-stream" {
		return nil, "", errors.New("Downloaded file is not a video.")
	}
	if contentType == "application/octet-stream" {
		contentType = "video/mp4"
	}
	return body, contentType, nil
}

// Bound JSON from the SDK while retaining the injected client used by provider tests.
type studioVideoBoundedTransport struct{ base http.RoundTripper }
type studioVideoBoundedBody struct {
	io.Reader
	io.Closer
}

func (t studioVideoBoundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err == nil && response.Body != nil {
		response.Body = studioVideoBoundedBody{Reader: io.LimitReader(response.Body, xAIVideoResponseLimit+1), Closer: response.Body}
	}
	return response, err
}
func studioVideoProviderClient() *http.Client {
	client := *http.DefaultClient
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = studioVideoBoundedTransport{base: transport}
	return &client
}
