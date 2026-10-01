package notifications

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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

func TestNotificationSMTPRealTLS(t *testing.T) {
	cert, roots := testNotificationTLS(t)
	for _, scenario := range []string{"success", "auth-rejected", "rcpt-rejected", "data-rejected", "unknown", "certificate-rejected"} {
		t.Run(scenario, func(t *testing.T) {
			listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan string, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- ""
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				write := func(value string) { _, _ = fmt.Fprint(conn, value+"\r\n") }
				write("220 smtp.test ESMTP")
				var content strings.Builder
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						done <- content.String()
						return
					}
					command := strings.TrimSpace(line)
					switch {
					case strings.HasPrefix(command, "EHLO"):
						write("250-smtp.test\r\n250 AUTH PLAIN")
					case strings.HasPrefix(command, "AUTH"):
						if scenario == "auth-rejected" {
							write("535 rejected")
						} else {
							write("235 authenticated")
						}
					case strings.HasPrefix(command, "MAIL"):
						write("250 sender accepted")
					case strings.HasPrefix(command, "RCPT"):
						if scenario == "rcpt-rejected" {
							write("550 rejected")
						} else {
							write("250 recipient accepted")
						}
					case command == "DATA":
						write("354 send content")
						for {
							line, err := reader.ReadString('\n')
							if err != nil {
								done <- content.String()
								return
							}
							if line == ".\r\n" {
								break
							}
							content.WriteString(line)
						}
						if scenario == "unknown" {
							done <- content.String()
							return
						}
						if scenario == "data-rejected" {
							write("550 rejected")
						} else {
							write("250 accepted")
						}
					case command == "QUIT":
						write("221 bye")
						done <- content.String()
						return
					default:
						write("500 unexpected")
					}
				}
			}()
			cfg := Settings{Sender: "sender@qq.com", Password: "fake-code"}
			tlsConfig := &tls.Config{ServerName: "smtp.test", RootCAs: roots, MinVersion: tls.VersionTLS12}
			if scenario == "certificate-rejected" {
				tlsConfig.RootCAs = x509.NewCertPool()
			}
			unknown, sendErr := sendNotificationSMTPAt(context.Background(), cfg, notificationSampleMailForTest(), listener.Addr().String(), tlsConfig)
			content := <-done
			if scenario == "success" {
				if sendErr != nil {
					t.Fatal(sendErr)
				}
				_, parts := testutil.MessageParts(t, []byte(content))
				for _, body := range parts {
					if !strings.Contains(body, "你的 API 用量报告") || strings.Contains(body, "示例") || !strings.Contains(body, "$12.3456") || strings.Contains(body, "fake-code") || strings.Contains(body, "sk-9f2b7k4m6q8r3c1a7x9q") {
						t.Fatal("SMTP 收到的邮件内容或脱敏不正确")
					}
				}
			} else if sendErr == nil {
				t.Fatal("expected error")
			}
			if unknown != (scenario == "unknown") {
				t.Fatalf("unknown=%v", unknown)
			}
		})
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
	_, err = sendNotificationSMTPAt(ctx, Settings{}, Mail{}, listener.Addr().String(), &tls.Config{ServerName: "smtp.test", MinVersion: tls.VersionTLS12})
	if err == nil {
		t.Fatal("expected cancellation")
	}
	<-done
}
