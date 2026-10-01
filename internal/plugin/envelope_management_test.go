package plugin

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Jacko-John/cpa-usage-statistics/internal/protocol"
)

func TestOKManagementEnvelopeMatchesGenericJSON(t *testing.T) {
	responses := []protocol.ManagementResponse{
		jsonManagementResponse(200, map[string]any{
			"items": []string{strings.Repeat("large payload ", 100_000)},
		}),
		{
			StatusCode: 204,
			Headers:    map[string][]string{"X-Test": {"one", "two"}},
			Body:       nil,
		},
	}

	for _, response := range responses {
		got, err := protocol.OK(response)
		if err != nil {
			t.Fatalf("okEnvelope: %v", err)
		}
		inner, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("marshal management response: %v", err)
		}
		want, err := json.Marshal(protocol.Envelope{OK: true, Result: inner})
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("optimized envelope differs from generic JSON encoding")
		}
	}
}
