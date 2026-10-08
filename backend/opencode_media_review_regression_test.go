package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reviewImageData(t *testing.T) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(studioAspectTestImage(t, 16, 16))
}

func reviewMediaProject(t *testing.T, html string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	prototype := filepath.Join(root, "prototype")
	if err := os.MkdirAll(prototype, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prototype, "index.html"), []byte(html), 0644); err != nil {
		t.Fatal(err)
	}
	return root
}

func reviewSaveStudioImage(t *testing.T, prompt string) {
	t.Helper()
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(config, "Glowbom", "Studio", "Assets")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(studioAssetRecord{ID: "review-reusable", MediaType: "image", Prompt: prompt, SourceService: openAIImageSourceLabel, DataBase64: reviewImageData(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "review-image.json"), payload, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReviewedMediaResolvesReusableAndSelectedImages(t *testing.T) {
	root := reviewMediaProject(t, `<main><img src="glowbomimages:Saved hero"><img src="glowbomimages:New hero"></main>`)
	reviewSaveStudioImage(t, "Saved hero")
	plan, err := buildOpenCodeMediaApproval(OpenCodeMediaPostPassRequest{ProjectPath: root, ImageSource: "openai-api"})
	if err != nil || plan == nil || len(plan.Items) != 1 {
		t.Fatalf("approval = %+v, error = %v; want only the new image", plan, err)
	}
	previous := generatePostPassSelectedImage
	defer func() { generatePostPassSelectedImage = previous }()
	calls := 0
	generatePostPassSelectedImage = func(_ context.Context, _, _, prompt, _, _ string) (string, string, error) {
		calls++
		if prompt != "New hero" {
			t.Fatalf("provider called for reusable or unapproved image %q", prompt)
		}
		return "data:image/png;base64," + reviewImageData(t), openAIImageSourceLabel, nil
	}
	result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: root, ImageSource: "openai-api", OpenAIKey: "test-key", Items: plan.Items})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "prototype", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || strings.Contains(string(content), "glowbomimages:") || len(result.ReusedStudioAssets) != 1 {
		t.Fatalf("mixed replacement = %s; provider calls = %d; result = %+v", content, calls, result)
	}
}

func TestReviewedMediaAllowsExcludedBlankAddedAsset(t *testing.T) {
	pending := &pendingOpenCodeMediaApproval{}
	item := OpenCodeMediaApprovalItem{ID: "added-empty-excluded", MediaType: "image", Excluded: true}
	if err := validateMediaApprovalSelection(pending, []OpenCodeMediaApprovalItem{item}); err != nil {
		t.Fatalf("excluded added asset requires generation details: %v", err)
	}
}

func TestReviewedMediaNeverGeneratesOmittedOrExcludedImages(t *testing.T) {
	excludedToken := "glowbomimages:Excluded hero"
	omittedToken := "glowbomimages:Unapproved hero"
	root := reviewMediaProject(t, `<main><img src="`+excludedToken+`"><img src="`+omittedToken+`"></main>`)
	reviewSaveStudioImage(t, "Excluded hero")
	previous := generatePostPassSelectedImage
	defer func() { generatePostPassSelectedImage = previous }()
	generatePostPassSelectedImage = func(context.Context, string, string, string, string, string) (string, string, error) {
		t.Fatal("provider called for an excluded or unapproved image")
		return "", "", nil
	}
	item := OpenCodeMediaApprovalItem{ID: mediaApprovalItemID("image", excludedToken), Placeholder: excludedToken, MediaType: "image", Prompt: "Excluded hero", SourceID: "openai-api", Excluded: true}
	result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: root, ImageSource: "openai-api", OpenAIKey: "test-key", Items: []OpenCodeMediaApprovalItem{item}})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "prototype", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.GeneratedAssets) != 0 || len(result.ReusedStudioAssets) != 0 || !strings.Contains(string(content), excludedToken) || !strings.Contains(string(content), omittedToken) {
		t.Fatalf("excluded or unapproved asset materialized: %s, %+v", content, result)
	}
}

func TestReviewedImageShapeKeepsDistinctAssetFiles(t *testing.T) {
	token := "glowbomimages:Same hero"
	root := reviewMediaProject(t, `<main><img src="`+token+`"></main>`)
	previous := generatePostPassSelectedImage
	defer func() { generatePostPassSelectedImage = previous }()
	generatePostPassSelectedImage = func(_ context.Context, _, _, _, _, shape string) (string, string, error) {
		width, height := 16, 16
		if shape == "16:9" {
			height = 9
		}
		pixels := studioAspectTestImage(t, width, height)
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(pixels), openAIImageSourceLabel, nil
	}
	var names []string
	for _, shape := range []string{"1:1", "16:9"} {
		item := OpenCodeMediaApprovalItem{ID: mediaApprovalItemID("image", token), Placeholder: token, MediaType: "image", Prompt: "Same hero", SourceID: "openai-api", AspectRatio: shape}
		req := OpenCodeMediaPostPassRequest{ProjectPath: root, OpenAIKey: "test-key", Items: []OpenCodeMediaApprovalItem{item}, ScanTargets: []string{"prototype/index.html"}}
		result := &OpenCodeMediaPostPassResponse{}
		if _, err := materializeApprovedMedia(context.Background(), req, root, nil, result); err != nil {
			t.Fatal(err)
		}
		if len(result.GeneratedAssets) != 1 {
			t.Fatalf("shape %s generated assets = %+v", shape, result)
		}
		names = append(names, result.GeneratedAssets[0].Filename)
	}
	if names[0] == names[1] {
		t.Fatalf("different image shapes overwrite the same asset file %q", names[0])
	}
}
