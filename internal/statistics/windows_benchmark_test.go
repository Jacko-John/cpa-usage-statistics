package statistics

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkUsageAPIWindowsBatch(b *testing.B) {
	now := FloorUnix(time.Now().Unix(), 60)
	pricing, pricingKey := NormalizePricing(Pricing{Prices: map[string]ModelPrice{"model": {Input: 1}}})
	s := Engine{state: NewAccumulator(now, pricing, pricingKey)}
	queries := make([]APIWindowQuery, 100)
	for i := range queries {
		queries[i] = APIWindowQuery{APIKey: fmt.Sprintf("key-%d", i), Start: now - 86400, End: now}
	}
	for minute := now - 86400; minute < now; minute += 60 {
		for _, query := range queries {
			s.state.AddRecord(Entry{APIKey: query.APIKey, Model: "model", Timestamp: minute, InputTokens: 100, TotalTokens: 100}, now)
		}
	}
	b.Run("batch-100-keys", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s.apiWindows(queries, now)
		}
	})
	b.Run("separate-100-keys", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for j := range queries {
				s.apiWindows(queries[j:j+1], now)
			}
		}
	})
}
