package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func continuityPixels(t *testing.T, shade color.RGBA) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			value.SetRGBA(x, y, shade)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, value); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func continuityFixture(t *testing.T, previous string) (string, *prototypeImageReferenceSnapshot, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := mediaEditProject(t, previous)
	assets := filepath.Join(root, "prototype", "assets")
	if err := os.MkdirAll(assets, 0755); err != nil {
		t.Fatal(err)
	}
	portrait := continuityPixels(t, color.RGBA{R: 150, G: 70, B: 30, A: 255})
	other := continuityPixels(t, color.RGBA{R: 10, G: 50, B: 200, A: 255})
	if err := os.WriteFile(filepath.Join(assets, "portrait.png"), portrait, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "other.png"), other, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := capturePrototypeImageReferences(root, previous)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizePreviousPrototypeImage(portrait, "openai-api")
	if err != nil {
		t.Fatal(err)
	}
	return root, snapshot, "data:image/png;base64," + base64.StdEncoding.EncodeToString(normalized)
}

func TestPreviousImageMatchesOnlyStableUniqueSlots(t *testing.T) {
	token := "glowbomimages:The same person in a green jacket"
	cases := []struct {
		name, previous, current string
		want                    bool
	}{
		{"stable id reordered", `<img id="portrait" src="assets/portrait.png"><img id="scenery" src="assets/other.png">`, `<img id="new-scenery" src="assets/other.png"><img id="scenery" src="assets/other.png"><img id="portrait" src="` + token + `">`, true},
		{"qualified ancestor", `<section id="profile"><img alt="Portrait" src="assets/portrait.png"></section><section id="gallery"><img alt="Portrait" src="assets/other.png"></section>`, `<section id="gallery"><img alt="Portrait" src="assets/other.png"></section><section id="profile"><img alt="Portrait" src="` + token + `"></section>`, true},
		{"unique exact alt", `<img alt="My profile portrait" src="assets/portrait.png">`, `<img alt="My profile portrait" src="` + token + `">`, true},
		{"inline background", `<div id="hero" style="background-image:url('assets/portrait.png')"></div>`, `<div id="hero" style="color:green;background-image:url('` + token + `')"></div>`, true},
		{"no positional match", `<img src="assets/portrait.png">`, `<img src="` + token + `">`, false},
		{"duplicate old alt", `<img alt="Portrait" src="assets/portrait.png"><img alt="Portrait" src="assets/other.png">`, `<img alt="Portrait" src="` + token + `">`, false},
		{"duplicate current ancestor alt", `<section id="profile"><img alt="Portrait" src="assets/portrait.png"></section>`, `<section id="profile"><img alt="Portrait" src="` + token + `"><img alt="Portrait" src="glowbomimages:A second new portrait"></section>`, false},
		{"duplicate current ancestor role", `<section id="profile"><img data-role="portrait" src="assets/portrait.png"></section>`, `<section id="profile"><img data-role="portrait" src="` + token + `"><img data-role="portrait" src="glowbomimages:A second new portrait"></section>`, false},
		{"duplicate old ancestor role", `<section id="profile"><img data-role="portrait" src="assets/portrait.png"><img data-role="portrait" src="assets/other.png"></section>`, `<section id="profile"><img data-role="portrait" src="` + token + `"></section>`, false},
		{"duplicate current id", `<img id="portrait" src="assets/portrait.png">`, `<img id="portrait" src="` + token + `"><img id="portrait" src="glowbomimages:Second portrait">`, false},
		{"duplicate src attribute", `<img id="portrait" src="assets/portrait.png">`, `<img id="portrait" src="` + token + `" src="glowbomimages:Second portrait">`, false},
		{"shared token different people", `<img id="portrait" src="assets/portrait.png"><img id="other" src="assets/other.png">`, `<img id="portrait" src="` + token + `"><img id="other" src="` + token + `">`, false},
		{"fresh optout", `<img id="portrait" src="assets/portrait.png">`, `<img id="portrait" data-glowbom-reference="none" src="` + token + `">`, false},
		{"duplicate optout attribute", `<img id="portrait" src="assets/portrait.png">`, `<img id="portrait" data-glowbom-reference="auto" data-glowbom-reference="none" src="` + token + `">`, false},
		{"background optout", `<div id="hero" style="background-image:url('assets/portrait.png')"></div>`, `<div id="hero" data-glowbom-reference="none" style="background-image:url('` + token + `')"></div>`, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, snapshot, expected := continuityFixture(t, test.previous)
			match, matched, err := matchPreviousPrototypeImageReference(snapshot, test.current, token, "openai-api")
			if err != nil || matched != test.want {
				t.Fatalf("matched=%t expected=%t err=%v", matched, test.want, err)
			}
			if test.want && (match.ReferenceImage != expected || match.AssetURL != "assets/portrait.png") {
				t.Fatalf("wrong reference: %+v", match)
			}
			if !test.want && match.ReferenceImage != "" {
				t.Fatal("an ambiguous or opted out slot inherited pixels")
			}
		})
	}
}

func TestPreviousImageSnapshotFreezesPixelsBeforeDeletionOrReplacement(t *testing.T) {
	previous := `<img id="portrait" src="assets/portrait.png?version=1#photo">`
	current := `<img id="portrait" src="glowbomimages:Updated portrait">`
	for _, change := range []string{"delete", "replace"} {
		t.Run(change, func(t *testing.T) {
			root, snapshot, expected := continuityFixture(t, previous)
			path := filepath.Join(root, "prototype", "assets", "portrait.png")
			if change == "delete" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, continuityPixels(t, color.RGBA{G: 200, A: 255}), 0600); err != nil {
				t.Fatal(err)
			}
			actual, err := resolvePreviousPrototypeImageReference(snapshot, current, "glowbomimages:Updated portrait", "openai-api")
			if err != nil || actual != expected {
				t.Fatalf("frozen ref changed: err=%v", err)
			}
		})
	}
}

func TestPreviousImageRejectsRemoteAndEscapingPaths(t *testing.T) {
	for _, source := range []string{"https://example.test/person.png", "../../private.png", "/assets/../private.png", "/prototype/assets/../../private.png", "/assets/%2e%2e/private.png", "/prototype/assets/%2e%2e/%2e%2e/private.png", "assets/escape.png", "assets/link/person.png"} {
		t.Run(source, func(t *testing.T) {
			root, snapshot, _ := continuityFixture(t, `<img id="portrait" src="`+source+`">`)
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "person.png"), continuityPixels(t, color.RGBA{B: 200, A: 255}), 0600); err != nil {
				t.Fatal(err)
			}
			if source == "assets/escape.png" {
				if err := os.Symlink(filepath.Join(outside, "person.png"), filepath.Join(root, "prototype", "assets", "escape.png")); err != nil {
					t.Fatal(err)
				}
			}
			if source == "assets/link/person.png" {
				if err := os.Symlink(outside, filepath.Join(root, "prototype", "assets", "link")); err != nil {
					t.Fatal(err)
				}
			}
			privatePixels := continuityPixels(t, color.RGBA{B: 200, A: 255})
			for _, path := range []string{filepath.Join(root, "private.png"), filepath.Join(root, "prototype", "private.png")} {
				if err := os.WriteFile(path, privatePixels, 0600); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, _ = capturePrototypeImageReferences(root, `<img id="portrait" src="`+source+`">`)
			match, matched, err := matchPreviousPrototypeImageReference(snapshot, `<img id="portrait" src="glowbomimages:Updated portrait">`, "glowbomimages:Updated portrait", "openai-api")
			if !matched || err == nil || match.ReferenceImage != "" {
				t.Fatalf("unsafe previous source accepted: matched=%t err=%v", matched, err)
			}
		})
	}
}

func TestPreviousImageResizesWithinProviderReferenceLimits(t *testing.T) {
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewNRGBA(image.Rect(0, 0, 4100, 20))); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"openai-api", "glowbom-api"} {
		data, err := normalizePreviousPrototypeImage(raw.Bytes(), source)
		if err != nil {
			t.Fatal(err)
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width > 4096 || config.Height > 4096 {
			t.Fatalf("bad normalized dimensions=%+v err=%v", config, err)
		}
		if _, err := normalizeProjectIconReference("data:image/png;base64," + base64.StdEncoding.EncodeToString(data)); err != nil {
			t.Fatal(err)
		}
		if source == "glowbom-api" {
			if err := validateGlowbomImageReference(base64.StdEncoding.EncodeToString(data)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestMediaApprovalDefaultsPreviousImageAndExplicitReferenceWins(t *testing.T) {
	root, snapshot, expected := continuityFixture(t, `<img id="portrait" src="assets/portrait.png">`)
	current := `<img id="portrait" src="glowbomimages:Updated portrait">`
	if err := os.WriteFile(filepath.Join(root, "prototype", "index.html"), []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	req := OpenCodeMediaPostPassRequest{ProjectPath: root, ImageSource: "openai-api", previousImageReferences: snapshot}
	plan, err := buildOpenCodeMediaApproval(req)
	if err != nil || plan == nil || len(plan.Items) != 1 || plan.Items[0].ReferenceOrigin != "previous-image" || len(plan.Items[0].ReferenceImages) != 1 || plan.Items[0].ReferenceImages[0] != expected {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	req.ReferenceImagePath = "prototype/assets/other.png"
	plan, err = buildOpenCodeMediaApproval(req)
	if err != nil || len(plan.Items) != 1 || plan.Items[0].ReferenceOrigin != "" || len(plan.Items[0].ReferenceImages) != 1 || plan.Items[0].ReferenceImages[0] == expected {
		t.Fatalf("explicit reference lost: plan=%+v err=%v", plan, err)
	}
}

func TestPreviousImageDefaultDoesNotChargeForExactReusableAsset(t *testing.T) {
	root, snapshot, _ := continuityFixture(t, `<img id="portrait" src="assets/portrait.png">`)
	current := `<img id="portrait" src="glowbomimages:Same portrait">`
	if err := os.WriteFile(filepath.Join(root, "prototype", "index.html"), []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	config, _ := os.UserConfigDir()
	studio := filepath.Join(config, "Glowbom", "Studio", "Assets")
	if err := os.MkdirAll(studio, 0755); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(studioAssetRecord{ID: "reusable", MediaType: "image", Prompt: "Same portrait", SourceService: openAIImageSourceLabel, DataBase64: reviewImageData(t)})
	if err := os.WriteFile(filepath.Join(studio, "same.json"), record, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := buildOpenCodeMediaApproval(OpenCodeMediaPostPassRequest{ProjectPath: root, ImageSource: "openai-api", previousImageReferences: snapshot})
	if err != nil || plan != nil {
		t.Fatalf("reusable asset requested provider generation: %+v err=%v", plan, err)
	}
}

func TestReviewedPreviousReferenceCanBeClearedAndFailureDoesNotRetry(t *testing.T) {
	for _, choice := range []string{"default", "clear", "replace"} {
		t.Run(choice, func(t *testing.T) {
			root, snapshot, expected := continuityFixture(t, `<img id="portrait" src="assets/portrait.png">`)
			current := `<img id="portrait" src="glowbomimages:Updated portrait">`
			if err := os.WriteFile(filepath.Join(root, "prototype", "index.html"), []byte(current), 0600); err != nil {
				t.Fatal(err)
			}
			plan, err := buildOpenCodeMediaApproval(OpenCodeMediaPostPassRequest{ProjectPath: root, ImageSource: "openai-api", previousImageReferences: snapshot})
			if err != nil {
				t.Fatal(err)
			}
			item := plan.Items[0]
			if choice == "clear" {
				item.ReferenceImages = []string{}
				item.ReferenceOrigin = ""
			}
			if choice == "replace" {
				data, _ := os.ReadFile(filepath.Join(root, "prototype", "assets", "other.png"))
				item.ReferenceImages = []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(data)}
				item.ReferenceOrigin = ""
			}
			old := generatePostPassSelectedImage
			defer func() { generatePostPassSelectedImage = old }()
			calls := 0
			generatePostPassSelectedImage = func(ctx context.Context, source, key, prompt, reference, aspect string) (string, string, error) {
				calls++
				if choice == "default" && ("data:image/png;base64,"+reference != expected || !strings.Contains(prompt, "Preserve the person's recognizable identity")) {
					t.Fatal("previous portrait was not forwarded with continuity guidance")
				}
				if choice == "clear" && (reference != "" || strings.Contains(prompt, "supplied previous image")) {
					t.Fatal("cleared reference was inferred again")
				}
				if choice == "replace" && (reference == "" || "data:image/png;base64,"+reference == expected || strings.Contains(prompt, "supplied previous image")) {
					t.Fatal("replacement reference was lost")
				}
				return "", "", fmt.Errorf("provider failed")
			}
			resp, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: root, Items: []OpenCodeMediaApprovalItem{item}, ImageAPIKeys: map[string]string{"openai-api": "test-key"}, previousImageReferences: snapshot})
			if err != nil || calls != 1 || len(resp.GeneratedAssets) != 0 {
				t.Fatalf("calls=%d result=%+v err=%v", calls, resp, err)
			}
			if choice == "default" && len(resp.ReusedStudioAssets) != 1 {
				t.Fatal("provider failure did not keep the frozen previous image")
			}
			if choice != "default" && len(resp.ReusedStudioAssets) != 0 {
				t.Fatal("removed or replaced prior reference was restored")
			}
		})
	}
}

func TestAutomaticMediaPassUsesFrozenPreviousReference(t *testing.T) {
	root, snapshot, expected := continuityFixture(t, `<img id="portrait" src="assets/portrait.png">`)
	current := `<img id="portrait" src="glowbomimages:Updated portrait">`
	if err := os.WriteFile(filepath.Join(root, "prototype", "index.html"), []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	old := generatePostPassSelectedImage
	defer func() { generatePostPassSelectedImage = old }()
	calls := 0
	generatePostPassSelectedImage = func(ctx context.Context, source, key, prompt, reference, aspect string) (string, string, error) {
		calls++
		if "data:image/png;base64,"+reference != expected || !strings.Contains(prompt, "supplied previous image") {
			t.Fatal("automatic generation lost previous image reference")
		}
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEditImage(t, "")), openAIImageSourceLabel, nil
	}
	resp, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: root, ImageSource: "openai-api", ImageAPIKeys: map[string]string{"openai-api": "test-key"}, previousImageReferences: snapshot})
	if err != nil || calls != 1 || len(resp.GeneratedAssets) != 1 || resp.GeneratedAssets[0].Prompt != "Updated portrait" {
		t.Fatalf("stored prompt changed or reference lost: result=%+v calls=%d err=%v", resp, calls, err)
	}
}

func TestPreviousImageFallbackPreservesChangedHashFile(t *testing.T) {
	root, _, reference := continuityFixture(t, `<img id="portrait" src="assets/portrait.png">`)
	data, _, _ := decodeBase64Payload(reference, "image/png")
	data, _ = normalizeProjectIcon(data)
	digest := sha256.Sum256(data)
	filename := fmt.Sprintf("glowbom-previous-image-%x.png", digest[:16])
	path := filepath.Join(root, "prototype", "assets", filename)
	changed := continuityPixels(t, color.RGBA{G: 200, A: 255})
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := materializePreviousImageReference(root, "portrait", "glowbomimages:portrait", reference, &OpenCodeMediaPostPassResponse{})
	if err == nil {
		t.Fatal("changed hash file was overwritten")
	}
	current, _ := os.ReadFile(path)
	if !bytes.Equal(current, changed) {
		t.Fatal("changed previous image file was lost")
	}
}

func TestPreviousReferenceSourceSwitchResizesAndClearStaysEmpty(t *testing.T) {
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewNRGBA(image.Rect(0, 0, 2100, 2100))); err != nil {
		t.Fatal(err)
	}
	items := []OpenCodeMediaApprovalItem{{MediaType: "image", SourceID: "glowbom-api", ReferenceOrigin: "previous-image", ReferenceImages: []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(raw.Bytes())}}, {MediaType: "image", SourceID: "glowbom-api", ReferenceOrigin: "previous-image", ReferenceImages: []string{}}}
	if err := preparePreviousImageApprovalReferences(items); err != nil {
		t.Fatal(err)
	}
	data, _, err := decodeBase64Payload(items[0].ReferenceImages[0], "image/png")
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || int64(config.Width)*int64(config.Height) > 4_194_304 {
		t.Fatalf("source bound exceeded: %+v err=%v", config, err)
	}
	if len(items[1].ReferenceImages) != 0 {
		t.Fatal("cleared prior reference was restored on source change")
	}
}
