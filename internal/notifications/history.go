package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
	"github.com/Jacko-John/cpa-usage-statistics/internal/storage"
)

const notificationPeriodColumns = `id,start,end,timezone,email,key_label,note,partial,status,requests,tokens,cost,unpriced,attempts,retry_at,error,skipped,details`

func scanNotificationPeriods(ctx context.Context, q storage.SQLQueryer, where string, args ...any) ([]Period, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+notificationPeriodColumns+` FROM notification_periods `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Period{}
	for rows.Next() {
		var p Period
		var details []byte
		if err := rows.Scan(&p.ID, &p.Start, &p.End, &p.Timezone, &p.Email, &p.KeyLabel, &p.Note, &p.Partial, &p.Status, &p.Requests, &p.Tokens, &p.Cost, &p.Unpriced, &p.Attempts, &p.RetryAt, &p.Error, &p.Skipped, &details); err != nil {
			return nil, err
		}
		if len(details) > 0 {
			if err := json.Unmarshal(details, &p.Details); err != nil {
				return nil, fmt.Errorf("usage sqlite decode notification details %s: %w", p.ID, err)
			}
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

type History struct {
	Current []Period `json:"current"`
	Periods []Period `json:"periods"`
}

func (s *Service) HistoryGet(ctx context.Context, id string, now int64) (History, error) {
	result := History{Current: []Period{}, Periods: []Period{}}
	err := s.store.WithConn(ctx, func(conn *sql.Conn) error {
		if err := s.Settle(ctx, conn, now); err != nil {
			return err
		}
		var key string
		for _, seg := range s.segments {
			if seg.Subscription != id {
				continue
			}
			key = seg.Key
			if now >= seg.Start && (seg.Until == 0 || now < seg.Until) {
				start, end := seg.period(now)
				if err := ensureNotificationPeriod(ctx, conn, seg, start, end); err != nil {
					return err
				}
			}
		}
		var err error
		result.Current, err = scanNotificationPeriods(ctx, conn, `WHERE subscription=? AND status='open' ORDER BY start`, id)
		if err != nil {
			return err
		}
		queries := make([]statistics.APIWindowQuery, len(result.Current))
		for i, period := range result.Current {
			queries[i] = statistics.APIWindowQuery{APIKey: key, Start: period.Start, End: period.End}
		}
		amounts, err := s.store.UsageAPIWindows(ctx, conn, queries, now)
		if err != nil {
			return err
		}
		for i, amount := range amounts {
			p := &result.Current[i]
			p.Cost = amount.Cost
			p.Requests = amount.Requests
			p.Tokens = amount.Tokens
			p.Unpriced = amount.Unpriced
			p.Details = amount.Details
		}
		result.Periods, err = scanNotificationPeriods(ctx, conn, `WHERE subscription=? AND status!='open' ORDER BY end DESC,id DESC LIMIT 10`, id)
		return err
	})
	return result, err
}
