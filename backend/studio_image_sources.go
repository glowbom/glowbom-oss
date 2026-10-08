package main

import (
	"context"
	"errors"
)

// Explicit Studio source selections never fall back or replay a failed request.
func generateStudioSelectedImage(ctx context.Context, source, key, prompt, reference, aspect string) (string, string, error) {
	switch source {
	case "openai-subscription":
		value, err := callCodexImageGeneration(ctx, prompt, reference, aspect)
		return value, codexImageSourceLabel, err
	case "glowbom-api":
		value, err := callGlowbomImageGeneration(ctx, prompt, reference)
		return value, glowbomImageSourceLabel, err
	case "openai-api":
		if reference != "" {
			value, err := callOpenAIImageGenerationWithReference(prompt, reference, aspect, "png", key, ctx)
			return value, openAIImageSourceLabel, err
		}
		value, err := callOpenAIImageGeneration(prompt, aspect, "png", key, ctx)
		return value, openAIImageSourceLabel, err
	case "gemini-api":
		if reference != "" {
			value, err := callGeminiImageGenerationWithReference(prompt, reference, aspect, "png", key, ctx)
			return value, "Glowbom Images (Nano Banana 2)", err
		}
		value, err := callGeminiImageGeneration(prompt, aspect, "png", key, ctx)
		return value, "Glowbom Images (Nano Banana 2)", err
	case "xai-api", "xai-subscription":
		if source == "xai-subscription" {
			credential, err := resolveProjectIconSubscription(ctx, false)
			if err != nil {
				return "", xAIImageSourceLabel, err
			}
			key = credential.Bearer
		}
		if reference != "" {
			value, err := callGrokImageGenerationWithReference(prompt, "data:image/png;base64,"+reference, key, aspect, ctx)
			return value, xAIImageSourceLabel, err
		}
		value, err := callGrokImageGeneration(prompt, key, aspect, ctx)
		return value, xAIImageSourceLabel, err
	}
	return "", "", errors.New("Choose a supported image source.")
}
