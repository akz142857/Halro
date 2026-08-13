# Provider 适配缺口 — 待决与待建

状态：**#0 待修（已确认的既有缺陷）；#3 设计已推翻、待重做；#4 已决定不做；#1/#2 阻塞于凭据**
建立日期：2026-08-13
来源：[`docs/review/260813`](../review/260813/README.md) 摸底的 #2、#4、#5、#6
范围：`internal/provider/{anthropic,bedrock}`、`internal/compatibility`、`internal/gateway`、`docs/compatibility`

原本四项都不是缺陷，是**已知且当前可接受的能力边界**。记录下来是为了让"要不要做"成为一次明确的决定，而不是每次评审重新发现一遍。

2026-08-13 的四角色评审推翻了 #3 的设计，并顺带确认了一处**已发布的缺陷**，现列为 #0。执行顺序因此变成 **#0 → 资源模型决定 → #3 重提**：后两步都建立在第一步之上，颠倒顺序会在一个不成立的模型上做设计。

先读一遍前置约束，四项里有三项受它约束：

> Don't widen Gemini/Bedrock Beta capability limits or make Azure API versions implicit without deliberate contract review — they're pinned on purpose.（`CLAUDE.md`）

对应的中文表述在 [`docs/guides/aws-surface-selection.md`](../guides/aws-surface-selection.md)：「这些上限是刻意钉死的。目录、上游元数据和管理员声明都不得放宽 Beta Profile 的上限；放宽属于需要独立契约评审的决定，不能作为别的改动的副作用发生。」

---

## 0. 批处理的文件标识符从未被翻译（既有缺陷，先修）

**这一条不是能力边界，是缺陷，而且与 Anthropic 无关。**

`OutputFileID` 在整个非测试代码里只出现一次——结构体字段定义（`internal/provider/inference_resources.go:95`）。没有任何地方读它、翻译它、把它登记成 Halro 资源。`GetBatch` 只重写 `result.ID = resource.ID`（`internal/gateway/inference_resources_store.go:619`），于是 `input_file_id`、`output_file_id`、`error_file_id` 全部原样透出上游的 `file-...`。

两个后果：

1. **拿返回的 `output_file_id` 去 `GET /v1/files/{id}/content` 必然 404。** `fileOwner` 只在本 Project 的资源桶里按 Halro id 查（`:290-296`）。也就是说 OpenAI 批处理"取结果"这条路今天是断的。
2. **违反 manifest 自己的声明。** `openai.batches.get.v1` 同时写着 `output_file_id` 在响应字段里、以及"resource identifiers are opaque Halro identifiers scoped to one project"（`docs/compatibility/endpoint-manifests.json`）。两句话不能同时为真，且泄漏了上游标识符。

**为什么排在最前**：#3 的整个形状决定建立在"调用方用 `output_file_id` 下载结果"之上。这条路对 OpenAI 都不通，对 Anthropic 更谈不上。不修它，#3 无论怎么设计都落不了地。

**修的方向**（尚未动工）：批处理结束时把上游结果文件登记为一条 Halro `ProviderResource`，翻译 id 后再返回。与下面的资源模型决定相关，但不依赖它——OpenAI 侧的结果文件在上游是有孪生的。

---

## 1. Bedrock Converse 的工具调用

**现状**：`bedrock.runtime.converse.text.v1` 的能力上限硬编码为 `{Chat, Streaming, StreamUsage}`（`internal/provider/bedrock/adapter.go:296`），`tools`/`tool_choice`/`parallel_tool_calls`/`response_format`/`reasoning_effort` 在 Provider I/O 之前被拒绝（`internal/compatibility/provider_fields.go:44-53`）。

**定位已经写清**，不需要补文档：`docs/guides/aws-surface-selection.md` 的「能力面的差别」一节明确说 Converse 被"刻意钉死在纯文本对话"，并列出了八个 Profile 的能力对照表。

**做的话要做什么**：
- 请求侧 `tools` → Converse `toolConfig`，`tool_choice` → `toolChoice`
- 响应侧 `toolUse` 内容块 → `semantic.ContentToolCall`
- 流式 `contentBlockStart` 携带 `toolUse` 的分片累积（与现有 `eventstream.go` 的解析衔接）
- 放宽 `adapter.go:296` 的能力上限与 `provider_fields.go` 的拒绝列表 —— **这一步是契约放宽**

**决定前需要的信息**：Converse 的 `toolConfig` 在不同基础模型上的支持度不一致（Anthropic 系、Nova 系、Llama 系各不相同）。放宽是 Profile 级的，而支持度是模型级的——需要先确定是按 Profile 放宽（简单，但会让不支持工具的模型收到运行时错误）还是引入模型级能力证据（复杂，但与既有的能力证据机制一致）。

**阻塞项**：需要一份 Bedrock Runtime 的 SigV4 凭据才能验证。当前手头只有 Mantle 的 API key，Runtime 侧无法跑真实冒烟——在这个前提下放宽能力上限，等于声明一个没有证据的能力面。

---

## 2. 非文本模态的第二供应商

**现状**（`docs/compatibility/endpoint-manifests.json`）：

| 能力 | 南向实现 |
|---|---|
| 语音合成 / 转写 / 审核 / 文件 / 批处理 | 仅 OpenAI |
| 重排 | 仅 Bedrock Cohere Rerank 3.5 |
| 异步视频 | 仅 Bedrock Nova Reel |
| 图片生成 | OpenAI + Bedrock Titan Image V2（唯一有两家的） |

**问题的实质不是"少一家"，是没有故障转移余地**：上游或凭据出问题时，这些能力直接不可用，路由无处可去。文本对话则有九个 Profile 可以承接。

**做的话要先决定做哪一类**，四类的代价完全不同：
- 图片：已有两家，补第三家收益最低
- 语音：需要新集成（候选：Azure OpenAI 的同名端点最省事，共用 OpenAI 线协议）
- 重排 / 异步：需要新上游，且这两个北向端点本身是 Halro 扩展，没有事实标准

**建议的最小动作**：语音与审核走 Azure OpenAI —— 它们与 OpenAI 共享线协议，`openai.media-resources.v1` 的适配器已经支持 Azure 模式（`internal/provider/openai/adapter.go` 的 `azure` 分支），需要的是新增一个 `azure-openai.media-resources` Profile 而不是新的适配器。这一条投入产出比最高，且不涉及 Beta 上限。

---

## 3. Anthropic 的 Message Batches 与 Files API

**现状**：Halro 的 `/v1/batches` 与 `/v1/files` 只能路由到 `openai.media-resources.v1`。Anthropic 平台两者都有，均未适配。

**代价**：批处理这个能力在换平台时会消失，而调用方从端点形状上看不出来——他们只会得到路由失败。Anthropic 的 Message Batches 有 50% 成本折扣，这是它最实际的价值。

**做的话要做什么**：
- 新增 Profile（或给 `anthropic.messages.2023-06-01` 增加 Operations —— 需要先定，因为 OpenAI 侧是拆成独立 Profile 的，见 `openai.media-resources.v1`）
- `batches`：创建、查询、取消、结果拉取；Anthropic 的批处理结果是 JSONL 流式下载，与 OpenAI 的 file-id 间接引用不同
- `files`：需要 `files-api-2025-04-14` beta 头，因此要与既有的每连接 beta 令牌允许列表机制衔接（`AllowedAnthropicBetas`）
- 资源标识符仍需按 Project 隔离为 Halro 自己的不透明 ID，与 OpenAI 侧一致

### 3.1 形状决定已被推翻（2026-08-13 四角色评审）

原来的决定是"北向保持 OpenAI 形状（`/v1/batches` 收 `input_file_id`），Anthropic 差异在南向消化"，依据是 Halro 自己保存上传文件的字节。

**这个决定在当前资源模型下走不通。** `CreateBatch` 的第一行就是
`s.fileOwner(ctx, key, call.InputFileID)`（`internal/gateway/inference_resources_store.go:501`），
要求输入是一条 owner adapter 实现了 `ResourceInferenceResourcesAdapter` 的 Halro 文件资源；
而 `anthropic.messages.2023-06-01` 的 Operations 只有 chat/messages 四项，**没有 files**
（`internal/provider/profile.go:368`）。

要走通就必须发明"只有本地副本、上游没有孪生"的文件资源，而 `domain.ProviderResource` 没有这个
概念：`GetFile` 无条件调上游（`:318`），`CleanupExpiredProviderResource` 无条件去上游删（`:480`）。
合成文件会让这两处对着不存在的上游发请求。

### 3.2 两个平台的真实契约（2026-08-13 核对官方文档）

**Files**（`POST /v1/files`，需 `anthropic-beta: files-api-2025-04-14`）

- multipart 字段名 `file`，**没有 `purpose` 参数**（OpenAI 必填）
- 响应：`id`、`created_at`（RFC 3339 字符串）、`filename`、`mime_type`、`size_bytes`、`type: "file"`、`downloadable`、可选 `scope`
- 与 OpenAI `FileObject` 对不上：字节数是 `size_bytes` 不是 `bytes`；时间是 RFC 3339 不是 Unix 整数；没有 `purpose`、没有 `status`

**Message Batches**（`POST /v1/messages/batches`）

- 请求体 `{"requests":[{"custom_id":"...","params":{ ...Messages 参数... }}]}`，**内联，不引用任何文件**
- 响应：`id`、`type: "message_batch"`、`processing_status`（`in_progress`/`canceling`/`ended`）、`request_counts{succeeded,errored,canceled,expired,processing}`、`results_url`（流式 JSONL）、以及 RFC 3339 的 `created_at`/`ended_at`/`expires_at`/`archived_at`/`cancel_initiated_at`

**Files 不是 Batches 的前置**：Anthropic 批处理不引用文件，两者互相独立。那个依赖只存在于 Halro 的北向 OpenAI 形状里，而那个形状引用的是 Halro 自己的存储对象。

### 3.3 三条实施决定的评审结果：无一原样通过

四个角色独立评审，结论如下。完整记录见 [`docs/review/260813/batch-design-review.zh-CN.md`](../review/260813/batch-design-review.zh-CN.md)。

| 决定 | 架构 | 核心逻辑 | 安全 | 可用性 |
|---|---|---|---|---|
| 惰性拉取结果，不引入后台轮询 | 通过 | 有条件 | 有条件 | **不通过** |
| 结果流式落盘、绕开字节上限 | **不通过** | **不通过** | **不通过** | **不通过** |
| `archived`/过期映射为 `expired` | 通过 | 有条件 | 有条件 | 有条件 |

**第二条四票全否**，理由一致且比提案人设想的更硬：这不是"绕开上限"，是把无界缓冲从写路径挪到读路径。
`writeResourceObject` 的入参是 `[]byte`（`:116`），根本不存在流式落盘这条路；`DownloadFile` 用
`os.ReadFile` 全量入内存（`:345`），handler 再整块写出。落盘 500 MB，取的时候照样整块进堆——防线只是
换了位置。要通过，得先把 `FileContent` 改成 `io.ReadCloser` 并给读写两侧同时设界。

**跨角色证实的另外三条**：

- 不能跟随上游返回的 `results_url`（安全、核心逻辑）。它是上游给的 URL，而 SafeTransport 的意义就是
  约束出站目标。应由 Halro 用配置 endpoint 自行拼 `{endpoint}/v1/messages/batches/{id}/results`，
  `results_url` 只用于一致性校验。
- **结果是出站方向，必须过脱敏，现在完全不过**（安全、核心逻辑）。落盘等于把模型输出持久化到一次性
  响应路径之外，是 CLAUDE.md 那条不变量的字面违反。提案人完全没想到这一条。
- 惰性拉取会被 `routeTimeout`（默认 2 分钟）掐断（核心逻辑、可用性），且官方 SDK 默认
  `max_retries=2` 会把一次超时放大成三次完整重下。

**一处真实分歧**：架构认为不该为"用户少等一次"往单进程里塞隐式写者；可用性认为应改为提交时登记、
后台拉取、GET 只读本地状态。两边都成立，说明这条决定问的问题不对——它依赖于资源模型先定下来。

### 3.4 未被认真评估的替代方案

架构角色提出：**直接暴露原生 `POST /v1/messages/batches`**。`internal/provider/profile.go:119-121`
的 `nativeOperationPrimitive` 已是原生直通的既有先例。这条路省掉整层：不需要发明本地文件资源、不需要
JSONL ↔ inline 转换、不需要把 `results_url` 落盘再伪装成 `output_file_id`。代价是 SDK 不通用。

**它应当在 ADR 里被显式否决或采纳，而不是默认跳过。** 这条意见已被接受。

### 3.5 重排后的执行顺序

1. **修 #0**（批处理文件标识符翻译）——独立、有明确正确答案，且是本项的前置
2. **定资源模型**：文件资源是否必须有上游孪生？ADR 级决定，连同 §3.4 的原生直通方案一并裁决
3. **重提三条实施决定**——在 1、2 有答案之后。当前三条已作废，不要在它们之上继续设计

## 4. Bedrock 固定模型 Profile 的模型 pin

**现状**：四个 Profile 各写死一个模型 ID（`internal/provider/bedrock/invoke_titan_embedding.go:65-70`），`ValidateProfileModel` 强制一致，枚举也直接从 pin 返回单条而不问上游。

**这是有意的**，理由写在 `internal/provider/bedrock/models.go:42-51`：一个只接受一个模型的 Profile 去列一百个基础模型，等于给操作者九十九个建不出来的选项。

**代价**：Bedrock 每次上新模型，Halro 需要一次发版。

**做的话**：让这四个 Profile 接受操作者指定的模型 ID，仍然走能力过滤与请求体校验。但 Titan Embed 的请求体形状（`inputText` + `dimensions` 枚举）、Titan Image 的形状、Nova Reel 的异步 S3 输出各不相同——放开模型 ID 但保留请求体形状，等于假设新模型沿用同一形状，而这在 Bedrock 上并不成立。真正要做的是"按模型族选择请求体形状"，那是一个比放开 pin 大得多的改动。

**结论倾向**：保持现状。发版成本低于形状猜错的成本。如果要动，正确的方向是每个模型族一个 Profile（现在就是这么做的），而不是放开 pin。

---

## 决策记录

| 项 | 决定 | 状态 | 日期 |
|---|---|---|---|
| 0. 批处理文件标识符未翻译 | 既有缺陷，与 Anthropic 无关；是 #3 的前置 | **待修，排第一** | 2026-08-13 |
| 1. Converse 工具 | 阻塞：需要一份 Bedrock Runtime 的 SigV4 凭据才能验证，另需决定按 Profile 放宽还是按模型级能力证据 | **触发条件**：凭据到位 | 2026-08-13 |
| 2. 第二供应商 | 阻塞：最小方案（Azure 语音/审核，复用现有适配器的 azure 分支 + 一个 media-resources Profile）需要 Azure OpenAI 凭据 | **触发条件**：凭据到位 | 2026-08-13 |
| 3. Anthropic Batches | 形状决定已被四角色评审推翻；三条实施决定作废 | **待重做**，排在 #0 与资源模型决定之后 | 2026-08-13 |
| 3b. Anthropic Files | 与 Batches 无依赖关系，独立评估 | 未排期 | 2026-08-13 |
| 4. Bedrock 模型 pin | **不做。** 放开 pin 但保留请求体形状，等于假设新模型沿用同一形状——Titan Embed、Titan Image、Nova Reel 的请求体各不相同，这在 Bedrock 上不成立。正确方向是每个模型族一个 Profile，现在就是这么做的；发版成本低于形状猜错的成本 | **已关闭** | 2026-08-13 |
