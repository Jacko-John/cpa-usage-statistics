package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type deleteUsageRequest struct {
	IDs []string `json:"ids"`
}

func usageGet(req managementRequest) ([]byte, error) {
	rng, errorResponse := parseUsageRange(req.Query)
	if errorResponse != nil {
		return okEnvelope(*errorResponse)
	}
	store := currentStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(200, APIUsage{}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	result, err := store.Query(ctx, rng)
	if err != nil {
		return okEnvelope(managementErrorResponse(500, "failed to query usage"))
	}
	return okEnvelope(jsonManagementResponse(200, result))
}

func usageDelete(req managementRequest) ([]byte, error) {
	store := currentStore()
	if store == nil {
		return okEnvelope(managementErrorResponse(400, "usage store unavailable"))
	}
	var body deleteUsageRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return okEnvelope(managementErrorResponse(400, "invalid body"))
		}
	}
	ids := NewStableStringSet(len(body.IDs))
	for _, id := range body.IDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			ids.Add(trimmed)
		}
	}
	if ids.Len() == 0 {
		return okEnvelope(managementErrorResponse(400, "ids required"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	result, err := store.Delete(ctx, ids)
	if err != nil {
		return okEnvelope(managementErrorResponse(500, "failed to delete usage records"))
	}
	return okEnvelope(jsonManagementResponse(200, result))
}

func parseUsageRange(query map[string][]string) (QueryRange, *managementResponse) {
	var rng QueryRange
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
