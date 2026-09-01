package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

type safetyHistoryJSONItem struct {
	UsageRecordID     string          `json:"usage_record_id"`
	Provider          string          `json:"provider"`
	Model             string          `json:"model"`
	FailureStatusCode int             `json:"failure_status_code"`
	FailureBody       json.RawMessage `json:"failure_body"`
	Status            string          `json:"status"`
}

type safetyHistoryJSONResponse struct {
	History []safetyHistoryJSONItem `json:"history"`
}

func TestFailureBodyJSON(t *testing.T) {
	tests := []struct {
		name string
		body failureBody
		want string
	}{
		{name: "json object", body: `{"error":{"code":"cyber_policy"}}`, want: `{"failure_body":{"error":{"code":"cyber_policy"}}}`},
		{name: "plain text", body: "rate limited", want: `{"failure_body":"rate limited"}`},
		{name: "empty", want: `{"failure_body":null}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(struct {
				FailureBody failureBody `json:"failure_body"`
			}{FailureBody: test.body})
			if err != nil {
				t.Fatalf("marshal failure body: %v", err)
			}
			if string(encoded) != test.want {
				t.Fatalf("encoded = %s, want %s", encoded, test.want)
			}
		})
	}

	emptyUsage, err := json.Marshal(RequestDetail{})
	if err != nil {
		t.Fatalf("marshal empty usage: %v", err)
	}
	if bytes.Contains(emptyUsage, []byte(`"failure_body"`)) {
		t.Fatalf("empty usage contains failure_body: %s", emptyUsage)
	}
}

func TestParseSafetyConfig(t *testing.T) {
	defaults := parseConfig(nil).Safety
	if defaults.RateLimitEnabled || defaults.Threshold != 3 || defaults.Window != 600 || defaults.Cooldown != 1800 {
		t.Fatalf("default safety config = %+v", defaults)
	}

	configured := parseConfig([]byte(`
rate_limit_enabled: true
threshold: 5
window: 720
cooldown: 2700
`)).Safety
	if !configured.RateLimitEnabled || configured.Threshold != 5 || configured.Window != 720 || configured.Cooldown != 2700 {
		t.Fatalf("configured safety = %+v", configured)
	}

	aliased := parseConfig([]byte(`
启用限流: true
触发阈值: 4
滑动窗口秒数: 300
限流时长秒数: 900
`)).Safety
	if !aliased.RateLimitEnabled || aliased.Threshold != 4 || aliased.Window != 300 || aliased.Cooldown != 900 {
		t.Fatalf("aliased safety = %+v", aliased)
	}

	converted := parseConfig([]byte(`
rate_limit_enabled: true
threshold: 3.5
window: "600"
cooldown: "1800"
`)).Safety
	if !converted.RateLimitEnabled || converted.Threshold != 3 || converted.Window != 600 || converted.Cooldown != 1800 {
		t.Fatalf("converted safety = %+v", converted)
	}

	invalid := []string{
		"rate_limit_enabled: true\nthreshold: 0\n",
		"rate_limit_enabled: true\nthreshold: -1\n",
		"rate_limit_enabled: true\nthreshold: \"3.5\"\n",
		"rate_limit_enabled: true\nwindow: 10m\n",
		"rate_limit_enabled: true\nwindow: 0\n",
		"rate_limit_enabled: true\nwindow: -1\n",
		"rate_limit_enabled: true\ncooldown: 30s\n",
		"rate_limit_enabled: true\ncooldown: 0\n",
		"rate_limit_enabled: true\ncooldown: -1\n",
		"rate_limit_enabled: true\ncooldown: 0.5\n",
	}
	for _, raw := range invalid {
		cfg := parseConfig([]byte(raw)).Safety
		if cfg.RateLimitEnabled {
			t.Fatalf("invalid config kept rate limiting enabled: %q => %+v", raw, cfg)
		}
	}
}

func TestRegistrationNegotiatesSchemaAndInterceptorCapability(t *testing.T) {
	previousSafety := safety
	previousSchema := negotiatedSchemaVersion.Load()
	previousRetention := retentionDays.Load()
	safety = &safetyLimiter{states: make(map[string]SafetyState)}
	t.Cleanup(func() {
		closeStore()
		safety = previousSafety
		negotiatedSchemaVersion.Store(previousSchema)
		retentionDays.Store(previousRetention)
	})

	configureForSchemaTest(t, 4, filepath.Join(t.TempDir(), "v4"))
	registration := registerResponse()
	if registration.SchemaVersion != 2 || !registration.Capabilities.RequestInterceptor {
		t.Fatalf("schema 4 registration = %+v", registration)
	}
	fields := registration.Metadata.ConfigFields
	if len(fields) != 6 ||
		fields[2].Name != "rate_limit_enabled" || fields[2].Type != "boolean" ||
		fields[3].Name != "threshold" || fields[3].Type != "integer" ||
		fields[4].Name != "window" || fields[4].Type != "integer" ||
		fields[5].Name != "cooldown" || fields[5].Type != "integer" {
		t.Fatalf("registration config fields = %+v", fields)
	}
	if !safety.config.RateLimitEnabled {
		t.Fatal("rate limiting was disabled for schema 4")
	}

	closeStore()
	configureForSchemaTest(t, 1, filepath.Join(t.TempDir(), "v1"))
	registration = registerResponse()
	if registration.SchemaVersion != 1 || registration.Capabilities.RequestInterceptor {
		t.Fatalf("schema 1 registration = %+v", registration)
	}
	if safety.config.RateLimitEnabled {
		t.Fatal("rate limiting remained enabled for schema 1")
	}
}

func TestSafetyHistoryRouteUsesPost(t *testing.T) {
	var methods []string
	for _, route := range managementRegistration().Routes {
		if route.Path == managementSafetyHistoryPath {
			methods = append(methods, route.Method)
		}
	}
	if len(methods) != 1 || methods[0] != "POST" {
		t.Fatalf("safety history methods = %v", methods)
	}
}

func TestSafetyManagementRoutes(t *testing.T) {
	store := newTestStore(t)
	cfg := safetyConfig{RateLimitEnabled: true, Threshold: 1, Window: 600, Cooldown: 1800}
	limiter := configuredLimiter(t, store, cfg)
	now := time.Now()
	record := cyberRecord("record-1", "client-key", now)
	record.Provider = "codex"
	record.Model = "gpt-5-codex"
	record.FailureStatusCode = 400
	if err := limiter.storeUsage(t.Context(), store, record, now); err != nil {
		t.Fatalf("storeUsage: %v", err)
	}
	useGlobalSafetyStore(t, limiter, store)

	usageResponse := callManagementForTest(t, managementRequest{
		Method: "GET",
		Path:   "/v0/management" + managementUsagePath,
	})
	if usageResponse.StatusCode != 200 {
		t.Fatalf("usage status = %d body=%s", usageResponse.StatusCode, usageResponse.Body)
	}
	var usage map[string]map[string][]struct {
		FailureBody json.RawMessage `json:"failure_body"`
	}
	if err := json.Unmarshal(usageResponse.Body, &usage); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	details := usage["client-key"]["gpt-5-codex"]
	if len(details) != 1 || failureCodeForTest(t, details[0].FailureBody) != cyberPolicyCode {
		t.Fatalf("usage response = %s", usageResponse.Body)
	}

	keysResponse := callManagementForTest(t, managementRequest{
		Method: "GET",
		Path:   "/v0/management" + managementSafetyKeysPath,
	})
	if keysResponse.StatusCode != 200 {
		t.Fatalf("keys status = %d body=%s", keysResponse.StatusCode, keysResponse.Body)
	}
	var keys safetyKeysResponse
	if err := json.Unmarshal(keysResponse.Body, &keys); err != nil {
		t.Fatalf("unmarshal keys: %v", err)
	}
	if len(keys.Keys) != 1 || keys.Keys[0].APIKey != "client-key" || !keys.Keys[0].Blocked || keys.Keys[0].WindowTriggerCount != 1 {
		t.Fatalf("keys response = %+v", keys)
	}

	legacyHistoryResponse := callManagementForTest(t, managementRequest{
		Method: "GET",
		Path:   "/v0/management" + managementSafetyHistoryPath,
		Query:  map[string][]string{"api_key": {"client-key"}},
	})
	if legacyHistoryResponse.StatusCode != 405 {
		t.Fatalf("legacy history status = %d body=%s", legacyHistoryResponse.StatusCode, legacyHistoryResponse.Body)
	}

	historyBody, err := json.Marshal(safetyHistoryRequest{APIKey: "client-key", BlockedOnly: true})
	if err != nil {
		t.Fatalf("marshal history body: %v", err)
	}
	historyResponse := callManagementForTest(t, managementRequest{
		Method: "POST",
		Path:   "/v0/management" + managementSafetyHistoryPath,
		Body:   historyBody,
	})
	if historyResponse.StatusCode != 200 {
		t.Fatalf("history status = %d body=%s", historyResponse.StatusCode, historyResponse.Body)
	}
	var history safetyHistoryJSONResponse
	if err := json.Unmarshal(historyResponse.Body, &history); err != nil {
		t.Fatalf("unmarshal history: %v", err)
	}
	if len(history.History) != 1 || history.History[0].UsageRecordID != "record-1" || history.History[0].Status != "active" {
		t.Fatalf("history response = %+v", history)
	}
	item := history.History[0]
	if item.Provider != "codex" || item.Model != "gpt-5-codex" || item.FailureStatusCode != 400 || failureCodeForTest(t, item.FailureBody) != cyberPolicyCode {
		t.Fatalf("history request fields = %+v", item)
	}
	for _, field := range []string{`"auth_id"`, `"tokens"`, `"latency_ms"`} {
		if bytes.Contains(historyResponse.Body, []byte(field)) {
			t.Fatalf("history response contains unapproved field %s: %s", field, historyResponse.Body)
		}
	}

	releaseBody, err := json.Marshal(releaseSafetyRequest{APIKeys: []string{"client-key"}})
	if err != nil {
		t.Fatalf("marshal release body: %v", err)
	}
	releaseResponse := callManagementForTest(t, managementRequest{
		Method: "DELETE",
		Path:   "/v0/management" + managementSafetyLimitsPath,
		Body:   releaseBody,
	})
	if releaseResponse.StatusCode != 200 {
		t.Fatalf("release status = %d body=%s", releaseResponse.StatusCode, releaseResponse.Body)
	}
	var release SafetyReleaseResult
	if err := json.Unmarshal(releaseResponse.Body, &release); err != nil {
		t.Fatalf("unmarshal release: %v", err)
	}
	if len(release.Released) != 1 || release.Released[0] != "client-key" {
		t.Fatalf("release response = %+v", release)
	}
	state := safetyStateForTest(limiter, "client-key")
	if state.BlockedUntil != 0 || len(state.RecentTriggerTimes) != 0 {
		t.Fatalf("state after management release = %+v", state)
	}

	ids := NewStableStringSet(1)
	ids.Add(record.ID)
	deleteResult, err := store.Delete(t.Context(), ids)
	if err != nil {
		t.Fatalf("delete linked usage: %v", err)
	}
	if deleteResult.Deleted != 0 || len(deleteResult.Missing) != 1 || deleteResult.Missing[0] != record.ID {
		t.Fatalf("delete linked usage result = %+v", deleteResult)
	}
	retainedResponse := callManagementForTest(t, managementRequest{
		Method: "POST",
		Path:   "/v0/management" + managementSafetyHistoryPath,
		Body:   historyBody,
	})
	if retainedResponse.StatusCode != 200 {
		t.Fatalf("retained history status = %d body=%s", retainedResponse.StatusCode, retainedResponse.Body)
	}
	var retainedHistory safetyHistoryJSONResponse
	if err := json.Unmarshal(retainedResponse.Body, &retainedHistory); err != nil {
		t.Fatalf("unmarshal retained history: %v", err)
	}
	if len(retainedHistory.History) != 1 {
		t.Fatalf("retained history response = %+v", retainedHistory)
	}
	retained := retainedHistory.History[0]
	if retained.Provider != "codex" || retained.Model != "gpt-5-codex" || retained.FailureStatusCode != 400 || failureCodeForTest(t, retained.FailureBody) != cyberPolicyCode {
		t.Fatalf("retained history fields = %+v", retained)
	}
}

func TestSafetyHistoryManagementValidation(t *testing.T) {
	invalid := [][]byte{
		nil,
		[]byte(`{`),
		[]byte(`{"blocked_only":false}`),
		[]byte(`{"api_key":"client-key","blocked_only":"false"}`),
		[]byte(`{"api_key":"client-key","limit":0}`),
		[]byte(`{"api_key":"client-key","before_time":1}`),
		[]byte(`{"api_key":"client-key","before_id":"id"}`),
		[]byte(`{"api_key":"client-key","before_time":"invalid","before_id":"id"}`),
	}
	for _, raw := range invalid {
		if _, response := parseSafetyHistoryRequest(raw); response == nil || response.StatusCode != 400 {
			t.Fatalf("body %s was not rejected: %+v", raw, response)
		}
	}

	query, response := parseSafetyHistoryRequest([]byte(`{"api_key":" client-key ","blocked_only":true,"limit":1000000,"before_time":10,"before_id":" id "}`))
	if response != nil {
		t.Fatalf("valid body was rejected: %+v", response)
	}
	if query.APIKey != "client-key" || !query.BlockedOnly || query.Limit != 1000000 || query.BeforeTime != 10 || query.BeforeID != "id" {
		t.Fatalf("parsed query = %+v", query)
	}

	query, response = parseSafetyHistoryRequest([]byte(`{"api_key":"client-key"}`))
	if response != nil || query.Limit != 100 {
		t.Fatalf("default query = %+v, response=%+v", query, response)
	}
}

func configureForSchemaTest(t *testing.T, schemaVersion uint32, dataDir string) {
	t.Helper()
	config := []byte("data_dir: " + dataDir + "\nrate_limit_enabled: true\nthreshold: 3\nwindow: 600\ncooldown: 1800\n")
	request, err := json.Marshal(struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}{ConfigYAML: config, SchemaVersion: schemaVersion})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	if err := configurePlugin(request); err != nil {
		t.Fatalf("configurePlugin schema %d: %v", schemaVersion, err)
	}
}

func useGlobalSafetyStore(t *testing.T, limiter *safetyLimiter, store *SQLiteStore) {
	t.Helper()
	previousSafety := safety
	storeMu.Lock()
	previousStore := globalStore
	previousPath := currentDBPath
	globalStore = store
	currentDBPath = "test"
	storeMu.Unlock()
	safety = limiter
	t.Cleanup(func() {
		safety = previousSafety
		storeMu.Lock()
		globalStore = previousStore
		currentDBPath = previousPath
		storeMu.Unlock()
	})
}

func callManagementForTest(t *testing.T, request managementRequest) managementResponse {
	t.Helper()
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal management request: %v", err)
	}
	rawEnvelope, err := handleManagement(rawRequest)
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	var result envelope
	if err := json.Unmarshal(rawEnvelope, &result); err != nil {
		t.Fatalf("unmarshal management envelope: %v", err)
	}
	var response managementResponse
	if err := json.Unmarshal(result.Result, &response); err != nil {
		t.Fatalf("unmarshal management response: %v", err)
	}
	return response
}

func failureCodeForTest(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal failure body %s: %v", raw, err)
	}
	return body.Error.Code
}
