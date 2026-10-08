package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"
)

func companionCommandPrefix(raw json.RawMessage) []string {
	var prefix []string
	if json.Unmarshal(raw, &prefix) != nil || len(prefix) == 0 || len(prefix) > 32 {
		return nil
	}
	total := 0
	for _, part := range prefix {
		if part == "" || len(part) > 512 || strings.IndexFunc(part, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return nil
		}
		total += len(part)
	}
	if total > 2000 {
		return nil
	}
	return prefix
}

func companionPublicText(value string, limit int) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	value = sanitizeProviderError(errors.New(value))
	value = previewSecret.ReplaceAllString(value, "$1=[redacted]")
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return strings.ToValidUTF8(value, "")
}

// Stream fragments must retain whitespace until the next fragment arrives.
// Apply the same secret redaction without error-summary truncation.
func companionPublicRunText(value string, limit int) string {
	value = sensitiveValueRegex.ReplaceAllString(value, "$1:[redacted]")
	value = previewSecret.ReplaceAllString(value, "$1=[redacted]")
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return strings.ToValidUTF8(value, "")
}

func companionPublicPending(data json.RawMessage, kind string) map[string]any {
	if len(data) == 0 || len(data) > 64<<10 {
		return nil
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	id := companionPendingID(data)
	if id == "" || len(id) > 160 {
		return nil
	}
	result := map[string]any{"id": id}
	textField := func(source map[string]json.RawMessage, target map[string]any, key string, limit int) {
		var value string
		if json.Unmarshal(source[key], &value) == nil && value != "" {
			target[key] = companionPublicText(value, limit)
		}
	}
	if kind == "permission" {
		for _, key := range []string{"title", "type", "message", "pattern"} {
			textField(raw, result, key, 2000)
		}
		if advertised, exists := raw["availableResponses"]; exists {
			var session string
			_ = json.Unmarshal(raw["sessionID"], &session)
			var tool string
			_ = json.Unmarshal(raw["buildApprovalTool"], &tool)
			prefix := companionCommandPrefix(raw["execPolicyAmendment"])
			var responses []string
			_ = json.Unmarshal(advertised, &responses)
			clean := []string{}
			seen := map[string]bool{}
			onceOffered := false
			for _, response := range responses {
				onceOffered = onceOffered || response == "once"
			}
			for _, response := range responses {
				if response == "all" && !onceOffered {
					continue
				}
				if response == "build" && (!strings.HasPrefix(session, acpSessionPrefix) || !acpToolIdentifier.MatchString(tool)) {
					continue
				}
				if response == "execpolicy" && (!strings.HasPrefix(session, codexSessionPrefix) || prefix == nil) {
					continue
				}
				if response == "always" && (strings.HasPrefix(session, codexSessionPrefix) || strings.HasPrefix(session, acpSessionPrefix) || strings.HasPrefix(id, "codex-") || strings.HasPrefix(id, "acp-")) {
					continue
				}
				switch response {
				case "once", "always", "session", "build", "all", "execpolicy", "reject", "cancel":
					if !seen[response] {
						clean = append(clean, response)
						seen[response] = true
					}
				}
			}
			result["availableResponses"] = clean
			for _, response := range clean {
				if response == "build" {
					result["buildApprovalTool"] = tool
				}
				if response == "execpolicy" {
					for index := range prefix {
						prefix[index] = companionPublicRunText(prefix[index], 512)
					}
					result["execPolicyAmendment"] = prefix
				}
			}
		}
		return result
	}
	textField(raw, result, "prompt", 4000)
	var choices []string
	if json.Unmarshal(raw["choices"], &choices) == nil {
		clean := []string{}
		for _, choice := range choices {
			clean = append(clean, companionPublicText(choice, 512))
			if len(clean) == 32 {
				break
			}
		}
		result["choices"] = clean
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw["questions"], &items) == nil {
		questions := []map[string]any{}
		for index, item := range items {
			if index >= 16 {
				break
			}
			question := map[string]any{}
			for _, key := range []string{"id", "header", "prompt", "question", "inputType"} {
				textField(item, question, key, 4000)
			}
			for _, key := range []string{"multiple", "custom"} {
				var flag bool
				if json.Unmarshal(item[key], &flag) == nil {
					question[key] = flag
				}
			}
			var options []map[string]json.RawMessage
			if json.Unmarshal(item["options"], &options) == nil {
				clean := []map[string]any{}
				for index, option := range options {
					if index >= 32 {
						break
					}
					choice := map[string]any{}
					textField(option, choice, "id", 160)
					textField(option, choice, "label", 512)
					textField(option, choice, "description", 2000)
					clean = append(clean, choice)
				}
				question["options"] = clean
			}
			questions = append(questions, question)
		}
		result["questions"] = questions
	}
	return result
}

func companionValidateQuestionResponse(pending json.RawMessage, request companionResponseRequest) error {
	if len(request.Answers) > 16 || len(request.AnswerByQuestionID) > 16 || len(request.Answer) > 8000 {
		return errors.New("Keep this answer within the displayed question options.")
	}
	valid := strings.TrimSpace(request.Answer) != ""
	validate := func(answers []string) bool {
		if len(answers) == 0 || len(answers) > 32 {
			return false
		}
		for _, answer := range answers {
			if strings.TrimSpace(answer) == "" || len(answer) > 4000 {
				return false
			}
		}
		return true
	}
	for _, answers := range request.Answers {
		if !validate(answers) {
			return errors.New("Answer each displayed question before submitting.")
		}
		valid = true
	}
	for id, answers := range request.AnswerByQuestionID {
		if id == "" || len(id) > 160 || !validate(answers) {
			return errors.New("Answer each displayed question before submitting.")
		}
		valid = true
	}
	if !valid {
		return errors.New("Answer the waiting question before submitting.")
	}
	var value struct {
		Questions []struct {
			ID string `json:"id"`
		} `json:"questions"`
	}
	if json.Unmarshal(pending, &value) == nil && len(value.Questions) > 0 {
		if len(request.Answers) > 0 && len(request.Answers) != len(value.Questions) {
			return errors.New("Answer each displayed question before submitting.")
		}
		if len(request.AnswerByQuestionID) > 0 {
			allowed := map[string]bool{}
			for _, question := range value.Questions {
				allowed[question.ID] = true
			}
			for id := range request.AnswerByQuestionID {
				if !allowed[id] {
					return errors.New("This answer no longer matches the displayed question.")
				}
			}
		}
	}
	return nil
}

func (m *companionManager) cancelJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		JobID string `json:"jobId"`
	}
	if !companionDecode(w, r, &request, 1024) {
		return
	}
	job := m.run(request.JobID)
	if job == nil {
		http.NotFound(w, r)
		return
	}
	job.cancelJob(m.now())
	writeJSON(w, m.status())
}
