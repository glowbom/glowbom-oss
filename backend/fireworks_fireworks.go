package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const fireworksChatCompletionsURL = "https://api.fireworks.ai/inference/v1/chat/completions"

const (
	fireworksChatMaxTokens       = 4096
	fireworksDrawToCodeMaxTokens = 16384
	fireworksDefaultTemperature  = 0.6
	// Hold back a small tail so normalization can safely fix near-boundary chunks.
	fireworksStreamNormalizeHoldback = 256
)

var (
	compactCommentCodeRegex        = regexp.MustCompile(`(?m)(//[^\n]*?)\s+((?:const|let|var|function|document\.|window\.|if\s*\(|for\s*\(|[A-Za-z_$][A-Za-z0-9_$]*\.|[A-Za-z_$][A-Za-z0-9_$]*\())`)
	hexColorPercentCompactRegex    = regexp.MustCompile(`(#[0-9A-Fa-f]{3,8})(\d{1,3}%)`)
	transitionAllCompactRegex      = regexp.MustCompile(`transition:\s*all([0-9]+(?:\.[0-9]+)?s)`)
	animationDurationCompactRegex  = regexp.MustCompile(`animation:\s*([a-zA-Z_-][a-zA-Z0-9_-]*)([0-9]+(?:\.[0-9]+)?s)`)
	boxShadowDoubleZeroCompact     = regexp.MustCompile(`box-shadow:\s*00([0-9]+px)`)
	boxShadowCompactTwoOffsetRegex = regexp.MustCompile(`box-shadow:\s*0([0-9]+px)([0-9]+px)`)
	fireworksGenericDataURIRegex   = regexp.MustCompile(`(?is)data:[^"'\s>]+;base64,[A-Za-z0-9+/=\s]{128,}`)
	fireworksImgSrcBase64Regex     = regexp.MustCompile(`(?is)<img\b[^>]*\bsrc\s*=\s*["'][A-Za-z0-9+/=\s]{256,}["']`)
	fireworksImageSigBase64Regex   = regexp.MustCompile(`(?is)["'](?:iVBORw0KGgo|/9j/|R0lGOD|UklGR|Qk0)[A-Za-z0-9+/=\s]{256,}["']`)
	fireworksFunctionalityConstRE  = regexp.MustCompile(`(?i)\bfunctionality\s+const\b`)
	fireworksThinkTagRegex         = regexp.MustCompile(`(?is)</?think>`)
	// Go's regexp (RE2) rejects counted repeats above 1000.
	fireworksLongBase64RunRegex = regexp.MustCompile(`[A-Za-z0-9+/]{1000,}={0,2}`)
)

func callFireworksDrawToCodeApiFull(imageBase64, userPrompt, template, imageSource, apiKey, fireworksModel string) (*R1Response, error) {
	if strings.TrimSpace(apiKey) == "" {
		return &R1Response{
			AIResponse: "No Fireworks API key provided. Please add your key in Settings.",
			TokenUsage: map[string]int{"inputTokens": 0, "outputTokens": 0, "totalTokens": 0},
			Cost:       0,
		}, nil
	}

	messages := buildFireworksDrawToCodeMessages(imageBase64, userPrompt, template, imageSource, fireworksModel, apiKey)

	aiResp, inputTokens, outputTokens, err := callFireworksChatCompletions(messages, apiKey, fireworksDrawToCodeMaxTokens, fireworksDefaultTemperature, fireworksModel, true)
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

func callFireworksDrawToCodeStreaming(w http.ResponseWriter, imageBase64, userPrompt, template, imageSource, apiKey, fireworksModel string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("no Fireworks API key provided")
	}

	messages := buildFireworksDrawToCodeMessages(imageBase64, userPrompt, template, imageSource, fireworksModel, apiKey)

	return callFireworksChatCompletionsStreaming(w, messages, apiKey, fireworksDrawToCodeMaxTokens, fireworksDefaultTemperature, fireworksModel, true)
}

func callFireworksApiGo(prevMsgs []ChatMessage, newMsg, apiKey, fireworksModel, attachmentBase64, attachmentMime string) (*R1Response, error) {
	if strings.TrimSpace(apiKey) == "" {
		return &R1Response{
			AIResponse: "No Fireworks API key provided. Please add your key in Settings.",
			TokenUsage: map[string]int{"inputTokens": 0, "outputTokens": 0, "totalTokens": 0},
			Cost:       0,
		}, nil
	}

	messages := buildFireworksChatMessages(prevMsgs, newMsg, fireworksModel, attachmentBase64, attachmentMime)
	aiResp, inputTokens, outputTokens, err := callFireworksChatCompletions(messages, apiKey, fireworksChatMaxTokens, fireworksDefaultTemperature, fireworksModel, false)
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

func callFireworksChatStreaming(w http.ResponseWriter, prevMsgs []ChatMessage, newMsg, apiKey, fireworksModel, attachmentBase64, attachmentMime string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("no Fireworks API key provided")
	}
	messages := buildFireworksChatMessages(prevMsgs, newMsg, fireworksModel, attachmentBase64, attachmentMime)
	return callFireworksChatCompletionsStreaming(w, messages, apiKey, fireworksChatMaxTokens, fireworksDefaultTemperature, fireworksModel, false)
}

func buildFireworksChatMessages(prevMsgs []ChatMessage, newMsg, fireworksModel, attachmentBase64, attachmentMime string) []map[string]interface{} {
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
		resolvedModel := normalizeFireworksModelID(fireworksModel)
		if fireworksModelSupportsImageInput(resolvedModel) {
			if strings.HasPrefix(strings.ToLower(trimmedMime), "image/") {
				userContent = []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": newMsg,
					},
					map[string]interface{}{
						"type": "image_url",
						"image_url": map[string]interface{}{
							"url": fireworksAttachmentDataURI(trimmedAttachment, trimmedMime),
						},
					},
				}
				fmt.Printf("[Fireworks DEBUG] inline image attachment enabled model=%s mime=%s\n", resolvedModel, trimmedMime)
			} else {
				fmt.Printf("[Fireworks DEBUG] attachment ignored for model=%s (non-image mime=%q)\n", resolvedModel, trimmedMime)
			}
		} else {
			fmt.Printf("[Fireworks DEBUG] attachment ignored for model=%s (image input unsupported)\n", resolvedModel)
		}
	}

	messages = append(messages, map[string]interface{}{
		"role":    "user",
		"content": userContent,
	})
	return messages
}

func callFireworksChatCompletions(messages []map[string]interface{}, apiKey string, maxTokens int, temperature float64, fireworksModel string, enforceNoEmbeddedImageData bool) (string, int, int, error) {
	resolvedModel := normalizeFireworksModelID(fireworksModel)
	hasImage := fireworksMessagesContainImage(messages)
	logFireworksRequestSummary(resolvedModel, false, hasImage, maxTokens, messages)
	reqBody := map[string]interface{}{
		"model":             resolvedModel,
		"max_tokens":        maxTokens,
		"top_p":             1,
		"top_k":             40,
		"presence_penalty":  0,
		"frequency_penalty": 0,
		"temperature":       temperature,
		"messages":          messages,
	}
	if prettyBody, err := json.MarshalIndent(reqBody, "", "  "); err == nil {
		fmt.Printf("[Fireworks DEBUG] request body stream=false >>>\n%s\n<<< END Fireworks request body\n", string(prettyBody))
	}

	jsonBytes, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", fireworksChatCompletionsURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return "", 0, 0, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyText := strings.TrimSpace(string(body))
		fmt.Printf("[Fireworks ERROR] model=%s stream=false status=%d hasImage=%v body=%s\n", resolvedModel, resp.StatusCode, hasImage, truncateForLog(bodyText))
		return "", 0, 0, fmt.Errorf("Fireworks API error (%d): %s", resp.StatusCode, bodyText)
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content          interface{} `json:"content"`
				ReasoningContent interface{} `json:"reasoning_content"`
			} `json:"message"`
			ReasoningContent interface{} `json:"reasoning_content"`
			FinishReason     interface{} `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return "", 0, 0, fmt.Errorf("failed to parse Fireworks response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return "", 0, 0, fmt.Errorf("no choices in Fireworks response")
	}

	content := fireworkContentToText(completion.Choices[0].Message.Content)
	reasoning := fireworkContentToText(completion.Choices[0].Message.ReasoningContent)
	if strings.TrimSpace(reasoning) == "" {
		reasoning = fireworkContentToText(completion.Choices[0].ReasoningContent)
	}
	if !enforceNoEmbeddedImageData && strings.TrimSpace(reasoning) != "" {
		content = "<think>" + strings.TrimSpace(reasoning) + "</think>" + content
	}
	if enforceNoEmbeddedImageData {
		content = normalizeFireworksDrawToCodeOutput(content)
	}
	if enforceNoEmbeddedImageData {
		if reason, blocked := detectFireworksEmbeddedImagePayload(content); blocked {
			return "", 0, 0, embeddedImagePayloadError(reason)
		}
	}
	finishReason := normalizeFinishReason(completion.Choices[0].FinishReason)
	if finishReason != "" {
		fmt.Printf("[Fireworks] completion finish_reason=%s model=%s stream=false\n", finishReason, resolvedModel)
	}
	if strings.EqualFold(finishReason, "length") {
		fmt.Printf("[Fireworks WARN] completion reached max_tokens=%d model=%s stream=false\n", maxTokens, resolvedModel)
		if enforceNoEmbeddedImageData {
			return "", 0, 0, fmt.Errorf("Fireworks draw-to-code output was truncated at max_tokens=%d. Please regenerate with a shorter prompt.", maxTokens)
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

func callFireworksChatCompletionsStreaming(w http.ResponseWriter, messages []map[string]interface{}, apiKey string, maxTokens int, temperature float64, fireworksModel string, enforceNoEmbeddedImageData bool) error {
	resolvedModel := normalizeFireworksModelID(fireworksModel)
	hasImage := fireworksMessagesContainImage(messages)
	logFireworksRequestSummary(resolvedModel, true, hasImage, maxTokens, messages)
	reqBody := map[string]interface{}{
		"model":             resolvedModel,
		"max_tokens":        maxTokens,
		"top_p":             1,
		"top_k":             40,
		"presence_penalty":  0,
		"frequency_penalty": 0,
		"temperature":       temperature,
		"messages":          messages,
		"stream":            true,
	}
	if prettyBody, err := json.MarshalIndent(reqBody, "", "  "); err == nil {
		fmt.Printf("[Fireworks DEBUG] request body stream=true >>>\n%s\n<<< END Fireworks request body\n", string(prettyBody))
	}

	jsonBytes, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", fireworksChatCompletionsURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyText := strings.TrimSpace(string(body))
		fmt.Printf("[Fireworks ERROR] model=%s stream=true status=%d hasImage=%v body=%s\n", resolvedModel, resp.StatusCode, hasImage, truncateForLog(bodyText))
		return fmt.Errorf("Fireworks API error (%d): %s", resp.StatusCode, bodyText)
	}

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	fmt.Printf("[Fireworks] stream response model=%s hasImage=%v contentType=%q\n", resolvedModel, hasImage, contentType)

	// Some models/multimodal paths may ignore stream=true and return a regular JSON completion.
	// Handle that gracefully instead of silently returning an empty streamed result.
	if !strings.Contains(contentType, "text/event-stream") {
		body, _ := io.ReadAll(resp.Body)
		bodyText := strings.TrimSpace(string(body))
		fmt.Printf("[Fireworks] stream fallback model=%s hasImage=%v contentType=%q body=%s\n", resolvedModel, hasImage, contentType, truncateForLog(bodyText))

		var completion struct {
			Choices []struct {
				Message struct {
					Content          interface{} `json:"content"`
					ReasoningContent interface{} `json:"reasoning_content"`
				} `json:"message"`
				ReasoningContent interface{} `json:"reasoning_content"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &completion); err != nil {
			return fmt.Errorf("Fireworks stream fallback parse error: %w", err)
		}
		if len(completion.Choices) == 0 {
			return fmt.Errorf("Fireworks stream fallback returned no choices")
		}

		content := fireworkContentToText(completion.Choices[0].Message.Content)
		reasoning := fireworkContentToText(completion.Choices[0].Message.ReasoningContent)
		if strings.TrimSpace(reasoning) == "" {
			reasoning = fireworkContentToText(completion.Choices[0].ReasoningContent)
		}
		if !enforceNoEmbeddedImageData && strings.TrimSpace(reasoning) != "" {
			content = "<think>" + strings.TrimSpace(reasoning) + "</think>" + content
		}
		if enforceNoEmbeddedImageData {
			content = normalizeFireworksDrawToCodeOutput(content)
		}
		if enforceNoEmbeddedImageData {
			if reason, blocked := detectFireworksEmbeddedImagePayload(content); blocked {
				return embeddedImagePayloadError(reason)
			}
		}
		if strings.TrimSpace(content) == "" {
			return fmt.Errorf("Fireworks stream fallback returned empty content")
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
	// Keep real-time streaming for draw-to-code UX.
	bufferDrawOutput := false
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
						} `json:"delta"`
						Message struct {
							Content          interface{} `json:"content"`
							ReasoningContent interface{} `json:"reasoning_content"`
						} `json:"message"`
						ReasoningContent interface{} `json:"reasoning_content"`
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
						fmt.Printf("[Fireworks WARN] stream chunk parse failed model=%s hasImage=%v err=%v raw=%s\n", resolvedModel, hasImage, unmarshalErr, truncateForLog(dataStr))
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

						// Emit better step titles as soon as enough reasoning text arrives.
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
					if !bufferDrawOutput {
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
	}
	if !bufferDrawOutput && enforceNoEmbeddedImageData {
		if normalizedEmitted < len(finalText) {
			if err := emitFireworksStreamChunk(w, finalText[normalizedEmitted:]); err != nil {
				return err
			}
			normalizedEmitted = len(finalText)
		}
		responseBuilder.Reset()
		responseBuilder.WriteString(finalText)
	}
	if bufferDrawOutput {
		if enforceNoEmbeddedImageData {
			finalText = normalizeFireworksDrawToCodeOutput(finalText)
		}
		responseBuilder.Reset()
		responseBuilder.WriteString(finalText)
		if strings.TrimSpace(finalText) != "" {
			if err := emitFireworksStreamChunk(w, finalText); err != nil {
				return err
			}
		}
	}

	if promptTokens == 0 && completionTokens == 0 {
		promptTokens = estimateTokenCountForMessages(messages)
		completionTokens = estimateTokenCount(responseBuilder.String())
	}
	if strings.TrimSpace(responseBuilder.String()) == "" {
		return fmt.Errorf("Fireworks stream ended without content (model=%s, hasImage=%v, events=%d)", resolvedModel, hasImage, dataEvents)
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
	if enforceNoEmbeddedImageData {
		if reason, blocked := detectFireworksEmbeddedImagePayload(responseBuilder.String()); blocked {
			return embeddedImagePayloadError(reason)
		}
	}
	if seenFinishReason != "" {
		fmt.Printf("[Fireworks] stream finish_reason=%s model=%s\n", seenFinishReason, resolvedModel)
	}
	if finishReasonLength {
		fmt.Printf("[Fireworks WARN] stream reached max_tokens=%d model=%s\n", maxTokens, resolvedModel)
		if enforceNoEmbeddedImageData {
			return fmt.Errorf("Fireworks draw-to-code output was truncated at max_tokens=%d. Please regenerate with a shorter prompt.", maxTokens)
		}
	}

	fmt.Printf("[Fireworks] stream complete model=%s hasImage=%v events=%d parseErrors=%d chars=%d\n",
		resolvedModel, hasImage, dataEvents, parseErrors, responseBuilder.Len())
	return emitFireworksStreamDone(w, promptTokens, completionTokens)
}

func fireworkContentToText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case map[string]interface{}:
		if text, ok := v["text"].(string); ok {
			return text
		}
		if nested, ok := v["content"]; ok {
			return fireworkContentToText(nested)
		}
		return ""
	case []interface{}:
		var builder strings.Builder
		for _, part := range v {
			partMap, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			partType, _ := partMap["type"].(string)
			if (partType == "text" || partType == "output_text") && partMap["text"] != nil {
				if text, ok := partMap["text"].(string); ok {
					builder.WriteString(text)
					continue
				}
			}
			if text, ok := partMap["text"].(string); ok {
				builder.WriteString(text)
				continue
			}
			if nested, ok := partMap["content"]; ok {
				builder.WriteString(fireworkContentToText(nested))
			}
		}
		return builder.String()
	default:
		return ""
	}
}

func estimateTokenCountForMessages(messages []map[string]interface{}) int {
	totalChars := 0
	for _, msg := range messages {
		if content, ok := msg["content"]; ok {
			totalChars += len(fireworkContentToText(content))
		}
	}
	return estimateTokenCount(strings.Repeat("x", totalChars))
}

func estimateTokenCount(text string) int {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0
	}
	return (len(trimmed) + 3) / 4
}

func fireworksMessagesContainImage(messages []map[string]interface{}) bool {
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

func fireworksModelSupportsImageInput(modelID string) bool {
	resolved := strings.ToLower(strings.TrimSpace(normalizeFireworksModelID(modelID)))
	switch resolved {
	case "accounts/fireworks/models/kimi-k2p5":
		return true
	default:
		return false
	}
}

func fireworksAttachmentDataURI(attachmentBase64, attachmentMime string) string {
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

func buildFireworksDrawToCodeMessages(imageBase64, userPrompt, template, imageSource, fireworksModel, apiKey string) []map[string]interface{} {
	effectiveImageSource := imageSource
	if !isGlowbomImagesSource(effectiveImageSource) {
		effectiveImageSource = "Glowbom Images"
		fmt.Printf("[Fireworks DEBUG] forcing imageSource to Glowbom Images for safe placeholder output (was=%q)\n", imageSource)
	}
	resolvedModel := normalizeFireworksModelID(fireworksModel)

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
		if fireworksModelSupportsImageInput(resolvedModel) {
			useNativeImageInput = true
			fmt.Printf("[Fireworks DEBUG] draw received image payload (base64_len=%d), using native image_url input for model=%s\n", len(imageBase64), resolvedModel)
		} else {
			fmt.Printf("[Fireworks DEBUG] draw received image payload (base64_len=%d), deriving image description via Fireworks Kimi K2.5\n", len(imageBase64))
			desc, err := describeImageWithFireworksKimi(imageBase64, apiKey)
			if err != nil {
				// Keep legacy fallback so GLM-5 draw-to-code doesn't hard-fail if Kimi vision is temporarily unavailable.
				fmt.Printf("[Fireworks WARN] Kimi image description unavailable; falling back to Ollama llama3.2-vision: %v\n", err)
				desc, err = getImageDescription(imageBase64)
				if err != nil {
					// Models like GLM-5 reject image_url on this endpoint.
					// Continue with text-only context instead of failing the whole request.
					fmt.Printf("[Fireworks WARN] image description unavailable; proceeding text-only: %v\n", err)
				} else if trimmed := strings.TrimSpace(desc); trimmed != "" {
					usedImageDescription = true
					imageDescriptionSource = "ollama-llama3.2-vision"
					fmt.Printf("[Fireworks DEBUG] draw image description (%s) >>>\n%s\n<<< END image description\n", imageDescriptionSource, trimmed)
					lines = append([]string{fmt.Sprintf("Image description: %s", trimmed)}, lines...)
				}
			} else if trimmed := strings.TrimSpace(desc); trimmed != "" {
				usedImageDescription = true
				imageDescriptionSource = "fireworks-kimi-k2p5"
				fmt.Printf("[Fireworks DEBUG] draw image description (%s) >>>\n%s\n<<< END image description\n", imageDescriptionSource, trimmed)
				lines = append([]string{fmt.Sprintf("Image description: %s", trimmed)}, lines...)
			}
		}
	}
	userContent := strings.Join(lines, "\n")
	fmt.Printf("[Fireworks DEBUG] draw prompt usesImageDescription=%v imageDescriptionSource=%s nativeImageInput=%v\n", usedImageDescription, imageDescriptionSource, useNativeImageInput)
	fmt.Printf("[Fireworks DEBUG] draw system prompt >>>\n%s\n<<< END system prompt\n", systemPrompt)
	fmt.Printf("[Fireworks DEBUG] draw user prompt >>>\n%s\n<<< END user prompt\n", userContent)

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
					"url": fireworksAttachmentDataURI(imageBase64, "image/jpeg"),
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

func describeImageWithFireworksKimi(imageBase64, apiKey string) (string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return "", fmt.Errorf("missing Fireworks API key")
	}
	if strings.TrimSpace(imageBase64) == "" {
		return "", fmt.Errorf("missing image payload")
	}

	const visionModel = "accounts/fireworks/models/kimi-k2p5"
	payload := map[string]interface{}{
		"model":             visionModel,
		"max_tokens":        700,
		"top_p":             1,
		"top_k":             40,
		"presence_penalty":  0,
		"frequency_penalty": 0,
		"temperature":       0.2,
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
							"url": fireworksAttachmentDataURI(imageBase64, "image/jpeg"),
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

	req, err := http.NewRequest("POST", fireworksChatCompletionsURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Fireworks Kimi vision API error (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content interface{} `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return "", fmt.Errorf("failed to parse Fireworks Kimi vision response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return "", fmt.Errorf("Fireworks Kimi vision returned no choices")
	}

	description := strings.TrimSpace(fireworkContentToText(completion.Choices[0].Message.Content))
	description = fireworksThinkTagRegex.ReplaceAllString(description, "")
	description = strings.TrimSpace(description)
	if description == "" {
		return "", fmt.Errorf("Fireworks Kimi vision returned empty description")
	}
	return description, nil
}

func emitFireworksStreamChunk(w http.ResponseWriter, content string) error {
	eventData, err := json.Marshal(map[string]string{"chunk": content})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", eventData); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func emitFireworksStreamDone(w http.ResponseWriter, promptTokens, completionTokens int) error {
	doneData := map[string]interface{}{
		"done": true,
		"tokenUsage": map[string]int{
			"inputTokens":  promptTokens,
			"outputTokens": completionTokens,
			"totalTokens":  promptTokens + completionTokens,
		},
		"cost": 0.0,
	}
	doneBytes, err := json.Marshal(doneData)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", doneBytes); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func logFireworksRequestSummary(model string, stream bool, hasImage bool, maxTokens int, messages []map[string]interface{}) {
	promptChars := 0
	for _, message := range messages {
		content, ok := message["content"]
		if !ok {
			continue
		}
		promptChars += len(fireworkContentToText(content))
	}
	preview := truncateForLog(fireworksLatestUserPreview(messages))
	fmt.Printf("[Fireworks] request model=%s stream=%v hasImage=%v max_tokens=%d messages=%d prompt_chars=%d preview=%q\n",
		model, stream, hasImage, maxTokens, len(messages), promptChars, preview)
}

func fireworksLatestUserPreview(messages []map[string]interface{}) string {
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

func normalizeFinishReason(reason interface{}) string {
	switch v := reason.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

func detectFireworksEmbeddedImagePayload(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "", false
	}

	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "data:image/") && strings.Contains(lower, ";base64,") {
		return "data:image base64 URI", true
	}
	if fireworksGenericDataURIRegex.MatchString(trimmed) {
		return "generic data URI with base64 payload", true
	}
	if fireworksImgSrcBase64Regex.MatchString(trimmed) {
		return "img src contains inline base64 payload", true
	}
	if fireworksImageSigBase64Regex.MatchString(trimmed) {
		return "quoted base64 image-signature payload", true
	}
	if strings.Contains(lower, "base64") && fireworksLongBase64RunRegex.MatchString(trimmed) {
		return "long base64-like payload", true
	}

	return "", false
}

func embeddedImagePayloadError(reason string) error {
	return fmt.Errorf("generation blocked: model returned embedded image payload (%s). Regenerate using glowbyimage placeholders only", reason)
}

func normalizeFireworksDrawToCodeOutput(content string) string {
	normalized := strings.TrimSpace(content)
	if normalized == "" {
		return normalized
	}

	// Common GLM-5 compacting artifacts that break JS/CSS execution/readability.
	normalized = compactCommentCodeRegex.ReplaceAllString(normalized, "$1\n$2")
	normalized = hexColorPercentCompactRegex.ReplaceAllString(normalized, "$1 $2")
	normalized = transitionAllCompactRegex.ReplaceAllString(normalized, "transition: all $1")
	normalized = animationDurationCompactRegex.ReplaceAllString(normalized, "animation: $1 $2")
	normalized = boxShadowDoubleZeroCompact.ReplaceAllString(normalized, "box-shadow: 0 0 $1")
	normalized = boxShadowCompactTwoOffsetRegex.ReplaceAllString(normalized, "box-shadow: 0 $1 $2")
	normalized = fireworksFunctionalityConstRE.ReplaceAllString(normalized, "const")

	normalized = strings.ReplaceAll(normalized, "0%,100%", "0%, 100%")
	normalized = strings.ReplaceAll(normalized, "viewBox=\"002424\"", "viewBox=\"0 0 24 24\"")
	normalized = strings.ReplaceAll(normalized, "viewBox=\"002020\"", "viewBox=\"0 0 20 20\"")
	normalized = strings.ReplaceAll(normalized, "rootMargin: '0px0px -50px0px'", "rootMargin: '0px 0px -50px 0px'")

	return normalized
}

func extractFireworksThinkingStepTitle(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	lines := strings.Split(trimmed, "\n")
	for _, line := range lines {
		candidate := strings.TrimSpace(line)
		if candidate == "" {
			continue
		}

		// Convert "1. Analyze the input: ..." -> "Analyze the input"
		if dot := strings.Index(candidate, ". "); dot > 0 {
			prefix := candidate[:dot]
			isNumber := true
			for _, ch := range prefix {
				if ch < '0' || ch > '9' {
					isNumber = false
					break
				}
			}
			if isNumber {
				candidate = strings.TrimSpace(candidate[dot+2:])
			}
		}

		if colon := strings.Index(candidate, ":"); colon > 0 {
			candidate = strings.TrimSpace(candidate[:colon])
		}
		if len(candidate) > 96 {
			candidate = strings.TrimSpace(candidate[:96]) + "..."
		}
		return candidate
	}

	return ""
}

func isMeaningfulThinkingStepTitle(title string) bool {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return false
	}
	if strings.EqualFold(trimmed, "thinking...") {
		return false
	}
	// Avoid tiny first-token fragments like "The" while stream is still warming up.
	if len(trimmed) < 10 {
		return false
	}
	if !strings.Contains(trimmed, " ") {
		return false
	}
	return true
}
