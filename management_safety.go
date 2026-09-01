package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type safetyKeysRequest struct {
	APIKeys []string `json:"api_keys"`
}

type safetyHistoryRequest struct {
	APIKey      string `json:"api_key"`
	BlockedOnly bool   `json:"blocked_only"`
	BeforeTime  *int64 `json:"before_time"`
	BeforeID    string `json:"before_id"`
	Limit       *int   `json:"limit"`
}

type safetyKeysResponse struct {
	Keys []safetyKeyStatus `json:"keys"`
}

type safetyHistoryResponse struct {
	History []safetyHistoryItem `json:"history"`
}

// safetyKeysGet 返回全部 API Key 的内存安全状态，不查询 SQLite。
func safetyKeysGet() ([]byte, error) {
	return okEnvelope(jsonManagementResponse(200, safetyKeysResponse{Keys: safety.keyStatuses(time.Now())}))
}

// safetyHistoryPost 查询一个 API Key 的安全触发历史，并计算每条记录的当前状态。
func safetyHistoryPost(req managementRequest) ([]byte, error) {
	query, errorResponse := parseSafetyHistoryRequest(req.Body)
	if errorResponse != nil {
		return okEnvelope(*errorResponse)
	}
	store := currentStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(200, safetyHistoryResponse{History: []safetyHistoryItem{}}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	records, err := store.QuerySafetyHistory(ctx, query)
	if err != nil {
		return okEnvelope(managementErrorResponse(500, "failed to query safety history"))
	}
	now := time.Now().Unix()
	history := make([]safetyHistoryItem, len(records))
	for i, record := range records {
		history[i] = historyItem(record, now)
	}
	return okEnvelope(jsonManagementResponse(200, safetyHistoryResponse{History: history}))
}

// parseSafetyHistoryRequest 解析历史查询条件，并校验 API Key、数量和复合游标。
func parseSafetyHistoryRequest(raw []byte) (SafetyHistoryQuery, *managementResponse) {
	var body safetyHistoryRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			response := managementErrorResponse(400, "invalid body")
			return SafetyHistoryQuery{}, &response
		}
	}
	query := SafetyHistoryQuery{
		APIKey:      strings.TrimSpace(body.APIKey),
		BlockedOnly: body.BlockedOnly,
		BeforeID:    strings.TrimSpace(body.BeforeID),
		Limit:       100,
	}
	if query.APIKey == "" {
		response := managementErrorResponse(400, "api_key required")
		return query, &response
	}
	if body.Limit != nil {
		if *body.Limit <= 0 {
			response := managementErrorResponse(400, "invalid limit")
			return query, &response
		}
		query.Limit = *body.Limit
	}
	if (body.BeforeTime == nil) != (query.BeforeID == "") {
		response := managementErrorResponse(400, "before_time and before_id must be provided together")
		return query, &response
	}
	if body.BeforeTime != nil {
		query.BeforeTime = *body.BeforeTime
	}
	return query, nil
}

// parseSafetyKeysRequest 解析、清理并去重管理请求中的 API Key。
func parseSafetyKeysRequest(raw []byte) ([]string, *managementResponse) {
	var body safetyKeysRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			response := managementErrorResponse(400, "invalid body")
			return nil, &response
		}
	}
	keys := NewStableStringSet(len(body.APIKeys))
	for _, apiKey := range body.APIKeys {
		if trimmed := strings.TrimSpace(apiKey); trimmed != "" {
			keys.Add(trimmed)
		}
	}
	if keys.Len() == 0 {
		response := managementErrorResponse(400, "api_keys required")
		return nil, &response
	}
	return keys.Values(), nil
}

// safetyLimitsDelete 解除指定 API Key 尚未到期的自动限流。
func safetyLimitsDelete(req managementRequest) ([]byte, error) {
	store := currentStore()
	if store == nil {
		return okEnvelope(managementErrorResponse(400, "usage store unavailable"))
	}
	keys, errorResponse := parseSafetyKeysRequest(req.Body)
	if errorResponse != nil {
		return okEnvelope(*errorResponse)
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	result, err := safety.release(ctx, store, keys, time.Now())
	if err != nil {
		return okEnvelope(managementErrorResponse(500, "failed to release safety limits"))
	}
	return okEnvelope(jsonManagementResponse(200, result))
}

// safetyDisabledPost 永久禁用已经存在安全状态的 API Key。
func safetyDisabledPost(req managementRequest) ([]byte, error) {
	store := currentStore()
	if store == nil {
		return okEnvelope(managementErrorResponse(400, "usage store unavailable"))
	}
	keys, errorResponse := parseSafetyKeysRequest(req.Body)
	if errorResponse != nil {
		return okEnvelope(*errorResponse)
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	result, err := safety.disable(ctx, store, keys, time.Now())
	if err != nil {
		return okEnvelope(managementErrorResponse(500, "failed to disable api keys"))
	}
	return okEnvelope(jsonManagementResponse(200, result))
}

// safetyDisabledDelete 重新启用 API Key，并同时解除其自动限流。
func safetyDisabledDelete(req managementRequest) ([]byte, error) {
	store := currentStore()
	if store == nil {
		return okEnvelope(managementErrorResponse(400, "usage store unavailable"))
	}
	keys, errorResponse := parseSafetyKeysRequest(req.Body)
	if errorResponse != nil {
		return okEnvelope(*errorResponse)
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	result, err := safety.enable(ctx, store, keys, time.Now())
	if err != nil {
		return okEnvelope(managementErrorResponse(500, "failed to enable api keys"))
	}
	return okEnvelope(jsonManagementResponse(200, result))
}
