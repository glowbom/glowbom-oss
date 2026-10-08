package main

import "testing"

func TestResolveAgentImageProviderKeysDoesNotUseChatGPTTokenForPlatformImages(t *testing.T) {
	for _, imageKey := range []string{"", "platform-image-key"} {
		key, _, _ := resolveAgentImageProviderKeys(OpenCodeAgentRequest{
			OpenAIAuthMode: "codex-jwt", OpenAIKey: "subscription-token", OpenAIImageKey: imageKey,
		})
		if key != imageKey {
			t.Fatal("ChatGPT token used in place of a platform image key")
		}
	}
}

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
