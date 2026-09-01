package main

import (
	"context"
	"fmt"
	"strings"
)

// initSchema 创建当前表、索引，并补齐旧版 usage_records 缺失的列。
func (s *SQLiteStore) initSchema(ctx context.Context) error {
	statements := [...]string{
		`CREATE TABLE IF NOT EXISTS usage_records (
		id TEXT PRIMARY KEY,
		timestamp INTEGER NOT NULL,
		api_key TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		alias TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT '',
		auth_id TEXT NOT NULL DEFAULT '',
		auth_index TEXT NOT NULL DEFAULT '',
		auth_type TEXT NOT NULL DEFAULT '',
		executor_type TEXT NOT NULL DEFAULT '',
		reasoning_effort TEXT NOT NULL DEFAULT '',
		service_tier TEXT NOT NULL DEFAULT '',
		latency_ms INTEGER NOT NULL DEFAULT 0 CHECK (latency_ms >= 0),
		ttft_ms INTEGER NOT NULL DEFAULT 0 CHECK (ttft_ms >= 0),
		input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
		output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
		reasoning_tokens INTEGER NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
		cached_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cached_tokens >= 0),
		cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
		cache_creation_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_creation_tokens >= 0),
		total_tokens INTEGER NOT NULL DEFAULT 0 CHECK (total_tokens >= 0),
		failed INTEGER NOT NULL DEFAULT 0 CHECK (failed IN (0, 1)),
		failure_status_code INTEGER NOT NULL DEFAULT 0 CHECK (failure_status_code >= 0),
		failure_body TEXT NOT NULL DEFAULT ''
	)`,
		`CREATE TABLE IF NOT EXISTS api_key_safety_state (
			api_key TEXT PRIMARY KEY,
			total_trigger_count INTEGER NOT NULL DEFAULT 0,
			total_limit_count INTEGER NOT NULL DEFAULT 0,
			recent_trigger_times BLOB NOT NULL DEFAULT X'',
			blocked_until INTEGER NOT NULL DEFAULT 0,
			active_limit_started_at INTEGER NOT NULL DEFAULT 0,
			active_limit_usage_record_id TEXT NOT NULL DEFAULT ''
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS api_key_safety_history (
			api_key TEXT NOT NULL,
			occurred_at INTEGER NOT NULL,
			usage_record_id TEXT NOT NULL,
			blocked_until INTEGER NOT NULL DEFAULT 0,
			manual_released_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (api_key, occurred_at, usage_record_id)
		) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS idx_safety_history_usage_record_id ON api_key_safety_history(usage_record_id)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("usage sqlite init schema: %w", err)
		}
	}
	if err := s.migrateSchema(ctx); err != nil {
		return err
	}
	if err := s.migrateUsageTimestamp(ctx); err != nil {
		return err
	}
	return s.initUsageIndexes(ctx)
}

func (s *SQLiteStore) initUsageIndexes(ctx context.Context) error {
	statements := [...]string{
		`CREATE INDEX IF NOT EXISTS idx_usage_records_timestamp_id ON usage_records(timestamp, id)`,
		`DROP INDEX IF EXISTS idx_usage_records_timestamp`,
		`DROP INDEX IF EXISTS idx_usage_records_api_model`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("usage sqlite init indexes: %w", err)
		}
	}
	return nil
}

// migrateSchema 补齐旧版 usage_records 缺失的列。ADD COLUMN 不附加 CHECK，
// 数值字段由写入前归一化保证非负。
func (s *SQLiteStore) migrateSchema(ctx context.Context) error {
	existing, err := s.existingColumns(ctx)
	if err != nil {
		return err
	}
	additions := [...]struct {
		name string
		ddl  string
	}{
		{"api_key", `ALTER TABLE usage_records ADD COLUMN api_key TEXT NOT NULL DEFAULT ''`},
		{"provider", `ALTER TABLE usage_records ADD COLUMN provider TEXT NOT NULL DEFAULT ''`},
		{"model", `ALTER TABLE usage_records ADD COLUMN model TEXT NOT NULL DEFAULT ''`},
		{"alias", `ALTER TABLE usage_records ADD COLUMN alias TEXT NOT NULL DEFAULT ''`},
		{"source", `ALTER TABLE usage_records ADD COLUMN source TEXT NOT NULL DEFAULT ''`},
		{"auth_id", `ALTER TABLE usage_records ADD COLUMN auth_id TEXT NOT NULL DEFAULT ''`},
		{"auth_index", `ALTER TABLE usage_records ADD COLUMN auth_index TEXT NOT NULL DEFAULT ''`},
		{"auth_type", `ALTER TABLE usage_records ADD COLUMN auth_type TEXT NOT NULL DEFAULT ''`},
		{"executor_type", `ALTER TABLE usage_records ADD COLUMN executor_type TEXT NOT NULL DEFAULT ''`},
		{"reasoning_effort", `ALTER TABLE usage_records ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT ''`},
		{"service_tier", `ALTER TABLE usage_records ADD COLUMN service_tier TEXT NOT NULL DEFAULT ''`},
		{"latency_ms", `ALTER TABLE usage_records ADD COLUMN latency_ms INTEGER NOT NULL DEFAULT 0`},
		{"ttft_ms", `ALTER TABLE usage_records ADD COLUMN ttft_ms INTEGER NOT NULL DEFAULT 0`},
		{"input_tokens", `ALTER TABLE usage_records ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`},
		{"output_tokens", `ALTER TABLE usage_records ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`},
		{"reasoning_tokens", `ALTER TABLE usage_records ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0`},
		{"cached_tokens", `ALTER TABLE usage_records ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`},
		{"cache_read_tokens", `ALTER TABLE usage_records ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`},
		{"cache_creation_tokens", `ALTER TABLE usage_records ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`},
		{"total_tokens", `ALTER TABLE usage_records ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`},
		{"failed", `ALTER TABLE usage_records ADD COLUMN failed INTEGER NOT NULL DEFAULT 0`},
		{"failure_status_code", `ALTER TABLE usage_records ADD COLUMN failure_status_code INTEGER NOT NULL DEFAULT 0`},
		{"failure_body", `ALTER TABLE usage_records ADD COLUMN failure_body TEXT NOT NULL DEFAULT ''`},
	}
	for _, addition := range additions {
		if _, ok := existing[addition.name]; ok {
			continue
		}
		if _, err := s.db.ExecContext(ctx, addition.ddl); err != nil {
			return fmt.Errorf("usage sqlite migrate add %s: %w", addition.name, err)
		}
	}
	return nil
}

func (s *SQLiteStore) migrateUsageTimestamp(ctx context.Context) error {
	var columnType string
	if err := s.db.QueryRowContext(
		ctx,
		"SELECT type FROM pragma_table_info('usage_records') WHERE name = 'timestamp'",
	).Scan(&columnType); err != nil {
		return fmt.Errorf("usage sqlite read timestamp type: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(columnType), "INTEGER") {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usage sqlite begin timestamp migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var invalid int64
	if err := tx.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM usage_records WHERE unixepoch(timestamp) IS NULL",
	).Scan(&invalid); err != nil {
		return fmt.Errorf("usage sqlite validate timestamps: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("usage sqlite migrate timestamp: %d invalid values", invalid)
	}

	statements := [...]string{
		`DROP INDEX IF EXISTS idx_usage_records_timestamp_id`,
		`DROP INDEX IF EXISTS idx_usage_records_timestamp`,
		`ALTER TABLE usage_records RENAME COLUMN timestamp TO timestamp_text`,
		`ALTER TABLE usage_records ADD COLUMN timestamp INTEGER NOT NULL DEFAULT 0`,
		`UPDATE usage_records SET timestamp = unixepoch(timestamp_text)`,
		`ALTER TABLE usage_records DROP COLUMN timestamp_text`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("usage sqlite migrate timestamp: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage sqlite commit timestamp migration: %w", err)
	}
	return nil
}

// existingColumns 返回 usage_records 当前已有的列名集合。
func (s *SQLiteStore) existingColumns(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pragma_table_info('usage_records')")
	if err != nil {
		return nil, fmt.Errorf("usage sqlite table_info: %w", err)
	}
	defer func() { _ = rows.Close() }()

	columns := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("usage sqlite table_info scan: %w", err)
		}
		columns[name] = struct{}{}
	}
	return columns, rows.Err()
}
