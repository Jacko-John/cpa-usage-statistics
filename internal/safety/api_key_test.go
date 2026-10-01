package safety

import (
	"net/http"
	"testing"
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
			wantError: AmbiguousAPIKey,
		},
		{
			name:      "unsupported transport",
			headers:   http.Header{"Content-Type": {"application/json"}},
			wantError: UnsupportedAPIKeyTransport,
		},
		{
			name:    "case-insensitive bearer scheme",
			headers: http.Header{"Authorization": {"bEaReR client-key"}},
			wantKey: "client-key",
		},
		{
			name:      "empty bearer",
			headers:   http.Header{"Authorization": {"Bearer"}},
			wantError: UnsupportedAPIKeyTransport,
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
