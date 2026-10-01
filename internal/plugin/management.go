package plugin

import (
	"encoding/json"
	"strings"

	"github.com/Jacko-John/cpa-usage-statistics/internal/protocol"
)

var jsonResponseHeaders = map[string][]string{
	"Content-Type": {"application/json; charset=utf-8"},
}

func handleManagement(request []byte) ([]byte, error) {
	var req protocol.ManagementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	path := strings.TrimPrefix(strings.TrimSpace(req.Path), "/v0/management")

	switch path {
	case managementNotificationSettingsPath:
		switch method {
		case "GET":
			return notificationSettingsGet()
		case "PUT":
			return notificationSettingsPut(req)
		}
	case managementNotificationSubscriptionsPath:
		switch method {
		case "GET":
			return notificationSubscriptionsGet()
		case "POST":
			return notificationSubscriptionsPost(req)
		case "PATCH":
			return notificationSubscriptionsPatch(req)
		case "DELETE":
			return notificationSubscriptionsDelete(req)
		}
	case managementNotificationHistoryPath:
		if method == "POST" {
			return notificationHistoryPost(req)
		}
	case managementNotificationTestEmailPath:
		if method == "POST" {
			return notificationTestEmailPost(req)
		}
	case managementNotificationRetryPath:
		if method == "POST" {
			return notificationRetryPost(req)
		}
	case managementUsagePath:
		switch method {
		case "GET":
			return usageGet(req)
		case "POST":
			return usagePost(req)
		case "DELETE":
			return usageDelete(req)
		}
	case managementUsageCredentialWindowsPath:
		if method == "POST" {
			return usageCredentialWindowsPost(req)
		}
	case managementPricingPath:
		switch method {
		case "GET":
			return usagePricingGet()
		case "PUT":
			return usagePricingPut(req)
		}
	case managementSafetyKeysPath:
		if method == "GET" {
			return safetyKeysGet()
		}
	case managementSafetyHistoryPath:
		if method == "POST" {
			return safetyHistoryPost(req)
		}
	case managementSafetyLimitsPath:
		if method == "DELETE" {
			return safetyLimitsDelete(req)
		}
	case managementSafetyDisabledPath:
		switch method {
		case "POST":
			return safetyDisabledPost(req)
		case "DELETE":
			return safetyDisabledDelete(req)
		}
	default:
		return protocol.OK(managementErrorResponse(404, "route not found"))
	}
	return protocol.OK(managementErrorResponse(405, "method not allowed"))
}

func managementErrorResponse(status int, message string) protocol.ManagementResponse {
	return jsonManagementResponse(status, protocol.ManagementError{Error: message})
}

func rawJSONManagementResponse(status int, body []byte) protocol.ManagementResponse {
	return protocol.ManagementResponse{
		StatusCode: status,
		Headers:    jsonResponseHeaders,
		Body:       body,
	}
}

func jsonManagementResponse(status int, payload any) protocol.ManagementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"error":"failed to encode response"}`)
		status = 500
	}
	return protocol.ManagementResponse{
		StatusCode: status,
		Headers:    jsonResponseHeaders,
		Body:       body,
	}
}
