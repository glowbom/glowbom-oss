package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type companionJob struct {
	mu                   sync.Mutex
	responseMu           sync.Mutex
	id                   string
	projectID            string
	projectPath          string
	kind                 string
	status               string
	output               []string
	errorText            string
	startedAt            string
	finishedAt           string
	image                json.RawMessage
	imagePrompt          string
	imageSourceID        string
	video                json.RawMessage
	pendingPermission    json.RawMessage
	pendingQuestion      json.RawMessage
	ctx                  context.Context
	cancel               context.CancelFunc
	responded            map[string]bool
	instructions         string
	model                string
	reasoningEffort      string
	agentDriver          string
	agentName            string
	buildTargets         []string
	attachments          []companionImageAttachment
	runID                string
	resultText           string
	source               string
	agentSessionID       string
	resolvedDecisions    []companionDecision
	pendingMediaApproval json.RawMessage
	buildStatus          json.RawMessage
	liveStatuses         []json.RawMessage
	partialLine          string
	changedFiles         []string
	save                 func()
	saveMu               sync.Mutex
	savedAt              time.Time
	savedStatus          string
	savedFinishedAt      string
	permissionPolicy     *buildPermissionPolicy
}

type companionImageRequest struct {
	Prompt         string `json:"prompt"`
	AspectRatio    string `json:"aspectRatio"`
	SourceID       string `json:"sourceId"`
	ReferenceID    string `json:"referenceId,omitempty"`
	ReferenceImage string `json:"referenceImage,omitempty"`
}

// Desktop and phone builds share one lane per project and worker. Independent
// workers keep separate instruction staging directories.
func (m *companionManager) guardBuild(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next(w, r)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "Build request is too large.", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		var request struct {
			ProjectPath    string `json:"projectPath"`
			AgentDriver    string `json:"agentDriver"`
			PermissionMode string `json:"permissionMode"`
		}
		if json.Unmarshal(data, &request) != nil {
			next(w, r)
			return
		}
		if !validBuildPermissionMode(request.PermissionMode) {
			http.Error(w, "Choose ask or all for this build's permission mode.", http.StatusBadRequest)
			return
		}
		root, err := chatProjectRoot(request.ProjectPath)
		if err != nil {
			next(w, r)
			return
		}
		key := root + "|" + normalizedBuildDriver(request.AgentDriver)
		m.buildMu.Lock()
		if m.building[key] {
			m.buildMu.Unlock()
			http.Error(w, "A build is already running in this project.", http.StatusConflict)
			return
		}
		m.building[key] = true
		m.buildMu.Unlock()
		release := func() { m.buildMu.Lock(); delete(m.building, key); m.buildMu.Unlock() }
		m.trackDesktopBuild(next, w, r, root, data, release)
	}
}

func (m *companionManager) respond(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	var request companionResponseRequest
	if !companionDecode(w, r, &request, 16<<10) {
		return
	}
	job := m.run(request.JobID)
	if job == nil {
		http.NotFound(w, r)
		return
	}
	session := &companionSession{manager: m, api: m.api, ctx: m.runContext, now: m.now, jobs: map[string]*companionJob{job.id: job}}
	if session.submitResponse(w, r, request, false) {
		writeJSON(w, m.status())
	}
}

type companionResponseRequest struct {
	JobID              string             `json:"jobId,omitempty"`
	Kind               string             `json:"kind"`
	ID                 string             `json:"id"`
	Response           string             `json:"response,omitempty"`
	Answer             string             `json:"answer,omitempty"`
	AnswerByQuestionID AnswerByQuestionID `json:"answerByQuestionID,omitempty"`
	Answers            QuestionAnswers    `json:"answers,omitempty"`
}

type companionDecision struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func (s *companionSession) submitResponse(w http.ResponseWriter, r *http.Request, request companionResponseRequest, phone bool) bool {
	job := s.sharedJob(request.JobID)
	if job == nil {
		http.NotFound(w, r)
		return false
	}
	if phone {
		if _, ok := s.project(job.projectID); !ok || job.kind != "build" {
			http.NotFound(w, r)
			return false
		}
	}
	job.responseMu.Lock()
	defer job.responseMu.Unlock()
	key := request.Kind + ":" + request.ID
	job.mu.Lock()
	if job.responded[key] {
		job.mu.Unlock()
		return true
	}
	pending := job.pendingPermission
	if request.Kind == "question" {
		pending = job.pendingQuestion
	}
	if job.status != "waiting" || request.ID == "" || (request.Kind != "permission" && request.Kind != "question") || companionPendingID(pending) != request.ID {
		job.mu.Unlock()
		http.Error(w, "This request is no longer waiting.", http.StatusConflict)
		return false
	}
	var value struct {
		SessionID string `json:"sessionID"`
	}
	_ = json.Unmarshal(pending, &value)
	projectPath := job.projectPath
	jobContext := job.ctx
	driver := job.agentDriver
	job.mu.Unlock()
	path := "/opencode/question/respond"
	var payload any = OpenCodeQuestionRespondRequest{ProjectPath: projectPath, SessionID: value.SessionID, QuestionID: request.ID, Answer: request.Answer, AnswerByQuestionID: request.AnswerByQuestionID, Answers: request.Answers}
	var permissionChange *buildPermissionChange
	defer func() { permissionChange.finish(false) }()
	if request.Kind == "permission" {
		choices := []string{"once", "reject"}
		if public := companionPublicPending(pending, "permission"); public != nil {
			if advertised, exists := public["availableResponses"]; exists {
				choices = advertised.([]string)
			}
		}
		offered := false
		for _, choice := range choices {
			offered = offered || request.Response == choice
		}
		if !offered {
			http.Error(w, "Choose a permission option offered for this build request.", http.StatusBadRequest)
			return false
		}
		if request.Response == "all" {
			if !companionPermissionOffered(pending, "once") || !buildPermissionAllAvailable(jobContext, driver, projectPath, value.SessionID, true) {
				http.Error(w, "This build does not offer one-time approvals.", http.StatusBadRequest)
				return false
			}
			var err error
			permissionChange, err = beginBuildPermissionAll(jobContext, driver, projectPath, value.SessionID, request.ID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return false
			}
			request.Response = "once"
		}
		path = "/opencode/permission/respond"
		payload = OpenCodePermissionRespondRequest{ProjectPath: projectPath, SessionID: value.SessionID, PermissionID: request.ID, Response: request.Response}
	} else {
		if err := companionValidateQuestionResponse(pending, request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
	}
	if jobContext == nil || jobContext.Err() != nil {
		http.Error(w, "This build has stopped.", http.StatusConflict)
		return false
	}
	if request.Kind == "permission" {
		buildPermissionReserve(jobContext, driver, projectPath, value.SessionID, request.ID)
	}
	ctx, cancel := context.WithTimeout(jobContext, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	writer := &companionWriter{header: make(http.Header), job: job}
	s.api.ServeHTTP(writer, s.internalRequest(ctx, http.MethodPost, path, payload))
	if writer.code >= 400 || ctx.Err() != nil {
		if writer.code < 400 {
			writer.code = http.StatusConflict
		}
		http.Error(w, "Desktop could not submit this response. Check the build before trying again.", writer.code)
		return false
	}
	if request.Kind == "permission" {
		buildPermissionConfirm(jobContext, driver, projectPath, value.SessionID, request.ID)
	}
	permissionChange.finish(true)
	job.mu.Lock()
	if job.responded == nil {
		job.responded = map[string]bool{}
	}
	job.responded[key] = true
	job.recordDecision(request.Kind, request.ID)
	job.mu.Unlock()
	_ = job.acknowledge(request.Kind, request.ID)
	return true
}

func (j *companionJob) snapshot(local bool) map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	result := map[string]any{"id": j.id, "projectId": j.projectID, "kind": j.kind, "status": j.status,
		"output": append([]string{}, j.output...), "startedAt": j.startedAt}
	if j.kind == "image" {
		if j.imagePrompt != "" {
			result["prompt"] = j.imagePrompt
		}
		if j.imageSourceID != "" {
			result["sourceId"] = j.imageSourceID
		}
	}
	if j.kind == "build" {
		result["permissionMode"] = j.permissionPolicy.mode()
		result["instructions"], result["model"] = j.instructions, j.model
		result["agentDriver"] = j.agentDriver
		if j.agentDriver == "acp" && j.agentName != "" {
			result["agentName"] = j.agentName
		}
		if j.agentDriver == "codex" && j.reasoningEffort != "" {
			result["reasoningEffort"] = j.reasoningEffort
		}
		if j.buildTargets != nil {
			result["buildTargets"] = append([]string{}, j.buildTargets...)
		}
		if len(j.attachments) > 0 {
			result["attachments"] = append([]companionImageAttachment{}, j.attachments...)
		}
		result["source"] = j.source
		if j.agentSessionID != "" && local {
			result["sessionID"] = j.agentSessionID
		}
		if len(j.buildStatus) > 0 {
			result["buildStatus"] = append(json.RawMessage{}, j.buildStatus...)
		}
		if len(j.liveStatuses) > 0 {
			result["liveStatuses"] = append([]json.RawMessage{}, j.liveStatuses...)
		}
		if j.partialLine != "" {
			result["partialLine"] = j.partialLine
		}
		if len(j.changedFiles) > 0 {
			result["changedFiles"] = append([]string{}, j.changedFiles...)
		}
		if len(j.resolvedDecisions) > 0 {
			result["resolvedDecisions"] = append([]companionDecision{}, j.resolvedDecisions...)
		}
		if j.runID != "" {
			result["runId"] = j.runID
		}
		if j.resultText != "" {
			result["resultText"] = j.resultText
		}
	}
	if j.errorText != "" {
		result["error"] = j.errorText
	}
	if j.finishedAt != "" {
		result["finishedAt"] = j.finishedAt
	}
	if len(j.image) > 0 {
		result["image"] = append(json.RawMessage{}, j.image...)
	}
	if len(j.video) > 0 {
		result["video"] = append(json.RawMessage{}, j.video...)
	}
	if local {
		result["projectPath"] = j.projectPath
		if len(j.pendingPermission) > 0 {
			result["pendingPermission"] = append(json.RawMessage{}, j.pendingPermission...)
		}
		if len(j.pendingQuestion) > 0 {
			result["pendingQuestion"] = append(json.RawMessage{}, j.pendingQuestion...)
		}
		if len(j.pendingMediaApproval) > 0 {
			result["pendingMediaApproval"] = append(json.RawMessage{}, j.pendingMediaApproval...)
		}
	} else {
		if pending := companionPublicPending(j.pendingPermission, "permission"); pending != nil {
			result["pendingPermission"] = pending
		}
		if pending := companionPublicPending(j.pendingQuestion, "question"); pending != nil {
			result["pendingQuestion"] = pending
		}
	}
	return result
}

// Caller holds the job mutex; expose only request IDs needed to reconcile a
// Desktop control after its paired phone has already answered.
func (j *companionJob) recordDecision(kind, id string) {
	j.resolvedDecisions = append(j.resolvedDecisions, companionDecision{Kind: kind, ID: id})
	if len(j.resolvedDecisions) > 128 {
		j.resolvedDecisions = j.resolvedDecisions[len(j.resolvedDecisions)-128:]
	}
}

func companionPendingID(data json.RawMessage) string {
	var value struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(data, &value)
	return value.ID
}

func (j *companionJob) acknowledge(kind, id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if id == "" {
		return false
	}
	switch {
	case kind == "permission" && companionPendingID(j.pendingPermission) == id:
		j.pendingPermission = nil
	case kind == "question" && companionPendingID(j.pendingQuestion) == id:
		j.pendingQuestion = nil
	case kind == "media" && companionPendingID(j.pendingMediaApproval) == id:
		j.pendingMediaApproval = nil
	default:
		return false
	}
	if len(j.pendingPermission) == 0 && len(j.pendingQuestion) == 0 && len(j.pendingMediaApproval) == 0 && j.status == "waiting" {
		j.status = "running"
	}
	return true
}

func (j *companionJob) event(data []byte) {
	var event map[string]json.RawMessage
	if json.Unmarshal(data, &event) != nil {
		return
	}
	j.mu.Lock()
	defer func() {
		j.mu.Unlock()
		if j.save != nil {
			j.save()
		}
	}()
	if j.status == "canceled" {
		return
	}
	var runID, resultText string
	if j.agentDriver == "acp" {
		var name string
		if json.Unmarshal(event["agentName"], &name) == nil && name != "" {
			j.agentName = companionPublicRunText(name, 80)
		}
	}
	_ = json.Unmarshal(event["runId"], &runID)
	_ = json.Unmarshal(event["resultText"], &resultText)
	if runID != "" {
		j.runID = companionPublicText(runID, 160)
	}
	if resultText != "" {
		j.resultText = companionPublicRunText(resultText, 4000)
	}
	var sessionID string
	_ = json.Unmarshal(event["sessionID"], &sessionID)
	if sessionID != "" {
		j.agentSessionID = sessionID
	}
	if value := event["status"]; len(value) > 0 {
		var status buildStatus
		if json.Unmarshal(value, &status) == nil {
			status.Text = companionPublicRunText(status.Text, 200)
			status.Source = companionPublicRunText(status.Source, 40)
			status.At = companionPublicRunText(status.At, 40)
			clean, _ := json.Marshal(status)
			j.buildStatus = clean
			j.liveStatuses = append(j.liveStatuses, clean)
			if len(j.liveStatuses) > 50 {
				j.liveStatuses = j.liveStatuses[len(j.liveStatuses)-50:]
			}
		}
	}
	if value := event["mediaApproval"]; len(value) > 0 && string(value) != "null" {
		j.pendingMediaApproval = append(json.RawMessage{}, value...)
		j.status = "waiting"
	}
	var changed []string
	if json.Unmarshal(event["changedFiles"], &changed) == nil && len(changed) > 0 {
		j.changedFiles = nil
		for _, path := range changed {
			path = normalizeChangedFilePath(j.projectPath, path)
			if path == ".." || strings.HasPrefix(path, "../") || filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\\") || isBuildRunContextPath(path) {
				continue
			}
			j.changedFiles = append(j.changedFiles, companionPublicRunText(path, 512))
			if len(j.changedFiles) == 200 {
				break
			}
		}
	}
	var chunk string
	_ = json.Unmarshal(event["outputChunk"], &chunk)
	if chunk != "" {
		parts := strings.Split(j.partialLine+chunk, "\n")
		for _, line := range parts[:len(parts)-1] {
			j.output = append(j.output, companionPublicRunText(line, 2048))
		}
		j.partialLine = companionPublicRunText(parts[len(parts)-1], 4096)
		if len(j.output) > 256 {
			j.output = j.output[len(j.output)-256:]
		}
	}
	var output string
	_ = json.Unmarshal(event["output"], &output)
	var status struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(event["status"], &status)
	if status.Text != "" {
		output = status.Text
	}
	if output != "" {
		if strings.HasPrefix(output, "Session created: ") {
			j.agentSessionID = strings.TrimPrefix(output, "Session created: ")
		}
		output = companionPublicRunText(output, 2048)
		j.output = append(j.output, output)
		if len(j.output) > 256 {
			j.output = j.output[len(j.output)-256:]
		}
	}
	if value := event["permission"]; len(value) > 0 && string(value) != "null" {
		var pending struct {
			SessionID string `json:"sessionID"`
		}
		_ = json.Unmarshal(value, &pending)
		if !j.responded["permission:"+companionPendingID(value)] {
			j.pendingPermission = append(json.RawMessage{}, value...)
			j.agentSessionID = pending.SessionID
			j.status = "waiting"
		}
	}
	if value := event["question"]; len(value) > 0 && string(value) != "null" {
		var pending struct {
			SessionID string `json:"sessionID"`
		}
		_ = json.Unmarshal(value, &pending)
		if !j.responded["question:"+companionPendingID(value)] {
			j.pendingQuestion = append(json.RawMessage{}, value...)
			j.agentSessionID = pending.SessionID
			j.status = "waiting"
		}
	}
	var done, success bool
	_ = json.Unmarshal(event["done"], &done)
	_ = json.Unmarshal(event["success"], &success)
	if done {
		j.status = "failed"
		if success {
			j.status = "completed"
		}
		var message string
		_ = json.Unmarshal(event["error"], &message)
		if message != "" {
			j.errorText = sanitizeProviderError(errors.New(message))
		}
		j.pendingPermission, j.pendingQuestion, j.pendingMediaApproval = nil, nil, nil
		j.permissionPolicy.close()
	}
}

// Existing handlers stream into a bounded collector while the phone polls.
// Accepted builds belong to Desktop, independent of the requesting device.
type companionWriter struct {
	mu      sync.Mutex
	header  http.Header
	code    int
	buffer  []byte
	job     *companionJob
	observe func([]byte, http.Header, int)
}

func (w *companionWriter) Header() http.Header { return w.header }
func (w *companionWriter) WriteHeader(code int) {
	w.mu.Lock()
	if w.code == 0 {
		w.code = code
	}
	w.mu.Unlock()
}
func (w *companionWriter) Flush() {}
func (w *companionWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if len(w.buffer)+len(data) > 1<<20 {
		return 0, errors.New("companion response too large")
	}
	if w.observe != nil {
		w.observe(data, w.header, w.code)
	}
	w.buffer = append(w.buffer, data...)
	if strings.HasPrefix(w.header.Get("Content-Type"), "text/event-stream") {
		for {
			end := bytes.Index(w.buffer, []byte("\n\n"))
			if end < 0 {
				break
			}
			for _, line := range bytes.Split(w.buffer[:end], []byte("\n")) {
				if bytes.HasPrefix(line, []byte("data: ")) {
					w.job.event(line[6:])
				}
			}
			w.buffer = w.buffer[end+2:]
		}
	}
	return len(data), nil
}

func (s *companionSession) internalRequest(ctx context.Context, method, path string, body any) *http.Request {
	var encoded []byte
	if body != nil {
		encoded, _ = json.Marshal(body)
	}
	request, _ := http.NewRequestWithContext(ctx, method, "http://127.0.0.1"+path, bytes.NewReader(encoded))
	request.RemoteAddr = "127.0.0.1:0"
	request.Header.Set("Authorization", "Bearer "+glowbomServerToken())
	request.Header.Set("Content-Type", "application/json")
	return request
}

func (s *companionSession) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, companionPrefix)
	if path == r.URL.Path {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if s.routeBook(w, r, parts) {
		return
	}
	if s.routeTransfer(w, r, path, parts) {
		return
	}
	if s.routeDesktopPrototype(w, r, path, parts) {
		return
	}
	if s.routeProjectSettings(w, r, parts) {
		return
	}
	if s.routeAudioLibrary(w, r, path, parts) {
		return
	}
	switch {
	case path == "/chat/models" && r.Method == http.MethodGet:
		s.chatModels(w, r)
	case path == "/chat" && r.Method == http.MethodPost:
		s.localChat(w, r)
	case path == "/prototype" && r.Method == http.MethodPost:
		s.phonePrototype(w, r)
	case path == "/chat/attachments" && r.Method == http.MethodPost:
		project, err := s.localChatProject(r.Context())
		if err != nil {
			http.Error(w, "This connection stopped. Pair again to attach an image.", http.StatusConflict)
			return
		}
		s.uploadAttachment(w, r, project)
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "chat" && r.Method == http.MethodPost:
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.projectChat(w, r, project)
	case path == "/projects" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"projects": s.projectCatalog()})
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "history" && r.Method == http.MethodGet:
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.history(w, r, project)
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "build" && r.Method == http.MethodPost:
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Instructions   string   `json:"instructions"`
			Model          *string  `json:"model,omitempty"`
			BuildTargets   []string `json:"buildTargets,omitempty"`
			AttachmentIDs  []string `json:"attachmentIds,omitempty"`
			PermissionMode string   `json:"permissionMode,omitempty"`
		}
		if !companionDecode(w, r, &request, 16<<10) {
			return
		}
		if len(request.Instructions) > 8000 {
			http.Error(w, "Describe the build in up to 8,000 characters.", http.StatusBadRequest)
			return
		}
		if !validBuildPermissionMode(request.PermissionMode) {
			http.Error(w, "Choose ask or all for this build's permission mode.", http.StatusBadRequest)
			return
		}
		attachmentPaths, attachments, err := s.buildAttachments(r.Context(), project, request.AttachmentIDs)
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errCompanionAttachmentUnavailable) {
				code = http.StatusGone
			}
			http.Error(w, err.Error(), code)
			return
		}
		if strings.TrimSpace(request.Instructions) == "" {
			if len(attachments) == 0 {
				http.Error(w, "Describe the build or attach an image.", http.StatusBadRequest)
				return
			}
			request.Instructions = "Use the attached images."
		}
		model := s.modelChoice(project, false).Model
		if request.Model != nil {
			model = *request.Model
		}
		if err := s.validateModel(r.Context(), model); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := companionValidateBuildTargets(project.path, request.BuildTargets); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		driver, buildModel := companionBuildDriver(model)
		payload := OpenCodeAgentRequest{ProjectPath: project.path, Instructions: request.Instructions, Model: buildModel, BuildTargets: request.BuildTargets, InstructionAttachmentPaths: attachmentPaths,
			AgentDriver: driver, PermissionMode: request.PermissionMode, OpenAIAuthMode: "opencode-config", PersistCurrentInstructionsToHistory: true, MediaGenerationPolicy: "skip"}
		s.startJob(w, r, "build", project, "/opencode/refine", payload, attachments)
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "attachments" && r.Method == http.MethodPost:
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.uploadAttachment(w, r, project)
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "build-targets" && r.Method == http.MethodGet:
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.buildTargetCatalog(w, r, project)
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "open" && r.Method == http.MethodPost:
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.openProject(w, r, project)
	case path == "/models" && r.Method == http.MethodGet:
		models, err := s.models(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{"models": models})
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "build-model":
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.buildModel(w, r, project, false)
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "previews":
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.preview(w, r, project)
	case len(parts) == 5 && parts[0] == "projects" && parts[2] == "preview" && parts[4] == "browser-session":
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.previewBrowserSession(w, r, project, parts[3])
	case len(parts) == 5 && parts[0] == "projects" && parts[2] == "preview" && parts[4] == "browser-snapshot":
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.previewBrowserSnapshot(w, r, project, parts[3])
	case len(parts) >= 6 && parts[0] == "projects" && parts[2] == "preview" && parts[4] == "content":
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.previewContent(w, r, project, parts[3], strings.Join(parts[5:], "/"))
	case len(parts) == 3 && parts[0] == "builds" && parts[2] == "respond" && r.Method == http.MethodPost:
		var request companionResponseRequest
		if !companionDecode(w, r, &request, 16<<10) {
			return
		}
		if request.JobID != "" && request.JobID != parts[1] {
			http.Error(w, "Use the waiting build request.", http.StatusBadRequest)
			return
		}
		request.JobID = parts[1]
		if s.submitResponse(w, r, request, true) {
			job := s.sharedJob(request.JobID)
			writeJSON(w, job.snapshot(false))
		}
	case len(parts) == 2 && parts[0] == "builds":
		s.jobHandler(w, r, parts[1], "build")
	case path == "/builds" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"builds": s.jobList("build")})
	case path == "/images" && r.Method == http.MethodGet:
		s.api.ServeHTTP(w, s.internalRequest(r.Context(), http.MethodGet, "/studio/images", nil))
	case path == "/videos" && r.Method == http.MethodGet:
		s.api.ServeHTTP(w, s.internalRequest(r.Context(), http.MethodGet, "/studio/videos", nil))
	case path == "/videos/sources" && r.Method == http.MethodGet:
		catalogue := studioVideoCapabilities()
		for i := range catalogue.Sources {
			source := &catalogue.Sources[i]
			if source.RequiresAPIKey && !source.Connected {
				key, err := studioVideoKeys.Get(source.ID)
				source.Connected = err == nil && strings.TrimSpace(key) != ""
			}
		}
		writeJSON(w, catalogue)
	case path == "/videos" && r.Method == http.MethodPost:
		s.startStudioVideo(w, r)
	case path == "/videos/generations" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"generations": s.jobList("video")})
	case len(parts) == 3 && parts[0] == "videos" && parts[1] == "generations":
		s.jobHandler(w, r, parts[2], "video")
	case path == "/images/sources" && r.Method == http.MethodGet:
		s.api.ServeHTTP(w, s.internalRequest(r.Context(), http.MethodGet, "/opencode/project/icon/sources", nil))
	case len(parts) == 3 && parts[0] == "images" && parts[2] == "content" && r.Method == http.MethodGet:
		id := normalizedStudioUUID(parts[1])
		if id == "" {
			http.NotFound(w, r)
			return
		}
		s.api.ServeHTTP(w, s.internalRequest(r.Context(), http.MethodGet, "/studio/images/content?id="+url.QueryEscape(id), nil))
	case len(parts) == 3 && parts[0] == "videos" && parts[2] == "content" && r.Method == http.MethodGet:
		id := normalizedStudioUUID(parts[1])
		if id == "" {
			http.NotFound(w, r)
			return
		}
		s.api.ServeHTTP(w, s.internalRequest(r.Context(), http.MethodGet, "/studio/videos/content?id="+url.QueryEscape(id), nil))
	case path == "/images" && r.Method == http.MethodPost:
		var request companionImageRequest
		if !companionDecode(w, r, &request, 3<<20) {
			return
		}
		if err := validateCompanionStudioReference(request.ReferenceID, request.ReferenceImage); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > 4000 || (request.AspectRatio != "1:1" && request.AspectRatio != "16:9" && request.AspectRatio != "9:16" && !(request.SourceID == "glowbom-api" && request.AspectRatio == "")) || len(request.SourceID) > 80 || (request.ReferenceID != "" && normalizedStudioUUID(request.ReferenceID) == "") {
			http.Error(w, "Choose an image source, shape, and prompt up to 4,000 characters.", http.StatusBadRequest)
			return
		}
		allowed := false
		for _, source := range projectIconSources(r.Context()) {
			allowed = allowed || source.ID == request.SourceID
		}
		if !allowed {
			http.Error(w, "Choose a Desktop image source.", http.StatusBadRequest)
			return
		}
		// Older phone clients always send a shape. Account images use the CLI's
		// automatic shape, just like Desktop Studio, which omits this setting.
		if request.SourceID == "glowbom-api" {
			request.AspectRatio = ""
		}
		s.startJob(w, r, "image", companionProject{}, "/studio/images/generate", request)
	case len(parts) == 3 && parts[0] == "images" && parts[1] == "generations":
		s.jobHandler(w, r, parts[2], "image")
	case path == "/images/generations" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"generations": s.jobList("image")})
	default:
		http.NotFound(w, r)
	}
}

func (s *companionSession) project(id string) (companionProject, bool) {
	s.mu.Lock()
	project, ok := s.projects[id]
	s.mu.Unlock()
	if !ok {
		return project, false
	}
	root, err := chatProjectRoot(project.path)
	if err != nil || root != project.path {
		return companionProject{}, false
	}
	return project, true
}

func (s *companionSession) history(w http.ResponseWriter, r *http.Request, project companionProject) {
	text, err := readChatProjectFile(project.path, ".glowbom/chat.json", maxChatHistoryBytes)
	messages := []chatMessage{}
	if err != nil || (text != "" && json.Unmarshal([]byte(text), &messages) != nil) {
		http.Error(w, "Could not read this saved conversation.", http.StatusBadRequest)
		return
	}
	if messages == nil {
		messages = []chatMessage{}
	}
	entries := []map[string]any{}
	root, err := os.OpenRoot(project.path)
	if err != nil {
		http.Error(w, "This project is unavailable.", http.StatusBadRequest)
		return
	}
	defer root.Close()
	if folder, err := root.Open("history"); err == nil {
		children, _ := folder.ReadDir(-1)
		_ = folder.Close()
		sort.Slice(children, func(i, j int) bool { return children[i].Name() > children[j].Name() })
		for _, child := range children {
			if !child.IsDir() || len(entries) >= 50 {
				continue
			}
			file, err := root.Open(filepath.Join("history", child.Name(), "entry.json"))
			if err != nil {
				continue
			}
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 {
				_ = file.Close()
				continue
			}
			data, err := io.ReadAll(io.LimitReader(file, 256<<10))
			_ = file.Close()
			var record agentHistoryEntryRecord
			if err != nil || json.Unmarshal(data, &record) != nil {
				continue
			}
			entries = append(entries, map[string]any{"id": record.ID, "timestamp": record.Timestamp, "instructions": record.Instructions,
				"status": record.Status, "outputSummary": record.OutputSummary, "model": record.Model, "changedFiles": record.ChangedFiles})
		}
	}
	writeJSON(w, map[string]any{"messages": messages, "entries": entries})
}

func (s *companionSession) startJob(w http.ResponseWriter, r *http.Request, kind string, project companionProject, path string, payload any, attachments ...[]companionImageAttachment) {
	if kind == "build" && s.manager != nil {
		build, _ := payload.(OpenCodeAgentRequest)
		s.manager.runMu.Lock()
		busy := false
		for _, existing := range s.manager.runs {
			existing.mu.Lock()
			busy = busy || existing.projectID == project.ID && normalizedBuildDriver(existing.agentDriver) == normalizedBuildDriver(build.AgentDriver) && (existing.status == "running" || existing.status == "waiting")
			existing.mu.Unlock()
		}
		s.manager.runMu.Unlock()
		if busy {
			http.Error(w, "This worker is already building this project.", http.StatusConflict)
			return
		}
	}
	s.mu.Lock()
	if s.ctx.Err() != nil || !s.now().Before(s.expires) {
		s.mu.Unlock()
		http.Error(w, "This connection expired. Pair again on Desktop.", http.StatusUnauthorized)
		return
	}
	if len(s.jobs) >= 32 {
		s.mu.Unlock()
		http.Error(w, "Pair again to start more requests.", http.StatusTooManyRequests)
		return
	}
	for _, existing := range s.jobs {
		existing.mu.Lock()
		busy := existing.kind == kind && (existing.status == "running" || existing.status == "waiting")
		if kind == "build" {
			build, _ := payload.(OpenCodeAgentRequest)
			busy = busy && existing.projectID == project.ID && normalizedBuildDriver(existing.agentDriver) == normalizedBuildDriver(build.AgentDriver)
		}
		existing.mu.Unlock()
		if busy {
			s.mu.Unlock()
			http.Error(w, "A companion request is already running. Check it before starting another.", http.StatusConflict)
			return
		}
	}
	owner := s.ctx
	if kind == "build" && s.manager != nil {
		owner = s.manager.runContext
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if kind == "build" {
		ctx, cancel = context.WithCancel(owner)
	} else {
		ctx, cancel = context.WithTimeout(owner, 30*time.Minute)
	}
	job := &companionJob{id: strings.ToLower(randomUUIDString()), projectID: project.ID, projectPath: project.path, kind: kind, status: "running", output: []string{}, startedAt: s.now().UTC().Format(time.RFC3339Nano), ctx: ctx, cancel: cancel}
	job.source = "phone"
	if image, ok := payload.(companionImageRequest); ok && kind == "image" {
		job.imagePrompt, job.imageSourceID = image.Prompt, image.SourceID
	}
	if len(attachments) > 0 {
		job.attachments = append([]companionImageAttachment{}, attachments[0]...)
	}
	if build, ok := payload.(OpenCodeAgentRequest); ok && kind == "build" {
		job.instructions, job.model = build.Instructions, build.Model
		job.agentDriver = build.AgentDriver
		job.buildTargets = appendBuildTargets(build.BuildTargets)
		job.agentDriver = normalizedBuildDriver(build.AgentDriver)
		job.reasoningEffort = companionRunReasoningEffort(job.agentDriver, build.ReasoningEffort)
		job.permissionPolicy = newBuildPermissionPolicy(ctx, job.agentDriver, project.path, build.PermissionMode)
		job.ctx = withBuildPermissionPolicy(ctx, job.permissionPolicy)
		ctx = job.ctx
		if job.agentDriver != "opencode" && !strings.HasPrefix(job.model, job.agentDriver+"/") {
			job.model = job.agentDriver + "/" + build.Model
		}
	}
	if kind == "build" && s.manager != nil {
		job.ctx = context.WithValue(ctx, companionRunContextKey{}, job.id)
		ctx = job.ctx
		job.save = func() { s.manager.saveRun(job) }
	}
	s.jobs[job.id] = job
	s.mu.Unlock()
	if kind == "build" && s.manager != nil {
		s.manager.registerRun(job)
		s.manager.saveRun(job)
	}
	if kind == "build" {
		s.rememberBuildTargets(project, job.buildTargets)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, job.snapshot(false))
	go func() {
		defer cancel()
		writer := &companionWriter{header: make(http.Header), job: job}
		if ctx.Err() == nil {
			s.api.ServeHTTP(writer, s.internalRequest(ctx, http.MethodPost, path, payload))
		}
		writer.mu.Lock()
		code, body := writer.code, append([]byte{}, writer.buffer...)
		writer.mu.Unlock()
		job.mu.Lock()
		defer func() {
			job.mu.Unlock()
			if job.save != nil {
				job.save()
			}
		}()
		if job.status != "canceled" {
			if ctx.Err() != nil {
				job.status = "canceled"
			} else if code >= 400 {
				job.status = "failed"
				job.errorText = "Desktop could not finish this request. Check its connection and try again."
				if kind == "image" || kind == "video" {
					job.errorText = companionStudioFailure(body)
				}
			} else if kind == "image" || kind == "video" {
				var result struct {
					Image json.RawMessage `json:"image"`
					Video json.RawMessage `json:"video"`
				}
				if json.Unmarshal(body, &result) == nil && ((kind == "image" && len(result.Image) > 0) || (kind == "video" && len(result.Video) > 0)) {
					job.image = result.Image
					job.video = result.Video
					job.status = "completed"
				} else {
					job.status = "failed"
					job.errorText = "No saved " + kind + " was returned."
				}
			} else if job.status == "running" || job.status == "waiting" {
				job.status = "failed"
				job.errorText = "The build ended without a completion result."
			}
		}
		job.pendingPermission, job.pendingQuestion, job.pendingMediaApproval = nil, nil, nil
		job.permissionPolicy.close()
		job.finishedAt = s.now().UTC().Format(time.RFC3339Nano)
	}()
}

func (s *companionSession) jobList(kind string) []map[string]any {
	result := []map[string]any{}
	if s.manager != nil && kind == "build" {
		for _, job := range s.manager.runList() {
			if job.kind == kind {
				if _, ok := s.project(job.projectID); ok {
					result = append(result, job.snapshot(false))
				}
			}
		}
	} else {
		s.mu.Lock()
		for _, job := range s.jobs {
			if job.kind == kind {
				result = append(result, job.snapshot(false))
			}
		}
		s.mu.Unlock()
	}
	sort.Slice(result, func(i, j int) bool {
		order := companionCompareStartedAt(result[i]["startedAt"].(string), result[j]["startedAt"].(string))
		if order != 0 {
			return order > 0
		}
		leftActive := result[i]["status"] == "running" || result[i]["status"] == "waiting"
		rightActive := result[j]["status"] == "running" || result[j]["status"] == "waiting"
		if leftActive != rightActive {
			return leftActive
		}
		return result[i]["id"].(string) > result[j]["id"].(string)
	})
	return result
}

func (s *companionSession) jobHandler(w http.ResponseWriter, r *http.Request, id, kind string) {
	job := s.sharedJob(id)
	if job == nil || job.kind != kind {
		http.NotFound(w, r)
		return
	}
	if job.kind == "build" && job.projectID != "" {
		if _, ok := s.project(job.projectID); !ok {
			http.NotFound(w, r)
			return
		}
	}
	if r.Method == http.MethodDelete {
		job.cancelJob(s.now())
	} else if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, job.snapshot(false))
}

func (job *companionJob) cancelJob(now time.Time) {
	job.mu.Lock()
	defer func() {
		job.mu.Unlock()
		if job.save != nil {
			job.save()
		}
	}()
	if job.status == "running" || job.status == "waiting" {
		job.status = "canceled"
		job.pendingPermission, job.pendingQuestion, job.pendingMediaApproval = nil, nil, nil
		job.finishedAt = now.UTC().Format(time.RFC3339Nano)
		job.permissionPolicy.close()
		if job.cancel != nil {
			job.cancel()
		}
	}
}

func companionCompareStartedAt(left, right string) int {
	// RFC3339Nano accepts legacy whole-second values too. Comparing parsed times
	// avoids ordering fractional timestamps before their same-second predecessors.
	leftTime, _ := time.Parse(time.RFC3339Nano, left)
	rightTime, _ := time.Parse(time.RFC3339Nano, right)
	if leftTime.Before(rightTime) {
		return -1
	}
	if leftTime.After(rightTime) {
		return 1
	}
	return 0
}
