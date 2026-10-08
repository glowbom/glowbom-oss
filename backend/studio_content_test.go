package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestStudioImageContentPreservesBytesAndReportsStoredFormat(t *testing.T) {
	png := projectIconTestImage(t, "png")
	jpeg := projectIconTestImage(t, "jpeg")
	webp, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		body     []byte
		declared string
		want     string
	}{
		{"bare PNG", png, "", "image/png"},
		{"bare JPEG", jpeg, "", "image/jpeg"},
		{"bare WebP", webp, "", "image/webp"},
		{"mislabeled PNG", png, "image/jpeg", "image/png"},
		{"explicit unsupported signature", []byte("unsniffable fixture"), "image/avif", "image/avif"},
		{"nonmedia declaration", []byte("unsniffable fixture"), "text/html", "image/jpeg"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("GLOWBOM_STUDIO_DIR", dir)
			const id = "C5C6B416-6161-4FC9-BA81-BA823A5A3F47"
			payload := base64.StdEncoding.EncodeToString(test.body)
			if test.declared != "" {
				payload = "data:" + test.declared + ";base64," + payload
			}
			record, err := json.Marshal(studioImageRecord{ID: id, MediaType: "image", DataBase64: payload})
			if err != nil {
				t.Fatal(err)
			}
			assets := filepath.Join(dir, "Assets")
			if err := os.MkdirAll(assets, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(assets, id+".json"), record, 0600); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			studioImageContentHandler(response, httptest.NewRequest(http.MethodGet, "/studio/images/content?id="+id, nil))
			if response.Code != http.StatusOK || response.Header().Get("Content-Type") != test.want {
				t.Fatalf("got status %d, type %q; want 200, %q", response.Code, response.Header().Get("Content-Type"), test.want)
			}
			if !bytes.Equal(response.Body.Bytes(), test.body) {
				t.Fatal("Studio changed the stored media bytes")
			}
		})
	}
}
