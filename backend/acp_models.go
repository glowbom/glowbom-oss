package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const acpMaxModelOptions = 1024

type acpModelOption struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type acpModelSelection struct {
	configID string
	options  []acpModelOption
}

type acpModelConfig struct {
	ID           string          `json:"id"`
	Category     string          `json:"category"`
	Type         string          `json:"type"`
	CurrentValue json.RawMessage `json:"currentValue"`
	Options      json.RawMessage `json:"options"`
}

func validACPModelID(value string) bool {
	return acpModelText(value, 256) && value != "" && strings.TrimSpace(value) == value
}

func acpModelText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0 && !strings.ContainsAny(value, "\u2028\u2029")
}

func acpModelConfigs(raw json.RawMessage) []acpModelConfig {
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || len(values) > 128 {
		return nil
	}
	configs := make([]acpModelConfig, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		var config acpModelConfig
		if json.Unmarshal(value, &config) != nil || !validACPModelID(config.ID) {
			continue
		}
		if seen[config.ID] {
			return nil
		}
		seen[config.ID] = true
		configs = append(configs, config)
	}
	return configs
}

func acpModelOptions(raw json.RawMessage, legacy bool) []acpModelOption {
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || len(values) > acpMaxModelOptions {
		return nil
	}
	var options []acpModelOption
	seen := make(map[string]bool)
	appendOption := func(value json.RawMessage) bool {
		var option struct {
			Value       string `json:"value"`
			ModelID     string `json:"modelId"`
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if json.Unmarshal(value, &option) != nil {
			return false
		}
		id := option.Value
		if legacy {
			id = option.ModelID
		}
		if !validACPModelID(id) || seen[id] || len(options) >= acpMaxModelOptions {
			return false
		}
		seen[id] = true
		if !acpModelText(option.Name, 256) || strings.TrimSpace(option.Name) == "" {
			option.Name = id
		}
		if !acpModelText(option.Description, 1024) {
			option.Description = ""
		}
		options = append(options, acpModelOption{ID: id, Name: option.Name, Description: option.Description})
		return true
	}
	grouped := false
	groups := make(map[string]bool)
	for index, value := range values {
		var group struct {
			Group   *string         `json:"group"`
			Options json.RawMessage `json:"options"`
		}
		if json.Unmarshal(value, &group) != nil {
			return nil
		}
		isGroup := group.Group != nil
		if index == 0 {
			grouped = isGroup
		}
		if grouped != isGroup || (legacy && isGroup) {
			return nil
		}
		if !isGroup {
			if !appendOption(value) {
				return nil
			}
			continue
		}
		if !validACPModelID(*group.Group) || groups[*group.Group] {
			return nil
		}
		groups[*group.Group] = true
		var members []json.RawMessage
		if json.Unmarshal(group.Options, &members) != nil || len(members) > acpMaxModelOptions {
			return nil
		}
		for _, member := range members {
			if !appendOption(member) {
				return nil
			}
		}
	}
	return options
}

func acpDiscoverSessionModels(response acpSessionResponse) acpModelSelection {
	configs := acpModelConfigs(response.ConfigOptions)
	// Prefer an explicit model selector over a related selector such as provider.
	for _, explicit := range []bool{true, false} {
		for _, config := range configs {
			if config.Type != "select" || (explicit && config.ID != "model") || (!explicit && (config.Category != "model" || config.ID == "provider" || config.ID == "model")) {
				continue
			}
			if options := acpModelOptions(config.Options, false); len(options) > 0 {
				return acpModelSelection{configID: config.ID, options: options}
			}
		}
	}
	var legacy struct {
		AvailableModels json.RawMessage `json:"availableModels"`
	}
	if json.Unmarshal(response.Models, &legacy) == nil {
		return acpModelSelection{options: acpModelOptions(legacy.AvailableModels, true)}
	}
	return acpModelSelection{}
}

func (p *acpProcess) selectModel(ctx context.Context, model string) error {
	if model == "" {
		return nil
	}
	if !validACPModelID(model) {
		return errors.New("Choose a valid ACP model in Settings.")
	}
	available := false
	for _, option := range p.modelSelection.options {
		if option.ID == model {
			available = true
			break
		}
	}
	if !available {
		return errors.New("The saved ACP model is unavailable. Test the connection in Settings and choose an available model or Use agent default.")
	}
	setupContext, cancel := context.WithTimeout(ctx, acpSetupTimeout)
	defer cancel()
	method := "session/set_model"
	params := map[string]any{"sessionId": p.session, "modelId": model}
	if p.modelSelection.configID != "" {
		method = "session/set_config_option"
		params = map[string]any{"sessionId": p.session, "configId": p.modelSelection.configID, "value": model}
	}
	var response acpSessionResponse
	if err := p.call(setupContext, method, params, &response); err != nil {
		return acpSetupError(ctx, err)
	}
	if p.modelSelection.configID != "" {
		confirmed := false
		for _, config := range acpModelConfigs(response.ConfigOptions) {
			if config.ID == p.modelSelection.configID {
				var current string
				confirmed = json.Unmarshal(config.CurrentValue, &current) == nil && current == model
				break
			}
		}
		if !confirmed {
			return errors.New("The ACP agent did not confirm the selected model. No build was started. Test the connection in Settings and try again.")
		}
	} else if reported := acpReportedSessionModel(response); reported != "" && reported != model {
		return errors.New("The ACP agent reported a different model from your selection. No build was started. Check the connection in Settings.")
	}
	p.model = model
	return nil
}
