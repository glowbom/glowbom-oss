package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompanionImageProgressKeepsPromptWithoutReferencePayload(t *testing.T) {
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	request := companionImageRequest{Prompt: "Draw a small mountain village", SourceID: "qa-images",
		AspectRatio: "1:1", ReferenceImage: "private-reference-payload", ReferenceID: "private-reference-id"}
	w := httptest.NewRecorder()
	s.startJob(w, httptest.NewRequest(http.MethodPost, "/", nil), "image", companionProject{}, "/mock-images", request)
	if w.Code != http.StatusAccepted {
		t.Fatalf("image request status: %d", w.Code)
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["prompt"] != request.Prompt || result["sourceId"] != request.SourceID {
		t.Fatal("running image lost its prompt or provider")
	}
	for _, snapshot := range []string{w.Body.String(), string(mustImageProgressJSON(t, s.jobList("image")))} {
		if strings.Contains(snapshot, "private-reference") {
			t.Fatal("image progress exposed its reference payload")
		}
	}
	build := &companionJob{kind: "build", imagePrompt: request.Prompt, imageSourceID: request.SourceID}
	if result := build.snapshot(false); result["prompt"] != nil || result["sourceId"] != nil {
		t.Fatal("image prompt leaked into an unrelated build")
	}
}

func mustImageProgressJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
