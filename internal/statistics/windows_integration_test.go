package statistics_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
	"github.com/Jacko-John/cpa-usage-statistics/internal/storage"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testfixture"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testutil"
)

type windowQueryRecorder struct {
	storage.SQLQueryer
	calls int
	query string
	args  []any
}

func (q *windowQueryRecorder) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.calls++
	q.query, q.args = query, args
	if q.SQLQueryer == nil {
		return nil, errors.New("unexpected usage-record query")
	}
	return q.SQLQueryer.QueryContext(ctx, query, args...)
}

func assertAPIWindowsForTest(t *testing.T, got []statistics.APIWindowSummary, queries []statistics.APIWindowQuery, records []usagemodel.Record, pricing statistics.Pricing, now int64) {
	t.Helper()
	if len(got) != len(queries) {
		t.Fatalf("results=%d queries=%d", len(got), len(queries))
	}
	multipliers := statistics.BuildTierMultiplierIndex(pricing.TierMultipliers)
	for i, query := range queries {
		var want statistics.APIWindowSummary
		for _, record := range records {
			r := usagemodel.NormalizeRecord(record)
			if usagemodel.GroupingKey(r.APIKey, r.Provider) != strings.TrimSpace(query.APIKey) || r.Timestamp.Unix() < query.Start || r.Timestamp.Unix() >= min(query.End, statistics.FloorUnix(now, 60)+60) {
				continue
			}
			want.Requests++
			want.Tokens += r.Tokens.TotalTokens
			want.Cost += statistics.CalculateStoredRecordCost(r, pricing.Prices, multipliers)
			if _, priced := pricing.Prices[r.Model]; !priced {
				want.Unpriced++
			}
		}
		if got[i].Requests != want.Requests || got[i].Tokens != want.Tokens || got[i].Unpriced != want.Unpriced {
			t.Fatalf("window %d: got=%+v want=%+v", i, got[i], want)
		}
		testutil.AssertCost(t, got[i].Cost, want.Cost)
	}
}

func TestUsageAPIWindowsShareBuckets(t *testing.T) {
	s := testfixture.New(t)
	ctx := context.Background()
	now := statistics.FloorUnix(time.Now().Unix(), 60)
	pricing := statistics.Pricing{Prices: map[string]statistics.ModelPrice{
		"model": {Input: 2, Output: 3, CacheRead: 0.5, CacheCreate: 4, ContextTiers: []statistics.ContextTierPrice{{Threshold: 100, Input: 5, Output: 6, CacheRead: 1, CacheCreate: 7}}},
		"free":  {},
	}, TierMultipliers: []statistics.TierMultiplier{{Model: "model", Tier: "priority", Multiplier: 2}}}
	var records []usagemodel.Record
	for _, key := range []string{"key-a", "key-b"} {
		for _, model := range []string{"model", "free", "unpriced"} {
			for _, offset := range []int64{-121, -120, -61, -60, -1, 0} {
				r := usagemodel.Record{ID: fmt.Sprintf("%s-%s-%d", key, model, offset), APIKey: key, Model: model, Timestamp: time.Unix(now+offset, 0), Provider: "codex", Source: "credential.json", ServiceTier: "PRIORITY", Failed: offset == -1, Tokens: usagemodel.TokenStats{InputTokens: 120, OutputTokens: 20, CachedTokens: 10, CacheCreationTokens: 10}}
				if key == "key-b" {
					r.Provider = "anthropic"
				}
				if err := s.Insert(ctx, r); err != nil {
					t.Fatal(err)
				}
				records = append(records, r)
			}
		}
	}
	if _, err := s.UpdatePricing(ctx, pricing); err != nil {
		t.Fatal(err)
	}
	queries := []statistics.APIWindowQuery{
		{APIKey: "key-a", Start: now - 120, End: now},
		{APIKey: "key-a", Start: now, End: now + 60},
		{APIKey: "key-b", Start: now - 60, End: now + 86400},
		{APIKey: "missing", Start: now - 120, End: now},
		{APIKey: "key-a", Start: now, End: now},
	}
	check := func() {
		t.Helper()
		q := &windowQueryRecorder{}
		err := s.WithConn(ctx, func(_ *sql.Conn) error {
			got, err := s.UsageAPIWindows(ctx, q, queries, now)
			if err == nil {
				assertAPIWindowsForTest(t, got, queries, records, pricing, now)
			}
			return err
		})
		if err != nil || q.calls != 0 {
			t.Fatal("recent windows must use shared memory", err, q.calls)
		}
	}
	check()
	ids := usagemodel.NewStableStringSet(1)
	ids.Add(records[1].ID)
	if result, err := s.Delete(ctx, ids); err != nil || result.Deleted != 1 {
		t.Fatal(result, err)
	}
	records = append(records[:1], records[2:]...)
	check()
	delete(pricing.Prices, "model")
	pricing.Prices["unpriced"] = statistics.ModelPrice{Input: 1}
	if _, err := s.UpdatePricing(ctx, pricing); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestUsageAPIWindowsHistoricalFallback(t *testing.T) {
	s := testfixture.New(t)
	ctx := context.Background()
	now := statistics.FloorUnix(time.Now().Unix(), 60)
	oldest := s.Statistics.StateForTest().OldestMinute
	pricing := statistics.Pricing{Prices: map[string]statistics.ModelPrice{"model": {Input: 2}}}
	var records []usagemodel.Record
	for _, key := range []string{"key-a", "key-b"} {
		for _, at := range []int64{now - 70*86400, now - 65*86400, oldest - 1, oldest, oldest + 59, now - 1} {
			r := usagemodel.Record{ID: fmt.Sprintf("%s-%d", key, at), APIKey: key, Model: "model", Timestamp: time.Unix(at, 0), Tokens: usagemodel.TokenStats{InputTokens: 1_000_000}}
			if err := s.Insert(ctx, r); err != nil {
				t.Fatal(err)
			}
			records = append(records, r)
		}
	}
	if _, err := s.UpdatePricing(ctx, pricing); err != nil {
		t.Fatal(err)
	}
	queries := []statistics.APIWindowQuery{
		{APIKey: "key-a", Start: now - 70*86400, End: now - 65*86400},
		{APIKey: "key-b", Start: now - 70*86400, End: now - 60*86400},
		{APIKey: "key-a", Start: oldest - 60, End: oldest + 60},
		{APIKey: "key-b", Start: now - 60, End: now},
	}
	err := s.WithConn(ctx, func(conn *sql.Conn) error {
		q := &windowQueryRecorder{SQLQueryer: conn}
		got, err := s.UsageAPIWindows(ctx, q, queries, now)
		if err != nil {
			return err
		}
		assertAPIWindowsForTest(t, got, queries, records, pricing, now)
		if q.calls != 1 || !strings.Contains(q.query, "WHERE timestamp >= ? AND timestamp < ?") || len(q.args) != 2 || q.args[0] != queries[0].Start || q.args[1] != queries[2].End {
			t.Fatalf("unbounded or repeated scan: calls=%d query=%s args=%v", q.calls, q.query, q.args)
		}
		if _, err := s.UsageAPIWindows(ctx, &windowQueryRecorder{}, queries, now); err == nil {
			t.Fatal("failed fallback must not return a successful zero summary")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for minute := range s.Statistics.StateForTest().MinuteBuckets {
		if minute < oldest {
			t.Fatal("historical query expanded minute cache")
		}
	}
}

func TestStatisticsReusesDeletedDimensionIDs(t *testing.T) {
	store := testfixture.New(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	for _, record := range []usagemodel.Record{
		{ID: "first", Timestamp: now, APIKey: "key-first", Model: "model-first", AuthIndex: "auth-first", Source: "source-first"},
		{ID: "retained", Timestamp: now, APIKey: "key-retained", Model: "model-retained", AuthIndex: "auth-retained", Source: "source-retained"},
	} {
		if err := store.Insert(ctx, record); err != nil {
			t.Fatalf("insert %s: %v", record.ID, err)
		}
	}
	state := store.Statistics.StateForTest()
	allocated := len(state.Dimensions.Values)
	if allocated != 6 {
		t.Fatalf("allocated dimensions = %d, want 6", allocated)
	}

	ids := usagemodel.NewStableStringSet(1)
	ids.Add("first")
	if result, err := store.Delete(ctx, ids); err != nil || result.Deleted != 1 {
		t.Fatalf("delete first record = %+v, err=%v", result, err)
	}
	if len(state.Dimensions.IDs) != 3 || len(state.Dimensions.Free) != 3 {
		t.Fatalf("dimensions after delete = active %d, free %d", len(state.Dimensions.IDs), len(state.Dimensions.Free))
	}

	if err := store.Insert(ctx, usagemodel.Record{
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

	ids = usagemodel.NewStableStringSet(2)
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
