package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jacko-John/cpa-usage-statistics/internal/config"
	"github.com/Jacko-John/cpa-usage-statistics/internal/notifications"
	safetycontrol "github.com/Jacko-John/cpa-usage-statistics/internal/safety"
	"github.com/Jacko-John/cpa-usage-statistics/internal/storage"
)

var (
	safety              = safetycontrol.New()
	storeMu             sync.RWMutex
	globalStore         *storage.Store
	globalNotifications *notifications.Service
	currentDBPath       string

	retentionDays           atomic.Int64
	negotiatedSchemaVersion atomic.Uint32

	cleanupMu   sync.Mutex
	lastCleanup time.Time
)

func currentStore() *storage.Store {
	storeMu.RLock()
	defer storeMu.RUnlock()
	return globalStore
}

func closeStore() {
	storeMu.Lock()
	defer storeMu.Unlock()
	if globalStore != nil {
		_ = globalStore.Close()
		globalStore = nil
		globalNotifications = nil
		currentDBPath = ""
	}
}

func ensureStore(cfg config.Config) error {
	path, err := config.DBPath(cfg)
	if err != nil {
		return err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	if globalStore != nil && currentDBPath == path {
		return nil
	}
	store, err := storage.Open(path)
	if err != nil {
		return err
	}
	notifier, err := notifications.New(context.Background(), store, nil)
	if err != nil {
		_ = store.Close()
		return err
	}
	if globalStore != nil {
		_ = globalStore.Close()
	}
	globalNotifications = notifier
	globalStore = store
	currentDBPath = path
	return nil
}

func maybeCleanup() {
	days := retentionDays.Load()
	if days <= 0 {
		return
	}
	cleanupMu.Lock()
	now := time.Now()
	if !lastCleanup.IsZero() && now.Sub(lastCleanup) < time.Hour {
		cleanupMu.Unlock()
		return
	}
	lastCleanup = now
	cleanupMu.Unlock()

	store := currentStore()
	if store == nil {
		return
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	_, _ = store.DeleteBefore(ctx, cutoff)
}

type lifecycleRequest struct {
	ConfigYAML    json.RawMessage `json:"config_yaml"`
	SchemaVersion uint32          `json:"schema_version"`
}

func configurePlugin(request []byte) error {
	var req lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return err
		}
	}
	hostSchemaVersion := req.SchemaVersion
	if hostSchemaVersion == 0 {
		hostSchemaVersion = 1
	}
	schemaVersion := min(hostSchemaVersion, pluginSchemaVersion)

	raw, err := lifecycleConfigYAML(req.ConfigYAML)
	if err != nil {
		return err
	}
	cfg := config.Parse(raw)
	if err := ensureStore(cfg); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	if err := safety.Configure(ctx, currentStore(), cfg.Safety, schemaVersion >= 2); err != nil {
		return err
	}
	retentionDays.Store(int64(cfg.RetentionDays))
	currentNotifications().Start()
	negotiatedSchemaVersion.Store(schemaVersion)
	if cfg.RetentionDays > 0 {
		cleanupMu.Lock()
		lastCleanup = time.Time{}
		cleanupMu.Unlock()
		maybeCleanup()
	}
	return nil
}

// lifecycleConfigYAML 接受 Base64 字符串、普通 YAML 字符串或字节数组。
func lifecycleConfigYAML(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if decoded, decodeErr := base64.StdEncoding.DecodeString(text); decodeErr == nil && strings.Contains(string(decoded), ":") {
			return decoded, nil
		}
		return []byte(text), nil
	}
	var bytes []byte
	if err := json.Unmarshal(raw, &bytes); err == nil {
		return bytes, nil
	}
	return nil, errors.New("config_yaml must be a string or byte array")
}

func currentNotifications() *notifications.Service {
	storeMu.RLock()
	defer storeMu.RUnlock()
	return globalNotifications
}
func Close() { closeStore() }
