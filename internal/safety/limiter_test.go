package safety

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jacko-John/cpa-usage-statistics/internal/config"
	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testfixture"
)

func TestSafetySlidingWindowStartsLimitAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store, err := testfixture.Open(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	cfg := config.Safety{
		RateLimitEnabled: true,
		Threshold:        3,
		Window:           600,
		Cooldown:         1800,
	}
	limiter := configuredLimiter(t, store, cfg)
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	for i := range 3 {
		now := base.Add(time.Duration(i) * time.Minute)
		if err := limiter.StoreUsage(ctx, store.Store, testfixture.CyberRecord(string(rune('a'+i)), "client-key", now), now); err != nil {
			t.Fatalf("storeUsage %d: %v", i, err)
		}
	}

	state := safetyStateForTest(limiter, "client-key")
	if state.TotalTriggerCount != 3 || state.TotalLimitCount != 1 || len(state.RecentTriggerTimes) != 3 {
		t.Fatalf("state = %+v", state)
	}
	wantBlockedUntil := base.Add(32 * time.Minute).Unix()
	if state.BlockedUntil != wantBlockedUntil || state.ActiveLimitUsageRecordID != "c" {
		t.Fatalf("limit state = %+v, want blocked_until=%d record=c", state, wantBlockedUntil)
	}

	history, err := store.QuerySafetyHistory(ctx, usagemodel.SafetyHistoryQuery{APIKey: "client-key", Limit: 10})
	if err != nil {
		t.Fatalf("QuerySafetyHistory: %v", err)
	}
	if len(history) != 3 || history[0].UsageRecordID != "c" || history[0].BlockedUntil != wantBlockedUntil {
		t.Fatalf("history = %+v", history)
	}
	blockedHistory, err := store.QuerySafetyHistory(ctx, usagemodel.SafetyHistoryQuery{APIKey: "client-key", BlockedOnly: true, Limit: 10})
	if err != nil || len(blockedHistory) != 1 || blockedHistory[0].UsageRecordID != "c" {
		t.Fatalf("blocked history = %+v, err=%v", blockedHistory, err)
	}
	var encoded []byte
	if err := store.DB.QueryRow("SELECT recent_trigger_times FROM api_key_safety_state WHERE api_key = ?", "client-key").Scan(&encoded); err != nil {
		t.Fatalf("query trigger blob: %v", err)
	}
	if len(encoded) != 24 {
		t.Fatalf("trigger blob len = %d, want 24", len(encoded))
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := testfixture.Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := configuredLimiter(t, reopened, cfg)
	restored := safetyStateForTest(restarted, "client-key")
	if restored.TotalTriggerCount != 3 || restored.TotalLimitCount != 1 || restored.BlockedUntil != wantBlockedUntil || len(restored.RecentTriggerTimes) != 3 {
		t.Fatalf("restored state = %+v", restored)
	}
	decision := restarted.InspectRequest(http.Header{"Authorization": {"Bearer client-key"}})
	if decision.BlockedUntil != wantBlockedUntil {
		t.Fatalf("restored decision = %+v", decision)
	}
}

func TestSafetySlidingWindowPrunesOnlyEarlierTimestamps(t *testing.T) {
	store := testfixture.New(t)
	cfg := config.Safety{
		RateLimitEnabled: true,
		Threshold:        100,
		Window:           600,
		Cooldown:         60,
	}
	limiter := configuredLimiter(t, store, cfg)
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	for _, event := range []struct {
		id  string
		now time.Time
	}{
		{id: "at-start", now: base},
		{id: "at-boundary", now: base.Add(10 * time.Minute)},
		{id: "after-boundary", now: base.Add(10*time.Minute + time.Second)},
	} {
		if err := limiter.StoreUsage(ctx, store.Store, testfixture.CyberRecord(event.id, "client-key", event.now), event.now); err != nil {
			t.Fatalf("storeUsage %s: %v", event.id, err)
		}
	}
	state := safetyStateForTest(limiter, "client-key")
	if len(state.RecentTriggerTimes) != 2 {
		t.Fatalf("recent times = %v, want 2 entries", state.RecentTriggerTimes)
	}
	if state.RecentTriggerTimes[0] != base.Add(10*time.Minute).Unix() || state.RecentTriggerTimes[1] != base.Add(10*time.Minute+time.Second).Unix() {
		t.Fatalf("recent times = %v", state.RecentTriggerTimes)
	}
}

func TestSafetyConfigurationChangeClearsWindowOnly(t *testing.T) {
	store := testfixture.New(t)
	cfg := config.Safety{RateLimitEnabled: true, Threshold: 3, Window: 600, Cooldown: 1800}
	limiter := configuredLimiter(t, store, cfg)
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if err := limiter.StoreUsage(context.Background(), store, testfixture.CyberRecord("one", "client-key", now), now); err != nil {
		t.Fatalf("storeUsage: %v", err)
	}

	cfg.Threshold = 4
	if err := limiter.Configure(context.Background(), store, cfg, true); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	state := safetyStateForTest(limiter, "client-key")
	if state.TotalTriggerCount != 1 || state.TotalLimitCount != 0 || len(state.RecentTriggerTimes) != 0 {
		t.Fatalf("state after threshold change = %+v", state)
	}
	persisted, err := store.LoadSafetyStates(context.Background())
	if err != nil {
		t.Fatalf("LoadSafetyStates: %v", err)
	}
	if len(persisted["client-key"].RecentTriggerTimes) != 0 || persisted["client-key"].TotalTriggerCount != 1 {
		t.Fatalf("persisted state = %+v", persisted["client-key"])
	}
}

func TestManualReleaseClearsLimitAndMarksHistory(t *testing.T) {
	store := testfixture.New(t)
	cfg := config.Safety{RateLimitEnabled: true, Threshold: 1, Window: 600, Cooldown: 1800}
	limiter := configuredLimiter(t, store, cfg)
	startedAt := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if err := limiter.StoreUsage(context.Background(), store, testfixture.CyberRecord("limit-record", "client-key", startedAt), startedAt); err != nil {
		t.Fatalf("storeUsage: %v", err)
	}

	releasedAt := startedAt.Add(time.Minute)
	result, err := limiter.Release(context.Background(), store, []string{"client-key", "not-blocked"}, releasedAt)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(result.Released) != 1 || result.Released[0] != "client-key" || len(result.NotBlocked) != 1 || result.NotBlocked[0] != "not-blocked" {
		t.Fatalf("release result = %+v", result)
	}
	state := safetyStateForTest(limiter, "client-key")
	if state.BlockedUntil != 0 || state.ActiveLimitStartedAt != 0 || state.ActiveLimitUsageRecordID != "" || len(state.RecentTriggerTimes) != 0 {
		t.Fatalf("released state = %+v", state)
	}
	if state.TotalTriggerCount != 1 || state.TotalLimitCount != 1 {
		t.Fatalf("release changed totals: %+v", state)
	}
	history, err := store.QuerySafetyHistory(context.Background(), usagemodel.SafetyHistoryQuery{APIKey: "client-key", Limit: 10})
	if err != nil || len(history) != 1 || history[0].ManualReleasedAt != releasedAt.Unix() {
		t.Fatalf("history after release = %+v, err=%v", history, err)
	}
}

func TestManualDisablePersistsWithoutAutomaticRateLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store, err := testfixture.Open(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	cfg := config.Safety{RateLimitEnabled: false, Threshold: 3, Window: 600, Cooldown: 1800}
	limiter := configuredLimiter(t, store, cfg)
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	if err := limiter.StoreUsage(context.Background(), store, testfixture.CyberRecord("record", "client-key", now), now); err != nil {
		t.Fatalf("storeUsage: %v", err)
	}

	result, err := limiter.Disable(context.Background(), store, []string{"client-key", "missing"}, now)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if len(result.Disabled) != 1 || result.Disabled[0] != "client-key" || len(result.Missing) != 1 || result.Missing[0] != "missing" {
		t.Fatalf("disable result = %+v", result)
	}
	result, err = limiter.Disable(context.Background(), store, []string{"client-key"}, now)
	if err != nil || len(result.AlreadyDisabled) != 1 || result.AlreadyDisabled[0] != "client-key" {
		t.Fatalf("second disable result = %+v, err=%v", result, err)
	}
	state := safetyStateForTest(limiter, "client-key")
	if !state.ManualDisabled || state.TotalTriggerCount != 1 || state.TotalLimitCount != 0 || limiter.disabledCount != 1 {
		t.Fatalf("disabled state = %+v count=%d", state, limiter.disabledCount)
	}
	decision := limiter.InspectRequest(http.Header{"Authorization": {"Bearer client-key"}})
	if !decision.ManualDisabled || decision.BlockedUntil != 0 {
		t.Fatalf("disabled decision = %+v", decision)
	}
	history, err := store.QuerySafetyHistory(context.Background(), usagemodel.SafetyHistoryQuery{APIKey: "client-key", Limit: 10})
	if err != nil || len(history) != 1 {
		t.Fatalf("history after disable = %+v, err=%v", history, err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := testfixture.Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := configuredLimiter(t, reopened, cfg)
	restored := safetyStateForTest(restarted, "client-key")
	if !restored.ManualDisabled || restarted.disabledCount != 1 {
		t.Fatalf("restored state = %+v count=%d", restored, restarted.disabledCount)
	}

	enabled, err := restarted.Enable(context.Background(), reopened, []string{"client-key", "missing"}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if len(enabled.Enabled) != 1 || enabled.Enabled[0] != "client-key" || len(enabled.Missing) != 1 || enabled.Missing[0] != "missing" {
		t.Fatalf("enable result = %+v", enabled)
	}
	enabled, err = restarted.Enable(context.Background(), reopened, []string{"client-key"}, now.Add(time.Minute))
	if err != nil || len(enabled.AlreadyEnabled) != 1 || enabled.AlreadyEnabled[0] != "client-key" {
		t.Fatalf("second enable result = %+v, err=%v", enabled, err)
	}
}

func TestManualEnableClearsAutomaticLimitWhileReleaseKeepsDisabled(t *testing.T) {
	store := testfixture.New(t)
	cfg := config.Safety{RateLimitEnabled: true, Threshold: 1, Window: 600, Cooldown: 1800}
	limiter := configuredLimiter(t, store, cfg)
	startedAt := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	for _, apiKey := range []string{"enable-key", "release-key"} {
		if err := limiter.StoreUsage(context.Background(), store, testfixture.CyberRecord(apiKey+"-record", apiKey, startedAt), startedAt); err != nil {
			t.Fatalf("storeUsage %s: %v", apiKey, err)
		}
	}
	if _, err := limiter.Disable(context.Background(), store, []string{"enable-key", "release-key"}, startedAt); err != nil {
		t.Fatalf("disable: %v", err)
	}

	enabledAt := startedAt.Add(time.Minute)
	enabled, err := limiter.Enable(context.Background(), store, []string{"enable-key"}, enabledAt)
	if err != nil || len(enabled.Enabled) != 1 {
		t.Fatalf("enable result = %+v, err=%v", enabled, err)
	}
	enabledState := safetyStateForTest(limiter, "enable-key")
	if enabledState.ManualDisabled || enabledState.BlockedUntil != 0 || len(enabledState.RecentTriggerTimes) != 0 {
		t.Fatalf("enabled state = %+v", enabledState)
	}
	history, err := store.QuerySafetyHistory(context.Background(), usagemodel.SafetyHistoryQuery{APIKey: "enable-key", Limit: 10})
	if err != nil || len(history) != 1 || history[0].ManualReleasedAt != enabledAt.Unix() {
		t.Fatalf("enabled history = %+v, err=%v", history, err)
	}

	if _, err := limiter.Release(context.Background(), store, []string{"release-key"}, enabledAt); err != nil {
		t.Fatalf("release: %v", err)
	}
	releasedState := safetyStateForTest(limiter, "release-key")
	if !releasedState.ManualDisabled || releasedState.BlockedUntil != 0 || limiter.disabledCount != 1 {
		t.Fatalf("released disabled state = %+v count=%d", releasedState, limiter.disabledCount)
	}
	if _, err := limiter.Enable(context.Background(), store, []string{"release-key"}, enabledAt); err != nil {
		t.Fatalf("enable released key: %v", err)
	}
	if limiter.disabledCount != 0 {
		t.Fatalf("disabled count = %d", limiter.disabledCount)
	}
}

func TestSafetyHistoryStatus(t *testing.T) {
	now := int64(1_000)
	tests := []struct {
		record usagemodel.SafetyHistory
		want   string
	}{
		{record: usagemodel.SafetyHistory{}, want: "not_limited"},
		{record: usagemodel.SafetyHistory{BlockedUntil: 1_100}, want: "active"},
		{record: usagemodel.SafetyHistory{BlockedUntil: 1_000}, want: "expired"},
		{record: usagemodel.SafetyHistory{BlockedUntil: 1_100, ManualReleasedAt: 900}, want: "manual_released"},
	}
	for _, test := range tests {
		if got := DescribeHistory(test.record, now).Status; got != test.want {
			t.Fatalf("history status = %q, want %q for %+v", got, test.want, test.record)
		}
	}
}

func TestSafetyDatabaseFailureDoesNotAdvanceMemory(t *testing.T) {
	store := testfixture.New(t)
	cfg := config.Safety{RateLimitEnabled: true, Threshold: 1, Window: 60, Cooldown: 60}
	limiter := configuredLimiter(t, store, cfg)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	now := time.Now()
	if err := limiter.StoreUsage(context.Background(), store, testfixture.CyberRecord("failed-write", "client-key", now), now); err == nil {
		t.Fatal("storeUsage error = nil after database close")
	}
	if _, exists := limiter.states["client-key"]; exists {
		t.Fatalf("memory state advanced after database failure: %+v", limiter.states["client-key"])
	}
}

func TestConcurrentSafetyTriggersKeepCounts(t *testing.T) {
	store := testfixture.New(t)
	cfg := config.Safety{RateLimitEnabled: false, Threshold: 3, Window: 3600, Cooldown: 60}
	limiter := configuredLimiter(t, store, cfg)
	const perKey = 20
	base := time.Now()
	errors := make(chan error, perKey*2)
	var wait sync.WaitGroup
	for _, apiKey := range []string{"first-key", "second-key"} {
		for i := range perKey {
			wait.Add(1)
			go func(apiKey string, i int) {
				defer wait.Done()
				id := apiKey + "-" + string(rune('a'+i))
				errors <- limiter.StoreUsage(context.Background(), store, testfixture.CyberRecord(id, apiKey, base), base)
			}(apiKey, i)
		}
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent storeUsage: %v", err)
		}
	}
	for _, apiKey := range []string{"first-key", "second-key"} {
		state := safetyStateForTest(limiter, apiKey)
		if state.TotalTriggerCount != perKey || len(state.RecentTriggerTimes) != perKey || state.TotalLimitCount != 0 {
			t.Fatalf("state for %s = %+v", apiKey, state)
		}
	}
}

func TestNonSafetyUsageDoesNotWriteSafetyTables(t *testing.T) {
	store := testfixture.New(t)
	cfg := config.Safety{RateLimitEnabled: true, Threshold: 3, Window: 60, Cooldown: 60}
	limiter := configuredLimiter(t, store, cfg)
	now := time.Now()
	records := []usagemodel.Record{
		{ID: "success", APIKey: "client-key", Timestamp: now},
		{ID: "content-filter", APIKey: "client-key", Timestamp: now, Failed: true, FailureBody: `{"error":{"code":"content_filter"}}`},
		{ID: "anonymous", Timestamp: now, Failed: true, FailureBody: `{"error":{"code":"cyber_policy"}}`},
	}
	for _, record := range records {
		if err := limiter.StoreUsage(context.Background(), store, record, now); err != nil {
			t.Fatalf("storeUsage %s: %v", record.ID, err)
		}
	}
	states, err := store.LoadSafetyStates(context.Background())
	if err != nil {
		t.Fatalf("LoadSafetyStates: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("unexpected safety states: %+v", states)
	}
}

func configuredLimiter(t *testing.T, store *testfixture.Store, cfg config.Safety) *Limiter {
	t.Helper()
	limiter := &Limiter{states: make(map[string]usagemodel.SafetyState)}
	if err := limiter.Configure(context.Background(), store, cfg, true); err != nil {
		t.Fatalf("configure limiter: %v", err)
	}
	return limiter
}

func safetyStateForTest(limiter *Limiter, apiKey string) usagemodel.SafetyState {
	limiter.mu.RLock()
	defer limiter.mu.RUnlock()
	return limiter.states[apiKey]
}
