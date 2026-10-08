package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	buildStatusPrefix   = "GLOWBOM_STATUS:"
	buildResultPrefix   = "GLOWBOM_RESULT:"
	maxBuildStatusRunes = 160
)

type buildStatus struct {
	Text   string `json:"text"`
	Source string `json:"source"`
	At     string `json:"at"`
}

func sendBuildStatus(w http.ResponseWriter, flusher http.Flusher, text, source string) {
	text = cleanBuildStatus(text)
	if text == "" {
		return
	}
	sendSSEData(w, flusher, map[string]interface{}{
		"status": newBuildStatus(text, source),
	})
}

func newBuildStatus(text, source string) buildStatus {
	return buildStatus{Text: cleanBuildStatus(text), Source: source, At: time.Now().UTC().Format(time.RFC3339)}
}

func cleanBuildStatus(text string) string {
	// A model may repeat the marker within one line. Show only its latest
	// nonempty update, including when the line ends with another marker.
	if strings.Contains(text, buildStatusPrefix) {
		parts := strings.Split(text, buildStatusPrefix)
		text = ""
		for index := len(parts) - 1; index >= 0; index-- {
			if candidate := strings.TrimSpace(parts[index]); candidate != "" {
				text = candidate
				break
			}
		}
	}
	var out strings.Builder
	space := false
	for _, r := range strings.TrimSpace(text) {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		space = false
		out.WriteRune(r)
		if utf8.RuneCountInString(out.String()) >= maxBuildStatusRunes {
			break
		}
	}
	return strings.TrimSpace(out.String())
}

// Agent updates only become card statuses when the agent intentionally marks a line.
// Unmarked output remains available in the detailed build log.
func buildAgentStatusLines(content string, includeTrailing bool) []string {
	lines := strings.Split(content, "\n")
	if !includeTrailing && !strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	statuses := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, buildStatusPrefix) {
			continue
		}
		status := cleanBuildStatus(strings.TrimSpace(strings.TrimPrefix(line, buildStatusPrefix)))
		if status != "" {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

func buildAgentResultLine(content string) string {
	lines := strings.Split(content, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if !strings.HasPrefix(line, buildResultPrefix) {
			continue
		}
		result := strings.Join(strings.Fields(strings.TrimPrefix(line, buildResultPrefix)), " ")
		if result != "" && utf8.RuneCountInString(result) <= 800 {
			return result
		}
	}
	return ""
}

func buildToolStatus(toolName string, input map[string]interface{}) string {
	file := buildStatusFile(input)
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "read":
		if file != "" {
			return fmt.Sprintf("Exploring the project: reading %s to understand its existing behavior before making changes.", file)
		}
		return "Exploring the project: reading existing files to understand where this change belongs."
	case "glob", "list":
		return "Exploring the project: locating the files that define this feature and its current behavior."
	case "grep", "search":
		return "Exploring the project: tracing existing references to find where the requested change belongs."
	case "edit", "write", "apply_patch", "patch", "multiedit":
		if file != "" {
			return fmt.Sprintf("Making the changes: updating %s with the code needed for this build.", file)
		}
		return "Making the changes: applying the requested edits to the relevant project files."
	case "bash", "shell", "terminal", "exec":
		switch buildStatusCommandKind(input) {
		case "test":
			return "Checking the project: running tests to catch problems introduced by these changes."
		case "build":
			return "Checking the project: building the app to catch compile and packaging errors."
		case "typecheck":
			return "Checking the project: running type checks to find errors before finishing."
		case "lint":
			return "Checking the project: running lint checks to catch code-quality issues before finishing."
		case "inspect":
			return "Exploring the project: reading its files and references to understand the existing implementation."
		default:
			return "Working in the project: running a command; its details are available in Build."
		}
	default:
		return "Working on the project: using a tool for this step; details are available in Build."
	}
}

func buildStatusFile(input map[string]interface{}) string {
	path := getMapStringCaseInsensitive(input, "Path", "path", "File", "file", "FilePath", "filePath", "file_path", "Filename", "filename")
	name := filepath.Base(strings.ReplaceAll(path, "\\", "/"))
	if name == "." || len(name) > 64 || strings.HasPrefix(name, ".") {
		return ""
	}
	lower := strings.ToLower(name)
	for _, sensitive := range []string{"secret", "token", "password", "credential", "private", "key", "env"} {
		if strings.Contains(lower, sensitive) {
			return ""
		}
	}
	for _, r := range name {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-') {
			return ""
		}
	}
	return name
}

func buildStatusCommandKind(input map[string]interface{}) string {
	command := strings.ToLower(strings.TrimSpace(getMapStringCaseInsensitive(input, "Command", "command", "Cmd", "cmd")))
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	first := filepath.Base(fields[0])
	second := ""
	if len(fields) > 1 {
		second = fields[1]
	}
	switch first {
	case "rg", "grep", "find", "ls", "cat", "sed", "head", "tail":
		return "inspect"
	case "go":
		switch second {
		case "test":
			return "test"
		case "build":
			return "build"
		case "vet":
			return "lint"
		}
	case "xcodebuild":
		return "build"
	case "npm", "pnpm", "yarn", "bun", "flutter":
		for _, field := range fields[1:] {
			switch field {
			case "test":
				return "test"
			case "build":
				return "build"
			case "typecheck", "analyze":
				return "typecheck"
			case "lint":
				return "lint"
			}
			if field == "&&" || field == ";" || field == "|" {
				break
			}
		}
	}
	return ""
}

func buildRunUpdatesPrompt(projectPath string) string {
	initialChatEntries := 0
	baselineKnown := true
	if saved, err := readChatProjectFile(projectPath, ".glowbom/chat.json", maxChatHistoryBytes); err != nil {
		baselineKnown = false
	} else if saved != "" {
		var messages []chatMessage
		if json.Unmarshal([]byte(saved), &messages) == nil {
			initialChatEntries = len(messages)
		} else {
			baselineKnown = false
		}
	}
	chatBaseline := "The saved chat baseline could not be read at build start. Do not assume its existing entries are new."
	if baselineKnown {
		chatBaseline = fmt.Sprintf("At build start it had %d array entries. Consider user messages added after those entries as possible context or course corrections.", initialChatEntries)
	}
	return fmt.Sprintf(`

## Updates and messages during this build
After each meaningful step, write one plain-language line beginning exactly with %s followed by what you are doing or have finished. Aim for about 12 to 18 words. Name the actual screen, file, test, or visible change and why it matters when known. For example: %s Updating the sign-in screen so people can see password errors before submitting. Avoid vague updates such as "exploring the project" or "checking the project" alone. Do not include secrets, raw commands, or speculative completion claims. Continue your normal detailed work as needed.

Reread .glowbom/chat.json at the start and between major steps. %s New messages may arrive while you work. Use your judgment about whether a new user message matters to this build, and how to respond. Do not treat earlier messages, assistant replies, or a repeat of the original build request as new corrections. If the chat history was replaced or reordered, avoid assuming every entry is new. Mention a relevant new chat message in a %s line when you act on it or choose to defer it.

When you finish, end with one line beginning exactly with %s followed by a short plain-language result for the person. Say what changed, what they can now do, and whether you verified it. Keep technical details in the earlier work report. Do not use this marker before the final result.`, buildStatusPrefix, buildStatusPrefix, chatBaseline, buildStatusPrefix, buildResultPrefix)
}
