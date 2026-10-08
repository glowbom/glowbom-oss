package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	opencode "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"
)

func companionImageFixture(t *testing.T, format string, width, height int) []byte {
	t.Helper()
	var data bytes.Buffer
	pixels := image.NewNRGBA(image.Rect(0, 0, width, height))
	pixels.Set(0, 0, color.NRGBA{R: 250, A: 255})
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&data, pixels, &jpeg.Options{Quality: 80})
	} else {
		err = png.Encode(&data, pixels)
	}
	if err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func companionImageUploadBody(t *testing.T, name string, data []byte) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"filename": name, "dataBase64": base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func uploadCompanionImage(t *testing.T, session *companionSession, project companionProject, name string, data []byte) companionImageAttachment {
	t.Helper()
	w := httptest.NewRecorder()
	session.ServeHTTP(w, companionRequest(session, http.MethodPost, "/projects/"+project.ID+"/attachments", companionImageUploadBody(t, name, data)))
	var attachment companionImageAttachment
	if w.Code != http.StatusCreated || w.Header().Get("Content-Type") != "application/json" || json.Unmarshal(w.Body.Bytes(), &attachment) != nil || attachment.ID == "" {
		t.Fatalf("image upload: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), project.path) || strings.Contains(w.Body.String(), "relativePath") || strings.Contains(w.Body.String(), "digest") {
		t.Fatal("image upload exposed local storage or private state")
	}
	return attachment
}

func TestCompanionImagesPersistExactVerifiedBytesAndSafeNames(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	for _, format := range []string{"jpeg", "png"} {
		data := companionImageFixture(t, format, 4, 3)
		attachment := uploadCompanionImage(t, s, p, `../private\marked up.exe`, data)
		wantName, wantMIME := "marked up.jpg", "image/jpeg"
		if format == "png" {
			wantName, wantMIME = "marked up.png", "image/png"
		}
		if attachment.Filename != wantName || attachment.MimeType != wantMIME || attachment.ProjectID != p.ID || attachment.Width != 4 || attachment.Height != 3 || attachment.ByteCount != int64(len(data)) {
			t.Fatal("image metadata changed", attachment)
		}
		paths, public, err := s.buildAttachments(context.Background(), p, []string{attachment.ID, attachment.ID})
		if err != nil || len(paths) != 1 || len(public) != 1 || !strings.HasPrefix(paths[0], filepath.Join(p.path, ".glowbom", "attachments")) {
			t.Fatal("image did not resolve inside its own project", paths, err)
		}
		stored, err := os.ReadFile(paths[0])
		info, statErr := os.Stat(paths[0])
		if err != nil || statErr != nil || !bytes.Equal(stored, data) || info.Mode().Perm() != 0600 {
			t.Fatal("durable image changed or was not private", err, statErr)
		}
	}
	if len(s.attachments) != 2 || s.uploadsActive != 0 {
		t.Fatal("session did not bound or release upload bookkeeping")
	}
}

func TestCompanionImagesRejectInvalidOversizedAndTruncatedPayloads(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	pngData := companionImageFixture(t, "png", 4, 3)
	for _, test := range []struct {
		name string
		body string
	}{
		{"empty", companionImageUploadBody(t, "image.png", nil)},
		{"not image", companionImageUploadBody(t, "image.png", []byte("<svg><script>ignored</script></svg>"))},
		{"truncated", companionImageUploadBody(t, "image.png", pngData[:len(pngData)-12])},
		{"pixel limit", companionImageUploadBody(t, "image.png", companionImageFixture(t, "png", 2500, 1700))},
		{"edge limit", companionImageUploadBody(t, "image.png", companionImageFixture(t, "png", 4097, 1))},
		{"byte limit", companionImageUploadBody(t, "image.png", make([]byte, companionAttachmentMaxBytes+1))},
		{"invalid base64", `{"dataBase64":"not base64!"}`},
		{"arbitrary path", `{"dataBase64":"AA==","path":"/private/image.png"}`},
		{"arbitrary project", `{"dataBase64":"AA==","projectPath":"/private"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/attachments", test.body))
			if w.Code != 400 || len(s.attachments) != 0 || s.uploadsActive != 0 {
				t.Fatalf("rejected image left state: %d %s", w.Code, w.Body.String())
			}
		})
	}
	if _, err := os.Stat(filepath.Join(p.path, ".glowbom", "attachments")); !os.IsNotExist(err) {
		t.Fatal("rejected images created project files", err)
	}
}

func TestCompanionImagesKeepProjectPairingAndExistingByteOwnership(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	other := sharedCompanionProject(t, s)
	data := companionImageFixture(t, "jpeg", 4, 3)
	attachment := uploadCompanionImage(t, s, p, "reference.jpg", data)
	for _, test := range []struct {
		name    string
		session *companionSession
		project companionProject
		ids     []string
	}{
		{"wrong project", s, other, []string{attachment.ID}},
		{"path instead of handle", s, p, []string{"/private/image.png"}},
		{"unknown", s, p, []string{"unknown"}},
		{"too many", s, p, []string{attachment.ID, attachment.ID, attachment.ID, attachment.ID, attachment.ID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"instructions": "Use image", "attachmentIds": test.ids})
			w := httptest.NewRecorder()
			test.session.ServeHTTP(w, companionRequest(test.session, http.MethodPost, "/projects/"+test.project.ID+"/build", string(body)))
			want := http.StatusGone
			if test.name == "too many" {
				want = http.StatusBadRequest
			}
			if w.Code != want || len(test.session.jobs) != 0 {
				t.Fatalf("invalid handle started work: %d %s", w.Code, w.Body.String())
			}
		})
	}
	fresh := testCompanion(t, http.NotFoundHandler())
	fresh.projects[p.ID] = p
	if _, _, err := fresh.buildAttachments(context.Background(), p, []string{attachment.ID}); !errorsIsAttachmentUnavailable(err) {
		t.Fatal("new pairing imported an old handle", err)
	}
	stored := s.attachments[attachment.ID]
	path := filepath.Join(p.path, stored.relativePath)
	modified := append([]byte{}, data...)
	modified[len(modified)-1] ^= 1
	if err := os.WriteFile(path, modified, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.buildAttachments(context.Background(), p, []string{attachment.ID}); !errorsIsAttachmentUnavailable(err) {
		t.Fatal("build accepted drifted bytes", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "image.jpg")
	if err := os.WriteFile(outside, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.buildAttachments(context.Background(), p, []string{attachment.ID}); !errorsIsAttachmentUnavailable(err) {
		t.Fatal("build accepted a replaced outside-project symlink", err)
	}
}

func errorsIsAttachmentUnavailable(err error) bool { return err == errCompanionAttachmentUnavailable }

func TestCompanionImagesCancelAfterWriteWithoutLeavingFiles(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprintf("revoke=%t", revoke), func(t *testing.T) {
			s := testCompanion(t, http.NotFoundHandler())
			p := sharedCompanionProject(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now, calls := s.now(), 0
			s.now = func() time.Time {
				calls++
				if calls == 4 {
					if revoke {
						s.cancel()
					} else {
						cancel()
					}
				}
				return now
			}
			body := companionImageUploadBody(t, "reference.png", companionImageFixture(t, "png", 4, 3))
			w := httptest.NewRecorder()
			s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/attachments", body).WithContext(ctx))
			entries, err := os.ReadDir(filepath.Join(p.path, ".glowbom", "attachments"))
			if w.Code != http.StatusConflict || err != nil || len(entries) != 0 || len(s.attachments) != 0 || s.uploadBytes != 0 || s.uploadsActive != 0 {
				t.Fatal("canceled upload left partial files or handles", w.Code, calls, entries, err)
			}
		})
	}
}

func TestCompanionImagesRespectSessionAndDirectoryLimits(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	data := companionImageFixture(t, "png", 2, 2)
	for i := 0; i < companionAttachmentLimit; i++ {
		uploadCompanionImage(t, s, p, "image.png", data)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/attachments", companionImageUploadBody(t, "image.png", data)))
	if w.Code != http.StatusTooManyRequests || len(s.attachments) != companionAttachmentLimit {
		t.Fatal("upload count was unbounded", w.Code)
	}
	other := testCompanion(t, http.NotFoundHandler())
	q := sharedCompanionProject(t, other)
	other.uploadBytes = companionAttachmentTotalBytes
	w = httptest.NewRecorder()
	other.ServeHTTP(w, companionRequest(other, http.MethodPost, "/projects/"+q.ID+"/attachments", companionImageUploadBody(t, "image.png", data)))
	if w.Code != http.StatusTooManyRequests || len(other.attachments) != 0 {
		t.Fatal("upload byte reservation was unbounded", w.Code)
	}
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(q.path, ".glowbom"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(q.path, ".glowbom", "attachments")); err != nil {
		t.Fatal(err)
	}
	other.uploadBytes = 0
	w = httptest.NewRecorder()
	other.ServeHTTP(w, companionRequest(other, http.MethodPost, "/projects/"+q.ID+"/attachments", companionImageUploadBody(t, "image.png", data)))
	entries, err := os.ReadDir(outside)
	if w.Code != http.StatusBadRequest || err != nil || len(entries) != 0 || len(other.attachments) != 0 {
		t.Fatal("upload escaped through its storage directory", w.Code, entries, err)
	}
}

func TestCompanionImagesRequirePairingAndAnExplicitlySharedProject(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	body := companionImageUploadBody(t, "image.png", companionImageFixture(t, "png", 2, 2))
	for _, guard := range []string{"missing token", "wrong token", "public peer", "origin", "wrong host", "query", "unshared project", "wrong method"} {
		t.Run(guard, func(t *testing.T) {
			r := companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/attachments", body)
			switch guard {
			case "missing token":
				r.Header.Del("Authorization")
			case "wrong token":
				r.Header.Set("Authorization", "Bearer different-pairing")
			case "public peer":
				r.RemoteAddr = "203.0.113.7:2000"
			case "origin":
				r.Header.Set("Origin", "https://untrusted.example")
			case "wrong host":
				r.Host = "untrusted.example"
			case "query":
				r.URL.RawQuery = "path=/private"
			case "unshared project":
				r.URL.Path = companionPrefix + "/projects/unshared/attachments"
			case "wrong method":
				r.Method = http.MethodPut
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code < 400 || len(s.attachments) != 0 || s.uploadsActive != 0 {
				t.Fatal("untrusted image request persisted state", w.Code)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(p.path, ".glowbom", "attachments")); !os.IsNotExist(err) {
		t.Fatal("untrusted images created project files", err)
	}
	s.cancel()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/attachments", body))
	if w.Code != 401 || len(s.attachments) != 0 {
		t.Fatal("revoked pairing saved an image", w.Code)
	}
}

func TestCompanionImagesUseNormalCursorAttachmentStagingAndHistory(t *testing.T) {
	t.Setenv("GLOWBOM_CURSOR_BIN", fakeCursor(t, `cat > received-prompt.txt
printf '%s\n' '{"type":"result","subtype":"success","result":"Reviewed the drawing"}'
`))
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[{"id":"cursor/auto","name":"Auto","provider":"Cursor"}]}`)
	})
	manager := newCompanionManager(mux)
	mux.HandleFunc("/opencode/refine", manager.guardBuild(openCodeRefineHandler))
	s := testCompanion(t, mux)
	manager.session = s
	p := sharedCompanionProject(t, s)
	data := companionImageFixture(t, "jpeg", 4, 3)
	attachment := uploadCompanionImage(t, s, p, "marked up.jpg", data)
	body, _ := json.Marshal(map[string]any{"model": "cursor/auto", "buildTargets": []string{"prototype"}, "attachmentIds": []string{attachment.ID}})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/build", string(body)))
	var initial struct {
		ID string `json:"id"`
	}
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &initial) != nil {
		t.Fatal("attachment-only Cursor build did not start", w.Code, w.Body.String())
	}
	final := waitCompanionBuild(t, s, initial.ID)
	if final["status"] != "completed" || final["instructions"] != "Use the attached images." || len(final["attachments"].([]companionImageAttachment)) != 1 {
		t.Fatal("normal build lost its uploaded image or fallback", final)
	}
	verifyCompanionStagedImage(t, p.path, attachment, data, "Cursor")
	prompt, err := os.ReadFile(filepath.Join(p.path, "received-prompt.txt"))
	if err != nil || !strings.Contains(string(prompt), "current_instructions/marked up.jpg") || !strings.Contains(string(prompt), "Images may include drawing annotations") {
		t.Fatal("normal Cursor agent did not receive image reference boundary", err)
	}
	encoded, _ := json.Marshal(final)
	if strings.Contains(string(encoded), filepath.Join(p.path, ".glowbom", "attachments")) {
		t.Fatal("job exposed image storage paths")
	}
	s.cancel()
	if _, err := os.Stat(filepath.Join(p.path, s.attachments[attachment.ID].relativePath)); err != nil {
		t.Fatal("accepted durable reference was lost when pairing ended", err)
	}
}

func verifyCompanionStagedImage(t *testing.T, project string, attachment companionImageAttachment, data []byte, contributor string) {
	t.Helper()
	staged, err := os.ReadFile(filepath.Join(project, "current_instructions", attachment.Filename))
	if err != nil || !bytes.Equal(staged, data) {
		t.Fatal("normal build changed the staged image", err)
	}
	entries, err := os.ReadDir(filepath.Join(project, "history"))
	if err != nil || len(entries) != 1 {
		t.Fatal("normal build did not save a single history entry", err, len(entries))
	}
	folder := filepath.Join(project, "history", entries[0].Name())
	archived, err := os.ReadFile(filepath.Join(folder, attachment.Filename))
	if err != nil || !bytes.Equal(archived, data) {
		t.Fatal("normal build changed or omitted archived image", err)
	}
	recordData, err := os.ReadFile(filepath.Join(folder, "entry.json"))
	var record agentHistoryEntryRecord
	if err != nil || json.Unmarshal(recordData, &record) != nil || record.Status != "completed" || record.Contributor != contributor {
		t.Fatal("normal build image history was incomplete", err, string(recordData))
	}
	found := false
	for _, image := range record.Attachments {
		found = found || image.Filename == attachment.Filename && image.MediaType == "screenshot" && image.MimeType == attachment.MimeType && image.FileSizeBytes == attachment.ByteCount
	}
	if !found {
		t.Fatal("normal history lost attachment type or byte metadata")
	}
}

func TestCompanionImagesUseNormalOpenCodeAttachmentStagingAndHistory(t *testing.T) {
	promptSent := make(chan struct{})
	var captured string
	var captureMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			io.WriteString(w, `{"healthy":true}`)
		case "/session":
			io.WriteString(w, `{"id":"ses-fixture","title":"Fixture"}`)
		case "/session/ses-fixture/prompt_async":
			var value struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&value)
			captureMu.Lock()
			if len(value.Parts) > 0 {
				captured = value.Parts[0].Text
			}
			captureMu.Unlock()
			close(promptSent)
			w.WriteHeader(http.StatusNoContent)
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-promptSent:
			case <-r.Context().Done():
				return
			}
			for _, event := range []string{
				`{"type":"message.updated","properties":{"info":{"id":"assistant","role":"assistant","sessionID":"ses-fixture","parts":[{"type":"text","text":"GLOWBOM_STATUS: Reviewed the attached drawing\n"}]}}}`,
				`{"type":"session.idle","properties":{"sessionID":"ses-fixture"}}`,
			} {
				fmt.Fprintf(w, "data: %s\n\n", event)
				w.(http.Flusher).Flush()
			}
		case "/session/ses-fixture/message":
			io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	address, _ := url.Parse(server.URL)
	t.Setenv("OPENCODE_SERVER_HOSTNAME", address.Hostname())
	t.Setenv("GLOWBOM_AGENT_PORT", address.Port())
	t.Setenv("OPENCODE_URL", server.URL)
	previousDriver, previousAuth := openCodeDriver, getOpenCodeOpenAIAuthState()
	openCodeDriver = &OpenCodeDriver{serverURL: server.URL, client: opencode.NewClient(option.WithBaseURL(server.URL))}
	setOpenCodeOpenAIAuthState("opencode-config", "")
	t.Cleanup(func() {
		openCodeDriver = previousDriver
		openCodeOpenAIAuthStateCache.mu.Lock()
		openCodeOpenAIAuthStateCache.state = previousAuth
		openCodeOpenAIAuthStateCache.mu.Unlock()
	})
	mux := http.NewServeMux()
	manager := newCompanionManager(mux)
	mux.HandleFunc("/opencode/refine", manager.guardBuild(openCodeRefineHandler))
	s := testCompanion(t, mux)
	manager.session = s
	p := sharedCompanionProject(t, s)
	data := companionImageFixture(t, "png", 4, 3)
	attachment := uploadCompanionImage(t, s, p, "drawing.png", data)
	body, _ := json.Marshal(map[string]any{"instructions": "Use my drawing", "buildTargets": []string{"prototype"}, "attachmentIds": []string{attachment.ID}})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+p.ID+"/build", string(body)))
	var initial struct {
		ID string `json:"id"`
	}
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &initial) != nil {
		t.Fatal("OpenCode image build did not start", w.Code, w.Body.String())
	}
	final := waitCompanionBuild(t, s, initial.ID)
	if final["status"] != "completed" || final["agentDriver"] != "opencode" || len(final["attachments"].([]companionImageAttachment)) != 1 {
		t.Fatal("normal OpenCode image build failed", final)
	}
	verifyCompanionStagedImage(t, p.path, attachment, data, "OpenCode")
	captureMu.Lock()
	prompt := captured
	captureMu.Unlock()
	if !strings.Contains(prompt, "current_instructions/drawing.png") || !strings.Contains(prompt, "Images may include drawing annotations") {
		t.Fatal("normal OpenCode agent did not receive marked-up image context")
	}
}
