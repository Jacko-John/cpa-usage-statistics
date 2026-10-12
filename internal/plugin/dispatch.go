package plugin

import (
	"time"

	"github.com/Jacko-John/cpa-usage-statistics/internal/protocol"
)

const (
	pluginSchemaVersion uint32 = 2
	pluginID                   = protocol.PluginID
	pluginName                 = "Jacko Usage Statistics"

	// 管理路由由宿主统一挂载到 /v0/management。
	managementUsagePath                     = "/plugins/" + pluginID + "/usage"
	managementUsageCredentialWindowsPath    = managementUsagePath + "/credential-windows"
	managementPricingPath                   = "/plugins/" + pluginID + "/pricing"
	managementSafetyKeysPath                = "/plugins/" + pluginID + "/safety/keys"
	managementSafetyHistoryPath             = "/plugins/" + pluginID + "/safety/history"
	managementSafetyLimitsPath              = "/plugins/" + pluginID + "/safety/limits"
	managementSafetyDisabledPath            = "/plugins/" + pluginID + "/safety/disabled"
	managementNotificationsPath             = "/plugins/" + pluginID + "/notifications"
	managementNotificationSettingsPath      = managementNotificationsPath + "/settings"
	managementNotificationSubscriptionsPath = managementNotificationsPath + "/subscriptions"
	managementNotificationHistoryPath       = managementNotificationsPath + "/history"
	managementNotificationTestEmailPath     = managementNotificationsPath + "/test-email"
	managementNotificationRetryPath         = managementNotificationsPath + "/retry"
	insertTimeout                           = 5 * time.Second
)

func Handle(method string, request []byte, metadata Metadata) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configurePlugin(request); err != nil {
			return nil, err
		}
		return protocol.OK(registerResponse(metadata))
	case "usage.handle":
		return handleUsage(request)
	case "request.intercept_before":
		return interceptRequestBefore(request)
	case "request.intercept_after":
		return interceptRequestAfter(request)
	case "management.register":
		return protocol.OK(managementRegistration())
	case "management.handle":
		return handleManagement(request)
	default:
		return protocol.Error("unknown_method", "unknown method: "+method), nil
	}
}
