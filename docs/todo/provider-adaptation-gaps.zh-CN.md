# Provider 适配缺口 — 待决与待建

状态：**#0 已修；资源模型已定（ADR 0021）；#3 代码完成待真实验证；#4 已决定不做；#1/#2/媒体证据阻塞于凭据；#3b 未排期**
建立日期：2026-08-13
来源：[`docs/review/260813`](../review/260813/README.md) 摸底的 #2、#4、#5、#6
范围：`internal/provider/{anthropic,bedrock}`、`internal/compatibility`、`internal/gateway`、`docs/compatibility`

原本四项都不是缺陷，是**已知且当前可接受的能力边界**。记录下来是为了让"要不要做"成为一次明确的决定，而不是每次评审重新发现一遍。

2026-08-13 的四角色评审推翻了 #3 的设计，并顺带确认了一处**已发布的缺陷**，现列为 #0。执行顺序因此变成 **#0 → 资源模型决定 → #3 重提**：后两步都建立在第一步之上，颠倒顺序会在一个不成立的模型上做设计。

先读一遍前置约束，四项里有三项受它约束：

> Don't widen Gemini/Bedrock Beta capability limits or make Azure API versions implicit without deliberate contract review — they're pinned on purpose.（`CLAUDE.md`）

对应的中文表述在 [`docs/guides/aws-surface-selection.md`](../guides/aws-surface-selection.md)：「这些上限是刻意钉死的。目录、上游元数据和管理员声明都不得放宽 Beta Profile 的上限；放宽属于需要独立契约评审的决定，不能作为别的改动的副作用发生。」

---

## 0. 批处理的文件标识符从未被翻译（既有缺陷）— 已修

**这一条不是能力边界，是缺陷，而且与 Anthropic 无关。**

`OutputFileID` 在整个非测试代码里只出现一次——结构体字段定义（`internal/provider/inference_resources.go:95`）。没有任何地方读它、翻译它、把它登记成 Halro 资源。`GetBatch` 只重写 `result.ID = resource.ID`（`internal/gateway/inference_resources_store.go:619`），于是 `input_file_id`、`output_file_id`、`error_file_id` 全部原样透出上游的 `file-...`。

两个后果：

1. **拿返回的 `output_file_id` 去 `GET /v1/files/{id}/content` 必然 404。** `fileOwner` 只在本 Project 的资源桶里按 Halro id 查（`:290-296`）。也就是说 OpenAI 批处理"取结果"这条路今天是断的。
2. **违反 manifest 自己的声明。** `openai.batches.get.v1` 同时写着 `output_file_id` 在响应字段里、以及"resource identifiers are opaque Halro identifiers scoped to one project"（`docs/compatibility/endpoint-manifests.json`）。两句话不能同时为真，且泄漏了上游标识符。

**为什么排在最前**：#3 的整个形状决定建立在"调用方用 `output_file_id` 下载结果"之上。这条路对 OpenAI 都不通，对 Anthropic 更谈不上。不修它，#3 无论怎么设计都落不了地。

**已修（2026-08-13）**，三处：

1. `domain.ProviderResource` 新增 `InputFileID`/`OutputFileID`/`ErrorFileID`，记的是 **Halro 侧**的标识符。放在记录上而不是按需推导，因为登记结果文件是一次写入，而批处理是被轮询的——没地方记住答案，每次轮询都会为同一个上游文件铸一个新标识符。
2. `nameBatchFiles`（`internal/gateway/inference_resources_store.go`）：输入文件从记录里取；结果文件首次见到时登记并记住。**旧记录三个字段皆空的批处理，字段缺席而不是回退到上游值**——不知道是个比正确差、比错误好的答案。
3. `DownloadFile` 对没有本地副本的文件走上游取。批处理产出的结果文件 Halro 从未上传过，翻译出的标识符必须真的能下载，否则只是换了个 404。这条受适配器现有的 32 MiB 响应上限约束，**因此没有偷偷预判 §3.3 里被四票否决的"大结果怎么存"**。

manifest 无需改动：`openai.batches.*` 那句 "resource identifiers are opaque Halro identifiers scoped to one project" **现在才成为真的**。修复让既有声明诚实。

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

1. ~~**修 #0**（批处理文件标识符翻译）~~ **已完成**
2. ~~**定资源模型**：文件资源是否必须有上游孪生？~~ **已决定：可以没有**（
   [ADR 0021](../adr/0021-provider-resource-upstream-twin.md)，Accepted）。
   `UpstreamID` 为空是正常状态，不是例外——批处理是一种模态，谁能服务它是运营者配置的属性，
   回答这个问题是网关的职责而不是调用方的。供应商形状的端点与路径里写供应商名两种做法都被否决，
   理由记在 ADR 里
3. **重提三条实施决定**——在 1、2 有答案之后。当前三条已作废，不要在它们之上继续设计

## 4. Bedrock 固定模型 Profile 的模型 pin

**现状**：四个 Profile 各写死一个模型 ID（`internal/provider/bedrock/invoke_titan_embedding.go:65-70`），`ValidateProfileModel` 强制一致，枚举也直接从 pin 返回单条而不问上游。

**这是有意的**，理由写在 `internal/provider/bedrock/models.go:42-51`：一个只接受一个模型的 Profile 去列一百个基础模型，等于给操作者九十九个建不出来的选项。

**代价**：Bedrock 每次上新模型，Halro 需要一次发版。

**做的话**：让这四个 Profile 接受操作者指定的模型 ID，仍然走能力过滤与请求体校验。但 Titan Embed 的请求体形状（`inputText` + `dimensions` 枚举）、Titan Image 的形状、Nova Reel 的异步 S3 输出各不相同——放开模型 ID 但保留请求体形状，等于假设新模型沿用同一形状，而这在 Bedrock 上并不成立。真正要做的是"按模型族选择请求体形状"，那是一个比放开 pin 大得多的改动。

**结论倾向**：保持现状。发版成本低于形状猜错的成本。如果要动，正确的方向是每个模型族一个 Profile（现在就是这么做的），而不是放开 pin。

---

## 未完成项总览（2026-08-13 收尾）

按"卡在什么上"分类，而不是按优先级——优先级会变，阻塞条件不会。

### 需要一次真实账号运行

| 项 | 需要什么 | 为什么重要 |
|---|---|---|
| Anthropic 批处理端到端 | 已有的 Anthropic 密钥 | 五个切片全部只有假上游验证。同一天里假上游三次没拦住真实缺陷，见[批处理方案 §5](anthropic-batches-plan.zh-CN.md)。**首次运行已走到第 4 步**，前三步真实通过，挡路的四个缺陷都已修复，续跑起点与踩过的坑见[§5.1](anthropic-batches-plan.zh-CN.md) |
| 媒体资源 6 项中的 2 项 | 一把带 `api.files.write` 的 OpenAI 密钥，或把组织角色提到 Writer | 文件与批处理无任何真实证据；现有密钥是模型推理可用、资源写入被拒 |

### 阻塞于手头没有的凭据

| 项 | 需要什么 |
|---|---|
| Bedrock Converse 工具调用（#1） | Bedrock Runtime 的 SigV4 凭据。现有两把 AWS 凭据都是 Mantle，改了也无法验证 |
| 非文本模态第二供应商（#2） | Azure OpenAI 凭据。最小方案是复用现有适配器的 azure 分支加一个 media-resources profile |

### 已知重复：能力上限有两份真相

`domain.DefaultProviderCapabilitiesForProfile`（`internal/domain/models.go`）与
`defaultProviderCapabilities`（`web/src/pages/ProvidersPage.tsx`）各自维护一份"某个 profile 能开哪些
能力"的表，**没有任何东西阻止两者漂移**。

2026-08-13 就漂了一次：后端给 Anthropic 打开 Files/Batches 后，控制台的能力网格里根本不显示这两项，
操作者无法勾选，功能等于不可用。前端那份已补齐，但重复本身还在。

**正确的修法是让 Admin API 直接给出上限，前端不再自己算**——现在 API 只回传已选能力，不回传"可选
范围"。这是一块独立改动，涉及 Admin 响应形状与控制台表单。

没有仓促加"用 Go 测试解析 TSX 文本"的守卫：那种断言脆弱到维护成本可能高过它挡住的问题，而真实的
解法是消除重复而不是监视重复。

### 首次真实运行暴露的两个控制台缺口（2026-08-13）

| 项 | 症状 | 位置 |
|---|---|---|
| 文件对象的 `created_at` 恒为 0 | `POST /v1/files` 返回 `"created_at":0`。北向形状照抄 OpenAI，那里这是文件创建时间戳，客户端会拿它排序和判新旧 | `internal/gateway/inference_resources_store.go` 的本地文件对象构造 |
| 网关密钥不显示剩余有效期 | 密钥过期后调用只回 401，控制台看不出"过期了"还是"配错了"。本次排查是直接读 bolt 才看到 `expires_at` 已过 | 密钥列表与详情 |

两条都是操作者可见面的缺陷，不阻塞批处理续跑。

### 未排期

**Anthropic Files API（#3b）**。与批处理无依赖——Anthropic 的批处理不引用文件，本次实现里的"文件"是
Halro 自持的本地对象。真正的 Files API 价值在 Messages 的文档/PDF 输入，需要 `files-api-2025-04-14`
beta 头，并与既有的每连接 beta 令牌允许列表衔接。

### 已关闭

**Bedrock 固定模型 pin（#4）**：保持现状，理由见 §4。

### 与本文无关但仍开着的

`docs/todo/` 下另有三份早于本次工作的文档：`alert-delivery-design.md`、`dlp-upgrade-plan.zh-CN.md`
（753 行，状态"提案待评审"）、`halro-ha-architecture.zh-CN.md`。它们不在这条链条里。

## 决策记录

| 项 | 决定 | 状态 | 日期 |
|---|---|---|---|
| 0. 批处理文件标识符未翻译 | 既有缺陷，与 Anthropic 无关；是 #3 的前置 | **已修** | 2026-08-13 |
| 1. Converse 工具 | 阻塞：需要一份 Bedrock Runtime 的 SigV4 凭据才能验证，另需决定按 Profile 放宽还是按模型级能力证据 | **触发条件**：凭据到位 | 2026-08-13 |
| 2. 第二供应商 | 阻塞：最小方案（Azure 语音/审核，复用现有适配器的 azure 分支 + 一个 media-resources Profile）需要 Azure OpenAI 凭据 | **触发条件**：凭据到位 | 2026-08-13 |
| 3. Anthropic Batches | 形状经 ADR 0021 定为 A；实施见 [批处理方案](anthropic-batches-plan.zh-CN.md)，切片 1–4 完成 | **实施中** | 2026-08-13 |
| 3b. Anthropic Files | 与 Batches 无依赖关系，独立评估 | 未排期 | 2026-08-13 |
| 4. Bedrock 模型 pin | **不做。** 放开 pin 但保留请求体形状，等于假设新模型沿用同一形状——Titan Embed、Titan Image、Nova Reel 的请求体各不相同，这在 Bedrock 上不成立。正确方向是每个模型族一个 Profile，现在就是这么做的；发版成本低于形状猜错的成本 | **已关闭** | 2026-08-13 |
