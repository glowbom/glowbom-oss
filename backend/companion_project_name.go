package main

import (
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Naming runs on Desktop's existing chat workspace without creating a project.
func (s *companionSession) suggestDesktopProjectName(w http.ResponseWriter, r *http.Request) {
	var request projectNameRequest
	if !companionDecode(w, r, &request, 64<<10) {
		return
	}
	if !request.valid() {
		companionPrototypeFailure(w, http.StatusBadRequest, "invalid_input", "Use a shorter idea and a valid model selection.")
		return
	}
	ctx, stop := s.chatContext(r.Context())
	defer stop()
	s.api.ServeHTTP(w, s.internalRequest(ctx, http.MethodPost, "/chat/project-name", request))
}

var projectNameTags = regexp.MustCompile(`<[^>]*>`)
var projectNameReplyPrefix = regexp.MustCompile(`(?i)^(?:project name|name):\s*`)

func cleanProjectSuggestedName(value string) string {
	value = projectNameTags.ReplaceAllString(value, "")
	value = strings.Map(func(character rune) rune {
		if character < 32 || character == 127 || strings.ContainsRune("/\\:", character) {
			return ' '
		}
		return character
	}, value)
	value = strings.TrimSpace(strings.TrimLeft(strings.Join(strings.Fields(value), " "), "."))
	for len(value) > 90 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return strings.TrimSpace(value)
}

func projectNameSuggestion(reply string) string {
	value := projectNameReplyPrefix.ReplaceAllString(strings.TrimSpace(reply), "")
	value = strings.Trim(value, "\"'`“”‘’")
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") || len(strings.Fields(value)) > 8 {
		return ""
	}
	return cleanProjectSuggestedName(value)
}

// The reverse domain rule matches Desktop Settings' suggestBundleID.
func suggestProjectBundleID(name string) string {
	slug := strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			return character
		}
		return '.'
	}, strings.ToLower(name))
	parts := strings.FieldsFunc(slug, func(character rune) bool { return character == '.' })
	for index, part := range parts {
		if part[0] >= '0' && part[0] <= '9' {
			parts[index] = "app" + part
		}
	}
	slug = strings.Join(parts, ".")
	if slug == "" {
		slug = "myapp"
	}
	return "app.glowbom." + slug
}
