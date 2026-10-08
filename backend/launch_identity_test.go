package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestHealthIdentifiesLaunchWithoutCredentials(t *testing.T) {
	t.Setenv("GLOWBOM_INSTANCE", "desktop-dev")
	t.Setenv("GLOWBOM_LAUNCH_ID", "one-launch")
	t.Setenv("GLOWBOM_SERVER_TOKEN", "private-backend-token")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "private-agent-password")
	response := httptest.NewRecorder()
	glowbomHealthHandler(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var value glowbomHealthResponse
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if !value.OK || value.Instance != "desktop-dev" || value.LaunchID != "one-launch" {
		t.Fatalf("unexpected identity: %+v", value)
	}
	if strings.Contains(response.Body.String(), "private-") {
		t.Fatal("health exposed credentials")
	}
}

func TestInstanceDoesNotDiscoverForeignAgentProcesses(t *testing.T) {
	t.Setenv("GLOWBOM_INSTANCE", "oss")
	t.Setenv("GLOWBOM_DESKTOP", "")
	// With no owned process, no external process lookup should run at all.
	t.Setenv("PATH", t.TempDir())
	if err := stopOpenCodeServerOnPort("4593"); err != nil {
		t.Fatalf("instance tried to discover an unowned process: %v", err)
	}
}

func TestInstanceStopsOnlyOwnedAgentProcess(t *testing.T) {
	t.Setenv("GLOWBOM_INSTANCE", "desktop-dev")
	t.Setenv("GLOWBOM_DESKTOP", "")
	child := exec.Command(os.Args[0], "-test.run=^TestOwnedAgentProcessHelper$")
	child.Env = append(os.Environ(), "GLOWBOM_OWNED_AGENT_HELPER=1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })
	desktopOpenCodeProcess.Lock()
	desktopOpenCodeProcess.process = child.Process
	desktopOpenCodeProcess.Unlock()
	if err := stopOpenCodeServerOnPort("4593"); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("owned helper unexpectedly exited successfully")
	}
	desktopOpenCodeProcess.Lock()
	defer desktopOpenCodeProcess.Unlock()
	if desktopOpenCodeProcess.process != nil {
		t.Fatal("stopped agent still recorded")
	}
}

func TestOwnedAgentProcessHelper(t *testing.T) {
	if os.Getenv("GLOWBOM_OWNED_AGENT_HELPER") == "1" {
		time.Sleep(time.Minute)
		os.Exit(9)
	}
}
