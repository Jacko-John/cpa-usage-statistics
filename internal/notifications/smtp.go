package notifications

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/smtp"
	"net/textproto"
	"time"
)

func sendNotificationSMTP(ctx context.Context, cfg Settings, mail Mail) (bool, error) {
	return sendNotificationSMTPAt(ctx, cfg, mail, "smtp.qq.com:465", &tls.Config{ServerName: "smtp.qq.com", MinVersion: tls.VersionTLS12})
}

// bool 表示 SMTP 最终接收结果未知；已明确拒收可以重试，未知结果不自动重发。
func sendNotificationSMTPAt(ctx context.Context, cfg Settings, mail Mail, address string, tlsConfig *tls.Config) (bool, error) {
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
	if _, err := writer.Write(Message(mail, cfg.Sender)); err != nil {
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
