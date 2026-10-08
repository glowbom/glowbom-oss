package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func companionDestinationFixture(t *testing.T, s *companionSession, parent string, requestID string) companionImportDestination {
	t.Helper()
	s.pickImportFolder = func(context.Context) (string, bool, error) { return parent, false, nil }
	w := companionTransferCall(t, s, "/projects/import/destination", map[string]string{"requestId": requestID})
	var grant companionImportDestination
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &grant) != nil || grant.Canceled || normalizedStudioUUID(grant.ID) == "" || grant.DisplayPath != canonicalDirectory(parent) || grant.ExpiresAt == "" {
		t.Fatal("invalid selected save folder", w.Code, w.Body.String())
	}
	return grant
}

func TestCompanionTransferDestinationCancelStrictScopeAndCapability(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", directory)
	calls := 0
	s := testCompanion(t, http.NotFoundHandler())
	s.pickImportFolder = func(context.Context) (string, bool, error) { calls++; return "", true, nil }
	for _, body := range []string{`{"requestId":"invalid"}`, `{"requestId":"` + companionTransferTestID + `","path":"/private"}`, `{"requestId":"` + companionTransferTestID + `","start":"/private"}`} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, "POST", "/projects/import/destination", body))
		if w.Code != 400 {
			t.Fatal("unsafe folder request accepted", w.Code, w.Body.String())
		}
	}
	r := companionRequest(s, "POST", "/projects/import/destination", `{"requestId":"`+companionTransferTestID+`"}`)
	r.Header.Del("Authorization")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 || calls != 0 {
		t.Fatal("unauthorized request opened native picker")
	}
	w = companionTransferCall(t, s, "/projects/import/destination", map[string]string{"requestId": companionTransferTestID})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"canceled":true`) || len(s.importDestinations) != 0 || len(s.projects) != 0 {
		t.Fatal("cancellation saved a folder grant", w.Body.String())
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "GET", "/projects/import", ""))
	var capabilities struct {
		DestinationSelection bool   `json:"destinationSelection"`
		DefaultParentPath    string `json:"defaultParentPath"`
	}
	if json.Unmarshal(w.Body.Bytes(), &capabilities) != nil || !capabilities.DestinationSelection || capabilities.DefaultParentPath != filepath.Join(canonicalDirectory(directory), "PhoneProjects") {
		t.Fatal("missing destination/default capabilities", w.Body.String())
	}
	files, _ := os.ReadDir(directory)
	if len(files) != 0 {
		t.Fatal("choosing/canceling mutated project storage", files)
	}
}

func TestCompanionTransferDestinationChoiceRetryIsStable(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.NotFoundHandler())
	parent := canonicalDirectory(t.TempDir())
	calls := 0
	s.pickImportFolder = func(context.Context) (string, bool, error) { calls++; return parent, false, nil }
	payload := map[string]string{"requestId": companionTransferTestID}
	w := companionTransferCall(t, s, "/projects/import/destination", payload)
	first := w.Body.String()
	w = companionTransferCall(t, s, "/projects/import/destination", payload)
	if w.Code != 200 || w.Body.String() != first || calls != 1 || !strings.Contains(first, `"canceled":false`) {
		t.Fatal("retry reopened picker or changed grant", w.Body.String(), calls)
	}
	w = companionTransferCall(t, s, "/projects/import/destination", map[string]string{"requestId": "AEFD6EF0-DC32-4DE0-AEE4-9C5FC0933A41"})
	if w.Code != 200 || calls != 2 || w.Body.String() == first {
		t.Fatal("deliberate new choice used cached grant")
	}
}

func TestCompanionTransferDestinationPickerRevocationAndConcurrency(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.NotFoundHandler())
	entered := make(chan struct{})
	s.pickImportFolder = func(ctx context.Context) (string, bool, error) {
		close(entered)
		<-ctx.Done()
		return "", false, ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		companionTransferCall(t, s, "/projects/import/destination", map[string]string{"requestId": companionTransferTestID})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("picker did not open")
	}
	w := companionTransferCall(t, s, "/projects/import/destination", map[string]string{"requestId": "AEFD6EF0-DC32-4DE0-AEE4-9C5FC0933A41"})
	if w.Code != 409 {
		t.Fatal("concurrent picker accepted", w.Code)
	}
	s.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("revocation kept native picker active")
	}
	if len(s.importDestinations) != 0 {
		t.Fatal("revoked picker saved a grant")
	}
	next := testCompanion(t, http.NotFoundHandler())
	companionDestinationFixture(t, next, t.TempDir(), companionTransferTestID)
}

func TestCompanionTransferChosenFolderPreservesMediaBookAndRetry(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	parent := canonicalDirectory(t.TempDir())
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("folder transfer invoked a model") }))
	grant := companionDestinationFixture(t, s, parent, "AEFD6EF0-DC32-4DE0-AEE4-9C5FC0933A41")
	request := companionProjectImageFixture(t)
	request.Name = "Boiler Game"
	request.DestinationID = grant.ID
	w := companionTransferCall(t, s, "/projects/import", request)
	var receipt struct {
		companionProject
		DestinationID string `json:"destinationId"`
		StoragePath   string `json:"storagePath"`
		SavedPath     string `json:"savedPath"`
	}
	expected := filepath.Join(parent, "Boiler Game-c8b57d59")
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.DestinationID != grant.ID || receipt.StoragePath != expected || receipt.SavedPath != expected || receipt.AssetCount != 2 {
		t.Fatal("incorrect chosen transfer receipt", w.Code, w.Body.String())
	}
	project, ok := s.project(receipt.ID)
	if !ok || project.path != expected {
		t.Fatal("chosen project not shared", project)
	}
	fields, _, err := readStudioProjectState(project.path)
	if err != nil {
		t.Fatal(err)
	}
	studioID := studioJSONString(fields, "projectID")
	images, _, _, _, err := listStudioProjectCatalogPage(studioID, 0, 48, 0, 24)
	if err != nil || len(images) != 2 {
		t.Fatal("chosen project lost Studio links", len(images), err)
	}
	bookRoot, name, err := openProjectBookProject(project.path)
	if err != nil {
		t.Fatal(err)
	}
	book, exists, err := readProjectBook(bookRoot, name)
	bookRoot.Close()
	if err != nil || !exists || len(book.Entries) != 1 || len(book.Entries[0].Images) != 1 {
		t.Fatal("chosen project lost Book input", book, err)
	}
	next := testCompanion(t, http.NotFoundHandler())
	w = companionTransferCall(t, next, "/projects/import", request)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.StoragePath != expected || receipt.AssetCount != 2 {
		t.Fatal("re-pair duplicated or redirected saved transfer", w.Code, w.Body.String())
	}
	children, _ := os.ReadDir(parent)
	if len(children) != 1 {
		t.Fatal("retry made extra project folders", len(children))
	}
	request.DestinationID = "aefd6ef0-dc32-4de0-aee4-9c5fc0933a41"
	if w = companionTransferCall(t, next, "/projects/import", request); w.Code != 409 {
		t.Fatal("saved transfer redirected on retry", w.Code)
	}
}

func TestCompanionTransferDestinationExpiryIdentityAndPairScope(t *testing.T) {
	for _, change := range []string{"expired", "replaced", "symlink", "other-pair"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
			parent := canonicalDirectory(t.TempDir())
			s := testCompanion(t, http.NotFoundHandler())
			grant := companionDestinationFixture(t, s, parent, companionTransferTestID)
			request := companionProjectImageFixture(t)
			request.DestinationID = grant.ID
			switch change {
			case "expired":
				s.now = func() time.Time { return time.Now().Add(companionDestinationLifetime + time.Second) }
			case "replaced":
				if err := os.Rename(parent, parent+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(parent, parent+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), parent); err != nil {
					t.Fatal(err)
				}
			case "other-pair":
				s = testCompanion(t, http.NotFoundHandler())
			}
			w := companionTransferCall(t, s, "/projects/import", request)
			if w.Code != http.StatusGone || len(s.projects) != 0 {
				t.Fatal("unsafe selected folder accepted", w.Code, w.Body.String())
			}
			files, err := os.ReadDir(parent)
			if err != nil || len(files) != 0 {
				t.Fatal("invalid choice wrote project files", files, err)
			}
		})
	}
}

func TestCompanionTransferDestinationNeverOverwritesAndGrantSurvivesFailure(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	parent := canonicalDirectory(t.TempDir())
	s := testCompanion(t, http.NotFoundHandler())
	grant := companionDestinationFixture(t, s, parent, companionTransferTestID)
	request := companionProjectImageFixture(t)
	request.DestinationID = grant.ID
	child := filepath.Join(parent, companionChosenProjectFolder(request.Name, request.RequestID))
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "owner.txt"), []byte("keep existing"), 0600); err != nil {
		t.Fatal(err)
	}
	w := companionTransferCall(t, s, "/projects/import", request)
	if w.Code != 409 {
		t.Fatal("existing selected project folder overwritten", w.Code, w.Body.String())
	}
	data, _ := os.ReadFile(filepath.Join(child, "owner.txt"))
	if string(data) != "keep existing" {
		t.Fatal("existing content changed")
	}
	if err := os.RemoveAll(child); err != nil {
		t.Fatal(err)
	}
	w = companionTransferCall(t, s, "/projects/import", request)
	if w.Code != 201 {
		t.Fatal("failed send consumed destination grant", w.Code, w.Body.String())
	}
}
