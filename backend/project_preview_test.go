package main

import (
	"bytes"
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
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func previewFixture(t *testing.T, root, path, content string) {
	t.Helper()
	name := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func previewCall(t *testing.T, h http.Handler, req previewRequest) ([]previewTarget, int) {
	t.Helper()
	data, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4569/preview", bytes.NewReader(data))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var result struct {
		Targets []previewTarget `json:"targets"`
	}
	if w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("invalid response: %s", w.Body.String())
	}
	return result.Targets, w.Code
}

func TestPreviewDetectsFrameworksAndCustomFolders(t *testing.T) {
	project, _ := filepath.EvalSymlinks(t.TempDir())
	previewFixture(t, project, "prototype/index.html", "<h1>Prototype</h1>")
	previewFixture(t, project, "web/package.json", `{"dependencies":{"next":"16.1.6"}}`)
	previewFixture(t, project, "experiments/react/package.json", `{"devDependencies":{"vite":"6.2.0"}}`)
	previewFixture(t, project, "node_modules/vite/bin/vite.js", "// installed")
	for _, tc := range []struct {
		dir, kind string
		install   bool
	}{{"prototype", "static", false}, {"web", "next", true}, {"experiments/react", "vite", false}} {
		v := inspectPreviewDefinition(project, previewDefinition{ID: "custom-test", Name: "Test", Directory: tc.dir})
		if !v.Available || v.Kind != tc.kind || v.NeedsInstall != tc.install {
			t.Fatalf("%s: %+v", tc.dir, v)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(project, "outside")); err != nil {
		t.Fatal(err)
	}
	if inspectPreviewTarget(project, "outside").Available {
		t.Fatal("accepted a target outside the project")
	}
	previewFixture(t, project, "unknown/package.json", `{"scripts":{"dev":"different-server"}}`)
	previewFixture(t, project, "unknown/index.html", "<h1>Needs compilation</h1>")
	if inspectPreviewTarget(project, "unknown").Available {
		t.Fatal("treated an unknown server app as static HTML")
	}
}

func TestPreviewStaticAccessAndRefresh(t *testing.T) {
	project, _ := filepath.EvalSymlinks(t.TempDir())
	previewFixture(t, project, "prototype/index.html", "<h1>Version one</h1>")
	previewFixture(t, project, "prototype/assets/icon.svg", "<svg></svg>")
	previewFixture(t, project, "prototype/.env", "SECRET=private")
	previewFixture(t, project, "private.txt", "outside preview")
	_ = os.Symlink("../private.txt", filepath.Join(project, "prototype", "escape.txt"))
	_ = os.Symlink(".env", filepath.Join(project, "prototype", "alias.txt"))
	m := newProjectPreviewManager()
	defer m.Close()
	views, code := previewCall(t, m, previewRequest{Path: project, Target: "prototype", Action: "start"})
	if code != 200 || views[0].Status != "running" {
		t.Fatalf("start: %d %+v", code, views)
	}
	v := views[0]
	u, _ := url.Parse(v.URL)
	base := "http://" + u.Host
	res, err := http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("unprotected preview: %d", res.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: time.Second}
	res, err = client.Get(v.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), "Version one") || res.Request.URL.RawQuery != "" {
		t.Fatalf("bootstrap: %d %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), `/__glowbom_preview__/reload.js`) {
		t.Fatal("standalone preview has no live reload script")
	}
	res, err = client.Get(base + "/__glowbom_preview__/revision")
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(revision) != v.Revision {
		t.Fatal("standalone revision differs from embedded preview")
	}
	for _, path := range []string{"/.env", "/escape.txt", "/alias.txt", "/assets/", "/../private.txt", "/%2e%2e/private.txt"} {
		res, err = client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Fatalf("exposed %s: %d", path, res.StatusCode)
		}
	}
	res, err = client.Get(base + "/assets/icon.svg")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("asset failed")
	}
	previewFixture(t, project, "prototype/index.html", "<h1>Version two with a change</h1>")
	views, _ = previewCall(t, m, previewRequest{Path: project, Action: "inspect"})
	if views[0].Revision == v.Revision {
		t.Fatal("file edit did not change revision")
	}
	res, err = client.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), "Version two") {
		t.Fatal("served stale HTML")
	}
	req, _ := http.NewRequest(http.MethodGet, base, nil)
	req.Host = "evil.example:" + u.Port()
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("accepted foreign host")
	}
	previewCall(t, m, previewRequest{Path: project, Action: "stop", Target: "prototype", ID: "stale-id"})
	views, _ = previewCall(t, m, previewRequest{Path: project, Action: "inspect"})
	if views[0].Status != "running" {
		t.Fatal("stale stop killed a newer session")
	}
	previewCall(t, m, previewRequest{Path: project, Action: "stop", Target: "prototype", ID: v.ID})
	if res, err = client.Get(base); err == nil {
		res.Body.Close()
		t.Fatal("preview still listening after stop")
	}
}

func TestPreviewSettingsAndAuthorization(t *testing.T) {
	project, _ := filepath.EvalSymlinks(t.TempDir())
	m := newProjectPreviewManager()
	defer m.Close()
	d := previewDefinition{Name: "Another React app", Directory: "another-app", Description: "React and TypeScript with Vite. Preserve the prototype's design.", Preset: "react-vite"}
	views, code := previewCall(t, m, previewRequest{Path: project, Action: "save", Config: &d})
	if code != 200 || len(views) != 3 {
		t.Fatalf("save %d %+v", code, views)
	}
	defs, err := readPreviewDefinitions(project)
	if err != nil || defs[2].Name != d.Name || defs[2].Description != d.Description || views[2].Description != d.Description || defs[2].Preset != d.Preset {
		t.Fatalf("reload %v %v", defs, err)
	}
	tooLarge := append([]previewDefinition(nil), defs...)
	tooLarge[2].Command = []string{strings.Repeat("x", 64*1024)}
	if err := savePreviewDefinitions(project, tooLarge); err == nil {
		t.Fatal("saved settings too large to read back")
	}
	if saved, err := readPreviewDefinitions(project); err != nil || len(saved[2].Command) != 0 {
		t.Fatal("failed save damaged the previous settings")
	}
	d = defs[2]
	d.Description = strings.Repeat("x", 8001)
	_, code = previewCall(t, m, previewRequest{Path: project, Action: "save", Config: &d})
	if code != 400 {
		t.Fatal("allowed an oversized stack description")
	}
	d = defs[2]
	d.Directory = "../escape"
	_, code = previewCall(t, m, previewRequest{Path: project, Action: "save", Config: &d})
	if code != 400 {
		t.Fatal("allowed escaping directory")
	}
	d.Directory = "another-app"
	d.Command = []string{"server", "--port", "{port}"}
	_, code = previewCall(t, m, previewRequest{Path: project, Action: "save", Config: &d})
	if code != 400 {
		t.Fatal("allowed command without loopback host")
	}
	d.Command = []string{"server", "--host", "{host}", "--port", "{port}"}
	_, code = previewCall(t, m, previewRequest{Path: project, Action: "save", Config: &d})
	if code != 200 {
		t.Fatal("valid custom command rejected")
	}
	views, code = previewCall(t, m, previewRequest{Path: project, Action: "remove", Target: d.ID})
	if code != 200 || len(views) != 2 {
		t.Fatal("remove failed")
	}
	t.Setenv("GLOWBOM_SERVER_TOKEN", "test-only-token")
	_, code = previewCall(t, withGlowbomSecurity(m), previewRequest{Path: project, Action: "inspect"})
	if code != 401 {
		t.Fatal("preview API bypassed backend authentication")
	}
	if err := os.Remove(filepath.Join(project, ".glowbom", "previews.json")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, ".glowbom", "previews.json")); err != nil {
		t.Fatal(err)
	}
	if err := savePreviewDefinitions(project, defs); err == nil {
		t.Fatal("wrote settings outside project")
	}
	data, _ := os.ReadFile(outside)
	if string(data) != "unchanged" {
		t.Fatal("modified unrelated file")
	}
}

func TestPreviewDoesNotInheritBackendCredentials(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "private-backend-token")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "private-password")
	t.Setenv("OPENAI_API_KEY", "private-provider-key")
	env := strings.Join(previewProcessEnv(), "\n")
	if strings.Contains(env, "private-") {
		t.Fatal("inherited backend credentials")
	}
	if !strings.Contains(env, "HOST=127.0.0.1") {
		t.Fatal("missing loopback host")
	}
}

// The test binary is a real child server, so no framework download is needed.
func TestPreviewProcessHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--" || len(os.Args) < i+4 {
			continue
		}
		if os.Args[i+1] == "fail" {
			os.Exit(3)
		}
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if os.Getenv("GLOWBOM_SERVER_TOKEN") != "" || os.Getenv("OPENAI_API_KEY") != "" {
				http.Error(w, "inherited private credentials", 500)
				return
			}
			_, _ = io.WriteString(w, "custom preview is ready")
		})
		if err := http.ListenAndServe(net.JoinHostPort(os.Args[i+2], os.Args[i+3]), handler); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
}

func TestPreviewCustomProcessLifecycle(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "private-backend-token")
	t.Setenv("OPENAI_API_KEY", "private-provider-key")
	project, _ := filepath.EvalSymlinks(t.TempDir())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := newProjectPreviewManager()
	defer m.Close()
	d := previewDefinition{ID: "custom-test", Name: "Test server", Directory: ".", Command: []string{executable, "-test.run=^TestPreviewProcessHelper$", "--", "serve", "{host}", "{port}"}}
	if _, code := previewCall(t, m, previewRequest{Path: project, Action: "save", Config: &d}); code != 200 {
		t.Fatal("could not save custom server")
	}
	previewCall(t, m, previewRequest{Path: project, Action: "start", Target: d.ID})
	waitForStatus := func(status string) previewTarget {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			views, code := previewCall(t, m, previewRequest{Path: project, Action: "inspect"})
			if code == 200 && len(views) == 3 && views[2].Status == status {
				return views[2]
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("custom server never became %s", status)
		return previewTarget{}
	}
	v := waitForStatus("running")
	client := &http.Client{Timeout: time.Second}
	res, err := client.Get(v.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "custom preview is ready" {
		t.Fatalf("unexpected preview: %s", body)
	}
	// Stop must work even when an editor has temporarily broken the settings.
	previewFixture(t, project, ".glowbom/previews.json", "{")
	if _, code := previewCall(t, m, previewRequest{Path: project, Action: "stop", Target: d.ID, ID: v.ID}); code != 200 {
		t.Fatal("broken settings prevented stopping the server")
	}
	stopped := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		res, err = client.Get(v.URL)
		if err != nil {
			stopped = true
			break
		}
		res.Body.Close()
		time.Sleep(20 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("custom server still listening after stop")
	}
	d.Command[3] = "fail"
	if err := savePreviewDefinitions(project, []previewDefinition{{ID: "prototype"}, {ID: "web"}, d}); err != nil {
		t.Fatal(err)
	}
	previewCall(t, m, previewRequest{Path: project, Action: "start", Target: d.ID})
	failed := waitForStatus("failed")
	if failed.URL != "" || !strings.Contains(failed.Error, "exited") {
		t.Fatalf("server failure was not surfaced: %+v", failed)
	}
}

func TestPreviewLogsRedactSplitCredentials(t *testing.T) {
	s := &previewSession{}
	_, _ = s.Write([]byte("Authorization: Bear"))
	_, _ = s.Write([]byte("er private-token\nAPI_KEY=private-key\n"))
	_, _ = s.Write([]byte("password=unfinished-private-password"))
	logs := strings.Join(s.snapshot().Logs, "\n")
	if strings.Contains(logs, "private") || !strings.Contains(logs, "[redacted]") {
		t.Fatalf("credentials were exposed: %s", logs)
	}
}

func TestStackFolderAvailabilityAndBoundaries(t *testing.T) {
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(project, "existing app")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	_, supported := folderOpenLaunchCommand()
	target := inspectPreviewDefinition(project, previewDefinition{ID: "custom-folder", Directory: "existing app", PreviewMode: "none"})
	if target.CanOpenFolder != supported || target.directory != directory {
		t.Fatalf("wrong folder capability: %#v", target)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(project, "web")); err != nil {
		t.Fatal(err)
	}
	manager := newProjectPreviewManager()
	for _, id := range []string{"prototype", "web", "unknown"} {
		_, status := previewCall(t, manager, previewRequest{Path: project, Target: id, Action: "folder"})
		if status != http.StatusBadRequest {
			t.Fatalf("folder %s: expected rejection, got %d", id, status)
		}
	}
	if inspectPreviewTarget(project, "web").CanOpenFolder {
		t.Fatal("outside folder must not be openable")
	}
}

func TestPreviewReadinessLoopbackFamilies(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			host := "127.0.0.1"
			if network == "tcp6" {
				host = "::1"
			}
			listener, err := net.Listen(network, net.JoinHostPort(host, "0"))
			if err != nil {
				t.Skipf("loopback unavailable: %v", err)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
			server.Listener = listener
			server.Start()
			defer server.Close()
			port := listener.Addr().(*net.TCPAddr).Port
			client := &http.Client{Timeout: time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
			got := respondingPreviewURL(context.Background(), client, port)
			if got != server.URL {
				t.Fatalf("expected reachable URL %s, got %s", server.URL, got)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if got := respondingPreviewURL(ctx, client, port); got != "" {
				t.Fatalf("canceled probe returned %s", got)
			}
		})
	}
}
