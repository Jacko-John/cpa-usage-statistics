// SMTP 配置复用原设置记录：配置和密码分别存入现有 JSON 与密码列，不扩展数据库结构。
package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

type SMTPProfile struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	SMTPHost           string `json:"smtp_host"`
	SMTPPort           int    `json:"smtp_port"`
	SMTPSecurity       string `json:"smtp_security"`
	SMTPUsername       string `json:"smtp_username"`
	Sender             string `json:"sender"`
	Password           string `json:"-"`
	PasswordConfigured bool   `json:"password_configured"`
}

// SMTPProfileUpdate 只允许写入密码，读取配置不会带回密码或清除指令。
type SMTPProfileUpdate struct {
	SMTPProfile
	Password      string `json:"password"`
	ClearPassword bool   `json:"clear_password"`
}

// storedSMTPProfile 仅保存账号参数；密码和密码是否已配置的状态不混入配置 JSON。
type storedSMTPProfile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	SMTPHost     string `json:"smtp_host"`
	SMTPPort     int    `json:"smtp_port"`
	SMTPSecurity string `json:"smtp_security"`
	SMTPUsername string `json:"smtp_username,omitempty"`
	Sender       string `json:"sender"`
}

// storedNotificationSettings 不重复保存启用账号的投影，也不保存接口计算得到的状态。
type storedNotificationSettings struct {
	storedNotificationSchedule
	ActiveSMTPProfileID string              `json:"active_smtp_profile_id"`
	SMTPProfiles        []storedSMTPProfile `json:"smtp_profiles"`
}

// smtpProfileFromSettings 将旧单账号配置转为一套独立邮箱配置，保留原凭据。
func smtpProfileFromSettings(cfg Settings, id, name string) SMTPProfile {
	return SMTPProfile{
		ID: id, Name: name, SMTPHost: cfg.SMTPHost, SMTPPort: cfg.SMTPPort,
		SMTPSecurity: cfg.SMTPSecurity, SMTPUsername: cfg.SMTPUsername,
		Sender: cfg.Sender, Password: cfg.Password,
	}
}

// legacySMTPProfile 为旧账号提供稳定的配置 ID，无需在读取时写回数据库。
func legacySMTPProfile(cfg Settings) SMTPProfile {
	id := cfg.ActiveSMTPProfileID
	if id == "" {
		id = "default"
	}
	name := cfg.Sender
	if name == "" || utf8.RuneCountInString(name) > 100 {
		name = "SMTP"
	}
	return smtpProfileFromSettings(cfg, id, name)
}

// applySMTPProfile 让原来的发送链路和旧面板字段始终对应当前启用的邮箱。
func applySMTPProfile(cfg *Settings, profile SMTPProfile) {
	cfg.ActiveSMTPProfileID = profile.ID
	cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPSecurity = profile.SMTPHost, profile.SMTPPort, profile.SMTPSecurity
	cfg.SMTPUsername, cfg.Sender, cfg.Password = profile.SMTPUsername, profile.Sender, profile.Password
	cfg.PasswordConfigured = profile.Password != ""
}

// validate 校验可稳定标识的配置名称和连接参数；未启用账号允许暂不保存发件凭据。
func (p SMTPProfile) validate() error {
	if p.ID == "" || len(p.ID) > 64 {
		return errors.New("邮箱配置 ID 无效")
	}
	for _, char := range p.ID {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '-' && char != '_' {
			return errors.New("邮箱配置 ID 无效")
		}
	}
	if p.Name == "" || utf8.RuneCountInString(p.Name) > 100 || strings.IndexFunc(p.Name, unicode.IsControl) >= 0 {
		return errors.New("邮箱配置名称无效，请填写 1 到 100 个字符")
	}
	cfg := DefaultSettings()
	applySMTPProfile(&cfg, p)
	return cfg.validate()
}

// prepareSMTPSettings 根据各配置自身 ID 保留密码，并验证启用项属于保存的列表。
func prepareSMTPSettings(input SettingsUpdate, old Settings) (Settings, error) {
	cfg := input.Settings
	if input.Profiles == nil || input.Password != nil || input.ClearPassword != nil {
		// 旧面板仅更新启用项，不删除或覆盖其他配置。
		// 顶层密码字段识别旧面板直接回传 GET 列表的情况，避免忽略其编辑。
		if cfg.SMTPHost == "" && cfg.SMTPPort == 0 && cfg.SMTPSecurity == "" {
			cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPSecurity = old.SMTPHost, old.SMTPPort, old.SMTPSecurity
			if cfg.SMTPUsername == "" {
				cfg.SMTPUsername = old.SMTPUsername
			}
		}
		cfg.SMTPHost, cfg.SMTPUsername, cfg.Sender = strings.TrimSpace(cfg.SMTPHost), strings.TrimSpace(cfg.SMTPUsername), strings.TrimSpace(cfg.Sender)
		cfg.Password = old.Password
		if input.ClearPassword != nil && *input.ClearPassword {
			cfg.Password = ""
		}
		if input.Password != nil && *input.Password != "" {
			cfg.Password = *input.Password
		}
		cfg.SMTPProfiles = append([]SMTPProfile(nil), old.SMTPProfiles...)
		if len(cfg.SMTPProfiles) == 0 {
			cfg.SMTPProfiles = []SMTPProfile{legacySMTPProfile(cfg)}
			cfg.ActiveSMTPProfileID = cfg.SMTPProfiles[0].ID
		} else {
			cfg.ActiveSMTPProfileID = old.ActiveSMTPProfileID
			for i, profile := range cfg.SMTPProfiles {
				if profile.ID == cfg.ActiveSMTPProfileID {
					cfg.SMTPProfiles[i] = smtpProfileFromSettings(cfg, profile.ID, profile.Name)
				}
			}
		}
		return cfg, nil
	}
	if len(input.Profiles) == 0 {
		return Settings{}, errors.New("请至少保留一套发件邮箱配置")
	}
	previous := make(map[string]SMTPProfile, len(old.SMTPProfiles))
	for _, profile := range old.SMTPProfiles {
		previous[profile.ID] = profile
	}
	seen := make(map[string]bool, len(input.Profiles))
	cfg.SMTPProfiles = make([]SMTPProfile, 0, len(input.Profiles))
	activeFound := false
	for _, update := range input.Profiles {
		profile := update.SMTPProfile
		profile.Name = strings.TrimSpace(profile.Name)
		profile.SMTPHost, profile.SMTPUsername, profile.Sender = strings.TrimSpace(profile.SMTPHost), strings.TrimSpace(profile.SMTPUsername), strings.TrimSpace(profile.Sender)
		if err := profile.validate(); err != nil {
			return Settings{}, err
		}
		if seen[profile.ID] {
			return Settings{}, errors.New("邮箱配置 ID 重复")
		}
		seen[profile.ID] = true
		// 新账号不能借用其他账号的密码；只有同一 ID 的空白密码会沿用原值。
		profile.Password = previous[profile.ID].Password
		if update.ClearPassword {
			profile.Password = ""
		}
		if update.Password != "" {
			profile.Password = update.Password
		}
		profile.PasswordConfigured = profile.Password != ""
		cfg.SMTPProfiles = append(cfg.SMTPProfiles, profile)
		if profile.ID == cfg.ActiveSMTPProfileID {
			applySMTPProfile(&cfg, profile)
			activeFound = true
		}
	}
	if !activeFound {
		return Settings{}, errors.New("请选择列表中的一套邮箱配置用于发送通知")
	}
	return cfg, nil
}

// saveSMTPSettings 只更新原有一条记录，配置和密码映射随计划在同一事务中保存。
func saveSMTPSettings(ctx context.Context, tx *sql.Tx, cfg Settings) error {
	stored := storedNotificationSettings{
		storedNotificationSchedule: cfg.storedSchedule(),
		ActiveSMTPProfileID:        cfg.ActiveSMTPProfileID,
		SMTPProfiles:               make([]storedSMTPProfile, len(cfg.SMTPProfiles)),
	}
	passwords := make(map[string]string, len(cfg.SMTPProfiles))
	for i, profile := range cfg.SMTPProfiles {
		stored.SMTPProfiles[i] = storedSMTPProfile{
			ID: profile.ID, Name: profile.Name, SMTPHost: profile.SMTPHost, SMTPPort: profile.SMTPPort,
			SMTPSecurity: profile.SMTPSecurity, SMTPUsername: profile.SMTPUsername, Sender: profile.Sender,
		}
		// 清除和删除账号后，密码映射不保留空值或遗留条目。
		if profile.Password != "" {
			passwords[profile.ID] = profile.Password
		}
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	credentials, err := json.Marshal(passwords)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notification_settings(id,config,password) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET config=excluded.config,password=excluded.password`, encoded, string(credentials))
	return err
}

// restoreSMTPSettings 由配置列表区分新旧密码格式，只恢复内存而不写入数据库。
func restoreSMTPSettings(cfg *Settings, password string) error {
	if len(cfg.SMTPProfiles) == 0 {
		// 旧密码即使恰好是合法 JSON，也必须作为原始密码完整保留。
		cfg.Password = password
		if cfg.Configured {
			profile := legacySMTPProfile(*cfg)
			cfg.SMTPProfiles = []SMTPProfile{profile}
			applySMTPProfile(cfg, profile)
		}
		return nil
	}
	var passwords map[string]string
	if password != "" && json.Unmarshal([]byte(password), &passwords) != nil {
		return errors.New("通知邮箱密码数据无效")
	}
	activeFound := false
	for i := range cfg.SMTPProfiles {
		profile := &cfg.SMTPProfiles[i]
		profile.Password = passwords[profile.ID]
		if profile.ID == cfg.ActiveSMTPProfileID {
			applySMTPProfile(cfg, *profile)
			activeFound = true
		}
	}
	if !activeFound {
		return errors.New("启用的邮箱配置不存在")
	}
	return nil
}
