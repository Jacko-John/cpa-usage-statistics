package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/mail"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

type notificationSettings struct {
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

func defaultNotificationSettings() notificationSettings {
	return notificationSettings{Frequency: "weekly", Weekday: 5, Hour: 20, Timezone: "Asia/Shanghai"}
}

func validNotificationEmail(value string) bool {
	if strings.ContainsAny(value, "\r\n") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

func (s notificationSettings) validate() error {
	if s.Frequency != "daily" && s.Frequency != "weekly" {
		return errors.New("通知周期必须是 daily 或 weekly")
	}
	if s.Weekday < 0 || s.Weekday > 6 || s.Hour < 0 || s.Hour > 23 || s.Minute < 0 || s.Minute > 59 {
		return errors.New("通知时间无效")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return errors.New("通知时区无效")
	}
	if s.Sender != "" && !validNotificationEmail(s.Sender) {
		return errors.New("发件邮箱无效")
	}
	if s.Enabled && (s.Sender == "" || s.Password == "") {
		return errors.New("请先配置发件邮箱及 SMTP 授权码")
	}
	return nil
}

// 按当地日历枚举，DST 跳过的本地时分不执行，重复时分只执行一次。
func (s notificationSettings) boundary(unix int64, next bool) int64 {
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

func notificationID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func notificationMask(key string) string {
	if len(key) <= 12 {
		return "••••"
	}
	return key[:8] + "…" + key[len(key)-4:]
}

type notificationSubscription struct {
	ID      string `json:"id"`
	Key     string `json:"api_key,omitempty"`
	Mask    string `json:"key_label"`
	Email   string `json:"email"`
	Note    string `json:"note"`
	Enabled bool   `json:"enabled"`
	Deleted bool   `json:"deleted"`
}

type notificationSegment struct {
	ID           int64
	Subscription string
	Key          string
	Email        string
	Note         string
	Start        int64
	Until        int64
	Cursor       int64
	Schedule     notificationSettings
}

func (s notificationSegment) period(minute int64) (int64, int64) {
	start := max(s.Start, s.Schedule.boundary(minute, false))
	end := s.Schedule.boundary(minute, true)
	if s.Until > 0 {
		end = min(end, s.Until)
	}
	return start, end
}

type notificationRuntime struct {
	settings notificationSettings
	segments map[int64]notificationSegment
	workerMu sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	send     func(context.Context, notificationSettings, notificationMail) (bool, error)
}

func (s *SQLiteStore) startNotifications() {
	n := s.notifications
	n.workerMu.Lock()
	defer n.workerMu.Unlock()
	if n.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.done = make(chan struct{})
	go func() {
		defer close(n.done)
		for {
			if ctx.Err() != nil {
				return
			}
			if err := s.notificationTick(ctx, time.Now().Unix()); err != nil {
				if ctx.Err() == nil {
					log.Print("usage-statistics: 通知周期封存失败，下个分钟重试")
				}
			} else if err := s.sendNotificationBatch(ctx); err != nil && ctx.Err() == nil {
				log.Print("usage-statistics: 通知发件状态处理失败，下个分钟重试")
			}
			delay := time.Until(time.Now().Truncate(time.Minute).Add(time.Minute))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (s *SQLiteStore) stopNotifications() {
	n := s.notifications
	if n == nil {
		return
	}
	n.workerMu.Lock()
	defer n.workerMu.Unlock()
	if n.cancel != nil {
		n.cancel()
		<-n.done
		n.cancel = nil
	}
}
