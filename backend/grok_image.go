package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	neturl "net/url"
	"path"
	"strings"
	"time"
)

const (
	xAIImageGenerationURL   = "https://api.x.ai/v1/images/generations"
	xAIImageEditURL         = "https://api.x.ai/v1/images/edits"
	xAIImageModel           = "grok-imagine-image-quality"
	xAIImageResolution      = "1k"
	xAIImageOutputFilename  = "grok-imagine-image-quality.jpg"
	xAIImageSourceLabel     = "Glowbom Images (Grok Imagine Image Quality)"
	xAIMediaDownloadTimeout = 2 * time.Minute
	xAIMediaDownloadLimit   = 80 << 20
)

type xAIAPIError struct {
	Status int
	Body   string
}

func (e *xAIAPIError) Error() string {
	return fmt.Sprintf("xAI API error (status %d): %s", e.Status, e.Body)
}

// callGrokImageGeneration calls xAI's Grok image generation API.
// Returns a base64 data URI on success.
func callGrokImageGeneration(prompt, apiKey, aspectRatio string, contexts ...context.Context) (string, error) {
	reqBody := map[string]interface{}{
		"model":        xAIImageModel,
		"prompt":       imageAspectPrompt(prompt, aspectRatio, false),
		"n":            1,
		"resolution":   xAIImageResolution,
		"image_format": "url",
	}

	if trimmedAspectRatio := strings.TrimSpace(aspectRatio); trimmedAspectRatio != "" {
		reqBody["aspect_ratio"] = trimmedAspectRatio
	}

	return callXAIImageAPI(xAIImageGenerationURL, xAIImageModel, reqBody, apiKey, false, aspectRatio, contexts...)
}

// callGrokImageGenerationWithReference sends a single reference image to Grok image edits API.
//
// PRIVACY NOTE: Reference image is only sent to xAI API and not cached.
func callGrokImageGenerationWithReference(prompt, referenceImageBase64, apiKey, aspectRatio string, contexts ...context.Context) (string, error) {
	referenceImageURL := ensureImageDataURI(referenceImageBase64, "image/jpeg")
	if strings.TrimSpace(referenceImageURL) == "" {
		return "", fmt.Errorf("reference image is required")
	}

	reqBody := map[string]interface{}{
		"model":      xAIImageModel,
		"prompt":     imageAspectPrompt(prompt, aspectRatio, true),
		"n":          1,
		"resolution": xAIImageResolution,
	}
	trimmedAspectRatio := strings.TrimSpace(aspectRatio)
	reference := map[string]interface{}{"type": "image_url", "url": referenceImageURL}
	switch trimmedAspectRatio {
	case "1:1", "16:9", "9:16":
		// The singular image edit mode follows its input shape. The images mode
		// accepts one reference and honors an explicit output aspect ratio.
		reqBody["images"] = []map[string]interface{}{reference}
		reqBody["aspect_ratio"] = trimmedAspectRatio
	default:
		reqBody["image"] = reference
		if trimmedAspectRatio == "" {
			trimmedAspectRatio = "auto"
		}
		reqBody["aspect_ratio"] = trimmedAspectRatio
	}

	return callXAIImageAPI(xAIImageEditURL, xAIImageModel, reqBody, apiKey, true, aspectRatio, contexts...)
}

func callXAIImageAPI(endpointURL, model string, reqBody map[string]interface{}, apiKey string, hasReference bool, aspectRatio string, contexts ...context.Context) (string, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(imageRequestContext(contexts), "POST", endpointURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	fmt.Printf("[DEBUG] Calling xAI %s image endpoint=%s (reference=%v, aspect_ratio=%q)\n",
		model, endpointURL, hasReference, strings.TrimSpace(aspectRatio))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call xAI API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 24<<20))
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", &xAIAPIError{Status: resp.StatusCode, Body: string(respBody)}
	}

	dataURI, err := parseXAIImageGenerationResponse(respBody, apiKey, contexts...)
	if err != nil {
		return "", err
	}
	return dataURI, nil
}

func ensureImageDataURI(value, defaultMimeType string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "data:image/") {
		return trimmed
	}
	return fmt.Sprintf("data:%s;base64,%s", defaultMimeType, trimmed)
}

func parseXAIImageGenerationResponse(respBody []byte, apiKey string, contexts ...context.Context) (string, error) {
	var result struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
		Images []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"images"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	type xAIImageResult struct {
		b64 string
		url string
	}
	candidates := make([]xAIImageResult, 0, len(result.Data)+len(result.Images))
	for _, item := range result.Data {
		candidates = append(candidates, xAIImageResult{b64: item.B64JSON, url: item.URL})
	}
	for _, item := range result.Images {
		candidates = append(candidates, xAIImageResult{b64: item.B64JSON, url: item.URL})
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no image data in response")
	}

	first := candidates[0]
	if strings.TrimSpace(first.b64) != "" {
		return fmt.Sprintf("data:image/jpeg;base64,%s", first.b64), nil
	}
	if strings.TrimSpace(first.url) != "" {
		return downloadImageURLAsDataURI(first.url, apiKey, contexts...)
	}
	return "", fmt.Errorf("image response did not include b64_json or url")
}

func downloadImageURLAsDataURI(imageURL, apiKey string, contexts ...context.Context) (string, error) {
	body, contentType, err := downloadXAIMedia(imageURL, apiKey, "image/jpeg", contexts...)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(body)), nil
}

func downloadXAIMedia(mediaURL, apiKey, fallbackMime string, contexts ...context.Context) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(imageRequestContext(contexts), xAIMediaDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", mediaURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create media download request")
	}
	// Only send the provider credential to an xAI-owned HTTPS download host.
	if req.URL.Scheme == "https" && (req.URL.Hostname() == "api.x.ai" || strings.HasSuffix(req.URL.Hostname(), ".x.ai")) && strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// url.Error includes temporary download credentials in its URL.
		if requestErr, ok := err.(*neturl.Error); ok {
			err = requestErr.Err
		}
		return nil, "", fmt.Errorf("failed to download media: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, xAIMediaDownloadLimit+1))
	if err != nil {
		return nil, "", fmt.Errorf("failed to read downloaded media: %w", err)
	}
	if len(body) > xAIMediaDownloadLimit {
		return nil, "", fmt.Errorf("downloaded media exceeds 80 MB")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, "", &xAIAPIError{Status: resp.StatusCode, Body: string(body)}
	}
	if len(body) == 0 {
		return nil, "", fmt.Errorf("downloaded media is empty")
	}

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		if parsedURL, err := neturl.Parse(mediaURL); err == nil {
			if ext := strings.ToLower(path.Ext(parsedURL.Path)); ext != "" {
				contentType = mime.TypeByExtension(ext)
			}
		}
	}
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	if contentType == "" {
		contentType = fallbackMime
	}
	if semicolonIdx := strings.Index(contentType, ";"); semicolonIdx != -1 {
		contentType = strings.TrimSpace(contentType[:semicolonIdx])
	}
	if strings.HasPrefix(fallbackMime, "video/") {
		mimeType := strings.ToLower(contentType)
		if !strings.HasPrefix(mimeType, "video/") && mimeType != "application/octet-stream" {
			return nil, "", fmt.Errorf("downloaded file is not a video")
		}
		trimmed := bytes.TrimSpace(body)
		sniffed := http.DetectContentType(body)
		if strings.HasPrefix(sniffed, "text/html") || strings.HasPrefix(sniffed, "text/xml") ||
			(len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')) {
			return nil, "", fmt.Errorf("downloaded file contains an error response instead of a video")
		}
	}
	return body, contentType, nil
}
