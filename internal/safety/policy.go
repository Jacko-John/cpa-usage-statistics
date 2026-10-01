package safety

import (
	"encoding/json"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
)

const CyberPolicyCode = "cyber_policy"

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

func IsCyberPolicyFailure(record usagemodel.Record) bool {
	if !record.Failed || record.FailureBody == "" {
		return false
	}
	var body safetyFailureBody
	if err := json.Unmarshal([]byte(record.FailureBody), &body); err != nil {
		return false
	}
	switch {
	case body.Error.Code != "":
		return body.Error.Code == CyberPolicyCode
	case body.Response.Error.Code != "":
		return body.Response.Error.Code == CyberPolicyCode
	case body.Body.Error.Code != "":
		return body.Body.Error.Code == CyberPolicyCode
	default:
		return body.Code == CyberPolicyCode
	}
}
