package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const claudeSonnetDefaultModel = "claude-sonnet-4-6"
const claudeOpusDefaultModel = "claude-opus-4-7"

func isClaudeOpusModel(modelID string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(modelID)), "claude-opus-")
}

func supportsClaudeAdaptiveThinking(modelID string) bool {
	normalizedModelID := strings.ToLower(strings.TrimSpace(modelID))
	return strings.HasPrefix(normalizedModelID, "claude-opus-4-") || strings.HasPrefix(normalizedModelID, "claude-sonnet-4-")
}

func buildClaudeThinkingConfig(modelID string, thinkingBudget int) map[string]interface{} {
	if supportsClaudeAdaptiveThinking(modelID) {
		return map[string]interface{}{
			"type":    "adaptive",
			"display": "summarized",
		}
	}

	return map[string]interface{}{
		"type":          "enabled",
		"budget_tokens": thinkingBudget,
	}
}

func buildClaudeOutputConfig(modelID string) map[string]interface{} {
	if supportsClaudeAdaptiveThinking(modelID) {
		return map[string]interface{}{
			"effort": "high",
		}
	}

	return nil
}

func buildClaudeRequestBody(messages []map[string]interface{}, systemPrompt, modelID string, maxTokens int, thinkingBudget int, stream bool, tools []map[string]interface{}) map[string]interface{} {
	reqBody := map[string]interface{}{
		"model":      modelID,
		"max_tokens": maxTokens,
		"messages":   messages,
		"system":     systemPrompt,
		"thinking":   buildClaudeThinkingConfig(modelID, thinkingBudget),
	}

	if outputConfig := buildClaudeOutputConfig(modelID); outputConfig != nil {
		reqBody["output_config"] = outputConfig
	}

	if stream {
		reqBody["stream"] = true
	}

	if len(tools) > 0 {
		reqBody["tools"] = tools
	}

	return reqBody
}

func claudeModelTokenRates(modelID string) (inputCostPer1M float64, outputCostPer1M float64) {
	if isClaudeOpusModel(modelID) {
		return 5.0, 25.0
	}
	return 3.0, 15.0
}

// callClaudeDrawToCodeApiFull handles draw-to-code using Claude API with native vision
func callClaudeDrawToCodeApiFull(imageBase64, userPrompt, template, imageSource, apiKey string) (*R1Response, error) {
	return callClaudeDrawToCodeApiFullWithModel(imageBase64, userPrompt, template, imageSource, apiKey, claudeSonnetDefaultModel)
}

// callClaudeDrawToCodeApiFullWithModel handles draw-to-code with specific Claude model
func callClaudeDrawToCodeApiFullWithModel(imageBase64, userPrompt, template, imageSource, apiKey, modelID string) (*R1Response, error) {
	if apiKey == "" {
		return &R1Response{
			AIResponse: "No Claude API key provided. Please add your key in Settings.",
			TokenUsage: map[string]int{"inputTokens": 0, "outputTokens": 0, "totalTokens": 0},
			Cost:       0,
		}, nil
	}

	// 1) Build system prompt
	systemPrompt := getSystemPrompt(template, imageSource)
	systemPrompt += " Replace @tailwind placeholders with the Tailwind CSS CDN link to load the real framework."

	// 2) Build detailed task
	detailedTask := buildDetailedTaskDescription(template, imageSource, userPrompt)

	// 3) Construct multimodal message with image + text for Claude's native vision
	// Claude expects content array with image and text blocks
	userContent := []interface{}{
		map[string]interface{}{
			"type": "image",
			"source": map[string]interface{}{
				"type":       "base64",
				"media_type": "image/jpeg",
				"data":       imageBase64,
			},
		},
		map[string]interface{}{
			"type": "text",
			"text": strings.Join([]string{
				fmt.Sprintf("Detailed Task Description: %s", detailedTask),
				fmt.Sprintf("Human: %s", userPrompt),
			}, "\n"),
		},
	}

	// 4) Call Claude API with vision support
	messages := []map[string]interface{}{
		{"role": "user", "content": userContent},
	}

	// Use max tokens for draw-to-code (Claude Sonnet 4.6 supports up to 64k output tokens)
	// Use 32768 tokens (32K) with 4096 thinking budget for comprehensive HTML generation
	aiResp, inputTokens, outputTokens, err := callClaudeAPIWithThinkingBudgetAndModel(messages, systemPrompt, apiKey, modelID, 32768, 4096)
	if err != nil {
		return nil, err
	}

	// 5) Calculate cost based on model
	// Sonnet 4.6: Input $3/1M, Output $15/1M
	// Opus 4.7: Input $5/1M, Output $25/1M
	inputCostPer1M, outputCostPer1M := claudeModelTokenRates(modelID)
	cost := (float64(inputTokens) / 1_000_000.0 * inputCostPer1M) + (float64(outputTokens) / 1_000_000.0 * outputCostPer1M)

	return &R1Response{
		AIResponse: aiResp,
		TokenUsage: map[string]int{
			"inputTokens":  inputTokens,
			"outputTokens": outputTokens,
			"totalTokens":  inputTokens + outputTokens,
		},
		Cost: cost,
	}, nil
}

// callClaudeDrawToCodeStreaming handles draw-to-code with streaming for specified Claude model
func callClaudeDrawToCodeStreaming(w http.ResponseWriter, imageBase64, userPrompt, template, imageSource, apiKey, modelID string) error {
	if apiKey == "" {
		return fmt.Errorf("no Claude API key provided")
	}

	// Build system prompt (reuse existing logic)
	systemPrompt := getSystemPrompt(template, imageSource)
	systemPrompt += " Replace @tailwind placeholders with the Tailwind CSS CDN link to load the real framework."

	// Build detailed task
	detailedTask := buildDetailedTaskDescription(template, imageSource, userPrompt)

	// Construct multimodal message with image + text for Claude's native vision
	userContent := []interface{}{
		map[string]interface{}{
			"type": "image",
			"source": map[string]interface{}{
				"type":       "base64",
				"media_type": "image/jpeg",
				"data":       imageBase64,
			},
		},
		map[string]interface{}{
			"type": "text",
			"text": strings.Join([]string{
				fmt.Sprintf("Detailed Task Description: %s", detailedTask),
				fmt.Sprintf("Human: %s", userPrompt),
			}, "\n"),
		},
	}

	messages := []map[string]interface{}{
		{"role": "user", "content": userContent},
	}

	// Call existing streaming function with 32K tokens and 4K thinking budget
	return callClaudeAPIStreamingWithModel(w, messages, systemPrompt, apiKey, modelID, 32768)
}

// callClaudeApiGo handles normal chat using Claude API
func callClaudeApiGo(prevMsgs []ChatMessage, newMsg, apiKey string) (*R1Response, error) {
	return callClaudeApiGoWithModel(prevMsgs, newMsg, apiKey, claudeSonnetDefaultModel)
}

// callClaudeApiGoWithModel handles normal chat using a specific Claude model
func callClaudeApiGoWithModel(prevMsgs []ChatMessage, newMsg, apiKey, modelID string) (*R1Response, error) {
	if apiKey == "" {
		return &R1Response{
			AIResponse: "No Claude API key provided. Please add your key in Settings.",
			TokenUsage: map[string]int{},
			Cost:       0,
		}, nil
	}

	// Gather system message
	systemMsg := defaultSystemPrompt
	var messages []map[string]interface{}

	for _, m := range prevMsgs {
		if m.Role == "system" {
			systemMsg = m.Content
		} else if m.Role == "user" || m.Role == "assistant" {
			messages = append(messages, map[string]interface{}{
				"role":    m.Role,
				"content": m.Content,
			})
		}
	}

	// Add new user message
	messages = append(messages, map[string]interface{}{
		"role":    "user",
		"content": newMsg,
	})

	// Call Claude API with increased output limit for comprehensive responses.
	aiResp, inputTokens, outputTokens, err := callClaudeAPIWithThinkingBudgetAndModel(messages, systemMsg, apiKey, modelID, 8192, 2048)
	if err != nil {
		return nil, err
	}

	// Calculate cost based on selected model.
	inputCostPer1M, outputCostPer1M := claudeModelTokenRates(modelID)
	cost := (float64(inputTokens) / 1_000_000.0 * inputCostPer1M) + (float64(outputTokens) / 1_000_000.0 * outputCostPer1M)

	return &R1Response{
		AIResponse: aiResp,
		TokenUsage: map[string]int{
			"inputTokens":  inputTokens,
			"outputTokens": outputTokens,
			"totalTokens":  inputTokens + outputTokens,
		},
		Cost: cost,
	}, nil
}

// callClaudeAPI makes the actual HTTP request to Anthropic API
func callClaudeAPI(messages []map[string]interface{}, systemPrompt, apiKey string, maxTokens int) (string, int, int, error) {
	return callClaudeAPIWithThinkingBudget(messages, systemPrompt, apiKey, maxTokens, 2048)
}

// callClaudeAPIWithThinkingBudget makes the actual HTTP request with custom thinking budget
func callClaudeAPIWithThinkingBudget(messages []map[string]interface{}, systemPrompt, apiKey string, maxTokens int, thinkingBudget int) (string, int, int, error) {
	return callClaudeAPIWithThinkingBudgetAndModel(messages, systemPrompt, apiKey, claudeSonnetDefaultModel, maxTokens, thinkingBudget)
}

// callClaudeAPIWithThinkingBudgetAndModel makes the actual HTTP request with custom thinking budget and model
func callClaudeAPIWithThinkingBudgetAndModel(messages []map[string]interface{}, systemPrompt, apiKey, modelID string, maxTokens int, thinkingBudget int) (string, int, int, error) {
	// Thinking budget must be less than max_tokens
	if maxTokens <= thinkingBudget {
		thinkingBudget = maxTokens / 4 // Use quarter for thinking if max_tokens is small
	}

	reqBody := buildClaudeRequestBody(messages, systemPrompt, modelID, maxTokens, thinkingBudget, false, nil)

	jsonBytes, _ := json.Marshal(reqBody)
	fmt.Printf("[DEBUG] Claude request body: %s\n", string(jsonBytes))

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBytes))
	if err != nil {
		fmt.Printf("[ERROR] Failed to create request: %v\n", err)
		return "", 0, 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	fmt.Println("[DEBUG] Calling Claude API...")

	resp, err := http.DefaultClient.Do(req)
	fmt.Printf("[DEBUG] Claude API responded with status: %v, error: %v\n", resp, err)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		errMsg := fmt.Sprintf("Claude API error (%d): %s", resp.StatusCode, string(b))
		fmt.Println("[ERROR]", errMsg)
		return "", 0, 0, fmt.Errorf("%s", errMsg)
	}

	// Parse response
	bodyBytes, _ := io.ReadAll(resp.Body)

	var claudeResp struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"` // For extended thinking blocks
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(bodyBytes, &claudeResp); err != nil {
		return "", 0, 0, fmt.Errorf("failed to parse Claude response: %v", err)
	}

	// Extract text from content blocks, including thinking blocks
	var responseText strings.Builder
	var thinkingText strings.Builder

	for _, block := range claudeResp.Content {
		if block.Type == "thinking" && block.Thinking != "" {
			thinkingText.WriteString(block.Thinking)
		} else if block.Text != "" {
			responseText.WriteString(block.Text)
		}
	}

	// Prepend thinking in <think> tags if present
	var fullResponse strings.Builder
	if thinkingText.Len() > 0 {
		fullResponse.WriteString("<think>")
		fullResponse.WriteString(thinkingText.String())
		fullResponse.WriteString("</think>")
	}
	fullResponse.WriteString(responseText.String())

	fmt.Printf("[DEBUG] Claude response: %d input tokens, %d output tokens\n",
		claudeResp.Usage.InputTokens, claudeResp.Usage.OutputTokens)

	return fullResponse.String(),
		claudeResp.Usage.InputTokens,
		claudeResp.Usage.OutputTokens,
		nil
}

// getMagicEditTool returns the tool definition for magic_edit
func getMagicEditTool() map[string]interface{} {
	return map[string]interface{}{
		"name":        "magic_edit",
		"description": "Trigger a visual code edit on the currently loaded project. Use this when the user asks you to make visual or code changes to their project (like changing colors, adding elements, modifying layouts, etc.).",
		"input_schema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"edit_description": map[string]interface{}{
					"type":        "string",
					"description": "A clear, specific description of what changes to make to the code/design. Be detailed about colors, positions, sizes, etc.",
				},
			},
			"required": []string{"edit_description"},
		},
	}
}

// callClaudeChatStreaming streams Claude response via SSE to the writer (for chat)
func callClaudeChatStreaming(w http.ResponseWriter, prevMsgs []ChatMessage, newMsg, apiKey string) error {
	return callClaudeChatStreamingWithModel(w, prevMsgs, newMsg, apiKey, claudeSonnetDefaultModel)
}

// callClaudeChatStreamingWithModel streams Claude response with specific model
func callClaudeChatStreamingWithModel(w http.ResponseWriter, prevMsgs []ChatMessage, newMsg, apiKey, modelID string) error {
	return callClaudeChatStreamingWithModelAndTools(w, prevMsgs, newMsg, apiKey, modelID, nil)
}

// callClaudeChatStreamingWithModelAndTools streams Claude response with specific model and optional tools
func callClaudeChatStreamingWithModelAndTools(w http.ResponseWriter, prevMsgs []ChatMessage, newMsg, apiKey, modelID string, tools []map[string]interface{}) error {
	if apiKey == "" {
		return fmt.Errorf("no Claude API key provided")
	}

	// Gather system message
	systemMsg := defaultSystemPrompt
	var messages []map[string]interface{}

	for _, m := range prevMsgs {
		if m.Role == "system" {
			systemMsg = m.Content
		} else if m.Role == "user" || m.Role == "assistant" {
			messages = append(messages, map[string]interface{}{
				"role":    m.Role,
				"content": m.Content,
			})
		}
	}

	// Add new user message
	messages = append(messages, map[string]interface{}{
		"role":    "user",
		"content": newMsg,
	})

	// Call streaming Claude API with increased output limit
	return callClaudeAPIStreamingWithModelAndTools(w, messages, systemMsg, apiKey, modelID, 8192, tools)
}

// callClaudeAPIStreaming streams Claude responses via SSE directly to the HTTP response writer
func callClaudeAPIStreaming(w http.ResponseWriter, messages []map[string]interface{}, systemPrompt, apiKey string, maxTokens int) error {
	return callClaudeAPIStreamingWithModel(w, messages, systemPrompt, apiKey, claudeSonnetDefaultModel, maxTokens)
}

// callClaudeAPIStreamingWithModel streams Claude responses with specific model
func callClaudeAPIStreamingWithModel(w http.ResponseWriter, messages []map[string]interface{}, systemPrompt, apiKey, modelID string, maxTokens int) error {
	return callClaudeAPIStreamingWithModelAndTools(w, messages, systemPrompt, apiKey, modelID, maxTokens, nil)
}

// callClaudeAPIStreamingWithModelAndTools streams Claude responses with tools support
func callClaudeAPIStreamingWithModelAndTools(w http.ResponseWriter, messages []map[string]interface{}, systemPrompt, apiKey, modelID string, maxTokens int, tools []map[string]interface{}) error {
	// Thinking budget must be less than max_tokens
	thinkingBudget := 2048
	if maxTokens <= thinkingBudget {
		thinkingBudget = maxTokens / 2 // Use half for thinking if max_tokens is small
	}

	reqBody := buildClaudeRequestBody(messages, systemPrompt, modelID, maxTokens, thinkingBudget, true, tools)

	jsonBytes, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	fmt.Println("[DEBUG] Calling Claude API with streaming...")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		errMsg := fmt.Sprintf("Claude API error (%d): %s", resp.StatusCode, string(b))
		fmt.Println("[ERROR]", errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	fmt.Println("[DEBUG] Claude API responded with status 200, starting to read stream...")

	// Stream response chunks to client
	var responseBuilder strings.Builder
	var thinkingBuilder strings.Builder
	var thinkingSent bool
	var thinkingStart time.Time
	var thinkingDurationSent bool
	var inputTokens, outputTokens int
	var currentThinkingStep strings.Builder
	var currentStepTitleSent bool
	var currentBlockType string

	// Tool use tracking
	var currentToolId string
	var currentToolName string
	var currentToolInput strings.Builder

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				dataStr := strings.TrimSpace(line[6:])

				var event struct {
					Type  string `json:"type"`
					Index int    `json:"index"`
					Delta struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						Thinking    string `json:"thinking"`     // For extended thinking deltas
						PartialJson string `json:"partial_json"` // For tool use JSON deltas
					} `json:"delta"`
					ContentBlock struct {
						Type     string `json:"type"`
						Text     string `json:"text"`
						Thinking string `json:"thinking"`
						Id       string `json:"id"`   // Tool use ID
						Name     string `json:"name"` // Tool use name
					} `json:"content_block"`
					Message struct {
						Usage struct {
							InputTokens  int `json:"input_tokens"`
							OutputTokens int `json:"output_tokens"`
						} `json:"usage"`
					} `json:"message"`
					Usage struct {
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}

				if e := json.Unmarshal([]byte(dataStr), &event); e == nil {
					// Check for error events
					if event.Type == "error" || event.Error.Type != "" {
						errMsg := fmt.Sprintf("Claude streaming error: %s - %s", event.Error.Type, event.Error.Message)
						fmt.Println("[ERROR]", errMsg)
						return fmt.Errorf("%s", errMsg)
					}

					// Debug: log event types
					if event.Type != "" {
						fmt.Printf("[Claude DEBUG] Event type: %s, block type: %s, index: %d\n", event.Type, event.ContentBlock.Type, event.Index)
					}

					// Handle content_block_start - track what type of block we're in
					if event.Type == "content_block_start" {
						currentBlockType = event.ContentBlock.Type

						if currentBlockType == "thinking" {
							fmt.Printf("[Claude DEBUG] Starting thinking block (index %d)\n", event.Index)
							if thinkingStart.IsZero() {
								thinkingStart = time.Now()
							}
							currentThinkingStep.Reset()
							currentStepTitleSent = false
						} else if currentBlockType == "tool_use" {
							// Tool use block starting
							currentToolId = event.ContentBlock.Id
							currentToolName = event.ContentBlock.Name
							currentToolInput.Reset()
							fmt.Printf("[Claude DEBUG] Starting tool_use block: %s (id: %s)\n", currentToolName, currentToolId)
						}
					}

					// Handle content_block_stop - send tool_use event when tool block ends
					if event.Type == "content_block_stop" && currentBlockType == "tool_use" {
						// Parse the accumulated JSON input
						inputJSON := currentToolInput.String()
						fmt.Printf("[Claude] Tool use complete: %s, input: %s\n", currentToolName, inputJSON)

						// Send tool_use event to client
						toolUseData := map[string]interface{}{
							"tool_use": map[string]interface{}{
								"id":    currentToolId,
								"name":  currentToolName,
								"input": inputJSON,
							},
						}
						toolUseBytes, _ := json.Marshal(toolUseData)
						fmt.Fprintf(w, "data: %s\n\n", toolUseBytes)
						if f, ok := w.(http.Flusher); ok {
							f.Flush()
						}

						// Reset tool tracking
						currentToolId = ""
						currentToolName = ""
						currentToolInput.Reset()
					}

					// Handle content_block_delta - process content based on current block type
					if event.Type == "content_block_delta" {
						// Handle tool_use input JSON deltas
						if currentBlockType == "tool_use" && event.Delta.PartialJson != "" {
							currentToolInput.WriteString(event.Delta.PartialJson)
						} else if currentBlockType == "thinking" && event.Delta.Thinking != "" {
							// Thinking content - use Delta.Thinking field, not Delta.Text!
							thinkingBuilder.WriteString(event.Delta.Thinking)
							currentThinkingStep.WriteString(event.Delta.Thinking)

							// Try to extract and send the title once we have a complete title
							if !currentStepTitleSent {
								text := currentThinkingStep.String()
								hasCompleteMarkdown := strings.HasPrefix(text, "**") && strings.Count(text, "**") >= 2
								hasNewlineSeparation := strings.Count(text, "\n") >= 2

								if hasCompleteMarkdown || hasNewlineSeparation {
									stepTitle := extractThinkingStepTitle(text)
									if stepTitle != "" {
										thinkingStepData := map[string]string{"thinkingStep": stepTitle}
										thinkingStepBytes, _ := json.Marshal(thinkingStepData)
										fmt.Fprintf(w, "data: %s\n\n", thinkingStepBytes)
										fmt.Printf("[Claude] Sending thinking step: %s\n", stepTitle)
										if f, ok := w.(http.Flusher); ok {
											f.Flush()
										}
										currentStepTitleSent = true
									}
								}
							}
						} else if currentBlockType == "text" && event.Delta.Text != "" {
							// Text content (actual response)
							// Send complete thinking before first content chunk
							if !thinkingSent && thinkingBuilder.Len() > 0 {
								// Send the last thinking step if we haven't sent its title yet
								if !currentStepTitleSent && currentThinkingStep.Len() > 0 {
									stepTitle := extractThinkingStepTitle(currentThinkingStep.String())
									if stepTitle != "" {
										thinkingStepData := map[string]string{"thinkingStep": stepTitle}
										thinkingStepBytes, _ := json.Marshal(thinkingStepData)
										fmt.Fprintf(w, "data: %s\n\n", thinkingStepBytes)
										fmt.Printf("[Claude] Sending final thinking step: %s\n", stepTitle)
										if f, ok := w.(http.Flusher); ok {
											f.Flush()
										}
										currentStepTitleSent = true
									}
								}

								thinkingText := "<think>" + thinkingBuilder.String() + "</think>"
								chunkData := map[string]string{"chunk": thinkingText}
								chunkBytes, _ := json.Marshal(chunkData)
								fmt.Fprintf(w, "data: %s\n\n", chunkBytes)
								fmt.Println("[Claude] Sent complete thinking to client")
								if f, ok := w.(http.Flusher); ok {
									f.Flush()
								}
								thinkingSent = true

								// Send thinking duration
								if !thinkingDurationSent && !thinkingStart.IsZero() {
									duration := time.Since(thinkingStart).Seconds()
									duration = math.Round(duration*10) / 10
									metaData := map[string]any{
										"meta": map[string]any{
											"thinkingSeconds": duration,
										},
									}
									metaBytes, _ := json.Marshal(metaData)
									fmt.Fprintf(w, "data: %s\n\n", metaBytes)
									fmt.Printf("[Claude] Sent thinking duration: %.1fs\n", duration)
									if f, ok := w.(http.Flusher); ok {
										f.Flush()
									}
									thinkingDurationSent = true
								}
							}

							responseBuilder.WriteString(event.Delta.Text)

							// Send content chunk to client
							chunkData := map[string]string{"chunk": event.Delta.Text}
							chunkBytes, _ := json.Marshal(chunkData)
							fmt.Fprintf(w, "data: %s\n\n", chunkBytes)
							if f, ok := w.(http.Flusher); ok {
								f.Flush()
							}
						}
					}

					// Extract usage from message_start
					if event.Type == "message_start" {
						inputTokens = event.Message.Usage.InputTokens
					}

					// Extract usage from message_delta
					if event.Type == "message_delta" {
						outputTokens = event.Usage.OutputTokens

						// Calculate cost based on model
						// Sonnet 4.6: Input $3/1M, Output $15/1M
						// Opus 4.7: Input $5/1M, Output $25/1M
						inputCostPer1M, outputCostPer1M := claudeModelTokenRates(modelID)
						cost := (float64(inputTokens) / 1_000_000.0 * inputCostPer1M) + (float64(outputTokens) / 1_000_000.0 * outputCostPer1M)

						// Send done event with usage
						doneData := map[string]interface{}{
							"done": true,
							"tokenUsage": map[string]int{
								"inputTokens":  inputTokens,
								"outputTokens": outputTokens,
								"totalTokens":  inputTokens + outputTokens,
							},
							"cost": cost,
						}
						doneBytes, _ := json.Marshal(doneData)
						fmt.Fprintf(w, "data: %s\n\n", doneBytes)
						if f, ok := w.(http.Flusher); ok {
							f.Flush()
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	fmt.Printf("[Claude] Stream complete: %d input tokens, %d output tokens\n", inputTokens, outputTokens)
	return nil
}
