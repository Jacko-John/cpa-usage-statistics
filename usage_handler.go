package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

type usageRecord struct {
	Provider        string       `json:"Provider"`
	ExecutorType    string       `json:"ExecutorType"`
	Model           string       `json:"Model"`
	Alias           string       `json:"Alias"`
	APIKey          string       `json:"APIKey"`
	AuthID          string       `json:"AuthID"`
	AuthIndex       string       `json:"AuthIndex"`
	AuthType        string       `json:"AuthType"`
	Source          string       `json:"Source"`
	ReasoningEffort string       `json:"ReasoningEffort"`
	ServiceTier     string       `json:"ServiceTier"`
	RequestedAt     time.Time    `json:"RequestedAt"`
	Latency         int64        `json:"Latency"`
	TTFT            int64        `json:"TTFT"`
	Failed          bool         `json:"Failed"`
	Failure         usageFailure `json:"Failure"`
	Detail          usageDetail  `json:"Detail"`
}

type usageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

type usageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

var (
	ignoredUsageEnvelope = []byte(`{"ok":true,"result":{"ignored":true}}`)
	storedUsageEnvelope  = []byte(`{"ok":true,"result":{"stored":true}}`)
)

func handleUsage(request []byte) ([]byte, error) {
	if len(request) == 0 {
		return ignoredUsageEnvelope, nil
	}
	var record usageRecord
	if err := json.Unmarshal(request, &record); err != nil {
		return ignoredUsageEnvelope, nil
	}
	store := currentStore()
	if store == nil {
		return ignoredUsageEnvelope, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	if err := safety.storeUsage(ctx, store, toRecord(record), time.Now()); err != nil {
		return nil, err
	}
	maybeCleanup()
	return storedUsageEnvelope, nil
}

func toRecord(record usageRecord) Record {
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	return Record{
		ID:              uuid.NewString(),
		Timestamp:       timestamp.UTC(),
		APIKey:          strings.TrimSpace(record.APIKey),
		Provider:        strings.TrimSpace(record.Provider),
		Model:           normalizeModel(record.Model),
		Alias:           strings.TrimSpace(record.Alias),
		Source:          strings.TrimSpace(record.Source),
		AuthID:          strings.TrimSpace(record.AuthID),
		AuthIndex:       strings.TrimSpace(record.AuthIndex),
		AuthType:        strings.TrimSpace(record.AuthType),
		ExecutorType:    strings.TrimSpace(record.ExecutorType),
		ReasoningEffort: strings.TrimSpace(record.ReasoningEffort),
		ServiceTier:     strings.TrimSpace(record.ServiceTier),
		LatencyMs:       nsToMs(record.Latency),
		TTFTMs:          nsToMs(record.TTFT),
		Tokens: TokenStats{
			InputTokens:         record.Detail.InputTokens,
			OutputTokens:        record.Detail.OutputTokens,
			ReasoningTokens:     record.Detail.ReasoningTokens,
			CachedTokens:        record.Detail.CachedTokens,
			CacheReadTokens:     record.Detail.CacheReadTokens,
			CacheCreationTokens: record.Detail.CacheCreationTokens,
			TotalTokens:         record.Detail.TotalTokens,
		},
		Failed:            record.Failed || record.Failure.StatusCode >= 400,
		FailureStatusCode: record.Failure.StatusCode,
		FailureBody:       strings.TrimSpace(record.Failure.Body),
	}
}

func nsToMs(nanoseconds int64) int64 {
	if nanoseconds <= 0 {
		return 0
	}
	return nanoseconds / int64(time.Millisecond)
}
