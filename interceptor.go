package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type requestInterceptRequest struct {
	SourceFormat string
	Headers      http.Header
}

type requestInterceptResponse struct {
	Terminate       bool
	StatusCode      int
	ResponseHeaders http.Header
	ResponseBody    []byte
}

type protocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type"`
}

type openAIProtocolErrorResponse struct {
	Error protocolError `json:"error"`
}

type claudeProtocolErrorResponse struct {
	Error protocolError `json:"error"`
	Type  string        `json:"type"`
}

type geminiErrorDetail struct {
	Type   string `json:"@type"`
	Reason string `json:"reason"`
}

type geminiProtocolError struct {
	Code    int                  `json:"code"`
	Details [1]geminiErrorDetail `json:"details"`
	Message string               `json:"message"`
	Status  string               `json:"status"`
}

type geminiProtocolErrorResponse struct {
	Error geminiProtocolError `json:"error"`
}

var emptyInterceptorEnvelope = []byte(`{"ok":true,"result":{}}`)

func interceptRequestBefore(request []byte) ([]byte, error) {
	var req requestInterceptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	decision := safety.inspectRequest(req.Headers)
	switch decision.APIKeyError {
	case unsupportedAPIKeyTransport:
		return terminatedRequest(req.SourceFormat, http.StatusUnauthorized, decision.APIKeyError, "A supported API key request header is required.", 0)
	case ambiguousAPIKey:
		return terminatedRequest(req.SourceFormat, http.StatusBadRequest, decision.APIKeyError, "Multiple different API keys were provided.", 0)
	}
	if decision.BlockedUntil > now {
		return terminatedRequest(req.SourceFormat, http.StatusTooManyRequests, safetyRateLimitedCode, safetyRateLimitedMessage, decision.BlockedUntil-now)
	}
	return emptyInterceptorEnvelope, nil
}

func interceptRequestAfter([]byte) ([]byte, error) {
	return emptyInterceptorEnvelope, nil
}

func terminatedRequest(sourceFormat string, status int, code, message string, retryAfter int64) ([]byte, error) {
	body, err := protocolErrorBody(sourceFormat, status, code, message)
	if err != nil {
		return nil, err
	}
	headers := http.Header{"Content-Type": {"application/json"}}
	if retryAfter > 0 {
		headers.Set("Retry-After", strconv.FormatInt(retryAfter, 10))
	}
	return okEnvelope(requestInterceptResponse{
		Terminate:       true,
		StatusCode:      status,
		ResponseHeaders: headers,
		ResponseBody:    body,
	})
}

func protocolErrorBody(sourceFormat string, status int, code, message string) ([]byte, error) {
	errorType := "invalid_request_error"
	switch status {
	case http.StatusUnauthorized:
		errorType = "authentication_error"
	case http.StatusTooManyRequests:
		errorType = "rate_limit_error"
	}
	err := protocolError{Code: code, Message: message, Type: errorType}

	format := strings.TrimSpace(sourceFormat)
	switch {
	case strings.EqualFold(format, "claude"):
		return json.Marshal(claudeProtocolErrorResponse{Error: err, Type: "error"})
	case strings.EqualFold(format, "gemini"):
		geminiStatus := "INVALID_ARGUMENT"
		switch status {
		case http.StatusUnauthorized:
			geminiStatus = "UNAUTHENTICATED"
		case http.StatusTooManyRequests:
			geminiStatus = "RESOURCE_EXHAUSTED"
		}
		return json.Marshal(geminiProtocolErrorResponse{Error: geminiProtocolError{
			Code: status,
			Details: [1]geminiErrorDetail{{
				Type:   "type.googleapis.com/google.rpc.ErrorInfo",
				Reason: code,
			}},
			Message: message,
			Status:  geminiStatus,
		}})
	default:
		return json.Marshal(openAIProtocolErrorResponse{Error: err})
	}
}
