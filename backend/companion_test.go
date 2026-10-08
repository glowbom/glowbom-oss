package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCompanion(t *testing.T, api http.Handler) *companionSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &companionSession{host: "192.168.1.2:4443", expires: time.Now().Add(time.Hour),
		pairing: companionPairing{Token: strings.Repeat("a", 64)}, projects: map[string]companionProject{},
		jobs: map[string]*companionJob{}, api: api, now: time.Now, ctx: ctx, cancel: cancel,
		importTemplateCLI: starterFixtureRunner(t, nil)}
}

func companionRequest(s *companionSession, method, path, body string) *http.Request {
	r := httptest.NewRequest(method, "https://"+s.host+companionPrefix+path, strings.NewReader(body))
	r.RemoteAddr = "192.168.1.3:50000"
	r.Header.Set("Authorization", "Bearer "+s.pairing.Token)
	return r
}

func TestCompanionCertificatePin(t *testing.T) {
	now := time.Now()
	cert, pin, err := companionCertificate("192.168.1.2", now, now.Add(companionLifetime))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || leaf.VerifyHostname("192.168.1.2") != nil || leaf.NotAfter.Sub(now) > companionLifetime {
		t.Fatal("certificate does not have the selected address and bounded lifetime")
	}
	for _, valid := range []bool{true, false} {
		serverSide, clientSide := net.Pipe()
		server := tls.Server(serverSide, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
		client := tls.Client(clientSide, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13,
			VerifyConnection: func(state tls.ConnectionState) error {
				digest := sha256.Sum256(state.PeerCertificates[0].Raw)
				if !valid || hex.EncodeToString(digest[:]) != pin {
					return errors.New("certificate pin mismatch")
				}
				return nil
			}})
		_ = server.SetDeadline(time.Now().Add(time.Second))
		_ = client.SetDeadline(time.Now().Add(time.Second))
		serverDone := make(chan error, 1)
		go func() { serverDone <- server.Handshake() }()
		err := client.Handshake()
		_ = clientSide.Close()
		_ = serverSide.Close()
		<-serverDone
		if valid && err != nil {
			t.Fatal(err)
		}
		if !valid && err == nil {
			t.Fatal("wrong pin accepted")
		}
	}
}

func TestCompanionSecurityScope(t *testing.T) {
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("non-allowlisted handler reached") }))
	for _, test := range []struct {
		name, path string
		change     func(*http.Request)
		want       int
	}{
		{"token", "/projects", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"wrong token", "/projects", func(r *http.Request) { r.Header.Set("Authorization", "Bearer other") }, 401},
		{"origin", "/projects", func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }, 403},
		{"host", "/projects", func(r *http.Request) { r.Host = "attacker.example" }, 403},
		{"public peer", "/projects", func(r *http.Request) { r.RemoteAddr = "8.8.8.8:50000" }, 403},
		{"query token", "/projects?token=private", nil, 403},
		{"encoded traversal", "/projects/%2e%2e/settings", nil, 403},
		{"settings", "/settings/providers/status", nil, 404},
		{"permissions", "/opencode/permission/respond", nil, 404},
		{"unknown project", "/projects/not-shared/history", nil, 404},
		{"arbitrary path", "/projects/../../tmp/history", nil, 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := companionRequest(s, http.MethodGet, test.path, "")
			if test.change != nil {
				test.change(r)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != test.want {
				t.Fatalf("status=%d want=%d", w.Code, test.want)
			}
		})
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects", ""))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.expires = time.Now().Add(-time.Second)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects", ""))
	if w.Code != 401 {
		t.Fatal("expired pairing accepted")
	}
	s.expires = time.Now().Add(time.Hour)
	s.close()
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects", ""))
	if w.Code != 401 {
		t.Fatal("revoked pairing accepted")
	}
}

func TestCompanionLocalControls(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	m := newCompanionManager(http.NotFoundHandler())
	m.interfaces = func() []companionInterface { return []companionInterface{{Address: "192.168.1.2", Name: "test"}} }
	for _, test := range []struct {
		peer, token, body string
		method            string
		want              int
	}{
		{"192.168.1.3:1000", "desktop-token", "", http.MethodGet, 403},
		{"127.0.0.1:1000", "", "", http.MethodGet, 401},
		{"127.0.0.1:1000", "desktop-token", "", http.MethodGet, 200},
		{"127.0.0.1:1000", "desktop-token", `{"address":"0.0.0.0"}`, http.MethodPost, 400},
		{"127.0.0.1:1000", "desktop-token", `{"address":"192.168.4.8"}`, http.MethodPost, 400},
		{"127.0.0.1:1000", "desktop-token", `{"address":"192.168.1.2","apiKey":"private"}`, http.MethodPost, 400},
		{"127.0.0.1:1000", "desktop-token", `{"address":"192.168.1.2"} {}`, http.MethodPost, 400},
	} {
		r := httptest.NewRequest(test.method, "http://127.0.0.1/companion", strings.NewReader(test.body))
		r.RemoteAddr = test.peer
		r.Header.Set("Authorization", "Bearer "+test.token)
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatalf("%s %s status=%d want=%d", test.method, test.peer, w.Code, test.want)
		}
	}
}

func TestCompanionSharedProjectHistory(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	if err := SaveProject(filepath.Join(root, "glowbom.json"), &GlowbomProject{Name: "Shared", Version: "1.0.0", Targets: map[string]Target{}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "history", "entry"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "history", "entry", "entry.json"), []byte(`{"id":"run","instructions":"Build a clock","status":"complete"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".glowbom"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".glowbom", "chat.json"), []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "entry.json"), []byte(`{"id":"secret","instructions":"should not be read"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "history", "outside")); err != nil {
		t.Fatal(err)
	}
	s := testCompanion(t, http.NotFoundHandler())
	id := companionProjectID(root)
	s.projects[id] = companionProject{ID: id, Name: "Shared", Available: true, path: root}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/"+id+"/history", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Build a clock") || strings.Contains(w.Body.String(), "should not be read") || strings.Contains(w.Body.String(), root) {
		t.Fatalf("unsafe history response: %s", w.Body.String())
	}
	var history struct {
		Messages []chatMessage `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &history); err != nil || history.Messages == nil {
		t.Fatal("empty saved conversation must use an array for the iOS client")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+id+"/build", `{"instructions":"Do it","projectPath":"/private"}`))
	if w.Code != 400 {
		t.Fatal("arbitrary project path accepted")
	}
}

func TestCompanionAsynchronousBuildAndApproval(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	if err := SaveProject(filepath.Join(root, "glowbom.json"), &GlowbomProject{Name: "Shared", Targets: map[string]Target{}}); err != nil {
		t.Fatal(err)
	}
	started := make(chan OpenCodeAgentRequest, 1)
	finish := make(chan struct{})
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/opencode/refine" || r.Header.Get("Authorization") != "Bearer desktop-token" {
			t.Error("unexpected internal route or authorization")
		}
		var payload OpenCodeAgentRequest
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Error("invalid payload")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"permission\":{\"id\":\"permission-one\",\"sessionID\":\"session\",\"title\":\"Write files\"}}\n\n")
		started <- payload
		select {
		case <-finish:
			_, _ = io.WriteString(w, "data: {\"done\":true,\"success\":true}\n\n")
		case <-r.Context().Done():
		}
	}))
	id := companionProjectID(root)
	s.projects[id] = companionProject{ID: id, Name: "Shared", Available: true, path: root}
	phoneCtx, cancelPhone := context.WithCancel(context.Background())
	r := companionRequest(s, http.MethodPost, "/projects/"+id+"/build", `{"instructions":"Build a clock"}`).WithContext(phoneCtx)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	cancelPhone()
	select {
	case payload := <-started:
		if payload.ProjectPath != root || payload.OpenAIAuthMode != "opencode-config" || payload.MediaGenerationPolicy != "skip" || payload.OpenAIKey != "" {
			t.Fatal("unsafe build payload")
		}
	case <-time.After(time.Second):
		t.Fatal("build never started")
	}
	job := s.jobs[result.ID]
	list := httptest.NewRecorder()
	s.ServeHTTP(list, companionRequest(s, http.MethodGet, "/builds", ""))
	if list.Code != 200 || !strings.Contains(list.Body.String(), result.ID) || strings.Contains(list.Body.String(), root) || strings.Contains(list.Body.String(), "sessionID") {
		t.Fatal("could not restore scoped jobs after reconnecting")
	}
	remote := job.snapshot(false)
	local := job.snapshot(true)
	if remote["status"] != "waiting" || remote["pendingPermission"] == nil || remote["projectPath"] != nil || local["pendingPermission"] == nil {
		t.Fatal("scoped approval must be actionable on phone without exposing its session or project path")
	}
	if job.acknowledge("permission", "wrong") || !job.acknowledge("permission", "permission-one") {
		t.Fatal("approval acknowledgement did not match exact request")
	}
	close(finish)
	deadline := time.Now().Add(time.Second)
	for job.snapshot(false)["status"] != "completed" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if job.snapshot(false)["status"] != "completed" {
		t.Fatal("phone disconnect interrupted asynchronous build")
	}
}

func TestCompanionCancelStopsWorker(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(stopped) }))
	w := httptest.NewRecorder()
	s.startJob(w, httptest.NewRequest(http.MethodPost, "/", nil), "build", companionProject{}, "/opencode/refine", nil)
	var result struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	w = httptest.NewRecorder()
	s.jobHandler(w, httptest.NewRequest(http.MethodDelete, "/", nil), result.ID, "build")
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop worker")
	}
	if s.jobs[result.ID].snapshot(false)["status"] != "canceled" {
		t.Fatal("cancel reported success")
	}
}

func TestCompanionCanceledPendingJobCannotBeApproved(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("canceled request reached the permission handler")
	}))
	ctx, cancel := context.WithCancel(s.ctx)
	job := &companionJob{id: "job", kind: "build", projectPath: "/shared", status: "waiting", output: []string{},
		pendingPermission: json.RawMessage(`{"id":"permission-one","sessionID":"session"}`),
		pendingQuestion:   json.RawMessage(`{"id":"question-one","sessionID":"session"}`), ctx: ctx, cancel: cancel}
	s.jobs[job.id] = job
	m := newCompanionManager(s.api)
	m.session = s
	w := httptest.NewRecorder()
	s.jobHandler(w, httptest.NewRequest(http.MethodDelete, "/", nil), job.id, "build")
	if ctx.Err() == nil || job.snapshot(true)["pendingPermission"] != nil || job.snapshot(true)["pendingQuestion"] != nil {
		t.Fatal("cancel kept a pending owner request")
	}
	for _, kind := range []string{"permission", "question"} {
		id := kind + "-one"
		payload, _ := json.Marshal(map[string]string{"jobId": "job", "kind": kind, "id": id, "response": "once", "answer": "yes"})
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/companion/respond", bytes.NewReader(payload))
		r.RemoteAddr = "127.0.0.1:1000"
		r.Header.Set("Authorization", "Bearer desktop-token")
		w = httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != http.StatusConflict {
			t.Fatalf("canceled %s response returned %d", kind, w.Code)
		}
	}
}

func TestCompanionCancelInterruptsOwnerResponse(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	started, stopped := make(chan struct{}), make(chan struct{})
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		http.Error(w, "Stopped", http.StatusConflict)
		close(stopped)
	}))
	ctx, cancel := context.WithCancel(s.ctx)
	job := &companionJob{id: "job", kind: "build", status: "waiting", output: []string{}, ctx: ctx, cancel: cancel, pendingPermission: json.RawMessage(`{"id":"permission-one","sessionID":"session"}`)}
	s.jobs[job.id] = job
	m := newCompanionManager(s.api)
	m.session = s
	done := make(chan struct{})
	go func() {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/companion/respond", strings.NewReader(`{"jobId":"job","kind":"permission","id":"permission-one","response":"once"}`))
		r.RemoteAddr = "127.0.0.1:1000"
		r.Header.Set("Authorization", "Bearer desktop-token")
		m.ServeHTTP(httptest.NewRecorder(), r)
		close(done)
	}()
	<-started
	s.jobHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, "/", nil), job.id, "build")
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("cancel left owner response in flight")
	}
	<-done
	if job.snapshot(true)["status"] != "canceled" || job.snapshot(true)["pendingPermission"] != nil {
		t.Fatal("owner response restored canceled work")
	}
}

func TestCompanionOwnerResponseIsAtomic(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	calls := 0
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/opencode/permission/respond" {
			t.Fatal("unexpected response route")
		}
		var request OpenCodePermissionRespondRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.SessionID != "session" || request.ProjectPath != "/shared" || request.PermissionID != "permission-one" || request.Response != "once" {
			t.Fatal("response escaped its pending request")
		}
		calls++
		writeJSON(w, map[string]bool{"ok": true})
	}))
	job := &companionJob{id: "job", projectPath: "/shared", status: "waiting", output: []string{}, pendingPermission: json.RawMessage(`{"id":"permission-one","sessionID":"session"}`), ctx: s.ctx}
	s.jobs[job.id] = job
	m := newCompanionManager(s.api)
	m.session = s
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/companion/respond", strings.NewReader(`{"jobId":"job","kind":"permission","id":"permission-one","response":"once"}`))
		r.RemoteAddr = "127.0.0.1:1000"
		r.Header.Set("Authorization", "Bearer desktop-token")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if calls != 1 || job.snapshot(true)["pendingPermission"] != nil {
		t.Fatal("owner response replayed or did not clear")
	}
}

func TestCompanionBuildGuard(t *testing.T) {
	root := t.TempDir()
	if err := SaveProject(filepath.Join(root, "glowbom.json"), &GlowbomProject{Name: "Shared", Targets: map[string]Target{}}); err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	m := newCompanionManager(http.NotFoundHandler())
	handler := m.guardBuild(func(w http.ResponseWriter, r *http.Request) { close(started); <-finish })
	payload, _ := json.Marshal(map[string]string{"projectPath": root, "instructions": "Keep this request"})
	done := make(chan struct{})
	go func() {
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/opencode/refine", bytes.NewReader(payload)))
		close(done)
	}()
	<-started
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest(http.MethodPost, "/opencode/refine", bytes.NewReader(payload)))
	if w.Code != http.StatusConflict {
		t.Fatal("concurrent build was allowed to overwrite instructions")
	}
	close(finish)
	<-done
}

func TestCompanionListenerLifecycle(t *testing.T) {
	interfaces := companionInterfaces()
	if len(interfaces) == 0 {
		t.Skip("No private IPv4 interface is connected.")
	}
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Close)
	// Listener coverage must not publish a real Bonjour service.
	discovery := &companionDiscoveryFixture{}
	m.registerDiscovery = discovery.register
	if err := m.enable(interfaces[0].Address, map[string]companionProject{}); err != nil {
		t.Fatal(err)
	}
	first := m.session.pairing
	if first.Token == "" || first.Version != 1 || !m.status()["active"].(bool) {
		t.Fatal("no active ephemeral pairing")
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13,
		VerifyConnection: func(state tls.ConnectionState) error {
			digest := sha256.Sum256(state.PeerCertificates[0].Raw)
			if hex.EncodeToString(digest[:]) != first.CertificateSHA256 {
				return errors.New("certificate pin mismatch")
			}
			return nil
		}}}}
	defer client.CloseIdleConnections()
	request, _ := http.NewRequest(http.MethodGet, first.URL+"/projects", nil)
	request.Header.Set("Authorization", "Bearer "+first.Token)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("private TLS listener returned %d", response.StatusCode)
	}
	if err := m.enable(interfaces[0].Address, map[string]companionProject{}); err != nil {
		t.Fatal(err)
	}
	if m.session.pairing.Token == first.Token || m.session.pairing.CertificateSHA256 == first.CertificateSHA256 {
		t.Fatal("pairing reused its token or certificate")
	}
	if _, err := client.Do(request); err == nil {
		t.Fatal("replaced listener still reachable")
	}
	m.Close()
	if m.status()["active"].(bool) {
		t.Fatal("revoke left active pairing")
	}
}
