package safety

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/Jacko-John/cpa-usage-statistics/internal/config"
	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
)

const (
	SafetyRateLimitedCode    = "safety_rate_limited"
	SafetyRateLimitedMessage = "Too many requests were rejected by the upstream safety policy. Retry later."
	SafetyDisabledCode       = "api_key_disabled"
	SafetyDisabledMessage    = "This API key has been disabled by an administrator."
)

type Limiter struct {
	mu            sync.RWMutex
	config        config.Safety
	states        map[string]usagemodel.SafetyState
	disabledCount int
	configured    bool
}

// Store 提供安全状态变更需要的原子持久化操作。
type Store interface {
	ClearSafetyWindows(context.Context) error
	LoadSafetyStates(context.Context) (map[string]usagemodel.SafetyState, error)
	Insert(context.Context, usagemodel.Record) error
	InsertSafety(context.Context, usagemodel.Record, usagemodel.SafetyState, usagemodel.SafetyHistory) error
	ReleaseSafetyLimits(context.Context, []usagemodel.SafetyState, int64) error
	SetManualDisabled(context.Context, []usagemodel.SafetyState, bool, int64) error
}

func (l *Limiter) Configure(ctx context.Context, store Store, cfg config.Safety, interceptionSupported bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	resetWindows := l.configured && (l.config.Threshold != cfg.Threshold || l.config.Window != cfg.Window)
	if resetWindows {
		if err := store.ClearSafetyWindows(ctx); err != nil {
			return err
		}
	}
	states, err := store.LoadSafetyStates(ctx)
	if err != nil {
		return err
	}
	disabledCount := 0
	for _, state := range states {
		if state.ManualDisabled {
			disabledCount++
		}
	}
	cfg.RateLimitEnabled = cfg.RateLimitEnabled && interceptionSupported
	l.config = cfg
	l.states = states
	l.disabledCount = disabledCount
	l.configured = true
	return nil
}

func (l *Limiter) StoreUsage(ctx context.Context, store Store, record usagemodel.Record, now time.Time) error {
	if record.APIKey == "" || !IsCyberPolicyFailure(record) {
		return store.Insert(ctx, record)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	nowUnix := now.Unix()
	next, history := advanceSafetyState(l.states[record.APIKey], record.APIKey, record.ID, nowUnix, l.config)
	if err := store.InsertSafety(ctx, record, next, history); err != nil {
		return err
	}
	l.states[record.APIKey] = next
	return nil
}

func advanceSafetyState(state usagemodel.SafetyState, apiKey, recordID string, now int64, cfg config.Safety) (usagemodel.SafetyState, usagemodel.SafetyHistory) {
	blocked := state.BlockedUntil > now
	state.APIKey = apiKey
	state.TotalTriggerCount++
	state.RecentTriggerTimes = appendTriggerTime(state.RecentTriggerTimes, now-cfg.Window, now)
	if !blocked {
		state.BlockedUntil = 0
		state.ActiveLimitStartedAt = 0
		state.ActiveLimitUsageRecordID = ""
	}
	history := usagemodel.SafetyHistory{
		APIKey:        apiKey,
		OccurredAt:    now,
		UsageRecordID: recordID,
	}
	if cfg.RateLimitEnabled && !blocked && int64(len(state.RecentTriggerTimes)) >= cfg.Threshold {
		state.BlockedUntil = now + cfg.Cooldown
		state.ActiveLimitStartedAt = now
		state.ActiveLimitUsageRecordID = recordID
		state.TotalLimitCount++
		history.BlockedUntil = state.BlockedUntil
	}
	return state, history
}

func appendTriggerTime(times []int64, cutoff, now int64) []int64 {
	start := 0
	for start < len(times) && times[start] < cutoff {
		start++
	}
	recent := make([]int64, len(times)-start+1)
	copy(recent, times[start:])
	recent[len(recent)-1] = now
	return recent
}

type Decision struct {
	APIKeyError    string
	ManualDisabled bool
	BlockedUntil   int64
}

func (l *Limiter) InspectRequest(headers http.Header) Decision {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.config.RateLimitEnabled && l.disabledCount == 0 {
		return Decision{}
	}
	apiKey, errorCode := extractAPIKey(headers)
	if errorCode != "" {
		return Decision{APIKeyError: errorCode}
	}
	state := l.states[apiKey]
	decision := Decision{ManualDisabled: state.ManualDisabled}
	if l.config.RateLimitEnabled {
		decision.BlockedUntil = state.BlockedUntil
	}
	return decision
}

type KeyStatus struct {
	APIKey             string `json:"api_key"`
	TotalTriggerCount  int64  `json:"total_trigger_count"`
	WindowTriggerCount int    `json:"window_trigger_count"`
	TotalLimitCount    int64  `json:"total_limit_count"`
	ManualDisabled     bool   `json:"manual_disabled"`
	Blocked            bool   `json:"blocked"`
	BlockedUntil       int64  `json:"blocked_until"`
}

func (l *Limiter) KeyStatuses(now time.Time) []KeyStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()

	nowUnix := now.Unix()
	cutoff := nowUnix - l.config.Window
	result := make([]KeyStatus, 0, len(l.states))
	for _, state := range l.states {
		windowCount := 0
		for _, triggeredAt := range state.RecentTriggerTimes {
			if triggeredAt >= cutoff {
				windowCount++
			}
		}
		result = append(result, KeyStatus{
			APIKey:             state.APIKey,
			TotalTriggerCount:  state.TotalTriggerCount,
			WindowTriggerCount: windowCount,
			TotalLimitCount:    state.TotalLimitCount,
			ManualDisabled:     state.ManualDisabled,
			Blocked:            state.BlockedUntil > nowUnix,
			BlockedUntil:       state.BlockedUntil,
		})
	}
	return result
}

// release 解除指定 API Key 尚未到期的限制，并返回已解除和未受限的 Key。
func (l *Limiter) Release(ctx context.Context, store Store, apiKeys []string, now time.Time) (usagemodel.SafetyReleaseResult, error) {
	result := usagemodel.SafetyReleaseResult{
		Released:   make([]string, 0, len(apiKeys)),
		NotBlocked: make([]string, 0, len(apiKeys)),
	}
	nowUnix := now.Unix()

	// 持锁完成筛选、持久化和内存更新，避免解除期间状态发生变化。
	l.mu.Lock()
	defer l.mu.Unlock()
	active := make([]usagemodel.SafetyState, 0, len(apiKeys))
	for _, apiKey := range apiKeys {
		state, exists := l.states[apiKey]
		if !exists || state.BlockedUntil <= nowUnix {
			result.NotBlocked = append(result.NotBlocked, apiKey)
			continue
		}
		active = append(active, state)
		result.Released = append(result.Released, apiKey)
	}
	if len(active) == 0 {
		return result, nil
	}

	// 先持久化解除结果，数据库成功后再更新内存状态。
	if err := store.ReleaseSafetyLimits(ctx, active, nowUnix); err != nil {
		return usagemodel.SafetyReleaseResult{}, err
	}
	// 清空当前限制和滑动窗口，保留累计触发与限流次数。
	for _, state := range active {
		state.RecentTriggerTimes = nil
		state.BlockedUntil = 0
		state.ActiveLimitStartedAt = 0
		state.ActiveLimitUsageRecordID = ""
		l.states[state.APIKey] = state
	}
	return result, nil
}

func (l *Limiter) Disable(ctx context.Context, store Store, apiKeys []string, now time.Time) (usagemodel.SafetyDisableResult, error) {
	result := usagemodel.SafetyDisableResult{
		Disabled:        make([]string, 0, len(apiKeys)),
		AlreadyDisabled: make([]string, 0, len(apiKeys)),
		Missing:         make([]string, 0, len(apiKeys)),
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	states := make([]usagemodel.SafetyState, 0, len(apiKeys))
	for _, apiKey := range apiKeys {
		state, exists := l.states[apiKey]
		switch {
		case !exists:
			result.Missing = append(result.Missing, apiKey)
		case state.ManualDisabled:
			result.AlreadyDisabled = append(result.AlreadyDisabled, apiKey)
		default:
			states = append(states, state)
			result.Disabled = append(result.Disabled, apiKey)
		}
	}
	if err := store.SetManualDisabled(ctx, states, true, now.Unix()); err != nil {
		return usagemodel.SafetyDisableResult{}, err
	}
	for _, state := range states {
		state.ManualDisabled = true
		l.states[state.APIKey] = state
	}
	l.disabledCount += len(states)
	return result, nil
}

func (l *Limiter) Enable(ctx context.Context, store Store, apiKeys []string, now time.Time) (usagemodel.SafetyEnableResult, error) {
	result := usagemodel.SafetyEnableResult{
		Enabled:        make([]string, 0, len(apiKeys)),
		AlreadyEnabled: make([]string, 0, len(apiKeys)),
		Missing:        make([]string, 0, len(apiKeys)),
	}
	nowUnix := now.Unix()

	l.mu.Lock()
	defer l.mu.Unlock()
	states := make([]usagemodel.SafetyState, 0, len(apiKeys))
	for _, apiKey := range apiKeys {
		state, exists := l.states[apiKey]
		switch {
		case !exists:
			result.Missing = append(result.Missing, apiKey)
		case !state.ManualDisabled:
			result.AlreadyEnabled = append(result.AlreadyEnabled, apiKey)
		default:
			states = append(states, state)
			result.Enabled = append(result.Enabled, apiKey)
		}
	}
	if err := store.SetManualDisabled(ctx, states, false, nowUnix); err != nil {
		return usagemodel.SafetyEnableResult{}, err
	}
	for _, state := range states {
		state.ManualDisabled = false
		state.RecentTriggerTimes = nil
		state.BlockedUntil = 0
		state.ActiveLimitStartedAt = 0
		state.ActiveLimitUsageRecordID = ""
		l.states[state.APIKey] = state
	}
	l.disabledCount -= len(states)
	return result, nil
}

type HistoryItem struct {
	usagemodel.SafetyHistory
	Status string `json:"status"`
}

func DescribeHistory(record usagemodel.SafetyHistory, now int64) HistoryItem {
	status := "not_limited"
	switch {
	case record.ManualReleasedAt > 0:
		status = "manual_released"
	case record.BlockedUntil > now:
		status = "active"
	case record.BlockedUntil > 0:
		status = "expired"
	}
	return HistoryItem{SafetyHistory: record, Status: status}
}

func New() *Limiter { return &Limiter{states: make(map[string]usagemodel.SafetyState)} }
