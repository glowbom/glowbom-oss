package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type companionTemplateImportReceipt struct {
	companionProject
	FullProjectTemplate bool   `json:"fullProjectTemplate"`
	SavedPath           string `json:"savedPath"`
}

func companionTemplateImportResult(t *testing.T, s *companionSession, w *httptest.ResponseRecorder, status int) (companionTemplateImportReceipt, string) {
	t.Helper()
	var receipt companionTemplateImportReceipt
	if w.Code != status || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || !receipt.FullProjectTemplate {
		t.Fatal("full project template not confirmed", w.Code, w.Body.String())
	}
	project, ok := s.project(receipt.ID)
	if !ok || receipt.SavedPath != project.path || !receipt.Available {
		t.Fatal("imported template not published with its saved path", receipt)
	}
	return receipt, project.path
}

func TestCompanionTransferFullTemplatePreservesPlatformsIntentAndOriginals(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("template import invoked a model or Build", r.URL.Path)
	}))
	calls := 0
	fixture := starterFixtureRunner(t, nil)
	s.importTemplateCLI = func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		return fixture(ctx, args...)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/import", ""))
	var capabilities struct {
		FullProjectTemplate bool `json:"fullProjectTemplate"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &capabilities) != nil || !capabilities.FullProjectTemplate {
		t.Fatal("full template capability missing", w.Code, w.Body.String())
	}
	request := companionProjectImageFixture(t)
	request.Name = "Pocket orchard"
	request.Prompt = "Build a harvest planner from my original sketch"
	w = companionTransferCall(t, s, "/projects/import", request)
	_, root := companionTemplateImportResult(t, s, w, http.StatusCreated)
	if calls != 1 {
		t.Fatal("template was not loaded exactly once", calls)
	}
	for path, expected := range map[string]string{
		"AGENTS.md":                      "Build project instructions",
		"apple/Custom/ContentView.swift": "SwiftUI starter",
		"android/app/build.gradle.kts":   "Android starter",
		"web/package.json":               `{"name":"starter"}`,
	} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || string(data) != expected {
			t.Fatal("platform template path missing or changed", path, err)
		}
	}
	gradle, err := os.Stat(filepath.Join(root, "android/gradlew"))
	if err != nil || gradle.Mode()&0111 == 0 {
		t.Fatal("Android starter executable mode lost", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "glowbom.json"))
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]json.RawMessage
	if json.Unmarshal([]byte(starterFixtureManifest), &before) != nil || json.Unmarshal(data, &after) != nil {
		t.Fatal("invalid template manifest")
	}
	for _, key := range []string{"targets", "future", "version"} {
		compact := func(raw json.RawMessage) string {
			var out bytes.Buffer
			if json.Compact(&out, raw) != nil {
				t.Fatal("invalid template metadata", key)
			}
			return out.String()
		}
		if compact(before[key]) != compact(after[key]) {
			t.Fatal("starter target or future metadata changed", key)
		}
	}
	for key, expected := range map[string]string{
		"name": request.Name, "displayName": request.Name, "description": request.Prompt,
		"prototypePath": "prototype/index.html", "agentsPath": "AGENTS.md", "prototypeAssetsManifestPath": "prototype/assets.json",
	} {
		var actual string
		if json.Unmarshal(after[key], &actual) != nil || actual != expected {
			t.Fatal("phone intent or build metadata lost", key, actual)
		}
	}
	for _, path := range []string{"prototype/assets/demo.png", "icon.png", "android/local.properties", "android/.idea", "apple/Custom.xcodeproj/xcuserdata"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("starter demo or local files reached the imported project", path, err)
		}
	}
	preview, err := os.ReadFile(filepath.Join(root, "prototype/index.html"))
	if err != nil || !bytes.Contains(preview, []byte("Phone image transfer")) || bytes.Contains(preview, []byte("Build Anything")) {
		t.Fatal("starter demo replaced the phone preview", err)
	}
	for _, asset := range request.Assets {
		original, err := base64.StdEncoding.DecodeString(asset.DataBase64)
		if err != nil {
			t.Fatal(err)
		}
		copied, err := os.ReadFile(filepath.Join(root, "prototype/assets", asset.Filename))
		if err != nil || !bytes.Equal(copied, original) || !bytes.Contains(preview, []byte("assets/"+asset.Filename)) {
			t.Fatal("phone media bytes or preview link lost", asset.ID, err)
		}
		if companionImportInputRole(asset.Role) {
			input, err := os.ReadFile(filepath.Join(root, "inputs", asset.Filename))
			if err != nil || !bytes.Equal(input, original) {
				t.Fatal("original sketch input lost", asset.ID, err)
			}
		}
	}
	for filename, expected := range map[string]string{"saved-preview.html": request.HTML, "original.html": request.OriginalHTML, "source-project.json": request.SourceSnapshot} {
		original, err := os.ReadFile(filepath.Join(root, ".glowbom/phone-import", filename))
		if err != nil || string(original) != expected {
			t.Fatal("archived phone source changed", filename, err)
		}
	}
	data, err = os.ReadFile(filepath.Join(root, "prototype/assets.json"))
	var assets prototypeAssetsManifest
	if err != nil || json.Unmarshal(data, &assets) != nil || len(assets.Assets) < len(request.Assets) {
		t.Fatal("phone assets catalog missing or still contains starter demos", err, string(data))
	}
	for _, asset := range assets.Assets {
		if asset.Filename == "demo.png" {
			t.Fatal("starter demo remained in phone assets catalog")
		}
		if _, err := os.Stat(filepath.Join(root, "prototype/assets", asset.Filename)); err != nil {
			t.Fatal("catalog references a missing original or embedded image", asset.Filename, err)
		}
	}
	for _, expected := range request.Assets {
		found := false
		for _, asset := range assets.Assets {
			if asset.Filename == expected.Filename && asset.Prompt == expected.Prompt && asset.SourceService == expected.SourceService {
				found = true
			}
		}
		if !found {
			t.Fatal("media catalog lost phone provenance", expected.ID)
		}
	}
}

func TestCompanionTransferTemplateFailureAndCancellationRetrySameRequest(t *testing.T) {
	for _, scenario := range []string{"cli failure", "missing CLI", "request canceled"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("GLOWBOM_STUDIO_DIR", directory)
			s := testCompanion(t, http.NotFoundHandler())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			stage := ""
			fixture := starterFixtureRunner(t, func(path string) error {
				stage = filepath.Dir(path)
				if calls != 1 {
					return nil
				}
				switch scenario {
				case "missing CLI":
					return errAccountCLIMissing
				case "request canceled":
					cancel()
					return context.Canceled
				default:
					return errors.New("fixture-secret from failed CLI output")
				}
			})
			s.importTemplateCLI = func(ctx context.Context, args ...string) ([]byte, error) {
				calls++
				return fixture(ctx, args...)
			}
			request := companionProjectImageFixture(t)
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/import", string(body)).WithContext(ctx))
			if w.Code >= 200 && w.Code < 300 || strings.Contains(w.Body.String(), "fixture-secret") {
				t.Fatal("failed template import reported success or leaked CLI output", w.Code, w.Body.String())
			}
			if len(s.projects) != 0 {
				t.Fatal("failed or canceled template published a project")
			}
			folders, err := os.ReadDir(filepath.Join(directory, "PhoneProjects"))
			if err != nil && !errors.Is(err, os.ErrNotExist) || len(folders) != 0 {
				t.Fatal("failed template left a project folder", err, len(folders))
			}
			if stage == "" {
				t.Fatal("fixture CLI was never reached")
			}
			if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed template staging was not cleaned up", err)
			}
			w = companionTransferCall(t, s, "/projects/import", request)
			companionTemplateImportResult(t, s, w, http.StatusCreated)
			if calls != 2 {
				t.Fatal("same request did not retry template loading once", calls)
			}
		})
	}
}

func TestCompanionTransferCompletedTemplateRetryPreservesDesktopEdits(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.NotFoundHandler())
	request := companionProjectImageFixture(t)
	first, root := companionTemplateImportResult(t, s, companionTransferCall(t, s, "/projects/import", request), http.StatusCreated)
	edits := map[string]string{
		"AGENTS.md":                      "Desktop owner instructions after import",
		"apple/Custom/ContentView.swift": "Desktop SwiftUI edits",
		"web/package.json":               `{"name":"owner-edited-app"}`,
		"prototype/index.html":           "<!doctype html><html><body>Owner changed the preview</body></html>",
	}
	for path, content := range edits {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	failedRunner := func(context.Context, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("completed retry must not redownload the template")
	}
	s.importTemplateCLI = failedRunner
	for _, session := range []*companionSession{s, testCompanion(t, http.NotFoundHandler())} {
		session.importTemplateCLI = failedRunner
		recovered, recoveredPath := companionTemplateImportResult(t, session,
			companionTransferCall(t, session, "/projects/import", request), http.StatusOK)
		if recovered.ID != first.ID || recoveredPath != root {
			t.Fatal("completed retry created a different project", recovered, recoveredPath)
		}
		for path, expected := range edits {
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
			if err != nil || string(data) != expected {
				t.Fatal("completed retry overwrote Desktop edits", path, err)
			}
		}
	}
	if calls != 0 {
		t.Fatal("completed receipt downloaded the starter again", calls)
	}
}

func TestCompanionTransferTemplateInstallsSelectedIconWithoutChangingOriginal(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.NotFoundHandler())
	request := companionProjectImageFixture(t)
	var snapshot map[string]any
	if json.Unmarshal([]byte(request.SourceSnapshot), &snapshot) != nil {
		t.Fatal("invalid source fixture")
	}
	iconAsset := request.Assets[1]
	snapshot["customIconAssetID"] = iconAsset.ID
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	request.SourceSnapshot = string(data)
	_, root := companionTemplateImportResult(t, s, companionTransferCall(t, s, "/projects/import", request), http.StatusCreated)
	original, err := base64.StdEncoding.DecodeString(iconAsset.DataBase64)
	if err != nil {
		t.Fatal(err)
	}
	var selectedPNG []byte
	for _, path := range []string{"icon.png", "web/src/app/icon.png", "web/public/icon.png"} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal("selected icon missing", path, err)
		}
		decoded, err := png.Decode(bytes.NewReader(data))
		if err != nil || decoded.Bounds().Dx() != 40 || decoded.Bounds().Dy() != 30 {
			t.Fatal("selected JPEG was not installed as its PNG image", path, err)
		}
		if selectedPNG == nil {
			selectedPNG = data
		} else if !bytes.Equal(selectedPNG, data) {
			t.Fatal("web and root icon copies differ", path)
		}
	}
	copied, err := os.ReadFile(filepath.Join(root, "prototype/assets", iconAsset.Filename))
	if err != nil || !bytes.Equal(copied, original) {
		t.Fatal("icon normalization changed the original phone media", err)
	}
}

func TestCompanionTransferLegacyReceiptDoesNotUpgradeOrClaimFullTemplate(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", directory)
	s := testCompanion(t, http.NotFoundHandler())
	request := companionProjectImageFixture(t)
	first, root := companionTemplateImportResult(t, s, companionTransferCall(t, s, "/projects/import", request), http.StatusCreated)
	// Model a saved receipt from before platform starters were included.
	receiptPath := filepath.Join(directory, "PhoneImports", strings.ToLower(request.RequestID)+".json")
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		t.Fatal("invalid saved transfer fixture")
	}
	delete(fields, "fullProjectTemplate")
	data, err = json.Marshal(fields)
	if err != nil || os.WriteFile(receiptPath, data, 0600) != nil {
		t.Fatal("could not prepare legacy receipt fixture", err)
	}
	for _, path := range []string{"AGENTS.md", "apple", "android", "web"} {
		if err := os.RemoveAll(filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := json.Marshal(map[string]any{"name": request.Name, "version": "1.0.0", "description": request.Prompt, "targets": map[string]any{}})
	if err != nil || os.WriteFile(filepath.Join(root, "glowbom.json"), manifest, 0644) != nil {
		t.Fatal("could not prepare legacy project fixture", err)
	}
	next := testCompanion(t, http.NotFoundHandler())
	calls := 0
	next.importTemplateCLI = func(context.Context, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("old completed imports must not download or upgrade")
	}
	w := companionTransferCall(t, next, "/projects/import", request)
	var recovered companionTemplateImportReceipt
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &recovered) != nil || recovered.ID != first.ID || recovered.FullProjectTemplate || calls != 0 {
		t.Fatal("legacy receipt was changed or falsely described as a full starter", w.Code, w.Body.String(), calls)
	}
	for _, path := range []string{"AGENTS.md", "apple", "android", "web"} {
		if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("legacy retry installed unrequested platform files", path, err)
		}
	}
	data, err = os.ReadFile(filepath.Join(root, "glowbom.json"))
	if err != nil || !bytes.Equal(data, manifest) {
		t.Fatal("legacy retry rewrote the project manifest", err)
	}
}

func TestCompanionTransferChosenFolderRecheckedAfterTemplateDownload(t *testing.T) {
	for _, scenario := range []string{"child appeared", "parent moved"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
			parent := filepath.Join(canonicalDirectory(t.TempDir()), "chosen")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			s := testCompanion(t, http.NotFoundHandler())
			grant := companionDestinationFixture(t, s, parent, companionTransferTestID)
			request := companionProjectImageFixture(t)
			request.DestinationID = grant.ID
			child := companionChosenProjectFolder(request.Name, request.RequestID)
			ownerPath := ""
			s.importTemplateCLI = starterFixtureRunner(t, func(string) error {
				switch scenario {
				case "child appeared":
					ownerPath = filepath.Join(parent, child)
					if err := os.Mkdir(ownerPath, 0700); err != nil {
						t.Fatal(err)
					}
				default:
					ownerPath = parent + "-moved"
					if err := os.Rename(parent, ownerPath); err != nil {
						t.Fatal(err)
					}
				}
				return os.WriteFile(filepath.Join(ownerPath, "owner.txt"), []byte("keep Desktop edits"), 0600)
			})
			w := companionTransferCall(t, s, "/projects/import", request)
			if w.Code >= 200 && w.Code < 300 || len(s.projects) != 0 {
				t.Fatal("folder changed during template download but a project was published", w.Code, w.Body.String())
			}
			data, err := os.ReadFile(filepath.Join(ownerPath, "owner.txt"))
			if err != nil || string(data) != "keep Desktop edits" {
				t.Fatal("download collision or move overwrote Desktop content", err)
			}
			if _, err := os.Stat(filepath.Join(ownerPath, "glowbom.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("template installed into the changed Desktop folder", err)
			}
			if scenario == "parent moved" {
				if _, err := os.Stat(filepath.Join(ownerPath, child)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("template installed into the moved parent", err)
				}
				if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("template recreated the selected parent after it moved", err)
				}
			} else if files, err := os.ReadDir(ownerPath); err != nil || len(files) != 1 {
				t.Fatal("partial template files left in the owner's new folder", err, len(files))
			}
		})
	}
}
