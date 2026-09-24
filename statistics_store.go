package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const createUsagePricingSQL = `CREATE TABLE IF NOT EXISTS usage_pricing (
	name TEXT PRIMARY KEY,
	config BLOB NOT NULL
) WITHOUT ROWID`

const statisticsRecordColumns = `timestamp, api_key, provider, model, source, auth_type, service_tier,
       latency_ms, ttft_ms, input_tokens, output_tokens, cached_tokens,
       cache_creation_tokens, total_tokens, failed`

func (s *usageStatistics) rebuild(ctx context.Context, queryer sqlQueryer, pricing usagePricing) error {
	pricing, pricingKey := normalizeUsagePricing(pricing)
	now := time.Now().Unix()
	next := newUsageStatisticsState(now, pricing, pricingKey)
	if err := scanStatisticsRecords(ctx, queryer, func(record usageStatisticsRecord) {
		next.addRecord(record, now)
	}); err != nil {
		return err
	}
	s.mu.Lock()
	s.state = next
	s.mu.Unlock()
	return nil
}

func (s *SQLiteStore) loadUsagePricing(ctx context.Context) (usagePricing, error) {
	var encoded []byte
	err := s.db.QueryRowContext(
		ctx,
		"SELECT config FROM usage_pricing WHERE name = ?",
		usagePricingRowName,
	).Scan(&encoded)
	if err == sql.ErrNoRows {
		pricing, _ := normalizeUsagePricing(usagePricing{})
		return pricing, nil
	}
	if err != nil {
		return usagePricing{}, fmt.Errorf("usage sqlite load pricing: %w", err)
	}
	var pricing usagePricing
	if err := json.Unmarshal(encoded, &pricing); err != nil {
		return usagePricing{}, fmt.Errorf("usage sqlite decode pricing: %w", err)
	}
	pricing, _ = normalizeUsagePricing(pricing)
	return pricing, nil
}

func saveUsagePricing(ctx context.Context, exec sqlExecer, pricingKey string) error {
	if _, err := exec.ExecContext(ctx, `
INSERT INTO usage_pricing (name, config)
VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET config = excluded.config
`, usagePricingRowName, []byte(pricingKey)); err != nil {
		return fmt.Errorf("usage sqlite save pricing: %w", err)
	}
	return nil
}

func (s *SQLiteStore) usagePricing() usagePricing {
	return s.statistics.pricing()
}

func (s *SQLiteStore) updateUsagePricing(ctx context.Context, pricing usagePricing) (usagePricing, error) {
	pricing, pricingKey := normalizeUsagePricing(pricing)
	if s.statistics.pricingKey() == pricingKey {
		return pricing, nil
	}

	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()
	if s.statistics.pricingKey() == pricingKey {
		return pricing, nil
	}

	readerPath := filepath.ToSlash(s.path)
	// file URL 中的 Windows 盘符路径必须以斜杠开头。
	if filepath.VolumeName(s.path) != "" && !strings.HasPrefix(readerPath, "/") {
		readerPath = "/" + readerPath
	}
	readerURL := &url.URL{Scheme: "file", Path: readerPath, RawQuery: "mode=ro"}
	reader, err := sql.Open("sqlite", readerURL.String())
	if err != nil {
		return usagePricing{}, fmt.Errorf("usage sqlite open statistics reader: %w", err)
	}
	reader.SetMaxOpenConns(1)
	defer func() { _ = reader.Close() }()
	if err := reader.PingContext(ctx); err != nil {
		return usagePricing{}, fmt.Errorf("usage sqlite connect statistics reader: %w", err)
	}

	var snapshot *sql.Tx
	if err := s.withConn(ctx, func(_ *sql.Conn) error {
		var beginErr error
		snapshot, beginErr = reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if beginErr != nil {
			return fmt.Errorf("usage sqlite begin statistics snapshot: %w", beginErr)
		}
		var exists int
		if err := snapshot.QueryRowContext(
			ctx,
			"SELECT EXISTS(SELECT 1 FROM usage_records)",
		).Scan(&exists); err != nil {
			return fmt.Errorf("usage sqlite establish statistics snapshot: %w", err)
		}
		s.statistics.beginPricingRebuild()
		return nil
	}); err != nil {
		if snapshot != nil {
			_ = snapshot.Rollback()
		}
		return usagePricing{}, err
	}

	pending := true
	defer func() {
		if pending {
			s.statistics.cancelPricingRebuild()
		}
	}()

	now := time.Now().Unix()
	next := newUsageStatisticsState(now, pricing, pricingKey)
	if err := scanStatisticsRecords(ctx, snapshot, func(record usageStatisticsRecord) {
		next.addRecord(record, now)
	}); err != nil {
		_ = snapshot.Rollback()
		return usagePricing{}, err
	}
	if err := snapshot.Commit(); err != nil {
		return usagePricing{}, fmt.Errorf("usage sqlite finish statistics snapshot: %w", err)
	}

	if err := s.withConn(ctx, func(conn *sql.Conn) error {
		// 到期周期先按旧价格封存；尚未结束的周期随后读取重建后的共享统计。
		if err := s.settleNotifications(ctx, conn, time.Now().Unix()); err != nil {
			return err
		}
		if err := saveUsagePricing(ctx, conn, pricingKey); err != nil {
			return err
		}
		s.statistics.finishPricingRebuild(next)
		pending = false
		return nil
	}); err != nil {
		return usagePricing{}, err
	}
	return pricing, nil
}

// usageAPIWindows 的调用方持有唯一连接，保证缓存、补查明细与价格版本一致。
func (s *SQLiteStore) usageAPIWindows(ctx context.Context, queryer sqlQueryer, queries []usageAPIWindowQuery, now int64) ([]usageAPIWindowSummary, error) {
	results, missing, pricing := s.statistics.apiWindows(queries, now)
	if len(missing) == 0 {
		return results, nil
	}
	limit := floorUnix(now, statisticsMinute) + statisticsMinute
	start, end := limit, int64(0)
	byAPI := make(map[string][]int)
	for _, i := range missing {
		query := queries[i]
		start = min(start, query.Start)
		end = max(end, min(query.End, limit))
		key := strings.TrimSpace(query.APIKey)
		byAPI[key] = append(byAPI[key], i)
	}
	rows, err := queryer.QueryContext(ctx, `SELECT `+statisticsRecordColumns+`
FROM usage_records WHERE timestamp >= ? AND timestamp < ?`, start, end)
	if err != nil {
		return nil, fmt.Errorf("usage sqlite query historical windows: %w", err)
	}
	multipliers := buildTierMultiplierIndex(pricing.TierMultipliers)
	err = scanStatisticsRows(rows, func(record usageStatisticsRecord) {
		indices := byAPI[groupingKey(record.APIKey, record.Provider)]
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
			metric := &results[i]
			metric.Requests++
			metric.Tokens += record.TotalTokens
			metric.Cost += cost
			if !priced {
				metric.Unpriced++
			}
		}
	})
	return results, err
}

func scanStatisticsRecords(
	ctx context.Context,
	queryer sqlQueryer,
	visit func(usageStatisticsRecord),
) error {
	rows, err := queryer.QueryContext(ctx, `SELECT `+statisticsRecordColumns+`
FROM usage_records`)
	if err != nil {
		return fmt.Errorf("usage sqlite scan statistics: %w", err)
	}
	return scanStatisticsRows(rows, visit)
}

func scanStatisticsRows(rows *sql.Rows, visit func(usageStatisticsRecord)) error {
	defer func() { _ = rows.Close() }()
	// Scan 保留在循环内，避免每条轻量记录整体逃逸到堆。
	for rows.Next() {
		var record usageStatisticsRecord
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
