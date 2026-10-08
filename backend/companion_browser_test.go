package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCompanionBrowserActualPHPFormSession(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("PHP is not installed")
	}
	directory := t.TempDir()
	page := `<?php session_start(); if ($_SERVER['REQUEST_METHOD'] === 'POST') { $_SESSION['count'] = ($_SESSION['count'] ?? 0) + 1; header('Location: /'); exit; } ?><!doctype html><html><title>Local PHP preview</title><h1>Local PHP preview</h1><p>Saved count: <?= (int)($_SESSION['count'] ?? 0) ?></p><form method="post"><button>Save one</button></form></html>`
	if err := os.WriteFile(filepath.Join(directory, "index.php"), []byte(page), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, php, "-d", "session.save_path="+directory, "-S", address, "-t", directory)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	probe := &http.Client{Timeout: 200 * time.Millisecond}
	ready := false
	for attempt := 0; attempt < 30; attempt++ {
		if response, err := probe.Get("http://" + address); err == nil {
			response.Body.Close()
			ready = response.StatusCode == http.StatusOK
			if ready {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("isolated PHP test server did not start")
	}
	f := newCompanionBrowserFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	f.runner.view.URL = "http://" + address
	link, client, gateway := f.open(t)
	if response := redeemCompanionBrowser(t, link, client); response.StatusCode != http.StatusNoContent {
		t.Fatal("could not redeem PHP preview")
	}
	initial, err := client.Get(gateway.origin + "/")
	if err != nil {
		t.Fatal(err)
	}
	initial.Body.Close()
	if initial.StatusCode != http.StatusOK || initial.Header.Get("Content-Security-Policy") != "" {
		t.Fatal("bootstrap policy leaked into the PHP page")
	}
	if initial.Header.Get("Referrer-Policy") != "same-origin" {
		t.Fatal("PHP form navigation would lose its same-origin Origin header")
	}
	for index, form := range []string{"", "save=1"} {
		request, _ := http.NewRequest(http.MethodPost, gateway.origin+"/", strings.NewReader(form))
		request.Header.Set("Origin", gateway.origin)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusFound || response.Header.Get("Set-Cookie") != "" {
			t.Fatal("PHP form or session isolation failed", response.StatusCode)
		}
		response, err = client.Get(gateway.origin + "/")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), fmt.Sprintf("Saved count: %d", index+1)) {
			t.Fatal("real PHP session did not retain its submitted value", index)
		}
	}
}

type companionBrowserFixture struct {
	session  *companionSession
	project  companionProject
	runner   *previewSession
	upstream *httptest.Server
	listens  atomic.Int32
}

func newCompanionBrowserFixture(t *testing.T, handler http.Handler) *companionBrowserFixture {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	manager := newProjectPreviewManager()
	s := testCompanion(t, manager)
	s.previews = manager
	project := sharedCompanionProject(t, s)
	runner := &previewSession{view: previewTarget{Target: "web", Kind: "custom", Available: true, Status: "running", ID: strings.Repeat("c", 48), URL: upstream.URL}, done: make(chan struct{})}
	manager.sessions[project.path+"/web"] = runner
	f := &companionBrowserFixture{session: s, project: project, runner: runner, upstream: upstream}
	s.browserListen = func(network, address string) (net.Listener, error) {
		f.listens.Add(1)
		return net.Listen("tcp4", "127.0.0.1:0")
	}
	t.Cleanup(s.close)
	return f
}

func (f *companionBrowserFixture) issue(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.session.ServeHTTP(w, companionRequest(f.session, http.MethodPost, "/projects/"+f.project.ID+"/preview/web/browser-session", body))
	return w
}

func (f *companionBrowserFixture) open(t *testing.T) (*url.URL, *http.Client, *companionBrowserSession) {
	t.Helper()
	w := f.issue(t, `{"previewID":"`+f.runner.snapshot().ID+`","allowLocalHTTP":true}`)
	var result struct{ URL, ExpiresAt, Message string }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal("could not create browser session", w.Code)
	}
	address, err := url.Parse(result.URL)
	if err != nil || address.Scheme != "http" || address.Fragment == "" || address.RawQuery != "" {
		t.Fatal("invalid browser bootstrap URL")
	}
	if strings.Contains(result.URL, f.session.pairing.Token) || strings.Contains(w.Body.String(), f.project.path) {
		t.Fatal("browser session exposed control credentials or project path")
	}
	expires, err := time.Parse(time.RFC3339, result.ExpiresAt)
	if err != nil || expires.After(f.session.expires) || expires.After(time.Now().Add(companionBrowserLifetime)) {
		t.Fatal("browser session expiry exceeded its bounds")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	f.session.browserMu.Lock()
	gateway := f.session.browsers[f.project.path+"/web"]
	f.session.browserMu.Unlock()
	return address, client, gateway
}

func redeemCompanionBrowser(t *testing.T, address *url.URL, client *http.Client) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": address.Fragment})
	request, _ := http.NewRequest(http.MethodPost, "http://"+address.Host+companionBrowserBootstrap+"/redeem", strings.NewReader(string(body)))
	request.Header.Set("Origin", "http://"+address.Host)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response
}

func TestCompanionBrowserRequiresExplicitOptInAndCurrentRunner(t *testing.T) {
	f := newCompanionBrowserFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, body := range []string{`{"previewID":"current"}`, `{"previewID":"current","allowLocalHTTP":false}`} {
		if response := f.issue(t, body); response.Code != http.StatusForbidden || f.listens.Load() != 0 {
			t.Fatal("a browser listener opened without explicit HTTP approval")
		}
	}
	if response := f.issue(t, `{"previewID":"stale","allowLocalHTTP":true}`); response.Code != http.StatusConflict || f.listens.Load() != 0 {
		t.Fatal("a browser listener opened for a stale preview")
	}
	r := companionRequest(f.session, http.MethodPost, "/projects/"+f.project.ID+"/preview/web/browser-session", `{"previewID":"current","allowLocalHTTP":true}`)
	r.Header.Del("Authorization")
	w := httptest.NewRecorder()
	f.session.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || f.listens.Load() != 0 {
		t.Fatal("unauthenticated request opened a browser listener")
	}
}

func TestCompanionBrowserBootstrapAndPHPFormSession(t *testing.T) {
	f := newCompanionBrowserFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, cookie := range r.Cookies() {
			if cookie.Name != "PHPSESSID" {
				t.Error("browser or unrelated cookie reached the project")
			}
		}
		switch r.URL.Path {
		case "/login.php":
			if r.Method != http.MethodPost || r.ParseForm() != nil || r.Form.Get("name") != "Preview visitor" {
				t.Error("PHP form fields were not preserved")
			}
			http.SetCookie(w, &http.Cookie{Name: "PHPSESSID", Value: "project-session", Path: "/", HttpOnly: true})
			w.Header().Set("Location", "http://"+r.Host+"/account.php")
			w.WriteHeader(http.StatusSeeOther)
		case "/account.php":
			cookie, err := r.Cookie("PHPSESSID")
			if err != nil || cookie.Value != "project-session" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, "Signed in to this preview")
		}
	}))
	address, client, gateway := f.open(t)
	bootstrap, err := client.Get(address.String())
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(bootstrap.Body)
	bootstrap.Body.Close()
	if bootstrap.StatusCode != http.StatusOK || strings.Contains(string(html), address.Fragment) || !strings.Contains(string(html), "history.replaceState") || !strings.Contains(bootstrap.Header.Get("Content-Security-Policy"), "sha256-") {
		t.Fatal("bootstrap did not keep the one-use token out of HTML")
	}
	if bootstrap.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("bootstrap did not suppress its referrer")
	}
	response := redeemCompanionBrowser(t, address, client)
	if response.StatusCode != http.StatusNoContent || len(response.Cookies()) != 1 || !response.Cookies()[0].HttpOnly || response.Cookies()[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("bootstrap did not set a scoped HTTP-only browser cookie")
	}
	if response := redeemCompanionBrowser(t, address, client); response.StatusCode != http.StatusUnauthorized {
		t.Fatal("bootstrap token could be reused")
	}
	request, _ := http.NewRequest(http.MethodPost, gateway.origin+"/login.php", strings.NewReader(url.Values{"name": {"Preview visitor"}}.Encode()))
	request.Header.Set("Origin", gateway.origin)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "another_gateway", Value: "private-browser-cookie"})
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != gateway.origin+"/account.php" || response.Header.Get("Set-Cookie") != "" {
		t.Fatal("PHP session cookie or local redirect escaped the gateway")
	}
	response, err = client.Get(gateway.origin + "/account.php")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("PHP session did not persist between requests")
	}
	if policies := response.Header.Values("Referrer-Policy"); len(policies) != 1 || policies[0] != "same-origin" {
		t.Fatal("project response did not preserve the origin of same-origin form submissions")
	}
	reopened, _, same := f.open(t)
	if same != gateway || reopened.Host != address.Host || f.listens.Load() != 1 || reopened.Fragment == address.Fragment {
		t.Fatal("reopening did not reuse the current browser session")
	}
	if response := redeemCompanionBrowser(t, reopened, client); response.StatusCode != http.StatusNoContent {
		t.Fatal("reopened link could not be redeemed")
	}
	response, err = client.Get(gateway.origin + "/account.php")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("reopening discarded the project session")
	}
}

func TestCompanionBrowserJSONAndSecurityBoundary(t *testing.T) {
	var calls atomic.Int32
	f := newCompanionBrowserFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("X-Glowbom-Preview-ID") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			t.Error("control credentials or browser metadata reached the runner")
		}
		if r.URL.Path == "/external" {
			http.Redirect(w, r, "http://192.168.100.200/private", http.StatusFound)
			return
		}
		if r.URL.Path == "/api" {
			if r.Header.Get("Origin") != "http://"+r.Host || r.Method != http.MethodPost {
				t.Error("validated browser origin was not mapped to the fixed runner")
			}
			w.Header().Set("Content-Type", "application/json")
			io.Copy(w, r.Body)
		}
	}))
	address, client, gateway := f.open(t)
	if response := redeemCompanionBrowser(t, address, client); response.StatusCode != http.StatusNoContent {
		t.Fatal("could not redeem browser link")
	}
	for _, test := range []struct {
		name, method, path, origin, host, site string
		withoutCookie                          bool
		want                                   int
	}{
		{name: "unauthenticated", method: "GET", path: "/", withoutCookie: true, want: 401},
		{name: "host", method: "GET", path: "/", host: "evil.test", want: 403},
		{name: "cross-origin", method: "POST", path: "/api", origin: "https://evil.test", want: 403},
		{name: "missing origin", method: "POST", path: "/api", want: 403},
		{name: "opaque origin", method: "POST", path: "/api", origin: "null", site: "same-origin", want: 403},
		{name: "cross-site", method: "GET", path: "/", site: "cross-site", want: 403},
		{name: "sibling port", method: "GET", path: "/", site: "same-site", want: 403},
		{name: "hidden file", method: "GET", path: "/.env", want: 403},
		{name: "outside project", method: "GET", path: "/@fs/private/file", want: 403},
		{name: "preview credential", method: "GET", path: "/?glowbom_preview=private", want: 403},
		{name: "external redirect", method: "GET", path: "/external", want: 502},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, _ := http.NewRequest(test.method, gateway.origin+test.path, nil)
			request.Header.Set("Origin", test.origin)
			request.Header.Set("Sec-Fetch-Site", test.site)
			if test.host != "" {
				request.Host = test.host
			}
			browser := client
			if test.withoutCookie {
				copy := *client
				copy.Jar = nil
				browser = &copy
			}
			response, err := browser.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != test.want {
				t.Fatal("unexpected boundary response", response.StatusCode)
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatal("rejected request reached the runner")
	}
	request, _ := http.NewRequest(http.MethodPost, gateway.origin+"/api", strings.NewReader(`{"name":"New item"}`))
	request.Header.Set("Origin", gateway.origin)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+f.session.pairing.Token)
	request.Header.Set("X-Forwarded-Host", "evil.test")
	request.Header.Set("X-Glowbom-Preview-ID", gateway.previewID)
	request.Header.Set("Referer", gateway.origin+"/private-path")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != `{"name":"New item"}` {
		t.Fatal("JSON mutation did not survive proxying")
	}
	request, _ = http.NewRequest(http.MethodPost, gateway.origin+"/api", strings.NewReader(strings.Repeat("x", companionBrowserBodyLimit+1)))
	request.Header.Set("Origin", gateway.origin)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge || calls.Load() != 2 {
		t.Fatal("oversized body reached the runner")
	}
}

func TestCompanionBrowserSSEStopsWhenPairingIsRevoked(t *testing.T) {
	closed := make(chan struct{})
	f := newCompanionBrowserFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: ready\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	address, client, gateway := f.open(t)
	redeemCompanionBrowser(t, address, client)
	response, err := client.Get(gateway.origin + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data := make([]byte, len("data: ready\n\n"))
	if _, err := io.ReadFull(response.Body, data); err != nil || string(data) != "data: ready\n\n" {
		t.Fatal("event stream was buffered", err)
	}
	f.session.close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("pairing revocation did not cancel the event stream")
	}
}

func TestCompanionBrowserKeepsProjectCookiesSeparateAcrossGateways(t *testing.T) {
	var sessionCount atomic.Int32
	f := newCompanionBrowserFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("PHPSESSID")
		if err != nil {
			value := "first"
			if sessionCount.Add(1) > 1 {
				value = "second"
			}
			cookie = &http.Cookie{Name: "PHPSESSID", Value: value, Path: "/"}
			http.SetCookie(w, cookie)
		}
		if len(r.Cookies()) > 1 {
			t.Error("gateway cookies leaked into the project")
		}
		io.WriteString(w, cookie.Value)
	}))
	first, client, firstGateway := f.open(t)
	redeemCompanionBrowser(t, first, client)
	read := func(address, want string) {
		t.Helper()
		response, err := client.Get(address)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || string(body) != want {
			t.Fatal("project sessions were shared between browser gateways")
		}
	}
	read(firstGateway.origin+"/", "first")
	f.session.previews.mu.Lock()
	f.session.previews.sessions[f.project.path+"/php"] = &previewSession{view: previewTarget{Target: "php", Kind: "custom", Status: "running", ID: strings.Repeat("d", 48), URL: f.upstream.URL}}
	f.session.previews.mu.Unlock()
	w := httptest.NewRecorder()
	f.session.ServeHTTP(w, companionRequest(f.session, http.MethodPost, "/projects/"+f.project.ID+"/preview/php/browser-session", `{"previewID":"`+strings.Repeat("d", 48)+`","allowLocalHTTP":true}`))
	var result struct{ URL string }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal("second browser gateway did not open")
	}
	second, err := url.Parse(result.URL)
	if err != nil {
		t.Fatal(err)
	}
	if response := redeemCompanionBrowser(t, second, client); response.StatusCode != http.StatusNoContent {
		t.Fatal("second gateway could not be redeemed")
	}
	read("http://"+second.Host+"/", "second")
	read(firstGateway.origin+"/", "first")
}

func TestCompanionBrowserWebSocketStopsWhenRunnerChanges(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "http://"+r.Host }}
	f := newCompanionBrowserFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			kind, data, err := connection.ReadMessage()
			if err != nil {
				return
			}
			if connection.WriteMessage(kind, data) != nil {
				return
			}
		}
	}))
	address, client, gateway := f.open(t)
	redeemCompanionBrowser(t, address, client)
	headers := http.Header{"Origin": {gateway.origin}}
	for _, cookie := range client.Jar.Cookies(&url.URL{Scheme: "http", Host: gateway.host, Path: "/"}) {
		headers.Add("Cookie", cookie.String())
	}
	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	foreign := headers.Clone()
	foreign.Set("Origin", "https://evil.test")
	if connection, response, err := dialer.Dial("ws://"+gateway.host+"/socket", foreign); err == nil {
		connection.Close()
		t.Fatal("cross-origin WebSocket reached the runner")
	} else if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("cross-origin WebSocket was not rejected at the gateway")
	}
	connection, _, err := dialer.Dial("ws://"+gateway.host+"/socket", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteMessage(websocket.TextMessage, []byte("preview change")); err != nil {
		t.Fatal(err)
	}
	_, data, err := connection.ReadMessage()
	if err != nil || string(data) != "preview change" {
		t.Fatal("WebSocket was not proxied", err)
	}
	f.runner.mu.Lock()
	f.runner.view.ID = "replacement-preview"
	f.runner.mu.Unlock()
	connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("stale runner kept its WebSocket open")
	} else if networkError, ok := err.(net.Error); ok && networkError.Timeout() {
		t.Fatal("runner was revoked but the WebSocket did not close")
	}
	select {
	case <-gateway.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("runner replacement did not revoke the browser grant")
	}
}

func TestCompanionBrowserExpiryAndPrivateRemote(t *testing.T) {
	f := newCompanionBrowserFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := time.Now()
	var elapsed atomic.Int64
	f.session.now = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	address, client, gateway := f.open(t)
	redeemCompanionBrowser(t, address, client)
	r := httptest.NewRequest(http.MethodGet, gateway.origin+"/", nil)
	r.RemoteAddr = "203.0.113.5:12345"
	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("public remote address reached a LAN preview")
	}
	elapsed.Store(int64(companionBrowserLifetime + time.Second))
	select {
	case <-gateway.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("expired browser grant remained open")
	}
	if response, err := http.Get(gateway.origin + "/"); err == nil {
		response.Body.Close()
		// The handler can still finish a queued request while the listener closes.
		select {
		case <-gateway.ctx.Done():
		default:
			t.Fatal("expired gateway accepted a request")
		}
	}
}
