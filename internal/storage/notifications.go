package storage

import (
	"context"
	"database/sql"
)

// PeriodSettler 在唯一连接内按当前价格封存通知；Stop 必须等待后台任务退出。
type PeriodSettler interface {
	Settle(context.Context, *sql.Conn, int64) error
	Stop()
}

// AttachNotifications 在启动后台任务前绑定通知服务。存储层负责关停顺序。
func (s *Store) AttachNotifications(service PeriodSettler) {
	s.notifications = service
}

func (s *Store) settleNotifications(ctx context.Context, conn *sql.Conn, now int64) error {
	if s.notifications == nil {
		return nil
	}
	return s.notifications.Settle(ctx, conn, now)
}
