package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	_ "time/tzdata"

	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
	"github.com/Jacko-John/cpa-usage-statistics/internal/storage"
)

type notificationSegment struct {
	ID           int64
	Subscription string
	Key          string
	Email        string
	Note         string
	Start        int64
	Until        int64
	Cursor       int64
	Schedule     Settings
}

func (s notificationSegment) period(minute int64) (int64, int64) {
	start := max(s.Start, s.Schedule.Boundary(minute, false))
	end := s.Schedule.Boundary(minute, true)
	if s.Until > 0 {
		end = min(end, s.Until)
	}
	return start, end
}

// restoreNotificationFirstPeriods 仅补齐旧版尚未结束、未封存且从未切换过的首段。
func restoreNotificationFirstPeriods(ctx context.Context, conn *sql.Conn, now int64) error {
	rows, err := conn.QueryContext(ctx, `SELECT s.id,s.start,s.schedule FROM notification_segments s
 JOIN notification_subscriptions b ON b.id=s.subscription
 WHERE s.until_at=0 AND s.cursor=s.start AND b.enabled=1 AND b.deleted=0
 AND NOT EXISTS(SELECT 1 FROM notification_segments other WHERE other.subscription=s.subscription AND other.id!=s.id)
 AND NOT EXISTS(SELECT 1 FROM notification_periods p WHERE p.segment=s.id AND p.status!='open')`)
	if err != nil {
		return err
	}
	type adjustment struct{ id, oldStart, start int64 }
	var changes []adjustment
	for rows.Next() {
		var id, start int64
		var encoded []byte
		if err := rows.Scan(&id, &start, &encoded); err != nil {
			rows.Close()
			return err
		}
		var cfg Settings
		if err := json.Unmarshal(encoded, &cfg); err != nil {
			rows.Close()
			return err
		}
		if !cfg.Enabled || start == cfg.Boundary(start, false) {
			continue
		}
		begin := cfg.Boundary(start-1, false)
		if cfg.Boundary(start-1, true) > now {
			changes = append(changes, adjustment{id, start, begin})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(changes) == 0 {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, change := range changes {
		if _, err := tx.ExecContext(ctx, `UPDATE notification_periods SET id=?,start=?,partial=0 WHERE segment=? AND start=? AND status='open'`, notificationPeriodID(change.id, change.start), change.start, change.id, change.oldStart); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notification_segments SET start=?,cursor=? WHERE id=?`, change.start, change.start, change.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func notificationPeriodID(segment, start int64) string { return fmt.Sprintf("%d-%d", segment, start) }

type Period struct {
	ID       string                       `json:"id"`
	Start    int64                        `json:"start"`
	End      int64                        `json:"end"`
	Timezone string                       `json:"timezone"`
	Email    string                       `json:"email"`
	KeyLabel string                       `json:"key_label"`
	Note     string                       `json:"note"`
	Partial  bool                         `json:"partial"`
	Status   string                       `json:"status"`
	Requests int64                        `json:"requests"`
	Tokens   int64                        `json:"tokens"`
	Cost     float64                      `json:"cost"`
	Unpriced int64                        `json:"unpriced_requests"`
	Details  []statistics.APIWindowDetail `json:"details"`
	Attempts int                          `json:"attempts"`
	RetryAt  int64                        `json:"retry_at"`
	Error    string                       `json:"error"`
	Skipped  int64                        `json:"skipped_cycles"`
}

func ensureNotificationPeriod(ctx context.Context, exec storage.SQLExecer, seg notificationSegment, start, end int64) error {
	partial := start != seg.Schedule.Boundary(start, false) || end != seg.Schedule.Boundary(start, true)
	_, err := exec.ExecContext(ctx, `INSERT INTO notification_periods(id,segment,subscription,start,end,timezone,email,key_label,note,partial)
 VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(segment,start) DO UPDATE SET end=excluded.end,partial=excluded.partial WHERE status='open'`,
		notificationPeriodID(seg.ID, start), seg.ID, seg.Subscription, start, end, seg.Schedule.Timezone, seg.Email, notificationMask(seg.Key), seg.Note, partial)
	return err
}

// 调用方持有唯一连接；读取共享统计后，在同一事务中保存快照和推进游标。
func (s *Service) Settle(ctx context.Context, conn *sql.Conn, now int64) error {
	if s == nil {
		return nil
	}
	type closingPeriod struct {
		segment notificationSegment
		start   int64
		end     int64
		skipped int64
	}
	type progress struct {
		segment   int64
		cursor    int64
		keepStart int64
	}
	var periods []closingPeriod
	var advances []progress
	var queries []statistics.APIWindowQuery
	for _, seg := range s.segments {
		limit := now
		if seg.Until > 0 {
			limit = min(limit, seg.Until)
		}
		if seg.Start > limit {
			continue
		}
		_, end := seg.period(seg.Cursor)
		if end > limit {
			continue
		}
		keepStart := limit
		for range 10 {
			keepStart = seg.Schedule.Boundary(keepStart-1, false)
		}
		cursor := seg.Cursor
		var skipped int64
		for cursor < limit {
			start, end := seg.period(cursor)
			if end > limit || end <= cursor {
				break
			}
			if end <= keepStart {
				skipped++
			} else {
				periods = append(periods, closingPeriod{seg, start, end, skipped})
				queries = append(queries, statistics.APIWindowQuery{APIKey: seg.Key, Start: start, End: end})
			}
			cursor = end
		}
		if cursor != seg.Cursor {
			advances = append(advances, progress{seg.ID, cursor, keepStart})
		}
	}
	if len(advances) == 0 {
		return nil
	}
	amounts, err := s.store.UsageAPIWindows(ctx, conn, queries, now)
	if err != nil {
		return err
	}
	subscriptions, err := notificationSubscriptions(ctx, conn)
	if err != nil {
		return err
	}
	active := make(map[string]bool, len(subscriptions))
	for _, sub := range subscriptions {
		active[sub.ID] = s.settings.Enabled && sub.Enabled && !sub.Deleted
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, period := range periods {
		if err := ensureNotificationPeriod(ctx, tx, period.segment, period.start, period.end); err != nil {
			return err
		}
		status := "pending"
		if !active[period.segment.Subscription] {
			status = "cancelled"
		}
		amount := amounts[i]
		details, err := json.Marshal(amount.Details)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notification_periods SET status=?,requests=?,tokens=?,cost=?,unpriced=?,skipped=?,details=? WHERE id=? AND status='open'`, status, amount.Requests, amount.Tokens, amount.Cost, amount.Unpriced, period.skipped, details, notificationPeriodID(period.segment.ID, period.start)); err != nil {
			return err
		}
	}
	for _, advance := range advances {
		// 太旧的开放记录只有元数据，不再补算；最近十期之外的终结记录随后统一裁剪。
		if _, err := tx.ExecContext(ctx, `UPDATE notification_periods SET status='skipped' WHERE segment=? AND status='open' AND end<=?`, advance.segment, advance.keepStart); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notification_segments SET cursor=? WHERE id=?`, advance.cursor, advance.segment); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM notification_periods WHERE status IN ('sent','failed','unknown','skipped','cancelled') AND id NOT IN
 (SELECT p.id FROM notification_periods p WHERE p.subscription=notification_periods.subscription AND p.status!='open' ORDER BY p.end DESC,p.id DESC LIMIT 10)`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.loadNotificationState(ctx, conn)
}

func (s *Service) Tick(ctx context.Context, now int64) error {
	return s.store.WithConn(ctx, func(conn *sql.Conn) error { return s.Settle(ctx, conn, now) })
}

func insertNotificationSegment(ctx context.Context, exec storage.SQLExecer, sub Subscription, cfg Settings, now int64) error {
	if !cfg.Enabled || !sub.Enabled || sub.Deleted {
		return nil
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	// 首次生效覆盖当前完整周期；后续恢复或计划变更仍从下一分钟开始。
	_, err = exec.ExecContext(ctx, `INSERT INTO notification_segments(subscription,api_key,email,note,start,cursor,schedule)
 SELECT ?,?,?,?,at,at,? FROM
 (SELECT CASE WHEN EXISTS(SELECT 1 FROM notification_segments WHERE subscription=?) THEN ? ELSE ? END AS at)`,
		sub.ID, sub.Key, sub.Email, sub.Note, encoded, sub.ID, statistics.FloorUnix(now, 60)+60, cfg.Boundary(now, false))
	return err
}
