// Created by Codex under Jacob's direction, Glowbom Labs.
package main

import (
	"context"
	"errors"
)

// This deliberately excludes credentials and generation balances. Consumers
// must check Version and Status before using identity or subscription fields.
type accountStatusJSON struct {
	Version            int    `json:"version"`
	Status             string `json:"status"`
	UID                string `json:"uid,omitempty"`
	Email              string `json:"email,omitempty"`
	SubscriptionStatus string `json:"subscriptionStatus,omitempty"`
	Code               string `json:"code,omitempty"`
}

func (c *accountClient) accountStatus(ctx context.Context, forceRefresh bool) accountStatusJSON {
	credentials, summary, err := c.readAccount(ctx, forceRefresh)
	result := accountStatusJSON{Version: 1, Status: "unavailable", Code: "account_unavailable"}
	var apiError *accountAPIError
	if errors.Is(err, errAccountSignedOut) || errors.Is(err, errAccountInvalid) || (errors.As(err, &apiError) && apiError.status == 401) {
		result.Status, result.Code = "signed_out", "sign_in_required"
		return result
	}
	// The hosted account endpoint verifies identity before reporting that the
	// customer's billing record is not ready. Free account content still works.
	notReady := errors.As(err, &apiError) && apiError.status == 404 && apiError.code == "account_not_found"
	if err != nil && !notReady {
		return result
	}
	result.Status, result.Code = "signed_in", ""
	result.UID, result.Email = credentials.UID, credentials.Email
	result.SubscriptionStatus = summary.SubscriptionStatus
	if notReady {
		result.SubscriptionStatus, result.Code = "unknown", "account_not_ready"
	}
	return result
}
