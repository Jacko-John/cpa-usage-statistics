# CPA Usage Statistics

CLIProxyAPI 的持久化用量统计插件。插件将请求用量写入本地 SQLite，并提供受管理鉴权保护的查询、删除、安全统计、手动限流解除和 API Key 禁用接口。

当前版本：`0.3.1`

## 建议使用配套的前端面板

[配套面板](https://github.com/Jacko-John/Cli-Proxy-API-Management-Center)

## 功能

- 持久化每次请求的用量、延迟、失败信息和 Token 统计。
- 在内存中增量维护监控摘要、时间序列和凭证配额窗口统计。
- 由插件持久化模型价格和 Service Tier 倍率，并逐请求精确计算成本。
- 请求事件按需查询，服务端最多返回最新 100 条。
- 按保留天数清理旧记录。
- 识别上游 `cyber_policy` 安全错误。
- 按下游 API Key 统计安全触发总数、精确滑动窗口次数和历史限流次数。
- 在请求到达上游前按 API Key 执行内存限流检查。
- 查看安全触发历史和历史限流记录。
- 手动解除 API Key 的当前限制并清空其滑动窗口。
- 永久禁用已经记录安全状态的 API Key，并在重新启用时同时解除自动限流。

## 安装

下载对应平台的 release zip，将动态库放到 CLIProxyAPI 的插件目录：

```text
plugins/linux/amd64/usage-statistics.so
plugins/windows/amd64/usage-statistics.dll
plugins/darwin/arm64/usage-statistics.dylib
```

放好后重启 CLIProxyAPI。

自动限流和手动禁用需要宿主支持插件 JSON 消息协议 `schema_version >= 2`。当前检出的 CLIProxyAPI 支持版本 4。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    usage-statistics:
      enabled: true
      priority: 100

      # usage.db 存放目录；缺省为 ~/.cli-proxy-api/plugins/usage-statistics
      data_dir: ""
      # 保留天数；0 表示不清理
      retention_days: 0
      # 对后续请求执行限流；安全事件始终记录，限流缺省为 false
      rate_limit_enabled: false
      # 滑动窗口内达到该次数后启动限流
      threshold: 3
      # 精确滑动窗口，单位为秒
      window: 600
      # 每次限制持续时间，单位为秒
      cooldown: 1800
```

`data_dir` 也可用环境变量 `USAGE_STATISTICS_DIR` 指定。所有配置字段都直接位于 `usage-statistics` 下，不使用嵌套的 `safety` 对象。配置支持以下中文别名：`数据目录`、`用量保留天数`、`启用限流`、`触发阈值`、`滑动窗口秒数`、`限流时长秒数`。

安全配置规则：

- 安全事件始终记录；`rate_limit_enabled` 只控制是否自动拦截后续请求。
- 手动禁用独立于 `rate_limit_enabled`，重启后继续生效，直到管理员重新启用。
- `threshold`、`window` 和 `cooldown` 复用已有宽松整数转换：整数字符串可直接解析，数值小数会截断小数部分；转换结果必须大于 0。
- `window` 和 `cooldown` 的单位为秒；不接受 `10m`、`30s`、`500ms` 等 duration 字符串，也不接受 `"3.5"` 等小数字符串。
- 安全配置无效时关闭限流，原有用量统计继续工作。
- 运行中修改 `threshold` 或 `window` 会清空所有 Key 的当前滑动窗口；累计触发次数、累计限流次数和未到期限制不清空。

## API Key 请求头

启用 `rate_limit_enabled` 或存在手动禁用的 Key 时，插件只从以下请求头提取 API Key：

```http
Authorization: Bearer <APIKey>
Authorization: <APIKey>
X-Api-Key: <APIKey>
X-Goog-Api-Key: <APIKey>
```

提取规则：

- API Key 只清理首尾空白，不修改大小写。
- 多个请求头包含相同 Key 时按同一个 Key 处理。
- 多个请求头包含不同 Key 时返回 HTTP 400，错误码为 `ambiguous_api_key`。
- 没有受支持请求头时返回 HTTP 401，错误码为 `unsupported_api_key_transport`。
- URL 查询参数 `key` 和 `auth_token` 不受支持。

## 管理 API

以下路由均受 CLIProxyAPI 管理鉴权保护。

### 按需查询请求事件

```http
GET /v0/management/plugins/usage-statistics/usage?start=<RFC3339>&end=<RFC3339>&limit=<数量>
```

- `start` 和 `end` 可选，采用左闭右开时间范围。
- `limit` 可选，默认 100；无论传入多大值，服务端最多返回最新 100 条。
- 此接口仅用于事件明细按需加载，不用于监控摘要。

响应按“API Key 或 provider → 模型”两层聚合：

```json
{
  "cpa-client-key-1": {
    "claude-sonnet-4-5": [
      {
        "id": "request-record-id",
        "timestamp": "2026-05-02T10:30:00Z",
        "provider": "anthropic",
        "source": "claude-code",
        "auth_index": "auth-1",
        "reasoning_effort": "high",
        "service_tier": "priority",
        "latency_ms": 1250,
        "ttft_ms": 200,
        "tokens": {
          "input_tokens": 10,
          "output_tokens": 20,
          "reasoning_tokens": 3,
          "cached_tokens": 4,
          "cache_read_tokens": 0,
          "cache_creation_tokens": 0,
          "total_tokens": 33
        },
        "failed": true,
        "failure_status_code": 429,
        "failure_body": "rate limited"
      }
    ]
  }
}
```

`failure_body` 保留数据库中的完整原始正文，并在管理响应中按内容输出：合法 JSON 直接成为对应的 JSON 对象、数组或其他 JSON 值；非 JSON 正文作为普通 JSON 字符串；空正文继续省略该字段。不使用 Base64 编码或额外包装字段。

### 读取和更新价格配置

```http
GET /v0/management/plugins/usage-statistics/pricing
```

```http
PUT /v0/management/plugins/usage-statistics/pricing
Content-Type: application/json
```

```json
{
  "prices": {
    "gpt-5.4": {
      "input": 2.5,
      "output": 15,
      "cacheCreate": 0,
      "cacheRead": 0.25,
      "contextTiers": []
    }
  },
  "tier_multipliers": [
    {"model": "gpt-5.4", "tier": "priority", "multiplier": 2}
  ]
}
```

价格配置由插件持久化，是统计成本的唯一数据源。`PUT` 会逐条重算历史成本；重建使用独立 WAL 只读快照，普通用量写入继续使用主连接。重建期间的新记录通过内存增量补齐，最终只短暂占用主连接来保存价格并切换统计状态。重建失败时继续使用旧价格和旧统计。

Context tier 仅在请求的上下文 Token 严格大于 `threshold` 时生效；多个 tier 同时命中时选择最高阈值。Service Tier 倍率按模型名称和 `service_tier` 大小写不敏感精确匹配。

### 查询预计算统计摘要

```http
POST /v0/management/plugins/usage-statistics/usage
Content-Type: application/json
```

```json
{
  "range": "24h"
}
```

`range` 支持 `7h`、`24h`、`7d`、`30d` 和 `all`。响应直接来自插件维护的内存快照，包含总请求数、成功/失败数、Token、成本、延迟、TPS、API Key、模型、凭证以及小时/天时间桶。小时桶使用 Unix 秒作为键；天桶按后端进程所在时区的自然日聚合，使用 `YYYY-MM-DD` 作为键。`all` 不统计 RPM/TPM，其 `rate_requests`、`rate_tokens` 和 `rate_minutes` 均为 `0`。

```json
{
  "range": "24h",
  "generated_at": 1788343200,
  "rate_requests": 120,
  "rate_tokens": 240000,
  "rate_minutes": 1440,
  "total_requests": 120,
  "success_count": 118,
  "failure_count": 2,
  "total_tokens": 240000,
  "total_cost": 1.23,
  "apis": {},
  "credentials": {},
  "hours": {},
  "days": {}
}
```

插件启动时先读取持久化价格，再扫描 SQLite 一次建立统计状态；每次成功写入用量后按当前价格增量更新。摘要和凭证窗口请求只读取内存，不接收价格配置，也不查询 SQLite。只有 `PUT /pricing` 修改价格时才重建历史成本。

### 查询凭证配额窗口统计

```http
POST /v0/management/plugins/usage-statistics/usage/credential-windows
Content-Type: application/json
```

```json
{
  "windows": [
    {
      "id": "credential.json",
      "auth_index": "auth-1",
      "source": "credential.json",
      "category": "claude_gpt",
      "start": 1787738400,
      "end": 1788343200
    }
  ]
}
```

- `id` 是响应 Map 的键。
- 凭证优先按 `auth_index` 匹配；为空时按 `source` 匹配。
- `category` 支持 `all`、`claude_gpt`（Claude 和 GPT）以及 `gemini`。
- `start`、`end` 使用 Unix 秒，窗口两端沿用 60 秒容差。
- 插件通过最近 31 天的分钟桶和小时桶计算，不扫描原始事件表。
- 窗口成本直接使用插件当前持久化价格，与监控摘要保持一致。

```json
{
  "windows": {
    "credential.json": {
      "requests": 20,
      "success_count": 19,
      "failure_count": 1,
      "tokens": 40000,
      "cost": 0.42
    }
  }
}
```

### 删除用量记录

```http
DELETE /v0/management/plugins/usage-statistics/usage
Content-Type: application/json

{"ids": ["<record-id>", "..."]}
```

响应示例：

```json
{"deleted": 1, "missing": ["..."]}
```

安全历史关联的用量记录不会被删除；删除请求中的此类 ID 会保留在现有 `missing` 列表中，以保持接口结构兼容。

### 查看每个 API Key 的安全状态

```http
GET /v0/management/plugins/usage-statistics/safety/keys
```

此接口只读取内存，不查询 SQLite。

```json
{
  "keys": [
    {
      "api_key": "cpa-client-key-1",
      "total_trigger_count": 8,
      "window_trigger_count": 3,
      "total_limit_count": 2,
      "manual_disabled": false,
      "blocked": true,
      "blocked_until": 1788177600
    }
  ]
}
```

管理响应按已确认设计显示完整 API Key。插件不会把 API Key 写入普通日志或限流错误响应。

### 查看安全触发历史

```http
POST /v0/management/plugins/usage-statistics/safety/history
Content-Type: application/json
```

```json
{
  "api_key": "cpa-client-key-1",
  "blocked_only": false,
  "before_time": 1788175800,
  "before_id": "usage-record-id",
  "limit": 100
}
```

字段规则：

- `api_key`：必填，只查询一个 API Key；完整 Key 位于受管理鉴权保护的请求体中，不进入 URL。
- `blocked_only=true`：只返回启动过限流的触发记录；默认 `false`。
- `before_time` 和 `before_id`：可选，必须同时提供，用于复合游标分页。
- `limit`：可选，默认 100，必须大于 0，不设置上限。

响应中的 `status`：

- `not_limited`：普通安全触发，没有启动限流。
- `active`：本次触发启动的限制仍在生效。
- `expired`：限制已自然到期。
- `manual_released`：限制已被手动解除。

```json
{
  "history": [
    {
      "api_key": "cpa-client-key-1",
      "occurred_at": 1788175800,
      "usage_record_id": "request-record-id",
      "blocked_until": 1788177600,
      "manual_released_at": 0,
      "provider": "codex",
      "model": "gpt-5-codex",
      "failure_status_code": 400,
      "failure_body": {
        "error": {
          "code": "cyber_policy",
          "message": "..."
        }
      },
      "status": "active"
    }
  ]
}
```

历史接口通过 `usage_record_id` 按 `usage_records.id` 主键关联，只补充 `provider`、`model`、`failure_status_code` 和 `failure_body`，不返回账号、Token、延迟等其他用量详情。`failure_body` 使用与 `/usage` 相同的内容转换规则，但字段固定存在。安全历史关联的用量记录不会被插件删除；如果升级前记录已经缺失，正文和关联字段按缺失记录返回：`provider` 和 `model` 为空字符串，`failure_status_code` 为 0，`failure_body` 为 `null`。

### 手动解除限制

```http
DELETE /v0/management/plugins/usage-statistics/safety/limits
Content-Type: application/json

{"api_keys": ["cpa-client-key-1"]}
```

响应示例：

```json
{
  "released": ["cpa-client-key-1"],
  "not_blocked": []
}
```

解除后：

- 请求立即恢复放行，除非该 Key 仍处于手动禁用状态。
- 当前滑动窗口被清空。
- 历史触发总数和历史限流总数保留。
- 对应历史行写入 `manual_released_at`。

### 手动禁用和重新启用 Key

只能操作已经出现在安全状态列表中的 Key。手动禁用不会增加安全触发数、累计限流数或安全历史。

禁用：

```http
POST /v0/management/plugins/usage-statistics/safety/disabled
Content-Type: application/json

{"api_keys": ["cpa-client-key-1"]}
```

```json
{
  "disabled": ["cpa-client-key-1"],
  "already_disabled": [],
  "missing": []
}
```

重新启用：

```http
DELETE /v0/management/plugins/usage-statistics/safety/disabled
Content-Type: application/json

{"api_keys": ["cpa-client-key-1"]}
```

```json
{
  "enabled": ["cpa-client-key-1"],
  "already_enabled": [],
  "missing": []
}
```

手动禁用永久保存并独立于 `rate_limit_enabled`。重新启用时同时清空当前滑动窗口和自动限流；如果自动限流仍在生效，对应历史行会写入 `manual_released_at`。

## 按 Key 的周期用量邮件通知

配套面板的“用量通知”页面支持管理员为客户端 API Key 绑定收件邮箱。全部 Key 共用通知计划，支持每天、每周及指定 IANA 时区，默认 `Asia/Shanghai`，时间精确到分钟。邮件任务由插件运行，不依赖浏览器保持打开。

### 配置与价格口径

1. 在 QQ 邮箱开启 SMTP，生成 **SMTP 授权码**，不是 QQ 登录密码。
2. 在面板填写发件 QQ 邮箱、授权码、通知计划与时区并保存；默认连接 `smtp.qq.com:465`，强制 TLS 和证书校验。
3. 使用“发送测试邮件”检查已保存的配置，然后为 Key 添加邮箱绑定。

正式通知和测试邮件共用面向收件人的 HTML 报告，展示预计花费、周期、请求数和 Token，并提供纯文本备用正文。测试邮件仅使用虚拟数据，主题、文案和样式与正式通知完全一致，不增加测试标记；周期按已保存的计划生成，不读取真实用量或改变正式通知状态。

授权码只在保存时提交，读取接口仅返回 `password_configured`，不会回传授权码。空白输入保留原值，清除需显式勾选。授权码存于插件私有 SQLite，由目录/文件权限保护，**不是加密存储**。请使用 HTTPS 或可信本地管理连接，妥善保护数据库备份。

通知与原用量统计共用**请求时间分钟桶**，按 `usage_records.timestamp` 归属周期，使用严格的 `[开始,结束)`，没有前后宽限。周期快照只包含封存时已上报且仍保留的明细；封存后迟到的记录不改写旧通知，也不挪入下一周期。首次启用订阅从当前计划周期的起点统计，包含数据库中仍保留的该周期历史用量，不补发更早周期。暂停恢复、删除后重新绑定和后续计划变更仍从下一分钟生效，不补算暂停间隔；计划变更的过渡期标记为非完整周期。

升级时会自动补齐旧版尚未结束、未封存且从未切换过计划的有效首段，同时保留已有首期的收件地址和备注。已结束或已封存周期、已推进游标及暂停或计划切换后的分段不回写。已对齐周期边界的首段保持不变，重复启动不会再次回溯。

- 当前未结束周期：修改模型价格、Context tier 或 Service Tier multiplier 后，整个当前周期按新价格重新计算。
- 已结束周期：按结算价格固定金额，不随改价重算；邮件重试使用原结算金额。
- 金额为配置模型价格下的本地用量估值，不是上游订阅额度或官方账单。未配置模型价格的请求单独计数，不冒充真实零花费。
- 当前及尚未封存周期以仍保留的明细为准，手动删除或保留期清理会同步扣减其用量；改价复用原统计的明细重建机制。已经封存的周期快照不随明细删除而改变。
- 修改收件邮箱不切断当前周期；当前周期沿用原邮箱，后续周期使用新邮箱。暂停保留历史且停止后续统计和发送，删除绑定也保留历史；重新绑定同一个 Key 可以继续查看原历史，不把旧 Key 的历史转移到新 Key。

页面分别显示当前周期和最近 10 个已结束周期，包括请求数、Token、美元花费、未定价提示和邮件状态。已结束且发送已终结的旧记录会裁剪；待处理邮件不会为裁剪历史而丢弃。

### 调度和恢复

通知直接查询原统计中的 Key 分钟桶，不维护另一套分钟桶、计价基数或恢复标记，不修改 `usage_records` 的结构和原写入流程。启动和改价复用原统计重建；到期时批量查询周期用量，在同一事务中保存快照并推进周期游标。原删除规则及安全历史关联记录保护不变。

无保留用量的周期仍发送零用量报告。重启后恢复待发任务，长时间停机仅补最近 10 个结束周期，同一 Key 和收件地址合并发送；更早遗漏数量会提示。超过约 31 天缓存的窗口复用原扫描及计价代码，按有界时间范围查询仍保留的明细，不扩张常驻缓存。补发无法恢复已清理的用量，不代表供应商完整账单。邮件按分钟触发，实际到达时间受 SMTP 服务和网络影响。

明确失败后按 1、5、30 分钟最多重试三次。SMTP 最终接收确认丢失，或发送过程中进程停止时，标记“发送结果未知”，不自动重发；管理员可手动重试，但可能重复投递。已交给 SMTP 的邮件不能撤回。

### 管理接口

均使用现有宿主管理认证，路径前缀为 `/v0/management/plugins/usage-statistics/notifications`：

| 方法 | 路径 | 用途 |
|---|---|---|
| GET / PUT | `/settings` | 读取或保存计划及 QQ SMTP 配置 |
| GET / POST / PATCH / DELETE | `/subscriptions` | 列表、新增、编辑或删除 Key 邮箱绑定 |
| POST | `/history` | 按订阅 `id` 查询当前与最近十期 |
| POST | `/test-email` | 按 `email` 发送测试邮件 |
| POST | `/retry` | 按周期 `id` 重试失败或未知状态邮件 |

Key 不放在 URL 中，邮件只包含脱敏 Key 和备注。新生成的脱敏文本保留前 8 位和后 4 位，中间使用省略号；长度不超过 12 位时完全隐藏，不回写已封存记录中的旧脱敏文本。`/settings` 的写入字段包括 `enabled`、`frequency`（`daily` / `weekly`）、`weekday`（0 为周日）、`hour`、`minute`、`timezone`、`sender`、`password` 和 `clear_password`。订阅字段包括 `id`（新建时省略）、`api_key`、`email`、`note`、`enabled`。

## 拦截响应

手动禁用的 Key 会在进入上游前收到：

```http
HTTP/1.1 403 Forbidden
Content-Type: application/json
```

```json
{
  "error": {
    "type": "permission_error",
    "code": "api_key_disabled",
    "message": "This API key has been disabled by an administrator."
  }
}
```

该响应不包含完整 API Key，也不返回 `Retry-After`。

达到自动限流阈值后，该 API Key 的所有模型请求都会在进入上游前被拦截：

```http
HTTP/1.1 429 Too Many Requests
Content-Type: application/json
Retry-After: 1800
```

OpenAI 和 Codex 响应示例：

```json
{
  "error": {
    "type": "rate_limit_error",
    "code": "safety_rate_limited",
    "message": "Too many requests were rejected by the upstream safety policy. Retry later."
  }
}
```

插件会根据 `SourceFormat` 返回 OpenAI/Codex、Claude 或 Gemini 兼容错误结构。

首次触发安全提醒的流式请求仍可能表现为：

```text
HTTP 200 + response.failed
```

原因是 HTTP 流已经建立。`usage.handle` 在请求完成后记录事件并激活限制，后续请求才会收到 HTTP 429；极短时间内紧接着到来的请求可能先通过。

## 安全识别范围

插件只在失败请求中解析 `Failure.Body`，依次检查：

```text
error.code
response.error.code
body.error.code
code
```

第一版只统计精确错误码：

```text
cyber_policy
```

不统计 `content_filter`、普通模型拒绝文本、认证失败、配额不足、网络错误或超时。完整失败内容继续保存在 `usage_records.failure_body`。

## 存储与性能

SQLite 新增两张小表：

- `api_key_safety_state`：每个触发过安全提醒的 API Key 一行。
- `api_key_safety_history`：每次 `cyber_policy` 触发一行。

请求热路径为：

```text
请求头提取 → 内存 Map 查询 → 放行或返回 403/429
```

普通请求限流检查：

- 时间复杂度 O(1)。
- SQLite 查询 0 次。
- 不读取大型 `usage_records`。
- 不解码滑动窗口 BLOB。

安全事件会在一个事务中写入用量记录、安全历史和 Key 状态；事务提交成功后才更新内存状态。插件重启时从状态表恢复滑动窗口、未到期限制和手动禁用状态。

SQLite 表不使用 `CHECK` 约束。请求数据写入前由 Go 对延迟、Token、失败状态码等数值执行非负归一化；升级时会一次性重建仍含 `CHECK` 的旧 `usage_records` 表并保留数据。

安全历史、API Key 安全状态及安全历史关联的 `usage_records` 永久保留，不受 `retention_days`、用量保留期清理或按 ID 删除影响。只有未被安全历史关联的用量记录可以删除。

安全历史查询先按小型历史表定位记录，再按 `usage_records.id` 主键关联模型、provider 和错误信息，不扫描大型用量表。普通请求热路径不会执行该查询。

## 开发与模块结构

根目录只保留 `main.go` 和 `abi_cgo.go`，负责发布时的版本信息注入及宿主 C ABI。业务代码按职责放在 `internal/`：

| 模块 | 职责 |
| --- | --- |
| `plugin` | 宿主注册、生命周期、请求拦截、用量接收和管理路由 |
| `config` | YAML 配置解析和数据库路径 |
| `protocol` | 宿主请求、响应类型和 JSON 信封 |
| `model` | 共享用量、安全数据类型及数值归一化 |
| `storage` | SQLite 连接、迁移、用量与安全数据持久化、价格重算协调 |
| `statistics` | 价格计算、增量聚合、时间桶和历史窗口补查 |
| `notifications` | 设置、订阅、周期结算、历史、发件队列、SMTP 和邮件渲染 |
| `safety` | API Key 提取、安全错误识别和限流状态变更 |

`plugin` 负责组装各模块，业务模块不反向引用它。通知模块使用存储层的唯一连接；修改价格前，存储层通过结算接口按旧价格封存已结束的通知周期。数据库写入成功后才更新内存统计，关闭数据库前先停止并等待通知任务退出。

测试随模块放置；`testfixture` 和 `testutil` 仅供测试使用。修改模块边界后运行：

```sh
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o /tmp/usage-statistics.so .
```

SMTP 测试启动本地 TLS 服务，不发送真实邮件。发布继续使用 `build.sh` 和 `package-release.sh`，版本变量仍为 `main.pluginVersion`、`main.pluginAuthor` 和 `main.pluginRepository`。后续修改规则见 [AGENTS.md](AGENTS.md)。

## 已知限制

- 只支持请求头 API Key。
- 不支持查询参数认证、其他认证插件、Realtime 临时客户端密钥或匿名请求的按 Key 限流。
- 本功能面向正常 Agent 客户端，不是无法绕过的安全边界。
- SQLite 状态只在单个 CLIProxyAPI 实例内共享。
- 限流状态由 `usage.handle` 异步激活，首次触发与后续拦截之间存在极短竞态窗口。
