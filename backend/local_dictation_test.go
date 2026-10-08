package main

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testDictationWAV(seconds int, amplitude int16) []byte {
	data := make([]byte, 44+seconds*32000)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 16000)
	binary.LittleEndian.PutUint32(data[28:], 32000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(data)-44))
	for i := 44; i < len(data); i += 2 {
		binary.LittleEndian.PutUint16(data[i:], uint16(amplitude))
	}
	return data
}
func TestDictationWAV(t *testing.T) {
	for _, seconds := range []int{1, 60} {
		if err := validateDictationWAV(testDictationWAV(seconds, 500)); err != nil {
			t.Fatal(err)
		}
	}
	for _, data := range [][]byte{nil, []byte("audio"), testDictationWAV(0, 500), testDictationWAV(61, 500), testDictationWAV(1, 0)} {
		if validateDictationWAV(data) == nil {
			t.Fatal("accepted invalid recording")
		}
	}
	data := testDictationWAV(1, 500)
	binary.LittleEndian.PutUint32(data[40:], 0xffffffff)
	if validateDictationWAV(data) == nil {
		t.Fatal("accepted invalid chunk")
	}
	data = testDictationWAV(1, 500)
	binary.LittleEndian.PutUint32(data[24:], 48000)
	if validateDictationWAV(data) == nil {
		t.Fatal("accepted wrong sample rate")
	}
}
func TestDictationRequiresAuthentication(t *testing.T) {
	for _, handler := range []http.HandlerFunc{dictationSettingsHandler, dictationTranscribeHandler} {
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest("GET", "/", nil))
		if recorder.Code != 401 {
			t.Fatalf("status %d", recorder.Code)
		}
	}
}

func TestDictationTranscriptionCleansTemporaryAudio(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable")
	}
	dir := t.TempDir()
	engine := filepath.Join(dir, "whisper-cli")
	marker := filepath.Join(dir, "recording-path")
	script := `#!/bin/sh
while [ "$#" -gt 0 ]; do
 case "$1" in -f) shift; audio=$1;; -of) shift; output=$1;; esac
 shift
done
printf '%s' "$audio" > "$DICTATION_TEST_MARKER"
printf 'Hello local world.' > "$output.txt"
`
	if err := os.WriteFile(engine, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "model.bin")
	if err := os.WriteFile(model, make([]byte, 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLOWBOM_WHISPER_BIN", engine)
	t.Setenv("GLOWBOM_WHISPER_MODEL", model)
	t.Setenv("GLOWBOM_SERVER_TOKEN", "dictation-test")
	t.Setenv("DICTATION_TEST_MARKER", marker)
	req := httptest.NewRequest("POST", "/audio/transcribe", bytes.NewReader(testDictationWAV(1, 500)))
	req.Header.Set("Authorization", "Bearer dictation-test")
	rec := httptest.NewRecorder()
	dictationTranscribeHandler(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Hello local world.") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	path, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(string(path))); !os.IsNotExist(err) {
		t.Fatalf("temporary audio remains: %v", err)
	}

	// A failed engine must remove the same private temporary directory.
	if err := os.WriteFile(engine, []byte(script+"exit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("POST", "/audio/transcribe", bytes.NewReader(testDictationWAV(1, 500)))
	req.Header.Set("Authorization", "Bearer dictation-test")
	rec = httptest.NewRecorder()
	dictationTranscribeHandler(rec, req)
	if rec.Code != 502 {
		t.Fatalf("failure status %d", rec.Code)
	}
	path, err = os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(string(path))); !os.IsNotExist(err) {
		t.Fatalf("failed recording remains: %v", err)
	}
}

func TestDictationLiveSample(t *testing.T) {
	sample := os.Getenv("GLOWBOM_TEST_DICTATION_SAMPLE")
	if sample == "" {
		t.Skip("set GLOWBOM_TEST_DICTATION_SAMPLE for installed Whisper verification")
	}
	data, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLOWBOM_SERVER_TOKEN", "dictation-live-test")
	req := httptest.NewRequest("POST", "/audio/transcribe", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer dictation-live-test")
	rec := httptest.NewRecorder()
	dictationTranscribeHandler(rec, req)
	if rec.Code != 200 || !strings.Contains(strings.ToLower(rec.Body.String()), "local microphone transcription test") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
