package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testInputSketch(t *testing.T) *sketchDocument {
	t.Helper()
	var document sketchDocument
	if err := json.Unmarshal([]byte(`{"version":1,"width":1000,"height":750,"annotations":[{"kind":"stroke","points":[{"x":0.1,"y":0.2},{"x":0.7,"y":0.8}],"color":"#111111","erase":false,"width":0.004},{"kind":"shape","shape":"arrow","start":{"x":0.2,"y":0.2},"end":{"x":0.8,"y":0.7},"color":"#e53935"},{"kind":"text","text":"Original idea","x":0.2,"y":0.3,"width":0.4,"fontSize":0.02,"color":"#111111"}]}`), &document); err != nil {
		t.Fatal(err)
	}
	return &document
}

func sketchTestPNG(t *testing.T, shade color.Color) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	picture.Set(0, 0, shade)
	var data bytes.Buffer
	if err := png.Encode(&data, picture); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestValidateSketchDocument(t *testing.T) {
	document := testInputSketch(t)
	document.Background = "data:image/png;base64," + base64.StdEncoding.EncodeToString(sketchTestPNG(t, color.Black))
	if err := validateSketchDocument(document); err != nil {
		t.Fatal(err)
	}
	for name, annotation := range map[string]string{
		"outside canvas":     `{"kind":"stroke","points":[{"x":1.1,"y":0.2}],"color":"#111111"}`,
		"missing coordinate": `{"kind":"stroke","points":[{"x":0.1}],"color":"#111111"}`,
		"unknown shape":      `{"kind":"shape","shape":"script","start":{"x":0.1,"y":0.1},"end":{"x":0.2,"y":0.2},"color":"#111111"}`,
		"external paint":     `{"kind":"stroke","points":[{"x":0.1,"y":0.2}],"color":"url(https://example.com)"}`,
		"unknown property":   `{"kind":"stroke","points":[{"x":0.1,"y":0.2}],"color":"#111111","href":"https://example.com"}`,
		"negative text size": `{"kind":"text","text":"note","x":0.1,"y":0.2,"width":0.3,"fontSize":-1,"color":"#111111"}`,
	} {
		t.Run(name, func(t *testing.T) {
			document := testInputSketch(t)
			document.Annotations = []json.RawMessage{json.RawMessage(annotation)}
			if validateSketchDocument(document) == nil {
				t.Fatal("invalid drawing accepted")
			}
		})
	}
	for _, background := range []string{"https://example.com/image.png", "data:image/svg+xml;base64,YQ==", "data:image/png;base64,YQ==", "data:image/png;base64," + strings.Repeat("A", maxSketchBackgroundCharacters)} {
		document.Background = background
		if validateSketchDocument(document) == nil {
			t.Fatal("invalid background accepted")
		}
	}
	document = testInputSketch(t)
	document.Version = 2
	if validateSketchDocument(document) == nil {
		t.Fatal("unknown document version accepted")
	}
}

func TestPrototypePreservesInputSketchAndAttachments(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	drawing := testInputSketch(t)
	original := sketchTestPNG(t, color.Black)
	part := map[string]any{"type": "file", "filename": "sketch.png", "url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(original)}
	first, second := "", ""
	html := "<!doctype html><html><body>First</body></html>"
	if err := saveChatPrototypeWithSketch(root, html, "", []chatMessage{{Role: "user", Text: "Original request"}}, "test/model", []map[string]any{part}, drawing, &first); err != nil {
		t.Fatal(err)
	}
	var request struct {
		InputSketchPath string                                  `json:"inputSketchPath"`
		Saved           bool                                    `json:"saved"`
		Attachments     []struct{ Filename, Path, Mime string } `json:"attachments"`
	}
	data, err := os.ReadFile(filepath.Join(first, "request.json"))
	if err != nil || json.Unmarshal(data, &request) != nil || !request.Saved || request.InputSketchPath != "input-sketch.json" || len(request.Attachments) != 1 {
		t.Fatalf("missing input links: %s, %v", data, err)
	}
	before, err := os.ReadFile(filepath.Join(first, request.InputSketchPath))
	if err != nil {
		t.Fatal(err)
	}
	drawing.Annotations = nil
	part["url"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(sketchTestPNG(t, color.White))
	if err := saveChatPrototypeWithSketch(root, "<!doctype html><html><body>Second</body></html>", html, nil, "test/other", []map[string]any{part}, drawing, &second); err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("new result reused its predecessor's record")
	}
	after, _ := os.ReadFile(filepath.Join(first, request.InputSketchPath))
	if !bytes.Equal(before, after) {
		t.Fatal("earlier vector drawing changed")
	}
	snapshot, _ := os.ReadFile(filepath.Join(first, request.Attachments[0].Path))
	if !bytes.Equal(snapshot, original) {
		t.Fatal("earlier attachment was overwritten")
	}
	current, _ := os.ReadFile(filepath.Join(root, "prototype/assets/sketch.png"))
	if bytes.Equal(current, original) {
		t.Fatal("test did not replace the current asset")
	}
}

func TestInputSketchRejectsInvalidOrUnrelatedSaves(t *testing.T) {
	document := testInputSketch(t)
	request := chatRequest{Mode: "chat", Model: "test/model", Messages: []chatMessage{{Role: "user", Text: "Hi"}}, InputSketch: document}
	if validateChatRequest(request) == nil {
		t.Fatal("ordinary chat accepted a prototype sketch")
	}
	request.Mode = "prototype"
	if err := validateChatRequest(request); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	document.Width = -1
	if saveChatPrototypeWithSketch(root, "<!doctype html><html></html>", "", nil, "test/model", nil, document) == nil {
		t.Fatal("invalid sketch saved")
	}
	if _, err := os.Stat(filepath.Join(root, "prototype/index.html")); !os.IsNotExist(err) {
		t.Fatal("invalid input changed the project")
	}
}
