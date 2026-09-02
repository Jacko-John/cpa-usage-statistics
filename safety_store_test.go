package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSafetyHistoryUsesCompositeCursor(t *testing.T) {
	store := newTestStore(t)
	rows := []SafetyHistory{
		{APIKey: "client-key", OccurredAt: 100, UsageRecordID: "a"},
		{APIKey: "client-key", OccurredAt: 100, UsageRecordID: "b"},
		{APIKey: "client-key", OccurredAt: 99, UsageRecordID: "z"},
	}
	for _, record := range rows {
		if _, err := store.db.Exec(`
INSERT INTO api_key_safety_history (api_key, occurred_at, usage_record_id, blocked_until, manual_released_at)
VALUES (?, ?, ?, ?, ?)
`, record.APIKey, record.OccurredAt, record.UsageRecordID, record.BlockedUntil, record.ManualReleasedAt); err != nil {
			t.Fatalf("insert history: %v", err)
		}
	}

	first, err := store.QuerySafetyHistory(context.Background(), SafetyHistoryQuery{APIKey: "client-key", Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first) != 2 || first[0].UsageRecordID != "b" || first[1].UsageRecordID != "a" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := store.QuerySafetyHistory(context.Background(), SafetyHistoryQuery{
		APIKey:     "client-key",
		BeforeTime: first[1].OccurredAt,
		BeforeID:   first[1].UsageRecordID,
		Limit:      2,
	})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second) != 1 || second[0].UsageRecordID != "z" {
		t.Fatalf("second page = %+v", second)
	}
}

func TestSafetyHistoryJoinsUsageFieldsAndProtectsLinkedUsage(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Record{
		ID:                "linked",
		Timestamp:         time.Now(),
		APIKey:            "client-key",
		Provider:          "codex",
		Model:             "gpt-5-codex",
		Failed:            true,
		FailureStatusCode: 400,
		FailureBody:       `{"error":{"code":"cyber_policy"}}`,
	}); err != nil {
		t.Fatalf("Insert usage: %v", err)
	}
	for _, history := range []SafetyHistory{
		{APIKey: "client-key", OccurredAt: 100, UsageRecordID: "linked"},
		{APIKey: "client-key", OccurredAt: 99, UsageRecordID: "missing"},
	} {
		if _, err := store.db.Exec(`
INSERT INTO api_key_safety_history (api_key, occurred_at, usage_record_id, blocked_until, manual_released_at)
VALUES (?, ?, ?, 0, 0)
`, history.APIKey, history.OccurredAt, history.UsageRecordID); err != nil {
			t.Fatalf("insert history: %v", err)
		}
	}

	history, err := store.QuerySafetyHistory(ctx, SafetyHistoryQuery{APIKey: "client-key", Limit: 10})
	if err != nil {
		t.Fatalf("QuerySafetyHistory: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history = %+v", history)
	}
	linked := history[0]
	if linked.Provider != "codex" || linked.Model != "gpt-5-codex" || linked.FailureStatusCode != 400 || linked.FailureBody == "" {
		t.Fatalf("linked history = %+v", linked)
	}
	missing := history[1]
	if missing.Provider != "" || missing.Model != "" || missing.FailureStatusCode != 0 || missing.FailureBody != "" {
		t.Fatalf("missing history = %+v", missing)
	}

	ids := NewStableStringSet(1)
	ids.Add("linked")
	deleteResult, err := store.Delete(ctx, ids)
	if err != nil {
		t.Fatalf("Delete usage: %v", err)
	}
	if deleteResult.Deleted != 0 || len(deleteResult.Missing) != 1 || deleteResult.Missing[0] != "linked" {
		t.Fatalf("delete result = %+v", deleteResult)
	}
	history, err = store.QuerySafetyHistory(ctx, SafetyHistoryQuery{APIKey: "client-key", Limit: 10})
	if err != nil {
		t.Fatalf("QuerySafetyHistory after delete: %v", err)
	}
	if len(history) != 2 || history[0].UsageRecordID != "linked" || history[1].UsageRecordID != "missing" {
		t.Fatalf("history after usage delete = %+v", history)
	}
	linked = history[0]
	if linked.Provider != "codex" || linked.Model != "gpt-5-codex" || linked.FailureStatusCode != 400 || linked.FailureBody == "" {
		t.Fatalf("protected linked history = %+v", linked)
	}
}

func TestSafetyHistoryLargeLimitDoesNotPreallocateLimit(t *testing.T) {
	store := newTestStore(t)
	history, err := store.QuerySafetyHistory(context.Background(), SafetyHistoryQuery{APIKey: "client-key", Limit: 1_000_000_000})
	if err != nil {
		t.Fatalf("QuerySafetyHistory: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("history = %+v", history)
	}
}

func TestUsageCleanupKeepsSafetyLinkedRecords(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	old := time.Unix(800, 0).UTC()
	for _, record := range []Record{
		{ID: "linked", Timestamp: old, APIKey: "client-key", Provider: "codex", Model: "gpt-5-codex"},
		{ID: "ordinary", Timestamp: old, APIKey: "client-key"},
	} {
		if err := store.Insert(ctx, record); err != nil {
			t.Fatalf("Insert usage: %v", err)
		}
	}
	for _, history := range []SafetyHistory{
		{APIKey: "client-key", OccurredAt: 801, UsageRecordID: "linked"},
		{APIKey: "client-key", OccurredAt: 800, UsageRecordID: "missing"},
	} {
		if _, err := store.db.Exec(`
INSERT INTO api_key_safety_history (api_key, occurred_at, usage_record_id, blocked_until, manual_released_at)
VALUES (?, ?, ?, 0, 0)
`, history.APIKey, history.OccurredAt, history.UsageRecordID); err != nil {
			t.Fatalf("insert history: %v", err)
		}
	}

	deleted, err := store.DeleteBefore(ctx, time.Unix(900, 0).UTC())
	if err != nil {
		t.Fatalf("DeleteBefore: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	var linkedCount, ordinaryCount int
	if err := store.db.QueryRow("SELECT count(*) FROM usage_records WHERE id = 'linked'").Scan(&linkedCount); err != nil {
		t.Fatalf("query linked usage: %v", err)
	}
	if err := store.db.QueryRow("SELECT count(*) FROM usage_records WHERE id = 'ordinary'").Scan(&ordinaryCount); err != nil {
		t.Fatalf("query ordinary usage: %v", err)
	}
	if linkedCount != 1 || ordinaryCount != 0 {
		t.Fatalf("usage counts: linked=%d ordinary=%d", linkedCount, ordinaryCount)
	}
	usage, err := store.Query(ctx, QueryRange{})
	if err != nil {
		t.Fatalf("Query usage: %v", err)
	}
	if details := usage["client-key"]["gpt-5-codex"]; len(details) != 1 || details[0].ID != "linked" {
		t.Fatalf("usage after cleanup = %+v", usage)
	}
	payload, err := store.usageSummaryJSON("all", time.Now().Unix())
	if err != nil {
		t.Fatalf("usage summary after cleanup: %v", err)
	}
	if summary := decodeUsageSummaryForTest(t, payload); summary.TotalRequests != 1 {
		t.Fatalf("usage summary after cleanup = %+v", summary)
	}
	history, err := store.QuerySafetyHistory(ctx, SafetyHistoryQuery{APIKey: "client-key", Limit: 10})
	if err != nil {
		t.Fatalf("QuerySafetyHistory: %v", err)
	}
	if len(history) != 2 || history[0].UsageRecordID != "linked" || history[0].Provider != "codex" || history[1].UsageRecordID != "missing" {
		t.Fatalf("history after cleanup = %+v", history)
	}
}

func TestSafetyTablesUseWithoutRowID(t *testing.T) {
	store := newTestStore(t)
	for _, table := range []string{"api_key_safety_state", "api_key_safety_history"} {
		var schema string
		if err := store.db.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&schema); err != nil {
			t.Fatalf("query schema for %s: %v", table, err)
		}
		if !strings.Contains(strings.ToUpper(schema), "WITHOUT ROWID") {
			t.Fatalf("schema for %s does not use WITHOUT ROWID: %s", table, schema)
		}
	}
}
