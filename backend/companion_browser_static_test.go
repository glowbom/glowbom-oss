package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompanionBrowserStaticPrototypeKeepsRelativeResources(t *testing.T) {
	manager := newProjectPreviewManager()
	t.Cleanup(manager.Close)
	s := testCompanion(t, manager)
	s.previews = manager
	t.Cleanup(s.close)
	s.browserListen = func(string, string) (net.Listener, error) {
		return net.Listen("tcp4", "127.0.0.1:0")
	}
	project := sharedCompanionProject(t, s)
	files := map[string]string{
		"index.html":       `<!doctype html><html><head><link rel="stylesheet" href="./assets/style.css"></head><body><h1 id="count">Prototype</h1><img src="./assets/icon.svg"><script type="module" src="./js/app.js"></script></body></html>`,
		"assets/style.css": `h1 { color: navy; }`,
		"assets/icon.svg":  `<svg xmlns="http://www.w3.org/2000/svg"><circle cx="5" cy="5" r="5"/></svg>`,
		"js/app.js":        `import { label } from './label.js'; document.querySelector('#count').textContent = label; fetch('./data.json');`,
		"js/label.js":      `export const label = 'Modules loaded';`,
		"data.json":        `{"count":1}`,
	}
	for name, content := range files {
		previewFixture(t, project.path, "prototype/"+name, content)
	}
	previewFixture(t, project.path, "prototype/.env", "PRIVATE=hidden")
	previewFixture(t, project.path, "private.txt", "outside preview")
	if err := os.Symlink("../private.txt", filepath.Join(project.path, "prototype", "escape.txt")); err != nil {
		t.Fatal(err)
	}
	request := func(suffix, body string, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		r := companionRequest(s, http.MethodPost, "/projects/"+project.ID+suffix, body)
		if !authenticated {
			r.Header.Del("Authorization")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	started := request("/previews", `{"target":"prototype","action":"start","install":false}`, true)
	var result struct{ Targets []companionPreviewTarget }
	if started.Code != http.StatusOK || json.Unmarshal(started.Body.Bytes(), &result) != nil || len(result.Targets) == 0 || result.Targets[0].Status != "running" {
		t.Fatal("could not start the actual static prototype runner", started.Code)
	}
	previewID := result.Targets[0].ID
	payload, _ := json.Marshal(map[string]any{"previewID": previewID, "allowLocalHTTP": true})
	snapshot := request("/preview/prototype/browser-snapshot", `{"previewID":"`+previewID+`"}`, true)
	var snapshotResult struct{ Kind string }
	if snapshot.Code != http.StatusOK || json.Unmarshal(snapshot.Body.Bytes(), &snapshotResult) != nil || snapshotResult.Kind != "needs-live-browser-route" {
		t.Fatal("local module imports must keep a live route instead of becoming a broken snapshot", snapshot.Code)
	}
	if response := request("/preview/prototype/browser-session", string(payload), false); response.Code != http.StatusUnauthorized {
		t.Fatal("prototype browser route accepted an unauthenticated control request")
	}
	opened := request("/preview/prototype/browser-session", string(payload), true)
	var issued struct{ URL string }
	if opened.Code != http.StatusOK || json.Unmarshal(opened.Body.Bytes(), &issued) != nil {
		t.Fatal("could not open the prototype browser route", opened.Code)
	}
	address, err := url.Parse(issued.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.browserMu.Lock()
	gateway := s.browsers[project.path+"/prototype"]
	s.browserMu.Unlock()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	if response := redeemCompanionBrowser(t, address, client); response.StatusCode != http.StatusNoContent {
		t.Fatal("could not redeem the static prototype browser link")
	}
	for name, expected := range files {
		path := "/" + name
		if name == "index.html" {
			path = "/"
		}
		r, _ := http.NewRequest(http.MethodGet, gateway.origin+path+"?v=1", nil)
		r.Header.Set("Origin", gateway.origin)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), expected) {
			t.Fatalf("static prototype resource %q failed: %d", name, response.StatusCode)
		}
		if response.Header.Get("Set-Cookie") != "" || strings.Contains(string(body), previewID) || strings.Contains(string(body), s.pairing.Token) {
			t.Fatal("static runner or control credentials reached the browser")
		}
		if strings.HasSuffix(name, ".js") && !strings.Contains(response.Header.Get("Content-Type"), "javascript") {
			t.Fatal("module response did not have a JavaScript MIME type")
		}
	}
	for _, path := range []string{"/__glowbom_preview__/reload.js", "/__glowbom_preview__/revision"} {
		response, err := client.Get(gateway.origin + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatal("static live reload could not use the scoped browser route", path)
		}
	}
	for _, test := range []struct {
		address string
		client  *http.Client
		status  int
	}{
		{gateway.origin + "/js/app.js", &http.Client{Timeout: time.Second}, http.StatusUnauthorized},
		{gateway.upstream.String() + "/js/app.js", &http.Client{Timeout: time.Second}, http.StatusUnauthorized},
		{gateway.origin + "/.env", client, http.StatusForbidden},
		{gateway.origin + "/@fs/private.txt", client, http.StatusForbidden},
		{gateway.origin + "/escape.txt", client, http.StatusNotFound},
	} {
		response, err := test.client.Get(test.address)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("static prototype resource boundary returned %d, want %d", response.StatusCode, test.status)
		}
	}
	for _, cookie := range jar.Cookies(&url.URL{Scheme: "http", Host: gateway.host}) {
		if cookie.Name != gateway.cookieName {
			t.Fatal("static runner cookie was stored in the browser")
		}
	}
	if stopped := request("/previews", `{"target":"prototype","action":"stop","id":"`+previewID+`"}`, true); stopped.Code != http.StatusOK {
		t.Fatal("could not stop the static preview", stopped.Code)
	}
	select {
	case <-gateway.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stopping the static prototype did not revoke its browser gateway")
	}
}
