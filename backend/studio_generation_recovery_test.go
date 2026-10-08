package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	studioRecoveryOperation = "private-fixture-video-operation"
	studioRecoveryURL       = "https://media.example.test/result.mp4?signature=private-fixture-signature"
	studioRecoveryMP4       = "\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2fixture-video"
)

func isolateStudioRecovery(t *testing.T) {
	t.Helper()
	isolateStudioProgress(t)
	isolateProjectIconCredentials(t)
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	path := filepath.Join(dataHome, "opencode", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"xai":{"type":"api","key":"fixture-recovery-xai-key"}}`), 0600); err != nil {
		t.Fatal(err)
	}
}

func studioRecoveryResponse(status int, contentType, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

func studioRecoveryJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func studioRecoveryReference(t *testing.T) string {
	t.Helper()
	data := "data:image/png;base64," + base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))
	record, err := saveStudioAsset(studioSaveOptions{Prompt: "Starting still", DataURI: data, MediaType: "image", Source: "Studio upload", AssetType: "reference"})
	if err != nil {
		t.Fatal(err)
	}
	return record.ID
}

func readStudioRecoveryStatus(t *testing.T, id string) (studioProgressState, string) {
	t.Helper()
	w := httptest.NewRecorder()
	studioGenerationStatusHandler(w, studioProgressRequest(http.MethodGet, "/studio/generation/status?id="+id, ""))
	var state studioProgressState
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil {
		t.Fatal("could not read generation status")
	}
	return state, w.Body.String()
}

func assertStudioRecoveryPublicJSON(t *testing.T, body string) {
	t.Helper()
	for _, private := range []string{studioRecoveryOperation, studioRecoveryURL, "private-fixture-signature", "fixture-recovery-xai-key", "operationId", "videoUrl"} {
		if strings.Contains(body, private) {
			t.Fatal("public generation response exposed private provider data")
		}
	}
}

func TestStudioGenerationRecoveryRetriesExistingVideoAfterDownloadFailure(t *testing.T) {
	isolateStudioRecovery(t)
	id := "fixture-download-recovery"
	referenceID := studioRecoveryReference(t)
	providerPosts, polls, downloads := 0, 0, 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos/generations":
			providerPosts++
			return studioRecoveryResponse(200, "application/json", `{"request_id":"`+studioRecoveryOperation+`"}`), nil
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/"+studioRecoveryOperation:
			polls++
			return studioRecoveryResponse(200, "application/json", studioRecoveryJSON(t, map[string]any{"status": "completed", "video": map[string]any{"url": studioRecoveryURL}})), nil
		case r.Method == http.MethodGet && r.URL.String() == studioRecoveryURL:
			downloads++
			if r.Header.Get("Authorization") != "" {
				return nil, errors.New("provider credential was sent to an unrelated media host")
			}
			if downloads == 1 {
				return nil, errors.New("temporary download failure")
			}
			return studioRecoveryResponse(200, "video/mp4", studioRecoveryMP4), nil
		default:
			return nil, errors.New("unexpected provider request")
		}
	})
	body := studioRecoveryJSON(t, map[string]any{"generationId": id, "prompt": "A quiet performance", "aspectRatio": "16:9", "durationSeconds": 8, "referenceId": referenceID})
	w := httptest.NewRecorder()
	studioVideoGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/videos/generate", body))
	state, public := readStudioRecoveryStatus(t, id)
	if w.Code != http.StatusBadGateway || providerPosts != 1 || downloads != 1 || state.Stage != "failed" || !state.CanResume || state.Prompt != "A quiet performance" || state.ReferenceID != referenceID || state.DurationSeconds != 8 || state.AspectRatio != "16:9" {
		t.Fatal("download failure lost the resumable request or submitted more than one render")
	}
	assertStudioRecoveryPublicJSON(t, w.Body.String())
	assertStudioRecoveryPublicJSON(t, public)
	checkpoint, found, err := loadStudioGenerationCheckpoint(id)
	if err != nil || !found || checkpoint.OperationID != studioRecoveryOperation || checkpoint.VideoURL != studioRecoveryURL {
		t.Fatal("download failure did not retain its private recovery information")
	}
	studioGenerations = newStudioProgressStore()
	state, public = readStudioRecoveryStatus(t, id)
	if state.Stage != "failed" || !state.CanResume || state.Prompt != "A quiet performance" {
		t.Fatal("backend restart lost a failed recoverable request")
	}
	assertStudioRecoveryPublicJSON(t, public)
	w = httptest.NewRecorder()
	studioGenerationResumeHandler(w, studioProgressRequest(http.MethodPost, "/studio/generation/resume", studioRecoveryJSON(t, map[string]string{"generationId": id})))
	var result struct {
		Video studioImageSummary `json:"video"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Video.ID == "" || providerPosts != 1 || polls != 2 || downloads != 2 {
		t.Fatal("recovery did not download the existing operation without another provider POST")
	}
	assertStudioRecoveryPublicJSON(t, w.Body.String())
	state, public = readStudioRecoveryStatus(t, id)
	if state.Stage != "complete" || state.CanResume || state.Asset == nil || state.Asset.ID != result.Video.ID {
		t.Fatal("recovered result was not retained as the completed generation")
	}
	assertStudioRecoveryPublicJSON(t, public)
	studioGenerations = newStudioProgressStore()
	w = httptest.NewRecorder()
	studioGenerationResumeHandler(w, studioProgressRequest(http.MethodPost, "/studio/generation/resume", studioRecoveryJSON(t, map[string]string{"generationId": id})))
	var repeated struct {
		Video studioImageSummary `json:"video"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &repeated) != nil || repeated.Video.ID != result.Video.ID || providerPosts != 1 || polls != 2 || downloads != 2 {
		t.Fatal("completed recovery made another media/provider request or returned a different asset")
	}
	videos, err := listStudioAssets("video", 12)
	if err != nil || len(videos) != 1 || videos[0].ID != result.Video.ID {
		t.Fatal("recovery duplicated the completed clip")
	}
}

func TestStudioGenerationRecoveryRestoresInterruptedDownloadAsStopped(t *testing.T) {
	isolateStudioRecovery(t)
	id := "fixture-interrupted-download"
	w := httptest.NewRecorder()
	progress, ok := beginStudioProgress(w, studioProgressRequest(http.MethodPost, "/studio/videos/generate", ""), id, "video")
	if !ok {
		t.Fatal("could not begin generation")
	}
	progress.store.mu.Lock()
	cancel := progress.store.entries[id].cancel
	progress.store.mu.Unlock()
	t.Cleanup(cancel)
	if err := progress.configure("Keep the camera still", "9:16", "fixture-reference", "xai", 6); err != nil {
		t.Fatal(err)
	}
	if err := progress.operation(studioRecoveryOperation); err != nil {
		t.Fatal(err)
	}
	if err := progress.output(studioRecoveryURL); err != nil {
		t.Fatal(err)
	}
	progress.stage("downloading")
	checkpoint, found, err := loadStudioGenerationCheckpoint(id)
	if err != nil || !found || checkpoint.State.Stage != "downloading" {
		t.Fatal("the in-progress download was not checkpointed")
	}
	studioGenerations = newStudioProgressStore()
	state, public := readStudioRecoveryStatus(t, id)
	if !state.Found || state.ID != id || state.Stage != "stopped" || !state.CanResume || state.Prompt != "Keep the camera still" || state.ReferenceID != "fixture-reference" || state.AspectRatio != "9:16" || state.DurationSeconds != 6 || state.StartedAt == "" {
		t.Fatal("interrupted download did not recover its saved request and resumable stopped state")
	}
	assertStudioRecoveryPublicJSON(t, public)
}

func TestStudioGenerationRecoveryControlHandlersRequireTokenAndOrigin(t *testing.T) {
	for _, control := range []struct {
		name    string
		handler http.HandlerFunc
	}{{"cancel", studioGenerationCancelHandler}, {"resume", studioGenerationResumeHandler}} {
		for _, violation := range []string{"token", "origin", "method", "identifier"} {
			t.Run(control.name+"/"+violation, func(t *testing.T) {
				isolateStudioRecovery(t)
				id := "fixture-guarded-generation"
				checkpoint := studioGenerationCheckpoint{State: studioProgressState{Found: true, ID: id, Kind: "video", Stage: "failed", Prompt: "Retain this request", CanResume: true}, OperationID: studioRecoveryOperation, VideoURL: studioRecoveryURL}
				if err := saveStudioGenerationCheckpoint(checkpoint); err != nil {
					t.Fatal(err)
				}
				calls := 0
				mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
					calls++
					return nil, errors.New("unexpected provider request")
				})
				r := studioProgressRequest(http.MethodPost, "/studio/generation/"+control.name, studioRecoveryJSON(t, map[string]string{"generationId": id}))
				want := http.StatusUnauthorized
				switch violation {
				case "token":
					r.Header.Del("X-Glowbom-Token")
				case "origin":
					r.Header.Set("Origin", "https://untrusted.example")
					want = http.StatusForbidden
				case "method":
					r.Method = http.MethodGet
					want = http.StatusMethodNotAllowed
				case "identifier":
					r.Body = io.NopCloser(strings.NewReader(`{"generationId":"../bad-generation"}`))
					want = http.StatusBadRequest
				}
				w := httptest.NewRecorder()
				control.handler(w, r)
				if w.Code != want || calls != 0 || studioGenerations.read(id).Stage != "failed" {
					t.Fatal("invalid control request reached the provider or changed the saved generation")
				}
			})
		}
	}
}

func TestStudioGenerationRecoveryCancelStopsProviderWorkAfterBrowserDisconnect(t *testing.T) {
	for _, stalledPhase := range []string{"starting", "polling", "downloading"} {
		t.Run(stalledPhase, func(t *testing.T) {
			isolateStudioRecovery(t)
			id := "fixture-cancel-" + stalledPhase
			referenceID := studioRecoveryReference(t)
			entered := make(chan context.Context, 1)
			finished := make(chan struct{})
			var posts atomic.Int32
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				block := func() (*http.Response, error) {
					entered <- r.Context()
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/videos/generations":
					posts.Add(1)
					if stalledPhase == "starting" {
						return block()
					}
					return studioRecoveryResponse(200, "application/json", `{"request_id":"`+studioRecoveryOperation+`"}`), nil
				case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/"+studioRecoveryOperation:
					if stalledPhase == "polling" {
						return block()
					}
					return studioRecoveryResponse(200, "application/json", `{"status":"completed","video":{"url":"`+studioRecoveryURL+`"}}`), nil
				case r.Method == http.MethodGet && r.URL.String() == studioRecoveryURL:
					return block()
				default:
					return nil, errors.New("unexpected provider request")
				}
			})
			requestContext, disconnect := context.WithCancel(context.Background())
			body := studioRecoveryJSON(t, map[string]any{"generationId": id, "prompt": "A quiet performance", "aspectRatio": "16:9", "durationSeconds": 8, "referenceId": referenceID})
			r := studioProgressRequest(http.MethodPost, "/studio/videos/generate", body).WithContext(requestContext)
			w := httptest.NewRecorder()
			t.Cleanup(func() {
				disconnect()
				_, _ = studioGenerations.stop(id)
				select {
				case <-finished:
				case <-time.After(2 * time.Second):
					t.Error("generation handler did not finish after cleanup cancellation")
				}
			})
			go func() { defer close(finished); studioVideoGenerateHandler(w, r) }()
			var providerContext context.Context
			select {
			case providerContext = <-entered:
			case <-finished:
				t.Fatal("generation did not reach the expected stalled provider request")
			case <-time.After(2 * time.Second):
				t.Fatal("generation did not start")
			}
			disconnect()
			if providerContext.Err() != nil {
				t.Fatal("browser disconnect abandoned the tracked provider request")
			}
			cancelResponse := httptest.NewRecorder()
			studioGenerationCancelHandler(cancelResponse, studioProgressRequest(http.MethodPost, "/studio/generation/cancel", studioRecoveryJSON(t, map[string]string{"generationId": id})))
			if cancelResponse.Code != http.StatusOK {
				t.Fatal("could not stop the tracked generation")
			}
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("Stop did not abort the provider request or media download")
			}
			state, public := readStudioRecoveryStatus(t, id)
			if !errors.Is(providerContext.Err(), context.Canceled) || state.Stage != "stopped" || state.Prompt != "A quiet performance" || posts.Load() != 1 {
				t.Fatal("cancelled work did not retain its request or made another render")
			}
			if stalledPhase != "starting" && !state.CanResume {
				t.Fatal("stopping an accepted provider operation lost recovery")
			}
			assertStudioRecoveryPublicJSON(t, public)
			assertStudioRecoveryPublicJSON(t, cancelResponse.Body.String())
			assertStudioRecoveryPublicJSON(t, w.Body.String())
		})
	}
}

func TestStudioGenerationRecoveryStopBeforeSubmissionBlocksProvider(t *testing.T) {
	isolateStudioRecovery(t)
	id := "fixture-stop-before-submit"
	providerCalls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		providerCalls++
		return nil, errors.New("unexpected provider request")
	})
	w := httptest.NewRecorder()
	studioGenerationCancelHandler(w, studioProgressRequest(http.MethodPost, "/studio/generation/cancel", studioRecoveryJSON(t, map[string]string{"generationId": id})))
	if w.Code != http.StatusOK {
		t.Fatal("could not stop a request before submission")
	}
	w = httptest.NewRecorder()
	studioVideoGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/videos/generate", studioRecoveryJSON(t, map[string]any{"generationId": id, "prompt": "This must stay stopped", "durationSeconds": 8})))
	if w.Code != http.StatusConflict || providerCalls != 0 {
		t.Fatal("Stop-before-submit allowed a charged provider request")
	}
}

func TestStudioGenerationRecoveryTerminalProviderFailureDoesNotResubmit(t *testing.T) {
	isolateStudioRecovery(t)
	id := "fixture-terminal-failure"
	referenceID := studioRecoveryReference(t)
	posts := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/videos/generations" {
			posts++
			return studioRecoveryResponse(200, "application/json", `{"request_id":"`+studioRecoveryOperation+`"}`), nil
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/videos/"+studioRecoveryOperation {
			return studioRecoveryResponse(200, "application/json", studioRecoveryJSON(t, map[string]any{"status": "failed", "error": "Temporarily unavailable, please retry " + studioRecoveryURL})), nil
		}
		return nil, errors.New("unexpected provider request")
	})
	w := httptest.NewRecorder()
	studioVideoGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/videos/generate", studioRecoveryJSON(t, map[string]any{"generationId": id, "prompt": "Keep the first request", "referenceId": referenceID})))
	state, public := readStudioRecoveryStatus(t, id)
	if w.Code != http.StatusBadGateway || posts != 1 || state.Stage != "failed" || state.CanResume || state.Prompt != "Keep the first request" {
		t.Fatal("terminal provider failure triggered another render or discarded the request")
	}
	assertStudioRecoveryPublicJSON(t, public)
	assertStudioRecoveryPublicJSON(t, w.Body.String())
}

func TestStudioGenerationRecoveryFindsSavedAssetBeforeFinalCheckpoint(t *testing.T) {
	isolateStudioRecovery(t)
	id := "fixture-crash-after-asset-save"
	checkpoint := studioGenerationCheckpoint{State: studioProgressState{
		Found: true, ID: id, Kind: "video", Stage: "saving", Prompt: "Keep this completed performance",
		AspectRatio: "16:9", DurationSeconds: 8, CanResume: true,
	}, OperationID: studioRecoveryOperation, VideoURL: studioRecoveryURL}
	if err := saveStudioGenerationCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	record, err := saveStudioAsset(studioSaveOptions{
		GenerationID: id, Prompt: checkpoint.State.Prompt, MediaType: "video", Source: xAIVideoSourceLabel,
		DataURI:     "data:video/mp4;base64," + base64.StdEncoding.EncodeToString([]byte(studioRecoveryMP4)),
		AspectRatio: checkpoint.State.AspectRatio, Duration: float64(checkpoint.State.DurationSeconds),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, found, err := loadStudioGenerationCheckpoint(id)
	if err != nil || !found || checkpoint.State.Stage != "saving" || checkpoint.State.Asset != nil {
		t.Fatal("fixture did not preserve the crash window before final checkpoint")
	}
	studioGenerations = newStudioProgressStore()
	providerCalls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		providerCalls++
		return nil, errors.New("completed asset recovery must not call a provider or media host")
	})
	state, public := readStudioRecoveryStatus(t, id)
	if state.Stage != "complete" || state.CanResume || state.Asset == nil || state.Asset.ID != record.ID {
		t.Fatal("restart did not find the asset saved before its completion checkpoint")
	}
	assertStudioRecoveryPublicJSON(t, public)
	w := httptest.NewRecorder()
	studioGenerationResumeHandler(w, studioProgressRequest(http.MethodPost, "/studio/generation/resume", studioRecoveryJSON(t, map[string]string{"generationId": id})))
	var response struct {
		Video studioImageSummary `json:"video"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Video.ID != record.ID || providerCalls != 0 {
		t.Fatal("recovery redownloaded a saved clip or returned a different asset")
	}
	assertStudioRecoveryPublicJSON(t, w.Body.String())
	checkpoint, found, err = loadStudioGenerationCheckpoint(id)
	if err != nil || !found || checkpoint.State.Stage != "complete" || checkpoint.State.Asset == nil || checkpoint.State.Asset.ID != record.ID || checkpoint.VideoURL != "" {
		t.Fatal("asset recovery did not repair the completion checkpoint")
	}
	videos, err := listStudioAssets("video", 12)
	if err != nil || len(videos) != 1 || videos[0].ID != record.ID {
		t.Fatal("crash-window recovery duplicated the saved clip")
	}
}

func TestStudioGenerationRecoveryStopsUncachedJobWithoutGrowingFullCache(t *testing.T) {
	isolateStudioProgress(t)
	id := "fixture-uncached-stop-generation"
	checkpoint := studioGenerationCheckpoint{State: studioProgressState{Found: true, ID: id, Kind: "video", Stage: "failed", Prompt: "Retain this request", CanResume: true}, OperationID: studioRecoveryOperation, VideoURL: studioRecoveryURL}
	if err := saveStudioGenerationCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	for index := range 512 {
		cachedID := fmt.Sprintf("fixture-cached-%03d", index)
		studioGenerations.entries[cachedID] = studioProgressEntry{state: studioProgressState{Found: true, ID: cachedID, Kind: "video", Stage: "complete"}, updated: time.Now()}
	}
	w := httptest.NewRecorder()
	studioGenerationCancelHandler(w, studioProgressRequest(http.MethodPost, "/studio/generation/cancel", studioRecoveryJSON(t, map[string]string{"generationId": id})))
	if w.Code != http.StatusOK || len(studioGenerations.entries) != 512 {
		t.Fatal("stopping an uncached saved job grew the full progress cache")
	}
	checkpoint, found, err := loadStudioGenerationCheckpoint(id)
	if err != nil || !found || checkpoint.State.Stage != "stopped" || checkpoint.State.Prompt != "Retain this request" || !checkpoint.State.CanResume {
		t.Fatal("uncached cancellation did not persist the stopped recoverable request")
	}
	state, public := readStudioRecoveryStatus(t, id)
	if state.Stage != "stopped" || !state.CanResume || len(studioGenerations.entries) != 512 {
		t.Fatal("reading the stopped uncached job grew the full progress cache")
	}
	assertStudioRecoveryPublicJSON(t, public)
}
