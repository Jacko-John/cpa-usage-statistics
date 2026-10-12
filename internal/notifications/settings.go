// 通知设置负责计划、多套 SMTP 发件配置的持久化和读取时的凭据脱敏。
package notifications

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/mail"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
)

type Settings struct {
	Configured          bool          `json:"configured"`
	Enabled             bool          `json:"enabled"`
	Frequency           string        `json:"frequency"`
	Weekday             int           `json:"weekday"`
	Hour                int           `json:"hour"`
	Minute              int           `json:"minute"`
	Timezone            string        `json:"timezone"`
	SMTPHost            string        `json:"smtp_host"`
	SMTPPort            int           `json:"smtp_port"`
	SMTPSecurity        string        `json:"smtp_security"`
	SMTPUsername        string        `json:"smtp_username"`
	Sender              string        `json:"sender"`
	Password            string        `json:"-"`
	PasswordConfigured  bool          `json:"password_configured"`
	NextAt              int64         `json:"next_at"`
	ActiveSMTPProfileID string        `json:"active_smtp_profile_id"`
	SMTPProfiles        []SMTPProfile `json:"smtp_profiles"`
}

// SettingsUpdate 将仅写入的密码操作与读取时的设置分开；省略配置列表兼容旧面板。
type SettingsUpdate struct {
	Settings
	Profiles      []SMTPProfileUpdate `json:"smtp_profiles"`
	Password      *string             `json:"password"`
	ClearPassword *bool               `json:"clear_password"`
}

// storedNotificationSchedule 供设置和周期快照复用，只持久化计划所需字段。
type storedNotificationSchedule struct {
	Enabled   bool   `json:"enabled"`
	Frequency string `json:"frequency"`
	Weekday   int    `json:"weekday"`
	Hour      int    `json:"hour"`
	Minute    int    `json:"minute"`
	Timezone  string `json:"timezone"`
}

// storedSchedule 保留零值时分和启用状态，供旧周期读取逻辑直接恢复。
func (s Settings) storedSchedule() storedNotificationSchedule {
	return storedNotificationSchedule{
		Enabled: s.Enabled, Frequency: s.Frequency, Weekday: s.Weekday,
		Hour: s.Hour, Minute: s.Minute, Timezone: s.Timezone,
	}
}

// DefaultSettings 保留原来的 QQ TLS 连接作为新安装与旧配置的默认值。
func DefaultSettings() Settings {
	return Settings{
		Frequency: "weekly", Weekday: 5, Hour: 20, Timezone: "Asia/Shanghai",
		SMTPHost: "smtp.qq.com", SMTPPort: 465, SMTPSecurity: "tls",
	}
}

// validSMTPHost 只接受独立主机名或 IP，端口与连接方式由各自字段提供。
func validSMTPHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if char != '-' && !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') {
				return false
			}
		}
	}
	return true
}

func validEmail(value string) bool {
	if strings.ContainsAny(value, "\r\n") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

// validate 在保存前校验计划与 SMTP 字段，启用通知时必须配置发件凭据。
func (s Settings) validate() error {
	if s.Frequency != "daily" && s.Frequency != "weekly" {
		return errors.New("通知周期必须是 daily 或 weekly")
	}
	if s.Weekday < 0 || s.Weekday > 6 || s.Hour < 0 || s.Hour > 23 || s.Minute < 0 || s.Minute > 59 {
		return errors.New("通知时间无效")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return errors.New("通知时区无效")
	}
	if s.Sender != "" && !validEmail(s.Sender) {
		return errors.New("发件邮箱无效")
	}
	if !validSMTPHost(s.SMTPHost) {
		return errors.New("SMTP 服务器地址无效，请填写主机名或 IP，不含协议和端口")
	}
	if s.SMTPPort < 1 || s.SMTPPort > 65535 {
		return errors.New("SMTP 端口必须在 1 到 65535 之间")
	}
	if s.SMTPSecurity != "tls" && s.SMTPSecurity != "starttls" {
		return errors.New("SMTP 加密方式必须是 tls 或 starttls")
	}
	if strings.ContainsAny(s.SMTPUsername, "\r\n\x00") {
		return errors.New("SMTP 登录账号无效")
	}
	if s.Enabled && (s.Sender == "" || s.Password == "") {
		return errors.New("请先配置发件邮箱及 SMTP 密码或授权码")
	}
	return nil
}

// 按当地日历枚举，DST 跳过的本地时分不执行，重复时分只执行一次。
func (s Settings) Boundary(unix int64, next bool) int64 {
	loc, _ := time.LoadLocation(s.Timezone)
	t := time.Unix(unix, 0).In(loc)
	direction := 1
	if !next {
		direction = -1
	}
	for day := 0; ; day += direction {
		date := time.Date(t.Year(), t.Month(), t.Day()+day, 12, 0, 0, 0, loc)
		if s.Frequency == "weekly" && int(date.Weekday()) != s.Weekday {
			continue
		}
		candidate := time.Date(date.Year(), date.Month(), date.Day(), s.Hour, s.Minute, 0, 0, loc)
		if candidate.Hour() != s.Hour || candidate.Minute() != s.Minute {
			continue
		}
		value := candidate.Unix()
		if next && value > unix || !next && value <= unix {
			return value
		}
	}
}

// SettingsGet 返回计划、启用项和全部邮箱配置，始终隐藏每套配置的密码。
func (s *Service) SettingsGet(ctx context.Context) (Settings, error) {
	var result Settings
	err := s.store.WithConn(ctx, func(_ *sql.Conn) error {
		result = s.settings
		result.PasswordConfigured = result.Password != ""
		result.Password = ""
		// 复制列表后再脱敏，不能清除服务内部保存的其他邮箱密码。
		result.SMTPProfiles = make([]SMTPProfile, len(s.settings.SMTPProfiles))
		for i, profile := range s.settings.SMTPProfiles {
			profile.PasswordConfigured = profile.Password != ""
			profile.Password = ""
			result.SMTPProfiles[i] = profile
		}
		if result.Enabled {
			result.NextAt = result.Boundary(time.Now().Unix(), true)
		}
		return nil
	})
	return result, err
}

// SettingsPut 先按旧计划封存，再保存设置；密码留空保留原值，清除需显式指定。
func (s *Service) SettingsPut(ctx context.Context, cfg Settings, password string, clear bool, now int64) error {
	return s.SettingsSave(ctx, SettingsUpdate{Settings: cfg, Password: &password, ClearPassword: &clear}, now)
}

// SettingsSave 原子保存计划、邮箱列表及启用项；切换邮箱不切断正在统计的周期。
func (s *Service) SettingsSave(ctx context.Context, input SettingsUpdate, now int64) error {
	return s.store.WithConn(ctx, func(conn *sql.Conn) error {
		old := s.settings
		cfg, err := prepareSMTPSettings(input, old)
		if err != nil {
			return err
		}
		if err := cfg.validate(); err != nil {
			return err
		}
		if err := s.Settle(ctx, conn, now); err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := saveSMTPSettings(ctx, tx, cfg); err != nil {
			return err
		}
		changed := old.Enabled != cfg.Enabled || old.Frequency != cfg.Frequency || old.Weekday != cfg.Weekday || old.Hour != cfg.Hour || old.Minute != cfg.Minute || old.Timezone != cfg.Timezone
		if changed {
			at := statistics.FloorUnix(now, 60) + 60
			if _, err := tx.ExecContext(ctx, `UPDATE notification_segments SET until_at=? WHERE until_at=0`, at); err != nil {
				return err
			}
			subs, err := notificationSubscriptions(ctx, tx)
			if err != nil {
				return err
			}
			for _, sub := range subs {
				if err := insertNotificationSegment(ctx, tx, sub, cfg, now); err != nil {
					return err
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return s.loadNotificationState(ctx, conn)
	})
}
