package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func policyProjectFixture(t *testing.T) string {
	t.Helper()
	project, err := codexInputProject(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveProject(filepath.Join(project, "glowbom.json"), &GlowbomProject{Name: "Policy fixture", Targets: map[string]Target{}}); err != nil {
		t.Fatal(err)
	}
	return project
}

func permissionPolicyFixture(t *testing.T, mode string) (context.Context, *buildPermissionPolicy, string, context.CancelFunc) {
	t.Helper()
	project := policyProjectFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	policy := newBuildPermissionPolicy(ctx, "opencode", project, mode)
	t.Cleanup(policy.close)
	return withBuildPermissionPolicy(ctx, policy), policy, project, cancel
}

func policyEvent(value map[string]any) []byte { data, _ := json.Marshal(value); return data }

func policyPending(choices []string) []byte {
	return policyEvent(map[string]any{"permission": map[string]any{
		"id": "current", "sessionID": "session", "availableResponses": choices,
	}})
}

func TestBuildPermissionPolicyScopeAndSession(t *testing.T) {
	ctx, policy, project, _ := permissionPolicyFixture(t, "ask")
	if buildPermissionAllAvailable(ctx, "opencode", project, "wrong-first-session", false) {
		t.Fatal("all available without once")
	}
	if !buildPermissionAllAvailable(ctx, "opencode", project, "session-one", true) {
		t.Fatal("managed once choice unavailable")
	}
	if buildPermissionAllEnabled(ctx, "opencode", project, "session-one") || policy.mode() != "ask" {
		t.Fatal("ask build auto-enabled")
	}
	for _, scope := range []struct{ driver, project, session string }{
		{"codex", project, "session-one"}, {"opencode", policyProjectFixture(t), "session-one"}, {"opencode", project, "session-two"},
	} {
		if buildPermissionAllAvailable(ctx, scope.driver, scope.project, scope.session, true) {
			t.Fatal("accepted different scope", scope)
		}
	}
	for _, unmanaged := range []context.Context{context.Background(), nil} {
		if buildPermissionAllAvailable(unmanaged, "opencode", project, "session-one", true) {
			t.Fatal("unmanaged policy available")
		}
	}
	if buildPermissionChanged(context.Background()) != nil {
		t.Fatal("unmanaged context has notifications")
	}
}

func TestBuildPermissionPolicyClaimsOnceAndNeverInheritsResumedSession(t *testing.T) {
	ctx, policy, project, cancel := permissionPolicyFixture(t, "all")
	if !buildPermissionAllAvailable(ctx, "opencode", project, "resumed-session", true) {
		t.Fatal("all unavailable")
	}
	var claims atomic.Int32
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if buildPermissionClaim(ctx, "opencode", project, "resumed-session", "permission-one") {
				claims.Add(1)
			}
		}()
	}
	workers.Wait()
	if claims.Load() != 1 || !buildPermissionClaimed(ctx, "opencode", project, "resumed-session", "permission-one") {
		t.Fatal("request was not reserved once")
	}
	if !buildPermissionClaim(ctx, "opencode", project, "resumed-session", "permission-two") {
		t.Fatal("new permission not allowed")
	}
	next := newBuildPermissionPolicy(context.Background(), "opencode", project, "ask")
	defer next.close()
	nextContext := withBuildPermissionPolicy(context.Background(), next)
	if buildPermissionAllEnabled(nextContext, "opencode", project, "resumed-session") {
		t.Fatal("new build inherited an old session grant")
	}
	cancel()
	if policy.mode() != "ask" || buildPermissionClaim(ctx, "opencode", project, "resumed-session", "after-stop") {
		t.Fatal("canceled build still allows actions")
	}
}

func TestBuildPermissionPolicyTransitionWaitsForAcknowledgedCurrentReply(t *testing.T) {
	for _, success := range []bool{true, false} {
		name := "uncertain"
		if success {
			name = "acknowledged"
		}
		t.Run(name, func(t *testing.T) {
			ctx, policy, project, _ := permissionPolicyFixture(t, "ask")
			change, err := beginBuildPermissionAll(ctx, "opencode", project, "session", "current")
			if err != nil {
				t.Fatal(err)
			}
			if policy.mode() != "ask" || !buildPermissionAllEnabled(ctx, "opencode", project, "session") {
				t.Fatal("transition did not expose waiting state correctly")
			}
			changed := buildPermissionChanged(ctx)
			result := make(chan bool, 1)
			go func() { result <- buildPermissionClaim(ctx, "opencode", project, "session", "next") }()
			select {
			case <-result:
				t.Fatal("later action bypassed current acknowledgement")
			case <-time.After(20 * time.Millisecond):
			}
			if !buildPermissionReserve(ctx, "opencode", project, "session", "current-wire-id") {
				t.Fatal("manual wire reservation blocked during transition")
			}
			change.finish(success)
			select {
			case <-changed:
			case <-time.After(time.Second):
				t.Fatal("transition did not wake event stream")
			}
			select {
			case got := <-result:
				if got != success {
					t.Fatalf("next permission = %v, want %v", got, success)
				}
			case <-time.After(time.Second):
				t.Fatal("later permission stayed blocked")
			}
			want := "ask"
			if success {
				want = "all"
			}
			if policy.mode() != want {
				t.Fatal("mode", policy.mode(), "want", want)
			}
			if buildPermissionClaim(ctx, "opencode", project, "session", "current") || buildPermissionClaim(ctx, "opencode", project, "session", "current-wire-id") {
				t.Fatal("current explicit action was replayed")
			}
			change.finish(!success)
			if policy.mode() != want {
				t.Fatal("completed transition changed again")
			}
		})
	}
}

func TestBuildPermissionPolicyCancelUnblocksTransitionWithoutGrant(t *testing.T) {
	ctx, policy, project, cancel := permissionPolicyFixture(t, "ask")
	change, err := beginBuildPermissionAll(ctx, "opencode", project, "session", "current")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan bool, 1)
	go func() { result <- buildPermissionClaim(ctx, "opencode", project, "session", "next") }()
	cancel()
	select {
	case got := <-result:
		if got {
			t.Fatal("canceled transition allowed action")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock claim")
	}
	change.finish(true)
	if policy.mode() != "ask" {
		t.Fatal("late success reopened canceled policy")
	}
}

func TestBuildPermissionPolicySeparatesAttemptedAndConfirmedReplies(t *testing.T) {
	ctx, policy, project, cancel := permissionPolicyFixture(t, "ask")
	if buildPermissionConfirm(ctx, "opencode", project, "session", "unknown") {
		t.Fatal("unknown reply became confirmed")
	}
	if !buildPermissionReserve(ctx, "opencode", project, "session", "uncertain") ||
		!buildPermissionClaimed(ctx, "opencode", project, "session", "uncertain") ||
		buildPermissionConfirmed(ctx, "opencode", project, "session", "uncertain") {
		t.Fatal("attempt was mistaken for an acknowledgement")
	}
	if !buildPermissionReserve(ctx, "opencode", project, "session", "settled") {
		t.Fatal("could not reserve explicit decision")
	}
	changed := buildPermissionChanged(ctx)
	if !buildPermissionConfirm(ctx, "opencode", project, "session", "settled") {
		t.Fatal("acknowledged decision was not settled")
	}
	select {
	case <-changed:
	default:
		t.Fatal("acknowledgement did not wake waiting requests")
	}
	change, err := beginBuildPermissionAll(ctx, "opencode", project, "session", "new-current")
	if err != nil {
		t.Fatal(err)
	}
	change.finish(true)
	if buildPermissionClaim(ctx, "opencode", project, "session", "uncertain") ||
		buildPermissionClaim(ctx, "opencode", project, "session", "settled") {
		t.Fatal("later All retried an already attempted decision")
	}
	if buildPermissionConfirmed(ctx, "opencode", project, "session", "uncertain") ||
		!buildPermissionConfirmed(ctx, "opencode", project, "session", "settled") {
		t.Fatal("All lost the distinction between uncertain and settled replies")
	}
	if buildPermissionConfirm(ctx, "opencode", project, "wrong-session", "uncertain") {
		t.Fatal("different session settled a reply")
	}
	cancel()
	if buildPermissionConfirmed(ctx, "opencode", project, "session", "settled") || policy.mode() != "ask" {
		t.Fatal("cancellation retained authorization")
	}
}

func TestBuildPermissionPolicyIsNotSavedOrRestored(t *testing.T) {
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	project := companionProject{path: policyProjectFixture(t)}
	project.ID = companionProjectID(project.path)
	job := m.newRun(project, "desktop", OpenCodeAgentRequest{ProjectPath: project.path, PermissionMode: "all"})
	if job.snapshot(false)["permissionMode"] != "all" {
		t.Fatal("live snapshot does not expose all")
	}
	path := filepath.Join(project.path, ".glowbom", "runs", job.id, "run.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if json.Unmarshal(data, &saved) != nil {
		t.Fatal("saved run malformed")
	}
	if _, exists := saved["permissionMode"]; exists {
		t.Fatal("authorization mode reached disk")
	}
	saved["permissionMode"], saved["status"] = "all", "completed"
	data, _ = json.Marshal(saved)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	restored := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(restored.Shutdown)
	restored.loadProjectRuns(project.path)
	if value := restored.run(job.id); value == nil || value.snapshot(false)["permissionMode"] != "ask" {
		t.Fatal("restored run inherited authorization")
	}
}

func TestBuildPermissionPolicyClearsAtEveryTerminalEvent(t *testing.T) {
	for _, terminal := range []string{"success", "failure", "cancel"} {
		t.Run(terminal, func(t *testing.T) {
			ctx, policy, project, cancel := permissionPolicyFixture(t, "all")
			job := &companionJob{kind: "build", status: "running", ctx: ctx, cancel: cancel, permissionPolicy: policy}
			if terminal == "cancel" {
				job.cancelJob(time.Now())
			} else {
				job.event(policyEvent(map[string]any{"done": true, "success": terminal == "success"}))
			}
			if job.snapshot(false)["permissionMode"] != "ask" || buildPermissionAllAvailable(ctx, "opencode", project, "session", true) {
				t.Fatal("terminal job kept approval grant")
			}
		})
	}
}

func TestBuildPermissionAllResponseSharesPhoneAndDesktopTransaction(t *testing.T) {
	for _, desktop := range []bool{false, true} {
		for _, accepted := range []bool{false, true} {
			name := "phone"
			if desktop {
				name = "desktop"
			}
			name += "/"
			if accepted {
				name += "acknowledged"
			} else {
				name += "uncertain"
			}
			t.Run(name, func(t *testing.T) {
				m := newCompanionManager(http.NotFoundHandler())
				t.Cleanup(m.Shutdown)
				project := companionProject{path: policyProjectFixture(t)}
				project.ID = companionProjectID(project.path)
				job := m.newRun(project, "desktop", OpenCodeAgentRequest{ProjectPath: project.path})
				job.save = nil
				job.event(policyPending([]string{"once", "all", "reject"}))
				nextAction := make(chan bool, 1)
				var calls atomic.Int32
				api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var response OpenCodePermissionRespondRequest
					if err := json.NewDecoder(r.Body).Decode(&response); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if response.Response != "once" || response.PermissionID != "current" || response.SessionID != "session" {
						t.Error("provider received broad or wrong reply", response)
					}
					if buildPermissionPolicyFromContext(r.Context()) != job.permissionPolicy {
						t.Error("response lost managed policy context")
					}
					go func() { nextAction <- buildPermissionClaim(job.ctx, "opencode", project.path, "session", "next") }()
					select {
					case <-nextAction:
						t.Error("future action allowed before provider acknowledgement")
					case <-time.After(20 * time.Millisecond):
					}
					if !accepted {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					writeJSON(w, map[string]bool{"ok": true})
				})
				s := testCompanion(t, api)
				s.manager = m
				s.projects[project.ID] = project
				w := httptest.NewRecorder()
				if desktop {
					body, _ := json.Marshal(OpenCodePermissionRespondRequest{ProjectPath: project.path, SessionID: "session", PermissionID: "current", Response: "all"})
					m.guardResponse("permission", api.ServeHTTP)(w, httptest.NewRequest(http.MethodPost, "/opencode/permission/respond", strings.NewReader(string(body))))
				} else {
					ok := s.submitResponse(w, httptest.NewRequest(http.MethodPost, "/respond", nil), companionResponseRequest{JobID: job.id, Kind: "permission", ID: "current", Response: "all"}, true)
					if ok != accepted {
						t.Error("response result", ok, "want", accepted)
					}
				}
				if calls.Load() != 1 {
					t.Fatal("current request forwarded more than once", calls.Load())
				}
				select {
				case got := <-nextAction:
					if got != accepted {
						t.Fatal("next action", got, "want", accepted)
					}
				case <-time.After(time.Second):
					t.Fatal("next action did not wake")
				}
				want := "ask"
				if accepted {
					want = "all"
				}
				if job.snapshot(false)["permissionMode"] != want {
					t.Fatal("wrong final permission mode")
				}
				if buildPermissionConfirmed(job.ctx, "opencode", project.path, "session", "current") != accepted {
					t.Fatal("current response settlement did not match acknowledgement")
				}
				if accepted && len(job.pendingPermission) != 0 {
					t.Fatal("acknowledged permission still waiting")
				}
			})
		}
	}
}

func TestBuildPermissionExplicitDecisionsCannotReplayAfterAll(t *testing.T) {
	for _, desktop := range []bool{false, true} {
		for _, accepted := range []bool{false, true} {
			for _, decision := range []string{"once", "reject"} {
				name := "phone/"
				if desktop {
					name = "desktop/"
				}
				if accepted {
					name += "acknowledged/"
				} else {
					name += "uncertain/"
				}
				t.Run(name+decision, func(t *testing.T) {
					m := newCompanionManager(http.NotFoundHandler())
					t.Cleanup(m.Shutdown)
					project := companionProject{path: policyProjectFixture(t)}
					project.ID = companionProjectID(project.path)
					job := m.newRun(project, "desktop", OpenCodeAgentRequest{ProjectPath: project.path})
					job.save = nil
					job.event(policyPending([]string{"once", "all", "reject"}))
					var calls atomic.Int32
					api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if accepted {
							writeJSON(w, map[string]bool{"ok": true})
						} else {
							w.WriteHeader(http.StatusBadGateway)
						}
					})
					s := testCompanion(t, api)
					s.manager = m
					s.projects[project.ID] = project
					w := httptest.NewRecorder()
					if desktop {
						body, _ := json.Marshal(OpenCodePermissionRespondRequest{ProjectPath: project.path, SessionID: "session", PermissionID: "current", Response: decision})
						m.guardResponse("permission", api.ServeHTTP)(w, httptest.NewRequest(http.MethodPost, "/opencode/permission/respond", strings.NewReader(string(body))))
					} else {
						s.submitResponse(w, httptest.NewRequest(http.MethodPost, "/respond", nil), companionResponseRequest{JobID: job.id, Kind: "permission", ID: "current", Response: decision}, true)
					}
					if !buildPermissionClaimed(job.ctx, "opencode", project.path, "session", "current") ||
						buildPermissionConfirmed(job.ctx, "opencode", project.path, "session", "current") != accepted {
						t.Fatal("explicit decision did not retain exact attempt and acknowledgement")
					}
					change, err := beginBuildPermissionAll(job.ctx, "opencode", project.path, "session", "later")
					if err != nil {
						t.Fatal(err)
					}
					change.finish(true)
					if buildPermissionClaim(job.ctx, "opencode", project.path, "session", "current") || calls.Load() != 1 {
						t.Fatal("All replayed a previous explicit decision")
					}
				})
			}
		}
	}
}

func TestBuildPermissionAllRefusesStaleAndUnsupportedChoices(t *testing.T) {
	for _, choices := range [][]string{{"once", "all", "reject"}, {"session", "all", "reject"}} {
		m := newCompanionManager(http.NotFoundHandler())
		project := companionProject{path: policyProjectFixture(t)}
		project.ID = companionProjectID(project.path)
		job := m.newRun(project, "desktop", OpenCodeAgentRequest{ProjectPath: project.path})
		job.save = nil
		job.event(policyPending(choices))
		s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid choice reached provider") }))
		s.manager = m
		s.projects[project.ID] = project
		for _, id := range []string{"old", "current"} {
			if id == "current" && choices[0] == "once" {
				continue
			}
			w := httptest.NewRecorder()
			if s.submitResponse(w, httptest.NewRequest(http.MethodPost, "/respond", nil), companionResponseRequest{JobID: job.id, Kind: "permission", ID: id, Response: "all"}, true) || w.Code < 400 {
				t.Fatal("invalid all choice accepted")
			}
			if job.permissionPolicy.mode() != "ask" {
				t.Fatal("invalid response enabled all")
			}
		}
		m.Shutdown()
	}
}
