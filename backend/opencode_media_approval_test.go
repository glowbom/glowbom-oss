package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeMediaGenerationPolicy(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: "", want: "auto"},
		{value: "auto", want: "auto"},
		{value: "ASK", want: "ask"},
		{value: " skip ", want: "skip"},
		{value: "unexpected", want: "ask"},
	}

	for _, test := range tests {
		if got := normalizeMediaGenerationPolicy(test.value); got != test.want {
			t.Errorf("normalizeMediaGenerationPolicy(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestBuildOpenCodeMediaApprovalListsPaidPlaceholders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectPath := t.TempDir()
	prototypePath := filepath.Join(projectPath, "prototype")
	if err := os.MkdirAll(prototypePath, 0755); err != nil {
		t.Fatalf("create prototype directory: %v", err)
	}

	html := `<script>
const hero = "glowbyimage:approval-test-hero";
const clip = "glowbyvideo:approval-test-motion|from:approval-test-hero|aspect:16:9";
const voice = "glowbyaudio:approval-test-narration|type:voice";
</script>`
	if err := os.WriteFile(filepath.Join(prototypePath, "index.html"), []byte(html), 0644); err != nil {
		t.Fatalf("write prototype: %v", err)
	}

	plan, err := buildOpenCodeMediaApproval(OpenCodeMediaPostPassRequest{
		ProjectPath: projectPath,
		ImageSource: "Approval Test Images",
	})
	if err != nil {
		t.Fatalf("buildOpenCodeMediaApproval() error = %v", err)
	}
	if plan == nil {
		t.Fatal("buildOpenCodeMediaApproval() = nil, want approval plan")
	}
	if got, want := len(plan.Items), 3; got != want {
		t.Fatalf("len(plan.Items) = %d, want %d", got, want)
	}

	mediaTypes := map[string]bool{}
	for _, item := range plan.Items {
		mediaTypes[item.MediaType] = true
	}
	for _, mediaType := range []string{"image", "video", "audio"} {
		if !mediaTypes[mediaType] {
			t.Errorf("approval plan missing %s item", mediaType)
		}
	}
}

func TestBuildOpenCodeMediaApprovalReturnsNilWithoutPaidPlaceholders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectPath := t.TempDir()
	prototypePath := filepath.Join(projectPath, "prototype")
	if err := os.MkdirAll(prototypePath, 0755); err != nil {
		t.Fatalf("create prototype directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prototypePath, "index.html"), []byte("<main>No media placeholders</main>"), 0644); err != nil {
		t.Fatalf("write prototype: %v", err)
	}

	plan, err := buildOpenCodeMediaApproval(OpenCodeMediaPostPassRequest{ProjectPath: projectPath})
	if err != nil {
		t.Fatalf("buildOpenCodeMediaApproval() error = %v", err)
	}
	if plan != nil {
		t.Fatalf("buildOpenCodeMediaApproval() = %#v, want nil", plan)
	}
}

func TestOpenCodeMediaApprovalRespondHandlerDeliversDecision(t *testing.T) {
	projectPath := t.TempDir()
	approvalID, response := registerOpenCodeMediaApproval(projectPath)
	defer removeOpenCodeMediaApproval(approvalID)

	body := bytes.NewBufferString(`{"approvalID":"` + approvalID + `","response":"skip","projectPath":"` + projectPath + `"}`)
	request := httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", body)
	recorder := httptest.NewRecorder()

	openCodeMediaApprovalRespondHandler(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("handler status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	select {
	case got := <-response:
		if got != "skip" {
			t.Fatalf("approval response = %q, want skip", got)
		}
	default:
		t.Fatal("approval response was not delivered")
	}

	secondRequest := httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", bytes.NewBufferString(
		`{"approvalID":"`+approvalID+`","response":"generate","projectPath":"`+projectPath+`"}`,
	))
	secondRecorder := httptest.NewRecorder()
	openCodeMediaApprovalRespondHandler(secondRecorder, secondRequest)
	if secondRecorder.Code != http.StatusNotFound {
		t.Fatalf("second handler status = %d, want %d", secondRecorder.Code, http.StatusNotFound)
	}
}

func TestOpenCodeMediaApprovalRespondHandlerRejectsWrongProject(t *testing.T) {
	approvalID, _ := registerOpenCodeMediaApproval(t.TempDir())
	defer removeOpenCodeMediaApproval(approvalID)

	body := bytes.NewBufferString(`{"approvalID":"` + approvalID + `","response":"generate","projectPath":"` + t.TempDir() + `"}`)
	request := httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", body)
	recorder := httptest.NewRecorder()

	openCodeMediaApprovalRespondHandler(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("handler status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}
