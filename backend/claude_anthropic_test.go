package main

import "testing"

func TestBuildClaudeRequestBodyUsesAdaptiveThinkingForOpus47(t *testing.T) {
	reqBody := buildClaudeRequestBody(
		[]map[string]interface{}{{"role": "user", "content": "hello"}},
		"system",
		"claude-opus-4-7",
		8192,
		2048,
		true,
		nil,
	)

	thinking, ok := reqBody["thinking"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected thinking config map, got %T", reqBody["thinking"])
	}

	if thinking["type"] != "adaptive" {
		t.Fatalf("expected adaptive thinking for Opus 4.7, got %#v", thinking["type"])
	}

	if thinking["display"] != "summarized" {
		t.Fatalf("expected summarized thinking display for Opus 4.7, got %#v", thinking["display"])
	}

	if _, exists := thinking["budget_tokens"]; exists {
		t.Fatalf("did not expect budget_tokens for Opus 4.7 adaptive thinking")
	}

	outputConfig, ok := reqBody["output_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected output_config map, got %T", reqBody["output_config"])
	}

	if outputConfig["effort"] != "high" {
		t.Fatalf("expected high effort for Opus 4.7, got %#v", outputConfig["effort"])
	}

	if reqBody["stream"] != true {
		t.Fatalf("expected stream=true in request body, got %#v", reqBody["stream"])
	}
}

func TestBuildClaudeRequestBodyKeepsLegacyThinkingForOlderModels(t *testing.T) {
	reqBody := buildClaudeRequestBody(
		[]map[string]interface{}{{"role": "user", "content": "hello"}},
		"system",
		"claude-3-7-sonnet-20250219",
		8192,
		2048,
		false,
		nil,
	)

	thinking, ok := reqBody["thinking"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected thinking config map, got %T", reqBody["thinking"])
	}

	if thinking["type"] != "enabled" {
		t.Fatalf("expected manual thinking for legacy model, got %#v", thinking["type"])
	}

	if thinking["budget_tokens"] != 2048 {
		t.Fatalf("expected legacy budget_tokens to be preserved, got %#v", thinking["budget_tokens"])
	}

	if _, exists := reqBody["output_config"]; exists {
		t.Fatalf("did not expect output_config for legacy model")
	}

	if _, exists := reqBody["stream"]; exists {
		t.Fatalf("did not expect stream flag when stream=false")
	}
}
