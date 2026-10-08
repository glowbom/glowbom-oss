package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func liveAppFixture(t *testing.T) *liveAppService {
	t.Helper()
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-live-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	t.Setenv("GLOWBOM_PORT", "4569")
	t.Setenv("GLOWBOM_ALLOWED_ORIGINS", "http://localhost:4572")
	return &liveAppService{
		platform: "darwin",
		discover: func(context.Context) (liveAppTarget, error) {
			return liveAppTarget{executable: "/Applications/Glowbom Live.app/Contents/MacOS/Glowbom Live"}, nil
		},
		running: func(context.Context, liveAppTarget) (bool, error) { return false, nil },
		start:   func(liveAppTarget, []string) (<-chan error, error) { return make(chan error), nil },
	}
}

func liveAppRequest(s *liveAppService, method string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/live/app", nil)
	r.Header.Set("Origin", "http://localhost:4572")
	r.Header.Set("X-Glowbom-Token", "fixture-live-token")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestLiveAppRequiresLocalAuthenticationBeforeDiscovery(t *testing.T) {
	for _, name := range []string{"missing token", "wrong token", "untrusted origin", "no configured token", "nonlocal backend"} {
		t.Run(name, func(t *testing.T) {
			s := liveAppFixture(t)
			s.discover = func(context.Context) (liveAppTarget, error) {
				t.Fatal("unauthorized discovery")
				return liveAppTarget{}, nil
			}
			r := httptest.NewRequest(http.MethodPost, "/live/app", nil)
			r.Header.Set("Origin", "http://localhost:4572")
			r.Header.Set("X-Glowbom-Token", "fixture-live-token")
			status := http.StatusUnauthorized
			switch name {
			case "missing token":
				r.Header.Del("X-Glowbom-Token")
			case "wrong token":
				r.Header.Set("X-Glowbom-Token", "incorrect")
			case "untrusted origin":
				r.Header.Set("Origin", "https://untrusted.example")
				status = http.StatusForbidden
			case "no configured token":
				t.Setenv("GLOWBOM_SERVER_TOKEN", "")
			case "nonlocal backend":
				t.Setenv("GLOWBOM_BIND_HOST", "0.0.0.0")
				status = http.StatusServiceUnavailable
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != status {
				t.Fatalf("got %d, want %d", w.Code, status)
			}
			if strings.Contains(w.Body.String(), "fixture-live-token") {
				t.Fatal("token exposed")
			}
		})
	}
}

func TestLiveAppInstalledStatusDoesNotLaunch(t *testing.T) {
	s := liveAppFixture(t)
	s.start = func(liveAppTarget, []string) (<-chan error, error) { t.Fatal("GET launched app"); return nil, nil }
	w := liveAppRequest(s, http.MethodGet)
	var status struct {
		Installed       bool   `json:"installed"`
		Platform        string `json:"platform"`
		LaunchSupported bool   `json:"launchSupported"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || !status.Installed || status.Platform != "darwin" || !status.LaunchSupported {
		t.Fatalf("unexpected status: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "/Applications") || strings.Contains(w.Body.String(), "token") {
		t.Fatal("status exposed local details")
	}
	s.discover = func(context.Context) (liveAppTarget, error) { return liveAppTarget{}, errLiveAppNotInstalled }
	if w = liveAppRequest(s, http.MethodGet); w.Code != 200 || !strings.Contains(w.Body.String(), `"installed":false`) {
		t.Fatal("missing app status incorrect")
	}
	if w = liveAppRequest(s, http.MethodPost); w.Code != 404 || !strings.Contains(w.Body.String(), `"code":"not_installed"`) {
		t.Fatal("missing app launch should fail")
	}
}

func TestLiveAppUnsupportedPlatformIsExplicit(t *testing.T) {
	s := liveAppFixture(t)
	s.platform = "linux"
	s.discover = func(context.Context) (liveAppTarget, error) {
		t.Fatal("unsupported discovery")
		return liveAppTarget{}, nil
	}
	w := liveAppRequest(s, http.MethodGet)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"launchSupported":false`) || !strings.Contains(w.Body.String(), `"installed":false`) {
		t.Fatalf("unexpected status: %s", w.Body.String())
	}
	if liveAppRequest(s, http.MethodPost).Code != 501 {
		t.Fatal("unsupported launch should fail")
	}
}

func TestLiveAppLaunchHandsOnlyIntendedCredentialsToChild(t *testing.T) {
	s := liveAppFixture(t)
	t.Setenv("OPENAI_API_KEY", "unrelated-secret")
	t.Setenv("GLOWBOM_BACKEND_URL", "https://untrusted.example")
	t.Setenv("GLOWBOM_PORT", "4581")
	done := make(chan error, 1)
	starts := 0
	s.start = func(target liveAppTarget, environment []string) (<-chan error, error) {
		starts++
		if target.executable != "/Applications/Glowbom Live.app/Contents/MacOS/Glowbom Live" {
			t.Fatal("unexpected executable")
		}
		values := map[string]string{}
		for _, entry := range environment {
			key, value, _ := strings.Cut(entry, "=")
			values[key] = value
		}
		if values["GLOWBOM_SERVER_TOKEN"] != "fixture-live-token" || values["GLOWBOM_BACKEND_URL"] != "http://127.0.0.1:4581" || values["GLOWBOM_LIVE_AUTOSTART"] != "1" {
			t.Fatal("missing launch contract")
		}
		if values["OPENAI_API_KEY"] != "" || values["GLOWBY_SERVER_TOKEN"] != "" {
			t.Fatal("unrelated credentials inherited")
		}
		return done, nil
	}
	w := liveAppRequest(s, http.MethodPost)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"success":true}` {
		t.Fatalf("unexpected launch response: %s", w.Body.String())
	}
	if liveAppRequest(s, http.MethodPost).Code != 409 || starts != 1 {
		t.Fatal("duplicate launch was not prevented")
	}
	done <- nil
	if liveAppRequest(s, http.MethodPost).Code != 200 || starts != 2 {
		t.Fatal("could not reopen after exit")
	}
}

func TestLiveAppExistingProcessDoesNotStartAnother(t *testing.T) {
	s := liveAppFixture(t)
	s.running = func(context.Context, liveAppTarget) (bool, error) { return true, nil }
	s.start = func(liveAppTarget, []string) (<-chan error, error) {
		t.Fatal("started duplicate process")
		return nil, nil
	}
	w := liveAppRequest(s, http.MethodPost)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"already_running"`) {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}
}

func TestLiveAppImmediateExitReportsSafeFailure(t *testing.T) {
	s := liveAppFixture(t)
	s.start = func(liveAppTarget, []string) (<-chan error, error) {
		done := make(chan error, 1)
		done <- errors.New("loader failure fixture-live-token")
		close(done)
		return done, nil
	}
	w := liveAppRequest(s, http.MethodPost)
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"launch_failed"`) || strings.Contains(w.Body.String(), "fixture-live-token") {
		t.Fatalf("unsafe immediate exit response: %s", w.Body.String())
	}
	if s.done != nil {
		t.Fatal("failed launch was kept active")
	}
}

func TestLiveAppFailuresAreSanitized(t *testing.T) {
	for _, operation := range []string{"discover", "running", "start"} {
		t.Run(operation, func(t *testing.T) {
			s := liveAppFixture(t)
			err := errors.New("process failure fixture-live-token /private/home")
			switch operation {
			case "discover":
				s.discover = func(context.Context) (liveAppTarget, error) { return liveAppTarget{}, err }
			case "running":
				s.running = func(context.Context, liveAppTarget) (bool, error) { return false, err }
			case "start":
				s.start = func(liveAppTarget, []string) (<-chan error, error) { return nil, err }
			}
			w := liveAppRequest(s, http.MethodPost)
			if w.Code != 503 || strings.Contains(w.Body.String(), "fixture-live-token") || strings.Contains(w.Body.String(), "/private") {
				t.Fatalf("unsafe response: %s", w.Body.String())
			}
		})
	}
}

func TestLiveAppBundleDiscoveryRestrictsExecutableToBundle(t *testing.T) {
	app := filepath.Join(t.TempDir(), "Glowbom Live.app")
	macos := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(macos, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(macos, "Glowbom Live")
	if err := os.WriteFile(executable, []byte("not executed"), 0700); err != nil {
		t.Fatal(err)
	}
	read := func(name string) func(context.Context, string) (string, error) {
		return func(context.Context, string) (string, error) { return name, nil }
	}
	target, err := inspectLiveApp(context.Background(), app, read("Glowbom Live"))
	resolvedExecutable, _ := filepath.EvalSymlinks(executable)
	if err != nil || target.executable != resolvedExecutable {
		t.Fatalf("valid bundle was rejected: %v", err)
	}
	for _, name := range []string{"", "..", "../../outside", "/bin/sh", "bad\nname", "bad\\name"} {
		if _, err := inspectLiveApp(context.Background(), app, read(name)); !errors.Is(err, errLiveAppNotInstalled) {
			t.Fatalf("accepted unsafe executable %q", name)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("not executed"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(macos, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectLiveApp(context.Background(), app, read("escape")); !errors.Is(err, errLiveAppNotInstalled) {
		t.Fatal("accepted executable outside app bundle")
	}
	if err := os.Chmod(executable, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectLiveApp(context.Background(), app, read("Glowbom Live")); !errors.Is(err, errLiveAppNotInstalled) {
		t.Fatal("accepted nonexecutable file")
	}
}

func TestLiveAppBackendURLIgnoresUntrustedRequestHost(t *testing.T) {
	liveAppFixture(t)
	t.Setenv("GLOWBOM_BIND_HOST", "::1")
	t.Setenv("GLOWBOM_PORT", "4582")
	value, err := liveAppBackendURL()
	if err != nil || value != "http://[::1]:4582" {
		t.Fatalf("incorrect local URL: %q %v", value, err)
	}
	for _, port := range []string{"0", "65536", "4582/path", "https://untrusted.example"} {
		t.Setenv("GLOWBOM_PORT", port)
		if _, err := liveAppBackendURL(); err == nil {
			t.Fatal("accepted invalid backend port")
		}
	}
}

func TestLiveAppEnvironmentExcludesOtherCredentialsAndInjection(t *testing.T) {
	input := []string{"PATH=/bin", "HOME=/home/user", "OPENAI_API_KEY=private", "BUZZ_PRIVATE_KEY=private", "GLOWBOM_SERVER_TOKEN=old", "GLOWBOM_BACKEND_URL=https://untrusted.example", "DYLD_INSERT_LIBRARIES=/tmp/inject", "GLOWBOM_LIVE_AUTOSTART=old"}
	if got := strings.Join(liveAppBaseEnvironment(input), "\n"); got != "PATH=/bin\nHOME=/home/user" {
		t.Fatalf("unexpected child environment: %s", got)
	}
}
