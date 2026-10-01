package notifications

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/Jacko-John/cpa-usage-statistics/internal/storage"
)

func notificationID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type Service struct {
	store    *storage.Store
	settings Settings
	segments map[int64]notificationSegment
	workerMu sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	send     Sender
}

// Sender 返回发送是否存在未知结果及具体错误。
type Sender func(context.Context, Settings, Mail) (bool, error)

func (s *Service) Start() {
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		for {
			if ctx.Err() != nil {
				return
			}
			if err := s.Tick(ctx, time.Now().Unix()); err != nil {
				if ctx.Err() == nil {
					log.Print("usage-statistics: 通知周期封存失败，下个分钟重试")
				}
			} else if err := s.SendBatch(ctx); err != nil && ctx.Err() == nil {
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

func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	if s.cancel != nil {
		s.cancel()
		<-s.done
		s.cancel = nil
	}
}

func New(ctx context.Context, store *storage.Store, send Sender) (*Service, error) {
	if send == nil {
		send = sendNotificationSMTP
	}
	s := &Service{store: store, settings: DefaultSettings(), send: send}
	err := s.store.WithConn(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, notificationSchema); err != nil {
			return fmt.Errorf("usage sqlite init schema: %w", err)
		}
		if err := restoreNotificationFirstPeriods(ctx, conn, time.Now().Unix()); err != nil {
			return err
		}
		if err := s.loadNotificationState(ctx, conn); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `UPDATE notification_periods SET status='unknown',error='发送期间进程停止，结果未知' WHERE status='sending'`)
		return err
	})
	if err != nil {
		return nil, err
	}
	store.AttachNotifications(s)
	return s, nil
}

func (s *Service) loadNotificationState(ctx context.Context, conn *sql.Conn) error {
	settings := DefaultSettings()
	var data []byte
	var password string
	err := conn.QueryRowContext(ctx, `SELECT config,password FROM notification_settings WHERE id=1`).Scan(&data, &password)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return err
		}
	}
	settings.Configured = len(data) > 0
	settings.Password = password
	segments := make(map[int64]notificationSegment)
	rows, err := conn.QueryContext(ctx, `SELECT id,subscription,api_key,email,note,start,until_at,cursor,schedule FROM notification_segments WHERE until_at=0 OR cursor<until_at ORDER BY id DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var seg notificationSegment
		var encoded []byte
		if err := rows.Scan(&seg.ID, &seg.Subscription, &seg.Key, &seg.Email, &seg.Note, &seg.Start, &seg.Until, &seg.Cursor, &encoded); err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &seg.Schedule); err != nil {
			return err
		}
		segments[seg.ID] = seg
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.settings = settings
	s.segments = segments
	return nil
}
