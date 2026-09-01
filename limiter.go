package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	safetyRateLimitedCode    = "safety_rate_limited"
	safetyRateLimitedMessage = "Too many requests were rejected by the upstream safety policy. Retry later."
	safetyDisabledCode       = "api_key_disabled"
	safetyDisabledMessage    = "This API key has been disabled by an administrator."
)

type safetyLimiter struct {
	mu            sync.RWMutex
	config        safetyConfig
	states        map[string]SafetyState
	disabledCount int
	configured    bool
}

var safety = &safetyLimiter{states: make(map[string]SafetyState)}

func (l *safetyLimiter) configure(ctx context.Context, store *SQLiteStore, cfg safetyConfig, interceptionSupported bool) error {
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

func (l *safetyLimiter) storeUsage(ctx context.Context, store *SQLiteStore, record Record, now time.Time) error {
	if record.APIKey == "" || !isCyberPolicyFailure(record) {
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

func advanceSafetyState(state SafetyState, apiKey, recordID string, now int64, cfg safetyConfig) (SafetyState, SafetyHistory) {
	blocked := state.BlockedUntil > now
	state.APIKey = apiKey
	state.TotalTriggerCount++
	state.RecentTriggerTimes = appendTriggerTime(state.RecentTriggerTimes, now-cfg.Window, now)
	if !blocked {
		state.BlockedUntil = 0
		state.ActiveLimitStartedAt = 0
		state.ActiveLimitUsageRecordID = ""
	}
	history := SafetyHistory{
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

type requestSafetyDecision struct {
	APIKeyError    string
	ManualDisabled bool
	BlockedUntil   int64
}

func (l *safetyLimiter) inspectRequest(headers http.Header) requestSafetyDecision {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.config.RateLimitEnabled && l.disabledCount == 0 {
		return requestSafetyDecision{}
	}
	apiKey, errorCode := extractAPIKey(headers)
	if errorCode != "" {
		return requestSafetyDecision{APIKeyError: errorCode}
	}
	state := l.states[apiKey]
	decision := requestSafetyDecision{ManualDisabled: state.ManualDisabled}
	if l.config.RateLimitEnabled {
		decision.BlockedUntil = state.BlockedUntil
	}
	return decision
}

type safetyKeyStatus struct {
	APIKey             string `json:"api_key"`
	TotalTriggerCount  int64  `json:"total_trigger_count"`
	WindowTriggerCount int    `json:"window_trigger_count"`
	TotalLimitCount    int64  `json:"total_limit_count"`
	ManualDisabled     bool   `json:"manual_disabled"`
	Blocked            bool   `json:"blocked"`
	BlockedUntil       int64  `json:"blocked_until"`
}

func (l *safetyLimiter) keyStatuses(now time.Time) []safetyKeyStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()

	nowUnix := now.Unix()
	cutoff := nowUnix - l.config.Window
	result := make([]safetyKeyStatus, 0, len(l.states))
	for _, state := range l.states {
		windowCount := 0
		for _, triggeredAt := range state.RecentTriggerTimes {
			if triggeredAt >= cutoff {
				windowCount++
			}
		}
		result = append(result, safetyKeyStatus{
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
func (l *safetyLimiter) release(ctx context.Context, store *SQLiteStore, apiKeys []string, now time.Time) (SafetyReleaseResult, error) {
	result := SafetyReleaseResult{
		Released:   make([]string, 0, len(apiKeys)),
		NotBlocked: make([]string, 0, len(apiKeys)),
	}
	nowUnix := now.Unix()

	// 持锁完成筛选、持久化和内存更新，避免解除期间状态发生变化。
	l.mu.Lock()
	defer l.mu.Unlock()
	active := make([]SafetyState, 0, len(apiKeys))
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
		return SafetyReleaseResult{}, err
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

func (l *safetyLimiter) disable(ctx context.Context, store *SQLiteStore, apiKeys []string, now time.Time) (SafetyDisableResult, error) {
	result := SafetyDisableResult{
		Disabled:        make([]string, 0, len(apiKeys)),
		AlreadyDisabled: make([]string, 0, len(apiKeys)),
		Missing:         make([]string, 0, len(apiKeys)),
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	states := make([]SafetyState, 0, len(apiKeys))
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
		return SafetyDisableResult{}, err
	}
	for _, state := range states {
		state.ManualDisabled = true
		l.states[state.APIKey] = state
	}
	l.disabledCount += len(states)
	return result, nil
}

func (l *safetyLimiter) enable(ctx context.Context, store *SQLiteStore, apiKeys []string, now time.Time) (SafetyEnableResult, error) {
	result := SafetyEnableResult{
		Enabled:        make([]string, 0, len(apiKeys)),
		AlreadyEnabled: make([]string, 0, len(apiKeys)),
		Missing:        make([]string, 0, len(apiKeys)),
	}
	nowUnix := now.Unix()

	l.mu.Lock()
	defer l.mu.Unlock()
	states := make([]SafetyState, 0, len(apiKeys))
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
		return SafetyEnableResult{}, err
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

type safetyHistoryItem struct {
	SafetyHistory
	Status string `json:"status"`
}

func historyItem(record SafetyHistory, now int64) safetyHistoryItem {
	status := "not_limited"
	switch {
	case record.ManualReleasedAt > 0:
		status = "manual_released"
	case record.BlockedUntil > now:
		status = "active"
	case record.BlockedUntil > 0:
		status = "expired"
	}
	return safetyHistoryItem{SafetyHistory: record, Status: status}
}

func encodeTriggerTimes(times []int64) []byte {
	encoded := make([]byte, len(times)*8)
	for i, timestamp := range times {
		binary.LittleEndian.PutUint64(encoded[i*8:], uint64(timestamp))
	}
	return encoded
}

func decodeTriggerTimes(encoded []byte) ([]int64, error) {
	if len(encoded)%8 != 0 {
		return nil, fmt.Errorf("invalid recent trigger times")
	}
	times := make([]int64, len(encoded)/8)
	for i := range times {
		times[i] = int64(binary.LittleEndian.Uint64(encoded[i*8:]))
	}
	return times, nil
}
