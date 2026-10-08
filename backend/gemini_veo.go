package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"
)

// MARK: - Request/Response Types

type VeoImageInput struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

type VeoVideoAsset struct {
	URI         string `json:"uri"`
	AspectRatio string `json:"aspectRatio"`
}

type VeoGenerationRequest struct {
	ModelID         string          `json:"modelId,omitempty"`
	Prompt          string          `json:"prompt"`
	Images          []VeoImageInput `json:"images"`
	AspectRatio     string          `json:"aspectRatio"`
	UseKeyframes    bool            `json:"useKeyframes"`
	ExtensionSource *VeoVideoAsset  `json:"extensionSource"`
	DurationSeconds int             `json:"durationSeconds,omitempty"`
	Resolution      string          `json:"resolution,omitempty"`
	VideoSource     string          `json:"videoSource,omitempty"`
	GeminiKey       string          `json:"geminiKey"`
	XaiKey          string          `json:"xaiKey,omitempty"`
}

type VeoGenerationResponse struct {
	OperationID string `json:"operationId"`
	Message     string `json:"message"`
}

type VeoPollRequest struct {
	OperationID string `json:"operationId"`
	VideoSource string `json:"videoSource,omitempty"`
	GeminiKey   string `json:"geminiKey"`
	XaiKey      string `json:"xaiKey,omitempty"`
}

type VeoPollResponse struct {
	DurationSeconds float64        `json:"durationSeconds,omitempty"`
	Done            bool           `json:"done"`
	Status          string         `json:"status"`
	VideoURL        string         `json:"videoUrl,omitempty"`
	VideoAsset      *VeoVideoAsset `json:"videoAsset,omitempty"`
	Error           string         `json:"error,omitempty"`
}

// Provider options are explicit so polling never changes the billable model.
func startVeoVideoGeneration(req VeoGenerationRequest, contexts ...context.Context) (*VeoGenerationResponse, error) {
	if strings.TrimSpace(req.GeminiKey) == "" {
		return nil, fmt.Errorf("Google API key required")
	}
	// Older clients did not select a model. Preserve their feature defaults;
	// an explicit model never changes to another billable model.
	if strings.TrimSpace(req.ModelID) == "" {
		req.ModelID = "veo-3.1-fast-generate-preview"
		if req.ExtensionSource != nil || (len(req.Images) > 1 && !req.UseKeyframes) {
			req.ModelID = "veo-3.1-generate-preview"
		}
		if req.DurationSeconds == 0 && (req.ExtensionSource != nil || len(req.Images) > 1) {
			req.DurationSeconds = 8
		}
	}
	options, err := normalizeStudioVideoOptions(studioVideoOptions{SourceID: "veo-api", ModelID: req.ModelID, DurationSeconds: req.DurationSeconds, Resolution: req.Resolution, AspectRatio: req.AspectRatio})
	if err != nil {
		return nil, err
	}
	if len(req.Images) > 3 {
		return nil, fmt.Errorf("Veo supports up to three reference images")
	}
	if options.ModelID == "veo-3.1-lite-generate-preview" && (req.ExtensionSource != nil || len(req.Images) > 1) {
		return nil, fmt.Errorf("Veo Lite supports text or one starting image")
	}
	if req.ExtensionSource != nil && (options.Resolution != "720p" || options.DurationSeconds != 8) {
		return nil, fmt.Errorf("Veo extension requires 8 seconds at 720p")
	}
	if len(req.Images) > 1 && !req.UseKeyframes && (options.ModelID != "veo-3.1-generate-preview" || options.DurationSeconds != 8 || options.AspectRatio != "16:9") {
		return nil, fmt.Errorf("Veo reference images require Standard, 8 seconds, and landscape")
	}
	if req.UseKeyframes && len(req.Images) != 2 {
		return nil, fmt.Errorf("Veo keyframes require two images")
	}
	ctx, cancel := context.WithTimeout(imageRequestContext(contexts), 60*time.Second)
	defer cancel()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: req.GeminiKey, Backend: genai.BackendGeminiAPI, HTTPClient: studioVideoProviderClient()})
	if err != nil {
		return nil, fmt.Errorf("Could not create the Google video client")
	}
	duration := int32(options.DurationSeconds)
	config := &genai.GenerateVideosConfig{NumberOfVideos: 1, Resolution: options.Resolution, AspectRatio: options.AspectRatio, DurationSeconds: &duration}
	images := make([]*genai.Image, 0, len(req.Images))
	for _, input := range req.Images {
		data, decodeErr := base64.StdEncoding.DecodeString(input.Data)
		if decodeErr != nil {
			return nil, fmt.Errorf("Invalid video starting image")
		}
		images = append(images, &genai.Image{ImageBytes: data, MIMEType: input.MimeType})
	}
	var operation *genai.GenerateVideosOperation
	if req.ExtensionSource != nil {
		operation, err = client.Models.GenerateVideosFromSource(ctx, options.ModelID, &genai.GenerateVideosSource{Prompt: req.Prompt, Video: &genai.Video{URI: req.ExtensionSource.URI}}, config)
	} else if req.UseKeyframes {
		config.LastFrame = images[1]
		operation, err = client.Models.GenerateVideos(ctx, options.ModelID, req.Prompt, images[0], config)
	} else if len(images) > 1 {
		for _, image := range images {
			config.ReferenceImages = append(config.ReferenceImages, &genai.VideoGenerationReferenceImage{Image: image, ReferenceType: "ASSET"})
		}
		operation, err = client.Models.GenerateVideosFromSource(ctx, options.ModelID, &genai.GenerateVideosSource{Prompt: req.Prompt}, config)
	} else {
		var image *genai.Image
		if len(images) == 1 {
			image = images[0]
		}
		operation, err = client.Models.GenerateVideos(ctx, options.ModelID, req.Prompt, image, config)
	}
	if err != nil {
		return nil, studioSelectedVideoError(err, "Google")
	}
	if operation == nil || strings.TrimSpace(operation.Name) == "" {
		return nil, fmt.Errorf("Google video generation did not return an operation")
	}
	return &VeoGenerationResponse{OperationID: operation.Name, Message: "Video generation started successfully"}, nil
}

func pollVeoOperation(operationID, apiKey string, contexts ...context.Context) (*VeoPollResponse, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("Google API key required")
	}
	if strings.Contains(operationID, ":") || strings.Contains(operationID, "?") || strings.Contains(operationID, "..") || !strings.Contains(operationID, "operations/") {
		return nil, fmt.Errorf("Invalid Google video operation")
	}
	ctx, cancel := context.WithTimeout(imageRequestContext(contexts), 60*time.Second)
	defer cancel()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: apiKey, Backend: genai.BackendGeminiAPI, HTTPClient: studioVideoProviderClient()})
	if err != nil {
		return nil, fmt.Errorf("Could not create the Google video client")
	}
	operation, err := client.Operations.GetVideosOperation(ctx, &genai.GenerateVideosOperation{Name: operationID}, nil)
	if err != nil {
		return nil, studioSelectedVideoError(err, "Google")
	}
	if operation.Error != nil {
		return &VeoPollResponse{Done: true, Status: "failed", Error: "Google could not finish this video. Check account limits and content permissions."}, nil
	}
	if !operation.Done {
		return &VeoPollResponse{Status: "processing"}, nil
	}
	if operation.Response == nil || len(operation.Response.GeneratedVideos) == 0 || operation.Response.GeneratedVideos[0].Video == nil || operation.Response.GeneratedVideos[0].Video.URI == "" {
		return &VeoPollResponse{Done: true, Status: "failed", Error: "Google finished without a video file. The request may have been filtered."}, nil
	}
	uri := operation.Response.GeneratedVideos[0].Video.URI
	return &VeoPollResponse{Done: true, Status: "completed", VideoURL: uri, VideoAsset: &VeoVideoAsset{URI: uri}}, nil
}
