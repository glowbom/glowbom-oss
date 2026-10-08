package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestStudioProjectListingKeepsMediaCountsAndLinkedProjects(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	root := studioTestProject(t, "Saved project")
	project, err := registerStudioProject(root)
	if err != nil {
		t.Fatal(err)
	}
	linkedID := "BBBBBBBB-1111-2222-3333-444444444444"
	for _, mediaType := range []string{"image", "video", "audio"} {
		_, err := saveStudioAsset(studioSaveOptions{
			MediaType: mediaType, DataURI: "dGVzdA==", Source: "Test",
			SourceProjectID: project.ID, UsedInProjects: []string{project.ID, linkedID},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	registered, err := listRegisteredStudioProjects()
	if err != nil || len(registered) != 1 || registered[0].ID != project.ID || !registered[0].Available {
		t.Fatalf("registered projects: %+v %v", registered, err)
	}
	projects, err := listStudioProjects()
	if err != nil || len(projects) != 2 {
		t.Fatalf("Studio projects: %+v %v", projects, err)
	}
	for _, item := range projects {
		if item.AssetCount != 3 {
			t.Fatalf("image, video and audio counts changed: %+v", item)
		}
		switch item.ID {
		case project.ID:
			if !item.Available || item.Name != "Saved project" {
				t.Fatalf("registered project changed: %+v", item)
			}
		case linkedID:
			if item.Available || item.Name != "Linked project" {
				t.Fatalf("linked project changed: %+v", item)
			}
		default:
			t.Fatalf("unexpected project: %+v", item)
		}
	}
	for _, query := range []string{"", "?metadataOnly=false"} {
		response := httptest.NewRecorder()
		studioProjectsHandler(response, httptest.NewRequest(http.MethodGet, "/studio/projects"+query, nil))
		var catalog struct{ Projects []studioProjectSummary }
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &catalog) != nil || len(catalog.Projects) != 2 {
			t.Fatal("default project endpoint changed its media-linked catalog", response.Code)
		}
		for _, item := range catalog.Projects {
			if item.AssetCount != 3 {
				t.Fatal("default project endpoint did not preserve media counts")
			}
		}
	}
}

func TestStudioProjectMetadataEndpointDoesNotReadMediaAndChecksIdentity(t *testing.T) {
	studio := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	root := studioTestProject(t, "Registered project")
	project, err := registerStudioProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(studio, "Assets"), []byte("unreadable media directory"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, available := range []bool{true, false} {
		if !available {
			if err := os.WriteFile(filepath.Join(root, ".glowbom", "studio.json"), []byte(`{"projectID":"CCCCCCCC-1111-2222-3333-444444444444"}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
		response := httptest.NewRecorder()
		studioProjectsHandler(response, httptest.NewRequest(http.MethodGet, "/studio/projects?metadataOnly=true", nil))
		var catalog struct{ Projects []studioProjectSummary }
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &catalog) != nil || len(catalog.Projects) != 1 {
			t.Fatal("metadata listing depended on the media directory", response.Code)
		}
		if item := catalog.Projects[0]; item.ID != project.ID || item.Path != root || item.Available != available {
			t.Fatal("metadata listing changed registered project scope or ignored its identity")
		}
	}
	for _, query := range []string{"", "?metadataOnly=false"} {
		response := httptest.NewRecorder()
		studioProjectsHandler(response, httptest.NewRequest(http.MethodGet, "/studio/projects"+query, nil))
		if response.Code != http.StatusInternalServerError {
			t.Fatal("default catalog silently skipped its required media scan", response.Code)
		}
	}
}
