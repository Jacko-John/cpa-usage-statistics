package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testutil"
)

func TestStoreStatisticsPricingDeleteAndMemoryRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	ctx := context.Background()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	for index := 0; index < 2; index++ {
		if err := store.Insert(ctx, usagemodel.Record{
			ID:        fmt.Sprintf("record-%d", index),
			Timestamp: now.Add(time.Duration(index) * time.Second),
			APIKey:    "client-key",
			Model:     "model-a",
			AuthIndex: "auth-1",
			Source:    "credential.json",
			Tokens:    usagemodel.TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
		}); err != nil {
			_ = store.Close()
			t.Fatalf("insert: %v", err)
		}
	}

	pricing := statistics.Pricing{Prices: map[string]statistics.ModelPrice{"model-a": {Input: 1}}}
	if _, err := store.UpdatePricing(ctx, pricing); err != nil {
		_ = store.Close()
		t.Fatalf("update usage pricing: %v", err)
	}
	payload, err := store.SummaryJSON("all", now.Unix()+2)
	if err != nil {
		_ = store.Close()
		t.Fatalf("usage summary: %v", err)
	}
	summary := testutil.DecodeUsageSummary(t, payload)
	if summary.TotalRequests != 2 {
		_ = store.Close()
		t.Fatalf("summary requests = %d, want 2", summary.TotalRequests)
	}
	testutil.AssertCost(t, summary.TotalCost, 2)

	windowPayload, err := store.CredentialWindowsJSON([]statistics.CredentialWindowQuery{{
		ID: "auth-1", AuthIndex: "auth-1", Source: "credential.json", Start: now.Unix() - 1, End: now.Unix() + 2,
	}}, now.Unix()+2)
	if err != nil {
		_ = store.Close()
		t.Fatalf("credential window: %v", err)
	}
	testutil.AssertCost(t, testutil.DecodeCredentialWindows(t, windowPayload).Windows["auth-1"].Cost, 2)

	ids := usagemodel.NewStableStringSet(1)
	ids.Add("record-0")
	if result, err := store.Delete(ctx, ids); err != nil || result.Deleted != 1 {
		_ = store.Close()
		t.Fatalf("delete result = %+v, err=%v", result, err)
	}
	payload, err = store.SummaryJSON("all", now.Unix()+2)
	if err != nil {
		_ = store.Close()
		t.Fatalf("summary after delete: %v", err)
	}
	summary = testutil.DecodeUsageSummary(t, payload)
	if summary.TotalRequests != 1 {
		_ = store.Close()
		t.Fatalf("summary after delete requests = %d, want 1", summary.TotalRequests)
	}
	testutil.AssertCost(t, summary.TotalCost, 1)

	if err := store.db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	payload, err = store.SummaryJSON("all", now.Unix()+2)
	if err != nil {
		t.Fatalf("in-memory summary after database close: %v", err)
	}
	if got := testutil.DecodeUsageSummary(t, payload).TotalRequests; got != 1 {
		t.Fatalf("in-memory requests = %d, want 1", got)
	}
}

func TestDeleteBeforeSubtractsPrecomputedStatistics(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.UpdatePricing(ctx, statistics.Pricing{
		Prices: map[string]statistics.ModelPrice{"model-a": {Input: 1}},
	}); err != nil {
		t.Fatalf("set pricing: %v", err)
	}

	minute := time.Now().Add(-2 * time.Hour).Truncate(time.Minute)
	for _, record := range []usagemodel.Record{
		{
			ID:        "deleted",
			Timestamp: minute.Add(10 * time.Second),
			APIKey:    "client-key",
			Model:     "model-a",
			AuthIndex: "auth-1",
			Source:    "credential.json",
			Failed:    true,
			Tokens:    usagemodel.TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
		},
		{
			ID:        "retained",
			Timestamp: minute.Add(50 * time.Second),
			APIKey:    "client-key",
			Model:     "model-a",
			AuthIndex: "auth-1",
			Source:    "credential.json",
			Tokens:    usagemodel.TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
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
		payload, err := store.SummaryJSON(rangeName, now)
		if err != nil {
			t.Fatalf("%s summary: %v", rangeName, err)
		}
		summary := testutil.DecodeUsageSummary(t, payload)
		if summary.TotalRequests != 1 || summary.SuccessCount != 1 || summary.FailureCount != 0 {
			t.Fatalf("%s summary = %+v", rangeName, summary)
		}
		testutil.AssertCost(t, summary.TotalCost, 1)
		api := summary.APIs["client-key"]
		if api == nil || api.TotalRequests != 1 || api.Models["model-a"].TotalRequests != 1 {
			t.Fatalf("%s API summary = %+v", rangeName, api)
		}
		if credential := summary.Credentials["source:credential.json"]; credential == nil || credential.TotalRequests != 1 {
			t.Fatalf("%s credential summary = %+v", rangeName, credential)
		}
		if summary.Hours[statistics.FloorUnix(minute.Unix(), statistics.StatisticsHour)].TotalRequests != 1 {
			t.Fatalf("%s hour summary = %+v", rangeName, summary.Hours)
		}
		if summary.Days[statistics.ServerUsageDay(minute.Unix()).String()].TotalRequests != 1 {
			t.Fatalf("%s day summary = %+v", rangeName, summary.Days)
		}
	}

	windowPayload, err := store.CredentialWindowsJSON([]statistics.CredentialWindowQuery{{
		ID:        "auth-1",
		AuthIndex: "auth-1",
		Source:    "credential.json",
		Start:     minute.Unix(),
		End:       minute.Add(time.Minute).Unix(),
	}}, now)
	if err != nil {
		t.Fatalf("credential window: %v", err)
	}
	window := testutil.DecodeCredentialWindows(t, windowPayload).Windows["auth-1"]
	if window.Requests != 1 || window.SuccessCount != 1 || window.FailureCount != 0 {
		t.Fatalf("credential window = %+v", window)
	}
	testutil.AssertCost(t, window.Cost, 1)

	payload, err := store.SummaryJSON("7h", now+8*statistics.StatisticsHour)
	if err != nil {
		t.Fatalf("advanced 7h summary: %v", err)
	}
	if summary := testutil.DecodeUsageSummary(t, payload); summary.TotalRequests != 0 || len(summary.APIs) != 0 {
		t.Fatalf("advanced 7h summary = %+v", summary)
	}
	payload, err = store.SummaryJSON("all", now+8*statistics.StatisticsHour)
	if err != nil {
		t.Fatalf("advanced all summary: %v", err)
	}
	if summary := testutil.DecodeUsageSummary(t, payload); summary.TotalRequests != 1 {
		t.Fatalf("advanced all summary = %+v", summary)
	}
}

func TestDeleteBeforeSubtractsRecordsOutsideBucketHistory(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := store.Insert(ctx, usagemodel.Record{
		ID:        "old",
		Timestamp: now.Add(-40 * 24 * time.Hour),
		APIKey:    "client-key",
		Model:     "model-a",
		Tokens:    usagemodel.TokenStats{TotalTokens: 10},
	}); err != nil {
		t.Fatalf("insert old record: %v", err)
	}
	if deleted, err := store.DeleteBefore(ctx, now.Add(-35*24*time.Hour)); err != nil || deleted != 1 {
		t.Fatalf("deleted = %d, err=%v", deleted, err)
	}
	payload, err := store.SummaryJSON("all", now.Unix())
	if err != nil {
		t.Fatalf("all summary: %v", err)
	}
	summary := testutil.DecodeUsageSummary(t, payload)
	if summary.TotalRequests != 0 || len(summary.APIs) != 0 || len(summary.Hours) != 0 || len(summary.Days) != 0 {
		t.Fatalf("all summary after old deletion = %+v", summary)
	}
}

func TestDeleteBeforeMatchesFreshStatisticsRebuild(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pricing := statistics.Pricing{Prices: map[string]statistics.ModelPrice{
		"model-0": {Input: 1},
		"model-1": {Input: 2},
		"model-2": {Input: 3},
		"model-3": {Input: 4},
		"model-4": {Input: 5},
	}}
	if _, err := store.UpdatePricing(ctx, pricing); err != nil {
		t.Fatalf("set pricing: %v", err)
	}

	now := time.Now().Truncate(time.Minute)
	base := now.Add(-40 * 24 * time.Hour)
	for index := 0; index < 160; index++ {
		if err := store.Insert(ctx, usagemodel.Record{
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
			Tokens:    usagemodel.TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
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
		payload, err := store.SummaryJSON(rangeName, queryTime)
		if err != nil {
			t.Fatalf("incremental %s summary: %v", rangeName, err)
		}
		before[rangeName] = string(payload)
	}
	queries := []statistics.CredentialWindowQuery{
		{ID: "auth-0", AuthIndex: "auth-0", Source: "credential-0.json", Start: now.Add(-31 * 24 * time.Hour).Unix(), End: queryTime},
		{ID: "auth-1", AuthIndex: "auth-1", Source: "credential-1.json", Start: now.Add(-7 * 24 * time.Hour).Unix(), End: queryTime},
	}
	windowBefore, err := store.CredentialWindowsJSON(queries, queryTime)
	if err != nil {
		t.Fatalf("incremental credential windows: %v", err)
	}

	if err := store.statistics.Rebuild(ctx, store.db, store.statistics.Pricing()); err != nil {
		t.Fatalf("fresh statistics rebuild: %v", err)
	}
	for _, rangeName := range []string{"all", "7h", "24h", "7d", "30d"} {
		payload, err := store.SummaryJSON(rangeName, queryTime)
		if err != nil {
			t.Fatalf("rebuilt %s summary: %v", rangeName, err)
		}
		if string(payload) != before[rangeName] {
			t.Fatalf("%s summary differs after fresh rebuild\nincremental: %s\nrebuilt: %s", rangeName, before[rangeName], payload)
		}
	}
	windowAfter, err := store.CredentialWindowsJSON(queries, queryTime)
	if err != nil {
		t.Fatalf("rebuilt credential windows: %v", err)
	}
	if string(windowAfter) != string(windowBefore) {
		t.Fatalf("credential windows differ after fresh rebuild\nincremental: %s\nrebuilt: %s", windowBefore, windowAfter)
	}
}

func TestUsagePricingPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	ctx := context.Background()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := store.Insert(ctx, usagemodel.Record{
		ID:          "priced-record",
		Timestamp:   now,
		Model:       "model-a",
		ServiceTier: "priority",
		Tokens:      usagemodel.TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	}); err != nil {
		_ = store.Close()
		t.Fatalf("insert: %v", err)
	}
	pricing := statistics.Pricing{
		Prices: map[string]statistics.ModelPrice{
			"model-a": {
				Input: 1,
				ContextTiers: []statistics.ContextTierPrice{
					{Threshold: 500_000, Input: 2},
				},
			},
		},
		TierMultipliers: []statistics.TierMultiplier{
			{Model: "model-a", Tier: "priority", Multiplier: 3},
		},
	}
	updated, err := store.UpdatePricing(ctx, pricing)
	if err != nil {
		_ = store.Close()
		t.Fatalf("update pricing: %v", err)
	}
	_, wantKey := statistics.NormalizePricing(pricing)
	_, updatedKey := statistics.NormalizePricing(updated)
	if updatedKey != wantKey {
		_ = store.Close()
		t.Fatalf("updated pricing = %+v", updated)
	}
	var storageType string
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT typeof(config) FROM usage_pricing WHERE name = ?",
		pricingRowName,
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

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	_, reopenedKey := statistics.NormalizePricing(reopened.Pricing())
	if reopenedKey != wantKey {
		t.Fatalf("reopened pricing = %+v", reopened.Pricing())
	}
	payload, err := reopened.SummaryJSON("all", now.Unix()+1)
	if err != nil {
		t.Fatalf("summary after reopen: %v", err)
	}
	testutil.AssertCost(t, testutil.DecodeUsageSummary(t, payload).TotalCost, 6)
}

func TestUsagePricingSaveFailureKeepsCurrentStatistics(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Insert(ctx, usagemodel.Record{
		ID:        "record",
		Timestamp: time.Now().Add(-time.Minute),
		Model:     "model-a",
		Tokens:    usagemodel.TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
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

	if _, err := store.UpdatePricing(ctx, statistics.Pricing{
		Prices: map[string]statistics.ModelPrice{"model-a": {Input: 2}},
	}); err == nil {
		t.Fatal("pricing update unexpectedly succeeded")
	}
	payload, err := store.SummaryJSON("all", time.Now().Unix())
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	testutil.AssertCost(t, testutil.DecodeUsageSummary(t, payload).TotalCost, 0)
	_, emptyKey := statistics.NormalizePricing(statistics.Pricing{})
	if store.statistics.PricingKey() != emptyKey {
		t.Fatalf("active pricing changed after failed save: %+v", store.Pricing())
	}
}

func TestStoreConcurrentInsertAndStatisticsRebuild(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	prices := []statistics.Pricing{
		{Prices: map[string]statistics.ModelPrice{"model-a": {Input: 1}}},
		{Prices: map[string]statistics.ModelPrice{"model-a": {Input: 2}}},
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		for index := 0; index < 40; index++ {
			if err := store.Insert(ctx, usagemodel.Record{
				ID:        fmt.Sprintf("record-%d", index),
				Timestamp: now,
				APIKey:    "client-key",
				Model:     "model-a",
				AuthIndex: "auth-1",
				Source:    "credential.json",
				Tokens:    usagemodel.TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
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
			if _, err := store.UpdatePricing(ctx, prices[index%len(prices)]); err != nil {
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
	payload, err := store.statistics.SummaryJSON("all", time.Now().Unix())
	if err != nil {
		t.Fatalf("read in-memory summary: %v", err)
	}
	summary := testutil.DecodeUsageSummary(t, payload)
	if summary.TotalRequests != stored || stored != 40 {
		t.Fatalf("stored=%d summarized=%d, want 40", stored, summary.TotalRequests)
	}
	testutil.AssertCost(t, summary.TotalCost, 80)
}

func TestStoreCoordinatesDeleteWithPricingRebuild(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	ids := usagemodel.NewStableStringSet(10)
	for index := 0; index < 20; index++ {
		id := fmt.Sprintf("record-%d", index)
		if index < 10 {
			ids.Add(id)
		}
		if err := store.Insert(ctx, usagemodel.Record{
			ID:        id,
			Timestamp: now,
			Model:     "model-a",
			Tokens:    usagemodel.TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	if _, err := store.UpdatePricing(ctx, statistics.Pricing{
		Prices: map[string]statistics.ModelPrice{"model-a": {Input: 1}},
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
		_, err := store.UpdatePricing(ctx, statistics.Pricing{
			Prices: map[string]statistics.ModelPrice{"model-a": {Input: 2}},
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

	payload, err := store.SummaryJSON("all", time.Now().Unix())
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	summary := testutil.DecodeUsageSummary(t, payload)
	if summary.TotalRequests != 10 {
		t.Fatalf("summary requests = %d, want 10", summary.TotalRequests)
	}
	testutil.AssertCost(t, summary.TotalCost, 20)
}
