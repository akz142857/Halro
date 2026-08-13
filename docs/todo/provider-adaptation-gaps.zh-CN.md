# Provider 适配缺口 — 待决与待建

状态：**待决策**，四项均未开工
建立日期：2026-08-13
来源：[`docs/review/260813`](../review/260813/README.md) 摸底的 #2、#4、#5、#6
范围：`internal/provider/{anthropic,bedrock}`、`internal/compatibility`、`internal/gateway`、`docs/compatibility`

这四项都不是缺陷，是**已知且当前可接受的能力边界**。记录下来是为了让"要不要做"成为一次明确的决定，而不是每次评审重新发现一遍。

先读一遍前置约束，四项里有三项受它约束：

> Don't widen Gemini/Bedrock Beta capability limits or make Azure API versions implicit without deliberate contract review — they're pinned on purpose.（`CLAUDE.md`）

对应的中文表述在 [`docs/guides/aws-surface-selection.md`](../guides/aws-surface-selection.md)：「这些上限是刻意钉死的。目录、上游元数据和管理员声明都不得放宽 Beta Profile 的上限；放宽属于需要独立契约评审的决定，不能作为别的改动的副作用发生。」

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

**决定前需要的信息**：两家的批处理语义差异有多大——OpenAI 的批处理输入是一个已上传文件的 ID，Anthropic 是请求数组内联。北向 `/v1/batches` 当前是 OpenAI 形状，要么给 Anthropic 做一层转换（可能失真），要么承认这个端点只服务 OpenAI 形状的输入而 Anthropic 走另一个端点。这个选择决定了工作量级别。

---

## 4. Bedrock 固定模型 Profile 的模型 pin

**现状**：四个 Profile 各写死一个模型 ID（`internal/provider/bedrock/invoke_titan_embedding.go:65-70`），`ValidateProfileModel` 强制一致，枚举也直接从 pin 返回单条而不问上游。

**这是有意的**，理由写在 `internal/provider/bedrock/models.go:42-51`：一个只接受一个模型的 Profile 去列一百个基础模型，等于给操作者九十九个建不出来的选项。

**代价**：Bedrock 每次上新模型，Halro 需要一次发版。

**做的话**：让这四个 Profile 接受操作者指定的模型 ID，仍然走能力过滤与请求体校验。但 Titan Embed 的请求体形状（`inputText` + `dimensions` 枚举）、Titan Image 的形状、Nova Reel 的异步 S3 输出各不相同——放开模型 ID 但保留请求体形状，等于假设新模型沿用同一形状，而这在 Bedrock 上并不成立。真正要做的是"按模型族选择请求体形状"，那是一个比放开 pin 大得多的改动。

**结论倾向**：保持现状。发版成本低于形状猜错的成本。如果要动，正确的方向是每个模型族一个 Profile（现在就是这么做的），而不是放开 pin。

---

## 决策记录

| 项 | 决定 | 日期 |
|---|---|---|
| 1. Converse 工具 | 记入待办，等 SigV4 凭据到位后重新评估 | 2026-08-13 |
| 2. 第二供应商 | 记入待办，倾向先做 Azure 语音/审核 | 2026-08-13 |
| 3. Anthropic Batches/Files | 记入待办，需先定北向端点形状 | 2026-08-13 |
| 4. Bedrock 模型 pin | 记入待办，倾向保持现状 | 2026-08-13 |
