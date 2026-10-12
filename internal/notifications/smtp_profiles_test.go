// 验证多邮箱迁移、密码隔离、启用项选择与数据库事务的一致性。
package notifications

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// profileUpdates 从脱敏读取结果构造不修改密码的完整列表。
func profileUpdates(cfg Settings) []SMTPProfileUpdate {
	updates := make([]SMTPProfileUpdate, len(cfg.SMTPProfiles))
	for i, profile := range cfg.SMTPProfiles {
		updates[i].SMTPProfile = profile
	}
	return updates
}

// TestSMTPProfilesMigrationPersist 确认读取旧配置不写库，保存后复用原记录并稳定重启。
func TestSMTPProfilesMigrationPersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	legacy := `{"enabled":true,"frequency":"daily","hour":10,"timezone":"UTC","smtp_host":"smtp.example.com","smtp_port":587,"smtp_security":"starttls","smtp_username":"legacy-login","sender":"legacy@example.com"}`
	// 即使旧密码看起来就是新格式的映射，也不能按 JSON 拆解。
	const legacyPassword = `{"default":"literal-legacy-secret"}`
	if _, err := s.DB.Exec(`INSERT INTO notification_settings(id,config,password) VALUES(1,?,?)`, legacy, legacyPassword); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	cfg, err := first.SettingsGet(ctx)
	if err != nil || len(cfg.SMTPProfiles) != 1 || cfg.ActiveSMTPProfileID != cfg.SMTPProfiles[0].ID || cfg.SMTPHost != "smtp.example.com" || cfg.SMTPUsername != "legacy-login" {
		t.Fatalf("migration failed: %+v err=%v", cfg, err)
	}
	id := cfg.ActiveSMTPProfileID
	if first.settings.SMTPProfiles[0].Password != legacyPassword || cfg.SMTPProfiles[0].Password != "" || !cfg.SMTPProfiles[0].PasswordConfigured {
		t.Fatal("migration changed or exposed credentials")
	}
	var encoded []byte
	var password string
	if err := first.DB.QueryRow(`SELECT config,password FROM notification_settings WHERE id=1`).Scan(&encoded, &password); err != nil || string(encoded) != legacy || password != legacyPassword {
		t.Fatalf("reading legacy settings rewrote the database: err=%v", err)
	}
	if err := first.SettingsSave(ctx, SettingsUpdate{Settings: cfg, Profiles: profileUpdates(cfg)}, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := first.DB.QueryRow(`SELECT config,password FROM notification_settings WHERE id=1`).Scan(&encoded, &password); err != nil {
		t.Fatal(err)
	}
	var passwords map[string]string
	if err := json.Unmarshal([]byte(password), &passwords); err != nil || passwords[id] != legacyPassword || len(passwords) != 1 || strings.Contains(string(encoded), "literal-legacy-secret") {
		t.Fatalf("credentials were not saved separately in the existing record: err=%v", err)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"configured", "next_at", "password_configured", "smtp_host", "smtp_port", "smtp_security", "smtp_username", "sender"} {
		if _, exists := stored[key]; exists {
			t.Fatalf("redundant top-level field stored: %s", key)
		}
	}
	var profiles []map[string]json.RawMessage
	if err := json.Unmarshal(stored["smtp_profiles"], &profiles); err != nil || len(profiles) != 1 {
		t.Fatalf("profile configuration not persisted: err=%v", err)
	}
	for _, key := range []string{"password", "password_configured", "clear_password"} {
		if _, exists := profiles[0][key]; exists {
			t.Fatalf("credential or derived field stored in profile JSON: %s", key)
		}
	}
	var count int
	if err := first.DB.QueryRow(`SELECT count(*) FROM notification_settings`).Scan(&count); err != nil || count != 1 {
		t.Fatal("settings no longer use one record")
	}
	if err := first.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='notification_smtp_profiles'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("SMTP settings created an additional table")
	}
	if err := first.DB.QueryRow(`SELECT count(*) FROM pragma_table_info('notification_settings')`).Scan(&count); err != nil || count != 3 {
		t.Fatal("SMTP settings added database columns")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.settings.ActiveSMTPProfileID != id || len(second.settings.SMTPProfiles) != 1 || second.settings.Password != legacyPassword {
		t.Fatal("restart duplicated the migrated account or lost its password")
	}
}

// TestSMTPProfilesSwitchAndCredentials 在不切断周期的情况下切换待发送通知，并逐账号操作凭据。
func TestSMTPProfilesSwitchAndCredentials(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 19, 58, 0, 0, time.UTC).Unix()
	s, _ := notificationTestStore(t, base)
	cfg, err := s.SettingsGet(ctx)
	if err != nil {
		t.Fatal(err)
	}
	primaryID := cfg.ActiveSMTPProfileID
	backup := SMTPProfile{ID: "backup", Name: "Backup", SMTPHost: "backup.example.com", SMTPPort: 587, SMTPSecurity: "starttls", SMTPUsername: "backup-login", Sender: "backup@example.com"}
	updates := append(profileUpdates(cfg), SMTPProfileUpdate{SMTPProfile: backup, Password: "backup-secret"})
	if err := s.SettingsSave(ctx, SettingsUpdate{Settings: cfg, Profiles: updates}, base+30); err != nil {
		t.Fatal(err)
	}
	// 已封存的待发送周期应使用切换后的账号，周期本身及启用状态保持不变。
	if err := s.Tick(ctx, base+120); err != nil {
		t.Fatal(err)
	}
	var segmentsBefore int
	if err := s.DB.QueryRow(`SELECT count(*) FROM notification_segments`).Scan(&segmentsBefore); err != nil {
		t.Fatal(err)
	}
	cfg, _ = s.SettingsGet(ctx)
	cfg.ActiveSMTPProfileID = backup.ID
	if err := s.SettingsSave(ctx, SettingsUpdate{Settings: cfg, Profiles: profileUpdates(cfg)}, base+125); err != nil {
		t.Fatal(err)
	}
	var segmentsAfter int
	if err := s.DB.QueryRow(`SELECT count(*) FROM notification_segments`).Scan(&segmentsAfter); err != nil || segmentsBefore != segmentsAfter {
		t.Fatal("switching the sender changed notification segments")
	}
	calls := 0
	s.send = func(_ context.Context, actual Settings, _ Mail) (bool, error) {
		calls++
		if actual.ActiveSMTPProfileID != backup.ID || actual.Password != "backup-secret" || actual.SMTPHost != backup.SMTPHost || actual.SMTPUsername != backup.SMTPUsername || actual.Sender != backup.Sender {
			t.Fatal("delivery mixed profile settings or credentials")
		}
		return false, nil
	}
	result, err := s.TestEmail(ctx, "recipient@example.com")
	if err != nil || result.Err != nil {
		t.Fatalf("test email failed: %v %v", err, result.Err)
	}
	if err := s.SendBatch(ctx); err != nil || calls != 2 {
		t.Fatalf("scheduled delivery failed: calls=%d err=%v", calls, err)
	}
	cfg, _ = s.SettingsGet(ctx)
	encoded, _ := json.Marshal(cfg)
	if cfg.SMTPProfiles[0].Password != "" || cfg.SMTPProfiles[1].Password != "" || strings.Contains(string(encoded), "backup-secret") || strings.Contains(string(encoded), "fake-authorization") {
		t.Fatal("settings returned a credential")
	}
	updates = profileUpdates(cfg)
	updates[0].ClearPassword = true
	updates[1].Password = "replacement-secret"
	if err := s.SettingsSave(ctx, SettingsUpdate{Settings: cfg, Profiles: updates}, base+130); err != nil {
		t.Fatal(err)
	}
	if s.settings.SMTPProfiles[0].Password != "" || s.settings.SMTPProfiles[1].Password != "replacement-secret" || s.settings.Password != "replacement-secret" {
		t.Fatal("password edits affected the wrong profile")
	}
	cfg, _ = s.SettingsGet(ctx)
	cfg.ActiveSMTPProfileID = primaryID
	if err := s.SettingsSave(ctx, SettingsUpdate{Settings: cfg, Profiles: profileUpdates(cfg)}, base+135); err == nil {
		t.Fatal("enabled notification accepted a sender whose password was cleared")
	}
	// 删除未启用账号同时删除凭据，重启只恢复保留项及其启用状态。
	cfg.ActiveSMTPProfileID = backup.ID
	updates = profileUpdates(cfg)
	updates[0].Password = "restored-primary-secret"
	if err := s.SettingsSave(ctx, SettingsUpdate{Settings: cfg, Profiles: updates}, base+140); err != nil {
		t.Fatal(err)
	}
	if s.settings.SMTPProfiles[0].Password != "restored-primary-secret" {
		t.Fatal("inactive profile credential was not restored before deletion")
	}
	if err := s.SettingsSave(ctx, SettingsUpdate{Settings: cfg, Profiles: profileUpdates(cfg)[1:]}, base+145); err != nil {
		t.Fatal(err)
	}
	var credentials string
	if err := s.DB.QueryRow(`SELECT password FROM notification_settings WHERE id=1`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	var passwords map[string]string
	if err := json.Unmarshal([]byte(credentials), &passwords); err != nil || len(passwords) != 1 || passwords[backup.ID] != "replacement-secret" {
		t.Fatal("password mapping did not retain only the active sender")
	}
	if _, exists := passwords[primaryID]; exists {
		t.Fatal("deleted or cleared profile retained a credential entry")
	}
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.settings.SMTPProfiles) != 1 || reopened.settings.ActiveSMTPProfileID != backup.ID || reopened.settings.Password != "replacement-secret" {
		t.Fatal("active profile did not survive restart")
	}
}

// TestSMTPProfilesValidationAtomic 拒绝无效列表时不能更改现有配置或借用其他账号密码。
func TestSMTPProfilesValidationAtomic(t *testing.T) {
	s, _ := notificationTestStore(t, time.Now().Unix())
	ctx := context.Background()
	for _, test := range []struct {
		name string
		edit func(*SettingsUpdate)
	}{
		{"empty-list", func(input *SettingsUpdate) { input.Profiles = []SMTPProfileUpdate{} }},
		{"duplicate-id", func(input *SettingsUpdate) { input.Profiles = append(input.Profiles, input.Profiles[0]) }},
		{"missing-active", func(input *SettingsUpdate) { input.ActiveSMTPProfileID = "missing" }},
		{"deleted-active", func(input *SettingsUpdate) { input.Profiles[0].ID = "new-account" }},
		{"blank-name", func(input *SettingsUpdate) { input.Profiles[0].Name = "  " }},
		{"control-name", func(input *SettingsUpdate) { input.Profiles[0].Name = "a\nb" }},
		{"invalid-id", func(input *SettingsUpdate) { input.Profiles[0].ID = "bad/id" }},
		{"invalid-host", func(input *SettingsUpdate) { input.Profiles[0].SMTPHost = "smtp://bad" }},
		{"new-account-without-secret", func(input *SettingsUpdate) {
			input.Profiles[0].ID, input.ActiveSMTPProfileID = "new-account", "new-account"
			input.Profiles[0].PasswordConfigured = true
		}},
		{"clear-active-secret", func(input *SettingsUpdate) { input.Profiles[0].ClearPassword = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := s.SettingsGet(ctx)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(cfg)
			input := SettingsUpdate{Settings: cfg, Profiles: profileUpdates(cfg)}
			test.edit(&input)
			if err := s.SettingsSave(ctx, input, time.Now().Unix()); err == nil {
				t.Fatal("invalid profile settings were accepted")
			}
			after, _ := s.SettingsGet(ctx)
			encoded, _ := json.Marshal(after)
			if string(before) != string(encoded) || s.settings.Password != "fake-authorization" {
				t.Fatal("failed validation changed saved settings")
			}
		})
	}
}

// TestSMTPProfilesSaveRollback 数据库写入中途失败时，列表、启用项和密码一起回滚。
func TestSMTPProfilesSaveRollback(t *testing.T) {
	s, _ := notificationTestStore(t, time.Now().Unix())
	ctx := context.Background()
	cfg, _ := s.SettingsGet(ctx)
	before, _ := json.Marshal(cfg)
	var beforeConfig []byte
	var beforePasswords string
	if err := s.DB.QueryRow(`SELECT config,password FROM notification_settings WHERE id=1`).Scan(&beforeConfig, &beforePasswords); err != nil {
		t.Fatal(err)
	}
	// 在设置写入后的计划分段操作中失败，确保原记录和原计划一同回滚。
	if _, err := s.DB.Exec(`CREATE TRIGGER reject_schedule BEFORE INSERT ON notification_segments BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	backup := cfg.SMTPProfiles[0]
	backup.ID, backup.Name = "backup", "Backup"
	cfg.ActiveSMTPProfileID = backup.ID
	cfg.Hour = (cfg.Hour + 1) % 24
	updates := append(profileUpdates(cfg), SMTPProfileUpdate{SMTPProfile: backup, Password: "new-secret"})
	updates[0].ClearPassword = true
	if err := s.SettingsSave(ctx, SettingsUpdate{Settings: cfg, Profiles: updates}, time.Now().Unix()); err == nil {
		t.Fatal("database failure was ignored")
	}
	after, _ := s.SettingsGet(ctx)
	encoded, _ := json.Marshal(after)
	if string(before) != string(encoded) || s.settings.Password != "fake-authorization" {
		t.Fatal("failed transaction changed the in-memory settings")
	}
	var afterConfig []byte
	var afterPasswords string
	if err := s.DB.QueryRow(`SELECT config,password FROM notification_settings WHERE id=1`).Scan(&afterConfig, &afterPasswords); err != nil || string(afterConfig) != string(beforeConfig) || afterPasswords != beforePasswords {
		t.Fatal("failed transaction changed the persisted record")
	}
	var count int
	if err := s.DB.QueryRow(`SELECT count(*) FROM notification_segments WHERE until_at=0`).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed transaction closed the existing notification schedule")
	}
}
