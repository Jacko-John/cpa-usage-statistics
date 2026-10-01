package plugin

type Metadata struct {
	Version    string
	Author     string
	Repository string
}

type registerResponsePayload struct {
	SchemaVersion uint32           `json:"schema_version"`
	Metadata      metadataInfo     `json:"metadata"`
	Capabilities  capabilitiesInfo `json:"capabilities"`
}

type metadataInfo struct {
	Name             string            `json:"Name"`
	Version          string            `json:"Version"`
	Author           string            `json:"Author"`
	GitHubRepository string            `json:"GitHubRepository"`
	Logo             string            `json:"Logo"`
	ConfigFields     []configFieldInfo `json:"ConfigFields"`
}

type configFieldInfo struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Description string `json:"Description"`
}

type capabilitiesInfo struct {
	RequestInterceptor bool `json:"request_interceptor"`
	UsagePlugin        bool `json:"usage_plugin"`
	ManagementAPI      bool `json:"management_api"`
}

type managementRegistrationPayload struct {
	Routes []managementRoute `json:"routes"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description"`
}

var registrationConfigFields = [...]configFieldInfo{
	{Name: "data_dir", Type: "string", Description: "Directory for usage.db. Defaults to ~/.cli-proxy-api/plugins/usage-statistics."},
	{Name: "retention_days", Type: "integer", Description: "Delete usage records older than this many days. 0 disables cleanup."},
	{Name: "rate_limit_enabled", Type: "boolean", Description: "Block subsequent requests by API key after repeated cyber_policy failures. Defaults to false."},
	{Name: "threshold", Type: "integer", Description: "cyber_policy failures required within the sliding window. Defaults to 3."},
	{Name: "window", Type: "integer", Description: "Sliding-window duration in seconds. Defaults to 600."},
	{Name: "cooldown", Type: "integer", Description: "Rate-limit duration in seconds. Defaults to 1800."},
}

var registeredManagementRoutes = [...]managementRoute{
	{Method: "GET", Path: managementNotificationSettingsPath, Description: "读取用量通知设置。"},
	{Method: "PUT", Path: managementNotificationSettingsPath, Description: "配置通知时间与 QQ SMTP。"},
	{Method: "GET", Path: managementNotificationSubscriptionsPath, Description: "列出 Key 邮箱订阅。"},
	{Method: "POST", Path: managementNotificationSubscriptionsPath, Description: "新增通知订阅。"},
	{Method: "PATCH", Path: managementNotificationSubscriptionsPath, Description: "修改通知订阅。"},
	{Method: "DELETE", Path: managementNotificationSubscriptionsPath, Description: "删除订阅并保留周期历史。"},
	{Method: "POST", Path: managementNotificationHistoryPath, Description: "读取当前与最近十期通知用量。"},
	{Method: "POST", Path: managementNotificationTestEmailPath, Description: "发送测试邮件。"},
	{Method: "POST", Path: managementNotificationRetryPath, Description: "重试指定周期邮件。"},
	{Method: "GET", Path: managementUsagePath, Description: "Query at most 100 recent usage events."},
	{Method: "POST", Path: managementUsagePath, Description: "Read precomputed usage statistics for a fixed range."},
	{Method: "DELETE", Path: managementUsagePath, Description: "Delete persisted usage records by id."},
	{Method: "POST", Path: managementUsageCredentialWindowsPath, Description: "Read precomputed credential usage for quota windows."},
	{Method: "GET", Path: managementPricingPath, Description: "Read the persisted usage pricing configuration."},
	{Method: "PUT", Path: managementPricingPath, Description: "Update usage pricing and rebuild precomputed costs."},
	{Method: "GET", Path: managementSafetyKeysPath, Description: "List API-key safety counters and current limit state."},
	{Method: "POST", Path: managementSafetyHistoryPath, Description: "Query safety trigger and limit history for one API key."},
	{Method: "DELETE", Path: managementSafetyLimitsPath, Description: "Release current API-key safety limits."},
	{Method: "POST", Path: managementSafetyDisabledPath, Description: "Disable recorded API keys."},
	{Method: "DELETE", Path: managementSafetyDisabledPath, Description: "Enable disabled API keys and release their current safety limits."},
}

func registerResponse(metadata Metadata) registerResponsePayload {
	schemaVersion := negotiatedSchemaVersion.Load()
	if schemaVersion == 0 {
		schemaVersion = 1
	}
	return registerResponsePayload{
		SchemaVersion: schemaVersion,
		Metadata: metadataInfo{
			Name:             pluginName,
			Version:          metadata.Version,
			Author:           metadata.Author,
			GitHubRepository: metadata.Repository,
			ConfigFields:     registrationConfigFields[:],
		},
		Capabilities: capabilitiesInfo{
			RequestInterceptor: schemaVersion >= 2,
			UsagePlugin:        true,
			ManagementAPI:      true,
		},
	}
}

func managementRegistration() managementRegistrationPayload {
	return managementRegistrationPayload{Routes: registeredManagementRoutes[:]}
}
