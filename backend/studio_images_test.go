package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadXAIStoredCredentialFindsSubscription(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "1")
	path := filepath.Join(t.TempDir(), "auth.json")
	payload := `{"xai":{"type":"oauth","access":"subscription-token","refresh":"refresh-token","expires":4102444800000}}`
	if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	credential, ok, err := readXAIStoredCredential(path)
	if err != nil || !ok {
		t.Fatal(credential, ok, err)
	}
	if credential.Kind != "subscription" || credential.Bearer != "subscription-token" || credential.Refresh != "refresh-token" {
		t.Fatalf("unexpected credential: %+v", credential)
	}
}

func TestRefreshXAICredentialPersistsRotatedTokens(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "1")
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"openai":{"type":"api","key":"keep-me"},"xai":{"type":"oauth","access":"old","refresh":"old-refresh","expires":1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != xAIOAuthClientID || r.Form.Get("refresh_token") != "old-refresh" {
			t.Fatalf("unexpected refresh request: %v", r.Form)
		}
		fmt.Fprint(w, `{"access_token":"new","refresh_token":"new-refresh","expires_in":3600}`)
	}))
	defer server.Close()

	credential, err := refreshXAICredential(xAICredential{
		Bearer: "old", Kind: "subscription", AuthFile: path, Refresh: "old-refresh", Expires: 1,
	}, server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if credential.Bearer != "new" || credential.Refresh != "new-refresh" {
		t.Fatalf("unexpected refreshed credential: %+v", credential)
	}
	var saved map[string]json.RawMessage
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved["openai"]; !ok {
		t.Fatal("refresh removed another provider credential")
	}
}

func TestSaveAndListStudioImage(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	image := base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xd9})
	record, err := saveStudioImage("A tiny test image", "data:image/jpeg;base64,"+image)
	if err != nil {
		t.Fatal(err)
	}
	images, err := listStudioImages()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].ID != record.ID || images[0].Prompt != "A tiny test image" {
		t.Fatalf("unexpected images: %+v", images)
	}
}

func TestSaveAndListStudioVideoSeparately(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	image := base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xd9})
	if _, err := saveStudioImage("Still", "data:image/jpeg;base64,"+image); err != nil {
		t.Fatal(err)
	}
	video := base64.StdEncoding.EncodeToString([]byte("ftyp"))
	record, err := saveStudioAsset(studioSaveOptions{
		Prompt: "Clip", DataURI: "data:video/mp4;base64," + video, MediaType: "video", Source: xAIVideoSourceLabel,
	})
	if err != nil {
		t.Fatal(err)
	}
	images, err := listStudioAssets("image", studioImageLimit)
	if err != nil {
		t.Fatal(err)
	}
	videos, err := listStudioAssets("video", studioVideoLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].Prompt != "Still" {
		t.Fatalf("unexpected images: %+v", images)
	}
	if len(videos) != 1 || videos[0].ID != record.ID || videos[0].Prompt != "Clip" {
		t.Fatalf("unexpected videos: %+v", videos)
	}

	req := httptest.NewRequest(http.MethodGet, "/studio/assets", nil)
	rec := httptest.NewRecorder()
	studioAssetsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status %d body %s", rec.Code, rec.Body.String())
	}
	var catalog struct {
		Images []studioImageSummary `json:"images"`
		Videos []studioImageSummary `json:"videos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Images) != 1 || len(catalog.Videos) != 1 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
}

func TestStudioCatalogPaginatesImagesAndVideosIndependently(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", dir)
	for index := 0; index < 30; index++ {
		writeMacStudioAsset(
			t, dir, fmt.Sprintf("AAAAAAAA-0000-0000-0000-%012d", index),
			"image", fmt.Sprintf("Image %02d", index), fmt.Sprintf("%d.000000", 2000+index),
		)
	}
	for index := 0; index < 15; index++ {
		writeMacStudioAsset(
			t, dir, fmt.Sprintf("BBBBBBBB-0000-0000-0000-%012d", index),
			"video", fmt.Sprintf("Video %02d", index), fmt.Sprintf("%d.500000", 2000+index),
		)
	}

	req := httptest.NewRequest(
		http.MethodGet,
		"/studio/assets?imageOffset=24&imageLimit=24&videoOffset=12&videoLimit=12",
		nil,
	)
	rec := httptest.NewRecorder()
	studioAssetsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status %d body %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Images        []studioImageSummary `json:"images"`
		Videos        []studioImageSummary `json:"videos"`
		HasMoreImages bool                 `json:"hasMoreImages"`
		HasMoreVideos bool                 `json:"hasMoreVideos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Images) != 6 || page.Images[0].Prompt != "Image 05" || page.HasMoreImages {
		t.Fatalf("unexpected image page: %+v", page)
	}
	if len(page.Videos) != 3 || page.Videos[0].Prompt != "Video 02" || page.HasMoreVideos {
		t.Fatalf("unexpected video page: %+v", page)
	}
}

func TestStudioCatalogCanPageOnlyTheActiveMediaType(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", dir)
	for index := 0; index < 26; index++ {
		writeMacStudioAsset(
			t, dir, fmt.Sprintf("CCCCCCCC-0000-0000-0000-%012d", index),
			"image", fmt.Sprintf("Image %02d", index), fmt.Sprintf("%d.000000", 3000+index),
		)
	}

	req := httptest.NewRequest(
		http.MethodGet,
		"/studio/assets?imageOffset=24&imageLimit=24&videoLimit=0",
		nil,
	)
	rec := httptest.NewRecorder()
	studioAssetsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status %d body %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Images        []studioImageSummary `json:"images"`
		Videos        []studioImageSummary `json:"videos"`
		HasMoreImages bool                 `json:"hasMoreImages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Images) != 2 || len(page.Videos) != 0 || page.HasMoreImages {
		t.Fatalf("unexpected active-only page: %+v", page)
	}
}

func TestStudioAssetInfoIncludesSavedReferenceAndSize(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", dir)
	image := base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xd9})
	still, err := saveStudioImage("Reference still", "data:image/jpeg;base64,"+image)
	if err != nil {
		t.Fatal(err)
	}
	duration := 5.0
	record, err := saveStudioAsset(studioSaveOptions{
		Prompt: "Walk", DataURI: "data:video/mp4;base64," + base64.StdEncoding.EncodeToString([]byte("ftyp")),
		MediaType: "video", Source: xAIVideoSourceLabel, AspectRatio: "16:9", Duration: duration, SourceAssetID: still.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/studio/assets/info?id="+record.ID, nil)
	rec := httptest.NewRecorder()
	studioAssetInfoHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("info status %d body %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Asset studioImageSummary `json:"asset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Asset.SourceAssetID != still.ID {
		t.Fatalf("expected source still %s, got %+v", still.ID, payload.Asset)
	}
	if payload.Asset.Duration == nil || *payload.Asset.Duration != 5 {
		t.Fatalf("expected 5 second duration, got %+v", payload.Asset.Duration)
	}
	if payload.Asset.Dimensions == nil || payload.Asset.Dimensions.Width != 1280 || payload.Asset.Dimensions.Height != 720 {
		t.Fatalf("unexpected video size: %+v", payload.Asset.Dimensions)
	}
}

// writeMacStudioAsset writes a record in the format the Mac app saves: an
// uppercase identifier, a CGSize stored as [width, height], and an ISO 8601
// timestamp without fractional seconds.
func writeMacStudioAsset(t *testing.T, dir, id, mediaType, prompt string, epoch string) {
	t.Helper()
	assets := filepath.Join(dir, "Assets")
	if err := os.MkdirAll(assets, 0755); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"id":"%s","timestamp":"2026-08-12T08:33:27Z","mediaType":"%s",`+
		`"dataBase64":"%s","fileSize":4,"prompt":"%s","assetType":"generated",`+
		`"sourceService":"Glowby Images (Nano Banana 2)","dimensions":[1536,1024],`+
		`"sourceType":"generated","usedInProjects":[],"tags":[],"isFavorite":false,"notes":""}`,
		id, mediaType, base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xd9}), prompt)
	name := filepath.Join(assets, epoch+"_"+id+".json")
	if err := os.WriteFile(name, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestStudioServesAssetsSavedByTheMacApp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", dir)
	id := "1E5256D6-85A5-47D2-988A-9748A88800E9"
	writeMacStudioAsset(t, dir, id, "image", "Happy family video call", "1786523607.754236")

	images, _, err := listStudioCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].ID != id {
		t.Fatalf("unexpected catalog: %+v", images)
	}

	record, err := findStudioAsset(id)
	if err != nil || record.MediaType != "image" {
		t.Fatalf("record %+v err %v", record, err)
	}
	if record.Dimensions == nil || record.Dimensions.Width != 1536 || record.Dimensions.Height != 1024 {
		t.Fatalf("unexpected dimensions: %+v", record.Dimensions)
	}

	req := httptest.NewRequest(http.MethodGet, "/studio/images/content?id="+id, nil)
	rec := httptest.NewRecorder()
	studioImageContentHandler(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("content status %d length %d", rec.Code, rec.Body.Len())
	}
}

func TestSaveStudioAssetUsesMacAppRecordFormat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", dir)
	image := base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xd9})
	record, err := saveStudioImage("A tiny test image", "data:image/jpeg;base64,"+image)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != strings.ToUpper(record.ID) {
		t.Fatalf("identifier should be uppercase for the Mac app: %q", record.ID)
	}
	if strings.Contains(record.Timestamp, ".") || !strings.HasSuffix(record.Timestamp, "Z") {
		t.Fatalf("the Mac app decodes ISO 8601 without fractional seconds: %q", record.Timestamp)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "Assets"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries %v err %v", entries, err)
	}
	if !strings.Contains(entries[0].Name(), record.ID) {
		t.Fatalf("the Mac app finds files by identifier: %q", entries[0].Name())
	}
	data, err := os.ReadFile(filepath.Join(dir, "Assets", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Dimensions []int `json:"dimensions"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("a Mac app CGSize is an array: %v", err)
	}
	if len(saved.Dimensions) != 2 {
		t.Fatalf("unexpected dimensions: %v", saved.Dimensions)
	}
}

func TestStudioCatalogOrdersSecondAndMillisecondFilenames(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", dir)
	writeMacStudioAsset(t, dir, "AAAAAAAA-0000-0000-0000-000000000001", "image", "Older", "1789978933457")
	writeMacStudioAsset(t, dir, "BBBBBBBB-0000-0000-0000-000000000002", "image", "Newer", "1789979999.000000")

	images, _, err := listStudioCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || images[0].Prompt != "Newer" {
		t.Fatalf("unexpected order: %+v", images)
	}
}

func TestResolveStudioReferenceImageFromSavedAsset(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	image := base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xd9})
	record, err := saveStudioImage("Reference still", "data:image/jpeg;base64,"+image)
	if err != nil {
		t.Fatal(err)
	}
	dataURI, err := resolveStudioReferenceImage(record.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dataURI, "data:image/") {
		t.Fatalf("expected image data URI, got %q", dataURI)
	}
}

func TestStudioVideoPollFailureKeepsProviderMessage(t *testing.T) {
	err := studioVideoPollFailure("Temporarily unable to store the generated file. Please retry.")
	var failure *studioVideoFailure
	if !errors.As(err, &failure) {
		t.Fatalf("unexpected error type: %T", err)
	}
	if !failure.retryable {
		t.Fatal("a temporary xAI storage failure should be retried")
	}
	recorder := httptest.NewRecorder()
	writeStudioProviderError(recorder, err, "generic fallback")
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "Please retry") {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body.String())
	}

	permanent := studioVideoPollFailure("moderated prompt")
	if errors.As(permanent, &failure) && failure.retryable {
		t.Fatal("a moderation failure should not be retried")
	}
}

func TestStudioVideoGenerateChecksRequestBeforeCallingXAI(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
		want string
	}{
		{"empty prompt", `{"prompt":"  ","referenceId":"still"}`, "video prompt"},
		{"unsupported ratio", `{"prompt":"make it move","aspectRatio":"5:1"}`, "aspect ratio"},
		{"unknown reference", `{"prompt":"make it move","referenceId":"missing"}`, "reference"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
			req := httptest.NewRequest(http.MethodPost, "/studio/videos/generate", strings.NewReader(testCase.body))
			rec := httptest.NewRecorder()
			studioVideoGenerateHandler(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(strings.ToLower(rec.Body.String()), testCase.want) {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
		})
	}
}
