package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func companionDefaultDestinationFixture(t *testing.T, s *companionSession, requestID string) companionImportDestination {
	t.Helper()
	w := companionTransferCall(t, s, "/projects/import/default-destination", map[string]string{"requestId": requestID})
	var grant companionImportDestination
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &grant) != nil || grant.Canceled || normalizedStudioUUID(grant.ID) == "" || grant.DisplayPath == "" || grant.ExpiresAt == "" {
		t.Fatal("default destination failed", w.Code, w.Body.String())
	}
	return grant
}

func TestCompanionDefaultDestinationCreatesWithoutNativeChooser(t *testing.T) {
	studio := canonicalDirectory(t.TempDir())
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	s := testCompanion(t, http.NotFoundHandler())
	s.pickImportFolder = func(context.Context) (string, bool, error) {
		t.Fatal("default destination opened the native chooser")
		return "", false, nil
	}
	grant := companionDefaultDestinationFixture(t, s, "AEFD6EF0-DC32-4DE0-AEE4-9C5FC0933A41")
	if grant.Source != "managed" || grant.DisplayPath != filepath.Join(studio, "PhoneProjects") {
		t.Fatal("default destination did not use managed project storage", grant)
	}
	if files, err := os.ReadDir(grant.DisplayPath); err != nil || len(files) != 0 {
		t.Fatal("preparing a destination left project or probe files", files, err)
	}
	if _, err := os.Stat(filepath.Join(studio, companionDestinationPreferenceFile)); !os.IsNotExist(err) {
		t.Fatal("managed fallback overwrote the explicit folder preference")
	}
	request := companionProjectImageFixture(t)
	request.DestinationID = grant.ID
	w := companionTransferCall(t, s, "/projects/import", request)
	var receipt struct {
		companionProject
		SavedPath string `json:"savedPath"`
	}
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.ID == "" || filepath.Dir(receipt.SavedPath) != grant.DisplayPath {
		t.Fatal("default folder could not create a complete project", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(receipt.SavedPath, "glowbom.json")); err != nil {
		t.Fatal("default project lost its manifest", err)
	}
}

func TestCompanionDefaultDestinationRemembersOnlyExplicitChoices(t *testing.T) {
	studio := canonicalDirectory(t.TempDir())
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	parent := canonicalDirectory(t.TempDir())
	first := testCompanion(t, http.NotFoundHandler())
	chosen := companionDestinationFixture(t, first, parent, companionTransferTestID)
	data, err := os.ReadFile(filepath.Join(studio, companionDestinationPreferenceFile))
	var preference map[string]string
	if err != nil || json.Unmarshal(data, &preference) != nil || len(preference) != 1 || preference["parentPath"] != parent || strings.Contains(string(data), chosen.ID) || strings.Contains(string(data), first.pairing.Token) {
		t.Fatal("folder preference persisted more than the selected path", err)
	}
	first.cancel()
	second := testCompanion(t, http.NotFoundHandler())
	grant := companionDefaultDestinationFixture(t, second, companionTransferTestID)
	if grant.Source != "recent" || grant.DisplayPath != parent || grant.ID == chosen.ID {
		t.Fatal("new pairing did not recreate a grant for the intended parent", grant)
	}
	if _, _, err := second.importDestinationParent(chosen.ID); err == nil {
		t.Fatal("new pairing restored the old grant")
	}
	second.pickImportFolder = func(context.Context) (string, bool, error) { return "", true, nil }
	w := companionTransferCall(t, second, "/projects/import/destination", map[string]string{"requestId": "AEFD6EF0-DC32-4DE0-AEE4-9C5FC0933A41"})
	if w.Code != http.StatusOK || rememberedCompanionDestination() != parent {
		t.Fatal("canceling a new choice erased the previous parent")
	}
	if files, err := os.ReadDir(parent); err != nil || len(files) != 0 {
		t.Fatal("reading a remembered destination left a file", files, err)
	}
}

func TestCompanionDefaultDestinationFallsBackFromInvalidPreference(t *testing.T) {
	for _, kind := range []string{"missing", "file", "symlink", "relative", "malformed", "record symlink", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			studio := canonicalDirectory(t.TempDir())
			t.Setenv("GLOWBOM_STUDIO_DIR", studio)
			path := filepath.Join(canonicalDirectory(t.TempDir()), "selected")
			preferencePath := filepath.Join(studio, companionDestinationPreferenceFile)
			switch kind {
			case "file":
				if err := os.WriteFile(path, []byte("owner file"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			case "relative":
				path = "../outside"
			}
			data, _ := json.Marshal(map[string]string{"parentPath": path})
			switch kind {
			case "malformed":
				data = []byte("not json")
			case "oversized":
				data = []byte(strings.Repeat("x", (32<<10)+1))
			case "record symlink":
				outside := filepath.Join(t.TempDir(), "record.json")
				if os.WriteFile(outside, data, 0600) != nil || os.Symlink(outside, preferencePath) != nil {
					t.Fatal("could not prepare symlink preference")
				}
			}
			if kind != "record symlink" {
				if err := os.WriteFile(preferencePath, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			grant := companionDefaultDestinationFixture(t, testCompanion(t, http.NotFoundHandler()), companionTransferTestID)
			if grant.Source != "managed" || grant.DisplayPath != filepath.Join(studio, "PhoneProjects") {
				t.Fatal("invalid preference escaped the managed fallback", grant)
			}
		})
	}
}

func TestCompanionDefaultDestinationRejectsManagedSymlink(t *testing.T) {
	studio := canonicalDirectory(t.TempDir())
	outside := canonicalDirectory(t.TempDir())
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	if err := os.Symlink(outside, filepath.Join(studio, "PhoneProjects")); err != nil {
		t.Fatal(err)
	}
	s := testCompanion(t, http.NotFoundHandler())
	w := companionTransferCall(t, s, "/projects/import/default-destination", map[string]string{"requestId": companionTransferTestID})
	if w.Code != http.StatusServiceUnavailable || len(s.importDestinations) != 0 || len(s.projects) != 0 {
		t.Fatal("managed symlink received a grant", w.Code)
	}
	if files, err := os.ReadDir(outside); err != nil || len(files) != 0 {
		t.Fatal("managed symlink wrote outside project storage", files, err)
	}
}

func TestCompanionDefaultDestinationFallsBackFromReadOnlyFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires ordinary Unix directory permissions")
	}
	studio := canonicalDirectory(t.TempDir())
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	parent := canonicalDirectory(t.TempDir())
	if err := rememberCompanionDestination(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
	grant := companionDefaultDestinationFixture(t, testCompanion(t, http.NotFoundHandler()), companionTransferTestID)
	if grant.Source != "managed" || grant.DisplayPath != filepath.Join(studio, "PhoneProjects") || rememberedCompanionDestination() != parent {
		t.Fatal("unwritable preference did not safely fall back", grant)
	}
}

func TestCompanionDefaultDestinationRetryExpiryAndPairScope(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.NotFoundHandler())
	first := companionDefaultDestinationFixture(t, s, companionTransferTestID)
	retry := companionDefaultDestinationFixture(t, s, companionTransferTestID)
	if first.ID != retry.ID || first.ExpiresAt != retry.ExpiresAt {
		t.Fatal("uncertain default request created another grant")
	}
	other := testCompanion(t, http.NotFoundHandler())
	if _, _, err := other.importDestinationParent(first.ID); err == nil {
		t.Fatal("default destination escaped its paired session")
	}
	s.now = func() time.Time { return time.Now().Add(companionDestinationLifetime + time.Second) }
	if _, _, err := s.importDestinationParent(first.ID); err == nil {
		t.Fatal("expired default destination was accepted")
	}
	renewed := companionDefaultDestinationFixture(t, s, companionTransferTestID)
	if renewed.ID == first.ID || len(s.importDestinations) != 1 {
		t.Fatal("expired destination was reused or leaked grants")
	}
	s.cancel()
	if _, _, err := s.importDestinationParent(renewed.ID); err == nil {
		t.Fatal("revoked default destination was accepted")
	}
}

func TestCompanionDefaultDestinationRetainsPairingAndInputBoundaries(t *testing.T) {
	studio := canonicalDirectory(t.TempDir())
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	s := testCompanion(t, http.NotFoundHandler())
	path := "/projects/import/default-destination"
	for _, body := range []string{`{"requestId":"invalid"}`, `{"requestId":"` + companionTransferTestID + `","path":"/private"}`, `{"requestId":"` + companionTransferTestID + `","parentPath":"/private"}`} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, http.MethodPost, path, body))
		if w.Code != http.StatusBadRequest {
			t.Fatal("default destination accepted a client path", w.Code)
		}
	}
	for _, variant := range []string{"missing token", "origin", "public peer", "expired", "revoked"} {
		isolated := testCompanion(t, http.NotFoundHandler())
		request := companionRequest(isolated, http.MethodPost, path, `{"requestId":"`+companionTransferTestID+`"}`)
		switch variant {
		case "missing token":
			request.Header.Del("Authorization")
		case "origin":
			request.Header.Set("Origin", "https://untrusted.example")
		case "public peer":
			request.RemoteAddr = "8.8.8.8:50000"
		case "expired":
			isolated.expires = time.Now().Add(-time.Second)
		case "revoked":
			isolated.cancel()
		}
		w := httptest.NewRecorder()
		isolated.ServeHTTP(w, request)
		if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Fatal("untrusted pairing prepared a save folder", variant, w.Code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := companionRequest(s, http.MethodPost, path, `{"requestId":"`+companionTransferTestID+`"}`).WithContext(ctx)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, canceled)
	if w.Code != http.StatusConflict || len(s.importDestinations) != 0 {
		t.Fatal("canceled request prepared a destination", w.Code)
	}
	if files, err := os.ReadDir(studio); err != nil || len(files) != 0 {
		t.Fatal("invalid destination request mutated storage", files, err)
	}
}
