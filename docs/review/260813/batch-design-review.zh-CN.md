# Anthropic Message Batches 设计评审（四角色）

日期：2026-08-13
被评审对象：为 Anthropic 适配 Message Batches 时提出的三条实施决定
角色：架构设计、核心逻辑、安全/边界、可用性（按 [`docs/review/README.md`](../README.md) 的框架，各自独立通读代码，不共享中间判断）
结论：**三条无一原样通过，设计推翻重做**

## 被评审的三条决定

提案人（本次会话）在写下这三条时，已经先定了形状：北向保持 OpenAI 形状（`POST /v1/batches` 收 `input_file_id`），Anthropic 的差异在南向消化。三条是这个形状下的实施细节：

1. 查询批处理时**惰性拉取**结果，不引入后台轮询
2. 结果**流式落盘**到 `resourceObjectDir`，绕开 `maxInferenceResourcesResponseBytes`
3. Anthropic 的 `archived`/过期映射为 OpenAI 的 `expired`

## 结论矩阵

| 决定 | 架构 | 核心逻辑 | 安全 | 可用性 |
|---|---|---|---|---|
| 1 惰性拉取 | 通过 | 有条件 | 有条件 | **不通过** |
| 2 绕开字节上限 | **不通过** | **不通过** | **不通过** | **不通过** |
| 3 `expired` 映射 | 通过 | 有条件 | 有条件 | 有条件 |

## 跨角色证实的发现

多个角色独立发现同一处问题，按框架这是置信度最高的一类。以下每条都经提案人复核。

### 1. 第 2 条描述的能力并不存在（四票）

不是"绕开上限"，是把无界缓冲从写路径挪到读路径：

- `writeResourceObject(idValue string, data []byte)`（`internal/gateway/inference_resources_store.go:116`）——入参是整块字节，**今天根本没有流式落盘这条路**
- `DownloadFile` 用 `os.ReadFile` 全量读入内存（`:345`），handler 再整块 `w.Write`（`internal/gatewayapi/inference_resources.go:389`）
- `maxInferenceResourcesResponseBytes = 32 << 20`（`internal/provider/openai/inference_resources.go:17`）是目前**唯一**的写入上界，且是 adapter 私有常量而非网关策略

落盘 500 MB，取的时候照样整块进堆。要通过，得先把 `FileContent` 改成 `io.ReadCloser` 并给读写两侧同时设界。安全角色还指出后果不止"磁盘满"：data 目录同时承载 ledger WAL 与 audit，写不进去会让记账权威与审计 fail-closed，一个故障上游可以用一条无限响应打死实例。

### 2. 不能跟随上游返回的 `results_url`（安全、核心逻辑）

`results_url` 是**上游返回的 URL**，而 SafeTransport 的全部意义就是约束出站目标。主机允许清单、逐 IP 校验、禁止重定向在拨号期仍然生效（`internal/safetransport/transport.go:152`、`:170-174`、`:80-83`），但 `RequireHTTPS`、userinfo、端口归一三道闸只在 `ValidateURL` 里，client 不查；且策略只锁 host，路径不受任何约束——被攻陷的上游返回同域另一条路径，Halro 会带着 Provider 凭据去打它。

现状是干净的：Anthropic 适配器所有 URL 都由配置 endpoint 拼接（`internal/provider/anthropic/adapter.go:98-112`），从不拨号上游给的 URL。

**结论**：自己拼 `{endpoint}/v1/messages/batches/{id}/results`，`results_url` 只用于一致性校验。

### 3. 结果是出站方向，必须过脱敏，现在完全不过（安全、核心逻辑）

批处理输入 JSONL 走的是入站脱敏（`inference_resources_store.go:196-202`）；结果是 provider 输出，属于出站方向，而 batch 路径只脱敏 metadata 与 `RawErrors`（`:649-663`），`DownloadFile` 也不脱敏（`:330-352`）。

落盘等于把模型输出持久化到一次性响应路径之外——CLAUDE.md 那条不变量的字面违反。`internal/redaction` 已有 `StreamInspector`（`inspect.go:125-190`）可按行使用。

**提案人完全没有想到这一条。**

### 4. `expired` 映射与生命周期冲突（安全、核心逻辑、可用性）

`expired` 是 `ExpiryReapable` 的终态之一（`internal/domain/models.go:88-93`）。若结果已经拉到本地，映射成 `expired` 会让回收器删掉客户仍可下载的数据。而 `CleanupExpiredProviderResource` 对非 file kind 只删记录、不删对象文件（`inference_resources_store.go:442-443`），结果对象会永久残留。

另需：映射为 `expired` 时同时清空 `OutputFileID`，否则客户端拿到一个取不到的 id；`ended` 需按 `request_counts` 分解为 completed/failed/cancelled，不能一律 completed。

### 5. 惰性拉取会被网关自己掐断（核心逻辑、可用性）

`GET /v1/batches/{id}` 被 `routeTimeout` 包住（`internal/gatewayapi/inference_resources.go:423`），默认 **2m0s**（`internal/config/default.yaml`）。调大它的代价是运维级的：`server.shutdown_timeout` 必须 ≥ `route_total_timeout`，提到 15 分钟意味着每次重启最长等 15 分钟。

且官方 SDK 默认 `max_retries=2` 且对 GET 重试，一次超时会被放大成三次完整重下。仓库自带的兼容性 harness 恰好用 `max_retries=0`，永远看不到这个行为。

## 真实分歧（未收敛）

**第 1 条**：架构认为不该为"用户少等一次"往单进程里塞隐式写者——整个资源路径没有任何后台 worker，TTL 清理也是被动调用；可用性认为应改为提交时登记、后台拉取、GET 只读本地状态，一次解掉超时、重复拉取、轮询惊讶三件事。

两边都成立。这说明这条决定问的问题不对：它依赖于资源模型先定下来。

## 比三条决定更根本的问题（架构角色）

`CreateBatch` 第一行就是 `s.fileOwner(ctx, key, call.InputFileID)`（`inference_resources_store.go:501`），要求输入是一条 owner adapter 实现了 `ResourceInferenceResourcesAdapter` 的 Halro 文件资源；而 `anthropic.messages.2023-06-01` 的 Operations 没有 files（`internal/provider/profile.go:368`）。

**"北向保持 OpenAI 形状"这个已拍板的决定，在当前资源模型下走不通**，除非发明"只有本地副本、上游没有孪生"的文件资源——而 `domain.ProviderResource` 没有这个概念：`GetFile` 无条件调上游（`:318`），`CleanupExpiredProviderResource` 无条件去上游删（`:480`）。

三条实施决定都建立在这个不成立的模型上。

## 顺带确认的既有缺陷（三个角色独立撞上）

`OutputFileID` 在整个非测试代码里只出现一次——结构体字段定义（`internal/provider/inference_resources.go:95`）。`GetBatch` 只重写 `result.ID`（`inference_resources_store.go:619`），`input_file_id`/`output_file_id`/`error_file_id` 原样透出上游的 `file-...`，而 `fileOwner` 只认 Halro id（`:290-296`）。

**OpenAI 的批处理取结果今天就是断的**，且与 manifest 自己的声明矛盾——`openai.batches.get.v1` 同时写着 `output_file_id` 在响应字段里、以及"resource identifiers are opaque Halro identifiers scoped to one project"。

与 Anthropic 无关，已列为 [`docs/todo/provider-adaptation-gaps.zh-CN.md`](../../todo/provider-adaptation-gaps.zh-CN.md) 的 #0。

## 未被认真评估的替代方案

架构角色提出：直接暴露原生 `POST /v1/messages/batches`。`internal/provider/profile.go:119-121` 的 `nativeOperationPrimitive` 已是原生直通的先例。这条路省掉整层——不需要发明本地文件资源、不需要 JSONL ↔ inline 转换、不需要把结果落盘再伪装成 `output_file_id`。代价是 SDK 不通用。

**应在 ADR 里被显式否决或采纳，而不是默认跳过。** 这条意见已被接受。

## 这轮评审值得记住的

三条决定是提案人在读过两家 API 文档、核实过 Halro 保存文件字节之后提出的，自认为已经做足功课。四个角色独立评审后，一条四票全否、一条出现真实分歧、一条全是有条件通过，另有一个比三条都根本的问题和一个已发布的缺陷。

其中脱敏那条最能说明问题：它不是细节，是这个项目的核心不变量之一，而提案人在三条决定里一个字都没提到。**独立评审的价值不在于挑错，在于挑出提案人根本没有想到的那一类。**
