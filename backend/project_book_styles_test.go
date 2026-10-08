package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBookWritingPreferencesRoundTripAndValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := readBookWritingPreferences(); got.Style != "announcement" || got.Profanity {
		t.Fatalf("unsafe default: %+v", got)
	}
	prompt := strings.Repeat("界", 4000)
	value := bookWritingPreferences{Style: "custom", CustomPrompt: prompt}
	data, _ := json.Marshal(value)
	response := httptest.NewRecorder()
	projectBookWritingSettingsHandler(response, httptest.NewRequest("POST", "/settings/project-book", bytes.NewReader(data)))
	if response.Code != 200 || readBookWritingPreferences() != value {
		t.Fatalf("custom preference did not persist: %d", response.Code)
	}
	for _, body := range []string{`{"style":"unknown"}`, `{"style":"custom","customPrompt":" "}`, `{"style":"roast"} {}`, `{"style":"roast","extra":true}`, `{"style":"custom","customPrompt":"` + strings.Repeat("x", 4001) + `"}`} {
		response = httptest.NewRecorder()
		projectBookWritingSettingsHandler(response, httptest.NewRequest("POST", "/settings/project-book", strings.NewReader(body)))
		if response.Code != 400 || readBookWritingPreferences() != value {
			t.Fatal("invalid preference changed saved settings", response.Code)
		}
	}
	path, _ := bookWritingPreferencesPath()
	if err := os.WriteFile(path, []byte(`{"style":"custom","customPrompt":""}`), 0600); err != nil {
		t.Fatal(err)
	}
	if readBookWritingPreferences().Style != "announcement" {
		t.Fatal("invalid saved settings did not fall back")
	}
}

func TestBookStylesKeepGroundingAndPairDrawingDirections(t *testing.T) {
	for _, style := range bookWritingStyles {
		preferences := bookWritingPreferences{Style: style.ID, CustomPrompt: "Write a short social post. Draw a notebook sketch."}
		prompt := bookStyledVisualPrompt(preferences)
		for _, required := range []string{style.Direction, bookDrawingDirection(style.ID), "Never turn a request alone", "Do not use profanity.", "normalized coordinates"} {
			if !strings.Contains(prompt, required) {
				t.Fatalf("%s missing %q", style.ID, required)
			}
		}
		if style.ID == "custom" && !strings.Contains(prompt, preferences.CustomPrompt) {
			t.Fatal("custom guidance was lost")
		}
	}
	if !strings.Contains(bookWritingPrompt(bookStoryPrompt, bookWritingPreferences{Style: "unhinged", Profanity: true}), "Use occasional profanity") {
		t.Fatal("swearing opt-in ignored")
	}
}

func TestBookProfanityOnlyAppliesToOptedInComedyVoices(t *testing.T) {
	for _, style := range bookWritingStyles {
		for _, enabled := range []bool{false, true} {
			prompt := bookWritingPrompt(bookStoryPrompt, bookWritingPreferences{Style: style.ID, Profanity: enabled})
			wantProfanity := enabled && (style.ID == "unhinged" || style.ID == "standup")
			if strings.Contains(prompt, "Use occasional profanity") != wantProfanity || strings.Contains(prompt, "Do not use profanity.") == wantProfanity {
				t.Fatalf("%s with profanity=%t has conflicting swearing instructions", style.ID, enabled)
			}
			if wantProfanity && (!strings.Contains(prompt, "one or two swear words") || !strings.Contains(prompt, "Never aim profanity at people")) {
				t.Fatal("comedy swearing lost its scope")
			}
		}
	}
}

func TestBookSuperFunnyCatalogProvidesDistinctCleanComedy(t *testing.T) {
	var selected map[string]string
	for _, style := range bookWritingStyleCatalog() {
		if style["id"] == "superfunny" {
			if selected != nil {
				t.Fatal("Super Funny appears twice in the style catalog")
			}
			selected = style
		}
	}
	if selected == nil || selected["name"] != "Super Funny" || selected["description"] == "" || selected["sample"] == "" || selected["drawing"] == "" {
		t.Fatal("Super Funny is missing from the complete public style catalog")
	}
	prompt := bookStyledVisualPrompt(bookWritingPreferences{Style: "superfunny", Profanity: true})
	for _, required := range []string{"rapid-fire punchlines", "escalating silly metaphors", "general reader", "actual change in plain words", "not evidence about this result", selected["sample"], selected["drawing"], "Do not use profanity."} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("Super Funny lost %q", required)
		}
	}
	if strings.Contains(prompt, "Use occasional profanity") || bookStyleAllowsProfanity("superfunny") {
		t.Fatal("Super Funny inherited another comedy voice's swearing opt-in")
	}
}

func TestBookComedyPreferencesPersistAndReachAutomaticGeneration(t *testing.T) {
	for _, style := range []string{"unhinged", "standup", "superfunny"} {
		t.Run(style, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			preferences := bookWritingPreferences{Style: style, Profanity: bookStyleAllowsProfanity(style)}
			data, _ := json.Marshal(preferences)
			response := httptest.NewRecorder()
			projectBookWritingSettingsHandler(response, httptest.NewRequest("POST", "/settings/project-book", bytes.NewReader(data)))
			if response.Code != 200 || readBookWritingPreferences() != preferences {
				t.Fatal("comedy preferences were not saved", response.Code)
			}
			project := bookTestProject(t)
			bookTestBuild(t, project)
			bookTestWrite(t, project, "web/page.tsx", []byte("export function Garden() { return <main><h1>Garden</h1><button>Plant</button></main> }"))
			_, imported := bookTestCall(t, project, "sync", nil)
			answer := bookParserTestResponse
			var calls atomic.Int32
			profanityDirection := "Do not use profanity."
			if preferences.Profanity {
				profanityDirection = "Use occasional profanity"
			}
			server := bookStoryModelServer(t, &answer, &calls, "at least two distinct", profanityDirection, bookDrawingDirection(style), bookStoryInstructions)
			defer server.Close()
			t.Setenv("OPENCODE_URL", server.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			book, changed, err := drawProjectBookVisual(ctx, project, imported.Entries[0].Source, "", "")
			if err != nil || !changed || calls.Load() != 1 || book.Entries[0].Title != "A clearer garden" || book.Entries[0].Sketch.Visual == nil {
				t.Fatal("automatic generation lost the selected comedy voice", err)
			}
		})
	}
}

func TestBookStylePreviewPreservesSavedEntryAndDetectsLaterEdits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := bookTestProject(t)
	bookTestBuild(t, project)
	bookTestWrite(t, project, "web/page.tsx", []byte("export function Garden() { return <main><h1>Garden</h1><button>Plant</button></main> }"))
	_, imported := bookTestCall(t, project, "sync", nil)
	entry := imported.Entries[0]
	_, edited := bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": "Owner title", "body": "Owner text preserved in the book.", "expectedContentVersion": entry.ContentVersion})
	entry = edited.Entries[0]
	answer := `{"story":{"title":"A garden, with personality","body":"The saved garden page includes a Plant button. A small new beginning for this project's web experience."}}`
	var calls atomic.Int32
	server := bookStoryModelServer(t, &answer, &calls, "Write a short social post.")
	defer server.Close()
	t.Setenv("OPENCODE_URL", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	preview, changed, err := writeProjectBookStory(ctx, project, entry.ID, bookStoryOptions{Preview: true, Style: "custom", CustomPrompt: "Write a short social post."})
	if err != nil || changed || preview.Entries[0].Title != "A garden, with personality" {
		t.Fatal("preview failed", err)
	}
	_, saved := bookTestCall(t, project, "", nil)
	if saved.Entries[0].Title != entry.Title || saved.Entries[0].ContentVersion != entry.ContentVersion {
		t.Fatal("preview changed saved entry")
	}
	if _, _, err = writeProjectBookStory(ctx, project, entry.ID); err == nil {
		t.Fatal("automatic writing replaced owner edit")
	}
	bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": "New owner title", "body": entry.Body, "expectedContentVersion": entry.ContentVersion})
	response, _ := bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": preview.Entries[0].Title, "body": preview.Entries[0].Body, "expectedContentVersion": entry.ContentVersion})
	if response.Code != 409 {
		t.Fatal("stale preview overwrote newer edit", response.Code)
	}
}

func TestBookAutomaticGenerationUsesSavedWritingAndDrawingStyle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	preferences := bookWritingPreferences{Style: "epic"}
	if err := saveBookWritingPreferences(preferences); err != nil {
		t.Fatal(err)
	}
	project := bookTestProject(t)
	bookTestBuild(t, project)
	bookTestWrite(t, project, "web/page.tsx", []byte("export function Garden() { return <main><h1>Garden</h1><button>Plant</button></main> }"))
	_, imported := bookTestCall(t, project, "sync", nil)
	answer := `{"story":{"title":"A garden takes root","body":"The garden page now includes a Plant button. The saved work gives this project's next chapter a visible starting point."},"description":"Garden page.","annotations":[{"kind":"shape","shape":"rectangle","start":{"x":0.1,"y":0.1},"end":{"x":0.8,"y":0.8},"color":"#111111"},{"kind":"text","text":"Plant","x":0.2,"y":0.2,"width":0.3,"fontSize":0.03,"color":"#111111"}]}`
	var calls atomic.Int32
	server := bookStoryModelServer(t, &answer, &calls, bookDrawingDirection("epic"), "playful miniature adventure")
	defer server.Close()
	t.Setenv("OPENCODE_URL", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	book, changed, err := drawProjectBookVisual(ctx, project, imported.Entries[0].Source, "", "")
	if err != nil || !changed || book.Entries[0].Sketch.Visual == nil || book.Entries[0].Title != "A garden takes root" {
		t.Fatal("styled automatic generation failed", err)
	}
}

func TestCustomStyleAcceptsShortSocialPostsWithoutWeakeningDefaultParser(t *testing.T) {
	short := `{"story":{"title":"Dark mode","body":"Added dark mode. Your eyes are welcome."}}`
	if parseBookStoryForStyle(short, "custom") == nil {
		t.Fatal("short custom post rejected")
	}
	if parseBookStory(short) != nil {
		t.Fatal("default article validation weakened")
	}
	if parseBookStoryForStyle(`{"story":{"title":"Empty","body":" "}}`, "custom") != nil {
		t.Fatal("empty custom story accepted")
	}
}

func TestBookRetryCompletesMissingStoryWithoutReplacingDrawing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := bookTestProject(t)
	bookTestBuild(t, project)
	bookTestWrite(t, project, "web/page.tsx", []byte("export function Garden() { return <main><h1>Garden</h1><button>Plant</button></main> }"))
	_, imported := bookTestCall(t, project, "sync", nil)
	entry := imported.Entries[0]
	answer := `{"description":"Garden page.","annotations":[{"kind":"shape","shape":"rectangle","start":{"x":0.1,"y":0.1},"end":{"x":0.8,"y":0.8},"color":"#111111"},{"kind":"text","text":"Plant","x":0.2,"y":0.2,"width":0.3,"fontSize":0.03,"color":"#111111"}]}`
	var calls atomic.Int32
	server := bookStoryModelServer(t, &answer, &calls)
	defer server.Close()
	t.Setenv("OPENCODE_URL", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	book, changed, err := drawProjectBookVisual(ctx, project, entry.Source, "", "", "both")
	if err == nil || !changed || book.Entries[0].Sketch.Visual == nil || !book.Entries[0].CanWriteStory || calls.Load() != 2 {
		t.Fatal("missing story was not reported while keeping drawing", err)
	}
	originalDrawing, _ := json.Marshal(book.Entries[0].Sketch.Visual)
	answer = `{"story":{"title":"The garden takes shape","body":"The saved garden page includes a Plant button. The recorded change gives visitors a visible starting point for the garden experience."}}`
	book, changed, err = drawProjectBookVisual(ctx, project, entry.Source, "", "", "both")
	finalDrawing, _ := json.Marshal(book.Entries[0].Sketch.Visual)
	if err != nil || !changed || book.Entries[0].CanWriteStory || !bytes.Equal(originalDrawing, finalDrawing) {
		t.Fatal("retry did not fill only missing story", err)
	}
	_, changed, err = drawProjectBookVisual(ctx, project, entry.Source, "", "", "both")
	if err != nil || changed || calls.Load() != 3 {
		t.Fatal("complete entry was regenerated", err)
	}
}

func TestBookRetryHandlerReturnsPartialSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := bookTestProject(t)
	bookTestBuild(t, project)
	bookTestWrite(t, project, "web/page.tsx", []byte("export function Garden() { return <main><h1>Garden</h1><button>Plant</button></main> }"))
	_, imported := bookTestCall(t, project, "sync", nil)
	answer := `{"story":{"title":"A garden takes shape","body":"The saved garden page includes a Plant button. The recorded change gives visitors a visible starting point for the garden experience."},"description":"Invalid drawing","annotations":[]}`
	var calls atomic.Int32
	server := bookStoryModelServer(t, &answer, &calls)
	defer server.Close()
	t.Setenv("OPENCODE_URL", server.URL)
	data, _ := json.Marshal(map[string]string{"projectPath": project, "entryId": imported.Entries[0].ID})
	response := httptest.NewRecorder()
	projectBookVisualHandler(response, httptest.NewRequest("POST", "/project-book/visual", bytes.NewReader(data)))
	var book projectBook
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &book) != nil || len(book.Warnings) == 0 || book.Entries[0].Title != "A garden takes shape" || book.Entries[0].Sketch.Visual != nil {
		t.Fatal("partial success was hidden", response.Code, response.Body.String())
	}
}

func TestBookRetrySelectedParts(t *testing.T) {
	for _, parts := range []string{"drawing", "text"} {
		t.Run(parts, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			project := bookTestProject(t)
			bookTestBuild(t, project)
			bookTestWrite(t, project, "web/page.tsx", []byte("export function Garden() { return <main><h1>Garden</h1><button>Plant</button></main> }"))
			_, imported := bookTestCall(t, project, "sync", nil)
			entry := imported.Entries[0]
			answer := `{"story":{"title":"The garden takes shape","body":"The saved garden page includes a Plant button. The recorded change gives visitors a visible starting point for the garden experience."},"description":"Garden page.","annotations":[{"kind":"shape","shape":"rectangle","start":{"x":0.1,"y":0.1},"end":{"x":0.8,"y":0.8},"color":"#111111"},{"kind":"text","text":"Plant","x":0.2,"y":0.2,"width":0.3,"fontSize":0.03,"color":"#111111"}]}`
			var calls atomic.Int32
			expected := "Generate only the drawing"
			if parts == "text" {
				expected = bookStoryPrompt
			}
			server := bookStoryModelServer(t, &answer, &calls, expected)
			defer server.Close()
			t.Setenv("OPENCODE_URL", server.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			book, changed, err := drawProjectBookVisual(ctx, project, entry.Source, "", "", parts)
			if err != nil || !changed {
				t.Fatal("selected retry failed", err)
			}
			got := book.Entries[0]
			if parts == "drawing" && (got.Title != entry.Title || got.Body != entry.Body || got.Sketch.Visual == nil) {
				t.Fatal("drawing-only retry changed story or lost drawing")
			}
			if parts == "text" && (got.Title != "The garden takes shape" || got.Sketch.Visual != nil) {
				t.Fatal("text-only retry changed drawing or lost story")
			}
		})
	}
}
