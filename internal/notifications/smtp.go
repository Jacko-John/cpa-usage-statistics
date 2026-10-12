// SMTP 发送支持配置服务商、两种 TLS 连接方式和密码认证，并保留最终确认语义。
package notifications

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// sendNotificationSMTP 使用保存的连接配置，所有认证都在验证证书的 TLS 连接内执行。
func sendNotificationSMTP(ctx context.Context, cfg Settings, mail Mail) (bool, error) {
	address := net.JoinHostPort(cfg.SMTPHost, strconv.Itoa(cfg.SMTPPort))
	return sendNotificationSMTPAt(ctx, cfg, mail, address, &tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12})
}

// bool 表示 SMTP 最终接收结果未知；已明确拒收可以重试，未知结果不自动重发。
func sendNotificationSMTPAt(ctx context.Context, cfg Settings, mail Mail, address string, tlsConfig *tls.Config) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// TLS 直连在建立 SMTP 会话前握手；STARTTLS 在会话建立后强制升级。
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	switch cfg.SMTPSecurity {
	case "tls":
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	case "starttls":
		conn, err = dialer.DialContext(ctx, "tcp", address)
	default:
		return false, errors.New("SMTP 加密方式无效")
	}
	if err != nil {
		return false, errors.New("SMTP 连接失败，请检查服务器、端口和加密方式")
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
	if cfg.SMTPSecurity == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return false, errors.New("SMTP 服务器不支持 STARTTLS")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return false, errors.New("SMTP STARTTLS 握手失败")
		}
	}

	// 登录账号与发件地址分开，留空时沿用原先以发件邮箱登录的行为。
	username := cfg.SMTPUsername
	if username == "" {
		username = cfg.Sender
	}
	ok, mechanisms := client.Extension("AUTH")
	if !ok {
		return false, errors.New("SMTP 服务器不支持账号认证")
	}
	var auth smtp.Auth
	for _, mechanism := range strings.Fields(mechanisms) {
		if strings.EqualFold(mechanism, "PLAIN") {
			auth = smtp.PlainAuth("", username, cfg.Password, tlsConfig.ServerName)
			break
		}
		if strings.EqualFold(mechanism, "LOGIN") {
			auth = &smtpLoginAuth{username: username, password: cfg.Password, host: tlsConfig.ServerName}
		}
	}
	if auth == nil {
		return false, errors.New("SMTP 服务器不支持 PLAIN 或 LOGIN 认证")
	}
	if err := client.Auth(auth); err != nil {
		return false, errors.New("SMTP 认证失败，请检查登录账号和密码或授权码")
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

// smtpLoginAuth 适配仅提供 LOGIN 的 SMTP 服务，认证前仍验证加密状态和主机名。
type smtpLoginAuth struct {
	username, password, host string
	step                     int
}

// Start 要求 TLS 与预期主机匹配，随后由服务器依次请求账号和密码。
func (a *smtpLoginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS || server.Name != a.host {
		return "", nil, errors.New("SMTP LOGIN requires TLS for the configured host")
	}
	return "LOGIN", nil, nil
}

// Next 只接受账号、密码两次挑战，不向额外挑战发送凭据。
func (a *smtpLoginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.username), nil
	case 2:
		return []byte(a.password), nil
	default:
		return nil, errors.New("unexpected SMTP LOGIN challenge")
	}
}
