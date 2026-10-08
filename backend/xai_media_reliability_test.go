package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func xAIMediaTestResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestXAIVideoNetworkCallsRespectCancellationAndDeadline(t *testing.T) {
	for _, action := range []string{"start", "poll"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > xAIVideoRequestTimeout || time.Until(deadline) <= 0 {
					t.Fatal("video network call has no bounded deadline")
				}
				cancel()
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			var err error
			if action == "start" {
				_, _, err = xaiStartVideoGenerationRequest(map[string]interface{}{"prompt": "fixture"}, "fixture-key", ctx)
			} else {
				_, err = pollGrokImagineVideoOperation("fixture-operation", "fixture-key", ctx)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation did not reach %s: %v", action, err)
			}
		})
	}
}

func TestXAIVideoLegacyFallbackOnlyRetriesRejectedPayload(t *testing.T) {
	for _, status := range []int{400, 422, 401, 403, 404, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return xAIMediaTestResponse(status, "application/json", `{"error":"fixture rejection"}`), nil
				}
				return xAIMediaTestResponse(200, "application/json", `{"request_id":"fixture-operation"}`), nil
			})
			result, err := startGrokImagineVideoGeneration(VeoGenerationRequest{
				Prompt: "Fixture", XaiKey: "fixture-key", Images: []VeoImageInput{{Data: "fixture"}},
			})
			if status == 400 || status == 422 {
				if err != nil || result.OperationID != "fixture-operation" || calls != 2 {
					t.Fatalf("legacy payload compatibility failed: calls=%d result=%v err=%v", calls, result, err)
				}
			} else if err == nil || calls != 1 {
				t.Fatalf("HTTP %d submitted another generation: calls=%d err=%v", status, calls, err)
			}
		})
	}
}

func TestXAIVideoResponseBodyIsBounded(t *testing.T) {
	for _, action := range []string{"start", "poll"} {
		t.Run(action, func(t *testing.T) {
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{'x'}, xAIVideoResponseLimit+1)))}, nil
			})
			var err error
			if action == "start" {
				_, _, err = xaiStartVideoGenerationRequest(map[string]interface{}{}, "fixture-key")
			} else {
				_, err = pollGrokImagineVideoOperation("fixture-operation", "fixture-key")
			}
			if err == nil || !strings.Contains(err.Error(), "too large") {
				t.Fatalf("oversized response accepted: %v", err)
			}
		})
	}
}

func TestXAIMediaDownloadCanCancelWithoutExposingSignedURL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > xAIMediaDownloadTimeout || time.Until(deadline) <= 0 {
			t.Fatal("download has no bounded deadline")
		}
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	_, _, err := downloadXAIMedia("https://vidgen.x.ai/fixture.mp4?signature=secret-download-token", "fixture-key", "video/mp4", ctx)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "secret-download-token") {
		t.Fatalf("unsafe or lost cancellation error: %v", err)
	}
}

func TestXAIMediaDownloadRejectsErrorPagesAndKeepsVideos(t *testing.T) {
	cases := []struct {
		name, mime, body string
		wantError        bool
	}{
		{"video", "video/mp4", "fixture video bytes", false},
		{"binary", "application/octet-stream", "fixture video bytes", false},
		{"html", "text/html", "<html>Expired</html>", true},
		{"json", "application/json", `{"error":"expired"}`, true},
		{"mislabeled html", "video/mp4", "<!DOCTYPE html><html>Expired</html>", true},
		{"mislabeled json", "video/mp4", `{"error":"expired"}`, true},
		{"wrong type", "image/jpeg", "fixture image", true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				return xAIMediaTestResponse(200, test.mime, test.body), nil
			})
			body, _, err := downloadXAIMedia("https://vidgen.x.ai/fixture.mp4", "fixture-key", "video/mp4")
			if (err != nil) != test.wantError || (!test.wantError && string(body) != test.body) {
				t.Fatalf("unexpected downloaded video validation: body=%q err=%v", body, err)
			}
		})
	}
}

type xAIRepeatedMediaReader struct {
	remaining, read int
}

func (r *xAIRepeatedMediaReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	count := min(len(p), r.remaining)
	for index := range count {
		p[index] = 'v'
	}
	r.remaining -= count
	r.read += count
	return count, nil
}

func TestXAIMediaDownloadRejectsTruncatedOversizedVideo(t *testing.T) {
	body := &xAIRepeatedMediaReader{remaining: xAIMediaDownloadLimit + 20}
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"video/mp4"}}, Body: io.NopCloser(body)}, nil
	})
	data, _, err := downloadXAIMedia("https://vidgen.x.ai/fixture.mp4", "fixture-key", "video/mp4")
	if err == nil || len(data) != 0 || body.read != xAIMediaDownloadLimit+1 {
		t.Fatalf("oversized video silently truncated: saved=%d read=%d err=%v", len(data), body.read, err)
	}
}

func TestXAIMediaDownloadDoesNotSendCredentialsAcrossRedirects(t *testing.T) {
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			if r.Header.Get("Authorization") != "Bearer fixture-key" {
				t.Fatal("xAI-owned download did not receive its credential")
			}
			response := xAIMediaTestResponse(302, "text/plain", "")
			response.Header.Set("Location", "https://cdn.example.com/fixture.mp4")
			return response, nil
		}
		if r.URL.Host != "cdn.example.com" || r.Header.Get("Authorization") != "" {
			t.Fatal("provider credential leaked to a third-party redirect")
		}
		return xAIMediaTestResponse(200, "video/mp4", "fixture video bytes"), nil
	})
	if _, _, err := downloadXAIMedia("https://vidgen.x.ai/fixture.mp4", "fixture-key", "video/mp4"); err != nil || calls != 2 {
		t.Fatalf("authenticated download redirect failed: calls=%d err=%v", calls, err)
	}
}

func TestXAIRefreshUsesBoundedCancellation(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 30*time.Second || time.Until(deadline) <= 0 {
			t.Fatal("refresh has no bounded deadline")
		}
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	_, err := refreshXAICredential(xAICredential{Kind: "subscription", Refresh: "fixture-refresh"}, xAIOAuthTokenURL, http.DefaultClient, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh lost cancellation: %v", err)
	}
}

func TestXAIBearerContextDoesNotStartCanceledWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err := withXAIBearerContext(ctx, func(bearer string) (string, error) {
		called = true
		return "fixture", nil
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled action started: called=%t err=%v", called, err)
	}
}
