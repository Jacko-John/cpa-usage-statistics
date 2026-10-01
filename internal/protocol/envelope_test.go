package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestOKEnvelopeMatchesGenericJSON(t *testing.T) {
	values := []any{
		nil,
		map[string]any{"stored": true, "message": "<ok>"},
		struct {
			ID    string `json:"id"`
			Count int    `json:"count"`
		}{ID: "request-id", Count: 3},
	}
	for _, value := range values {
		got, err := OK(value)
		if err != nil {
			t.Fatalf("okEnvelope: %v", err)
		}
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		want, err := json.Marshal(Envelope{OK: true, Result: payload})
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("envelope mismatch:\n got: %s\nwant: %s", got, want)
		}
	}
}
