package main

import "encoding/json"

const cyberPolicyCode = "cyber_policy"

type codedFailure struct {
	Code string `json:"code"`
}

type nestedFailure struct {
	Error codedFailure `json:"error"`
}

type safetyFailureBody struct {
	Error    codedFailure  `json:"error"`
	Response nestedFailure `json:"response"`
	Body     nestedFailure `json:"body"`
	Code     string        `json:"code"`
}

func isCyberPolicyFailure(record Record) bool {
	if !record.Failed || record.FailureBody == "" {
		return false
	}
	var body safetyFailureBody
	if err := json.Unmarshal([]byte(record.FailureBody), &body); err != nil {
		return false
	}
	switch {
	case body.Error.Code != "":
		return body.Error.Code == cyberPolicyCode
	case body.Response.Error.Code != "":
		return body.Response.Error.Code == cyberPolicyCode
	case body.Body.Error.Code != "":
		return body.Body.Error.Code == cyberPolicyCode
	default:
		return body.Code == cyberPolicyCode
	}
}
