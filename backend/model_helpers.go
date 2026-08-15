package main

import "strings"

const defaultOpenAIModelID = "gpt-5.5"

type openAITextPricing struct {
	InputPerMillion  float64
	OutputPerMillion float64
}

func isOpenAIChatAlias(modelID string) bool {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "gpt-5", "gpt-5.1", "gpt-5.2", "gpt-5.4", "gpt-5.5":
		return true
	default:
		return false
	}
}

func normalizeOpenAIModelID(modelID string) string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return defaultOpenAIModelID
	}
	switch strings.ToLower(trimmed) {
	case "gpt-5", "gpt-5.1", "gpt-5.4":
		return defaultOpenAIModelID
	}
	// ChatGPT Codex rejects spark for many accounts; transparently route to gpt-5.3-codex.
	if strings.EqualFold(trimmed, "gpt-5.3-codex-spark") {
		return "gpt-5.3-codex"
	}
	return trimmed
}

func openAITextPricingForModel(modelID string) (openAITextPricing, bool) {
	switch strings.ToLower(strings.TrimSpace(normalizeOpenAIModelID(modelID))) {
	case "gpt-5.5":
		return openAITextPricing{InputPerMillion: 5.00, OutputPerMillion: 30.00}, true
	case "gpt-5.4":
		return openAITextPricing{InputPerMillion: 2.50, OutputPerMillion: 20.00}, true
	case "gpt-5.4-pro":
		return openAITextPricing{InputPerMillion: 15.00, OutputPerMillion: 120.00}, true
	case "gpt-5.2":
		return openAITextPricing{InputPerMillion: 1.25, OutputPerMillion: 10.00}, true
	default:
		return openAITextPricing{}, false
	}
}

func estimateOpenAITextCost(modelID string, inputTokens, outputTokens int) float64 {
	pricing, ok := openAITextPricingForModel(modelID)
	if !ok {
		return 0
	}
	return (float64(inputTokens) / 1_000_000.0 * pricing.InputPerMillion) +
		(float64(outputTokens) / 1_000_000.0 * pricing.OutputPerMillion)
}

func normalizeGeminiModelID(modelID string) string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return "gemini-3.1-pro-preview"
	}

	switch strings.ToLower(trimmed) {
	case "gemini", "gemini-3-pro", "gemini-3-pro-preview", "gemini-3.1-pro", "gemini-3.1-pro-preview":
		return "gemini-3.1-pro-preview"
	case "gemini-3-flash", "gemini-3-flash-preview":
		return "gemini-3-flash-preview"
	default:
		// Keep forward-compatibility for newly supported Gemini model IDs.
		return trimmed
	}
}

func normalizeFireworksModelID(modelID string) string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return "accounts/fireworks/models/glm-5"
	}

	switch strings.ToLower(trimmed) {
	case "glm-5", "fireworks/glm-5":
		return "accounts/fireworks/models/glm-5"
	case "kimi-k2.5", "kimi-k2p5", "fireworks/kimi-k2.5", "fireworks/kimi-k2p5":
		return "accounts/fireworks/models/kimi-k2p5"
	case "minimax-m2.5", "minimax-m2p5", "minimax-m2.5-free", "minimax-m2p5-free",
		"fireworks/minimax-m2.5", "fireworks/minimax-m2p5", "fireworks/minimax-m2.5-free", "fireworks/minimax-m2p5-free":
		return "accounts/fireworks/models/minimax-m2p5"
	default:
		// Allow full model ids for forward compatibility.
		return trimmed
	}
}

func normalizeOpenRouterModelID(modelID string) string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return "z-ai/glm-5"
	}

	switch strings.ToLower(trimmed) {
	case "glm-5", "fireworks/glm-5", "openrouter/glm-5", "z-ai/glm-5":
		return "z-ai/glm-5"
	case "kimi-k2.5", "kimi-k2p5", "kimi-k2", "fireworks/kimi-k2.5", "fireworks/kimi-k2p5",
		"openrouter/kimi-k2", "openrouter/kimi-k2.5", "moonshotai/kimi-k2", "moonshotai/kimi-k2.5", "moonshotai/kimi-k2-0905":
		return "moonshotai/kimi-k2.5"
	case "minimax-m2", "minimax-m2.5", "minimax-m2p5", "minimax-m2.5-free", "minimax-m2p5-free",
		"fireworks/minimax-m2.5", "fireworks/minimax-m2p5", "openrouter/minimax-m2.5",
		"minimax/minimax-m2", "minimax/minimax-m2.5", "minimax/minimax-m2-250918":
		return "minimax/minimax-m2.5"
	default:
		// Allow full model ids for forward compatibility.
		return trimmed
	}
}

func normalizeOpenCodeZenModelID(modelID string) string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return "kimi-k2.5-free"
	}
	normalized := strings.ToLower(trimmed)
	if strings.HasPrefix(normalized, "opencode/") {
		normalized = strings.TrimPrefix(normalized, "opencode/")
	}

	switch normalized {
	case "glm-5", "glm5", "glm-5-free", "glm5-free":
		return "glm-5-free"
	case "kimi-k2", "kimi-k2.5", "kimi-k2p5", "kimi-k2.5-free", "kimi-k2p5-free":
		return "kimi-k2.5-free"
	case "minimax-m2", "minimax-m2.5", "minimax-m2p5", "minimax-m2.5-free", "minimax-m2p5-free":
		return "minimax-m2.5-free"
	case "big-pickle", "bigpickle":
		return "big-pickle"
	default:
		// Allow full/forward-compatible model ids.
		return normalized
	}
}

func normalizeXAIModelID(modelID string) string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return grokTextModelID
	}

	switch strings.ToLower(trimmed) {
	case "grok", "xai", "grok-4", "grok-4.1", "grok-4-1", "grok-4.1-fast", "grok-4-1-fast",
		"grok-4.1-fast-reasoning", "grok-4-1-fast-reasoning",
		"grok-4.1-reasoning", "grok-4-1-reasoning",
		"grok-4.1-fast-non-reasoning", "grok-4-1-fast-non-reasoning",
		"grok-4-fast", "grok-4-fast-reasoning", "grok-4-fast-non-reasoning",
		"grok-4.3", "grok-4-3", "grok-4.3-latest", "grok-4-3-latest", "grok-latest":
		return grokTextModelID
	default:
		// Allow full/forward-compatible model ids.
		return trimmed
	}
}
