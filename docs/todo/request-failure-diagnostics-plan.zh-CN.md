# 请求失败诊断与错误专用日志：设计与实施方案

- 状态：提案待评审，尚未实施
- 日期：2026-09-01
- 文档语言：中文
- 适用范围：Gateway 请求生命周期、Usage 派生数据、Admin API、控制台「用量与调用」、进程日志
- 数据迁移：第一阶段不需要；后续若持久化 Provider 错误标识，需要升级 Ledger / Usage 派生结构

## 一、问题与结论

控制台已经能报告某个时间范围内有多少请求最终失败，但「2 条最终失败」只是一项聚合数字。
运维看不见失败发生在哪个 Provider / Deployment、属于认证、限流、超时还是请求参数问题，也拿不到
可交给上游支持团队的 Provider Request ID。其结果是系统知道请求失败，使用者仍只能靠猜。

结论：**可以增加错误日志，但不应从零再造一套保存原始请求和响应的日志系统。** Halro 已经有三块
可复用基础：

1. 进程日志支持级别、JSON / text、stderr / file / both、文件轮转和统一脱敏；
2. Provider 尝试失败已经生成包含 Request ID、路由目标和错误分类的结构化记录；
3. Usage 的 Attempt 数据已经持久化 `error_class` 和 `http_status`，Admin API 也已返回它们。

真正的缺口是：

- Provider 尝试失败写在 `WARN`，所以把日志级别设为 `error` 会把它过滤掉；
- 尝试失败和最终请求失败没有被明确区分，回退成功的请求不能算最终错误；
- Usage 控制台把已有错误字段丢在响应里，只显示「错误」两个字；
- Provider Code / Provider Request ID 目前只存在于进程日志，Usage 历史里没有；
- 缺少一条与「最终失败」口径一一对应的结构化 `ERROR` 事件。

推荐分两层解决：

- **控制台错误详情以 Ledger → Usage 派生数据为事实来源**，保证能分页、筛选、回看历史，并与汇总数字
  使用同一口径；
- **错误专用文件以结构化进程日志为运维出口**，只收最终请求失败和系统级 `ERROR`，不让控制台反向读取
  主机日志文件。

## 二、当前实现证据

| 能力 | 当前实现 | 结论 |
| --- | --- | --- |
| 日志配置 | `internal/config/config.go` 的 `Logging` 支持 level / format / output / file / max size / max files | 文件、JSON、轮转已经具备，不需重写 |
| 日志输出 | `internal/logging/logger.go` 通过一个 LevelVar 和同一 Handler 写 stderr / file | 当前不能让普通日志和错误专用文件使用不同级别 |
| 脱敏 | `internal/safelog` 在 Handler 前统一处理消息和属性 | 所有新增日志必须继续经过这一层 |
| Provider 失败 | `internal/gateway/service.go:logProviderFailure` 记录 request / deployment / provider / binding / class / status / code | 信息基本够用，但事件级别是 `WARN` |
| Provider 原文 | 上述路径刻意不记录上游响应正文 | 正确的安全边界，方案不得推翻 |
| 尝试历史 | `internal/usage/aggregate.go:AttemptEvent` 已有 `ErrorClass`、`HTTPStatus`、重试和回退计数 | 第一阶段可以无迁移直接展示 |
| 最终请求 | `RequestSummary` 只有 `Outcome`，没有终态错误描述 | 需要由请求和尝试关联得到终态失败详情 |
| 明细 API | `GET /admin/api/v1/usage` 已支持 `status=error` 和时间、项目、Provider、Deployment 等筛选 | 当前 `status` 是尝试级，不是最终请求级 |
| 明细 UI | `web/src/pages/UsagePage.tsx` 的状态列只显示成功 / 错误 | 已有字段没有呈现 |
| Dashboard | 近期异常已经显示 `error_class` 并按 Request ID 跳转 Usage | 可复用交互，但不能替代长周期错误历史 |

## 三、术语与统计口径

本方案固定以下口径，避免页面、API 和日志各自解释「失败」。

### 3.1 尝试失败（attempt failure）

一次真实 Provider 调用没有成功。它可能触发重试或切换到下一目标；后续尝试成功时，整个请求仍然成功。

示例：目标 A 超时，自动回退到目标 B 成功。结果是 1 次失败尝试、1 次成功尝试、0 条最终失败。

### 3.2 最终请求失败（final request failure）

`RequestFinalized.outcome != success`。它与 Usage 汇总里的 `request_errors` 使用同一事实，截图中的
「2 条最终失败」指的就是这个口径。

### 3.3 系统错误（system error）

不一定对应某次 Gateway 请求，例如审计投递失败、运行时激活失败、日志文件不可写、恢复路径异常。
这类错误仍进入错误专用文件，但不进入 Usage 的请求失败列表，也不改变 `request_errors`。

### 3.4 必须保持的不变量

1. `最终失败数 = 终态为非 success 的请求数`；不能拿失败尝试数代替。
2. 一个请求最多产生一条 `request failed` 终态日志。
3. 回退后成功的请求可以保留 `WARN` 尝试记录，但不得产生终态 `ERROR`。
4. 控制台不通过扫描日志文件计算数字，日志丢失或轮转不影响 Usage 报表。
5. 日志和 Usage 都不保存 Prompt、响应正文、凭据或原始 Header。

## 四、目标与非目标

### 4.1 目标

1. 从「最终失败」数字一键进入对应失败请求列表；
2. 运维不离开控制台即可判断常见失败类别并定位 Provider / Deployment；
3. 每个最终失败请求生成一条安全、结构化、可按 Request ID 检索的 `ERROR` 记录；
4. 可选地将全部 `ERROR` 单独写入有大小上限和代数上限的错误文件；
5. 现有历史记录至少能显示错误分类和 HTTP 状态；
6. 失败日志不改变请求响应、重试、回退、熔断、计费和审计语义。

### 4.2 非目标

- 不记录原始请求、Prompt、响应或 Provider 错误正文；
- 不建设全文检索、日志采集平台或分布式 Trace；
- 不把进程日志变成账务或 Usage 的第二真相源；
- 不改变哪些错误可重试、可回退或计入熔断；
- 不承诺仅凭错误分类就能解释每个 Provider 的全部业务语义；
- 不在本方案中增加告警通知，错误告警仍归现有 Alerts 体系。

## 五、设计决策

### D1 · 控制台的数据来源

**采用：Ledger → Usage 派生数据。**

原因：错误详情需要历史查询、分页、时间范围和资源筛选，而这些能力已经属于 Usage。读取主机日志会引入
文件权限、多实例、轮转边界、容器无本地盘和非结构化解析问题，并让日志文件错误地成为产品数据源。

### D2 · `ERROR` 代表什么

**采用：仅在请求最终失败时写请求级 `ERROR`；每次 Provider 尝试失败继续写 `WARN`。**

不能把 `logProviderFailure` 直接从 `Warn` 改成 `Error`，否则回退成功也会污染错误日志，错误文件条数与
控制台最终失败数无法解释。

### D3 · 是否记录 Provider 原始错误正文

**不记录。** 只记录由适配器拆出的安全标识：错误分类、状态码、受长度和字符集约束的 Provider Code、
Provider Request ID。上游错误正文最可能回显凭据、输入片段或外部 URL，通用正则脱敏无法证明安全。

### D4 · 是否新增独立错误文件

**采用可选的独立错误 Sink，不复用全局最低日志级别。**

`logging.level: error` 虽然能得到一个只含错误的主日志，却会同时丢掉证书临期、探针失败、回退尝试等有价值
的 `WARN`。独立 Sink 允许普通 stderr 继续保持 `info` / `warn`，错误文件固定只接收 `ERROR`。

建议配置形状：

```yaml
logging:
  level: info
  format: json
  output: stderr
  error_file:
    enabled: true
    file: ""          # 默认：<data-dir>/logs/halro-error.log
    max_size_mb: 32
    max_files: 10
```

错误文件固定使用 JSON；这是机器检索和故障工单最稳定的格式。`file` 不得与普通 `logging.file` 指向同一
路径，配置校验必须拒绝这种情况。

### D5 · 历史兼容

第一阶段只展示已存在的 `error_class` / `http_status`，无需迁移。第二阶段增加 Provider Code / Request ID
时，旧记录允许字段为空，界面显示「历史记录未保存该字段」，不得伪造成 `unknown`。

## 六、数据设计

### 6.1 安全失败描述

在 Gateway 内引入内部值对象 `FailureDescriptor`。它不是上游错误正文的包装，而是白名单字段集合：

```text
phase                 pre_provider | provider | response_render | accounting | client
error_class           authentication | rate_limit | timeout | provider_5xx |
                      bad_request | connect | malformed_response | canceled | unknown
gateway_status        返回给调用方的 HTTP 状态（若已确定）
provider_status       上游 HTTP 状态（若存在）
provider_code         经 SafeProviderIdentifier 限制的上游错误标识
provider_request_id   经长度与字符集限制的上游请求标识
error_type            未分类 Go 错误的类型名；不包含 Error() 文本
retryable             是否可重试
ambiguous             上游是否可能已经接受请求
```

规则：

- 有 Provider HTTP 状态时不保存 Provider `Message` / `Cause`；
- 没有 Provider HTTP 状态且确认错误由 Halro 自己产生时，进程日志可以保留现有安全原因文本；
- Usage 持久层不保存自由文本原因，只保存枚举或受约束标识；
- `provider_code` 与 `provider_request_id` 设置长度上限，拒绝换行、控制字符和看起来像自然语言的长文本；
- `request_id`、资源 ID 和模型名沿用现有长度与脱敏规则。

### 6.2 请求生命周期

`requestRun` 保存本次请求的终态失败描述和最后一次失败目标。流程如下：

```text
请求受理
  → 0..N 次 Provider 尝试
      → 失败：写 WARN 尝试记录，保存安全 FailureDescriptor
      → 成功：请求可能最终成功
  → RequestFinalized
      → success：不写 request failed
      → 非 success：恰好写一条 ERROR request failed
```

必须让所有出口汇入同一终态函数，包括 Provider 全部失败、响应转换失败、计费结算失败、准入后的本地错误、
超时和客户端取消。已有 `requestRun.finalized` 保证结算一次；终态日志应使用同一一次性边界，而不是散落在
各 handler 的 return 分支。

### 6.3 Usage 字段

切片 1 不改存储，直接使用：

- `AttemptEvent.ErrorClass`
- `AttemptEvent.HTTPStatus`
- `AttemptEvent.RetryCount`
- `AttemptEvent.FallbackCount`
- `RequestSummary.Outcome`

切片 2 再评审是否把以下安全标识加入 Ledger 的 Attempt Settled 事件，并由 Usage / Parquet 派生：

- `ProviderCode`
- `ProviderRequestID`
- `FailurePhase`

若实施切片 2：

1. Ledger 解码必须向后兼容字段缺失；
2. Usage checkpoint 版本和 Parquet schema 版本按现有升级规则递增；
3. 重建 Usage 后字段必须与增量路径一致；
4. 旧数据不回填臆测值；
5. 不需要重新初始化数据目录。

## 七、日志设计

### 7.1 最终请求失败事件

事件名固定为 `request failed`，建议输出：

```json
{
  "time": "2026-09-01T08:15:30Z",
  "level": "ERROR",
  "msg": "request failed",
  "request_id": "req_...",
  "outcome": "provider_error",
  "phase": "provider",
  "error_class": "authentication",
  "gateway_status": 502,
  "provider_status": 401,
  "provider_code": "invalid_api_key",
  "provider_request_id": "upstream-request-...",
  "public_model": "chat",
  "deployment_id": "deployment_...",
  "provider_id": "provider_...",
  "binding_id": "binding_...",
  "attempts": 2,
  "fallbacks": 1,
  "latency_ms": 1260,
  "accounting_recorded": true
}
```

字段不存在时省略，不写空字符串。`accounting_recorded` 用于区分「请求本身失败且已落账」与「连终态账务记录
都没能提交」；后者必须另有系统级错误记录，不能假装 Usage 一定能查到。

### 7.2 尝试失败事件

保留现有 `provider attempt failed` 的 `WARN` 语义和字段。它负责解释重试 / 回退链，不负责代表最终请求结果。
当普通日志最低级别为 `warn` 或更低时可见；错误专用文件不接收它。

### 7.3 错误专用 Sink

实现为 Handler 分流，而不是在各调用点直接写第二个文件：

- 主 Handler：遵守 `logging.level`、`format`、`output`；
- 错误 Handler：固定最低 `ERROR`、固定 JSON、写独立轮转 Sink；
- 两个分支都必须位于 `safelog` 脱敏之后；
- 错误文件创建权限 0600、目录 0700，沿用现有 `logging.Sink`；
- 写错误文件失败时回退 stderr，并只报告一次退化原因；
- SIGHUP 可重新打开两个文件；路径、格式和是否启用仍需重启，级别仍可热更新。

## 八、Admin API 与控制台

### 8.1 第一阶段：复用现有接口

`GET /admin/api/v1/usage?status=error` 继续表示失败尝试。前端补齐 `UsageAttempt.http_status` 类型并展示：

- 错误分类；
- HTTP 状态；
- Request ID；
- Provider / Deployment / 实际模型；
- 重试和回退信息。

这是最小改动，但不能直接回答「哪些请求最终失败」。

### 8.2 第二阶段：最终失败查询

推荐新增：

```text
GET /admin/api/v1/usage/failures
```

查询参数沿用 Usage：`cursor`、`limit`、`start`、`end`、`project_id`、`provider_id`、
`deployment_id`、`model`、`provider_model`。结果一行代表一个最终失败请求，而不是一次尝试：

```json
{
  "items": [{
    "request_id": "req_...",
    "outcome": "provider_error",
    "accepted_at": "...",
    "completed_at": "...",
    "attempts": 2,
    "fallbacks": 1,
    "last_failure": {
      "error_class": "authentication",
      "provider_status": 401,
      "provider_code": "invalid_api_key",
      "deployment_id": "deployment_...",
      "provider_id": "provider_..."
    }
  }],
  "next_cursor": "..."
}
```

该接口从 `RequestSummary.Outcome != success` 选请求，再按 Request ID 关联它的 Attempts。详情继续复用
`GET /admin/api/v1/usage/requests/{requestID}`，避免列表重复返回完整失败链。

### 8.3 页面交互

1. 「{{count}} 条最终失败」变为链接；
2. 链接携带汇总的绝对 `start/end`，进入「调用明细」的「最终失败」视图；
3. 默认一行一个失败请求，显示时间、分类、资源上下文、尝试数；
4. 展开后按顺序展示全部尝试，并标出重试 / 回退；
5. Request ID、Deployment ID、Provider Request ID 可复制；
6. Deployment 跳转部署页，Request ID 可回到完整请求详情；
7. 历史数据缺少 Provider Code 时明确显示「该历史版本未保存 Provider 错误码」。

### 8.4 中文诊断建议

错误分类的解释由前端本地化字典提供，不把建议文本写进 Ledger：

| error_class | 界面说明 | 建议动作 |
| --- | --- | --- |
| `authentication` | Provider 认证或权限拒绝 | 检查凭据状态、账号权限、Region / Project 归属 |
| `rate_limit` | Provider 限流或容量不足 | 检查配额、并发、Retry-After 与备用目标 |
| `timeout` | 上游响应超时 | 检查超时配置、Provider 状态和网络延迟 |
| `connect` | 无法建立安全连接 | 检查 DNS、TLS、代理、出口规则和 Endpoint allowlist |
| `provider_5xx` | Provider 服务端异常 | 用 Provider Request ID 联系上游，检查是否持续发生 |
| `bad_request` | 上游拒绝请求形状或参数 | 查看 Provider Code 指向的字段和模型能力 |
| `malformed_response` | 返回内容不符合适配器契约 | 检查 Provider 兼容面、模型与协议 Profile |
| `canceled` | 调用方取消或连接断开 | 确认是否为客户端超时；不要自动归因 Provider |
| `unknown` | 无法安全分类 | 使用 Request ID 查询日志并检查适配器分类缺口 |

## 九、安全、隐私与容量边界

### 9.1 永不记录

- Prompt、messages、工具参数、文件内容、图片 / 音频 / 视频；
- Provider 响应正文、模型输出、流式片段；
- Authorization、Cookie、API Key、Gateway Key、Admin Session；
- 完整 Headers、URL query、未经约束的错误字符串；
- 原始客户端 IP。

### 9.2 允许记录

- Halro 生成的 Request / Attempt / Project / Route / Deployment / Provider / Binding ID；
- 公开模型名和 Provider 模型标识；
- 枚举型错误分类、HTTP 状态、重试 / 回退 / 延迟计数；
- 通过安全标识校验的 Provider Code 和 Provider Request ID；
- 已确认由 Halro 自身生成、且不含请求内容的内部原因。

### 9.3 容量控制

- 错误文件必须按大小轮转并限制代数；
- 一次请求最多一条终态错误；
- 不把完整尝试链序列化到终态日志，链条通过 Request ID 在 Usage 查看；
- Provider Code / Request ID 设置严格长度上限；
- UI 接口继续使用游标和 1–100 的页大小限制。

## 十、实施切片

### S0 · 口径测试先行

- 为成功、单次失败、失败后回退成功、全部目标失败、响应转换失败、客户端取消建立表格测试；
- 断言每种场景的 attempts、request_errors、WARN 数和最终 ERROR 数；
- 固定「回退成功不产生最终 ERROR」这一核心契约。

### S1 · 控制台展示现有错误字段

- 补齐 `UsageAttempt.http_status`、retry / fallback 类型；
- 错误状态单元格显示本地化错误分类和 HTTP 状态；
- 增加可展开的尝试详情；
- 「最终失败」卡片先跳转时间范围内的失败尝试，并明确标签是「失败尝试」；
- 不改 Ledger、checkpoint 或 Parquet。

### S2 · 最终失败列表

- 新增 Usage 最终失败查询及 Admin API；
- 汇总卡片准确跳转最终失败列表；
- 列表按 Request ID 聚合，详情展示尝试链；
- 断言列表数量与同范围 `request_errors` 相等。

### S3 · 请求级终态 ERROR

- 引入安全 `FailureDescriptor`；
- 在统一终态边界写一次 `request failed`；
- 保持尝试失败为 WARN；
- 覆盖 Provider 前、本地转换、Provider、计费和客户端取消路径；
- 增加秘密 canary、Provider 正文和未分类错误文本不泄漏测试。

### S4 · 独立错误文件

- 增加 `logging.error_file` 配置、默认路径与校验；
- 复用轮转 Sink，增加 Handler 分流；
- 支持 SIGHUP reopen；
- 文件写失败回退 stderr；
- 更新 Operator Guide 和配置参考。

### S5 · 可选的 Provider 安全标识持久化

- 评审 Ledger 事件兼容性和字段上限；
- 增加 Provider Code / Request ID / Failure Phase；
- 升级 Usage checkpoint / Parquet schema；
- 验证增量、重放、重建和导出一致；
- 旧数据字段缺失保持可解释。

S1、S2、S3 是解决当前问题的主体；S4 在确实需要单独主机文件时实施；S5 只有在长期历史必须保留上游工单
标识时才实施，不能为了界面丰富就扩大 Ledger 数据面。

## 十一、验证计划

遵循仓库「运行改动能够影响的检查」策略。

### 11.1 Gateway

- 扩展 `internal/gateway/provider_failure_log_test.go`；
- 新增请求终态日志测试，覆盖一次性、分类、状态、回退成功和全部失败；
- 对涉及并发请求生命周期的改动，对受影响 gateway package 运行 `-race`；
- 断言 Provider 原始正文、Prompt 和 secret canary 不进入任何 Handler。

### 11.2 Usage / Admin API

- `internal/usage/query_test.go`：最终失败选择、游标、时间边界和 Request / Attempt 关联；
- `internal/app/admin_usage_test.go` 或对应新增测试：筛选、权限、错误参数和返回契约；
- 若改持久格式，补 checkpoint restore、Ledger replay、Parquet round-trip 和重建一致性。

### 11.3 Frontend

- `web/src/pages/UsagePage.test.tsx`：错误字段、空字段、跳转、展开和复制；
- `web/src/pages/UsageSummaryPanel.test.tsx`：最终失败链接携带正确绝对区间；
- TypeScript typecheck；
- 若只改 CSS，再运行 `src/design-system.test.ts` 和聚焦视觉检查；
- 最终推送前按仓库规则运行一次完整 frontend gate，并重建 / 校验嵌入 bundle。

### 11.4 日志 Sink

- ERROR 只进入错误文件，WARN 不进入；
- 主日志继续遵守原 level；
- 轮转代数、权限、oversized record、写失败回退和 reopen；
- 普通文件与错误文件同路径时配置拒绝启动；
- JSON 每行可独立解码。

## 十二、验收标准

以下条件全部满足才算完成：

1. 点击汇总页「2 条最终失败」后，目标列表恰好有 2 个最终失败请求；
2. 失败后回退成功的请求不出现在最终失败列表，也不产生 `request failed` ERROR；
3. 每个最终失败请求能看到 Request ID、错误分类、相关 Provider / Deployment 和全部尝试顺序；
4. 有安全 Provider Code / Request ID 时可见并可复制，没有时不伪造；
5. 开启错误文件后，其中没有 INFO / WARN；
6. 关闭错误文件不改变请求行为和普通日志；
7. Prompt、响应、Provider 原始错误正文和所有凭据 canary 均未出现在日志、Usage、API 和浏览器产物；
8. 错误文件达到大小上限后按配置轮转，写失败时 stderr 有且仅有一次退化通知；
9. 旧数据目录能够直接升级，第一阶段无需重建；实施 S5 时重放与增量结果一致；
10. 文档明确说明「最终失败」与「失败尝试」不是同一个数字。

## 十三、运维过渡方案

在 S3 / S4 实施前，如需立即保留现有 Provider 尝试失败，可使用：

```yaml
logging:
  level: warn
  format: json
  output: both
  file: ""
  max_size_mb: 64
  max_files: 5
```

再按 `msg = "provider attempt failed"`、`request_id`、`deployment_id` 和 `error_class` 检索。这个配置会同时
保留其他 WARN，所以它只是过渡手段，不等于本方案定义的错误专用日志；并且它只能记录启用之后发生的事件。

## 十四、实施顺序建议

推荐顺序：**S0 → S1 → S2 → S3 → S4**，S5 独立评审。

这个顺序先解决「界面已有数据却不展示」的直接问题，再固定最终失败口径，最后增加文件出口。它避免为了一个
可见性问题先修改持久格式，也避免错误文件先上线后才发现里面记录的是失败尝试而不是最终失败。
