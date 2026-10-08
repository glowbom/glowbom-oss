package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewedAudioModelsReachProviderOnce(t *testing.T) {
	for _, test := range []struct {
		name, audioType, model, path string
	}{
		{"v4 dialogue", "voice", "eleven_v4", "/v1/text-to-dialogue"},
		{"custom voice", "voice", "custom_model-2026.10", "/v1/text-to-speech/reviewedNarrator"},
		{"music v2.5", "music", "music_v2_5", "/v1/music"},
		{"custom music", "music", "custom_model-2026.10", "/v1/music"},
		{"custom sound", "sound", "custom_model-2026.10", "/v1/sound-generation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
			token := "glowbyaudio:reviewed audio|type:" + test.audioType
			project := mediaEditProject(t, `<audio src="`+token+`"></audio>`)
			if err := os.WriteFile(filepath.Join(project, "glowbom.json"), []byte(`{"name":"Reviewed audio models"}`), 0600); err != nil {
				t.Fatal(err)
			}
			plan, err := buildOpenCodeMediaApproval(OpenCodeMediaPostPassRequest{ProjectPath: project})
			if err != nil || plan == nil || len(plan.Items) != 1 {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			item := plan.Items[0]
			item.ModelID = test.model
			if test.audioType == "voice" {
				item.VoiceID = "reviewedNarrator"
			} else {
				item.DurationSeconds = 12
			}
			approvalID, responses := registerOpenCodeMediaApproval(project, plan)
			t.Cleanup(func() { removeOpenCodeMediaApproval(approvalID) })
			payload, _ := json.Marshal(openCodeMediaApprovalResponse{ApprovalID: approvalID, ProjectPath: project, Response: "generate", Items: []OpenCodeMediaApprovalItem{item}})
			recorder := httptest.NewRecorder()
			openCodeMediaApprovalRespondHandler(recorder, httptest.NewRequest(http.MethodPost, "/media", strings.NewReader(string(payload))))
			if recorder.Code != http.StatusOK {
				t.Fatalf("review failed: %d %s", recorder.Code, recorder.Body.String())
			}
			decision, err := waitForOpenCodeMediaApproval(context.Background(), approvalID, responses)
			if err != nil || len(decision.Items) != 1 || decision.Items[0].ModelID != test.model {
				t.Fatalf("review changed model: decision=%+v err=%v", decision, err)
			}
			calls := 0
			mockStudioAudioProvider(t, func(request *http.Request) (*http.Response, error) {
				calls++
				var body map[string]any
				if request.URL.Path != test.path || request.Header.Get("xi-api-key") != "audio-model-fixture-key" || json.NewDecoder(request.Body).Decode(&body) != nil || body["model_id"] != test.model {
					t.Fatalf("reviewed model did not reach provider: path=%s body=%#v", request.URL.Path, body)
				}
				if test.model == "eleven_v4" {
					inputs, ok := body["inputs"].([]any)
					if !ok || len(inputs) != 1 {
						t.Fatalf("dialogue inputs=%#v", body["inputs"])
					}
					input, ok := inputs[0].(map[string]any)
					if !ok || input["text"] != "reviewed audio" || input["voice_id"] != "reviewedNarrator" {
						t.Fatalf("dialogue input=%#v", inputs[0])
					}
				}
				if test.audioType == "music" && body["music_length_ms"] != float64(12000) {
					t.Fatalf("reviewed music length changed: %#v", body)
				}
				return studioAudioProviderResponse(), nil
			})
			result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: decision.Items, ElevenLabsKey: "audio-model-fixture-key", ElevenLabsVoiceID: "reviewedNarrator"})
			if err != nil || result == nil || len(result.GeneratedAssets) != 1 || calls != 1 {
				t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
			}
			saved, err := findStudioAsset(result.GeneratedAssets[0].StudioAssetID)
			if err != nil || saved.Model != test.model || saved.AudioType != test.audioType {
				t.Fatalf("saved audio lost model settings: %+v err=%v", saved, err)
			}
		})
	}
}

func TestInvalidReviewedAudioModelsRejectBeforeAnyAssetGeneration(t *testing.T) {
	for _, test := range []struct {
		name, audioType, model, prompt string
		loop                           bool
	}{
		{"v4 reliable length", "voice", "eleven_v4", strings.Repeat("あ", 2001), false},
		{"v4 Turbo requires WebSocket", "voice", "eleven_v4_turbo", "Hello", false},
		{"custom sound cannot loop", "sound", "custom_sound", "Rain", true},
		{"unsafe model identifier", "music", "bad/model", "Piano", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, plan := imageSettingsFixture(t, `<img src="glowbomimage:paid first image"><audio src="glowbyaudio:invalid audio|type:`+test.audioType+`"></audio>`)
			item := plan.Items[1]
			item.Prompt, item.ModelID, item.Loop = test.prompt, test.model, test.loop
			plan.Items[1] = item
			calls := 0
			previousImage := generatePostPassSelectedImage
			generatePostPassSelectedImage = func(context.Context, string, string, string, string, string) (string, string, error) {
				calls++
				t.Fatal("invalid audio settings spent on an earlier image in the batch")
				return "", "", nil
			}
			t.Cleanup(func() { generatePostPassSelectedImage = previousImage })
			mockStudioAudioProvider(t, func(*http.Request) (*http.Response, error) {
				calls++
				t.Fatal("invalid audio settings reached the provider")
				return nil, nil
			})
			approvalID, responses := registerOpenCodeMediaApproval(project, plan)
			t.Cleanup(func() { removeOpenCodeMediaApproval(approvalID) })
			body, _ := json.Marshal(openCodeMediaApprovalResponse{ApprovalID: approvalID, ProjectPath: project, Response: "generate", Items: plan.Items})
			recorder := httptest.NewRecorder()
			openCodeMediaApprovalRespondHandler(recorder, httptest.NewRequest(http.MethodPost, "/media", strings.NewReader(string(body))))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("invalid audio accepted at review: %d %s", recorder.Code, recorder.Body.String())
			}
			select {
			case <-responses:
				t.Fatal("invalid audio consumed the pending approval")
			default:
			}
			if result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: plan.Items, OpenAIKey: "image-fixture-key", ElevenLabsKey: "audio-fixture-key"}); err == nil || result != nil || calls != 0 {
				t.Fatalf("invalid audio reached generation: calls=%d result=%+v err=%v", calls, result, err)
			}
		})
	}
}

func TestReviewedCustomAudioProviderFailureHasNoFallback(t *testing.T) {
	for _, audioType := range []string{"music", "sound"} {
		t.Run(audioType, func(t *testing.T) {
			t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
			token := "glowbyaudio:custom reviewed audio|type:" + audioType
			project := mediaEditProject(t, `<audio src="`+token+`"></audio>`)
			item := OpenCodeMediaApprovalItem{ID: mediaApprovalItemID("audio", token), Placeholder: token, MediaType: "audio", SourceID: "elevenlabs-api", Prompt: "custom reviewed audio", AudioType: audioType, ModelID: "custom_model-2026.10", DurationSeconds: 12}
			calls := 0
			mockStudioAudioProvider(t, func(request *http.Request) (*http.Response, error) {
				calls++
				var body map[string]any
				if json.NewDecoder(request.Body).Decode(&body) != nil || body["model_id"] != item.ModelID {
					t.Fatalf("custom model changed: %#v", body)
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"detail":{"status":"model_not_found","message":"private provider message and audio-fixture-key"}}`))}, nil
			})
			result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: []OpenCodeMediaApprovalItem{item}, ElevenLabsKey: "audio-fixture-key", ElevenLabsVoiceID: "reviewedNarrator"})
			if err != nil || result == nil || calls != 1 || len(result.GeneratedAssets) != 0 || len(result.Warnings) == 0 {
				t.Fatalf("custom provider failure changed flow: calls=%d result=%+v err=%v", calls, result, err)
			}
			warnings := strings.Join(result.Warnings, " ")
			if strings.Contains(warnings, "private provider") || strings.Contains(warnings, "audio-fixture-key") {
				t.Fatal("provider failure exposed private data")
			}
		})
	}
}
