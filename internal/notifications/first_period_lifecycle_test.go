package notifications

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
)

func TestNotificationFirstPeriodSettlementAndResume(t *testing.T) {
	ctx := context.Background()
	now := statistics.FloorUnix(time.Now().Unix(), 60)
	s, err := openTestStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := DefaultSettings()
	cfg.Enabled = true
	cfg.Timezone = "UTC"
	cfg.Sender = "test@qq.com"
	clock := time.Unix(now-3600, 0).UTC()
	cfg.Hour, cfg.Minute = clock.Hour(), clock.Minute()
	start := cfg.Boundary(now, false)
	end := cfg.Boundary(now, true)
	notificationTestInsert(t, s, start-60, "outside", 1000000)
	notificationTestInsert(t, s, start, "before", 1000000)
	if _, err := s.UpdatePricing(ctx, statistics.Pricing{Prices: map[string]statistics.ModelPrice{"model": {Input: 1, Output: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SettingsPut(ctx, cfg, "fake-authorization", false, now); err != nil {
		t.Fatal(err)
	}
	sub := Subscription{Key: "test-client-key", Email: "user@example.com", Enabled: true}
	id, err := s.SubscriptionPut(ctx, sub, false, now)
	if err != nil {
		t.Fatal(err)
	}
	sub.ID = id
	notificationTestInsert(t, s, now, "after", 1000000)
	if err := s.Insert(ctx, usagemodel.Record{ID: "unpriced", Timestamp: time.Unix(start+60, 0), APIKey: sub.Key, Provider: "codex", Model: "unpriced", Tokens: usagemodel.TokenStats{InputTokens: 5, TotalTokens: 5}}); err != nil {
		t.Fatal(err)
	}
	h, err := s.HistoryGet(ctx, id, now)
	if err != nil || len(h.Current) != 1 {
		t.Fatal(err)
	}
	p := h.Current[0]
	if p.Requests != 3 || p.Tokens != 4000005 || p.Cost != 4 || p.Unpriced != 1 || p.Partial {
		t.Fatal("完整首期未按原统计计价")
	}
	for range 2 {
		if err := s.Tick(ctx, end); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var requests int64
	var cost float64
	if err := s.DB.QueryRow("SELECT COUNT(*),SUM(requests),SUM(cost) FROM notification_periods WHERE subscription=? AND status='pending'", id).Scan(&count, &requests, &cost); err != nil {
		t.Fatal(err)
	}
	if count != 1 || requests != 3 || cost != 4 {
		t.Fatal("首期重复封存或金额改变")
	}
	// 暂停、恢复和删除后重绑仍使用下一分钟，不重复覆盖已经封存的首期。
	sub.Enabled = false
	if _, err := s.SubscriptionPut(ctx, sub, false, end+60); err != nil {
		t.Fatal(err)
	}
	sub.Enabled = true
	if _, err := s.SubscriptionPut(ctx, sub, false, end+120); err != nil {
		t.Fatal(err)
	}
	assertStart := func(want int64) {
		t.Helper()
		var got int64
		if err := s.DB.QueryRow("SELECT start FROM notification_segments WHERE subscription=? ORDER BY id DESC LIMIT 1", id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatal("后续恢复不应回溯")
		}
	}
	assertStart(end + 180)
	if _, err := s.SubscriptionPut(ctx, sub, true, end+240); err != nil {
		t.Fatal(err)
	}
	sub.ID = ""
	rebound, err := s.SubscriptionPut(ctx, sub, false, end+300)
	if err != nil || rebound != id {
		t.Fatal("重绑失败", err)
	}
	assertStart(end + 360)
	var frozenRequests int64
	var frozenCost float64
	if err := s.DB.QueryRow("SELECT requests,cost FROM notification_periods WHERE id=?", p.ID).Scan(&frozenRequests, &frozenCost); err != nil {
		t.Fatal(err)
	}
	if frozenRequests != 3 || frozenCost != 4 {
		t.Fatal("恢复或重绑改写冻结金额")
	}
}
