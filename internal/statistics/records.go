package statistics

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
)

const RecordColumns = `timestamp, api_key, provider, model, source, auth_type, service_tier,
       latency_ms, ttft_ms, input_tokens, output_tokens, cached_tokens,
       cache_creation_tokens, total_tokens, failed`

func (s *Engine) Rebuild(ctx context.Context, queryer Queryer, pricing Pricing) error {
	pricing, pricingKey := NormalizePricing(pricing)
	now := time.Now().Unix()
	next := NewAccumulator(now, pricing, pricingKey)
	if err := ScanStatisticsRecords(ctx, queryer, func(record Entry) {
		next.AddRecord(record, now)
	}); err != nil {
		return err
	}
	s.mu.Lock()
	s.state = next
	s.mu.Unlock()
	return nil
}

// usageAPIWindows 的调用方持有唯一连接，保证缓存、补查明细与价格版本一致。
func (s *Engine) APIWindows(ctx context.Context, queryer Queryer, queries []APIWindowQuery, now int64) ([]APIWindowSummary, error) {
	results, missing, pricing := s.apiWindows(queries, now)
	if len(missing) == 0 {
		return results, nil
	}
	limit := FloorUnix(now, StatisticsMinute) + StatisticsMinute
	start, end := limit, int64(0)
	byAPI := make(map[string][]int)
	windows := make([]apiWindowAccumulator, len(queries))
	for _, i := range missing {
		query := queries[i]
		start = min(start, query.Start)
		end = max(end, min(query.End, limit))
		key := strings.TrimSpace(query.APIKey)
		byAPI[key] = append(byAPI[key], i)
	}
	rows, err := queryer.QueryContext(ctx, `SELECT `+RecordColumns+`
FROM usage_records WHERE timestamp >= ? AND timestamp < ?`, start, end)
	if err != nil {
		return nil, fmt.Errorf("usage sqlite query historical windows: %w", err)
	}
	multipliers := BuildTierMultiplierIndex(pricing.TierMultipliers)
	err = scanStatisticsRows(rows, func(record Entry) {
		indices := byAPI[usagemodel.GroupingKey(record.APIKey, record.Provider)]
		if len(indices) == 0 {
			return
		}
		cost := calculateStatisticsRecordCost(record, pricing.Prices, multipliers)
		_, priced := pricing.Prices[record.Model]
		for _, i := range indices {
			query := queries[i]
			if record.Timestamp < query.Start || record.Timestamp >= query.End {
				continue
			}
			var unpriced int64
			if !priced {
				unpriced = 1
			}
			windows[i].add(usagemodel.NormalizeModel(record.Model), normalizeServiceTier(record.ServiceTier), 1, record.TotalTokens, cost, unpriced)
		}
	})
	if err != nil {
		return nil, err
	}
	for _, i := range missing {
		results[i] = windows[i].summary()
	}
	return results, nil
}

func ScanStatisticsRecords(
	ctx context.Context,
	queryer Queryer,
	visit func(Entry),
) error {
	rows, err := queryer.QueryContext(ctx, `SELECT `+RecordColumns+`
FROM usage_records`)
	if err != nil {
		return fmt.Errorf("usage sqlite scan statistics: %w", err)
	}
	return scanStatisticsRows(rows, visit)
}

func scanStatisticsRows(rows *sql.Rows, visit func(Entry)) error {
	defer func() { _ = rows.Close() }()
	// Scan 保留在循环内，避免每条轻量记录整体逃逸到堆。
	for rows.Next() {
		var record Entry
		var failed int
		if err := rows.Scan(
			&record.Timestamp,
			&record.APIKey,
			&record.Provider,
			&record.Model,
			&record.Source,
			&record.AuthType,
			&record.ServiceTier,
			&record.LatencyMs,
			&record.TTFTMs,
			&record.InputTokens,
			&record.OutputTokens,
			&record.CachedTokens,
			&record.CacheCreationTokens,
			&record.TotalTokens,
			&failed,
		); err != nil {
			return fmt.Errorf("usage sqlite scan statistics row: %w", err)
		}
		record.Failed = failed != 0
		visit(record)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("usage sqlite statistics rows: %w", err)
	}
	return nil
}
