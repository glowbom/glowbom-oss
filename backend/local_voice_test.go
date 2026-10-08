package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type localVoiceTransport func(*http.Request) (*http.Response, error)

func (f localVoiceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func localVoiceTestClient(t *testing.T, check localVoiceTransport) *http.Client {
	t.Helper()
	return &http.Client{Transport: check, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestLocalVoiceListsVoiceStudioVoices(t *testing.T) {
	client := localVoiceTestClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != voiceStudioBaseURL+"/v1/audio/voices" {
			t.Fatalf("unexpected voice discovery request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"voices":[{"id":"preset:soft","name":"Soft"},{"voice_id":"clone:mine","name":"My voice"},"preset:bright"]}`)), Header: make(http.Header)}, nil
	})
	recorder := httptest.NewRecorder()
	localVoiceVoicesHandler(client)(recorder, httptest.NewRequest(http.MethodGet, "/audio/local/voices", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected response: %d %s", recorder.Code, recorder.Body.String())
	}
	var reply ElevenLabsVoicesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Voices) != 3 || reply.Voices[0].Name != "Soft" || reply.Voices[1].VoiceID != "clone:mine" || reply.Voices[2].VoiceID != "preset:bright" {
		t.Fatalf("unexpected voices: %+v", reply.Voices)
	}
}

func TestLocalVoiceEmptyListRemainsEmpty(t *testing.T) {
	voices, err := parseLocalVoices([]byte(`{"voices":[]}`))
	if err != nil || len(voices) != 0 {
		t.Fatalf("empty upstream list must stay empty: %+v %v", voices, err)
	}
}

func TestLocalVoiceSpeechUsesFixedLoopbackProvider(t *testing.T) {
	wav := append([]byte("RIFF\x04\x00\x00\x00WAVE"), []byte("fixture")...)
	client := localVoiceTestClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.String() != voiceStudioBaseURL+"/v1/audio/speech" {
			t.Fatalf("unexpected speech request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("missing JSON content type")
		}
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input["model"] != voiceStudioSpeechModel || input["voice"] != "preset:soft" || input["input"] != "Hello locally." || input["response_format"] != "wav" {
			t.Fatalf("unexpected VoiceStudio payload: %+v", input)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(wav)), Header: http.Header{"Content-Type": []string{"audio/wav"}}}, nil
	})
	recorder := httptest.NewRecorder()
	localVoiceSpeechHandler(client)(recorder, httptest.NewRequest(http.MethodPost, "/audio/local", strings.NewReader(`{"prompt":"Hello locally.","voiceId":"preset:soft"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("speech failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var reply ElevenLabsAudioResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	encoded := strings.TrimPrefix(reply.Audio, "data:audio/wav;base64,")
	got, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !bytes.Equal(got, wav) || reply.MimeType != "audio/wav" || reply.SavedPath != "" {
		t.Fatalf("unexpected local speech response: %+v, decode: %v", reply, err)
	}
}

func TestLocalVoiceUnavailableAndInvalidResponses(t *testing.T) {
	for _, test := range []struct {
		name   string
		client *http.Client
		status int
	}{
		{"offline", localVoiceTestClient(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("private local path") }), http.StatusServiceUnavailable},
		{"invalid audio", localVoiceTestClient(t, func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("<html>not audio</html>")), Header: make(http.Header)}, nil
		}), http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			localVoiceSpeechHandler(test.client)(recorder, httptest.NewRequest(http.MethodPost, "/audio/local", strings.NewReader(`{"prompt":"Hello"}`)))
			if recorder.Code != test.status || strings.Contains(recorder.Body.String(), "private local path") {
				t.Fatalf("unsafe or wrong error: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestLocalVoiceRejectsInvalidInputWithoutCallingProvider(t *testing.T) {
	client := localVoiceTestClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid input reached VoiceStudio")
		return nil, nil
	})
	for _, body := range []string{`{"prompt":""}`, `{"prompt":"` + strings.Repeat("x", localVoiceTextLimit+1) + `"}`, `{"prompt":"hello","voiceId":"bad\nvoice"}`} {
		recorder := httptest.NewRecorder()
		localVoiceSpeechHandler(client)(recorder, httptest.NewRequest(http.MethodPost, "/audio/local", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid request accepted: %d %s", recorder.Code, recorder.Body.String())
		}
	}
}
