package notifications

import (
	"context"
	"database/sql"
	"errors"
	_ "time/tzdata"

	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
	"github.com/Jacko-John/cpa-usage-statistics/internal/storage"
)

func (s *Service) Subscriptions(ctx context.Context) ([]Subscription, error) {
	var subscriptions []Subscription
	err := s.store.WithConn(ctx, func(conn *sql.Conn) error {
		var err error
		subscriptions, err = notificationSubscriptions(ctx, conn)
		return err
	})
	return subscriptions, err
}

func notificationMask(key string) string {
	if len(key) <= 12 {
		return "••••"
	}
	return key[:8] + "…" + key[len(key)-4:]
}

type Subscription struct {
	ID      string `json:"id"`
	Key     string `json:"api_key,omitempty"`
	Mask    string `json:"key_label"`
	Email   string `json:"email"`
	Note    string `json:"note"`
	Enabled bool   `json:"enabled"`
	Deleted bool   `json:"deleted"`
}

func notificationSubscriptions(ctx context.Context, q storage.SQLQueryer) ([]Subscription, error) {
	result := []Subscription{}
	rows, err := q.QueryContext(ctx, `SELECT id,api_key,email,note,enabled,deleted FROM notification_subscriptions ORDER BY deleted,note,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v Subscription
		if err := rows.Scan(&v.ID, &v.Key, &v.Email, &v.Note, &v.Enabled, &v.Deleted); err != nil {
			return nil, err
		}
		v.Mask = notificationMask(v.Key)
		result = append(result, v)
	}
	return result, rows.Err()
}

func (s *Service) SubscriptionPut(ctx context.Context, sub Subscription, remove bool, now int64) (string, error) {
	id := sub.ID
	err := s.store.WithConn(ctx, func(conn *sql.Conn) error {
		if err := s.Settle(ctx, conn, now); err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var existing Subscription
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
		if sub.Key == "" || len(sub.Key) > 4096 || !validEmail(sub.Email) || len(sub.Note) > 100 {
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
			for _, seg := range s.segments {
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
			at := statistics.FloorUnix(now, 60) + 60
			if _, err := tx.ExecContext(ctx, `UPDATE notification_segments SET until_at=? WHERE subscription=? AND until_at=0`, at, id); err != nil {
				return err
			}
			if err := insertNotificationSegment(ctx, tx, sub, s.settings, now); err != nil {
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
