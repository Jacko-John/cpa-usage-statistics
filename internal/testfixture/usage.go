package testfixture

import (
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
)

func CyberRecord(id, apiKey string, timestamp time.Time) usagemodel.Record {
	return usagemodel.Record{
		ID:          id,
		APIKey:      apiKey,
		Timestamp:   timestamp,
		Failed:      true,
		FailureBody: `{"error":{"code":"cyber_policy"}}`,
	}
}
