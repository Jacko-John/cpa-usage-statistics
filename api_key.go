package main

import (
	"net/http"
	"strings"
)

const (
	unsupportedAPIKeyTransport = "unsupported_api_key_transport"
	ambiguousAPIKey            = "ambiguous_api_key"
	bearerScheme               = "Bearer"
)

func extractAPIKey(headers http.Header) (string, string) {
	// CLIProxyAPI passes canonical http.Header keys, so direct lookups avoid scanning unrelated headers.
	apiKey, errorCode := mergeHeaderAPIKeys("", headers["Authorization"], true)
	if errorCode != "" {
		return "", errorCode
	}
	apiKey, errorCode = mergeHeaderAPIKeys(apiKey, headers["X-Api-Key"], false)
	if errorCode != "" {
		return "", errorCode
	}
	apiKey, errorCode = mergeHeaderAPIKeys(apiKey, headers["X-Goog-Api-Key"], false)
	if errorCode != "" {
		return "", errorCode
	}
	if apiKey == "" {
		return "", unsupportedAPIKeyTransport
	}
	return apiKey, ""
}

func mergeHeaderAPIKeys(current string, values []string, authorization bool) (string, string) {
	for _, value := range values {
		candidate := strings.TrimSpace(value)
		if authorization {
			candidate = authorizationAPIKey(candidate)
		}
		if candidate == "" {
			continue
		}
		if current == "" || current == candidate {
			current = candidate
			continue
		}
		return "", ambiguousAPIKey
	}
	return current, ""
}

func authorizationAPIKey(value string) string {
	switch {
	case len(value) == len(bearerScheme) && strings.EqualFold(value, bearerScheme):
		return ""
	case len(value) > len(bearerScheme) &&
		value[len(bearerScheme)] == ' ' &&
		strings.EqualFold(value[:len(bearerScheme)], bearerScheme):
		return strings.TrimSpace(value[len(bearerScheme)+1:])
	default:
		return value
	}
}
