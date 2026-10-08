package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompanionStaticSnapshotPreservesInlineDocument(t *testing.T) {
	project := t.TempDir()
	document := `<!doctype html><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'"><style>body { color: black }</style><h1>Solar countdown</h1><script>document.querySelector('h1').textContent = 'Next eclipse';</script>`
	previewFixture(t, project, "prototype/index.html", document)
	got, err := companionStaticSnapshot(context.Background(), project, filepath.Join(project, "prototype"))
	if err != nil || got != document {
		t.Fatalf("inline content or CSP changed: err=%v", err)
	}
}

func TestCompanionStaticSnapshotEmbedsBoundedAssets(t *testing.T) {
	project := t.TempDir()
	previewFixture(t, project, "prototype/index.html", `<link rel="stylesheet" href="style.css"><img src="/images/picture.png"><script src="app.js"></script>`)
	assets := map[string]string{"style.css": "body { color: black }", "images/picture.png": "image bytes", "app.js": "document.title = 'App';"}
	for name, data := range assets {
		previewFixture(t, project, "prototype/"+name, data)
	}
	got, err := companionStaticSnapshot(context.Background(), project, filepath.Join(project, "prototype"))
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range assets {
		if !strings.Contains(got, base64.StdEncoding.EncodeToString([]byte(data))) {
			t.Fatal("local asset was not embedded")
		}
	}
	if strings.Contains(got, project) {
		t.Fatal("snapshot exposed its Desktop path")
	}
}

func TestCompanionStaticSnapshotRejectsUnsafeAndLiveDependencies(t *testing.T) {
	for _, test := range []struct {
		name, document, asset string
	}{
		{"parent traversal", `<img src="../private.png">`, ""},
		{"encoded traversal", `<img src="%2e%2e/private.png">`, ""},
		{"hidden file", `<script src=".private.js"></script>`, ""},
		{"hidden directory", `<script src=".secrets/app.js"></script>`, ""},
		{"unsupported file", `<img src="private.json">`, ""},
		{"asset query", `<script src="app.js?token=private"></script>`, ""},
		{"relative module", `<script type="module" src="app.js"></script>`, ""},
		{"inline module import", `<script type="module">import './app.js';</script>`, ""},
		{"inline module from", `<script type="module">import { app } from './app.js';</script>`, ""},
		{"inline module export", `<script type="module">export { app } from './app.js';</script>`, ""},
		{"dynamic module import", `<script>import('./app.js');</script>`, ""},
		{"local fetch", `<script>fetch('/api/items');</script>`, ""},
		{"event fetch", `<button onclick="fetch('/api/items')">Load</button>`, ""},
		{"local image script", `<script>image.src = 'assets/image.png';</script>`, ""},
		{"local SVG image", `<svg><image href="image.png" /></svg>`, ""},
		{"local SVG use", `<svg><use xlink:href="icons.svg#app" /></svg>`, ""},
		{"CSS dependency", `<link rel="stylesheet" href="app.css">`, `body { background: url(image.png) }`},
		{"CSS import", `<link rel="stylesheet" href="app.css">`, `@import "other.css";`},
		{"inline CSS dependency", `<style>body { background: url(image.png) }</style>`, ""},
		{"CSS image set", `<style>body { background: image-set("image.png" 1x, "large.png" 2x) }</style>`, ""},
		{"CSS prefixed image set", `<div style="background: -webkit-image-set('image.png' 1x)"></div>`, ""},
		{"local form", `<form method="POST" action="/login"><input name="email"></form>`, ""},
		{"empty form action", `<form action=""><input name="email"></form>`, ""},
		{"page link", `<a href="page.html">Next page</a>`, ""},
		{"frame", `<iframe src="page.html"></iframe>`, ""},
		{"source set", `<img srcset="one.png 1x, two.png 2x">`, ""},
		{"base URL", `<base href="https://example.test/"><img src="image.png">`, ""},
		{"CSP rewrite", `<meta http-equiv="Content-Security-Policy" content="script-src 'self'"><script src="app.js"></script>`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := t.TempDir()
			previewFixture(t, project, "prototype/index.html", test.document)
			previewFixture(t, project, "prototype/app.js", "console.log('app')")
			previewFixture(t, project, "prototype/app.css", test.asset)
			if _, err := companionStaticSnapshot(context.Background(), project, filepath.Join(project, "prototype")); err == nil {
				t.Fatal("unsupported snapshot was accepted")
			}
		})
	}
}

func TestCompanionStaticSnapshotPreservesExternalDependencies(t *testing.T) {
	project := t.TempDir()
	document := `<style>body { background: image-set("https://example.test/a.webp" type("image/webp") 1x, url(https://example.test/b.png) 2x) }</style><svg><use href="#icon" /></svg><script type="module">import { app } from 'https://example.test/app.js'; fetch('https://example.test/api');</script>`
	previewFixture(t, project, "prototype/index.html", document)
	got, err := companionStaticSnapshot(context.Background(), project, filepath.Join(project, "prototype"))
	if err != nil || got != document {
		t.Fatalf("external references were rejected or changed: %v", err)
	}
}

func TestCompanionStaticSnapshotPreservesClientFormAndMedia(t *testing.T) {
	project := t.TempDir()
	document := `<form id="login"><input name="name"></form><script>document.querySelector('form').addEventListener('submit', event => event.preventDefault());</script><audio src="assets/music.mp3"></audio><video src="assets/movie.mp4"></video><img src="glowbomimages:Unfinished picture"><audio src="glowbyaudio:Unfinished sound"></audio>`
	previewFixture(t, project, "prototype/index.html", document)
	previewFixture(t, project, "prototype/assets/music.mp3", "audio bytes")
	previewFixture(t, project, "prototype/assets/movie.mp4", "video bytes")
	got, err := companionStaticSnapshot(context.Background(), project, filepath.Join(project, "prototype"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`<form id="login">`, "event.preventDefault()", "data:audio/mpeg;base64,", "data:video/mp4;base64,", "glowbomimages:Unfinished picture", "glowbyaudio:Unfinished sound"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("client form or media was lost: %s", expected)
		}
	}
}

func TestCompanionStaticSnapshotRejectsDependenciesInEmbeddedScript(t *testing.T) {
	project := t.TempDir()
	previewFixture(t, project, "prototype/index.html", `<script src="app.js"></script>`)
	previewFixture(t, project, "prototype/app.js", `fetch('./data.json')`)
	if _, err := companionStaticSnapshot(context.Background(), project, filepath.Join(project, "prototype")); err == nil {
		t.Fatal("embedded script retained its local dependency")
	}
}

func TestCompanionStaticSnapshotValidatesOpenedDirectoryIdentity(t *testing.T) {
	project := t.TempDir()
	previewFixture(t, project, "prototype/index.html", "selected preview")
	previewFixture(t, project, ".private/index.html", "private content")
	projectRoot, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer projectRoot.Close()
	expected, err := projectRoot.Lstat("prototype")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := projectRoot.OpenRoot("prototype")
	if err != nil {
		t.Fatal(err)
	}
	defer selected.Close()
	if err := companionSnapshotValidateRoot(projectRoot, selected, "prototype", expected); err != nil {
		t.Fatal("unchanged preview directory was rejected", err)
	}
	wrong, err := projectRoot.OpenRoot(".private")
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if err := companionSnapshotValidateRoot(projectRoot, wrong, "prototype", expected); err == nil {
		t.Fatal("a swapped directory handle was accepted")
	}
	if err := os.Rename(filepath.Join(project, "prototype"), filepath.Join(project, "previous")); err != nil {
		t.Fatal(err)
	}
	previewFixture(t, project, "prototype/index.html", "replacement preview")
	if err := companionSnapshotValidateRoot(projectRoot, selected, "prototype", expected); err == nil {
		t.Fatal("a replaced directory path was accepted")
	}
}

func TestCompanionStaticSnapshotRejectsSymlinkAndOversize(t *testing.T) {
	project := t.TempDir()
	previewFixture(t, project, "prototype/index.html", `<script src="app.js"></script>`)
	previewFixture(t, project, "prototype/.private.js", "private content")
	if err := os.Symlink(".private.js", filepath.Join(project, "prototype", "app.js")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := companionStaticSnapshot(context.Background(), project, filepath.Join(project, "prototype")); err == nil {
		t.Fatal("symlink to hidden content was exported")
	}
	if err := os.Remove(filepath.Join(project, "prototype", "app.js")); err != nil {
		t.Fatal(err)
	}
	previewFixture(t, project, "prototype/app.js", strings.Repeat("x", companionSnapshotAssetLimit+1))
	if _, err := companionStaticSnapshot(context.Background(), project, filepath.Join(project, "prototype")); err == nil {
		t.Fatal("oversized asset was exported")
	}
	previewFixture(t, project, "prototype/index.html", strings.Repeat("x", companionSnapshotLimit+1))
	if _, err := companionStaticSnapshot(context.Background(), project, filepath.Join(project, "prototype")); err == nil {
		t.Fatal("oversized document was exported")
	}
}

func TestCompanionBrowserSnapshotIsScopedAndRevocable(t *testing.T) {
	manager := newProjectPreviewManager()
	s := testCompanion(t, manager)
	s.previews = manager
	project := sharedCompanionProject(t, s)
	document := `<h1>Private prototype</h1><script>document.title = 'Prototype';</script>`
	previewFixture(t, project.path, "prototype/index.html", document)
	previewID := strings.Repeat("a", 48)
	manager.sessions[project.path+"/prototype"] = &previewSession{project: project.path, view: previewTarget{Target: "prototype", Name: "Prototype", Kind: "static", Status: "running", ID: previewID, URL: "http://127.0.0.1:45678/", directory: filepath.Join(project.path, "prototype")}}
	route := "/projects/" + project.ID + "/preview/prototype/browser-snapshot"
	request := func(id string, change func(*http.Request)) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]string{"previewID": id})
		r := companionRequest(s, http.MethodPost, route, string(payload))
		if change != nil {
			change(r)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	response := request(previewID, nil)
	var result map[string]string
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result["kind"] != "snapshot" || result["html"] != document {
		t.Fatalf("snapshot failed: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), project.path) || strings.Contains(response.Body.String(), s.pairing.Token) || strings.Contains(response.Body.String(), "127.0.0.1") {
		t.Fatal("snapshot exposed control or filesystem metadata")
	}
	if got := request("stale", nil); got.Code != http.StatusConflict {
		t.Fatal("stale preview ID was accepted", got.Code)
	}
	if got := request(previewID, func(r *http.Request) { r.Header.Del("Authorization") }); got.Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated snapshot was accepted", got.Code)
	}
	if got := request(previewID, func(r *http.Request) { r.Header.Set("Origin", "https://example.test") }); got.Code != http.StatusForbidden {
		t.Fatal("browser origin reached snapshot export", got.Code)
	}
	if got := request(previewID, func(r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		*r = *r.WithContext(ctx)
	}); got.Code != http.StatusGatewayTimeout {
		t.Fatal("canceled export was reported as a pairing failure", got.Code)
	}
	manager.sessions[project.path+"/prototype"].view.Kind = "custom"
	response = request(previewID, nil)
	result = nil
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result["kind"] != "needs-live-browser-route" || result["html"] != "" {
		t.Fatal("live server was flattened into a snapshot")
	}
	manager.sessions[project.path+"/prototype"].view.Kind = "static"
	s.expires = time.Now().Add(-time.Second)
	if got := request(previewID, nil); got.Code != http.StatusUnauthorized {
		t.Fatal("expired pairing exported a snapshot", got.Code)
	}
	s.expires = time.Now().Add(time.Hour)
	s.close()
	if got := request(previewID, nil); got.Code != http.StatusUnauthorized {
		t.Fatal("revoked pairing exported a snapshot", got.Code)
	}
}
