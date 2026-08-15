package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const openRouterChatCompletionsURL = "https://openrouter.ai/api/v1/chat/completions"
const openRouterHTTPReferer = "https://glowbom.com"
const openRouterXTitle = "Glowbom"

const (
	openRouterChatMaxTokens       = 4096
	openRouterDrawToCodeMaxTokens = 16384
	openRouterDefaultTemperature  = 0.6
)

func setOpenRouterRequestHeaders(req *http.Request, apiKey string, accept string) {
	req.Header.Set("Accept", accept)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("HTTP-Referer", openRouterHTTPReferer)
	req.Header.Set("X-Title", openRouterXTitle)
}

func callOpenRouterDrawToCodeApiFull(imageBase64, userPrompt, template, imageSource, apiKey, openRouterModel string) (*R1Response, error) {
	if strings.TrimSpace(apiKey) == "" {
		return &R1Response{
			AIResponse: "No OpenRouter API key provided. Please add your key in Settings.",
			TokenUsage: map[string]int{"inputTokens": 0, "outputTokens": 0, "totalTokens": 0},
			Cost:       0,
		}, nil
	}

	messages := buildOpenRouterDrawToCodeMessages(imageBase64, userPrompt, template, imageSource, openRouterModel, apiKey)
	aiResp, inputTokens, outputTokens, err := callOpenRouterChatCompletions(messages, apiKey, openRouterDrawToCodeMaxTokens, openRouterDefaultTemperature, openRouterModel, true)
	if err != nil {
		return nil, err
	}

	return &R1Response{
		AIResponse: aiResp,
		TokenUsage: map[string]int{
			"inputTokens":  inputTokens,
			"outputTokens": outputTokens,
			"totalTokens":  inputTokens + outputTokens,
		},
		Cost: 0,
	}, nil
}

func callOpenRouterDrawToCodeStreaming(w http.ResponseWriter, imageBase64, userPrompt, template, imageSource, apiKey, openRouterModel string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("no OpenRouter API key provided")
	}

	messages := buildOpenRouterDrawToCodeMessages(imageBase64, userPrompt, template, imageSource, openRouterModel, apiKey)
	return callOpenRouterChatCompletionsStreaming(w, messages, apiKey, openRouterDrawToCodeMaxTokens, openRouterDefaultTemperature, openRouterModel, true)
}

func callOpenRouterApiGo(prevMsgs []ChatMessage, newMsg, apiKey, openRouterModel, attachmentBase64, attachmentMime string) (*R1Response, error) {
	if strings.TrimSpace(apiKey) == "" {
		return &R1Response{
			AIResponse: "No OpenRouter API key provided. Please add your key in Settings.",
			TokenUsage: map[string]int{"inputTokens": 0, "outputTokens": 0, "totalTokens": 0},
			Cost:       0,
		}, nil
	}

	messages := buildOpenRouterChatMessages(prevMsgs, newMsg, openRouterModel, attachmentBase64, attachmentMime)
	aiResp, inputTokens, outputTokens, err := callOpenRouterChatCompletions(messages, apiKey, openRouterChatMaxTokens, openRouterDefaultTemperature, openRouterModel, false)
	if err != nil {
		return nil, err
	}

	return &R1Response{
		AIResponse: aiResp,
		TokenUsage: map[string]int{
			"inputTokens":  inputTokens,
			"outputTokens": outputTokens,
			"totalTokens":  inputTokens + outputTokens,
		},
		Cost: 0,
	}, nil
}

func callOpenRouterChatStreaming(w http.ResponseWriter, prevMsgs []ChatMessage, newMsg, apiKey, openRouterModel, attachmentBase64, attachmentMime string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("no OpenRouter API key provided")
	}
	messages := buildOpenRouterChatMessages(prevMsgs, newMsg, openRouterModel, attachmentBase64, attachmentMime)
	return callOpenRouterChatCompletionsStreaming(w, messages, apiKey, openRouterChatMaxTokens, openRouterDefaultTemperature, openRouterModel, false)
}

func buildOpenRouterChatMessages(prevMsgs []ChatMessage, newMsg, openRouterModel, attachmentBase64, attachmentMime string) []map[string]interface{} {
	systemMsg := defaultSystemPrompt
	for _, m := range prevMsgs {
		if m.Role == "system" {
			systemMsg = m.Content
		}
	}

	messages := []map[string]interface{}{
		{
			"role":    "system",
			"content": systemMsg,
		},
	}

	for _, m := range prevMsgs {
		if m.Role == "user" || m.Role == "assistant" {
			messages = append(messages, map[string]interface{}{
				"role":    m.Role,
				"content": m.Content,
			})
		}
	}

	userContent := interface{}(newMsg)
	trimmedAttachment := strings.TrimSpace(attachmentBase64)
	trimmedMime := strings.TrimSpace(attachmentMime)
	if trimmedAttachment != "" && trimmedMime != "" {
		resolvedModel := normalizeOpenRouterModelID(openRouterModel)
		if openRouterModelSupportsImageInput(resolvedModel) && strings.HasPrefix(strings.ToLower(trimmedMime), "image/") {
			userContent = []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": newMsg,
				},
				map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]interface{}{
						"url": openRouterAttachmentDataURI(trimmedAttachment, trimmedMime),
					},
				},
			}
			fmt.Printf("[OpenRouter DEBUG] inline image attachment enabled model=%s mime=%s\n", resolvedModel, trimmedMime)
		} else {
			fmt.Printf("[OpenRouter DEBUG] attachment ignored for model=%s mime=%q\n", resolvedModel, trimmedMime)
		}
	}

	messages = append(messages, map[string]interface{}{
		"role":    "user",
		"content": userContent,
	})
	return messages
}

func callOpenRouterChatCompletions(messages []map[string]interface{}, apiKey string, maxTokens int, temperature float64, openRouterModel string, enforceNoEmbeddedImageData bool) (string, int, int, error) {
	resolvedModel := normalizeOpenRouterModelID(openRouterModel)
	hasImage := openRouterMessagesContainImage(messages)
	logOpenRouterRequestSummary(resolvedModel, false, hasImage, maxTokens, messages)

	reqBody := map[string]interface{}{
		"model":       resolvedModel,
		"max_tokens":  maxTokens,
		"temperature": temperature,
		"messages":    messages,
	}
	if prettyBody, err := json.MarshalIndent(reqBody, "", "  "); err == nil {
		fmt.Printf("[OpenRouter DEBUG] request body stream=false >>>\n%s\n<<< END OpenRouter request body\n", string(prettyBody))
	}

	jsonBytes, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", openRouterChatCompletionsURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return "", 0, 0, err
	}

	setOpenRouterRequestHeaders(req, apiKey, "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyText := strings.TrimSpace(string(body))
		fmt.Printf("[OpenRouter ERROR] model=%s stream=false status=%d hasImage=%v body=%s\n", resolvedModel, resp.StatusCode, hasImage, truncateForLog(bodyText))
		return "", 0, 0, fmt.Errorf("OpenRouter API error (%d): %s", resp.StatusCode, bodyText)
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content          interface{} `json:"content"`
				ReasoningContent interface{} `json:"reasoning_content"`
				Reasoning        interface{} `json:"reasoning"`
			} `json:"message"`
			ReasoningContent interface{} `json:"reasoning_content"`
			Reasoning        interface{} `json:"reasoning"`
			FinishReason     interface{} `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return "", 0, 0, fmt.Errorf("failed to parse OpenRouter response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return "", 0, 0, fmt.Errorf("no choices in OpenRouter response")
	}

	content := fireworkContentToText(completion.Choices[0].Message.Content)
	reasoning := fireworkContentToText(completion.Choices[0].Message.ReasoningContent)
	if strings.TrimSpace(reasoning) == "" {
		reasoning = fireworkContentToText(completion.Choices[0].ReasoningContent)
	}
	if strings.TrimSpace(reasoning) == "" {
		reasoning = openRouterReasoningToText(completion.Choices[0].Message.Reasoning)
	}
	if strings.TrimSpace(reasoning) == "" {
		reasoning = openRouterReasoningToText(completion.Choices[0].Reasoning)
	}
	if !enforceNoEmbeddedImageData && strings.TrimSpace(reasoning) != "" {
		content = "<think>" + strings.TrimSpace(reasoning) + "</think>" + content
	}
	if enforceNoEmbeddedImageData {
		content = normalizeFireworksDrawToCodeOutput(content)
		if reason, blocked := detectFireworksEmbeddedImagePayload(content); blocked {
			return "", 0, 0, embeddedImagePayloadError(reason)
		}
	}

	finishReason := normalizeFinishReason(completion.Choices[0].FinishReason)
	if finishReason != "" {
		fmt.Printf("[OpenRouter] completion finish_reason=%s model=%s stream=false\n", finishReason, resolvedModel)
	}
	if strings.EqualFold(finishReason, "length") {
		fmt.Printf("[OpenRouter WARN] completion reached max_tokens=%d model=%s stream=false\n", maxTokens, resolvedModel)
		if enforceNoEmbeddedImageData {
			return "", 0, 0, fmt.Errorf("OpenRouter draw-to-code output was truncated at max_tokens=%d. Please regenerate with a shorter prompt.", maxTokens)
		}
	}

	inputTokens := completion.Usage.PromptTokens
	outputTokens := completion.Usage.CompletionTokens
	if inputTokens == 0 && outputTokens == 0 {
		inputTokens = estimateTokenCountForMessages(messages)
		outputTokens = estimateTokenCount(content)
	}

	return content, inputTokens, outputTokens, nil
}

func callOpenRouterChatCompletionsStreaming(w http.ResponseWriter, messages []map[string]interface{}, apiKey string, maxTokens int, temperature float64, openRouterModel string, enforceNoEmbeddedImageData bool) error {
	resolvedModel := normalizeOpenRouterModelID(openRouterModel)
	hasImage := openRouterMessagesContainImage(messages)
	logOpenRouterRequestSummary(resolvedModel, true, hasImage, maxTokens, messages)

	reqBody := map[string]interface{}{
		"model":       resolvedModel,
		"max_tokens":  maxTokens,
		"temperature": temperature,
		"messages":    messages,
		"stream":      true,
	}
	if prettyBody, err := json.MarshalIndent(reqBody, "", "  "); err == nil {
		fmt.Printf("[OpenRouter DEBUG] request body stream=true >>>\n%s\n<<< END OpenRouter request body\n", string(prettyBody))
	}

	jsonBytes, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", openRouterChatCompletionsURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}

	setOpenRouterRequestHeaders(req, apiKey, "text/event-stream, application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyText := strings.TrimSpace(string(body))
		fmt.Printf("[OpenRouter ERROR] model=%s stream=true status=%d hasImage=%v body=%s\n", resolvedModel, resp.StatusCode, hasImage, truncateForLog(bodyText))
		return fmt.Errorf("OpenRouter API error (%d): %s", resp.StatusCode, bodyText)
	}

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	fmt.Printf("[OpenRouter] stream response model=%s hasImage=%v contentType=%q\n", resolvedModel, hasImage, contentType)

	if !strings.Contains(contentType, "text/event-stream") {
		body, _ := io.ReadAll(resp.Body)
		bodyText := strings.TrimSpace(string(body))
		fmt.Printf("[OpenRouter] stream fallback model=%s hasImage=%v contentType=%q body=%s\n", resolvedModel, hasImage, contentType, truncateForLog(bodyText))

		var completion struct {
			Choices []struct {
				Message struct {
					Content          interface{} `json:"content"`
					ReasoningContent interface{} `json:"reasoning_content"`
					Reasoning        interface{} `json:"reasoning"`
				} `json:"message"`
				ReasoningContent interface{} `json:"reasoning_content"`
				Reasoning        interface{} `json:"reasoning"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &completion); err != nil {
			return fmt.Errorf("OpenRouter stream fallback parse error: %w", err)
		}
		if len(completion.Choices) == 0 {
			return fmt.Errorf("OpenRouter stream fallback returned no choices")
		}

		content := fireworkContentToText(completion.Choices[0].Message.Content)
		reasoning := fireworkContentToText(completion.Choices[0].Message.ReasoningContent)
		if strings.TrimSpace(reasoning) == "" {
			reasoning = fireworkContentToText(completion.Choices[0].ReasoningContent)
		}
		if strings.TrimSpace(reasoning) == "" {
			reasoning = openRouterReasoningToText(completion.Choices[0].Message.Reasoning)
		}
		if strings.TrimSpace(reasoning) == "" {
			reasoning = openRouterReasoningToText(completion.Choices[0].Reasoning)
		}
		if !enforceNoEmbeddedImageData && strings.TrimSpace(reasoning) != "" {
			content = "<think>" + strings.TrimSpace(reasoning) + "</think>" + content
		}
		if enforceNoEmbeddedImageData {
			content = normalizeFireworksDrawToCodeOutput(content)
			if reason, blocked := detectFireworksEmbeddedImagePayload(content); blocked {
				return embeddedImagePayloadError(reason)
			}
		}
		if strings.TrimSpace(content) == "" {
			return fmt.Errorf("OpenRouter stream fallback returned empty content")
		}
		if err := emitFireworksStreamChunk(w, content); err != nil {
			return err
		}

		promptTokens := completion.Usage.PromptTokens
		completionTokens := completion.Usage.CompletionTokens
		if promptTokens == 0 && completionTokens == 0 {
			promptTokens = estimateTokenCountForMessages(messages)
			completionTokens = estimateTokenCount(content)
		}
		return emitFireworksStreamDone(w, promptTokens, completionTokens)
	}

	reader := bufio.NewReader(resp.Body)
	var responseBuilder strings.Builder
	promptTokens := 0
	completionTokens := 0
	dataEvents := 0
	parseErrors := 0
	finishReasonLength := false
	seenFinishReason := ""
	rollingTail := ""
	normalizedEmitted := 0
	var reasoningBuilder strings.Builder
	var reasoningSent bool
	var reasoningStartTimeSet bool
	var reasoningStartTimestamp int64
	var reasoningDurationSent bool
	lastThinkingStep := ""

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmedLine := strings.TrimSpace(line)
			if strings.HasPrefix(trimmedLine, "data:") {
				dataEvents++
				dataStr := strings.TrimSpace(trimmedLine[5:])
				if dataStr == "" {
					continue
				}
				if dataStr == "[DONE]" {
					break
				}

				var chunk struct {
					Choices []struct {
						Delta struct {
							Content          interface{} `json:"content"`
							ReasoningContent interface{} `json:"reasoning_content"`
							Reasoning        interface{} `json:"reasoning"`
						} `json:"delta"`
						Message struct {
							Content          interface{} `json:"content"`
							ReasoningContent interface{} `json:"reasoning_content"`
							Reasoning        interface{} `json:"reasoning"`
						} `json:"message"`
						ReasoningContent interface{} `json:"reasoning_content"`
						Reasoning        interface{} `json:"reasoning"`
						FinishReason     interface{} `json:"finish_reason"`
					} `json:"choices"`
					Usage struct {
						PromptTokens     int `json:"prompt_tokens"`
						CompletionTokens int `json:"completion_tokens"`
					} `json:"usage"`
				}

				if unmarshalErr := json.Unmarshal([]byte(dataStr), &chunk); unmarshalErr != nil {
					parseErrors++
					if parseErrors <= 3 {
						fmt.Printf("[OpenRouter WARN] stream chunk parse failed model=%s hasImage=%v err=%v raw=%s\n", resolvedModel, hasImage, unmarshalErr, truncateForLog(dataStr))
					}
					continue
				}

				if chunk.Usage.PromptTokens > 0 {
					promptTokens = chunk.Usage.PromptTokens
					completionTokens = chunk.Usage.CompletionTokens
				}

				if len(chunk.Choices) > 0 {
					reasoningChunk := fireworkContentToText(chunk.Choices[0].Delta.ReasoningContent)
					if strings.TrimSpace(reasoningChunk) == "" {
						reasoningChunk = fireworkContentToText(chunk.Choices[0].Message.ReasoningContent)
					}
					if strings.TrimSpace(reasoningChunk) == "" {
						reasoningChunk = fireworkContentToText(chunk.Choices[0].ReasoningContent)
					}
					if strings.TrimSpace(reasoningChunk) == "" {
						reasoningChunk = openRouterReasoningToText(chunk.Choices[0].Delta.Reasoning)
					}
					if strings.TrimSpace(reasoningChunk) == "" {
						reasoningChunk = openRouterReasoningToText(chunk.Choices[0].Message.Reasoning)
					}
					if strings.TrimSpace(reasoningChunk) == "" {
						reasoningChunk = openRouterReasoningToText(chunk.Choices[0].Reasoning)
					}
					if !enforceNoEmbeddedImageData && strings.TrimSpace(reasoningChunk) != "" {
						if !reasoningStartTimeSet {
							reasoningStartTimeSet = true
							reasoningStartTimestamp = time.Now().UnixMilli()
							placeholderStep := "Thinking..."
							thinkingStepData := map[string]string{"thinkingStep": placeholderStep}
							thinkingStepBytes, _ := json.Marshal(thinkingStepData)
							fmt.Fprintf(w, "data: %s\n\n", thinkingStepBytes)
							if f, ok := w.(http.Flusher); ok {
								f.Flush()
							}
							lastThinkingStep = placeholderStep
						}
						reasoningBuilder.WriteString(reasoningChunk)

						stepTitle := extractFireworksThinkingStepTitle(reasoningBuilder.String())
						if isMeaningfulThinkingStepTitle(stepTitle) && stepTitle != lastThinkingStep {
							thinkingStepData := map[string]string{"thinkingStep": stepTitle}
							thinkingStepBytes, _ := json.Marshal(thinkingStepData)
							fmt.Fprintf(w, "data: %s\n\n", thinkingStepBytes)
							if f, ok := w.(http.Flusher); ok {
								f.Flush()
							}
							lastThinkingStep = stepTitle
						}
					}

					reason := normalizeFinishReason(chunk.Choices[0].FinishReason)
					if reason != "" {
						seenFinishReason = reason
						if strings.EqualFold(reason, "length") {
							finishReasonLength = true
						}
					}

					content := fireworkContentToText(chunk.Choices[0].Delta.Content)
					if strings.TrimSpace(content) == "" {
						content = fireworkContentToText(chunk.Choices[0].Message.Content)
					}
					if strings.TrimSpace(content) == "" {
						continue
					}
					if !enforceNoEmbeddedImageData && !reasoningSent && reasoningBuilder.Len() > 0 {
						thinkingText := "<think>" + strings.TrimSpace(reasoningBuilder.String()) + "</think>"
						if err := emitFireworksStreamChunk(w, thinkingText); err != nil {
							return err
						}
						reasoningSent = true
					}
					if enforceNoEmbeddedImageData {
						candidate := rollingTail + content
						if reason, blocked := detectFireworksEmbeddedImagePayload(candidate); blocked {
							return embeddedImagePayloadError(reason)
						}
						if len(candidate) > 4096 {
							rollingTail = candidate[len(candidate)-4096:]
						} else {
							rollingTail = candidate
						}
					}

					responseBuilder.WriteString(content)
					chunkToEmit := content
					if enforceNoEmbeddedImageData {
						normalizedCurrent := normalizeFireworksDrawToCodeOutput(responseBuilder.String())
						safeLen := len(normalizedCurrent) - fireworksStreamNormalizeHoldback
						if safeLen < 0 {
							safeLen = 0
						}
						if safeLen > normalizedEmitted {
							chunkToEmit = normalizedCurrent[normalizedEmitted:safeLen]
							normalizedEmitted = safeLen
						} else {
							chunkToEmit = ""
						}
					}
					if chunkToEmit != "" {
						if err := emitFireworksStreamChunk(w, chunkToEmit); err != nil {
							return err
						}
					}

					if !enforceNoEmbeddedImageData && !reasoningDurationSent && reasoningStartTimeSet {
						durationSeconds := float64(time.Now().UnixMilli()-reasoningStartTimestamp) / 1000.0
						metaData := map[string]interface{}{
							"meta": map[string]float64{
								"thinkingSeconds": durationSeconds,
							},
						}
						metaBytes, _ := json.Marshal(metaData)
						fmt.Fprintf(w, "data: %s\n\n", metaBytes)
						if f, ok := w.(http.Flusher); ok {
							f.Flush()
						}
						reasoningDurationSent = true
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

	finalText := responseBuilder.String()
	if enforceNoEmbeddedImageData {
		finalText = normalizeFireworksDrawToCodeOutput(finalText)
		if normalizedEmitted < len(finalText) {
			if err := emitFireworksStreamChunk(w, finalText[normalizedEmitted:]); err != nil {
				return err
			}
			normalizedEmitted = len(finalText)
		}
		responseBuilder.Reset()
		responseBuilder.WriteString(finalText)
	}

	if promptTokens == 0 && completionTokens == 0 {
		promptTokens = estimateTokenCountForMessages(messages)
		completionTokens = estimateTokenCount(responseBuilder.String())
	}
	if strings.TrimSpace(responseBuilder.String()) == "" {
		return fmt.Errorf("OpenRouter stream ended without content (model=%s, hasImage=%v, events=%d)", resolvedModel, hasImage, dataEvents)
	}

	if !enforceNoEmbeddedImageData && !reasoningDurationSent && reasoningStartTimeSet {
		durationSeconds := float64(time.Now().UnixMilli()-reasoningStartTimestamp) / 1000.0
		metaData := map[string]interface{}{
			"meta": map[string]float64{
				"thinkingSeconds": durationSeconds,
			},
		}
		metaBytes, _ := json.Marshal(metaData)
		fmt.Fprintf(w, "data: %s\n\n", metaBytes)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	if enforceNoEmbeddedImageData {
		if reason, blocked := detectFireworksEmbeddedImagePayload(responseBuilder.String()); blocked {
			return embeddedImagePayloadError(reason)
		}
	}
	if seenFinishReason != "" {
		fmt.Printf("[OpenRouter] stream finish_reason=%s model=%s\n", seenFinishReason, resolvedModel)
	}
	if finishReasonLength {
		fmt.Printf("[OpenRouter WARN] stream reached max_tokens=%d model=%s\n", maxTokens, resolvedModel)
		if enforceNoEmbeddedImageData {
			return fmt.Errorf("OpenRouter draw-to-code output was truncated at max_tokens=%d. Please regenerate with a shorter prompt.", maxTokens)
		}
	}

	fmt.Printf("[OpenRouter] stream complete model=%s hasImage=%v events=%d parseErrors=%d chars=%d\n",
		resolvedModel, hasImage, dataEvents, parseErrors, responseBuilder.Len())
	return emitFireworksStreamDone(w, promptTokens, completionTokens)
}

func openRouterMessagesContainImage(messages []map[string]interface{}) bool {
	for _, msg := range messages {
		content, ok := msg["content"]
		if !ok {
			continue
		}
		parts, ok := content.([]interface{})
		if !ok {
			continue
		}
		for _, part := range parts {
			partMap, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			if partType, _ := partMap["type"].(string); strings.EqualFold(partType, "image_url") {
				return true
			}
		}
	}
	return false
}

func openRouterModelSupportsImageInput(modelID string) bool {
	resolved := strings.ToLower(strings.TrimSpace(normalizeOpenRouterModelID(modelID)))
	switch resolved {
	case "moonshotai/kimi-k2.5":
		return true
	default:
		return false
	}
}

func openRouterAttachmentDataURI(attachmentBase64, attachmentMime string) string {
	trimmed := strings.TrimSpace(attachmentBase64)
	if strings.HasPrefix(strings.ToLower(trimmed), "data:") {
		return trimmed
	}
	mime := strings.TrimSpace(attachmentMime)
	if mime == "" {
		mime = "image/jpeg"
	}
	return fmt.Sprintf("data:%s;base64,%s", mime, trimmed)
}

func buildOpenRouterDrawToCodeMessages(imageBase64, userPrompt, template, imageSource, openRouterModel, apiKey string) []map[string]interface{} {
	effectiveImageSource := imageSource
	if !isGlowbomImagesSource(effectiveImageSource) {
		effectiveImageSource = "Glowbom Images"
		fmt.Printf("[OpenRouter DEBUG] forcing imageSource to Glowbom Images for safe placeholder output (was=%q)\n", imageSource)
	}
	resolvedModel := normalizeOpenRouterModelID(openRouterModel)

	systemPrompt := getSystemPrompt(template, effectiveImageSource)
	systemPrompt += " Replace @tailwind placeholders with the Tailwind CSS CDN link to load the real framework."
	systemPrompt += " Glowbom Images mode is strict: NEVER embed image bytes in output. NEVER output data: URIs, base64 blobs, blob: URLs, object URLs, or inline binary arrays."
	systemPrompt += " Use glowbyimage:<prompt> placeholders ONLY. Every <img> must start with src='about:blank', then JavaScript assigns .src from a glowbyimage variable."
	systemPrompt += " If you are uncertain about an image, create another descriptive glowbyimage:<prompt> placeholder instead of embedding bytes."
	systemPrompt += " Return a COMPLETE, valid HTML document from <!DOCTYPE html> through </html>. Never truncate output."
	systemPrompt += " Do NOT minify. Format output for humans with consistent indentation and spacing in CSS/JS (example: transition: all 0.3s ease; and color stops like #1a1a2e 0%)."
	systemPrompt += " Never put JavaScript code on the same line as // comments. If comments are used, they must be standalone lines."
	systemPrompt += " Glowbom image wiring contract: in one plain <script> block, first declare const image variables (each starting with glowbyimage:), then assign each to its matching img id via document.getElementById('...').src = ...."
	systemPrompt += " The image wiring script must contain ONLY: const image declarations and document.getElementById(...).src assignments. No other logic, no function definitions, no event handlers, no extra words."
	systemPrompt += " Put interactive JavaScript in a separate second <script> block so image assignment always works even if other logic has issues."
	systemPrompt += " Do not put declarations or assignments on the same line as comments. Keep one statement per line."
	systemPrompt += " Leave one blank line between major HTML sections so the output stays readable in code view."
	systemPrompt += " JavaScript must be syntactically valid. Never emit stray tokens before declarations (for example: 'functionality const')."
	systemPrompt += " Validate that every glowbyimage variable is assigned to an existing img id via document.getElementById(...).src before finishing."

	detailedTask := buildDetailedTaskDescription(template, effectiveImageSource, userPrompt)
	lines := []string{
		fmt.Sprintf("Detailed Task Description: %s", detailedTask),
		fmt.Sprintf("Human: %s", userPrompt),
	}
	usedImageDescription := false
	imageDescriptionSource := "none"
	useNativeImageInput := false

	if strings.TrimSpace(imageBase64) != "" {
		if openRouterModelSupportsImageInput(resolvedModel) {
			useNativeImageInput = true
			fmt.Printf("[OpenRouter DEBUG] draw received image payload (base64_len=%d), using native image_url input for model=%s\n", len(imageBase64), resolvedModel)
		} else {
			desc, err := describeImageWithOpenRouterKimi(imageBase64, apiKey)
			if err != nil {
				// Keep fallback so draw-to-code doesn't hard-fail if OpenRouter vision is temporarily unavailable.
				fmt.Printf("[OpenRouter WARN] Kimi image description unavailable; falling back to Ollama llama3.2-vision: %v\n", err)
				desc, err = getImageDescription(imageBase64)
				if err != nil {
					fmt.Printf("[OpenRouter WARN] image description unavailable; proceeding text-only: %v\n", err)
				} else if trimmed := strings.TrimSpace(desc); trimmed != "" {
					usedImageDescription = true
					imageDescriptionSource = "ollama-llama3.2-vision"
					fmt.Printf("[OpenRouter DEBUG] draw image description (%s) >>>\n%s\n<<< END image description\n", imageDescriptionSource, trimmed)
					lines = append([]string{fmt.Sprintf("Image description: %s", trimmed)}, lines...)
				}
			} else if trimmed := strings.TrimSpace(desc); trimmed != "" {
				usedImageDescription = true
				imageDescriptionSource = "openrouter-kimi-k2.5"
				fmt.Printf("[OpenRouter DEBUG] draw image description (%s) >>>\n%s\n<<< END image description\n", imageDescriptionSource, trimmed)
				lines = append([]string{fmt.Sprintf("Image description: %s", trimmed)}, lines...)
			}
		}
	}

	userContent := strings.Join(lines, "\n")
	fmt.Printf("[OpenRouter DEBUG] draw prompt usesImageDescription=%v imageDescriptionSource=%s nativeImageInput=%v\n", usedImageDescription, imageDescriptionSource, useNativeImageInput)
	fmt.Printf("[OpenRouter DEBUG] draw system prompt >>>\n%s\n<<< END system prompt\n", systemPrompt)
	fmt.Printf("[OpenRouter DEBUG] draw user prompt >>>\n%s\n<<< END user prompt\n", userContent)

	finalUserContent := interface{}(userContent)
	if useNativeImageInput {
		finalUserContent = []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": userContent,
			},
			map[string]interface{}{
				"type": "image_url",
				"image_url": map[string]interface{}{
					"url": openRouterAttachmentDataURI(imageBase64, "image/jpeg"),
				},
			},
		}
	}

	return []map[string]interface{}{
		{
			"role":    "system",
			"content": systemPrompt,
		},
		{
			"role":    "user",
			"content": finalUserContent,
		},
	}
}

func describeImageWithOpenRouterKimi(imageBase64, apiKey string) (string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return "", fmt.Errorf("missing OpenRouter API key")
	}
	if strings.TrimSpace(imageBase64) == "" {
		return "", fmt.Errorf("missing image payload")
	}

	const visionModel = "moonshotai/kimi-k2.5"
	payload := map[string]interface{}{
		"model":       visionModel,
		"max_tokens":  700,
		"temperature": 0.2,
		"messages": []map[string]interface{}{
			{
				"role": "system",
				"content": "Describe the image clearly in plain English. " +
					"Return only the description text. Do not use markdown, XML, JSON, or <think> tags.",
			},
			{
				"role": "user",
				"content": []map[string]interface{}{
					{
						"type": "text",
						"text": "Describe this image in human language. " +
							"If text appears in a foreign language, keep it as-is and mention it. " +
							"Include people, objects, setting, style, and notable details.",
					},
					{
						"type": "image_url",
						"image_url": map[string]interface{}{
							"url": openRouterAttachmentDataURI(imageBase64, "image/jpeg"),
						},
					},
				},
			},
		},
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", openRouterChatCompletionsURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return "", err
	}
	setOpenRouterRequestHeaders(req, apiKey, "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("OpenRouter Kimi vision API error (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content interface{} `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return "", fmt.Errorf("failed to parse OpenRouter Kimi vision response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return "", fmt.Errorf("OpenRouter Kimi vision returned no choices")
	}

	description := strings.TrimSpace(fireworkContentToText(completion.Choices[0].Message.Content))
	description = fireworksThinkTagRegex.ReplaceAllString(description, "")
	description = strings.TrimSpace(description)
	if description == "" {
		return "", fmt.Errorf("OpenRouter Kimi vision returned empty description")
	}
	return description, nil
}

func openRouterReasoningToText(reasoning interface{}) string {
	switch v := reasoning.(type) {
	case string:
		return v
	case map[string]interface{}:
		if text, ok := v["text"].(string); ok {
			return text
		}
		if content, ok := v["content"]; ok {
			return fireworkContentToText(content)
		}
		if summary, ok := v["summary"].([]interface{}); ok {
			var builder strings.Builder
			for _, item := range summary {
				itemMap, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				if text, ok := itemMap["text"].(string); ok {
					builder.WriteString(text)
				}
			}
			return builder.String()
		}
		return ""
	case []interface{}:
		var builder strings.Builder
		for _, item := range v {
			builder.WriteString(openRouterReasoningToText(item))
		}
		return builder.String()
	default:
		return ""
	}
}

func logOpenRouterRequestSummary(model string, stream bool, hasImage bool, maxTokens int, messages []map[string]interface{}) {
	promptChars := 0
	for _, message := range messages {
		content, ok := message["content"]
		if !ok {
			continue
		}
		promptChars += len(fireworkContentToText(content))
	}
	preview := truncateForLog(openRouterLatestUserPreview(messages))
	fmt.Printf("[OpenRouter] request model=%s stream=%v hasImage=%v max_tokens=%d messages=%d prompt_chars=%d preview=%q\n",
		model, stream, hasImage, maxTokens, len(messages), promptChars, preview)
}

func openRouterLatestUserPreview(messages []map[string]interface{}) string {
	for i := len(messages) - 1; i >= 0; i-- {
		role, _ := messages[i]["role"].(string)
		if strings.ToLower(strings.TrimSpace(role)) != "user" {
			continue
		}
		content := fireworkContentToText(messages[i]["content"])
		if strings.TrimSpace(content) != "" {
			return content
		}
	}
	return ""
}
