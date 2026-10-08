package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCompanionPublicTextPreservesEmptyFieldsAndRedactsErrors(t *testing.T) {
	for _, value := range []string{"", " \t\n\r"} {
		if got := companionPublicText(value, 2000); got != "" {
			t.Fatalf("empty optional text became %q", got)
		}
	}
	if got := companionPublicText("Preview failed: api_key=private-value", 2000); got != "Preview failed: api_key=[redacted]" {
		t.Fatalf("preview error lost its message or exposed a credential: %q", got)
	}
}

func TestCompanionPermissionPreservesAvailableResponses(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		want    []string
		present bool
	}{
		{"codex", `{"id":"codex-request","sessionID":"private-session","availableResponses":["once","session","reject","cancel"]}`, []string{"once", "session", "reject", "cancel"}, true},
		{"filtered", `{"id":"codex-request","availableResponses":["session","always","session","unknown","reject"]}`, []string{"session", "reject"}, true},
		{"empty", `{"id":"codex-request","availableResponses":[]}`, []string{}, true},
		{"invalid", `{"id":"codex-request","availableResponses":"invalid"}`, []string{}, true},
		{"opencode", `{"id":"permission","sessionID":"private-session"}`, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			pending := companionPublicPending(json.RawMessage(test.payload), "permission")
			responses, present := pending["availableResponses"]
			if present != test.present || present && !reflect.DeepEqual(responses, test.want) {
				t.Fatalf("availableResponses = %#v, present = %v; want %#v, %v", responses, present, test.want, test.present)
			}
			if _, exposed := pending["sessionID"]; exposed {
				t.Fatal("public permission exposed its private session")
			}
		})
	}
}
