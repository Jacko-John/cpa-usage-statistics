package notifications

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Jacko-John/cpa-usage-statistics/internal/testutil"
)

func testNotificationTLS(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "smtp.test"}, DNSNames: []string{"smtp.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(parsed)
	return certificate, pool
}

type smtpTestDelivery struct {
	content, username, sender string
	unencryptedAuth           bool
}

// testNotificationSMTPServer 在本地模拟两种连接方式，记录认证账号与邮件发件地址。
func testNotificationSMTPServer(t *testing.T, cert tls.Certificate, security, scenario string) (string, <-chan smtpTestDelivery) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan smtpTestDelivery, 1)
	go func() {
		var delivery smtpTestDelivery
		defer func() { done <- delivery }()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		serverTLS := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		secured := security == "tls"
		if secured {
			conn = tls.Server(conn, serverTLS)
		}
		reader := bufio.NewReader(conn)
		write := func(value string) { _, _ = fmt.Fprint(conn, value+"\r\n") }
		decode := func(value string) string {
			data, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
			return string(data)
		}
		write("220 smtp.test ESMTP")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			command := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(command, "EHLO"):
				if !secured && scenario != "starttls-missing" {
					write("250-smtp.test\r\n250-STARTTLS\r\n250 AUTH PLAIN")
				} else if scenario == "login-only" {
					write("250-smtp.test\r\n250 AUTH LOGIN")
				} else if scenario == "unsupported-auth" {
					write("250-smtp.test\r\n250 AUTH XOAUTH2")
				} else {
					write("250-smtp.test\r\n250 AUTH PLAIN LOGIN")
				}
			case command == "STARTTLS":
				if scenario == "starttls-rejected" {
					write("454 TLS unavailable")
					continue
				}
				write("220 upgrade connection")
				conn = tls.Server(conn, serverTLS)
				reader = bufio.NewReader(conn)
				secured = true
			case strings.HasPrefix(command, "AUTH"):
				delivery.unencryptedAuth = !secured
				var password string
				if command == "AUTH LOGIN" {
					write("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
					line, _ = reader.ReadString('\n')
					delivery.username = decode(line)
					write("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
					line, _ = reader.ReadString('\n')
					password = decode(line)
				} else {
					parts := strings.Split(decode(strings.TrimPrefix(command, "AUTH PLAIN ")), "\x00")
					if len(parts) == 3 {
						delivery.username, password = parts[1], parts[2]
					}
				}
				if scenario == "auth-rejected" || password != "fake-code" {
					write("535 rejected")
				} else {
					write("235 authenticated")
				}
			case strings.HasPrefix(command, "MAIL"):
				delivery.sender = command
				write("250 sender accepted")
			case strings.HasPrefix(command, "RCPT"):
				if scenario == "rcpt-rejected" {
					write("550 rejected")
				} else {
					write("250 recipient accepted")
				}
			case command == "DATA":
				write("354 send content")
				var content strings.Builder
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if line == ".\r\n" {
						break
					}
					content.WriteString(line)
				}
				delivery.content = content.String()
				if scenario == "unknown" {
					return
				}
				if scenario == "data-rejected" {
					write("550 rejected")
				} else {
					write("250 accepted")
				}
			case command == "QUIT":
				write("221 bye")
				return
			default:
				write("500 unexpected")
			}
		}
	}()
	return listener.Addr().String(), done
}

// TestNotificationSMTPRealTLS 验证加密、认证和最终接收状态，所有邮件仅发送至本地模拟服务器。
func TestNotificationSMTPRealTLS(t *testing.T) {
	cert, roots := testNotificationTLS(t)
	for _, security := range []string{"tls", "starttls"} {
		for _, scenario := range []string{"success", "sender-login", "login-only", "auth-rejected", "rcpt-rejected", "data-rejected", "unknown", "certificate-rejected", "unsupported-auth", "starttls-missing", "starttls-rejected"} {
			if security == "tls" && strings.HasPrefix(scenario, "starttls-") {
				continue
			}
			t.Run(security+"/"+scenario, func(t *testing.T) {
				address, done := testNotificationSMTPServer(t, cert, security, scenario)
				cfg := Settings{SMTPSecurity: security, SMTPUsername: "smtp-login", Sender: "sender@example.com", Password: "fake-code"}
				if scenario == "sender-login" {
					cfg.SMTPUsername = ""
				}
				tlsConfig := &tls.Config{ServerName: "smtp.test", RootCAs: roots, MinVersion: tls.VersionTLS12}
				if scenario == "certificate-rejected" {
					tlsConfig.RootCAs = x509.NewCertPool()
				}
				unknown, sendErr := sendNotificationSMTPAt(context.Background(), cfg, notificationSampleMailForTest(), address, tlsConfig)
				delivery := <-done
				if delivery.unencryptedAuth {
					t.Fatal("credentials sent before TLS")
				}
				if scenario == "success" || scenario == "sender-login" || scenario == "login-only" {
					if sendErr != nil {
						t.Fatal(sendErr)
					}
					wantUsername := cfg.SMTPUsername
					if wantUsername == "" {
						wantUsername = cfg.Sender
					}
					if delivery.username != wantUsername || delivery.sender != "MAIL FROM:<"+cfg.Sender+">" || !strings.Contains(delivery.content, "From: "+cfg.Sender) {
						t.Fatalf("login or sender mismatch: username=%q sender=%q", delivery.username, delivery.sender)
					}
					_, parts := testutil.MessageParts(t, []byte(delivery.content))
					for _, body := range parts {
						if !strings.Contains(body, "你的 API 用量报告") || strings.Contains(body, "示例") || !strings.Contains(body, "$12.3456") || strings.Contains(body, "fake-code") || strings.Contains(body, "sk-9f2b7k4m6q8r3c1a7x9q") {
							t.Fatal("SMTP 收到的邮件内容或脱敏不正确")
						}
					}
				} else if sendErr == nil {
					t.Fatal("expected error")
				}
				if sendErr != nil && strings.Contains(sendErr.Error(), cfg.Password) {
					t.Fatal("credential leaked in error")
				}
				if unknown != (scenario == "unknown") {
					t.Fatalf("unknown=%v", unknown)
				}
			})
		}
	}
}

func TestNotificationSMTPCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = bufio.NewReader(conn).ReadString('\n')
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = sendNotificationSMTPAt(ctx, Settings{SMTPSecurity: "tls"}, Mail{}, listener.Addr().String(), &tls.Config{ServerName: "smtp.test", MinVersion: tls.VersionTLS12})
	if err == nil {
		t.Fatal("expected cancellation")
	}
	<-done
}
