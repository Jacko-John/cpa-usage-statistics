package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestNotificationFirstPeriodIncludesExistingUsage(t *testing.T) {
	for _, frequency := range []string{"daily", "weekly"} {
		for _, mode := range []string{"settings-first", "subscription-first", "subscription-disabled"} {
			t.Run(frequency+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "usage.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = s.Close() }()
				now := floorUnix(time.Now().Unix(), 60)
				cfg := defaultNotificationSettings()
				cfg.Enabled, cfg.Frequency, cfg.Timezone = true, frequency, "UTC"
				cfg.Sender = "test@qq.com"
				cfg.Hour, cfg.Minute, cfg.Weekday = 20, 0, 5
				start := cfg.boundary(now, false)
				notificationTestInsert(t, s, start-60, "outside", 1_000_000)
				notificationTestInsert(t, s, start, "before-registration", 1_000_000)
				if _, err := s.updateUsagePricing(ctx, usagePricing{Prices: map[string]usageModelPrice{"model": {Input: 1, Output: 1}}}); err != nil {
					t.Fatal(err)
				}
				sub := notificationSubscription{Key: "test-client-key", Email: "user@example.com", Enabled: true}
				var id string
				if mode == "settings-first" {
					if err := s.notificationSettingsPut(ctx, cfg, "fake-authorization", false, now); err != nil {
						t.Fatal(err)
					}
				} else if mode == "subscription-disabled" {
					sub.Enabled = false
				}
				id, err = s.notificationSubscriptionPut(ctx, sub, false, now)
				if err != nil {
					t.Fatal(err)
				}
				if mode != "settings-first" {
					if err := s.notificationSettingsPut(ctx, cfg, "fake-authorization", false, now); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "subscription-disabled" {
					sub.ID, sub.Enabled = id, true
					if _, err := s.notificationSubscriptionPut(ctx, sub, false, now); err != nil {
						t.Fatal(err)
					}
				}
				for attempt := 0; attempt < 2; attempt++ {
					h, err := s.notificationHistoryGet(ctx, id, now)
					if err != nil || len(h.Current) != 1 || len(h.Periods) != 0 {
						t.Fatal("首期查询失败", err)
					}
					p := h.Current[0]
					if p.Start != start || p.Partial || p.Requests != 1 || p.Tokens != 2_000_000 || p.Cost != 2 || p.Unpriced != 0 {
						t.Fatal("首期未覆盖已有用量")
					}
					if attempt == 0 {
						path := s.path
						if err := s.Close(); err != nil {
							t.Fatal(err)
						}
						s, err = NewSQLiteStore(path)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
			})
		}
	}
}

func TestNotificationFirstSegmentBoundaries(t *testing.T) {
	ctx := context.Background()
	for _, timezone := range []string{"UTC", "Asia/Hong_Kong", "America/New_York"} {
		for _, frequency := range []string{"daily", "weekly"} {
			t.Run(timezone+"/"+frequency, func(t *testing.T) {
				s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "usage.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				cfg := defaultNotificationSettings()
				cfg.Enabled = true
				cfg.Timezone = timezone
				cfg.Frequency = frequency
				cfg.Hour = 3
				cfg.Minute = 0
				cfg.Weekday = 0
				loc, err := time.LoadLocation(timezone)
				if err != nil {
					t.Fatal(err)
				}
				edge := time.Date(2026, 3, 8, 3, 0, 0, 0, loc).Unix()
				for i, now := range []int64{edge - 1, edge, edge + 1} {
					sub := notificationSubscription{ID: notificationID(), Key: "test-key", Email: "user@example.com", Enabled: true}
					if err := insertNotificationSegment(ctx, s.db, sub, cfg, now); err != nil {
						t.Fatal(err)
					}
					var start, cursor int64
					if err := s.db.QueryRow("SELECT start,cursor FROM notification_segments WHERE subscription=?", sub.ID).Scan(&start, &cursor); err != nil {
						t.Fatal(err)
					}
					if start != cfg.boundary(now, false) || cursor != start {
						t.Fatalf("边界样例 %d 起点错误", i)
					}
					if err := insertNotificationSegment(ctx, s.db, sub, cfg, now); err != nil {
						t.Fatal(err)
					}
					if err := s.db.QueryRow("SELECT start FROM notification_segments WHERE subscription=? ORDER BY id DESC LIMIT 1", sub.ID).Scan(&start); err != nil {
						t.Fatal(err)
					}
					if start != floorUnix(now, 60)+60 {
						t.Fatal("后续分段不应回溯")
					}
				}
			})
		}
	}
}

func TestNotificationRestoreFirstPeriod(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC).Unix()
	for _, scenario := range []string{"missing-open", "open", "future-start", "expired", "advanced", "closed", "disabled", "deleted", "second-segment", "frozen", "aligned", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			s, id := notificationTestStore(t, now)
			cfg := s.notifications.settings
			start := now - 120
			if scenario == "future-start" {
				start = now + 60
			}
			if scenario == "expired" {
				start = now - 2*86400
			}
			if scenario == "aligned" {
				start = cfg.boundary(now, false)
			}
			cursor, until := start, int64(0)
			if scenario == "advanced" {
				cursor = start + 60
			}
			if scenario == "closed" {
				until = now + 60
			}
			if _, err := s.db.Exec("UPDATE notification_segments SET start=?,cursor=?,until_at=? WHERE subscription=?", start, cursor, until, id); err != nil {
				t.Fatal(err)
			}
			var seg notificationSegment
			for _, v := range s.notifications.segments {
				seg = v
			}
			seg.Start, seg.Cursor, seg.Until = start, cursor, until
			if scenario == "second-segment" {
				if err := insertNotificationSegment(ctx, s.db, notificationSubscription{ID: id, Enabled: true}, cfg, now); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "disabled" {
				if _, err := s.db.Exec("UPDATE notification_subscriptions SET enabled=0 WHERE id=?", id); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "deleted" {
				if _, err := s.db.Exec("UPDATE notification_subscriptions SET deleted=1 WHERE id=?", id); err != nil {
					t.Fatal(err)
				}
			}
			hasOpen := scenario != "missing-open" && scenario != "future-start"
			if hasOpen {
				_, end := seg.period(start)
				if err := ensureNotificationPeriod(ctx, s.db, seg, start, end); err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.Exec("UPDATE notification_periods SET email='original@example.com',note='原备注',key_label='原缩略值' WHERE segment=?", seg.ID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "frozen" {
				if _, err := s.db.Exec("UPDATE notification_periods SET status='sent',cost=3,attempts=1 WHERE segment=?", seg.ID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "rollback" {
				if _, err := s.db.Exec("CREATE TRIGGER reject_first_restore BEFORE UPDATE ON notification_segments BEGIN SELECT RAISE(ABORT,'test rollback'); END"); err != nil {
					t.Fatal(err)
				}
			}
			shouldChange := scenario == "missing-open" || scenario == "open" || scenario == "future-start"
			for attempt := 0; attempt < 2; attempt++ {
				err := s.withConn(ctx, func(conn *sql.Conn) error { return restoreNotificationFirstPeriods(ctx, conn, now) })
				if (err != nil) != (scenario == "rollback") {
					t.Fatal("首期迁移结果异常", err)
				}
				var gotStart, gotCursor int64
				if err := s.db.QueryRow("SELECT start,cursor FROM notification_segments WHERE id=?", seg.ID).Scan(&gotStart, &gotCursor); err != nil {
					t.Fatal(err)
				}
				want, wantCursor := start, cursor
				if shouldChange {
					want = cfg.boundary(start-1, false)
					wantCursor = want
				}
				if gotStart != want || gotCursor != wantCursor {
					t.Fatal("首段迁移或重复调用错误")
				}
				if hasOpen {
					var pID, email, note, label, status string
					var pStart int64
					var partial bool
					var cost float64
					if err := s.db.QueryRow("SELECT id,start,email,note,key_label,partial,status,cost FROM notification_periods WHERE segment=?", seg.ID).Scan(&pID, &pStart, &email, &note, &label, &partial, &status, &cost); err != nil {
						t.Fatal(err)
					}
					if pID != notificationPeriodID(seg.ID, want) || pStart != want || email != "original@example.com" || note != "原备注" || label != "原缩略值" {
						t.Fatal("首期 ID 或收件快照发生错误")
					}
					if shouldChange && partial {
						t.Fatal("补齐后不应仍为非完整首期")
					}
					if scenario == "frozen" && (status != "sent" || cost != 3) {
						t.Fatal("冻结快照被改写")
					}
				}
			}
		})
	}
}

func TestNotificationFirstPeriodUpgradeOnOpen(t *testing.T) {
	ctx := context.Background()
	now := floorUnix(time.Now().Unix(), 60)
	s, id := notificationTestStore(t, now)
	cfg := s.notifications.settings
	start := cfg.boundary(now, false) + 60
	notificationTestInsert(t, s, cfg.boundary(now, false), "historical", 100)
	if _, err := s.db.Exec("UPDATE notification_segments SET start=?,cursor=? WHERE subscription=?", start, start, id); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE notification_segments SET schedule=? WHERE subscription=?", encoded, id); err != nil {
		t.Fatal(err)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, err := s.notificationHistoryGet(ctx, id, now)
	if err != nil || len(h.Current) != 1 || h.Current[0].Requests != 1 || h.Current[0].Start != cfg.boundary(now, false) {
		t.Fatal("真实重开未恢复首期已有数据", err)
	}
}
