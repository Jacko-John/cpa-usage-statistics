// 验证 SMTP 配置校验、旧 QQ 记录升级和持久化凭据的读取边界。
package notifications

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNotificationSMTPSettingsValidation 拒绝错误的连接字段，同时接受不同服务商的独立账号。
func TestNotificationSMTPSettingsValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		update func(*Settings)
		valid  bool
	}{
		{"default-qq", func(*Settings) {}, true},
		{"custom-provider", func(s *Settings) {
			s.SMTPHost, s.SMTPPort, s.SMTPSecurity, s.SMTPUsername = "smtp.example.com", 587, "starttls", "smtp-account"
		}, true},
		{"ipv6", func(s *Settings) { s.SMTPHost = "::1" }, true},
		{"empty-host", func(s *Settings) { s.SMTPHost = "" }, false},
		{"url", func(s *Settings) { s.SMTPHost = "smtp://smtp.example.com" }, false},
		{"host-with-port", func(s *Settings) { s.SMTPHost = "smtp.example.com:587" }, false},
		{"host-newline", func(s *Settings) { s.SMTPHost = "smtp.example.com\r\n" }, false},
		{"invalid-label", func(s *Settings) { s.SMTPHost = "smtp..example.com" }, false},
		{"zero-port", func(s *Settings) { s.SMTPPort = 0 }, false},
		{"negative-port", func(s *Settings) { s.SMTPPort = -1 }, false},
		{"large-port", func(s *Settings) { s.SMTPPort = 65536 }, false},
		{"plaintext", func(s *Settings) { s.SMTPSecurity = "none" }, false},
		{"username-newline", func(s *Settings) { s.SMTPUsername = "login\r\naccount" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultSettings()
			test.update(&cfg)
			if err := cfg.validate(); (err == nil) != test.valid {
				t.Fatalf("valid=%v err=%v", test.valid, err)
			}
		})
	}
}

// TestNotificationSMTPSettingsPersist 保留服务商配置和密码，读取接口及重启均不暴露凭据。
func TestNotificationSMTPSettingsPersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := DefaultSettings()
	cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPSecurity = "smtp.example.com", 587, "starttls"
	cfg.SMTPUsername, cfg.Sender = "smtp-account", "reports@example.com"
	if err := s.SettingsPut(ctx, cfg, "first-password", false, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	// 模拟旧面板仅修改计划，不能将已配置的其他服务商重置为 QQ。
	legacy := cfg
	legacy.SMTPHost, legacy.SMTPPort, legacy.SMTPSecurity, legacy.SMTPUsername = "", 0, "", ""
	legacy.Hour = 9
	if err := s.SettingsPut(ctx, legacy, "", false, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	read, err := reopened.SettingsGet(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if read.SMTPHost != cfg.SMTPHost || read.SMTPPort != cfg.SMTPPort || read.SMTPSecurity != cfg.SMTPSecurity || read.SMTPUsername != cfg.SMTPUsername || read.Sender != cfg.Sender || read.Hour != 9 {
		t.Fatalf("SMTP configuration changed across restart: %+v", read)
	}
	encoded, err := json.Marshal(read)
	if err != nil || read.Password != "" || !read.PasswordConfigured || strings.Contains(string(encoded), "first-password") {
		t.Fatalf("secret returned by settings: %s err=%v", encoded, err)
	}

	// 测试邮件读取同一份持久化配置，发送时才获得密码与连接字段。
	reopened.send = func(_ context.Context, actual Settings, _ Mail) (bool, error) {
		if actual.Password != "first-password" || actual.SMTPHost != cfg.SMTPHost || actual.SMTPSecurity != "starttls" || actual.SMTPUsername != cfg.SMTPUsername {
			t.Fatal("test email did not use saved SMTP configuration")
		}
		return false, nil
	}
	result, err := reopened.TestEmail(ctx, "recipient@example.com")
	if err != nil || result.Err != nil {
		t.Fatalf("test email failed: %+v err=%v", result, err)
	}
	if err := reopened.SettingsPut(ctx, read, "replacement-password", false, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if reopened.settings.Password != "replacement-password" {
		t.Fatal("password replacement was not saved")
	}
	if err := reopened.SettingsPut(ctx, read, "", true, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	read, err = reopened.SettingsGet(ctx)
	if err != nil || read.PasswordConfigured {
		t.Fatalf("password was not cleared: %+v err=%v", read, err)
	}
}

// TestNotificationLegacyQQSettings 验证没有 SMTP 连接字段的旧记录仍使用原来的 QQ TLS 配置。
func TestNotificationLegacyQQSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	legacy := `{"enabled":true,"frequency":"weekly","weekday":5,"hour":20,"minute":0,"timezone":"UTC","sender":"legacy@qq.com"}`
	if _, err := s.DB.Exec(`INSERT INTO notification_settings(id,config,password) VALUES(1,?,?)`, legacy, "legacy-password"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cfg := reopened.settings
	if cfg.SMTPHost != "smtp.qq.com" || cfg.SMTPPort != 465 || cfg.SMTPSecurity != "tls" || cfg.SMTPUsername != "" || cfg.Sender != "legacy@qq.com" || cfg.Password != "legacy-password" {
		t.Fatalf("legacy QQ configuration did not survive upgrade: %+v", cfg)
	}
	if len(cfg.SMTPProfiles) != 1 || cfg.SMTPProfiles[0].Password != "legacy-password" || cfg.ActiveSMTPProfileID != cfg.SMTPProfiles[0].ID {
		t.Fatal("legacy QQ sender was not migrated to a profile")
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
}
