package main

import "time"

const (
	abiVersion          uint32 = 1
	pluginSchemaVersion uint32 = 2
	pluginID                   = "usage-statistics"
	pluginName                 = "Usage Statistics"

	// 管理路由由宿主统一挂载到 /v0/management。
	managementUsagePath                  = "/plugins/" + pluginID + "/usage"
	managementUsageCredentialWindowsPath = managementUsagePath + "/credential-windows"
	managementPricingPath                = "/plugins/" + pluginID + "/pricing"
	managementSafetyKeysPath             = "/plugins/" + pluginID + "/safety/keys"
	managementSafetyHistoryPath          = "/plugins/" + pluginID + "/safety/history"
	managementSafetyLimitsPath           = "/plugins/" + pluginID + "/safety/limits"
	managementSafetyDisabledPath         = "/plugins/" + pluginID + "/safety/disabled"
	insertTimeout                        = 5 * time.Second
)

// 可通过 -ldflags "-X main.pluginVersion=..." 覆盖。
var (
	pluginVersion    = "0.2.0"
	pluginAuthor     = "Fwindy"
	pluginRepository = "https://github.com/Jacko-John/cpa-usage-statistics"
)

func main() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configurePlugin(request); err != nil {
			return nil, err
		}
		return okEnvelope(registerResponse())
	case "usage.handle":
		return handleUsage(request)
	case "request.intercept_before":
		return interceptRequestBefore(request)
	case "request.intercept_after":
		return interceptRequestAfter(request)
	case "management.register":
		return okEnvelope(managementRegistration())
	case "management.handle":
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}
