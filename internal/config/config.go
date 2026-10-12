package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultSafetyThreshold = int64(3)
	defaultSafetyWindow    = int64(600)
	defaultSafetyCooldown  = int64(1800)
	// 数据位置独立于插件安装 ID，独立发布后继续使用已有数据库。
	defaultDataDirectory = "usage-statistics"
)

// Config 保存宿主传入的当前插件配置，不依赖安装身份解析数据目录。
type Config struct {
	DataDir       string
	RetentionDays int
	Safety        Safety
}

type Safety struct {
	RateLimitEnabled bool
	Threshold        int64
	Window           int64
	Cooldown         int64
}

func DefaultSafety() Safety {
	return Safety{
		Threshold: defaultSafetyThreshold,
		Window:    defaultSafetyWindow,
		Cooldown:  defaultSafetyCooldown,
	}
}

// parseConfig 解析插件 YAML 配置并兼容中文别名。
func Parse(raw []byte) Config {
	cfg := Config{Safety: DefaultSafety()}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return cfg
	}
	if value, ok := lookupConfig(values, "data_dir", "数据目录"); ok {
		cfg.DataDir = strings.TrimSpace(toConfigString(value))
	}
	if value, ok := lookupConfig(values, "retention_days", "用量保留天数"); ok {
		cfg.RetentionDays = toConfigInt(value)
	}
	cfg.Safety = parseSafetyConfig(values)
	return cfg
}

func parseSafetyConfig(values map[string]any) Safety {
	cfg := DefaultSafety()
	rateLimitValid := setConfigBool(values, "rate_limit_enabled", "启用限流", &cfg.RateLimitEnabled)
	thresholdValid := setPositiveConfigInteger(values, "threshold", "触发阈值", &cfg.Threshold)
	windowValid := setPositiveConfigInteger(values, "window", "滑动窗口秒数", &cfg.Window)
	cooldownValid := setPositiveConfigInteger(values, "cooldown", "限流时长秒数", &cfg.Cooldown)

	valid := rateLimitValid && thresholdValid && windowValid && cooldownValid
	cfg.RateLimitEnabled = valid && cfg.RateLimitEnabled
	return cfg
}

func setConfigBool(values map[string]any, key, alias string, target *bool) bool {
	raw, exists := lookupConfig(values, key, alias)
	if !exists {
		return true
	}
	switch value := raw.(type) {
	case bool:
		*target = value
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return false
		}
		*target = parsed
	default:
		return false
	}
	return true
}

func setPositiveConfigInteger(values map[string]any, key, alias string, target *int64) bool {
	raw, exists := lookupConfig(values, key, alias)
	if !exists {
		return true
	}
	value, valid := normalizePositiveConfigInt(toConfigInt(raw))
	if valid {
		*target = value
	}
	return valid
}

func normalizePositiveConfigInt(value int) (int64, bool) {
	return int64(value), value > 0
}

// resolveDBPath 按配置目录、环境变量、用户主目录的顺序确定 usage.db 路径。
func DBPath(cfg Config) (string, error) {
	dir := strings.TrimSpace(cfg.DataDir)
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("USAGE_STATISTICS_DIR"))
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = ""
		}
		dir = defaultDBDirectory(home)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("usage-statistics: create data dir: %w", err)
	}
	return filepath.Join(dir, "usage.db"), nil
}

// defaultDBDirectory 保留历史数据位置；无用户目录时使用相同名称的相对目录。
func defaultDBDirectory(home string) string {
	if strings.TrimSpace(home) == "" {
		return filepath.Join(".cli-proxy-api", "plugins", defaultDataDirectory)
	}
	return filepath.Join(home, ".cli-proxy-api", "plugins", defaultDataDirectory)
}

func lookupConfig(values map[string]any, key, alias string) (any, bool) {
	if value, ok := values[key]; ok {
		return value, true
	}
	value, ok := values[alias]
	return value, ok
}

func toConfigString(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func toConfigInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return 0
}
