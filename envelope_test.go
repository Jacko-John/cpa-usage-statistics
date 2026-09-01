package main

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
		got, err := okEnvelope(value)
		if err != nil {
			t.Fatalf("okEnvelope: %v", err)
		}
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		want, err := json.Marshal(envelope{OK: true, Result: payload})
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("envelope mismatch:\n got: %s\nwant: %s", got, want)
		}
	}
}

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
			want, err := json.Marshal(envelope{OK: true, Result: payload})
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
