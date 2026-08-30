# 多目标别名今天保证什么

一个公共模型别名可以挂多条模型路由。多于一条时，网关会在这些目标之间自动切换。
这份文档写的是**当前代码的实际行为**，用于指导配置——它不是提案，也不描述计划中的改动。
计划中的改动在[路由自动切换：设计变更提案](../prd/route-auto-failover.zh-CN.md)。

一句话结论：

> 多目标别名的自动切换**只在 OpenAI 兼容的 Chat、流式 Chat、Embeddings 三条路径上生效**，
> 且**只对可证明未产生上游计费的失败**换目标。凭据失效、请求被拒、上游 5xx
> 都不会换目标。Phase 2 资源类操作在多目标别名上**直接返回 409**。

下面是这句话的每一部分的依据。

## 一、按协议面的覆盖

| 协议面 | 入口 | 多目标行为 |
|---|---|---|
| Chat Completions（含 Responses、可移植 Anthropic Messages） | `generate()` `internal/gateway/service.go:975` | **逐目标回退**，双层循环 `:1030` |
| 流式 Chat（含 ResponsesStream、可移植 MessagesStream） | `generateStream()` `service.go:1968` | **逐目标回退**，`:2031`；首字节吐出后停止 |
| Embeddings | `service.go:2207` | **逐目标回退** |
| Anthropic **原生** Messages / MessagesStream / CountTokens | `prepareNativeMessages()` `service.go:1688` | **无回退**，固定取 `targets[0]` |
| images / speech / transcriptions / moderations / rerank / batches | `inferenceResourcesTarget()` `internal/gateway/inference_resources.go:22` | **候选数 ≠ 1 时返回 409 `ambiguous_resource_route`** |
| files 创建 | `inference_resources_store.go:217` | 同上，409 |
| async invoke | `inference_resources_store.go:1113` | 同上，409 |

原生 Messages 不回退是有意的——它走 profile 固定的热路径，代码注释写明「so it cannot
accidentally inherit portable fallback behavior」。

**这条直接决定配置边界**：一个别名一旦挂上第二条启用路由，它就**不能再用于 Phase 2
操作**。今天能正常调用图片、音频、批处理、文件的别名，加一条兜底路由之后会开始返回 409。

## 二、哪些失败会换目标

判定在 `retryable()`（`internal/gateway/service.go:2332`）：非 `*provider.Error` 一律不换；
`Ambiguous` 直接短路为否；其余取 `Retryable || ErrorMalformed || ErrorProvider5xx`。

| 主目标的失败 | 换下一个目标？ | 依据 |
|---|---|---|
| 503 Service Unavailable | **是** | `Retryable: true`。适配器注释：「Nothing ran, so a fallback deployment can serve it」 |
| 429 限流、408 超时 | **是** | `Retryable: true` |
| 可证明未发出的连接/DNS/拨号错误 | **是** | `provider.Unsent(err)` ⇒ `Ambiguous: false` |
| 响应无法解析（`ErrorMalformed`） | **是** | 在 `retryable` 白名单内 |
| 断路器打开 | **是**，且**不消耗尝试预算** | `startAttempt` 在计数前返回 |
| 部署/服务商并发额度耗尽 | **是**，且**不消耗尝试预算** | 同上 |
| **401 / 403 凭据失效** | **否**，直接终止 | `ErrorAuthentication`，`Retryable` 未置位 |
| **400 及其他 4xx** | **否**，直接终止 | `ErrorBadRequest` |
| **500 / 502 / 504** | **否**，直接终止 | `Ambiguous: true`——请求已到达上游，可能已经产生计费 |
| 传输错误但无法证明未发出 | **否** | `Ambiguous: !provider.Unsent(err)` |
| 输出被脱敏策略拒绝 | **否**，422 | 策略属于项目，换目标不改变结论 |
| 日预算耗尽 | **否**，403 | `budget.ErrExceeded` 在 `startAttempt` 处终止整条链 |
| Token Guard 拒绝 | **否**，403 | `startAttempt` 返回致命错误 |
| 计价不可用 / 定价隔离 | **否**，409 或 503 | 同上 |

5xx 不回退是**账务保守性的刻意选择**，不是缺陷：一个 500 可能是生成进行到一半才抛出的，
502/504 来自边缘而源站可能仍在运行并计费。重发会重复这次生成，按免费结算会隐藏这笔开销。

**运维要记住的一条**：「主目标的密钥过期了，自动切到备用服务商」——**这个场景今天不成立**。
401 直接终止，后面的目标一次都不会试。恢复靠主动探活把该部署移出候选集（默认探活间隔
30 秒），而探活覆盖不了「只有特定请求形状会被拒」的 400。

## 三、尝试预算怎么算

两个配置项共同封顶：

```yaml
gateway:
  max_total_attempts: 3        # 整个请求的尝试总数上限
retry:
  max_attempts_per_target: 2   # 单个目标最多重试几次
```

循环结构是 `for target { for targetTry }`，`attemptCount++` 在 `startAttempt` **成功之后**。
所以：

- **最坏情况**下（每次尝试都真正发出并返回可重试错误），保证可达的目标数是
  `⌈max_total_attempts / max_attempts_per_target⌉`。默认值下是 **2**：目标一用掉 2 次，
  目标二用掉第 3 次，目标三不会被尝试。
- 但被断路器或并发闸门挡住的目标**消耗 0 次预算**，此时更靠后的目标仍可达。
  `TestOpenCircuitSkipsFailedTarget`（`internal/gateway/service_test.go:623`）就是这个情形：
  `MaxAttempts: 3, MaxAttemptsPerTarget: 2`，主目标断路器打开，备目标拿到 2 次调用。

所以 `⌈⌉` 是**保证可达数的下界，不是上界**。配三级回退时，第三级「不保证被尝试」，
不是「一定不会被尝试」。

`gateway.max_total_attempts` 是**启动期设置**，控制台只读。改它要编辑 `config.yaml`
并重启进程——在单写者/单副本约束下，这是一次数据面中断，不是一次热更新。省略该键不会
默认成 3，而是启动校验失败（`gateway.max_total_attempts must be at least 1`）。

## 四、候选集是怎么算出来的

「这个别名有几个目标」不是一个静态数字，它随请求形状变化。一条路由要成为候选，必须
连续通过：

1. **路由已启用**且未删除 —— 否则根本不注册进 Registry（`internal/app/providers.go:464`）。
2. **部署已启用**、未删除、能力校验通过 —— 否则注册时被扣留（`providers.go:475`、`:489`）。
3. **策略与同别名的其他启用路由一致** —— 指 `ordered` 与 `round_robin` 不能并存；空策略
   两侧都归一为 `ordered`（`provider.go:539-541`），不构成不一致。真正混合时 Registry
   拒绝注册该目标，它会被记为 `Dangling` 并写日志；路由列表把这类路由显示为**已扣留**
   并给出原因（`/admin/api/v1/routes` 的 `withheld` 字段），不再显示成 Enabled。
4. **探活未失败** —— `probed && !probe.Healthy` 的目标在候选解析阶段被剔除
   （`internal/provider/provider.go:645`）。注意「从未探活」不会被剔除。
5. **支持该操作** —— `filterByOperation`（`provider.go:663`），按 chat / streaming /
   embeddings / images … 粒度过滤，并校验能力证据等级。
6. **满足细粒度能力** —— 视觉、工具、结构化输出、profile 兼容性由
   `filterSemanticCapabilities`、`filterGenerateProfileCompatibility`、
   `filterPrimitiveTargets`（`service.go:2622-2650`）在发起前过滤。
7. **token 上限装得下** —— `filterTokenCapabilities`（`service.go:2652`）。

结论：一个「3 个目标」的别名，对一个带图片的请求可能只有 1 个有效候选；对一个探活失败
两个部署的时刻只有 1 个。**判断冗余度时要按请求形状看，不能只看路由条数。**

## 五、多目标会改变的三个准入行为

这三个闸门在候选集**整体**上工作，加一个目标就会改变别名上**每一个**请求的行为——
包括那些从来不会用到新目标的请求。

**1. Token Guard 按候选集的最高价准入。**
`captureTokenGuardPricingView`（`service.go:2439`）遍历全部候选取 `maximumCost`，作为本次
请求的预估成本喂给 Token Guard；`RecheckCost` 只上调、不下调，`Complete` 也不按实际
成本回冲。所以给 `chat` 加一个贵的兜底目标之后，**每一个** `chat` 请求都按最贵单价消耗
`cost_per_minute` 窗口，哪怕它全程由最便宜的目标服务。配了成本维度的项目会在比原先低
得多的吞吐上开始返回 `token_guard_blocked`。

**2. 任一候选缺少价格版本 ⇒ 整个别名 409。**
同一个函数里，任何候选取不到覆盖的价格版本就走 `unknownPricePolicyEvidence`
（`service.go:2506`），除非实例策略是 `allow_without_cost_governance` **且**项目日预算为 0
**且**项目没有 Token Guard 成本维度。对任何有日预算的项目，**加一条没配价（或定价被隔离）
的兜底路由，会让该别名的全部请求 409**，包括健康主目标本来能服务的那些。加兜底目标在
这种情况下降低可用性。

**3. 日预算耗尽会终止整条链，不会去试更便宜的候选。**
预留金额按目标定价（`accountingTermsFromSnapshot`），但三条循环都在
`errors.Is(err, budget.ErrExceeded)` 时直接返回 403。代码注释给的理由是「日预算属于项目
不属于目标，所以换候选不改变答案」——这个前提在候选价格不同时不成立。预算边缘会出现
「便宜目标本可服务，却收到 `budget_exceeded`」。叠加 `round_robin` 的轮转偏移，同一秒的
两个相同请求可能得到不同答案。

## 六、授权语义

数据面授权只有一处，`internal/gateway/service.go:231`：

```go
if !slices.Contains(principal.Project.AllowedModels, model) { … 403 … }
```

项目绑定的是**别名**，`Project` 上没有路由、部署、服务商或凭据维度。因此：

> **项目被授予一个别名，就等于被授予该别名下现在和将来的全部目标**——包括它从未被单独
> 授权过的凭据、服务商和区域。

这一点在删除方向上有护栏而创建方向上没有：`validateAliasKeepsServingProjects`
（`internal/app/admin_providers.go:1728`）阻止删掉某个项目仍在使用的最后一条路由，而
`validateAdminRoute`（`:1562`）在创建路由时**完全不查询项目**——它只校验部署/服务商可用、
profile 模型合法、同别名启用路由策略一致。

实际后果：今天 `chat-aws` / `chat-deepseek` 这种一别名一服务商的配法，事实上就是按服务商
授权；一旦合并成 `chat`，运维手里就只剩全有或全无。日后任何人给 `chat` 加一条路由，都会
静默把所有已授权项目扩展到新的凭据与数据驻留姿态，没有确认、没有与受影响项目关联的审计。

另外，**同一别名下两条已启用路由指向同一个部署现在会被拒绝**（`validateAdminRoute`）。
这样的别名会显示成两个目标，实际是一个部署、一个凭据、一个故障域；断路器按路由 ID 分键，
也不会把它们合并。禁用状态的重复路由仍可保存——那是维护状态，而把它启用会走同一条校验
并被拒绝。

## 七、可观测

每一次尝试都独立记账，落到账本与用量：

- `internal/ledger/event.go` 的 `deployment_id`、`route_id`、`provider_id`、`provider_model`、
  `attempt_number`、`retry_count`、`fallback_count`
- `internal/usage/parquet.go` 的同名列，**每次尝试一行**

所以一条回退链的每一跳都可以事后重建。但**控制台的「用量与调用」看不到它**：表格的模型
列渲染的是 `requested_model`（别名），`deployment_id` 只出现在链接的 href 里
（`web/src/pages/UsagePage.tsx:127`）。对「同一个上游模型的多个部署」这种最安全的冗余配法，
两个目标在页面上完全无法区分。

`halro_fallbacks_total`（无标签）统计的是每请求 `FallbackCount` 最大值之和，即「多用了几个
目标」，不是「有多少请求发生了回退」。告警 `HalroFallbackSaturation`
（`deploy/observability/prometheus/alert-rules.yml:88`）在 5 分钟回退比 > 0.25 且 10 分钟
请求数 ≥ 20 时触发。

**注意**：在全是单目标别名的实例上，`FallbackCount` 恒为 0，这条告警结构性地永不触发。
第一次配置多目标别名，等于第一次武装这条告警。

## 八、怎么正确验证回退

**禁用主路由、或让主部署探活失败——这两种做法都验证不了回退。** 它们都只是把目标从候选
集里拿掉，请求由剩下的唯一目标服务，重试循环一行没跑。看到「换了个部署」是候选集构建的
结果，不是回退的结果。

要真正验证运行时回退，主目标必须**保持启用且探活健康**，同时返回一个可重发的错误：

1. 让主目标的上游返回 503（临时把它的连接指向一个恒返 503 的端点，或用一个已知会限流的
   凭据触发 429）。
2. 发一次请求，确认返回 200。
3. 查这次请求的账本/用量记录，应看到**两条尝试**：第一条 `deployment_id` 是主目标、
   状态失败，第二条是备目标、状态成功，`fallback_count` = 1。
4. 确认客户端拿到的 `model` 仍然是别名。

第 3 步目前需要直接查 Parquet 或账本，控制台不显示 `deployment_id`。

## 九、什么时候用多目标，什么时候不用

**适合**：同一个上游模型的多个部署（多区域、多凭据、多配额池），价格相同或接近，
只用 Chat / 流式 / Embeddings，主要防的是限流与 503。

**不适合**：
- 该别名要用于 images / audio / batches / files / async —— 会 409。
- 该别名要走 Anthropic 原生 Messages —— 不回退，多配的目标是死重量。
- 候选之间价格差距大 —— Token Guard 按最高价准入，预算边缘行为不确定。
- 主要想防的是凭据失效或上游 5xx —— 这两类今天不回退。
- 需要按项目限制可触达的服务商/区域 —— 别名授权是全有或全无。

在这些情况下，保留一别名一目标的配法，让应用显式选择，是当前代码下更诚实的配置。
