package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testutil"
)

func notificationTestStore(t *testing.T, now int64) (*testStore, string) {
	t.Helper()
	s, err := openTestStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := DefaultSettings()
	cfg.Enabled = true
	cfg.Frequency = "daily"
	cfg.Timezone = "UTC"
	cfg.Hour = 20
	cfg.Sender = "test@qq.com"
	if err := s.SettingsPut(context.Background(), cfg, "fake-authorization", false, now); err != nil {
		t.Fatal(err)
	}
	id, err := s.SubscriptionPut(context.Background(), Subscription{Key: "test-client-key", Email: "user@example.com", Enabled: true}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	return s, id
}

func notificationTestInsert(t testing.TB, s *testStore, minute int64, id string, tokens int64) {
	t.Helper()
	r := usagemodel.Record{ID: id, Timestamp: time.Unix(minute, 0), APIKey: "test-client-key", Provider: "codex", Model: "model", Tokens: usagemodel.TokenStats{InputTokens: tokens, OutputTokens: tokens, TotalTokens: 2 * tokens}}
	err := s.Insert(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNotificationSettingsConfigured(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg, err := s.SettingsGet(ctx)
	if err != nil || cfg.Configured {
		t.Fatalf("new settings: %+v err=%v", cfg, err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil || !strings.Contains(string(encoded), `"configured":false`) {
		t.Fatalf("missing initial configuration flag: %s err=%v", encoded, err)
	}
	cfg.Timezone = "Europe/Paris"
	if err := s.SettingsPut(ctx, cfg, "", false, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	cfg, err = s.SettingsGet(ctx)
	if err != nil || !cfg.Configured || cfg.PasswordConfigured || cfg.Timezone != "Europe/Paris" {
		t.Fatalf("saved settings without SMTP: %+v err=%v", cfg, err)
	}
	// 旧设置没有 configured 字段，也应被识别为已保存，不能覆盖原时区。
	if _, err := s.DB.Exec(`UPDATE notification_settings SET config=?,password='' WHERE id=1`, []byte(`{"frequency":"weekly","weekday":5,"hour":20,"timezone":"UTC"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cfg, err = reopened.SettingsGet(ctx)
	if err != nil || !cfg.Configured || cfg.Timezone != "UTC" || cfg.PasswordConfigured {
		t.Fatalf("legacy settings after restart: %+v err=%v", cfg, err)
	}
}

func TestNotificationMinuteCycleRepricingAndFrozenHistory(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 19, 58, 0, 0, time.UTC).Unix()
	s, id := notificationTestStore(t, base)
	// 测试固定时间不调用真实时钟的改价入口，价格算法与状态替换一致。
	setPrice := func(price float64) {
		pricing := statistics.Pricing{Prices: map[string]statistics.ModelPrice{"model": {Input: price, Output: price}}}
		if err := s.Statistics.Rebuild(ctx, s.DB, pricing); err != nil {
			t.Fatal(err)
		}
	}
	setPrice(1)
	notificationTestInsert(t, s, base+60, "before", 1_000_000)
	h, err := s.HistoryGet(ctx, id, base+90)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Current) != 1 || h.Current[0].Cost != 2 {
		t.Fatalf("current=%+v", h)
	}
	setPrice(2)
	h, err = s.HistoryGet(ctx, id, base+100)
	if err != nil {
		t.Fatal(err)
	}
	if h.Current[0].Cost != 4 {
		t.Fatalf("repriced=%+v", h)
	}
	notificationTestInsert(t, s, base+120, "boundary", 1_000_000)
	h, err = s.HistoryGet(ctx, id, base+120)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Periods) != 1 || h.Periods[0].Requests != 1 || h.Periods[0].Cost != 4 || h.Current[0].Requests != 1 {
		t.Fatalf("boundary=%+v", h)
	}
	setPrice(3)
	h, err = s.HistoryGet(ctx, id, base+130)
	if err != nil {
		t.Fatal(err)
	}
	if h.Periods[0].Cost != 4 || h.Current[0].Cost != 6 {
		t.Fatalf("frozen=%+v", h)
	}
	// 迟到请求仍进入原统计，但不改写已封存通知，也不挪入当前周期。
	notificationTestInsert(t, s, base+90, "late", 1_000_000)
	h, err = s.HistoryGet(ctx, id, base+135)
	if err != nil || h.Periods[0].Cost != 4 || h.Periods[0].Requests != 1 || h.Current[0].Requests != 1 {
		t.Fatalf("late report changed snapshot: %+v err=%v", h, err)
	}
	if _, err := s.DeleteBefore(ctx, time.Unix(base+120, 0)); err != nil {
		t.Fatal(err)
	}
	h, err = s.HistoryGet(ctx, id, base+140)
	if err != nil || h.Periods[0].Cost != 4 || h.Periods[0].Requests != 1 || h.Current[0].Requests != 1 || h.Current[0].Cost != 6 {
		t.Fatalf("late report and deletion changed snapshot: %+v err=%v", h, err)
	}
}

func TestNotificationCurrentDeleteAndRecovery(t *testing.T) {
	ctx := context.Background()
	base := statistics.FloorUnix(time.Now().Unix(), 60) - 120
	s, id := notificationTestStore(t, base)
	cfg := s.Service.settings
	cfg.Hour = time.Unix(base, 0).UTC().Add(time.Hour).Hour()
	if err := s.SettingsPut(ctx, cfg, "", false, base); err != nil {
		t.Fatal(err)
	}
	minute := base + 60
	notificationTestInsert(t, s, minute, "first", 100)
	if _, err := s.HistoryGet(ctx, id, minute+10); err != nil {
		t.Fatal(err)
	}
	notificationTestInsert(t, s, minute, "second", 200)
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	s, err = openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, err := s.HistoryGet(ctx, id, minute+20)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Current) != 1 || h.Current[0].Requests != 2 || h.Current[0].Tokens != 600 {
		t.Fatalf("recovered=%+v", h)
	}
	ids := usagemodel.NewStableStringSet(1)
	ids.Add("first")
	if result, err := s.Delete(ctx, ids); err != nil || result.Deleted != 1 {
		t.Fatal(result, err)
	}
	pricing := statistics.Pricing{Prices: map[string]statistics.ModelPrice{"model": {Input: 10, Output: 10}}}
	if _, err := s.UpdatePricing(ctx, pricing); err != nil {
		t.Fatal(err)
	}
	h, err = s.HistoryGet(ctx, id, minute+30)
	if err != nil {
		t.Fatal(err)
	}
	if h.Current[0].Requests != 1 || math.Abs(h.Current[0].Cost-0.004) > 1e-10 {
		t.Fatalf("deleted reprice=%+v", h)
	}
	if _, err := s.DeleteBefore(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, err = s.HistoryGet(ctx, id, minute+40)
	if err != nil || h.Current[0].Requests != 0 || h.Current[0].Cost != 0 {
		t.Fatalf("cleanup recovery=%+v err=%v", h, err)
	}
}

func TestNotificationSnapshotRollbackAndDuplicate(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 19, 58, 0, 0, time.UTC).Unix()
	s, id := notificationTestStore(t, base)
	notificationTestInsert(t, s, base+60, "one", 10)
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_notification BEFORE UPDATE ON notification_periods WHEN NEW.status='pending' BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx, base+120); err == nil {
		t.Fatal("snapshot should fail")
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_periods`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial snapshot", count, err)
	}
	for _, seg := range s.Service.segments {
		if seg.Cursor != seg.Start {
			t.Fatal("cursor advanced before commit")
		}
	}
	if _, err := s.DB.Exec(`DROP TRIGGER fail_notification`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		h, err := s.HistoryGet(ctx, id, base+120)
		if err != nil || len(h.Periods) != 1 || h.Periods[0].Requests != 1 {
			t.Fatal(h, err)
		}
	}
	if err := s.Insert(ctx, usagemodel.Record{ID: "one", APIKey: "test-client-key", Timestamp: time.Now()}); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestNotificationSharedContextTierPricing(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC).Unix()
	s, id := notificationTestStore(t, base)
	pricing := statistics.Pricing{Prices: map[string]statistics.ModelPrice{"model": {Input: 1, Output: 2, CacheRead: 0.5, CacheCreate: 3, ContextTiers: []statistics.ContextTierPrice{{Threshold: 100, Input: 5, Output: 10, CacheRead: 2, CacheCreate: 8}}}}, TierMultipliers: []statistics.TierMultiplier{{Model: "model", Tier: "priority", Multiplier: 2}}}
	var records []usagemodel.Record
	for _, provider := range []string{"codex", "anthropic"} {
		for _, input := range []int64{100, 101, 1000} {
			r := usagemodel.Record{ID: fmt.Sprintf("%s-%d", provider, input), APIKey: "test-client-key", Timestamp: time.Unix(base+60, 0), Model: "model", Provider: provider, ServiceTier: "PRIORITY", Tokens: usagemodel.TokenStats{InputTokens: input, OutputTokens: 50, CachedTokens: 20, CacheCreationTokens: 10}}
			if err := s.Insert(ctx, r); err != nil {
				t.Fatal(err)
			}
			records = append(records, r)
		}
	}
	for _, threshold := range []float64{100, 500} {
		price := pricing.Prices["model"]
		price.ContextTiers[0].Threshold = threshold
		pricing.Prices["model"] = price
		if err := s.Statistics.Rebuild(ctx, s.DB, pricing); err != nil {
			t.Fatal(err)
		}
		var want float64
		for _, r := range records {
			want += statistics.CalculateStoredRecordCost(r, pricing.Prices, statistics.BuildTierMultiplierIndex(pricing.TierMultipliers))
		}
		h, err := s.HistoryGet(ctx, id, base+120)
		if err != nil || len(h.Current) != 1 || h.Current[0].Requests != int64(len(records)) {
			t.Fatal(h, err)
		}
		testutil.AssertCost(t, h.Current[0].Cost, want)
	}
}

func TestNotificationScheduleDSTAndWeekly(t *testing.T) {
	cfg := DefaultSettings()
	start := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC).Unix()
	if cfg.Boundary(start, false) != start || cfg.Boundary(start, true) != start+7*86400 {
		t.Fatal("weekly boundary")
	}
	cfg.Frequency = "daily"
	cfg.Timezone = "America/New_York"
	cfg.Hour = 2
	cfg.Minute = 30
	loc, _ := time.LoadLocation(cfg.Timezone)
	before := time.Date(2026, 3, 7, 2, 30, 0, 0, loc)
	next := time.Unix(cfg.Boundary(before.Unix(), true), 0).In(loc)
	if next.Day() != 9 || next.Hour() != 2 || next.Minute() != 30 {
		t.Fatal(next)
	}
	cfg.Hour = 1
	before = time.Date(2026, 10, 31, 1, 30, 0, 0, loc)
	next = time.Unix(cfg.Boundary(before.Unix(), true), 0).In(loc)
	after := time.Unix(cfg.Boundary(next.Unix(), true), 0).In(loc)
	if next.Day() != 1 || after.Day() != 2 {
		t.Fatalf("%v %v", next, after)
	}
}

func TestNotificationMailStatesAndSecrets(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 19, 58, 0, 0, time.UTC).Unix()
	s, id := notificationTestStore(t, base)
	notificationTestInsert(t, s, base+60, "one", 10)
	if err := s.Tick(ctx, base+120); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.SettingsGet(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(cfg)
	if cfg.Password != "" || !cfg.PasswordConfigured || strings.Contains(string(encoded), "fake-authorization") {
		t.Fatal(string(encoded))
	}
	calls := 0
	s.Service.send = func(_ context.Context, _ Settings, m Mail) (bool, error) {
		calls++
		_, parts := testutil.MessageParts(t, Message(m, "test@qq.com"))
		for _, content := range parts {
			if strings.Contains(content, "test-client-key") || strings.Contains(content, "fake-authorization") {
				t.Fatal("secret leaked")
			}
		}
		return true, errors.New("发送结果未知")
	}
	if err := s.SendBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SendBatch(ctx); err != nil {
		t.Fatal(err)
	}
	h, err := s.HistoryGet(ctx, id, base+120)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || h.Periods[0].Status != "unknown" {
		t.Fatal(h, calls)
	}
	if err := s.Retry(ctx, h.Periods[0].ID); err != nil {
		t.Fatal(err)
	}
	s.Service.send = func(context.Context, Settings, Mail) (bool, error) {
		calls++
		return false, nil
	}
	if err := s.SendBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestNotificationEmailPauseAndScheduleChanges(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 19, 55, 0, 0, time.UTC).Unix()
	s, id := notificationTestStore(t, base)
	notificationTestInsert(t, s, base+60, "before-email", 100)
	sub := Subscription{ID: id, Key: "test-client-key", Email: "new@example.com", Enabled: true}
	if _, err := s.SubscriptionPut(ctx, sub, false, base+90); err != nil {
		t.Fatal(err)
	}
	h, err := s.HistoryGet(ctx, id, base+100)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Current) != 1 || h.Current[0].Email != "user@example.com" {
		t.Fatal(h)
	}
	boundary := base + 5*60
	h, err = s.HistoryGet(ctx, id, boundary)
	if err != nil {
		t.Fatal(err)
	}
	if h.Periods[0].Email != "user@example.com" || h.Current[0].Email != "new@example.com" {
		t.Fatal(h)
	}
	notificationTestInsert(t, s, boundary, "after-email", 200)
	sub.Enabled = false
	if _, err := s.SubscriptionPut(ctx, sub, false, boundary+10); err != nil {
		t.Fatal(err)
	}
	h, err = s.HistoryGet(ctx, id, boundary+60)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Current) != 0 || h.Periods[0].Status != "cancelled" || h.Periods[0].Requests != 1 {
		t.Fatal(h)
	}
	notificationTestInsert(t, s, boundary+60, "paused", 300)
	sub.Enabled = true
	if _, err := s.SubscriptionPut(ctx, sub, false, boundary+70); err != nil {
		t.Fatal(err)
	}
	notificationTestInsert(t, s, boundary+120, "resumed", 400)
	cfg := s.Service.settings
	cfg.Minute = 30
	if err := s.SettingsPut(ctx, cfg, "", false, boundary+130); err != nil {
		t.Fatal(err)
	}
	h, err = s.HistoryGet(ctx, id, boundary+180)
	if err != nil {
		t.Fatal(err)
	}
	if h.Periods[0].Requests != 1 || !h.Periods[0].Partial || len(h.Current) != 1 {
		t.Fatal(h)
	}
}

func TestNotificationCatchupOnlyTenCycles(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 1, 19, 58, 0, 0, time.UTC).Unix()
	s, id := notificationTestStore(t, base)
	notificationTestInsert(t, s, base+60, "old", 10)
	end := base + 120 + 30*86400
	if err := s.Tick(ctx, end); err != nil {
		t.Fatal(err)
	}
	h, err := s.HistoryGet(ctx, id, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Periods) != 10 || h.Periods[0].Skipped != 21 {
		t.Fatalf("history=%+v", h)
	}
	var pending int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_periods WHERE status='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 10 {
		t.Fatal(pending)
	}
	calls := 0
	s.Service.send = func(_ context.Context, _ Settings, m Mail) (bool, error) {
		calls++
		if len(m.Periods) != 10 {
			t.Errorf("periods=%d", len(m.Periods))
		}
		return false, nil
	}
	if err := s.SendBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestNotificationWeeklyCatchupBeyondCache(t *testing.T) {
	ctx := context.Background()
	now := statistics.FloorUnix(time.Now().Unix(), 60)
	base := now - 12*7*86400 - 120
	s, id := notificationTestStore(t, base)
	cfg := s.Service.settings
	at := time.Unix(now, 0).UTC()
	cfg.Frequency, cfg.Weekday, cfg.Hour, cfg.Minute = "weekly", int(at.Weekday()), at.Hour(), at.Minute()
	if err := s.SettingsPut(ctx, cfg, "", false, base); err != nil {
		t.Fatal(err)
	}
	pricing := statistics.Pricing{Prices: map[string]statistics.ModelPrice{"model": {Input: 1, Output: 1}}}
	if err := s.Statistics.Rebuild(ctx, s.DB, pricing); err != nil {
		t.Fatal(err)
	}
	for i, minute := range []int64{now - 65*86400, now - 36*86400, now - 10*86400, now - 60} {
		notificationTestInsert(t, s, minute, fmt.Sprint(i), 1_000_000)
	}
	h, err := s.HistoryGet(ctx, id, now)
	if err != nil || len(h.Periods) != 10 || h.Periods[0].Skipped != 3 {
		t.Fatal(h, err)
	}
	var requests int64
	var cost float64
	for _, period := range h.Periods {
		requests += period.Requests
		cost += period.Cost
	}
	if requests != 4 || cost != 8 {
		t.Fatal("historical windows lost or duplicated", requests, cost)
	}
	calls, delivered := 0, 0
	s.Service.send = func(_ context.Context, _ Settings, mail Mail) (bool, error) {
		calls++
		delivered += len(mail.Periods)
		// 新首期产生额外的过渡快照，沿用单封最多十期的投递上限。
		want := 10
		if calls == 2 {
			want = 1
		}
		if len(mail.Periods) != want {
			t.Fatalf("catch-up was not combined: %d", len(mail.Periods))
		}
		return false, nil
	}
	if err := s.SendBatch(ctx); err != nil || calls != 2 || delivered != 11 {
		t.Fatal(calls, delivered, err)
	}
}

func TestNotificationPricingChangeFreezesDuePeriod(t *testing.T) {
	ctx := context.Background()
	now := statistics.FloorUnix(time.Now().Unix(), 60)
	base := now - 180
	s, id := notificationTestStore(t, base)
	cfg := s.Service.settings
	end := time.Unix(now-60, 0).UTC()
	cfg.Hour, cfg.Minute = end.Hour(), end.Minute()
	if err := s.SettingsPut(ctx, cfg, "", false, base); err != nil {
		t.Fatal(err)
	}
	oldPrice := statistics.Pricing{Prices: map[string]statistics.ModelPrice{"model": {Input: 1, Output: 1}}}
	if err := s.Statistics.Rebuild(ctx, s.DB, oldPrice); err != nil {
		t.Fatal(err)
	}
	notificationTestInsert(t, s, now-120, "before-end", 1_000_000)
	notificationTestInsert(t, s, now-60, "at-end", 1_000_000)
	if _, err := s.UpdatePricing(ctx, statistics.Pricing{Prices: map[string]statistics.ModelPrice{"model": {Input: 2, Output: 2}}}); err != nil {
		t.Fatal(err)
	}
	h, err := s.HistoryGet(ctx, id, time.Now().Unix())
	if err != nil || len(h.Periods) != 2 || h.Periods[1].Cost != 0 || h.Periods[0].End != end.Unix() || h.Periods[0].Cost != 2 || len(h.Current) != 1 || h.Current[0].Cost != 4 {
		t.Fatal(h, err)
	}
	if _, err := s.DB.Exec(`UPDATE notification_periods SET status='sending' WHERE id=?`, h.Periods[0].ID); err != nil {
		t.Fatal(err)
	}
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, err = s.HistoryGet(ctx, id, time.Now().Unix())
	if err != nil || h.Periods[0].Cost != 2 || h.Periods[0].Status != "unknown" || h.Current[0].Cost != 4 {
		t.Fatal("snapshot or current pricing changed after restart", h, err)
	}
}

func TestNotificationLifecycleStopsWorker(t *testing.T) {
	s, _ := notificationTestStore(t, time.Now().Unix())
	s.Start()
	done := s.Service.done
	s.Start()
	if done != s.Service.done {
		t.Fatal("duplicate worker")
	}
	closed := make(chan struct{})
	go func() { _ = s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestNotificationInvalidConfigurationAndSafetyInsert(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Unix()
	s, id := notificationTestStore(t, now-120)
	cfg := s.Service.settings
	cfg.Timezone = "not-a-zone"
	if err := s.SettingsPut(ctx, cfg, "", false, now); err == nil {
		t.Fatal("invalid timezone accepted")
	}
	if validEmail("a@example.com\r\nBcc: other@example.com") {
		t.Fatal("header injection")
	}
	r := usagemodel.Record{ID: "safety", Timestamp: time.Now(), APIKey: "test-client-key", Model: "model", Tokens: usagemodel.TokenStats{InputTokens: 2}}
	err := s.InsertSafety(ctx, r, usagemodel.SafetyState{APIKey: r.APIKey}, usagemodel.SafetyHistory{APIKey: r.APIKey, UsageRecordID: r.ID, OccurredAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteBefore(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var records int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM usage_records WHERE id='safety'`).Scan(&records); err != nil || records != 1 {
		t.Fatal(records, err)
	}
	h, err := s.HistoryGet(ctx, id, now)
	if err != nil {
		t.Fatal(err)
	}
	if h.Current[0].Requests != 1 {
		t.Fatal(h)
	}
}

func TestNotificationConcurrentPricingAndSnapshots(t *testing.T) {
	ctx := context.Background()
	base := statistics.FloorUnix(time.Now().Unix(), 60) - 120
	s, id := notificationTestStore(t, base)
	cfg := s.Service.settings
	cfg.Hour = time.Unix(base, 0).UTC().Add(time.Hour).Hour()
	if err := s.SettingsPut(ctx, cfg, "", false, base); err != nil {
		t.Fatal(err)
	}
	setPrice := func(price float64) error {
		_, err := s.UpdatePricing(ctx, statistics.Pricing{Prices: map[string]statistics.ModelPrice{"model": {Input: price, Output: price}}})
		return err
	}
	if err := setPrice(1); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 2; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				err := s.Insert(ctx, usagemodel.Record{ID: fmt.Sprintf("%d-%d", worker, i), APIKey: "test-client-key", Timestamp: time.Now(), Model: "model", Tokens: usagemodel.TokenStats{InputTokens: 1_000_000, OutputTokens: 1_000_000}})
				if err != nil {
					t.Error(err)
				}
			}
		}(worker)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			if err := setPrice(float64(1 + i%2)); err != nil {
				t.Error(err)
			}
		}
	}()
	for i := 0; i < 20; i++ {
		h, err := s.HistoryGet(ctx, id, time.Now().Unix())
		if err != nil || len(h.Current) != 1 {
			t.Fatal(h, err)
		}
		p := h.Current[0]
		if p.Cost != float64(p.Requests)*2 && p.Cost != float64(p.Requests)*4 {
			t.Fatalf("mixed pricing versions: %+v", p)
		}
	}
	wg.Wait()
	h, err := s.HistoryGet(ctx, id, time.Now().Unix())
	if err != nil || h.Current[0].Requests != 40 || h.Current[0].Cost != 160 {
		t.Fatal(h, err)
	}
	end := h.Current[0].End
	if err := s.Tick(ctx, end); err != nil {
		t.Fatal(err)
	}
	if err := setPrice(1); err != nil {
		t.Fatal(err)
	}
	h, err = s.HistoryGet(ctx, id, end)
	if err != nil || h.Periods[0].Requests != 40 || h.Periods[0].Cost != 160 {
		t.Fatal("frozen snapshot changed", h, err)
	}
}

func BenchmarkNotificationInsert(b *testing.B) {
	for _, subscribed := range []bool{false, true} {
		b.Run(fmt.Sprintf("subscribed=%v", subscribed), func(b *testing.B) {
			s, err := openTestStore(filepath.Join(b.TempDir(), "usage.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			if subscribed {
				cfg := DefaultSettings()
				cfg.Enabled = true
				cfg.Sender = "test@qq.com"
				if err := s.SettingsPut(ctx, cfg, "test", false, time.Now().Unix()-120); err != nil {
					b.Fatal(err)
				}
				if _, err := s.SubscriptionPut(ctx, Subscription{Key: "test-client-key", Email: "user@example.com", Enabled: true}, false, time.Now().Unix()-120); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.Insert(ctx, usagemodel.Record{ID: fmt.Sprint(i), APIKey: "test-client-key", Timestamp: time.Now(), Model: "model", Tokens: usagemodel.TokenStats{InputTokens: 100, OutputTokens: 10}}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestNotificationConcurrentInsertAndQuery(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Unix()
	s, id := notificationTestStore(t, now-120)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				err := s.Insert(ctx, usagemodel.Record{ID: fmt.Sprintf("%d-%d", worker, i), Timestamp: time.Now(), APIKey: "test-client-key", Model: "model", Tokens: usagemodel.TokenStats{InputTokens: 1}})
				if err != nil {
					t.Error(err)
				}
			}
		}(worker)
	}
	for i := 0; i < 10; i++ {
		if _, err := s.HistoryGet(ctx, id, now); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	h, err := s.HistoryGet(ctx, id, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	for _, p := range h.Current {
		count += p.Requests
	}
	for _, p := range h.Periods {
		count += p.Requests
	}
	if count != 100 {
		t.Fatal(count)
	}
}
