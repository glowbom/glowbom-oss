package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectIconsAppearInStudioAndKeepEarlierGenerations(t *testing.T) {
	isolateProjectIconCredentials(t)
	dir := t.TempDir()
	generated := projectIconTestImage(t, "png")
	mockProjectIconProvider(t, func(_ *http.Request) (*http.Response, error) {
		return iconProviderResponse(generated), nil
	})
	seen := map[string]bool{}
	for range 2 {
		response := postProjectIcon(t, projectIconRequest{Path: dir, Prompt: "A friendly garden icon", SourceID: "openai-api", APIKey: "test-key"})
		var result struct {
			Success       bool   `json:"success"`
			StudioAssetID string `json:"studioAssetId"`
			Warning       string `json:"warning"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.Success || result.Warning != "" || result.StudioAssetID == "" || seen[result.StudioAssetID] {
			t.Fatalf("missing unique Studio asset: %s", response.Body.String())
		}
		seen[result.StudioAssetID] = true
		record, err := findStudioAsset(result.StudioAssetID)
		if err != nil {
			t.Fatal(err)
		}
		projectImage, err := os.ReadFile(filepath.Join(dir, "icon.png"))
		if err != nil {
			t.Fatal(err)
		}
		studioImage, err := base64.StdEncoding.DecodeString(record.DataBase64)
		if err != nil || !bytes.Equal(studioImage, projectImage) {
			t.Fatal("Studio did not preserve the saved project icon")
		}
		if record.Prompt != "App icon: A friendly garden icon" || record.SourceService != openAIImageSourceLabel || record.MediaType != "image" || record.AssetType != "generated" || record.Dimensions == nil || record.Dimensions.Width != 2 || record.Dimensions.Height != 2 {
			t.Fatal("Studio icon metadata missing or incorrect")
		}
	}
	images, err := listStudioImages()
	if err != nil || len(images) != 2 {
		t.Fatalf("expected both icon generations in Studio: %d, %v", len(images), err)
	}
}

func TestProjectIconStudioFailureKeepsTheProjectIcon(t *testing.T) {
	isolateProjectIconCredentials(t)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLOWBOM_STUDIO_DIR", blocked)
	dir := t.TempDir()
	generated := projectIconTestImage(t, "png")
	mockProjectIconProvider(t, func(_ *http.Request) (*http.Response, error) {
		return iconProviderResponse(generated), nil
	})
	response := postProjectIcon(t, projectIconRequest{Path: dir, Prompt: "A garden icon", SourceID: "openai-api", APIKey: "test-key"})
	var result struct {
		Success       bool   `json:"success"`
		StudioAssetID string `json:"studioAssetId"`
		Warning       string `json:"warning"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.Success || result.Warning == "" || result.StudioAssetID != "" {
		t.Fatalf("expected success with Studio warning: %s", response.Body.String())
	}
	if _, err := os.ReadFile(filepath.Join(dir, "icon.png")); err != nil {
		t.Fatal("Studio failure lost the generated project icon")
	}
}
