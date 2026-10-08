package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in check uses the owner's Claude Code account in a temporary folder.
func TestClaudeCodeLiveSmoke(t *testing.T) {
	if os.Getenv("GLOWBOM_CLAUDE_CODE_SMOKE") != "1" {
		t.Skip("set GLOWBOM_CLAUDE_CODE_SMOKE=1 to test a signed-in CLI")
	}
	binary, err := claudeCodeExecutable()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	session := ""
	emit := func(event map[string]interface{}) {
		if output, ok := event["output"].(string); ok && strings.HasPrefix(output, "Session created: ") {
			session = strings.TrimPrefix(output, "Session created: ")
		}
	}
	prompt := "This is a tiny integration test in a disposable directory. Create smoke.txt containing exactly glowbom-smoke followed by a newline, then run wc -c smoke.txt to verify its size. Do not access other directories, use Git, install anything, or do any other task. Report the verified byte count."
	if _, err := streamClaudeCode(ctx, binary, project, claudeCodeArguments("sonnet", ""), prompt, emit); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(project, "smoke.txt"))
	if err != nil || string(data) != "glowbom-smoke\n" || !strings.HasPrefix(session, claudeCodeSessionPrefix) {
		t.Fatal("live build did not create the expected file and resumable session", err)
	}
	firstSession := session
	prompt = "Continue the integration test. Append exactly resumed followed by a newline to smoke.txt. Run wc -c smoke.txt to verify its new size. Do not access other directories, use Git, install anything, or do any other task. Report the verified byte count."
	if _, err := streamClaudeCode(ctx, binary, project, claudeCodeArguments("sonnet", session), prompt, emit); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(project, "smoke.txt"))
	if err != nil || string(data) != "glowbom-smoke\nresumed\n" || session != firstSession {
		t.Fatal("live build did not resume the same session and update the file", err)
	}
}
