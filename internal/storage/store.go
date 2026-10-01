package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
)

// Store 持久化用量记录和 API Key 安全状态。
type Store struct {
	db            *sql.DB
	path          string
	statistics    *statistics.Engine
	notifications PeriodSettler
	rebuildMu     sync.Mutex
}

// Open 打开或创建用量数据库。
func Open(path string) (*Store, error) {
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
	// 唯一连接同时保证 SQLite 操作与内存统计更新的顺序一致。
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage sqlite enable wal: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA synchronous = NORMAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage sqlite synchronous normal: %w", err)
	}

	store := &Store{db: db, path: path, statistics: &statistics.Engine{}}
	if err := store.initSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	pricing, err := store.loadUsagePricing(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.statistics.Rebuild(ctx, db, pricing); err != nil {
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

// WithConn 在回调完成后归还唯一 SQLite 连接。
func (s *Store) WithConn(ctx context.Context, use func(*sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("usage sqlite acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	return use(conn)
}

// Close 关闭数据库。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if s.notifications != nil {
		s.notifications.Stop()
	}
	return s.db.Close()
}
