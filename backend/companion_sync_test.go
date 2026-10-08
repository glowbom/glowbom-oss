package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func sharedCompanionProject(t *testing.T, s *companionSession) companionProject {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveProject(filepath.Join(root, "glowbom.json"), &GlowbomProject{Name: "Shared", Targets: map[string]Target{}}); err != nil {
		t.Fatal(err)
	}
	project := companionProject{ID: companionProjectID(root), Name: "Shared", Available: true, path: root}
	s.projects[project.ID] = project
	return project
}

func TestChatModelsReportBuildCapabilities(t *testing.T) {
	t.Setenv("GLOWBOM_CURSOR_BIN", "/missing/cursor-test")
	rememberCursorModels(nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"connected":["provider"],"all":[{"id":"provider","name":"Provider","models":{"code":{"tool_call":true,"modalities":{"output":["text"]}},"no-tools":{"tool_call":false},"image":{"tool_call":true,"modalities":{"output":["image"]}},"legacy":{},"maternion/mimo-v2.6:9b":{}}}]}`)
	}))
	defer server.Close()
	service := &chatService{directory: "/isolated", serverURL: server.URL, client: server.Client()}
	models, err := service.models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"provider/code": true, "provider/no-tools": false, "provider/image": false, "provider/legacy": true, "provider/maternion/mimo-v2.6:9b": false}
	if len(models) != len(want) {
		t.Fatal("unexpected connected catalog", models)
	}
	for _, model := range models {
		if model.Build != want[model.ID] {
			t.Errorf("%s build=%v want=%v", model.ID, model.Build, want[model.ID])
		}
	}
}

func TestCompanionBuildModelSelectionIsSharedAndValidated(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	builds := make(chan OpenCodeAgentRequest, 1)
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/models":
			io.WriteString(w, `{"models":[{"id":"connected/build","name":"Build","provider":"Connected","build":true},{"id":"connected/image","name":"Image","build":false},{"id":"cursor/build","name":"Cursor","build":true},{"id":"malformed","build":true}]}`)
		case "/opencode/refine":
			var req OpenCodeAgentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			builds <- req
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"done\":true,\"success\":true}\n\n")
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	}))
	p := sharedCompanionProject(t, s)
	if initial := s.buildModelList(true); len(initial) != 1 || !initial[0].Available || initial[0].ProjectPath != p.path || initial[0].Revision != 0 {
		t.Fatal("shared project missing initial Desktop model choice", initial)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/models", ""))
	if w.Code != 200 || strings.Contains(w.Body.String(), "Image") || !strings.Contains(w.Body.String(), "Cursor") || strings.Contains(w.Body.String(), "malformed") {
		t.Fatalf("unsafe models: %d %s", w.Code, w.Body.String())
	}
	for _, model := range []string{"connected/image", "unconnected/model", "cursor/unconnected", "cursor/../bad", "bad"} {
		w = httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/build", `{"instructions":"Build it","model":"`+model+`"}`))
		if w.Code != 400 {
			t.Errorf("model %q accepted: %d", model, w.Code)
		}
	}
	if len(s.jobs) != 0 {
		t.Fatal("invalid model started a job")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPut, "/projects/"+p.ID+"/build-model", `{"model":"connected/build"}`))
	var choice companionBuildModel
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &choice) != nil || choice.Revision != 1 || choice.Source != "phone" || choice.ProjectPath != "" {
		t.Fatalf("choice: %d %s", w.Code, w.Body.String())
	}
	m := newCompanionManager(s.api)
	m.session = s
	local := m.status()["buildModels"].([]companionBuildModel)
	if len(local) != 1 || local[0].Model != "connected/build" || local[0].ProjectPath != p.path || !local[0].Available {
		t.Fatal("Desktop did not receive the same model")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/build", `{"instructions":"Build a clock"}`))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case build := <-builds:
		if build.Model != "connected/build" || build.Instructions != "Build a clock" || build.ProjectPath != p.path {
			t.Fatal("build did not use shared selection")
		}
	case <-time.After(time.Second):
		t.Fatal("build did not start immediately")
	}
	var snapshot map[string]any
	if json.Unmarshal(w.Body.Bytes(), &snapshot) != nil || snapshot["model"] != "connected/build" || snapshot["instructions"] != "Build a clock" {
		t.Fatal("job lost model or instruction")
	}
	// The same model does not produce a new revision each polling cycle.
	s.setModel(p, "connected/build", "desktop")
	if s.modelChoice(p, false).Revision != 1 {
		t.Fatal("unchanged selection churned revision")
	}
	s.setModel(p, "", "desktop")
	if choice := s.modelChoice(p, false); choice.Revision != 2 || choice.Source != "desktop" || choice.Model != "" {
		t.Fatal("Desktop selection did not reach phone")
	}
	zero := uint64(0)
	s.setModelIfRevision(p, "connected/build", "desktop", &zero)
	if current := s.modelChoice(p, false); current.Revision != 2 || current.Model != "" {
		t.Fatal("initial Desktop choice overwrote a newer shared selection")
	}
	seed, _ := json.Marshal(map[string]any{"projectPath": p.path, "model": "connected/build", "expectedRevision": 0})
	r := httptest.NewRequest(http.MethodPut, "http://127.0.0.1/companion/build-model", strings.NewReader(string(seed)))
	r.RemoteAddr = "127.0.0.1:1000"
	r.Header.Set("Authorization", "Bearer desktop-token")
	w = httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &choice) != nil || choice.Revision != 2 || choice.Model != "" {
		t.Fatal("HTTP model seeding ignored newer phone choice", w.Code, w.Body.String())
	}
}

func TestCompanionBuildSubmissionPreservesNewerSharedModelChoice(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	for _, source := range []string{"phone", "desktop"} {
		t.Run(source, func(t *testing.T) {
			validationStarted := make(chan struct{})
			validationRelease := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(validationRelease) }) }
			defer release()
			var catalogCalls atomic.Int32
			started := make(chan OpenCodeAgentRequest, 1)
			worker := func(w http.ResponseWriter, r *http.Request) {
				var request OpenCodeAgentRequest
				_ = json.NewDecoder(r.Body).Decode(&request)
				started <- request
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"done\":true,\"success\":true}\n\n")
			}
			api := http.NewServeMux()
			api.HandleFunc("/chat/models", func(w http.ResponseWriter, r *http.Request) {
				if catalogCalls.Add(1) == 1 && source == "phone" {
					close(validationStarted)
					select {
					case <-validationRelease:
					case <-r.Context().Done():
						return
					}
				}
				io.WriteString(w, `{"models":[{"id":"connected/older","build":true},{"id":"connected/newer","build":true}]}`)
			})
			manager := newCompanionManager(api)
			t.Cleanup(manager.Shutdown)
			guardedWorker := manager.guardBuild(worker)
			api.HandleFunc("/opencode/refine", guardedWorker)
			s := testCompanion(t, api)
			s.manager, manager.session = manager, s
			project := sharedCompanionProject(t, s)
			s.setModel(project, "connected/older", source)
			response := httptest.NewRecorder()
			buildFinished := make(chan struct{})
			if source == "phone" {
				go func() {
					defer close(buildFinished)
					s.ServeHTTP(response, companionRequest(s, http.MethodPost, "/projects/"+project.ID+"/build", `{"instructions":"Build with the earlier model","model":"connected/older"}`))
				}()
			} else {
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				request := registryLocalRequest(http.MethodPost, "/opencode/refine", "")
				request.Body = reader
				go func() {
					defer close(buildFinished)
					guardedWorker(response, request)
				}()
				body, _ := json.Marshal(OpenCodeAgentRequest{ProjectPath: project.path, Instructions: "Build with the earlier model", Model: "connected/older", AgentDriver: "opencode"})
				go func() {
					if _, err := writer.Write(body[:1]); err != nil {
						return
					}
					close(validationStarted)
					<-validationRelease
					_, _ = writer.Write(body[1:])
					_ = writer.Close()
				}()
			}
			select {
			case <-validationStarted:
			case <-time.After(3 * time.Second):
				t.Fatal("earlier build did not reach the delayed acceptance point")
			}
			selection := httptest.NewRecorder()
			s.ServeHTTP(selection, companionRequest(s, http.MethodPut, "/projects/"+project.ID+"/build-model", `{"model":"connected/newer"}`))
			var newer companionBuildModel
			if selection.Code != http.StatusOK || json.Unmarshal(selection.Body.Bytes(), &newer) != nil || newer.Revision != 2 || newer.Model != "connected/newer" {
				t.Fatal("new explicit selection failed", selection.Code, selection.Body.String())
			}
			release()
			select {
			case <-buildFinished:
			case <-time.After(3 * time.Second):
				t.Fatal("earlier build did not finish accepting")
			}
			wantStatus := http.StatusOK
			if source == "phone" {
				wantStatus = http.StatusAccepted
			}
			if response.Code != wantStatus {
				t.Fatal("earlier build was rejected", response.Code, response.Body.String())
			}
			select {
			case request := <-started:
				if request.Model != "connected/older" || request.AgentDriver != "opencode" {
					t.Fatal("run lost its explicitly requested model", request)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("earlier build did not start")
			}
			jobs := manager.runList()
			if len(jobs) != 1 || jobs[0].snapshot(false)["model"] != "connected/older" {
				t.Fatal("recorded run lost its earlier model")
			}
			awaitRegistryStatus(t, manager, jobs[0].id, "completed")
			// The completion event precedes the final save. The worker cancels
			// its context after persistence, before the project can be removed.
			select {
			case <-jobs[0].ctx.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("completed build did not finish saving")
			}
			if current := s.modelChoice(project, false); current != newer {
				t.Fatal("older build overwrote the newer explicit selection", current, newer)
			}
		})
	}
}

func TestCompanionPhoneAndDesktopShareAtomicApproval(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	var calls int
	var callMu sync.Mutex
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/opencode/permission/respond" || r.Header.Get("Authorization") != "Bearer desktop-token" {
			t.Error("wrong authenticated approval route")
		}
		var req OpenCodePermissionRespondRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.SessionID != "session-private" || req.PermissionID != "permission" || req.Response != "once" {
			t.Error("approval changed pending request")
		}
		callMu.Lock()
		calls++
		callMu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	}))
	p := sharedCompanionProject(t, s)
	job := &companionJob{id: "job", projectID: p.ID, projectPath: p.path, kind: "build", status: "waiting", output: []string{}, ctx: s.ctx,
		pendingPermission: json.RawMessage(`{"id":"permission","sessionID":"session-private","title":"Read files","metadata":{"token":"private"},"message":"token=secret"}`)}
	s.jobs[job.id] = job
	remote, _ := json.Marshal(job.snapshot(false))
	if strings.Contains(string(remote), "session-private") || strings.Contains(string(remote), "metadata") || strings.Contains(string(remote), "secret") {
		t.Fatalf("unsafe pending details: %s", remote)
	}
	for _, body := range []string{`{"kind":"permission","id":"permission","response":"always"}`, `{"kind":"permission","id":"stale","response":"once"}`, `{"kind":"permission","id":"permission","response":"once","sessionID":"arbitrary"}`} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/builds/job/respond", body))
		if w.Code != 400 && w.Code != 409 {
			t.Fatal("unsupported or stale response reached worker", w.Code)
		}
	}
	m := newCompanionManager(s.api)
	m.session = s
	start := make(chan struct{})
	done := make(chan int, 2)
	go func() {
		<-start
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/builds/job/respond", `{"kind":"permission","id":"permission","response":"once"}`))
		done <- w.Code
	}()
	go func() {
		<-start
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/companion/respond", strings.NewReader(`{"jobId":"job","kind":"permission","id":"permission","response":"once"}`))
		r.RemoteAddr = "127.0.0.1:1000"
		r.Header.Set("Authorization", "Bearer desktop-token")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		done <- w.Code
	}()
	close(start)
	for i := 0; i < 2; i++ {
		if code := <-done; code != 200 {
			t.Fatal(code)
		}
	}
	callMu.Lock()
	total := calls
	callMu.Unlock()
	if total != 1 || job.snapshot(false)["pendingPermission"] != nil || job.snapshot(false)["status"] != "running" {
		t.Fatal("approval replayed or did not resume")
	}
}

func TestCompanionPhoneAnswersPendingQuestion(t *testing.T) {
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req OpenCodeQuestionRespondRequest
		if r.URL.Path != "/opencode/question/respond" || json.NewDecoder(r.Body).Decode(&req) != nil || req.QuestionID != "question" || req.AnswerByQuestionID["color"][0] != "Blue" {
			t.Error("wrong question response")
		}
		writeJSON(w, map[string]bool{"ok": true})
	}))
	p := sharedCompanionProject(t, s)
	job := &companionJob{id: "job", projectID: p.ID, projectPath: p.path, kind: "build", status: "waiting", output: []string{}, ctx: s.ctx,
		pendingQuestion: json.RawMessage(`{"id":"question","sessionID":"private","questions":[{"id":"color","question":"Which color?","multiple":false,"custom":true,"options":[{"label":"Blue","description":"Ocean"}]}]}`)}
	s.jobs[job.id] = job
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/builds/job/respond", `{"kind":"question","id":"question","answerByQuestionID":{"color":["Blue"]}}`))
	if w.Code != 200 || job.snapshot(false)["pendingQuestion"] != nil {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCompanionQuestionAnswersPreserveDisplayedOrder(t *testing.T) {
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request OpenCodeQuestionRespondRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Answers) != 2 || request.Answers[0][0] != "Blue" || request.Answers[1][0] != "Compact" {
			t.Error("ordered answers were lost or sorted")
		}
		writeJSON(w, map[string]bool{"ok": true})
	}))
	p := sharedCompanionProject(t, s)
	job := &companionJob{id: "job", projectID: p.ID, projectPath: p.path, kind: "build", status: "waiting", ctx: s.ctx,
		pendingQuestion: json.RawMessage(`{"id":"question","sessionID":"private","questions":[{"id":"z-color","prompt":"Color?"},{"id":"a-layout","prompt":"Layout?"}]}`)}
	s.jobs[job.id] = job
	for _, body := range []string{`{"kind":"question","id":"question","answers":[["Blue"]]}`, `{"kind":"question","id":"question","answers":[["Blue"],[]]}`, `{"kind":"question","id":"question","answerByQuestionID":{"arbitrary":["yes"]}}`} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/builds/job/respond", body))
		if w.Code != 400 {
			t.Fatal("partial or mismatched questions accepted", w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/builds/job/respond", `{"kind":"question","id":"question","answers":[["Blue"],["Compact"]]}`))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	job.event([]byte(`{"question":{"id":"question","sessionID":"private","prompt":"Color?"}}`))
	if snapshot := job.snapshot(false); snapshot["pendingQuestion"] != nil || snapshot["status"] != "running" || len(snapshot["resolvedDecisions"].([]companionDecision)) != 1 {
		t.Fatal("replayed answered question became pending")
	}
}

func TestCompanionPreviewExportsLatestStartupLogs(t *testing.T) {
	logs := make([]string, 80)
	for index := range logs {
		logs[index] = fmt.Sprintf("Starting dependency %02d", index)
	}
	logs[77] = "x" + strings.Repeat("界", 600)
	logs[78] = strings.Repeat("Checking dependency. ", 20) + "Required PHP extension is missing."
	logs[79] = "\x1b[31mStartup failed: permission denied; api_key=private-api-key password=private-password authorization: Bearer private-bearer\x1b[0m"
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/preview" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		writeJSON(w, map[string]any{"targets": []previewTarget{{Target: "web", Name: "Web", Kind: "custom", Available: true, Status: "failed", Logs: logs}}})
	}))
	p := sharedCompanionProject(t, s)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/"+p.ID+"/previews", ""))
	var result struct {
		Targets []companionPreviewTarget `json:"targets"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Targets) != 1 {
		t.Fatal("preview logs were unavailable", w.Code)
	}
	got := result.Targets[0].Logs
	if len(got) != 40 || got[0] != "Starting dependency 40" || got[36] != "Starting dependency 76" {
		t.Fatal("preview did not retain the latest 40 lines in order")
	}
	if !strings.Contains(got[38], "Required PHP extension is missing.") {
		t.Fatal("startup details were shortened as an error summary")
	}
	wantFailure := "Startup failed: permission denied; api_key=[redacted] password=[redacted] authorization=[redacted]"
	if got[39] != wantFailure || strings.Contains(w.Body.String(), "private-") || strings.Contains(got[39], "\x1b") {
		t.Fatal("final startup failure was lost or exposed a credential")
	}
	for _, line := range got {
		if len(line) > 1500 || !utf8.ValidString(line) {
			t.Fatal("preview log exceeded its bound or contained invalid UTF-8")
		}
	}
}

func TestCompanionPreviewReusesDesktopRunnerAndScopesContent(t *testing.T) {
	manager := newProjectPreviewManager()
	t.Cleanup(manager.Close)
	s := testCompanion(t, manager)
	s.previews = manager
	p := sharedCompanionProject(t, s)
	previewFixture(t, p.path, "prototype/index.html", `<h1>Shared preview</h1><img src="/assets/logo.svg">`)
	previewFixture(t, p.path, "prototype/assets/logo.svg", `<svg></svg>`)
	previewFixture(t, p.path, "prototype/.env", "SECRET=private")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/previews", `{"target":"prototype","action":"start","install":false}`))
	var result struct {
		Targets []companionPreviewTarget `json:"targets"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Targets) != 2 || result.Targets[0].Status != "running" {
		t.Fatal(w.Code, w.Body.String())
	}
	if result.Targets[0].Error != "" || result.Targets[0].Reason != "" {
		t.Fatal("healthy preview reported a failure", result.Targets[0])
	}
	if result.Targets[1].Error != "" || result.Targets[1].Reason != "No web folder is available inside this project." {
		t.Fatal("unavailable Web preview reported an unrelated failure", result.Targets[1])
	}
	if strings.Contains(w.Body.String(), `"error"`) {
		t.Fatal("empty preview errors must be omitted")
	}
	if strings.Contains(w.Body.String(), p.path) || strings.Contains(w.Body.String(), "127.0.0.1") || strings.Contains(w.Body.String(), "glowbom_preview") || strings.Contains(w.Body.String(), "command") {
		t.Fatal("preview settings leaked Desktop internals")
	}
	previewID := result.Targets[0].ID
	views, code := previewCall(t, manager, previewRequest{Path: p.path, Action: "inspect"})
	if code != 200 || views[0].ID != previewID {
		t.Fatal("phone started an independent runner")
	}
	get := func(asset, token string) *httptest.ResponseRecorder {
		r := companionRequest(s, http.MethodGet, "/projects/"+p.ID+"/preview/prototype/content/"+asset, "")
		r.Header.Set("X-Glowbom-Preview-ID", token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	w = get("", previewID)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Shared preview") || w.Header().Get("Set-Cookie") != "" || w.Header().Get("X-Glowbom-Preview-Content") != "1" {
		t.Fatal(w.Code, w.Body.String(), w.Header())
	}
	w = get("assets/logo.svg?v=1", previewID)
	if w.Code != 200 || w.Body.String() != "<svg></svg>" {
		t.Fatal("relative asset/query failed", w.Code, w.Body.String())
	}
	for _, asset := range []string{".env", "../.env", "%2e%2e/.env", "%252e%252e/.env", "@fs/private/config", "node_modules/package/.env"} {
		if w = get(asset, previewID); w.Code != 403 {
			t.Errorf("unsafe asset %q: %d", asset, w.Code)
		}
	}
	if w = get("", "stale"); w.Code != 409 || w.Header().Get("X-Glowbom-Preview-Content") != "" {
		t.Fatal("stale preview accepted")
	}
	_, _ = previewCall(t, manager, previewRequest{Path: p.path, Target: "prototype", Action: "stop", ID: previewID})
	if w = get("", previewID); w.Code != 409 {
		t.Fatal("stopped preview still available")
	}
}

func TestCompanionPreviewProxyStripsSecretsAndRejectsRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Glowbom-Preview-ID") != "" {
			t.Error("companion credentials reached project content")
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://192.168.1.50/secret", 302)
			return
		}
		if r.URL.Path == "/compile-error" {
			w.Header().Set("X-Glowbom-Preview-Content", "counterfeit")
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(500)
			fmt.Fprint(w, "<h1>Compile error in shared preview</h1>")
			return
		}
		w.Header().Set("Set-Cookie", "private=session")
		w.Header().Set("Authorization", "Bearer private")
		fmt.Fprint(w, "<h1>Web</h1>")
	}))
	defer server.Close()
	manager := newProjectPreviewManager()
	t.Cleanup(manager.Close)
	s := testCompanion(t, http.NotFoundHandler())
	s.previews = manager
	p := sharedCompanionProject(t, s)
	manager.sessions[p.path+"/web"] = &previewSession{project: p.path, cancel: func() {}, view: previewTarget{ID: "preview-id", URL: server.URL, Status: "running", Kind: "vite"}}
	for _, asset := range []string{"", "redirect", "compile-error"} {
		r := companionRequest(s, http.MethodGet, "/projects/"+p.ID+"/preview/web/content/"+asset, "")
		r.Header.Set("X-Glowbom-Preview-ID", "preview-id")
		r.Header.Set("Cookie", "phone-private=secret")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if asset == "" && (w.Code != 200 || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Authorization") != "") {
			t.Fatal("unsafe forwarded headers")
		}
		if asset == "redirect" && (w.Code != 502 || w.Header().Get("X-Glowbom-Preview-Content") != "") {
			t.Fatal("redirect escaped loopback target", w.Code)
		}
		if asset == "compile-error" && (w.Code != 500 || w.Header().Get("X-Glowbom-Preview-Content") != "1" || !strings.Contains(w.Body.String(), "Compile error")) {
			t.Fatal("useful project error was lost or counterfeit marker forwarded")
		}
	}
}

func TestCompanionPreviewRevocationCancelsProxy(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(stopped) }))
	defer server.Close()
	manager := newProjectPreviewManager()
	t.Cleanup(manager.Close)
	s := testCompanion(t, http.NotFoundHandler())
	s.previews = manager
	p := sharedCompanionProject(t, s)
	manager.sessions[p.path+"/web"] = &previewSession{project: p.path, cancel: func() {}, view: previewTarget{ID: "preview-id", URL: server.URL, Status: "running", Kind: "vite"}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := companionRequest(s, http.MethodGet, "/projects/"+p.ID+"/preview/web/content/", "").WithContext(context.Background())
		r.Header.Set("X-Glowbom-Preview-ID", "preview-id")
		s.ServeHTTP(httptest.NewRecorder(), r)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("proxy did not start")
	}
	s.close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("revocation left proxy running")
	}
	<-done
}

func TestCompanionTracksDesktopBuildAndDirectApprovals(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	started, continueBuild := make(chan struct{}), make(chan struct{})
	mux := http.NewServeMux()
	m := newCompanionManager(mux)
	s := testCompanion(t, mux)
	m.session = s
	p := sharedCompanionProject(t, s)
	mux.HandleFunc("/opencode/refine", m.guardBuild(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"permission\":{\"id\":\"approval\",\"sessionID\":\"agent-session\",\"title\":\"Read shared files\"}}\n\n")
		close(started)
		select {
		case <-continueBuild:
			io.WriteString(w, "data: {\"resultText\":\"Done\",\"done\":true,\"success\":true}\n\n")
		case <-r.Context().Done():
		}
	}))
	calls := 0
	mux.HandleFunc("/opencode/permission/respond", m.guardResponse("permission", func(w http.ResponseWriter, r *http.Request) {
		var request OpenCodePermissionRespondRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Response != "always" || request.ProjectPath != p.path || request.SessionID != "agent-session" || request.PermissionID != "approval" {
			t.Fatal("Desktop Always allow lost its decision or pending request scope", request, err)
		}
		calls++
		writeJSON(w, map[string]bool{"ok": true})
	}))
	body, _ := json.Marshal(map[string]string{"projectPath": p.path, "instructions": "Build a clock", "model": "connected/model", "agentDriver": "opencode"})
	stream := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		mux.ServeHTTP(stream, httptest.NewRequest(http.MethodPost, "http://127.0.0.1/opencode/refine", strings.NewReader(string(body))))
	}()
	<-started
	jobs := s.jobList("build")
	if len(jobs) != 1 || jobs[0]["source"] != "desktop" || jobs[0]["pendingPermission"] == nil || jobs[0]["instructions"] != "Build a clock" {
		t.Fatal("Desktop stream not shared", jobs)
	}
	id := jobs[0]["id"].(string)
	response, _ := json.Marshal(OpenCodePermissionRespondRequest{ProjectPath: p.path, SessionID: "agent-session", PermissionID: "approval", Response: "always"})
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "http://127.0.0.1/opencode/permission/respond", strings.NewReader(string(response))))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/builds/"+id+"/respond", `{"kind":"permission","id":"approval","response":"once"}`))
	if w.Code != 200 || calls != 1 || s.jobList("build")[0]["pendingPermission"] != nil {
		t.Fatal("direct Desktop approval did not synchronize once", w.Code, calls)
	}
	close(continueBuild)
	<-done
	if final := s.jobList("build")[0]; final["status"] != "completed" || final["resultText"] != "Done" || final["finishedAt"] == nil {
		t.Fatal("Desktop completion not shared", final)
	}
	if !strings.Contains(stream.Body.String(), "Read shared files") || !strings.Contains(stream.Body.String(), `"done":true`) {
		t.Fatal("tap changed original Desktop stream")
	}
}

func TestCompanionRevocationPreservesDesktopBuildAndRemoteCancelStopsIt(t *testing.T) {
	for _, revoke := range []bool{true, false} {
		t.Run(fmt.Sprint(revoke), func(t *testing.T) {
			m := newCompanionManager(http.NotFoundHandler())
			s := testCompanion(t, m.api)
			m.session = s
			p := sharedCompanionProject(t, s)
			started, finish, stopped := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
			handler := m.guardBuild(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-r.Context().Done():
					stopped <- true
				case <-finish:
					stopped <- false
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"done\":true,\"success\":true}\n\n")
				}
			})
			body, _ := json.Marshal(map[string]string{"projectPath": p.path, "instructions": "Build"})
			done := make(chan struct{})
			go func() {
				defer close(done)
				handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://127.0.0.1/opencode/refine", strings.NewReader(string(body))))
			}()
			<-started
			if revoke {
				m.Close()
				close(finish)
				if <-stopped {
					t.Fatal("revoking phone access canceled Desktop build")
				}
			} else {
				id := s.jobList("build")[0]["id"].(string)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, companionRequest(s, http.MethodDelete, "/builds/"+id, ""))
				if w.Code != 200 || !<-stopped {
					t.Fatal("phone cancel did not stop Desktop worker")
				}
			}
			<-done
		})
	}
}

func TestCompanionJobOrderingPreservesFractionalAndLegacyTimestamps(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	for _, job := range []*companionJob{
		{id: "legacy", kind: "build", status: "completed", startedAt: "2026-10-02T07:00:00Z"},
		{id: "fractional-early", kind: "build", status: "completed", startedAt: "2026-10-02T07:00:00.01Z"},
		{id: "fractional-late", kind: "build", status: "completed", startedAt: "2026-10-02T07:00:00.1Z"},
		{id: "same-active", kind: "build", status: "waiting", startedAt: "2026-10-02T07:00:00.100Z"},
		{id: "next-second", kind: "build", status: "completed", startedAt: "2026-10-02T07:00:01Z"},
	} {
		s.jobs[job.id] = job
	}
	want := []string{"next-second", "same-active", "fractional-late", "fractional-early", "legacy"}
	for run := 0; run < 10; run++ {
		jobs := s.jobList("build")
		for index, id := range want {
			if jobs[index]["id"] != id {
				t.Fatalf("unstable chronological order: %v", jobs)
			}
		}
	}
	stamp := time.Date(2026, 10, 2, 7, 0, 0, 123456789, time.UTC)
	job := &companionJob{status: "running"}
	job.cancelJob(stamp)
	if job.finishedAt != "2026-10-02T07:00:00.123456789Z" {
		t.Fatal("cancel timestamp lost precision", job.finishedAt)
	}
}
