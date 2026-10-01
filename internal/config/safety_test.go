package config

import (
	"testing"
)

func TestParseSafetyConfig(t *testing.T) {
	defaults := Parse(nil).Safety
	if defaults.RateLimitEnabled || defaults.Threshold != 3 || defaults.Window != 600 || defaults.Cooldown != 1800 {
		t.Fatalf("default safety config = %+v", defaults)
	}

	configured := Parse([]byte(`
rate_limit_enabled: true
threshold: 5
window: 720
cooldown: 2700
`)).Safety
	if !configured.RateLimitEnabled || configured.Threshold != 5 || configured.Window != 720 || configured.Cooldown != 2700 {
		t.Fatalf("configured safety = %+v", configured)
	}

	aliased := Parse([]byte(`
启用限流: true
触发阈值: 4
滑动窗口秒数: 300
限流时长秒数: 900
`)).Safety
	if !aliased.RateLimitEnabled || aliased.Threshold != 4 || aliased.Window != 300 || aliased.Cooldown != 900 {
		t.Fatalf("aliased safety = %+v", aliased)
	}

	converted := Parse([]byte(`
rate_limit_enabled: true
threshold: 3.5
window: "600"
cooldown: "1800"
`)).Safety
	if !converted.RateLimitEnabled || converted.Threshold != 3 || converted.Window != 600 || converted.Cooldown != 1800 {
		t.Fatalf("converted safety = %+v", converted)
	}

	invalid := []string{
		"rate_limit_enabled: true\nthreshold: 0\n",
		"rate_limit_enabled: true\nthreshold: -1\n",
		"rate_limit_enabled: true\nthreshold: \"3.5\"\n",
		"rate_limit_enabled: true\nwindow: 10m\n",
		"rate_limit_enabled: true\nwindow: 0\n",
		"rate_limit_enabled: true\nwindow: -1\n",
		"rate_limit_enabled: true\ncooldown: 30s\n",
		"rate_limit_enabled: true\ncooldown: 0\n",
		"rate_limit_enabled: true\ncooldown: -1\n",
		"rate_limit_enabled: true\ncooldown: 0.5\n",
	}
	for _, raw := range invalid {
		cfg := Parse([]byte(raw)).Safety
		if cfg.RateLimitEnabled {
			t.Fatalf("invalid config kept rate limiting enabled: %q => %+v", raw, cfg)
		}
	}
}
