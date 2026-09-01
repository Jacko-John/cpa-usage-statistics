package main

import (
	"encoding/json"
	"strings"
)

type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers"`
	Query   map[string][]string `json:"Query"`
	Body    []byte              `json:"Body"`
}

type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

type managementErrorPayload struct {
	Error string `json:"error"`
}

var jsonResponseHeaders = map[string][]string{
	"Content-Type": {"application/json; charset=utf-8"},
}

func handleManagement(request []byte) ([]byte, error) {
	var req managementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	path := strings.TrimPrefix(strings.TrimSpace(req.Path), "/v0/management")

	switch path {
	case managementUsagePath:
		switch method {
		case "GET":
			return usageGet(req)
		case "DELETE":
			return usageDelete(req)
		}
	case managementSafetyKeysPath:
		if method == "GET" {
			return safetyKeysGet()
		}
	case managementSafetyHistoryPath:
		if method == "POST" {
			return safetyHistoryPost(req)
		}
	case managementSafetyLimitsPath:
		if method == "DELETE" {
			return safetyLimitsDelete(req)
		}
	default:
		return okEnvelope(managementErrorResponse(404, "route not found"))
	}
	return okEnvelope(managementErrorResponse(405, "method not allowed"))
}

func managementErrorResponse(status int, message string) managementResponse {
	return jsonManagementResponse(status, managementErrorPayload{Error: message})
}

func jsonManagementResponse(status int, payload any) managementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"error":"failed to encode response"}`)
		status = 500
	}
	return managementResponse{
		StatusCode: status,
		Headers:    jsonResponseHeaders,
		Body:       body,
	}
}
