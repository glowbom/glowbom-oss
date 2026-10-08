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

func TestAppleIntelligenceConfigMergesWithInlineOpenCodeConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveAppleIntelligenceEnabled(true); err != nil {
		t.Fatal(err)
	}
	env, err := addAppleIntelligenceOpenCodeConfig([]string{
		`OPENCODE_CONFIG_CONTENT={"theme":"system","provider":{"local":{"name":"Local"}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(envValue(env, "OPENCODE_CONFIG_CONTENT")), &config); err != nil {
		t.Fatal(err)
	}
	if config["theme"] != "system" {
		t.Fatal("existing inline config was lost")
	}
	providers := config["provider"].(map[string]any)
	if providers["local"] == nil || providers[appleIntelligenceProvider] == nil {
		t.Fatal("provider config was not merged")
	}
	apple := providers[appleIntelligenceProvider].(map[string]any)
	models := apple["models"].(map[string]any)
	model := models[appleIntelligenceModel].(map[string]any)
	if model["tool_call"] != true {
		t.Fatal("Apple Intelligence was not enabled for experimental tool use")
	}
}

func TestAppleIntelligenceConfigRejectsInvalidInlineConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveAppleIntelligenceEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := addAppleIntelligenceOpenCodeConfig([]string{"OPENCODE_CONFIG_CONTENT=not-json"}); err == nil {
		t.Fatal("invalid inline config was replaced")
	}
}

func TestAppleIntelligenceStatePersistsPrivately(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if appleIntelligenceEnabled() {
		t.Fatal("Apple Intelligence defaulted on")
	}
	if err := saveAppleIntelligenceEnabled(true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".glowbom", "apple-intelligence.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 || !appleIntelligenceEnabled() {
		t.Fatalf("unexpected state file mode=%v enabled=%v", info.Mode().Perm(), appleIntelligenceEnabled())
	}
	if err := saveAppleIntelligenceEnabled(false); err != nil {
		t.Fatal(err)
	}
	if appleIntelligenceEnabled() {
		t.Fatal("Apple Intelligence remained enabled")
	}
}

func TestAppleIntelligenceSettingsHandler(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	status := appleIntelligenceStatus{Supported: true, Installed: true, Running: true, Enabled: true, Reason: "Ready"}
	enables, disables := 0, 0
	handler := appleIntelligenceSettingsHandlerWithDependencies(appleIntelligenceSettingsDependencies{
		status: func(context.Context) appleIntelligenceStatus { return status },
		enable: func(context.Context) error {
			enables++
			return nil
		},
		disable: func(context.Context) error {
			disables++
			return nil
		},
	})
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		request := httptest.NewRequest(method, "/settings/apple-intelligence", nil)
		request.Header.Set("Authorization", "Bearer token")
		recorder := httptest.NewRecorder()
		handler(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"enabled":true`) {
			t.Fatalf("%s returned %d: %s", method, recorder.Code, recorder.Body.String())
		}
	}
	if enables != 1 || disables != 1 {
		t.Fatalf("enables=%d disables=%d", enables, disables)
	}
}

func TestAppleIntelligenceSettingsHandlerProtectsAndRedacts(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	handler := appleIntelligenceSettingsHandlerWithDependencies(appleIntelligenceSettingsDependencies{
		status:  func(context.Context) appleIntelligenceStatus { return appleIntelligenceStatus{} },
		enable:  func(context.Context) error { return errors.New("private /Users/name/path") },
		disable: func(context.Context) error { return nil },
	})
	unauthorized := httptest.NewRecorder()
	handler(unauthorized, httptest.NewRequest(http.MethodGet, "/settings/apple-intelligence", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated request was allowed")
	}
	request := httptest.NewRequest(http.MethodPost, "/settings/apple-intelligence", nil)
	request.Header.Set("Authorization", "Bearer token")
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "/Users/name") {
		t.Fatalf("setup error was not safely reported: %s", recorder.Body.String())
	}
}

func TestAppleIntelligenceSettingsHandlerShowsPlainLanguageProblems(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	handler := appleIntelligenceSettingsHandlerWithDependencies(appleIntelligenceSettingsDependencies{
		status: func(context.Context) appleIntelligenceStatus { return appleIntelligenceStatus{} },
		enable: func(context.Context) error {
			return appleIntelligenceUserError("Turn on Apple Intelligence in System Settings.")
		},
		disable: func(context.Context) error { return nil },
	})
	request := httptest.NewRequest(http.MethodPost, "/settings/apple-intelligence", nil)
	request.Header.Set("Authorization", "Bearer token")
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "Turn on Apple Intelligence in System Settings.") {
		t.Fatalf("plain-language problem was hidden: %s", recorder.Body.String())
	}
}

func TestAppleIntelligenceBinaryUsesPathFirst(t *testing.T) {
	bin := t.TempDir()
	tool := filepath.Join(bin, "glowbom-test-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if found, err := appleIntelligenceBinary("glowbom-test-tool"); err != nil || found != tool {
		t.Fatalf("tool on PATH was not found: %q %v", found, err)
	}
	if _, err := appleIntelligenceBinary("glowbom-missing-tool"); err == nil {
		t.Fatal("missing tool was reported as installed")
	}
}
