package notifications

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type SendResult struct {
	Unknown bool
	Err     error
}

// TestEmail 先读取配置，释放数据库连接后再发送完整周期样例。
func (s *Service) TestEmail(ctx context.Context, email string) (SendResult, error) {
	if !validEmail(email) {
		return SendResult{}, errors.New("测试收件邮箱无效")
	}
	var cfg Settings
	if err := s.store.WithConn(ctx, func(_ *sql.Conn) error { cfg = s.settings; return nil }); err != nil {
		return SendResult{}, err
	}
	if cfg.Sender == "" || cfg.Password == "" {
		return SendResult{}, errors.New("请先保存 SMTP 配置")
	}
	end := cfg.Boundary(time.Now().Unix(), false)
	sample := Period{
		Start: cfg.Boundary(end-1, false), End: end, Timezone: cfg.Timezone,
		KeyLabel: notificationMask("sk-9f2b7k4m6q8r3c1a7x9q"), Note: "个人项目",
		Requests: 1280, Tokens: 2_468_000, Cost: 12.3456,
	}
	unknown, err := s.send(ctx, cfg, Mail{ID: notificationID(), To: email, Periods: []Period{sample}})
	return SendResult{Unknown: unknown, Err: err}, nil
}
