package plugin

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/Jacko-John/cpa-usage-statistics/internal/protocol"
)

func TestFixedUsageEnvelopesMatchGenericJSON(t *testing.T) {
	for name, test := range map[string]struct {
		got     []byte
		payload map[string]any
	}{
		"ignored": {got: ignoredUsageEnvelope, payload: map[string]any{"ignored": true}},
		"stored":  {got: storedUsageEnvelope, payload: map[string]any{"stored": true}},
	} {
		t.Run(name, func(t *testing.T) {
			payload, err := json.Marshal(test.payload)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(protocol.Envelope{OK: true, Result: payload})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(test.got, want) {
				t.Fatalf("fixed envelope = %s, want %s", test.got, want)
			}
		})
	}
}

func TestManagementErrorPayloadMatchesMapJSON(t *testing.T) {
	response := managementErrorResponse(404, "route not found")
	want, err := json.Marshal(map[string]string{"error": "route not found"})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 404 || !bytes.Equal(response.Body, want) {
		t.Fatalf("response = %+v, want body %s", response, want)
	}
}
