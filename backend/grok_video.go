package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

const (
	xAIVideoGenerationURL   = "https://api.x.ai/v1/videos/generations"
	xAIVideoGenerationModel = "grok-imagine-video"
	xAIVideoRequestTimeout  = 60 * time.Second
	xAIVideoResponseLimit   = 1 << 20
)

func startGrokImagineVideoGeneration(req VeoGenerationRequest, contexts ...context.Context) (*VeoGenerationResponse, error) {
	if req.ModelID != "" && req.ExtensionSource == nil {
		options, err := normalizeStudioVideoOptions(studioVideoOptions{SourceID: "xai-api", ModelID: req.ModelID, DurationSeconds: req.DurationSeconds, Resolution: req.Resolution, AspectRatio: req.AspectRatio})
		if err != nil {
			return nil, err
		}
		req.DurationSeconds, req.Resolution, req.AspectRatio = options.DurationSeconds, options.Resolution, options.AspectRatio
	}
	if strings.TrimSpace(req.XaiKey) == "" {
		return nil, fmt.Errorf("xAI API key required")
	}

	payload := map[string]interface{}{
		"model":  xAIVideoGenerationModel,
		"prompt": req.Prompt,
	}

	if req.ModelID != "" {
		payload["model"] = req.ModelID
	}
	hasMediaInput := false
	hasVideoInput := false
	if len(req.Images) > 0 {
		hasMediaInput = true
		payload["image"] = map[string]interface{}{
			"url": veoImageInputToDataURI(req.Images[0]),
		}
	}
	if req.ExtensionSource != nil && strings.TrimSpace(req.ExtensionSource.URI) != "" {
		hasMediaInput = true
		hasVideoInput = true
		payload["video"] = map[string]interface{}{
			"url": strings.TrimSpace(req.ExtensionSource.URI),
		}
	}
	if !hasMediaInput && req.ModelID == "" {
		return nil, fmt.Errorf("at least one image or video input is required")
	}
	if hasVideoInput && (req.DurationSeconds != 0 || strings.TrimSpace(req.Resolution) != "" || strings.TrimSpace(req.AspectRatio) != "") {
		fmt.Printf("[GROK VIDEO] Edit mode override ignored per docs duration=%d resolution=%q aspect=%q\n",
			req.DurationSeconds, strings.TrimSpace(req.Resolution), strings.TrimSpace(req.AspectRatio))
	}

	// xAI docs: editing an existing video does not support duration/aspect_ratio/resolution overrides.
	if !hasVideoInput {
		payload["duration"] = normalizeXAIVideoDurationSeconds(req.DurationSeconds)
		payload["resolution"] = normalizeXAIVideoResolution(req.Resolution)
		if req.ModelID != "" {
			payload["resolution"] = req.Resolution
		}
		if aspectRatio := normalizeVideoAspectRatio(req.AspectRatio); aspectRatio != "" {
			payload["aspect_ratio"] = aspectRatio
		}
	}

	mode := "image-to-video"
	if hasVideoInput {
		mode = "video-to-video"
	}
	fmt.Printf("[GROK VIDEO] Start request mode=%s prompt_len=%d image=%t video=%t duration=%v resolution=%v aspect=%v\n",
		mode,
		len(strings.TrimSpace(req.Prompt)),
		payload["image"] != nil,
		payload["video"] != nil,
		payload["duration"],
		payload["resolution"],
		payload["aspect_ratio"],
	)

	respBody, statusCode, err := xaiStartVideoGenerationRequest(payload, req.XaiKey, contexts...)
	if err != nil {
		return nil, err
	}
	fmt.Printf("[GROK VIDEO] Start response status=%d\n", statusCode)
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		// Only a rejected payload can use the legacy format. Retrying other
		// failures could submit another billable generation.
		if req.ModelID != "" || (statusCode != http.StatusBadRequest && statusCode != http.StatusUnprocessableEntity) {
			return nil, &xAIAPIError{Status: statusCode, Body: string(respBody)}
		}
		legacyPayload := map[string]interface{}{
			"model":      payload["model"],
			"prompt":     payload["prompt"],
			"duration":   payload["duration"],
			"resolution": payload["resolution"],
		}
		if aspectRatio := payload["aspect_ratio"]; aspectRatio != nil {
			legacyPayload["aspect_ratio"] = aspectRatio
		}
		if imagePayload := xaiAsMap(payload["image"]); imagePayload != nil {
			if url := xaiMapStringValue(imagePayload, "url"); url != "" {
				legacyPayload["image_url"] = url
			}
		}
		if videoPayload := xaiAsMap(payload["video"]); videoPayload != nil {
			if url := xaiMapStringValue(videoPayload, "url"); url != "" {
				legacyPayload["video_url"] = url
			}
		}

		fmt.Printf("[GROK VIDEO] Start legacy fallback request status=%d\n", statusCode)
		legacyRespBody, legacyStatusCode, legacyErr := xaiStartVideoGenerationRequest(legacyPayload, req.XaiKey, contexts...)
		if legacyErr != nil {
			return nil, legacyErr
		}
		fmt.Printf("[GROK VIDEO] Start legacy fallback response status=%d\n", legacyStatusCode)
		if legacyStatusCode < http.StatusOK || legacyStatusCode >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("xAI video API error (%d): %s | fallback (%d): %s",
				statusCode,
				strings.TrimSpace(string(respBody)),
				legacyStatusCode,
				strings.TrimSpace(string(legacyRespBody)),
			)
		}
		respBody = legacyRespBody
	}

	var result struct {
		RequestID string `json:"request_id"`
		ID        string `json:"id"`
		Status    string `json:"status"`
		Data      struct {
			RequestID string `json:"request_id"`
			ID        string `json:"id"`
		} `json:"data"`
		Error interface{} `json:"error"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse xAI video start response: %w", err)
	}

	operationID := strings.TrimSpace(result.RequestID)
	if operationID == "" {
		operationID = strings.TrimSpace(result.ID)
	}
	if operationID == "" {
		operationID = strings.TrimSpace(result.Data.RequestID)
	}
	if operationID == "" {
		operationID = strings.TrimSpace(result.Data.ID)
	}
	if operationID == "" {
		if errMsg := xaiErrorValue(result.Error); errMsg != "" {
			return nil, fmt.Errorf("xAI video generation failed: %s", errMsg)
		}
		return nil, fmt.Errorf("xAI video generation did not return a request id")
	}

	return &VeoGenerationResponse{
		OperationID: operationID,
		Message:     "Video generation started successfully",
	}, nil
}

func xaiStartVideoGenerationRequest(payload map[string]interface{}, apiKey string, contexts ...context.Context) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(imageRequestContext(contexts), xAIVideoRequestTimeout)
	defer cancel()
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to marshal xAI video request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", xAIVideoGenerationURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create xAI video request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	httpReq.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to call xAI video API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, xAIVideoResponseLimit+1))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read xAI video response: %w", err)
	}
	if len(respBody) > xAIVideoResponseLimit {
		return nil, resp.StatusCode, fmt.Errorf("xAI video response is too large")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return respBody, resp.StatusCode, &xAIAPIError{Status: resp.StatusCode, Body: string(respBody)}
	}
	return respBody, resp.StatusCode, nil
}

func pollGrokImagineVideoOperation(operationID, apiKey string, contexts ...context.Context) (*VeoPollResponse, error) {
	if strings.TrimSpace(operationID) == "" {
		return nil, fmt.Errorf("operation id is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("xAI API key required")
	}
	ctx, cancel := context.WithTimeout(imageRequestContext(contexts), xAIVideoRequestTimeout)
	defer cancel()

	statusURL := fmt.Sprintf("https://api.x.ai/v1/videos/%s", neturl.PathEscape(strings.TrimSpace(operationID)))
	httpReq, err := http.NewRequestWithContext(ctx, "GET", statusURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create xAI video poll request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	httpReq.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to poll xAI video operation: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, xAIVideoResponseLimit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read xAI video poll response: %w", err)
	}
	if len(respBody) > xAIVideoResponseLimit {
		return nil, fmt.Errorf("xAI video poll response is too large")
	}
	fmt.Printf("[GROK VIDEO] Poll response operation=%s status_code=%d\n",
		strings.TrimSpace(operationID), resp.StatusCode)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &xAIAPIError{Status: resp.StatusCode, Body: string(respBody)}
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse xAI video poll response: %w", err)
	}
	fmt.Printf("[GROK VIDEO] Poll status candidates operation=%s top=%q data=%q result=%q response=%q\n",
		strings.TrimSpace(operationID),
		strings.TrimSpace(xaiMapStringValue(raw, "status", "state", "request_status", "requestStatus")),
		strings.TrimSpace(xaiMapStringValue(xaiAsMap(raw["data"]), "status", "state", "request_status", "requestStatus")),
		strings.TrimSpace(xaiMapStringValue(xaiAsMap(raw["result"]), "status", "state", "request_status", "requestStatus")),
		strings.TrimSpace(xaiMapStringValue(xaiAsMap(raw["response"]), "status", "state", "request_status", "requestStatus")),
	)

	rawStatus := xaiExtractVideoStatus(raw)
	status := rawStatus
	if status == "" {
		status = "unknown"
	}

	videoURL := xaiExtractVideoURL(raw)
	durationSeconds := xaiVideoDuration(raw)
	aspectRatio := normalizeVideoAspectRatio(xaiExtractAspectRatio(raw))
	errorMsg := xaiExtractErrorMessage(raw)
	fmt.Printf("[GROK VIDEO] Poll parsed operation=%s raw_status=%q normalized_status=%s has_video_url=%t has_error=%t\n",
		strings.TrimSpace(operationID), rawStatus, status, strings.TrimSpace(videoURL) != "", errorMsg != "")

	switch status {
	case "completed", "succeeded", "success", "done":
		if strings.TrimSpace(videoURL) == "" {
			if errorMsg == "" {
				errorMsg = "video generation completed but no video URL was returned"
			}
			return &VeoPollResponse{
				Done:   true,
				Status: "failed",
				Error:  errorMsg,
			}, nil
		}
		return &VeoPollResponse{
			Done:            true,
			Status:          "completed",
			VideoURL:        videoURL,
			DurationSeconds: durationSeconds,
			VideoAsset: &VeoVideoAsset{
				URI:         videoURL,
				AspectRatio: aspectRatio,
			},
		}, nil
	case "expired", "failed", "error", "cancelled", "canceled", "rejected":
		if errorMsg == "" {
			errorMsg = "video generation failed"
		}
		return &VeoPollResponse{
			Done:   true,
			Status: "failed",
			Error:  errorMsg,
		}, nil
	case "pending", "processing", "running", "queued", "in_progress":
		// Some xAI poll responses omit status once video.url is available.
		if strings.TrimSpace(videoURL) != "" {
			return &VeoPollResponse{
				Done:            true,
				Status:          "completed",
				VideoURL:        videoURL,
				DurationSeconds: durationSeconds,
				VideoAsset: &VeoVideoAsset{
					URI:         videoURL,
					AspectRatio: aspectRatio,
				},
			}, nil
		}
		return &VeoPollResponse{
			Done:   false,
			Status: "processing",
		}, nil
	default:
		if strings.TrimSpace(videoURL) != "" {
			return &VeoPollResponse{
				Done:            true,
				Status:          "completed",
				VideoURL:        videoURL,
				DurationSeconds: durationSeconds,
				VideoAsset: &VeoVideoAsset{
					URI:         videoURL,
					AspectRatio: aspectRatio,
				},
			}, nil
		}
		if errorMsg != "" {
			return &VeoPollResponse{
				Done:   true,
				Status: "failed",
				Error:  errorMsg,
			}, nil
		}
		return &VeoPollResponse{
			Done:   false,
			Status: "processing",
		}, nil
	}
}

func normalizeXAIVideoDurationSeconds(value int) int {
	if value <= 0 {
		return 5
	}
	if value < 1 {
		return 1
	}
	if value > 15 {
		return 15
	}
	return value
}

func normalizeXAIVideoResolution(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "480p":
		return "480p"
	case "720p":
		return "720p"
	default:
		return "480p"
	}
}

func veoImageInputToDataURI(input VeoImageInput) string {
	raw := strings.TrimSpace(input.Data)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:image/") {
		return raw
	}

	mimeType := strings.TrimSpace(input.MimeType)
	if mimeType == "" {
		mimeType = "image/jpeg"
	}
	return fmt.Sprintf("data:%s;base64,%s", mimeType, raw)
}

func normalizeVideoAspectRatio(value string) string {
	trimmed := strings.TrimSpace(value)
	switch strings.ToLower(trimmed) {
	case "", "auto":
		return ""
	case "16:9", "9:16", "1:1", "4:3", "3:4":
		return strings.ToLower(trimmed)
	default:
		return strings.ToLower(trimmed)
	}
}

func xaiExtractVideoURL(raw map[string]interface{}) string {
	if direct := xaiMapStringValue(raw, "video_url", "videoUrl", "url", "download_url", "downloadUrl"); strings.TrimSpace(direct) != "" {
		return strings.TrimSpace(direct)
	}

	for _, key := range []string{"video", "data", "result", "response"} {
		if nested := xaiAsMap(raw[key]); nested != nil {
			if url := xaiMapStringValue(nested, "video_url", "videoUrl", "url", "download_url", "downloadUrl"); strings.TrimSpace(url) != "" {
				return strings.TrimSpace(url)
			}
			if video := xaiAsMap(nested["video"]); video != nil {
				if url := xaiMapStringValue(video, "video_url", "videoUrl", "url", "download_url", "downloadUrl"); strings.TrimSpace(url) != "" {
					return strings.TrimSpace(url)
				}
			}
			for _, innerKey := range []string{"response", "result", "data"} {
				if inner := xaiAsMap(nested[innerKey]); inner != nil {
					if url := xaiMapStringValue(inner, "video_url", "videoUrl", "url", "download_url", "downloadUrl"); strings.TrimSpace(url) != "" {
						return strings.TrimSpace(url)
					}
					if video := xaiAsMap(inner["video"]); video != nil {
						if url := xaiMapStringValue(video, "video_url", "videoUrl", "url", "download_url", "downloadUrl"); strings.TrimSpace(url) != "" {
							return strings.TrimSpace(url)
						}
					}
				}
			}
		}
	}

	for _, key := range []string{"videos", "output", "results"} {
		items := xaiAsSlice(raw[key])
		for _, item := range items {
			if nested := xaiAsMap(item); nested != nil {
				if url := xaiMapStringValue(nested, "video_url", "videoUrl", "url", "download_url", "downloadUrl"); strings.TrimSpace(url) != "" {
					return strings.TrimSpace(url)
				}
				for _, innerKey := range []string{"response", "result", "data", "video"} {
					if inner := xaiAsMap(nested[innerKey]); inner != nil {
						if url := xaiMapStringValue(inner, "video_url", "videoUrl", "url", "download_url", "downloadUrl"); strings.TrimSpace(url) != "" {
							return strings.TrimSpace(url)
						}
					}
				}
			}
		}
	}

	return ""
}

func xaiExtractAspectRatio(raw map[string]interface{}) string {
	if direct := xaiMapStringValue(raw, "aspect_ratio", "aspectRatio"); strings.TrimSpace(direct) != "" {
		return strings.TrimSpace(direct)
	}
	for _, key := range []string{"video", "data", "result", "response"} {
		if nested := xaiAsMap(raw[key]); nested != nil {
			if ratio := xaiMapStringValue(nested, "aspect_ratio", "aspectRatio"); strings.TrimSpace(ratio) != "" {
				return strings.TrimSpace(ratio)
			}
			if video := xaiAsMap(nested["video"]); video != nil {
				if ratio := xaiMapStringValue(video, "aspect_ratio", "aspectRatio"); strings.TrimSpace(ratio) != "" {
					return strings.TrimSpace(ratio)
				}
			}
			for _, innerKey := range []string{"response", "result", "data"} {
				if inner := xaiAsMap(nested[innerKey]); inner != nil {
					if ratio := xaiMapStringValue(inner, "aspect_ratio", "aspectRatio"); strings.TrimSpace(ratio) != "" {
						return strings.TrimSpace(ratio)
					}
					if video := xaiAsMap(inner["video"]); video != nil {
						if ratio := xaiMapStringValue(video, "aspect_ratio", "aspectRatio"); strings.TrimSpace(ratio) != "" {
							return strings.TrimSpace(ratio)
						}
					}
				}
			}
		}
	}
	return ""
}

func xaiExtractVideoStatus(raw map[string]interface{}) string {
	if direct := xaiMapStringValue(raw, "status", "state", "request_status", "requestStatus"); strings.TrimSpace(direct) != "" {
		return strings.ToLower(strings.TrimSpace(direct))
	}

	for _, key := range []string{"status", "data", "result", "response"} {
		if nested := xaiAsMap(raw[key]); nested != nil {
			if status := xaiMapStringValue(nested, "status", "state", "request_status", "requestStatus"); strings.TrimSpace(status) != "" {
				return strings.ToLower(strings.TrimSpace(status))
			}
			for _, innerKey := range []string{"response", "result", "data", "status"} {
				if inner := xaiAsMap(nested[innerKey]); inner != nil {
					if status := xaiMapStringValue(inner, "status", "state", "request_status", "requestStatus"); strings.TrimSpace(status) != "" {
						return strings.ToLower(strings.TrimSpace(status))
					}
				}
			}
		}
	}

	if doneRaw, ok := raw["done"].(bool); ok && doneRaw {
		return "done"
	}
	return ""
}

func xaiExtractErrorMessage(raw map[string]interface{}) string {
	if errMsg := xaiErrorValue(raw["error"]); errMsg != "" {
		return errMsg
	}
	for _, key := range []string{"response", "data", "result"} {
		if nested := xaiAsMap(raw[key]); nested != nil {
			if errMsg := xaiErrorValue(nested["error"]); errMsg != "" {
				return errMsg
			}
			for _, innerKey := range []string{"response", "result", "data"} {
				if inner := xaiAsMap(nested[innerKey]); inner != nil {
					if errMsg := xaiErrorValue(inner["error"]); errMsg != "" {
						return errMsg
					}
					if errMsg := strings.TrimSpace(xaiMapStringValue(inner, "message", "error_message")); errMsg != "" {
						return errMsg
					}
				}
			}
		}
	}
	return strings.TrimSpace(xaiMapStringValue(raw, "message", "error_message"))
}

func xaiErrorValue(v interface{}) string {
	switch typed := v.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]interface{}:
		return strings.TrimSpace(xaiMapStringValue(typed, "message", "error", "detail"))
	default:
		return ""
	}
}

func xaiMapStringValue(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			if str := xaiStringValue(value); str != "" {
				return str
			}
		}
	}
	return ""
}

func xaiStringValue(v interface{}) string {
	switch typed := v.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func xaiAsMap(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

func xaiAsSlice(v interface{}) []interface{} {
	items, _ := v.([]interface{})
	return items
}

func xaiVideoDuration(raw map[string]interface{}) float64 {
	if video := xaiAsMap(raw["video"]); video != nil {
		if duration, ok := video["duration"].(float64); ok && duration > 0 {
			return duration
		}
	}
	for _, key := range []string{"data", "result", "response"} {
		if child := xaiAsMap(raw[key]); child != nil {
			if duration := xaiVideoDuration(child); duration > 0 {
				return duration
			}
		}
	}
	return 0
}
