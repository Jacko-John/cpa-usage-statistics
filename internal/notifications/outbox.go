package notifications

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Service) SendBatch(ctx context.Context) error {
	for ctx.Err() == nil {
		var cfg Settings
		var mail Mail
		err := s.store.WithConn(ctx, func(conn *sql.Conn) error {
			cfg = s.settings
			if !cfg.Enabled {
				return nil
			}
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var subscription, email string
			err = tx.QueryRowContext(ctx, `SELECT p.subscription,p.email FROM notification_periods p JOIN notification_subscriptions s ON s.id=p.subscription
 WHERE p.status='pending' AND p.retry_at<=? AND s.enabled=1 AND s.deleted=0 ORDER BY p.end,p.id LIMIT 1`, time.Now().Unix()).Scan(&subscription, &email)
			if err == sql.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			periods, err := scanNotificationPeriods(ctx, tx, `WHERE subscription=? AND email=? AND status='pending' AND retry_at<=? ORDER BY end LIMIT 10`, subscription, email, time.Now().Unix())
			if err != nil {
				return err
			}
			if len(periods) == 0 {
				return nil
			}
			mail = Mail{ID: periods[0].ID, To: email, Periods: periods}
			for _, p := range periods {
				if _, err := tx.ExecContext(ctx, `UPDATE notification_periods SET status='sending',attempts=attempts+1 WHERE id=? AND status='pending'`, p.ID); err != nil {
					return err
				}
			}
			return tx.Commit()
		})
		if err != nil || len(mail.Periods) == 0 {
			return err
		}
		unknown, sendErr := s.send(ctx, cfg, mail)
		// SMTP 已发送后即使停止，也尝试落库；失败则保留 sending，下次启动恢复为 unknown。
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = s.store.WithConn(saveCtx, func(conn *sql.Conn) error {
			tx, err := conn.BeginTx(saveCtx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			for _, p := range mail.Periods {
				status := "sent"
				retry := int64(0)
				message := ""
				if sendErr != nil {
					message = sendErr.Error()
					switch {
					case unknown:
						status = "unknown"
					case p.Attempts >= 3:
						status = "failed"
					default:
						status = "pending"
						retry = time.Now().Add([]time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}[p.Attempts]).Unix()
					}
				}
				if _, err := tx.ExecContext(saveCtx, `UPDATE notification_periods SET status=?,retry_at=?,error=? WHERE id=? AND status='sending'`, status, retry, message, p.ID); err != nil {
					return err
				}
			}
			return tx.Commit()
		})
		cancel()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *Service) Retry(ctx context.Context, id string) error {
	return s.store.WithConn(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE notification_periods SET status='pending',attempts=0,retry_at=0,error=''
 WHERE id=? AND status IN ('failed','unknown') AND subscription IN (SELECT id FROM notification_subscriptions WHERE enabled=1 AND deleted=0)`, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return errors.New("记录不可重试，或订阅已暂停")
		}
		return nil
	})
}
