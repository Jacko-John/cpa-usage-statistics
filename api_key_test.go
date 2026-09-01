package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestExtractAPIKey(t *testing.T) {
	tests := []struct {
		name      string
		headers   http.Header
		wantKey   string
		wantError string
	}{
		{
			name:    "bearer authorization",
			headers: http.Header{"Authorization": {"  Bearer client-key  "}},
			wantKey: "client-key",
		},
		{
			name:    "plain authorization",
			headers: http.Header{"Authorization": {" client-key "}},
			wantKey: "client-key",
		},
		{
			name:    "claude header",
			headers: http.Header{"X-Api-Key": {"client-key"}},
			wantKey: "client-key",
		},
		{
			name:    "gemini header",
			headers: http.Header{"X-Goog-Api-Key": {"client-key"}},
			wantKey: "client-key",
		},
		{
			name: "same key in multiple headers",
			headers: http.Header{
				"Authorization":  {"Bearer client-key"},
				"X-Api-Key":      {"client-key"},
				"X-Goog-Api-Key": {"client-key"},
			},
			wantKey: "client-key",
		},
		{
			name:      "different keys",
			headers:   http.Header{"Authorization": {"Bearer first"}, "X-Api-Key": {"second"}},
			wantError: ambiguousAPIKey,
		},
		{
			name:      "unsupported transport",
			headers:   http.Header{"Content-Type": {"application/json"}},
			wantError: unsupportedAPIKeyTransport,
		},
		{
			name:    "case-insensitive bearer scheme",
			headers: http.Header{"Authorization": {"bEaReR client-key"}},
			wantKey: "client-key",
		},
		{
			name:      "empty bearer",
			headers:   http.Header{"Authorization": {"Bearer"}},
			wantError: unsupportedAPIKeyTransport,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key, errorCode := extractAPIKey(test.headers)
			if key != test.wantKey || errorCode != test.wantError {
				t.Fatalf("extractAPIKey() = (%q, %q), want (%q, %q)", key, errorCode, test.wantKey, test.wantError)
			}
		})
	}
}

func TestRequestInterceptorAuthenticationErrors(t *testing.T) {
	useSafetyLimiter(t, &safetyLimiter{
		config: safetyConfig{RateLimitEnabled: true},
		states: make(map[string]SafetyState),
	})

	missing := interceptForTest(t, requestInterceptRequest{SourceFormat: "openai"})
	if !missing.Terminate || missing.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing header response = %+v", missing)
	}
	if !bytes.Contains(missing.ResponseBody, []byte(unsupportedAPIKeyTransport)) {
		t.Fatalf("missing header body = %s", missing.ResponseBody)
	}

	ambiguous := interceptForTest(t, requestInterceptRequest{
		SourceFormat: "claude",
		Headers: http.Header{
			"Authorization": {"Bearer first-secret"},
			"X-Api-Key":     {"second-secret"},
		},
	})
	if !ambiguous.Terminate || ambiguous.StatusCode != http.StatusBadRequest {
		t.Fatalf("ambiguous response = %+v", ambiguous)
	}
	if bytes.Contains(ambiguous.ResponseBody, []byte("first-secret")) || bytes.Contains(ambiguous.ResponseBody, []byte("second-secret")) {
		t.Fatalf("ambiguous response leaked an API key: %s", ambiguous.ResponseBody)
	}

	allowed := interceptForTest(t, requestInterceptRequest{
		Headers: http.Header{
			"Authorization": {"Bearer same-key"},
			"X-Api-Key":     {"same-key"},
		},
	})
	if allowed.Terminate {
		t.Fatalf("same-key request was terminated: %+v", allowed)
	}
}

func TestRequestInterceptorReturnsProtocolCompatibleRateLimit(t *testing.T) {
	blockedUntil := time.Now().Unix() + 600
	useSafetyLimiter(t, &safetyLimiter{
		config: safetyConfig{RateLimitEnabled: true},
		states: map[string]SafetyState{
			"client-key": {APIKey: "client-key", BlockedUntil: blockedUntil},
		},
	})

	tests := []struct {
		format string
		marker string
	}{
		{format: "openai", marker: `"code":"safety_rate_limited"`},
		{format: "openai-response", marker: `"type":"rate_limit_error"`},
		{format: "claude", marker: `"type":"error"`},
		{format: "gemini", marker: `"status":"RESOURCE_EXHAUSTED"`},
	}
	for _, test := range tests {
		t.Run(test.format, func(t *testing.T) {
			response := interceptForTest(t, requestInterceptRequest{
				SourceFormat: test.format,
				Headers:      http.Header{"Authorization": {"Bearer client-key"}},
			})
			if !response.Terminate || response.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("response = %+v", response)
			}
			if response.ResponseHeaders.Get("Content-Type") != "application/json" {
				t.Fatalf("content type = %q", response.ResponseHeaders.Get("Content-Type"))
			}
			if response.ResponseHeaders.Get("Retry-After") == "" {
				t.Fatal("Retry-After is empty")
			}
			if !bytes.Contains(response.ResponseBody, []byte(test.marker)) {
				t.Fatalf("body = %s, want marker %s", response.ResponseBody, test.marker)
			}
			if bytes.Contains(response.ResponseBody, []byte("client-key")) {
				t.Fatalf("response leaked API key: %s", response.ResponseBody)
			}
		})
	}
}

func TestRequestInterceptorDisabledIsNoOp(t *testing.T) {
	useSafetyLimiter(t, &safetyLimiter{
		config: safetyConfig{RateLimitEnabled: false},
		states: map[string]SafetyState{
			"client-key": {APIKey: "client-key", BlockedUntil: time.Now().Unix() + 600},
		},
	})
	response := interceptForTest(t, requestInterceptRequest{})
	if response.Terminate {
		t.Fatalf("disabled interceptor response = %+v", response)
	}
	raw, err := interceptRequestAfter(nil)
	if err != nil || len(raw) == 0 {
		t.Fatalf("interceptRequestAfter() = %q, %v", raw, err)
	}
}

func useSafetyLimiter(t *testing.T, limiter *safetyLimiter) {
	t.Helper()
	previous := safety
	safety = limiter
	t.Cleanup(func() { safety = previous })
}

func interceptForTest(t *testing.T, req requestInterceptRequest) requestInterceptResponse {
	t.Helper()
	rawRequest, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal intercept request: %v", err)
	}
	rawEnvelope, err := interceptRequestBefore(rawRequest)
	if err != nil {
		t.Fatalf("interceptRequestBefore: %v", err)
	}
	var result envelope
	if err := json.Unmarshal(rawEnvelope, &result); err != nil {
		t.Fatalf("unmarshal intercept envelope: %v", err)
	}
	var response requestInterceptResponse
	if err := json.Unmarshal(result.Result, &response); err != nil {
		t.Fatalf("unmarshal intercept response: %v", err)
	}
	return response
}
