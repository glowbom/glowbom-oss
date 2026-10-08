// Created by Codex under Jacob's direction, Glowbom Labs.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountProjectDownloadRenamesAndHidesCLIDetails(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	parent := t.TempDir()
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	b := newAccountBridge(func(_ context.Context, args ...string) ([]byte, error) {
		got = append([]string{}, args...)
		if len(args) != 3 || args[0] != "export" || args[1] != "--output" {
			t.Fatalf("command: %v", args)
		}
		if filepath.Dir(args[2]) != resolved || filepath.Base(args[2]) != "Glowbom" {
			t.Fatalf("destination: %s", args[3])
		}
		if err := os.Mkdir(args[2], 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(args[2], "glowbom.json"), []byte(`{"name":"Garden journal","idToken":"secret"}`), 0600); err != nil {
			t.Fatal(err)
		}
		return []byte("Downloading saved project...\nExported project: " + args[2] + "\nSaved 8 files with your generated code in its platform locations.\nsecret-token\n"), nil
	})
	request := httptest.NewRequest(http.MethodPost, "/account/project", strings.NewReader(`{"parent":"`+parent+`","command":"rm"}`))
	request.Header.Set("Authorization", "Bearer fixture-token")
	response := httptest.NewRecorder()
	b.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "secret-token") || strings.Contains(response.Body.String(), "rm") {
		t.Fatal(response.Body.String())
	}
	var saved accountProjectResponse
	if json.Unmarshal(response.Body.Bytes(), &saved) != nil || saved.Version != 1 || saved.Files != 8 || filepath.Base(saved.Path) != "Garden journal" {
		t.Fatal(response.Body.String())
	}
	if filepath.Dir(saved.Path) != resolved {
		t.Fatalf("project escaped parent: %s", saved.Path)
	}
	if _, err := os.Stat(filepath.Join(saved.Path, "glowbom.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(resolved, "Glowbom")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging folder remained")
	}
	if len(got) != 3 {
		t.Fatal(got)
	}
}

func TestAccountProjectDownloadRejectsUnsafeParentsAndResults(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	parent := t.TempDir()
	file := filepath.Join(parent, "notes.txt")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	b := newAccountBridge(func(context.Context, ...string) ([]byte, error) {
		calls++
		return []byte("Exported project: /tmp/elsewhere\nsecret\n"), nil
	})
	for _, parentPath := range []string{"", "relative/project", file, parent + "/missing", string(append([]byte(parent), 0))} {
		request := httptest.NewRequest(http.MethodPost, "/account/project", strings.NewReader(`{"parent":`+jsonString(parentPath)+`}`))
		request.Header.Set("Authorization", "Bearer fixture-token")
		response := httptest.NewRecorder()
		b.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"invalid_output"`) || (parentPath != "" && strings.Contains(response.Body.String(), parentPath)) {
			t.Fatalf("%q: %d %s", parentPath, response.Code, response.Body.String())
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/account/project", strings.NewReader(`{"parent":`+jsonString(parent)+`}`))
	request.Header.Set("Authorization", "Bearer fixture-token")
	response := httptest.NewRecorder()
	b.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || calls != 1 || strings.Contains(response.Body.String(), "elsewhere") || strings.Contains(response.Body.String(), "secret") {
		t.Fatal(response.Body.String())
	}
}

func TestAccountProjectDownloadUsesCLICodes(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	parent := t.TempDir()
	for _, tc := range []struct {
		output string
		err    error
		status int
		code   string
	}{
		{"private diagnostic", &accountCommandFailure{code: "project_not_found"}, http.StatusNotFound, "project_not_found"},
		{"private diagnostic", &accountCommandFailure{code: "sign_in_required"}, http.StatusForbidden, "sign_in_required"},
		{"private diagnostic", &accountCommandFailure{code: "not_a_real_code"}, http.StatusServiceUnavailable, "export_failed"},
		{"", errAccountCLIMissing, http.StatusServiceUnavailable, "cli_unavailable"},
		{"private diagnostic", errors.New("exit"), http.StatusServiceUnavailable, "export_failed"},
	} {
		b := newAccountBridge(func(context.Context, ...string) ([]byte, error) {
			return []byte(tc.output), tc.err
		})
		request := httptest.NewRequest(http.MethodPost, "/account/project", strings.NewReader(`{"parent":`+jsonString(parent)+`}`))
		request.Header.Set("Authorization", "Bearer fixture-token")
		response := httptest.NewRecorder()
		b.ServeHTTP(response, request)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"code":"`+tc.code+`"`) || strings.Contains(response.Body.String(), "private") {
			t.Fatalf("%s: %d %s", tc.code, response.Code, response.Body.String())
		}
	}
}

func TestAccountProjectNamesStayInsideTheParent(t *testing.T) {
	if projectFolderName("../secret") != "secret" || projectFolderName("Garden/journal") != "Garden journal" || projectFolderName("  ") != "" || projectFolderName(".") != "" {
		t.Fatal(projectFolderName("../secret"), projectFolderName("Garden/journal"))
	}
	parent := t.TempDir()
	if _, ok := accountProjectChild(parent, "../secret"); ok {
		t.Fatal("accepted a parent escape")
	}
	if _, code := accountProjectResult(nil, context.DeadlineExceeded, context.DeadlineExceeded, "/tmp/Glowbom"); code != "export_timeout" {
		t.Fatal(code)
	}
}

func TestInstalledExportFailureDoesNotEchoCLIText(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "glowbom")
	script := "#!/bin/sh\necho \"Glowbom: you are not signed in; run glowbom login first\" >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLOWBOM_CLI_BIN", bin)
	_, err := runAccountCLI(context.Background(), "export", "--output", filepath.Join(dir, "Glowbom"))
	var failure *accountCommandFailure
	if !errors.As(err, &failure) || failure.code != "sign_in_required" || strings.Contains(err.Error(), "signed") || strings.Contains(err.Error(), "Glowbom:") {
		t.Fatal(err)
	}
}

func TestAccountProjectDownloadReturnsSavedPromptAndDrawing(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	parent := t.TempDir()
	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
	b := newAccountBridge(func(_ context.Context, args ...string) ([]byte, error) {
		if err := os.Mkdir(args[2], 0700); err != nil {
			t.Fatal(err)
		}
		inputs := filepath.Join(args[2], "inputs")
		if err := os.Mkdir(inputs, 0700); err != nil {
			t.Fatal(err)
		}
		manifest := `{"name":"Garden journal","idToken":"secret","promptPath":"inputs/prompt.txt","initialDrawingPath":"inputs/sketch.png","iconPath":"inputs/icon.png"}`
		if err := os.WriteFile(filepath.Join(args[2], "glowbom.json"), []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inputs, "prompt.txt"), []byte("  Build a garden journal.\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inputs, "sketch.png"), png, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inputs, "icon.png"), []byte("not-the-drawing"), 0600); err != nil {
			t.Fatal(err)
		}
		return []byte("Exported project: " + args[2] + "\nSaved 4 files.\n"), nil
	})
	request := httptest.NewRequest(http.MethodPost, "/account/project", strings.NewReader(`{"parent":`+jsonString(parent)+`}`))
	request.Header.Set("Authorization", "Bearer fixture-token")
	response := httptest.NewRecorder()
	b.ServeHTTP(response, request)
	var saved accountProjectResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &saved) != nil {
		t.Fatal(response.Body.String())
	}
	if saved.Prompt != "Build a garden journal." || saved.DrawingType != "image/png" || saved.Drawing != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("prompt %q type %q", saved.Prompt, saved.DrawingType)
	}
	if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "not-the-drawing") || strings.Contains(response.Body.String(), "icon.png") {
		t.Fatal(response.Body.String())
	}
}

func TestAccountProjectDownloadSkipsUnsafeInputs(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	parent := t.TempDir()
	secret := []byte("hidden-token-value")
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), secret, 0600); err != nil {
		t.Fatal(err)
	}
	b := newAccountBridge(func(_ context.Context, args ...string) ([]byte, error) {
		if err := os.Mkdir(args[2], 0700); err != nil {
			t.Fatal(err)
		}
		inputs := filepath.Join(args[2], "inputs")
		if err := os.Mkdir(inputs, 0700); err != nil {
			t.Fatal(err)
		}
		manifest := `{"name":"Garden","promptPath":"../secret.txt","initialDrawingPath":"inputs/linked.txt"}`
		if err := os.WriteFile(filepath.Join(args[2], "glowbom.json"), []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(parent, "secret.txt"), filepath.Join(inputs, "linked.txt")); err != nil {
			t.Fatal(err)
		}
		return []byte("Exported project: " + args[2] + "\nSaved 3 files.\n"), nil
	})
	request := httptest.NewRequest(http.MethodPost, "/account/project", strings.NewReader(`{"parent":`+jsonString(parent)+`}`))
	request.Header.Set("Authorization", "Bearer fixture-token")
	response := httptest.NewRecorder()
	b.ServeHTTP(response, request)
	var saved accountProjectResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &saved) != nil || saved.Prompt != "" || saved.Drawing != "" || strings.Contains(response.Body.String(), "hidden-token-value") {
		t.Fatal(response.Code, response.Body.String())
	}
}

func jsonString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
