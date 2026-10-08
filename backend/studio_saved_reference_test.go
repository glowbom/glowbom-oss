package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func savedFourKReference(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewGray(image.Rect(0, 0, 5504, 3072))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func assertPreparedFourKReference(t *testing.T, reference string) {
	t.Helper()
	data, _, err := decodeBase64Payload(reference, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != 4096 || config.Height > 4096 || config.Height < 2285 || config.Height > 2287 || len(data) > projectIconMaxBytes {
		t.Fatalf("reference was not fitted without cropping: dimensions=%+v bytes=%d err=%v", config, len(data), err)
	}
	if _, err := normalizeProjectIconReference(reference); err != nil {
		t.Fatalf("resized reference does not satisfy existing provider input validation: %v", err)
	}
}

func TestStudioSavedFourKReferenceResizesBeforeSingleImageRequest(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	original := savedFourKReference(t)
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(original)
	record, err := saveStudioAsset(studioSaveOptions{Prompt: "Original 4K", DataURI: dataURI, MediaType: "image", Source: "Google", SourceID: "gemini-api", Model: "gemini-3.1-flash-image", Resolution: "4K", Dimensions: &studioDimensions{Width: 5504, Height: 3072}})
	if err != nil {
		t.Fatal(err)
	}
	originalBase64 := record.DataBase64
	calls := 0
	mockProjectIconProvider(t, func(request *http.Request) (*http.Response, error) {
		calls++
		var body struct {
			Images []struct {
				URL string `json:"url"`
			} `json:"images"`
		}
		if request.URL.Path != "/v1/images/edits" || json.NewDecoder(request.Body).Decode(&body) != nil || len(body.Images) != 1 {
			t.Fatalf("saved reference did not reach image editing: path=%s body=%+v", request.URL.Path, body)
		}
		assertPreparedFourKReference(t, body.Images[0].URL)
		output := base64.StdEncoding.EncodeToString(mediaEditImage(t, ""))
		return studioRecoveryResponse(http.StatusOK, "application/json", `{"data":[{"b64_json":"`+output+`"}]}`), nil
	})
	requestBody, _ := json.Marshal(map[string]any{"prompt": "Refine the saved image", "sourceId": "xai-api", "modelId": "grok-imagine-image-2.0", "aspectRatio": "1:1", "resolution": "1k", "quality": "low", "apiKey": "saved-reference-fixture-key", "referenceId": record.ID})
	recorder := httptest.NewRecorder()
	studioImageGenerateHandler(recorder, httptest.NewRequest(http.MethodPost, "/studio/images/generate", strings.NewReader(string(requestBody))))
	if recorder.Code != http.StatusOK || calls != 1 {
		t.Fatalf("4K reference failed or retried: status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
	saved, err := findStudioAsset(record.ID)
	if err != nil || saved.DataBase64 != originalBase64 || saved.Dimensions == nil || saved.Dimensions.Width != 5504 || saved.Dimensions.Height != 3072 {
		t.Fatalf("using a saved reference changed the original asset: %+v err=%v", saved, err)
	}
}

func TestPostPassExplicitSavedFourKReferencesReachOneImageRequest(t *testing.T) {
	original := savedFourKReference(t)
	for _, source := range []string{"Studio asset", "project relative path", "project absolute path"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
			project := mediaEditProject(t, `<img src="glowbomimage:refine saved image">`)
			if err := os.WriteFile(filepath.Join(project, "glowbom.json"), []byte(`{"name":"4K reference fixture"}`), 0600); err != nil {
				t.Fatal(err)
			}
			request := OpenCodeMediaPostPassRequest{ProjectPath: project, ImageSource: "gemini-api"}
			var originalPath, originalAssetID, originalBase64 string
			if source == "Studio asset" {
				record, err := saveStudioAsset(studioSaveOptions{Prompt: "Original 4K", DataURI: "data:image/png;base64," + base64.StdEncoding.EncodeToString(original), MediaType: "image", Source: "Google", Dimensions: &studioDimensions{Width: 5504, Height: 3072}})
				if err != nil {
					t.Fatal(err)
				}
				request.ReferenceAssetID = record.ID
				originalAssetID, originalBase64 = record.ID, record.DataBase64
			} else {
				originalPath = filepath.Join(project, "prototype", "assets", "original-4k.png")
				if err := os.MkdirAll(filepath.Dir(originalPath), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(originalPath, original, 0600); err != nil {
					t.Fatal(err)
				}
				request.ReferenceImagePath = "prototype/assets/original-4k.png"
				if source == "project absolute path" {
					request.ReferenceImagePath = originalPath
				}
			}
			plan, err := buildOpenCodeMediaApproval(request)
			if err != nil || plan == nil || len(plan.Items) != 1 || len(plan.Items[0].ReferenceImages) != 1 {
				t.Fatalf("explicit 4K reference could not be reviewed: plan=%+v err=%v", plan, err)
			}
			assertPreparedFourKReference(t, plan.Items[0].ReferenceImages[0])
			options := studioImageOptions{SourceID: "gemini-api", ModelID: "gemini-3.1-flash-image", AspectRatio: "1:1", Resolution: "1K"}
			setImageApprovalOptions(&plan.Items[0], options)
			calls := 0
			mockPostPassImageOptions(t, func(_ context.Context, actual studioImageOptions, _, _, reference string) (string, string, error) {
				calls++
				if actual != options {
					t.Fatalf("reference settings changed: %+v", actual)
				}
				assertPreparedFourKReference(t, reference)
				return "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEditImage(t, "")), "Google", nil
			})
			result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: plan.Items, ImageAPIKeys: map[string]string{"gemini-api": "four-k-reference-fixture-key"}})
			if err != nil || result == nil || calls != 1 || len(result.GeneratedAssets) != 1 {
				t.Fatalf("explicit 4K reference failed or retried: calls=%d result=%+v err=%v", calls, result, err)
			}
			if originalPath != "" {
				data, err := os.ReadFile(originalPath)
				if err != nil || !bytes.Equal(data, original) {
					t.Fatalf("using a project reference changed its original bytes: %v", err)
				}
			} else {
				record, err := findStudioAsset(originalAssetID)
				if err != nil || record.DataBase64 != originalBase64 || record.Dimensions == nil || record.Dimensions.Width != 5504 || record.Dimensions.Height != 3072 {
					t.Fatalf("using a Studio reference changed its original asset: %+v err=%v", record, err)
				}
			}
		})
	}
}

func TestSavedReferenceResizingKeepsUploadAndIconLimits(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	original := savedFourKReference(t)
	inline := "data:image/png;base64," + base64.StdEncoding.EncodeToString(original)
	if _, err := normalizeProjectIcon(original); err == nil {
		t.Fatal("saved-reference resizing changed icon dimensions")
	}
	reference, _, err := persistStudioReference("", inline)
	if err != nil || reference != inline {
		t.Fatalf("an inline upload was resized: err=%v", err)
	}
	if _, err := normalizeProjectIconReference(reference); err == nil {
		t.Fatal("saved-reference resizing changed uploaded-reference validation")
	}
	project := mediaEditProject(t, `<img src="glowbomimage:uploaded reference">`)
	uploadPath := filepath.Join(project, "uploaded-reference.png")
	if err := os.WriteFile(uploadPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildOpenCodeMediaApproval(OpenCodeMediaPostPassRequest{ProjectPath: project, ImageSource: "gemini-api", ReferenceImagePath: uploadPath}); err == nil {
		t.Fatal("an uploaded file outside project assets bypassed its reference limits")
	}
	for _, size := range []image.Point{{X: 6145, Y: 1}, {X: 5000, Y: 4100}} {
		var payload bytes.Buffer
		if err := png.Encode(&payload, image.NewGray(image.Rect(0, 0, size.X, size.Y))); err != nil {
			t.Fatal(err)
		}
		if _, err := normalizeSavedImageReference(payload.Bytes(), "gemini-api"); err == nil {
			t.Fatalf("saved-reference bounds were not enforced: %+v", size)
		}
	}
}
