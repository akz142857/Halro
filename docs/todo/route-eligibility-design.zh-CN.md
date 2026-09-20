# 路由准入：把熔断、探针与额度合成一个门

- 状态：Proposed（2026-09-20）。**未实现**——本文的 `internal/routegate`、scope 挂起、
  per-reason 时窗、`routing` 配置块在当前代码里都不存在
- 适用范围：Gateway 的候选解析与回退路径；不改协议门面、不改账务语义
- 目标版本：不绑定。进入条件是 §8 阶段 1 的取证结果，不是某个 tag
- 追踪：[#318](https://github.com/akz142857/Halro/issues/318) Epic、[#319](https://github.com/akz142857/Halro/issues/319) 阶段 1、[#320](https://github.com/akz142857/Halro/issues/320) 阶段 2、[#321](https://github.com/akz142857/Halro/issues/321) 阶段 3
- 由来：一次"上游额度用尽时系统怎么自动切换"的问题。答案是**大多数情况下不切换**，
  而缺口的成因是结构性的（§1）
- 本文引用的全部代码事实：见[附录 A](#附录-a核实过的事实索引)

---

## 1. 这是什么

### 1.1 现状：三个门，三套词汇，三个 key

决定"这个 target 现在能不能用"的地方有三个，互不知道对方存在：

| 位置 | key | 词汇 | 覆盖的失败 |
|---|---|---|---|
| `resolveCandidatesLocked`（`internal/provider/provider.go:763-765`） | `DeploymentID` | `Healthy bool` + `ErrorClass` | 主动探针看得见的 |
| `startAttempt`（`internal/gateway/service.go:570`） | `Target.ID` | 连续失败计数 | connect / timeout / 5xx / malformed |
| — | — | — | **额度、订阅、凭据** |

第三行是空的。这不是遗漏了一个特性，是前两行的分法本身不完整：它们按**Halro 观察到的症状**分类，
而没有一栏对应"上游好好的，只是不给这套凭据用"。

### 1.2 后果：额度用尽在三层都留不下痕迹

**熔断器不记。** `reportBreaker`（`service.go:890-896`）把错误交给
`availabilityFailure`（`service.go:2625-2638`），后者只认 `ErrorConnect`、`ErrorTimeout`、
`ErrorProvider5xx`、`ErrorMalformed`，其余一律返回 `nil`。熔断器拿到 `nil` 就是 `Done(nil, …)`
——那是**成功**，会清零失败计数。所以 429 风暴不但不开闸，还在主动维持闸门关闭。

**探针不记。** `probeDeployments`（`internal/app/health.go:32-95`）默认 30 s 一次，OpenAI 侧的
`Probe` 是 `GET /v1/models`（`internal/provider/openai/adapter.go:373-392`）。**列模型不消耗额度**，
余额为 0 的账号照样 200 → `Healthy: true`。探针能发现死 key（401）和死主机，发现不了额度用尽。

**registry 不记。** 候选筛选只看 `Priority` 与 probe 健康，没有"最近被拒"的状态。

净效果：**一个额度用尽的上游，每一个请求都会先撞它一次**，而且它在优先级里的位置一点都没变。

### 1.3 后果：一半的"不能用"根本不触发回退

回退循环（`service.go:1271-1380`）的分叉点只有一行（`service.go:1370-1378`）：

```go
lastErr = providerErr
if !retryable(providerErr) {
    run.finalize("provider_error")
    return ..., terminalProviderError(providerErr)   // 不试下一个 target
}
```

`retryable()`（`service.go:2612-2623`）= `!Ambiguous && (Retryable || Malformed || Provider5xx)`。
对上今天各 adapter 的分类：

| 上游情况 | HTTP | Class | 回退？ |
|---|---|---|---|
| 限流 | 429 | rate_limit | 会 |
| 明确拒收 | 503 | provider5xx | 会 |
| 其它 5xx | 500/502/504 | provider5xx（`Ambiguous`） | **不会**（可能已计费，不重放） |
| 凭据失效 / 订阅停用 | 401/403 | authentication | **不会** |
| 余额 / 额度用尽 | 402 | bad_request | **不会** |
| 额度用尽，而上游用 429 表达 | 429 | rate_limit | 会 |
| Kimi Code 402（已特例，**本表唯一有代码依据的厂商行为**） | 402 | unknown | 会 |

> **"订阅用尽能不能自动切换"完全取决于上游选了哪个状态码。** 用 429 表达的会切，用 402 或
> 401 表达的整个请求当场失败。这不是一个可以靠调参数绕开的行为。
>
> **哪一家用哪个状态码，本文不作断言**——除 Kimi Code 外，仓库里没有任何厂商级的取证，
> 而这正是 §7 第 1 条与阶段 1 要解决的事。

### 1.4 顺带的一个配置陷阱

`gateway.max_total_attempts: 3` 与 `retry.max_attempts_per_target: 2`（`internal/config/default.yaml:168,211`）
组合后：

```
target[0] try0 → count=1
target[0] try1 → count=2
target[1] try0 → count=3
target[1] try1 → 3 < 3 为假，内层退出 → 外层 break
```

**第三个候选永远试不到。** 配三个上游做跨平台回退时，第三个是摆设。本文不靠改这两个数字解决
§1.2/§1.3，但阶段 1 要把默认值订正过来（§8）。

### 1.5 非目标

- 不改 `Retryable` / `Ambiguous` 的含义。它们是**执行与计费语义**，由 adapter 声明，本文一个字
  不动（`internal/gateway/failure.go:165-172` 的立场保留）；
- 不做跨 Project 的配额调度。Halro 自己的预算与 Token Guard 是 per-Project 的另一根轴，
  上游额度是 per-credential 的，两者不得混在一个机制里；
- 不预测配额重置时刻。"OpenAI 每月 1 号重置"这类知识不该硬编码在 Halro 里（§4.4）；
- 不做上游幂等键。仓库既有立场不变。

---

## 2. 四个决定性判断

### 2.1 作用域由失败自己声明，不是固定的 key

不同失败的真实影响半径不同：

| 失败 | 真实作用域 |
|---|---|
| 连接失败 / 超时 / 5xx | provider instance（端点 + region） |
| 429 rate limit | credential × model，有时 credential |
| 额度用尽 | **credential**（余额）或 credential × model（配额） |
| 订阅停用 / 凭据失效 | **credential** |
| capability 拒绝 | deployment |

今天熔断器一律按 `Target.ID`——最窄的那个。后果是一个现实的 bug 类：**一把被吊销的 key 挂着 5 个
deployment，要被独立发现 5 次**，探针也要 30 s 后分 5 次标记。

所以判定单元是一组 scope，不是一个 ID：

```
deployment:<id>
credential:<id>
credential:<id>/model:<provider_model>
provider:<id>
```

target 可用 ⟺ 它所属的每个 scope 都没被挂起。第一个 401 挂起 credential，5 个 deployment
立刻全部退出候选。

**方向性原则：搞错窄方向只是少救一点，搞错宽方向会误伤**——一个模型的配额用尽不该让整个
credential 下线。**默认取最窄的 scope，只在有证据时放宽**（§7 第 2 条）。

前置改动：`Target`（`provider.go:480-522`）今天**没有** `CredentialID`。registry 构建时
`internal/app/providers.go:510` 已经 `GetCredential` 了，把 `CredentialID` 与 credential 的
`Revision`（`internal/domain/models.go:223`）带到 `Target` 上是本文的第一个改动。

### 2.2 `FailureReason` 从日志字段升级为路由输入

仓库已有这套词汇（`internal/provider/provider.go:43-44` 等）：`subscription_inactive`、
`subscription_quota_exhausted`、`rate_limited`、`invalid_credential`、
`entitlement_verification_unavailable`。今天它只喂日志——`applyCanonicalProviderFailure`
（`internal/gateway/failure.go:169-184`）明确写着 `Retryable` 与 `Ambiguous` 保持 adapter 报的原值。

那句话对那两个字段仍然成立。但**"要不要挂起这个 scope、挂多久"是第三个维度**，它正应该由 reason
决定。这是一次有意的立场扩展，要写进那段注释。

收益的前提是：**存在上游把"额度用尽"和"普通限流"压在同一个状态码上**（典型是都用 429、
`Retryable` 都是 true），此时**只有 reason 能区分"等 3 秒"和"等到下个月"**。

**这个前提本身要在阶段 1 证伪。** 如果取证发现每一家都用互不相同的状态码表达额度用尽，那么状态码
本身就够用，判断 2 可以退化成一张状态码表，`FailureReason` 不必进路由路径——那会是更便宜的设计。
反过来只要有一家把两种情况压在同一个码上，就必须按 reason 判。

### 2.3 401/403 不退避——绑 credential revision 自愈

凭据失效不是瞬态。任何退避阶梯用在它身上都是"永远锤一把死 key"。

正确行为：**无限期挂起 + 告警**，解除条件只有两个：

1. credential 的 `Revision` 前进了（运维改了 key）。挂起记录存下观察时的 revision，registry 重建时
   revision 不匹配即自动解除；
2. 运维显式清除。

第 1 条是本文最省事的一处：**"运维修好了"自动等于"解除挂起"**，不需要新 API，也不需要运维记得
第二步。`Credential.Revision` 已经在那儿了。

### 2.4 探针证明不了额度回来了

`GET /v1/models` 不消耗额度（§1.2）。所以：

- **可用性类**失败：主动探针可判定，继续用探针；
- **额度类**失败：**只有一次真实请求能判定**。

推论一：额度类的 half-open 探针必须是**一个真实调用者的请求被故意放行到被挂起的 target**。代价是
那一个调用者吃一次失败往返——与熔断器 half-open 同一种代价，可接受，前提是频率由挂起窗口控制。

推论二：**"候选被清空时要不要硬放一个进去"也是 per-reason 的**。全部因额度挂起时，放行只是多一次
往返再失败，**直接 503 严格更优**；全部因可用性挂起时，放一个探针是值的。

---

## 3. 不变量

实现与门禁必须持续证明：

1. **一个 scope 被挂起时，属于它的 target 不出现在候选里**，无论请求走哪个协议门面。
2. **挂起不改变账务语义。** 被挂起的 target 不进 `startAttempt`，因此没有 reservation、没有
   Ledger 写。挂起导致的 503 是一次拒绝，计入 `RejectionMetrics`（`internal/gateway/service.go:898`），
   不产生 usage 行。
3. **`Retryable` 与 `Ambiguous` 不被 reason 改写。** 计费语义与准入语义是两条独立的通道。
4. **候选为空一律 fail closed**，且答案要带原因类；不得因为"过滤后没人了"就放行一个已知不可用的
   target（可用性类的单个 half-open 探针除外，那是显式的状态迁移，不是兜底）。
5. **不自愈的 reason 必须可见。** `subscription_inactive` 与 `invalid_credential` 的挂起要有告警，
   因为解除它们需要人的动作。
6. **挂起状态是节点本地的派生态**，不进 metadata journal、不复制（§6）。
7. **指标不得带身份。** scope 的 ID、credential 名一律不进 Prometheus label；只有枚举进 label。
8. **reason 的分类来自实测的上游响应**，不来自推断（§7）。

---

## 4. 设计

### 4.1 组件形状

新包 `internal/routegate`，**吸收**熔断器与探针健康，不与它们并存：

```
输入：
  probe 结果（主动，每 30 s）            ──┐
  attempt 结果（被动，每次请求）           ──┼──► 状态机（per scope key）
  credential revision（registry 重建时）  ──┘
输出：
  Filter(targets, now) []Target          ← resolveCandidatesLocked 调它
  Admit(target, now) (lease, error)      ← startAttempt 调它
  Snapshot()                             ← console / metrics / Admin API
```

`Filter` 与 `Admit` 是同一份状态的两个面：前者在解析候选时剪枝（便宜，避免无用的 pricing 锁与
pin 写），后者在真正发起前做最后一次检查并占 half-open 名额（必要，解析与发起之间有时间差）。
**今天这两件事分属探针和熔断器，正是它们会互相矛盾的根源。**

`round_robin` 不用改：过滤发生在 `resolveCandidatesLocked` 内、轮转之前
（`provider.go:741-758`），轮转天然作用在已过滤的列表上——今天 probe 健康就是这么工作的。

### 4.2 状态机

每个 scope key 一条：

```
admitted ──失败累积──► degraded ──越过阈值──► suspended(until, reason, evidence)
                                                      │
                                              窗口到期 │
                                                      ▼
                                                  probing（限 1）
                                                   ┌──┴──┐
                                                成功│     │失败
                                                   ▼     ▼
                                              admitted  suspended(窗口 ×2)
```

`evidence` 记录：观察到的 `provider_status`、`provider_code`、`FailureReason`、
observed-at、以及（credential scope）观察时的 credential `Revision`。全部是已经过
`provider.SafeProviderIdentifier` 的标识符，**不含上游正文**。

### 4.3 每个 reason 的时窗与阈值

| reason | 阈值 | 初始窗口 | 扩展 | 上限 | 恢复判据 | 候选清空时 |
|---|---|---|---|---|---|---|
| availability（connect / timeout / 5xx / malformed） | 连续 5 次 | 30 s | ×2 | 5 min | 主动探针**或**真实请求 | 放一个探针 |
| `rate_limited` | 1 次 | `Retry-After`，无则 1 s | ×2 | 60 s | 真实请求 | 放一个探针 |
| `subscription_quota_exhausted` | 1 次 | `Retry-After`，无则 15 min | ×2 | 6 h | **仅真实请求** | **直接 503** |
| `subscription_inactive` / `invalid_credential` | 1 次 | **无限期** | — | — | **credential revision 前进** | **直接 503** |
| `entitlement_verification_unavailable` | 1 次 | 30 s | ×2 | 5 min | 真实请求 | 放一个探针 |

两点说明：

- **阈值也是 per-reason 的。** 可用性类要连续 N 次（沿用今天的 `consecutive_failures: 5`），因为
  单次 5xx 可能是噪声；额度与凭据类**一次即挂起**——上游明确说了"不给你用"，累积没有意义。
- `Retry-After` 已经解析好了（`internal/provider/anthropic/adapter.go:860`、`provider.Error.RetryAfter`），
  今天只用来调退避（`service.go:2659-2662`），这里直接复用作为窗口初值。

**表里的数字是起点，不是结论。** 它们要在阶段 1 的真实流量数据上复核（§8）。

### 4.4 不预测配额重置

配额什么时候恢复，只有三种信号：上游说的（`Retry-After`）、按计划的（月初、UTC 零点）、和试出来的。
**中间那种不要猜**：各家规则不同、会变、且猜错的方向是"在配额还没回来时反复试"或者更糟的
"配额回来了却继续挂着"。Halro 只用第一种和第三种。

---

## 5. fail-closed、可观测与账务

### 5.1 候选为空的答案

`resolveRequest`（`internal/gateway/service.go:334-348`）今天已经把零候选分成三种答案，注释写清了
为什么不能报 400——把上游状态说成请求的错，会让调用方去改 payload、运维去查 capability，两边都
找不到东西。新增第四种：

- code `all_candidates_suspended`，HTTP 503，带 reason 类（`quota_exhausted` / `credential_unusable` /
  `unavailable`）与最早的 `until`，并按 reason 决定要不要带 `Retry-After`。

**不得向 Gateway 调用方泄露是哪个上游、哪个 credential。** 身份走 console 与 Admin API。

### 5.2 指标（低基数）

```
halro_route_suspended{scope_kind, reason}                 gauge
halro_route_suspension_transitions_total{reason}          counter
halro_route_probe_admitted_total{reason, outcome}         counter
halro_provider_failure_reason_total{reason}               counter   ← 阶段 1 就加
```

`scope_kind ∈ {deployment, credential, credential_model, provider}`，`reason` 是 `FailureReason`
枚举。**ID 不进 label**（不变量 7）。

### 5.3 告警与 console

- `HalroCredentialUnusable`：任一 credential scope 因 `subscription_inactive` /
  `invalid_credential` 挂起。这两个不会自愈，修复是人的动作，**必须独立告警**；
- `HalroProviderQuotaExhausted`：warning 级；
- 可用性类沿用现有告警；
- runbook 链接要指向 `/docs/` 下真实文件的 `### <AlertName>` 小节，否则观测门禁不过
  （`deploy/observability/runbook_links_test.go`）。

console 复用 `persistedProbeClass`（`internal/app/admin_providers.go:777`）那张措辞表——它存在的
理由就是"探针失败和手动连接测试读起来一样"。挂起状态加入同一张表，**不要开第二张**。

### 5.4 Audit

**挂起本身不写 Audit。** Audit 是权威 mutation 的账，挂起是从流量派生的状态。
**运维清除挂起要写**，那是管理动作。

### 5.5 持久化与 HA

写 bbolt，**只在状态迁移时写**，不在请求路径上写——一个周期只迁移一次，成本可忽略。

归 HA 设计 §5.2 的**节点本地类**，**不进 metadata journal、不复制**。理由：Replica 没有流量、学不到
挂起状态；提升后每个 target 用一次失败请求重新学，很便宜。这一条要同时改
[`halro-ha-architecture.zh-CN.md`](halro-ha-architecture.zh-CN.md) 的 §5.2 分类表。

---

## 6. 取代的既有构造

pre-1.0.0 规则是"错误的构造不得与替代品并存"。本文取代：

| 现有 | 处置 |
|---|---|
| `internal/circuit` | **吸收**。`FailureThreshold` / `OpenDuration` / `HalfOpenMaxRequests` 变成 §4.3 表里 availability 那一行。包删除，不保留 |
| `Registry.health` / `SetDeploymentProbe` / `RetainDeploymentProbes` | 探针变成 routegate 的**输入**，不再是并行的一张 map；`provider.go:763-765` 的健康过滤换成 `routegate.Filter` |
| `config.CircuitBreaker` 块 | 换成 `routing:` 块，带 per-reason 策略。**这是配置破坏性变更**，pre-1.0 允许，但必须写进 release notes |
| `availabilityFailure`（`service.go:2625-2638`） | 并入 reason 分类，不再是只服务熔断器的私有函数 |

---

## 7. 开工前必须先核实的

整个设计押在 `FailureReason` 准确上，而它今天只从一小把字符串码推出来
（`internal/gateway/failure.go:126-150`）。

1. **各上游到底怎么表达"额度用尽"，必须从真实响应取证，不能猜。**
   **仓库今天对此只有一条厂商级依据：Kimi Code 的 402**（`internal/provider/openai/adapter.go:1034-1044`）。
   OpenAI、Anthropic、Azure、DeepSeek、MiniMax、Gemini、Bedrock Mantle 一家都没有——没有 adapter
   分支、没有测试、没有 verification 记录。所以本文**不对它们中任何一家的状态码作断言**，全部列为
   待取证项，用 `docs/verification/provider-real-matrix.md` 那条计费取证通道跑出来，按每家记录：
   状态码、`code` 字段、有无 `Retry-After`、以及它与普通限流是否同码。
   **"Halro 没枚举过"是关于 Halro 的事实，不是关于上游的答案**——这条规则在 MiniMax 的模型列表上
   栽过一次，本文初稿又在这里栽了第二次（把一条凭印象写下的厂商映射放进了专讲取证的小节）。
2. **额度是 per-credential 还是 per-model**，每家不同，决定 scope 宽窄。取不到证据就按窄的来。
3. **`Retry-After` 在额度场景下上游到底给不给**，给的话值是否可信。给了假值比不给更糟。
4. **半开探针用真实请求，意味着一个调用者被当成探针。** 要确认这在目标 SLA 里可接受，否则
   窗口要拉长、或者对额度类改成"只在候选全空时才探"。

---

## 8. 实施阶段

### 阶段 1：取证与前置改动（不改结构）

1. `Target` 带上 `CredentialID` 与 credential `Revision`（`internal/app/providers.go:510` 已经有
   credential 在手）；
2. `halro_provider_failure_reason_total{reason}` 上线，**先观察一段真实流量**；
3. 按 §7 第 1、2、3 条跑上游取证矩阵，把 reason 分类表建起来；
4. 订正 §1.4 的默认值：`retry.max_attempts_per_target: 1` + `gateway.max_total_attempts` 至少覆盖
   常见候选数。跨平台回退的价值在"换一家"，不在"同一家再试一次"；
5. 把 §4.3 的时窗数字按 2、3 的结果复核后再冻结。

**阶段 1 不依赖任何结构决定，且它的产出正是决定阶段 2 数字的依据。**

### 阶段 2：`internal/routegate`

1. scope 模型与 key 派生；状态机与 per-reason 时窗；
2. `Filter` / `Admit` 两个面，分别接到 `resolveCandidatesLocked` 与 `startAttempt`；
3. credential revision 自愈；
4. 吸收 `internal/circuit` 与 `Registry.health`，删除被取代的四项（§6）；
5. `routing:` 配置块与 `config check` 校验。

### 阶段 3：面向人的部分

1. `all_candidates_suspended` 的 503 分型（§5.1）；
2. 指标、告警规则、runbook；
3. console 状态展示（复用既有措辞表）与 Admin API；
4. `halro` 侧查看与清除挂起的命令，清除动作写 Audit；
5. bbolt 持久化，以及 HA §5.2 分类表归位。

---

## 9. 发布门禁

每条带可观测的 oracle：

| 门禁 | oracle |
|---|---|
| 一次 401 挂起整个 credential | 一把 credential 挂 3 个 deployment，其中一个收到 401；**另外两个在下一次请求就已不在候选里**，且没有各自再撞一次 |
| credential revision 自愈 | 挂起后更新 credential，registry 重建，挂起自动解除，无需任何显式清除 |
| 额度类不放兜底请求 | 全部候选因 `subscription_quota_exhausted` 挂起时，请求在**零次上游往返**内拿到 503 `all_candidates_suspended` |
| 可用性类放一个探针 | 同上但 reason 是 availability 时，恰好一个请求被放行，其余拿 503 |
| 挂起不产生账务 | 挂起路径上 Ledger 无写入、usage 无新行、`RejectionMetrics` +1 |
| `Retryable` / `Ambiguous` 未被改写 | 对每个 reason 构造错误，断言 adapter 报的两个字段原样到达结算路径 |
| 不泄露身份 | `/metrics` 全量抓取后对 credential ID、deployment ID、上游正文做 secret canary 扫描 |
| reason 分类来自实测 | 分类表的每一行在 `docs/verification/` 下有对应的真实响应证据 |
| 探针与被动信号不打架 | 探针报健康、被动流量报额度用尽时，target 保持挂起（被动信号在额度维度上胜过探针） |

---

## 附录 A：核实过的事实索引

2026-09-20 在 `main` 上核实。**实施时应重新验证，而不是引用本表。**

| # | 事实 | 证据 |
|---|---|---|
| 1 | `Route.Strategy` 只有 `ordered` 与 `round_robin` 两个合法值，空串按 ordered | `internal/domain/models.go:1298-1299` |
| 2 | 候选按 `Priority` 升序、同 priority 按 ID 排 | `internal/provider/provider.go:698-709` |
| 3 | round_robin 只旋转起点，且要求 `len(targets) >= 2` 且 `targets[0].Strategy == "round_robin"`——**同一 alias 下多条 route 策略不一致时，优先级最高那条说了算** | `internal/provider/provider.go:741-758` |
| 4 | 候选解析先删 probe 不健康的 deployment，再按 operation 过滤 | `internal/provider/provider.go:760-767` |
| 5 | `Target` 没有 `CredentialID`；`ProviderInstance.CredentialID` 有 | `internal/provider/provider.go:480-522`；`internal/domain/models.go:491` |
| 6 | registry 构建时已经 `GetCredential` | `internal/app/providers.go:510` |
| 7 | `Credential.Revision` 存在 | `internal/domain/models.go:223` |
| 8 | 不可重试即不回退，直接返回 | `internal/gateway/service.go:1370-1378` |
| 9 | `retryable()` = `!Ambiguous && (Retryable \|\| Malformed \|\| Provider5xx)` | `internal/gateway/service.go:2612-2623` |
| 10 | 熔断器只认 connect / timeout / 5xx / malformed，其余返回 `nil`，而 `Done(nil, …)` 是**成功**、会清零失败计数 | `internal/gateway/service.go:2625-2638,890-896` |
| 11 | OpenAI 侧分类：401/403 → authentication 且不可重试；429 → rate_limit 可重试；503 → provider5xx 可重试；其它 5xx → `Ambiguous`；**402 落到 default，即 bad_request、不可重试** | `internal/provider/openai/adapter.go:996-1031` |
| 12 | Kimi Code 的 402 已被特例改成 `entitlement_verification_unavailable` + 可重试 | `internal/provider/openai/adapter.go:1034-1044`；`internal/provider/anthropic/adapter.go:841-846` |
| 13 | `FailureReason` 目前只影响日志的 Class，不影响 `Retryable`/`Ambiguous` | `internal/gateway/failure.go:126-184` |
| 14 | OpenAI 的 `Probe` 是 `GET /v1/models`（Azure 例外，走 chat/completions 路径） | `internal/provider/openai/adapter.go:371-392` |
| 15 | 探针默认 30 s 一次，`Healthy = (err == nil)` | `internal/config/default.yaml:172`；`internal/app/health.go:115-125` |
| 16 | `Retry-After` 已解析，今天只用于调退避 | `internal/provider/anthropic/adapter.go:860`；`internal/gateway/service.go:2659-2662` |
| 17 | 默认 `gateway.max_total_attempts: 3` + `retry.max_attempts_per_target: 2` ⇒ **第三个候选永远试不到** | `internal/config/default.yaml:168,211`；`internal/gateway/service.go:1271-1380` |
| 18 | 零候选已分三种答案（unhealthy / unsupported / not found），注释写明为什么不能报 400 | `internal/gateway/service.go:334-348` |
| 19 | 拒绝计数结构已存在 | `internal/gateway/service.go:898` |
| 20 | console 措辞表 | `internal/app/admin_providers.go:777` |
| 21 | 观测门禁要求 runbook 链接指向真实文件的 `### <AlertName>` 小节 | `deploy/observability/runbook_links_test.go` |
| 22 | **仓库里唯一有依据的"厂商如何表达额度/权益问题"是 Kimi Code 的 402。** `insufficient_quota` 等字符串在全仓零命中（本文自身除外），其余厂商无 adapter 分支、无测试、无 verification 记录 | grep `insufficient_quota`；`internal/provider/openai/adapter.go:1034-1044` |
