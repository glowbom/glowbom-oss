package main

import "strings"

func isBuildOnlyCLIModel(model string) bool {
	return strings.HasPrefix(model, "cursor/") || strings.HasPrefix(model, "claude-code/") || strings.HasPrefix(model, "acp/")
}

func companionBuildDriver(model string) (string, string) {
	if strings.HasPrefix(model, "acp/") {
		return "acp", model
	}
	for _, driver := range []string{"cursor", "codex", "claude-code"} {
		if strings.HasPrefix(model, driver+"/") {
			return driver, strings.TrimPrefix(model, driver+"/")
		}
	}
	return "opencode", model
}

func normalizedBuildDriver(driver string) string {
	if driver == "" {
		return "opencode"
	}
	return driver
}
