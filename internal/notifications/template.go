package notifications

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"
)

type notificationEmail struct {
	Subject string
	Periods []notificationEmailPeriod
}

type notificationEmailPeriod struct {
	Title, KeyLabel, Start, End, Timezone string
	Cost, Unit, Requests, Tokens          string
	Unpriced, Partial, Skipped            string
}

const notificationEmailIntro = "本期用量已整理好，请查收你的使用情况。"

func notificationNumber(value int64) string {
	text := strconv.FormatInt(value, 10)
	for i := len(text) - 3; i > 0; i -= 3 {
		text = text[:i] + "," + text[i:]
	}
	return text
}

func notificationEmailData(mail Mail) notificationEmail {
	result := notificationEmail{Subject: "你的 API 用量报告"}
	for _, period := range mail.Periods {
		loc, err := time.LoadLocation(period.Timezone)
		if err != nil {
			loc = time.UTC
		}
		item := notificationEmailPeriod{
			Title: period.Note, KeyLabel: period.KeyLabel, Timezone: period.Timezone,
			Start: time.Unix(period.Start, 0).In(loc).Format("2006/01/02 15:04"),
			End:   time.Unix(period.End, 0).In(loc).Format("2006/01/02 15:04"),
			Cost:  fmt.Sprintf("$%.4f", period.Cost), Unit: "USD",
			Requests: notificationNumber(period.Requests), Tokens: notificationNumber(period.Tokens),
		}
		if item.Title == "" {
			item.Title = "API 使用情况"
		}
		if period.Unpriced > 0 {
			item.Unpriced = fmt.Sprintf("有 %s 次请求尚未计价，当前金额不包含这部分费用。", notificationNumber(period.Unpriced))
			if period.Unpriced == period.Requests {
				item.Cost, item.Unit = "暂无法估算", ""
			}
		}
		if period.Partial {
			item.Partial = "本期从服务生效或计划调整时开始统计，未覆盖完整周期。"
		}
		if period.Skipped > 0 {
			item.Skipped = fmt.Sprintf("更早的 %s 个周期未包含在本次报告中。", notificationNumber(period.Skipped))
		}
		result.Periods = append(result.Periods, item)
	}
	return result
}

func notificationEmailText(data notificationEmail) string {
	var body strings.Builder
	fmt.Fprintf(&body, "%s\r\n\r\n%s\r\n", data.Subject, notificationEmailIntro)
	for _, period := range data.Periods {
		fmt.Fprintf(&body, "\r\n%s\r\nAPI Key：%s\r\n统计周期：%s — %s\r\n%s · 不含结束时刻\r\n\r\n本期预计花费：%s %s\r\n请求次数：%s\r\n使用 Token：%s\r\n", period.Title, period.KeyLabel, period.Start, period.End, period.Timezone, period.Cost, period.Unit, period.Requests, period.Tokens)
		for _, notice := range []string{period.Unpriced, period.Partial, period.Skipped} {
			if notice != "" {
				fmt.Fprintf(&body, "%s\r\n", notice)
			}
		}
	}
	return body.String()
}

// 邮件使用表格布局与内联样式，不依赖外部资源；动态字段由 html/template 自动转义。
var notificationEmailHTML = template.Must(template.New("notification").Parse(`<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Subject}}</title></head>
<body style="margin:0;padding:0;background-color:#f3f5f9;color:#172033;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','Microsoft YaHei',Arial,sans-serif;">
<div style="display:none;max-height:0;overflow:hidden;mso-hide:all;">你的周期用量、预计花费和请求统计已准备好。</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f3f5f9;">
<tr><td align="center" style="padding:32px 12px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:640px;background-color:#ffffff;border:1px solid #e2e7ef;border-radius:16px;overflow:hidden;">
<tr><td height="5" style="height:5px;background-color:#4f46e5;font-size:0;line-height:0;">&nbsp;</td></tr>
<tr><td style="padding:28px 24px 8px;">
<p style="margin:0 0 18px;color:#5148c9;font-size:12px;font-weight:700;letter-spacing:2px;">CPA · 用量通知</p>
<h1 style="margin:0 0 10px;color:#172033;font-size:28px;font-weight:700;line-height:1.4;">你的 API 用量报告</h1>
<p style="margin:0;color:#5e6b80;font-size:14px;line-height:1.8;">` + notificationEmailIntro + `</p>
</td></tr>
{{range .Periods}}
<tr><td style="padding:18px 24px 24px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="table-layout:fixed;border:1px solid #e2e7ef;border-radius:12px;">
<tr><td style="padding:20px 18px 16px;">
<h2 style="margin:0 0 8px;color:#172033;font-size:18px;font-weight:600;line-height:1.5;overflow-wrap:anywhere;word-break:break-word;">{{.Title}}</h2>
<p style="margin:0;color:#66738a;font-size:12px;line-height:1.7;">API Key <span style="font-family:Consolas,Monaco,monospace;color:#475569;">{{.KeyLabel}}</span></p>
</td></tr>
<tr><td style="padding:0 18px 20px;">
<p style="margin:0 0 5px;color:#66738a;font-size:12px;line-height:1.6;">统计周期</p>
<p style="margin:0;color:#334155;font-size:13px;font-weight:500;line-height:1.8;"><span style="white-space:nowrap;">{{.Start}}</span><span style="color:#64748b;"> — </span><span style="white-space:nowrap;">{{.End}}</span></p>
<p style="margin:4px 0 0;color:#64748b;font-size:12px;line-height:1.7;overflow-wrap:anywhere;">{{.Timezone}} · 不含结束时刻</p>
</td></tr>
<tr><td style="padding:22px 18px;background-color:#f7f8fc;border-top:1px solid #e9edf4;border-bottom:1px solid #e9edf4;">
<p style="margin:0 0 8px;color:#66738a;font-size:12px;line-height:1.6;">本期预计花费</p>
<p style="margin:0;color:#172033;font-size:36px;font-weight:700;line-height:1.3;letter-spacing:-1px;overflow-wrap:anywhere;">{{.Cost}} <span style="color:#64748b;font-size:12px;font-weight:500;letter-spacing:0;">{{.Unit}}</span></p>
{{if .Unpriced}}<p style="margin:12px 0 0;color:#8a5700;font-size:12px;line-height:1.8;">{{.Unpriced}}</p>{{end}}
</td></tr>
<tr><td style="padding:20px 18px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="table-layout:fixed;"><tr>
<td width="50%" valign="top" style="padding-right:12px;border-right:1px solid #e9edf4;">
<p style="margin:0 0 6px;color:#66738a;font-size:12px;line-height:1.6;">请求次数</p><p style="margin:0;color:#172033;font-size:20px;font-weight:600;line-height:1.4;overflow-wrap:anywhere;">{{.Requests}}</p>
</td>
<td width="50%" valign="top" style="padding-left:12px;">
<p style="margin:0 0 6px;color:#66738a;font-size:12px;line-height:1.6;">使用 Token</p><p style="margin:0;color:#172033;font-size:20px;font-weight:600;line-height:1.4;overflow-wrap:anywhere;">{{.Tokens}}</p>
</td>
</tr></table>
{{if .Partial}}<p style="margin:16px 0 0;color:#66738a;font-size:12px;line-height:1.8;">{{.Partial}}</p>{{end}}
{{if .Skipped}}<p style="margin:12px 0 0;color:#66738a;font-size:12px;line-height:1.8;">{{.Skipped}}</p>{{end}}
</td></tr>
</table>
</td></tr>
{{end}}
</table>
</td></tr></table>
</body></html>`))
