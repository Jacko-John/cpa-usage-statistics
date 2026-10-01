// Package testfixture provides SQLite fixtures for tests outside the storage package.
package testfixture

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
	"github.com/Jacko-John/cpa-usage-statistics/internal/storage"
)

type Store struct {
	*storage.Store
	DB         *sql.DB
	Path       string
	Statistics *statistics.Engine
}

// Open uses a separate SQL handle for fault injection and database assertions.
// Application writes still use the store's single connection.
func Open(path string) (*Store, error) {
	store, err := storage.Open(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return &Store{Store: store, DB: db, Path: path, Statistics: store.Stats()}, nil
}

func New(t testing.TB) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func (s *Store) Close() error {
	err := s.Store.Close()
	if closeErr := s.DB.Close(); err == nil {
		err = closeErr
	}
	return err
}
