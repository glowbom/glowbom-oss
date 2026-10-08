package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCompanionDesktopPrototypeStreamsBoundedSourceAsDisplayText(t *testing.T) {
	job := &companionDesktopPrototypeJob{status: "running", stage: "Thinking", started: time.Now()}
	writer := &companionDesktopPrototypeWriter{header: make(http.Header), job: job}
	writer.Header().Set("Content-Type", "text/event-stream")
	first := "<!doctype html>\n<html>\n<body>First line</body>"
	body, _ := json.Marshal(map[string]any{"text": first, "reasoning": "private thinking", "reference": "private image"})
	frame := append(append([]byte("data: "), body...), []byte("\n\n")...)
	for _, fragment := range [][]byte{frame[:9], frame[9 : len(frame)-1], frame[len(frame)-1:]} {
		if _, err := writer.Write(fragment); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := job.snapshot(time.Now())
	if snapshot["sourceExcerpt"] != first || snapshot["sourceBytes"] != len(first) || snapshot["stage"] != "Writing code" {
		t.Fatal("fragmented cumulative source was not displayed", snapshot)
	}
	if strings.Contains(fmt.Sprint(snapshot), "private thinking") || strings.Contains(fmt.Sprint(snapshot), "private image") {
		t.Fatal("non-source private event fields were displayed")
	}
	next := first + "\n<footer>More code</footer>\n</html>"
	body, _ = json.Marshal(map[string]any{"text": next})
	writer.event(body)
	if snapshot = job.snapshot(time.Now()); snapshot["sourceExcerpt"] != next || snapshot["sourceBytes"] != len(next) {
		t.Fatal("cumulative source was appended twice", snapshot)
	}
	job.mu.Lock()
	job.status = "canceled"
	job.mu.Unlock()
	body, _ = json.Marshal(map[string]any{"text": "late code", "status": "late status", "previewReady": true})
	writer.event(body)
	if snapshot = job.snapshot(time.Now()); snapshot["sourceExcerpt"] != next || snapshot["previewReady"] != false {
		t.Fatal("a late event changed a stopped build", snapshot)
	}
}

func TestCompanionDesktopPrototypeSourceRedactsBeforeTruncating(t *testing.T) {
	source := "<!doctype html>\n<script>api_key=" + strings.Repeat("private-fixture-", 1000) + "\n</script>\n"
	source += "<img src=\"data:image/png;base64,private-picture\">\n"
	source += strings.Repeat("🌱", 4000) + "\n<footer>Latest visible code</footer>\x1b"
	excerpt := companionPrototypeSourceExcerpt(source)
	if !utf8.ValidString(excerpt) || len(excerpt) > companionDesktopPrototypeSourceExcerptBytes || !strings.HasSuffix(excerpt, "<footer>Latest visible code</footer>") {
		t.Fatal("source tail is invalid or missed the latest code")
	}
	if strings.Contains(excerpt, "private-fixture") || strings.Contains(excerpt, "private-picture") || strings.Contains(excerpt, "data:") || strings.ContainsRune(excerpt, '\x1b') {
		t.Fatal("redacted bytes reappeared after tail truncation")
	}
	short := companionPrototypeSourceExcerpt("<img src=\"data:image/png;base64,private-picture\">\napi_key=private-key\n<div>Visible</div>")
	if strings.Contains(short, "private-") || !strings.Contains(short, "[embedded media]") || !strings.Contains(short, "[redacted]") || !strings.Contains(short, "<div>Visible</div>") {
		t.Fatal("source redaction lost visible code or retained a secret", short)
	}
	if strings.Contains(companionPrototypeSourceExcerpt("<img src=\"data:"), "data:") {
		t.Fatal("an incomplete data URI was not redacted")
	}
}

func TestCompanionDesktopPrototypeSourceRejectsOversizedAndFailedTerminalText(t *testing.T) {
	job := &companionDesktopPrototypeJob{status: "running", stage: "Writing code", started: time.Now()}
	writer := &companionDesktopPrototypeWriter{job: job}
	for _, event := range []map[string]any{
		{"text": strings.Repeat("x", companionDesktopPrototypeSourceBytes+1)},
		{"done": true, "success": false, "text": "private failure body"},
	} {
		body, _ := json.Marshal(event)
		writer.event(body)
	}
	if snapshot := job.snapshot(time.Now()); snapshot["sourceExcerpt"] != nil || snapshot["sourceBytes"] != nil {
		t.Fatal("invalid stream content replaced progress")
	}
}

func desktopPrototypeTestAPI(t *testing.T, run func(http.ResponseWriter, *http.Request, chatRequest)) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"models": []chatModel{
			{ID: "provider/exact", Name: "Exact", Images: true, Build: true},
			{ID: "provider/text", Name: "Text", Images: false, Build: true},
			{ID: "cursor/auto", Name: "Cursor", Build: true},
		}})
	})
	mux.HandleFunc("/opencode/project/icon/sources", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"sources": []projectIconSource{
			{ID: "xai-api", Label: "xAI API", Available: true}, {ID: "openai-api", Label: "OpenAI API", Available: false},
		}})
	})
	mux.HandleFunc("/chat/stream", func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid internal prototype request")
		}
		run(w, r, request)
	})
	return mux
}

func desktopPrototypeTestRequest(t *testing.T, s *companionSession) companionDesktopPrototypeRequest {
	t.Helper()
	grant := companionDestinationFixture(t, s, t.TempDir(), companionTransferTestID)
	return companionDesktopPrototypeRequest{RequestID: companionTransferTestID, DestinationID: grant.ID, Name: "Travel journal", Prompt: "Make a travel journal", Model: "provider/exact"}
}

func desktopPrototypeTestSnapshot(t *testing.T, s *companionSession, id string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "GET", "/prototypes/"+id, ""))
	var value map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &value) != nil {
		t.Fatal("missing prototype snapshot", w.Code, w.Body.String())
	}
	return value
}

func waitDesktopPrototype(t *testing.T, s *companionSession, id string, status string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		value := desktopPrototypeTestSnapshot(t, s, id)
		if value["status"] == status {
			return value
		}
		if value["status"] != "running" {
			t.Fatal("unexpected terminal prototype state", value)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("prototype did not reach", status)
	return nil
}

func desktopPrototypeUpload(t *testing.T, s *companionSession, name, format string, width, height int) companionImageAttachment {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/chat/attachments", companionImageUploadBody(t, name, companionImageFixture(t, format, width, height))))
	var value companionImageAttachment
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &value) != nil {
		t.Fatal("fixture attachment upload", w.Code, w.Body.String())
	}
	return value
}

func TestCompanionDesktopPrototypeCreatesFullStarterAndExactModel(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	var calls atomic.Int32
	s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
		calls.Add(1)
		if request.Mode != "prototype" || request.Model != "provider/exact" || request.ProjectPath == "" || request.Images != nil || len(request.Messages) != 1 {
			t.Error("creation escaped exact Desktop prototype request")
		}
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) < 14*time.Minute || time.Until(deadline) > companionDesktopPrototypeTimeout {
			t.Error("prototype did not have a bounded 15-minute budget")
		}
		html := "<!doctype html><html><head><title>Travel</title></head><body>Travel journal</body></html>"
		if err := saveChatPrototype(request.ProjectPath, html, "", request.Messages, request.Model, nil); err != nil {
			t.Error(err)
		}
		emitCompanionChatTest(w, map[string]any{"previewReady": true, "status": "Writing the story and drawing the sketch"})
		emitCompanionChatTest(w, map[string]any{"done": true, "success": true, "text": html, "projectPath": request.ProjectPath})
	}))
	request := desktopPrototypeTestRequest(t, s)
	w := companionTransferCall(t, s, "/projects/prototype", request)
	if w.Code != http.StatusAccepted {
		t.Fatal("prototype not accepted", w.Code, w.Body.String())
	}
	value := waitDesktopPrototype(t, s, strings.ToLower(request.RequestID), "completed")
	if value["jobId"] != strings.ToLower(request.RequestID) || value["model"] != request.Model || value["projectName"] != request.Name || value["stage"] != "Ready" || value["previewReady"] != true || value["elapsedSeconds"].(float64) < 0 {
		t.Fatal("invalid completed snapshot", value)
	}
	root := value["projectPath"].(string)
	project, ok := s.project(value["projectId"].(string))
	if !ok || project.path != root {
		t.Fatal("created project not explicitly shared", value)
	}
	for _, name := range []string{"glowbom.json", "AGENTS.md", "apple/Custom/ContentView.swift", "android/app/build.gradle.kts", "web/package.json", "prototype/index.html", ".glowbom/chat.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Error("missing full starter or result", name, err)
		}
	}
	if value["sourceExcerpt"] != "<!doctype html><html><head><title>Travel</title></head><body>Travel journal</body></html>" || value["sourceBytes"] != float64(len(value["sourceExcerpt"].(string))) {
		t.Fatal("job snapshot did not retain the actual source for display")
	}
	w = companionTransferCall(t, s, "/projects/prototype", request)
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatal("retry started a second generation", w.Code, calls.Load())
	}
	request.Prompt = "Different idea"
	w = companionTransferCall(t, s, "/projects/prototype", request)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "request_changed") || calls.Load() != 1 {
		t.Fatal("changed retry accepted", w.Code, w.Body.String())
	}
}

func TestCompanionDesktopPrototypeCanvasAndOriginalReferenceStaySeparate(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
		data := companionChatRequestData(r.Context())
		if request.Images == nil || request.Images.SourceID != "xai-api" || !request.Images.Personalization || request.Images.APIKey != "" || request.Images.ReferencePath != "companion-personalization-reference" || data.prototype || len(data.images) != 1 || data.images[0]["filename"] != "annotated.jpg" || data.personalizationReference["filename"] != "original.png" {
			t.Error("canvas and explicit private image-generator reference were mixed")
		}
		if data.images[0]["url"] == data.personalizationReference["url"] {
			t.Error("fixture original became the annotated canvas")
		}
		emitCompanionChatTest(w, map[string]any{"status": "Creating image 1 of 4", "previewReady": true, "imagePrompt": map[string]any{"index": 1, "total": 4, "prompt": "Private fixture scene", "personalized": true}})
		emitCompanionChatTest(w, map[string]any{"warning": "One image source needs reconnecting"})
		emitCompanionChatTest(w, map[string]any{"done": true, "success": true})
	}))
	request := desktopPrototypeTestRequest(t, s)
	canvas := desktopPrototypeUpload(t, s, "annotated.jpg", "jpeg", 4, 3)
	original := desktopPrototypeUpload(t, s, "original.png", "png", 3, 4)
	request.AttachmentID = canvas.ID
	request.Images = &companionDesktopPrototypeImages{SourceID: "xai-api", PersonalizationReferenceID: original.ID}
	w := companionTransferCall(t, s, "/projects/prototype", request)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	value := waitDesktopPrototype(t, s, strings.ToLower(request.RequestID), "completed")
	if value["sourceId"] != "xai-api" || value["imageTotal"] != float64(4) || value["imageIndex"] != float64(1) || value["warnings"] == nil || strings.Contains(fmt.Sprint(value), "data:image") || strings.Contains(fmt.Sprint(value), "Private fixture scene") {
		t.Fatal("missing bounded public progress or private image leaked", value)
	}
}

func TestCompanionDesktopPrototypeSurvivesRequestCancelAndPreservesProject(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	entered, stopped := make(chan struct{}), make(chan struct{})
	s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
		close(entered)
		<-r.Context().Done()
		close(stopped)
	}))
	request := desktopPrototypeTestRequest(t, s)
	body, _ := json.Marshal(request)
	ctx, cancel := context.WithCancel(context.Background())
	r := companionRequest(s, "POST", "/projects/prototype", string(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	cancel()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("prototype did not start after original request ended")
	}
	value := desktopPrototypeTestSnapshot(t, s, strings.ToLower(request.RequestID))
	if value["status"] != "running" || value["project"] == nil {
		t.Fatal("saved starter not available during generation", value)
	}
	root := value["projectPath"].(string)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "DELETE", "/prototypes/"+strings.ToLower(request.RequestID), ""))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop Desktop model")
	}
	value = desktopPrototypeTestSnapshot(t, s, strings.ToLower(request.RequestID))
	if value["status"] != "canceled" || value["project"] == nil || value["code"] != "canceled" {
		t.Fatal("cancel discarded saved starter", value)
	}
	if _, err := os.Stat(filepath.Join(root, "glowbom.json")); err != nil {
		t.Fatal("cancel deleted saved project", err)
	}
	if _, ok := s.project(value["projectId"].(string)); !ok {
		t.Fatal("canceled project not available to open")
	}
}

func TestCompanionDesktopPrototypeRejectsInvalidChoicesBeforeCreating(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	var calls atomic.Int32
	s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
		calls.Add(1)
		t.Error("invalid request executed model")
	}))
	request := desktopPrototypeTestRequest(t, s)
	for _, change := range []func(*companionDesktopPrototypeRequest){
		func(r *companionDesktopPrototypeRequest) { r.Name = "../other" },
		func(r *companionDesktopPrototypeRequest) { r.Name = ".hidden" },
		func(r *companionDesktopPrototypeRequest) {
			r.Prompt = strings.Repeat("x", companionDesktopPrototypePromptBytes+1)
		},
		func(r *companionDesktopPrototypeRequest) { r.Model = "cursor/auto" },
		func(r *companionDesktopPrototypeRequest) { r.Model = "provider/missing" },
		func(r *companionDesktopPrototypeRequest) { r.AttachmentID = "AEFD6EF0-DC32-4DE0-AEE4-9C5FC0933A41" },
		func(r *companionDesktopPrototypeRequest) {
			r.Images = &companionDesktopPrototypeImages{SourceID: "openai-api"}
		},
		func(r *companionDesktopPrototypeRequest) {
			r.Images = &companionDesktopPrototypeImages{SourceID: "xai-api", PersonalizationReferenceID: "not-an-attachment"}
		},
		func(r *companionDesktopPrototypeRequest) { r.DestinationID = "AEFD6EF0-DC32-4DE0-AEE4-9C5FC0933A41" },
	} {
		copy := request
		change(&copy)
		w := companionTransferCall(t, s, "/projects/prototype", copy)
		if w.Code < 400 {
			t.Fatal("invalid choice accepted", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 0 || len(s.prototypeJobs) != 0 || len(s.projects) != 0 {
		t.Fatal("invalid choices created project or job")
	}
	parent, _, _ := s.importDestinationParent(request.DestinationID)
	files, _ := os.ReadDir(parent)
	if len(files) != 0 {
		t.Fatal("invalid choices wrote to chosen folder")
	}
	w := httptest.NewRecorder()
	r := companionRequest(s, "GET", "/projects/prototype", "")
	r.Header.Del("Authorization")
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("prototype capabilities bypassed pairing")
	}
	unknown := map[string]any{"requestId": request.RequestID, "destinationId": request.DestinationID, "name": request.Name, "prompt": request.Prompt, "model": request.Model, "apiKey": "forbidden"}
	w = companionTransferCall(t, s, "/projects/prototype", unknown)
	if w.Code != 400 {
		t.Fatal("phone supplied credentials")
	}
}

func TestCompanionDesktopPrototypeForeignAttachmentsAndTextModel(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
		if request.Model != "provider/text" || len(companionChatRequestData(r.Context()).images) != 0 || request.Images == nil || !request.Images.Personalization {
			t.Error("generator-only reference required text-model vision")
		}
		emitCompanionChatTest(w, map[string]any{"done": true, "success": true})
	}))
	request := desktopPrototypeTestRequest(t, s)
	p := sharedCompanionProject(t, s)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/projects/"+p.ID+"/attachments", companionImageUploadBody(t, "other.png", companionImageFixture(t, "png", 2, 2))))
	var foreign companionImageAttachment
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &foreign) != nil {
		t.Fatal("foreign fixture", w.Code)
	}
	request.AttachmentID = foreign.ID
	w = companionTransferCall(t, s, "/projects/prototype", request)
	if w.Code != 410 {
		t.Fatal("foreign-project attachment accepted", w.Code, w.Body.String())
	}
	original := desktopPrototypeUpload(t, s, "original.png", "png", 2, 3)
	request.Model = "provider/text"
	request.AttachmentID = original.ID
	w = companionTransferCall(t, s, "/projects/prototype", request)
	if w.Code != 400 {
		t.Fatal("text model accepted canvas", w.Code, w.Body.String())
	}
	request.AttachmentID = ""
	request.Images = &companionDesktopPrototypeImages{SourceID: "xai-api", PersonalizationReferenceID: original.ID}
	w = companionTransferCall(t, s, "/projects/prototype", request)
	if w.Code != 202 {
		t.Fatal("text model rejected generator-only reference", w.Code, w.Body.String())
	}
	waitDesktopPrototype(t, s, strings.ToLower(request.RequestID), "completed")
}

func TestCompanionDesktopPrototypeFailureKeepsProjectAndStructuredCode(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
		if err := saveChatPrototype(request.ProjectPath, "<!doctype html><html><body>Saved preview</body></html>", "", request.Messages, request.Model, nil); err != nil {
			t.Error(err)
		}
		emitCompanionChatTest(w, map[string]any{"previewReady": true})
		emitCompanionChatTest(w, map[string]any{"done": true, "success": false, "code": "usage_limit", "error": "mock provider private details"})
	}))
	request := desktopPrototypeTestRequest(t, s)
	w := companionTransferCall(t, s, "/projects/prototype", request)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	value := waitDesktopPrototype(t, s, strings.ToLower(request.RequestID), "failed")
	if value["project"] == nil || value["previewReady"] != true || value["code"] != "usage_limit" || strings.Contains(fmt.Sprint(value), "private details") {
		t.Fatal("failure lost project or leaked raw provider details", value)
	}
	if _, err := os.Stat(filepath.Join(value["projectPath"].(string), "glowbom.json")); err != nil {
		t.Fatal("failure removed starter", err)
	}
	if _, err := os.Stat(filepath.Join(value["projectPath"].(string), "prototype/index.html")); err != nil {
		t.Fatal("failure removed the saved preview", err)
	}
}

func TestCompanionDesktopPrototypeRealHandlerKeepsOriginalOutOfModel(t *testing.T) {
	isolateProjectIconCredentials(t)
	withCodexModels(t, nil)
	t.Setenv("OPENCODE_URL", "http://fixture")
	t.Setenv("OPENAI_API_KEY", "image-key")
	canvasBytes := companionImageFixture(t, "jpeg", 4, 3)
	originalBytes := companionImageFixture(t, "png", 3, 4)
	expectedReference, err := normalizeProjectIconReference(base64.StdEncoding.EncodeToString(originalBytes))
	if err != nil {
		t.Fatal(err)
	}
	var modelCalls, imageCalls atomic.Int32
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.openai.com" {
			imageCalls.Add(1)
			reference, _ := chatPersonalizationReference(t, r, "openai-api", true)
			if reference != expectedReference {
				t.Error("image generator lost the exact original reference")
			}
			return iconProviderResponse(projectIconTestImage(t, "png")), nil
		}
		if r.URL.Host != "fixture" {
			return nil, fmt.Errorf("unexpected mock-only host")
		}
		switch r.URL.Path {
		case "/provider":
			if r.URL.Query().Get("directory") != "/isolated" {
				return chatImageReply(`{"connected":[],"all":[]}`), nil
			}
			return chatImageReply(`{"connected":["test"],"all":[{"id":"test","models":{"selected":{"name":"Selected","capabilities":{"input":{"image":true}}}}}]}`), nil
		case "/session":
			return chatImageReply(`{"id":"desktop-prototype","permission":[{"permission":"*","pattern":"*","action":"deny"}]}`), nil
		case "/experimental/tool/ids":
			return chatImageReply(`["bash","read","edit"]`), nil
		case "/event":
			return chatImageReply("data: {}\n\n"), nil
		case "/session/desktop-prototype/message":
			modelCalls.Add(1)
			body, _ := io.ReadAll(r.Body)
			var request struct {
				Parts []map[string]any `json:"parts"`
				Tools map[string]bool  `json:"tools"`
			}
			if json.Unmarshal(body, &request) != nil || len(request.Parts) != 2 || request.Parts[1]["type"] != "file" || request.Parts[1]["filename"] != "canvas.jpg" {
				t.Error("model did not receive exactly the separate design canvas")
			}
			if bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(originalBytes))) || bytes.Contains(body, []byte("original.png")) || bytes.Contains(body, []byte("image-key")) {
				t.Error("private original reference or image credential reached text model")
			}
			if !bytes.Contains(body, []byte("provided only to the image generator")) || !bytes.Contains(body, []byte("Image personalization is enabled")) {
				t.Error("model lost accurate personalization planning instructions")
			}
			for _, name := range []string{"*", "bash", "read", "edit"} {
				if enabled, ok := request.Tools[name]; !ok || enabled {
					t.Error("prototype model was allowed to use tools", name)
				}
			}
			return chatImageReply(`{"info":{},"parts":[{"type":"text","text":"<!doctype html><html><body><img src=\"glowbomimages:The person from the reference photo exploring the seaside\"></body></html>"}]}`), nil
		default:
			return chatImageReply(`true`), nil
		}
	})
	previousTransport := http.DefaultTransport
	http.DefaultTransport = http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	service := &chatService{uploads: t.TempDir(), directory: "/isolated", serverURL: "http://fixture", client: http.DefaultClient, prepare: func() error { return nil }}
	api := http.NewServeMux()
	api.HandleFunc("/chat/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"models": []chatModel{{ID: "test/selected", Name: "Selected", Images: true, Build: true}}})
	})
	api.HandleFunc("/opencode/project/icon/sources", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"sources": []projectIconSource{{ID: "openai-api", Available: true}}})
	})
	api.HandleFunc("/chat/stream", service.streamHandler)
	s := testCompanion(t, api)
	request := desktopPrototypeTestRequest(t, s)
	request.Model = "test/selected"
	canvas := desktopPrototypeUpload(t, s, "canvas.jpg", "jpeg", 4, 3)
	original := desktopPrototypeUpload(t, s, "original.png", "png", 3, 4)
	request.AttachmentID = canvas.ID
	request.Images = &companionDesktopPrototypeImages{SourceID: "openai-api", PersonalizationReferenceID: original.ID}
	w := companionTransferCall(t, s, "/projects/prototype", request)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	value := waitDesktopPrototype(t, s, strings.ToLower(request.RequestID), "completed")
	if modelCalls.Load() != 1 || imageCalls.Load() != 1 || value["previewReady"] != true {
		t.Fatal("real Desktop handler did not create prototype and personalized image", value, modelCalls.Load(), imageCalls.Load())
	}
	progress, _ := json.Marshal(value["generatedImages"])
	var generated []companionPrototypeImage
	if json.Unmarshal(progress, &generated) != nil || len(generated) != 1 || normalizedStudioUUID(generated[0].ID) == "" || generated[0].Prompt != "The person from the reference photo exploring the seaside" {
		t.Fatal("real Desktop image pipeline did not publish its saved image", string(progress))
	}
	root := value["projectPath"].(string)
	saved, err := os.ReadFile(filepath.Join(root, "prototype/index.html"))
	if err != nil || bytes.Contains(saved, []byte("glowbomimages:")) || !bytes.Contains(saved, []byte("assets/glowbom-image-")) {
		t.Fatal("generated image was not linked to saved Desktop prototype", string(saved), err)
	}
	canvasFile, err := os.ReadFile(filepath.Join(root, "prototype/assets/canvas.jpg"))
	if err != nil || !bytes.Equal(canvasFile, canvasBytes) {
		t.Fatal("design input was not preserved", err)
	}
	if _, err := os.Stat(filepath.Join(root, "prototype/assets/original.png")); !os.IsNotExist(err) {
		t.Fatal("private generator-only original became a page asset", err)
	}
}

func TestCompanionDesktopPrototypeDestinationReplacementStopsBeforeWrite(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	var modelCalls atomic.Int32
	s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
		modelCalls.Add(1)
		t.Error("changed destination reached model")
	}))
	request := desktopPrototypeTestRequest(t, s)
	parent, _, err := s.importDestinationParent(request.DestinationID)
	if err != nil {
		t.Fatal(err)
	}
	moved := parent + "-original"
	s.importTemplateCLI = starterFixtureRunner(t, func(string) error {
		if err := os.Rename(parent, moved); err != nil {
			return err
		}
		return os.Mkdir(parent, 0700)
	})
	w := companionTransferCall(t, s, "/projects/prototype", request)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	value := waitDesktopPrototype(t, s, strings.ToLower(request.RequestID), "failed")
	if modelCalls.Load() != 0 || value["project"] != nil {
		t.Fatal("replaced destination created a project", value)
	}
	for _, root := range []string{parent, moved} {
		files, err := os.ReadDir(root)
		if err != nil || len(files) != 0 {
			t.Fatal("creation wrote to the changed folder", root, files, err)
		}
	}
}

func TestCompanionDesktopPrototypeConcurrentRetryCreatesOneProject(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	preflights := make(chan struct{}, 2)
	release := make(chan struct{})
	var catalogCalls, modelCalls atomic.Int32
	api := http.NewServeMux()
	api.HandleFunc("/chat/models", func(w http.ResponseWriter, r *http.Request) {
		if catalogCalls.Add(1) <= 2 {
			preflights <- struct{}{}
			<-release
		}
		writeJSON(w, map[string]any{"models": []chatModel{{ID: "provider/exact", Name: "Exact", Images: true, Build: true}}})
	})
	api.HandleFunc("/chat/stream", func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		emitCompanionChatTest(w, map[string]any{"done": true, "success": true})
	})
	s := testCompanion(t, api)
	request := desktopPrototypeTestRequest(t, s)
	body, _ := json.Marshal(request)
	responses := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() {
			response := httptest.NewRecorder()
			s.ServeHTTP(response, companionRequest(s, "POST", "/projects/prototype", string(body)))
			responses <- response
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-preflights:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("concurrent requests did not enter preflight")
		}
	}
	close(release)
	accepted := 0
	for i := 0; i < 2; i++ {
		select {
		case response := <-responses:
			if response.Code == 202 {
				accepted++
			} else if response.Code != 200 {
				t.Fatal("same concurrent retry was rejected", response.Code, response.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent creation did not return")
		}
	}
	waitDesktopPrototype(t, s, strings.ToLower(request.RequestID), "completed")
	if accepted != 1 || modelCalls.Load() != 1 {
		t.Fatal("concurrent retry started multiple jobs", accepted, modelCalls.Load())
	}
	parent, _, _ := s.importDestinationParent(request.DestinationID)
	children, err := os.ReadDir(parent)
	if err != nil || len(children) != 1 || children[0].Name() != request.Name {
		t.Fatal("concurrent retry duplicated the folder", children, err)
	}
}

func TestCompanionDesktopPrototypePairingRevocationStopsGeneration(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	entered, stopped := make(chan struct{}), make(chan struct{})
	s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
		close(entered)
		<-r.Context().Done()
		close(stopped)
	}))
	request := desktopPrototypeTestRequest(t, s)
	response := companionTransferCall(t, s, "/projects/prototype", request)
	if response.Code != http.StatusAccepted {
		t.Fatal(response.Code, response.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("prototype did not start")
	}
	s.mu.Lock()
	job := s.prototypeJobs[strings.ToLower(request.RequestID)]
	s.mu.Unlock()
	s.cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("pairing revocation did not cancel generation")
	}
	deadline := time.Now().Add(time.Second)
	var snapshot map[string]any
	for time.Now().Before(deadline) {
		snapshot = job.snapshot(s.now())
		if snapshot["status"] != "running" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if snapshot["status"] != "canceled" || snapshot["project"] == nil || snapshot["code"] != "canceled" {
		t.Fatal("revocation discarded saved starter or left job running", snapshot)
	}
	if _, err := os.Stat(filepath.Join(snapshot["projectPath"].(string), "glowbom.json")); err != nil {
		t.Fatal("revocation removed saved project", err)
	}
	response = httptest.NewRecorder()
	s.ServeHTTP(response, companionRequest(s, "GET", "/prototypes/"+job.id, ""))
	if response.Code != http.StatusUnauthorized {
		t.Fatal("revoked pairing could still read its job", response.Code)
	}
}

func TestCompanionDesktopPrototypeKeepsConversationAndOneCurrentPrompt(t *testing.T) {
	for _, promptAlreadyIncluded := range []bool{false, true} {
		t.Run(fmt.Sprintf("promptAlreadyIncluded=%v", promptAlreadyIncluded), func(t *testing.T) {
			t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
			history := []chatMessage{
				{Role: "user", Text: "Make an offline travel journal.\n" + strings.Repeat("Saved planning context. ", 4000)},
				{Role: "assistant", Text: "It can save trips and photos.", Model: "provider/exact", Reasoning: "Fixture planning", WorkedSeconds: 4},
				{Role: "user", Text: "Keep the blue map and large buttons."},
				{Role: "assistant", Text: "The journal will use a blue map and large buttons."},
			}
			expected := append(append([]chatMessage{}, history...), chatMessage{Role: "user", Text: "Make a travel journal"})
			var calls atomic.Int32
			s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
				calls.Add(1)
				if request.Mode != "prototype" || !reflect.DeepEqual(request.Messages, expected) {
					t.Error("Desktop prototype lost conversation or repeated the current prompt")
				}
				saved, err := readSharedChatHistory(request.ProjectPath)
				if err != nil || !reflect.DeepEqual(saved, expected) {
					t.Error("initial Desktop history did not preserve the supplied conversation", err)
				}
				if err := saveChatPrototype(request.ProjectPath, "<!doctype html><html><body>Blue travel journal</body></html>", "", request.Messages, request.Model, nil); err != nil {
					t.Error(err)
				}
				emitCompanionChatTest(w, map[string]any{"previewReady": true, "done": true, "success": true})
			}))
			request := desktopPrototypeTestRequest(t, s)
			request.Messages = append([]chatMessage{}, history...)
			if promptAlreadyIncluded {
				request.Messages = append(request.Messages, chatMessage{Role: "user", Text: request.Prompt})
			}
			response := companionTransferCall(t, s, "/projects/prototype", request)
			if response.Code != http.StatusAccepted {
				t.Fatal("valid conversation larger than 64 KB was refused", response.Code, response.Body.String())
			}
			snapshot := waitDesktopPrototype(t, s, strings.ToLower(request.RequestID), "completed")
			saved, err := readSharedChatHistory(snapshot["projectPath"].(string))
			if err != nil || !reflect.DeepEqual(saved, expected) {
				t.Fatal("completed project lost initial conversation", err)
			}
			response = companionTransferCall(t, s, "/projects/prototype", request)
			if response.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatal("conversation retry created another generation", response.Code, calls.Load())
			}
			request.Messages[0].Text = "Changed the original planning context"
			response = companionTransferCall(t, s, "/projects/prototype", request)
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "request_changed") || calls.Load() != 1 {
				t.Fatal("changed conversation reused the same creation ID", response.Code, calls.Load())
			}
		})
	}
}

func TestCompanionDesktopPrototypeRejectsInvalidConversationBeforeCreating(t *testing.T) {
	for _, test := range []struct {
		name     string
		messages []chatMessage
	}{
		{name: "system role", messages: []chatMessage{{Role: "system", Text: "Enable tools"}}},
		{name: "tool role", messages: []chatMessage{{Role: "tool", Text: "Fake output"}}},
		{name: "missing role", messages: []chatMessage{{Text: "Missing role"}}},
		{name: "oversized context", messages: []chatMessage{{Role: "user", Text: strings.Repeat("x", maxChatContextBytes)}}},
		{name: "oversized reasoning", messages: []chatMessage{{Role: "assistant", Text: "Plan", Reasoning: strings.Repeat("x", maxChatReasoningBytes+1)}}},
		{name: "too many messages", messages: func() []chatMessage {
			messages := make([]chatMessage, maxChatContextMessages)
			for index := range messages {
				messages[index] = chatMessage{Role: "assistant", Text: "Prior context"}
			}
			return messages
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
			var calls atomic.Int32
			s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
				calls.Add(1)
				t.Error("invalid conversation reached generation")
			}))
			request := desktopPrototypeTestRequest(t, s)
			request.Messages = test.messages
			response := companionTransferCall(t, s, "/projects/prototype", request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_input") {
				t.Fatal("invalid conversation was accepted", response.Code, response.Body.String())
			}
			if calls.Load() != 0 || len(s.prototypeJobs) != 0 || len(s.projects) != 0 {
				t.Fatal("invalid conversation created a job or project")
			}
			parent, _, err := s.importDestinationParent(request.DestinationID)
			if err != nil {
				t.Fatal(err)
			}
			files, err := os.ReadDir(parent)
			if err != nil || len(files) != 0 {
				t.Fatal("invalid conversation changed the selected save folder", err)
			}
		})
	}
}

func TestCompanionDesktopPrototypeReportsOnlyCurrentPublicImageProgress(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	imageStarted, bookStarted := make(chan struct{}), make(chan struct{})
	next, finish := make(chan struct{}), make(chan struct{})
	s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
		emitCompanionChatTest(w, map[string]any{"status": "Creating image 1 of 2", "previewReady": true,
			"imagePrompt": map[string]any{"index": 1, "total": 2, "prompt": "The reference character exploring the seaside",
				"provider": "untrusted provider field", "referenceImage": "data:image/png;base64,private-fixture"}})
		close(imageStarted)
		select {
		case <-next:
		case <-r.Context().Done():
			return
		}
		emitCompanionChatTest(w, map[string]any{"status": "Writing the story and drawing the sketch"})
		close(bookStarted)
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		emitCompanionChatTest(w, map[string]any{"done": true, "success": true})
	}))
	request := desktopPrototypeTestRequest(t, s)
	request.Images = &companionDesktopPrototypeImages{SourceID: "xai-api"}
	response := companionTransferCall(t, s, "/projects/prototype", request)
	if response.Code != http.StatusAccepted {
		t.Fatal(response.Code, response.Body.String())
	}
	select {
	case <-imageStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("image generation did not start")
	}
	snapshot := desktopPrototypeTestSnapshot(t, s, strings.ToLower(request.RequestID))
	if snapshot["imagePrompt"] != "The reference character exploring the seaside" || snapshot["imageProvider"] != "xAI API" || snapshot["stage"] != "Creating image 1 of 2" || snapshot["imageIndex"] != float64(1) || snapshot["imageTotal"] != float64(2) {
		t.Fatal("current image progress did not match Desktop", snapshot)
	}
	if strings.Contains(fmt.Sprint(snapshot), "private-fixture") || strings.Contains(fmt.Sprint(snapshot), "untrusted provider") {
		t.Fatal("image progress exposed private payload or unverified provider", snapshot)
	}
	close(next)
	select {
	case <-bookStarted:
	case <-time.After(time.Second):
		t.Fatal("post-image stage did not start")
	}
	snapshot = desktopPrototypeTestSnapshot(t, s, strings.ToLower(request.RequestID))
	if snapshot["imagePrompt"] != nil || snapshot["imageProvider"] != nil || snapshot["stage"] != "Writing the story and drawing the sketch" {
		t.Fatal("finished image remained shown as current", snapshot)
	}
	close(finish)
	snapshot = waitDesktopPrototype(t, s, strings.ToLower(request.RequestID), "completed")
	if snapshot["imagePrompt"] != nil || snapshot["imageProvider"] != nil || snapshot["imageTotal"] != float64(2) {
		t.Fatal("terminal snapshot lost count or showed active image work", snapshot)
	}
}

func TestCompanionDesktopPrototypeImageProgressRejectsPayloadsAndRedactsSecrets(t *testing.T) {
	job := &companionDesktopPrototypeJob{id: strings.ToLower(companionTransferTestID), request: companionDesktopPrototypeRequest{
		RequestID: strings.ToLower(companionTransferTestID), Images: &companionDesktopPrototypeImages{SourceID: "xai-api"}},
		status: "running", stage: "Creating image", started: time.Now(), imageProvider: "xAI API"}
	writer := &companionDesktopPrototypeWriter{job: job}
	for _, prompt := range []string{"data:image/png;base64,private", "base64,private", "Control\x1btext", strings.Repeat("x", 2001), " "} {
		body, _ := json.Marshal(map[string]any{"imagePrompt": map[string]any{"index": 1, "total": 2, "prompt": prompt}})
		writer.event(body)
		if snapshot := job.snapshot(time.Now()); snapshot["imagePrompt"] != nil || snapshot["imageProvider"] != nil {
			t.Fatal("invalid image description was exposed", snapshot)
		}
	}
	body, _ := json.Marshal(map[string]any{"imagePrompt": map[string]any{"index": 1, "total": 2, "prompt": "Seaside, api_key=private-fixture-secret"}})
	writer.event(body)
	if snapshot := job.snapshot(time.Now()); snapshot["imagePrompt"] == nil || strings.Contains(fmt.Sprint(snapshot["imagePrompt"]), "private-fixture-secret") {
		t.Fatal("image description was not retained with credentials redacted", snapshot)
	}
}
