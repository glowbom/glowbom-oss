package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func registryLocalRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("Authorization", "Bearer desktop-token")
	return r
}

func awaitRegistryStatus(t *testing.T, m *companionManager, id, status string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if job := m.run(id); job != nil && job.snapshot(true)["status"] == status {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("run %s did not reach %s", id, status)
}

func TestCompanionOwnedRunsSurviveDisconnectPairingAndUseSeparateInstructions(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	s := testCompanion(t, m.api)
	s.manager, m.session = m, s
	p := sharedCompanionProject(t, s)
	releases := map[string]chan struct{}{"opencode": make(chan struct{}), "cursor": make(chan struct{})}
	type startedRun struct{ driver, directory string }
	started := make(chan startedRun, 2)
	workerFinished := map[string]chan struct{}{"opencode": make(chan struct{}), "cursor": make(chan struct{})}
	handler := m.guardBuild(func(w http.ResponseWriter, r *http.Request) {
		var request OpenCodeAgentRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		driver := normalizedBuildDriver(request.AgentDriver)
		defer close(workerFinished[driver])
		directory := managedInstructionsDirectory(r.Context())
		if directory == "current_instructions" {
			t.Error("managed run used shared instructions")
		}
		if err := resetInstructionsDirectory(p.path, directory); err != nil {
			t.Error(err)
			return
		}
		if _, err := stageInstructionTextFileIn(p.path, request.Instructions, directory); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"status\":{\"text\":\"Cooking\",\"source\":\"agent\",\"at\":\"2026-10-02T08:00:00Z\"},\"outputChunk\":\"Saved first line\\nStill cooking\"}\n\n")
		started <- startedRun{driver, directory}
		status := "completed"
		select {
		case <-releases[driver]:
		case <-r.Context().Done():
			status = "canceled"
		}
		if err := persistCurrentInstructionsHistory(p.path, request.Instructions, status, "Done", agentHistoryMetadata{RunID: driver + "-fixture", Contributor: driver, InstructionsDirectory: directory}); err != nil {
			t.Error(err)
		}
		if status == "completed" {
			io.WriteString(w, "data: {\"done\":true,\"success\":true,\"changedFiles\":[\"prototype/index.html\"]}\n\n")
		}
	})
	streams := map[string]*httptest.ResponseRecorder{}
	finished := map[string]chan struct{}{}
	cancels := map[string]context.CancelFunc{}
	for _, driver := range []string{"opencode", "cursor"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[driver] = cancel
		defer cancel()
		body, _ := json.Marshal(OpenCodeAgentRequest{ProjectPath: p.path, AgentDriver: driver, Instructions: "Build with " + driver})
		stream, done := httptest.NewRecorder(), make(chan struct{})
		streams[driver], finished[driver] = stream, done
		go func() {
			defer close(done)
			handler(stream, registryLocalRequest("POST", "/opencode/refine", string(body)).WithContext(ctx))
		}()
	}
	directories := map[string]string{}
	for i := 0; i < 2; i++ {
		select {
		case run := <-started:
			directories[run.driver] = run.directory
		case <-time.After(3 * time.Second):
			t.Fatal("different workers did not run in parallel")
		}
	}
	if directories["opencode"] == directories["cursor"] {
		t.Fatal("workers share instruction directory")
	}
	ids := map[string]string{}
	for _, job := range m.runList() {
		ids[job.agentDriver] = job.id
	}
	w := httptest.NewRecorder()
	body, _ := json.Marshal(OpenCodeAgentRequest{ProjectPath: p.path, Instructions: "Duplicate", AgentDriver: "opencode"})
	handler(w, registryLocalRequest("POST", "/opencode/refine", string(body)))
	if w.Code != 409 {
		t.Fatal("same worker lane was not guarded", w.Code)
	}
	cancels["opencode"]()
	<-finished["opencode"]
	if streams["opencode"].Header().Get("X-Glowbom-Job-ID") != ids["opencode"] || !strings.Contains(streams["opencode"].Body.String(), `"jobId"`) {
		t.Fatal("job identity was not delivered")
	}
	if m.run(ids["opencode"]).snapshot(true)["status"] != "running" {
		t.Fatal("browser disconnect stopped worker")
	}
	m.Close()
	if m.status()["active"] != false || len(m.status()["jobs"].([]any)) != 2 {
		t.Fatal("registry depends on pairing")
	}
	for _, id := range ids {
		if m.run(id).ctx.Err() != nil {
			t.Fatal("pairing revocation canceled accepted run")
		}
	}
	fresh := testCompanion(t, m.api)
	fresh.manager, m.session = m, fresh
	fresh.projects[p.ID] = p
	w = httptest.NewRecorder()
	fresh.ServeHTTP(w, companionRequest(fresh, "GET", "/builds", ""))
	var list struct {
		Builds []map[string]any `json:"builds"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Builds) != 2 {
		t.Fatal("re-pair did not recover shared runs", w.Code, w.Body.String())
	}
	outside := &companionJob{id: "outside", projectID: "outside", projectPath: t.TempDir(), kind: "build", status: "completed", output: []string{}}
	m.registerRun(outside)
	for _, method := range []string{"GET", "DELETE"} {
		w = httptest.NewRecorder()
		fresh.ServeHTTP(w, companionRequest(fresh, method, "/builds/outside", ""))
		if w.Code != 404 {
			t.Fatal("phone saw or stopped unshared run", method, w.Code)
		}
	}
	w = httptest.NewRecorder()
	m.ServeHTTP(w, registryLocalRequest("POST", "/companion/cancel", `{"jobId":"`+ids["opencode"]+`"}`))
	if w.Code != 200 {
		t.Fatal("owner stop required pairing", w.Code)
	}
	awaitRegistryStatus(t, m, ids["opencode"], "canceled")
	<-workerFinished["opencode"]
	close(releases["cursor"])
	<-finished["cursor"]
	awaitRegistryStatus(t, m, ids["cursor"], "completed")
	if !strings.Contains(streams["cursor"].Body.String(), `"jobStatus":"completed"`) {
		t.Fatal("terminal registry state missing")
	}
	for driver, directory := range directories {
		data, err := os.ReadFile(filepath.Join(p.path, directory, "instructions.txt"))
		if err != nil || string(data) != "Build with "+driver+"\n" {
			t.Fatal("another worker overwrote instructions", driver, err, string(data))
		}
	}
	entries, err := os.ReadDir(filepath.Join(p.path, "history"))
	if err != nil || len(entries) != 2 {
		t.Fatal("parallel history missing", err, len(entries))
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(p.path, "history", entry.Name(), "instructions.txt"))
		metadata, _ := os.ReadFile(filepath.Join(p.path, "history", entry.Name(), "entry.json"))
		var record agentHistoryEntryRecord
		_ = json.Unmarshal(metadata, &record)
		if err != nil || strings.TrimSpace(string(data)) != record.Instructions {
			t.Fatal("history archived another run", err)
		}
	}
	restored := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(restored.Shutdown)
	restored.loadProjectRuns(p.path)
	if len(restored.runList()) != 2 {
		t.Fatal("saved run cards did not restore")
	}
	for driver, id := range ids {
		job := restored.run(id)
		if job == nil || job.snapshot(true)["status"] == "running" || job.snapshot(true)["buildStatus"] == nil || job.snapshot(true)["partialLine"] != "Still cooking" {
			t.Fatal("saved progress missing", driver)
		}
	}
}

func TestCompanionOwnedRunCancellationStreamAndPrivatePersistence(t *testing.T) {
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	s := testCompanion(t, m.api)
	s.manager, m.session = m, s
	p := sharedCompanionProject(t, s)
	started := make(chan struct{})
	done := make(chan struct{})
	handler := m.guardBuild(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"permission\":{\"id\":\"permission\",\"sessionID\":\"private-session\",\"title\":\"Read files\",\"metadata\":{\"token\":\"private-secret\"}}}\n\n")
		close(started)
		<-r.Context().Done()
	})
	body, _ := json.Marshal(OpenCodeAgentRequest{ProjectPath: p.path, Instructions: "Build"})
	stream := httptest.NewRecorder()
	go func() {
		defer close(done)
		handler(stream, registryLocalRequest("POST", "/opencode/refine", string(body)))
	}()
	<-started
	job := m.runList()[0]
	data, err := os.ReadFile(filepath.Join(p.path, ".glowbom", "runs", job.id, "run.json"))
	if err != nil || strings.Contains(string(data), "private-secret") || strings.Contains(string(data), "private-session") {
		t.Fatal("saved snapshot contains private approval details", err)
	}
	m.Close()
	job.cancelJob(time.Now())
	<-done
	if !strings.Contains(stream.Body.String(), `"cancelled":true`) {
		t.Fatal("explicit stop did not stream cancellation state", stream.Body.String())
	}
	file, err := os.Stat(filepath.Join(p.path, ".glowbom", "runs", job.id, "run.json"))
	if err != nil || file.Mode().Perm() != 0600 {
		t.Fatal("run snapshot is not private", err)
	}
	job2 := m.newRun(p, "desktop", OpenCodeAgentRequest{Instructions: "Interrupted"})
	restored := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(restored.Shutdown)
	restored.loadProjectRuns(p.path)
	if recovered := restored.run(job2.id); recovered == nil || recovered.snapshot(true)["status"] != "failed" || recovered.ctx != nil {
		t.Fatal("restart pretended worker was still running")
	}
}

func TestCompanionOwnedPhoneRunsKeepWorkerLanesAndOwnerAnswersAfterRevoke(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	mux := http.NewServeMux()
	m := newCompanionManager(mux)
	t.Cleanup(m.Shutdown)
	s := testCompanion(t, mux)
	s.manager, m.session = m, s
	p := sharedCompanionProject(t, s)
	started := make(chan string, 2)
	finished := make(chan string, 2)
	complete := make(chan struct{})
	mux.HandleFunc("/chat/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[{"id":"provider/code","name":"OpenCode","provider":"Provider","build":true},{"id":"cursor/auto","name":"Cursor","provider":"Cursor","build":true}]}`)
	})
	mux.HandleFunc("/opencode/refine", m.guardBuild(func(w http.ResponseWriter, r *http.Request) {
		var request OpenCodeAgentRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		w.Header().Set("Content-Type", "text/event-stream")
		if request.AgentDriver == "opencode" {
			io.WriteString(w, "data: {\"permission\":{\"id\":\"permission\",\"sessionID\":\"shared-session\",\"title\":\"Read files\"}}\n\n")
		}
		started <- request.AgentDriver
		select {
		case <-complete:
			io.WriteString(w, "data: {\"done\":true,\"success\":true}\n\n")
		case <-r.Context().Done():
		}
		finished <- request.AgentDriver
	}))
	answerCalls := 0
	mux.HandleFunc("/opencode/permission/respond", m.guardResponse("permission", func(w http.ResponseWriter, r *http.Request) {
		var request OpenCodePermissionRespondRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.ProjectPath != p.path || request.SessionID != "shared-session" || request.PermissionID != "permission" {
			t.Error("owner response lost exact run scope")
		}
		answerCalls++
		writeJSON(w, map[string]bool{"ok": true})
	}))
	ids := map[string]string{}
	for _, model := range []string{"provider/code", "cursor/auto"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, "POST", "/projects/"+p.ID+"/build", `{"instructions":"Build","model":"`+model+`"}`))
		var snapshot map[string]any
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &snapshot) != nil {
			t.Fatal("parallel phone worker was blocked", w.Code, w.Body.String())
		}
		ids[snapshot["agentDriver"].(string)] = snapshot["id"].(string)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("phone workers did not start")
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/projects/"+p.ID+"/build", `{"instructions":"Duplicate","model":"provider/code"}`))
	if w.Code != 409 {
		t.Fatal("same phone worker lane was not blocked", w.Code)
	}
	m.Close()
	for _, id := range ids {
		if m.run(id).ctx.Err() != nil {
			t.Fatal("phone revoke canceled accepted build")
		}
	}
	for i := 0; i < 2; i++ {
		w = httptest.NewRecorder()
		m.ServeHTTP(w, registryLocalRequest("POST", "/companion/respond", `{"jobId":"`+ids["opencode"]+`","kind":"permission","id":"permission","response":"once"}`))
		if w.Code != 200 {
			t.Fatal("unpaired owner could not answer build", w.Code, w.Body.String())
		}
	}
	if answerCalls != 1 {
		t.Fatal("owner approval replayed", answerCalls)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "DELETE", "/builds/"+ids["cursor"], ""))
	if w.Code != 401 {
		t.Fatal("revoked phone retained cancel access", w.Code)
	}
	fresh := testCompanion(t, mux)
	fresh.manager, m.session = m, fresh
	fresh.projects[p.ID] = p
	w = httptest.NewRecorder()
	fresh.ServeHTTP(w, companionRequest(fresh, "DELETE", "/builds/"+ids["cursor"], ""))
	if w.Code != 200 {
		t.Fatal("new pairing could not stop shared run", w.Code)
	}
	select {
	case driver := <-finished:
		if driver != "cursor" {
			t.Fatal("stopping Cursor interrupted OpenCode")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Cursor did not stop")
	}
	if m.run(ids["opencode"]).snapshot(true)["status"] != "running" {
		t.Fatal("other lane stopped")
	}
	close(complete)
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("OpenCode did not finish")
	}
	awaitRegistryStatus(t, m, ids["opencode"], "completed")
}

func TestCompanionOwnedMediaAnswerAndRunFilesStayScoped(t *testing.T) {
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	s := testCompanion(t, m.api)
	s.manager, m.session = m, s
	p := sharedCompanionProject(t, s)
	job := m.newRun(p, "desktop", OpenCodeAgentRequest{Instructions: "Build"})
	plan := &OpenCodeMediaApproval{Title: "Generate media?", Items: []OpenCodeMediaApprovalItem{}}
	id, responses := registerOpenCodeMediaApproval(p.path, plan)
	t.Cleanup(func() { removeOpenCodeMediaApproval(id) })
	plan.ID = id
	event, _ := json.Marshal(map[string]any{"mediaApproval": plan})
	job.event(event)
	if job.snapshot(true)["pendingMediaApproval"] == nil || job.snapshot(false)["pendingMediaApproval"] != nil {
		t.Fatal("media recovery escaped Desktop boundary")
	}
	m.Close()
	handler := m.guardResponse("media", openCodeMediaApprovalRespondHandler)
	wrong, _ := json.Marshal(openCodeMediaApprovalResponse{ApprovalID: id, Response: "skip", ProjectPath: t.TempDir()})
	w := httptest.NewRecorder()
	handler(w, registryLocalRequest("POST", "/opencode/media/approval/respond", string(wrong)))
	if w.Code != 400 {
		t.Fatal("media answer accepted wrong project", w.Code)
	}
	body, _ := json.Marshal(openCodeMediaApprovalResponse{ApprovalID: id, Response: "skip", ProjectPath: p.path})
	for i := 0; i < 2; i++ {
		w = httptest.NewRecorder()
		handler(w, registryLocalRequest("POST", "/opencode/media/approval/respond", string(body)))
		if w.Code != 200 {
			t.Fatal("media decision replay did not reconcile", w.Code, w.Body.String())
		}
	}
	select {
	case value := <-responses:
		if value != "skip" {
			t.Fatal("unexpected media response")
		}
	default:
		t.Fatal("media answer not sent")
	}
	select {
	case <-responses:
		t.Fatal("media answer replayed")
	default:
	}
	snapshot := job.snapshot(true)
	if snapshot["pendingMediaApproval"] != nil || snapshot["status"] != "running" || len(snapshot["resolvedDecisions"].([]companionDecision)) != 1 {
		t.Fatal("media decision did not synchronize")
	}
	files, err := captureProjectFileSnapshot(p.path)
	if err != nil {
		t.Fatal(err)
	}
	for path := range files {
		if strings.HasPrefix(path, ".glowbom/runs/") {
			t.Fatal("run state counted as generated source", path)
		}
	}
	job.event([]byte(`{"mediaApproval":{"id":"approval-to-cancel","title":"Generate media?","items":[]}}`))
	job.cancelJob(time.Now())
	if job.snapshot(true)["pendingMediaApproval"] != nil {
		t.Fatal("stopped run retained a media request")
	}
}

func TestCompanionOwnedProgressKeepsWhitespaceAndBoundedRequests(t *testing.T) {
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	s := testCompanion(t, m.api)
	p := sharedCompanionProject(t, s)
	request := strings.Repeat("More details. ", 60)
	job := m.newRun(p, "desktop", OpenCodeAgentRequest{Instructions: request})
	job.event([]byte(`{"outputChunk":"Hello "}`))
	job.event([]byte(`{"outputChunk":"world\nNext "}`))
	snapshot := job.snapshot(true)
	if snapshot["instructions"] != request || snapshot["partialLine"] != "Next " || snapshot["output"].([]string)[0] != "Hello world" {
		t.Fatal("recoverable progress altered whitespace or request")
	}
	job.event([]byte(`{"output":"token=private-secret","status":{"text":"token=private-secret","source":"agent","at":"2026-10-02T08:00:00Z"},"changedFiles":["../private.txt","prototype/index.html",".glowbom/runs/a/run.json"]}`))
	snapshot = job.snapshot(false)
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "private-secret") {
		t.Fatal("public progress did not redact credentials")
	}
	if files := snapshot["changedFiles"].([]string); len(files) != 1 || files[0] != "prototype/index.html" {
		t.Fatal("progress exposed files outside the source project", files)
	}
}
