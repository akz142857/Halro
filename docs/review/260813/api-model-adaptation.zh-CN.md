# 跨平台矩阵与共性发现

## 一、北向端点：Halro 对外提供什么

路由表在 `internal/app/runtime.go:1303-1331`，兼容性契约在 `docs/compatibility/endpoint-manifests.json`（20 条）。

| 北向端点 | 方法 | 状态 | 可路由到的平台 |
|---|---|---|---|
| `/v1/chat/completions` | POST | compatible | OpenAI、Anthropic、Azure、DeepSeek、OpenAI 兼容、Gemini、Bedrock Converse、Mantle ×3 |
| `/v1/responses` | POST | compatible | 同上 |
| `/v1/messages` | POST | compatible | 同上（原生模式仅限直连 Anthropic 与 Mantle Anthropic） |
| `/v1/messages/count_tokens` | POST | compatible | **仅**直连 Anthropic |
| `/v1/embeddings` | POST | compatible | OpenAI、Azure、OpenAI 兼容、Gemini、Bedrock Titan Embed V2 |
| `/v1/moderations` | POST | experimental | **仅** OpenAI |
| `/v1/images/generations` | POST | experimental | OpenAI、Bedrock Titan Image V2 |
| `/v1/audio/transcriptions` | POST | experimental | **仅** OpenAI |
| `/v1/audio/speech` | POST | experimental | **仅** OpenAI |
| `/v1/files`（create/get/content/delete） | POST/GET/GET/DELETE | experimental | **仅** OpenAI |
| `/v1/batches`（create/get/cancel） | POST/GET/POST | experimental | **仅** OpenAI |
| `/v1/rerank` | POST | experimental | **仅** Bedrock Cohere Rerank 3.5 |
| `/v1/async/invocations`（create/get/cancel） | POST/GET/POST | experimental | **仅** Bedrock Nova Reel |

不提供 `/v1/models`：应用只用 Project 上配置的公开别名寻址模型，404 会附带这句解释（`internal/gatewayapi/handler.go:994-996`）。这是有意的边界，不是缺口。

## 二、模态覆盖矩阵

行是模态，列是平台。"—" 表示该平台本身没有此能力，不是 Halro 的缺口。

| 模态 | OpenAI | Bedrock (Runtime) | Bedrock (Mantle) | Anthropic |
|---|---|---|---|---|
| 文本对话（非流式/流式） | ✅ | ✅ 仅 Converse | ✅ 三种 wire profile | ✅ 原生 + portable 双模 |
| 视觉输入 | ✅ | ❌ 适配器能力上限不含 Vision | ⚠️ 随 wire profile 与操作者声明 | ✅ |
| 工具调用 | ✅ | ❌ **被拒绝** | ✅ OpenAI Chat；⚠️ Responses 流式下被拒 | ✅ 原生模式完整 |
| 结构化输出 | ✅ | ❌ 被拒绝 | ⚠️ Anthropic wire 被拒 | ⚠️ 仅 strict json_schema |
| 推理力度 | ✅ | ❌ 被拒绝 | ⚠️ Responses/Anthropic wire 被拒 | ⚠️ 上限 xhigh（`max` 不可路由） |
| 嵌入 | ✅ | ✅ Titan Embed V2 单模型 | ❌ | — |
| 图片生成 | ✅ | ✅ Titan Image V2 单模型 | ❌ | — |
| 语音合成 | ✅ | ❌ | ❌ | — |
| 语音转写 | ✅ | ❌ | ❌ | — |
| 内容审核 | ✅ | ❌ | ❌ | — |
| 重排 | ❌ | ✅ Cohere Rerank 3.5 单模型 | ❌ | — |
| 异步生成（视频） | ❌ | ✅ Nova Reel 单模型 | ❌ | — |
| 文件 | ✅ | ❌ | ❌ | ❌ **平台有，未适配** |
| 批处理 | ✅ | ❌ | ❌ | ❌ **平台有，未适配** |
| Token 计数 | — | ❌ | ❌ | ✅ |

依据：`internal/provider/profile.go:357-437`（Profile 声明的 Operations）、`internal/compatibility/provider_fields.go:39-118`（能力过滤）、`docs/compatibility/endpoint-manifests.json`（端点覆盖）。

## 三、共性发现

### 【问题】1. 验证覆盖面远小于声明的适配面

GA 发布门禁的真实账号矩阵只跑四个 Profile（`tests/provider-matrix/main.go:52-57`）：

```go
var gaProfiles = []profile{
	{Name: "openai", ...},
	{Name: "azure_openai", ...},
	{Name: "deepseek", ...},
	{Name: "openai_compatible", ...},
}
```

Beta 档只有 Bedrock Mantle 一个（同文件 `:44-50`）。**Anthropic 两个都不在**。

适配器级的真实冒烟测试同样缺席：`internal/provider/{openai,bedrock,bedrockmantle,gemini}/real_smoke_test.go` 都存在，`internal/provider/anthropic/` 没有。

也就是说，三个平台里 Anthropic 是唯一**没有任何自动化真实账号回归**的那个——而它承担的是最复杂的适配（原生/portable 双模、beta 令牌转发、工具按执行位点分类、count_tokens 独占端点）。本次会话中在它身上连续发现四个缺陷，与这个空洞是一致的。

矩阵注释里那句话值得引用（`tests/provider-matrix/main.go:41-43`）：一次 Mantle 运行只证明一个 cell —— commit × region × wire profile × 认证方式 × project 模式。同样的标准套用到 Anthropic，当前证据是零。

### 【更正】2. 非文本能力没有自动探针 —— 其中大部分是有意排除

能力探测计划最多 8 个探针（`internal/provider/capability_detection.go:53-95`），覆盖：`chat`、`streaming`、`stream_usage`、`tools`、`json_mode`、`developer_role`、`vision`、`embeddings`、`moderations`、`rerank`。

不在其中的：图片生成、语音合成、语音转写、文件、批处理、异步生成。

**初稿把这条写成了缺陷，这是误判。** 计划的注释说明它"排除每一个持久化或高成本的 primitive"，而被排除的正好是这两类：图片/语音/转写/异步的一次探测就是一次真实生成，按生成价计费，且是对操作者问到的每个模型都来一次；文件与批处理会在操作者账户上创建一个此处不会删除的对象。自动探测在这两类上是不能做的。

**保留的部分**：这个限制的后果没有被写下来过。这些能力只能停在 `declared` 档，永远拿不到 `verified`，也永远不会因为声明与上游脱节而被自动降级。它们仍然在路由前过滤、不支持的字段仍然被拒绝——缺的只是"自动发现声明与现实不符"。要验证它们，需要的是一个由操作者发起、明确接受成本的动作，那是另一套机制。

已把这段说明补进 `internal/provider/capability_detection.go:44-59`，避免下一个评审者重复提出。

### 【问题】3. 非文本模态是单点适配

每一类非文本能力只有一个南向实现：

- 语音（合成/转写）、审核、文件、批处理：**只有 OpenAI**
- 重排：**只有 Bedrock Cohere Rerank 3.5**
- 异步视频：**只有 Bedrock Nova Reel**
- 图片：OpenAI 与 Bedrock Titan Image V2 —— 唯一有两家的

对一个"多平台网关"来说，这些端点当前没有故障转移的余地：上游或凭据出问题，能力即不可用，路由无处可去。文本对话则相反，九个 Profile 都能承接。

### 【肯定】4. 边界拒绝的方向是对的

三个平台的能力过滤都在 Provider I/O **之前**执行，不支持的字段被拒绝而不是静默丢弃（`internal/compatibility/provider_fields.go`）。这一点在三个平台上一致，且 manifest 中每个端点都显式记录了这条偏差。

同类设计还有：portable 模式会拒绝它无法承载的成员，native 模式原样转发（Anthropic）；Bedrock 控制面枚举需要通过独立的 host 允许列表，失败则回退到手工输入模型而不是静默调用未批准的主机（`internal/provider/bedrock/models.go:52-57`）。

### 【建议】5. "experimental" 当前是一个没有分级的标签

15 个实验层端点共用同一句免责声明："official SDK black-box matrix is not yet validated; current coverage is limited to gateway contracts and provider transport fixtures"。

这句话对 `/v1/images/generations`（两个南向实现、有传输层 fixture）和对 `/v1/async/invocations`（单一固定模型、异步状态机）表达的是同一件事，但两者的风险完全不同。建议把 experimental 拆成"契约已测/SDK 未测"与"整体未验证"两档，或者在每个端点上写明它实际有什么证据——manifest 已经有 `sdk_matrix` 和 `profile_coverage` 字段可以承载这个信息。
