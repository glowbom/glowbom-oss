package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
)

const (
	openAIImageModelID     = "gpt-image-2"
	openAIImageSourceLabel = "Glowbom Images (gpt-image-2)"
)

func isOpenAIImageSource(imageSource string) bool {
	normalized := strings.ToLower(strings.TrimSpace(imageSource))
	return strings.Contains(normalized, "gpt-image-") ||
		strings.Contains(normalized, "gpt image ")
}

// callOpenAIImageGeneration calls OpenAI's image generation API with the current GPT Image model.
// Returns base64 data URI on success
func callOpenAIImageGeneration(prompt string, aspectRatio string, outputFormat string, apiKey string) (string, error) {
	url := "https://api.openai.com/v1/images/generations"

	// Map aspect ratio to OpenAI size parameter
	size := "1024x1024" // default
	switch aspectRatio {
	case "9:16":
		size = "1024x1536"
	case "16:9":
		size = "1536x1024"
	case "1:1", "":
		size = "1024x1024"
	}

	// Request payload for GPT Image 2.
	// Using low quality for fastest experience
	reqBody := map[string]interface{}{
		"model":   openAIImageModelID,
		"prompt":  prompt,
		"size":    size,
		"quality": "low", // low quality for speed
		"n":       1,
	}

	// Add output_format if explicitly set (OpenAI supports: "png", "jpeg", "webp")
	if outputFormat == "jpeg" || outputFormat == "png" || outputFormat == "webp" {
		reqBody["output_format"] = outputFormat
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call OpenAI API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OpenAI API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	var result struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Data) == 0 {
		return "", fmt.Errorf("no image data in response")
	}

	// Return as data URI with correct mime type
	mimeType := "image/png"
	if outputFormat == "jpeg" {
		mimeType = "image/jpeg"
	} else if outputFormat == "webp" {
		mimeType = "image/webp"
	}
	dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, result.Data[0].B64JSON)
	return dataURI, nil
}

// callOpenAIImageGenerationWithReference calls OpenAI's /v1/images/edits endpoint
// to generate an image using a reference image (e.g., user's photo)
//
// PRIVACY NOTE: This function does not store or cache the reference image.
// The image is only sent to OpenAI's API and immediately discarded after the request.
//
// Returns base64 data URI on success
func callOpenAIImageGenerationWithReference(prompt string, referenceImageBase64 string, aspectRatio string, outputFormat string, apiKey string) (string, error) {
	url := "https://api.openai.com/v1/images/edits"

	// Decode base64 to bytes
	imageBytes, err := base64.StdEncoding.DecodeString(referenceImageBase64)
	if err != nil {
		return "", fmt.Errorf("failed to decode reference image: %w", err)
	}

	// Detect image format from magic bytes
	contentType := "image/png"
	filename := "reference.png"
	if len(imageBytes) > 2 {
		// JPEG magic bytes: FF D8 FF
		if imageBytes[0] == 0xFF && imageBytes[1] == 0xD8 && imageBytes[2] == 0xFF {
			contentType = "image/jpeg"
			filename = "reference.jpg"
		} else if len(imageBytes) > 3 && imageBytes[0] == 0x89 && imageBytes[1] == 0x50 && imageBytes[2] == 0x4E && imageBytes[3] == 0x47 {
			// PNG magic bytes: 89 50 4E 47
			contentType = "image/png"
			filename = "reference.png"
		} else if len(imageBytes) > 11 && string(imageBytes[8:12]) == "WEBP" {
			// WebP magic bytes check
			contentType = "image/webp"
			filename = "reference.webp"
		}
	}

	// Map aspect ratio to size
	size := "1024x1024" // default
	switch aspectRatio {
	case "9:16":
		size = "1024x1536"
	case "16:9":
		size = "1536x1024"
	case "1:1", "":
		size = "1024x1024"
	}

	// Create multipart form data
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	// Add image file with proper MIME type using CreatePart
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image[]"; filename="%s"`, filename))
	h.Set("Content-Type", contentType)
	part, err := writer.CreatePart(h)
	if err != nil {
		return "", fmt.Errorf("failed to create form part: %w", err)
	}
	if _, err := part.Write(imageBytes); err != nil {
		return "", fmt.Errorf("failed to write image data: %w", err)
	}

	// Add prompt
	if err := writer.WriteField("prompt", prompt); err != nil {
		return "", fmt.Errorf("failed to write prompt: %w", err)
	}

	// Add model
	if err := writer.WriteField("model", openAIImageModelID); err != nil {
		return "", fmt.Errorf("failed to write model: %w", err)
	}

	// Add size (using mapped aspect ratio)
	if err := writer.WriteField("size", size); err != nil {
		return "", fmt.Errorf("failed to write size: %w", err)
	}

	// Add quality
	if err := writer.WriteField("quality", "low"); err != nil {
		return "", fmt.Errorf("failed to write quality: %w", err)
	}

	// Add n (number of images)
	if err := writer.WriteField("n", "1"); err != nil {
		return "", fmt.Errorf("failed to write n: %w", err)
	}

	// Add output_format if explicitly set
	if outputFormat == "jpeg" || outputFormat == "png" || outputFormat == "webp" {
		if err := writer.WriteField("output_format", outputFormat); err != nil {
			return "", fmt.Errorf("failed to write output_format: %w", err)
		}
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("failed to close writer: %w", err)
	}

	// Create request
	req, err := http.NewRequest("POST", url, &requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiKey)

	// Execute request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call OpenAI API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OpenAI API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	var result struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Data) == 0 {
		return "", fmt.Errorf("no image data in response")
	}

	// Return as data URI with correct mime type
	mimeType := "image/png"
	if outputFormat == "jpeg" {
		mimeType = "image/jpeg"
	} else if outputFormat == "webp" {
		mimeType = "image/webp"
	}
	dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, result.Data[0].B64JSON)
	return dataURI, nil
}

// shouldUseReferenceImage detects if the prompt contains keywords that suggest
// using the reference image (person, product, pet, etc.)
func shouldUseReferenceImage(prompt string) bool {
	lowerPrompt := strings.ToLower(prompt)

	// Keywords that suggest personalization
	keywords := []string{
		// Person-related
		"person", "me", "myself", "i ", "user", "subject", "face", "portrait",
		"man", "woman", "boy", "girl", "individual", "character",
		// Object/product-related
		"product", "item", "this", "same", "original", "object",
		// Pet-related
		"dog", "cat", "pet", "puppy", "kitten",
		// Possessive pronouns
		"my ", "their ", "his ", "her ",
	}

	for _, keyword := range keywords {
		if strings.Contains(lowerPrompt, keyword) {
			return true
		}
	}

	return false
}
