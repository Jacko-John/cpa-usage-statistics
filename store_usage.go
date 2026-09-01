package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Insert 写入一条用量记录。
func (s *SQLiteStore) Insert(ctx context.Context, record Record) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("usage sqlite store is nil")
	}
	if strings.TrimSpace(record.ID) == "" {
		return fmt.Errorf("usage record id is empty")
	}

	record = normalizeStoredRecord(record)
	if err := insertUsageRecord(ctx, s.db, record); err != nil {
		return fmt.Errorf("usage sqlite insert: %w", err)
	}
	return nil
}

func insertUsageRecord(ctx context.Context, exec sqlExecer, record Record) error {
	failed := 0
	if record.Failed {
		failed = 1
	}
	_, err := exec.ExecContext(ctx, `
INSERT INTO usage_records (
	id, timestamp, api_key, provider, model, alias, source, auth_id, auth_index, auth_type, executor_type,
	reasoning_effort, service_tier, latency_ms, ttft_ms,
	input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
	failed, failure_status_code, failure_body
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`,
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
func (s *SQLiteStore) Query(ctx context.Context, rng QueryRange) (APIUsage, error) {
	if s == nil || s.db == nil {
		return APIUsage{}, nil
	}

	query := `
SELECT id, timestamp, api_key, provider, model, alias, source, auth_id, auth_index, auth_type, executor_type,
       reasoning_effort, service_tier, latency_ms, ttft_ms,
       input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
       failed, failure_status_code, failure_body
FROM usage_records`
	args := make([]any, 0, 2)
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
	query += " ORDER BY timestamp ASC, id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("usage sqlite query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := APIUsage{}
	for rows.Next() {
		var detail RequestDetail
		var apiKey string
		if err := scanUsageDetail(rows, &apiKey, &detail); err != nil {
			return nil, err
		}
		appendUsageDetail(result, groupingKey(apiKey, detail.Provider), normalizeModel(detail.Model), detail)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage sqlite rows: %w", err)
	}
	return result, nil
}

func scanUsageDetail(rows *sql.Rows, apiKey *string, detail *RequestDetail) error {
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

func appendUsageDetail(result APIUsage, groupKey, modelKey string, detail RequestDetail) {
	models := result[groupKey]
	if models == nil {
		models = make(map[string][]RequestDetail)
		result[groupKey] = models
	}
	models[modelKey] = append(models[modelKey], detail)
}

// Delete 删除未受安全历史保护的用量记录。Missing 保持请求顺序，包含不存在或受保护的 ID。
func (s *SQLiteStore) Delete(ctx context.Context, ids *StableStringSet) (DeleteResult, error) {
	result := DeleteResult{Missing: []string{}}
	values := ids.Values()
	if s == nil || s.db == nil {
		result.Missing = append(result.Missing, values...)
		return result, nil
	}
	if len(values) == 0 {
		return result, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("usage sqlite begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const deleteBatchSize = 500
	deletedIDs := make(map[string]struct{}, len(values))
	for start := 0; start < len(values); start += deleteBatchSize {
		end := min(start+deleteBatchSize, len(values))
		if err := deleteUsageBatch(ctx, tx, values[start:end], deletedIDs); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("usage sqlite commit delete: %w", err)
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

func deleteUsageBatch(ctx context.Context, tx *sql.Tx, ids []string, deleted map[string]struct{}) error {
	placeholders := strings.Repeat("?,", len(ids))
	query := `DELETE FROM usage_records
WHERE id IN (` + placeholders[:len(placeholders)-1] + `)
  AND NOT EXISTS (
      SELECT 1
      FROM api_key_safety_history AS history
      WHERE history.usage_record_id = usage_records.id
  )
RETURNING id`
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
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("usage sqlite scan deleted id: %w", err)
		}
		deleted[id] = struct{}{}
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
func (s *SQLiteStore) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}

	result, err := s.db.ExecContext(ctx, `
DELETE FROM usage_records
WHERE timestamp < ?
  AND NOT EXISTS (
      SELECT 1
      FROM api_key_safety_history AS history
      WHERE history.usage_record_id = usage_records.id
  )
`, ceilUnixSecond(cutoff))
	if err != nil {
		return 0, fmt.Errorf("usage sqlite delete before: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("usage sqlite rows affected: %w", err)
	}
	return rows, nil
}

// ceilUnixSecond 将精确时间边界映射到第一个不早于它的整数秒。
func ceilUnixSecond(timestamp time.Time) int64 {
	seconds := timestamp.Unix()
	if timestamp.Nanosecond() != 0 {
		seconds++
	}
	return seconds
}

func normalizeStoredRecord(record Record) Record {
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now()
	}
	record.Timestamp = record.Timestamp.UTC()
	record.Model = normalizeModel(record.Model)
	record.LatencyMs = max(record.LatencyMs, 0)
	record.TTFTMs = max(record.TTFTMs, 0)
	record.Tokens = nonNegativeTokenStats(record.Tokens)
	record.Tokens.TotalTokens = normalizeTotalTokens(record.Tokens)
	record.FailureStatusCode = max(record.FailureStatusCode, 0)
	return record
}

func groupingKey(apiKey, provider string) string {
	if trimmed := strings.TrimSpace(apiKey); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(provider); trimmed != "" {
		return trimmed
	}
	return "unknown"
}

func normalizeModel(model string) string {
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		return trimmed
	}
	return "unknown"
}

func normalizeTotalTokens(tokens TokenStats) int64 {
	if tokens.TotalTokens != 0 {
		return tokens.TotalTokens
	}
	total := tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	if total != 0 {
		return total
	}
	return tokens.CachedTokens
}

func nonNegativeTokenStats(tokens TokenStats) TokenStats {
	tokens.InputTokens = max(tokens.InputTokens, 0)
	tokens.OutputTokens = max(tokens.OutputTokens, 0)
	tokens.ReasoningTokens = max(tokens.ReasoningTokens, 0)
	tokens.CachedTokens = max(tokens.CachedTokens, 0)
	tokens.CacheReadTokens = max(tokens.CacheReadTokens, 0)
	tokens.CacheCreationTokens = max(tokens.CacheCreationTokens, 0)
	tokens.TotalTokens = max(tokens.TotalTokens, 0)
	return tokens
}
