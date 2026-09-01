package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func okEnvelope(value any) ([]byte, error) {
	if response, ok := value.(managementResponse); ok {
		return okManagementEnvelope(response)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	const prefix = `{"ok":true,"result":`
	result := make([]byte, 0, len(prefix)+len(payload)+1)
	result = append(result, prefix...)
	result = append(result, payload...)
	result = append(result, '}')
	return result, nil
}

// okManagementEnvelope 直接编码管理响应，避免完整的中间 JSON 缓冲区。
func okManagementEnvelope(response managementResponse) ([]byte, error) {
	headers, err := json.Marshal(response.Headers)
	if err != nil {
		return nil, err
	}

	const prefix = `{"ok":true,"result":{"StatusCode":`
	const headersPrefix = `,"Headers":`
	const bodyPrefix = `,"Body":`
	const suffix = `}}`

	bodyLen := 4 // null
	if response.Body != nil {
		bodyLen = base64.StdEncoding.EncodedLen(len(response.Body)) + 2
	}
	capacity := len(prefix) + 20 + len(headersPrefix) + len(headers) + len(bodyPrefix) + bodyLen + len(suffix)
	result := make([]byte, 0, capacity)
	result = append(result, prefix...)
	result = strconv.AppendInt(result, int64(response.StatusCode), 10)
	result = append(result, headersPrefix...)
	result = append(result, headers...)
	result = append(result, bodyPrefix...)
	if response.Body == nil {
		result = append(result, "null"...)
	} else {
		result = append(result, '"')
		result = base64.StdEncoding.AppendEncode(result, response.Body)
		result = append(result, '"')
	}
	result = append(result, suffix...)
	return result, nil
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}
