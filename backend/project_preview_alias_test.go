package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func previewAliasFixture(t *testing.T) (string, *projectPreviewManager, previewDefinition, previewDefinition) {
	t.Helper()
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	previewFixture(t, project, "web/index.html", "<h1>Preview</h1>")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := []string{executable, "-test.run=^TestPreviewProcessHelper$", "--", "serve", "{host}", "{port}"}
	first := previewDefinition{ID: "custom-first", Name: "First", Directory: "web", Command: command}
	second := previewDefinition{ID: "custom-second", Name: "Second", Directory: "web/.", Command: command}
	if err := savePreviewDefinitions(project, []previewDefinition{{ID: "prototype"}, {ID: "web"}, first, second}); err != nil {
		t.Fatal(err)
	}
	manager := newProjectPreviewManager()
	t.Cleanup(manager.Close)
	return project, manager, first, second
}

func waitForPreviewTarget(t *testing.T, manager *projectPreviewManager, project, target, status string) previewTarget {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last previewTarget
	for time.Now().Before(deadline) {
		views, code := previewCall(t, manager, previewRequest{Path: project, Action: "inspect"})
		for _, view := range views {
			if view.Target == target {
				last = view
			}
			if code == http.StatusOK && view.Target == target && view.Status == status {
				return view
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("preview %s never became %s: %+v", target, status, last)
	return previewTarget{}
}

func TestPreviewAliasesShareOneManagedServer(t *testing.T) {
	project, manager, first, second := previewAliasFixture(t)
	if _, code := previewCall(t, manager, previewRequest{Path: project, Target: first.ID, Action: "start"}); code != http.StatusOK {
		t.Fatal("could not start the first preview")
	}
	running := waitForPreviewTarget(t, manager, project, first.ID, "running")
	if _, code := previewCall(t, manager, previewRequest{Path: project, Target: second.ID, Action: "start"}); code != http.StatusOK {
		t.Fatal("could not open the same managed preview through its alias")
	}
	alias := waitForPreviewTarget(t, manager, project, second.ID, "running")
	if alias.ID != running.ID || alias.URL != running.URL {
		t.Fatalf("same folder started competing servers: first=%+v alias=%+v", running, alias)
	}
	if manager.sessions[project+"/"+first.ID] != manager.sessions[project+"/"+second.ID] {
		t.Fatal("aliases do not share process ownership")
	}
	// A stale stop request cannot revoke a newer preview through another name.
	previewCall(t, manager, previewRequest{Path: project, Target: second.ID, Action: "stop", ID: "stale-id"})
	if got := waitForPreviewTarget(t, manager, project, first.ID, "running"); got.ID != running.ID {
		t.Fatal("stale alias stop changed the running session")
	}
	previewCall(t, manager, previewRequest{Path: project, Target: second.ID, Action: "stop", ID: alias.ID})
	if len(manager.sessions) != 0 {
		t.Fatal("stopping the process left active aliases")
	}
	waitForPreviewTarget(t, manager, project, first.ID, "stopped")
	waitForPreviewTarget(t, manager, project, second.ID, "stopped")
	client := &http.Client{Timeout: time.Second}
	if response, err := client.Get(running.URL); err == nil {
		response.Body.Close()
		t.Fatal("owned server still runs after stopping its alias")
	}
	previewCall(t, manager, previewRequest{Path: project, Target: first.ID, Action: "start"})
	if restarted := waitForPreviewTarget(t, manager, project, first.ID, "running"); restarted.ID == running.ID {
		t.Fatal("restart retained the stopped process")
	}
}

func TestPreviewAliasesResolveCanonicalFoldersDuringStartup(t *testing.T) {
	project, manager, first, second := previewAliasFixture(t)
	second.Directory = "linked-web"
	if err := os.Symlink("web", filepath.Join(project, second.Directory)); err != nil {
		t.Skipf("folder symlinks unavailable: %v", err)
	}
	if err := savePreviewDefinitions(project, []previewDefinition{{ID: "prototype"}, {ID: "web"}, first, second}); err != nil {
		t.Fatal(err)
	}
	// A preview must be shared while starting, before its URL becomes available.
	previewCall(t, manager, previewRequest{Path: project, Target: first.ID, Action: "start"})
	previewCall(t, manager, previewRequest{Path: project, Target: second.ID, Action: "start"})
	if manager.sessions[project+"/"+first.ID] != manager.sessions[project+"/"+second.ID] {
		t.Fatal("a canonical folder alias launched another starting server")
	}
	running := waitForPreviewTarget(t, manager, project, first.ID, "running")
	alias := waitForPreviewTarget(t, manager, project, second.ID, "running")
	if alias.ID != running.ID || alias.URL != running.URL {
		t.Fatal("canonical aliases have different preview identities")
	}
}

func TestPreviewRejectsConflictingCommandInManagedFolder(t *testing.T) {
	project, manager, first, second := previewAliasFixture(t)
	second.Command = append(append([]string(nil), second.Command...), "different-configuration")
	if err := savePreviewDefinitions(project, []previewDefinition{{ID: "prototype"}, {ID: "web"}, first, second}); err != nil {
		t.Fatal(err)
	}
	previewCall(t, manager, previewRequest{Path: project, Target: first.ID, Action: "start"})
	running := waitForPreviewTarget(t, manager, project, first.ID, "running")
	if _, code := previewCall(t, manager, previewRequest{Path: project, Target: second.ID, Action: "start"}); code != http.StatusConflict {
		t.Fatalf("conflicting launch was accepted: HTTP %d", code)
	}
	if len(manager.sessions) != 1 {
		t.Fatal("conflicting launch changed process ownership")
	}
	if got := waitForPreviewTarget(t, manager, project, first.ID, "running"); got.ID != running.ID {
		t.Fatal("conflicting launch interrupted the existing server")
	}
}

func TestPreviewSettingsChangesStopEveryAlias(t *testing.T) {
	for _, action := range []string{"save", "remove"} {
		t.Run(action, func(t *testing.T) {
			project, manager, first, second := previewAliasFixture(t)
			previewCall(t, manager, previewRequest{Path: project, Target: first.ID, Action: "start"})
			running := waitForPreviewTarget(t, manager, project, first.ID, "running")
			previewCall(t, manager, previewRequest{Path: project, Target: second.ID, Action: "start"})
			request := previewRequest{Path: project, Target: second.ID, Action: action, ID: running.ID}
			if action == "save" {
				second.Name = "Changed preview"
				request.Config = &second
			}
			if _, code := previewCall(t, manager, request); code != http.StatusOK {
				t.Fatalf("could not %s the aliased preview", action)
			}
			if len(manager.sessions) != 0 {
				t.Fatal("a settings change left a stopped alias registered")
			}
			waitForPreviewTarget(t, manager, project, first.ID, "stopped")
		})
	}
}

func TestPreviewNextLockErrorsDoNotAdoptReportedServer(t *testing.T) {
	for _, message := range []string{
		"Another \x1b[36mnext dev\x1b[0m server is already running.\n- Local: http://127.0.0.1:45678\n- PID: 12345\n",
		"Unable to acquire lock at .next/dev/lock, is another instance of next dev running?\n",
	} {
		session := &previewSession{view: previewTarget{Kind: "next", Status: "starting"}}
		_, _ = session.Write([]byte(message))
		session.failServerExit(errors.New("exit status 1"))
		failed := session.snapshot()
		if failed.Status != "failed" || failed.URL != "" || !strings.Contains(failed.Error, "terminal or app that started it") {
			t.Fatalf("lock conflict was not explained safely: %+v", failed)
		}
		if strings.Contains(failed.Error, "12345") || strings.Contains(failed.Error, "45678") {
			t.Fatal("an unmanaged process became preview metadata")
		}
	}
	session := &previewSession{view: previewTarget{Kind: "next", Status: "starting"}}
	_, _ = session.Write([]byte("Invalid Next.js configuration\n"))
	session.failServerExit(errors.New("exit status 1"))
	if !strings.Contains(session.snapshot().Error, "preview server exited") {
		t.Fatal("an unrelated Next.js exit was treated as a lock conflict")
	}
}
