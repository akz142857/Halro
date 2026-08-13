# Anthropic Message Batches 实施方案

状态：**五个切片全部完成**；**尚未做过真实账号端到端验证**，见 §5
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

**两边的分歧建立在同一个前提上——结果可能无限大。** 一旦按 §1.3 给拉取设界，这个前提消失：有界的拉取同时是有时限的，16 MiB 在正常链路上是秒级，远在两分钟之内。

因此：**在 `GET /v1/batches/{id}` 中惰性拉取，不引入后台任务。** 这保住了"整个资源路径没有第二个写者"这条既有性质。

幂等：结果一旦落盘，`OutputFileID` 记在批处理记录上（#0 已建立的机制），后续轮询直接短路，不重复拉取。并发轮询由现有的 `reserved / in_flight` 阶梯与 `ReservedBy` 保护，第二个请求读到 `in_flight` 时回答"结果拉取中"，而不是重复下载。

### 1.2 结果脱敏：落盘前逐行过出站策略（接口见 §2 核实结论三）

批处理结果是模型输出，属于出站方向。落盘等于把响应体持久化到一次性响应路径之外，这是 CLAUDE.md 明确禁止的。

`internal/redaction` 已有 `StreamInspector`（`inspect.go:125-190`）。结果是 JSONL，按行喂给它，命中即按策略处置——与流式对话响应用的是同一套判定，不新造一条。

拒绝策略下的命中意味着这批结果不能交付：批处理记录标记为失败，`errors` 说明原因。**不允许"落盘时不脱敏、下载时再脱敏"**——那样磁盘上就留着未脱敏的副本。

### 1.3 字节界：写读两侧同界，沿用既有上限

拉取受 Anthropic 适配器的 `maxResponseBytes`（**16 MiB**，`internal/provider/anthropic/adapter.go:25`）约束，
`FetchBatchResults` 就是用它读结果流的。超限不是截断，是失败：批处理标记为无法交付，`errors` 说明结果超出
网关可承载的大小，并提示改用更小的批次。

> 本节原先写的是 `maxInferenceResourcesResponseBytes`（32 MiB）——那是 **OpenAI 适配器**的常量
> （`internal/provider/openai/inference_resources.go:17`），与这条链路无关。2026-08-14 更正。

这条同时决定了 §1.1，也避免了新增一个"网关级配额"概念——那是另一个决定，不该作为本功能的副作用发生。

**已知代价**：Anthropic 的批处理结果可能远超 16 MiB。这条限制要写进 manifest 的 documented deviations，让调用方在遇到之前就知道。若将来要放开，那是一次独立的、带配额设计的改动。

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
| 2 | Anthropic profile 声明 files 与 batches，files 走本地独有 Primitive | ✅ 已完成 |
| 3 | Anthropic 适配器的批处理原语 | ✅ 已完成 |
| 4 | 结果落盘（惰性拉取 + 逐行 `ProcessJSON` + 16 MiB 界 + 幂等短路） | ✅ 已完成 |
| 5 | 契约与 manifest | ✅ 已完成 |

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

### 2.5 升级注意事项

`DefaultProviderCapabilitiesForProfile` 对 `ProfileAnthropicMessages` 打开了 `Files`/`Batches`。该默认值是
**上限**（`ProviderCapabilitiesSubset(binding.Capabilities, defaults)`），放宽上限不会让既有记录失效——
它们仍是更大集合的子集。

因此**既有 Anthropic 连接不会自动获得批处理能力，需要操作者在控制台手工勾选**。与归档计划里
`json_mode` 的情形同类。`ProfileAnthropicMessages` 不在 `IsImmutableCapabilityProfile` 名单中，勾选是允许的。

Provider 侧 `ProfileManifest.Revision` 从 1 升到 2 仅为标注：该字段只被校验为非零
（`internal/provider/profile.go:24`），不持久化、不参与任何比较。

## 2.6 批处理端点契约（2026-08-13 核对官方文档）

| 操作 | 方法与路径 |
|---|---|
| 创建 | `POST /v1/messages/batches`，体为 `{"requests":[{custom_id, params}]}` |
| 查询 | `GET /v1/messages/batches/{id}` |
| 取消 | `POST /v1/messages/batches/{id}/cancel` |
| 结果 | `results_url` 指向的 `.jsonl`；**只在处理结束后才存在** |

核对推翻或收紧了三处判断：

**一、`expires_at` 是创建后固定 24 小时**，不是可配置窗口。这让 §1.4 的决定理由更硬：只接受
`completion_window: "24h"` 不是 Halro 的偏好，而是**填别的值都在谎报上游行为**。

**二、取消是"发起"而非"完成"。** 文档明说批处理进入 `canceling`，系统"可能先跑完进行中的不可中断
请求"，并且"取消可能不产生任何被取消的请求"。因此 `decodeBatchProcessingStatus` 里按计数分类而不是
按"取消过就是 cancelled"是必需的，不是保守。

**三、`request_counts` 在整批结束前除 `processing` 外全为 0。** 文档原话："This is zero until
processing of the entire Message Batch has ended."。现有实现只在 `ended` 分支读计数，因此成立；但这
条必须写下来——任何未来在 `in_progress` 期间读计数的改动都会读到 0 并据此做出错误判断。

**四、惰性拉取的触发条件是 `results_url` 非空，不是"状态为 ended"。** 文档："Specified only once
processing ends."。比 §1.1 原先写的更精确：以 URL 是否出现为准，不必自己推断生命周期。

另注意 `archived_at` 与 `expires_at` 是两个不同时刻：前者表示**结果已不可取**。ADR 0021 的
Consequences 里那条 `expired` 映射处理的正是 `archived_at`。

**结果顺序无保证**：文档说结果文件中的顺序不保证与请求一致，要用 `custom_id` 匹配。这也是为什么
`renderBatchRequests` 拒绝重复的 `custom_id`——重复会让匹配失去唯一解。

## 2.7 结果端点契约（2026-08-13 核对官方文档）

`GET /v1/messages/batches/{id}/results`，返回 `.jsonl` 流。路径与此前从 `results_url` 样例推断的一致
——但现在是核实过的，不是推断的。

**每行的形状（Anthropic）**：

```json
{"custom_id":"a","result":{"type":"succeeded","message":{ ...Message... }}}
```

`result.type` 取 `succeeded` / `errored` / `canceled` / `expired` 四值之一；成功时 `result.message`
是一个完整的 Message 对象，失败时 `result.error` 是错误信封。

**而 OpenAI 的批处理结果行是另一种形状**：`{id, custom_id, response:{status_code, request_id, body}, error}`。

### 这暴露了方案漏算的一整步

切片 4 原本写成"惰性拉取 + 逐行脱敏 + 落盘"。实际还有**逐行翻译**：Anthropic 的
`result.message` 要变成 OpenAI 的 `response.body`，也就是一个 ChatCompletionResponse。

翻译本身可以复用——`anthropicwire.DecodeResult` → semantic → `openaiwire.RenderGenerateResult`，
与 `adapter.Chat` 用的是同一对函数，同切片 3 的做法一致，不写第二份映射。

四种 `result.type` 到 OpenAI 行的对应：

| Anthropic | OpenAI 结果行 |
|---|---|
| `succeeded` | `response.status_code = 200`，`body` 为翻译后的 ChatCompletionResponse |
| `errored` | `error` 携带上游错误类型；不编造 status_code |
| `canceled` / `expired` | `error` 说明该请求未被执行的原因 |

**注意**：结果里可能含 `thinking` 块。切片 3 已让批处理请求发送 `thinking: {"type":"disabled"}`，
所以正常情况下不会出现；但若上游仍返回，`DecodeResult` 会拒绝该行（"signed thinking response requires
native mode"）。该行按 `errored` 处理并说明原因，而不是让整批失败——这一条与请求侧"整批拒绝"的取舍
不同，理由是：请求侧的问题是调用方可以修的，结果侧的问题调用方无能为力，丢掉其余可用结果没有意义。

## 3. 测试门禁

- 每个切片各自的单元与契约测试
- 反向验证：每处修复退回旧行为，确认对应测试变红
- 真实账号：Anthropic 凭据已在手且今日验证过（7/7），切片 2、3 完成后跑一次端到端批处理

## 5. 唯一的未完成项：真实账号端到端验证

五个切片的代码路径已经齐了，但**全部只有 fixture 与假上游验证**。这一天里假上游三次没能拦住真实缺陷：
`max_output_tokens` 字段名读错、`capabilities` 里三个键根本不存在、`unsupported_parameter:max_tokens`。
每一次都是测试与代码出自同一份假设，一起错、一起绿。

因此这一项不是收尾，是这条链条真正的完成条件。

**怎么跑**（凭据在手且今日验证过，7/7）：

1. 控制台给 Anthropic Provider 勾上 Files 与 Batches 能力——**升级不会自动打开**（见 §2.5）
2. 建一个指向该 Provider 的 Deployment 与 Route
3. `POST /v1/files`（`Halro-Route: <别名>`，multipart，JSONL 内容）
4. `POST /v1/batches`，`input_file_id` 用上一步返回的 id，`completion_window: "24h"`
5. 轮询 `GET /v1/batches/{id}` 直到 `status` 变为 `completed`
6. `GET /v1/files/{output_file_id}/content` 取结果

**预期会撞上的地方**（都是从样例或文档推断、未经真实上游验证的）：

- 批处理创建时 `params` 的完整形状是否被上游接受
- `request_counts` 的字段名（文档写 `canceled`，注意不是 `cancelled`）
- 结果文件每行的 `result.message` 是否能被 `DecodeMessage` 直接解析
- 16 MiB 上限在真实结果规模下是否过窄（**决定不验证**，见 §5.2）

### 5.1 首次真实运行到哪一步（2026-08-13）

走到第 4 步为止，前三步真实通过，第 4 步被两个真实缺陷挡住，都已修复并推到
`feat/anthropic-platform-api`：

1. **部署层能力没打开就无从路由。** Provider 勾上 Files/Batches 不够，`ResolveCandidatesFor`
   按 Deployment 的能力筛，Deployment 仍只有 chat，`POST /v1/files` 返回
   `ambiguous_resource_route`。§5 第 1 步只说了 Provider，实际两层都要开。
2. **编辑态发不出 `mode=operator_declared`。** 控制台的 `declaredModel` 带 `!current`，
   只有新建能声明；编辑时勾出新能力必然撞 `model_capabilities_unknown`，界面上没有出路。
3. **扩宽能力要先离开路由。** `capability_expansion_requires_revalidation` 是设计内的闸：
   停用路由 → 保存部署（自动落停用）→ 测试 → 启用部署 → 启用路由。跑之前就该按这个顺序排。
4. **`CreateBatch` 一律 "batches are unavailable"。** `ResourceInferenceResourcesAdapter`
   把文件与批处理方法捆在一个接口里，Anthropic 适配器（inline 批处理，无提供商侧文件）
   断言整体失败。已拆为 `ResourceFilesAdapter` / `ResourceBatchesAdapter`。

上面的「预期会撞上的地方」四条**一条都还没被验证**——真实上游还没收到过一次
`POST /v1/messages/batches`。

**续跑步骤**（承接 §5，从第 4 步起）：

1. `make build` 后重启 Halro——接口拆分改的是 Go 侧，不重启仍是旧的断言
2. 确认部署与路由都启用（若上一轮为扩宽能力停过路由，别忘了启用回来）
3. `POST /v1/files` 传 JSONL，拿 `input_file_id`
4. `POST /v1/batches`，`completion_window: "24h"`——**这里是真实上游的第一次批处理创建**，
   §5 那四条预期风险从这一步开始逐条兑现
5. 轮询 `GET /v1/batches/{id}` 至 `completed`
6. `GET /v1/files/{output_file_id}/content` 取结果，核对每行 `custom_id` 与 `response.body`

**跑之前要按对的顺序**：给部署扩宽能力必须先停用其路由（`capability_expansion_requires_revalidation`），
再保存 → 测试 → 启用部署 → 启用路由。Provider 与 Deployment 两层能力都要开，只开 Provider 会得到
`ambiguous_resource_route`。

另外记一笔：`POST /v1/files` 返回的 `created_at` 是 0，北向形状不该这样。已登记在
[provider-adaptation-gaps](provider-adaptation-gaps.zh-CN.md) 的控制台缺口一节。

还有一条 §5 没写的前置条件：**`POST /v1/files` 强制要 `Idempotency-Key` 头**，不带就是 400
`invalid_idempotency_key`（`internal/gateway/inference_resources_store.go:188`，头名在
`internal/gatewayapi/inference_resources.go:344` 读取）。§5 的六步里漏了这一条，照着跑的人会先撞一次 400。
`POST /v1/batches` 同样需要。

### 5.2 16 MiB 上限：决定不验证（2026-08-14）

§5 的第四条预期风险「上限在真实结果规模下是否过窄」**明确不验证**，理由记在这里，免得以后被当成遗漏。

验证它必须让某一边越线：要么把结果撑到 16 MiB，要么把界降到数据这边。小批处理测不到——上限管的是结果
文件字节数，小结果永远走不到那个分支。

撑满 16 MiB 的成本比直觉低，但仍不值得：结果文件的字节大头是每行的 JSON 外壳（`id`/`custom_id`/
`response` 骨架约 400 字节），而外壳不花钱，所以「很多条极短请求」比「少数条长输出」便宜得多——约 4.2 万条、
合计约 $2（Sonnet 5 批处理价）。但这测的是**这个数值本身合不合适**，属于容量问题，等有真实批量需求时再
测更有意义。

真正的代码风险——`readLimited` 超限时是否 fail-closed、错误信息是否说得清——可以用**把常量临时降到
几 KiB 再跑一次小批处理**来验证，代码路径完全相同、成本为零。这条留给需要动那段代码时再做。

本轮的目的是打通流程，不是压测容量。

## 4. 明确不做的

- 不引入后台轮询任务
- 不引入网关级磁盘配额（超限即失败，配额是独立决定）
- 不新增北向端点。`/v1/messages/batches` 作为第二种协议形状留待将来，见 ADR 0021
