package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type windowQueryRecorder struct {
	sqlQueryer
	calls int
	query string
	args  []any
}

func (q *windowQueryRecorder) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.calls++
	q.query, q.args = query, args
	if q.sqlQueryer == nil {
		return nil, errors.New("unexpected usage-record query")
	}
	return q.sqlQueryer.QueryContext(ctx, query, args...)
}

func assertAPIWindowsForTest(t *testing.T, got []usageAPIWindowSummary, queries []usageAPIWindowQuery, records []Record, pricing usagePricing, now int64) {
	t.Helper()
	if len(got) != len(queries) {
		t.Fatalf("results=%d queries=%d", len(got), len(queries))
	}
	multipliers := buildTierMultiplierIndex(pricing.TierMultipliers)
	for i, query := range queries {
		var want usageAPIWindowSummary
		for _, record := range records {
			r := normalizeStoredRecord(record)
			if groupingKey(r.APIKey, r.Provider) != strings.TrimSpace(query.APIKey) || r.Timestamp.Unix() < query.Start || r.Timestamp.Unix() >= min(query.End, floorUnix(now, 60)+60) {
				continue
			}
			want.Requests++
			want.Tokens += r.Tokens.TotalTokens
			want.Cost += calculateStoredRecordCost(r, pricing.Prices, multipliers)
			if _, priced := pricing.Prices[r.Model]; !priced {
				want.Unpriced++
			}
		}
		if got[i].Requests != want.Requests || got[i].Tokens != want.Tokens || got[i].Unpriced != want.Unpriced {
			t.Fatalf("window %d: got=%+v want=%+v", i, got[i], want)
		}
		assertCostForTest(t, got[i].Cost, want.Cost)
	}
}

func TestUsageAPIWindowsShareBuckets(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := floorUnix(time.Now().Unix(), 60)
	pricing := usagePricing{Prices: map[string]usageModelPrice{
		"model": {Input: 2, Output: 3, CacheRead: 0.5, CacheCreate: 4, ContextTiers: []usageContextTierPrice{{Threshold: 100, Input: 5, Output: 6, CacheRead: 1, CacheCreate: 7}}},
		"free":  {},
	}, TierMultipliers: []usageTierMultiplier{{Model: "model", Tier: "priority", Multiplier: 2}}}
	var records []Record
	for _, key := range []string{"key-a", "key-b"} {
		for _, model := range []string{"model", "free", "unpriced"} {
			for _, offset := range []int64{-121, -120, -61, -60, -1, 0} {
				r := Record{ID: fmt.Sprintf("%s-%s-%d", key, model, offset), APIKey: key, Model: model, Timestamp: time.Unix(now+offset, 0), Provider: "codex", Source: "credential.json", ServiceTier: "PRIORITY", Failed: offset == -1, Tokens: TokenStats{InputTokens: 120, OutputTokens: 20, CachedTokens: 10, CacheCreationTokens: 10}}
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
	if _, err := s.updateUsagePricing(ctx, pricing); err != nil {
		t.Fatal(err)
	}
	queries := []usageAPIWindowQuery{
		{APIKey: "key-a", Start: now - 120, End: now},
		{APIKey: "key-a", Start: now, End: now + 60},
		{APIKey: "key-b", Start: now - 60, End: now + 86400},
		{APIKey: "missing", Start: now - 120, End: now},
		{APIKey: "key-a", Start: now, End: now},
	}
	check := func() {
		t.Helper()
		q := &windowQueryRecorder{}
		err := s.withConn(ctx, func(_ *sql.Conn) error {
			got, err := s.usageAPIWindows(ctx, q, queries, now)
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
	ids := NewStableStringSet(1)
	ids.Add(records[1].ID)
	if result, err := s.Delete(ctx, ids); err != nil || result.Deleted != 1 {
		t.Fatal(result, err)
	}
	records = append(records[:1], records[2:]...)
	check()
	delete(pricing.Prices, "model")
	pricing.Prices["unpriced"] = usageModelPrice{Input: 1}
	if _, err := s.updateUsagePricing(ctx, pricing); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestUsageAPIWindowsHistoricalFallback(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := floorUnix(time.Now().Unix(), 60)
	oldest := s.statistics.state.OldestMinute
	pricing := usagePricing{Prices: map[string]usageModelPrice{"model": {Input: 2}}}
	var records []Record
	for _, key := range []string{"key-a", "key-b"} {
		for _, at := range []int64{now - 70*86400, now - 65*86400, oldest - 1, oldest, oldest + 59, now - 1} {
			r := Record{ID: fmt.Sprintf("%s-%d", key, at), APIKey: key, Model: "model", Timestamp: time.Unix(at, 0), Tokens: TokenStats{InputTokens: 1_000_000}}
			if err := s.Insert(ctx, r); err != nil {
				t.Fatal(err)
			}
			records = append(records, r)
		}
	}
	if _, err := s.updateUsagePricing(ctx, pricing); err != nil {
		t.Fatal(err)
	}
	queries := []usageAPIWindowQuery{
		{APIKey: "key-a", Start: now - 70*86400, End: now - 65*86400},
		{APIKey: "key-b", Start: now - 70*86400, End: now - 60*86400},
		{APIKey: "key-a", Start: oldest - 60, End: oldest + 60},
		{APIKey: "key-b", Start: now - 60, End: now},
	}
	err := s.withConn(ctx, func(conn *sql.Conn) error {
		q := &windowQueryRecorder{sqlQueryer: conn}
		got, err := s.usageAPIWindows(ctx, q, queries, now)
		if err != nil {
			return err
		}
		assertAPIWindowsForTest(t, got, queries, records, pricing, now)
		if q.calls != 1 || !strings.Contains(q.query, "WHERE timestamp >= ? AND timestamp < ?") || len(q.args) != 2 || q.args[0] != queries[0].Start || q.args[1] != queries[2].End {
			t.Fatalf("unbounded or repeated scan: calls=%d query=%s args=%v", q.calls, q.query, q.args)
		}
		if _, err := s.usageAPIWindows(ctx, &windowQueryRecorder{}, queries, now); err == nil {
			t.Fatal("failed fallback must not return a successful zero summary")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for minute := range s.statistics.state.MinuteBuckets {
		if minute < oldest {
			t.Fatal("historical query expanded minute cache")
		}
	}
}

func TestNotificationSchemaDoesNotChangeUsageRecords(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.db")
			if legacy {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(createUsageRecordsSQL); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO usage_records(id,timestamp) VALUES('legacy',?)`, time.Now().Unix()); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			s, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			columns, err := s.existingColumns(context.Background())
			if err != nil || len(columns) != 25 {
				t.Fatal(columns, err)
			}
			for name := range columns {
				if strings.HasPrefix(name, "notification_") {
					t.Fatal("notification column added to usage_records", name)
				}
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('notification_basis','notification_progress','idx_notification_pending')`).Scan(&count); err != nil || count != 0 {
				t.Fatal("redundant notification schema", count, err)
			}
			if legacy {
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_records WHERE id='legacy'`).Scan(&count); err != nil || count != 1 {
					t.Fatal("legacy record lost", count, err)
				}
			}
		})
	}
}

func BenchmarkUsageAPIWindowsBatch(b *testing.B) {
	now := floorUnix(time.Now().Unix(), 60)
	pricing, pricingKey := normalizeUsagePricing(usagePricing{Prices: map[string]usageModelPrice{"model": {Input: 1}}})
	s := usageStatistics{state: newUsageStatisticsState(now, pricing, pricingKey)}
	queries := make([]usageAPIWindowQuery, 100)
	for i := range queries {
		queries[i] = usageAPIWindowQuery{APIKey: fmt.Sprintf("key-%d", i), Start: now - 86400, End: now}
	}
	for minute := now - 86400; minute < now; minute += 60 {
		for _, query := range queries {
			s.state.addRecord(usageStatisticsRecord{APIKey: query.APIKey, Model: "model", Timestamp: minute, InputTokens: 100, TotalTokens: 100}, now)
		}
	}
	b.Run("batch-100-keys", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s.apiWindows(queries, now)
		}
	})
	b.Run("separate-100-keys", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for j := range queries {
				s.apiWindows(queries[j:j+1], now)
			}
		}
	})
}
