package plugin

import (
	"context"
	"testing"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	safetycontrol "github.com/Jacko-John/cpa-usage-statistics/internal/safety"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testutil"
)

func TestToRecordMapping(t *testing.T) {
	rec := usageRecord{
		APIKey: "k", Model: "m", ReasoningEffort: "medium",
		Latency: int64(2 * time.Second), TTFT: int64(300 * time.Millisecond),
		Failed: false, Failure: usageFailure{StatusCode: 500, Body: "err"},
		Detail: usageDetail{InputTokens: 5, CacheReadTokens: 7, CacheCreationTokens: 9},
	}
	r := toRecord(rec)
	if r.LatencyMs != 2000 {
		t.Fatalf("latency ms = %d, want 2000", r.LatencyMs)
	}
	if r.TTFTMs != 300 {
		t.Fatalf("ttft ms = %d, want 300", r.TTFTMs)
	}
	if r.ReasoningEffort != "medium" {
		t.Fatalf("effort = %q", r.ReasoningEffort)
	}
	if !r.Failed {
		t.Fatalf("failed should be true from status 500")
	}
	if r.Tokens.CacheReadTokens != 7 || r.Tokens.CacheCreationTokens != 9 {
		t.Fatalf("cache tokens = %+v", r.Tokens)
	}
	if r.ID == "" {
		t.Fatalf("id should be generated")
	}
}

func TestUsageHandleKeepsDistinctIDsAcrossRestart(t *testing.T) {
	previousSafety := safety
	previousSchema := negotiatedSchemaVersion.Load()
	previousRetention := retentionDays.Load()
	safety = safetycontrol.New()
	t.Cleanup(func() {
		closeStore()
		safety = previousSafety
		negotiatedSchemaVersion.Store(previousSchema)
		retentionDays.Store(previousRetention)
	})

	dataDir := t.TempDir()
	configureForSchemaTest(t, 1, dataDir)
	payload := []byte(`{
		"ID":"00000001",
		"RequestID":"00000001",
		"TraceID":"00000001",
		"RequestedAt":"2026-09-24T09:00:00Z",
		"APIKey":"client-key",
		"Model":"model-a",
		"Failed":true,
		"Failure":{"StatusCode":403,"Body":"{\"error\":{\"code\":\"cyber_policy\"}}"}
	}`)
	if _, err := Handle("usage.handle", payload, Metadata{}); err != nil {
		t.Fatalf("first usage.handle: %v", err)
	}
	ctx := context.Background()
	usage, err := currentStore().Query(ctx, usagemodel.QueryRange{})
	if err != nil {
		t.Fatalf("query before restart: %v", err)
	}
	first := usage["client-key"]["model-a"]
	if len(first) != 1 || first[0].ID == "" || first[0].ID == "00000001" {
		t.Fatalf("usage before restart = %+v", usage)
	}
	firstID := first[0].ID

	closeStore()
	configureForSchemaTest(t, 1, dataDir)
	if _, err := Handle("usage.handle", payload, Metadata{}); err != nil {
		t.Fatalf("second usage.handle: %v", err)
	}
	store := currentStore()
	usage, err = store.Query(ctx, usagemodel.QueryRange{})
	if err != nil {
		t.Fatalf("query after restart: %v", err)
	}
	details := usage["client-key"]["model-a"]
	if len(details) != 2 {
		t.Fatalf("usage after restart = %+v", usage)
	}
	ids := make(map[string]bool, 2)
	for _, detail := range details {
		if detail.ID == "" || detail.ID == "00000001" || ids[detail.ID] {
			t.Fatalf("duplicate or upstream usage ID: %+v", details)
		}
		ids[detail.ID] = true
	}
	if !ids[firstID] {
		t.Fatalf("original usage ID %q was lost after restart: %+v", firstID, details)
	}

	history, err := store.QuerySafetyHistory(ctx, usagemodel.SafetyHistoryQuery{APIKey: "client-key", Limit: 10})
	if err != nil {
		t.Fatalf("query safety history: %v", err)
	}
	if len(history) != 2 || !ids[history[0].UsageRecordID] || !ids[history[1].UsageRecordID] || history[0].UsageRecordID == history[1].UsageRecordID {
		t.Fatalf("safety history after restart = %+v", history)
	}
	idsToDelete := usagemodel.NewStableStringSet(2)
	for id := range ids {
		idsToDelete.Add(id)
	}
	deleted, err := store.Delete(ctx, idsToDelete)
	if err != nil {
		t.Fatalf("delete safety-linked usage: %v", err)
	}
	if deleted.Deleted != 0 || len(deleted.Missing) != 2 {
		t.Fatalf("safety-linked usage was deleted: %+v", deleted)
	}
	summary, err := store.SummaryJSON("all", time.Now().Unix())
	if err != nil {
		t.Fatalf("usage summary: %v", err)
	}
	if got := testutil.DecodeUsageSummary(t, summary).TotalRequests; got != 2 {
		t.Fatalf("usage summary requests = %d, want 2", got)
	}
}
