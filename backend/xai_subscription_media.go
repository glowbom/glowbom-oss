package main

import (
	"errors"
	"os"
	"strings"
)

const grokSubscriptionMediaFlag = "GLOWBOM_ENABLE_GROK_SUBSCRIPTION_MEDIA"

var errGrokSubscriptionMediaDisabled = errors.New("Grok subscription images and videos are disabled. Connect an xAI API key to generate media.")

func grokSubscriptionMediaEnabled() bool {
	return strings.TrimSpace(os.Getenv(grokSubscriptionMediaFlag)) != "0"
}

func requireGrokSubscriptionMedia() error {
	if !grokSubscriptionMediaEnabled() {
		return errGrokSubscriptionMediaDisabled
	}
	return nil
}
