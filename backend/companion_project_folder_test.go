package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCompanionOpenProjectFolderUsesScopedDesktopLauncher(t *testing.T) {
	var executable string
	switch runtime.GOOS {
	case "darwin":
		executable = "open"
	case "linux":
		executable = "xdg-open"
	default:
		t.Skip("the isolated command fixture uses a POSIX shell")
	}
	bin := t.TempDir()
	logPath := filepath.Join(bin, "opened-arguments")
	if err := os.WriteFile(filepath.Join(bin, executable), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"${0%/*}/opened-arguments\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	manager := newProjectPreviewManager()
	t.Cleanup(manager.Close)
	s := testCompanion(t, manager)
	project := sharedCompanionProject(t, s)
	request := func(method, id, body string, change func(*http.Request)) *httptest.ResponseRecorder {
		r := companionRequest(s, method, "/projects/"+id+"/open", body)
		if change != nil {
			change(r)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	catalog := httptest.NewRecorder()
	s.ServeHTTP(catalog, companionRequest(s, http.MethodGet, "/projects/"+project.ID+"/build-targets", ""))
	var targets struct{ Editors []stackEditor }
	if catalog.Code != http.StatusOK || json.Unmarshal(catalog.Body.Bytes(), &targets) != nil || len(targets.Editors) == 0 || targets.Editors[0] != (stackEditor{ID: "folder", Name: stackFileManagerLabel()}) {
		t.Fatal("shared project did not offer the existing Desktop folder launcher")
	}
	for _, test := range []struct {
		name, id, body string
		change         func(*http.Request)
		want           int
	}{
		{name: "unauthenticated", id: project.ID, body: `{"editor":"folder"}`, change: func(r *http.Request) { r.Header.Del("Authorization") }, want: http.StatusUnauthorized},
		{name: "foreign browser", id: project.ID, body: `{"editor":"folder"}`, change: func(r *http.Request) { r.Header.Set("Origin", "https://example.test") }, want: http.StatusForbidden},
		{name: "unshared project", id: "not-shared", body: `{"editor":"folder"}`, want: http.StatusNotFound},
		{name: "phone path", id: project.ID, body: `{"editor":"folder","path":"/private"}`, want: http.StatusBadRequest},
		{name: "arbitrary executable", id: project.ID, body: `{"editor":"/bin/sh"}`, want: http.StatusBadRequest},
		{name: "outside target", id: project.ID, body: `{"editor":"folder","target":"../private"}`, want: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			if response := request(http.MethodPost, test.id, test.body, test.change); response.Code != test.want {
				t.Fatalf("folder request returned %d, want %d", response.Code, test.want)
			}
		})
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("a rejected phone request launched a Desktop command")
	}
	response := request(http.MethodPost, project.ID, `{"editor":"folder"}`, nil)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), project.path) {
		t.Fatal("shared project folder did not open without exposing its path", response.Code)
	}
	var arguments []byte
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		arguments, _ = os.ReadFile(logPath)
		if len(arguments) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	expected := []string{project.path}
	if runtime.GOOS == "darwin" {
		expected = append([]string{"-R"}, expected...)
	}
	if !reflect.DeepEqual(strings.Split(strings.TrimSuffix(string(arguments), "\n"), "\n"), expected) {
		t.Fatal("folder command did not use exactly the paired project and existing platform arguments")
	}
}

func TestCompanionProjectFolderRequiresReportedCapability(t *testing.T) {
	for _, tools := range []string{
		`{"editors":[]}`,
		`{"editors":[],"canOpenFolder":false}`,
		`{"editors":[{"id":"folder","name":"Unverified folder action"}]}`,
	} {
		t.Run(tools, func(t *testing.T) {
			opened := false
			s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request previewRequest
				_ = json.NewDecoder(r.Body).Decode(&request)
				if request.Action == "tools" {
					io.WriteString(w, tools)
				} else {
					opened = true
				}
			}))
			project := sharedCompanionProject(t, s)
			editors, err := s.projectEditors(t.Context(), project)
			if err != nil || len(editors) != 0 {
				t.Fatal("old or unavailable Desktop folder capability appeared enabled")
			}
			response := httptest.NewRecorder()
			s.ServeHTTP(response, companionRequest(s, http.MethodPost, "/projects/"+project.ID+"/open", `{"editor":"folder"}`))
			if response.Code != http.StatusBadRequest || opened {
				t.Fatal("folder action bypassed Desktop capability detection")
			}
		})
	}
}
