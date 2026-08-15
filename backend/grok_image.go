package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	neturl "net/url"
	"path"
	"strings"
)

const (
	xAIImageGenerationURL  = "https://api.x.ai/v1/images/generations"
	xAIImageEditURL        = "https://api.x.ai/v1/images/edits"
	xAIImageModel          = "grok-imagine-image-quality"
	xAIImageResolution     = "1k"
	xAIImageOutputFilename = "grok-imagine-image-quality.jpg"
	xAIImageSourceLabel    = "Glowbom Images (Grok Imagine Image Quality)"
)

// callGrokImageGeneration calls xAI's Grok image generation API.
// Returns a base64 data URI on success.
func callGrokImageGeneration(prompt, apiKey, aspectRatio string) (string, error) {
	reqBody := map[string]interface{}{
		"model":        xAIImageModel,
		"prompt":       prompt,
		"n":            1,
		"resolution":   xAIImageResolution,
		"image_format": "url",
	}

	if trimmedAspectRatio := strings.TrimSpace(aspectRatio); trimmedAspectRatio != "" {
		reqBody["aspect_ratio"] = trimmedAspectRatio
	}

	return callXAIImageAPI(xAIImageGenerationURL, xAIImageModel, reqBody, apiKey, false, aspectRatio)
}

// callGrokImageGenerationWithReference sends a single reference image to Grok image edits API.
//
// PRIVACY NOTE: Reference image is only sent to xAI API and not cached.
func callGrokImageGenerationWithReference(prompt, referenceImageBase64, apiKey, aspectRatio string) (string, error) {
	referenceImageURL := ensureImageDataURI(referenceImageBase64, "image/jpeg")
	if strings.TrimSpace(referenceImageURL) == "" {
		return "", fmt.Errorf("reference image is required")
	}

	reqBody := map[string]interface{}{
		"model":      xAIImageModel,
		"prompt":     prompt,
		"n":          1,
		"resolution": xAIImageResolution,
		"image": map[string]interface{}{
			"url": referenceImageURL,
		},
	}

	if trimmedAspectRatio := strings.TrimSpace(aspectRatio); trimmedAspectRatio != "" {
		reqBody["aspect_ratio"] = trimmedAspectRatio
	} else {
		reqBody["aspect_ratio"] = "auto"
	}

	return callXAIImageAPI(xAIImageEditURL, xAIImageModel, reqBody, apiKey, true, aspectRatio)
}

func callXAIImageAPI(endpointURL, model string, reqBody map[string]interface{}, apiKey string, hasReference bool, aspectRatio string) (string, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", endpointURL, bytes.NewReader(bodyBytes))
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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("xAI API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	dataURI, err := parseXAIImageGenerationResponse(respBody, apiKey)
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

func parseXAIImageGenerationResponse(respBody []byte, apiKey string) (string, error) {
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
		return downloadImageURLAsDataURI(first.url, apiKey)
	}
	return "", fmt.Errorf("image response did not include b64_json or url")
}

func downloadImageURLAsDataURI(imageURL, apiKey string) (string, error) {
	req, err := http.NewRequest("GET", imageURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create image download request: %w", err)
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to download image URL: %w", err)
	}
	defer resp.Body.Close()

	imageBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read downloaded image: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("image download failed (status %d): %s", resp.StatusCode, string(imageBytes))
	}
	if len(imageBytes) == 0 {
		return "", fmt.Errorf("downloaded image is empty")
	}

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		if parsedURL, err := neturl.Parse(imageURL); err == nil {
			ext := strings.ToLower(path.Ext(parsedURL.Path))
			if ext != "" {
				contentType = mime.TypeByExtension(ext)
			}
		}
	}
	if contentType == "" {
		contentType = http.DetectContentType(imageBytes)
	}
	if contentType == "" {
		contentType = "image/jpeg"
	}
	if semicolonIdx := strings.Index(contentType, ";"); semicolonIdx != -1 {
		contentType = strings.TrimSpace(contentType[:semicolonIdx])
	}

	encoded := base64.StdEncoding.EncodeToString(imageBytes)
	return fmt.Sprintf("data:%s;base64,%s", contentType, encoded), nil
}
