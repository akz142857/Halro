# 请求失败诊断与错误专用日志：设计与实施方案

- 状态：**S0–S5 全部已实施**（2026-09-02）。以下正文保留为设计记录，与实现的两处偏差已就地订正：
  非 success 终态是六种而不是两种（3.2），`accounting_error` 也写 ERROR（3.3）。
  S5 原本标注为「需独立评审」，实施时按要求一并完成，代价是 Ledger 数据面增加了三个受约束字段
  （`provider_code`、`provider_request_id`、`failure_phase`），长度上限在 Ledger 的 `Validate` 处再次强制。
- 日期：2026-09-01（2026-09-02 依代码核对修订：补入 `rejected` 终态、准入前失败盲区与历史深度限定；
  实施 S0 时再次核对，把非 success 终态从「两种」订正为实际的六种，见 3.2）
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
- 缺少一条结构化 `ERROR` 事件来标记请求真的失败了。

评审补充的三点（详见第三章与 D1）：

- **「最终失败」不是一个同质的集合。** 非 success 终态今天有六种（3.2 逐一列出），其中四种没有任何
  上游调用可以解释它 —— `rejected` 由 `requestRun.close()` 的兜底路径写入，覆盖预算耗尽 / 熔断打开 /
  并发已满。任何按「Provider 失败」形状设计的产出都会在这类行上落空。
- **认证失败、路由未找到、限流拒绝根本不在这套口径里。** 它们在 `beginRequestRun` 之前返回，没有
  Ledger 请求。这是刻意的边界，但必须说出来 —— 否则「最终失败」会被读成「全部失败」。
- **失败历史的深度等于 Usage Aggregate 的保留窗口**，不等于 Parquet 的长期历史：Parquet 只导出
  Attempt 分区，没有请求级行。

推荐分两层解决：

- **控制台错误详情以 Ledger → Usage 派生数据为事实来源**，在 Aggregate 保留窗口内能分页、筛选、回看，
  并与汇总数字使用同一口径；
- **错误专用文件以结构化进程日志为运维出口**，只收 `provider_error` / `accounting_error` 终态和系统级
  `ERROR`，不让控制台反向读取主机日志文件。

## 二、当前实现证据

| 能力 | 当前实现 | 结论 |
| --- | --- | --- |
| 日志配置 | `internal/config/config.go` 的 `Logging` 支持 level / format / output / file / max size / max files | 文件、JSON、轮转已经具备，不需重写 |
| 日志输出 | `internal/logging/logger.go` 通过一个 LevelVar 和同一 Handler 写 stderr / file | 当前不能让普通日志和错误专用文件使用不同级别 |
| 脱敏 | `internal/safelog` 在 Handler 前统一处理消息和属性 | 所有新增日志必须继续经过这一层 |
| 写入成本 | 实测（Apple M4）：16 字段 ERROR 经 safelog + JSON 写单个轮转文件约 7µs / 6 次分配，分流到第二个文件约 8.4µs；低于门槛的记录约 4ns、零分配 | 按失败计数可忽略；但 `Sink.Write` 持进程级锁且每条一次 `write(2)`，故不得放到成功路径（D2） |
| Provider 失败 | `internal/gateway/service.go:logProviderFailure` 记录 request / deployment / provider / binding / class / status / code | 信息基本够用，但事件级别是 `WARN` |
| Provider 原文 | 上述路径刻意不记录上游响应正文 | 正确的安全边界，方案不得推翻 |
| 尝试历史 | `internal/usage/aggregate.go:AttemptEvent` 已有 `ErrorClass`、`HTTPStatus`、重试和回退计数 | 第一阶段可以无迁移直接展示 |
| 最终请求 | `RequestSummary` 只有 `Outcome`，没有终态错误描述 | 需要由请求和尝试关联得到终态失败详情 |
| 非 success 终态 | `service.go` 的 `requestRun.close()` 对未 finalize 的运行兜底写 `rejected`；`rollup.go` 把 `outcome != "success"` 一律计入 `RequestErrors` | 「最终失败」已经混着策略拒绝，方案必须分开定义（3.3） |
| 准入顺序 | `beginRequestRun` 在 Token Guard、限流、Request ID 和 `BeginRequestDetailed` 之后才建 `requestRun` | 认证 / 路由 / 限流失败没有 Ledger 请求，是已知盲区（3.4） |
| 请求级历史 | `RequestSummary` 只在内存 Aggregate 与 checkpoint 中；`internal/usage/parquet.go` 的 `publishPartition` 只导出 attempts | 最终失败列表的深度 = Aggregate 窗口，不是 Parquet 长期历史（D1） |
| 日志 Controls | `logging.Controls` 只持有单个 `*Sink`，`ReopenFile` / `Close` 按单文件写成 | 双出口要改 Controls，不只是加一个 Handler（S4） |
| 取消分类 | `logProviderFailure` 对未分类的 `context.Canceled` / `DeadlineExceeded` 写字面量 `client_disconnected_or_timed_out` | 该值不在 `provider.ErrorClass` 中，字典与归一都要处理（8.4） |
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

它不是一个同质的集合。**评审时说它只有两种终态，那是错的** —— 按 `internal/gateway` 里所有
`run.finalize(...)` 与 `attempt.abort(...)` 的实参逐一读出，今天能写进 Ledger 的非 success 终态有六种，
本方案的每一处产出都必须分别回答：

| outcome | 含义 | 是否调用过上游 |
| --- | --- | --- |
| `provider_error` | 上游失败，或上游答复无法安全呈现给调用方 | 是 |
| `policy_rejected` | 脱敏策略拒绝了上游输出，或改写后的输出无法表示 | 是 |
| `unsupported_feature` | 已开尝试，但目标不支持请求形状，`abort` 回滚 | 否 |
| `token_guard_rejected` | `RecheckCost` 用真实定价复核后超限 | 否 |
| `accounting_error` | 定价、预留、价格钉或 MarkStarted 失败 | 否 |
| `rejected` | `requestRun.close()` 的兜底：预算耗尽、熔断打开、目标并发已满 | 否 |

分界线不是「上游是否失败」，而是**它是不是一次值得看的系统异常**（见 D2 与 3.3）。

### 3.3 准入后拒绝 —— 不产生 ERROR 的那四种终态

`rejected`、`token_guard_rejected`、`unsupported_feature`、`policy_rejected` 都属于 3.2，都计入
`request_errors`，但都**没有一次失败的上游调用可以解释它们**（`policy_rejected` 调用过上游，但上游是
成功的，失败的是本地策略）。它们没有 Provider Request ID，`FailureDescriptor.phase` 只能是
`pre_provider` 或 `response_render`。所以它们不能默认套用 Provider 失败的形状，需要两条显式规则：

1. **这四种终态不写 `request failed` ERROR。** 它们是策略正常生效的结果，不是系统异常；一个跑飞的客户端
   可以在几秒内产生上万条，写进错误文件会直接击穿第九章的容量边界，并让错误文件不再等于「值得看的
   失败」。这类拒绝由既有的限流 / 熔断指标和 Usage 报表负责解释。
   `accounting_error` 是例外，它写 ERROR：那是 3.5 意义上的系统错误（Ledger 或定价不可用），既不由客户端
   驱动、也无法用速率放大，正是错误文件该留下的东西。
2. **最终失败列表必须收录它们，并标注为「策略拒绝」**，`last_failure` 允许缺失。列表不收录它们，列表数量
   就会与汇总卡片的数字对不上，而那正是第十二章第一条验收标准要守的东西。

由此得到一个必须写进文档和 Operator Guide 的推论：**ERROR 条数 ⊆ 最终失败数，两者不相等**，不能互相
校验。

### 3.4 准入前失败（pre-admission failure）—— 本方案的已知盲区

`internal/gateway/service.go` 的 `beginRequestRun` 是在 Token Guard、限流、Request ID 生成和
`BeginRequestDetailed` 全部通过之后才创建 `requestRun` 的。在它之前返回的失败**没有 Ledger 请求**，
因此不产生 `RequestFinalized`、不计入 `request_errors`、不出现在最终失败列表、也不产生 `request failed`
ERROR。

它至少包含：Gateway Key 认证失败、`model_not_found`、`unsupported_feature`、无健康部署的
`provider_unavailable`、RPM / TPM / 并发拒绝、Token Guard 拒绝、Request ID 生成失败、accounting 不可用。

这是刻意的边界而不是疏漏：这些请求从未占用预算、从未选定目标，把它们塞进以 Ledger 为事实来源的请求
历史，等于让计费真相源为未受理的流量建档。但它同时意味着**运维最常遇到的 401 与 404 不在这套诊断
里**，所以界面和文档必须主动说明，否则「最终失败」会被读成「全部失败」。这类失败的可见性归属 HTTP 层
指标、Alerts 与审计；若它成为真实痛点，应作为独立方案评审，而不是在本方案里扩张 Usage 的数据面。

### 3.5 系统错误（system error）

不一定对应某次 Gateway 请求，例如审计投递失败、运行时激活失败、日志文件不可写、恢复路径异常。
这类错误仍进入错误专用文件，但不进入 Usage 的请求失败列表，也不改变 `request_errors`。

### 3.6 必须保持的不变量

1. `最终失败数 = 终态为非 success 的请求数`，其中包含 3.2 表里全部六种；不能拿失败尝试数代替，也不能
   只算 Provider 失败。
2. 一个请求最多产生一条 `request failed` 终态日志，且只有 `provider_error` 与 `accounting_error` 会产生它。
3. 回退后成功的请求可以保留 `WARN` 尝试记录，但不得产生终态 `ERROR`。
4. `request failed` 的条数少于或等于最终失败数；两者不得被当作可互相校验的同一口径。
5. 准入前失败不进入任何以 `RequestFinalized` 为来源的计数或列表。
6. 控制台不通过扫描日志文件计算数字，日志丢失或轮转不影响 Usage 报表。
7. 日志和 Usage 都不保存 Prompt、响应正文、凭据或原始 Header。

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
- 不在本方案中增加告警通知，错误告警仍归现有 Alerts 体系；
- 不把准入前失败（认证、路由未找到、限流、Token Guard 拒绝）纳入最终失败列表或错误日志，见 3.4；
- 不为 3.3 那四种策略终态产生 `ERROR` 日志；
- 不为最终失败提供超出 Usage Aggregate 保留窗口的长期归档，见 D1。

## 五、设计决策

### D1 · 控制台的数据来源

**采用：Ledger → Usage 派生数据。**

原因：错误详情需要历史查询、分页、时间范围和资源筛选，而这些能力已经属于 Usage。读取主机日志会引入
文件权限、多实例、轮转边界、容器无本地盘和非结构化解析问题，并让日志文件错误地成为产品数据源。

> **2026-09-02 订正**：本节说「可见窗口等于 Aggregate 保留窗口」，当时的隐含前提是 Aggregate
> 有一个保留窗口。它没有——`attempts` 与 `summaries` 只 append、从不裁剪，可见窗口实际上是无限的，
> 而 `usage.retention_days` 只管 Parquet。窗口是 `docs/todo/data-retention-plan.zh-CN.md` 加上去的
> （`usage.console_window_days`）。下面这段在那之后才成立。

**但要限定「历史」有多长。** `RequestSummary` 今天只存在于内存 Aggregate（`internal/usage/aggregate.go`）
中，随 Usage checkpoint 一起持久化；Parquet 导出只写 Attempt 分区（`internal/usage/parquet.go` 的
`publishPartition` 只接收 attempts，没有请求级行）。因此最终失败列表能回看的窗口等于 Aggregate 保留的
窗口，**不等于 Parquet 的长期历史**。

对第一、二阶段这是够用的：它与现有「调用明细」页面的可见范围完全一致，操作者不会遇到「同一页面两种
时间深度」的困惑。但方案不得承诺超出该窗口的失败历史。若将来确实需要长期失败归档，必须把请求级行加入
导出格式并升级 Parquet schema —— 那是比 S2 大得多的改动，与 S5 同级，需要独立评审。

### D2 · `ERROR` 代表什么

**采用：仅在请求以 `provider_error` 或 `accounting_error` 终结时写请求级 `ERROR`；每次 Provider 尝试失败
继续写 `WARN`；`rejected` / `token_guard_rejected` / `unsupported_feature` / `policy_rejected` 终结的请求
不写 `ERROR`。**

不能把 `logProviderFailure` 直接从 `Warn` 改成 `Error`，否则回退成功也会污染错误日志。

也不能反过来把「非 success 就写 ERROR」当作等价说法。非 success 包含 3.3 那四种策略终态，它们是策略正常
生效的结果，可以在几秒内成千上万条。两种写法在成功路径上完全一致，差别只有在一个跑飞的客户端出现时
才暴露 —— 那时错误文件已经被写满了。

代价是错误文件条数与控制台最终失败数不再相等。这个代价是接受的：一个只收系统异常的错误文件是有价值
的，一个和 Usage 报表数字对得上但被 429 淹没的错误文件没有。差值必须被文档解释，不能被当作缺陷修掉。

**这条界线也有成本上的理由，不只是语义上的。** 在 Apple M4 上实测本方案 7.1 节那条 16 字段记录，
经 `safelog` 脱敏后写入一个轮转文件约 7µs、6 次分配；再分流到第二个错误文件约 8.4µs。对一个已经花掉
一次上游往返（LLM 常见 100ms 起）的失败请求，这是 0.01% 量级，完全不必权衡 —— 前提是它**按失败计数**。
按非 success 计数就不同了：限流和熔断的产出速率与客户端重试速率同阶，那时付出这份成本的不再是每秒
几条失败，而是每秒上万条拒绝。

同一组实测还给出一条更硬的约束：`logging.Sink.Write` 持进程级互斥锁，且每条记录一次 `write(2)`，没有
缓冲。低于门槛的记录几乎免费（约 4ns、零分配，因为 `safelog` 的 `Enabled` 直接委托下游、在 `Handle`
之前短路），但**写出来的每一条都会串行化**。这就是本方案不在成功路径上放任何日志的理由：不是记录本身
贵，而是它贵在一个所有请求共享的锁上。

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
请求受理（beginRequestRun 成功之后；在它之前失败的请求不在本图内，见 3.4）
  → 0..N 次 Provider 尝试
      → 失败：写 WARN 尝试记录，保存安全 FailureDescriptor
      → 成功：请求可能最终成功
  → RequestFinalized
      → success：不写 request failed
      → provider_error / accounting_error：恰好写一条 ERROR request failed
      → rejected / token_guard_rejected / unsupported_feature / policy_rejected：
        不写 ERROR，只进入最终失败列表并标注为策略拒绝（见 3.3）
```

必须让所有出口汇入同一终态函数，包括 Provider 全部失败、响应转换失败、计费结算失败、准入后的本地错误、
超时和客户端取消。已有 `requestRun.finalized` 保证结算一次；终态日志应使用同一一次性边界，而不是散落在
各 handler 的 return 分支。

`requestRun.close()` 的兜底 `finalize("rejected")` 也走这条边界，所以把日志挂在 `finalize` 上天然覆盖了
它 —— 正因如此，`finalize` 必须按 outcome 分支，而不是「非 success 就写 ERROR」。这是本方案里最容易被
写错的一行：两种写法在成功路径上表现相同，差别只在一个跑飞的客户端出现时才暴露。

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
  "latency_millis": 1260,
  "accounting_recorded": true
}
```

延迟字段叫 `latency_millis`，与 `AttemptEvent.LatencyMillis` 和 Usage API 返回的 `latency_millis` 同名。
同一个量在日志和 API 里用两个名字，会让按 Request ID 把日志和明细页对起来的人多做一次翻译。

字段不存在时省略，不写空字符串。`accounting_recorded` 用于区分「请求本身失败且已落账」与「连终态账务记录
都没能提交」；后者必须另有系统级错误记录，不能假装 Usage 一定能查到。

### 7.2 尝试失败事件

保留现有 `provider attempt failed` 的 `WARN` 语义和字段。它负责解释重试 / 回退链，不负责代表最终请求结果。
当普通日志最低级别为 `warn` 或更低时可见；错误专用文件不接收它。

### 7.3 错误专用 Sink

实现为 Handler 分流，而不是在各调用点直接写第二个文件：

- 主 Handler：遵守 `logging.level`、`format`、`output`；
- 错误 Handler：固定最低 `ERROR`、固定 JSON、写独立轮转 Sink；
- 分流点位于 `safelog` **之内**：`safelog.New(fanout{主 Handler, 错误 Handler})`。不能在外面并列两个
  已脱敏 logger 让调用点二选一 —— 那会让「是否脱敏」取决于调用点选对了哪一个，而脱敏不是一项设置；
- `logging.Controls` 需要一并改造。它今天只持有单个 `*Sink`，`HasFile`、`ReopenFile` 和 `Close` 都按
  单文件写成（`internal/logging/logger.go`）；双出口要求它持有两个 Sink，并让两者的 reopen 与 close
  都被覆盖。这不是「加一个 Handler」那么小的改动面；
- 错误文件创建权限 0600、目录 0700，沿用现有 `logging.Sink`；
- 写错误文件失败时回退 stderr，并只报告一次退化原因；
- SIGHUP 可重新打开两个文件；路径、格式和是否启用仍需重启，级别仍可热更新
  （错误 Handler 的下限固定为 `ERROR`，不随 `logging.level` 热更新而变）。

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

`outcome` 属于 3.3 那四种策略终态的行**没有 `last_failure`**（见 3.3）：它没有失败的上游调用，因此没有
`error_class`、`provider_status` 或 Provider 上下文可以填。接口必须省略该字段，界面按策略拒绝渲染，
不得为了列对齐而伪造一个空的 Provider 上下文 —— 那会把「没有调用上游」误报成「上游没有回答」。

### 8.3 页面交互

1. 「{{count}} 条最终失败」变为链接；
2. 链接携带汇总的绝对 `start/end`，进入「调用明细」的「最终失败」视图；
3. 默认一行一个失败请求，显示时间、分类、资源上下文、尝试数；
4. 展开后按顺序展示全部尝试，并标出重试 / 回退；
5. Request ID、Deployment ID、Provider Request ID 可复制；
6. Deployment 跳转部署页，Request ID 可回到完整请求详情；
7. 历史数据缺少 Provider Code 时明确显示「该历史版本未保存 Provider 错误码」；
8. `rejected` 行显示为「策略拒绝」并给出拒绝原因（预算 / 熔断 / 并发），不显示 Provider 错误分类，
   展开后没有尝试链 —— 因为确实没有；
9. 列表页顶部说明两件事：可见窗口等于 Usage 保留窗口（D1），以及认证失败、路由未找到、限流拒绝
   不在此列表内并指向查看它们的去处（3.4）。这不是脚注，是这个页面最容易被误读的地方。

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
| `client_disconnected_or_timed_out` | 调用方断开或超时，且错误未经适配器分类 | 同 `canceled`；出现即说明分类缺口 |
| `unknown` | 无法安全分类 | 使用 Request ID 查询日志并检查适配器分类缺口 |

倒数第二行不是 `provider.ErrorClass` 的成员：`logProviderFailure` 在错误不是 `*provider.Error` 而是裸的
`context.Canceled` / `context.DeadlineExceeded` 时，直接写了字面量 `client_disconnected_or_timed_out`
（`internal/gateway/service.go`）。字典漏掉它，界面就会对一类真实存在的记录显示原始英文。

S3 应当顺手把这个分支归一到枚举里 —— 取消归 `canceled`、超时归 `timeout` —— 让「日志里的 `error_class`
一定是 `provider.ErrorClass` 的成员」成为可断言的契约。归一之后本行仍需保留在字典中，因为归一之前写下的
日志不会被改写。

`rejected` 的请求在列表里不显示上表任何一行，而显示策略拒绝的原因（预算、熔断、并发），它来自既有的
限流 / 熔断状态，不是 Provider 错误分类。

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
- 3.3 的四种策略终态不写 ERROR。这是本章最重要的一条容量控制：限流和熔断在事故中的产出速率与
  客户端重试速率同阶，把它写进错误文件会让轮转在几分钟内吃掉全部代数，事故真正的第一条错误因此被
  挤出文件 —— 恰好在最需要它的时刻；
- 不把完整尝试链序列化到终态日志，链条通过 Request ID 在 Usage 查看；
- Provider Code / Request ID 设置严格长度上限；
- UI 接口继续使用游标和 1–100 的页大小限制。

## 十、实施切片

### S0 · 口径测试先行

- 为成功、单次失败、失败后回退成功、全部目标失败、响应转换失败、客户端取消、准入后拒绝
  （熔断打开 / 预算耗尽）建立表格测试；
- 断言每种场景的 attempts、request_errors、WARN 数和最终 ERROR 数；
- 固定两条核心契约：「回退成功不产生最终 ERROR」，以及「`rejected` 计入 request_errors 但不产生 ERROR」；
- 同时断言一条准入前失败（例如认证失败）既不产生 `RequestFinalized` 也不改变 `request_errors`，
  把 3.4 的盲区变成被测试钉住的事实，而不是一句说明。

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
- 列表收录全部六种非 success 终态，3.3 那四种标注为策略拒绝且不带 `last_failure`；
- 断言列表数量与同范围 `request_errors` 相等 —— 这条断言只有在收录 `rejected` 时才成立，它就是
  3.3 那条规则的回归测试；
- 页面注明可见窗口等于 Usage 保留窗口（D1），不暗示这是全量历史。

### S3 · 请求级终态 ERROR

- 引入安全 `FailureDescriptor`；
- 在统一终态边界写一次 `request failed`，且**按 outcome 分支**：只有 `provider_error` 与
  `accounting_error` 写 ERROR；
- 保持尝试失败为 WARN；
- 把 `client_disconnected_or_timed_out` 归一到 `canceled` / `timeout`，并断言日志里的 `error_class`
  一定是 `provider.ErrorClass` 的成员；
- 覆盖 Provider 前、本地转换、Provider、计费和客户端取消路径；
- 断言 3.3 那四种策略终态产生零条 ERROR；
- 增加秘密 canary、Provider 正文和未分类错误文本不泄漏测试。

### S4 · 独立错误文件

- 增加 `logging.error_file` 配置、默认路径与校验；
- 复用轮转 Sink，增加 Handler 分流；
- 改造 `logging.Controls`：它今天只持有单个 `*Sink`（`internal/logging/logger.go`），`ReopenFile` 与
  `Close` 都按单文件写成，双出口需要它持有两个 Sink，并让两者的 reopen / close 都被覆盖 —— 只写
  「Handler 分流」会低估这个切片的改动面；
- 分流点必须在 `safelog.New` 之内，即 `safelog.New(fanout{主 Handler, 错误 Handler})`，不能在外面并列
  两个 logger，否则脱敏是否生效取决于调用点选了哪一个；
- 支持 SIGHUP reopen 两个文件；
- 文件写失败回退 stderr；
- 更新 Operator Guide 和配置参考，写明 ERROR 条数不等于最终失败数（3.3）。

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
- 新增请求终态日志测试，覆盖一次性、分类、状态、回退成功、全部失败和 `rejected` 零 ERROR；
- 对涉及并发请求生命周期的改动，对受影响 gateway package 运行 `-race`；
- 断言 Provider 原始正文、Prompt 和 secret canary 不进入任何 Handler。

### 11.2 Usage / Admin API

- `internal/usage/query_test.go`：最终失败选择、游标、时间边界和 Request / Attempt 关联，
  以及 `rejected` 行在没有 Attempt 时仍被选中；
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

1. 点击汇总页「2 条最终失败」后，目标列表恰好有 2 个最终失败请求，其中包含该范围内的策略拒绝；
2. 失败后回退成功的请求不出现在最终失败列表，也不产生 `request failed` ERROR；
3. 每个最终失败请求能看到 Request ID、错误分类、相关 Provider / Deployment 和全部尝试顺序；
4. 有安全 Provider Code / Request ID 时可见并可复制，没有时不伪造；
5. 开启错误文件后，其中没有 INFO / WARN；
6. 关闭错误文件不改变请求行为和普通日志；
7. Prompt、响应、Provider 原始错误正文和所有凭据 canary 均未出现在日志、Usage、API 和浏览器产物；
8. 错误文件达到大小上限后按配置轮转，写失败时 stderr 有且仅有一次退化通知；
9. 旧数据目录能够直接升级，第一阶段无需重建；实施 S5 时重放与增量结果一致；
10. 文档明确说明「最终失败」与「失败尝试」不是同一个数字；
11. 3.3 的四种策略终态出现在最终失败列表中并标注为策略拒绝，但产生零条 `request failed` ERROR，
    因此错误文件的行数少于列表行数，且这一差值在文档中被解释而不是被当作缺陷；
12. 界面与文档说明准入前失败（认证、路由未找到、限流、Token Guard 拒绝）不在这套口径内，
    并指出应去哪里查它们；
13. 界面说明最终失败列表的可见窗口等于 Usage 保留窗口，不承诺长期归档。

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

它还有两处覆盖不到：`rejected` 的请求没有 Provider 尝试，因此不会出现在这条检索里（要看它们只能用
Usage 汇总的 `request_errors`）；准入前失败同样没有（3.4）。过渡期内这两类的可见性没有变化。

## 十四、实施顺序建议

推荐顺序：**S0 → S1 → S2 → S3 → S4**，S5 独立评审。

这个顺序先解决「界面已有数据却不展示」的直接问题，再固定最终失败口径，最后增加文件出口。它避免为了一个
可见性问题先修改持久格式，也避免错误文件先上线后才发现里面记录的是失败尝试而不是最终失败。

S0 之所以排在最前，在评审后有了第二个理由：`rejected` 与准入前失败这两类边界（3.3、3.4）都无法从
「最终失败」这个名字推出来，只能从代码读出来。先把它们写成断言，后面三个切片才不会各自解释一遍
「失败」是什么。
