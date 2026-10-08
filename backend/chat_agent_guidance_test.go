package main

import (
	"strings"
	"testing"
)

func TestChatAgentGuidanceMatchesBuildState(t *testing.T) {
	idle := chatAgentGuidance(&chatAgentState{Status: "idle"}, true)
	if !strings.Contains(idle, "no Build is running") || !strings.Contains(idle, "Build icon") || strings.Contains(idle, "Steer icon") {
		t.Fatalf("idle guidance misstates controls: %q", idle)
	}
	running := chatAgentGuidance(&chatAgentState{Status: "running", Driver: "opencode", SteerAvailable: true}, true)
	if !strings.Contains(running, "check saved project chat between major steps") || !strings.Contains(running, "Steer icon") || !strings.Contains(running, "do not promise") {
		t.Fatalf("running guidance misstates the Build: %q", running)
	}
	cursor := chatAgentGuidance(&chatAgentState{Status: "running", Driver: "cursor"}, true)
	if strings.Contains(cursor, "Steer icon") || strings.Contains(cursor, "check saved project chat") {
		t.Fatalf("Cursor guidance promised unsupported behavior: %q", cursor)
	}
	missing := chatAgentGuidance(nil, false)
	if !strings.Contains(missing, "project folder is needed") || strings.Contains(missing, "Build icon") {
		t.Fatalf("missing-project guidance promised Build: %q", missing)
	}
}

func TestLocalChatModelsReceiveAgentGuidance(t *testing.T) {
	guidance := chatAgentGuidance(&chatAgentState{Status: "running", Driver: "opencode", SteerAvailable: true}, true)
	messages := []chatMessage{{Role: "user", Text: "Should we add another ship?"}}
	apple, err := appleIntelligenceConversation(messages, appleIntelligenceInputBudgetBytes, guidance)
	if err != nil || !strings.Contains(apple[0].Content, "Steer icon") {
		t.Fatalf("Apple Intelligence guidance missing: %v", err)
	}
	local, err := localChatConversation(messages, "", guidance)
	if err != nil || !strings.Contains(local[0].Content, "Steer icon") {
		t.Fatalf("local MiMo guidance missing: %v", err)
	}
}
