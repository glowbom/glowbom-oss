package main

import (
	"strings"
	"testing"
)

const bookParserTestResponse = `{"story":{"title":"A clearer garden","body":"The saved garden screen places a Plant button beside its title, making the main action easier to find."},"description":"A garden screen with a Plant button.","annotations":[{"kind":"shape","shape":"rectangle","start":{"x":0.1,"y":0.1},"end":{"x":0.8,"y":0.8},"color":"#111111"},{"kind":"text","text":"Plant","x":0.2,"y":0.2,"width":0.3,"fontSize":0.03,"color":"#111111"}]}`

func TestBookModelJSONAcceptsCompleteUnambiguousWrappers(t *testing.T) {
	for name, response := range map[string]string{
		"raw":                   bookParserTestResponse,
		"whitespace":            " \n" + bookParserTestResponse + "\n ",
		"plain fence":           "```\n" + bookParserTestResponse + "\n```",
		"JSON fence":            "```JSON\n" + bookParserTestResponse + "\n```",
		"mixed case fence":      "```JsOn\r\n" + bookParserTestResponse + "\r\n```",
		"introduction":          "Here is the completed entry:\n" + bookParserTestResponse,
		"introduced JSON fence": "Here is the completed entry:\n```json\n" + bookParserTestResponse + "\n```",
	} {
		t.Run(name, func(t *testing.T) {
			if got := bookModelJSON(response); got != bookParserTestResponse {
				t.Fatal("did not extract the complete object")
			}
			if parseBookStory(response) == nil {
				t.Fatal("readable story was rejected")
			}
			if _, err := parseBookVisual(response, "provider/model"); err != nil {
				t.Fatal("valid drawing was rejected", err)
			}
		})
	}
}

func TestBookModelJSONRejectsAmbiguousIncompleteAndOversizedResponses(t *testing.T) {
	for name, response := range map[string]string{
		"second object":          bookParserTestResponse + `{"extra":true}`,
		"introduced second":      "Result:\n" + bookParserTestResponse + `{"extra":true}`,
		"truncated second":       "Result:\n" + bookParserTestResponse + `{"extra":`,
		"trailing prose":         "Result:\n" + bookParserTestResponse + "\nAnother response follows.",
		"truncated object":       "Result:\n" + strings.TrimSuffix(bookParserTestResponse, "}"),
		"missing closing fence":  "```json\n" + bookParserTestResponse,
		"missing opening fence":  bookParserTestResponse + "\n```",
		"unsupported fence":      "```javascript\n" + bookParserTestResponse + "\n```",
		"text after fence":       "```json\n" + bookParserTestResponse + "\n```\nMore text",
		"second fenced object":   "```json\n" + bookParserTestResponse + "\n```\n```json\n{}\n```",
		"prior incomplete JSON":  "An incomplete result: {\n```json\n" + bookParserTestResponse + "\n```",
		"prior array":            "[]\n" + bookParserTestResponse,
		"oversized introduction": strings.Repeat("x", 64<<10) + "\n" + bookParserTestResponse,
		"oversized whitespace":   strings.Repeat(" ", 64<<10) + bookParserTestResponse,
	} {
		t.Run(name, func(t *testing.T) {
			if parseBookStory(response) != nil {
				t.Fatal("invalid story response was accepted")
			}
			if _, err := parseBookVisual(response, "provider/model"); err == nil {
				t.Fatal("invalid drawing response was accepted")
			}
		})
	}
}

func TestBookVisualPreservesDrawingWhenStoryHasInvalidShape(t *testing.T) {
	for _, replacement := range []string{`"not a story object"`, `{"title":42,"body":[]}`, `null`} {
		start := strings.Index(bookParserTestResponse, `,"description"`)
		response := `{"story":` + replacement + bookParserTestResponse[start:]
		if parseBookStory(response) != nil {
			t.Fatal("invalid story was accepted")
		}
		if _, err := parseBookVisual(response, "provider/model"); err != nil {
			t.Fatal("malformed story discarded a valid drawing", err)
		}
		outsideCanvas := strings.Replace(response, `"x":0.1`, `"x":1.5`, 1)
		if _, err := parseBookVisual(outsideCanvas, "provider/model"); err == nil {
			t.Fatal("drawing coordinates were not validated independently")
		}
	}
}
