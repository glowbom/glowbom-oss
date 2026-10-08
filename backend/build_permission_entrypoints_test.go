package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildPermissionDesktopEntryPointCarriesModeAndDropsTerminalGrant(t *testing.T) {
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	project := policyProjectFixture(t)
	for _, driver := range []string{"opencode", "codex", "acp", "claude-code", "cursor"} {
		for _, mode := range []string{"all", ""} {
			body, _ := json.Marshal(OpenCodeAgentRequest{ProjectPath: project, AgentDriver: driver, PermissionMode: mode})
			w := httptest.NewRecorder()
			m.guardBuild(func(w http.ResponseWriter, r *http.Request) {
				var request OpenCodeAgentRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.PermissionMode != mode || buildPermissionPolicyFromContext(r.Context()) == nil {
					t.Error("Desktop dropped the reviewed mode or managed policy")
				}
				if enabled := buildPermissionAllEnabled(r.Context(), driver, project, "fixture-session"); enabled != (mode == "all") {
					t.Error("Desktop inherited another build's grant", driver, mode, enabled)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"done\":true,\"success\":true}\n\n")
			})(w, httptest.NewRequest(http.MethodPost, "/opencode/refine", strings.NewReader(string(body))))
			if w.Code != http.StatusOK {
				t.Fatalf("Desktop start: %d %s", w.Code, w.Body.String())
			}
			job := m.run(w.Header().Get("X-Glowbom-Job-ID"))
			if job == nil || job.snapshot(true)["permissionMode"] != "ask" {
				t.Fatal("completed Desktop build retained a grant")
			}
		}
	}
}

func TestBuildPermissionPhoneEntryPointCarriesModeWithoutInheritance(t *testing.T) {
	observed := make(chan OpenCodeAgentRequest, 1)
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/models":
			io.WriteString(w, `{"models":[{"id":"connected/build","build":true},{"id":"codex/fixture","build":true},{"id":"claude-code/fixture","build":true},{"id":"cursor/fixture","build":true},{"id":"acp/fixture","build":true}]}`)
		case "/opencode/refine":
			var request OpenCodeAgentRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if enabled := buildPermissionAllEnabled(r.Context(), request.AgentDriver, request.ProjectPath, "fixture-session"); enabled != (request.PermissionMode == "all") {
				t.Error("phone build lost its policy or inherited a previous grant")
			}
			observed <- request
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"done\":true,\"success\":true}\n\n")
		default:
			t.Error("unmocked route", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	s.manager, m.session = m, s
	project := sharedCompanionProject(t, s)
	for _, model := range []string{"connected/build", "codex/fixture", "claude-code/fixture", "cursor/fixture", "acp/fixture"} {
		for _, mode := range []string{"all", ""} {
			body, _ := json.Marshal(map[string]string{"instructions": "Fixture build", "model": model, "permissionMode": mode})
			w := httptest.NewRecorder()
			s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+project.ID+"/build", string(body)))
			if w.Code != http.StatusAccepted {
				t.Fatalf("phone start: %d %s", w.Code, w.Body.String())
			}
			var accepted struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
				t.Fatal(err)
			}
			select {
			case request := <-observed:
				if request.PermissionMode != mode || request.MediaGenerationPolicy != "skip" {
					t.Fatal("phone changed the mode or media policy", request.PermissionMode, request.MediaGenerationPolicy)
				}
			case <-time.After(time.Second):
				t.Fatal("mock worker did not receive the phone build")
			}
			awaitRegistryStatus(t, m, accepted.ID, "completed")
			if m.run(accepted.ID).snapshot(false)["permissionMode"] != "ask" {
				t.Fatal("completed phone build retained a grant")
			}
		}
	}
}

func TestBuildPermissionEntryPointsRejectUnknownModeBeforeWork(t *testing.T) {
	api := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid mode reached a worker or catalog") })
	m := newCompanionManager(api)
	t.Cleanup(m.Shutdown)
	s := testCompanion(t, api)
	project := sharedCompanionProject(t, s)
	for _, mode := range []string{"always", "ALL", " all ", "bypassPermissions"} {
		for _, phone := range []bool{true, false} {
			payload := map[string]any{"instructions": "Fixture", "model": "connected/build", "permissionMode": mode}
			if phone {
				payload["attachmentIds"] = []string{"missing"}
			} else {
				payload["projectPath"] = project.path
			}
			body, _ := json.Marshal(payload)
			w := httptest.NewRecorder()
			if phone {
				s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+project.ID+"/build", string(body)))
			} else {
				m.guardBuild(api.ServeHTTP)(w, httptest.NewRequest(http.MethodPost, "/opencode/refine", strings.NewReader(string(body))))
			}
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "permission mode") {
				t.Fatalf("invalid mode passed early review: %d %s", w.Code, w.Body.String())
			}
		}
	}
}
