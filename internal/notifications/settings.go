package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
)

type Settings struct {
	Configured         bool   `json:"configured"`
	Enabled            bool   `json:"enabled"`
	Frequency          string `json:"frequency"`
	Weekday            int    `json:"weekday"`
	Hour               int    `json:"hour"`
	Minute             int    `json:"minute"`
	Timezone           string `json:"timezone"`
	Sender             string `json:"sender"`
	Password           string `json:"-"`
	PasswordConfigured bool   `json:"password_configured"`
	NextAt             int64  `json:"next_at"`
}

func DefaultSettings() Settings {
	return Settings{Frequency: "weekly", Weekday: 5, Hour: 20, Timezone: "Asia/Shanghai"}
}

func validEmail(value string) bool {
	if strings.ContainsAny(value, "\r\n") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

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
	if s.Enabled && (s.Sender == "" || s.Password == "") {
		return errors.New("请先配置发件邮箱及 SMTP 授权码")
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

func (s *Service) SettingsGet(ctx context.Context) (Settings, error) {
	var result Settings
	err := s.store.WithConn(ctx, func(_ *sql.Conn) error {
		result = s.settings
		result.PasswordConfigured = result.Password != ""
		result.Password = ""
		if result.Enabled {
			result.NextAt = result.Boundary(time.Now().Unix(), true)
		}
		return nil
	})
	return result, err
}

func (s *Service) SettingsPut(ctx context.Context, cfg Settings, password string, clear bool, now int64) error {
	return s.store.WithConn(ctx, func(conn *sql.Conn) error {
		old := s.settings
		cfg.Password = old.Password
		if clear {
			cfg.Password = ""
		}
		if password != "" {
			cfg.Password = password
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
		encoded, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_settings(id,config,password) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET config=excluded.config,password=excluded.password`, encoded, cfg.Password); err != nil {
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
