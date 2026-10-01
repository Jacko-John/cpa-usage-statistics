package statistics

import (
	"fmt"
	"sync"
	"testing"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testutil"
)

func newStatisticsForTest(now int64, pricing Pricing) *Engine {
	pricing, pricingKey := NormalizePricing(pricing)
	return &Engine{state: NewAccumulator(now, pricing, pricingKey)}
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

func TestUsageStatisticsIncrementalRanges(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC).Unix()
	statistics := newStatisticsForTest(now, Pricing{})
	statistics.Add(usagemodel.Record{
		ID:        "recent",
		Timestamp: time.Unix(now-6*StatisticsHour, 0),
		APIKey:    "client-key",
		Model:     "model-a",
		AuthIndex: "auth-1",
		Source:    "credential.json",
		LatencyMs: 1000,
		TTFTMs:    200,
		Tokens:    usagemodel.TokenStats{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	}, now)
	statistics.Add(usagemodel.Record{
		ID:        "older",
		Timestamp: time.Unix(now-8*StatisticsHour, 0),
		APIKey:    "client-key",
		Model:     "model-a",
		AuthIndex: "auth-1",
		Source:    "credential.json",
		Failed:    true,
		Tokens:    usagemodel.TokenStats{TotalTokens: 25},
	}, now)

	payload, err := statistics.SummaryJSON("7h", now)
	if err != nil {
		t.Fatalf("summary 7h: %v", err)
	}
	summary := testutil.DecodeUsageSummary(t, payload)
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

	payload, err = statistics.SummaryJSON("24h", now)
	if err != nil {
		t.Fatalf("summary 24h: %v", err)
	}
	summary = testutil.DecodeUsageSummary(t, payload)
	if summary.TotalRequests != 2 || summary.SuccessCount != 1 || summary.FailureCount != 1 {
		t.Fatalf("24h summary = %+v", summary)
	}

	payload, err = statistics.SummaryJSON("7h", now+2*StatisticsHour)
	if err != nil {
		t.Fatalf("advanced summary 7h: %v", err)
	}
	summary = testutil.DecodeUsageSummary(t, payload)
	if summary.TotalRequests != 0 || len(summary.APIs) != 0 || len(summary.Credentials) != 0 {
		t.Fatalf("expired 7h summary = %+v", summary)
	}

	payload, err = statistics.SummaryJSON("all", now+2*StatisticsHour)
	if err != nil {
		t.Fatalf("summary all: %v", err)
	}
	summary = testutil.DecodeUsageSummary(t, payload)
	if summary.TotalRequests != 2 {
		t.Fatalf("all summary requests = %d, want 2", summary.TotalRequests)
	}
	if summary.RateRequests != 0 || summary.RateTokens != 0 || summary.RateMinutes != 0 {
		t.Fatalf("all rate = %+v, want disabled", summary)
	}
}

func TestUsageAggregateTracksValidTimingSamples(t *testing.T) {
	var metric UsageAggregate
	for _, record := range []Entry{
		{LatencyMs: 1000, TTFTMs: 200, OutputTokens: 10},
		{LatencyMs: 2200, TTFTMs: 200, OutputTokens: 100},
		{LatencyMs: 2, OutputTokens: 1000, Failed: true},
		{LatencyMs: 1000, TTFTMs: 100},
	} {
		metric.addRecord(record, 0)
	}

	if metric.FirstByteLatencyTotalMs != 500 || metric.FirstByteLatencySampleCount != 3 {
		t.Fatalf("first-byte aggregate = %+v", metric)
	}
	if metric.TPSOutputTokens != 110 || metric.TPSGenerationDurationMs != 2800 || metric.TPSSampleCount != 2 {
		t.Fatalf("TPS aggregate = %+v", metric)
	}
}

func TestUsageStatisticsTimingRangeSubtractsExpiredBuckets(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC).Unix()
	statistics := newStatisticsForTest(now, Pricing{})
	for _, record := range []usagemodel.Record{
		{
			ID:        "expired",
			Timestamp: time.Unix(now-6*StatisticsHour, 0),
			APIKey:    "client-key",
			Model:     "model-a",
			LatencyMs: 1000,
			TTFTMs:    200,
			Tokens:    usagemodel.TokenStats{OutputTokens: 10},
		},
		{
			ID:        "retained",
			Timestamp: time.Unix(now-4*StatisticsHour, 0),
			APIKey:    "client-key",
			Model:     "model-a",
			LatencyMs: 2200,
			TTFTMs:    200,
			Tokens:    usagemodel.TokenStats{OutputTokens: 100},
		},
	} {
		statistics.Add(record, now)
	}

	payload, err := statistics.SummaryJSON("7h", now)
	if err != nil {
		t.Fatalf("initial summary: %v", err)
	}
	api := testutil.DecodeUsageSummary(t, payload).APIs["client-key"]
	if api == nil || api.TPSOutputTokens != 110 || api.TPSGenerationDurationMs != 2800 ||
		api.FirstByteLatencyTotalMs != 400 || api.FirstByteLatencySampleCount != 2 {
		t.Fatalf("initial timing aggregate = %+v", api)
	}

	payload, err = statistics.SummaryJSON("7h", now+2*StatisticsHour)
	if err != nil {
		t.Fatalf("advanced summary: %v", err)
	}
	api = testutil.DecodeUsageSummary(t, payload).APIs["client-key"]
	if api == nil || api.TPSOutputTokens != 100 || api.TPSGenerationDurationMs != 2000 ||
		api.FirstByteLatencyTotalMs != 200 || api.FirstByteLatencySampleCount != 1 {
		t.Fatalf("advanced timing aggregate = %+v", api)
	}
}

func TestUsageStatisticsGroupsDaysByServerTimeZone(t *testing.T) {
	previousLocation := time.Local
	time.Local = time.FixedZone("UTC+8", 8*60*60)
	t.Cleanup(func() { time.Local = previousLocation })

	now := time.Date(2026, 9, 2, 17, 0, 0, 0, time.UTC).Unix()
	statistics := newStatisticsForTest(now, Pricing{})
	for _, timestamp := range []time.Time{
		time.Date(2026, 9, 1, 16, 30, 0, 0, time.UTC),
		time.Date(2026, 9, 2, 15, 30, 0, 0, time.UTC),
		time.Date(2026, 9, 2, 16, 30, 0, 0, time.UTC),
	} {
		statistics.Add(usagemodel.Record{Timestamp: timestamp, Model: "model-a"}, now)
	}

	payload, err := statistics.SummaryJSON("all", now)
	if err != nil {
		t.Fatalf("summary all: %v", err)
	}
	days := testutil.DecodeUsageSummary(t, payload).Days
	if day := days["2026-09-02"]; day == nil || day.TotalRequests != 2 {
		t.Fatalf("2026-09-02 summary = %+v", day)
	}
	if day := days["2026-09-03"]; day == nil || day.TotalRequests != 1 {
		t.Fatalf("2026-09-03 summary = %+v", day)
	}
}

func TestUsageStatisticsRangeBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 34, 56, 0, time.UTC).Unix()
	cutoff := FloorUnix(now-7*StatisticsHour, StatisticsMinute)
	statistics := newStatisticsForTest(now, Pricing{})
	for _, record := range []usagemodel.Record{
		{ID: "boundary", Timestamp: time.Unix(cutoff, 0), Model: "model-a"},
		{ID: "before", Timestamp: time.Unix(cutoff-1, 0), Model: "model-a"},
		{ID: "future", Timestamp: time.Unix(now+StatisticsMinute, 0), Model: "model-a"},
	} {
		statistics.Add(record, now)
	}

	payload, err := statistics.SummaryJSON("7h", now)
	if err != nil {
		t.Fatalf("summary 7h: %v", err)
	}
	if got := testutil.DecodeUsageSummary(t, payload).TotalRequests; got != 1 {
		t.Fatalf("7h boundary requests = %d, want 1", got)
	}
	payload, err = statistics.SummaryJSON("all", now)
	if err != nil {
		t.Fatalf("summary all: %v", err)
	}
	if got := testutil.DecodeUsageSummary(t, payload).TotalRequests; got != 3 {
		t.Fatalf("all requests = %d, want 3", got)
	}
	payload, err = statistics.SummaryJSON("7h", now+StatisticsMinute)
	if err != nil {
		t.Fatalf("advanced summary 7h: %v", err)
	}
	if got := testutil.DecodeUsageSummary(t, payload).TotalRequests; got != 0 {
		t.Fatalf("advanced boundary requests = %d, want 0", got)
	}
}

func TestCalculateStoredRecordCost(t *testing.T) {
	prices := map[string]ModelPrice{
		"model-a": {
			Input:       1,
			Output:      2,
			CacheCreate: 3,
			CacheRead:   4,
			ContextTiers: []ContextTierPrice{
				{Threshold: 100, Input: 10, Output: 20, CacheCreate: 30, CacheRead: 40},
				{Threshold: 100, Input: 99, Output: 99, CacheCreate: 99, CacheRead: 99},
			},
		},
	}
	multipliers := TierMultiplierIndex{"model-a\x00priority": 2}
	record := usagemodel.Record{
		Model:       "model-a",
		Provider:    "openai",
		ServiceTier: "Priority",
		Tokens: usagemodel.TokenStats{
			InputTokens:         101,
			OutputTokens:        10,
			CachedTokens:        20,
			CacheCreationTokens: 10,
		},
	}
	testutil.AssertCost(t, CalculateStoredRecordCost(record, prices, multipliers), 0.00402)

	record.Tokens.InputTokens = 100
	testutil.AssertCost(t, CalculateStoredRecordCost(record, prices, multipliers), 0.0004)
	record.Tokens.InputTokens = 101
	record.Provider = "anthropic"
	testutil.AssertCost(t, CalculateStoredRecordCost(record, prices, multipliers), 0.00462)

	record.Model = "missing"
	testutil.AssertCost(t, CalculateStoredRecordCost(record, prices, multipliers), 0)
}

func TestCredentialWindowUsesPrecomputedBuckets(t *testing.T) {
	now := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC).Unix()
	statistics := newStatisticsForTest(now, Pricing{})
	start := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC).Unix()
	end := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC).Unix()

	records := []usagemodel.Record{
		{ID: "before-grace", Timestamp: time.Unix(start-2*StatisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "claude-sonnet", Tokens: usagemodel.TokenStats{TotalTokens: 1}},
		{ID: "start-grace", Timestamp: time.Unix(start-StatisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "claude-sonnet", Tokens: usagemodel.TokenStats{TotalTokens: 2}},
		{ID: "inside-claude", Timestamp: time.Unix(start+StatisticsHour, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "claude-sonnet", Tokens: usagemodel.TokenStats{TotalTokens: 3}},
		{ID: "inside-gemini", Timestamp: time.Unix(start+StatisticsHour+StatisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "gemini-pro", Failed: true, Tokens: usagemodel.TokenStats{TotalTokens: 4}},
		{ID: "inside-gpt", Timestamp: time.Unix(start+StatisticsHour+2*StatisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "GPT-5.2", Tokens: usagemodel.TokenStats{TotalTokens: 8}},
		{ID: "end-grace", Timestamp: time.Unix(end+StatisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "other", Tokens: usagemodel.TokenStats{TotalTokens: 5}},
		{ID: "after-grace", Timestamp: time.Unix(end+2*StatisticsMinute, 0), AuthIndex: "auth-1", Source: "credential.json", Model: "other", Tokens: usagemodel.TokenStats{TotalTokens: 6}},
		{ID: "other-credential", Timestamp: time.Unix(start+StatisticsHour, 0), AuthIndex: "auth-2", Source: "other.json", Model: "claude-sonnet", Tokens: usagemodel.TokenStats{TotalTokens: 7}},
	}
	for _, record := range records {
		statistics.Add(record, now)
	}

	payload, err := statistics.CredentialWindowsJSON([]CredentialWindowQuery{
		{ID: "all", AuthIndex: "ignored", Source: "credential.json", Category: "all", Start: start, End: end},
		{ID: "claude_gpt", AuthIndex: "ignored", Source: "credential.json", Category: "claude_gpt", Start: start, End: end},
		{ID: "gemini", AuthIndex: "ignored", Source: "credential.json", Category: "gemini", Start: start, End: end},
	}, now)
	if err != nil {
		t.Fatalf("credential windows: %v", err)
	}
	windows := testutil.DecodeCredentialWindows(t, payload).Windows
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
	statistics := newStatisticsForTest(now, Pricing{})
	statistics.Add(usagemodel.Record{
		ID:        "completed-in-grace",
		Timestamp: time.Unix(start-2*StatisticsMinute, 0),
		Source:    "t: source-a ",
		Model:     "claude-sonnet",
		LatencyMs: StatisticsMinute * 1000,
		Tokens:    usagemodel.TokenStats{TotalTokens: 7},
	}, now)

	payload, err := statistics.CredentialWindowsJSON([]CredentialWindowQuery{{
		ID:       "source",
		Source:   "source-a",
		Category: "claude_gpt",
		Start:    start,
		End:      start,
	}}, now)
	if err != nil {
		t.Fatalf("credential windows: %v", err)
	}
	window := testutil.DecodeCredentialWindows(t, payload).Windows["source"]
	if window.Requests != 1 || window.Tokens != 7 {
		t.Fatalf("source completion window = %+v", window)
	}
}

func TestCredentialStatisticsAggregateOnlyBySource(t *testing.T) {
	now := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC).Unix()
	statistics := newStatisticsForTest(now, Pricing{})
	for _, record := range []usagemodel.Record{
		{Timestamp: time.Unix(now-StatisticsMinute, 0), AuthIndex: "auth-1", Source: " t: credential.json ", Tokens: usagemodel.TokenStats{TotalTokens: 2}},
		{Timestamp: time.Unix(now-StatisticsMinute, 0), AuthIndex: "auth-2", Source: "credential.json", Tokens: usagemodel.TokenStats{TotalTokens: 3}},
		{Timestamp: time.Unix(now-StatisticsMinute, 0), AuthIndex: "auth-only", Tokens: usagemodel.TokenStats{TotalTokens: 5}},
	} {
		statistics.Add(record, now)
	}

	payload, err := statistics.SummaryJSON("all", now)
	if err != nil {
		t.Fatalf("usage summary: %v", err)
	}
	summary := testutil.DecodeUsageSummary(t, payload)
	if len(summary.Credentials) != 1 {
		t.Fatalf("credential summaries = %+v", summary.Credentials)
	}
	credential := summary.Credentials["source:credential.json"]
	if credential == nil || credential.TotalRequests != 2 || credential.TotalTokens != 5 || credential.Source != "credential.json" {
		t.Fatalf("source credential summary = %+v", credential)
	}

	payload, err = statistics.CredentialWindowsJSON([]CredentialWindowQuery{
		{ID: "source", AuthIndex: "ignored", Source: " t: credential.json ", Start: now - StatisticsMinute, End: now},
		{ID: "auth-only", AuthIndex: "auth-only", Start: now - StatisticsMinute, End: now},
	}, now)
	if err != nil {
		t.Fatalf("credential windows: %v", err)
	}
	windows := testutil.DecodeCredentialWindows(t, payload).Windows
	if got := windows["source"]; got.Requests != 2 || got.Tokens != 5 {
		t.Fatalf("source window = %+v", got)
	}
	if got := windows["auth-only"]; got != (testutil.CredentialWindowSummary{}) {
		t.Fatalf("auth-only window = %+v", got)
	}
}

func TestUsageStatisticsConcurrentReadWrite(t *testing.T) {
	now := time.Now().Unix()
	statistics := newStatisticsForTest(now, Pricing{})
	var wait sync.WaitGroup

	wait.Add(1)
	go func() {
		defer wait.Done()
		for index := 0; index < 500; index++ {
			statistics.Add(usagemodel.Record{
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
				if _, err := statistics.SummaryJSON("24h", now); err != nil {
					t.Errorf("summary: %v", err)
					return
				}
				if _, err := statistics.CredentialWindowsJSON(nil, now); err != nil {
					t.Errorf("credential windows: %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()

	payload, err := statistics.SummaryJSON("all", now)
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.DecodeUsageSummary(t, payload).TotalRequests; got != 500 {
		t.Fatalf("total requests = %d, want 500", got)
	}
}

func BenchmarkUsageStatisticsAdd(b *testing.B) {
	now := time.Now().Unix()
	statistics := newStatisticsForTest(now, Pricing{})
	for minute := FloorUnix(now-credentialHistoryRange, StatisticsMinute); minute <= now; minute += StatisticsMinute {
		statistics.state.MinuteBuckets[minute] = nil
		statistics.state.CredentialMinutes[minute] = nil
	}
	for hour := FloorUnix(now-credentialHistoryRange, StatisticsHour); hour <= now; hour += StatisticsHour {
		statistics.state.CredentialHours[hour] = nil
	}
	record := usagemodel.Record{
		Timestamp: time.Unix(now, 0),
		APIKey:    "client-key",
		Model:     "model-a",
		AuthIndex: "auth-1",
		Source:    "credential.json",
		Tokens:    usagemodel.TokenStats{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	}
	b.ReportAllocs()
	for range b.N {
		statistics.Add(record, now)
	}
}

func BenchmarkUsageSummaryJSON(b *testing.B) {
	now := time.Now().Unix()
	statistics := newStatisticsForTest(now, Pricing{})
	for index := 0; index < 1000; index++ {
		statistics.Add(usagemodel.Record{
			ID:        fmt.Sprintf("record-%d", index),
			Timestamp: time.Unix(now-int64(index%3600), 0),
			APIKey:    fmt.Sprintf("key-%d", index%100),
			Model:     fmt.Sprintf("model-%d", index%20),
			AuthIndex: fmt.Sprintf("auth-%d", index%100),
			Source:    fmt.Sprintf("credential-%d.json", index%100),
			Tokens:    usagemodel.TokenStats{TotalTokens: 100},
		}, now)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := statistics.SummaryJSON("24h", now); err != nil {
			b.Fatal(err)
		}
	}
}
