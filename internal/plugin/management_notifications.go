package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Jacko-John/cpa-usage-statistics/internal/notifications"
	"github.com/Jacko-John/cpa-usage-statistics/internal/protocol"
)

// notificationManagement 统一处理通知接口的请求体、超时和错误响应，不参与路由分发。
func notificationManagement(req protocol.ManagementRequest, input any, run func(context.Context, *notifications.Service) (protocol.ManagementResponse, error)) ([]byte, error) {
	store := currentNotifications()
	if store == nil {
		return protocol.OK(managementErrorResponse(503, "用量插件尚未就绪"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if input != nil && (len(req.Body) > 64*1024 || json.Unmarshal(req.Body, input) != nil) {
		return protocol.OK(managementErrorResponse(400, "请求 JSON 无效"))
	}
	response, err := run(ctx, store)
	if err != nil {
		// SQLite 错误可能带参数或内部信息，不直接返回。
		message := err.Error()
		if strings.Contains(message, "SQL") || strings.Contains(message, "sqlite") || strings.Contains(message, "constraint") {
			message = "通知数据操作失败"
		}
		return protocol.OK(managementErrorResponse(400, message))
	}
	return protocol.OK(response)
}

func notificationSettingsGet() ([]byte, error) {
	return notificationManagement(protocol.ManagementRequest{}, nil, func(ctx context.Context, store *notifications.Service) (protocol.ManagementResponse, error) {
		settings, err := store.SettingsGet(ctx)
		return jsonManagementResponse(200, settings), err
	})
}

func notificationSettingsPut(req protocol.ManagementRequest) ([]byte, error) {
	var input struct {
		notifications.Settings
		Password      string `json:"password"`
		ClearPassword bool   `json:"clear_password"`
	}
	return notificationManagement(req, &input, func(ctx context.Context, store *notifications.Service) (protocol.ManagementResponse, error) {
		if err := store.SettingsPut(ctx, input.Settings, input.Password, input.ClearPassword, time.Now().Unix()); err != nil {
			return protocol.ManagementResponse{}, err
		}
		settings, err := store.SettingsGet(ctx)
		return jsonManagementResponse(200, settings), err
	})
}

func notificationSubscriptionsGet() ([]byte, error) {
	return notificationManagement(protocol.ManagementRequest{}, nil, func(ctx context.Context, store *notifications.Service) (protocol.ManagementResponse, error) {
		subscriptions, err := store.Subscriptions(ctx)
		return jsonManagementResponse(200, map[string]any{"subscriptions": subscriptions}), err
	})
}

func notificationSubscriptionsPost(req protocol.ManagementRequest) ([]byte, error) {
	return notificationSubscriptionUpdate(req, true, false)
}

func notificationSubscriptionsPatch(req protocol.ManagementRequest) ([]byte, error) {
	return notificationSubscriptionUpdate(req, false, false)
}

func notificationSubscriptionsDelete(req protocol.ManagementRequest) ([]byte, error) {
	return notificationSubscriptionUpdate(req, false, true)
}

func notificationSubscriptionUpdate(req protocol.ManagementRequest, create, remove bool) ([]byte, error) {
	var input notifications.Subscription
	return notificationManagement(req, &input, func(ctx context.Context, store *notifications.Service) (protocol.ManagementResponse, error) {
		if create {
			input.ID = ""
		} else if input.ID == "" {
			return protocol.ManagementResponse{}, errors.New("缺少订阅 ID")
		}
		id, err := store.SubscriptionPut(ctx, input, remove, time.Now().Unix())
		return jsonManagementResponse(200, map[string]string{"id": id}), err
	})
}

func notificationHistoryPost(req protocol.ManagementRequest) ([]byte, error) {
	var input struct {
		ID string `json:"id"`
	}
	return notificationManagement(req, &input, func(ctx context.Context, store *notifications.Service) (protocol.ManagementResponse, error) {
		history, err := store.HistoryGet(ctx, input.ID, time.Now().Unix())
		return jsonManagementResponse(200, history), err
	})
}

func notificationRetryPost(req protocol.ManagementRequest) ([]byte, error) {
	var input struct {
		ID string `json:"id"`
	}
	return notificationManagement(req, &input, func(ctx context.Context, store *notifications.Service) (protocol.ManagementResponse, error) {
		err := store.Retry(ctx, input.ID)
		return jsonManagementResponse(200, map[string]bool{"ok": true}), err
	})
}

func notificationTestEmailPost(req protocol.ManagementRequest) ([]byte, error) {
	var input struct {
		Email string `json:"email"`
	}
	return notificationManagement(req, &input, func(ctx context.Context, store *notifications.Service) (protocol.ManagementResponse, error) {
		result, err := store.TestEmail(ctx, input.Email)
		if err != nil {
			return protocol.ManagementResponse{}, err
		}
		if result.Err != nil {
			return jsonManagementResponse(502, map[string]any{"error": result.Err.Error(), "unknown": result.Unknown}), nil
		}
		return jsonManagementResponse(200, map[string]bool{"ok": true}), nil
	})
}
