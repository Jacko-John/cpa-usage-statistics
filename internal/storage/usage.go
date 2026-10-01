package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
)

type SQLExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type SQLQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Insert 写入一条用量记录。
func (s *Store) Insert(ctx context.Context, record usagemodel.Record) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("usage sqlite store is nil")
	}
	if strings.TrimSpace(record.ID) == "" {
		return fmt.Errorf("usage record id is empty")
	}

	record = usagemodel.NormalizeRecord(record)
	return s.WithConn(ctx, func(conn *sql.Conn) error {
		if err := insertUsageRecord(ctx, conn, record); err != nil {
			return fmt.Errorf("usage sqlite insert: %w", err)
		}
		s.statistics.Add(record, time.Now().Unix())
		return nil
	})
}

func insertUsageRecord(ctx context.Context, exec SQLExecer, record usagemodel.Record) error {
	failed := 0
	if record.Failed {
		failed = 1
	}
	_, err := exec.ExecContext(ctx, `INSERT INTO usage_records (`+usageRecordColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID,
		record.Timestamp.Unix(),
		record.APIKey,
		record.Provider,
		record.Model,
		record.Alias,
		record.Source,
		record.AuthID,
		record.AuthIndex,
		record.AuthType,
		record.ExecutorType,
		record.ReasoningEffort,
		record.ServiceTier,
		record.LatencyMs,
		record.TTFTMs,
		record.Tokens.InputTokens,
		record.Tokens.OutputTokens,
		record.Tokens.ReasoningTokens,
		record.Tokens.CachedTokens,
		record.Tokens.CacheReadTokens,
		record.Tokens.CacheCreationTokens,
		record.Tokens.TotalTokens,
		failed,
		record.FailureStatusCode,
		record.FailureBody,
	)
	return err
}

// Query 按 API Key（无 Key 时按 provider）和模型分组返回用量记录。
func (s *Store) Query(ctx context.Context, rng usagemodel.QueryRange) (usagemodel.APIUsage, error) {
	return s.queryUsage(ctx, rng, false, 0)
}

// QueryRecent 返回时间范围内最新的最多 100 条用量事件。
func (s *Store) QueryRecent(ctx context.Context, rng usagemodel.QueryRange, limit int) (usagemodel.APIUsage, error) {
	return s.queryUsage(ctx, rng, true, min(max(limit, 1), 100))
}

func (s *Store) queryUsage(ctx context.Context, rng usagemodel.QueryRange, recent bool, limit int) (usagemodel.APIUsage, error) {
	if s == nil || s.db == nil {
		return usagemodel.APIUsage{}, nil
	}

	query := "SELECT " + usageRecordColumns + " FROM usage_records"
	args := make([]any, 0, 3)
	separator := " WHERE "
	if rng.Start != nil && !rng.Start.IsZero() {
		query += separator + "timestamp >= ?"
		args = append(args, ceilUnixSecond(*rng.Start))
		separator = " AND "
	}
	if rng.End != nil && !rng.End.IsZero() {
		query += separator + "timestamp < ?"
		args = append(args, ceilUnixSecond(*rng.End))
	}
	queryError := "usage sqlite query"
	rowsError := "usage sqlite rows"
	if recent {
		query += " ORDER BY timestamp DESC, id DESC LIMIT ?"
		args = append(args, limit)
		queryError = "usage sqlite query recent"
		rowsError = "usage sqlite recent rows"
	} else {
		query += " ORDER BY timestamp ASC, id ASC"
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", queryError, err)
	}
	defer func() { _ = rows.Close() }()

	result := usagemodel.APIUsage{}
	for rows.Next() {
		var detail usagemodel.RequestDetail
		var apiKey string
		if err := scanUsageDetail(rows, &apiKey, &detail); err != nil {
			return nil, err
		}
		appendUsageDetail(result, usagemodel.GroupingKey(apiKey, detail.Provider), usagemodel.NormalizeModel(detail.Model), detail)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", rowsError, err)
	}
	return result, nil
}

func scanUsageDetail(rows *sql.Rows, apiKey *string, detail *usagemodel.RequestDetail) error {
	var timestamp int64
	var failed int
	if err := rows.Scan(
		&detail.ID,
		&timestamp,
		apiKey,
		&detail.Provider,
		&detail.Model,
		&detail.Alias,
		&detail.Source,
		&detail.AuthID,
		&detail.AuthIndex,
		&detail.AuthType,
		&detail.ExecutorType,
		&detail.ReasoningEffort,
		&detail.ServiceTier,
		&detail.LatencyMs,
		&detail.TTFTMs,
		&detail.Tokens.InputTokens,
		&detail.Tokens.OutputTokens,
		&detail.Tokens.ReasoningTokens,
		&detail.Tokens.CachedTokens,
		&detail.Tokens.CacheReadTokens,
		&detail.Tokens.CacheCreationTokens,
		&detail.Tokens.TotalTokens,
		&failed,
		&detail.FailureStatusCode,
		&detail.FailureBody,
	); err != nil {
		return fmt.Errorf("usage sqlite scan: %w", err)
	}
	detail.Timestamp = time.Unix(timestamp, 0).UTC()
	detail.LatencyMs = max(detail.LatencyMs, 0)
	detail.TTFTMs = max(detail.TTFTMs, 0)
	detail.Failed = failed != 0
	return nil
}

func appendUsageDetail(result usagemodel.APIUsage, groupKey, modelKey string, detail usagemodel.RequestDetail) {
	models := result[groupKey]
	if models == nil {
		models = make(map[string][]usagemodel.RequestDetail)
		result[groupKey] = models
	}
	models[modelKey] = append(models[modelKey], detail)
}

// Delete 删除未受安全历史保护的用量记录。Missing 保持请求顺序，包含不存在或受保护的 ID。
func (s *Store) Delete(ctx context.Context, ids *usagemodel.StableStringSet) (usagemodel.DeleteResult, error) {
	result := usagemodel.DeleteResult{Missing: []string{}}
	values := ids.Values()
	if s == nil || s.db == nil {
		result.Missing = append(result.Missing, values...)
		return result, nil
	}
	if len(values) == 0 {
		return result, nil
	}
	const deleteBatchSize = 500
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()

	now := time.Now().Unix()
	pricing := s.statistics.Pricing()
	pricingKey := s.statistics.PricingKey()
	removed := statistics.NewAccumulator(now, pricing, pricingKey)
	deletedIDs := make(map[string]struct{}, len(values))
	if err := s.WithConn(ctx, func(conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("usage sqlite begin delete: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		for start := 0; start < len(values); start += deleteBatchSize {
			end := min(start+deleteBatchSize, len(values))
			if err := deleteUsageBatch(
				ctx,
				tx,
				values[start:end],
				deletedIDs,
				&removed,
				now,
			); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("usage sqlite commit delete: %w", err)
		}
		if len(deletedIDs) > 0 {
			s.statistics.Remove(&removed, time.Now().Unix())
		}
		return nil
	}); err != nil {
		return result, err
	}

	result.Deleted = int64(len(deletedIDs))
	if len(deletedIDs) == len(values) {
		return result, nil
	}
	for _, id := range values {
		if _, deleted := deletedIDs[id]; !deleted {
			result.Missing = append(result.Missing, id)
		}
	}
	return result, nil
}

func deleteUsageBatch(
	ctx context.Context,
	tx *sql.Tx,
	ids []string,
	deleted map[string]struct{},
	removed *statistics.Accumulator,
	now int64,
) error {
	placeholders := strings.Repeat("?,", len(ids))
	query := `DELETE FROM usage_records
WHERE id IN (` + placeholders[:len(placeholders)-1] + `)
  AND NOT EXISTS (
      SELECT 1
      FROM api_key_safety_history AS history
      WHERE history.usage_record_id = usage_records.id
  )
RETURNING id, ` + statistics.RecordColumns
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("usage sqlite delete batch: %w", err)
	}
	for rows.Next() {
		var id string
		var record statistics.Entry
		var failed int
		if err := rows.Scan(
			&id,
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
			_ = rows.Close()
			return fmt.Errorf("usage sqlite scan deleted record: %w", err)
		}
		record.Failed = failed != 0
		deleted[id] = struct{}{}
		removed.AddRecord(record, now)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("usage sqlite deleted rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("usage sqlite close deleted rows: %w", err)
	}
	return nil
}

// DeleteBefore 删除早于截止时间且没有安全历史关联的用量记录。
func (s *Store) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()

	now := time.Now().Unix()
	pricing := s.statistics.Pricing()
	pricingKey := s.statistics.PricingKey()
	var deleted int64
	var removed *statistics.Accumulator
	cutoffSecond := ceilUnixSecond(cutoff)
	err := s.WithConn(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return fmt.Errorf("usage sqlite begin delete before: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			}
		}()

		where := `
WHERE timestamp < ?
  AND NOT EXISTS (
      SELECT 1
      FROM api_key_safety_history AS history
      WHERE history.usage_record_id = usage_records.id
  )`
		rows, err := conn.QueryContext(
			ctx,
			"SELECT "+statistics.RecordColumns+" FROM usage_records"+where,
			cutoffSecond,
		)
		if err != nil {
			return fmt.Errorf("usage sqlite select delete before: %w", err)
		}
		// 直接 Scan，避免大批清理时为每条记录额外分配对象。
		for rows.Next() {
			var record statistics.Entry
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
				_ = rows.Close()
				return fmt.Errorf("usage sqlite scan deleted record: %w", err)
			}
			record.Failed = failed != 0
			if removed == nil {
				state := statistics.NewAccumulator(now, pricing, pricingKey)
				removed = &state
			}
			removed.AddRecord(record, now)
			deleted++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("usage sqlite deleted records: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("usage sqlite close deleted records: %w", err)
		}

		result, err := conn.ExecContext(
			ctx,
			"DELETE FROM usage_records"+where,
			cutoffSecond,
		)
		if err != nil {
			return fmt.Errorf("usage sqlite delete before: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("usage sqlite rows affected: %w", err)
		}
		if affected != deleted {
			return fmt.Errorf(
				"usage sqlite delete before changed %d rows after selecting %d",
				affected,
				deleted,
			)
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("usage sqlite commit delete before: %w", err)
		}
		committed = true
		if removed != nil {
			s.statistics.Remove(removed, time.Now().Unix())
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// ceilUnixSecond 将精确时间边界映射到第一个不早于它的整数秒。
func ceilUnixSecond(timestamp time.Time) int64 {
	seconds := timestamp.Unix()
	if timestamp.Nanosecond() != 0 {
		seconds++
	}
	return seconds
}
