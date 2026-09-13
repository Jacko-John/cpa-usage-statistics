package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"time"
)

type notificationMail struct {
	ID      string
	To      string
	Periods []notificationPeriod
}

func notificationMessage(mail notificationMail, sender string) []byte {
	data := notificationEmailData(mail)
	var htmlBody bytes.Buffer
	_ = notificationEmailHTML.Execute(&htmlBody, data)
	var body bytes.Buffer
	parts := multipart.NewWriter(&body)
	for _, part := range []struct {
		contentType string
		body        []byte
	}{
		{"text/plain", []byte(notificationEmailText(data))},
		{"text/html", htmlBody.Bytes()},
	} {
		writer, _ := parts.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.contentType + "; charset=UTF-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		encoded := quotedprintable.NewWriter(writer)
		_, _ = encoded.Write(part.body)
		_ = encoded.Close()
	}
	_ = parts.Close()
	return fmt.Appendf(nil, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%s@usage-statistics.local>\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=%q\r\n\r\n%s", sender, mail.To, mime.QEncoding.Encode("UTF-8", data.Subject), mail.ID, time.Now().Format(time.RFC1123Z), parts.Boundary(), body.Bytes())
}

func sendNotificationSMTP(ctx context.Context, cfg notificationSettings, mail notificationMail) (bool, error) {
	return sendNotificationSMTPAt(ctx, cfg, mail, "smtp.qq.com:465", &tls.Config{ServerName: "smtp.qq.com", MinVersion: tls.VersionTLS12})
}

// bool 表示 SMTP 最终接收结果未知；已明确拒收可以重试，未知结果不自动重发。
func sendNotificationSMTPAt(ctx context.Context, cfg notificationSettings, mail notificationMail, address string, tlsConfig *tls.Config) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: tlsConfig}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return false, errors.New("SMTP TLS 连接失败")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	client, err := smtp.NewClient(conn, tlsConfig.ServerName)
	if err != nil {
		return false, errors.New("SMTP 握手失败")
	}
	defer client.Close()
	if err := client.Auth(smtp.PlainAuth("", cfg.Sender, cfg.Password, tlsConfig.ServerName)); err != nil {
		return false, errors.New("SMTP 认证失败，请检查授权码")
	}
	if err := client.Mail(cfg.Sender); err != nil {
		return false, errors.New("SMTP 拒绝发件地址")
	}
	if err := client.Rcpt(mail.To); err != nil {
		return false, errors.New("SMTP 拒绝收件地址")
	}
	writer, err := client.Data()
	if err != nil {
		return false, errors.New("SMTP 拒绝邮件内容")
	}
	if _, err := writer.Write(notificationMessage(mail, cfg.Sender)); err != nil {
		return false, errors.New("SMTP 邮件传输中断")
	}
	if err := writer.Close(); err != nil {
		var response *textproto.Error
		if errors.As(err, &response) {
			return false, errors.New("SMTP 未接受邮件")
		}
		return true, errors.New("SMTP 最终接收结果未知")
	}
	_ = client.Quit()
	return false, nil
}

func (s *SQLiteStore) sendNotificationBatch(ctx context.Context) error {
	for ctx.Err() == nil {
		var cfg notificationSettings
		var mail notificationMail
		err := s.withConn(ctx, func(conn *sql.Conn) error {
			cfg = s.notifications.settings
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
			mail = notificationMail{ID: periods[0].ID, To: email, Periods: periods}
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
		unknown, sendErr := s.notifications.send(ctx, cfg, mail)
		// SMTP 已发送后即使停止，也尝试落库；失败则保留 sending，下次启动恢复为 unknown。
		saveCtx, cancel := context.WithTimeout(context.Background(), insertTimeout)
		err = s.withConn(saveCtx, func(conn *sql.Conn) error {
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

func (s *SQLiteStore) retryNotification(ctx context.Context, id string) error {
	return s.withConn(ctx, func(conn *sql.Conn) error {
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
