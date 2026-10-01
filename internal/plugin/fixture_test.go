package plugin

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jacko-John/cpa-usage-statistics/internal/config"
	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/notifications"
	safetycontrol "github.com/Jacko-John/cpa-usage-statistics/internal/safety"
	"github.com/Jacko-John/cpa-usage-statistics/internal/storage"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testfixture"
)

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func configuredLimiter(t *testing.T, store *storage.Store, cfg config.Safety) *safetycontrol.Limiter {
	t.Helper()
	limiter := safetycontrol.New()
	if err := limiter.Configure(t.Context(), store, cfg, true); err != nil {
		t.Fatal(err)
	}
	return limiter
}

func seedLimiter(t *testing.T, cfg config.Safety, states map[string]usagemodel.SafetyState) *safetycontrol.Limiter {
	t.Helper()
	store := newTestStore(t)
	for key, state := range states {
		state.APIKey = key
		record := usagemodel.Record{ID: key, APIKey: key, Timestamp: time.Now()}
		history := usagemodel.SafetyHistory{APIKey: key, UsageRecordID: key, OccurredAt: record.Timestamp.Unix()}
		if err := store.InsertSafety(t.Context(), record, state, history); err != nil {
			t.Fatal(err)
		}
	}
	return configuredLimiter(t, store, cfg)
}

type notificationStore struct {
	*testfixture.Store
	*notifications.Service
	send notifications.Sender
}

func openNotificationTestStore(path string) (*notificationStore, error) {
	fixture, err := testfixture.Open(path)
	if err != nil {
		return nil, err
	}
	store := &notificationStore{Store: fixture}
	store.Service, err = notifications.New(context.Background(), fixture.Store, func(ctx context.Context, cfg notifications.Settings, mail notifications.Mail) (bool, error) {
		if store.send == nil {
			return false, nil
		}
		return store.send(ctx, cfg, mail)
	})
	if err != nil {
		_ = fixture.Close()
		return nil, err
	}
	return store, nil
}

func notificationTestStore(t *testing.T, now int64) (*notificationStore, string) {
	t.Helper()
	store, err := openNotificationTestStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := notifications.DefaultSettings()
	cfg.Enabled, cfg.Frequency, cfg.Timezone, cfg.Hour, cfg.Sender = true, "daily", "UTC", 20, "test@qq.com"
	if err := store.SettingsPut(t.Context(), cfg, "fake-authorization", false, now); err != nil {
		t.Fatal(err)
	}
	id, err := store.SubscriptionPut(t.Context(), notifications.Subscription{Key: "test-client-key", Email: "user@example.com", Enabled: true}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	return store, id
}
