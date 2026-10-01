package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Jacko-John/cpa-usage-statistics/internal/config"
	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/protocol"
	safetycontrol "github.com/Jacko-John/cpa-usage-statistics/internal/safety"
)

func TestRequestInterceptorAuthenticationErrors(t *testing.T) {
	useSafetyLimiter(t, seedLimiter(t, config.Safety{RateLimitEnabled: true}, make(map[string]usagemodel.SafetyState)))

	missing := interceptForTest(t, requestInterceptRequest{SourceFormat: "openai"})
	if !missing.Terminate || missing.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing header response = %+v", missing)
	}
	if !bytes.Contains(missing.ResponseBody, []byte(safetycontrol.UnsupportedAPIKeyTransport)) {
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
	useSafetyLimiter(t, seedLimiter(t, config.Safety{RateLimitEnabled: true}, map[string]usagemodel.SafetyState{
		"client-key": {APIKey: "client-key", BlockedUntil: blockedUntil},
	}))

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

func TestRequestInterceptorReturnsProtocolCompatibleDisabledError(t *testing.T) {
	useSafetyLimiter(t, seedLimiter(t, config.Safety{}, map[string]usagemodel.SafetyState{
		"client-key": {APIKey: "client-key", ManualDisabled: true},
	}))

	tests := []struct {
		format string
		marker string
	}{
		{format: "openai", marker: `"code":"api_key_disabled"`},
		{format: "openai-response", marker: `"type":"permission_error"`},
		{format: "claude", marker: `"type":"permission_error"`},
		{format: "gemini", marker: `"status":"PERMISSION_DENIED"`},
	}
	for _, test := range tests {
		t.Run(test.format, func(t *testing.T) {
			response := interceptForTest(t, requestInterceptRequest{
				SourceFormat: test.format,
				Headers:      http.Header{"Authorization": {"Bearer client-key"}},
			})
			if !response.Terminate || response.StatusCode != http.StatusForbidden {
				t.Fatalf("response = %+v", response)
			}
			if response.ResponseHeaders.Get("Retry-After") != "" {
				t.Fatalf("Retry-After = %q", response.ResponseHeaders.Get("Retry-After"))
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
	useSafetyLimiter(t, seedLimiter(t, config.Safety{RateLimitEnabled: false}, map[string]usagemodel.SafetyState{
		"client-key": {APIKey: "client-key", BlockedUntil: time.Now().Unix() + 600},
	}))
	response := interceptForTest(t, requestInterceptRequest{})
	if response.Terminate {
		t.Fatalf("disabled interceptor response = %+v", response)
	}
	raw, err := interceptRequestAfter(nil)
	if err != nil || len(raw) == 0 {
		t.Fatalf("interceptRequestAfter() = %q, %v", raw, err)
	}
}

func useSafetyLimiter(t *testing.T, limiter *safetycontrol.Limiter) {
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
	var result protocol.Envelope
	if err := json.Unmarshal(rawEnvelope, &result); err != nil {
		t.Fatalf("unmarshal intercept envelope: %v", err)
	}
	var response requestInterceptResponse
	if err := json.Unmarshal(result.Result, &response); err != nil {
		t.Fatalf("unmarshal intercept response: %v", err)
	}
	return response
}
