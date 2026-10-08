package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func acpSettingsFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings", "acp.json")
	t.Setenv("GLOWBOM_ACP_CONFIG", path)
	t.Setenv("GLOWBOM_SERVER_TOKEN", "acp-test-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	return path
}

func acpSettingsRequest(method, body string) *http.Request {
	request := httptest.NewRequest(method, "http://127.0.0.1/settings/acp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer acp-test-token")
	return request
}

func TestACPSettingsRoundTripDoesNotStartAnAgent(t *testing.T) {
	path := acpSettingsFixture(t)
	marker := filepath.Join(t.TempDir(), "agent-started")
	binary := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	profiles := []acpProfile{{ID: "acp-2", Name: "My agent", Command: binary, Args: []string{"acp", "a literal argument", "$(not executed)"}, Model: "provider/selected-model"}}
	data, _ := json.Marshal(acpSettings{Connections: profiles})
	for _, method := range []string{http.MethodPut, http.MethodGet} {
		recorder := httptest.NewRecorder()
		acpSettingsHandler()(recorder, acpSettingsRequest(method, string(data)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", method, recorder.Code, recorder.Body.String())
		}
		var settings acpSettings
		if err := json.Unmarshal(recorder.Body.Bytes(), &settings); err != nil || !reflect.DeepEqual(settings.Connections, profiles) {
			t.Fatalf("profiles changed: %+v, %v", settings, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("reading or saving a connection launched its executable")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("saved settings mode = %o", info.Mode().Perm())
	}
	profile, err := resolveACPProfile("acp-2")
	if err != nil || !reflect.DeepEqual(profile, profiles[0]) {
		t.Fatalf("resolve = %+v, %v", profile, err)
	}
	if err := saveACPProfiles(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveACPProfile("acp-2"); err == nil {
		t.Fatal("removed connection still resolves")
	}
}

func TestACPSettingsRejectsInvalidProfilesWithoutReplacingSavedOnes(t *testing.T) {
	acpSettingsFixture(t)
	valid := acpProfile{ID: "acp-1", Name: "Saved", Command: "agent", Args: []string{"acp"}}
	if err := saveACPProfiles([]acpProfile{valid}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`,
		`null`,
		`{"connections":null}`,
		`{"connections":[{"id":"acp-4","name":"Agent","command":"agent","args":[]}]}`,
		`{"connections":[{"id":"acp-1","name":"Agent","command":"agent"},{"id":"acp-1","name":"Other","command":"other"}]}`,
		`{"connections":[{"id":"acp-1","name":"Agent","command":"agent","env":{"KEY":"private"}}]}`,
		`{"connections":[{"id":"acp-1","name":"Agent","command":"agent\nrun"}]}`,
		`{"connections":[{"id":"acp-1","name":"Agent","command":"agent","args":["bad\u0000arg"]}]}`,
		`{"connections":[{"id":"acp-1","name":"Agent","command":"agent","model":"bad\nmodel"}]}`,
		`{"connections":[{"id":"acp-1","name":"Agent","command":"agent","model":"bad\u2028model"}]}`,
		`{"connections":[{"id":"acp-1","name":"Agent","command":"agent","model":"   "}]}`,
		`{"connections":[{"id":"acp-1","name":"Agent","command":"agent","model":123}]}`,
		`{"connections":[{"id":"acp-1","name":"Agent","command":"agent","model":"` + strings.Repeat("x", 257) + `"}]}`,
		`{"connections":[{"id":"acp-1","name":"","command":"agent"}]}`,
		`{"connections":[]} {"connections":[]}`,
		`{"connections":[],"ignored":"private"}`,
		`{"connections":[{"id":"acp-1","name":"Agent","command":"` + strings.Repeat("x", acpMaxConfigBytes) + `"}]}`,
	} {
		recorder := httptest.NewRecorder()
		acpSettingsHandler()(recorder, acpSettingsRequest(http.MethodPut, body))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid body status = %d", recorder.Code)
		}
		profiles, err := loadACPProfiles()
		if err != nil || !reflect.DeepEqual(profiles, []acpProfile{valid}) {
			t.Fatal("invalid save replaced existing connections")
		}
	}
	for _, profiles := range [][]acpProfile{
		{valid, valid, valid, valid},
		{{ID: "acp-1", Name: strings.Repeat("x", 81), Command: "agent"}},
		{{ID: "acp-1", Name: "Agent", Command: "agent", Args: make([]string, 33)}},
		{{ID: "acp-1", Name: "Agent", Command: "agent", Args: []string{strings.Repeat("x", 4097)}}},
	} {
		if validateACPProfiles(profiles) == nil {
			t.Fatal("accepted an oversized connection")
		}
	}
}

func TestACPSettingsAndTestRequireAuthenticationAndAllowedOrigin(t *testing.T) {
	acpSettingsFixture(t)
	called := false
	probe := acpTestHandlerWithProbe(func(context.Context, acpProfile) (acpProbeResult, error) {
		called = true
		return acpProbeResult{}, nil
	})
	for _, handler := range []http.HandlerFunc{acpSettingsHandler(), probe} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPost} {
			request := acpSettingsRequest(method, `{}`)
			request.Header.Del("Authorization")
			recorder := httptest.NewRecorder()
			handler(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("unauthorized %s returned %d", method, recorder.Code)
			}
			request = acpSettingsRequest(method, `{}`)
			request.Header.Set("Origin", "https://untrusted.invalid")
			recorder = httptest.NewRecorder()
			handler(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("untrusted origin returned %d", recorder.Code)
			}
		}
	}
	if called {
		t.Fatal("unauthorized test launched an agent")
	}
}

func TestACPSettingsRejectsEscapedConfigurationOverflow(t *testing.T) {
	acpSettingsFixture(t)
	saved := acpProfile{ID: "acp-1", Name: "Saved", Command: "agent"}
	if err := saveACPProfiles([]acpProfile{saved}); err != nil {
		t.Fatal(err)
	}
	profiles := []acpProfile{
		{ID: "acp-1", Name: "First", Command: "agent"},
		{ID: "acp-2", Name: "Second", Command: "agent"},
		{ID: "acp-3", Name: "Third", Command: "agent"},
	}
	for index := range profiles {
		profiles[index].Args = []string{strings.Repeat("<", 4096), strings.Repeat("<", 4096)}
	}
	if err := saveACPProfiles(profiles); err == nil {
		t.Fatal("saved arguments whose JSON encoding exceeds the readable file size")
	}
	loaded, err := resolveACPProfile(saved.ID)
	if err != nil || loaded.Name != saved.Name || loaded.Command != saved.Command {
		t.Fatal("oversized save replaced the existing connection")
	}
}

func TestACPConnectionTestIsExplicitAndDoesNotSave(t *testing.T) {
	path := acpSettingsFixture(t)
	want := acpProfile{ID: "acp-3", Name: "Unsaved", Command: "agent", Args: []string{"--acp", "literal value"}}
	called := 0
	probe := acpTestHandlerWithProbe(func(ctx context.Context, profile acpProfile) (acpProbeResult, error) {
		called++
		if !reflect.DeepEqual(profile, want) {
			t.Fatal("probe changed command arguments")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("connection test has no timeout")
		}
		return acpProbeResult{Name: "Agent", Version: "1", Images: true, LoadSession: true}, nil
	})
	data, _ := json.Marshal(map[string]any{"connection": want})
	recorder := httptest.NewRecorder()
	probe(recorder, acpSettingsRequest(http.MethodGet, string(data)))
	if recorder.Code != http.StatusMethodNotAllowed || called != 0 {
		t.Fatal("GET started a connection test")
	}
	recorder = httptest.NewRecorder()
	probe(recorder, acpSettingsRequest(http.MethodPost, string(data)))
	if recorder.Code != http.StatusOK || called != 1 {
		t.Fatalf("test result = %d, calls = %d", recorder.Code, called)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("connection test saved its draft")
	}
	recorder = httptest.NewRecorder()
	acpTestHandlerWithProbe(func(context.Context, acpProfile) (acpProbeResult, error) {
		return acpProbeResult{}, errors.New("private-token-in-process-error")
	})(recorder, acpSettingsRequest(http.MethodPost, string(data)))
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "private-token") {
		t.Fatal("probe diagnostics were exposed")
	}
}

func TestACPProfileFingerprintTracksConfigurationAndKeepsArgumentsLiteral(t *testing.T) {
	profile := acpProfile{ID: "acp-1", Name: "Agent", Command: "/path with spaces/agent", Args: []string{"--acp", "a b"}}
	first := acpProfileFingerprint(profile)
	if len(first) != 64 || first != acpProfileFingerprint(profile) {
		t.Fatal("profile fingerprint is unstable")
	}
	profile.Args = []string{"--acp", "a", "b"}
	if first == acpProfileFingerprint(profile) {
		t.Fatal("different literal argument boundaries share a fingerprint")
	}
	profile.Args = nil
	first = acpProfileFingerprint(profile)
	profile.Args = []string{}
	if first != acpProfileFingerprint(profile) {
		t.Fatal("empty argument representations changed the fingerprint")
	}
	profile.Command = "another-agent"
	if first == acpProfileFingerprint(profile) {
		t.Fatal("changed executable shares the old session namespace")
	}
}

func TestACPModelPreferencePreservesDefaultProfilesAndChangesSessionScope(t *testing.T) {
	path := acpSettingsFixture(t)
	const oldProfile = `{"id":"acp-1","name":"Agent","command":"agent","args":[]}`
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"connections":[`+oldProfile+`]}`), 0600); err != nil {
		t.Fatal(err)
	}
	profile, err := resolveACPProfile("acp-1")
	if err != nil || profile.Model != "" {
		t.Fatalf("legacy profile = %+v, %v", profile, err)
	}
	encoded, err := json.Marshal(profile)
	if err != nil || string(encoded) != oldProfile {
		t.Fatalf("default profile serialization changed: %s, %v", encoded, err)
	}
	defaultFingerprint := acpProfileFingerprint(profile)
	profile.Model = "provider/selected-model"
	if acpProfileFingerprint(profile) == defaultFingerprint {
		t.Fatal("selected model reused the default model's session scope")
	}
	if err := saveACPProfiles([]acpProfile{profile}); err != nil {
		t.Fatal(err)
	}
	loaded, err := resolveACPProfile("acp-1")
	if err != nil || loaded.Model != profile.Model {
		t.Fatalf("model preference did not round trip: %+v, %v", loaded, err)
	}
	profile.Model = ""
	if defaultFingerprint != acpProfileFingerprint(profile) {
		t.Fatal("returning to agent default changed the original session scope")
	}
}

func TestACPSettingsEmptyAndMalformedFiles(t *testing.T) {
	path := acpSettingsFixture(t)
	profiles, err := loadACPProfiles()
	if err != nil || profiles == nil || len(profiles) != 0 {
		t.Fatal("missing settings must return an empty configured list")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"connections":"private-invalid-data"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = loadACPProfiles()
	if err == nil || strings.Contains(err.Error(), "private-invalid-data") || strings.Contains(err.Error(), path) {
		t.Fatal("malformed settings should fail without exposing file data")
	}
}
