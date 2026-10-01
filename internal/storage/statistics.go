package storage

import (
	"context"

	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
)

func (s *Store) SummaryJSON(rangeName string, now int64) ([]byte, error) {
	return s.statistics.SummaryJSON(rangeName, now)
}

func (s *Store) CredentialWindowsJSON(queries []statistics.CredentialWindowQuery, now int64) ([]byte, error) {
	return s.statistics.CredentialWindowsJSON(queries, now)
}

// Stats exposes the shared engine for readers; writers must hold the store connection.
func (s *Store) Stats() *statistics.Engine { return s.statistics }

// UsageAPIWindows requires the caller to hold the unique connection.
func (s *Store) UsageAPIWindows(ctx context.Context, q statistics.Queryer, queries []statistics.APIWindowQuery, now int64) ([]statistics.APIWindowSummary, error) {
	return s.statistics.APIWindows(ctx, q, queries, now)
}
