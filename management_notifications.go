package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// notificationManagement 统一处理通知接口的请求体、超时和错误响应，不参与路由分发。
func notificationManagement(req managementRequest, input any, run func(context.Context, *SQLiteStore) (managementResponse, error)) ([]byte, error) {
	store := currentStore()
	if store == nil {
		return okEnvelope(managementErrorResponse(503, "用量插件尚未就绪"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if input != nil && (len(req.Body) > 64*1024 || json.Unmarshal(req.Body, input) != nil) {
		return okEnvelope(managementErrorResponse(400, "请求 JSON 无效"))
	}
	response, err := run(ctx, store)
	if err != nil {
		// SQLite 错误可能带参数或内部信息，不直接返回。
		message := err.Error()
		if strings.Contains(message, "SQL") || strings.Contains(message, "sqlite") || strings.Contains(message, "constraint") {
			message = "通知数据操作失败"
		}
		return okEnvelope(managementErrorResponse(400, message))
	}
	return okEnvelope(response)
}

func notificationSettingsGet() ([]byte, error) {
	return notificationManagement(managementRequest{}, nil, func(ctx context.Context, store *SQLiteStore) (managementResponse, error) {
		settings, err := store.notificationSettingsGet(ctx)
		return jsonManagementResponse(200, settings), err
	})
}

func notificationSettingsPut(req managementRequest) ([]byte, error) {
	var input struct {
		notificationSettings
		Password      string `json:"password"`
		ClearPassword bool   `json:"clear_password"`
	}
	return notificationManagement(req, &input, func(ctx context.Context, store *SQLiteStore) (managementResponse, error) {
		if err := store.notificationSettingsPut(ctx, input.notificationSettings, input.Password, input.ClearPassword, time.Now().Unix()); err != nil {
			return managementResponse{}, err
		}
		settings, err := store.notificationSettingsGet(ctx)
		return jsonManagementResponse(200, settings), err
	})
}

func notificationSubscriptionsGet() ([]byte, error) {
	return notificationManagement(managementRequest{}, nil, func(ctx context.Context, store *SQLiteStore) (managementResponse, error) {
		var subscriptions []notificationSubscription
		err := store.withConn(ctx, func(conn *sql.Conn) error {
			var err error
			subscriptions, err = notificationSubscriptions(ctx, conn)
			return err
		})
		return jsonManagementResponse(200, map[string]any{"subscriptions": subscriptions}), err
	})
}

func notificationSubscriptionsPost(req managementRequest) ([]byte, error) {
	return notificationSubscriptionUpdate(req, true, false)
}

func notificationSubscriptionsPatch(req managementRequest) ([]byte, error) {
	return notificationSubscriptionUpdate(req, false, false)
}

func notificationSubscriptionsDelete(req managementRequest) ([]byte, error) {
	return notificationSubscriptionUpdate(req, false, true)
}

func notificationSubscriptionUpdate(req managementRequest, create, remove bool) ([]byte, error) {
	var input notificationSubscription
	return notificationManagement(req, &input, func(ctx context.Context, store *SQLiteStore) (managementResponse, error) {
		if create {
			input.ID = ""
		} else if input.ID == "" {
			return managementResponse{}, errors.New("缺少订阅 ID")
		}
		id, err := store.notificationSubscriptionPut(ctx, input, remove, time.Now().Unix())
		return jsonManagementResponse(200, map[string]string{"id": id}), err
	})
}

func notificationHistoryPost(req managementRequest) ([]byte, error) {
	var input struct {
		ID string `json:"id"`
	}
	return notificationManagement(req, &input, func(ctx context.Context, store *SQLiteStore) (managementResponse, error) {
		history, err := store.notificationHistoryGet(ctx, input.ID, time.Now().Unix())
		return jsonManagementResponse(200, history), err
	})
}

func notificationRetryPost(req managementRequest) ([]byte, error) {
	var input struct {
		ID string `json:"id"`
	}
	return notificationManagement(req, &input, func(ctx context.Context, store *SQLiteStore) (managementResponse, error) {
		err := store.retryNotification(ctx, input.ID)
		return jsonManagementResponse(200, map[string]bool{"ok": true}), err
	})
}

func notificationTestEmailPost(req managementRequest) ([]byte, error) {
	var input struct {
		Email string `json:"email"`
	}
	return notificationManagement(req, &input, func(ctx context.Context, store *SQLiteStore) (managementResponse, error) {
		if !validNotificationEmail(input.Email) {
			return managementResponse{}, errors.New("测试收件邮箱无效")
		}
		var cfg notificationSettings
		if err := store.withConn(ctx, func(_ *sql.Conn) error { cfg = store.notifications.settings; return nil }); err != nil {
			return managementResponse{}, err
		}
		if cfg.Sender == "" || cfg.Password == "" {
			return managementResponse{}, errors.New("请先保存 SMTP 配置")
		}
		end := cfg.boundary(time.Now().Unix(), false)
		sample := notificationPeriod{
			Start: cfg.boundary(end-1, false), End: end, Timezone: cfg.Timezone,
			KeyLabel: notificationMask("sk-9f2b7k4m6q8r3c1a7x9q"), Note: "个人项目",
			Requests: 1280, Tokens: 2_468_000, Cost: 12.3456,
		}
		unknown, err := store.notifications.send(ctx, cfg, notificationMail{ID: notificationID(), To: input.Email, Periods: []notificationPeriod{sample}})
		if err != nil {
			return jsonManagementResponse(502, map[string]any{"error": err.Error(), "unknown": unknown}), nil
		}
		return jsonManagementResponse(200, map[string]bool{"ok": true}), nil
	})
}
