package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func acpApprovalMessage(session, title, kind, option string) acpMessage {
	params, _ := json.Marshal(map[string]any{
		"sessionId": session, "toolCall": map[string]any{"title": title, "kind": kind},
		"options": []map[string]string{
			{"kind": "allow_once", "optionId": option},
			{"kind": "allow_always", "optionId": "persistent"},
			{"kind": "reject_once", "optionId": "reject"},
		},
	})
	return acpMessage{Method: "session/request_permission", Params: params}
}

func acpCheckApproval(t *testing.T, ctx context.Context, profile acpProfile, project string, message acpMessage, approvals *acpBuildApprovals, decision, expected string) map[string]any {
	t.Helper()
	var shown map[string]any
	result, err := waitForACPPermission(ctx, profile, project, message, func(event map[string]interface{}) {
		permission, ok := event["permission"].(map[string]any)
		if !ok {
			return
		}
		shown = permission
		if decision == "" {
			t.Fatal("an already approved tool asked again")
		}
		if err := respondToACPPermission(project, permission["sessionID"].(string), permission["id"].(string), decision); err != nil {
			t.Fatal(err)
		}
	}, approvals)
	if err != nil {
		t.Fatal(err)
	}
	if decision != "" && shown == nil {
		t.Fatal("a different scope was approved without asking")
	}
	encoded, _ := json.Marshal(result)
	if string(encoded) != `{"outcome":{"optionId":"`+expected+`","outcome":"selected"}}` {
		t.Fatalf("unexpected agent decision: %s", encoded)
	}
	return shown
}

func TestACPBuildApprovalRemembersOnlyTheNamedToolAndCurrentScope(t *testing.T) {
	project, profile := acpBuildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	approvals := newACPBuildApprovals(ctx)
	defer approvals.close()
	first := acpApprovalMessage("session", "read_files: first.html", "read", "first")
	shown := acpCheckApproval(t, ctx, profile, project, first, approvals, "build", "first")
	if shown["buildApprovalTool"] != "read_files" {
		t.Fatal("the remembered tool was not explained")
	}
	acpCheckApproval(t, ctx, profile, project, acpApprovalMessage("session", "read_files: second.html", "read", "new-once-id"), approvals, "", "new-once-id")
	for _, message := range []acpMessage{
		acpApprovalMessage("session", "list_files: folder", "read", "one"),
		acpApprovalMessage("session", "read_files: command", "execute", "one"),
		acpApprovalMessage("different-session", "read_files: first.html", "read", "one"),
	} {
		acpCheckApproval(t, ctx, profile, project, message, approvals, "once", "one")
	}
	otherProject, err := codexInputProject(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	acpCheckApproval(t, ctx, profile, otherProject, first, approvals, "once", "first")
	otherProfile := profile
	otherProfile.Command = "another-agent"
	acpCheckApproval(t, ctx, otherProfile, project, first, approvals, "once", "first")
	// One-time approval for another tool never creates a remembered grant.
	acpCheckApproval(t, ctx, profile, project, acpApprovalMessage("session", "list_files: folder", "read", "one"), approvals, "once", "one")
}

func TestACPBuildApprovalNeedsAnIdentifiableToolAndCurrentOneTimeChoice(t *testing.T) {
	project, profile := acpBuildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	approvals := newACPBuildApprovals(ctx)
	defer approvals.close()
	acpCheckApproval(t, ctx, profile, project, acpApprovalMessage("session", "read_files", "read", "first"), approvals, "build", "first")
	for _, raw := range []string{
		`{"sessionId":"session","toolCall":{"title":"Read the next file","kind":"read"},"options":[{"kind":"allow_once","optionId":"one"}]}`,
		`{"sessionId":"session","toolCall":{"name":"invalid name","title":"read_files","kind":"read"},"options":[{"kind":"allow_once","optionId":"one"}]}`,
	} {
		shown := acpCheckApproval(t, ctx, profile, project, acpMessage{Method: "session/request_permission", Params: json.RawMessage(raw)}, approvals, "once", "one")
		if _, offered := shown["buildApprovalTool"]; offered {
			t.Fatal("offered repeated approval without an identifiable tool")
		}
		for _, choice := range shown["availableResponses"].([]string) {
			if choice == "build" {
				t.Fatal("offered build approval without an identifiable tool")
			}
		}
	}
	message := acpMessage{Method: "session/request_permission", Params: json.RawMessage(`{"sessionId":"session","toolCall":{"title":"read_files","kind":"read"},"options":[{"kind":"allow_always","optionId":"persistent"},{"kind":"reject_once","optionId":"reject"}]}`)}
	shown := acpCheckApproval(t, ctx, profile, project, message, approvals, "reject", "reject")
	if _, offered := shown["buildApprovalTool"]; offered {
		t.Fatal("offered a build grant that requires persistent agent approval")
	}
	// An explicit protocol name takes precedence over a different title.
	message.Params = json.RawMessage(`{"sessionId":"session","toolCall":{"name":"write_files","title":"read_files: misleading","kind":"read"},"options":[{"kind":"allow_once","optionId":"write"}]}`)
	shown = acpCheckApproval(t, ctx, profile, project, message, approvals, "once", "write")
	if shown["buildApprovalTool"] != "write_files" {
		t.Fatal("ignored the explicit protocol tool name")
	}
}

func TestACPBuildApprovalExpiresWhenBuildStops(t *testing.T) {
	project, profile := acpBuildFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	approvals := newACPBuildApprovals(ctx)
	message := acpApprovalMessage("session", "read_files: page.html", "read", "one")
	acpCheckApproval(t, ctx, profile, project, message, approvals, "build", "one")
	cancel()
	result, err := waitForACPPermission(ctx, profile, project, message, func(map[string]interface{}) { t.Fatal("stopped build asked for permission") }, approvals)
	if err != context.Canceled || result != nil {
		t.Fatal("stopped build reused a remembered approval")
	}
	approvals.close()
	nextCtx, nextCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer nextCancel()
	shown := acpCheckApproval(t, nextCtx, profile, project, message, approvals, "once", "one")
	if _, offered := shown["buildApprovalTool"]; offered {
		t.Fatal("closed approval state offered a new grant")
	}
}

type acpApprovalWriter struct {
	*httptest.ResponseRecorder
	onPermission func(map[string]any)
}

func (w *acpApprovalWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	for _, line := range strings.Split(string(data), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil {
			if permission, ok := event["permission"].(map[string]any); ok {
				w.onPermission(permission)
			}
		}
	}
	return n, err
}

func TestACPBuildApprovalIsNotReusedWhenConversationContinues(t *testing.T) {
	project, profile := acpBuildFixture(t)
	original := acpRunTurn
	t.Cleanup(func() { acpRunTurn = original })
	acpRunTurn = func(ctx context.Context, _ acpProfile, _, _ string, _ []map[string]any, emit func(acpMessage) error, request func(context.Context, acpMessage) (any, error)) (string, error) {
		for _, title := range []string{"read_files: first.html", "read_files: second.html"} {
			result, err := request(ctx, acpApprovalMessage("same-session", title, "read", "once"))
			data, _ := json.Marshal(result)
			if err != nil || string(data) != `{"outcome":{"optionId":"once","outcome":"selected"}}` {
				t.Fatal("build failed to authorize the current one-time option", err, string(data))
			}
		}
		return "same-session", emit(acpMessage{Method: "session/update", Params: json.RawMessage(`{"sessionId":"same-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Reviewed both project files."}}}`)})
	}
	for build := 0; build < 2; build++ {
		asked := 0
		writer := &acpApprovalWriter{ResponseRecorder: httptest.NewRecorder(), onPermission: func(permission map[string]any) {
			asked++
			if err := respondToACPPermission(project, permission["sessionID"].(string), permission["id"].(string), "build"); err != nil {
				t.Fatal(err)
			}
		}}
		request := httptest.NewRequest(http.MethodPost, "/opencode/refine", nil)
		ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
		status, _ := runACPRefine(writer, request.WithContext(ctx), OpenCodeAgentRequest{ProjectPath: project, SessionID: acpSavedSession(profile, project, "same-session")}, profile, "Build", nil, nil)
		cancel()
		if status != "completed" || asked != 1 {
			t.Fatalf("build %d status %s asked %d times; expected once per build", build, status, asked)
		}
	}
}
