package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
)

const (
	createSafetyStateSQL = `CREATE TABLE IF NOT EXISTS api_key_safety_state (
		api_key TEXT PRIMARY KEY,
		total_trigger_count INTEGER NOT NULL DEFAULT 0,
		total_limit_count INTEGER NOT NULL DEFAULT 0,
		recent_trigger_times BLOB NOT NULL DEFAULT X'',
		blocked_until INTEGER NOT NULL DEFAULT 0,
		active_limit_started_at INTEGER NOT NULL DEFAULT 0,
		active_limit_usage_record_id TEXT NOT NULL DEFAULT '',
		manual_disabled INTEGER NOT NULL DEFAULT 0
	) WITHOUT ROWID`
	createSafetyHistorySQL = `CREATE TABLE IF NOT EXISTS api_key_safety_history (
		api_key TEXT NOT NULL,
		occurred_at INTEGER NOT NULL,
		usage_record_id TEXT NOT NULL,
		blocked_until INTEGER NOT NULL DEFAULT 0,
		manual_released_at INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (api_key, occurred_at, usage_record_id)
	) WITHOUT ROWID`
	createSafetyHistoryIndexSQL = `CREATE INDEX IF NOT EXISTS idx_safety_history_usage_record_id ON api_key_safety_history(usage_record_id)`
)

func (s *Store) migrateSafetyStateSchema(ctx context.Context) error {
	var exists int
	if err := s.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM pragma_table_info('api_key_safety_state') WHERE name = 'manual_disabled'",
	).Scan(&exists); err != nil {
		return fmt.Errorf("usage sqlite inspect safety state schema: %w", err)
	}
	if exists != 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
ALTER TABLE api_key_safety_state
ADD COLUMN manual_disabled INTEGER NOT NULL DEFAULT 0
`); err != nil {
		return fmt.Errorf("usage sqlite migrate safety state schema: %w", err)
	}
	return nil
}

// InsertSafety 原子写入用量、安全历史和新的 Key 状态。
func (s *Store) InsertSafety(ctx context.Context, record usagemodel.Record, state usagemodel.SafetyState, history usagemodel.SafetyHistory) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("usage sqlite store is nil")
	}
	if strings.TrimSpace(record.ID) == "" {
		return fmt.Errorf("usage record id is empty")
	}

	record = usagemodel.NormalizeRecord(record)
	return s.WithConn(ctx, func(conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("usage sqlite begin safety insert: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		if err := insertUsageRecord(ctx, tx, record); err != nil {
			return fmt.Errorf("usage sqlite insert: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO api_key_safety_history (
	api_key, occurred_at, usage_record_id, blocked_until, manual_released_at
) VALUES (?, ?, ?, ?, ?)
`, history.APIKey, history.OccurredAt, history.UsageRecordID, history.BlockedUntil, history.ManualReleasedAt); err != nil {
			return fmt.Errorf("usage sqlite insert safety history: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO api_key_safety_state (
	api_key,
	total_trigger_count,
	total_limit_count,
	recent_trigger_times,
	blocked_until,
	active_limit_started_at,
	active_limit_usage_record_id,
	manual_disabled
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(api_key) DO UPDATE SET
	total_trigger_count          = excluded.total_trigger_count,
	total_limit_count            = excluded.total_limit_count,
	recent_trigger_times         = excluded.recent_trigger_times,
	blocked_until                = excluded.blocked_until,
	active_limit_started_at      = excluded.active_limit_started_at,
	active_limit_usage_record_id = excluded.active_limit_usage_record_id,
	manual_disabled             = excluded.manual_disabled
`,
			state.APIKey,
			state.TotalTriggerCount,
			state.TotalLimitCount,
			encodeTriggerTimes(state.RecentTriggerTimes),
			state.BlockedUntil,
			state.ActiveLimitStartedAt,
			state.ActiveLimitUsageRecordID,
			state.ManualDisabled,
		); err != nil {
			return fmt.Errorf("usage sqlite upsert safety state: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("usage sqlite commit safety insert: %w", err)
		}
		s.statistics.Add(record, time.Now().Unix())
		return nil
	})
}

// LoadSafetyStates 加载全部 API Key 安全状态。
func (s *Store) LoadSafetyStates(ctx context.Context) (map[string]usagemodel.SafetyState, error) {
	if s == nil || s.db == nil {
		return map[string]usagemodel.SafetyState{}, nil
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT
	api_key,
	total_trigger_count,
	total_limit_count,
	recent_trigger_times,
	blocked_until,
	active_limit_started_at,
	active_limit_usage_record_id,
	manual_disabled
FROM api_key_safety_state
`)
	if err != nil {
		return nil, fmt.Errorf("usage sqlite load safety states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	states := make(map[string]usagemodel.SafetyState)
	for rows.Next() {
		var state usagemodel.SafetyState
		var encoded []byte
		if err := rows.Scan(
			&state.APIKey,
			&state.TotalTriggerCount,
			&state.TotalLimitCount,
			&encoded,
			&state.BlockedUntil,
			&state.ActiveLimitStartedAt,
			&state.ActiveLimitUsageRecordID,
			&state.ManualDisabled,
		); err != nil {
			return nil, fmt.Errorf("usage sqlite scan safety state: %w", err)
		}
		times, err := decodeTriggerTimes(encoded)
		if err != nil {
			return nil, fmt.Errorf("usage sqlite decode safety state: %w", err)
		}
		state.RecentTriggerTimes = times
		states[state.APIKey] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage sqlite safety state rows: %w", err)
	}
	return states, nil
}

// ClearSafetyWindows 清空全部持久化滑动窗口。
func (s *Store) ClearSafetyWindows(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE api_key_safety_state SET recent_trigger_times = X''"); err != nil {
		return fmt.Errorf("usage sqlite clear safety windows: %w", err)
	}
	return nil
}

// ReleaseSafetyLimits 原子解除指定 Key 的限流并标记对应历史。
func (s *Store) ReleaseSafetyLimits(ctx context.Context, states []usagemodel.SafetyState, releasedAt int64) error {
	if s == nil || s.db == nil || len(states) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usage sqlite begin safety release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, state := range states {
		if err := markSafetyLimitReleased(ctx, tx, state, releasedAt); err != nil {
			return err
		}
		if err := clearSafetyLimit(ctx, tx, state.APIKey, state.ManualDisabled); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage sqlite commit safety release: %w", err)
	}
	return nil
}

// SetManualDisabled 持久化手动禁用状态；启用时同时解除当前自动限流。
func (s *Store) SetManualDisabled(ctx context.Context, states []usagemodel.SafetyState, disabled bool, now int64) error {
	if s == nil || s.db == nil || len(states) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usage sqlite begin manual disable update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, state := range states {
		if disabled {
			if _, err := tx.ExecContext(ctx, "UPDATE api_key_safety_state SET manual_disabled = 1 WHERE api_key = ?", state.APIKey); err != nil {
				return fmt.Errorf("usage sqlite disable api key: %w", err)
			}
			continue
		}
		if state.BlockedUntil > now {
			if err := markSafetyLimitReleased(ctx, tx, state, now); err != nil {
				return err
			}
		}
		if err := clearSafetyLimit(ctx, tx, state.APIKey, false); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage sqlite commit manual disable update: %w", err)
	}
	return nil
}

func markSafetyLimitReleased(ctx context.Context, tx *sql.Tx, state usagemodel.SafetyState, releasedAt int64) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE api_key_safety_history
SET manual_released_at = ?
WHERE api_key = ?
  AND occurred_at = ?
  AND usage_record_id = ?
  AND blocked_until > 0
  AND manual_released_at = 0
`, releasedAt, state.APIKey, state.ActiveLimitStartedAt, state.ActiveLimitUsageRecordID); err != nil {
		return fmt.Errorf("usage sqlite release safety history: %w", err)
	}
	return nil
}

func clearSafetyLimit(ctx context.Context, tx *sql.Tx, apiKey string, manualDisabled bool) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE api_key_safety_state
SET recent_trigger_times = X'',
    blocked_until = 0,
    active_limit_started_at = 0,
    active_limit_usage_record_id = '',
    manual_disabled = ?
WHERE api_key = ?
`, manualDisabled, apiKey); err != nil {
		return fmt.Errorf("usage sqlite clear safety state: %w", err)
	}
	return nil
}

// QuerySafetyHistory 查询一个 API Key 的安全触发历史。
func (s *Store) QuerySafetyHistory(ctx context.Context, query usagemodel.SafetyHistoryQuery) ([]usagemodel.SafetyHistory, error) {
	if s == nil || s.db == nil {
		return []usagemodel.SafetyHistory{}, nil
	}

	statement := `
SELECT
	h.occurred_at,
	h.usage_record_id,
	h.blocked_until,
	h.manual_released_at,
	COALESCE(u.provider, ''),
	COALESCE(u.model, ''),
	COALESCE(u.failure_status_code, 0),
	COALESCE(u.failure_body, '')
FROM api_key_safety_history AS h
LEFT JOIN usage_records AS u ON u.id = h.usage_record_id
WHERE h.api_key = ?`
	args := make([]any, 1, 5)
	args[0] = query.APIKey
	if query.BlockedOnly {
		statement += " AND h.blocked_until > 0"
	}
	if query.BeforeID != "" {
		statement += " AND (h.occurred_at < ? OR (h.occurred_at = ? AND h.usage_record_id < ?))"
		args = append(args, query.BeforeTime, query.BeforeTime, query.BeforeID)
	}
	statement += " ORDER BY h.occurred_at DESC, h.usage_record_id DESC LIMIT ?"
	args = append(args, query.Limit)

	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("usage sqlite query safety history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	history := make([]usagemodel.SafetyHistory, 0, min(query.Limit, 100))
	for rows.Next() {
		record := usagemodel.SafetyHistory{APIKey: query.APIKey}
		if err := rows.Scan(
			&record.OccurredAt,
			&record.UsageRecordID,
			&record.BlockedUntil,
			&record.ManualReleasedAt,
			&record.Provider,
			&record.Model,
			&record.FailureStatusCode,
			&record.FailureBody,
		); err != nil {
			return nil, fmt.Errorf("usage sqlite scan safety history: %w", err)
		}
		history = append(history, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage sqlite safety history rows: %w", err)
	}
	return history, nil
}
