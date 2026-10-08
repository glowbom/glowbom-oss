package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCompanionPermissionRememberChoicesRetainProviderScope(t *testing.T) {
	for _, test := range []struct {
		name, payload string
		choices       []string
		tool          string
		prefix        []string
	}{
		{"opencode", `{"id":"permission","sessionID":"provider-session","availableResponses":["once","always","reject"]}`, []string{"once", "always", "reject"}, "", nil},
		{"codex session", `{"id":"codex-request","sessionID":"codex:thread","availableResponses":["once","session","always","reject"]}`, []string{"once", "session", "reject"}, "", nil},
		{"acp build", `{"id":"acp-request","sessionID":"acp:agent","availableResponses":["once","build","always","reject"],"buildApprovalTool":"read_files"}`, []string{"once", "build", "reject"}, "read_files", nil},
		{"missing tool", `{"id":"acp-request","sessionID":"acp:agent","availableResponses":["once","build","reject"]}`, []string{"once", "reject"}, "", nil},
		{"foreign build", `{"id":"permission","sessionID":"provider-session","availableResponses":["once","build","reject"],"buildApprovalTool":"read_files"}`, []string{"once", "reject"}, "", nil},
		{"command prefix", `{"id":"codex-request","sessionID":"codex:thread","availableResponses":["once","execpolicy","reject"],"execPolicyAmendment":["npm","run","build"]}`, []string{"once", "execpolicy", "reject"}, "", []string{"npm", "run", "build"}},
		{"missing prefix", `{"id":"codex-request","sessionID":"codex:thread","availableResponses":["once","execpolicy","reject"]}`, []string{"once", "reject"}, "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			public := companionPublicPending(json.RawMessage(test.payload), "permission")
			if !reflect.DeepEqual(public["availableResponses"], test.choices) {
				t.Fatal(public)
			}
			if test.tool != "" && public["buildApprovalTool"] != test.tool {
				t.Fatal(public)
			}
			if test.prefix != nil && !reflect.DeepEqual(public["execPolicyAmendment"], test.prefix) {
				t.Fatal(public)
			}
			if _, leaked := public["sessionID"]; leaked {
				t.Fatal("private session leaked", public)
			}
		})
	}
}

func TestCompanionPermissionRespondsOnlyWithTheWaitingProvidersChoices(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	for _, test := range []struct{ response, session, metadata string }{
		{"session", "codex:thread", `"availableResponses":["once","session","reject"]`},
		{"build", "acp:agent", `"availableResponses":["once","build","reject"],"buildApprovalTool":"read_files"`},
		{"always", "provider-session", `"availableResponses":["once","always","reject"]`},
		{"execpolicy", "codex:thread", `"availableResponses":["once","execpolicy","reject"],"execPolicyAmendment":["npm","run"]`},
	} {
		t.Run(test.response, func(t *testing.T) {
			calls := 0
			var forwarded OpenCodePermissionRespondRequest
			s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/opencode/permission/respond" || json.NewDecoder(r.Body).Decode(&forwarded) != nil {
					t.Error("wrong adapter route")
				}
				writeJSON(w, map[string]bool{"ok": true})
			}))
			p := sharedCompanionProject(t, s)
			pending := json.RawMessage(`{"id":"permission","sessionID":"` + test.session + `",` + test.metadata + `}`)
			job := &companionJob{id: "job", projectID: p.ID, projectPath: p.path, kind: "build", status: "waiting", ctx: s.ctx, pendingPermission: pending}
			s.jobs[job.id] = job
			w := httptest.NewRecorder()
			s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/builds/job/respond", `{"kind":"permission","id":"permission","response":"unknown"}`))
			if w.Code != 400 || calls != 0 {
				t.Fatal("unoffered response reached provider", w.Code, calls)
			}
			body := `{"kind":"permission","id":"permission","response":"` + test.response + `"}`
			for i := 0; i < 2; i++ {
				w = httptest.NewRecorder()
				s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/builds/job/respond", body))
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			if calls != 1 || forwarded.ProjectPath != p.path || forwarded.SessionID != test.session || forwarded.Response != test.response || forwarded.PermissionID != "permission" {
				t.Fatal("decision replayed or changed its scope", calls, forwarded)
			}
		})
	}
}

func TestCodexRememberCommandPrefixReturnsOnlyTheExactOfferedAmendment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	project := t.TempDir()
	amendment := `{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["npm","run","build"]}}`
	params := `{"threadId":"thread","command":"npm run build","availableDecisions":["accept",` + amendment + `,"decline"]}`
	result, err := waitForCodexInput(ctx, project, codexTestMessage("item/commandExecution/requestApproval", params), func(event map[string]interface{}) {
		permission := event["permission"].(map[string]any)
		if !reflect.DeepEqual(permission["availableResponses"], []string{"once", "execpolicy", "reject"}) || !reflect.DeepEqual(permission["execPolicyAmendment"], []string{"npm", "run", "build"}) {
			t.Fatal(permission)
		}
		for _, choice := range []string{"always", "session", "build", "execpolicy:npm"} {
			if err := respondToCodexInput(project, permission["sessionID"].(string), permission["id"].(string), "permission", choice, nil, nil); err == nil {
				t.Fatal("accepted invented scope", choice)
			}
		}
		if err := respondToCodexInput(project, permission["sessionID"].(string), permission["id"].(string), "permission", "execpolicy", nil, nil); err != nil {
			t.Fatal(err)
		}
	})
	encoded, _ := json.Marshal(result)
	if err != nil || string(encoded) != `{"decision":`+amendment+`}` {
		t.Fatal(string(encoded), err)
	}
}

func TestCodexRememberCommandPrefixFailsClosedForAmbiguousOrUnsafeScope(t *testing.T) {
	for _, decisions := range []string{
		`["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":[]}},{"applyNetworkPolicyAmendment":{}},"decline"]`,
		`["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["npm"]}},{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["bun"]}},"decline"]`,
		`["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["npm\nrun"]}},"decline"]`,
	} {
		var offered []json.RawMessage
		if err := json.Unmarshal([]byte(decisions), &offered); err != nil {
			t.Fatal(err)
		}
		responses, payloads := codexApprovalResponses(offered)
		if !reflect.DeepEqual(responses, []string{"once", "reject"}) || payloads["execpolicy"] != nil {
			t.Fatal(responses, payloads)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := waitForCodexInput(ctx, t.TempDir(), codexTestMessage("item/fileChange/requestApproval", `{"threadId":"thread","availableDecisions":[{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["npm"]}}]}`), func(map[string]interface{}) { t.Fatal("file approval exposed a command grant") })
	if err == nil || !strings.Contains(err.Error(), "file approval") {
		t.Fatal(err)
	}
}
