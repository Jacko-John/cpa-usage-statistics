package notifications

import (
	"html"
	"strings"
	"testing"
	"time"

	"github.com/Jacko-John/cpa-usage-statistics/internal/testutil"
)

func notificationSampleMailForTest() Mail {
	loc, _ := time.LoadLocation("Asia/Hong_Kong")
	return Mail{
		ID: "test", To: "receiver@example.com",
		Periods: []Period{{
			Start: time.Date(2026, 9, 4, 20, 0, 0, 0, loc).Unix(),
			End:   time.Date(2026, 9, 11, 20, 0, 0, 0, loc).Unix(), Timezone: loc.String(),
			KeyLabel: notificationMask("sk-9f2b7k4m6q8r3c1a7x9q"), Note: "个人项目",
			Requests: 1280, Tokens: 2_468_000, Cost: 12.3456,
		}},
	}
}

func TestNotificationMask(t *testing.T) {
	for _, test := range []struct{ key, expected string }{
		{"", "••••"},
		{"12345678", "••••"},
		{"123456789", "••••"},
		{"123456789012", "••••"},
		{"1234567890123", "12345678…0123"},
		{"sk-demo-usage-example-7x9q", "sk-demo-…7x9q"},
	} {
		if actual := notificationMask(test.key); actual != test.expected {
			t.Errorf("长度 %d 的 Key 脱敏结果不正确：%q", len(test.key), actual)
		}
	}
}

func TestNotificationHTMLMessage(t *testing.T) {
	message := notificationSampleMailForTest()
	message.Periods[0].Note = `项目 <script>alert("test")</script> & 团队`
	subject, parts := testutil.MessageParts(t, Message(message, "sender@example.com"))
	if subject != "你的 API 用量报告" {
		t.Fatal("邮件主题不正确")
	}
	for kind, body := range parts {
		for _, text := range []string{"你的 API 用量报告", "本期预计花费", "$12.3456", "USD", "1,280", "2,468,000", "2026/09/04 20:00", "2026/09/11 20:00", "Asia/Hong_Kong", "sk-9f2b7…7x9q"} {
			if !strings.Contains(body, text) {
				t.Errorf("%s 正文缺少 %q", kind, text)
			}
		}
		if strings.Contains(body, "sk-9f2b7k4m6q8r3c1a7x9q") {
			t.Fatal("邮件泄露完整 Key")
		}
		for _, marker := range []string{"示例", "测试邮件", "预览", "费用按服务配置", "感谢你的使用"} {
			if strings.Contains(body, marker) {
				t.Fatal("邮件不应包含测试专属文案或已移除的页脚")
			}
		}
	}
	if !strings.Contains(parts["text/plain"], message.Periods[0].Note) {
		t.Fatal("纯文本正文不应进行 HTML 转义")
	}
	if strings.Contains(parts["text/html"], "<script>") || !strings.Contains(parts["text/html"], html.EscapeString(message.Periods[0].Note)) {
		t.Fatal("HTML 正文未安全转义备注")
	}
	if strings.Contains(parts["text/html"], "<img") || strings.Contains(parts["text/html"], "<link") {
		t.Fatal("邮件不应依赖外部图片或样式")
	}
}

func TestNotificationHTMLNotices(t *testing.T) {
	for _, test := range []struct {
		name     string
		period   Period
		expected string
	}{
		{"zero", Period{}, "$0.0000"},
		{"unpriced", Period{Requests: 2, Unpriced: 2}, "暂无法估算"},
		{"partly-priced", Period{Requests: 3, Unpriced: 1, Cost: 1.25}, "有 1 次请求尚未计价，当前金额不包含这部分费用。"},
		{"partial", Period{Partial: true}, "未覆盖完整周期"},
		{"skipped", Period{Skipped: 1000}, "更早的 1,000 个周期未包含在本次报告中"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := notificationSampleMailForTest()
			period := test.period
			period.Start, period.End = message.Periods[0].Start, message.Periods[0].End
			period.Timezone = "Asia/Hong_Kong"
			period.KeyLabel = "••••"
			message.Periods = []Period{period}
			_, parts := testutil.MessageParts(t, Message(message, "sender@example.com"))
			for kind, body := range parts {
				if !strings.Contains(body, test.expected) {
					t.Errorf("%s 未展示正确的 %s 提示", kind, test.name)
				}
				if test.name == "unpriced" && strings.Contains(body, "$0.0000") {
					t.Fatal("未定价不能显示为零花费")
				}
			}
		})
	}
}

func TestNotificationHTMLMultiplePeriods(t *testing.T) {
	message := notificationSampleMailForTest()
	second := message.Periods[0]
	second.Note = "另一统计周期"
	message.Periods = append(message.Periods, second)
	_, parts := testutil.MessageParts(t, Message(message, "sender@example.com"))
	for _, body := range parts {
		if !strings.Contains(body, "个人项目") || !strings.Contains(body, "另一统计周期") || strings.Count(body, "$12.3456") != 2 {
			t.Fatal("合并通知应保留每个周期的独立报告")
		}
	}
}
