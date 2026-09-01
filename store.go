package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// SQLiteStore 持久化用量记录和 API Key 安全状态。
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore 打开或创建用量数据库。
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("usage sqlite path is empty")
	}
	if err := prepareSQLitePath(path); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("usage sqlite open: %w", err)
	}
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage sqlite enable wal: %w", err)
	}

	store := &SQLiteStore{db: db}
	if err := store.initSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func prepareSQLitePath(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("usage sqlite mkdir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("usage sqlite create: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("usage sqlite close created file: %w", err)
	}
	return nil
}

// Close 关闭数据库。
func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
