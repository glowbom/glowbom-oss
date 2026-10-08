package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const nanoBanana2ModelID = "gemini-3.1-flash-image"

// callGeminiImageGeneration calls Gemini image generation via generativelanguage.googleapis.com
// Returns base64 data URI on success
func callGeminiImageGeneration(prompt string, aspectRatio string, outputFormat string, apiKey string, contexts ...context.Context) (string, error) {
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", nanoBanana2ModelID)

	// Build request with text prompt
	contents := []map[string]interface{}{
		{
			"parts": []map[string]interface{}{
				{
					"text": prompt,
				},
			},
		},
	}

	// Build request with aspect ratio in correct structure (based on official example)
	generationConfig := map[string]interface{}{
		"temperature":        1.0,
		"maxOutputTokens":    8192,
		"responseModalities": []string{"IMAGE"},
	}

	// Add imageConfig with aspectRatio ONLY if specified
	// Note: outputFormat is not supported by the Gemini API (only Vertex AI)
	if strings.TrimSpace(aspectRatio) != "" {
		generationConfig["imageConfig"] = map[string]interface{}{
			"aspectRatio": strings.TrimSpace(aspectRatio),
		}
	}

	reqBody := map[string]interface{}{
		"contents":         contents,
		"generationConfig": generationConfig,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(imageRequestContext(contexts), "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	fmt.Println("[DEBUG] Calling Gemini image generation API...")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call Gemini API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 24<<20))
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Gemini API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text       string `json:"text,omitempty"`
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Candidates) == 0 {
		return "", fmt.Errorf("no candidates in response")
	}

	// Find the image data in parts
	var textFallback string
	for _, part := range result.Candidates[0].Content.Parts {
		if part.InlineData.Data != "" {
			mimePrefix := "image/png"
			if part.InlineData.MimeType != "" {
				mimePrefix = part.InlineData.MimeType
			}
			dataURI := fmt.Sprintf("data:%s;base64,%s", mimePrefix, part.InlineData.Data)
			return dataURI, nil
		}
		if part.Text != "" && textFallback == "" {
			textFallback = part.Text
		}
	}

	if textFallback != "" {
		return "", fmt.Errorf("no image generated — model responded with text: %s", textFallback)
	}
	return "", fmt.Errorf("no image data in response")
}

// callGeminiImageGenerationWithReference calls Gemini image generation with a reference image
// for personalized generation (e.g., user photo, product image)
//
// PRIVACY NOTE: This function does not store or cache the reference image.
// The image is only sent to Gemini's API and immediately discarded after the request.
//
// Returns base64 data URI on success
func callGeminiImageGenerationWithReference(prompt string, referenceImageBase64 string, aspectRatio string, outputFormat string, apiKey string, contexts ...context.Context) (string, error) {
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", nanoBanana2ModelID)

	geminiAspectRatio := strings.TrimSpace(aspectRatio)

	// Detect image format from base64 data (first few characters after decoding)
	mimeType := "image/jpeg"
	imageBytes, err := base64.StdEncoding.DecodeString(referenceImageBase64)
	if err == nil && len(imageBytes) > 2 {
		// PNG magic bytes: 89 50 4E 47
		if imageBytes[0] == 0x89 && imageBytes[1] == 0x50 && imageBytes[2] == 0x4E && imageBytes[3] == 0x47 {
			mimeType = "image/png"
		} else if len(imageBytes) > 11 && string(imageBytes[8:12]) == "WEBP" {
			mimeType = "image/webp"
		}
	}

	// Build request with both text prompt and reference image
	contents := []map[string]interface{}{
		{
			"parts": []map[string]interface{}{
				{
					"text": prompt,
				},
				{
					"inline_data": map[string]interface{}{
						"mime_type": mimeType,
						"data":      referenceImageBase64,
					},
				},
			},
		},
	}

	// Build request with aspect ratio in correct structure (based on official example)
	generationConfig := map[string]interface{}{
		"temperature":        1.0,
		"maxOutputTokens":    8192,
		"responseModalities": []string{"IMAGE"},
	}

	// Add imageConfig with aspectRatio if specified
	// Note: outputFormat is not supported by the Gemini API (only Vertex AI)
	if geminiAspectRatio != "" {
		generationConfig["imageConfig"] = map[string]interface{}{
			"aspectRatio": geminiAspectRatio,
		}
	}

	reqBody := map[string]interface{}{
		"contents":         contents,
		"generationConfig": generationConfig,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(imageRequestContext(contexts), "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	fmt.Println("[DEBUG] Calling Gemini image generation API with reference image...")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call Gemini API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 24<<20))
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Gemini API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text       string `json:"text,omitempty"`
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Candidates) == 0 {
		return "", fmt.Errorf("no candidates in response")
	}

	// Find the image data in parts
	var textFallback string
	for _, part := range result.Candidates[0].Content.Parts {
		if part.InlineData.Data != "" {
			mimePrefix := "image/png"
			if part.InlineData.MimeType != "" {
				mimePrefix = part.InlineData.MimeType
			}
			dataURI := fmt.Sprintf("data:%s;base64,%s", mimePrefix, part.InlineData.Data)
			return dataURI, nil
		}
		if part.Text != "" && textFallback == "" {
			textFallback = part.Text
		}
	}

	if textFallback != "" {
		return "", fmt.Errorf("no image generated — model responded with text: %s", textFallback)
	}
	return "", fmt.Errorf("no image data in response")
}
