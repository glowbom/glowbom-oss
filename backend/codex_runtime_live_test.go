package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in: this test uses the local Codex account for a small chat turn.
func TestCodexRuntimeLiveChat(t *testing.T) {
	if os.Getenv("GLOWBOM_CODEX_LIVE_TEST") != "1" {
		t.Skip("set GLOWBOM_CODEX_LIVE_TEST=1 to check a local Codex account")
	}
	previous := glowbomCodex
	glowbomCodex = &codexRuntime{}
	t.Cleanup(func() { glowbomCodex.close(); glowbomCodex = previous })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	models, err := codexChatModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 {
		t.Fatal("Codex has no connected models; sign in through Tools first")
	}
	model := strings.TrimPrefix(models[0].ID, "codex/")
	t.Logf("Using %s with %s reasoning effort", model, models[0].DefaultReasoningEffort)
	text := ""
	_, err = runCodexTurn(ctx, codexRunOptions{
		Directory: t.TempDir(), Model: model, ChatOnly: true,
		ReasoningEffort: models[0].DefaultReasoningEffort,
		Instructions:    "Reply to the user's message with plain text. Do not use tools.",
		Input:           []map[string]any{{"type": "text", "text": "Reply with exactly: Glowbom Codex is ready."}},
	}, func(message codexRPCMessage) error {
		var stream codexTextStream
		if message.Method == "item/completed" {
			value, _, err := stream.accept(message)
			if err != nil {
				return err
			}
			text += value
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Glowbom Codex is ready.") {
		t.Fatal("Codex did not return the requested readiness response")
	}
	if err := glowbomCodex.restart(ctx); err != nil {
		t.Fatal(err)
	}
	status := readCodexStatus(ctx)
	if !status.Running || !status.Connected || status.Busy {
		t.Fatalf("Codex did not recover after restart: running=%t connected=%t busy=%t", status.Running, status.Connected, status.Busy)
	}
}

func TestCodexRuntimeLiveBuild(t *testing.T) {
	if os.Getenv("GLOWBOM_CODEX_LIVE_TEST") != "1" {
		t.Skip("set GLOWBOM_CODEX_LIVE_TEST=1 to check a local Codex account")
	}
	previous := glowbomCodex
	glowbomCodex = &codexRuntime{}
	t.Cleanup(func() { glowbomCodex.close(); glowbomCodex = previous })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	models, err := codexChatModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 {
		t.Fatal("Codex has no connected models")
	}
	t.Logf("Using %s with %s reasoning effort", models[0].ID, models[0].DefaultReasoningEffort)
	directory := t.TempDir()
	options := codexRunOptions{
		Directory: directory, Model: strings.TrimPrefix(models[0].ID, "codex/"),
		ReasoningEffort: models[0].DefaultReasoningEffort,
		Instructions:    "Make only the requested text file change in the selected directory. Do not run Git commands, install dependencies, use the network, or read files outside this directory.",
		Input:           []map[string]any{{"type": "text", "text": "Create hello.txt with exactly this content: hello from Glowbom\n"}},
	}
	var stream codexTextStream
	response := ""
	thread, err := runCodexTurn(ctx, options, func(message codexRPCMessage) error {
		if text, _, acceptErr := stream.accept(message); acceptErr != nil {
			return acceptErr
		} else if text != "" {
			response = text
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "hello.txt"))
	if err != nil || !strings.Contains(string(data), "hello from Glowbom") {
		t.Fatalf("Codex did not create the requested test file: %s", response)
	}
	if err := glowbomCodex.restart(ctx); err != nil {
		t.Fatal(err)
	}
	options.ThreadID = thread
	options.Input = []map[string]any{{"type": "text", "text": "Append one line to hello.txt: resumed after restart\n"}}
	if _, err := runCodexTurn(ctx, options, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(directory, "hello.txt"))
	if err != nil || !strings.Contains(string(data), "hello from Glowbom") || !strings.Contains(string(data), "resumed after restart") {
		t.Fatal("Codex did not resume the test build after restart")
	}
}
