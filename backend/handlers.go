package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// extractProjectName extracts and removes [PROJECT_NAME: ...] marker from AI response
func extractProjectName(response string) (name string, cleaned string) {
	re := regexp.MustCompile(`\[PROJECT_NAME:\s*([^\]]+)\]`)
	if matches := re.FindStringSubmatch(response); len(matches) > 1 {
		return strings.TrimSpace(matches[1]), strings.TrimSpace(re.ReplaceAllString(response, ""))
	}
	return "", response
}

func chatWithAIHandler(w http.ResponseWriter, r *http.Request) {
	isStream := r.URL.Query().Get("stream") == "true"
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	var req ChatWithAIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.AttachmentContext) != "" {
		req.Message = fmt.Sprintf("%s\n\n[Attached video context]\n%s", req.Message, req.AttachmentContext)
	}

	if req.Model == "" {
		req.Model = "gpt-oss"
	} else if req.Model == "r1" {
		// Legacy DeepSeek-R1 path (temporarily routed through GPT-OSS)
		req.Model = "gpt-oss"
	}
	if req.ImageSource == "" {
		req.ImageSource = "Lorem Picsum"
	}

	if strings.TrimSpace(req.SearchContext) != "" {
		injectSearchContext(&req.PreviousMessages, req.SearchContext)
	} else if req.UseSearch && !isOpenAIChatAlias(req.Model) && !isStream {
		fmt.Printf("[SEARCH] Missing client-provided context, performing server-side search for query %q\n", truncateForLog(req.Message))
		if context, err := performWebSearch(req.PreviousMessages, req.Message, req.OpenAIKey); err != nil {
			fmt.Printf("[SEARCH] performWebSearch failed: %v\n", err)
		} else {
			injectSearchContext(&req.PreviousMessages, context)
		}
	}

	fmt.Printf("[CHAT] Using model: %s (stream=%v)\n", req.Model, isStream)

	// if there's an image => draw to code
	if req.Image != nil && *req.Image != "" {
		// Augment prompt with attachment context for models that cannot accept inline data
		userPrompt := req.Message
		if strings.TrimSpace(req.AttachmentContext) != "" {
			userPrompt = fmt.Sprintf("%s\n\n[Attachment]\n%s", userPrompt, req.AttachmentContext)
		}

		// STREAMING MODE: Check if ?stream=true and route to streaming functions
		if isStream {
			fmt.Printf("[STREAM] Draw-to-code streaming enabled for model: %s\n", req.Model)
			var err error
			if req.Model == "claude" {
				err = callClaudeDrawToCodeStreaming(w, *req.Image, userPrompt,
					req.Template, req.ImageSource, req.ClaudeKey, claudeSonnetDefaultModel)
			} else if req.Model == "claude-opus" {
				err = callClaudeDrawToCodeStreaming(w, *req.Image, userPrompt,
					req.Template, req.ImageSource, req.ClaudeKey, claudeOpusDefaultModel)
			} else if isOpenAIChatAlias(req.Model) {
				if req.OpenAIAuthMode == "codex-jwt" {
					err = callChatGPTCodexDrawToCodeStreaming(w, *req.Image, userPrompt,
						req.Template, req.ImageSource, req.OpenAIKey, req.OpenAIAccountID, req.OpenAIModel)
				} else {
					err = callGPT5DrawToCodeStreaming(w, *req.Image, userPrompt,
						req.Template, req.ImageSource, req.OpenAIKey, req.OpenAIModel)
				}
			} else if req.Model == "gemini" {
				err = callGeminiDrawToCodeStreaming(w, *req.Image, userPrompt,
					req.Template, req.ImageSource, req.GeminiKey, req.AttachmentData, req.AttachmentMime, req.GeminiModel)
			} else if req.Model == "grok" {
				err = callGrok4DrawToCodeStreaming(w, *req.Image, userPrompt,
					req.Template, req.ImageSource, req.XaiKey)
			} else if req.Model == "fireworks" {
				err = callFireworksDrawToCodeStreaming(w, *req.Image, userPrompt,
					req.Template, req.ImageSource, req.FireworksKey, req.FireworksModel)
			} else if req.Model == "openrouter" {
				err = callOpenRouterDrawToCodeStreaming(w, *req.Image, userPrompt,
					req.Template, req.ImageSource, req.OpenRouterKey, req.OpenRouterModel)
			} else if req.Model == "opencode" {
				err = callOpenCodeZenDrawToCodeStreaming(w, *req.Image, userPrompt,
					req.Template, req.ImageSource, req.OpenCodeZenKey, req.OpenCodeZenModel)
			} else if req.Model == "r1-groq" {
				err = callGroqDrawToCodeStreaming(w, *req.Image, userPrompt,
					req.Template, req.ImageSource, req.GroqKey)
			} else if req.Model == "gpt-oss" {
				err = callOllamaDrawToCodeStreaming(w, *req.Image, userPrompt,
					req.Template, req.ImageSource, "gpt-oss:20b")
			} else if req.Model == "qwen3.5" {
				err = callOllamaDrawToCodeStreaming(w, *req.Image, userPrompt,
					req.Template, req.ImageSource, "qwen3.5")
			} else if req.Model == "qwen3-coder" {
				err = callOllamaDrawToCodeStreaming(w, *req.Image, userPrompt,
					req.Template, req.ImageSource, "qwen3-coder:30b")
			}

			if err != nil {
				errData := map[string]interface{}{
					"error": err.Error(),
					"done":  true,
				}
				errBytes, _ := json.Marshal(errData)
				fmt.Fprintf(w, "data: %s\n\n", errBytes)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			return
		}

		// BLOCKING MODE (existing code, unchanged)
		if req.Model == "r1-groq" {
			if req.Image != nil && *req.Image != "" {
				respData, err := callR1GroqDrawToCodeApiFull(
					*req.Image, userPrompt, // userPrompt + prompt
					"", req.Template, req.ImageSource, req.GroqKey,
				)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				projectName, cleanedResponse := extractProjectName(respData.AIResponse)
				resp := map[string]interface{}{
					"message":     req.Message,
					"aiResponse":  cleanedResponse,
					"projectName": projectName,
					"tokenUsage":  respData.TokenUsage,
					"cost":        respData.Cost,
				}
				writeJSON(w, resp)
				return
			} else {
				respData, err := callR1GroqApiGo(req.PreviousMessages, req.Message, req.GroqKey)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				projectName, cleanedResponse := extractProjectName(respData.AIResponse)
				resp := map[string]interface{}{
					"message":     req.Message,
					"aiResponse":  cleanedResponse,
					"projectName": projectName,
					"tokenUsage":  respData.TokenUsage,
					"cost":        respData.Cost,
				}
				writeJSON(w, resp)
				return
			}
		} else if req.Model == "gpt-oss" {
			// Local draw-to-code using Ollama chat API with gpt-oss:20b
			respData, err := callOllamaChatDrawToCodeWithModel(
				*req.Image, userPrompt,
				req.Template, req.ImageSource,
				"gpt-oss:20b",
			)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			projectName, cleanedResponse := extractProjectName(respData.AIResponse)
			resp := map[string]interface{}{
				"message":     req.Message,
				"aiResponse":  cleanedResponse,
				"projectName": projectName,
				"tokenUsage":  respData.TokenUsage,
				"cost":        respData.Cost,
			}
			writeJSON(w, resp)
			return
		} else if req.Model == "qwen3.5" {
			// Local draw-to-code using Ollama chat API with qwen3.5
			respData, err := callOllamaChatDrawToCodeWithModel(
				*req.Image, userPrompt,
				req.Template, req.ImageSource,
				"qwen3.5",
			)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			projectName, cleanedResponse := extractProjectName(respData.AIResponse)
			resp := map[string]interface{}{
				"message":     req.Message,
				"aiResponse":  cleanedResponse,
				"projectName": projectName,
				"tokenUsage":  respData.TokenUsage,
				"cost":        respData.Cost,
			}
			writeJSON(w, resp)
			return
		} else if req.Model == "qwen3-coder" {
			// Local draw-to-code using Ollama chat API with qwen3-coder:30b
			respData, err := callOllamaChatDrawToCodeWithModel(
				*req.Image, userPrompt,
				req.Template, req.ImageSource,
				"qwen3-coder:30b",
			)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			projectName, cleanedResponse := extractProjectName(respData.AIResponse)
			resp := map[string]interface{}{
				"message":     req.Message,
				"aiResponse":  cleanedResponse,
				"projectName": projectName,
				"tokenUsage":  respData.TokenUsage,
				"cost":        respData.Cost,
			}
			writeJSON(w, resp)
			return
		} else if req.Model == "claude" {
			// Draw-to-code using Claude Sonnet 4.6 API
			respData, err := callClaudeDrawToCodeApiFull(
				*req.Image, userPrompt,
				req.Template, req.ImageSource,
				req.ClaudeKey,
			)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			projectName, cleanedResponse := extractProjectName(respData.AIResponse)
			resp := map[string]interface{}{
				"message":     req.Message,
				"aiResponse":  cleanedResponse,
				"projectName": projectName,
				"tokenUsage":  respData.TokenUsage,
				"cost":        respData.Cost,
			}
			writeJSON(w, resp)
			return
		} else if req.Model == "claude-opus" {
			// Draw-to-code using Claude Opus 4.7 API
			respData, err := callClaudeDrawToCodeApiFullWithModel(
				*req.Image, userPrompt,
				req.Template, req.ImageSource,
				req.ClaudeKey,
				claudeOpusDefaultModel,
			)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			projectName, cleanedResponse := extractProjectName(respData.AIResponse)
			resp := map[string]interface{}{
				"message":     req.Message,
				"aiResponse":  cleanedResponse,
				"projectName": projectName,
				"tokenUsage":  respData.TokenUsage,
				"cost":        respData.Cost,
			}
			writeJSON(w, resp)
			return
		} else if isOpenAIChatAlias(req.Model) {
			// Draw-to-code using the selected OpenAI model.
			var respData *R1Response
			var err error
			if req.OpenAIAuthMode == "codex-jwt" {
				respData, err = callChatGPTCodexNonStreaming(nil, userPrompt, req.OpenAIKey, req.OpenAIAccountID, req.OpenAIModel)
			} else {
				respData, err = callGPT5DrawToCodeApiFull(
					*req.Image, userPrompt,
					req.Template, req.ImageSource,
					req.OpenAIKey,
					req.OpenAIModel,
				)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			projectName, cleanedResponse := extractProjectName(respData.AIResponse)
			resp := map[string]interface{}{
				"message":     req.Message,
				"aiResponse":  cleanedResponse,
				"projectName": projectName,
				"tokenUsage":  respData.TokenUsage,
				"cost":        respData.Cost,
			}
			writeJSON(w, resp)
			return
		} else if req.Model == "grok" {
			// Draw-to-code using Grok 4.3.
			respData, err := callGrok4DrawToCodeApiFull(
				*req.Image, userPrompt,
				req.Template, req.ImageSource,
				req.XaiKey,
			)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			projectName, cleanedResponse := extractProjectName(respData.AIResponse)
			resp := map[string]interface{}{
				"message":     req.Message,
				"aiResponse":  cleanedResponse,
				"projectName": projectName,
				"tokenUsage":  respData.TokenUsage,
				"cost":        respData.Cost,
			}
			writeJSON(w, resp)
			return
		} else if req.Model == "gemini" {
			// Draw-to-code using Gemini 3.1 Pro (with optional attachment)
			respData, err := callGeminiDrawToCodeApiFull(
				*req.Image, req.Message,
				req.Template, req.ImageSource,
				req.GeminiKey,
				req.AttachmentData, req.AttachmentMime,
				req.GeminiModel,
			)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			projectName, cleanedResponse := extractProjectName(respData.AIResponse)
			resp := map[string]interface{}{
				"message":     req.Message,
				"aiResponse":  cleanedResponse,
				"projectName": projectName,
				"tokenUsage":  respData.TokenUsage,
				"cost":        respData.Cost,
			}
			writeJSON(w, resp)
			return
		} else if req.Model == "fireworks" {
			// Draw-to-code using Fireworks (GLM-5)
			respData, err := callFireworksDrawToCodeApiFull(
				*req.Image, userPrompt,
				req.Template, req.ImageSource,
				req.FireworksKey, req.FireworksModel,
			)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			projectName, cleanedResponse := extractProjectName(respData.AIResponse)
			resp := map[string]interface{}{
				"message":     req.Message,
				"aiResponse":  cleanedResponse,
				"projectName": projectName,
				"tokenUsage":  respData.TokenUsage,
				"cost":        respData.Cost,
			}
			writeJSON(w, resp)
			return
		} else if req.Model == "openrouter" {
			// Draw-to-code using OpenRouter
			respData, err := callOpenRouterDrawToCodeApiFull(
				*req.Image, userPrompt,
				req.Template, req.ImageSource,
				req.OpenRouterKey, req.OpenRouterModel,
			)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			projectName, cleanedResponse := extractProjectName(respData.AIResponse)
			resp := map[string]interface{}{
				"message":     req.Message,
				"aiResponse":  cleanedResponse,
				"projectName": projectName,
				"tokenUsage":  respData.TokenUsage,
				"cost":        respData.Cost,
			}
			writeJSON(w, resp)
			return
		} else if req.Model == "opencode" {
			// Draw-to-code using OpenCode Zen
			respData, err := callOpenCodeZenDrawToCodeApiFull(
				*req.Image, userPrompt,
				req.Template, req.ImageSource,
				req.OpenCodeZenKey, req.OpenCodeZenModel,
			)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			projectName, cleanedResponse := extractProjectName(respData.AIResponse)
			resp := map[string]interface{}{
				"message":     req.Message,
				"aiResponse":  cleanedResponse,
				"projectName": projectName,
				"tokenUsage":  respData.TokenUsage,
				"cost":        respData.Cost,
			}
			writeJSON(w, resp)
			return
		}
	} else {
		// normal chat
		if req.Model == "r1-groq" {
			if isStream {
				err := callGroqGPTOSSChatStreaming(w, req.PreviousMessages, req.Message, req.GroqKey)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			}
			respData, err := callR1GroqApiGo(req.PreviousMessages, req.Message, req.GroqKey)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			resp := map[string]interface{}{
				"message":    req.Message,
				"aiResponse": respData.AIResponse,
				"tokenUsage": respData.TokenUsage,
				"cost":       respData.Cost,
			}
			writeJSON(w, resp)
			return
		} else if req.Model == "gpt-oss" {
			if isStream {
				err := callOllamaChatWithModelStreaming(w, req.PreviousMessages, req.Message, "gpt-oss:20b", "")
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			} else {
				respData, err := callOllamaChatWithModel(req.PreviousMessages, req.Message, "gpt-oss:20b", "")
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				fmt.Println("req.AIResponse: ", respData.AIResponse)
				writeJSON(w, resp)
				return
			}
		} else if req.Model == "qwen3.5" {
			if isStream {
				err := callOllamaChatWithModelStreamingAndAttachment(
					w,
					req.PreviousMessages,
					req.Message,
					"qwen3.5",
					"",
					req.AttachmentData,
					req.AttachmentMime,
				)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			} else {
				respData, err := callOllamaChatWithModelAndAttachment(
					req.PreviousMessages,
					req.Message,
					"qwen3.5",
					"",
					req.AttachmentData,
					req.AttachmentMime,
				)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				fmt.Println("req.AIResponse: ", respData.AIResponse)
				writeJSON(w, resp)
				return
			}
		} else if req.Model == "qwen3-coder" {
			if isStream {
				err := callOllamaChatWithModelStreaming(w, req.PreviousMessages, req.Message, "qwen3-coder:30b", "")
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			} else {
				respData, err := callOllamaChatWithModel(req.PreviousMessages, req.Message, "qwen3-coder:30b", "")
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				fmt.Println("req.AIResponse: ", respData.AIResponse)
				writeJSON(w, resp)
				return
			}
		} else if req.Model == "claude" {
			if isStream {
				// Use streaming with Extended Thinking support (Sonnet 4.6)
				var err error
				if req.EnableMagicEdit {
					// Include magic_edit tool for AI-triggered edits
					tools := []map[string]interface{}{getMagicEditTool()}
					err = callClaudeChatStreamingWithModelAndTools(w, req.PreviousMessages, req.Message, req.ClaudeKey, claudeSonnetDefaultModel, tools)
				} else {
					err = callClaudeChatStreaming(w, req.PreviousMessages, req.Message, req.ClaudeKey)
				}
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			} else {
				respData, err := callClaudeApiGoWithModel(req.PreviousMessages, req.Message, req.ClaudeKey, claudeSonnetDefaultModel)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				writeJSON(w, resp)
				return
			}
		} else if req.Model == "claude-opus" {
			if isStream {
				// Use streaming with Extended Thinking support (Opus 4.7)
				var err error
				if req.EnableMagicEdit {
					// Include magic_edit tool for AI-triggered edits
					tools := []map[string]interface{}{getMagicEditTool()}
					err = callClaudeChatStreamingWithModelAndTools(w, req.PreviousMessages, req.Message, req.ClaudeKey, claudeOpusDefaultModel, tools)
				} else {
					err = callClaudeChatStreamingWithModel(w, req.PreviousMessages, req.Message, req.ClaudeKey, claudeOpusDefaultModel)
				}
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			} else {
				// Non-streaming Opus chat not commonly used, but supported
				respData, err := callClaudeApiGoWithModel(req.PreviousMessages, req.Message, req.ClaudeKey, claudeOpusDefaultModel)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				writeJSON(w, resp)
				return
			}
		} else if isOpenAIChatAlias(req.Model) {
			if req.UseSearch && strings.TrimSpace(req.SearchContext) == "" && req.OpenAIAuthMode == "codex-jwt" {
				// Web search requires the standard OpenAI API, not supported via ChatGPT JWT
				fmt.Println("[SEARCH] Web search skipped: codex-jwt auth mode does not support web search API")
			} else if req.UseSearch && strings.TrimSpace(req.SearchContext) == "" {
				respData, err := callGPT5Search(req.PreviousMessages, req.Message, req.OpenAIKey)
				if err != nil {
					fmt.Printf("[SEARCH] OpenAI web search failed: %v\n", err)
					searchError := fmt.Sprintf("Search is unavailable right now (details: %v). Please try again later or disable search.", err)
					if isOpenAIMissingModelScope(err.Error()) {
						searchError = openAIModelScopeHelpMessage()
					}
					fallback := map[string]interface{}{
						"message":    req.Message,
						"aiResponse": searchError,
						"tokenUsage": nil,
						"cost":       0,
					}
					writeJSON(w, fallback)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				writeJSON(w, resp)
				return
			}
			if isStream {
				var err error
				if req.OpenAIAuthMode == "codex-jwt" {
					err = callChatGPTCodexStreaming(w, req.PreviousMessages, req.Message, req.OpenAIKey, req.OpenAIAccountID, req.OpenAIModel)
				} else {
					// Use Responses API for streaming with reasoning support
					err = callGPT5ResponsesAPIStreaming(w, req.PreviousMessages, req.Message, req.OpenAIKey, req.OpenAIModel)
				}
				if err != nil {
					if isOpenAIMissingModelScope(err.Error()) {
						msg := map[string]string{"error": openAIModelScopeHelpMessage()}
						msgBytes, _ := json.Marshal(msg)
						fmt.Fprintf(w, "data: %s\n\n", msgBytes)
						if f, ok := w.(http.Flusher); ok {
							f.Flush()
						}
						return
					}
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			} else {
				var respData *R1Response
				var err error
				if req.OpenAIAuthMode == "codex-jwt" {
					respData, err = callChatGPTCodexNonStreaming(req.PreviousMessages, req.Message, req.OpenAIKey, req.OpenAIAccountID, req.OpenAIModel)
				} else {
					respData, err = callGPT5ApiGo(req.PreviousMessages, req.Message, req.OpenAIKey, req.OpenAIModel)
				}
				if err != nil {
					if isOpenAIMissingModelScope(err.Error()) {
						fallback := map[string]interface{}{
							"message":    req.Message,
							"aiResponse": openAIModelScopeHelpMessage(),
							"tokenUsage": nil,
							"cost":       0,
						}
						writeJSON(w, fallback)
						return
					}
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				writeJSON(w, resp)
				return
			}
		} else if req.Model == "grok" {
			if isStream {
				// Use streaming with chain of thought support
				err := callGrok4ChatStreaming(w, req.PreviousMessages, req.Message, req.XaiKey)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			} else {
				respData, err := callGrok4ApiGo(req.PreviousMessages, req.Message, req.XaiKey)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				writeJSON(w, resp)
				return
			}
		} else if req.Model == "gemini" {
			if isStream {
				var err error
				if req.EnableMagicEdit {
					// Include magic_edit tool for AI-triggered edits
					tools := []map[string]interface{}{getGeminiMagicEditTool()}
					err = callGeminiChatStreamingWithTools(w, req.PreviousMessages, req.Message, req.GeminiKey, req.AttachmentData, req.AttachmentMime, tools, req.GeminiModel)
				} else {
					err = callGeminiChatStreaming(w, req.PreviousMessages, req.Message, req.GeminiKey, req.AttachmentData, req.AttachmentMime, req.GeminiModel)
				}
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			} else {
				// If attachment data is present, include it in the chat call
				if strings.TrimSpace(req.AttachmentData) != "" && strings.TrimSpace(req.AttachmentMime) != "" {
					respData, err := callGeminiApiGoWithAttachment(req.PreviousMessages, req.Message, req.AttachmentData, req.AttachmentMime, req.GeminiKey, req.GeminiModel)
					if err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					resp := map[string]interface{}{
						"message":    req.Message,
						"aiResponse": respData.AIResponse,
						"tokenUsage": respData.TokenUsage,
						"cost":       respData.Cost,
					}
					writeJSON(w, resp)
					return
				}
				respData, err := callGeminiApiGo(req.PreviousMessages, req.Message, req.GeminiKey, req.GeminiModel)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				writeJSON(w, resp)
				return
			}
		} else if req.Model == "fireworks" {
			if isStream {
				err := callFireworksChatStreaming(w, req.PreviousMessages, req.Message, req.FireworksKey, req.FireworksModel, req.AttachmentData, req.AttachmentMime)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			} else {
				respData, err := callFireworksApiGo(req.PreviousMessages, req.Message, req.FireworksKey, req.FireworksModel, req.AttachmentData, req.AttachmentMime)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				writeJSON(w, resp)
				return
			}
		} else if req.Model == "openrouter" {
			if isStream {
				err := callOpenRouterChatStreaming(w, req.PreviousMessages, req.Message, req.OpenRouterKey, req.OpenRouterModel, req.AttachmentData, req.AttachmentMime)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			} else {
				respData, err := callOpenRouterApiGo(req.PreviousMessages, req.Message, req.OpenRouterKey, req.OpenRouterModel, req.AttachmentData, req.AttachmentMime)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				writeJSON(w, resp)
				return
			}
		} else if req.Model == "opencode" {
			if isStream {
				err := callOpenCodeZenChatStreaming(w, req.PreviousMessages, req.Message, req.OpenCodeZenKey, req.OpenCodeZenModel, req.AttachmentData, req.AttachmentMime)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				return
			} else {
				respData, err := callOpenCodeZenApiGo(req.PreviousMessages, req.Message, req.OpenCodeZenKey, req.OpenCodeZenModel, req.AttachmentData, req.AttachmentMime)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp := map[string]interface{}{
					"message":    req.Message,
					"aiResponse": respData.AIResponse,
					"tokenUsage": respData.TokenUsage,
					"cost":       respData.Cost,
				}
				writeJSON(w, resp)
				return
			}
		}

	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func webSearchHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req WebSearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.Message) == "" {
		http.Error(w, "Empty search query", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.OpenAIKey) == "" {
		http.Error(w, "Missing OpenAI key", http.StatusBadRequest)
		return
	}

	context, err := performWebSearch(req.PreviousMessages, req.Message, req.OpenAIKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := map[string]string{"searchContext": context}
	writeJSON(w, resp)
}

func analyzeVideoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AnalyzeVideoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	videoData := strings.TrimSpace(req.VideoBase64)
	if videoData == "" && strings.TrimSpace(req.VideoPath) == "" {
		http.Error(w, "videoBase64 or videoPath is required", http.StatusBadRequest)
		return
	}

	mimeType := strings.TrimSpace(req.MimeType)
	if mimeType == "" {
		mimeType = "video/mp4"
	}

	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = "Analyze the attached video and summarize what the user explains or demonstrates. Capture clear instructions, actions on screen, objects involved, and any observed emotions or tone (e.g., excited, frustrated). Return a concise bullet list plus a one-line headline we can attach to their chat message."
	}

	if videoData == "" && req.VideoPath != "" {
		data, err := os.ReadFile(req.VideoPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to read videoPath: %v", err), http.StatusBadRequest)
			return
		}
		videoData = base64.StdEncoding.EncodeToString(data)
		fmt.Printf("[analyzeVideo] read file %s size=%.2fMB\n", req.VideoPath, float64(len(data))/1_048_576.0)
	}

	fmt.Printf("[analyzeVideo] payload size=%.2fMB mime=%s\n", float64(len(videoData))/1_048_576.0, mimeType)

	respData, err := callGeminiVideoAnalysis(videoData, mimeType, prompt, req.GeminiKey)
	if err != nil {
		fmt.Printf("[analyzeVideo] error: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := AnalyzeVideoResponse{
		Analysis:   respData.AIResponse,
		TokenUsage: respData.TokenUsage,
		Cost:       respData.Cost,
	}
	writeJSON(w, response)
}

// A simple node-graph in "API format" for ComfyUI.
// Each key is a node ID (string), with a "class_type" and "inputs".
var baseNodeGraph = map[string]interface{}{
	"5": map[string]interface{}{
		"class_type": "EmptyLatentImage",
		"inputs": map[string]interface{}{
			"width":      512,
			"height":     512,
			"batch_size": 1,
		},
	},
	"6": map[string]interface{}{
		"class_type": "CLIPTextEncode",
		"inputs": map[string]interface{}{
			// We'll replace "text" dynamically:
			"text": "placeholder",
			"clip": []interface{}{"11", 0},
		},
	},
	"8": map[string]interface{}{
		"class_type": "VAEDecode",
		"inputs": map[string]interface{}{
			"samples": []interface{}{"13", 0},
			"vae":     []interface{}{"10", 0},
		},
	},
	"9": map[string]interface{}{
		"class_type": "SaveImage",
		"inputs": map[string]interface{}{
			"filename_prefix": "ComfyUI",
			"images":          []interface{}{"8", 0},
		},
	},
	"10": map[string]interface{}{
		"class_type": "VAELoader",
		"inputs": map[string]interface{}{
			"vae_name": "diffusion_pytorch_model.safetensors",
		},
	},
	"11": map[string]interface{}{
		"class_type": "DualCLIPLoader",
		"inputs": map[string]interface{}{
			"clip_name1": "t5xxl_fp8_e4m3fn.safetensors",
			"clip_name2": "clip_l.safetensors",
			"type":       "flux",
			"device":     "default",
		},
	},
	"12": map[string]interface{}{
		"class_type": "UNETLoader",
		"inputs": map[string]interface{}{
			"unet_name":    "flux1-dev.safetensors",
			"weight_dtype": "default",
		},
	},
	"13": map[string]interface{}{
		"class_type": "SamplerCustomAdvanced",
		"inputs": map[string]interface{}{
			"noise":        []interface{}{"25", 0},
			"guider":       []interface{}{"22", 0},
			"sampler":      []interface{}{"16", 0},
			"sigmas":       []interface{}{"17", 0},
			"latent_image": []interface{}{"5", 0},
		},
	},
	"16": map[string]interface{}{
		"class_type": "KSamplerSelect",
		"inputs": map[string]interface{}{
			"sampler_name": "euler",
		},
	},
	"17": map[string]interface{}{
		"class_type": "BasicScheduler",
		"inputs": map[string]interface{}{
			"scheduler": "simple",
			"steps":     20,
			"denoise":   1,
			"model":     []interface{}{"12", 0},
		},
	},
	"22": map[string]interface{}{
		"class_type": "BasicGuider",
		"inputs": map[string]interface{}{
			"model":        []interface{}{"12", 0},
			"conditioning": []interface{}{"6", 0},
		},
	},
	"25": map[string]interface{}{
		"class_type": "RandomNoise",
		"inputs": map[string]interface{}{
			"noise_seed": 377605356023600,
		},
	},
}

// generateImageHandler uses a "history" poll approach with fixes to read status.completed
// and handle subfolder from the final image object.
func generateImageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// 1) Parse { "prompt": "...", "imageSource": "...", "aspectRatio": "...", "openaiKey": "...", "geminiKey": "...", "xaiKey": "...", "referenceImage": "...", "forcePersonalization": false }
	var reqBody struct {
		Prompt               string  `json:"prompt"`
		ImageSource          string  `json:"imageSource,omitempty"`
		AspectRatio          string  `json:"aspectRatio,omitempty"`
		OutputFormat         string  `json:"outputFormat,omitempty"`
		OpenAIKey            string  `json:"openaiKey,omitempty"`
		GeminiKey            string  `json:"geminiKey,omitempty"`
		XaiKey               string  `json:"xaiKey,omitempty"`
		ReferenceImage       *string `json:"referenceImage,omitempty"`
		ForcePersonalization bool    `json:"forcePersonalization,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil || reqBody.Prompt == "" {
		http.Error(w, "Missing prompt", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(reqBody.AspectRatio) != "" {
		reqBody.AspectRatio = normalizeImageAspectRatio(reqBody.AspectRatio)
	}
	fmt.Println("[DEBUG] Received prompt:", reqBody.Prompt)

	// 2) Check if using the OpenAI GPT Image family.
	if isOpenAIImageSource(reqBody.ImageSource) && reqBody.OpenAIKey != "" {
		fmt.Printf("[DEBUG] Using OpenAI (%s) for image generation\n", openAIImageModelID)

		var dataURI string
		var err error

		// Auto-detect if we should use reference image (unless forced)
		useRef := reqBody.ReferenceImage != nil && *reqBody.ReferenceImage != "" && (reqBody.ForcePersonalization || shouldUseReferenceImage(reqBody.Prompt))

		if useRef {
			fmt.Println("[DEBUG] Using reference image for personalization")
			dataURI, err = callOpenAIImageGenerationWithReference(reqBody.Prompt, *reqBody.ReferenceImage, reqBody.AspectRatio, reqBody.OutputFormat, reqBody.OpenAIKey)
		} else {
			fmt.Println("[DEBUG] Using standard generation")
			dataURI, err = callOpenAIImageGeneration(reqBody.Prompt, reqBody.AspectRatio, reqBody.OutputFormat, reqBody.OpenAIKey)
		}

		if err != nil {
			msg := fmt.Sprintf("[ERROR] OpenAI image generation failed: %v", err)
			fmt.Println(msg)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		// Return response in same format as ComfyUI
		respJSON := map[string]interface{}{
			"prompt":     reqBody.Prompt,
			"filename":   openAIImageModelID + ".png",
			"subfolder":  "",
			"saved_path": "",
			"image":      dataURI,
		}
		json.NewEncoder(w).Encode(respJSON)
		return
	}

	// 2b) Check if using Nano Banana family (Gemini)
	if strings.Contains(strings.ToLower(strings.TrimSpace(reqBody.ImageSource)), "nano banana") && reqBody.GeminiKey != "" {
		fmt.Println("[DEBUG] Using Gemini (Nano Banana 2) for image generation")

		var dataURI string
		var err error

		// Auto-detect if we should use reference image (unless forced)
		useRef := reqBody.ReferenceImage != nil && *reqBody.ReferenceImage != "" && (reqBody.ForcePersonalization || shouldUseReferenceImage(reqBody.Prompt))

		if useRef {
			fmt.Println("[DEBUG] Using reference image for personalization")
			dataURI, err = callGeminiImageGenerationWithReference(reqBody.Prompt, *reqBody.ReferenceImage, reqBody.AspectRatio, reqBody.OutputFormat, reqBody.GeminiKey)
		} else {
			fmt.Println("[DEBUG] Using standard generation")
			dataURI, err = callGeminiImageGeneration(reqBody.Prompt, reqBody.AspectRatio, reqBody.OutputFormat, reqBody.GeminiKey)
		}

		if err != nil {
			msg := fmt.Sprintf("[ERROR] Gemini image generation failed: %v", err)
			fmt.Println(msg)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		// Return response in same format as ComfyUI
		respJSON := map[string]interface{}{
			"prompt":     reqBody.Prompt,
			"filename":   "nano-banana.png",
			"subfolder":  "",
			"saved_path": "",
			"image":      dataURI,
		}
		json.NewEncoder(w).Encode(respJSON)
		return
	}

	// 2c) Check if using Grok Imagine Image Quality (xAI)
	if isGrokImagineImageSource(reqBody.ImageSource) && reqBody.XaiKey != "" {
		fmt.Println("[DEBUG] Using xAI Grok Imagine Image Quality for image generation")

		var dataURI string
		var err error

		// Auto-detect if we should use reference image (unless forced)
		useRef := reqBody.ReferenceImage != nil && *reqBody.ReferenceImage != "" && (reqBody.ForcePersonalization || shouldUseReferenceImage(reqBody.Prompt))

		if useRef {
			fmt.Println("[DEBUG] Using reference image for personalization")
			dataURI, err = callGrokImageGenerationWithReference(reqBody.Prompt, *reqBody.ReferenceImage, reqBody.XaiKey, reqBody.AspectRatio)
		} else {
			fmt.Println("[DEBUG] Using standard generation")
			dataURI, err = callGrokImageGeneration(reqBody.Prompt, reqBody.XaiKey, reqBody.AspectRatio)
		}

		if err != nil {
			msg := fmt.Sprintf("[ERROR] xAI Grok Imagine image generation failed: %v", err)
			fmt.Println(msg)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		// Return response in same format as ComfyUI
		respJSON := map[string]interface{}{
			"prompt":     reqBody.Prompt,
			"filename":   xAIImageOutputFilename,
			"subfolder":  "",
			"saved_path": "",
			"image":      dataURI,
		}
		json.NewEncoder(w).Encode(respJSON)
		return
	}

	// 3) Otherwise use ComfyUI (existing flow)

	// 2) Clone the base node graph
	rawBase, _ := json.Marshal(baseNodeGraph) // baseNodeGraph is your global map
	nodeGraph := map[string]interface{}{}
	if err := json.Unmarshal(rawBase, &nodeGraph); err != nil {
		http.Error(w, "Cannot copy baseNodeGraph", http.StatusInternalServerError)
		return
	}

	// 3) Insert user prompt into node "6"
	sixNode := nodeGraph["6"].(map[string]interface{})
	sixInputs := sixNode["inputs"].(map[string]interface{})
	sixInputs["text"] = reqBody.Prompt
	fmt.Println("[DEBUG] Updated node 6 text to:", reqBody.Prompt)

	// 4) Wrap in top-level "prompt" + optionally "client_id"
	finalPayload := map[string]interface{}{
		"client_id": "glowbom-client", // optional, can be any string
		"prompt":    nodeGraph,
	}
	payloadBytes, _ := json.Marshal(finalPayload)

	// 5) POST to /prompt?queue=true
	postURL := "http://127.0.0.1:8000/prompt"
	fmt.Println("[DEBUG] Sending POST to:", postURL)

	postReq, err := http.NewRequest("POST", postURL, bytes.NewReader(payloadBytes))
	if err != nil {
		msg := fmt.Sprintf("[ERROR] Creating ComfyUI request: %v", err)
		fmt.Println(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	postReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		msg := fmt.Sprintf("[ERROR] Calling ComfyUI: %v", err)
		fmt.Println(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		msg := fmt.Sprintf("ComfyUI returned %d: %s", resp.StatusCode, string(body))
		fmt.Println("[ERROR]", msg)
		http.Error(w, msg, resp.StatusCode)
		return
	}

	// 6) Parse immediate response to get prompt_id
	var firstResp struct {
		PromptID string `json:"prompt_id"`
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	fmt.Println("[DEBUG] Immediate POST response:", string(bodyBytes))

	if err := json.Unmarshal(bodyBytes, &firstResp); err != nil {
		msg := "[ERROR] No prompt_id in immediate response"
		fmt.Println(msg, err)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	if firstResp.PromptID == "" {
		msg := "[ERROR] ComfyUI didn't return a prompt_id"
		fmt.Println(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	fmt.Println("[DEBUG] Got prompt_id:", firstResp.PromptID)

	// 7) Poll /history/<prompt_id> up to 10 minutes
	const maxWait = 600 * time.Second
	deadline := time.Now().Add(maxWait)

	var finalFilename, finalSubfolder, finalType string

pollLoop:
	for {
		if time.Now().After(deadline) {
			msg := "[ERROR] Timeout waiting for ComfyUI job to finish"
			fmt.Println(msg)
			http.Error(w, msg, http.StatusGatewayTimeout)
			return
		}

		fmt.Println("[DEBUG] Sleeping 15s before next /history check...")
		time.Sleep(15 * time.Second)

		histURL := fmt.Sprintf("http://127.0.0.1:8000/history/%s", firstResp.PromptID)
		fmt.Println("[DEBUG] GET", histURL)
		histResp, err := http.Get(histURL)
		if err != nil {
			msg := fmt.Sprintf("[ERROR] Polling /history: %v", err)
			fmt.Println(msg)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}
		if histResp.StatusCode != 200 {
			fmt.Printf("[DEBUG] /history responded %d; continuing...\n", histResp.StatusCode)
			histResp.Body.Close()
			continue
		}

		bodyData, _ := io.ReadAll(histResp.Body)
		histResp.Body.Close()
		fmt.Println("[DEBUG] /history response JSON:", string(bodyData))

		var histData map[string]interface{}
		if err := json.Unmarshal(bodyData, &histData); err != nil {
			fmt.Println("[DEBUG] JSON unmarshal error:", err)
			continue
		}

		jobData, ok := histData[firstResp.PromptID].(map[string]interface{})
		if !ok {
			fmt.Println("[DEBUG] No key for prompt_id in history yet; continuing...")
			continue
		}

		// Check status.completed
		statusMap, _ := jobData["status"].(map[string]interface{})
		if statusMap == nil {
			fmt.Println("[DEBUG] No 'status' map yet; continuing...")
			continue
		}
		doneVal, _ := statusMap["completed"].(bool)
		fmt.Println("[DEBUG] status.completed =", doneVal)
		if !doneVal {
			fmt.Println("[DEBUG] Not finished yet; continuing...")
			continue
		}

		// job is done => parse outputs
		outMap, _ := jobData["outputs"].(map[string]interface{})
		if outMap == nil {
			fmt.Println("[DEBUG] No 'outputs' in jobData; continuing...")
			continue
		}
		// we assume SaveImage node is ID "9"
		node9, _ := outMap["9"].(map[string]interface{})
		if node9 == nil {
			fmt.Println("[DEBUG] 'outputs' has no node9; continuing...")
			continue
		}
		images, _ := node9["images"].([]interface{})
		if len(images) == 0 {
			fmt.Println("[DEBUG] node9 has no images; continuing...")
			continue
		}

		// read first image object
		firstImg := images[0].(map[string]interface{})
		fname, _ := firstImg["filename"].(string)
		subf, _ := firstImg["subfolder"].(string)
		typ, _ := firstImg["type"].(string)
		if fname == "" {
			fmt.Println("[DEBUG] no 'filename' in images[0]; continuing...")
			continue
		}

		finalFilename = fname
		finalSubfolder = subf
		finalType = typ
		fmt.Printf("[DEBUG] Found finalFilename=%s, subfolder=%s, type=%s\n", fname, subf, typ)

		// break out of poll loop
		break pollLoop
	}

	if finalFilename == "" {
		msg := "[ERROR] Job finished but no images found in history"
		fmt.Println(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}

	// 8) Now fetch final image from /view?filename=..., subfolder=..., type=...
	viewURL := fmt.Sprintf("http://127.0.0.1:8000/view?filename=%s&subfolder=%s&type=%s",
		finalFilename, finalSubfolder, finalType)
	fmt.Println("[DEBUG] GET final image from:", viewURL)

	viewResp, err := http.Get(viewURL)
	if err != nil {
		msg := fmt.Sprintf("[ERROR] fetching final image /view: %v", err)
		fmt.Println(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	if viewResp.StatusCode != 200 {
		body, _ := io.ReadAll(viewResp.Body)
		viewResp.Body.Close()
		msg := fmt.Sprintf("[ERROR] /view responded %d: %s", viewResp.StatusCode, string(body))
		fmt.Println(msg)
		http.Error(w, msg, viewResp.StatusCode)
		return
	}

	finalImageBytes, _ := io.ReadAll(viewResp.Body)
	viewResp.Body.Close()
	fmt.Printf("[DEBUG] Fetched %d bytes from /view\n", len(finalImageBytes))

	// 9) Base64-encode & store locally
	b64 := base64.StdEncoding.EncodeToString(finalImageBytes)
	if err := os.MkdirAll("saved_images", 0755); err != nil {
		fmt.Println("[ERROR] Creating saved_images folder:", err)
	}
	path := filepath.Join("saved_images", fmt.Sprintf("image_%d.png", time.Now().UnixNano()))
	err = os.WriteFile(path, finalImageBytes, 0644)
	if err != nil {
		msg := fmt.Sprintf("[ERROR] writing image file: %v", err)
		fmt.Println(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	fmt.Println("[DEBUG] Wrote final image to:", path)

	// 10) Return JSON
	respJSON := map[string]interface{}{
		"prompt":     reqBody.Prompt,
		"filename":   finalFilename,
		"subfolder":  finalSubfolder,
		"saved_path": path,
		"image":      "data:image/png;base64," + b64,
	}
	fmt.Println("[DEBUG] Done. Returning JSON to client.")
	json.NewEncoder(w).Encode(respJSON)
}

func normalizeImageAspectRatio(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	switch strings.ToLower(trimmed) {
	case "square", "1x1", "1:1":
		return "1:1"
	case "portrait", "9:16":
		return "9:16"
	case "landscape", "16:9":
		return "16:9"
	case "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "21:9":
		return strings.ToLower(trimmed)
	default:
		// Defensive fallback for strict providers (e.g., Gemini).
		return "1:1"
	}
}

func isGrokImagineImageSource(imageSource string) bool {
	normalized := strings.ToLower(strings.TrimSpace(imageSource))
	if normalized == "" {
		return false
	}

	return strings.Contains(normalized, "grok imagine image") ||
		strings.Contains(normalized, "grok-imagine-image") ||
		strings.Contains(normalized, "grok 2 image gen")
}

func normalizeVideoProvider(videoSource string, hasGeminiKey bool, hasXAIKey bool) string {
	normalized := strings.ToLower(strings.TrimSpace(videoSource))
	switch {
	case strings.Contains(normalized, "grok"):
		return "grok"
	case strings.Contains(normalized, "veo"):
		return "veo"
	}

	// Backward-compatible fallback when older clients do not send videoSource.
	if hasXAIKey && !hasGeminiKey {
		return "grok"
	}
	return "veo"
}

// MARK: - Veo Video Generation Handlers

func generateVeoVideoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req VeoGenerationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	// Validate inputs
	if len(req.Images) == 0 && req.ExtensionSource == nil {
		http.Error(w, "Images or extensionSource required", http.StatusBadRequest)
		return
	}

	provider := normalizeVideoProvider(req.VideoSource, strings.TrimSpace(req.GeminiKey) != "", strings.TrimSpace(req.XaiKey) != "")
	fmt.Printf("[VIDEO] Starting generation provider=%s (images: %d, extension: %v, keyframes: %v)\n",
		provider, len(req.Images), req.ExtensionSource != nil, req.UseKeyframes)

	var (
		resp *VeoGenerationResponse
		err  error
	)
	if provider == "grok" {
		if strings.TrimSpace(req.XaiKey) == "" {
			http.Error(w, "xAI API key required for Grok Imagine Video", http.StatusBadRequest)
			return
		}
		resp, err = startGrokImagineVideoGeneration(req)
	} else {
		if strings.TrimSpace(req.GeminiKey) == "" {
			http.Error(w, "Gemini API key required for Veo generation", http.StatusBadRequest)
			return
		}
		resp, err = startVeoVideoGeneration(req)
	}

	if err != nil {
		fmt.Printf("[VIDEO] Generation failed (provider=%s): %v\n", provider, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Printf("[VIDEO] Generation started successfully. provider=%s operationID=%s\n", provider, resp.OperationID)

	// Return operation ID
	json.NewEncoder(w).Encode(resp)
}

func pollVeoOperationHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req VeoPollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.OperationID == "" {
		http.Error(w, "Operation ID required", http.StatusBadRequest)
		return
	}

	provider := normalizeVideoProvider(req.VideoSource, strings.TrimSpace(req.GeminiKey) != "", strings.TrimSpace(req.XaiKey) != "")
	fmt.Printf("[VIDEO] Poll request provider=%s operationID=%s\n", provider, strings.TrimSpace(req.OperationID))
	var (
		resp *VeoPollResponse
		err  error
	)
	if provider == "grok" {
		if strings.TrimSpace(req.XaiKey) == "" {
			http.Error(w, "xAI API key required for Grok Imagine Video", http.StatusBadRequest)
			return
		}
		resp, err = pollGrokImagineVideoOperation(req.OperationID, req.XaiKey)
	} else {
		if strings.TrimSpace(req.GeminiKey) == "" {
			http.Error(w, "Gemini API key required for Veo generation", http.StatusBadRequest)
			return
		}
		resp, err = pollVeoOperation(req.OperationID, req.GeminiKey)
	}

	if err != nil {
		fmt.Printf("[VIDEO] Poll failed (provider=%s): %v\n", provider, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Printf("[VIDEO] Poll response provider=%s operationID=%s done=%t status=%s hasVideo=%t hasError=%t\n",
		provider,
		strings.TrimSpace(req.OperationID),
		resp.Done,
		strings.TrimSpace(resp.Status),
		strings.TrimSpace(resp.VideoURL) != "",
		strings.TrimSpace(resp.Error) != "",
	)

	if resp.Done && resp.Status == "completed" {
		fmt.Printf("[VIDEO] Operation completed (provider=%s): %s\n", provider, req.OperationID)
	}

	// Return poll response
	json.NewEncoder(w).Encode(resp)
}
