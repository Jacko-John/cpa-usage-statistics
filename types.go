package main

import (
	"encoding/json"
	"time"
)

// TokenStats holds token accounting for one request, aligned with upstream
// usage.Detail field semantics.
type TokenStats struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

type failureBody string

func (body failureBody) MarshalJSON() ([]byte, error) {
	if body == "" {
		return []byte("null"), nil
	}
	raw := []byte(body)
	if json.Valid(raw) {
		return raw, nil
	}
	return json.Marshal(string(body))
}

// Record is one persisted usage row. Column names mirror upstream
// usage.Record / usage.Detail naming to avoid ambiguity.
type Record struct {
	ID                string
	Timestamp         time.Time
	APIKey            string
	Provider          string
	Model             string
	Alias             string
	Source            string
	AuthID            string
	AuthIndex         string
	AuthType          string
	ExecutorType      string
	ReasoningEffort   string
	ServiceTier       string
	LatencyMs         int64
	TTFTMs            int64
	Tokens            TokenStats
	Failed            bool
	FailureStatusCode int
	FailureBody       string
}

// SafetyState 是一个下游 API Key 的持久化与内存状态。
type SafetyState struct {
	APIKey                   string
	TotalTriggerCount        int64
	TotalLimitCount          int64
	RecentTriggerTimes       []int64
	BlockedUntil             int64
	ActiveLimitStartedAt     int64
	ActiveLimitUsageRecordID string
}

// SafetyHistory 记录一次 cyber_policy 触发及其可选限流周期。
type SafetyHistory struct {
	APIKey            string      `json:"api_key"`
	OccurredAt        int64       `json:"occurred_at"`
	UsageRecordID     string      `json:"usage_record_id"`
	BlockedUntil      int64       `json:"blocked_until"`
	ManualReleasedAt  int64       `json:"manual_released_at"`
	Provider          string      `json:"provider"`
	Model             string      `json:"model"`
	FailureStatusCode int         `json:"failure_status_code"`
	FailureBody       failureBody `json:"failure_body"`
}

type SafetyHistoryQuery struct {
	APIKey      string
	BlockedOnly bool
	BeforeTime  int64
	BeforeID    string
	Limit       int
}

type SafetyReleaseResult struct {
	Released   []string `json:"released"`
	NotBlocked []string `json:"not_blocked"`
}

// RequestDetail is the outward JSON shape returned by the usage API. It omits
// storage-only grouping fields (api_key/model live in the map keys).
type RequestDetail struct {
	ID                string      `json:"id"`
	Timestamp         time.Time   `json:"timestamp"`
	Provider          string      `json:"provider,omitempty"`
	Model             string      `json:"model,omitempty"`
	Alias             string      `json:"alias,omitempty"`
	Source            string      `json:"source"`
	AuthID            string      `json:"auth_id,omitempty"`
	AuthIndex         string      `json:"auth_index"`
	AuthType          string      `json:"auth_type,omitempty"`
	ExecutorType      string      `json:"executor_type,omitempty"`
	ReasoningEffort   string      `json:"reasoning_effort"`
	ServiceTier       string      `json:"service_tier"`
	LatencyMs         int64       `json:"latency_ms"`
	TTFTMs            int64       `json:"ttft_ms"`
	Tokens            TokenStats  `json:"tokens"`
	Failed            bool        `json:"failed"`
	FailureStatusCode int         `json:"failure_status_code,omitempty"`
	FailureBody       failureBody `json:"failure_body,omitempty"`
}

// APIUsage groups details by grouping key (api_key or provider) then by model.
type APIUsage map[string]map[string][]RequestDetail

// QueryRange bounds a usage query by timestamp.
type QueryRange struct {
	Start *time.Time
	End   *time.Time
}

// DeleteResult reports the outcome of a delete-by-id request.
type DeleteResult struct {
	Deleted int64    `json:"deleted"`
	Missing []string `json:"missing"`
}

// StableStringSet keeps each string once while preserving insertion order.
type StableStringSet struct {
	values []string
	seen   map[string]struct{}
}

func NewStableStringSet(capacity int) *StableStringSet {
	return &StableStringSet{
		values: make([]string, 0, capacity),
		seen:   make(map[string]struct{}, capacity),
	}
}

// Add inserts value when it is new and reports whether the set changed.
func (s *StableStringSet) Add(value string) bool {
	if _, exists := s.seen[value]; exists {
		return false
	}
	s.seen[value] = struct{}{}
	s.values = append(s.values, value)
	return true
}

func (s *StableStringSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.values)
}

// Values returns values in insertion order. Callers use the slice as read-only.
func (s *StableStringSet) Values() []string {
	if s == nil {
		return nil
	}
	return s.values
}
