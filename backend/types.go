package main

// ====================================================================
// Data structures for the backend

type R1Response struct {
	AIResponse string      `json:"aiResponse"`
	TokenUsage interface{} `json:"tokenUsage"`
	Cost       float64     `json:"cost"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type DrawToCodeRequest struct {
	ImageBase64 string `json:"imageBase64"`
	UserPrompt  string `json:"userPrompt"`
	Prompt      string `json:"prompt"`
	Template    string `json:"template"`
	ImageSource string `json:"imageSource"`
}

type ChatWithAIRequest struct {
	Message           string        `json:"message"`
	PreviousMessages  []ChatMessage `json:"previousMessages"`
	Image             *string       `json:"image,omitempty"`
	AttachmentContext string        `json:"attachmentContext,omitempty"`
	AttachmentData    string        `json:"attachmentData,omitempty"`
	AttachmentMime    string        `json:"attachmentMime,omitempty"`
	Model             string        `json:"model,omitempty"`
	GroqKey           string        `json:"groq,omitempty"`
	ClaudeKey         string        `json:"claudeKey,omitempty"`
	OpenAIKey         string        `json:"openaiKey,omitempty"`
	OpenAIAuthMode    string        `json:"openaiAuthMode,omitempty"`  // "api-key" or "codex-jwt"
	OpenAIAccountID   string        `json:"openaiAccountID,omitempty"` // chatgpt_account_id for JWT mode
	OpenAIModel       string        `json:"openaiModel,omitempty"`     // concrete OpenAI model id (e.g. gpt-5.5)
	XaiKey            string        `json:"xaiKey,omitempty"`
	GeminiKey         string        `json:"geminiKey,omitempty"`
	GeminiModel       string        `json:"geminiModel,omitempty"` // concrete Gemini model id (e.g. gemini-3.1-pro-preview)
	FireworksKey      string        `json:"fireworksKey,omitempty"`
	FireworksModel    string        `json:"fireworksModel,omitempty"` // concrete Fireworks model id (e.g. accounts/fireworks/models/glm-5)
	OpenRouterKey     string        `json:"openrouterKey,omitempty"`
	OpenRouterModel   string        `json:"openrouterModel,omitempty"` // concrete OpenRouter model id (e.g. z-ai/glm-5)
	OpenCodeZenKey    string        `json:"opencodeZenKey,omitempty"`
	OpenCodeZenModel  string        `json:"opencodeZenModel,omitempty"` // concrete OpenCode Zen model id (e.g. kimi-k2.5-free)
	Template          string        `json:"template,omitempty"`
	ImageSource       string        `json:"imageSource,omitempty"`
	UseSearch         bool          `json:"useSearch,omitempty"`
	SearchProvider    string        `json:"searchProvider,omitempty"`
	SearchContext     string        `json:"searchContext,omitempty"`
	EnableMagicEdit   bool          `json:"enableMagicEdit,omitempty"` // Enable magic_edit tool for AI-triggered edits
}

type WebSearchRequest struct {
	Message          string        `json:"message"`
	PreviousMessages []ChatMessage `json:"previousMessages"`
	Model            string        `json:"model,omitempty"`
	OpenAIKey        string        `json:"openaiKey,omitempty"`
	OpenAIAuthMode   string        `json:"openaiAuthMode,omitempty"`
	OpenAIAccountID  string        `json:"openaiAccountID,omitempty"`
}

type AnalyzeVideoRequest struct {
	VideoBase64 string `json:"videoBase64"`
	VideoPath   string `json:"videoPath,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	GeminiKey   string `json:"geminiKey,omitempty"`
}

type AnalyzeVideoResponse struct {
	Analysis        string      `json:"analysis"`
	DurationSeconds float64     `json:"durationSeconds,omitempty"`
	TokenUsage      interface{} `json:"tokenUsage,omitempty"`
	Cost            float64     `json:"cost,omitempty"`
}
