# 告警投递适配方案（设计提案）

状态：提案，待评审
范围：`internal/alert`、`internal/app/admin_alerts.go`、`internal/app/alerts.go`、`internal/safetransport`、`internal/domain`、Web 控制台「告警与审计」页

---

## 勘误（撰写后发现，未修改正文）

本文 §1.2 称「`docs/` 下没有 payload 契约说明」，**这是错的**。
[`docs/contracts/webhook-payloads.md`](../contracts/webhook-payloads.md) 已经存在，且已经
文档化了 payload 结构与安全约束。

更重要的是，那份文档记录了 v1 的既定决策：

> Halro intentionally keeps platform-specific formatting **outside** the Gateway
> process in v1. A small trusted relay can map the generic event as follows.

并给出了 Slack、飞书、企业微信、Discord 四个映射示例。

也就是说，本文 §7 作为「被否决方案」写掉的中继路线，实际上是团队已经明确选定并写进文档的
v1 路线。评审本提案时必须先回答一个前置问题：**是延续既定的进程外中继路线，还是推翻它把
适配收进进程。** 本文其余部分（payload 契约化、内网可达性、成功判定）在两种路线下都成立，
只有第三期「内置适配器」与该决策直接冲突。

---

## 1. 问题

Halro 目前向所有告警 Webhook 投递同一种 JSON：

```json
{
  "id": "alt_9km8trnc6mqheymeyfjdqq9xg8",
  "type": "admin_test",
  "severity": "info",
  "dedup_key": "",
  "summary": "Halro alert connection test",
  "timestamp": "2026-08-05T10:43:11Z",
  "details": { "source": "admin" }
}
```

这个结构只有「能接受任意 JSON 的接收端」才处理得了。真实世界的接收端分三类，它们需要的东西完全不同：

| 类别 | 接收端归谁控制 | 数量 | 正确的解法 |
| --- | --- | --- | --- |
| **A. 第三方 SaaS** — Slack、Teams、Discord、PagerDuty、Opsgenie、飞书、钉钉、企业微信 | 厂商 | 有限可枚举（约 10 个覆盖绝大多数） | 我们适配它们 |
| **B. 企业自建接收端** — 内部告警总线、工单系统、SIEM | 客户 | 无限 | 给一份稳定契约，客户照着写 receiver |
| **C. 企业改不动的接收端** — 老旧 SIEM、只吃固定格式的系统 | 客户，但不可改 | 少数 | 受限的 body 转换 |

把三类混为一谈会导致两种错误结论：以为「做个模板引擎就解决了」（对 A 无效，因为 A 的差异不只是 body），或者以为「多写几个内置适配器就够了」（对 B 无效，因为 B 无法枚举）。

### 1.1 A 类的差异不只是 body

如果适配器接口设计成 `render(event) -> []byte`，做出来仍然不通。实际存在四处差异：

| 差异点 | 例子 |
| --- | --- |
| body 结构 | 飞书 `{"msg_type":"text","content":{"text":…}}`；Slack `{"text":…}`；钉钉 `{"msgtype":"text","text":{"content":…}}`；Teams 用 MessageCard / Adaptive Card |
| 凭据用法 | 通用：header。Slack incoming webhook：URL 本身即凭据，不需要额外密钥。钉钉：URL query 上的 `timestamp` + HMAC-SHA256 `sign`。飞书自定义机器人：body 内的 `sign` 字段 |
| 成功判定 | 通用：2xx 即成功。飞书 / 钉钉 / 企微：**HTTP 200 但 body 里 `code != 0` 表示失败** |
| 内容约束 | 飞书群机器人关键词白名单、钉钉关键词与 IP 白名单、各家长度上限 |

因此适配器契约至少是：

```
render(event)                  -> body, extraHeaders, urlQuery
classify(statusCode, body)     -> success | failure(reason)
authModes()                    -> 该平台支持的凭据用法集合
```

第二行直接决定了「你现在遇到的飞书收不到消息」能否被系统自己发现：当前 `deliver` 把响应体丢弃、只看状态码，所以对方在应用层拒收时我们报成功。

### 1.2 B 类真正缺的不是适配器

B 类客户控制自己的接收端，业界惯例是产品提供**契约**而非适配。Halro 已经在发自己的事件 JSON，方向是对的，但这份 payload 目前不够格当契约：

- **没有 schema 版本号**。以后加字段或改语义时，客户的 parser 会静默错位。
- **没有对 body 的签名**。现在把密钥当 header 原样发出（`Authorization: <secret>`），接收端只能比对字符串。密钥每次投递都出网一次；TLS 中间设备或接收端访问日志都会留下明文。
- **没有投递 ID 与重放窗口**。重试时接收端无法去重，也无法拒绝被录制后重放的旧请求。
- **没有文档**。`docs/` 下没有 payload 契约说明。

补齐这四条，B 类客户根本不需要适配层。这部分投入比做模板引擎小，收益大得多。

### 1.3 比格式更早出现的两个硬阻断

面向企业自建接收端时，客户会先撞上这两个问题，跟 payload 格式无关：

- **私网地址被拒**。`internal/config/config.go` 的 `Security.AllowPrivateWebhooks` 默认为 `false`，而企业自建系统按定义在私网。这个开关**没有出现在 `configs/config.example.yaml`**，也没有文档，客户只会看到「地址无效」而不知道怎么打开。
- **不支持私有 CA**。`internal/safetransport/transport.go` 全文没有 `TLSClientConfig` / `RootCAs`，走 Go 默认系统信任库。企业内网接收端用内部 CA 签发证书是常态，握手会直接失败；而 `RequireHTTPS: true` 意味着无法降级到 http 绕开。

---

## 2. 目标与非目标

### 目标

1. B 类客户能仅凭一份文档写出可用且可验证真实性的 receiver。
2. A 类主流平台开箱可用，且平台在应用层拒收时系统能自己判定为失败。
3. 企业内网、私有 CA 的接收端可达。
4. 不削弱现有的出站防护与凭据边界。
5. 存量 Webhook 行为不变，无需人工迁移。

### 非目标

- 不做可加载的第三方代码模块（Go plugin / WASM / 子进程）。理由见 §7。
- 不做富文本/卡片编排。适配器只保证消息可读地送达，不追求各平台的排版能力。
- 本方案不涉及 syslog / CEF / OTLP 等非 HTTP 通道。若后续 SIEM 场景需要，另开提案。

---

## 3. 总体架构

把当前「序列化事件 → POST」的单步流程拆成一条有明确接缝的管线：

```
                 ┌──────────────────────────────────────────────┐
   alert.Event ─►│ Formatter.Render                             │
                 │  generic / cloudevents / slack / feishu / ... │
                 └──────────────────┬───────────────────────────┘
                                    │ body, headers, query
                                    ▼
                 ┌──────────────────────────────────────────────┐
                 │ Signer.Apply                                 │
                 │  none / header / hmac_body / hmac_query      │
                 └──────────────────┬───────────────────────────┘
                                    │
                                    ▼
                 ┌──────────────────────────────────────────────┐
                 │ safetransport（现有：pinned dial、拒私网、      │
                 │  不跟随重定向、不读代理环境变量）+ 重试         │
                 └──────────────────┬───────────────────────────┘
                                    │ statusCode, body
                                    ▼
                 ┌──────────────────────────────────────────────┐
                 │ Formatter.Classify → success | failure(reason)│
                 └──────────────────────────────────────────────┘
```

三个接缝各自可测：Render 是纯函数，Classify 是纯函数，Signer 是纯函数。只有 transport 需要网络。

对应的 Go 接口（放 `internal/alert/format.go`）：

```go
type FormatID string

const (
    FormatGeneric     FormatID = "generic"      // 今天的行为，默认值
    FormatCloudEvents FormatID = "cloudevents"
    FormatSlack       FormatID = "slack"
    FormatTeams       FormatID = "teams"
    FormatDiscord     FormatID = "discord"
    FormatFeishu      FormatID = "feishu"
    FormatDingTalk    FormatID = "dingtalk"
    FormatWeCom       FormatID = "wecom"
    FormatPagerDuty   FormatID = "pagerduty"
    FormatOpsgenie    FormatID = "opsgenie"
    FormatTemplate    FormatID = "template"     // 第四期
)

type Rendered struct {
    Body    []byte
    Headers map[string]string
    Query   map[string]string
}

type Outcome struct {
    Success bool
    Reason  string // delivered / rejected_by_endpoint / http_client_error / transport_error / ...
    Detail  string // 对方回包片段，已截断且去控制字符
}

type Formatter interface {
    ID() FormatID
    AuthModes() []AuthMode          // 该平台允许的凭据用法
    Render(event Event) (Rendered, error)
    Classify(status int, body []byte) Outcome
}
```

注册表用编译期 map，与 `internal/domain/provider_profile.go` 里 `ProviderProfileID` 的既有约定保持一致——服务商的差异（access surface、凭据方案、能力矩阵）本来就是用编译期 profile 表达的，告警适配器是同一类问题。

---

## 4. 分期实施

四期按「阻断严重度 × 投入」排序，每一期独立可发布。

### 第一期：把 payload 变成正式契约（服务 B 类）

**这一期不需要任何适配器，就能解决大部分企业自建场景。**

#### 1.1 payload 加版本

```json
{
  "schema": "halro.alert/v1",
  "id": "alt_9km8trnc6mqheymeyfjdqq9xg8",
  "type": "token_guard.block",
  "severity": "warning",
  "dedup_key": "prj_x:subject_y",
  "summary": "…",
  "project_id": "prj_x",
  "timestamp": "2026-08-05T10:43:11Z",
  "details": { }
}
```

`schema` 是 payload 的第一个字段，取值形如 `halro.alert/v1`。版本策略：加可选字段不升版本；删字段、改字段语义、改类型才升版本。升版本时新旧并行至少一个大版本周期。

#### 1.2 HMAC 签名（新增 `auth_mode: signature`）

请求头：

```
X-Halro-Delivery:  dlv_01j...            每次投递唯一，重试时不变
X-Halro-Event:     token_guard.block
X-Halro-Timestamp: 1786012991            Unix 秒
X-Halro-Signature: v1=<hex(hmac_sha256)>
```

签名基串（与 Slack / Stripe 的做法一致，便于客户复用现成实现）：

```
base   = "v1:" + timestamp + ":" + rawBody
sig    = hex(HMAC_SHA256(secret, base))
header = "v1=" + sig
```

接收端验证步骤，写进文档：

1. 拒绝 `|now - timestamp| > 300s`（防重放）。
2. 用**原始字节**重算签名，不要先反序列化再序列化。
3. 常量时间比较。
4. 用 `X-Halro-Delivery` 去重；同一 delivery id 的重试必须幂等处理。

相对现有 header 模式的改进：密钥不再出网；接收端能验证消息确实来自本实例且未被篡改；能拒绝重放。

`auth_mode` 取值：

| 值 | 含义 | 备注 |
| --- | --- | --- |
| `none` | 不带凭据 | Slack incoming webhook 这类「URL 即凭据」的场景 |
| `header` | 密钥原样放在指定 header | **现有行为，存量数据默认值** |
| `signature` | 对 body 做 HMAC | 新的推荐默认值 |
| `hmac_query` | 时间戳 + 签名放 URL query | 钉钉，第三期引入 |

#### 1.3 重试与去重语义（文档化，不改行为）

现有实现：`MaxAttempts` 次尝试，指数退避带抖动，队列容量 1024，满则丢弃并计入 `Stats.Dropped`；`DedupCooldown` 窗口内相同 `dedup_key` 只投一次。这些都要写进契约文档，明确「至少一次投递」语义，接收端必须幂等。

#### 1.4 文档

新增 `docs/alert-webhook-contract.md`：payload schema、字段语义、事件类型清单、签名验证步骤、重试与去重语义、示例 receiver（Go / Python / Node 各 20 行）。

#### 数据模型改动

```go
type AlertWebhook struct {
    // …现有字段…
    PayloadFormat FormatID  `json:"payload_format"` // 空值读作 generic
    AuthMode      AuthMode  `json:"auth_mode"`      // 空值读作 header
}
```

零值即现有行为，存量记录无需迁移。

#### 凭据 audience 的影响

当前 audience 为 `webhook:<header_name>`，绑定在 URL + header 上。引入 `auth_mode` 后改为：

```
webhook:<auth_mode>:<header_name>     // header 模式
webhook:signature                     // signature 模式，无 header
```

并且 §5 已实现的「更换目的地必须重填密钥」规则要扩展：**URL、header_name、auth_mode 三者任一变化，都必须重新输入密钥**。理由一致——控制台从不回显已存密钥，不能把操作者从未见过的明文重新绑定到新的目标或新的传输方式。

#### 测试

- 签名基串对固定输入的黄金值测试。
- 时间戳超窗被拒。
- 同一 delivery id 在重试中保持不变。
- `auth_mode` 变更时拒绝复用密钥（扩展现有的 `TestAdminAlertWebhookRefusesToReuseSecretForANewDestination`）。
- payload 含 `schema` 字段的契约测试。

---

### 第二期：企业内网可达性

不解决这一期，B 类客户走不到格式问题。

#### 2.1 私网开关文档化

`configs/config.example.yaml` 补上，并说明风险：

```yaml
security:
  # 允许告警 Webhook 指向私网地址（RFC1918 / ULA / 回环除外）。
  # 企业内部接收端需要打开；打开后请确保只有可信管理员能创建 Webhook。
  allow_private_webhooks: false
```

注意：`validateAddress` 目前对 unspecified、loopback、multicast、link-local 是**无条件拒绝**，`AllowPrivate` 只放开 RFC1918 / ULA。这个区分要写进文档——有些客户的接收端跑在 `127.0.0.1`，需要明确告诉他们这条路不开放以及为什么（元数据服务与本机服务的 SSRF 面）。

#### 2.2 私有 CA 支持

```yaml
alerts:
  # 附加信任锚，用于内部 CA 签发证书的接收端。PEM 文件，可含多张证书。
  # 追加到系统信任库，不替换。
  ca_bundle_file: ""
```

`safetransport.Options` 增加 `RootCAs *x509.CertPool`，构造时 `x509.SystemCertPool()` 克隆后 `AppendCertsFromPEM`。

**明确不做**：`InsecureSkipVerify`。任何形式的「跳过证书校验」开关都不提供——它会把所有出站防护变成摆设，而且一旦提供，客户在排障时必然会打开并永久留着。

#### 2.3 mTLS（可选，视需求）

部分企业接收端要求客户端证书。若需要，`alerts.client_cert_file` / `client_key_file`。建议先不做，等出现真实需求。

---

### 第三期：内置适配器（服务 A 类）

首批建议：`cloudevents`、`slack`、`teams`、`discord`、`feishu`、`dingtalk`、`wecom`。PagerDuty / Opsgenie 属于事件管理平台而非聊天工具，字段映射（dedup key、severity 分级、resolve 语义）复杂度高一档，建议单独排期。

`cloudevents` 值得优先做：很多企业消息总线（Knative、Argo、各家 iPaaS）直接吃 `application/cloudevents+json`，一个适配器就能覆盖一批 B 类客户，且是公开标准而非某厂商私有格式。

每个适配器必须同时提供 `Render`、`Classify`、`AuthModes`。以飞书为例：

```go
func (feishuFormatter) Classify(status int, body []byte) Outcome {
    if status < 200 || status >= 300 {
        return Outcome{Reason: "http_client_error", Detail: snippet(body)}
    }
    var reply struct {
        Code int    `json:"code"`
        Msg  string `json:"msg"`
    }
    // 解析失败按成功处理：不能因为对方改了回包结构就把成功投递判成失败。
    if err := json.Unmarshal(body, &reply); err == nil && reply.Code != 0 {
        return Outcome{Reason: "rejected_by_endpoint", Detail: snippet(body)}
    }
    return Outcome{Success: true, Reason: "delivered"}
}
```

这样「HTTP 200 但对方拒收」会被正确判为失败、进入审计的 `reason_code`、并触发重试，而不是像现在这样静默成功。

#### 控制台改动

创建/编辑表单加「接收平台」下拉，选中后**动态调整下方字段**：

| 选择 | 显示的字段 |
| --- | --- |
| 通用 / CloudEvents | 认证方式（签名 / 请求头 / 无）、密钥 |
| Slack / Teams / Discord | 仅 URL（URL 即凭据），隐藏密钥与 header |
| 飞书 | 密钥（可选，用于加签）、关键词提示 |
| 钉钉 | 加签密钥（`hmac_query`），隐藏 header 选择 |

同时在表单里给出该平台的注意事项链接（如飞书关键词白名单、钉钉 IP 白名单）——这些约束是投递失败的高频原因，事后再查很浪费时间。

#### 适配器漂移风险

厂商会改 API。缓解措施：每个适配器的 `Render` 输出用黄金文件测试锁定；文档标注各适配器对应的厂商 API 版本与最后验证日期；`Classify` 对无法解析的回包一律按成功处理，避免厂商改回包结构时把正常投递判成失败。

---

### 第四期：受限模板（服务 C 类）

仅在前三期都上线、且出现真实的「接收端改不动」需求时才做。

```yaml
payload_format: template
template: |
  {"alert":"{{ .Summary }}","level":"{{ .Severity }}","at":"{{ .Timestamp }}"}
```

约束，缺一不可：

1. **字段白名单**。模板可见的不是 `alert.Event` 本身，而是一个显式定义的 `TemplateContext` 结构体，只含 `ID / Type / Severity / Summary / Timestamp / DedupKey / ProjectID`。`Details` 不暴露——它是自由 map，可能承载我们没打算外发的内容。
2. **`text/template` + 无函数**。不注册任何自定义函数，尤其不能有任何能发起 IO 的函数。
3. **输出必须是合法 JSON**，渲染后校验，失败则投递判失败并写审计。
4. **大小上限**（如 64 KiB），渲染结果超限即失败。
5. **成功判定固定为 2xx**，不允许配置。
6. 模板变更进审计链，`target_type: alert_webhook`，`metadata` 记录模板哈希。

模板是这套设计里唯一的用户可控执行面，上述约束不能因为「用起来不方便」而放宽。

---

## 5. 兼容性与迁移

| 项 | 处理 |
| --- | --- |
| 存量 `AlertWebhook` 记录 | `PayloadFormat` 空值读作 `generic`，`AuthMode` 空值读作 `header`。行为与今天完全一致 |
| 存量凭据 audience | `webhook:<header>` 继续有效；只有当用户主动改 `auth_mode` 时才重新封装，且必须重填密钥 |
| payload 加 `schema` 字段 | 加字段属于向后兼容变更，现有接收端忽略未知字段即可 |
| API | `alertWebhookInput` / `alertWebhookView` 增加两个可选字段，旧客户端不传即取默认值 |
| 配置 | 两个新配置项都有安全的默认值（`""` / `false`） |

无需数据迁移脚本。

---

## 6. 实施顺序与理由

```
第一期  payload 契约（schema + 签名 + delivery id + 文档）
   │    ── 覆盖 B 类，投入最小，且不依赖任何其他改动
   ▼
第二期  内网可达性（allow_private_webhooks 文档化 + 私有 CA）
   │    ── B 类客户的硬阻断，不修则第一期在企业内网用不上
   ▼
第三期  内置适配器（含 Classify，修掉「200 但被拒」静默成功）
   │    ── 覆盖 A 类
   ▼
第四期  受限模板
        ── 覆盖 C 类，按真实需求触发
```

第一、二期是「让客户能自己解决问题」，第三期是「我们替客户解决」。前者的杠杆更高，所以排在前面——即使第三期永远不做，第一二期上线后 B 类客户已经可用。

---

## 7. 被否决的方案

### 可加载的第三方代码模块（Go plugin / WASM / 子进程）

否决理由不是技术难度，是与产品定位互斥：

- 模块能拿到解密后的 webhook 密钥。
- 模块能发起任意出站请求，`safetransport` 的 pinned dial、拒私网、不跟随重定向、忽略代理环境变量这一整套防护会被绕开。
- 单二进制、无外部依赖、HMAC 链式审计的可信基会被一个动态加载面破坏。

需要扩展性时，正确的出口是**签名过的标准 payload + 客户自己的 receiver**（第一期），而不是把外部代码拉进我们的进程。第四期的受限模板是这条线上唯一的让步，且已被约束到不含代码执行。

### 中继/转发服务（「让客户自己部署一个适配服务」）

会增加一次部署与一跳网络，与「单二进制、本地控制」的定位冲突。第一期的契约化本质上把这件事变成可选：客户想加中继随时可以，但不加也能用。

### 无条件的 `InsecureSkipVerify`

见 §2.2。一旦提供，排障时必然被打开并永久留着。

---

## 8. 开放问题

1. **首批适配器清单**需要产品侧确认。全球化优先级下，Slack / Teams 应该排在飞书 / 钉钉之前，但现有客户构成可能相反。
2. **PagerDuty / Opsgenie 是否纳入**。它们需要 severity 分级映射与 resolve 事件语义，工作量约等于其余适配器之和。
3. **`auth_mode` 的新建默认值**。推荐 `signature`，但会让「随便找个 URL 试一下」的上手路径多一步。备选：新建默认 `none`，在表单里显著提示未签名。
4. **签名密钥与 header 密钥是否共用同一个 Credential**。当前倾向共用（都是 `webhookCredentialType`，audience 区分用途）。若要支持同时启用 header 与签名，则需要两个凭据。
5. **飞书 / 钉钉的关键词与 IP 白名单约束**是否需要在控制台做主动校验，还是仅在文档与表单提示中说明。
