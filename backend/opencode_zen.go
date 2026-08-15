package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const openCodeZenChatCompletionsURL = "https://opencode.ai/zen/v1/chat/completions"

const (
	openCodeZenChatMaxTokens       = 4096
	openCodeZenDrawToCodeMaxTokens = 16384
	openCodeZenDefaultTemperature  = 0.6
	openCodeZenHeaderTimeout       = 90 * time.Second
)

var openCodeZenHTTPClient = newOpenCodeZenHTTPClient()

type openCodeZenAPIError struct {
	Status  int
	Model   string
	Message string
}

func (e *openCodeZenAPIError) Error() string {
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return fmt.Sprintf("OpenCode Zen API error (%d) for %s", e.Status, e.Model)
}

func newOpenCodeZenHTTPClient() *http.Client {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport := base.Clone()
		transport.ResponseHeaderTimeout = openCodeZenHeaderTimeout
		return &http.Client{Transport: transport}
	}
	return &http.Client{Timeout: openCodeZenHeaderTimeout}
}

func openCodeZenModelCandidates(modelID string, allowCrossModelFallback bool) []string {
	normalized := normalizeOpenCodeZenModelID(modelID)
	candidates := []string{normalized}

	if allowCrossModelFallback {
		switch normalized {
		case "glm-5-free", "minimax-m2.5-free", "big-pickle":
			// Free-tier fallback that is currently the most stable in practice.
			candidates = append(candidates, "kimi-k2.5-free")
		}
	}

	seen := make(map[string]struct{})
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

func openCodeZenShouldTryFallbackModel(err error) bool {
	var apiErr *openCodeZenAPIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		if strings.Contains(strings.ToLower(apiErr.Message), "rate limit") {
			return true
		}
		if strings.Contains(strings.ToLower(apiErr.Message), "timeout") {
			return true
		}
		return false
	}

	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "temporarily unavailable")
}

func openCodeZenExtractProviderErrorMessage(bodyText string) string {
	trimmed := strings.TrimSpace(bodyText)
	if trimmed == "" {
		return ""
	}

	extractFromObject := func(obj map[string]interface{}) string {
		if errVal, ok := obj["error"]; ok {
			switch e := errVal.(type) {
			case string:
				if strings.TrimSpace(e) != "" {
					return strings.TrimSpace(e)
				}
			case map[string]interface{}:
				if msg, ok := e["message"].(string); ok && strings.TrimSpace(msg) != "" {
					return strings.TrimSpace(msg)
				}
				if msg, ok := e["type"].(string); ok && strings.TrimSpace(msg) != "" {
					return strings.TrimSpace(msg)
				}
			}
		}
		if msg, ok := obj["message"].(string); ok && strings.TrimSpace(msg) != "" {
			return strings.TrimSpace(msg)
		}
		return ""
	}

	lines := strings.Split(trimmed, "\n")
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(line), "data:") {
			line = strings.TrimSpace(line[5:])
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err == nil {
			if msg := extractFromObject(obj); msg != "" {
				return msg
			}
		}
	}

	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
		if msg := extractFromObject(obj); msg != "" {
			return msg
		}
	}

	if idx := strings.Index(strings.ToLower(trimmed), "\ndata:"); idx >= 0 {
		trimmed = strings.TrimSpace(trimmed[:idx])
	}
	if len(trimmed) > 400 {
		return trimmed[:400] + "..."
	}
	return trimmed
}

func openCodeZenStatusError(status int, model, bodyText string) error {
	providerMessage := openCodeZenExtractProviderErrorMessage(bodyText)
	if providerMessage == "" {
		providerMessage = fmt.Sprintf("HTTP %d", status)
	}

	lower := strings.ToLower(providerMessage)
	if status == http.StatusTooManyRequests || strings.Contains(lower, "rate limit") {
		return &openCodeZenAPIError{
			Status:  http.StatusTooManyRequests,
			Model:   model,
			Message: fmt.Sprintf("OpenCode Zen rate limit for %s: %s", model, providerMessage),
		}
	}

	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &openCodeZenAPIError{
			Status:  status,
			Model:   model,
			Message: fmt.Sprintf("OpenCode Zen authentication failed for %s: %s", model, providerMessage),
		}
	}

	if status == http.StatusRequestTimeout || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout || strings.Contains(lower, "timeout") {
		return &openCodeZenAPIError{
			Status:  status,
			Model:   model,
			Message: fmt.Sprintf("OpenCode Zen temporary timeout for %s: %s", model, providerMessage),
		}
	}

	return &openCodeZenAPIError{
		Status:  status,
		Model:   model,
		Message: fmt.Sprintf("OpenCode Zen API error (%d) for %s: %s", status, model, providerMessage),
	}
}

func setOpenCodeZenRequestHeaders(req *http.Request, apiKey string, accept string) {
	req.Header.Set("Accept", accept)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

func openCodeZenAPIModelID(modelID string) string {
	// Zen OpenAI-compatible endpoints expect bare model ids (for example "kimi-k2.5"),
	// not OpenCode config ids like "opencode/kimi-k2.5".
	return normalizeOpenCodeZenModelID(modelID)
}

func callOpenCodeZenDrawToCodeApiFull(imageBase64, userPrompt, template, imageSource, apiKey, openCodeZenModel string) (*R1Response, error) {
	if strings.TrimSpace(apiKey) == "" {
		return &R1Response{
			AIResponse: "No OpenCode Zen API key provided. Please add your key in Settings.",
			TokenUsage: map[string]int{"inputTokens": 0, "outputTokens": 0, "totalTokens": 0},
			Cost:       0,
		}, nil
	}

	messages := buildOpenCodeZenDrawToCodeMessages(imageBase64, userPrompt, template, imageSource, openCodeZenModel, apiKey)
	aiResp, inputTokens, outputTokens, err := callOpenCodeZenChatCompletions(messages, apiKey, openCodeZenDrawToCodeMaxTokens, openCodeZenDefaultTemperature, openCodeZenModel, true, true)
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

func callOpenCodeZenDrawToCodeStreaming(w http.ResponseWriter, imageBase64, userPrompt, template, imageSource, apiKey, openCodeZenModel string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("no OpenCode Zen API key provided")
	}

	messages := buildOpenCodeZenDrawToCodeMessages(imageBase64, userPrompt, template, imageSource, openCodeZenModel, apiKey)
	return callOpenCodeZenChatCompletionsStreaming(w, messages, apiKey, openCodeZenDrawToCodeMaxTokens, openCodeZenDefaultTemperature, openCodeZenModel, true, true)
}

func callOpenCodeZenApiGo(prevMsgs []ChatMessage, newMsg, apiKey, openCodeZenModel, attachmentBase64, attachmentMime string) (*R1Response, error) {
	if strings.TrimSpace(apiKey) == "" {
		return &R1Response{
			AIResponse: "No OpenCode Zen API key provided. Please add your key in Settings.",
			TokenUsage: map[string]int{"inputTokens": 0, "outputTokens": 0, "totalTokens": 0},
			Cost:       0,
		}, nil
	}

	messages := buildOpenCodeZenChatMessages(prevMsgs, newMsg, openCodeZenModel, attachmentBase64, attachmentMime)
	aiResp, inputTokens, outputTokens, err := callOpenCodeZenChatCompletions(messages, apiKey, openCodeZenChatMaxTokens, openCodeZenDefaultTemperature, openCodeZenModel, false, false)
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

func callOpenCodeZenChatStreaming(w http.ResponseWriter, prevMsgs []ChatMessage, newMsg, apiKey, openCodeZenModel, attachmentBase64, attachmentMime string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("no OpenCode Zen API key provided")
	}
	messages := buildOpenCodeZenChatMessages(prevMsgs, newMsg, openCodeZenModel, attachmentBase64, attachmentMime)
	return callOpenCodeZenChatCompletionsStreaming(w, messages, apiKey, openCodeZenChatMaxTokens, openCodeZenDefaultTemperature, openCodeZenModel, false, false)
}

func buildOpenCodeZenChatMessages(prevMsgs []ChatMessage, newMsg, openCodeZenModel, attachmentBase64, attachmentMime string) []map[string]interface{} {
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
		resolvedModel := normalizeOpenCodeZenModelID(openCodeZenModel)
		if openCodeZenModelSupportsImageInput(resolvedModel) && strings.HasPrefix(strings.ToLower(trimmedMime), "image/") {
			userContent = []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": newMsg,
				},
				map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]interface{}{
						"url": openCodeZenAttachmentDataURI(trimmedAttachment, trimmedMime),
					},
				},
			}
			fmt.Printf("[OpenCode Zen DEBUG] inline image attachment enabled model=%s mime=%s\n", resolvedModel, trimmedMime)
		} else {
			fmt.Printf("[OpenCode Zen DEBUG] attachment ignored for model=%s mime=%q\n", resolvedModel, trimmedMime)
		}
	}

	messages = append(messages, map[string]interface{}{
		"role":    "user",
		"content": userContent,
	})
	return messages
}

func callOpenCodeZenChatCompletions(messages []map[string]interface{}, apiKey string, maxTokens int, temperature float64, openCodeZenModel string, enforceNoEmbeddedImageData bool, allowCrossModelFallback bool) (string, int, int, error) {
	candidates := openCodeZenModelCandidates(openCodeZenModel, allowCrossModelFallback)
	var lastErr error
	for idx, candidate := range candidates {
		content, inputTokens, outputTokens, err := callOpenCodeZenChatCompletionsSingle(messages, apiKey, maxTokens, temperature, candidate, enforceNoEmbeddedImageData)
		if err == nil {
			if idx > 0 {
				fmt.Printf("[OpenCode Zen] fallback model used original=%s fallback=%s\n", normalizeOpenCodeZenModelID(openCodeZenModel), candidate)
			}
			return content, inputTokens, outputTokens, nil
		}
		lastErr = err
		if idx < len(candidates)-1 && openCodeZenShouldTryFallbackModel(err) {
			fmt.Printf("[OpenCode Zen WARN] model=%s failed (%v); trying fallback model=%s\n", candidate, err, candidates[idx+1])
			continue
		}
		return "", 0, 0, err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("OpenCode Zen request failed")
	}
	return "", 0, 0, lastErr
}

func callOpenCodeZenChatCompletionsSingle(messages []map[string]interface{}, apiKey string, maxTokens int, temperature float64, zenModelToUse string, enforceNoEmbeddedImageData bool) (string, int, int, error) {
	resolvedModel := normalizeOpenCodeZenModelID(zenModelToUse)
	apiModel := openCodeZenAPIModelID(resolvedModel)
	hasImage := openCodeZenMessagesContainImage(messages)
	logOpenCodeZenRequestSummary(apiModel, false, hasImage, maxTokens, messages)

	reqBody := map[string]interface{}{
		"model":       apiModel,
		"max_tokens":  maxTokens,
		"temperature": temperature,
		"messages":    messages,
	}
	if prettyBody, err := json.MarshalIndent(reqBody, "", "  "); err == nil {
		fmt.Printf("[OpenCode Zen DEBUG] request body stream=false >>>\n%s\n<<< END OpenCode Zen request body\n", string(prettyBody))
	}

	jsonBytes, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", openCodeZenChatCompletionsURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return "", 0, 0, err
	}
	setOpenCodeZenRequestHeaders(req, apiKey, "application/json")

	resp, err := openCodeZenHTTPClient.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyText := strings.TrimSpace(string(body))
		fmt.Printf("[OpenCode Zen ERROR] model=%s stream=false status=%d hasImage=%v body=%s\n", apiModel, resp.StatusCode, hasImage, truncateForLog(bodyText))
		return "", 0, 0, openCodeZenStatusError(resp.StatusCode, apiModel, bodyText)
	}

	var completion struct {
		Error   interface{} `json:"error"`
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
		} `json:"usage"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return "", 0, 0, fmt.Errorf("failed to parse OpenCode Zen response: %w", err)
	}
	if len(completion.Choices) == 0 {
		if completion.Error != nil {
			raw, _ := json.Marshal(map[string]interface{}{"error": completion.Error})
			return "", 0, 0, openCodeZenStatusError(http.StatusBadGateway, apiModel, string(raw))
		}
		return "", 0, 0, fmt.Errorf("no choices in OpenCode Zen response")
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
		fmt.Printf("[OpenCode Zen] completion finish_reason=%s model=%s stream=false\n", finishReason, apiModel)
	}
	if strings.EqualFold(finishReason, "length") {
		fmt.Printf("[OpenCode Zen WARN] completion reached max_tokens=%d model=%s stream=false\n", maxTokens, apiModel)
	}

	inputTokens := completion.Usage.PromptTokens
	outputTokens := completion.Usage.CompletionTokens
	if inputTokens == 0 && outputTokens == 0 {
		inputTokens = estimateTokenCountForMessages(messages)
		outputTokens = estimateTokenCount(content)
	}

	return content, inputTokens, outputTokens, nil
}

func callOpenCodeZenChatCompletionsStreaming(w http.ResponseWriter, messages []map[string]interface{}, apiKey string, maxTokens int, temperature float64, openCodeZenModel string, enforceNoEmbeddedImageData bool, allowCrossModelFallback bool) error {
	candidates := openCodeZenModelCandidates(openCodeZenModel, allowCrossModelFallback)
	var lastErr error
	for idx, candidate := range candidates {
		err := callOpenCodeZenChatCompletionsStreamingSingle(w, messages, apiKey, maxTokens, temperature, candidate, enforceNoEmbeddedImageData)
		if err == nil {
			if idx > 0 {
				fmt.Printf("[OpenCode Zen] fallback model used original=%s fallback=%s\n", normalizeOpenCodeZenModelID(openCodeZenModel), candidate)
			}
			return nil
		}
		lastErr = err
		if idx < len(candidates)-1 && openCodeZenShouldTryFallbackModel(err) {
			fmt.Printf("[OpenCode Zen WARN] stream model=%s failed (%v); trying fallback model=%s\n", candidate, err, candidates[idx+1])
			continue
		}
		return err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("OpenCode Zen stream request failed")
	}
	return lastErr
}

func callOpenCodeZenChatCompletionsStreamingSingle(w http.ResponseWriter, messages []map[string]interface{}, apiKey string, maxTokens int, temperature float64, zenModelToUse string, enforceNoEmbeddedImageData bool) error {
	resolvedModel := normalizeOpenCodeZenModelID(zenModelToUse)
	apiModel := openCodeZenAPIModelID(resolvedModel)
	hasImage := openCodeZenMessagesContainImage(messages)
	logOpenCodeZenRequestSummary(apiModel, true, hasImage, maxTokens, messages)

	reqBody := map[string]interface{}{
		"model":       apiModel,
		"max_tokens":  maxTokens,
		"temperature": temperature,
		"messages":    messages,
		"stream":      true,
	}
	if prettyBody, err := json.MarshalIndent(reqBody, "", "  "); err == nil {
		fmt.Printf("[OpenCode Zen DEBUG] request body stream=true >>>\n%s\n<<< END OpenCode Zen request body\n", string(prettyBody))
	}

	jsonBytes, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", openCodeZenChatCompletionsURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}
	setOpenCodeZenRequestHeaders(req, apiKey, "text/event-stream, application/json")

	resp, err := openCodeZenHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyText := strings.TrimSpace(string(body))
		fmt.Printf("[OpenCode Zen ERROR] model=%s stream=true status=%d hasImage=%v body=%s\n", apiModel, resp.StatusCode, hasImage, truncateForLog(bodyText))
		return openCodeZenStatusError(resp.StatusCode, apiModel, bodyText)
	}

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	fmt.Printf("[OpenCode Zen] stream response model=%s hasImage=%v contentType=%q\n", apiModel, hasImage, contentType)
	if !strings.Contains(contentType, "text/event-stream") {
		body, _ := io.ReadAll(resp.Body)
		bodyText := strings.TrimSpace(string(body))
		fmt.Printf("[OpenCode Zen] stream fallback model=%s hasImage=%v contentType=%q body=%s\n", apiModel, hasImage, contentType, truncateForLog(bodyText))
		var completion struct {
			Error   interface{} `json:"error"`
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
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &completion); err != nil {
			return fmt.Errorf("OpenCode Zen stream fallback parse error: %w", err)
		}
		if len(completion.Choices) == 0 {
			if completion.Error != nil {
				raw, _ := json.Marshal(map[string]interface{}{"error": completion.Error})
				return openCodeZenStatusError(http.StatusBadGateway, apiModel, string(raw))
			}
			return fmt.Errorf("OpenCode Zen stream fallback returned no choices")
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
			return fmt.Errorf("OpenCode Zen stream fallback returned empty content")
		}
		if err := emitFireworksStreamChunk(w, content); err != nil {
			return err
		}

		finishReason := ""
		if len(completion.Choices) > 0 {
			finishReason = normalizeFinishReason(completion.Choices[0].FinishReason)
		}
		if strings.EqualFold(finishReason, "length") {
			fmt.Printf("[OpenCode Zen WARN] stream fallback reached max_tokens=%d model=%s\n", maxTokens, apiModel)
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
						fmt.Printf("[OpenCode Zen WARN] stream chunk parse failed model=%s hasImage=%v err=%v raw=%s\n", apiModel, hasImage, unmarshalErr, truncateForLog(dataStr))
					}
					continue
				}

				if chunk.Usage.PromptTokens > 0 {
					promptTokens = chunk.Usage.PromptTokens
					completionTokens = chunk.Usage.CompletionTokens
				}

				if len(chunk.Choices) == 0 {
					continue
				}

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

		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	finalContent := responseBuilder.String()
	if enforceNoEmbeddedImageData {
		finalContent = normalizeFireworksDrawToCodeOutput(finalContent)
		if normalizedEmitted < len(finalContent) {
			if err := emitFireworksStreamChunk(w, finalContent[normalizedEmitted:]); err != nil {
				return err
			}
			normalizedEmitted = len(finalContent)
		}
		responseBuilder.Reset()
		responseBuilder.WriteString(finalContent)
	}

	if promptTokens == 0 && completionTokens == 0 {
		promptTokens = estimateTokenCountForMessages(messages)
		completionTokens = estimateTokenCount(responseBuilder.String())
	}
	if strings.TrimSpace(responseBuilder.String()) == "" {
		return fmt.Errorf("OpenCode Zen stream ended without content (model=%s, hasImage=%v, events=%d)", apiModel, hasImage, dataEvents)
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
		fmt.Printf("[OpenCode Zen] stream finish_reason=%s model=%s\n", seenFinishReason, apiModel)
	}
	if finishReasonLength {
		fmt.Printf("[OpenCode Zen WARN] stream reached max_tokens=%d model=%s\n", maxTokens, apiModel)
	}

	fmt.Printf("[OpenCode Zen] stream complete model=%s hasImage=%v events=%d parseErrors=%d chars=%d\n",
		apiModel, hasImage, dataEvents, parseErrors, responseBuilder.Len())
	return emitFireworksStreamDone(w, promptTokens, completionTokens)
}

func openCodeZenMessagesContainImage(messages []map[string]interface{}) bool {
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

func openCodeZenModelSupportsImageInput(modelID string) bool {
	resolved := strings.ToLower(strings.TrimSpace(normalizeOpenCodeZenModelID(modelID)))
	switch resolved {
	case "kimi-k2.5", "kimi-k2.5-free":
		return true
	default:
		return false
	}
}

func openCodeZenAttachmentDataURI(attachmentBase64, attachmentMime string) string {
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

func buildOpenCodeZenDrawToCodeMessages(imageBase64, userPrompt, template, imageSource, openCodeZenModel, apiKey string) []map[string]interface{} {
	effectiveImageSource := imageSource
	if !isGlowbomImagesSource(effectiveImageSource) {
		effectiveImageSource = "Glowbom Images"
		fmt.Printf("[OpenCode Zen DEBUG] forcing imageSource to Glowbom Images for safe placeholder output (was=%q)\n", imageSource)
	}
	resolvedModel := normalizeOpenCodeZenModelID(openCodeZenModel)

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
		if openCodeZenModelSupportsImageInput(resolvedModel) {
			useNativeImageInput = true
			fmt.Printf("[OpenCode Zen DEBUG] draw received image payload (base64_len=%d), using native image_url input for model=%s\n", len(imageBase64), resolvedModel)
		} else {
			desc, err := describeImageWithOpenCodeZenKimi(imageBase64, apiKey)
			if err != nil {
				// Keep fallback so draw-to-code doesn't hard-fail if Zen vision is temporarily unavailable.
				fmt.Printf("[OpenCode Zen WARN] Kimi image description unavailable; falling back to Ollama llama3.2-vision: %v\n", err)
				desc, err = getImageDescription(imageBase64)
				if err != nil {
					fmt.Printf("[OpenCode Zen WARN] image description unavailable; proceeding text-only: %v\n", err)
				} else if trimmed := strings.TrimSpace(desc); trimmed != "" {
					usedImageDescription = true
					imageDescriptionSource = "ollama-llama3.2-vision"
					fmt.Printf("[OpenCode Zen DEBUG] draw image description (%s) >>>\n%s\n<<< END image description\n", imageDescriptionSource, trimmed)
					lines = append([]string{fmt.Sprintf("Image description: %s", trimmed)}, lines...)
				}
			} else if trimmed := strings.TrimSpace(desc); trimmed != "" {
				usedImageDescription = true
				imageDescriptionSource = "opencode-zen-kimi-k2.5"
				fmt.Printf("[OpenCode Zen DEBUG] draw image description (%s) >>>\n%s\n<<< END image description\n", imageDescriptionSource, trimmed)
				lines = append([]string{fmt.Sprintf("Image description: %s", trimmed)}, lines...)
			}
		}
	}

	userContent := strings.Join(lines, "\n")
	fmt.Printf("[OpenCode Zen DEBUG] draw prompt usesImageDescription=%v imageDescriptionSource=%s nativeImageInput=%v\n", usedImageDescription, imageDescriptionSource, useNativeImageInput)
	fmt.Printf("[OpenCode Zen DEBUG] draw system prompt >>>\n%s\n<<< END system prompt\n", systemPrompt)
	fmt.Printf("[OpenCode Zen DEBUG] draw user prompt >>>\n%s\n<<< END user prompt\n", userContent)

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
					"url": openCodeZenAttachmentDataURI(imageBase64, "image/jpeg"),
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

func describeImageWithOpenCodeZenKimi(imageBase64, apiKey string) (string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return "", fmt.Errorf("missing OpenCode Zen API key")
	}
	if strings.TrimSpace(imageBase64) == "" {
		return "", fmt.Errorf("missing image payload")
	}

	const visionModel = "kimi-k2.5-free"
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
							"url": openCodeZenAttachmentDataURI(imageBase64, "image/jpeg"),
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

	req, err := http.NewRequest("POST", openCodeZenChatCompletionsURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return "", err
	}
	setOpenCodeZenRequestHeaders(req, apiKey, "application/json")

	resp, err := openCodeZenHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("OpenCode Zen Kimi vision API error (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content interface{} `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return "", fmt.Errorf("failed to parse OpenCode Zen Kimi vision response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return "", fmt.Errorf("OpenCode Zen Kimi vision returned no choices")
	}

	description := strings.TrimSpace(fireworkContentToText(completion.Choices[0].Message.Content))
	description = fireworksThinkTagRegex.ReplaceAllString(description, "")
	description = strings.TrimSpace(description)
	if description == "" {
		return "", fmt.Errorf("OpenCode Zen Kimi vision returned empty description")
	}
	return description, nil
}

func logOpenCodeZenRequestSummary(model string, stream bool, hasImage bool, maxTokens int, messages []map[string]interface{}) {
	preview := truncateForLog(fireworksLatestUserPreview(messages))
	fmt.Printf("[OpenCode Zen] request model=%s stream=%v hasImage=%v max_tokens=%d user=%q\n", model, stream, hasImage, maxTokens, preview)
}
