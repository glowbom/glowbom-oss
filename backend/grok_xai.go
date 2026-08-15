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
	"unicode/utf8"
)

const (
	grokTextModelID        = "grok-4.3"
	grokTextInputCostPerM  = 1.25
	grokTextOutputCostPerM = 2.50
)

// findValidUTF8CutPoint finds a safe position to cut a string without breaking UTF-8 characters
// Returns the largest position <= maxPos that doesn't split a multi-byte character
func findValidUTF8CutPoint(s string, maxPos int) int {
	if maxPos >= len(s) {
		return len(s)
	}
	// Walk backwards from maxPos to find a valid UTF-8 boundary
	for i := maxPos; i > 0; i-- {
		if utf8.ValidString(s[:i]) {
			return i
		}
	}
	return 0
}

// callGrok4DrawToCodeApiFull handles draw-to-code using Grok 4.3 with native vision.
func callGrok4DrawToCodeApiFull(imageBase64, userPrompt, template, imageSource, apiKey string) (*R1Response, error) {
	if apiKey == "" {
		return &R1Response{
			AIResponse: "No xAI API key provided. Please add your key in Settings.",
			TokenUsage: map[string]int{"inputTokens": 0, "outputTokens": 0, "totalTokens": 0},
			Cost:       0,
		}, nil
	}

	// 1) Build system prompt (developer role equivalent, but Grok uses system)
	systemPrompt := getSystemPrompt(template, imageSource)
	systemPrompt += " Replace @tailwind placeholders with the Tailwind CSS CDN link to load the real framework."

	// 2) Build detailed task
	detailedTask := buildDetailedTaskDescription(template, imageSource, userPrompt)

	// 3) Construct Grok 4.3 request with vision.
	messages := []map[string]interface{}{
		{
			"role":    "system",
			"content": systemPrompt,
		},
		{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": detailedTask,
				},
				map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]interface{}{
						"url": fmt.Sprintf("data:image/jpeg;base64,%s", imageBase64),
					},
				},
			},
		},
	}

	// 4) Call xAI API with Grok 4.3.
	aiResp, inputTokens, outputTokens, err := callGrok4API(messages, apiKey, 8192, "")
	if err != nil {
		return nil, err
	}

	cost := calculateGrokTextCost(inputTokens, outputTokens)

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

// callGrok4DrawToCodeStreaming handles draw-to-code with streaming for Grok 4.3.
func callGrok4DrawToCodeStreaming(w http.ResponseWriter, imageBase64, userPrompt, template, imageSource, apiKey string) error {
	if apiKey == "" {
		return fmt.Errorf("no xAI API key provided")
	}

	// Build system prompt
	systemPrompt := getSystemPrompt(template, imageSource)
	systemPrompt += " Replace @tailwind placeholders with the Tailwind CSS CDN link to load the real framework."

	// Build detailed task
	detailedTask := buildDetailedTaskDescription(template, imageSource, userPrompt)

	// Construct Grok 4.3 request with vision.
	messages := []map[string]interface{}{
		{
			"role":    "system",
			"content": systemPrompt,
		},
		{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": detailedTask,
				},
				map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]interface{}{
						"url": fmt.Sprintf("data:image/jpeg;base64,%s", imageBase64),
					},
				},
			},
		},
	}

	// Call existing streaming function
	return callGrok4APIStreaming(w, messages, apiKey, 8192, "")
}

// callGrok4ApiGo handles normal chat using Grok 4.3.
func callGrok4ApiGo(prevMsgs []ChatMessage, newMsg, apiKey string) (*R1Response, error) {
	if apiKey == "" {
		return &R1Response{
			AIResponse: "No xAI API key provided. Please add your key in Settings.",
			TokenUsage: map[string]int{},
			Cost:       0,
		}, nil
	}

	// Gather system message and convert to Grok 4.3 format.
	systemMsg := defaultSystemPrompt
	var messages []map[string]interface{}

	for _, m := range prevMsgs {
		if m.Role == "system" {
			systemMsg = m.Content
		}
	}

	// For Grok, add instruction to explain thinking step by step in <think> tags
	systemMsg += " Explain your thinking step by step, wrapping the reasoning in <think> tags, before giving the final answer."

	// Add system message
	messages = append(messages, map[string]interface{}{
		"role":    "system",
		"content": systemMsg,
	})

	// Add conversation history
	for _, m := range prevMsgs {
		if m.Role == "user" || m.Role == "assistant" {
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

	// Call Grok 4.3 API.
	aiResp, inputTokens, outputTokens, err := callGrok4API(messages, apiKey, 4096, "")
	if err != nil {
		return nil, err
	}

	cost := calculateGrokTextCost(inputTokens, outputTokens)

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

// callGrok4API makes the actual HTTP request to xAI API with streaming support
func callGrok4API(messages []map[string]interface{}, apiKey string, maxTokens int, reasoningEffort string) (string, int, int, error) {
	reqBody := map[string]interface{}{
		"model":       grokTextModelID,
		"max_tokens":  maxTokens,
		"messages":    messages,
		"stream":      true,
		"temperature": 0,
	}

	jsonBytes, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", "https://api.x.ai/v1/chat/completions", bytes.NewReader(jsonBytes))
	if err != nil {
		return "", 0, 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	fmt.Printf("[DEBUG] Calling Grok API with %s streaming...\n", grokTextModelID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", 0, 0, fmt.Errorf("xAI API error (%d): %s", resp.StatusCode, string(b))
	}

	// Parse streaming response
	var responseText strings.Builder
	var reasoningBuilder strings.Builder
	var inputTokens, outputTokens int

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				dataStr := strings.TrimSpace(line[5:])
				if dataStr == "[DONE]" {
					break
				}

				var chunk struct {
					Choices []struct {
						Delta struct {
							Content          string `json:"content"`
							ReasoningContent string `json:"reasoning_content"`
						} `json:"delta"`
					} `json:"choices"`
					Usage struct {
						PromptTokens     int `json:"prompt_tokens"`
						CompletionTokens int `json:"completion_tokens"`
					} `json:"usage"`
				}

				if e := json.Unmarshal([]byte(dataStr), &chunk); e == nil {
					if len(chunk.Choices) > 0 {
						// Extract reasoning and content
						reasoning := chunk.Choices[0].Delta.ReasoningContent
						content := chunk.Choices[0].Delta.Content
						if reasoning != "" {
							reasoningBuilder.WriteString(reasoning)
						}
						responseText.WriteString(content)
					}

					// Extract usage from final chunk
					if chunk.Usage.PromptTokens > 0 {
						inputTokens = chunk.Usage.PromptTokens
						outputTokens = chunk.Usage.CompletionTokens
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", 0, 0, err
		}
	}

	fullResponse := responseText.String()
	if reasoningBuilder.Len() > 0 {
		fullResponse = "<think>" + reasoningBuilder.String() + "</think>" + fullResponse
	}

	fmt.Printf("[DEBUG] %s response: %d input tokens, %d output tokens\n", grokTextModelID, inputTokens, outputTokens)

	return fullResponse, inputTokens, outputTokens, nil
}

// callGrok4ChatStreaming streams Grok 4.3 response via SSE to the writer (for chat).
func callGrok4ChatStreaming(w http.ResponseWriter, prevMsgs []ChatMessage, newMsg, apiKey string) error {
	if apiKey == "" {
		return fmt.Errorf("no xAI API key provided")
	}

	// Gather system message and convert to Grok 4.3 format.
	systemMsg := defaultSystemPrompt
	var messages []map[string]interface{}

	for _, m := range prevMsgs {
		if m.Role == "system" {
			systemMsg = m.Content
		}
	}

	// For Grok, add instruction to explain thinking step by step in <think> tags
	systemMsg += " Explain your thinking step by step, wrapping the reasoning in <think> tags, before giving the final answer."

	// Add system message
	messages = append(messages, map[string]interface{}{
		"role":    "system",
		"content": systemMsg,
	})

	// Add conversation history
	for _, m := range prevMsgs {
		if m.Role == "user" || m.Role == "assistant" {
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

	// Call streaming Grok 4.3 API.
	return callGrok4APIStreaming(w, messages, apiKey, 4096, "")
}

// callGrok4APIStreaming streams Grok 4.3 responses via SSE directly to the HTTP response writer.
func callGrok4APIStreaming(w http.ResponseWriter, messages []map[string]interface{}, apiKey string, maxTokens int, reasoningEffort string) error {
	reqBody := map[string]interface{}{
		"model":       grokTextModelID,
		"max_tokens":  maxTokens,
		"messages":    messages,
		"stream":      true,
		"temperature": 0,
	}

	jsonBytes, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", "https://api.x.ai/v1/chat/completions", bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	fmt.Printf("[DEBUG] Calling Grok API with %s streaming (SSE)...\n", grokTextModelID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("xAI API error (%d): %s", resp.StatusCode, string(b))
	}

	// Stream response chunks to client
	var reasoningBuilder strings.Builder
	var thinkingSent bool
	var thinkingStart time.Time
	var thinkingDurationSent bool
	var inputTokens, outputTokens int
	var inThinkTag bool
	var contentBuffer strings.Builder
	var thinkBuffer strings.Builder

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				dataStr := strings.TrimSpace(line[5:])
				if dataStr == "[DONE]" {
					break
				}

				var chunk struct {
					Choices []struct {
						Delta struct {
							Content          string `json:"content"`
							ReasoningContent string `json:"reasoning_content"`
						} `json:"delta"`
					} `json:"choices"`
					Usage struct {
						PromptTokens     int `json:"prompt_tokens"`
						CompletionTokens int `json:"completion_tokens"`
					} `json:"usage"`
				}

				if e := json.Unmarshal([]byte(dataStr), &chunk); e == nil {
					if len(chunk.Choices) > 0 {
						reasoning := chunk.Choices[0].Delta.ReasoningContent
						content := chunk.Choices[0].Delta.Content

						// Handle reasoning_content (Grok's native reasoning field)
						if reasoning != "" {
							if thinkingStart.IsZero() {
								thinkingStart = time.Now()
							}
							reasoningBuilder.WriteString(reasoning)
						}

						// Handle regular content with <think> tag parsing
						if content != "" {
							//fmt.Printf("[Grok DEBUG] Received content: %q\n", content)
							contentBuffer.WriteString(content)
							bufferStr := contentBuffer.String()
							//fmt.Printf("[Grok DEBUG] Buffer state: inThinkTag=%v, bufferLen=%d\n", inThinkTag, len(bufferStr))

							// Process buffer for <think> tags
							for {
								if !inThinkTag {
									// Look for <think> opening tag
									if idx := strings.Index(bufferStr, "<think>"); idx != -1 {
										// Send content before <think> immediately
										if idx > 0 {
											chunkData := map[string]string{"chunk": bufferStr[:idx]}
											chunkBytes, _ := json.Marshal(chunkData)
											fmt.Fprintf(w, "data: %s\n\n", chunkBytes)
											//fmt.Printf("[Grok DEBUG] Sent chunk before <think>: %q\n", bufferStr[:idx])
											if f, ok := w.(http.Flusher); ok {
												f.Flush()
											}
										}
										// Start tracking thinking time
										if thinkingStart.IsZero() {
											thinkingStart = time.Now()
										}
										bufferStr = bufferStr[idx+7:] // Skip "<think>"
										inThinkTag = true
										thinkBuffer.Reset()
									} else {
										// No <think> found yet
										// Keep last 8 chars in buffer (in case "<think>" is split across chunks)
										if len(bufferStr) > 8 {
											// Find safe UTF-8 cut point to avoid breaking multi-byte characters
											sendLen := findValidUTF8CutPoint(bufferStr, len(bufferStr)-8)
											if sendLen > 0 {
												chunkData := map[string]string{"chunk": bufferStr[:sendLen]}
												chunkBytes, _ := json.Marshal(chunkData)
												fmt.Fprintf(w, "data: %s\n\n", chunkBytes)
												//fmt.Printf("[Grok DEBUG] Sent regular chunk: %q\n", bufferStr[:sendLen])
												if f, ok := w.(http.Flusher); ok {
													f.Flush()
												}
												bufferStr = bufferStr[sendLen:]
											}
										}
										break
									}
								} else {
									// Inside <think> tag - accumulate into thinkBuffer and look for </think>
									thinkBuffer.WriteString(bufferStr)
									thinkBufferStr := thinkBuffer.String()

									closeIdx := strings.Index(thinkBufferStr, "</think>")
									//fmt.Printf("[Grok DEBUG] Looking for </think>, closeIdx=%d, thinkBufferStr=%q\n", closeIdx, thinkBufferStr)
									if closeIdx != -1 {
										//fmt.Println("[Grok DEBUG] Found </think>! Sending thinking block and switching to regular content mode")
										// Extract thinking content (before </think>)
										thinkContent := thinkBufferStr[:closeIdx]

										// Send complete thinking block
										if !thinkingSent {
											thinkingText := "<think>" + thinkContent + "</think>"
											chunkData := map[string]string{"chunk": thinkingText}
											chunkBytes, _ := json.Marshal(chunkData)
											fmt.Fprintf(w, "data: %s\n\n", chunkBytes)
											fmt.Println("[Grok] Sent complete thinking to client")
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
												fmt.Printf("[Grok] Sent thinking duration: %.1fs\n", duration)
												if f, ok := w.(http.Flusher); ok {
													f.Flush()
												}
												thinkingDurationSent = true
											}
										}

										// Continue with content after </think>
										bufferStr = thinkBufferStr[closeIdx+8:] // Skip "</think>"
										inThinkTag = false
										thinkBuffer.Reset()
										// Loop back to process remaining content
										continue
									} else {
										// No closing tag yet - keep accumulating
										bufferStr = ""
										break
									}
								}
							}

							// Update content buffer with what remains
							contentBuffer.Reset()
							contentBuffer.WriteString(bufferStr)
						}
					}

					// Capture usage if the provider includes it.
					// Some Grok responses omit usage in SSE; we still must send a final "done" event.
					if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
						inputTokens = chunk.Usage.PromptTokens
						outputTokens = chunk.Usage.CompletionTokens
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

	// Flush any remaining content in buffer before finishing
	if contentBuffer.Len() > 0 && !inThinkTag {
		remaining := contentBuffer.String()
		chunkData := map[string]string{"chunk": remaining}
		chunkBytes, _ := json.Marshal(chunkData)
		fmt.Fprintf(w, "data: %s\n\n", chunkBytes)
		fmt.Printf("[Grok DEBUG] Flushed remaining buffer: %q\n", remaining)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	// Always send a terminal done event so the client can finalize post-processing.
	// If xAI omitted usage in stream chunks, these fields will be zero.
	cost := calculateGrokTextCost(inputTokens, outputTokens)
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

	fmt.Printf("[%s] Stream complete: %d input tokens, %d output tokens\n", grokTextModelID, inputTokens, outputTokens)
	return nil
}

func calculateGrokTextCost(inputTokens, outputTokens int) float64 {
	// xAI Grok 4.3 public API pricing snapshot (May 2026).
	return (float64(inputTokens) / 1_000_000.0 * grokTextInputCostPerM) +
		(float64(outputTokens) / 1_000_000.0 * grokTextOutputCostPerM)
}
