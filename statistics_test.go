package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newStatisticsForTest(now int64, pricing usagePricing) *usageStatistics {
	pricing, pricingKey := normalizeUsagePricing(pricing)
	return &usageStatistics{state: newUsageStatisticsState(now, pricing, pricingKey)}
}

type usageSummaryForTest struct {
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
	APIs          map[string]*usageAPIAggregate        `json:"apis"`
	Credentials   map[string]*usageCredentialAggregate `json:"credentials"`
	Hours         map[int64]*usageAggregate            `json:"hours"`
	Days          map[string]*usageAggregate           `json:"days"`
}

func decodeUsageSummaryForTest(t testing.TB, payload []byte) usageSummaryForTest {
	t.Helper()
	var summary usageSummaryForTest
	if err := json.Unmarshal(payload, &summary); err != nil {
		t.Fatalf("unmarshal usage summary: %v", err)
	}
	return summary
}

func decodeCredentialWindowsForTest(t testing.TB, payload []byte) credentialWindowResponse {
	t.Helper()
	var response credentialWindowResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("unmarshal credential windows: %v", err)
	}
	return response
}

func assertCostForTest(t testing.TB, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %.12f, want %.12f", got, want)
	}
}

func TestUsageStatisticsMaintainsOnlyPublicRanges(t *testing.T) {
	if len(statisticsRanges) != len(statisticsRangeSeconds) {
		t.Fatalf("maintained ranges = %d, public ranges = %d", len(statisticsRanges), len(statisticsRangeSeconds))
	}
	for name, seconds := range statisticsRangeSeconds {
		index, ok := statisticsRangeByName(name)
		if !ok {
			t.Fatalf("public range %q is not maintained", name)
		}
		if got := statisticsRanges[index]; got != seconds {
			t.Fatalf("range %q = %d, want %d", name, got, seconds)
		}
	}
}

func TestUsageSummaryWithoutStoreUsesRequestedRateMinutes(t *testing.T) {
	useGlobalSafetyStore(t, nil, nil)
	for _, test := range []struct {
		rangeName   string
		rateMinutes int64
	}{
		{rangeName: "7h", rateMinutes: 420},
		{rangeName: "24h", rateMinutes: 1440},
		{rangeName: "7d", rateMinutes: 10080},
		{rangeName: "30d", rateMinutes: 43200},
		{rangeName: "all"},
	} {
		t.Run(test.rangeName, func(t *testing.T) {
			body, err := json.Marshal(usageSummaryRequest{Range: test.rangeName})
			if err != nil {
				t.Fatal(err)
			}
			response := callManagementForTest(t, managementRequest{
				Method: "POST",
				Path:   "/v0/management" + managementUsagePath,
				Body:   body,
			})
			if response.StatusCode != 200 {
				t.Fatalf("status = %d, body=%s", response.StatusCode, response.Body)
			}
			summary := decodeUsageSummaryForTest(t, response.Body)
			if summary.Range != test.rangeName || summary.RateMinutes != test.rateMinutes {
				t.Fatalf("summary = %+v", summary)
			}
		})
	}
}

func TestUsageStatisticsIncrementalRanges(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC).Unix()
	statistics := newStatisticsForTest(now, usagePricing{})
	statistics.add(Record{
		ID:        "recent",
		Timestamp: time.Unix(now-6*statisticsHour, 0),
		APIKey:    "client-key",
		Model:     "model-a",
		AuthIndex: "auth-1",
		Source:    "credential.json",
		LatencyMs: 1000,
		TTFTMs:    200,
		Tokens:    TokenStats{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	}, now)
	statistics.add(Record{
		ID:        "older",
		Timestamp: time.Unix(now-8*statisticsHour, 0),
		APIKey:    "client-key",
		Model:     "model-a",
		AuthIndex: "auth-1",
		Source:    "credential.json",
		Failed:    true,
		Tokens:    TokenStats{TotalTokens: 25},
	}, now)

	payload, err := statistics.summaryJSON("7h", now)
	if err != nil {
		t.Fatalf("summary 7h: %v", err)
	}
	summary := decodeUsageSummaryForTest(t, payload)
	if summary.TotalRequests != 1 || summary.SuccessCount != 1 || summary.TotalTokens != 150 {
		t.Fatalf("7h summary = %+v", summary)
	}
	if summary.RateRequests != 1 || summary.RateTokens != 150 || summary.RateMinutes != 420 {
		t.Fatalf("7h rate = %+v", summary)
	}
	api := summary.APIs["client-key"]
	if api == nil || api.TotalRequests != 1 || api.Models["model-a"].TotalRequests != 1 {
		t.Fatalf("7h API summary = %+v", api)
	}
	credential := summary.Credentials["source:credential.json"]
	if credential == nil || credential.TotalRequests != 1 || credential.Source != "credential.json" {
		t.Fatalf("7h credential summary = %+v", credential)
	}

	payload, err = statistics.summaryJSON("24h", now)
	if err != nil {
		t.Fatalf("summary 24h: %v", err)
	}
	summary = decodeUsageSummaryForTest(t, payload)
	if summary.TotalRequests != 2 || summary.SuccessCount != 1 || summary.FailureCount != 1 {
		t.Fatalf("24h summary = %+v", summary)
	}

	payload, err = statistics.summaryJSON("7h", now+2*statisticsHour)
	if err != nil {
		t.Fatalf("advanced summary 7h: %v", err)
	}
	summary = decodeUsageSummaryForTest(t, payload)
	if summary.TotalRequests != 0 || len(summary.APIs) != 0 || len(summary.Credentials) != 0 {
		t.Fatalf("expired 7h summary = %+v", summary)
	}

	payload, err = statistics.summaryJSON("all", now+2*statisticsHour)
	if err != nil {
		t.Fatalf("summary all: %v", err)
	}
	summary = decodeUsageSummaryForTest(t, payload)
	if summary.TotalRequests != 2 {
		t.Fatalf("all summary requests = %d, want 2", summary.TotalRequests)
	}
	if summary.RateRequests != 0 || summary.RateTokens != 0 || summary.RateMinutes != 0 {
		t.Fatalf("all rate = %+v, want disabled", summary)
	}
}

func TestUsageStatisticsGroupsDaysByServerTimeZone(t *testing.T) {
	previousLocation := time.Local
	time.Local = time.FixedZone("UTC+8", 8*60*60)
	t.Cleanup(func() { time.Local = previousLocation })

	now := time.Date(2026, 9, 2, 17, 0, 0, 0, time.UTC).Unix()
	statistics := newStatisticsForTest(now, usagePricing{})
	for _, timestamp := range []time.Time{
		time.Date(2026, 9, 1, 16, 30, 0, 0, time.UTC),
		time.Date(2026, 9, 2, 15, 30, 0, 0, time.UTC),
		time.Date(2026, 9, 2, 16, 30, 0, 0, time.UTC),
	} {
		statistics.add(Record{Timestamp: timestamp, Model: "model-a"}, now)
	}

	payload, err := statistics.summaryJSON("all", now)
	if err != nil {
		t.Fatalf("summary all: %v", err)
	}
	days := decodeUsageSummaryForTest(t, payload).Days
	if day := days["2026-09-02"]; day == nil || day.TotalRequests != 2 {
		t.Fatalf("2026-09-02 summary = %+v", day)
	}
	if day := days["2026-09-03"]; day == nil || day.TotalRequests != 1 {
		t.Fatalf("2026-09-03 summary = %+v", day)
	}
}

func TestUsageStatisticsRangeBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 34, 56, 0, time.UTC).Unix()
	cutoff := floorUnix(now-7*statisticsHour, statisticsMinute)
	statistics := newStatisticsForTest(now, usagePricing{})
	for _, record := range []Record{
		{ID: "boundary", Timestamp: time.Unix(cutoff, 0), Model: "model-a"},
		{ID: "before", Timestamp: time.Unix(cutoff-1, 0), Model: "model-a"},
		{ID: "future", Timestamp: time.Unix(now+statisticsMinute, 0), Model: "model-a"},
	} {
		statistics.add(record, now)
	}

	payload, err := statistics.summaryJSON("7h", now)
	if err != nil {
		t.Fatalf("summary 7h: %v", err)
	}
	if got := decodeUsageSummaryForTest(t, payload).TotalRequests; got != 1 {
		t.Fatalf("7h boundary requests = %d, want 1", got)
	}
	payload, err = statistics.summaryJSON("all", now)
	if err != nil {
		t.Fatalf("summary all: %v", err)
	}
	if got := decodeUsageSummaryForTest(t, payload).TotalRequests; got != 3 {
		t.Fatalf("all requests = %d, want 3", got)
	}
	payload, err = statistics.summaryJSON("7h", now+statisticsMinute)
	if err != nil {
		t.Fatalf("advanced summary 7h: %v", err)
	}
	if got := decodeUsageSummaryForTest(t, payload).TotalRequests; got != 0 {
		t.Fatalf("advanced boundary requests = %d, want 0", got)
	}
}

func TestCalculateStoredRecordCost(t *testing.T) {
	prices := map[string]usageModelPrice{
		"model-a": {
			Input:       1,
			Output:      2,
			CacheCreate: 3,
			CacheRead:   4,
			ContextTiers: []usageContextTierPrice{
				{Threshold: 100, Input: 10, Output: 20, CacheCreate: 30, CacheRead: 40},
				{Threshold: 100, Input: 99, Output: 99, CacheCreate: 99, CacheRead: 99},
			},
		},
	}
	multipliers := usageTierMultiplierIndex{"model-a\x00priority": 2}
	record := Record{
		Model:       "model-a",
		Provider:    "openai",
		ServiceTier: "Priority",
		Tokens: TokenStats{
			InputTokens:         101,
			OutputTokens:        10,
			CachedTokens:        20,
			CacheCreationTokens: 10,
		},
	}
	assertCostForTest(t, calculateStoredRecordCost(record, prices, multipliers), 0.00402)

	record.Tokens.InputTokens = 100
	assertCostForTest(t, calculateStoredRecordCost(record, prices, multipliers), 0.0004)
	record.Tokens.InputTokens = 101
	record.Provider = "anthropic"
	assertCostForTest(t, calculateStoredRecordCost(record, prices, multipliers), 0.00462)

	record.Model = "missing"
	assertCostForTest(t, calculateStoredRecordCost(record, prices, multipliers), 0)
}

func TestCredentialWindowUsesPrecomputedBuckets(t *testing.T) {
	now := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC).Unix()
	statistics := newStatisticsForTest(now, usagePricing{})
	start := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC).Unix()
	end := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC).Unix()

	records := []Record{
		{ID: "before-grace", Timestamp: time.Unix(start-2*statisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "claude-sonnet", Tokens: TokenStats{TotalTokens: 1}},
		{ID: "start-grace", Timestamp: time.Unix(start-statisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "claude-sonnet", Tokens: TokenStats{TotalTokens: 2}},
		{ID: "inside-claude", Timestamp: time.Unix(start+statisticsHour, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "claude-sonnet", Tokens: TokenStats{TotalTokens: 3}},
		{ID: "inside-gemini", Timestamp: time.Unix(start+statisticsHour+statisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "gemini-pro", Failed: true, Tokens: TokenStats{TotalTokens: 4}},
		{ID: "inside-gpt", Timestamp: time.Unix(start+statisticsHour+2*statisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "GPT-5.2", Tokens: TokenStats{TotalTokens: 8}},
		{ID: "end-grace", Timestamp: time.Unix(end+statisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "other", Tokens: TokenStats{TotalTokens: 5}},
		{ID: "after-grace", Timestamp: time.Unix(end+2*statisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "other", Tokens: TokenStats{TotalTokens: 6}},
		{ID: "other-credential", Timestamp: time.Unix(start+statisticsHour, 0), AuthIndex: "auth-2", Source: "other.json", Model: "claude-sonnet", Tokens: TokenStats{TotalTokens: 7}},
	}
	for _, record := range records {
		statistics.add(record, now)
	}

	payload, err := statistics.credentialWindowsJSON([]credentialWindowQuery{
		{ID: "all", AuthIndex: "ignored", Source: "credential.json", Category: "all", Start: start, End: end},
		{ID: "claude_gpt", AuthIndex: "ignored", Source: "credential.json", Category: "claude_gpt", Start: start, End: end},
		{ID: "gemini", AuthIndex: "ignored", Source: "credential.json", Category: "gemini", Start: start, End: end},
	}, now)
	if err != nil {
		t.Fatalf("credential windows: %v", err)
	}
	windows := decodeCredentialWindowsForTest(t, payload).Windows
	if got := windows["all"]; got.Requests != 5 || got.SuccessCount != 4 || got.FailureCount != 1 || got.Tokens != 22 {
		t.Fatalf("all window = %+v", got)
	}
	if got := windows["claude_gpt"]; got.Requests != 3 || got.SuccessCount != 3 || got.FailureCount != 0 || got.Tokens != 13 {
		t.Fatalf("claude_gpt window = %+v", got)
	}
	if got := windows["gemini"]; got.Requests != 1 || got.FailureCount != 1 || got.Tokens != 4 {
		t.Fatalf("gemini window = %+v", got)
	}
}

func TestCredentialWindowUsesCompletionTimeAndSourceIdentity(t *testing.T) {
	now := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC).Unix()
	start := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC).Unix()
	statistics := newStatisticsForTest(now, usagePricing{})
	statistics.add(Record{
		ID:        "completed-in-grace",
		Timestamp: time.Unix(start-2*statisticsMinute, 0),
		Source:    "t: source-a ",
		Model:     "claude-sonnet",
		LatencyMs: statisticsMinute * 1000,
		Tokens:    TokenStats{TotalTokens: 7},
	}, now)

	payload, err := statistics.credentialWindowsJSON([]credentialWindowQuery{{
		ID:       "source",
		Source:   "source-a",
		Category: "claude_gpt",
		Start:    start,
		End:      start,
	}}, now)
	if err != nil {
		t.Fatalf("credential windows: %v", err)
	}
	window := decodeCredentialWindowsForTest(t, payload).Windows["source"]
	if window.Requests != 1 || window.Tokens != 7 {
		t.Fatalf("source completion window = %+v", window)
	}
}

func TestCredentialStatisticsAggregateOnlyBySource(t *testing.T) {
	now := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC).Unix()
	statistics := newStatisticsForTest(now, usagePricing{})
	for _, record := range []Record{
		{Timestamp: time.Unix(now-statisticsMinute, 0), AuthIndex: "auth-1", Source: " t: credential.json ", Tokens: TokenStats{TotalTokens: 2}},
		{Timestamp: time.Unix(now-statisticsMinute, 0), AuthIndex: "auth-2", Source: "credential.json", Tokens: TokenStats{TotalTokens: 3}},
		{Timestamp: time.Unix(now-statisticsMinute, 0), AuthIndex: "auth-only", Tokens: TokenStats{TotalTokens: 5}},
	} {
		statistics.add(record, now)
	}

	payload, err := statistics.summaryJSON("all", now)
	if err != nil {
		t.Fatalf("usage summary: %v", err)
	}
	summary := decodeUsageSummaryForTest(t, payload)
	if len(summary.Credentials) != 1 {
		t.Fatalf("credential summaries = %+v", summary.Credentials)
	}
	credential := summary.Credentials["source:credential.json"]
	if credential == nil || credential.TotalRequests != 2 || credential.TotalTokens != 5 || credential.Source != "credential.json" {
		t.Fatalf("source credential summary = %+v", credential)
	}

	payload, err = statistics.credentialWindowsJSON([]credentialWindowQuery{
		{ID: "source", AuthIndex: "ignored", Source: " t: credential.json ", Start: now - statisticsMinute, End: now},
		{ID: "auth-only", AuthIndex: "auth-only", Start: now - statisticsMinute, End: now},
	}, now)
	if err != nil {
		t.Fatalf("credential windows: %v", err)
	}
	windows := decodeCredentialWindowsForTest(t, payload).Windows
	if got := windows["source"]; got.Requests != 2 || got.Tokens != 5 {
		t.Fatalf("source window = %+v", got)
	}
	if got := windows["auth-only"]; got != (credentialWindowSummary{}) {
		t.Fatalf("auth-only window = %+v", got)
	}
}

func TestQueryRecentEnforcesMaximumAndOrder(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for index := 0; index < 105; index++ {
		id := fmt.Sprintf("record-%03d", index)
		if err := store.Insert(ctx, Record{
			ID:        id,
			Timestamp: base.Add(time.Duration(index) * time.Second),
			APIKey:    "client-key",
			Model:     "model-a",
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	usage, err := store.QueryRecent(ctx, QueryRange{}, 1000)
	if err != nil {
		t.Fatalf("QueryRecent: %v", err)
	}
	details := usage["client-key"]["model-a"]
	if len(details) != 100 {
		t.Fatalf("recent details = %d, want 100", len(details))
	}
	if details[0].ID != "record-104" || details[len(details)-1].ID != "record-005" {
		t.Fatalf("recent order = %q ... %q", details[0].ID, details[len(details)-1].ID)
	}

	useGlobalSafetyStore(t, safety, store)
	response := callManagementForTest(t, managementRequest{
		Method: "GET",
		Path:   "/v0/management" + managementUsagePath,
		Query:  map[string][]string{"limit": {"1000"}},
	})
	if response.StatusCode != 200 {
		t.Fatalf("management events status = %d body=%s", response.StatusCode, response.Body)
	}
	var managedUsage APIUsage
	if err := json.Unmarshal(response.Body, &managedUsage); err != nil {
		t.Fatalf("unmarshal management events: %v", err)
	}
	if got := len(managedUsage["client-key"]["model-a"]); got != 100 {
		t.Fatalf("management events = %d, want 100", got)
	}

	body, err := json.Marshal(usageSummaryRequest{Range: "all"})
	if err != nil {
		t.Fatalf("marshal summary request: %v", err)
	}
	response = callManagementForTest(t, managementRequest{
		Method: "POST",
		Path:   "/v0/management" + managementUsagePath,
		Body:   body,
	})
	if response.StatusCode != 200 {
		t.Fatalf("management summary status = %d body=%s", response.StatusCode, response.Body)
	}
	if got := decodeUsageSummaryForTest(t, response.Body).TotalRequests; got != 105 {
		t.Fatalf("management summary requests = %d, want 105", got)
	}
}

func TestStoreStatisticsPricingDeleteAndMemoryRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	ctx := context.Background()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	for index := 0; index < 2; index++ {
		if err := store.Insert(ctx, Record{
			ID:        fmt.Sprintf("record-%d", index),
			Timestamp: now.Add(time.Duration(index) * time.Second),
			APIKey:    "client-key",
			Model:     "model-a",
			AuthIndex: "auth-1",
			Source:    "credential.json",
			Tokens:    TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
		}); err != nil {
			_ = store.Close()
			t.Fatalf("insert: %v", err)
		}
	}

	pricing := usagePricing{Prices: map[string]usageModelPrice{"model-a": {Input: 1}}}
	if _, err := store.updateUsagePricing(ctx, pricing); err != nil {
		_ = store.Close()
		t.Fatalf("update usage pricing: %v", err)
	}
	payload, err := store.usageSummaryJSON("all", now.Unix()+2)
	if err != nil {
		_ = store.Close()
		t.Fatalf("usage summary: %v", err)
	}
	summary := decodeUsageSummaryForTest(t, payload)
	if summary.TotalRequests != 2 {
		_ = store.Close()
		t.Fatalf("summary requests = %d, want 2", summary.TotalRequests)
	}
	assertCostForTest(t, summary.TotalCost, 2)

	windowPayload, err := store.credentialWindowsJSON([]credentialWindowQuery{{
		ID: "auth-1", AuthIndex: "auth-1", Source: "credential.json", Start: now.Unix() - 1, End: now.Unix() + 2,
	}}, now.Unix()+2)
	if err != nil {
		_ = store.Close()
		t.Fatalf("credential window: %v", err)
	}
	assertCostForTest(t, decodeCredentialWindowsForTest(t, windowPayload).Windows["auth-1"].Cost, 2)

	ids := NewStableStringSet(1)
	ids.Add("record-0")
	if result, err := store.Delete(ctx, ids); err != nil || result.Deleted != 1 {
		_ = store.Close()
		t.Fatalf("delete result = %+v, err=%v", result, err)
	}
	payload, err = store.usageSummaryJSON("all", now.Unix()+2)
	if err != nil {
		_ = store.Close()
		t.Fatalf("summary after delete: %v", err)
	}
	summary = decodeUsageSummaryForTest(t, payload)
	if summary.TotalRequests != 1 {
		_ = store.Close()
		t.Fatalf("summary after delete requests = %d, want 1", summary.TotalRequests)
	}
	assertCostForTest(t, summary.TotalCost, 1)

	if err := store.db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	payload, err = store.usageSummaryJSON("all", now.Unix()+2)
	if err != nil {
		t.Fatalf("in-memory summary after database close: %v", err)
	}
	if got := decodeUsageSummaryForTest(t, payload).TotalRequests; got != 1 {
		t.Fatalf("in-memory requests = %d, want 1", got)
	}
}

func TestDeleteBeforeSubtractsPrecomputedStatistics(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.updateUsagePricing(ctx, usagePricing{
		Prices: map[string]usageModelPrice{"model-a": {Input: 1}},
	}); err != nil {
		t.Fatalf("set pricing: %v", err)
	}

	minute := time.Now().Add(-2 * time.Hour).Truncate(time.Minute)
	for _, record := range []Record{
		{
			ID:        "deleted",
			Timestamp: minute.Add(10 * time.Second),
			APIKey:    "client-key",
			Model:     "model-a",
			AuthIndex: "auth-1",
			Source:    "credential.json",
			Failed:    true,
			Tokens:    TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
		},
		{
			ID:        "retained",
			Timestamp: minute.Add(50 * time.Second),
			APIKey:    "client-key",
			Model:     "model-a",
			AuthIndex: "auth-1",
			Source:    "credential.json",
			Tokens:    TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
		},
	} {
		if err := store.Insert(ctx, record); err != nil {
			t.Fatalf("insert %s: %v", record.ID, err)
		}
	}

	deleted, err := store.DeleteBefore(ctx, minute.Add(30*time.Second))
	if err != nil {
		t.Fatalf("delete before: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}

	now := time.Now().Unix()
	for _, rangeName := range []string{"all", "7h", "24h", "7d", "30d"} {
		payload, err := store.usageSummaryJSON(rangeName, now)
		if err != nil {
			t.Fatalf("%s summary: %v", rangeName, err)
		}
		summary := decodeUsageSummaryForTest(t, payload)
		if summary.TotalRequests != 1 || summary.SuccessCount != 1 || summary.FailureCount != 0 {
			t.Fatalf("%s summary = %+v", rangeName, summary)
		}
		assertCostForTest(t, summary.TotalCost, 1)
		api := summary.APIs["client-key"]
		if api == nil || api.TotalRequests != 1 || api.Models["model-a"].TotalRequests != 1 {
			t.Fatalf("%s API summary = %+v", rangeName, api)
		}
		if credential := summary.Credentials["source:credential.json"]; credential == nil || credential.TotalRequests != 1 {
			t.Fatalf("%s credential summary = %+v", rangeName, credential)
		}
		if summary.Hours[floorUnix(minute.Unix(), statisticsHour)].TotalRequests != 1 {
			t.Fatalf("%s hour summary = %+v", rangeName, summary.Hours)
		}
		if summary.Days[serverUsageDay(minute.Unix()).String()].TotalRequests != 1 {
			t.Fatalf("%s day summary = %+v", rangeName, summary.Days)
		}
	}

	windowPayload, err := store.credentialWindowsJSON([]credentialWindowQuery{{
		ID:        "auth-1",
		AuthIndex: "auth-1",
		Source:    "credential.json",
		Start:     minute.Unix(),
		End:       minute.Add(time.Minute).Unix(),
	}}, now)
	if err != nil {
		t.Fatalf("credential window: %v", err)
	}
	window := decodeCredentialWindowsForTest(t, windowPayload).Windows["auth-1"]
	if window.Requests != 1 || window.SuccessCount != 1 || window.FailureCount != 0 {
		t.Fatalf("credential window = %+v", window)
	}
	assertCostForTest(t, window.Cost, 1)

	payload, err := store.usageSummaryJSON("7h", now+8*statisticsHour)
	if err != nil {
		t.Fatalf("advanced 7h summary: %v", err)
	}
	if summary := decodeUsageSummaryForTest(t, payload); summary.TotalRequests != 0 || len(summary.APIs) != 0 {
		t.Fatalf("advanced 7h summary = %+v", summary)
	}
	payload, err = store.usageSummaryJSON("all", now+8*statisticsHour)
	if err != nil {
		t.Fatalf("advanced all summary: %v", err)
	}
	if summary := decodeUsageSummaryForTest(t, payload); summary.TotalRequests != 1 {
		t.Fatalf("advanced all summary = %+v", summary)
	}
}

func TestDeleteBeforeSubtractsRecordsOutsideBucketHistory(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := store.Insert(ctx, Record{
		ID:        "old",
		Timestamp: now.Add(-40 * 24 * time.Hour),
		APIKey:    "client-key",
		Model:     "model-a",
		Tokens:    TokenStats{TotalTokens: 10},
	}); err != nil {
		t.Fatalf("insert old record: %v", err)
	}
	if deleted, err := store.DeleteBefore(ctx, now.Add(-35*24*time.Hour)); err != nil || deleted != 1 {
		t.Fatalf("deleted = %d, err=%v", deleted, err)
	}
	payload, err := store.usageSummaryJSON("all", now.Unix())
	if err != nil {
		t.Fatalf("all summary: %v", err)
	}
	summary := decodeUsageSummaryForTest(t, payload)
	if summary.TotalRequests != 0 || len(summary.APIs) != 0 || len(summary.Hours) != 0 || len(summary.Days) != 0 {
		t.Fatalf("all summary after old deletion = %+v", summary)
	}
}

func TestDeleteBeforeMatchesFreshStatisticsRebuild(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pricing := usagePricing{Prices: map[string]usageModelPrice{
		"model-0": {Input: 1},
		"model-1": {Input: 2},
		"model-2": {Input: 3},
		"model-3": {Input: 4},
		"model-4": {Input: 5},
	}}
	if _, err := store.updateUsagePricing(ctx, pricing); err != nil {
		t.Fatalf("set pricing: %v", err)
	}

	now := time.Now().Truncate(time.Minute)
	base := now.Add(-40 * 24 * time.Hour)
	for index := 0; index < 160; index++ {
		if err := store.Insert(ctx, Record{
			ID:        fmt.Sprintf("record-%03d", index),
			Timestamp: base.Add(time.Duration(index) * 6 * time.Hour),
			APIKey:    fmt.Sprintf("key-%d", index%3),
			Provider:  "openai",
			Model:     fmt.Sprintf("model-%d", index%5),
			AuthIndex: fmt.Sprintf("auth-%d", index%2),
			Source:    fmt.Sprintf("credential-%d.json", index%2),
			AuthType:  "oauth",
			LatencyMs: 1000,
			Failed:    index%7 == 0,
			Tokens:    TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
		}); err != nil {
			t.Fatalf("insert record %d: %v", index, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO api_key_safety_history (
    api_key, occurred_at, usage_record_id, blocked_until, manual_released_at
) VALUES (?, ?, ?, 0, 0)
`, "key-1", base.Add(10*6*time.Hour).Unix(), "record-010"); err != nil {
		t.Fatalf("protect usage record: %v", err)
	}

	deleted, err := store.DeleteBefore(ctx, now.Add(-15*24*time.Hour))
	if err != nil {
		t.Fatalf("delete before: %v", err)
	}
	if deleted != 99 {
		t.Fatalf("deleted = %d, want 99", deleted)
	}

	queryTime := now.Unix()
	before := make(map[string]string)
	for _, rangeName := range []string{"all", "7h", "24h", "7d", "30d"} {
		payload, err := store.usageSummaryJSON(rangeName, queryTime)
		if err != nil {
			t.Fatalf("incremental %s summary: %v", rangeName, err)
		}
		before[rangeName] = string(payload)
	}
	queries := []credentialWindowQuery{
		{ID: "auth-0", AuthIndex: "auth-0", Source: "credential-0.json", Start: now.Add(-31 * 24 * time.Hour).Unix(), End: queryTime},
		{ID: "auth-1", AuthIndex: "auth-1", Source: "credential-1.json", Start: now.Add(-7 * 24 * time.Hour).Unix(), End: queryTime},
	}
	windowBefore, err := store.credentialWindowsJSON(queries, queryTime)
	if err != nil {
		t.Fatalf("incremental credential windows: %v", err)
	}

	if err := store.statistics.rebuild(ctx, store.db, store.statistics.pricing()); err != nil {
		t.Fatalf("fresh statistics rebuild: %v", err)
	}
	for _, rangeName := range []string{"all", "7h", "24h", "7d", "30d"} {
		payload, err := store.usageSummaryJSON(rangeName, queryTime)
		if err != nil {
			t.Fatalf("rebuilt %s summary: %v", rangeName, err)
		}
		if string(payload) != before[rangeName] {
			t.Fatalf("%s summary differs after fresh rebuild\nincremental: %s\nrebuilt: %s", rangeName, before[rangeName], payload)
		}
	}
	windowAfter, err := store.credentialWindowsJSON(queries, queryTime)
	if err != nil {
		t.Fatalf("rebuilt credential windows: %v", err)
	}
	if string(windowAfter) != string(windowBefore) {
		t.Fatalf("credential windows differ after fresh rebuild\nincremental: %s\nrebuilt: %s", windowBefore, windowAfter)
	}
}

func TestStatisticsReusesDeletedDimensionIDs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	for _, record := range []Record{
		{ID: "first", Timestamp: now, APIKey: "key-first", Model: "model-first", AuthIndex: "auth-first", Source: "source-first"},
		{ID: "retained", Timestamp: now, APIKey: "key-retained", Model: "model-retained", AuthIndex: "auth-retained", Source: "source-retained"},
	} {
		if err := store.Insert(ctx, record); err != nil {
			t.Fatalf("insert %s: %v", record.ID, err)
		}
	}
	state := &store.statistics.state
	allocated := len(state.Dimensions.Values)
	if allocated != 6 {
		t.Fatalf("allocated dimensions = %d, want 6", allocated)
	}

	ids := NewStableStringSet(1)
	ids.Add("first")
	if result, err := store.Delete(ctx, ids); err != nil || result.Deleted != 1 {
		t.Fatalf("delete first record = %+v, err=%v", result, err)
	}
	if len(state.Dimensions.IDs) != 3 || len(state.Dimensions.Free) != 3 {
		t.Fatalf("dimensions after delete = active %d, free %d", len(state.Dimensions.IDs), len(state.Dimensions.Free))
	}

	if err := store.Insert(ctx, Record{
		ID:        "second",
		Timestamp: now,
		APIKey:    "key-second",
		Model:     "model-second",
		AuthIndex: "auth-second",
		Source:    "source-second",
	}); err != nil {
		t.Fatalf("insert second record: %v", err)
	}
	if got := len(state.Dimensions.Values); got != allocated {
		t.Fatalf("dimension slots after reuse = %d, want %d", got, allocated)
	}

	ids = NewStableStringSet(2)
	ids.Add("retained")
	ids.Add("second")
	if result, err := store.Delete(ctx, ids); err != nil || result.Deleted != 2 {
		t.Fatalf("delete remaining records = %+v, err=%v", result, err)
	}
	if len(state.Dimensions.IDs) != 0 || len(state.Dimensions.Values) != 0 ||
		len(state.Dimensions.References) != 0 || len(state.Dimensions.Free) != 0 ||
		len(state.CredentialIdentities) != 0 {
		t.Fatalf("retained empty dimension storage = %+v", state.Dimensions)
	}
}

func TestUsagePricingPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	ctx := context.Background()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := store.Insert(ctx, Record{
		ID:          "priced-record",
		Timestamp:   now,
		Model:       "model-a",
		ServiceTier: "priority",
		Tokens:      TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	}); err != nil {
		_ = store.Close()
		t.Fatalf("insert: %v", err)
	}
	pricing := usagePricing{
		Prices: map[string]usageModelPrice{
			"model-a": {
				Input: 1,
				ContextTiers: []usageContextTierPrice{
					{Threshold: 500_000, Input: 2},
				},
			},
		},
		TierMultipliers: []usageTierMultiplier{
			{Model: "model-a", Tier: "priority", Multiplier: 3},
		},
	}
	updated, err := store.updateUsagePricing(ctx, pricing)
	if err != nil {
		_ = store.Close()
		t.Fatalf("update pricing: %v", err)
	}
	_, wantKey := normalizeUsagePricing(pricing)
	_, updatedKey := normalizeUsagePricing(updated)
	if updatedKey != wantKey {
		_ = store.Close()
		t.Fatalf("updated pricing = %+v", updated)
	}
	var storageType string
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT typeof(config) FROM usage_pricing WHERE name = ?",
		usagePricingRowName,
	).Scan(&storageType); err != nil {
		_ = store.Close()
		t.Fatalf("query stored pricing: %v", err)
	}
	if storageType != "blob" {
		_ = store.Close()
		t.Fatalf("pricing storage type = %q, want blob", storageType)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	_, reopenedKey := normalizeUsagePricing(reopened.usagePricing())
	if reopenedKey != wantKey {
		t.Fatalf("reopened pricing = %+v", reopened.usagePricing())
	}
	payload, err := reopened.usageSummaryJSON("all", now.Unix()+1)
	if err != nil {
		t.Fatalf("summary after reopen: %v", err)
	}
	assertCostForTest(t, decodeUsageSummaryForTest(t, payload).TotalCost, 6)
}

func TestUsagePricingSaveFailureKeepsCurrentStatistics(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Record{
		ID:        "record",
		Timestamp: time.Now().Add(-time.Minute),
		Model:     "model-a",
		Tokens:    TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER reject_usage_pricing
BEFORE INSERT ON usage_pricing
BEGIN
	SELECT RAISE(FAIL, 'pricing rejected');
END
`); err != nil {
		t.Fatalf("create pricing trigger: %v", err)
	}

	if _, err := store.updateUsagePricing(ctx, usagePricing{
		Prices: map[string]usageModelPrice{"model-a": {Input: 2}},
	}); err == nil {
		t.Fatal("pricing update unexpectedly succeeded")
	}
	payload, err := store.usageSummaryJSON("all", time.Now().Unix())
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	assertCostForTest(t, decodeUsageSummaryForTest(t, payload).TotalCost, 0)
	_, emptyKey := normalizeUsagePricing(usagePricing{})
	if store.statistics.pricingKey() != emptyKey {
		t.Fatalf("active pricing changed after failed save: %+v", store.usagePricing())
	}
}

func TestUsagePricingManagementRoutes(t *testing.T) {
	var methods []string
	for _, route := range managementRegistration().Routes {
		if route.Path == managementPricingPath {
			methods = append(methods, route.Method)
		}
	}
	if len(methods) != 2 || methods[0] != "GET" || methods[1] != "PUT" {
		t.Fatalf("pricing methods = %v", methods)
	}

	store := newTestStore(t)
	if err := store.Insert(t.Context(), Record{
		ID:        "record",
		Timestamp: time.Now().Add(-time.Minute),
		Model:     "model-a",
		Tokens:    TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	useGlobalSafetyStore(t, &safetyLimiter{states: make(map[string]SafetyState)}, store)
	pricing := usagePricing{Prices: map[string]usageModelPrice{"model-a": {Input: 2}}}
	body, err := json.Marshal(pricing)
	if err != nil {
		t.Fatalf("marshal pricing: %v", err)
	}
	response := callManagementForTest(t, managementRequest{
		Method: "PUT",
		Path:   "/v0/management" + managementPricingPath,
		Body:   body,
	})
	if response.StatusCode != 200 {
		t.Fatalf("pricing PUT status = %d body=%s", response.StatusCode, response.Body)
	}
	var updated usagePricing
	if err := json.Unmarshal(response.Body, &updated); err != nil {
		t.Fatalf("unmarshal pricing PUT: %v", err)
	}
	_, wantKey := normalizeUsagePricing(pricing)
	_, updatedKey := normalizeUsagePricing(updated)
	if updatedKey != wantKey {
		t.Fatalf("pricing PUT response = %+v", updated)
	}

	response = callManagementForTest(t, managementRequest{
		Method: "GET",
		Path:   "/v0/management" + managementPricingPath,
	})
	if response.StatusCode != 200 {
		t.Fatalf("pricing GET status = %d body=%s", response.StatusCode, response.Body)
	}
	var loaded usagePricing
	if err := json.Unmarshal(response.Body, &loaded); err != nil {
		t.Fatalf("unmarshal pricing GET: %v", err)
	}
	_, loadedKey := normalizeUsagePricing(loaded)
	if loadedKey != wantKey {
		t.Fatalf("pricing GET response = %+v", loaded)
	}

	summaryBody, err := json.Marshal(usageSummaryRequest{Range: "all"})
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	response = callManagementForTest(t, managementRequest{
		Method: "POST",
		Path:   "/v0/management" + managementUsagePath,
		Body:   summaryBody,
	})
	if response.StatusCode != 200 {
		t.Fatalf("summary status = %d body=%s", response.StatusCode, response.Body)
	}
	assertCostForTest(t, decodeUsageSummaryForTest(t, response.Body).TotalCost, 2)
}

func TestStoreConcurrentInsertAndStatisticsRebuild(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	prices := []usagePricing{
		{Prices: map[string]usageModelPrice{"model-a": {Input: 1}}},
		{Prices: map[string]usageModelPrice{"model-a": {Input: 2}}},
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		for index := 0; index < 40; index++ {
			if err := store.Insert(ctx, Record{
				ID:        fmt.Sprintf("record-%d", index),
				Timestamp: now,
				APIKey:    "client-key",
				Model:     "model-a",
				AuthIndex: "auth-1",
				Source:    "credential.json",
				Tokens:    TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
			}); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wait.Done()
		<-start
		for index := 0; index < 12; index++ {
			if _, err := store.updateUsagePricing(ctx, prices[index%len(prices)]); err != nil {
				errs <- err
				return
			}
		}
	}()
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent operation: %v", err)
	}

	var stored int64
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_records").Scan(&stored); err != nil {
		t.Fatalf("count stored records: %v", err)
	}
	payload, err := store.statistics.summaryJSON("all", time.Now().Unix())
	if err != nil {
		t.Fatalf("read in-memory summary: %v", err)
	}
	summary := decodeUsageSummaryForTest(t, payload)
	if summary.TotalRequests != stored || stored != 40 {
		t.Fatalf("stored=%d summarized=%d, want 40", stored, summary.TotalRequests)
	}
	assertCostForTest(t, summary.TotalCost, 80)
}

func TestStoreCoordinatesDeleteWithPricingRebuild(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	ids := NewStableStringSet(10)
	for index := 0; index < 20; index++ {
		id := fmt.Sprintf("record-%d", index)
		if index < 10 {
			ids.Add(id)
		}
		if err := store.Insert(ctx, Record{
			ID:        id,
			Timestamp: now,
			Model:     "model-a",
			Tokens:    TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	if _, err := store.updateUsagePricing(ctx, usagePricing{
		Prices: map[string]usageModelPrice{"model-a": {Input: 1}},
	}); err != nil {
		t.Fatalf("set initial pricing: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, err := store.updateUsagePricing(ctx, usagePricing{
			Prices: map[string]usageModelPrice{"model-a": {Input: 2}},
		})
		errs <- err
	}()
	go func() {
		defer wait.Done()
		<-start
		result, err := store.Delete(ctx, ids)
		if err == nil && result.Deleted != 10 {
			err = fmt.Errorf("deleted = %d, want 10", result.Deleted)
		}
		errs <- err
	}()
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation: %v", err)
		}
	}

	payload, err := store.usageSummaryJSON("all", time.Now().Unix())
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	summary := decodeUsageSummaryForTest(t, payload)
	if summary.TotalRequests != 10 {
		t.Fatalf("summary requests = %d, want 10", summary.TotalRequests)
	}
	assertCostForTest(t, summary.TotalCost, 20)
}

func TestUsageStatisticsConcurrentReadWrite(t *testing.T) {
	now := time.Now().Unix()
	statistics := newStatisticsForTest(now, usagePricing{})
	var wait sync.WaitGroup

	wait.Add(1)
	go func() {
		defer wait.Done()
		for index := 0; index < 500; index++ {
			statistics.add(Record{
				Timestamp: time.Unix(now, 0),
				APIKey:    "client-key",
				Model:     "model-a",
				AuthIndex: "auth-1",
				Source:    "credential.json",
			}, now)
		}
	}()

	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 100 {
				if _, err := statistics.summaryJSON("24h", now); err != nil {
					t.Errorf("summary: %v", err)
					return
				}
				if _, err := statistics.credentialWindowsJSON(nil, now); err != nil {
					t.Errorf("credential windows: %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()

	payload, err := statistics.summaryJSON("all", now)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeUsageSummaryForTest(t, payload).TotalRequests; got != 500 {
		t.Fatalf("total requests = %d, want 500", got)
	}
}

func BenchmarkUsageStatisticsAdd(b *testing.B) {
	now := time.Now().Unix()
	statistics := newStatisticsForTest(now, usagePricing{})
	for minute := floorUnix(now-credentialHistoryRange, statisticsMinute); minute <= now; minute += statisticsMinute {
		statistics.state.MinuteBuckets[minute] = nil
		statistics.state.CredentialMinutes[minute] = nil
	}
	for hour := floorUnix(now-credentialHistoryRange, statisticsHour); hour <= now; hour += statisticsHour {
		statistics.state.CredentialHours[hour] = nil
	}
	record := Record{
		Timestamp: time.Unix(now, 0),
		APIKey:    "client-key",
		Model:     "model-a",
		AuthIndex: "auth-1",
		Source:    "credential.json",
		Tokens:    TokenStats{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	}
	b.ReportAllocs()
	for range b.N {
		statistics.add(record, now)
	}
}

func BenchmarkUsageSummaryJSON(b *testing.B) {
	now := time.Now().Unix()
	statistics := newStatisticsForTest(now, usagePricing{})
	for index := 0; index < 1000; index++ {
		statistics.add(Record{
			ID:        fmt.Sprintf("record-%d", index),
			Timestamp: time.Unix(now-int64(index%3600), 0),
			APIKey:    fmt.Sprintf("key-%d", index%100),
			Model:     fmt.Sprintf("model-%d", index%20),
			AuthIndex: fmt.Sprintf("auth-%d", index%100),
			Source:    fmt.Sprintf("credential-%d.json", index%100),
			Tokens:    TokenStats{TotalTokens: 100},
		}, now)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := statistics.summaryJSON("24h", now); err != nil {
			b.Fatal(err)
		}
	}
}
