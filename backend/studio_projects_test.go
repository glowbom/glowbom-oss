package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func studioTestProject(t *testing.T, name string) string {
	t.Helper()
	root, err := createSketchProject(t.TempDir(), name)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func studioTestAsset(t *testing.T) (studioImageRecord, []byte) {
	t.Helper()
	data := projectIconTestImage(t, "png")
	asset, err := saveStudioAsset(studioSaveOptions{Prompt: "A harbor", MediaType: "image", Source: "Test", DataURI: "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		t.Fatal(err)
	}
	return asset, data
}

func TestStudioProjectIdentitySurvivesMoveAndPreservesManifest(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	root := studioTestProject(t, "Harbor")
	manifestPath := filepath.Join(root, "glowbom.json")
	fields, _, err := readStudioJSON(manifestPath, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaa-bbbb-cccc-dddd-111111111111"
	fields["projectID"], _ = json.Marshal(id)
	fields["unknown"] = json.RawMessage(`{"precise":9007199254740993}`)
	original, _ := json.Marshal(fields)
	if err := os.WriteFile(manifestPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := registerStudioProject(root)
	if err != nil || first.ID != strings.ToUpper(id) {
		t.Fatalf("identity: %+v %v", first, err)
	}
	unchanged, _ := os.ReadFile(manifestPath)
	if !bytes.Equal(unchanged, original) {
		t.Fatal("registration rewrote the manifest")
	}
	moved := filepath.Join(filepath.Dir(root), "renamed-folder")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	second, err := registerStudioProject(moved)
	if err != nil || second.ID != first.ID || second.Path != moved {
		t.Fatalf("moved identity: %+v %v", second, err)
	}
	projects, err := listStudioProjects()
	if err != nil || len(projects) != 1 || projects[0].Path != moved || !projects[0].Available {
		t.Fatalf("registry: %+v %v", projects, err)
	}
}

func TestStudioImageReuseCopiesBeforeLinkAndKeepsNativeMetadata(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	asset, data := studioTestAsset(t)
	origin := "AAAAAAAA-1111-2222-3333-444444444444"
	path, fields, _, err := studioAssetRaw(asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	fields["sourceProjectID"], _ = json.Marshal(origin)
	fields["usedInProjects"], _ = json.Marshal([]string{origin})
	fields["isFavorite"] = json.RawMessage(`true`)
	fields["unknown"] = json.RawMessage(`{"precise":9007199254740993,"native":"keep"}`)
	encoded, _ := json.Marshal(fields)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	root := studioTestProject(t, "Second project")
	for range 2 {
		linked, project, relative, err := useStudioImage(strings.ToLower(asset.ID), root)
		if err != nil {
			t.Fatal(err)
		}
		if linked.ID != asset.ID || linked.SourceProjectID != origin || len(linked.UsedInProjects) != 2 || !studioHasProject(linked.UsedInProjects, project.ID) {
			t.Fatalf("wrong image provenance: %+v", linked)
		}
		if project.AssetCount != 1 {
			t.Fatalf("reuse response has a stale count: %+v", project)
		}
		copied, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil || !bytes.Equal(copied, data) {
			t.Fatal("project did not receive the image")
		}
	}
	_, retained, _, err := studioAssetRaw(asset.ID)
	if err != nil || string(retained["unknown"]) != string(fields["unknown"]) || string(retained["isFavorite"]) != "true" {
		t.Fatalf("native metadata lost: %v %v", retained, err)
	}
	images, _, err := allStudioProjectAssets()
	if err != nil || len(images) != 1 {
		t.Fatal("reuse created a duplicate Studio asset")
	}
}

func TestStudioImageCopyFailureDoesNotClaimUsageOrReplaceFiles(t *testing.T) {
	for _, blocked := range []string{"folder", "existing", "symlink"} {
		t.Run(blocked, func(t *testing.T) {
			t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
			asset, _ := studioTestAsset(t)
			root := studioTestProject(t, "Blocked")
			assets := filepath.Join(root, "prototype", "assets")
			filename := filepath.Join(assets, "studio-"+strings.ToLower(asset.ID)+".png")
			if blocked == "folder" {
				if err := os.Remove(assets); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(assets, []byte("keep folder blocker"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if blocked == "existing" {
				if err := os.WriteFile(filename, []byte("keep my newer edit"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				outside := filepath.Join(t.TempDir(), "private.png")
				if err := os.WriteFile(outside, []byte("keep outside file"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filename); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, _, err := useStudioImage(asset.ID, root); err == nil {
				t.Fatal("copy should fail")
			}
			saved, err := readStudioAssetForLink(asset.ID)
			if err != nil || len(saved.UsedInProjects) != 0 {
				t.Fatal("failed copy claimed usage")
			}
			if blocked == "existing" {
				data, _ := os.ReadFile(filename)
				if string(data) != "keep my newer edit" {
					t.Fatal("copy overwrote user edits")
				}
			}
		})
	}
}

func TestStudioGeneratedImageAndCacheShareIdentity(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	root := studioTestProject(t, "Generated")
	data := projectIconTestImage(t, "png")
	options := studioSaveOptions{Prompt: "Generated harbor", Source: "Test"}
	relative := "prototype/assets/harbor.png"
	if err := os.WriteFile(filepath.Join(root, relative), data, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := linkStudioProjectImage(root, relative, data, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := linkStudioProjectImage(root, relative, data, options)
	if err != nil || second.ID != first.ID || len(second.UsedInProjects) != 1 || first.SourceProjectID != first.UsedInProjects[0] {
		t.Fatalf("cache/provenance: %+v %+v %v", first, second, err)
	}
	options.NewGeneration = true
	fresh, err := linkStudioProjectImage(root, relative, data, options)
	if err != nil || fresh.ID == first.ID {
		t.Fatal("a fresh requested generation lost its own Studio record")
	}
	if err := os.WriteFile(filepath.Join(root, relative), []byte("newer edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := linkStudioProjectImage(root, relative, data, options); err == nil {
		t.Fatal("a stale image was linked")
	}
}

func TestStudioProjectUsageRejectsChangedAssetPixels(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	asset, expected := studioTestAsset(t)
	path, fields, _, err := studioAssetRaw(asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	fields["dataBase64"], _ = json.Marshal(base64.StdEncoding.EncodeToString(projectIconTestImage(t, "jpeg")))
	encoded, _ := json.Marshal(fields)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := addStudioProjectUsageLocked(asset.ID, "AAAAAAAA-1111-2222-3333-444444444444", expected); err == nil {
		t.Fatal("a different current image was recorded as copied")
	}
	saved, err := readStudioAssetForLink(asset.ID)
	if err != nil || len(saved.UsedInProjects) != 0 {
		t.Fatal("changed image received false usage")
	}
}

func TestStudioProjectIconGenerationLinksOriginAndUsage(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	rootPath := studioTestProject(t, "Icon project")
	data := projectIconTestImage(t, "png")
	if err := os.WriteFile(filepath.Join(rootPath, "icon.png"), data, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	options := studioSaveOptions{NewGeneration: true, Prompt: "App icon", MediaType: "image", DataURI: "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)}
	first, err := saveProjectIconStudioAsset(root, data, options)
	if err != nil || first.SourceProjectID == "" || len(first.UsedInProjects) != 1 || first.SourceProjectID != first.UsedInProjects[0] {
		t.Fatalf("icon linkage: %+v %v", first, err)
	}
	second, err := saveProjectIconStudioAsset(root, data, options)
	if err != nil || first.ID == second.ID {
		t.Fatal("earlier icon generation was not retained")
	}
	project, err := registerStudioProject(rootPath)
	if err != nil || project.AssetCount != 2 {
		t.Fatalf("icon count: %+v %v", project, err)
	}
}

func TestStudioNativeOriginOnlyImageIsAssociatedAndReusable(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	asset, _ := studioTestAsset(t)
	path, fields, _, err := studioAssetRaw(asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	origin := "CCCCCCCC-1111-2222-3333-444444444444"
	fields["sourceProjectID"], _ = json.Marshal(origin)
	delete(fields, "usedInProjects")
	delete(fields, "mediaType")
	encoded, _ := json.Marshal(fields)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	images, _, _, _, err := listStudioProjectCatalogPage(origin, 0, 24, 0, 12)
	if err != nil || len(images) != 1 || images[0].ID != asset.ID {
		t.Fatalf("origin-only native image missing: %+v %v", images, err)
	}
	root := studioTestProject(t, "Reuse native")
	_, project, _, err := useStudioImage(asset.ID, root)
	if err != nil || project.AssetCount != 1 {
		t.Fatalf("legacy image cannot be reused: %+v %v", project, err)
	}
	registered, err := registerStudioProject(root)
	if err != nil || registered.AssetCount != 1 {
		t.Fatalf("registration count: %+v %v", registered, err)
	}
	projects, err := listStudioProjects()
	if err != nil || len(projects) != 2 {
		t.Fatalf("missing project origin: %+v %v", projects, err)
	}
	for _, item := range projects {
		if item.AssetCount != 1 {
			t.Fatalf("asset association double counted: %+v", item)
		}
	}
}

func TestStudioProjectCatalogFiltersUsageAndIncludesNativeProjects(t *testing.T) {
	studio := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	asset, _ := studioTestAsset(t)
	root := studioTestProject(t, "Desktop")
	_, project, _, err := useStudioImage(asset.ID, root)
	if err != nil {
		t.Fatal(err)
	}
	studioTestAsset(t)
	nativeID := "BBBBBBBB-1111-2222-3333-444444444444"
	if err := os.MkdirAll(filepath.Join(studio, "Projects"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(studio, "Projects", "1_"+nativeID+".json"), []byte(`{"id":"`+nativeID+`","projectName":"Native app","timestamp":"2026-09-24T10:00:00Z","textPrompt":"native","referenceAssetIDs":[],"aiModel":"native"}`), 0600); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	studioAssetsHandler(response, httptest.NewRequest(http.MethodGet, "/studio/assets?projectId="+strings.ToLower(project.ID), nil))
	var body struct {
		Images            []studioImageSummary `json:"images"`
		ProjectsSupported bool                 `json:"projectsSupported"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.Images) != 1 || body.Images[0].ID != asset.ID || !body.ProjectsSupported {
		t.Fatalf("filtered catalog: %d %s", response.Code, response.Body.String())
	}
	projects, err := listStudioProjects()
	if err != nil || len(projects) != 2 {
		t.Fatalf("projects: %+v %v", projects, err)
	}
	for _, item := range projects {
		if item.ID == project.ID && (item.AssetCount != 1 || !item.Available) {
			t.Fatalf("desktop project: %+v", item)
		}
		if item.ID == nativeID && (item.Name != "Native app" || item.Available) {
			t.Fatalf("native project: %+v", item)
		}
	}
}
