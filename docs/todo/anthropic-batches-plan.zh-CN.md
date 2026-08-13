# Anthropic Message Batches 实施方案

状态：**待实施**，切片 1 进行中
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

### 1.2 结果脱敏：落盘前逐行过出站策略

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

## 2. 切片划分

每个切片独立可测、可提交。

### 切片 1：资源模型不再假设上游孪生

ADR 0021 的直接后果，不涉及 Anthropic 任何代码，可单独验证。

三处停止假设（`internal/gateway/inference_resources_store.go`）：

- `GetFile`（`:318`）——`UpstreamID` 为空时从记录答元数据，不问上游
- `CleanupExpiredProviderResource`（`:480`）——只删本地对象，不去上游删
- `CreateBatch`（`:501`）——输入文件的 owner 不再必须实现文件操作

验收：本地独有的文件资源能被创建、查询元数据、下载内容、过期回收，全程不产生任何上游调用；备份仍然通过（本地独有资源必然有对象文件）。

### 切片 2：Anthropic 适配器的批处理原语

`internal/provider/anthropic` 实现 `CreateBatch`/`GetBatch`/`CancelBatch`：

- 请求：读回本地 JSONL，逐行转成 `{custom_id, params}`；`params` 复用 `RenderPortableRequest`（`internal/compatibility/anthropic/mapping.go:253`），**不写第二份映射**
- 响应：`processing_status` → OpenAI `status`（`ended` 需按 `request_counts` 分解为 completed/failed/cancelled，不能一律 completed）；RFC 3339 时间戳 → Unix
- 结果 URL：**自己按配置 endpoint 拼** `{endpoint}/v1/messages/batches/{id}/results`，`results_url` 仅用于一致性校验

### 切片 3：结果落盘

惰性拉取 + 逐行脱敏 + 32 MiB 界 + 幂等短路，见 §1。

### 切片 4：Profile 与契约

`anthropic.messages.2023-06-01` 增加 batches operation（或新建 profile——按 `openai.media-resources.v1` 的先例，媒体资源与对话分家的理由是失败模式与计费方式不同，批处理同理，倾向新建）；manifest 的 `/v1/batches` 加入该 profile 与 ProfileCoverage；documented deviations 写明 32 MiB 上限与 `completion_window` 约束。

## 3. 测试门禁

- 每个切片各自的单元与契约测试
- 反向验证：每处修复退回旧行为，确认对应测试变红
- 真实账号：Anthropic 凭据已在手且今日验证过（7/7），切片 2、3 完成后跑一次端到端批处理

## 4. 明确不做的

- 不引入后台轮询任务
- 不引入网关级磁盘配额（超限即失败，配额是独立决定）
- 不新增北向端点。`/v1/messages/batches` 作为第二种协议形状留待将来，见 ADR 0021
