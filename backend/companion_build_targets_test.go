package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCompanionBuildTargetsIncludeNewAndSavedStacks(t *testing.T) {
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request previewRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if r.URL.Path != "/preview" || request.Action != "tools" || !request.ProjectRoot {
			t.Error("catalog did not inspect Desktop project tools")
		}
		io.WriteString(w, `{"editors":[{"id":"cursor","name":"Cursor private account"},{"id":"cursor","name":"Duplicate"},{"id":"shell","name":"Hidden"}],"path":"/private/hidden"}`)
	}))
	p := sharedCompanionProject(t, s)
	custom := previewDefinition{ID: "custom-game", Name: "Game", Directory: "game", Description: "Use Godot 4.", PreviewMode: "none"}
	stackBuildFixture(t, p.path, custom)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/"+p.ID+"/build-targets", ""))
	var result struct {
		Targets         []companionBuildTarget `json:"targets"`
		SelectedTargets []string               `json:"selectedTargets"`
		Editors         []stackEditor          `json:"editors"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("catalog: %d %s", w.Code, w.Body.String())
	}
	if len(result.Targets) != 5 || !reflect.DeepEqual(result.SelectedTargets, []string{custom.ID}) || !reflect.DeepEqual(result.Editors, []stackEditor{{ID: "cursor", Name: "Cursor"}}) {
		t.Fatal("catalog did not preserve saved stack scope or sanitize editors", result)
	}
	for _, target := range result.Targets {
		if !target.Available || filepath.IsAbs(target.Directory) {
			t.Fatal("a new stack could not be selected", target)
		}
	}
	if strings.Contains(w.Body.String(), p.path) || strings.Contains(w.Body.String(), "/private/hidden") || strings.Contains(w.Body.String(), custom.Description) {
		t.Fatal("catalog exposed private Desktop data", w.Body.String())
	}
	s.rememberBuildTargets(p, []string{"apple", custom.ID})
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/"+p.ID+"/build-targets", ""))
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if !reflect.DeepEqual(result.SelectedTargets, []string{"apple", custom.ID}) {
		t.Fatal("catalog forgot explicit selection", result)
	}
}

func TestCompanionBuildTargetsRejectInvalidAndEscapingFolders(t *testing.T) {
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/models" {
			io.WriteString(w, `{"models":[]}`)
			return
		}
		io.WriteString(w, `{"editors":[]}`)
	}))
	p := sharedCompanionProject(t, s)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(p.path, "outside")); err != nil {
		t.Fatal(err)
	}
	stackBuildFixture(t, p.path, previewDefinition{ID: "custom-outside", Name: "Outside", Directory: "outside/new"})
	for _, targets := range []string{`[]`, `["unknown"]`, `["../outside"]`, `["custom-outside"]`} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/build", `{"instructions":"Build","buildTargets":`+targets+`}`))
		if w.Code != 400 {
			t.Fatalf("accepted invalid targets %s: %d %s", targets, w.Code, w.Body.String())
		}
	}
	if len(s.jobs) != 0 {
		t.Fatal("invalid selection created a worker")
	}
	if !companionBuildDirectoryAllowed(p.path, "apple/new") || companionBuildDirectoryAllowed(p.path, "outside/new") {
		t.Fatal("directory boundary did not distinguish new and escaping targets")
	}
	if err := os.Symlink(filepath.Join(outside, "missing"), filepath.Join(p.path, "dangling")); err != nil {
		t.Fatal(err)
	}
	if companionBuildDirectoryAllowed(p.path, "dangling/new") {
		t.Fatal("dangling external symlink appeared safe to create")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/"+p.ID+"/build-targets", ""))
	var result struct {
		Targets []companionBuildTarget `json:"targets"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	for _, target := range result.Targets {
		if target.ID == "custom-outside" && target.Available {
			t.Fatal("escaping folder appeared available")
		}
	}
}

func TestCompanionBuildTargetsDefaultAndOldDesktopRecovery(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	for _, expected := range [][]string{{"prototype"}, {"apple", "web"}} {
		if len(expected) == 2 {
			for _, dir := range expected {
				if err := os.Mkdir(filepath.Join(p.path, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/"+p.ID+"/build-targets", ""))
		var result struct {
			SelectedTargets []string      `json:"selectedTargets"`
			Editors         []stackEditor `json:"editors"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !reflect.DeepEqual(result.SelectedTargets, expected) || result.Editors == nil {
			t.Fatalf("default selection: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestCompanionOpenUsesInstalledEditorAndSharedProject(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	var opened []previewRequest
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req previewRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/preview" || r.Header.Get("Authorization") != "Bearer desktop-token" {
			t.Error("open did not reuse authenticated Desktop tool handler")
		}
		if req.Action == "tools" {
			io.WriteString(w, `{"editors":[{"id":"cursor","name":"Cursor"}]}`)
		} else {
			opened = append(opened, req)
			writeJSON(w, map[string]bool{"success": true})
		}
	}))
	p := sharedCompanionProject(t, s)
	for _, test := range []struct {
		body string
		want int
	}{
		{`{"editor":"cursor"}`, 200},
		{`{"editor":"cursor","target":"web"}`, 200},
		{`{"editor":"vscode"}`, 400},
		{`{"editor":"../cursor"}`, 400},
		{`{"editor":"cursor","target":"../outside"}`, 400},
		{`{"editor":"cursor","target":"missing"}`, 400},
		{`{"editor":"cursor","path":"/private"}`, 400},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/open", test.body))
		if w.Code != test.want || strings.Contains(w.Body.String(), p.path) {
			t.Fatalf("open %s: %d %s", test.body, w.Code, w.Body.String())
		}
	}
	if len(opened) != 2 || opened[0].Path != p.path || !opened[0].ProjectRoot || opened[1].Path != p.path || opened[1].ProjectRoot || opened[1].Target != "web" {
		t.Fatal("open escaped the shared project or changed its scope", opened)
	}
}

func waitCompanionBuild(t *testing.T, s *companionSession, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		job := s.jobs[id]
		s.mu.Unlock()
		if job != nil {
			value := job.snapshot(false)
			if value["finishedAt"] != nil {
				return value
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("build did not finish")
	return nil
}

func TestCompanionCursorUsesRegularBuildStackStagingAndHistory(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	bin := fakeCursor(t, `cat > received-prompt.txt
printf '%s\n' '{"type":"result","subtype":"success","result":"Updated the selected app"}'
`)
	t.Setenv("GLOWBOM_CURSOR_BIN", bin)
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[{"id":"cursor/auto","name":"Auto","provider":"Cursor"},{"id":"cursor/../hidden","name":"Hidden"}]}`)
	})
	manager := newCompanionManager(mux)
	mux.HandleFunc("/opencode/refine", manager.guardBuild(openCodeRefineHandler))
	s := testCompanion(t, mux)
	manager.session = s
	p := sharedCompanionProject(t, s)
	custom := previewDefinition{ID: "custom-ios", Name: "iOS", Directory: "my-app", Description: "Use SwiftUI and async await."}
	stackBuildFixture(t, p.path, custom)
	s.setModel(p, "cursor/auto", "phone")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/build", `{"instructions":"Build my app","model":"cursor/auto","buildTargets":["custom-ios"]}`))
	var initial struct {
		ID string `json:"id"`
	}
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &initial) != nil {
		t.Fatalf("Cursor start: %d %s", w.Code, w.Body.String())
	}
	final := waitCompanionBuild(t, s, initial.ID)
	if final["status"] != "completed" || final["agentDriver"] != "cursor" || final["model"] != "cursor/auto" || final["runId"] == nil || final["resultText"] != "Updated the selected app" {
		t.Fatal("Cursor did not produce a regular saved build", final)
	}
	if s.modelChoice(p, false).Model != "cursor/auto" || !reflect.DeepEqual(final["buildTargets"], []string{custom.ID}) {
		t.Fatal("Cursor choice or selected stack was not shared", final)
	}
	for _, name := range []string{"received-prompt.txt", "current_instructions/instructions.txt"} {
		content, err := os.ReadFile(filepath.Join(p.path, name))
		if err != nil || !strings.Contains(string(content), custom.Description) || !strings.Contains(string(content), "Only edit code in the selected target folders") {
			t.Fatalf("regular staging lost stack instructions: %s %v", name, err)
		}
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/"+p.ID+"/history", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Updated the selected app") || !strings.Contains(w.Body.String(), "Build my app") {
		t.Fatal("Cursor build did not enter normal project history", w.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(p.path, "history"))
	if err != nil || len(entries) != 1 {
		t.Fatal("Cursor did not save exactly one regular history entry", err)
	}
	data, err := os.ReadFile(filepath.Join(p.path, "history", entries[0].Name(), "entry.json"))
	var record agentHistoryEntryRecord
	if err != nil || json.Unmarshal(data, &record) != nil || record.Contributor != "Cursor" || record.Provider != "cursor" || record.RunID != final["runId"] || record.Status != "completed" {
		t.Fatal("Cursor history lost its regular contributor or run association", err, string(data))
	}
}

func TestCompanionDesktopCursorBuildIsSharedWithPhone(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	s.setModel(p, "cursor/auto", "desktop")
	manager := newCompanionManager(s.api)
	manager.session = s
	handler := manager.guardBuild(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"done\":true,\"success\":true,\"resultText\":\"Updated\"}\n\n")
	})
	w := httptest.NewRecorder()
	request, _ := json.Marshal(OpenCodeAgentRequest{ProjectPath: p.path, Instructions: "Update the app", Model: "auto", AgentDriver: "cursor", BuildTargets: []string{"apple"}})
	handler(w, httptest.NewRequest(http.MethodPost, "/opencode/refine", strings.NewReader(string(request))))
	jobs := s.jobList("build")
	if len(jobs) != 1 || jobs[0]["source"] != "desktop" || jobs[0]["model"] != "cursor/auto" || jobs[0]["agentDriver"] != "cursor" || jobs[0]["status"] != "completed" || s.modelChoice(p, false).Model != "cursor/auto" {
		t.Fatal("Desktop Cursor build was hidden from phone", jobs)
	}
	if !reflect.DeepEqual(s.buildTargets[p.ID], []string{"apple"}) {
		t.Fatal("Desktop stack choice was not shared")
	}
}

func TestCompanionBuildForwardsRegularOpenCodeStackSelection(t *testing.T) {
	started := make(chan OpenCodeAgentRequest, 1)
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/models" {
			io.WriteString(w, `{"models":[{"id":"connected/build","name":"Build","build":true}]}`)
			return
		}
		var req OpenCodeAgentRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		started <- req
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"done\":true,\"success\":true}\n\n")
	}))
	p := sharedCompanionProject(t, s)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/build", `{"instructions":"Make an app","model":"connected/build","buildTargets":["apple","web"]}`))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case request := <-started:
		if request.AgentDriver != "opencode" || request.ProjectPath != p.path || request.Model != "connected/build" || !request.PersistCurrentInstructionsToHistory || !reflect.DeepEqual(request.BuildTargets, []string{"apple", "web"}) {
			t.Fatal("phone changed regular OpenCode request", request)
		}
	case <-time.After(time.Second):
		t.Fatal("OpenCode build did not start")
	}
	var initial struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &initial)
	final := waitCompanionBuild(t, s, initial.ID)
	if !reflect.DeepEqual(final["buildTargets"], []string{"apple", "web"}) || final["agentDriver"] != "opencode" {
		t.Fatal("phone progress lost actual stack selection", final)
	}
}

func TestCompanionProjectToolsKeepPairingRequestGuards(t *testing.T) {
	var calls int
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"editors":[{"id":"cursor","name":"Cursor"}]}`)
	}))
	p := sharedCompanionProject(t, s)
	for _, path := range []string{"/build-targets", "/open"} {
		method, body := http.MethodGet, ""
		if path == "/open" {
			method, body = http.MethodPost, `{"editor":"cursor"}`
		}
		for _, guard := range []string{"missing-token", "public-peer", "origin", "wrong-host", "query", "unshared"} {
			t.Run(path+"/"+guard, func(t *testing.T) {
				r := companionRequest(s, method, "/projects/"+p.ID+path, body)
				switch guard {
				case "missing-token":
					r.Header.Del("Authorization")
				case "public-peer":
					r.RemoteAddr = "203.0.113.7:2000"
				case "origin":
					r.Header.Set("Origin", "https://untrusted.example")
				case "wrong-host":
					r.Host = "untrusted.example"
				case "query":
					r.URL.RawQuery = "path=/private"
				case "unshared":
					r.URL.Path = companionPrefix + "/projects/unshared" + path
				}
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code < 400 {
					t.Fatal("untrusted request reached Desktop", w.Code)
				}
			})
		}
	}
	s.cancel()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/open", `{"editor":"cursor"}`))
	if w.Code != 401 || calls != 0 {
		t.Fatal("revoked or untrusted request reached Desktop tools", w.Code, calls)
	}
}

func TestCompanionOpenStopsWhenPairingIsRevokedDuringToolInspection(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	var launched bool
	s.api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request previewRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Action == "tools" {
			s.cancel()
			io.WriteString(w, `{"editors":[{"id":"cursor","name":"Cursor"}]}`)
		} else {
			launched = true
		}
	})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/open", `{"editor":"cursor"}`))
	if w.Code < 400 || launched {
		t.Fatal("revocation after inspection still opened an editor", w.Code)
	}
}
