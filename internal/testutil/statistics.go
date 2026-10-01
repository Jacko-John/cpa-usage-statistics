package testutil

import (
	"encoding/json"
	"math"
	"testing"
)

// UsageSummary decodes the wire format independently of the statistics implementation.
type UsageSummary struct {
	Range         string                               `json:"range"`
	GeneratedAt   int64                                `json:"generated_at"`
	RateRequests  int64                                `json:"rate_requests"`
	RateTokens    int64                                `json:"rate_tokens"`
	RateMinutes   int64                                `json:"rate_minutes"`
	TotalRequests int64                                `json:"total_requests"`
	SuccessCount  int64                                `json:"success_count"`
	FailureCount  int64                                `json:"failure_count"`
	TotalTokens   int64                                `json:"total_tokens"`
	TotalCost     float64                              `json:"total_cost"`
	APIs          map[string]*UsageAPIAggregate        `json:"apis"`
	Credentials   map[string]*UsageCredentialAggregate `json:"credentials"`
	Hours         map[int64]*UsageAggregate            `json:"hours"`
	Days          map[string]*UsageAggregate           `json:"days"`
}

func DecodeUsageSummary(t testing.TB, payload []byte) UsageSummary {
	t.Helper()
	var summary UsageSummary
	if err := json.Unmarshal(payload, &summary); err != nil {
		t.Fatalf("unmarshal usage summary: %v", err)
	}
	return summary
}

func DecodeCredentialWindows(t testing.TB, payload []byte) CredentialWindowResponse {
	t.Helper()
	var response CredentialWindowResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("unmarshal credential windows: %v", err)
	}
	return response
}

func AssertCost(t testing.TB, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %.12f, want %.12f", got, want)
	}
}

type UsageAggregate struct {
	TotalRequests               int64   `json:"total_requests"`
	SuccessCount                int64   `json:"success_count"`
	FailureCount                int64   `json:"failure_count"`
	TotalTokens                 int64   `json:"total_tokens"`
	TotalCost                   float64 `json:"total_cost"`
	LatencyTotalMs              int64   `json:"latency_total_ms"`
	LatencySampleCount          int64   `json:"latency_sample_count"`
	FirstByteLatencyTotalMs     int64   `json:"first_byte_latency_total_ms"`
	FirstByteLatencySampleCount int64   `json:"first_byte_latency_sample_count"`
	TPSOutputTokens             int64   `json:"tps_output_tokens"`
	TPSGenerationDurationMs     int64   `json:"tps_generation_duration_ms"`
	TPSSampleCount              int64   `json:"tps_sample_count"`
}

type UsageAPIAggregate struct {
	UsageAggregate
	Models map[string]*UsageAggregate `json:"models"`
}

type UsageCredentialAggregate struct {
	UsageAggregate
	Source   string `json:"source,omitempty"`
	AuthType string `json:"auth_type,omitempty"`
}

type CredentialWindowSummary struct {
	Requests     int64   `json:"requests"`
	SuccessCount int64   `json:"success_count"`
	FailureCount int64   `json:"failure_count"`
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost"`
}
type CredentialWindowResponse struct {
	Windows map[string]CredentialWindowSummary
}
