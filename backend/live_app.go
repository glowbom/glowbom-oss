package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errLiveAppNotInstalled = errors.New("Glowbom Live is not installed")

type liveAppTarget struct {
	executable string
}

type liveAppService struct {
	platform string
	discover func(context.Context) (liveAppTarget, error)
	running  func(context.Context, liveAppTarget) (bool, error)
	start    func(liveAppTarget, []string) (<-chan error, error)
	mu       sync.Mutex
	done     <-chan error
}

func newLiveAppHandler() http.Handler {
	return &liveAppService{
		platform: runtime.GOOS,
		discover: discoverLiveApp,
		running:  liveAppRunning,
		start:    startLiveApp,
	}
}

func (s *liveAppService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fail := func(status int, code, message string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
	}
	token := glowbomServerToken()
	if token == "" || !hasValidGlowbomServerToken(r, token) {
		fail(http.StatusUnauthorized, "auth_required", "Local authentication is required.")
		return
	}
	if !isAllowedOrigin(r, glowbomAllowedOrigins()) {
		fail(http.StatusForbidden, "origin_not_allowed", "This request is not allowed.")
		return
	}
	backendURL, err := liveAppBackendURL()
	if err != nil {
		fail(http.StatusServiceUnavailable, "local_backend_required", "Glowbom Live needs a local backend connection.")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		fail(http.StatusMethodNotAllowed, "method_not_allowed", "Use GET to check or POST to open Glowbom Live.")
		return
	}
	if s.platform != "darwin" {
		if r.Method == http.MethodPost {
			fail(http.StatusNotImplemented, "unsupported_platform", "Open Glowbom Live from your applications on this platform.")
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"installed": false, "platform": s.platform, "launchSupported": false})
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	target, err := s.discover(ctx)
	if err != nil && !errors.Is(err, errLiveAppNotInstalled) {
		fail(http.StatusServiceUnavailable, "discovery_failed", "Could not check for Glowbom Live. Try again.")
		return
	}
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]any{"installed": err == nil, "platform": s.platform, "launchSupported": true})
		return
	}
	if err != nil {
		fail(http.StatusNotFound, "not_installed", "Install Glowbom Live in Applications, then try again.")
		return
	}
	// Serialize launches so repeated clicks cannot create duplicate game windows.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		select {
		case <-s.done:
			s.done = nil
		default:
			fail(http.StatusConflict, "already_running", "Glowbom Live is already open. Close it and try again to connect automatically.")
			return
		}
	}
	running, err := s.running(ctx, target)
	if err != nil {
		fail(http.StatusServiceUnavailable, "launch_failed", "Could not open Glowbom Live. Try again.")
		return
	}
	if running {
		fail(http.StatusConflict, "already_running", "Glowbom Live is already open. Close it and try again to connect automatically.")
		return
	}
	// The token is handed to this one child process, never arguments, links, or logs.
	environment := append(liveAppBaseEnvironment(os.Environ()),
		"GLOWBOM_SERVER_TOKEN="+token,
		"GLOWBOM_BACKEND_URL="+backendURL,
		"GLOWBOM_LIVE_AUTOSTART=1",
	)
	done, err := s.start(target, environment)
	if err != nil {
		fail(http.StatusServiceUnavailable, "launch_failed", "Could not open Glowbom Live. Try again.")
		return
	}
	s.done = done
	// Catch immediate loader failures without waiting for the application to exit.
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-done:
		s.done = nil
		fail(http.StatusServiceUnavailable, "launch_failed", "Could not open Glowbom Live. Try again.")
		return
	case <-timer.C:
	case <-r.Context().Done():
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func liveAppBackendURL() (string, error) {
	host := backendBindHost()
	if !isLoopbackHost(host) {
		return "", errors.New("backend must bind to loopback")
	}
	port := os.Getenv("GLOWBOM_PORT")
	if port == "" {
		port = "4569"
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return "", errors.New("backend port is invalid")
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func discoverLiveApp(ctx context.Context) (liveAppTarget, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return liveAppTarget{}, err
	}
	// Only normal installed locations are inspected. Requests cannot provide paths.
	for _, app := range []string{"/Applications/Glowbom Live.app", filepath.Join(home, "Applications", "Glowbom Live.app")} {
		target, err := inspectLiveApp(ctx, app, liveAppBundleExecutable)
		if err == nil {
			return target, nil
		}
		if !errors.Is(err, errLiveAppNotInstalled) {
			return liveAppTarget{}, err
		}
	}
	return liveAppTarget{}, errLiveAppNotInstalled
}

func inspectLiveApp(ctx context.Context, app string, readExecutable func(context.Context, string) (string, error)) (liveAppTarget, error) {
	root, err := filepath.EvalSymlinks(app)
	if err != nil {
		if os.IsNotExist(err) {
			return liveAppTarget{}, errLiveAppNotInstalled
		}
		return liveAppTarget{}, err
	}
	plist := filepath.Join(root, "Contents", "Info.plist")
	info, err := os.Stat(plist)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return liveAppTarget{}, errLiveAppNotInstalled
	}
	name, err := readExecutable(ctx, plist)
	if err != nil {
		return liveAppTarget{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00\r\n") {
		return liveAppTarget{}, errLiveAppNotInstalled
	}
	executable, err := filepath.EvalSymlinks(filepath.Join(root, "Contents", "MacOS", name))
	if err != nil {
		return liveAppTarget{}, errLiveAppNotInstalled
	}
	if !strings.HasPrefix(executable, filepath.Join(root, "Contents", "MacOS")+string(filepath.Separator)) {
		return liveAppTarget{}, errLiveAppNotInstalled
	}
	info, err = os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return liveAppTarget{}, errLiveAppNotInstalled
	}
	return liveAppTarget{executable: executable}, nil
}

func liveAppBundleExecutable(ctx context.Context, plist string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "CFBundleExecutable", "raw", "-o", "-", plist)
	cmd.Env = liveAppBaseEnvironment(os.Environ())
	output, err := cmd.Output()
	return string(output), err
}

func liveAppRunning(ctx context.Context, target liveAppTarget) (bool, error) {
	cmd := exec.CommandContext(ctx, "/bin/ps", "-ww", "-axo", "comm=")
	cmd.Env = liveAppBaseEnvironment(os.Environ())
	output, err := cmd.Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == target.executable {
			return true, nil
		}
	}
	return false, nil
}

func startLiveApp(target liveAppTarget, environment []string) (<-chan error, error) {
	cmd := exec.Command(target.executable)
	cmd.Dir = filepath.Dir(target.executable)
	cmd.Env = environment
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	return done, nil
}

func liveAppBaseEnvironment(environment []string) []string {
	var result []string
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "__CF_USER_TEXT_ENCODING":
			result = append(result, entry)
		}
	}
	return result
}
