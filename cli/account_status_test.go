// Created by Codex under Jacob's direction, Glowbom Labs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type failingAccountStore struct{ err error }

func (s failingAccountStore) Load() (accountCredentials, error) { return accountCredentials{}, s.err }
func (s failingAccountStore) Save(accountCredentials) error     { return s.err }
func (s failingAccountStore) Delete() error                     { return s.err }

func TestAccountJSONDistinguishesMissingCredentialsFromLockedStore(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status string
	}{
		{errAccountSignedOut, "signed_out"}, {errors.New("keyring locked"), "unavailable"},
		{errAccountInvalid, "signed_out"},
	} {
		c := &accountClient{store: failingAccountStore{tc.err}}
		got := c.accountStatus(context.Background(), false)
		if got.Status != tc.status || got.UID != "" || got.Version != 1 {
			t.Fatalf("unexpected status: %+v", got)
		}
	}
}

func TestAccountJSONVerifiesStatusWithoutLeakingCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, body, status, subscription string
		code                             int
	}{
		{"premium with no credits", `{"subscriptionStatus":"premium","remainingUsd":0,"allowanceUsd":20}`, "signed_in", "premium", 200},
		{"free", `{"subscriptionStatus":"no subs"}`, "signed_in", "no subs", 200},
		{"trial", `{"subscriptionStatus":"trialing"}`, "signed_in", "trialing", 200},
		{"billing pending", `{"code":"account_not_found"}`, "signed_in", "unknown", 404},
		{"revoked", `{"code":"unauthorized"}`, "signed_out", "", 401},
		{"outage", `{"error":"private-server-detail"}`, "unavailable", "", 503},
		{"malformed", `{}`, "unavailable", "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "" && r.URL.Path == "/account" {
					t.Error("account request did not authenticate")
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := &accountClient{store: &memoryAccountStore{value: testCredentials()}, http: server.Client(), config: accountConfig{APIURL: server.URL}}
			got := c.accountStatus(context.Background(), false)
			if got.Status != tc.status || got.SubscriptionStatus != tc.subscription {
				t.Fatalf("unexpected status: %+v", got)
			}
			data, _ := json.Marshal(got)
			if strings.Contains(string(data), "private-") || strings.Contains(string(data), "Token") || strings.Contains(string(data), "Usd") {
				t.Fatal("status leaked credentials or balances")
			}
			if got.Status == "signed_in" && got.UID != "owner" {
				t.Fatal("verified identity missing")
			}
			if got.Status != "signed_in" && (got.UID != "" || got.Email != "") {
				t.Fatal("unverified identity exposed")
			}
		})
	}
}
