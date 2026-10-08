package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const companionDesktopPrototypePromptBytes = 32 << 10
const companionDesktopPrototypeSourceExcerptBytes = 12 << 10
const companionDesktopPrototypeSourceBytes = 2 << 20
const companionDesktopPrototypeTimeout = 15 * time.Minute

type companionDesktopPrototypeImages struct {
	SourceID                   string `json:"sourceId"`
	PersonalizationReferenceID string `json:"personalizationReferenceId,omitempty"`
}

type companionDesktopPrototypeRequest struct {
	RequestID     string                           `json:"requestId"`
	DestinationID string                           `json:"destinationId"`
	Name          string                           `json:"name"`
	Prompt        string                           `json:"prompt"`
	Messages      []chatMessage                    `json:"messages,omitempty"`
	Model         string                           `json:"model"`
	AttachmentID  string                           `json:"attachmentId,omitempty"`
	Images        *companionDesktopPrototypeImages `json:"images,omitempty"`
}

type companionDesktopPrototypeJob struct {
	mu              sync.Mutex
	id              string
	hash            string
	request         companionDesktopPrototypeRequest
	status          string
	stage           string
	started         time.Time
	finished        time.Time
	project         *companionProject
	projectPath     string
	previewReady    bool
	sourceExcerpt   string
	sourceBytes     int
	imageIndex      int
	imageTotal      int
	imagePrompt     string
	imageProvider   string
	generatedImages []companionPrototypeImage
	warnings        []string
	errorText       string
	code            string
	ctx             context.Context
	cancel          context.CancelFunc
}

func (j *companionDesktopPrototypeJob) snapshot(now time.Time) map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	end := now
	if !j.finished.IsZero() {
		end = j.finished
	}
	elapsed := end.Sub(j.started).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	result := map[string]any{
		"id": j.id, "jobId": j.id, "requestId": j.request.RequestID,
		"projectName": j.request.Name, "model": j.request.Model,
		"status": j.status, "stage": j.stage,
		"startedAt": j.started.UTC().Format(time.RFC3339Nano), "elapsedSeconds": elapsed,
		"maxImages": maxChatImages, "previewReady": j.previewReady,
	}
	if j.request.Images != nil {
		result["sourceId"] = j.request.Images.SourceID
	}
	if j.project != nil {
		result["project"] = *j.project
		result["projectId"] = j.project.ID
		result["projectPath"] = j.projectPath
	}
	if j.imageTotal > 0 {
		result["imageIndex"], result["imageTotal"] = j.imageIndex, j.imageTotal
	}
	if len(j.generatedImages) > 0 {
		result["generatedImages"] = append([]companionPrototypeImage{}, j.generatedImages...)
	}
	if j.sourceBytes > 0 {
		result["sourceBytes"] = j.sourceBytes
		result["sourceExcerpt"] = j.sourceExcerpt
	}
	if j.status == "running" && j.imagePrompt != "" {
		result["imagePrompt"] = j.imagePrompt
		if j.imageProvider != "" {
			result["imageProvider"] = j.imageProvider
		}
	}
	if len(j.warnings) > 0 {
		result["warnings"] = append([]string{}, j.warnings...)
	}
	if j.errorText != "" {
		result["error"], result["code"] = j.errorText, j.code
	}
	return result
}

func companionPrototypeFailure(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"success": false, "code": code, "error": message})
}

func (s *companionSession) routeDesktopPrototype(w http.ResponseWriter, r *http.Request, path string, parts []string) bool {
	if path == "/projects/prototype/name" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		} else {
			s.suggestDesktopProjectName(w, r)
		}
		return true
	}
	if path == "/projects/prototype" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]any{"version": 1, "newDesktopProject": true, "asyncJobs": true,
				"maxImages": maxChatImages, "personalization": true,
				"nameSuggestion": true, "defaultDestination": true,
				"destinationSelection": runtime.GOOS == "darwin" || s.pickImportFolder != nil,
				"maxNameBytes":         100, "maxPromptBytes": companionDesktopPrototypePromptBytes,
				"maxContextMessages": maxChatContextMessages, "maxContextBytes": maxChatContextBytes})
		case http.MethodPost:
			s.startDesktopPrototype(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return true
	}
	if len(parts) != 2 || parts[0] != "prototypes" {
		return false
	}
	id := strings.ToLower(normalizedStudioUUID(parts[1]))
	s.mu.Lock()
	job := s.prototypeJobs[id]
	s.mu.Unlock()
	if id == "" || job == nil {
		http.NotFound(w, r)
		return true
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, job.snapshot(s.now()))
	case http.MethodDelete:
		job.mu.Lock()
		if job.status == "running" {
			job.status, job.stage = "canceled", "Stopped"
			job.finished = s.now()
			job.errorText, job.code = "Creation stopped. Any saved Desktop project is still available.", "canceled"
			job.cancel()
		}
		job.mu.Unlock()
		writeJSON(w, job.snapshot(s.now()))
	default:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
	return true
}

func normalizeCompanionDesktopPrototype(request *companionDesktopPrototypeRequest) error {
	request.RequestID, request.DestinationID = strings.ToLower(normalizedStudioUUID(request.RequestID)), strings.ToLower(normalizedStudioUUID(request.DestinationID))
	if request.RequestID == "" || request.DestinationID == "" {
		return errors.New("Choose a Desktop save folder and a valid creation request.")
	}
	if _, err := sketchProjectDestination("/", request.Name); err != nil {
		return errors.New("Choose a project name of up to 100 bytes without slashes, a leading dot, or control characters.")
	}
	if strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > companionDesktopPrototypePromptBytes {
		return errors.New("Describe the app in up to 32 KB of text.")
	}
	if !validCompanionModelID(request.Model) || len(request.Model) > 512 || isBuildOnlyCLIModel(request.Model) || strings.HasPrefix(request.Model, "opencode/") || isAppleIntelligenceModel(request.Model) || isManagedMiMoModel(request.Model) || request.Model == "ollama/maternion/mimo-v2.6:9b" || request.Model == localAIProvider+"/maternion/mimo-v2.6:9b" {
		return errors.New("Choose a connected model that can create a Desktop prototype.")
	}
	messages := append([]chatMessage{}, request.Messages...)
	if len(messages) == 0 || messages[len(messages)-1].Role != "user" || messages[len(messages)-1].Text != request.Prompt {
		messages = append(messages, chatMessage{Role: "user", Text: request.Prompt})
	}
	if len(messages) > maxChatContextMessages {
		return errors.New("Send up to 80 recent messages, including the current prototype request.")
	}
	contextBytes := 0
	for _, message := range messages {
		contextBytes += len(message.Text)
	}
	if contextBytes > maxChatContextBytes {
		return errors.New("This conversation is too large. Send a shorter prototype request.")
	}
	if err := validateChatRequest(chatRequest{Model: request.Model, Mode: "prototype", Messages: messages}); err != nil {
		return err
	}
	request.Messages = messages
	if request.AttachmentID != "" {
		request.AttachmentID = strings.ToLower(normalizedStudioUUID(request.AttachmentID))
		if request.AttachmentID == "" {
			return errors.New("Attach the drawing or photo again.")
		}
	}
	if request.Images != nil {
		if request.Images.SourceID == "" || len(request.Images.SourceID) > 80 {
			return errors.New("Choose an available Desktop image source.")
		}
		if request.Images.PersonalizationReferenceID != "" {
			request.Images.PersonalizationReferenceID = strings.ToLower(normalizedStudioUUID(request.Images.PersonalizationReferenceID))
			if request.Images.PersonalizationReferenceID == "" {
				return errors.New("Attach the original personalization photo again.")
			}
		}
	}
	return nil
}

func (s *companionSession) prototypeImageProvider(parent context.Context, id string) (string, bool) {
	ctx, stop := context.WithTimeout(parent, 30*time.Second)
	defer stop()
	writer := &companionWriter{header: make(http.Header), job: &companionJob{}}
	s.api.ServeHTTP(writer, s.internalRequest(ctx, http.MethodGet, "/opencode/project/icon/sources", nil))
	var response struct {
		Sources []projectIconSource `json:"sources"`
	}
	if ctx.Err() != nil || writer.code < 200 || writer.code >= 300 || json.Unmarshal(writer.buffer, &response) != nil {
		return "", false
	}
	for _, source := range response.Sources {
		if source.ID == id && source.Available {
			label := companionPublicText(source.Label, 200)
			if label == "" {
				label = companionPublicText(source.ID, 200)
			}
			return label, true
		}
	}
	return "", false
}

func (s *companionSession) startDesktopPrototype(w http.ResponseWriter, r *http.Request) {
	var request companionDesktopPrototypeRequest
	if !companionDecode(w, r, &request, 2<<20) {
		return
	}
	if err := normalizeCompanionDesktopPrototype(&request); err != nil {
		companionPrototypeFailure(w, 400, "invalid_input", err.Error())
		return
	}
	encoded, _ := json.Marshal(request)
	hash := fmt.Sprintf("%x", sha256.Sum256(encoded))
	replyExisting := func(job *companionDesktopPrototypeJob) {
		if job.hash != hash {
			companionPrototypeFailure(w, 409, "request_changed", "This creation ID was already used with different choices. Review a new request.")
			return
		}
		writeJSON(w, job.snapshot(s.now()))
	}
	preflightFailure := func(status int, code, message string) {
		// Another copy of this request may have been accepted during preflight.
		s.mu.Lock()
		accepted := s.prototypeJobs[request.RequestID]
		s.mu.Unlock()
		if accepted != nil {
			replyExisting(accepted)
			return
		}
		companionPrototypeFailure(w, status, code, message)
	}
	s.mu.Lock()
	existing := s.prototypeJobs[request.RequestID]
	s.mu.Unlock()
	if existing != nil {
		replyExisting(existing)
		return
	}
	ctx, stop := s.chatContext(r.Context())
	defer stop()
	parent, _, err := s.importDestinationParent(request.DestinationID)
	if err != nil {
		preflightFailure(409, "destination_unavailable", "Refresh the save folder before creating. Your draft is still available.")
		return
	}
	check, err := checkSketchProject(parent, request.Name)
	if err != nil || check["exists"] == true {
		preflightFailure(409, "name_taken", "That Desktop folder already exists or is unavailable. Choose another project name.")
		return
	}
	models, err := s.chatModelOptions(ctx)
	if err != nil {
		preflightFailure(502, "model_unavailable", "Desktop could not verify the selected model. Your draft is still available.")
		return
	}
	var model *chatModel
	for i := range models {
		if models[i].ID == request.Model {
			model = &models[i]
			break
		}
	}
	if model == nil || request.AttachmentID != "" && !model.Images {
		preflightFailure(400, "model_unavailable", "Choose the same connected model, with photo support when attaching a drawing or photo.")
		return
	}
	if request.Images != nil {
		if _, available := s.prototypeImageProvider(ctx, request.Images.SourceID); !available {
			preflightFailure(400, "image_source_unavailable", "The chosen Desktop image source is unavailable. Refresh sources or create without images.")
			return
		}
	}
	data := companionChatData{}
	if request.AttachmentID != "" || request.Images != nil && request.Images.PersonalizationReferenceID != "" {
		project, projectErr := s.localChatProject(ctx)
		if projectErr != nil {
			preflightFailure(409, "connection_stopped", "Pair again before creating. Your draft is still available.")
			return
		}
		if request.AttachmentID != "" {
			data.images, err = s.chatImages(ctx, project, []string{request.AttachmentID})
			if err != nil {
				preflightFailure(410, "attachment_unavailable", "Attach the drawing or photo again. Your draft is still available.")
				return
			}
		}
		if request.Images != nil && request.Images.PersonalizationReferenceID != "" {
			references, referenceErr := s.chatImages(ctx, project, []string{request.Images.PersonalizationReferenceID})
			if referenceErr != nil || len(references) != 1 {
				preflightFailure(410, "attachment_unavailable", "Attach the original personalization photo again. Your draft is still available.")
				return
			}
			data.personalizationReference = references[0]
		}
	}
	s.mu.Lock()
	if existing = s.prototypeJobs[request.RequestID]; existing != nil {
		s.mu.Unlock()
		replyExisting(existing)
		return
	}
	if !s.attachmentRequestActive(ctx) {
		s.mu.Unlock()
		companionPrototypeFailure(w, 409, "connection_stopped", "Pair again before creating. Your draft is still available.")
		return
	}
	if len(s.prototypeJobs) >= 32 {
		s.mu.Unlock()
		companionPrototypeFailure(w, 429, "job_limit", "Pair again to start more Desktop projects.")
		return
	}
	for _, job := range s.prototypeJobs {
		job.mu.Lock()
		busy := job.status == "running"
		job.mu.Unlock()
		if busy {
			s.mu.Unlock()
			companionPrototypeFailure(w, 409, "creation_running", "A Desktop project is already being created. Open its progress before starting another.")
			return
		}
	}
	jobContext, cancel := context.WithTimeout(s.ctx, companionDesktopPrototypeTimeout)
	job := &companionDesktopPrototypeJob{id: request.RequestID, hash: hash, request: request, status: "running", stage: "Preparing project", started: s.now(), ctx: jobContext, cancel: cancel}
	if s.prototypeJobs == nil {
		s.prototypeJobs = map[string]*companionDesktopPrototypeJob{}
	}
	s.prototypeJobs[job.id] = job
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, job.snapshot(s.now()))
	go s.runDesktopPrototype(job, data)
}

func (s *companionSession) runDesktopPrototype(job *companionDesktopPrototypeJob, data companionChatData) {
	defer job.cancel()
	request := job.request
	parent, identity, err := s.importDestinationParent(request.DestinationID)
	if err != nil {
		s.finishDesktopPrototype(job, "destination_unavailable", "The save folder changed. Choose it again; your draft is still available.")
		return
	}
	run := s.importTemplateCLI
	if run == nil {
		run = runAccountCLI
	}
	// Pin the native picker's folder identity while loading the same Desktop starter.
	parentRoot, err := os.OpenRoot(parent)
	if err == nil {
		defer parentRoot.Close()
		opened, identityErr := parentRoot.Stat(".")
		if identityErr != nil || !os.SameFile(opened, identity) {
			err = errors.New("The selected save folder changed")
		}
	}
	var files []starterProjectFile
	if err == nil {
		files, err = loadStarterProject(job.ctx, request.Name, run)
	}
	if err == nil {
		_, _, err = s.importDestinationParent(request.DestinationID)
	}
	if err == nil {
		err = installStarterProject(job.ctx, parentRoot, request.Name, files)
	}
	root := filepath.Join(parent, request.Name)
	if err != nil {
		s.finishDesktopPrototype(job, "starter_unavailable", "Desktop could not prepare the starter. Check the Glowbom CLI and save folder. Your draft is still available.")
		return
	}
	project := companionProject{ID: companionProjectID(root), Name: request.Name, Available: true, path: root}
	project = companionProjectMetadata(project)
	projectSnapshot := project
	job.mu.Lock()
	job.project, job.projectPath = &projectSnapshot, root
	if job.status == "running" {
		job.stage = "Creating prototype"
	}
	job.mu.Unlock()
	s.shareImportedProject(project)
	// Once the starter exists, keep it available even if generation stops.
	manifest, err := LoadProject(GetProjectPaths(root).Manifest)
	if err == nil {
		manifest.Description = request.Prompt
		manifest.BundleID = suggestProjectBundleID(request.Name)
		err = SaveProject(GetProjectPaths(root).Manifest, manifest)
	}
	if err == nil {
		err = saveSharedChatHistory(root, request.Messages)
	}
	if err == nil {
		_, err = registerStudioProject(root)
	}
	if err != nil {
		s.finishDesktopPrototype(job, "project_save_failed", "The starter is saved, but Desktop could not record the project details. Open its folder to continue.")
		return
	}
	if job.ctx.Err() != nil {
		s.finishDesktopPrototype(job, "canceled", "Creation stopped. The saved Desktop project is still available.")
		return
	}
	// Resolve the model again after preparing the starter; never substitute a default.
	models, err := s.chatModelOptions(job.ctx)
	found := false
	for _, model := range models {
		found = found || model.ID == request.Model && (len(data.images) == 0 || model.Images)
	}
	if err != nil || !found {
		s.finishDesktopPrototype(job, "model_unavailable", "The selected model is no longer connected. The saved project is still available.")
		return
	}
	if request.Images != nil {
		provider, available := s.prototypeImageProvider(job.ctx, request.Images.SourceID)
		if !available {
			s.finishDesktopPrototype(job, "image_source_unavailable", "The selected image source is no longer available. The saved project is still available.")
			return
		}
		job.mu.Lock()
		job.imageProvider = provider
		job.mu.Unlock()
	}
	chat := chatRequest{ProjectPath: root, Model: request.Model, Mode: "prototype", Messages: request.Messages}
	if request.Images != nil {
		chat.Images = &chatImageOptions{SourceID: request.Images.SourceID, Personalization: data.personalizationReference != nil}
		if chat.Images.Personalization {
			chat.Images.ReferencePath = "companion-personalization-reference"
		}
	}
	ctx := context.WithValue(job.ctx, companionChatContextKey{}, data)
	ctx = context.WithValue(ctx, companionPrototypeImageContextKey{}, job)
	writer := &companionDesktopPrototypeWriter{header: make(http.Header), job: job}
	s.api.ServeHTTP(writer, s.internalRequest(ctx, http.MethodPost, "/chat/stream", chat))
	if job.ctx.Err() != nil {
		s.finishDesktopPrototype(job, "canceled", "Creation stopped. Any saved Desktop work is still available.")
		return
	}
	if writer.code >= 400 || !writer.completed {
		s.finishDesktopPrototype(job, "prototype_failed", "Desktop could not finish the prototype. Your draft and any saved project are still available.")
		return
	}
	if !writer.success {
		s.finishDesktopPrototype(job, writer.failureCode, "Desktop could not finish the prototype. Your draft and any saved project are still available.")
		return
	}
	if registered, err := registerStudioProject(root); err == nil {
		project.AssetCount = registered.AssetCount
	}
	s.shareImportedProject(project)
	job.mu.Lock()
	job.project = &project
	if job.status == "running" {
		job.status, job.stage, job.finished = "completed", "Ready", s.now()
	}
	job.mu.Unlock()
}

func (s *companionSession) finishDesktopPrototype(job *companionDesktopPrototypeJob, code, message string) {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.status != "running" {
		return
	}
	job.status, job.stage = "failed", "Creation stopped"
	if job.ctx.Err() != nil {
		job.status, job.stage, code = "canceled", "Stopped", "canceled"
		if errors.Is(job.ctx.Err(), context.DeadlineExceeded) {
			job.status, code = "failed", "chat_timeout"
			message = "Desktop creation reached its 15-minute limit. Your draft and any saved project are still available."
		}
	}
	job.code, job.errorText, job.finished = code, message, s.now()
}

// Send bounded, redacted source as display text. The complete result and private
// reference bytes stay on Desktop while the device polls short requests.
type companionDesktopPrototypeWriter struct {
	mu          sync.Mutex
	header      http.Header
	code        int
	buffer      []byte
	job         *companionDesktopPrototypeJob
	completed   bool
	success     bool
	failureCode string
}

func (w *companionDesktopPrototypeWriter) Header() http.Header { return w.header }
func (w *companionDesktopPrototypeWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code = code
	}
}
func (w *companionDesktopPrototypeWriter) Flush() {}
func (w *companionDesktopPrototypeWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if len(w.buffer)+len(data) > 8<<20 {
		return 0, errors.New("Desktop prototype progress is too large")
	}
	w.buffer = append(w.buffer, data...)
	if !strings.HasPrefix(w.header.Get("Content-Type"), "text/event-stream") {
		return len(data), nil
	}
	for {
		end := bytes.Index(w.buffer, []byte("\n\n"))
		if end < 0 {
			break
		}
		for _, line := range bytes.Split(w.buffer[:end], []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data: ")) {
				w.event(line[6:])
			}
		}
		w.buffer = w.buffer[end+2:]
	}
	return len(data), nil
}

func (w *companionDesktopPrototypeWriter) event(data []byte) {
	var event struct {
		Status       string   `json:"status"`
		Text         string   `json:"text"`
		Done         bool     `json:"done"`
		Success      bool     `json:"success"`
		Code         string   `json:"code"`
		PreviewReady bool     `json:"previewReady"`
		Warning      string   `json:"warning"`
		Warnings     []string `json:"warnings"`
		ImagePrompt  *struct {
			Index  int    `json:"index"`
			Total  int    `json:"total"`
			Prompt string `json:"prompt"`
		} `json:"imagePrompt"`
	}
	if json.Unmarshal(data, &event) != nil || w.completed {
		return
	}
	w.job.mu.Lock()
	defer w.job.mu.Unlock()
	if w.job.status != "running" {
		return
	}
	if event.Text != "" && len(event.Text) <= companionDesktopPrototypeSourceBytes && (!event.Done || event.Success) {
		// Chat events contain the cumulative source, including later image updates.
		w.job.sourceBytes = len(event.Text)
		w.job.sourceExcerpt = companionPrototypeSourceExcerpt(event.Text)
		if !w.job.previewReady && (w.job.stage == "Connecting" || w.job.stage == "Thinking" || w.job.stage == "Creating prototype") {
			w.job.stage = "Writing code"
		}
	}
	if event.Status != "" && w.job.status == "running" {
		w.job.stage = companionPublicRunText(event.Status, 300)
		if event.ImagePrompt == nil {
			w.job.imagePrompt = ""
		}
	}
	w.job.previewReady = w.job.previewReady || event.PreviewReady
	if event.ImagePrompt != nil {
		w.job.imagePrompt = ""
	}
	if event.ImagePrompt != nil && event.ImagePrompt.Index > 0 && event.ImagePrompt.Total <= maxChatImages && event.ImagePrompt.Index <= event.ImagePrompt.Total {
		w.job.imageIndex, w.job.imageTotal = event.ImagePrompt.Index, event.ImagePrompt.Total
		prompt := strings.TrimSpace(event.ImagePrompt.Prompt)
		invalidControl := strings.IndexFunc(prompt, func(character rune) bool {
			return character < 32 && character != '\n' && character != '\t' || character == 127
		}) >= 0
		if w.job.request.Images != nil && w.job.status == "running" && len(prompt) <= 2000 && !invalidControl && !strings.Contains(strings.ToLower(prompt), "data:image") && !strings.Contains(strings.ToLower(prompt), "base64,") {
			w.job.imagePrompt = companionPublicRunText(prompt, 2000)
		}
	}
	warnings := append(event.Warnings, event.Warning)
	for _, warning := range warnings {
		if warning != "" && len(w.job.warnings) < 16 {
			w.job.warnings = append(w.job.warnings, companionPublicRunText(warning, 500))
		}
	}
	if event.Done {
		w.job.imagePrompt = ""
		w.completed, w.success = true, event.Success
		w.failureCode = "prototype_failed"
		switch event.Code {
		case "provider_auth", "opencode_server_auth", "usage_limit", "context_limit", "chat_timeout", "provider_restricted", "provider_error":
			w.failureCode = event.Code
		}
	}
}

var companionPrototypeDataURL = regexp.MustCompile("(?i)data:[^\\s\"'<>`]*")

func companionPrototypeSourceExcerpt(source string) string {
	// Redact before taking the tail so a cut cannot remove a secret's key.
	source = companionPrototypeDataURL.ReplaceAllString(source, "[embedded media]")
	source = companionPublicRunText(source, len(source))
	source = strings.Map(func(character rune) rune {
		if character < 32 && character != '\n' && character != '\r' && character != '\t' || character == 127 {
			return -1
		}
		return character
	}, source)
	if len(source) > companionDesktopPrototypeSourceExcerptBytes {
		source = source[len(source)-companionDesktopPrototypeSourceExcerptBytes:]
		for len(source) > 0 && !utf8.RuneStart(source[0]) {
			source = source[1:]
		}
	}
	return strings.ToValidUTF8(source, "")
}
