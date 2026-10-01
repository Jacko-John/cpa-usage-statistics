package plugin

import (
	"testing"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	safetycontrol "github.com/Jacko-John/cpa-usage-statistics/internal/safety"
)

func TestCyberPolicyRecognition(t *testing.T) {
	bodies := []string{
		`{"error":{"code":"cyber_policy"}}`,
		`{"response":{"error":{"code":"cyber_policy"}}}`,
		`{"body":{"error":{"code":"cyber_policy"}}}`,
		`{"code":"cyber_policy"}`,
	}
	for _, status := range []int{400, 500, 502} {
		for _, body := range bodies {
			record := toRecord(usageRecord{Failure: usageFailure{StatusCode: status, Body: body}})
			if !safetycontrol.IsCyberPolicyFailure(record) {
				t.Fatalf("status=%d body=%s was not recognized", status, body)
			}
		}
	}

	rejected := []usagemodel.Record{
		{Failed: true, FailureBody: `{"error":{"code":"content_filter"}}`},
		{Failed: true, FailureBody: `{"error":{"code":"content_filter"},"code":"cyber_policy"}`},
		{Failed: true, FailureBody: `not-json`},
		{Failed: false, FailureBody: `{"error":{"code":"cyber_policy"}}`},
		{Failed: true, FailureBody: `{"error":{"message":"refused"}}`},
	}
	for _, record := range rejected {
		if safetycontrol.IsCyberPolicyFailure(record) {
			t.Fatalf("unexpected cyber_policy match: %+v", record)
		}
	}
}
