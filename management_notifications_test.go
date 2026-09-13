package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotificationManagementAndRebind(t *testing.T) {
	s, _ := notificationTestStore(t, time.Now().Unix()-120)
	storeMu.Lock()
	old := globalStore
	globalStore = s
	storeMu.Unlock()
	t.Cleanup(func() { storeMu.Lock(); globalStore = old; storeMu.Unlock() })
	call := func(method, path string, payload any) managementResponse {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return callManagementForTest(t, managementRequest{Method: method, Path: managementNotificationsPath + path, Body: body})
	}
	response := call("GET", "/settings", nil)
	if response.StatusCode != 200 || strings.Contains(string(response.Body), "fake-authorization") {
		t.Fatalf("%s", response.Body)
	}
	response = call("PUT", "/settings", map[string]any{"frequency": "daily", "hour": 25, "timezone": "UTC"})
	if response.StatusCode != 400 {
		t.Fatal(response.StatusCode)
	}
	response = call("GET", "/history", nil)
	if response.StatusCode != 405 {
		t.Fatal(response.StatusCode)
	}
	input := notificationSubscription{Key: "second-key", Email: "second@example.com", Enabled: true}
	response = call("POST", "/subscriptions", input)
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body, &created); err != nil {
		t.Fatal(err)
	}
	if call("POST", "/subscriptions", input).StatusCode != 400 {
		t.Fatal("duplicate accepted")
	}
	if call("DELETE", "/subscriptions", map[string]string{"id": created.ID}).StatusCode != 200 {
		t.Fatal("delete failed")
	}
	response = call("POST", "/subscriptions", input)
	var rebound struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body, &rebound); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || rebound.ID != created.ID {
		t.Fatal(string(response.Body))
	}
	calls := 0
	s.notifications.send = func(_ context.Context, _ notificationSettings, m notificationMail) (bool, error) {
		calls++
		if m.To != "test@example.com" {
			t.Error("wrong test recipient")
		}
		return false, nil
	}
	if call("POST", "/test-email", map[string]string{"email": "test@example.com"}).StatusCode != 200 || calls != 1 {
		t.Fatal("test mail failed")
	}
}

func TestNotificationTestEmailSample(t *testing.T) {
	for _, frequency := range []string{"daily", "weekly"} {
		t.Run(frequency, func(t *testing.T) {
			s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "usage.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			cfg := defaultNotificationSettings()
			cfg.Frequency = frequency
			cfg.Timezone = "Asia/Hong_Kong"
			cfg.Sender = "sender@example.com"
			if err := s.notificationSettingsPut(context.Background(), cfg, "test-authorization", false, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
			storeMu.Lock()
			old := globalStore
			globalStore = s
			storeMu.Unlock()
			t.Cleanup(func() { storeMu.Lock(); globalStore = old; storeMu.Unlock() })

			var sample notificationMail
			calls := 0
			s.notifications.send = func(_ context.Context, _ notificationSettings, message notificationMail) (bool, error) {
				calls++
				sample = message
				return false, nil
			}
			response := callManagementForTest(t, managementRequest{
				Method: "POST", Path: managementNotificationTestEmailPath,
				Body: []byte(`{"email":"receiver@example.com"}`),
			})
			if response.StatusCode != 200 || calls != 1 || sample.To != "receiver@example.com" || len(sample.Periods) != 1 {
				t.Fatal("测试接口未发送单期完整样例")
			}
			period := sample.Periods[0]
			if period.Timezone != cfg.Timezone || period.End > time.Now().Unix() || period.End != cfg.boundary(period.End, false) || period.Start != cfg.boundary(period.End-1, false) || period.Partial {
				t.Fatal("样例未使用当前计划下的完整已结束周期")
			}
			if period.Requests != 1280 || period.Tokens != 2_468_000 || period.Cost != 12.3456 || period.KeyLabel != "sk-9f2b7…7x9q" {
				t.Fatal("样例用量或脱敏 Key 不正确")
			}
			subject, parts := notificationMessagePartsForTest(t, notificationMessage(sample, cfg.Sender))
			regular := notificationMail{ID: "scheduled-report", To: sample.To, Periods: sample.Periods}
			regularSubject, regularParts := notificationMessagePartsForTest(t, notificationMessage(regular, cfg.Sender))
			if subject != "你的 API 用量报告" || subject != regularSubject {
				t.Fatal("测试邮件主题必须与正式通知一致")
			}
			for kind, content := range parts {
				if content != regularParts[kind] {
					t.Fatal("测试邮件正文必须与相同数据的正式通知一致")
				}
				for _, text := range []string{"个人项目", period.KeyLabel, cfg.Timezone, "$12.3456", "1,280", "2,468,000"} {
					if !strings.Contains(content, text) {
						t.Errorf("样例邮件缺少 %q", text)
					}
				}
				for _, marker := range []string{"示例", "测试邮件", "预览"} {
					if strings.Contains(content, marker) {
						t.Fatal("测试邮件不能添加专属展示内容")
					}
				}
			}
			for _, table := range []string{"usage_records", "notification_subscriptions", "notification_segments", "notification_periods"} {
				var count int
				if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("空库发送样例不应在业务表 %s 新增记录", table)
				}
			}
		})
	}
}

func TestNotificationRouteDispatch(t *testing.T) {
	storeMu.Lock()
	old := globalStore
	globalStore = nil
	storeMu.Unlock()
	t.Cleanup(func() { storeMu.Lock(); globalStore = old; storeMu.Unlock() })

	for _, route := range registeredManagementRoutes {
		if !strings.HasPrefix(route.Path, managementNotificationsPath+"/") {
			continue
		}
		response := callManagementForTest(t, managementRequest{Method: route.Method, Path: "/v0/management" + route.Path})
		if response.StatusCode != 503 {
			t.Fatalf("registered %s %s: %d", route.Method, route.Path, response.StatusCode)
		}
		response = callManagementForTest(t, managementRequest{Method: "OPTIONS", Path: route.Path})
		if response.StatusCode != 405 {
			t.Fatalf("unsupported method %s: %d", route.Path, response.StatusCode)
		}
	}
	for _, path := range []string{managementNotificationsPath, managementNotificationSettingsPath + "/", managementNotificationSettingsPath + "/extra", managementNotificationsPath + "/unknown"} {
		response := callManagementForTest(t, managementRequest{Method: "GET", Path: path})
		if response.StatusCode != 404 {
			t.Fatalf("unknown path %s: %d", path, response.StatusCode)
		}
	}
}

func TestNotificationBoundedRetries(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 19, 58, 0, 0, time.UTC).Unix()
	s, id := notificationTestStore(t, base)
	if err := s.notificationTick(ctx, base+120); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.notifications.send = func(context.Context, notificationSettings, notificationMail) (bool, error) {
		calls++
		return false, errors.New("SMTP rejected")
	}
	for i := 0; i < 4; i++ {
		if _, err := s.db.Exec(`UPDATE notification_periods SET retry_at=0`); err != nil {
			t.Fatal(err)
		}
		if err := s.sendNotificationBatch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	h, err := s.notificationHistoryGet(ctx, id, base+120)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 || h.Periods[0].Status != "failed" || h.Periods[0].Attempts != 4 {
		t.Fatal(calls, h)
	}
	if err := s.sendNotificationBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatal("unbounded retry")
	}
}
