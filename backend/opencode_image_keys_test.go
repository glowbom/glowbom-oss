package main

import "testing"

func TestResolveAgentImageProviderKeysPrefersDedicatedKeys(t *testing.T) {
	openAIKey, geminiKey, xaiKey := resolveAgentImageProviderKeys(OpenCodeAgentRequest{
		OpenAIKey:      "coding-openai",
		OpenAIImageKey: " image-openai ",
		GeminiKey:      "coding-gemini",
		GeminiImageKey: " image-gemini ",
		XaiKey:         "coding-xai",
		XaiImageKey:    " image-xai ",
	})

	if openAIKey != "image-openai" || geminiKey != "image-gemini" || xaiKey != "image-xai" {
		t.Fatalf("resolved keys = %q, %q, %q; want dedicated image keys", openAIKey, geminiKey, xaiKey)
	}
}

func TestResolveAgentImageProviderKeysFallsBackToCodingKeys(t *testing.T) {
	openAIKey, geminiKey, xaiKey := resolveAgentImageProviderKeys(OpenCodeAgentRequest{
		OpenAIKey: " coding-openai ",
		GeminiKey: " coding-gemini ",
		XaiKey:    " coding-xai ",
	})

	if openAIKey != "coding-openai" || geminiKey != "coding-gemini" || xaiKey != "coding-xai" {
		t.Fatalf("resolved keys = %q, %q, %q; want coding-key fallbacks", openAIKey, geminiKey, xaiKey)
	}
}
