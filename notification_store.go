package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *SQLiteStore) initNotifications(ctx context.Context) error {
	s.notifications = &notificationRuntime{settings: defaultNotificationSettings(), send: sendNotificationSMTP}
	return s.withConn(ctx, func(conn *sql.Conn) error {
		if err := restoreNotificationFirstPeriods(ctx, conn, time.Now().Unix()); err != nil {
			return err
		}
		if err := s.loadNotificationState(ctx, conn); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `UPDATE notification_periods SET status='unknown',error='发送期间进程停止，结果未知' WHERE status='sending'`)
		return err
	})
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
		var cfg notificationSettings
		if err := json.Unmarshal(encoded, &cfg); err != nil {
			rows.Close()
			return err
		}
		if !cfg.Enabled || start == cfg.boundary(start, false) {
			continue
		}
		begin := cfg.boundary(start-1, false)
		if cfg.boundary(start-1, true) > now {
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

func (s *SQLiteStore) loadNotificationState(ctx context.Context, conn *sql.Conn) error {
	n := s.notifications
	settings := defaultNotificationSettings()
	var data []byte
	var password string
	err := conn.QueryRowContext(ctx, `SELECT config,password FROM notification_settings WHERE id=1`).Scan(&data, &password)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return err
		}
	}
	settings.Configured = len(data) > 0
	settings.Password = password
	segments := make(map[int64]notificationSegment)
	rows, err := conn.QueryContext(ctx, `SELECT id,subscription,api_key,email,note,start,until_at,cursor,schedule FROM notification_segments WHERE until_at=0 OR cursor<until_at ORDER BY id DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var seg notificationSegment
		var encoded []byte
		if err := rows.Scan(&seg.ID, &seg.Subscription, &seg.Key, &seg.Email, &seg.Note, &seg.Start, &seg.Until, &seg.Cursor, &encoded); err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &seg.Schedule); err != nil {
			return err
		}
		segments[seg.ID] = seg
	}
	if err := rows.Err(); err != nil {
		return err
	}
	n.settings = settings
	n.segments = segments
	return nil
}

func notificationPeriodID(segment, start int64) string { return fmt.Sprintf("%d-%d", segment, start) }

type notificationPeriod struct {
	ID       string  `json:"id"`
	Start    int64   `json:"start"`
	End      int64   `json:"end"`
	Timezone string  `json:"timezone"`
	Email    string  `json:"email"`
	KeyLabel string  `json:"key_label"`
	Note     string  `json:"note"`
	Partial  bool    `json:"partial"`
	Status   string  `json:"status"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	Unpriced int64   `json:"unpriced_requests"`
	Attempts int     `json:"attempts"`
	RetryAt  int64   `json:"retry_at"`
	Error    string  `json:"error"`
	Skipped  int64   `json:"skipped_cycles"`
}

func ensureNotificationPeriod(ctx context.Context, exec sqlExecer, seg notificationSegment, start, end int64) error {
	partial := start != seg.Schedule.boundary(start, false) || end != seg.Schedule.boundary(start, true)
	_, err := exec.ExecContext(ctx, `INSERT INTO notification_periods(id,segment,subscription,start,end,timezone,email,key_label,note,partial)
 VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(segment,start) DO UPDATE SET end=excluded.end,partial=excluded.partial WHERE status='open'`,
		notificationPeriodID(seg.ID, start), seg.ID, seg.Subscription, start, end, seg.Schedule.Timezone, seg.Email, notificationMask(seg.Key), seg.Note, partial)
	return err
}

// 调用方持有唯一连接；读取共享统计后，在同一事务中保存快照和推进游标。
func (s *SQLiteStore) settleNotifications(ctx context.Context, conn *sql.Conn, now int64) error {
	n := s.notifications
	if n == nil {
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
	var queries []usageAPIWindowQuery
	for _, seg := range n.segments {
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
			keepStart = seg.Schedule.boundary(keepStart-1, false)
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
				queries = append(queries, usageAPIWindowQuery{APIKey: seg.Key, Start: start, End: end})
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
	amounts, err := s.usageAPIWindows(ctx, conn, queries, now)
	if err != nil {
		return err
	}
	subscriptions, err := notificationSubscriptions(ctx, conn)
	if err != nil {
		return err
	}
	active := make(map[string]bool, len(subscriptions))
	for _, sub := range subscriptions {
		active[sub.ID] = n.settings.Enabled && sub.Enabled && !sub.Deleted
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
		if _, err := tx.ExecContext(ctx, `UPDATE notification_periods SET status=?,requests=?,tokens=?,cost=?,unpriced=?,skipped=? WHERE id=? AND status='open'`, status, amount.Requests, amount.Tokens, amount.Cost, amount.Unpriced, period.skipped, notificationPeriodID(period.segment.ID, period.start)); err != nil {
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

func (s *SQLiteStore) notificationTick(ctx context.Context, now int64) error {
	return s.withConn(ctx, func(conn *sql.Conn) error { return s.settleNotifications(ctx, conn, now) })
}

func (s *SQLiteStore) notificationSettingsGet(ctx context.Context) (notificationSettings, error) {
	var result notificationSettings
	err := s.withConn(ctx, func(_ *sql.Conn) error {
		result = s.notifications.settings
		result.PasswordConfigured = result.Password != ""
		result.Password = ""
		if result.Enabled {
			result.NextAt = result.boundary(time.Now().Unix(), true)
		}
		return nil
	})
	return result, err
}

func notificationSubscriptions(ctx context.Context, q sqlQueryer) ([]notificationSubscription, error) {
	result := []notificationSubscription{}
	rows, err := q.QueryContext(ctx, `SELECT id,api_key,email,note,enabled,deleted FROM notification_subscriptions ORDER BY deleted,note,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v notificationSubscription
		if err := rows.Scan(&v.ID, &v.Key, &v.Email, &v.Note, &v.Enabled, &v.Deleted); err != nil {
			return nil, err
		}
		v.Mask = notificationMask(v.Key)
		result = append(result, v)
	}
	return result, rows.Err()
}

func insertNotificationSegment(ctx context.Context, exec sqlExecer, sub notificationSubscription, cfg notificationSettings, now int64) error {
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
		sub.ID, sub.Key, sub.Email, sub.Note, encoded, sub.ID, floorUnix(now, 60)+60, cfg.boundary(now, false))
	return err
}

func (s *SQLiteStore) notificationSettingsPut(ctx context.Context, cfg notificationSettings, password string, clear bool, now int64) error {
	return s.withConn(ctx, func(conn *sql.Conn) error {
		old := s.notifications.settings
		cfg.Password = old.Password
		if clear {
			cfg.Password = ""
		}
		if password != "" {
			cfg.Password = password
		}
		if err := cfg.validate(); err != nil {
			return err
		}
		if err := s.settleNotifications(ctx, conn, now); err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		encoded, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_settings(id,config,password) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET config=excluded.config,password=excluded.password`, encoded, cfg.Password); err != nil {
			return err
		}
		changed := old.Enabled != cfg.Enabled || old.Frequency != cfg.Frequency || old.Weekday != cfg.Weekday || old.Hour != cfg.Hour || old.Minute != cfg.Minute || old.Timezone != cfg.Timezone
		if changed {
			at := floorUnix(now, 60) + 60
			if _, err := tx.ExecContext(ctx, `UPDATE notification_segments SET until_at=? WHERE until_at=0`, at); err != nil {
				return err
			}
			subs, err := notificationSubscriptions(ctx, tx)
			if err != nil {
				return err
			}
			for _, sub := range subs {
				if err := insertNotificationSegment(ctx, tx, sub, cfg, now); err != nil {
					return err
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return s.loadNotificationState(ctx, conn)
	})
}

func (s *SQLiteStore) notificationSubscriptionPut(ctx context.Context, sub notificationSubscription, remove bool, now int64) (string, error) {
	id := sub.ID
	err := s.withConn(ctx, func(conn *sql.Conn) error {
		if err := s.settleNotifications(ctx, conn, now); err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var existing notificationSubscription
		if id == "" {
			id = notificationID()
		} else {
			if err := tx.QueryRowContext(ctx, `SELECT api_key,email,note,enabled,deleted FROM notification_subscriptions WHERE id=?`, id).Scan(&existing.Key, &existing.Email, &existing.Note, &existing.Enabled, &existing.Deleted); err != nil {
				return errors.New("订阅不存在")
			}
			if remove {
				sub = existing
				sub.Enabled = false
				sub.Deleted = true
			}
		}
		if existing.Key != "" && existing.Key != sub.Key {
			return errors.New("请为新 Key 单独创建订阅，不能转移旧 Key 历史")
		}
		if sub.Key == "" || len(sub.Key) > 4096 || !validNotificationEmail(sub.Email) || len(sub.Note) > 100 {
			return errors.New("Key、邮箱或备注无效")
		}
		var duplicate string
		var wasDeleted bool
		err = tx.QueryRowContext(ctx, `SELECT id,deleted FROM notification_subscriptions WHERE api_key=? AND id!=?`, sub.Key, id).Scan(&duplicate, &wasDeleted)
		if err == nil {
			if sub.ID != "" || !wasDeleted {
				return errors.New("该 Key 已绑定，请编辑原订阅")
			}
			// 重新绑定已删除的同一 Key，保留原历史而不转移给其他 Key。
			id = duplicate
		} else if err != sql.ErrNoRows {
			return err
		}
		sub.ID = id
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_subscriptions(id,api_key,email,note,enabled,deleted) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET api_key=excluded.api_key,email=excluded.email,note=excluded.note,enabled=excluded.enabled,deleted=excluded.deleted`, id, sub.Key, sub.Email, sub.Note, sub.Enabled, sub.Deleted); err != nil {
			return err
		}
		if existing.Key != "" && existing.Enabled == sub.Enabled && existing.Deleted == sub.Deleted {
			// 邮箱变更不切断当前周期，先固定旧周期收件地址，再更新后续周期地址。
			for _, seg := range s.notifications.segments {
				if seg.Subscription == id && seg.Start <= now && (seg.Until == 0 || now < seg.Until) {
					start, end := seg.period(now)
					if err := ensureNotificationPeriod(ctx, tx, seg, start, end); err != nil {
						return err
					}
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE notification_segments SET email=?,note=? WHERE subscription=? AND (until_at=0 OR until_at>?)`, sub.Email, sub.Note, id, now); err != nil {
				return err
			}
		} else {
			at := floorUnix(now, 60) + 60
			if _, err := tx.ExecContext(ctx, `UPDATE notification_segments SET until_at=? WHERE subscription=? AND until_at=0`, at, id); err != nil {
				return err
			}
			if err := insertNotificationSegment(ctx, tx, sub, s.notifications.settings, now); err != nil {
				return err
			}
		}
		if remove || !sub.Enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_periods SET status='cancelled' WHERE subscription=? AND status IN ('pending','failed','unknown')`, id); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return s.loadNotificationState(ctx, conn)
	})
	return id, err
}

const notificationPeriodColumns = `id,start,end,timezone,email,key_label,note,partial,status,requests,tokens,cost,unpriced,attempts,retry_at,error,skipped`

func scanNotificationPeriods(ctx context.Context, q sqlQueryer, where string, args ...any) ([]notificationPeriod, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+notificationPeriodColumns+` FROM notification_periods `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []notificationPeriod{}
	for rows.Next() {
		var p notificationPeriod
		if err := rows.Scan(&p.ID, &p.Start, &p.End, &p.Timezone, &p.Email, &p.KeyLabel, &p.Note, &p.Partial, &p.Status, &p.Requests, &p.Tokens, &p.Cost, &p.Unpriced, &p.Attempts, &p.RetryAt, &p.Error, &p.Skipped); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

type notificationHistory struct {
	Current []notificationPeriod `json:"current"`
	Periods []notificationPeriod `json:"periods"`
}

func (s *SQLiteStore) notificationHistoryGet(ctx context.Context, id string, now int64) (notificationHistory, error) {
	result := notificationHistory{Current: []notificationPeriod{}, Periods: []notificationPeriod{}}
	err := s.withConn(ctx, func(conn *sql.Conn) error {
		if err := s.settleNotifications(ctx, conn, now); err != nil {
			return err
		}
		var key string
		for _, seg := range s.notifications.segments {
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
		queries := make([]usageAPIWindowQuery, len(result.Current))
		for i, period := range result.Current {
			queries[i] = usageAPIWindowQuery{APIKey: key, Start: period.Start, End: period.End}
		}
		amounts, err := s.usageAPIWindows(ctx, conn, queries, now)
		if err != nil {
			return err
		}
		for i, amount := range amounts {
			p := &result.Current[i]
			p.Cost = amount.Cost
			p.Requests = amount.Requests
			p.Tokens = amount.Tokens
			p.Unpriced = amount.Unpriced
		}
		result.Periods, err = scanNotificationPeriods(ctx, conn, `WHERE subscription=? AND status!='open' ORDER BY end DESC,id DESC LIMIT 10`, id)
		return err
	})
	return result, err
}
