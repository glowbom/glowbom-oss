package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestInstructionUpload(t *testing.T) {
	directory := t.TempDir()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i := 0; i < 2; i++ {
		part, _ := writer.CreateFormFile("files", "../../same.txt")
		_, _ = part.Write([]byte("attachment content"))
	}
	_ = writer.Close()
	request := httptest.NewRequest("POST", "/", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	instructionUploadHandler(directory)(response, request)
	var result openCodeInstructionFilesPickResponse
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Files) != 2 {
		t.Fatal(response.Body.String())
	}
	if result.Files[0].Path == result.Files[1].Path {
		t.Fatal("duplicate names overwrote an upload")
	}
	for _, file := range result.Files {
		relative, err := filepath.Rel(directory, file.Path)
		if err != nil || !filepath.IsLocal(relative) || file.Name != "same.txt" {
			t.Fatal(file)
		}
		data, err := os.ReadFile(file.Path)
		if err != nil || string(data) != "attachment content" {
			t.Fatal("content changed")
		}
		info, _ := os.Stat(file.Path)
		if info.Mode().Perm() != 0600 {
			t.Fatal("upload must be private")
		}
	}
}
func TestInstructionUploadRejectsInvalidRequests(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		response := httptest.NewRecorder()
		instructionUploadHandler(t.TempDir())(response, httptest.NewRequest(method, "/", bytes.NewBufferString("invalid")))
		if response.Code < 400 {
			t.Fatal("invalid request accepted")
		}
	}
}

func TestInstructionUploadRejectsOversize(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("files", "large.txt")
	_, _ = part.Write(make([]byte, 42*1024*1024))
	_ = writer.Close()
	request := httptest.NewRequest("POST", "/", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	directory := t.TempDir()
	response := httptest.NewRecorder()
	instructionUploadHandler(directory)(response, request)
	if response.Code != 400 {
		t.Fatalf("oversize upload accepted: %d", response.Code)
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 0 {
		t.Fatal("rejected upload left files")
	}
}
