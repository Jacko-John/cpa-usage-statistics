package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
)

const pricingRowName = "current"

const createUsagePricingSQL = `CREATE TABLE IF NOT EXISTS usage_pricing (
	name TEXT PRIMARY KEY,
	config BLOB NOT NULL
) WITHOUT ROWID`

func (s *Store) loadUsagePricing(ctx context.Context) (statistics.Pricing, error) {
	var encoded []byte
	err := s.db.QueryRowContext(
		ctx,
		"SELECT config FROM usage_pricing WHERE name = ?",
		pricingRowName,
	).Scan(&encoded)
	if err == sql.ErrNoRows {
		pricing, _ := statistics.NormalizePricing(statistics.Pricing{})
		return pricing, nil
	}
	if err != nil {
		return statistics.Pricing{}, fmt.Errorf("usage sqlite load pricing: %w", err)
	}
	var pricing statistics.Pricing
	if err := json.Unmarshal(encoded, &pricing); err != nil {
		return statistics.Pricing{}, fmt.Errorf("usage sqlite decode pricing: %w", err)
	}
	pricing, _ = statistics.NormalizePricing(pricing)
	return pricing, nil
}

func saveUsagePricing(ctx context.Context, exec SQLExecer, pricingKey string) error {
	if _, err := exec.ExecContext(ctx, `
INSERT INTO usage_pricing (name, config)
VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET config = excluded.config
`, pricingRowName, []byte(pricingKey)); err != nil {
		return fmt.Errorf("usage sqlite save pricing: %w", err)
	}
	return nil
}

func (s *Store) Pricing() statistics.Pricing {
	return s.statistics.Pricing()
}

func (s *Store) UpdatePricing(ctx context.Context, pricing statistics.Pricing) (statistics.Pricing, error) {
	pricing, pricingKey := statistics.NormalizePricing(pricing)
	if s.statistics.PricingKey() == pricingKey {
		return pricing, nil
	}

	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()
	if s.statistics.PricingKey() == pricingKey {
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
		return statistics.Pricing{}, fmt.Errorf("usage sqlite open statistics reader: %w", err)
	}
	reader.SetMaxOpenConns(1)
	defer func() { _ = reader.Close() }()
	if err := reader.PingContext(ctx); err != nil {
		return statistics.Pricing{}, fmt.Errorf("usage sqlite connect statistics reader: %w", err)
	}

	var snapshot *sql.Tx
	if err := s.WithConn(ctx, func(_ *sql.Conn) error {
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
		s.statistics.BeginPricingRebuild()
		return nil
	}); err != nil {
		if snapshot != nil {
			_ = snapshot.Rollback()
		}
		return statistics.Pricing{}, err
	}

	pending := true
	defer func() {
		if pending {
			s.statistics.CancelPricingRebuild()
		}
	}()

	now := time.Now().Unix()
	next := statistics.NewAccumulator(now, pricing, pricingKey)
	if err := statistics.ScanStatisticsRecords(ctx, snapshot, func(record statistics.Entry) {
		next.AddRecord(record, now)
	}); err != nil {
		_ = snapshot.Rollback()
		return statistics.Pricing{}, err
	}
	if err := snapshot.Commit(); err != nil {
		return statistics.Pricing{}, fmt.Errorf("usage sqlite finish statistics snapshot: %w", err)
	}

	if err := s.WithConn(ctx, func(conn *sql.Conn) error {
		// 到期周期先按旧价格封存；尚未结束的周期随后读取重建后的共享统计。
		if err := s.settleNotifications(ctx, conn, time.Now().Unix()); err != nil {
			return err
		}
		if err := saveUsagePricing(ctx, conn, pricingKey); err != nil {
			return err
		}
		s.statistics.FinishPricingRebuild(next)
		pending = false
		return nil
	}); err != nil {
		return statistics.Pricing{}, err
	}
	return pricing, nil
}
