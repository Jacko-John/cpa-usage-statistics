package config

import (
	"testing"
)

func TestParseConfig(t *testing.T) {
	cfg := Parse([]byte("enabled: true\npriority: 120\ndata_dir: /tmp/usage\nretention_days: 45\n"))
	if cfg.DataDir != "/tmp/usage" {
		t.Fatalf("data_dir = %q", cfg.DataDir)
	}
	if cfg.RetentionDays != 45 {
		t.Fatalf("retention_days = %d", cfg.RetentionDays)
	}

	zh := Parse([]byte("用量保留天数: 7\n"))
	if zh.RetentionDays != 7 {
		t.Fatalf("chinese retention = %d", zh.RetentionDays)
	}
}
