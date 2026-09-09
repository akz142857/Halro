# BigModel / Z.AI 适配方案 —— 一个 Provider，两套地域契约

状态：**首期代码已实施（实验性）；真实账号门禁待执行**
建立日期：2026-09-08
范围：`internal/domain`、`internal/provider`、`internal/compatibility`、`internal/app`、
`internal/modelcatalog`、`internal/gateway`、`web/src`
上游范围：中国大陆通用 API（BigModel）与海外通用 API（Z.AI）
不在首期范围：GLM Coding Plan、Managed Agents、知识库、Realtime、视频生成
真实账号状态：**没有使用真实密钥；没有运行任何可能计费的请求。**已提供默认 skip 的
`TestBigModelRealSmoke`，本地契约与 fixture 验证不替代真实账号证据。本文除无凭据探测外，
上游结论均来自 2026-09-08 抓取的官方文档与 OpenAPI。

相关：
[Adding a provider platform](../contracts/adding-a-platform.md)、
[Provider 模型选择与能力解析](provider-model-selection-and-capability-resolution.zh-CN.md)、
[Kimi 适配方案](kimi-adaptation-plan.zh-CN.md)、
[MiniMax 适配方案](minimax-adaptation-plan.zh-CN.md)

---

## 0. 决策摘要

建议新增一个 `ProviderBigModel = "bigmodel"`，但把国内版与海外版建成两套
Access Surface 和两组 Provider Profile，而不是一个 profile 配两个默认地址：

| 层级 | 中国大陆 | 海外 |
| --- | --- | --- |
| Provider Type | `bigmodel`（共用） | `bigmodel`（共用） |
| Access Surface | `bigmodel-cn-general-api` | `bigmodel-global-general-api` |
| 默认根地址 | `https://open.bigmodel.cn` | `https://api.z.ai` |
| Chat 路径 | `/api/paas/v4/chat/completions` | `/api/paas/v4/chat/completions` |
| 模型目录 | `/api/paas/v4/models` | `/api/paas/v4/models` |
| Anthropic 路径 | `/api/anthropic/v1/messages` | `/api/anthropic/v1/messages` |
| 计费币种 | 人民币，国内价表 | 美元，海外价表 |

首期 profile：

```go
ProfileBigModelCNChatEmbeddings ProviderProfileID = "bigmodel.cn.chat-embeddings.v1"
ProfileBigModelGlobalChat      ProviderProfileID = "bigmodel.global.chat.v1"
```

完成真实账号验证后再加入：

```go
ProfileBigModelCNAnthropicMessages     ProviderProfileID = "bigmodel.cn.anthropic.messages.v1"
ProfileBigModelGlobalAnthropicMessages ProviderProfileID = "bigmodel.global.anthropic.messages.v1"
```

核心取舍：

1. **共享 Provider Type，不共享地域 profile。**两边 Chat 请求结构大体同构，可以共享一份
   BigModel 方言渲染器；但路径集合、模型集合、错误形状和资源能力不同，能力证据不能串用。
2. **上游模型列表是“谁存在”的权威来源。**两个真实 host 的 `/models` 均已无凭据探测到
   401，而不是 404。连接建立后必须给操作者 Refresh，不以内建模型名单替代上游列表。
3. **内建目录只回答“模型会什么”。**OpenAPI 的 model enum 可以作为能力 seed 的来源，
   不能覆盖账号实际可见列表；未知模型保持 unknown，不按 `glm-*` 名称猜能力。
4. **不能直接当普通 OpenAI-compatible 接入。**上游使用 `max_tokens`、`user_id`、
   `thinking`、`reasoning_effort`，不接受 Halro OpenAI 中间形状的全部字段；原样 marshal
   会发送没有契约依据的 `n`、`seed`、`parallel_tool_calls`、`stream_options`、
   `max_completion_tokens`、`user` 等成员。
5. **Coding Plan 单独评审。**它使用 `/api/coding/paas/v4`，且官方明确限制适用工具/场景，
   不能让普通网关流量因地址相似而消耗订阅权益或违反产品使用边界。

---

## 1. 证据与地域差异

### 1.1 一手来源

本方案使用以下官方来源：

- 国内入口：[BigModel API 快速开始](https://docs.bigmodel.cn/cn/api/introduction)
- 海外入口：[Z.AI Quick Start](https://docs.z.ai/guides/overview/quick-start)
- 国内文档索引：[BigModel `llms.txt`](https://docs.bigmodel.cn/llms.txt)
- 海外文档索引：[Z.AI `llms.txt`](https://docs.z.ai/llms.txt)
- 国内机器可读规范：[BigModel OpenAPI](https://docs.bigmodel.cn/openapi/openapi.json)
- 海外机器可读规范：[Z.AI OpenAPI](https://docs.z.ai/openapi.json)
- 国内 Anthropic 兼容：[Claude API 兼容](https://docs.bigmodel.cn/cn/guide/develop/claude/introduction)
- 海外 Anthropic 地址证据：[Z.AI Claude Code](https://docs.z.ai/devpack/tool/claude)
- 国内流式契约：[流式消息](https://docs.bigmodel.cn/cn/guide/capabilities/streaming)
- 海外流式契约：[Streaming Messages](https://docs.z.ai/guides/capabilities/streaming)
- 国内推理契约：[深度思考](https://docs.bigmodel.cn/cn/guide/capabilities/thinking)
- 海外推理契约：[Deep Thinking](https://docs.z.ai/guides/capabilities/thinking)
- 海外价格：[Z.AI Pricing](https://docs.z.ai/guides/overview/pricing)

OpenAPI 是字段集合与路径集合的首要依据，散文页用于补充默认值、模型约束和产品边界。
两者冲突时不自行挑一个“看起来更新”的版本；把冲突列入真实账号验证并先走保守实现。

### 1.2 两地不是“只差 host”

2026-09-08 的两份 OpenAPI 结构比较结果：

| 项目 | 国内 | 海外 | 结论 |
| --- | ---: | ---: | --- |
| `paths` | 100 | 14 | 海外 14 条全部包含在国内集合内；国内还有大量资源与管理 API |
| `components.schemas` | 309 | 53 | 仅 42 个 schema 名称共有 |
| Chat 文本请求属性 | 16 | 16 | 名称集合一致 |
| Chat 视觉请求属性 | 15 | 15 | 名称集合一致 |
| Chat 音频请求 | 有 | 无 | 国内独有 |
| Embeddings / Rerank / Moderation / Batch | 有 | 无 | 国内独有 |

两边共同的 Chat 主干可以共用代码，但不能共用 profile 证据：

- 国内文本 model enum 为 14 个，海外也是 14 个，但仅 11 个相同。
- 国内独有文本标识符：`glm-5-turbo`、`glm-4-flash-250414`、
  `glm-4-flashx-250414`。
- 海外独有文本标识符：`glm-4-32b-0414-128k`、`glm-4.5`、`glm-4.5-x`。
- 国内视觉 enum 为 9 个，海外为 6 个；`autoglm-phone` 与
  `autoglm-phone-multilingual` 也不是可互换别名。
- 国内 Chat 响应多出 `audio`、`video_result`、`content_filter` 等形状。
- 两份 OpenAPI 的错误 schema 不同；无凭据实测又显示海外实际错误带有文档 schema
  没写的 `error` 外层。因此错误解码必须容忍已观察到的两种合法形状，不能只照抄其一。

所以地域不是 `ProviderInstance.Region` 的展示属性，而是决定 profile、目录作用域和能力上限的
协议维度。把它们合并会让海外连接看见国内模型，并让国内独有资源能力错误落到海外连接上。

### 1.3 无凭据探测

2026-09-08 对公开 host 做了以下无凭据、无计费探测：

| 地域 | 请求 | HTTP | 结论 |
| --- | --- | ---: | --- |
| 国内 | `GET /api/paas/v4/models` | 401 | 模型枚举路径真实存在 |
| 海外 | `GET /api/paas/v4/models` | 401 | 模型枚举路径真实存在 |
| 国内 | `GET /api/paas/v4/models/glm-5.3` | 401 | 单模型描述路径至少经过统一鉴权门 |
| 海外 | `GET /api/paas/v4/models/glm-5.3` | 401 | 同上 |
| 国内 | `POST /api/paas/v4/chat/completions` | 401 | Chat 路径与 Bearer 鉴权门存在 |
| 海外 | `POST /api/paas/v4/chat/completions` | 401 | 同上 |
| 国内 | `POST /api/anthropic/v1/messages` | 401 | Anthropic 路径已上线 |
| 海外 | `POST /api/anthropic/v1/messages` | 401 | Anthropic 路径已上线 |
| 国内 | `POST /api/anthropic/v1/messages/count_tokens` | 401 | token count 路径已上线 |
| 海外 | `POST /api/anthropic/v1/messages/count_tokens` | 401 | token count 路径已上线 |

这只能证明路径存在并受鉴权保护，**不能**证明：

- 国内 key 能否打海外，或反过来；
- `/models` 响应一定是 OpenAI list 形状；
- 单模型路径在鉴权后一定实现，而不是统一网关先鉴权再返回 404；
- Anthropic 端点接受 Bearer、`x-api-key`，还是两者都接受；
- 通用 API key 与 Coding Plan key/权益是否能跨端点使用；
- Chat、Anthropic 两条 wire 上的模型集合相同。

这些都属于片 0 的真实账号门禁，不能从 401 推导。

### 1.4 通用 API 与 Coding Plan 必须隔离

官方给出的通用 Chat 地址是：

- 国内：`https://open.bigmodel.cn/api/paas/v4`
- 海外：`https://api.z.ai/api/paas/v4`

Coding Plan 则使用：

- 国内：`https://open.bigmodel.cn/api/coding/paas/v4`
- 海外：`https://api.z.ai/api/coding/paas/v4`

海外文档明确说明 Coding endpoint 仅供受支持工具与场景使用。Halro 是通用 API 网关，
首期不得把 Coding Plan 做成通用地址下拉项。若未来接入，应新增
`bigmodel-cn-coding-api` / `bigmodel-global-coding-api` surface，并对模型、并发、用途限制、
定价与告警单独评审，不能复用 general surface 的能力快照。

---

## 2. Halro 中的领域建模

### 2.1 Provider Type、Surface 与 Credential

新增：

```go
ProviderBigModel ProviderType = "bigmodel"

SurfaceBigModelCNGeneral     AccessSurface = "bigmodel-cn-general-api"
SurfaceBigModelGlobalGeneral AccessSurface = "bigmodel-global-general-api"

CredentialBigModelAPIKey CredentialScheme = "bigmodel.api-key"
```

使用一个 provider type 的原因是：两地属于同一厂商产品族，Chat 主体字段与响应主干同构，
适配器和方言渲染器应该共享。拆 surface 的原因是：凭据作用域、host、模型目录、能力与计费证据
必须隔离。

不直接复用 `CredentialBearerStatic`。Chat 确实使用 Bearer，但国内官方 Anthropic 示例使用
`x-api-key`，海外 Coding 文档又使用 `ANTHROPIC_AUTH_TOKEN`。一份密钥可能按 profile 需要不同
Header 渲染。`bigmodel.api-key` 表示“BigModel API key 这一秘密”，各 builder 再决定把它放进
`Authorization: Bearer` 还是 `x-api-key`。片 0 验证后若两地、两条 wire 都接受 Bearer，可再评审
是否退回复用 `bearer.static`；验证前不把未知写进类型名。

### 2.2 Profile 设计

第一阶段只注册经过官方 OpenAPI 描述、且现有 Halro 原语能够承载的两条 OpenAI Chat profile：

| Profile | Surface | Operations | Primitive |
| --- | --- | --- | --- |
| `bigmodel.cn.chat-embeddings.v1` | CN general | Chat、ChatStream、Embeddings | BigModel Chat 方言 + OpenAI-shaped Embeddings |
| `bigmodel.global.chat.v1` | Global general | Chat、ChatStream | BigModel Chat 方言 |

国内 Embeddings 请求与响应是 `model + input (+ dimensions)` → `data[].embedding + usage`，
与 Halro 现有 OpenAI embedding primitive 同构；海外 OpenAPI 没有该路径，所以不能随 Chat 共用。

Anthropic profile 只有在片 0 真实验证通过后才注册。不要先注册再 `Withheld`：
`TestNoNativeProfileIsWithheld` 正是为了阻止“控制台不可见但 native 表已经承诺”的半接入状态。

后续资源 profile 不与 Chat profile 混写：

| 候选 Profile | 地域 | 候选能力 | 是否首期 |
| --- | --- | --- | --- |
| `bigmodel.cn.openai-resources.v1` | 国内 | Images、Transcriptions、Speech、Files、Batches、Moderations | 否，逐端点实测后拆分或合并 |
| `bigmodel.cn.rerank.v1` | 国内 | Rerank | 否，需新南向 primitive/adapter |
| `bigmodel.global.openai-resources.v1` | 海外 | Images、Transcriptions、Files | 否，File 语义与 Halro 资源契约需先对齐 |

“在 OpenAPI 里有路径”不等于“现有 OpenAI adapter 可安全复用”。Files/Batches 的 purpose、ID、
生命周期和结果下载必须逐项满足 `ProviderResource` 的 Project 隔离契约；不能因为 JSON 看起来像
OpenAI 就打开能力位。

### 2.3 根 URL 与路径拼接

连接保存根地址，而不是保存 `/api/paas/v4`：

```text
https://open.bigmodel.cn
https://api.z.ai
```

理由是同一地域的一把 key 同时访问两个兄弟前缀：`/api/paas/v4` 与 `/api/anthropic`。
若连接保存 Chat 的完整前缀，Anthropic builder 会把路径错误拼成
`/api/paas/v4/api/anthropic/...`。

实现上为 adapter 显式提供两种路径，而不是从模型名推断：

```go
OperationPathPrefix: "api/paas/v4"
CatalogPathPrefix:   "api/paas/v4"
MessagesPath:        "api/anthropic/v1/messages"
CatalogPath:         "api/paas/v4/models"
```

`internal/provider/openai.Adapter` 目前会故意忽略 `OperationPathPrefix` 来构造模型目录，
这是为 Bedrock Mantle 的 `/v1/models` 特例写的。BigModel 需要新增独立的
`CatalogPathPrefix`（或等价的显式 catalog path），默认空值必须保持现有 provider 行为不变。

`internal/provider/anthropic.Adapter` 目前固定把目录拼成 `/v1/models`。BigModel Anthropic profile
需要从同 host 的 OpenAI 面读取 `/api/paas/v4/models`，所以同样需要显式 `CatalogPath`；
`CatalogShape` 只有在真实响应确认后才能设为 `CatalogOpenAI`。

---

## 3. Chat 契约映射

### 3.1 首期能力上限

建议的连接默认与上限：

| 能力 | 默认 | 上限 | 依据/处理 |
| --- | --- | --- | --- |
| Chat | 开 | 开 | 两地共同路径 |
| Streaming | 开 | 开 | SSE，`[DONE]` 结束 |
| StreamUsage | 开 | 开 | 最后一个 chunk 自带 usage，不依赖 `stream_options` |
| Tools | 开 | 开 | 首期只接 caller-executed function |
| JSONObject | 开 | 开 | `response_format.type=json_object` |
| StructuredOutputs | 关 | 关 | schema 只列 `text` / `json_object`，无 `json_schema` |
| Vision | 关 | 开 | 仅视觉模型；由模型目录/检测给具体模型能力 |
| FetchedImage | 关 | 开 | image URL 与 Base64 均有官方描述 |
| Reasoning | 关 | 开 | 模型间开关与 effort 语义不同，片 3 后按模型开启 |
| DeveloperRole | 关 | 关 | schema 没有 `developer` role |
| ProviderExecutedTools | 关 | 关 | web search/MCP 会产生上游 egress，另行安全评审 |

国内与海外初始 Chat 能力集合相同；差异落在 profile ID 与每个模型的目录条目中。
国内 Embeddings 只加在国内 profile。

### 3.2 必须新增 BigModel 方言渲染器

新增 `internal/compatibility/bigmodel.go`，职责类似现有的 `deepseek.go`、`kimi.go`：

| Halro / OpenAI 中间字段 | BigModel 处理 |
| --- | --- |
| `model`、`messages`、`stream` | 原样 |
| `temperature` | 仅 `0..1`；Halro 允许到 2，越界在 provider I/O 前拒绝 |
| `top_p` | 仅 `0.01..1`；`0` 在 provider I/O 前拒绝 |
| `max_completion_tokens` | 映射为 `max_tokens`，前提是已确认两者都约束“可见输出 + 推理” |
| `max_tokens` | 仅在 thinking 关闭时可安全映射；thinking 开启时语义待实测 |
| `n` | schema 无此成员；只允许省略或等价默认 `1`，不发送 |
| `seed` | 不支持，路由前拒绝 |
| `parallel_tool_calls` | 不支持；不能静默忽略 `false` |
| `stream_options` | 不发送；上游最后一个 chunk 无条件带 usage |
| `response_format=text` | 省略，等价默认 |
| `response_format=json_object` | 原样发送 |
| `response_format=json_schema` | 拒绝 |
| `user` | 改名为 `user_id`，并执行 6–128 字符限制 |
| `messages[].name` | 无对应字段，拒绝 |
| `messages[].role=developer` | 无对应角色，拒绝 |
| `messages[].content[].detail` | 上游无图像 fidelity 成员，非 `auto` 时拒绝 |
| `tool_choice` | schema 只保证 `auto`；`none`、`required`、指定函数均先拒绝 |
| `stop` | 最多 4 个；超过上限先拒绝 |

渲染器使用专门的结构体并 marshal，不能先 marshal `openaiapi.ChatCompletionRequest` 再删字段。
显式结构体能让新增北向字段在编译/测试时暴露，而不是悄悄穿透到上游。

### 3.3 推理不是一个 profile 级开关

BigModel 的推理契约至少有三种模型行为：

1. `GLM-5.3` / `GLM-5.3-Flash`：OpenAPI 写明只能开启，深度为 `low/high/max`。
2. `GLM-5.2`：支持完整拼写，但文档明确把 `low/medium` 映射为 `high`、把 `xhigh`
   映射为 `max`、把 `none/minimal` 都变成不思考。
3. 更早模型：主要是 `thinking.type=enabled|disabled`，没有可被 Halro 精确表达的深度阶梯。

因此不能在 profile 字段规则里简单写“支持 reasoning_effort”。Halro 的 field rules 只有 profile，
没有目标 model，无法表达 `none` 对 GLM-5.2 可用、对 GLM-5.3 不可用。

分两步落地：

- 片 2 先不在 Defaults 中声明 Reasoning；普通请求不强行发送 `thinking.disabled`，避免直接失去
  只能思考的旗舰模型。所有已证实会在未请求时返回 `reasoning_content` 的模型，在目录中标记
  `ReasonsUnasked=true`，由现有 `filterUnrenderableReasoning` 保护不能承载推理内容的北向端点。
- 片 3 增加“按 invocation target 的字段值约束”，或把等价约束并入现有 capability resolution。
  只有这层能在预算预留前判断某个模型能否精确满足 `none/low/high/...`。禁止在 renderer 阶段
  临时四舍五入 effort，也禁止把 `medium` 静默提高到 `high`。

在目标级约束完成前，Reasoning 只在 Ceiling 中可见但不默认开启；真实探测通过的精确模型可由
operator 声明，且 renderer 对不能精确表达的值 fail closed。

### 3.4 工具与上游执行工具

首期只映射 `type=function`。国内 OpenAPI 还列出 `retrieval`、`web_search`、`mcp`，海外列出
`retrieval`、`web_search`。它们不是普通函数调用：由上游发起检索或网络访问，绕过 Halro 的
`SafeTransport`。

后续若接 `web_search`：

- 复用 `semantic.Tool.Execution=provider` 与 `ProviderExecutedTools`；
- capability 默认关闭，只放在 Ceiling；
- 明确映射上游搜索结果及引用，不能把 `web_search` 顶层数组丢掉；
- MCP 与 retrieval 仍不接，直到 Halro 有上游状态/资源 ID 的所有权模型。

### 3.5 流式与用量

两地文档都给出同一形状：最后一个 SSE chunk 同时携带 `finish_reason` 与 `usage`，随后
`data: [DONE]`。因此：

- `StreamUsage=true` 可以声明；
- 上游请求不能发送 `stream_options.include_usage`，因为 schema 没有该成员；
- 现有 OpenAI SSE decoder 应以 fixture 验证 `reasoning_content`、tool call arguments 分片、
  空 choices、usage-only/finish chunk 与 `[DONE]`；
- `finish_reason` 除标准 `stop/tool_calls/length` 外，还有 `sensitive`、`network_error`、
  `model_context_window_exceeded`，必须映射到语义 termination/error，不能原样漏到只认识
  OpenAI 词汇的层。

用量字段与 Halro 现有 `openaiapi.Usage` 同构：

- `prompt_tokens`
- `completion_tokens`
- `total_tokens`
- `prompt_tokens_details.cached_tokens`

但文档没有独立 reasoning token 数。片 0 必须确认 `completion_tokens` 是否包含
`reasoning_content`；确认前可以按总 output 结算，但不得伪造 reasoning breakdown。

### 3.6 错误、安全审核与 HTTP 200

实现专用错误归类，输入至少包括 HTTP status、`error.code`、`error.message` 与可能出现的
Anthropic-shaped `error.type`。类别需映射到 Halro 的 authentication、rate_limit、bad_request、
provider_5xx、timeout，并正确设置 retryable/ambiguous。

需要真实验证三类危险形状：

1. 是否存在 HTTP 200 携业务错误码；若存在，必须像 MiniMax 一样在结算前拦截。
2. 流中是否出现错误事件，及已输出 token 后错误是否为 ambiguous。
3. 国内 `finish_reason=sensitive` / `content_filter` 是否代表完整失败、部分输出，还是成功但需撤回。

在语义确定前，`sensitive` 应 fail closed，不把被审核拦截的空/半截内容记成成功回答。

---

## 4. 模型枚举与能力目录

### 4.1 枚举路径必须上线

两个 host 都已证明 `/api/paas/v4/models` 存在鉴权门，所以适配器应声明：

```go
CanEnumerate: true
CanDescribe:  // 片 0 验证单模型路径成功后才置 true
CanVerify:    true
```

连接测试优先使用非计费的模型列表，不以生成请求测试凭据。Refresh 返回上游实际对该账号可见的
模型 ID；响应只有 `id/owned_by` 时只建立 availability，不产生 capability claim。

Anthropic profile 也从同地域的 `/api/paas/v4/models` 读取列表，因为上游没有证据表明
`/api/anthropic/v1/models` 是目录权威。是否所有列出的模型都能走 Anthropic wire，要由
真实 route probe 或官方 profile-specific 列表确认；不能给每个模型自动复制 Chat 能力。

### 4.2 内建能力 seed

`internal/modelcatalog/builtin.go` 只加入官方文档能精确支持的稳定 model ID，并分别写在
CN Chat、Global Chat、CN Anthropic、Global Anthropic profile 下。原则：

- 两地共同 model ID 也写两份 entry，因为 profile 与价格/可用性作用域不同；
- OpenAPI enum 证明标识符存在于文档，不证明当前账号有权限；
- 只有 OpenAPI 视觉请求 enum 中的精确 model ID 才可声明 Vision/FetchedImage；纯文本模型不得继承；
- JSONObject、Tools、Reasoning、上下文与输出上限逐模型记录；
- 文档未给或互相冲突的上限留 0，不从相邻型号猜；
- preview/latest/自动路由别名默认不 seed；
- OpenAPI 新出现而本地目录未知的模型会在 Refresh 后出现，但能力保持 unknown。

### 4.3 价格不进入模型目录

国内与海外分别使用人民币和美元价表，且缓存命中、免费型号、限时优惠会变化。Halro 的模型目录
回答能力，不回答价格。价格应继续作为 Deployment 的 versioned pricing 由 operator 配置；
同一 model ID 在两个 surface 上不能共享 price snapshot。

---

## 5. Anthropic Messages 适配

### 5.1 为什么不与 Chat 同批承诺

两地路径都已无凭据探测到 401，但官方可机读 OpenAPI 没有描述该 surface。国内散文页给出了
Anthropic SDK 示例；海外证据主要来自 Coding Plan/工具集成文档。缺少以下关键事实：

- 通用 API 余额是否能使用海外 Anthropic endpoint；
- Bearer 与 `x-api-key` 的接受矩阵；
- `anthropic-version`、`anthropic-beta` 的处理；
- Messages 请求字段子集、content block 子集、stop reason、usage 与 SSE 事件全集；
- `/count_tokens` 成功体是否真的符合 Anthropic 形状；
- Chat 与 Messages 的模型集合是否相同。

所以首期先交付两地 Chat；Anthropic profile 以真实 key 的契约捕获为硬前置。

### 5.2 验证通过后的实现形状

若真实响应符合现有 Anthropic adapter 的严格 decoder：

- `MessagesPath = "api/anthropic/v1/messages"`；
- `CountTokens` 使用同路径 `/count_tokens`；
- 模型目录显式指向 `api/paas/v4/models`；
- `CatalogShape` 以真实 body 决定；
- native schema 在 `internal/compatibility/anthropic/native.go` 单独注册两个 profile；
- `isNativeAnthropicProfile` 同步加入；
- `ProfileSendsAnthropicBetas` 默认 false；没有白名单证据时不转发任何 beta；
- `reasoningProbeEffort` 排除两个 Anthropic-wire profile，避免付费取得 portable decoder
  无法消费的签名 thinking block；
- 为两地分别写字段 manifest，不因为路径相同而共用能力结论。

若响应只“足够 Claude Code 使用”但不满足 Halro 的严格 Anthropic 契约，不应放宽公共 decoder；
应新增 BigModel Anthropic 方言 decoder，明确列出能保真的块与事件。无法保真的 native 字段先拒绝。

---

## 6. 实施切片

### 片 0：真实契约捕获（硬前置，国内/海外各一把测试 key）

每个地域执行并归档脱敏响应：

1. `GET /api/paas/v4/models` 与 `GET /models/{id}`；
2. Chat unary：纯文本、function tool、json_object、缓存命中；
3. Chat stream：文本、reasoning、tool call、usage、异常中断；
4. 推理矩阵：空值、disabled、`none/minimal/low/medium/high/xhigh/max`，至少覆盖 GLM-5.3、
   GLM-5.2 与一个早期模型；
5. `max_tokens` 是否包含 reasoning；
6. 400/401/429/5xx、无效模型、敏感内容；
7. Anthropic unary/stream/count_tokens、两种鉴权头、未知字段与 beta header；
8. 交叉 key：国内 key 打海外、海外 key 打国内，只记录拒绝类型，不重试。

落点：`internal/provider/openai/bigmodel_real_smoke_test.go`、
`internal/provider/anthropic/bigmodel_real_smoke_test.go`。测试必须 opt-in，默认 `go test` 不运行，
并限制调用次数；真实 smoke 不得在普通 CI 中触发。

### 片 1：领域注册与控制台可创建

- `internal/domain/models.go`：Provider Type 与 Validate 分支。
- `internal/domain/provider_profile.go`：两个 surface、credential scheme、首期 profile ID。
- `internal/domain/provider_table.go`：profile rows、Defaults/Ceiling、默认根 URL。
- `internal/provider/primitive.go`：BigModel Chat/Stream primitive；国内 Embeddings 可复用
  OpenAI primitive 或新增有名 alias，选择后固定在测试中。
- `internal/provider/profile_bindings.go`：operation → primitive。
- `internal/app/provider_adapters.go`：authorizer、路径前缀、adapter builder。
- 更新 provider profile golden 与前端 i18n 显示名。

必须通过 `adding-a-platform.md` 列出的 registration guards。

### 片 2：Chat 方言、错误、用量

- 新增 `internal/compatibility/bigmodel.go` 与 table-driven tests。
- `internal/provider/openai.Adapter` 增加 `bigModel` 方言分支与显式 catalog path。
- `internal/compatibility/provider_fields.go` 登记两个 profile 的逐值拒绝规则。
- `internal/compatibility/manifest.go` 覆盖 `/v1/chat/completions`、`/v1/responses`、
  `/v1/messages` 的 portable 路径与损失说明。
- 增加 unary/SSE/error fixtures；验证缓存 token 只计一次。
- 国内 Embeddings 增加 `encoding_format`、`user` 等不支持字段的申报测试。

### 片 3：目标级推理约束与模型目录

- 为精确 model ID 建内建能力 entry，地区分别作用域。
- 标记真实测得的 `ReasonsUnasked`。
- 实现按 invocation target 的 reasoning effort 约束，确保拒绝发生在预算预留前。
- 验证 capability detection 使用 BigModel 能接受的 `max_tokens` 与 thinking 组合，不能沿用
  一律发送 `max_completion_tokens` 的通用探针。

### 片 4：Anthropic Messages

仅在片 0 关闭鉴权、字段、事件和 usage 假设后实施 §5.2。Chat 与 Anthropic profile 同 surface，
因此一把地域 key 可形成一个 connection group；国内与海外仍是两个互不相连的组。

### 片 5：国内资源能力

按收益和契约相似度排序：

1. Embeddings（若未随片 1 完成）；
2. Rerank；
3. Images / Transcriptions / Speech / Moderations；
4. Files + Batches（必须一起完成 Project ID 翻译与结果下载闭环）；
5. 其余异步、多媒体、知识库与 Agent API。

每个端点先读真实响应，再决定复用 OpenAI adapter 还是新增方言。海外只对其 OpenAPI 真正列出的
子集另行实施，不能从国内完成状态继承。

### 片 6：运维与发布

- 控制台 profile 文案明确“中国大陆通用 API”与“海外通用 API（Z.AI）”。
- base URL 默认填根 host，并解释路径由 profile 固定；自定义反向代理仍需遵守 endpoint policy。
- 凭据表单提示两地账户/key/余额需分别确认，不宣称可互通。
- 指标 label 只使用有界的 provider/profile/error class，不放 model ID。
- `docs/verification/provider-real-matrix.md` 增加两地、两条 wire 的证据栏。
- 更新用户指南、兼容性 manifest 产物、provider profile golden。

---

## 7. 验证策略

### 7.1 迭代期最小测试

按改动范围运行：

```bash
go test ./internal/domain/ -count=1
go test ./internal/compatibility/ -count=1
go test ./internal/provider/openai/ -run 'TestBigModel|Test.*Catalog' -count=1
go test ./internal/provider/anthropic/ -run 'TestBigModel' -count=1
go test ./internal/app/ -run 'TestEveryReachableProfile|TestEveryOfferedProviderType|TestProviderProfilesGolden' -count=1
```

推理路由改动再运行：

```bash
go test ./internal/gateway/ -run 'Test.*Reasoning|Test.*Capability' -count=1
go test ./internal/app/ -run 'Test.*ReasonsUnasked|Test.*ReasoningReachability' -count=1
```

控制台只改文案/CSS 时按仓库策略做 focused visual check；组件逻辑变化才跑对应 Vitest 与 typecheck。

### 7.2 推送前完整门禁

只在最终推送前运行一次：

```bash
go test ./... -count=1
cd web && npm run typecheck && npm test && npm run build
git diff --exit-code -- internal/webui/dist
```

`web/` 变化后，生成的 `internal/webui/dist` 必须与源码同 commit 提交。

### 7.3 必测行为矩阵

| 场景 | 预期 |
| --- | --- |
| CN Refresh | 只显示 CN 账号实际模型；未知能力为 unknown |
| Global Refresh | 只显示 Global 账号实际模型；不混入 CN 独有 model enum |
| `temperature=1.5` | provider I/O 前拒绝 BigModel target |
| `top_p=0` | provider I/O 前拒绝 |
| `n=1` | 省略上游字段且语义不变 |
| `n=2` | 路由前拒绝 |
| `parallel_tool_calls=false` | 路由前拒绝，不能按默认并行执行 |
| `json_object` | 成功并验证 JSON |
| `json_schema` | 不路由到 BigModel |
| image URL | 仅视觉能力模型可路由；上游自行 fetch |
| stream usage | 最后 chunk 结算一次，缓存命中不重复累计 |
| sensitive finish | 不记成成功空响应 |
| GLM-5.3 未请求推理 | reasoning 能被 Chat 返回；不能送往无法渲染的北向端点 |
| GLM-5.3 `none` | 预算预留前拒绝 |
| CN key + Global surface | 明确 authentication 失败，不自动换 host |

---

## 8. 风险与未关闭假设

| 风险/假设 | 当前证据 | 关闭方式 | 未关闭时策略 |
| --- | --- | --- | --- |
| `/models` 为 OpenAI list 形状 | 只有 401，未见成功 body | 两地真实 key 捕获 | 不启用枚举 decoder |
| `/models/{id}` 可描述 | 只有 401 | 两地真实 key | `CanDescribe=false` |
| 两地 key 不互通 | 平台、key 管理和计费入口分离；尚未交叉实测 | 片 0 交叉请求 | UI 提示分别配置，不自动回退 |
| Anthropic 通用 API 可用 | 国内有兼容指南；海外主要是 Coding 文档 | 两地普通余额 key | 不注册 Anthropic profile |
| Anthropic 鉴权头 | 文档口径不同 | Bearer/x-api-key 矩阵 | 使用专用 credential scheme |
| `max_tokens` 包含 reasoning | 文档未拆 reasoning usage | 低上限推理实测 | 不映射双输出上限的危险组合 |
| `completion_tokens` 含 reasoning | 只有总量示例 | 与 tokenizer/响应长度对照 | 不伪造 reasoning token 明细 |
| HTTP 200 业务错误 | 未发现，也未排除 | 无效参数/余额不足实测 | 增加可配置 envelope guard 前先按 HTTP 分类 |
| Tool arguments 始终为字符串 | 国内 schema 是字符串，海外页面曾显示对象 | unary + stream tool fixture | decoder 容忍已证实形状，不猜第三种 |
| 价格与 model enum 同步 | 文档页面存在更新时间差 | 发布时重抓 | 价格由 operator 版本化配置 |

---

## 9. 完成定义

首期（CN/Global Chat + CN Embeddings）完成需同时满足：

- 两地 connection 可分别创建、测试、刷新模型，不会绑定错 surface；
- 请求字段严格按 §3.2 渲染，未支持字段在 provider I/O 与预算预留前拒绝；
- unary、stream、tool call、json_object、vision、cache usage 均有 fixture 测试；
- 两地至少各一把真实通用 API key 跑完片 0 的非破坏性矩阵；
- 真实模型列表是 UI 的可刷新来源，内建目录只补能力；
- 错误、敏感终止、429/5xx 的重试与 ambiguous 语义正确；
- 推送前完整 Go/frontend gate 通过，内嵌 bundle 无漂移；
- 文档与 `provider-real-matrix` 记录精确 commit、host、日期和未测项。

Anthropic Messages 不属于“首期 Chat 已完成”的暗含承诺；只有 §5 的独立门禁全部通过后，
对应 profile 才算完成。

---

## 10. 明确不做

- 不把国内和海外压成一个可自由改 host 的 profile。
- 不把 Coding Plan endpoint 当普通 API endpoint。
- 不用 OpenAPI model enum 替代真实 `/models`。
- 不按 `glm-` 前缀或相似名称继承能力。
- 不因 Chat 请求像 OpenAI 就原样 marshal 全部 OpenAI 字段。
- 不把 `json_object` 说成 `json_schema`。
- 不把上游 web search/MCP 当 caller-executed function。
- 不在 Halro 内代替上游抓取图片 URL。
- 不在没有真实 body 时编写“想象中的”model/error/Anthropic decoder。
- 不在普通测试或 CI 中运行真实、可能计费的 provider smoke。
