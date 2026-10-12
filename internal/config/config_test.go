package config

import (
	"os"
	"path/filepath"
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

// TestDBPathPreservesExistingFile verifies path precedence without opening or replacing SQLite.
func TestDBPathPreservesExistingFile(t *testing.T) {
	root := t.TempDir()
	envDir := filepath.Join(root, "environment")
	configDir := filepath.Join(root, "configured")
	t.Setenv("USAGE_STATISTICS_DIR", envDir)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(configDir, "usage.db")
	if err := os.WriteFile(existing, []byte("existing database"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := DBPath(Config{DataDir: configDir})
	if err != nil || path != existing {
		t.Fatalf("configured path = %q, error = %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "existing database" {
		t.Fatalf("existing data changed: %q, error = %v", data, err)
	}
	path, err = DBPath(Config{})
	if err != nil || path != filepath.Join(envDir, "usage.db") {
		t.Fatalf("environment path = %q, error = %v", path, err)
	}
}

// TestDefaultDBDirectoryKeepsLegacyLocation guards against data paths following a renamed plugin ID.
func TestDefaultDBDirectoryKeepsLegacyLocation(t *testing.T) {
	root := t.TempDir()
	for _, home := range []string{root, "", " "} {
		want := filepath.Join(".cli-proxy-api", "plugins", "usage-statistics")
		if home == root {
			want = filepath.Join(root, want)
		}
		if got := defaultDBDirectory(home); got != want {
			t.Fatalf("default directory for %q = %q, want historical location %q", home, got, want)
		}
	}
}
