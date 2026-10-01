package model

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestFailureBodyJSON(t *testing.T) {
	tests := []struct {
		name string
		body FailureBody
		want string
	}{
		{name: "json object", body: `{"error":{"code":"cyber_policy"}}`, want: `{"failure_body":{"error":{"code":"cyber_policy"}}}`},
		{name: "plain text", body: "rate limited", want: `{"failure_body":"rate limited"}`},
		{name: "empty", want: `{"failure_body":null}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(struct {
				FailureBody FailureBody `json:"failure_body"`
			}{FailureBody: test.body})
			if err != nil {
				t.Fatalf("marshal failure body: %v", err)
			}
			if string(encoded) != test.want {
				t.Fatalf("encoded = %s, want %s", encoded, test.want)
			}
		})
	}

	emptyUsage, err := json.Marshal(RequestDetail{})
	if err != nil {
		t.Fatalf("marshal empty usage: %v", err)
	}
	if bytes.Contains(emptyUsage, []byte(`"failure_body"`)) {
		t.Fatalf("empty usage contains failure_body: %s", emptyUsage)
	}
}
