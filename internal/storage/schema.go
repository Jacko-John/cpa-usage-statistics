package storage

import (
	"context"
	"fmt"
	"strings"
)

const CreateUsageRecordsSQL = `CREATE TABLE IF NOT EXISTS usage_records (
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
	latency_ms INTEGER NOT NULL DEFAULT 0,
	ttft_ms INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	failure_status_code INTEGER NOT NULL DEFAULT 0,
	failure_body TEXT NOT NULL DEFAULT ''
)`

const usageRecordColumns = `
	id, timestamp, api_key, provider, model, alias, source, auth_id, auth_index, auth_type, executor_type,
	reasoning_effort, service_tier, latency_ms, ttft_ms,
	input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
	failed, failure_status_code, failure_body
`

// initSchema 创建当前表、索引，并补齐旧版 usage_records 缺失的列。
func (s *Store) initSchema(ctx context.Context) error {
	statements := [...]string{
		CreateUsageRecordsSQL,
		createSafetyStateSQL,
		createSafetyHistorySQL,
		createUsagePricingSQL,
		createSafetyHistoryIndexSQL,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("usage sqlite init schema: %w", err)
		}
	}
	if err := s.migrateSchema(ctx); err != nil {
		return err
	}
	if err := s.migrateSafetyStateSchema(ctx); err != nil {
		return err
	}
	if err := s.migrateUsageTimestamp(ctx); err != nil {
		return err
	}
	if err := s.migrateUsageChecks(ctx); err != nil {
		return err
	}
	return s.initUsageIndexes(ctx)
}

func (s *Store) initUsageIndexes(ctx context.Context) error {
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
func (s *Store) migrateSchema(ctx context.Context) error {
	existing, err := s.ExistingColumns(ctx)
	if err != nil {
		return err
	}
	additions := [...]struct {
		name       string
		definition string
	}{
		{"api_key", `TEXT NOT NULL DEFAULT ''`},
		{"provider", `TEXT NOT NULL DEFAULT ''`},
		{"model", `TEXT NOT NULL DEFAULT ''`},
		{"alias", `TEXT NOT NULL DEFAULT ''`},
		{"source", `TEXT NOT NULL DEFAULT ''`},
		{"auth_id", `TEXT NOT NULL DEFAULT ''`},
		{"auth_index", `TEXT NOT NULL DEFAULT ''`},
		{"auth_type", `TEXT NOT NULL DEFAULT ''`},
		{"executor_type", `TEXT NOT NULL DEFAULT ''`},
		{"reasoning_effort", `TEXT NOT NULL DEFAULT ''`},
		{"service_tier", `TEXT NOT NULL DEFAULT ''`},
		{"latency_ms", `INTEGER NOT NULL DEFAULT 0`},
		{"ttft_ms", `INTEGER NOT NULL DEFAULT 0`},
		{"input_tokens", `INTEGER NOT NULL DEFAULT 0`},
		{"output_tokens", `INTEGER NOT NULL DEFAULT 0`},
		{"reasoning_tokens", `INTEGER NOT NULL DEFAULT 0`},
		{"cached_tokens", `INTEGER NOT NULL DEFAULT 0`},
		{"cache_read_tokens", `INTEGER NOT NULL DEFAULT 0`},
		{"cache_creation_tokens", `INTEGER NOT NULL DEFAULT 0`},
		{"total_tokens", `INTEGER NOT NULL DEFAULT 0`},
		{"failed", `INTEGER NOT NULL DEFAULT 0`},
		{"failure_status_code", `INTEGER NOT NULL DEFAULT 0`},
		{"failure_body", `TEXT NOT NULL DEFAULT ''`},
	}
	for _, addition := range additions {
		if _, ok := existing[addition.name]; ok {
			continue
		}
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE usage_records ADD COLUMN "+addition.name+" "+addition.definition); err != nil {
			return fmt.Errorf("usage sqlite migrate add %s: %w", addition.name, err)
		}
	}
	return nil
}

func (s *Store) migrateUsageTimestamp(ctx context.Context) error {
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

func (s *Store) migrateUsageChecks(ctx context.Context) error {
	var schema string
	if err := s.db.QueryRowContext(
		ctx,
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'usage_records'",
	).Scan(&schema); err != nil {
		return fmt.Errorf("usage sqlite read usage schema: %w", err)
	}
	if !strings.Contains(strings.ToUpper(schema), "CHECK") {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usage sqlite begin check removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "ALTER TABLE usage_records RENAME TO usage_records_checked"); err != nil {
		return fmt.Errorf("usage sqlite rename checked table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, CreateUsageRecordsSQL); err != nil {
		return fmt.Errorf("usage sqlite create unchecked table: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO usage_records ("+usageRecordColumns+") SELECT "+usageRecordColumns+" FROM usage_records_checked",
	); err != nil {
		return fmt.Errorf("usage sqlite copy unchecked table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE usage_records_checked"); err != nil {
		return fmt.Errorf("usage sqlite drop checked table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage sqlite commit check removal: %w", err)
	}
	return nil
}

// existingColumns 返回 usage_records 当前已有的列名集合。
func (s *Store) ExistingColumns(ctx context.Context) (map[string]struct{}, error) {
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
