package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func acpBuildFixture(t *testing.T) (string, acpProfile) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	acpSettingsFixture(t)
	project := t.TempDir()
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "glowbom.json"), []byte(`{"name":"ACP fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	profile := acpProfile{ID: "acp-1", Name: "Custom agent", Command: "custom-agent", Args: []string{"acp"}}
	if err := saveACPProfiles([]acpProfile{profile}); err != nil {
		t.Fatal(err)
	}
	return project, profile
}

func TestACPSessionsStayWithConnectionConfigurationAndProject(t *testing.T) {
	project, profile := acpBuildFixture(t)
	saved := acpSavedSession(profile, project, "agent/session:123")
	if !validSavedACPSession(saved) || acpResumeSession(profile, project, saved) != "agent/session:123" {
		t.Fatal("valid session cannot resume")
	}
	changed := profile
	changed.Command = "different-agent"
	other := profile
	other.ID = "acp-2"
	for _, session := range []string{"codex:123", "ses_123", "acp:invalid", saved + "\n"} {
		if acpResumeSession(profile, project, session) != "" {
			t.Fatal("foreign session accepted")
		}
	}
	if acpResumeSession(changed, project, saved) != "" || acpResumeSession(other, project, saved) != "" || acpResumeSession(profile, t.TempDir(), saved) != "" {
		t.Fatal("session crossed a connection or project boundary")
	}
}

func TestACPBuildPreservesHistoryAndSnapshotsConnection(t *testing.T) {
	project, profile := acpBuildFixture(t)
	original := acpRunTurn
	t.Cleanup(func() { acpRunTurn = original })
	called := false
	acpRunTurn = func(ctx context.Context, connection acpProfile, directory, session string, prompt []map[string]any, emit func(acpMessage) error, request func(context.Context, acpMessage) (any, error)) (string, error) {
		called = true
		if connection.Command != profile.Command || connection.ID != profile.ID || directory != project || session != "" || len(prompt) != 1 || !strings.Contains(prompt[0]["text"].(string), "Update the page") {
			t.Fatal("ACP build lost its selected project or instructions")
		}
		changed := profile
		changed.Name, changed.Command = "Replacement", "replacement-agent"
		if err := saveACPProfiles([]acpProfile{changed}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "changed.txt"), []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := emit(acpMessage{Method: "glowbom/session", Params: json.RawMessage(`{"sessionId":"session-123"}`)}); err != nil {
			return "", err
		}
		return "session-123", emit(acpMessage{Method: "session/update", Params: json.RawMessage(`{"sessionId":"session-123","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Updated the page"}}}`)})
	}
	body, _ := json.Marshal(OpenCodeAgentRequest{AgentDriver: "acp", Model: "acp/acp-1", ProjectPath: project, SessionID: "codex:foreign", Instructions: "Update the page", PersistCurrentInstructionsToHistory: true})
	w := httptest.NewRecorder()
	openCodeRefineHandler(w, acpSettingsRequest(http.MethodPost, string(body)))
	if !called || w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"success":true`) || !strings.Contains(w.Body.String(), acpSavedSession(profile, project, "session-123")) || !strings.Contains(w.Body.String(), "changed.txt") {
		t.Fatal("ACP did not complete through the existing build stream", w.Code, w.Body.String())
	}
	entries, err := loadProjectHistoryEntries(project)
	if err != nil || len(entries) != 1 || entries[0].Contributor != profile.Name || entries[0].Model != "acp/acp-1" || entries[0].Provider != "acp" || entries[0].OutputSummary != "Updated the page" {
		t.Fatalf("ACP history changed with settings: %+v %v", entries, err)
	}
}

func TestACPBuildRejectsMissingConnectionAndWrongDriver(t *testing.T) {
	project, _ := acpBuildFixture(t)
	original := acpRunTurn
	t.Cleanup(func() { acpRunTurn = original })
	acpRunTurn = func(context.Context, acpProfile, string, string, []map[string]any, func(acpMessage) error, func(context.Context, acpMessage) (any, error)) (string, error) {
		t.Fatal("invalid request started ACP")
		return "", nil
	}
	for _, req := range []OpenCodeAgentRequest{
		{AgentDriver: "opencode", Model: "acp/acp-1", ProjectPath: project},
		{AgentDriver: "acp", Model: "acp/acp-2", ProjectPath: project},
		{AgentDriver: "acp", Model: "provider/model", ProjectPath: project},
	} {
		body, _ := json.Marshal(req)
		w := httptest.NewRecorder()
		openCodeRefineHandler(w, acpSettingsRequest(http.MethodPost, string(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatal("accepted invalid ACP request", w.Code)
		}
	}
}

func TestACPModelsStayBuildOnlyAndCatalogDoesNotLaunch(t *testing.T) {
	acpBuildFixture(t)
	models := listACPModels()
	if len(models) != 1 || models[0].ID != "acp/acp-1" || !models[0].Build {
		t.Fatal("configured connection missing from catalog", models)
	}
	for _, mode := range []string{"chat", "prototype", "translation"} {
		if err := validateChatRequest(chatRequest{Mode: mode, Model: models[0].ID}); err == nil {
			t.Fatal("ACP entered a non-build workflow", mode)
		}
	}
	if bookModelEligible(models[0].ID) {
		t.Fatal("ACP entered Project Book generation")
	}
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"models": models}) }))
	if options, err := s.models(context.Background()); err != nil || len(options) != 1 || options[0].ID != models[0].ID {
		t.Fatal("saved ACP connection missing from phone build catalog", options, err)
	}
	if options, err := s.connectedChatModels(context.Background()); err != nil || len(options) != 0 {
		t.Fatal("ACP entered the phone chat catalog", options, err)
	}
}

func TestACPPermissionsAreOneTimeAndBoundToProjectAndSession(t *testing.T) {
	project, profile := acpBuildFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	message := acpMessage{Method: "session/request_permission", Params: json.RawMessage(`{"sessionId":"s1","toolCall":{"title":"Write the page","kind":"edit","rawInput":{"path":"index.html"}},"options":[{"optionId":"one","kind":"allow_once"},{"optionId":"forever","kind":"allow_always"},{"optionId":"no","kind":"reject_once"}]}`)}
	shown := make(chan map[string]any, 1)
	done := make(chan any, 1)
	go func() {
		result, err := waitForACPPermission(ctx, profile, project, message, func(event map[string]interface{}) { shown <- event["permission"].(map[string]any) }, nil)
		if err != nil {
			done <- err
			return
		}
		done <- result
	}()
	var permission map[string]any
	select {
	case permission = <-shown:
	case <-time.After(2 * time.Second):
		t.Fatal("permission not shown")
	}
	id, session := permission["id"].(string), permission["sessionID"].(string)
	for _, attempt := range []struct{ project, session, choice string }{{t.TempDir(), session, "once"}, {project, "wrong-session", "once"}, {project, session, "session"}} {
		if respondToACPPermission(attempt.project, attempt.session, id, attempt.choice) == nil {
			t.Fatal("unoffered or cross-project permission accepted")
		}
	}
	if err := respondToACPPermission(project, session, id, "once"); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		data, _ := json.Marshal(result)
		if string(data) != `{"outcome":{"optionId":"one","outcome":"selected"}}` {
			t.Fatal("wrong ACP decision", string(data))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("permission did not resolve")
	}
	if respondToACPPermission(project, session, id, "once") == nil {
		t.Fatal("permission reused")
	}
}

func TestACPSessionRestoresOnlyLocally(t *testing.T) {
	project, profile := acpBuildFixture(t)
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	job := m.newRun(companionProject{ID: companionProjectID(project), path: project}, "desktop", OpenCodeAgentRequest{AgentDriver: "acp", Model: "acp/acp-1"})
	session := acpSavedSession(profile, project, "saved-session")
	job.mu.Lock()
	job.agentSessionID, job.status, job.finishedAt = session, "completed", time.Now().UTC().Format(time.RFC3339Nano)
	job.mu.Unlock()
	m.saveRun(job)
	restored := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(restored.Shutdown)
	restored.loadProjectRuns(project)
	run := restored.run(job.id)
	if run == nil || run.snapshot(true)["sessionID"] != session || run.snapshot(true)["model"] != "acp/acp-1" {
		t.Fatal("ACP session did not restore locally")
	}
	if _, exists := run.snapshot(false)["sessionID"]; exists {
		t.Fatal("session leaked to phone")
	}
}

func TestACPBuildFailureNeverReportsSuccess(t *testing.T) {
	project, profile := acpBuildFixture(t)
	original := acpRunTurn
	t.Cleanup(func() { acpRunTurn = original })
	acpRunTurn = func(context.Context, acpProfile, string, string, []map[string]any, func(acpMessage) error, func(context.Context, acpMessage) (any, error)) (string, error) {
		return "", errors.New("ACP connection failed")
	}
	w := httptest.NewRecorder()
	status, _ := runACPRefine(w, httptest.NewRequest(http.MethodPost, "/opencode/refine", nil), OpenCodeAgentRequest{ProjectPath: project}, profile, "Build", nil, nil)
	if status != "failed" || !strings.Contains(w.Body.String(), `"success":false`) || strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatal(status, w.Body.String())
	}
}
