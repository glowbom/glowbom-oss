package main

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Leave time for a slower model response and one format repair.
const bookGenerationTimeout = 3 * time.Minute

// Styles affect composition and prose, not the storage format. Rounded objects,
// banners, and other decorations must be drawn with the supported primitives.
const bookDrawingFormat = `Drawing format is mandatory. Use ONLY these mark types:
{"kind":"shape","shape":"rectangle","start":{"x":0.1,"y":0.1},"end":{"x":0.9,"y":0.8},"color":"#111111","width":0.002}
{"kind":"shape","shape":"arrow","start":{"x":0.2,"y":0.4},"end":{"x":0.4,"y":0.4},"color":"#111111","width":0.002}
{"kind":"stroke","points":[{"x":0.1,"y":0.2},{"x":0.15,"y":0.15},{"x":0.2,"y":0.2}],"color":"#111111","width":0.002,"erase":false}
{"kind":"text","text":"Short label","x":0.2,"y":0.2,"width":0.4,"fontSize":0.025,"color":"#111111"}
Shape can ONLY be rectangle or arrow. For curves, circles, rounded corners, arches, banners, or bursts, use stroke points. Never emit circle, ellipse, line, path, SVG, or extra mark properties. All coordinates, widths, and fontSize values are normalized between 0 and 1, NOT pixels. Every mark needs color. Text marks need x, y, width, and fontSize. Include at least two marks, one being a shape or stroke. Keep description below 600 characters. Return a single JSON object. Do not describe drawing commands in prose.`

func bookGenerationPrompt(preferences bookWritingPreferences, story, drawing bool) string {
	if !drawing {
		return bookWritingPrompt(bookStoryPrompt, preferences)
	}
	prompt := bookStyledVisualPrompt(preferences) + "\n" + bookDrawingFormat
	if !story {
		prompt += "\nGenerate only the drawing. Set story to null; no story text is needed."
	}
	return prompt
}

type bookModelCompletion func(context.Context, string) (string, error)

// One repair request corrects invalid output with the same evidence and model.
// A valid part from either response survives failure of the other part.
func generateBookContent(ctx context.Context, complete bookModelCompletion, preferences bookWritingPreferences, model string, wantStory, wantDrawing bool) (*bookStory, *bookVisual, error) {
	var story *bookStory
	var visual *bookVisual
	var visualErr error
	prompt := bookGenerationPrompt(preferences, wantStory, wantDrawing)
	for attempt := 0; attempt < 2; attempt++ {
		text, err := complete(ctx, prompt)
		if err != nil {
			return story, visual, err
		}
		if wantStory && story == nil {
			story = parseBookStoryForStyle(text, preferences.Style)
		}
		if wantDrawing && visual == nil {
			visual, visualErr = parseBookVisual(text, model)
		}
		missingStory, missingDrawing := wantStory && story == nil, wantDrawing && visual == nil
		if !missingStory && !missingDrawing {
			return story, visual, nil
		}
		if attempt == 0 {
			prompt = bookGenerationPrompt(preferences, missingStory, missingDrawing)
			prompt += "\nThe previous response did not match the required JSON format. Correct the format using the same saved evidence. Return only the required JSON object, without commentary or Markdown fences."
			if missingStory {
				prompt += " The story must be an object with a nonempty title of at most 120 characters and a body string of at most 6000 characters. Do not put the story in description or return it as a string."
			}
			if missingDrawing && visualErr != nil {
				prompt += " Drawing validation: " + visualErr.Error()
			}
		}
	}
	missing := []string{}
	if wantStory && story == nil {
		missing = append(missing, "story")
	}
	if wantDrawing && visual == nil {
		missing = append(missing, "drawing")
	}
	return story, visual, errors.New("The model could not format the " + strings.Join(missing, " and ") + " after an automatic repair. Any completed part was saved. Try generating the missing part on its own.")
}
