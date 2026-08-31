# MiniMax 适配方案 —— 一次接入三条 wire 形状

状态：**片 2–5 已实施；片 1 与片 6 缺真实密钥，未执行**
建立日期：2026-08-31
范围：`internal/domain`、`internal/provider`、`internal/compatibility`、`internal/app`、
　　　`internal/modelcatalog`、`internal/gateway`、`web/src`
上游核对：2026-08-31 抓取 `platform.minimax.io/docs` 与大陆站 `platform.minimax.cn/docs`（见 §1.1）
　　　　　+ 同日对 `api.minimax.io`、`api.minimaxi.com`、`api.minimax.chat` 三个 host 的无凭据实测（见 §1.3）
地区结论：国际与大陆**只差 API 地址**，契约同构，共用同一组 profile（§1.2、§3.10）
真实账号：**尚无**。本方案 §1 除 §1.3 的实测外，全部是文档转述，未经真实密钥证实
文档评审：2026-08-31 多角色评审（供应商架构 / 账目 / 安全 / 契约 / 交付 / 一致性），
　　　　　结论已并入正文：新增 §3.13–§3.15，扩写 §3.1、§3.8，订正 §2.2、§2.4、§2.5、§2.6、§2.7
**前置依赖**：片 1 是硬前置，它需要一把真实密钥；片 6 的完成条件需要国际与大陆**各一把**。
　　　　　目前一把都没有。
实施说明：2026-08-31 在没有密钥的情况下实施了片 2–5，全部按本方案已定下的**保守假设**落地
　　　　　（未证实的一律按不支持、按 fail-closed）。片 1 的每一条实测项都变成了
　　　　　`TestRealMiniMaxSmoke` 里的一条断言，跑不了但写好了。§8 是实施记录：
　　　　　照方案落的、与方案不同的、以及方案写错被实现推翻的，逐条在那里。
相关：[Adding a provider platform](../contracts/adding-a-platform.md)、
　　　[DeepSeek 适配方案](deepseek-adaptation-plan.zh-CN.md)、
　　　[Amazon Bedrock Mantle](amazon-bedrock-mantle.zh-CN.md)、
　　　[Provider 适配缺口](provider-adaptation-gaps.zh-CN.md)、
　　　[Real-account Provider matrix](../verification/provider-real-matrix.md)

---

## 0. 这份方案要回答什么

MiniMax 与 DeepSeek 的情况相反：DeepSeek 早已接入，方案要回答的是「已经在跑的链哪里与上游不符」；
**MiniMax 一行代码都没有**，`ProviderMiniMax` 这个类型不存在，要回答的是「怎么接」。

但它也不是「再加一个 profile」那么简单，因为 MiniMax 一次带来**三条 wire 形状**：

| 上游文档里的名字 | 路径 | Halro 已有的对应 wire |
|---|---|---|
| Anthropic SDK（官方推荐） / Anthropic API | `POST /anthropic/v1/messages` | Anthropic Messages |
| OpenAI SDK / Chat Completions API | `POST /v1/chat/completions` | OpenAI Chat Completions |
| OpenAI Responses API | `POST /v1/responses` | OpenAI Responses |

三条共用**一个 host、一个 Bearer 密钥**。这个形状在本仓库里有先例，而且只有一个：**Bedrock Mantle**
（`internal/domain/provider_table.go` 里 5 行 Mantle profile，同 `SurfaceBedrockMantle`、同
`CredentialBedrockAPIKey`，靠 profile 而不是靠请求来决定走哪条路线）。所以 MiniMax 要加的不是一个
profile，是**一个 profile 组**——三行表、三份 manifest、三份字段申报、三处端点清单登记。

同时它带来了一件 Mantle 没有的事：**上游会用 HTTP 200 携带错误**。这一条已经实测到（§1.3），
它直接顶在「fail-closed, not fail-open」这条不变量上，也直接顶在账目正确性上，因此排在 §3 第一条。

---

## 1. 上游契约（2026-08-31）

### 1.1 抓取来源

均为 2026-08-31 当日抓取：

- `https://platform.minimax.io/docs/api-reference/api-overview`
- `https://platform.minimax.io/docs/api-reference/text-anthropic-api.md`（Anthropic SDK 接法与限制清单）
- `https://platform.minimax.io/docs/api-reference/text-chat-anthropic.md`（Messages 完整契约）
- `https://platform.minimax.io/docs/api-reference/text-openai-api.md`（OpenAI SDK 接法与不支持项）
- `https://platform.minimax.io/docs/api-reference/text-chat-openai.md`（Chat Completions 完整契约）
- `https://platform.minimax.io/docs/api-reference/responses-create.md`（Responses 契约）
- `https://platform.minimax.io/docs/api-reference/responses-input-tokens.md`（token 预估）
- `https://platform.minimax.io/docs/llms.txt`（文档索引）

大陆站（2026-08-31 补抓，见 §1.2）：

- `https://platform.minimax.cn/docs/api-reference/api-overview`
- `https://platform.minimax.cn/docs/api-reference/text-anthropic-api.md`
- `https://platform.minimax.cn/docs/api-reference/text-chat-anthropic.md`
- `https://platform.minimax.cn/docs/api-reference/text-openai-api.md`

> 这一节是**文档转述**。本仓库对这种转述有过教训（DeepSeek 方案 §1.1：第一版四处不准，其中两处已经
> 是实现缺陷）。所以 §5 的真实账号验证不是收尾，是完成条件；§7 逐条列出哪些结论目前**没有**证据。

### 1.2 三条 Access Surface、两个 host、一份契约

路径与认证：

- Anthropic：`POST /anthropic/v1/messages`。认证接受 `Authorization: Bearer <key>` **或**
  `x-api-key: <key>`；大陆站文档把 Bearer 标为**推荐**，并明写「两者同时出现时 Authorization 优先」。
- OpenAI Chat：`POST /v1/chat/completions`，`Authorization: Bearer <key>`。
- OpenAI Responses：`POST /v1/responses`，`Authorization: Bearer <key>`。
- 模型枚举：`GET /v1/models`（**已实测存在**，见 §1.3）。

**两个 host，一份契约。** MiniMax 按账号注册地分站：

| | 文档站 | API host |
|---|---|---|
| 国际 | `platform.minimax.io` | `https://api.minimax.io` |
| 中国大陆 | `platform.minimax.cn` | `https://api.minimaxi.com`（注意多一个 `i`） |

两站的密钥**不通用**，但**契约同构**：路径、认证头、请求字段、模型清单、响应与错误信封
全部一致——这一点不是转述，是逐条抓取两站文档 + 对两个 host 各打一轮无凭据请求核出来的
（证据见 §1.3）。`https://api.minimax.chat` 也答同样的东西，是旧别名，**不预填**。

对 Halro 的意义：**这里一处新机制都不需要**。base URL 本来就是 operator 可填的，出站 host
允许清单是从已保存的连接派生而非从 `BaseURLTemplate` 派生（见
`internal/domain/provider_table.go` 中 `BaseURLTemplate` 的注释——它是预填，不是约束）。
表里预填 `https://api.minimax.io`，大陆 operator 改成 `https://api.minimaxi.com` 即可，
两边共用同一组 profile、同一份字段申报、同一份端点清单。控制台的连接表单需要在 base URL
那一栏的说明文案里写清两个地址各自对应哪种账号——**这是本条唯一的落地动作**。

> 早先版本据第三方资料写过「大陆 host 的部分接口另需 `GroupId` 查询参数」，因此把大陆站
> 列进了「明确不做的」。**核官方文档后这条不成立**：`platform.minimax.cn` 的
> Anthropic 与 OpenAI 两份接入文档都没有 `GroupId`，请求体只要 `model` 与 `messages`。
> `GroupId` 属于 MiniMax 更早的原生接口世代，与本方案要接的三条 surface 无关。

### 1.3 无凭据实测（2026-08-31，唯一有证据的一节）

对 `https://api.minimax.io` 发无凭据请求，只为判定路由是否存在与错误信封形状，不产生计费：

| 请求 | HTTP | 响应体 |
|---|---|---|
| `GET /v1/models` | **401** | —— |
| `POST /v1/chat/completions` `{}` | **401** | `{"type":"error","error":{"type":"authorized_error","message":"login fail: … (1004)","http_code":"401"},"request_id":"…"}` |
| `POST /v1/responses` `{}` | **401** | 同上形状 |
| `POST /anthropic/v1/messages` `{}` | **401** | `{"type":"error","error":{"type":"authentication_error","message":"login fail: Please carry the API secret key in the 'X-Api-Key' field of the request header"},"request_id":"…"}` |
| `POST /anthropic/v1/messages/count_tokens` `{}` | **401** | —— |
| `POST /v1/responses/input_tokens` `{}` | **401** | —— |
| `POST /v1/embeddings` `{}` | **200** | `{"base_resp":{"status_code":1004,"status_msg":"login fail: …"}}` |

同一轮对大陆 host `api.minimaxi.com` 与旧别名 `api.minimax.chat` 各打一遍，**逐格相同**：
`GET /v1/models` 401；`/v1/chat/completions`、`/anthropic/v1/messages`、`/v1/responses`
三条 POST 全部 401；`/v1/embeddings` 同样 **200 + `base_resp`**。错误体也逐字同形——
OpenAI 路径 `{"type":"error","error":{"type":"authorized_error","message":"login fail: … (1004)","http_code":"401"}}`，
Anthropic 路径 `{"type":"error","error":{"type":"authentication_error",…}}`。

四条结论，都不是推测：

1. **`GET /v1/models` 存在**（401 而非 404）。`internal/provider/openai/adapter.go:292` 的 `Probe`
   与目标枚举正是打这条路径，所以连接测试与模型枚举**可以复用**，不需要为 MiniMax 另写。
2. **`/v1/embeddings` 用 HTTP 200 承载错误**。这不是文档推论，是抓到的响应。它证实了
   §3.1 那条风险在这个 host 上真实存在。
3. **两个 host 的行为在可观测的层面完全一致**：路径集合、状态码、错误信封三项逐格相同。
   这是「大陆与国际只差地址」这个说法目前能拿到的最强证据（仍不覆盖带凭据后的行为，见 §7）。
4. **同一个 host 上错误信封有两种形状**：OpenAI 与 Anthropic 路径返回
   `{"type":"error","error":{…}}`（带 `http_code` 字符串成员），原生路径返回 `base_resp`。
   前者能被 `internal/provider/openai/adapter.go:803` 的 `limitedErrorMessage` 解出
   `error.message` 与 `error.type`（`openaiapi.ErrorEnvelope` 认得这两个成员），**message 与 code
   都取得到**；`http_code` 是多余成员，被忽略，无害。

### 1.4 模型

八个：`MiniMax-M3`、`MiniMax-M2.7`、`MiniMax-M2.7-highspeed`、`MiniMax-M2.5`、
`MiniMax-M2.5-highspeed`、`MiniMax-M2.1`、`MiniMax-M2.1-highspeed`、`MiniMax-M2`。

- 上下文：M3 = 1,000,000；M2.x = 204,800。
- `max_tokens` 上限：M3 ≤ 524,288；M2.x ≤ 204,800。
  M2.x 的输出上限与上下文窗口相等，这**很可能是文档没有单列输出上限**而不是真的能输出满窗，
  内置目录不能照抄（见 §3.12）。
- **只有 M3 支持图片与视频**。M2.x 文档原文：「text and tool-call content blocks only; they do not
  support image or video input」。
- 思考：M3 可用 `thinking: {"type": "disabled" | "adaptive"}` 控制；**M2.x 无法关闭思考**。

### 1.5 逐 surface 的字段差异

**Anthropic Messages（`/anthropic/v1/messages`）**

接受：`model`、`messages`、`max_tokens`、`temperature`[0,2]、`top_p`、`system`、`stream`、
`tools`、`tool_choice`（只有 `{"type":"auto"}` 与 `{"type":"none"}`）、`thinking`（仅 M3）、
`service_tier`（`standard` / `priority`）、`metadata.user_id`。

文档明确**不支持**：`top_k`、`stop_sequences`、Batches API、Files API（M3 视频除外）、
prompt caching、beta headers、`container`、`context_management`、MCP server 参数。
`count_tokens` 仅 M3 部分支持。

内容块：`text`、`image`（仅 M3）、`video`（仅 M3）、`tool_use`、`tool_result`、`thinking`、
`mid_conv_system`。图片/视频来源为 `url`（含 `mm_file://{file_id}`）或 `base64`，另有 `detail`
（`low`/`default`/`high`）、`fps`、`max_long_side_pixel`。

响应与 SSE 事件名与 Anthropic 一致（`message_start` / `content_block_start` /
`content_block_delta`（`text_delta`、`thinking_delta`、`signature_delta`）/ `content_block_stop` /
`message_delta` / `message_stop` / `ping`）。`usage` 有
`input_tokens`、`output_tokens`、`cache_creation_input_tokens`、`cache_read_input_tokens`。

状态码：200/400/401/403/404/413/429/500/**529**（overloaded，可重试）。

**OpenAI Chat Completions（`/v1/chat/completions`）**

文档明确**不支持**：`presence_penalty`、`frequency_penalty`、`logit_bias`、已废弃的
`function_call`；`n` **只接受 1**；不支持音频输入。`temperature` 越界直接报错而非截断。

MiniMax 专有：`thinking`、`reasoning_split`、`service_tier`。多模态用
`image_url` / `video_url` 内容部件。

响应含 OpenAI 没有的成员：`input_sensitive`、`input_sensitive_type`、`output_sensitive`、
`output_sensitive_type`、`base_resp`，助手消息里可能有 `reasoning_content` 与 `reasoning_details`。

**OpenAI Responses（`/v1/responses`）**

必填 `model`、`input`；可选 `instructions`、`max_output_tokens`、`temperature`、`top_p`、
`stream`、`tools`、`tool_choice`（只有 `none` / `auto`）、`reasoning.effort`
（`minimal`/`low`/`medium`/`high`/`none`）、`service_tier`、`metadata`、`prompt_cache_key`。

文档**没有** `previous_response_id` / `store`，会话历史靠 `input` 数组自带 —— 与 Halro 的
「无状态 Responses 门面」（ADR 0005）刚好一致。`usage` 有 `input_tokens`、`output_tokens`、
`total_tokens`、`input_tokens_details.cached_tokens`、`output_tokens_details.reasoning_tokens`。

### 1.6 用量口径（三条 surface 不一致）

| surface | 文档化的 usage |
|---|---|
| Anthropic Messages | `input_tokens` / `output_tokens` / `cache_creation_input_tokens` / `cache_read_input_tokens` —— **齐** |
| Responses | `input_tokens` / `output_tokens` / `total_tokens` / `cached_tokens` / `reasoning_tokens` —— **齐** |
| Chat Completions | schema **只定义了 `total_tokens`**；示例里出现 `prompt_tokens`、`completion_tokens`、`prompt_tokens_details.cached_tokens`，但 schema 未定义 |

这条不对称直接决定 §2.2 里哪个 profile 当锚点。

---

## 2. 落点：六步清单 + 清单之外的五处

`docs/contracts/adding-a-platform.md` 规定了六步，每步都点名了漏掉时会红的测试。下面逐步给出
MiniMax 的具体内容，**再补上该文档没有覆盖、但 Anthropic wire 特有的五处闸门**。

### 2.1 类型、Access Surface、凭据方案

`internal/domain/models.go`（现有类型常量在 15–21 行，校验 switch 在 518 行）：

```go
ProviderMiniMax ProviderType = "minimax"
```

`internal/domain/provider_profile.go`：

```go
SurfaceMiniMax AccessSurface = "minimax-api"
```

**一个 surface 而不是三个**，与 Mantle 同法。理由是 Access Surface 描述的是「这套凭据打到的那个
API 面」，不是 wire 形状；三条路径同 host、同密钥、同账户，拆成三个 surface 会让
`ConnectionProfiles`（`internal/domain/provider_connection.go:82`）把它们分成三个互不相干的连接组，
operator 就得为同一把密钥建三个连接。

凭据方案复用 `CredentialBearerStatic`。**不新增 scheme**：MiniMax 的密钥就是一个 bearer token，
Anthropic 路径也接受 `Authorization: Bearer`（文档记 Bearer 优先）。这一点很关键，因为
`internal/provider/anthropic/adapter.go:60-66` 的 `New` 会校验 authorizer 的 scheme 与
`Options.CredentialScheme` 一致——传 `CredentialBearerStatic` 并用 Bearer authorizer 即可，
Mantle 已经走通了同一条路（`internal/app/providers.go:810-817` 传的是
`binding.CredentialScheme` 而非 Anthropic 默认值）。

> §7 第 1 条：「Anthropic 路径接受 Bearer」有**两站文档互相印证**（大陆站明写 Bearer 为推荐，
> 且与 `x-api-key` 同时出现时优先），但**没有真实密钥打通过**。§1.3 实测到的 401 文案说的是
> `'X-Api-Key' field`，那只是缺任何凭据时的默认提示，不构成对 Bearer 的否证，也不构成证实。
> 片 1 的第一件事就是用真实密钥打一次 Bearer。若不成立，改为
> `x-api-key` 为主、`Authorization` 为备的 authorizer（`NewStaticHeaderAuthorizer` 支持备用头名），
> 并给 MiniMax 单独一个 scheme —— 因为那时 Anthropic profile 与另外两个的凭据形状不同，
> 就不该再共用一个 `ConnectionProfiles` 组。**这一条会改变 §2.2 的分组结论，必须先验。**

### 2.2 三行表与能力上限

`internal/domain/provider_profile.go` 的 profile ID：

```go
ProfileMiniMaxAnthropicMessages ProviderProfileID = "minimax.anthropic.messages.v1"
ProfileMiniMaxChat              ProviderProfileID = "minimax.chat.v1"
ProfileMiniMaxResponses         ProviderProfileID = "minimax.responses.v1"
```

`internal/domain/provider_table.go` 三行，**顺序即凭据解析优先级**（同一 `(type, surface, scheme)`
的组内，`ResolveCredentialProfile` 取第一条）。三条建议顺序如上，**Anthropic 打头**，理由有二：

1. 上游自己把 Anthropic SDK 标为 recommended；
2. 只有它与 Responses 的 usage 口径是齐的，而 Anthropic 面还多出 cache 两档
   —— 锚点 profile 决定了 `AssignConnectionCapabilities` 把 `chat` 给谁
   （`internal/domain/provider_connection.go:245` 的 `homeForCapability`：锚点能服务就归锚点）。

能力集（`Defaults` 与 `Ceiling` 相等，Beta 期不留提权口）：

```go
minimaxAnthropicSet = ProviderCapabilities{
    Chat: true, Streaming: true, Tools: true, Vision: true, FetchedImage: true,
    Reasoning: true, StreamUsage: true,
}
minimaxChatSet = ProviderCapabilities{
    Chat: true, Streaming: true, Tools: true, Vision: true, FetchedImage: true,
    Reasoning: true, StreamUsage: true,
}
minimaxResponsesSet = ProviderCapabilities{
    Chat: true, Tools: true, Vision: true, FetchedImage: true,
}
```

逐条理由：

- **没有 `Embeddings`**。`/v1/embeddings` 存在，但它是 MiniMax 原生形状（`texts` + `type`，
  返回顶层 `vectors`，错误走 `base_resp`），不是 OpenAI 的 `input` → `data[].embedding`。
  声明 `Embeddings` 会让 `internal/provider/openai/adapter.go` 的嵌入原语打上去然后解不出来。见 §3.3。
- **没有 `JSONObject` / `StructuredOutputs`**。三份文档都没有列 `response_format`。**这是「文档没写」
  而不是「文档写了不支持」，属于 §7 的未验证项**；在验证之前按不支持处理（fail-closed）。
- **没有 `DeveloperRole`**。OpenAI 的 `developer` 角色在 MiniMax 文档里没有出现。
- `Vision` + `FetchedImage`：文档给的 `image_url.url` 与 Anthropic `source.type=url` 都由上游去取，
  与 OpenAI/Gemini 同类，不是 Halro 代取。**但只有 M3 支持**，所以这是连接级上限，
  M2.x 的「没有视觉」由模型目录逐条记（与 DeepSeek `withVision` 的处理同理，
  见 `internal/domain/provider_table.go` 中 DeepSeek 那行的注释）。
- Responses 面没有 `Streaming`/`StreamUsage`：**这是 Halro 侧的范围取舍，不是上游的限制**。
  MiniMax 文档明确 `/v1/responses` 接受 `stream`；不能流是因为首次接入选了
  `openaiprovider`（`Responses: true` 那条分支不绑流原语，见 §2.3），而
  `CapabilityDependencies()` 要求 `stream_usage → streaming → chat`。
  代价与另一条可选路径记在 §3.14——**不要**把这句读成「MiniMax 的 Responses 不能流」。
- **没有 `ProviderExecutedTools`**：MiniMax 文档提到 web search 是服务端工具，但把它放进上限意味着
  接受上游绕过 SafeTransport 发起出网。Beta 期不开；要开是一次独立的契约评审
  （CLAUDE.md「不变量」最后一条）。

`providerTypeTable` 加一行，默认 profile 取 Anthropic：

```go
{ProviderMiniMax, ProfileMiniMaxAnthropicMessages, minimaxAnthropicSet},
```

> **一个必须先想清楚的后果**：三个 profile 同组、锚点优先。若 operator 锚在 Anthropic profile 上，
> 而请求带了 `json_object`——两个 peer 都不支持，直接 `Unservable`，没问题。但如果将来给
> Chat 与 Responses 都补上 `JSONObject` 而 Anthropic 面仍然没有，那么锚在 Anthropic 的连接
> 声明 `json_object` 会因为「两个 peer 都能服务、锚点不能」而落进 `Ambiguous` 被拒。
> 这与 Mantle 今天的处境一样，不是缺陷，但要在能力表注释里写明，别让人以为是 bug。

### 2.3 Operation → Primitive 绑定

`internal/provider/primitive.go` 五个新常量：

```go
PrimitiveMiniMaxAnthropicMessages       Primitive = "minimax.anthropic.messages"
PrimitiveMiniMaxAnthropicMessagesStream Primitive = "minimax.anthropic.messages.stream"
PrimitiveMiniMaxChat                    Primitive = "minimax.chat-completions"
PrimitiveMiniMaxChatStream              Primitive = "minimax.chat-completions.stream"
PrimitiveMiniMaxResponses               Primitive = "minimax.responses"
```

**五个，不是六个。** Responses 面首次接入不绑流（§2.2、§3.14），所以
`PrimitiveMiniMaxResponsesStream` **现在不要定义**——一个没有绑定的 Primitive 常量是死代码，
而 `ProfileManifest.Validate` 只校验「绑定的操作已声明」，拦不住一个谁都不用的常量。
§3.14 若定论改走 Mantle 的 Responses 适配器，再连同绑定一起加。

`internal/provider/profile.go:70` 的 `profileAllowsPrimitive` 三行，以及同文件里三份
`ProfileManifest`：

```go
ProfileMiniMaxAnthropicMessages: {
    OperationChat: …Messages, OperationChatStream: …MessagesStream,
    OperationMessages: …Messages, OperationMessagesStream: …MessagesStream,
},
ProfileMiniMaxChat:      {OperationChat: …Chat, OperationChatStream: …ChatStream},
ProfileMiniMaxResponses: {OperationChat: …Responses},
```

Anthropic profile 绑 `OperationMessages` / `OperationMessagesStream` 是为了让北向
`/v1/messages` 端点能落到它上面（与 `ProfileAnthropicMessages`、
`ProfileBedrockMantleAnthropicMessages` 同形）。

`TestCeilingWithinProfileManifestOperations` 会拿 §2.2 的上限来核这里，所以两节必须一起改。

### 2.4 字段申报

`internal/compatibility/provider_fields.go` 的 `generateFieldRules` 三条注册，
漏一条会掉进 legacy 分支——那是「fail-closed 所以看起来像成功」的那种漏
（该文件 33–41 行的注释就在说这件事）。

Anthropic profile 的申报，起点抄 `ProfileAnthropicMessages` 那条，再叠 MiniMax 自己的收窄：

```
messages[].name              — Anthropic 无此成员
messages[].content[].detail  — 仅当值不是 auto（Anthropic source 无 fidelity 提示成员）
messages[].role=developer
n > 1
seed
response_format              — 全部值（§2.2：未证实支持任何 JSON 模式）
reasoning_effort             — 不在 portable 交集内的值
user
stop                         — 【MiniMax 特有】stop_sequences 会被上游静默忽略（§3.7）
```

Chat / Responses 两条以 OpenAI 那两条为起点，叠上：

```
n > 1                        — 文档：只接受 1
seed                         — 未列入接受清单
response_format              — 同上，未证实
stop                         — 未列入接受清单，与 Anthropic 面同因（见下）
parallel_tool_calls          — 仅当显式为 false；true 就是省略该成员的含义
user                         — Chat 面没有终端用户归属成员
（presence_penalty / frequency_penalty / logit_bias 无需申报：
  Halro 的 OpenAI 请求类型根本没有这几个成员，与 DeepSeek 同理，
  见 internal/compatibility/deepseek.go:27-29）
```

**`reasoning_effort` 只在 Responses 面申报，Chat 面不申报。** 第一版把它写在两条共用的那一栏里，
是错的：顶层拼法确实不可达，但 §3.13 的方言渲染器**把它翻译成了 `thinking`**，
所以 Chat 面的 reasoning 是能用的，申报不支持会把这个能力整个关掉。
Responses 面申报它是另一个理由——canonical mapper 保不住 reasoning item（见 §2.2），
那是能力层面的缺失，不是拼法问题。

因为这两条的损失清单不同，它们**各注册一条规则而不是共用一条**：
`generateFieldRules` 的 `register` 按 profile 覆盖写入，同一个 profile 注册两次会是
后一条替换前一条，而不是两条相加——损失会静默地少一半。

**`stop` 三个面都要申报，不是只有 Anthropic 面。** 第一版只在 Anthropic 那一栏写了它，是漏。
`openaiapi.ChatCompletionRequest` 有 `Stop` 成员（`internal/openaiapi/types.go:28`），
而 MiniMax 的 Chat 与 Responses 文档都没把 `stop` 列进接受清单——按 §3.7 立下的那条规律，
**未列入 = 被静默忽略**，不是报错。三个面同一个事实，就得有三处申报：
`TestTheManifestDeclaresEverythingTheRulesRefuse` 只查「拒了但没声明」，
查不出「该拒没拒」，所以这一类漏没有测试兜底，只能靠这一节写全。

它是请求成员的属性、不是目标的属性，所以按该文件的规则属于字段申报而非能力
（`adding-a-platform.md` 第 4 步末段）。

### 2.5 端点清单

`internal/compatibility/manifest.go`：`chatProfiles`（237 行）与 `responseProfiles`
两个列表要加人（`embedProfiles` **不加**），`ProfileCoverage` 要加行的是**三份** manifest，
另有**两份显式不加**：

- `openai.chat-completions.v1` — 三个 profile 全登记
- `openai.responses.v1` — 三个 profile 全登记
- `anthropic.messages.v1` — 三个 profile 全登记，其中 Anthropic profile 额外声明
  「native 模式可用」的转换说明
- `openai.embeddings.v1` — **一个都不登记**（§2.2 无 `Embeddings`）
- `anthropic.count_tokens.v1` — **暂不登记**，见 §3.11

两个方向的守卫都会红：`TestEveryChatProfileAppearsInAnEndpointManifest`（profile 没有端点）与
`TestTheManifestDeclaresEverythingTheRulesRefuse`（§2.4 拒了但这里没声明）。

### 2.6 适配器构造

`internal/app/providers.go` 的 `newProviderBindingAdapterWithClient` 加一个
`case domain.ProviderMiniMax`，三个分支，**复用现有适配器，不新建 `internal/provider/minimax` 包**。

注意这句话的边界：**不新建 provider 包 ≠ 没有 MiniMax 专属代码**。compatibility 层要新增一个
方言渲染器（§3.13），provider 层要新增一层 `base_resp` 后处理（§3.1）——两处都不在
`internal/provider/` 下面新开目录，但都是新写的、只服务 MiniMax 的逻辑。
DeepSeek 走的是同一条路：零 provider 包，一个 `internal/compatibility/deepseek.go`。

```go
case domain.ProviderMiniMax:
    authorizer, err = provider.NewStaticHeaderAuthorizer(
        binding.CredentialScheme, "Authorization", "Bearer ", plaintext, "x-api-key")
    if err == nil {
        switch binding.ProfileID {
        case domain.ProfileMiniMaxAnthropicMessages:
            adapter, err = anthropicprovider.New(anthropicprovider.Options{
                Endpoint: endpoint, Authorizer: authorizer, Client: client,
                Capabilities: capabilities,
                ProviderType: string(domain.ProviderMiniMax),
                CredentialScheme: binding.CredentialScheme,
                MessagesPath: "anthropic/v1/messages",
                ProfileID: binding.ProfileID,
            })
        case domain.ProfileMiniMaxChat, domain.ProfileMiniMaxResponses:
            adapter, err = openaiprovider.NewWithOptions(openaiprovider.Options{
                Endpoint: endpoint, Authorizer: authorizer, Client: client,
                ProviderType: string(domain.ProviderMiniMax),
                CredentialScheme: binding.CredentialScheme,
                Capabilities: capabilities,
                Responses: binding.ProfileID == domain.ProfileMiniMaxResponses,
            })
        default:
            err = errors.New("MiniMax provider profile is not implemented")
        }
    }
```

三点复用依据，都是读过代码而不是推测：

- `anthropicprovider.Options` 已有 `MessagesPath`、`ProviderType`、`CredentialScheme`
  （`internal/provider/anthropic/adapter.go:27-43`），Mantle 正是这么用的。
- `openaiprovider` 的路径拼接对 `/v1/chat/completions` 与 `/v1/responses` 就是默认形状，
  **不需要 `OperationPathPrefix`**（那是 Mantle 的 `/openai/v1` 才要的）。
- `anthropicprovider` 的 `InvocationTargetDiscovery`（同文件 85–91 行）在
  `messagesPath != ""` 时把 `CanEnumerate` 关掉——对 MiniMax 是**对的**：
  MiniMax 没有 Anthropic 形状的 `GET /v1/models`，它的模型列表在 OpenAI 那条路径上。
  枚举由 Chat profile 提供（§1.3 已测该路由存在）。

这一步是六步里**唯一没有测试覆盖**的（`adding-a-platform.md` 原文），漏了会在第一次建连接时
以 `provider profile is not implemented` 炸在运行时。

### 2.7 清单之外的五处（Anthropic wire 特有）

`adding-a-platform.md` 的六步覆盖不到下面这些，它们是「一个 profile 说自己是 Anthropic 面」
才会牵动的开关。**五处按名字列出**（前四处在 Go 侧，第五处在控制台），逐一给出 MiniMax 的答案：

| 位置 | 它决定什么 | MiniMax 的答案 |
|---|---|---|
| `internal/compatibility/anthropic/native.go:18` | 哪些 profile 允许 native 直通模式 | **加入**。native 是 Anthropic SDK 用户的主要价值 |
| `internal/gateway/service.go:1712` `isNativeAnthropicProfile` | `(profileID, surface)` 配对校验 | 加 `ProfileMiniMaxAnthropicMessages && SurfaceMiniMax` |
| `internal/domain/provider_profile.go:382` `ProfileSendsAnthropicBetas` | 能否外发 `anthropic-beta` | **不加**。文档明写 beta headers 不支持 |
| `internal/provider/capability_detection.go:205` `reasoningProbeEffort` | 探测 reasoning 时用哪一档 | Anthropic profile 走 `return "", false`（与另两个 Anthropic 面同）；Chat/Responses 见 §3.5 |
| `web/` 与 golden | 控制台呈现 | `web/src/types.ts:378,401` 两个联合类型、`web/src/pages/ProvidersPage.tsx:47` 的类型列表、`web/src/i18n/locales/en-US.ts:931,1042` 与 `zh-CN.ts:936,1047` 的两处词条、`web/src/test/provider-profiles.golden.json`（用 `HALRO_UPDATE_GOLDEN=1` 重生成，diff 就是评审对象），以及重新 `npm run build` 并提交 `internal/webui/dist` |

**第一行**需要展开（不是第三行——第三行只是「不发 beta 头」这一个决定）：native 模式
**逐字节转发调用方的请求体**。MiniMax 不支持 `top_k`、`stop_sequences`、prompt caching 的
`cache_control`。native 模式下 Halro 不会重写请求体，所以这些成员会原样打到上游，
而上游**静默忽略它们**——不是报错。请求 200 返回、调用方付了钱、语义已经不是它要的那个。
这是 §3.7，必须有处置。

---

## 3. 深度评审：实施前必须先定论的十五条

按「会不会算错账 / 会不会 fail-open」排序，不按工作量。

§3.13–§3.15 是 2026-08-31 多角色评审补的三条，**按严重度它们不该排在末尾**：§3.13 与 §3.1
同级（都会让调用方付钱买到不是自己要的东西），§3.14 是被误写成上游限制的一个 Halro 取舍，
§3.15 是一个「不决定就会被默认决定掉」的归类问题。编号排在后面只是为了不打乱既有交叉引用——
读的时候把 §3.13 当成 §3.2 看。

### 3.1 HTTP 200 携带错误 —— 最高级别，已实测

§1.3 抓到的实证：`POST /v1/embeddings` 无凭据返回 **HTTP 200** 加
`{"base_resp":{"status_code":1004,…}}`。文档同时说明 Chat Completions 的响应体里也有 `base_resp`，
错误码表覆盖 1000/1001/1002/1004/1008/1013/1027/1039/2013。

Halro 现在的 OpenAI 适配器**只看 HTTP 状态码**。一个 200 + `base_resp.status_code != 0` +
`choices` 为空的响应会被当成成功：预留被释放、成本被结算、审计写下一次成功的调用，
而调用方拿到的是一个空回答。这同时踩中两条不变量——「fail-closed, not fail-open」与
「账目权威」。

**处置**：在 MiniMax 的三条 surface 上，**解码后一律先看 `base_resp`**：非零即按上游拒绝处理，
错误类别由码表映射（1002 → rate limit，1004 → authentication，1008 → 余额不足，
1013 → server error，1027/1039 → bad request）。这必然要一层 MiniMax 专属的响应后处理，
是本方案里**唯一必须新写而不能纯复用**的逻辑。

**守卫要放两处，不是一处。** 第一版只写了非流那条，是漏：

- **非流**：`json.Unmarshal` 之后、把结果交给上层之前判一次。这条容易。
- **流式**：一条 SSE 流可能先正常吐几块、再在末块或中途转错并带上 `base_resp`。
  判定点在分块边界上（`internal/sse` 那一层），与非流是完全不同的代码，
  **不能指望非流那一处顺带覆盖**。而且流式还有第二个问题：已经发给调用方的字节收不回来。
  按本仓库既有口径（「下游响应字节对客户端可见之后，重试/回退就不再是隐形的」），
  这时不能改写已发出的内容，只能**终止流并把这次尝试按上游失败结算**。

**结算方式要明写，因为「按上游拒绝处理」不等于「释放预留」。** 一个 200 +
`base_resp.status_code != 0` 可能已经消耗了输入 token。CLAUDE.md 的规则是**模糊的上游结果
保守计账，绝不静默退款**。

实施时发现这不需要新代码：`settlementForResult`（`internal/gateway/service.go:2712`）
已经实现了这条规则，而且**开关就是 `provider.Error` 的 `Ambiguous` 标志**——

- `Ambiguous` 为真且带可信 `usage` → 按 `usage` 结算；
- `Ambiguous` 为真且不带 `usage` → 按预留保守结算，标 `TokenEstimated` / `CostEstimated`；
- `Ambiguous` 为假 → 零成本，因为没有东西跑过。

所以这一节要做的不是写结算逻辑，而是**在 §3.15 的归类表里把 `Ambiguous` 标对**。
1000 / 1001 / 1013 与未知码标为模糊（请求发出去了、结果不明）；1002 限流与 1004 / 1008
鉴权类不标（上游明确拒收，什么都没跑）。

**这里与方案第一版有一处出入，记下来而不是抹掉**：第一版写「只有 1004 走零成本」，
实施时把 1027 / 1039 / 2013 也归成非模糊的零成本。理由是它们是 `bad request` 类——
调用方的请求本身有问题，上游拒收而非生成，与本仓库对所有 4xx 的既有归类一致
（`classifyHTTPError` 的 default 分支）。**这条是判断，不是测量**：若片 1 发现 MiniMax 对
1039 这类也计费，把这三个码改成模糊即可，一行。

**同一条的第二半：`input_sensitive` / `output_sensitive`。**
§1.5 列了这四个成员，第一版没有给处置——补上：`output_sensitive: true` 意味着上游做了内容
过滤，**HTTP 200、`base_resp` 为 0、内容已经被改**。它与 `base_resp` 是同一类「200 但语义变了」，
只是更隐蔽，因为没有任何一个错误码会亮。

处置：**不当成失败**（内容确实生成了、也确实计费了），但要在尝试记录里留下这个事实，
并在 `anthropic.messages.v1` / `openai.chat-completions.v1` 的 `DocumentedDeviations` 里写明
「上游可能在返回成功的同时对内容做过过滤，Halro 转发上游的标记而不重写内容」。
**不要**把它翻译成 `finish_reason: content_filter` ——那是 OpenAI 的语义，
两边的判定口径不一样，翻译过去就是替上游做了一个它没做的声明。

**目前不知道的**：Chat Completions 与 Responses 在真实错误（非鉴权）下**到底是发非 2xx，
还是发 200+`base_resp`**。§1.3 只证明了鉴权失败时这两条路径发的是规矩的 401。
`/v1/embeddings` 证明这个 host 上「200 带错」是存在的做法。**必须用真实密钥各触发一次可控错误
（比如超长输入触发 1039）才能定论**，这是片 1 的验收条件之一，不能靠假上游。
`input_sensitive` / `output_sensitive` 同样没有实测——什么输入会点亮它、点亮时
`choices` 还有没有内容，都要在片 1 里试一次。

### 3.2 Chat Completions 的 usage 只文档化了 `total_tokens`

`internal/openaiapi/types.go:192` 的 `Usage` 有 `PromptTokens` / `CompletionTokens` /
`TotalTokens`。如果上游只回 `total_tokens`，前两个解出来都是 0。输入与输出是**两个价位**，
按 0 结算意味着这次调用的成本记成 0，而 ledger 是账目权威——一次错误会一直错下去。

文档的示例里出现了 `prompt_tokens` 与 `completion_tokens`，只是 schema 没定义。**这是必须实测的一条。**

**处置分两种情况**：
- 若真实响应带齐两档：照常，什么都不做。
- 若只有 `total_tokens`：**不能猜怎么分摊**。按「模糊的上游结果保守计账」的既有规则，
  应把整个 `total_tokens` 按**输出价**结算（贵的那一档），并在 profile 注释与端点清单的
  `DocumentedDeviations` 里写明。绝不按输入价，也绝不对半分。

同一条要在**流式**上再验一次：`stream_options.include_usage` 是否被接受、最后一块是否带 usage。
`internal/provider/openai/adapter.go:574` 在 `capabilities.StreamUsage` 为真时才发
`stream_options`，所以这条能力的真假直接决定流式调用有没有账。

### 3.3 `/v1/embeddings` 不是 OpenAI 形状

已在 §2.2 定了：**不声明 `Embeddings`**。这里补上不做的理由：要做就得新增
`PrimitiveMiniMaxEmbeddings` 与一个原生请求/响应渲染器（`texts`/`type` → `vectors`），
外加 §3.1 的 `base_resp` 处理（这条路径**确定**用 200 带错）。这是一块独立工作，
不该混在首次接入里。列进 §6「明确不做的」。

### 3.4 `n` 只接受 1，且三条 penalty 参数不支持

`n` 已在 §2.4 申报。三条 penalty 无需申报（Halro 的请求类型没有这些成员）。
唯一要注意的是**不要**因为「OpenAI 兼容」就把 `openAIChatSet` 整套抄过来——那正是 DeepSeek
当年的坑（`internal/compatibility/deepseek.go:34-40`：五个字段被渲染到一个一个都不接受的面上）。

### 3.5 `thinking` / `reasoning_split` / `service_tier` 是方言

Halro 的语义模型有 `ReasoningEffort`（`internal/semantic/request.go:127`），没有 `thinking` 对象、
没有 `service_tier`。MiniMax 的三条 surface 各有各的拼法：

| surface | 推理开关 | 深度 |
|---|---|---|
| Anthropic | `thinking: {type: disabled\|adaptive}` | 无独立档位 |
| Chat | `thinking: {type: disabled\|adaptive}` + `reasoning_split` | 无独立档位 |
| Responses | `reasoning: {effort: minimal\|low\|medium\|high\|none}` | 有 |

**Chat 与 Anthropic 两条上没有深度档位，只有开/关。** 这意味着 portable 的
`reasoning_effort` 在这两条上只能映射成「非 `none` 即开」，其余档位之间无差别——
这必须写进端点清单的 `DeclaredTransforms`，否则调用方会以为 `high` 与 `low` 有区别。

Responses 的 `minimal` 与 `none` 不在 `openaiapi.ReasoningEffortLevels` 的交集里（要核），
处理方式照搬 `internal/compatibility/deepseek.go:56-62` 的 `deepSeekPortableEfforts`：
**按值申报，不按字段申报**。

**还有一条容易漏的**：M2.x **无法关闭思考**。所以 `reasoning_effort: none` 打到 M2.x 上
要么被忽略（调用方以为省了钱，实际照算）要么报错。这是**模型级**而非 profile 级的事实，
落在模型目录里，不在字段申报里。

`service_tier` 与 `reasoning_split`：**首次接入不发，也不接受**。理由见 §3.6 与
「pre-1.0.0 不积压兼容层」——现在加一个只在一个 provider 上有意义的语义字段，
将来要么删要么背着走。

### 3.6 `service_tier: priority` 是 1.5 倍价格

MiniMax 的 priority 档按 1.5 倍计价。Halro 的定价模型（`internal/domain/pricing*.go`）
按「模型 + 档位（输入/输出/缓存读/缓存写）+ 时段」定价，**没有「服务等级」这一维**。

**处置**：首次接入**不发 `service_tier`**（等同 `standard`）。这样定价不用动。
若将来要开，它是一次独立的定价模型评审——和分时定价当年的处理一样
（DeepSeek 方案 §10.4：出了独立提案，仍然不实施）。要写进 §6。

### 3.7 native 模式会把 MiniMax **静默忽略**的成员逐字节送上去

native 模式的前提就是不改写请求体。MiniMax 不支持 `top_k`、`stop_sequences`、
`cache_control`（无 prompt caching）、beta headers、`container`、`context_management`、
`mcp_servers`。其中 `mcp_servers` / `container` Halro 本来就拒
（`internal/compatibility/manifest.go` 里 Messages 那份的 `RejectedRequestFields`），
但 `top_k` / `stop_sequences` / `cache_control` 是 Anthropic 的合法成员，Halro 今天会转发。

**大陆站文档把这件事说得比国际站清楚，而且方向更坏**：原文是这些参数**「会被忽略」**，
不是「会报错」。OpenAI 面同样措辞——`presence_penalty` / `frequency_penalty` / `logit_bias`
「会被忽略」。

所以后果不是「预算预留之后吃一个 400」，而是**200 成功、语义已经变了**：调用方设了
`stop_sequences`，上游当没看见，模型一路生成到 `max_tokens`，调用方付了这段钱、
拿到一段本该被截断的输出，链路上没有任何一处说过发生了什么。这比 400 难查得多，
也正好是本仓库反复点名的那一类——「200 但语义已经变了」。

这把 §3.7 从「体验问题」抬成了**必须做**：静默忽略不会自己暴露，只有网关这一侧拦得住。

**处置**：native 模式下对 MiniMax profile 增加一层**入站成员检查**——已有位置：
`checkNativeInboundRedaction`（`internal/gateway/service.go` 附近）就是走原始 payload 而不是
portable 投影的，理由与这里一模一样（「检查面必须等于接受面」）。在同一处按 profile 拒掉
`top_k`、`stop_sequences`、任何 `cache_control`，并在 `anthropic.messages.v1` 的
`DocumentedDeviations` 里写明。

**不要**用「静默丢弃」的做法。丢弃会改变调用方要的东西和它的花费，这在本仓库是明确的反模式
（`provider_fields.go` 里 `detail` 那条注释）。

### 3.8 视频内容块、`mm_file://` 与 `mid_conv_system`

MiniMax M3 支持 `video` / `video_url` 内容块。Halro 的语义模型**没有视频**：`ContentKind` 只有 `text`、`input_image`、`tool_call`、
`tool_result`、`reasoning`、`provider_tool_call` 六种（`internal/semantic/content.go:69-79`）。

**处置**：portable 路径天然到不了（语义模型没有这个成员，北向门面解不出来）。
**native 路径必须显式拒绝** —— 与 §3.7 同一处检查。理由是 native 直通会让一个
Halro 从未检查过、也无法做红字/内容扫描的媒体类型进到上游：`internal/redaction` 与
`internal/contentscan` 都看不懂它，等于在直通路径上开了一个不受检的通道。

`mm_file://{file_id}` 同理：它引用的是 MiniMax 账户里的对象，Halro 既没上传过也无从审计。
拒绝，写进 `DocumentedDeviations`。

**`mid_conv_system` 要单独说一句**，第一版把它漏了。它是 MiniMax 在 Anthropic 面自己加的
内容块类型，**不是 Anthropic 的合法块**，用来在对话中途插入系统指令。两条路径两个答案：

- **portable**：天然到不了。语义模型的 `ContentKind` 里没有它，北向门面解不出来。
- **native**：直通会把它原样送上去，而 `checkNativeInboundRedaction` 走的是原始 payload、
  按已知块类型逐个下钻。一个它不认识的块，**红字与内容扫描会不会走进去看里面的文本，
  是没有核过的**。这正是「检查面必须等于接受面」那条不变量要防的情况：
  一个能装任意文本、又可能不被扫描的块，是直通路径上的第二个不受检通道（第一个是视频）。

处置：**片 3 先去核 `checkNativeInboundRedaction` 对未知块类型的既有行为**——它是下钻还是跳过。
跳过就拒绝 `mid_conv_system`；下钻就可以接受，但要在 `DocumentedDeviations` 里写明
「这是 MiniMax 的扩展块，不是 Anthropic 的」。**在核清楚之前按拒绝处理**，理由是 fail-closed。

### 3.9 图片 URL 由上游去取

`image_url.url` 与 Anthropic `source.type=url` 都是上游代取。这就是 `FetchedImage` 能力的定义，
OpenAI 与 Gemini 已经这么建模，所以**不是新问题**，声明能力即可（§2.2）。
写在这里是为了防止有人误以为该由 Halro 去取——那会让网关去拉一个调用方给的地址，
正是 SafeTransport 的 host 允许清单要防的请求伪造（`provider_table.go` 里 Mantle 那段注释原话）。

### 3.10 双 host：不是缺口，是一栏文案

**这一条从「风险」降级成「文案」。** 详细证据在 §1.2 与 §1.3：两个 host 的路径、认证、
字段、模型、响应与错误信封逐格相同，`GroupId` 与这三条 surface 无关。

所以两个地区**共用同一组 profile**，代码零分叉。要做的只有两件事，都不在 Go 这边：

1. `BaseURLTemplate` 预填 `https://api.minimax.io`（国际），因为总得预填一个。
2. 控制台连接表单在 base URL 那一栏写明：国际账号 `https://api.minimax.io`，
   大陆账号 `https://api.minimaxi.com`，**两边密钥不通用**。

第 2 条不是可选的润色。`BaseURLTemplate` 的注释已经把道理写在那儿了——一个错的预填
比没有预填更糟，因为它是 operator 最不会回头再看一眼的那一栏。大陆 operator 拿着大陆密钥
打国际 host，收到的是 §1.3 那个 401 `login fail`，看上去像密钥错了，实际是地址错了。

**要留意的一个反面**：既然两个 host 只差地址，就**不要**为它们建两个 profile、两个 surface
或两套字段申报。那会把一份契约拆成两份真相，而两份真相里总有一份会先过期。

### 3.11 `count_tokens` 仅 M3 部分支持

`/anthropic/v1/messages/count_tokens` 路由存在（§1.3 实测 401）。但文档说
「partially supported for M3 only」。

Halro 的 `anthropic.count_tokens.v1` 端点今天**只服务直连 Anthropic 一个 profile**
（`internal/compatibility/manifest.go:313` 的清单原文：Mantle 共用 Messages wire，
但它的 count_tokens 面「未确立」）。

**处置**：照 Mantle 办——**首次接入不登记这个端点**。「部分支持」不是一个可以建模的状态，
要么实测清楚它对哪些模型返回什么，要么不提供。写进 §6。

### 3.12 M2.x 的输出上限等于上下文窗口，很可能是文档缺项

§1.4：M2.x 上下文 204,800，`max_tokens` 上限也记 204,800。内置目录
（`internal/modelcatalog/builtin.go`）的种子政策要求「精确标识符 + 逐条审阅过的能力，
带来源与日期」，并且明写「宁可少声明，operator 多做一次声明；不能声明错」
（该文件 32–42 行）。

**处置**：分开处理这两个数。

- **上下文窗口照记**：M3 = 1,000,000，M2.x = 204,800。这两个数文档是当作上下文单列的，
  不是推出来的。
- **输出上限**：M2.x **留空（0 = 不设界）**，而不是照抄 204,800。一个错的上界会在预算与
  路由筛选里真的生效（`filterTokenCapabilities`），把本来能跑的请求拦掉；留空只是少一层保护，
  方向是对的那一侧。M3 的 524,288 文档明确单列，可以记。


### 3.13 顶层 `reasoning_effort` 在 MiniMax 上不可达 —— 需要一个方言渲染器

**这一条按严重度与 §3.1 同级，编号在后只是为了不打乱交叉引用。**

§2.2 给三个 profile 都声明了 `Reasoning`，但第一版全文没有一处说**这个能力怎么发出去**。
把两边对齐来看，问题是明确的：

| | Halro 发什么 | MiniMax 接受什么 |
|---|---|---|
| Chat Completions | 顶层 `reasoning_effort`（`internal/compatibility/provider_fields.go:232` 那条通用规则渲染） | `thinking: {type: disabled\|adaptive}`，**接受清单里没有 `reasoning_effort`** |
| Anthropic Messages | portable 路径经 OpenAI 中间表示，同样落到 `reasoning_effort` | `thinking: {type: disabled\|adaptive}`（仅 M3） |
| Responses | `reasoning.effort` | `reasoning.effort` —— **这一条对得上** |

前两条对不上。按 §3.7 刚立下的那条规律，**未列入接受清单 = 被静默忽略**，所以后果不是 400，
而是：调用方发 `reasoning_effort: none` 想省钱，上游当没看见，M3 按默认的 `adaptive` 照思考、
照出 reasoning token、照计费，返回 200。**调用方付了一笔它明确要求不要的钱，链路上没人说过一句话。**

**这就是 DeepSeek 当年那条缺陷，一字不差**，而 DeepSeek 的解法是一整个方言渲染器：
`internal/compatibility/deepseek.go` 的 `RenderDeepSeekChatRequest` 把 `reasoning_effort`
翻译成 `thinking`，并对翻译不了的值**拒绝而不是丢弃**（该文件 114–122 行的注释写了为什么）。

**处置**：新增 `internal/compatibility/minimax.go`，形状照抄 DeepSeek 那份：

- `MiniMaxChatRequest` 只带 MiniMax 接受的成员，**没有** `reasoning_effort`、`n`、`seed`、
  `stop`、`response_format`（在证实之前）。
- `RenderMiniMaxChatRequest` 把 portable 的 `reasoning_effort` 映射到 `thinking`：
  `none` → `{"type":"disabled"}`，其余非空值 → `{"type":"adaptive"}`，
  **且只对 M3**——M2.x 关不掉思考（§1.4），对它发 `disabled` 是发一个上游做不到的要求。
- 映射不了的值**返回错误**，与 DeepSeek 同样的理由：路由本该先把这个目标筛掉，
  这个错误在跑起来的网关里够不到，它在这儿是为了让别的路径进来时**在最后一刻失败关闭**，
  而不是发出一个调用方没写过的请求。

**连带订正三处**：§2.6 的「全部复用现有适配器」要改成「不新建 provider 包，但 compatibility
层有新代码」（已改）；§3.1 的「唯一必须新写而不能纯复用的逻辑」不成立（已改）；
§2.4 的 Chat/Responses 申报要加 `reasoning_effort`（已加）。

**还有一个必须先定的问题**：`thinking` 不发时 MiniMax 的默认是什么。文档说 M3 默认 `adaptive`
（思考开着）。DeepSeek 在这一点上栽过——方案第一版以为「不发就是关」，实测是「不发就是开」，
于是每一次没提推理的调用都在付思考的钱（DeepSeek 方案 §7.1、§9.2）。
本仓库最后定的规则是**「未指定即关，与 Anthropic 同规则」**。MiniMax 要照同一条规则办：
portable 请求没写 `reasoning_effort` 时，**显式发 `{"type":"disabled"}`**（仅 M3），
而不是什么都不发。这一条写进 `RenderMiniMaxChatRequest` 的注释里，因为它看起来像多此一举。

### 3.14 Responses 面能不能流，是我们的取舍，不是上游的限制

第一版在 §2.2 把「Responses 没有 `Streaming`」的理由写成「该 profile 不绑流原语」。
那是**结果，不是原因**——MiniMax 文档明确 `/v1/responses` 接受 `stream`。真正的原因是首次接入
选了 `openaiprovider` 的 `Responses: true` 分支，而那条分支照搬 `ProfileOpenAIResponses`，
本来就不绑流。

**这件事必须写清楚，否则它会变成一条假的上游事实**：后来人读到「MiniMax Responses 不能流」，
不会知道它可以改。

两条路，各自的代价：

- **保持现状**（推荐，首次接入）：Responses 面只服务非流。代价是同一个 MiniMax 账号，
  走 Chat 面能流、走 Responses 面不能流，operator 会觉得莫名其妙——所以
  `openai.responses.v1` 的 `DocumentedDeviations` 必须写明这是 Halro 的范围取舍、上游支持。
- **改走 `bedrockmantle.ResponsesAdapter`**：它实现了 Responses 流
  （`mantleOpenAIResponsesSet` 带 `Streaming` + `StreamUsage`，绑
  `PrimitiveBedrockMantleOpenAIResponsesStream`）。但它现在与 Bedrock Mantle 绑得很死——
  校验 `bedrock-mantle.*.api.aws` 的 host、发 Bedrock project 头、认 `CredentialBedrockAPIKey`。
  复用它意味着先把这三处 Bedrock 专属的东西抽出去，那是一次独立的重构，
  **不该混在一个新供应商的首次接入里**。

**处置**：首次接入保持现状，并在 §6「明确不做的」里记一行，入口指回这一节。
`PrimitiveMiniMaxResponsesStream` 在定论之前**不要定义**（§2.3）。

### 3.15 529 与 200 里的限流码，对熔断和故障转移意味着什么

MiniMax 带来两条 Halro 现在没有归类的失败信号：

- **HTTP 529**（overloaded，文档标为可重试）。不是标准码，`internal/circuit` 怎么归类它没有核过。
- **`base_resp.status_code = 1002`（限流）**，可能装在一个 HTTP 200 里。

第二条尤其要紧：`route-auto-failover`（#245）看到的是 HTTP 层的成败。**一个 200 装着限流错误，
在故障转移眼里是一次健康的调用**——这条路会一直被判定为可用，请求持续打过去、持续被限流，
而备用路由永远不会被启用。

**这不是验证项，是设计决定**：归到哪一类不取决于上游怎么答，取决于我们怎么定。所以它进 §3，
不是进 §7。（§7 第 8 条只保留「529 的真实重试语义」那半，那部分确实要实测。）

**处置**：§3.1 的 `base_resp` 守卫**必须把码映射到 `provider.Error` 的类别上**，
而不是笼统地返回一个错误——因为熔断、故障转移、重试边界读的都是这个类别：

| `base_resp.status_code` | 归类 | 后果 |
|---|---|---|
| 1002 | rate limit | 计入限流，触发退避，**故障转移看得见** |
| 1004 | authentication | 不重试，连接标记为凭据问题 |
| 1008（余额不足） | authentication / 不可重试 | **绝不重试**——重试不会变有钱，只会放大账单 |
| 1013 | server error | 可重试，计入熔断 |
| 1027 / 1039 / 2013 | bad request | 不重试，不计入熔断（是调用方的问题） |
| 1000 / 1001 | server error / timeout | 可重试，计入熔断 |

HTTP 529 按 `server error` + 可重试归类，与 Anthropic 的 529 同待遇。
**这张表是片 2 的验收内容**：守卫写了但归类错，等于把一个新的失败面接进了熔断器却告诉它「一切正常」。

---

## 4. 切片划分

每片自身可验、可单独回退。片与片之间的依赖是硬的，不并行。

**先说清楚这份方案卡在哪。** 片 1 需要一把真实密钥，片 6 的完成条件需要国际与大陆各一把，
**目前一把都没有**。片 1 的结论会改写 §2.1 的凭据方案、§2.2 的能力集、§3.1 守卫的形状与
§3.13 的默认档规则——这四处都不是「先按假设写、回头再改」能承受的：profile 标识符一旦
写进表就不再复用，能力上限一旦发布就是每张连接表单会提供的东西。

所以：**没有密钥时，这份方案停在片 1，不写任何代码。** 这不是保守，是本仓库
「Verify, never assume」那一节的直接后果——一份从假上游长出来的适配，
测试会全绿，而绿的是那个假设。

**片 1 —— 上游契约实测（无代码）**
用一把真实密钥，对三条 surface 各打一次非流、一次流式，外加一次可控错误。要产出的证据：

1. Anthropic 路径 `Authorization: Bearer` 是否可用（§2.1，决定凭据方案与 profile 分组）
2. Chat 的 usage 是否带 `prompt_tokens` / `completion_tokens`（§3.2，决定结算方式）
3. 真实错误走非 2xx 还是 200+`base_resp`（§3.1，决定守卫的形状与位置）
4. `stream_options.include_usage` 是否被接受、末块是否带 usage（§3.2，决定流式有没有账）
5. `response_format` 到底支不支持（§2.2，决定两个 JSON 能力开不开）
6. **不发 `thinking` 时 M3 到底思不思考**（§3.13，决定「未指定即关」要不要显式发 `disabled`）
7. **顶层 `reasoning_effort` 是被忽略还是被拒**（§3.13，决定方言渲染器是必需还是保险）
8. **什么输入点亮 `input_sensitive` / `output_sensitive`，点亮时 `choices` 还有没有内容**（§3.1）

第 6、7 两条是这一片里最容易被跳过、后果最贵的：它们都不会报错，只会让调用方多付钱。
**这一片的结论会改写 §2.1、§2.2、§3.1、§3.13 四处，所以它必须在写代码之前。**

**片 2 —— 六步注册 + `base_resp` 守卫 + 方言渲染器**
§2.1–§2.6 全部，加 §3.1 的响应后处理（非流与流式两处）、§3.15 的错误码归类表、
以及 §3.13 的 `internal/compatibility/minimax.go`。跑
`TestCeilingWithinProfileManifestOperations`、`TestEveryProfileRegistersItsOwnFieldRules`、
`TestEveryChatProfileAppearsInAnEndpointManifest`、
`TestTheManifestDeclaresEverythingTheRulesRefuse`、
`TestProviderProfilesGoldenMatchesConsoleFixture`（`HALRO_UPDATE_GOLDEN=1` 后读 diff）。

**片 3 —— native 模式与它的入站检查**
§2.7 表里**前四行**的 Go 侧开关（第五行是控制台，归片 5）+ §3.7、§3.8 的拒绝规则。
这一片有一件事要先查再写：**`checkNativeInboundRedaction` 对未知内容块类型是下钻还是跳过**
（§3.8）——答案决定 `mid_conv_system` 是拒是收，查不出来就按拒绝办。

假上游可以覆盖这一片的绝大部分，因为要证的是「Halro 拒了」而不是「上游收了」。

**片 4 —— 模型目录**
八个精确标识符，按 §1.4 与 §3.12 的取舍写（上下文照记、M2.x 输出上限留空），
注释里带来源 URL 与抓取日期。M2.x 不给视觉能力，M3 给；M2.x 关不掉思考这条事实
也落在这里而不在字段申报里（§3.5）。
四个 `-highspeed` 变体与对应的非 highspeed 是否同能力、同价，文档没说，**不要猜**——
按同能力记（文档把它们列在同一张能力表里），价格由 operator 录入（§6 开头）。

**片 5 —— 控制台**
`web/` 五处（§2.7 末行）+ `npm run build` + 提交 `internal/webui/dist`。

**片 6 —— 真实账号 smoke（两个地区各一遍）**
`docs/verification/provider-real-matrix.md` 加 `HALRO_MATRIX_MINIMAX_` 一档，
三条 surface 各一个 profile。**这一片是完成条件，不是收尾**。

地区维度要显式建模：`HALRO_MATRIX_MINIMAX_BASE_URL` 与密钥成对给，跑两轮——
一轮 `api.minimax.io` + 国际密钥，一轮 `api.minimaxi.com` + 大陆密钥。
§1.3 那份逐格相同是无凭据测的，**只覆盖路由与错误信封**；模型清单、限流、`base_resp`
的真实表现都还没有跨地区证据（§7 第 13 条）。只有一边的密钥时，跑一边并把另一边
记为「未测量」，**不要**写成通过。

pre-1.0.0 的处理：本方案全程「就地改对」，不留并存构造。若片 1 推翻了 §2.1 的分组结论，
就直接改表，不新增第二组 profile ID —— profile 标识符一旦用过就不再复用
（`adding-a-platform.md` 第 1 步），所以**在片 1 出结论之前不要往表里写任何 ID**。

---

## 5. 验证计划

分层，按 `AGENTS.md` 的口径（不是每次都跑全量）：

- 片 2/3/4 迭代期：`go test ./internal/domain/ ./internal/provider/ ./internal/compatibility/`
- 片 5 迭代期：`cd web && npx vitest run <改到的那个文件>`
- 每片收口：`go test ./... && go vet ./...`
- 推送前一次全量门禁：CLAUDE.md「The full gate」那一段，含
  `git diff --exit-code -- internal/webui/dist`
- 片 6：真实账号 matrix

**假上游能证与不能证的**：能证 Halro 拒了什么、渲染成了什么形状（§3.13 的
`RenderMiniMaxChatRequest` 完全可以用金样本测——渲染器的输出是确定的）、
`base_resp` 守卫会不会触发、错误码归类表映射对不对（§3.15）。
**不能证** §3.1 的真实错误形状、§3.2 的真实 usage 字段、§2.1 的 Bearer 是否可用、
以及 §3.7 那批参数是不是真的被静默忽略（假上游只会照着方案的假设去忽略）——
这四条全是用假上游写出来就会自我印证的那一类。
**跨地区同构同样不能由假上游证**：两个 base URL 打到同一个假服务器上，当然处处相同。本仓库在这上面栽过
（见 [Anthropic 批处理方案 §5](anthropic-batches-plan.zh-CN.md)：同一天里假上游三次没拦住真缺陷）。

---

## 6. 明确不做的

**先澄清一件不在本方案范围内的事：价格。** Halro 的价格是 operator 按 deployment 录入的
（`internal/app/admin_prices.go`，`PriceSource` 带 URI、published_at、content_sha256），
没有内置价目表要种。所以本方案**不做定价数据源**，也不抄 MiniMax 的价目——
与 DeepSeek 方案同样的口径。八个模型的价格、四个 `-highspeed` 变体是否与对应型号同价，
都由录价的人对着官方页面填，本文不替他判断。唯一与定价有关的设计决定是
§3.6：不发 `service_tier`，所以 operator 录一个价就够，不会有 1.5 倍档在背后浮动。

| 项 | 理由 | 将来要做时的入口 |
|---|---|---|
| `/v1/embeddings` | 原生形状，需要新原语 + 新渲染器 + `base_resp` 处理 | 独立一片，§3.3 |
| `service_tier: priority` | 定价模型没有服务等级维度 | 独立的定价评审，§3.6 |
| `count_tokens` | 「仅 M3 部分支持」不是可建模的状态 | 实测清楚后按 Anthropic 那份清单登记，§3.11 |
| 服务端 web search（`ProviderExecutedTools`） | 上游绕过 SafeTransport 出网 | 独立契约评审 |
| 视频 / `mm_file://` | 语义模型没有视频；直通会绕过红字与内容扫描 | 需要语义模型层面的决定，§3.8 |
| 批处理 / 文件 | 文档明写不支持 | —— |
| Responses 面的流式 | 上游支持，但复用 Mantle 的 Responses 适配器要先把它与 Bedrock 解耦 | 独立重构，§3.14 |

---

## 7. 目前没有证据的结论（逐条）

写在这里是为了**不让它们被当成已知**。每一条都在片 1 里有对应的实测动作。

1. **Anthropic 路径接受 `Authorization: Bearer`** —— 两站文档互相印证（大陆站明写 Bearer 为推荐、
   且与 `x-api-key` 同时出现时优先），但仍**没有真实密钥打通过**。会改变 §2.1 的凭据方案与 §2.2 的分组。
2. **Chat Completions 真实响应带 `prompt_tokens` / `completion_tokens`** —— 只有文档示例，schema 未定义。会改变结算方式（§3.2）。
3. **Chat / Responses 的真实错误走非 2xx** —— §1.3 只证了鉴权失败那一种。会改变 §3.1 守卫的形状与位置。
4. **`response_format` 不支持** —— 三份文档都没提。这是「没写」不是「写了不支持」，按 fail-closed 处理，但它是推断。
5. **`stream_options.include_usage` 被接受** —— 文档列了，未实测。决定流式调用有没有账。
6. **M2.x 的真实输出上限** —— 文档记的 204,800 等于上下文窗口，疑为缺项（§3.12）。
7. **`reasoning_effort` 在 Chat / Anthropic 两条上只有开关语义** —— 由「文档没有档位字段」推出，
   未实测各档是否真的无差别。**同一条的另一半更要紧**：顶层 `reasoning_effort` 是被上游忽略
   还是被拒？被忽略的话 §3.13 的方言渲染器是**必需**，被拒的话它是**保险**——两种情况都要写，
   但严重度差一个数量级。
8. **529 的真实重试语义** —— 文档标为可重试，但上游多久恢复、会不会带 `Retry-After`，没有实测。
   （「Halro 这边把它归到哪一类」已经不是未验证项了——那是设计决定，见 §3.15。）
9. **不发 `thinking` 时 M3 的默认行为** —— 文档说默认 `adaptive`（思考开着）。DeepSeek 在这一点上
   栽过，所以 §3.13 按「未指定即关」处理并显式发 `disabled`。这条规则本身是仓库口径，
   但「MiniMax 的默认确实是开」这句是文档转述，未实测。
10. **`input_sensitive` / `output_sensitive` 的触发条件与后果** —— 四个成员都只是从响应示例里读到的。
    点亮时 `choices` 还有没有内容、`base_resp` 是不是仍然为 0，都没有证据（§3.1）。
11. **`checkNativeInboundRedaction` 对未知内容块的既有行为** —— 这一条不是关于 MiniMax 的，
    是关于 Halro 自己的：它下钻还是跳过，决定 `mid_conv_system` 是拒是收（§3.8）。
    **这条不需要密钥，片 3 一开始读代码就能定论**，是本清单里唯一现在就能关掉的。
12. **MiniMax `/v1/models` 带凭据时的真实返回体** —— **一半已关闭**：2026-08-31 一次真实的
    连接测试通过（§8.8），所以它带凭据时返回的是一个 JSON 对象。**「它确实是一份模型列表」
    仍然没有证据**——probe-only 的校验不读成员名，所以通过不代表形状对上了。
13. **两个 host 在带凭据之后仍然同构** —— §1.3 的逐格相同是**无凭据**测出来的，只覆盖到路由与
   错误信封。模型清单是否一致、限流额度、`thinking` 的默认档、乃至 §3.1 那条 `base_resp`
   在两边是否同样表现，都要各用一把当地密钥才知道。片 6 的真实 smoke **两个地区各跑一遍**，
   不能拿一边的通过当另一边的证据——这正是本仓库对「一种模式通过不构成另一种模式的证据」
   的既有口径（见 Anthropic 那两个执行模式的写法）。

---

## 8. 实施记录（2026-08-31，无真实密钥）

片 2–5 已落地。这一节记三类事：照方案实施的、**与方案不同的**、以及**方案写错、被实现推翻的**。
第三类最要紧，因为它是方案自己没能发现的东西。

### 8.1 方案写错、被实现推翻的三处

**§7 第 11 条已关闭，答案比方案担心的好。** 方案怕 `mid_conv_system` 与 `video` 这类
MiniMax 扩展块在 native 直通下绕过检查。读代码后：`internal/anthropicapi/types.go` 的
`validateBlock` 有 `default: unsupported content block type %q`，**未知块类型早就被现有解码器
拒了**。所以 §3.8 不需要新代码，只需要一条 documented deviation，外加一个把这件事钉住的测试
（`TestMiniMaxNativeRefusesExtendedContentBlocks`）——它存在的理由正是「没有额外检查」这个
事实需要有东西守着。

顺带也确认了 `checkNativeInboundRedaction` 走的是**无 schema 的通用遍历**
（`inspectOutboundJSON` 的注释原话：连不透明值也走，「检查面等于接受面」），
所以红字扫描本来就覆盖未知块的文本。方案 §3.8 担心的「第二个不受检通道」不存在。

**§2.2 给 Responses 面的 `Reasoning: true` 是错的（§2.2 的代码块已就地改对）。** 它与 `ProfileOpenAIResponses`
共用同一个适配器分支和同一个 canonical mapper，而那个 profile **故意不声明 reasoning**，
理由写在表里：mapper 保不住 reasoning item，声明一个它carry不了的能力等于「预算预留之后才失败」。
MiniMax 的 Responses 恰好返回带 `summary` 的 reasoning item，正是被丢掉的那种形状。
已改为不声明。

**§2.3 的第六个 primitive 是死代码。** 方案先写六个、评审时改成五个，实施确认了这是对的：
`ProfileManifest.Validate` 只校验「绑定的操作已声明」，拦不住一个谁都不绑的常量。
`PrimitiveMiniMaxResponsesStream` 没有定义。

### 8.2 与方案不同的两处决定

**发 `reasoning_split`，方案说不发。** 方案 §3.5 把它和 `service_tier` 一起列进「首次接入不发」。
实施时发现这会产生一个更坏的结果：不发的话 MiniMax 把思考**混在 `content` 里**返回，
调用方会把模型的思考读成回答的一部分。而发了之后思考走 `reasoning_content`，
canonical mapper 本来就认得这个成员（`internal/compatibility/openai/mapping.go:291`
把它映成 `ContentReasoning`），DeepSeek 用的也是同一条路。
所以：**只在思考开着时发**，思考关着时不发（没有东西可拆）。
`service_tier` 仍然不发，那条没变。

**`RenderMiniMaxChatRequest` 把所有非 `none` 档位都映到 `adaptive`，不拒绝。**
方案 §3.5 在这一点上留了余地。DeepSeek 的做法是拒绝没有对应档位的值，理由是「四舍五入会
serve 一个调用方没要的深度和账单」。MiniMax 不同：它的开关**只有开和关两个状态，没有更粗的
档位梯子**，所以不存在「round 到邻近档位」这件事——任何「要思考」的请求都只能落到同一个状态。
这作为 `DeclaredTransforms` 声明出来，而不是拒绝，否则 Chat 面的 reasoning 能力等于不可用。

### 8.3 照方案实施的部分

六步清单全部落地，以及 §2.7 的前四行：

| 位置 | 内容 |
|---|---|
| `internal/domain/models.go` | `ProviderMiniMax` + 校验 switch |
| `internal/domain/provider_profile.go` | `SurfaceMiniMax` + 三个 profile ID |
| `internal/domain/provider_table.go` | 三行表、三个能力集、`providerTypeTable` 一行 |
| `internal/provider/primitive.go` | 五个 primitive + `PrimitiveMiniMaxResponses` 注册进 `semanticGenerationPrimitives` |
| `internal/provider/profile.go` | `profileAllowsPrimitive` 三条 + 三份 `ProfileManifest` |
| `internal/compatibility/provider_fields.go` | 三条字段申报 |
| `internal/compatibility/manifest.go` | 四份 manifest 的 profile 列表与 coverage |
| `internal/compatibility/minimax.go` | §3.13 的方言渲染器 |
| `internal/compatibility/anthropic/native.go` | MiniMax 专属 native schema，拒 `top_k` / `stop_sequences` / `cache_control` |
| `internal/gateway/service.go` | `isNativeAnthropicProfile` |
| `internal/provider/capability_detection.go` | `reasoningProbeEffort` |
| `internal/provider/openai/minimax.go` | §3.1 的 `base_resp` 守卫 + §3.15 的错误码归类表 |
| `internal/provider/openai/adapter.go` | 方言分支 + 守卫接入（非流在 `postJSON`，流式在分块循环里） |
| `internal/app/providers.go` | 三个 profile 的适配器构造 |
| `internal/modelcatalog/builtin.go` | 八个精确标识符 × 三个 profile |
| `web/src` | 两个联合类型、类型下拉、两份 i18n、区域提示文案、golden |

`ProfileSendsAnthropicBetas` **没有加**——MiniMax 不接受 beta 头，这与方案一致。

**§3.10 那条「唯一的落地动作」做了**：连接表单与凭据表单的地址栏在类型为 `minimax` 时
显示区域提示（两个 host 各对应哪种账号、密钥不通用），中英双语。

**Anthropic 面没有单独的 `base_resp` 守卫**，这是一个有意的省略而不是遗漏：那条路径的成功体是
Anthropic 形状，一个 `base_resp` 体过不了 `anthropicapi.DecodeMessage`，会以 malformed +
ambiguous 结束——已经是 fail-closed 的方向。等片 1 证实那条路径也会用 200 带错，再补专属归类。

### 8.4 新增的测试

| 测试 | 守住什么 |
|---|---|
| `TestMiniMaxDisablesThinkingWhenNobodyAsked` | 没人要求推理时显式发 `disabled`，而不是让 M3 的默认「开着」去计费 |
| `TestMiniMaxBodyCarriesNoIgnoredMember` | 渲染出的 body 里没有 `reasoning_effort` / `n` / `seed` / `stop` / `response_format` / `user` |
| `TestMiniMaxOutputLimitFollowsTheThinkingSwitch` | `max_tokens` 只在思考关着时才等于 `max_completion_tokens` |
| `TestMiniMaxGuardCatchesAFailureWearingA200` | 用 2026-08-31 实测到的那个 200 响应原文当输入 |
| `TestMiniMaxStatusCodesReachTheRightClass` | 错误码 → `provider.Error` 类别，含 1008 绝不重试 |
| `TestMiniMaxNativeRefusesSilentlyIgnoredMembers` | native 直通拒 `top_k` / `stop_sequences` / `cache_control` |
| `TestMiniMaxNativeGuardDoesNotNarrowAnthropic` | 这个守卫没有连累直连 Anthropic |
| `TestMiniMaxWiringAddressesOneRoutePerProfile` | 三个 profile 各自打对路径——六步里唯一没有测试的那一步 |
| `TestRealMiniMaxSmoke` | 片 1 的六条实测项，无密钥时跳过 |

### 8.5 复核（2026-08-31）：文档与实现对不上的三处，已就地改对

实施完成后逐条把文档拿去核代码，找到三处文档落后于实现的地方。三处都改在原位，
而不是只在这一节记一笔——一个还写着错误代码块的 §2.2 会把下一个读者带偏。

| 位置 | 文档写的 | 实现是 | 处理 |
|---|---|---|---|
| §2.2 | `minimaxResponsesSet` 带 `Reasoning: true` | 不带（理由见 §8.1） | 改代码块 |
| §2.4 | `reasoning_effort` 在 Chat 与 Responses 两条上都申报不支持 | 只在 Responses 申报；Chat 靠方言渲染器翻成 `thinking`，申报了等于把能力关掉 | 拆成两条，并补记 `register` 按 profile 覆盖这条陷阱 |
| §3.1 | 「只有 1004 走零成本」，其余按预留保守结算 | 1027 / 1039 / 2013 也归零成本（`bad request` 类，与既有 4xx 归类一致）；且结算逻辑不需要新写，`settlementForResult` 已按 `Ambiguous` 实现 | 重写该段，把出入标成判断而非测量 |

第三条是这次复核里唯一有实质分歧的：方案凭想象写了一套结算规则，实施时发现
`settlementForResult`（`internal/gateway/service.go:2712`）早就实现了同一条规则，
开关是 `provider.Error.Ambiguous`。所以真正要做对的事情是**归类表把 `Ambiguous` 标对**，
而不是写结算代码——这正是「先读它要判的数据」那条工作方式救下来的一次。

### 8.6 第七处注册：`adding-a-platform.md` 的六步清单漏了它

**症状**：控制台的服务商类型下拉里有「MiniMax（测试版）」，选它、填好地址和密钥、保存，
返回 `400 provider type is not implemented`。**控制台在提供一个它的服务端不接受的类型。**

**成因**：Admin 写入路径有自己的一份 provider type 清单——`implementedProviderType`
（`internal/app/admin_providers.go`），一个手写 switch，同时管着凭据保存与连接保存两处。
它是这个类型清单的**第三份拷贝**：第一份是 profile 表，第二份是
`ProviderInstance.Validate`。`provider_table.go` 开头的注释早就写过这种东西的问题——
「私有清单没法被告知自己落后了」——而它确实落后了。

**为什么整套测试都没红**：`TestMiniMaxWiringAddressesOneRoutePerProfile` 直接调
`newProviderBindingAdapterWithClient`，那是适配器构造的入口，在这道校验的**下游**。
测试证明了三条路由都打得对，而 operator 根本走不到那一步。
这是「假上游不能证的事」之外的另一类盲区：**测试覆盖了真实路径的一段，却不是入口那一段。**

**修法**：不是把 `minimax` 加进那个 switch——那会留下第三份清单继续等下一次落后。
新增 `domain.IsRegisteredProviderType`，直接查 profile 表，`implementedProviderType`
改成一行转调。控制台的类型下拉本来就是从 `domain.AllProviderTypes()` 来的
（`internal/app/admin_provider_profiles.go:130`），所以两边现在读同一个来源，
再也不能各说各话。对既有七个类型行为不变——表里正好就是那七个加 MiniMax。

**补的两条测试**，第一条是通用的、与 MiniMax 无关：

- `TestEveryOfferedProviderTypeIsAcceptedOnSave` 走 `domain.AllProviderTypes()`，
  逐个断言写入路径接受。下一个平台漏了这一步会在这里红，而不是在 operator 的表单里。
- `TestMiniMaxCredentialAndConnectionSaveThroughTheAdminAPI` 走真实 Admin API：
  存一份凭据、在同一份凭据上建三个 profile 的连接、再存一份大陆 host 的凭据。

**反向验证做了**：把手写 switch 还原回去，两条测试都红（`-count=1`，非缓存），
错误正是 operator 看到的那一句 `provider type is not implemented`；改回来后转绿。

**这一条要回填到 `docs/contracts/adding-a-platform.md`**：那份清单说「六步」，
实际是七步，第七步是 Admin 写入路径的类型准入。现在它由表派生，所以对下一个平台来说
这一步消失了——但清单本身应该说明它曾经存在、以及为什么现在不用做。

### 8.7 第八处：凭据测试被拒,因为「能探测」和「能枚举」被绑在同一个开关上

**症状**：连接建好之后点测试,`bad_request` +
`this profile has no model catalog to test against; bind an enabled deployment and test that`。

**成因是我在 §2.6 下错的一个判断。** 那一节写着:`anthropicprovider` 的
`InvocationTargetDiscovery` 在 `MessagesPath` 非空时关掉 `CanEnumerate`,「对 MiniMax 是**对的**」。
关掉枚举确实是对的,但 `probeModelCatalog` 拿同一个标志当作**能不能做凭据测试**的判据,
而那是一个小得多的问题:

- `CanEnumerate` 问的是「能不能从这份列表构造目标描述符和能力声明」。
- 凭据测试问的是「这把密钥能不能到达这个 host」。

MiniMax 的 `/v1/models` 就在同一个 host 上、用同一把 bearer key
（2026-08-31 实测 401 而非 404,即路由存在）,`modelCatalogURL` 拼出来正是
`https://api.minimax.io/v1/models`。**第二个问题它回答得完整,第一个问题它回答不了**——
返回体是 OpenAI 形状,只有标识符,从它枚举会把账户里的语音、视频模型也
credited 成 chat + streaming、还带 `declared` 证据,那正是 `bedrockmantle`
明确拒绝做的那种「从标识符推出来的声明」。

**修法**：把两件事拆开。新增 `anthropicprovider.Options.CatalogProbeOnly`,只在 MiniMax
的分支置 true。`probeModelCatalog` 的门改成 `CanEnumerate || catalogProbeOnly`;
枚举路径一个字没动,`ListInvocationTargets` 仍然拒绝。

**探测的校验强度按证据调整**：非 probe-only 仍然要求 Anthropic 的 `data` 成员在;
probe-only 只要求响应是一个 JSON 对象。理由是 MiniMax 那条路由的**返回体形状没有实测过**——
断言一个猜出来的成员名,会把「猜错了」变成「凭据测试失败」。要求是 JSON 对象仍然拦得住
「HTTPS 上的代理登录页」,那是这条检查原本要防的东西。

**这一处与 §8.6 是同一类错误的两个面**：§8.6 是两份清单各说各话,这一处是一个标志被
两个问题共用。都不是 MiniMax 特有的,都会在下一个平台上重演。

**五条测试**（`internal/provider/anthropic/minimax_probe_test.go`）：探测打到
`/v1/models` 并带 bearer、探测通了枚举仍然拒绝、HTML 页面不算健康、401 以
`authentication` 类上报、直连 Anthropic 的枚举没有被连累。

**仍然未验证的一条**：MiniMax `/v1/models` 带凭据时的真实返回体形状。测试用的是
OpenAI 形状的假响应。这条进 §7。

### 8.8 第一份真实账号证据（2026-08-31）：连接测试通过

操作员用一把真实的国际账号密钥建了连接并点了测试：`https://api.minimax.io`，
锚点 `minimax.anthropic.messages.v1`，**通过，1280ms**，控制台显示开放能力 7 项。

**这条证据的边界要划清楚，否则它会被当成比它更大的东西。**

证到的：

1. 凭据到达 host 并被接受。探测走的是 `/v1/models`，带 `Authorization: Bearer`。
2. 那条路由带凭据时返回一个 JSON 对象——否则 §8.7 放宽后的那条检查会拒。
3. §8.7 的 probe / enumerate 拆分成立：凭据测试跑得通，枚举仍然关着。
4. 开放能力 7 项正好是 `minimaxAnthropicSet` 的七个，说明锚点 profile 绑定的能力集与表一致，
   `AssignConnectionCapabilities` 把它们分给了预期的那个 profile。

**没有证到的，一条都没少**：

- **不构成「Bearer 在 `/anthropic/v1/messages` 上可用」的证据**。探测打的是 `/v1/models`，
  是另一条路由。§7 第 1 条仍然开着，而它的结论会改动凭据方案与 profile 分组。
- `/v1/models` 返回的**是不是一份模型列表**仍然不知道。probe-only 的校验被放宽成
  「是个 JSON 对象」，正是因为形状没实测过。§7 第 12 条只关掉了一半。
- usage 拆分、真实错误是否走 `base_resp`、`thinking` 默认档、流式 usage、`response_format`、
  M2.x 能否关闭思考、两个地区是否同构——全部未动。

**片 1 仍然没有跑。** 一次连接测试与片 1 的六条断言不是同一件事：前者证明密钥能到达，
后者证明账目算得对。它们之间的距离正是本方案 §3.1 与 §3.2 那两条最贵的风险。

### 8.9 大陆地区的验证：有意推迟，不是遗漏

2026-08-31 决定：**国际账号先走完，大陆账号稍后再验。**

推迟的是**验证**，不是实现。代码里两个地区共用同一组 profile、同一份字段申报、同一份端点清单，
大陆账号今天就能建连接——控制台地址栏的区域提示写明了
`https://api.minimaxi.com` 与「密钥不通用」。§1.2 与 §1.3 的无凭据实测支持这个共用决定：
路径集合、状态码、错误信封三项在两个 host 上逐格相同。

**仍然缺的是带凭据之后的那一半**（§7 第 13 条）：模型清单是否一致、限流额度、
`thinking` 的默认档、以及 §3.1 那条 `base_resp` 在两边是否同样表现。

所以在大陆那一轮跑完之前：

- `docs/verification/provider-real-matrix.md` 的 MiniMax 一栏，大陆记 **未测量**，不记通过。
- **不要拿国际账号的通过当作大陆的证据**，两把密钥不通用，两条链路也没有共同跑过。
- 如果大陆那一轮发现了差异，**第一反应不是加第二个 profile**——那会把一份契约拆成两份真相
  （§3.10 的反面提醒）。先定位差异是地址级、账号级还是契约级。

### 8.10 现在的状态

**能编译、能通过全部测试、能建连接、能路由。没有一个字节到过真实的 MiniMax。**

所以 §7 那十二条一条都没关（第 11 条除外，见 §8.1）。代码建立在它们的保守答案上：
未证实的能力一律不声明，未证实的行为一律按最坏假设处理。这意味着**拿到密钥之后，
片 1 的结果只会让这套实现变宽，不会让它变错**——除非第 5 条（M2.x 拒绝 `disabled`）成立，
那一条会让 M2.x 的每个请求都失败，而且是响亮地失败。这是方案里选定的那一侧。
