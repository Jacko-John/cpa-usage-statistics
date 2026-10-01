package plugin

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/protocol"
	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
)

const maxUsageEvents = 100

type deleteUsageRequest struct {
	IDs []string `json:"ids"`
}

type usageSummaryRequest struct {
	Range string `json:"range"`
}

type credentialWindowsRequest struct {
	Windows []statistics.CredentialWindowQuery `json:"windows"`
}

// usageGet 只返回按需加载的最新事件，避免管理请求传输整张 usage_records。
func usageGet(req protocol.ManagementRequest) ([]byte, error) {
	rng, errorResponse := parseUsageRange(req.Query)
	if errorResponse != nil {
		return protocol.OK(*errorResponse)
	}
	limit := maxUsageEvents
	if raw := strings.TrimSpace(firstValue(req.Query, "limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return protocol.OK(managementErrorResponse(400, "invalid limit"))
		}
		limit = min(parsed, maxUsageEvents)
	}
	store := currentStore()
	if store == nil {
		return protocol.OK(jsonManagementResponse(200, usagemodel.APIUsage{}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	result, err := store.QueryRecent(ctx, rng, limit)
	if err != nil {
		return protocol.OK(managementErrorResponse(500, "failed to query usage events"))
	}
	return protocol.OK(jsonManagementResponse(200, result))
}

// usagePost 返回插件已增量计算的监控与凭证统计快照。
func usagePost(req protocol.ManagementRequest) ([]byte, error) {
	var body usageSummaryRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return protocol.OK(managementErrorResponse(400, "invalid body"))
		}
	}
	body.Range = strings.TrimSpace(body.Range)
	if body.Range == "" {
		body.Range = "24h"
	}
	seconds, exists := statistics.RangeSeconds(body.Range)
	if body.Range != "all" {
		if !exists {
			return protocol.OK(managementErrorResponse(400, "invalid range"))
		}
	}
	store := currentStore()
	if store == nil {
		empty := statistics.NewUsageSummaryData()
		response := statistics.UsageSummaryResponse{
			Range:            body.Range,
			GeneratedAt:      time.Now().Unix(),
			UsageSummaryData: &empty,
		}
		if body.Range != "all" {
			response.RateMinutes = seconds / statistics.StatisticsMinute
		}
		return protocol.OK(jsonManagementResponse(200, response))
	}
	payload, err := store.SummaryJSON(body.Range, time.Now().Unix())
	if err != nil {
		return protocol.OK(managementErrorResponse(500, "failed to read usage summary"))
	}
	return protocol.OK(rawJSONManagementResponse(200, payload))
}

// usageCredentialWindowsPost 从预计算的分钟/小时桶直接返回凭证窗口统计。
func usageCredentialWindowsPost(req protocol.ManagementRequest) ([]byte, error) {
	var body credentialWindowsRequest
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return protocol.OK(managementErrorResponse(400, "invalid body"))
	}
	store := currentStore()
	if store == nil {
		return protocol.OK(jsonManagementResponse(200, statistics.CredentialWindowResponse{
			Windows: map[string]statistics.CredentialWindowSummary{},
		}))
	}
	payload, err := store.CredentialWindowsJSON(body.Windows, time.Now().Unix())
	if err != nil {
		return protocol.OK(managementErrorResponse(500, "failed to read credential windows"))
	}
	return protocol.OK(rawJSONManagementResponse(200, payload))
}

// usagePricingGet 返回插件当前使用的价格配置。
func usagePricingGet() ([]byte, error) {
	store := currentStore()
	if store == nil {
		pricing, _ := statistics.NormalizePricing(statistics.Pricing{})
		return protocol.OK(jsonManagementResponse(200, pricing))
	}
	return protocol.OK(jsonManagementResponse(200, store.Pricing()))
}

// usagePricingPut 持久化价格并在切换前重建全部预计算成本。
func usagePricingPut(req protocol.ManagementRequest) ([]byte, error) {
	store := currentStore()
	if store == nil {
		return protocol.OK(managementErrorResponse(400, "usage store unavailable"))
	}
	var pricing statistics.Pricing
	if len(req.Body) == 0 || json.Unmarshal(req.Body, &pricing) != nil {
		return protocol.OK(managementErrorResponse(400, "invalid body"))
	}
	updated, err := store.UpdatePricing(context.Background(), pricing)
	if err != nil {
		return protocol.OK(managementErrorResponse(500, "failed to update usage pricing"))
	}
	return protocol.OK(jsonManagementResponse(200, updated))
}

func usageDelete(req protocol.ManagementRequest) ([]byte, error) {
	store := currentStore()
	if store == nil {
		return protocol.OK(managementErrorResponse(400, "usage store unavailable"))
	}
	var body deleteUsageRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return protocol.OK(managementErrorResponse(400, "invalid body"))
		}
	}
	ids := usagemodel.NewStableStringSet(len(body.IDs))
	for _, id := range body.IDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			ids.Add(trimmed)
		}
	}
	if ids.Len() == 0 {
		return protocol.OK(managementErrorResponse(400, "ids required"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	result, err := store.Delete(ctx, ids)
	if err != nil {
		return protocol.OK(managementErrorResponse(500, "failed to delete usage records"))
	}
	return protocol.OK(jsonManagementResponse(200, result))
}

func parseUsageRange(query map[string][]string) (usagemodel.QueryRange, *protocol.ManagementResponse) {
	var rng usagemodel.QueryRange
	if raw := strings.TrimSpace(firstValue(query, "start")); raw != "" {
		start, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			response := managementErrorResponse(400, "invalid start")
			return rng, &response
		}
		start = start.UTC()
		rng.Start = &start
	}
	if raw := strings.TrimSpace(firstValue(query, "end")); raw != "" {
		end, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			response := managementErrorResponse(400, "invalid end")
			return rng, &response
		}
		end = end.UTC()
		rng.End = &end
	}
	return rng, nil
}

func firstValue(values map[string][]string, key string) string {
	items := values[key]
	if len(items) == 0 {
		return ""
	}
	return items[0]
}
