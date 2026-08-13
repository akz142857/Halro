# Anthropic Message Batches 实施方案

状态：**切片 1 已完成**；切片 2 判据经三角色评审后重定，待实施
建立日期：2026-08-13
决定依据：[ADR 0021](../adr/0021-provider-resource-upstream-twin.md)（Accepted）
评审依据：[`docs/review/260813/batch-design-review.zh-CN.md`](../review/260813/batch-design-review.zh-CN.md)
范围：`internal/domain`、`internal/gateway`、`internal/provider/anthropic`、`internal/compatibility`

## 0. 这份方案要回答什么

ADR 0021 定了方向：批处理是模态，`/v1/batches` 是唯一的北向批处理端点，Anthropic 作为又一个能服务它的 profile 加入；`UpstreamID` 可以为空。

ADR 同时把四条约束原样交给实施方案，本文逐条给出答案。**它们不是建议，是评审拦下第一版设计的理由。**

## 1. 四条约束的答案

### 1.1 拉取时机：惰性拉取，因为拉取有界

评审在这一条上没有收敛：架构反对为"用户少等一次"引入隐式写者，可用性指出 `routeTimeout` 只有两分钟且 SDK 默认重试两次。

**两边的分歧建立在同一个前提上——结果可能无限大。** 一旦按 §1.3 给拉取设界，这个前提消失：有界的拉取同时是有时限的，32 MiB 在正常链路上是秒级，远在两分钟之内。

因此：**在 `GET /v1/batches/{id}` 中惰性拉取，不引入后台任务。** 这保住了"整个资源路径没有第二个写者"这条既有性质。

幂等：结果一旦落盘，`OutputFileID` 记在批处理记录上（#0 已建立的机制），后续轮询直接短路，不重复拉取。并发轮询由现有的 `reserved / in_flight` 阶梯与 `ReservedBy` 保护，第二个请求读到 `in_flight` 时回答"结果拉取中"，而不是重复下载。

### 1.2 结果脱敏：落盘前逐行过出站策略（接口见 §2 核实结论三）

批处理结果是模型输出，属于出站方向。落盘等于把响应体持久化到一次性响应路径之外，这是 CLAUDE.md 明确禁止的。

`internal/redaction` 已有 `StreamInspector`（`inspect.go:125-190`）。结果是 JSONL，按行喂给它，命中即按策略处置——与流式对话响应用的是同一套判定，不新造一条。

拒绝策略下的命中意味着这批结果不能交付：批处理记录标记为失败，`errors` 说明原因。**不允许"落盘时不脱敏、下载时再脱敏"**——那样磁盘上就留着未脱敏的副本。

### 1.3 字节界：写读两侧同界，沿用既有上限

拉取受 `maxInferenceResourcesResponseBytes`（32 MiB）约束。超限不是截断，是失败：批处理标记为无法交付，`errors` 说明结果超出网关可承载的大小，并提示改用更小的批次。

这条同时决定了 §1.1，也避免了新增一个"网关级配额"概念——那是另一个决定，不该作为本功能的副作用发生。

**已知代价**：Anthropic 的批处理结果可能远超 32 MiB。这条限制要写进 manifest 的 documented deviations，让调用方在遇到之前就知道。若将来要放开，那是一次独立的、带配额设计的改动。

### 1.4 `completion_window`：只接受 `24h`

OpenAI 形状里 `completion_window` 必填；Anthropic 没有这个参数，其 24 小时是取消阈值而非承诺。

按"不支持的字段拒绝而非静默丢弃"，Anthropic profile 只接受 `"24h"`，其余值在 Provider I/O 之前拒绝。

## 2. 切片划分（2026-08-13 核实后修订）

初版划分把一个**前置条件**当成了收尾的配置工作，核实后重排。

### 核实结论一：Anthropic 必须先能服务 `/v1/files`

`CreateFile` 要求 `ResolveCandidatesFor(route, provider.OperationFiles)` 恰好命中一个目标
（`internal/gateway/inference_resources_store.go:206-209`）。而切片 1 之后，批处理的 owner 取自
**输入文件的 owner**（`s.ownedTarget(file)`）。

因此：**要让一个批处理落到 Anthropic，它的输入文件必须先归属于一个声明了 `OperationFiles` 的
Anthropic Deployment。** 这个文件只存在于 Halro 本地、不上传上游——正是 ADR 0021 允许的形态。

这不是收尾的配置项，是批处理原语的前置。

### 核实结论二：JSONL 行的能力过滤位置

`DecodeGenerate`（`internal/compatibility/openai/mapping.go:17`）→ `RenderPortableRequest`
（`internal/compatibility/anthropic/mapping.go:253`）这条链路成立，不需要第二份映射。

但能力过滤发生在**路由时**，按整个请求选目标（`internal/gateway/service.go:866`、`:1461`、`:1749`）。
批处理是一次路由、N 行请求：某一行带了目标承载不了的字段，路由时看不见，渲染时才发现。

**渲染失败即整批拒绝，并指明是哪一行。** fail-closed，且错误可行动。不做逐行降级——静默丢弃调用方
写下的字段是这个项目明令禁止的。

### 核实结论三：结果脱敏不用 `StreamInspector`

`StreamInspector` 是按 channel 的增量推送模型（`Push`/`Close`/`Finish`，
`internal/redaction/inspect.go:136-183`），为 SSE 的多路增量设计。批处理结果的每一行是一条完整
JSON，没有跨片段的滚动窗口问题。

**改用 `ProcessJSON` 逐行处理。** 初版方案里"逐行过 StreamInspector"是没核实接口就写下的。

### 修订后的切片

| # | 内容 | 状态 |
|---|---|---|
| 1 | 资源模型不再假设上游孪生 | ✅ 已完成（`12bb3e0`） |
| 2 | Anthropic profile 声明 files 与 batches，files 走本地独有 Primitive | 判据已定（§2.2），待实施 |
| 3 | Anthropic 适配器的批处理原语 | 待 |
| 4 | 结果落盘（惰性拉取 + 逐行 `ProcessJSON` + 32 MiB 界 + 幂等短路） | 待 |
| 5 | 契约与 manifest | 待 |

### 2.1 本地独有文件模式的判据：三角色评审后重定（2026-08-13）

**候选判据已作废。** 原候选是"profile 声明 `OperationFiles` 但适配器不实现
`ResourceInferenceResourcesAdapter` → 视为本地独有"。架构、安全、核心逻辑三个角色独立评审，**三票不通过**，
且三者都先撞上同一条事实：

`*LegacyAdapterBridge` 无条件实现该接口的全部方法（`internal/provider/profile.go:248-296`），而
`Target.Adapter` 装的**始终**是这个 bridge（`internal/app/providers.go:434` 构造 → `:584` 赋值）。
编译期断言 `var _ ResourceInferenceResourcesAdapter = (*LegacyAdapterBridge)(nil)` 通过。

因此断言恒为真，**判据分支在生产中是死代码**；而单元测试注册的是裸 fake adapter，断言会失败，判据看起来
完全正常。**绿测试 + 坏生产。**

第二层问题：Go 的接口断言是全有全无。只实现 `CreateFile` 不实现 `DeleteFile` 的适配器整体断言失败，
会被判为"本地独有"从而**静默不上传**。而"缺少接口"同时表示"尚未实现"和"故意本地"，二者不可区分——
一次接线缺陷会与设计意图产生字节级相同的记录，事后无从分辨。

### 2.2 重定的判据：正向声明一个 Primitive

三个角色独立给出同一替代：**用 profile 的正向声明，而不是某个接口的缺席。**

`Primitive` 的定义是"southbound 使用的具体 provider API"（`internal/provider/primitive.go:12-14`），
而"没有 southbound、Halro 自持"正是它该表达的值：

- 新增 `PrimitiveHalroLocalFiles`
- `ProfileAnthropicMessages` 的 manifest 增加 `OperationFiles` + 该 primitive 的绑定
- 在 `profileAllowsPrimitive` 的表里补对应项——**`Validate()` 会在加载期强制说清楚，写错则拒绝启动**
- `CreateFile` 按 primitive 名分支，不做接口断言
- 模式**持久化到 `ProviderResource`**，不靠空 `UpstreamID` 反推

不新增 Operation（会把同一个北向端点的路由键劈成两个），不加 manifest 布尔字段（不受 primitive 表约束，
也不说明谁来服务）。

### 2.3 放行条件（切片 2 必须全部满足）

评审开出的清单，逐条都是拦下来的具体缺陷：

1. 判据改为 profile 正向声明（§2.2）
2. **本地路径不 `markInFlight`、不写 `creationUnknown`。** `markInFlight` 的语义是"调用可能已到达上游"，
   `classifyIdempotency` 会据此把幂等键判为 in-progress 并毒化 30 天；而本地写失败是确定性的"什么都没
   创建"（临时文件由 `defer os.Remove` 清掉、rename 原子），标 `unknown` 是把确定状态谎报成模糊状态
3. **`FixedRequestMicrosUSD = 0` + 1 单位。** 现状传 `len(call.Data)/4+1` 个单位，会为从未离开本机的
   字节按上游单价结算。既有惯例见 `GetFile`/`DownloadFile`/`DeleteFile` 三处
4. **把 `UpstreamID == ""` 分支提到 `fileOwner` 的接口断言之前**（`inference_resources_store.go:303-306`）。
   今天被 bridge 掩盖，判据一旦生效，本地文件的 GET/DELETE 会先吃 409
5. **给 `DeleteFile` 补本地分支。** 切片 1 修了过期回收却漏了交互式删除，会用空 `UpstreamID` 调上游

### 2.4 切片 1 留下的一处不一致（本轮一并修）

切片 1 新增的 `localFileObject` 绕过了 `accountedInferenceResources`，也就绕过了 Token Guard、限流、
并发与账本记录——本地文件的元数据查询可以无限轮询且不留请求记录。而同为本地读的 `DownloadFile` 走了
信封。二者必须统一：**保留信封，单价归零**。

## 3. 测试门禁

- 每个切片各自的单元与契约测试
- 反向验证：每处修复退回旧行为，确认对应测试变红
- 真实账号：Anthropic 凭据已在手且今日验证过（7/7），切片 2、3 完成后跑一次端到端批处理

## 4. 明确不做的

- 不引入后台轮询任务
- 不引入网关级磁盘配额（超限即失败，配额是独立决定）
- 不新增北向端点。`/v1/messages/batches` 作为第二种协议形状留待将来，见 ADR 0021
