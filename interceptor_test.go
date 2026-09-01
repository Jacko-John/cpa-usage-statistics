package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestEmptyInterceptorEnvelopeMatchesGenericJSON(t *testing.T) {
	payload, err := json.Marshal(struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(envelope{OK: true, Result: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(emptyInterceptorEnvelope, want) {
		t.Fatalf("empty interceptor envelope = %s, want %s", emptyInterceptorEnvelope, want)
	}
}

func TestProtocolErrorBodiesRemainByteCompatible(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests} {
		for _, format := range []string{"openai", " CLAUDE ", "Gemini"} {
			got, err := protocolErrorBody(format, status, "error_code", "error message")
			if err != nil {
				t.Fatalf("protocolErrorBody: %v", err)
			}
			want, err := previousProtocolErrorBody(format, status, "error_code", "error message")
			if err != nil {
				t.Fatalf("previousProtocolErrorBody: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("status=%d format=%q:\n got: %s\nwant: %s", status, format, got, want)
			}
		}
	}
}

func previousProtocolErrorBody(sourceFormat string, status int, code, message string) ([]byte, error) {
	errorType := "invalid_request_error"
	if status == http.StatusUnauthorized {
		errorType = "authentication_error"
	} else if status == http.StatusTooManyRequests {
		errorType = "rate_limit_error"
	}

	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case "claude":
		return json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    errorType,
				"code":    code,
				"message": message,
			},
		})
	case "gemini":
		geminiStatus := "INVALID_ARGUMENT"
		if status == http.StatusUnauthorized {
			geminiStatus = "UNAUTHENTICATED"
		} else if status == http.StatusTooManyRequests {
			geminiStatus = "RESOURCE_EXHAUSTED"
		}
		return json.Marshal(map[string]any{
			"error": map[string]any{
				"code":    status,
				"message": message,
				"status":  geminiStatus,
				"details": []map[string]string{{
					"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
					"reason": code,
				}},
			},
		})
	default:
		return json.Marshal(map[string]any{
			"error": map[string]any{
				"type":    errorType,
				"code":    code,
				"message": message,
			},
		})
	}
}
