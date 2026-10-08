package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func bookGenerationTestResponse(t *testing.T, change func(map[string]any)) string {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal([]byte(bookParserTestResponse), &response); err != nil {
		t.Fatal(err)
	}
	change(response)
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBookGenerationRepairsMalformedResponseWithOriginalStyle(t *testing.T) {
	calls := 0
	story, visual, err := generateBookContent(context.Background(), func(_ context.Context, prompt string) (string, error) {
		calls++
		if !strings.Contains(prompt, "playful miniature adventure") || !strings.Contains(prompt, bookDrawingDirection("epic")) || !strings.Contains(prompt, bookDrawingFormat) {
			t.Fatal("generation lost its writing style, drawing style, or required format")
		}
		if calls == 1 {
			return `{"story":`, nil
		}
		if !strings.Contains(prompt, "previous response did not match") {
			t.Fatal("invalid response was not followed by a format repair")
		}
		return bookParserTestResponse, nil
	}, bookWritingPreferences{Style: "epic"}, "provider/model", true, true)
	if err != nil || calls != 2 || story == nil || story.Title != "A clearer garden" || visual == nil || visual.Model != "provider/model" {
		t.Fatalf("format repair failed: calls=%d story=%+v visual=%+v err=%v", calls, story, visual, err)
	}
}

func TestBookGenerationRepairsUnsupportedMarksWithoutReplacingStory(t *testing.T) {
	first := strings.Replace(bookParserTestResponse, `"shape":"rectangle"`, `"shape":"circle"`, 1)
	second := strings.Replace(bookParserTestResponse, "A clearer garden", "An unwanted rewrite", 1)
	calls := 0
	story, visual, err := generateBookContent(context.Background(), func(_ context.Context, prompt string) (string, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		if !strings.Contains(prompt, "Generate only the drawing") || !strings.Contains(prompt, "Shape can ONLY be rectangle or arrow") || !strings.Contains(prompt, "Drawing validation:") {
			t.Fatal("unsupported mark was not repaired with a drawing-only format request")
		}
		return second, nil
	}, bookWritingPreferences{Style: "announcement"}, "provider/model", true, true)
	if err != nil || calls != 2 || story == nil || story.Title != "A clearer garden" || visual == nil {
		t.Fatalf("repair lost or rewrote valid content: calls=%d story=%+v visual=%+v err=%v", calls, story, visual, err)
	}
	if err := validateSketchDocument(&visual.Document); err != nil {
		t.Fatal("repaired drawing still has invalid marks", err)
	}
}

func TestBookGenerationKeepsValidPartWhenRepairFails(t *testing.T) {
	providerErr := errors.New("provider connection failed")
	for _, keep := range []string{"story", "drawing"} {
		for _, failure := range []string{"invalid response", "provider failure"} {
			t.Run(keep+"/"+failure, func(t *testing.T) {
				first := bookGenerationTestResponse(t, func(response map[string]any) {
					if keep == "story" {
						response["annotations"] = []any{}
					} else {
						response["story"] = "invalid story object"
					}
				})
				calls := 0
				story, visual, err := generateBookContent(context.Background(), func(_ context.Context, prompt string) (string, error) {
					calls++
					if calls == 1 {
						return first, nil
					}
					if keep == "story" && !strings.Contains(prompt, "Generate only the drawing") {
						t.Fatal("repair unnecessarily requested the already valid story")
					}
					if keep == "drawing" && (!strings.Contains(prompt, bookStoryPrompt) || strings.Contains(prompt, bookDrawingFormat)) {
						t.Fatal("repair unnecessarily requested the already valid drawing")
					}
					if failure == "provider failure" {
						return "", providerErr
					}
					return "still not JSON", nil
				}, bookWritingPreferences{Style: "announcement"}, "provider/model", true, true)
				if err == nil || calls != 2 {
					t.Fatalf("repair was not limited to one attempt: calls=%d err=%v", calls, err)
				}
				if failure == "provider failure" && !errors.Is(err, providerErr) {
					t.Fatal("repair lost the provider error", err)
				}
				if keep == "story" && (story == nil || story.Title != "A clearer garden" || visual != nil) {
					t.Fatal("failed drawing repair lost the original story")
				}
				if keep == "drawing" && (visual == nil || visual.Description != "A garden screen with a Plant button." || story != nil) {
					t.Fatal("failed story repair lost the original drawing")
				}
			})
		}
	}
}

func TestBookGenerationRequestsOnlySelectedParts(t *testing.T) {
	for _, scope := range []string{"story", "drawing", "both"} {
		t.Run(scope, func(t *testing.T) {
			wantStory, wantDrawing := scope != "drawing", scope != "story"
			calls := 0
			story, visual, err := generateBookContent(context.Background(), func(_ context.Context, prompt string) (string, error) {
				calls++
				if wantDrawing != strings.Contains(prompt, bookDrawingFormat) {
					t.Fatal("drawing request did not match the selected scope")
				}
				if scope == "drawing" && !strings.Contains(prompt, "Generate only the drawing") {
					t.Fatal("drawing-only selection requested a story")
				}
				if scope == "story" && !strings.Contains(prompt, bookStoryPrompt) {
					t.Fatal("story-only selection used the wrong request")
				}
				return bookParserTestResponse, nil
			}, bookWritingPreferences{Style: "announcement"}, "provider/model", wantStory, wantDrawing)
			if err != nil || calls != 1 || (story != nil) != wantStory || (visual != nil) != wantDrawing {
				t.Fatalf("generation exceeded its selected scope: calls=%d story=%+v visual=%+v err=%v", calls, story, visual, err)
			}
		})
	}
}

func TestBookGenerationDoesNotRetryProviderFailuresAsFormatErrors(t *testing.T) {
	providerErr := errors.New("provider request failed")
	calls := 0
	story, visual, err := generateBookContent(context.Background(), func(context.Context, string) (string, error) {
		calls++
		return "", providerErr
	}, bookWritingPreferences{Style: "announcement"}, "provider/model", true, true)
	if calls != 1 || !errors.Is(err, providerErr) || story != nil || visual != nil {
		t.Fatalf("provider failure triggered an unrelated retry: calls=%d err=%v", calls, err)
	}
}

func TestBookComedyVoiceControlsStoryWithAndWithoutDrawing(t *testing.T) {
	for _, style := range []string{"unhinged", "standup", "superfunny"} {
		for _, drawing := range []bool{false, true} {
			preferences := bookWritingPreferences{Style: style, Profanity: true}
			calls := 0
			story, visual, err := generateBookContent(context.Background(), func(_ context.Context, prompt string) (string, error) {
				calls++
				for _, forbidden := range []string{"Write like a thoughtful product announcement", "Prefer a concrete headline such as", "allowed, but not required"} {
					if strings.Contains(prompt, forbidden) {
						t.Fatalf("%s drawing=%t still overrides the selected voice with %q", style, drawing, forbidden)
					}
				}
				for _, required := range []string{bookStoryInstructions, "at least two distinct", "callback", "not evidence about this result", "Never turn a request alone"} {
					if !strings.Contains(prompt, required) {
						t.Fatalf("%s drawing=%t lost %q", style, drawing, required)
					}
				}
				wantProfanity := style == "unhinged" || style == "standup"
				if strings.Contains(prompt, "Use occasional profanity") != wantProfanity || strings.Contains(prompt, "Do not use profanity.") == wantProfanity {
					t.Fatal("generation lost this comedy voice's swearing choice")
				}
				return bookParserTestResponse, nil
			}, preferences, "provider/model", true, drawing)
			if err != nil || calls != 1 || story == nil || (visual != nil) != drawing {
				t.Fatalf("comedy generation failed: style=%s drawing=%t err=%v", style, drawing, err)
			}
		}
	}
}

func TestBookComedyRepairKeepsVoiceAndSwearingChoice(t *testing.T) {
	for _, style := range []string{"unhinged", "standup", "superfunny"} {
		calls := 0
		story, visual, err := generateBookContent(context.Background(), func(_ context.Context, prompt string) (string, error) {
			calls++
			wantProfanity := style == "unhinged" || style == "standup"
			if strings.Contains(prompt, "Use occasional profanity") != wantProfanity || strings.Contains(prompt, "Do not use profanity.") == wantProfanity || !strings.Contains(prompt, "at least two distinct") || !strings.Contains(prompt, "not evidence about this result") {
				t.Fatal("repair lost comedy preferences")
			}
			if calls == 1 {
				return bookGenerationTestResponse(t, func(response map[string]any) { response["story"] = nil }), nil
			}
			if strings.Contains(prompt, bookDrawingFormat) || !strings.Contains(prompt, "previous response did not match") {
				t.Fatal("repair did not request only the missing story")
			}
			return bookParserTestResponse, nil
		}, bookWritingPreferences{Style: style, Profanity: true}, "provider/model", true, true)
		if err != nil || calls != 2 || story == nil || visual == nil {
			t.Fatalf("comedy repair failed: style=%s err=%v", style, err)
		}
	}
}
