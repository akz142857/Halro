# Kimi（Moonshot AI）适配方案 —— 一个契约、两个 host、三条 wire

状态：**已实施并用真实大陆账号实测**，分支 `feat/kimi-adaptation-plan`。
第 9 节是实施记录，第 10 节是实测记录。

**阅读顺序：第 10 节 → 第 9 节 → 第 2 节。**三者冲突时，后写的推翻先写的 ——
第 10 节的依据是真实请求，第 9 节是实现，第 2 节是动手前的判断。第 2、9 节都
保留原样不回改，因为它们记录的是当时知道什么。

一句话概括实测改了什么：**§9.1 判 Anthropic 面不可用，是错的，已恢复**；
**方案 §3.10 判两个输出上限是同一个量，也是错的，已按实测改**。

抓取日期：2026-09-01。所有上游事实都注明了来源页面；凡是没有证据的推断，第 7 节逐条列出，不混在正文里当结论。

---

## 0. 这份方案要回答什么

Kimi 开放平台（Moonshot AI）同一个 bearer key 上同时提供三种 wire 形状：OpenAI
Chat Completions、OpenAI Responses、Anthropic Messages。这与 2026-08 接入的
MiniMax 是同一个形状，因此本方案大量复用
`docs/prd/minimax-adaptation-plan.zh-CN.md` 的结构与结论，并在每一处**不同**的
地方明确说明差异。

要回答三个问题：

1. Kimi 的三条 Access Surface 各自能表达什么、不能表达什么，落到
   `ProviderCapabilities` 与字段申报上分别是哪几位。
2. 大陆站（`platform.kimi.com` / `api.moonshot.cn`）与国际站
   （`platform.kimi.ai` / `api.moonshot.ai`）到底是一个契约还是两个 —— 决定它是
   一栏 base URL 文案，还是两套 profile。
3. 落点在树里有几处，哪些有守卫、哪些没有。`docs/contracts/adding-a-platform.md`
   是规范清单，本节不重复它，只标注 Kimi 特有的那几处。

---

## 1. 上游契约（2026-09-01）

### 1.1 抓取来源与方法

不靠渲染页面的猜读，三份机器可读源：

| 来源 | 大陆 | 国际 |
| --- | --- | --- |
| 文档索引 | `https://platform.kimi.com/docs/llms.txt` | `https://platform.kimi.ai/docs/llms.txt` |
| 单页 Markdown | `https://platform.kimi.com/docs/<slug>.md` | `https://platform.kimi.ai/docs/<slug>.md` |
| OpenAPI Schema | `https://platform.kimi.com/docs/openapi.json` | `https://platform.kimi.ai/docs/openapi.json` |

`docs/guide/ai-readable-docs` 就是上游自己声明这三份产物存在的页面，所以本方案的
一手依据是 OpenAPI Schema，散文页只用来补充 schema 不表达的约束（例如「传入其他
值会报错」）。

**这个做法本身值得回写进 `adding-a-platform.md`。**MiniMax 当时判断「两个 host 同
一份契约」靠的是对读散文页；这次是拿两份机器可读的 schema 做结构比对（1.2），强度
不是一个量级。一个上游只要提供 OpenAPI 或等价物，新平台适配的第一步就该是把它拉
下来做集合比对，而不是读页面。

### 1.2 两地是一个契约，不是两个

对两份 `openapi.json` 做结构比对，结果：

- `paths` 集合完全相同（12 条路径）。
- `components.schemas` 名称集合完全相同（48 个）。
- `MessagesRequest` / `ResponsesRequest` / `KimiK3ChatRequest` /
  `ChatRequestCommon` 的属性集合逐一相同。
- 唯一的结构差异是 `servers[0].url`：`https://api.moonshot.cn`
  与 `https://api.moonshot.ai`。

文档索引 `llms.txt` 的 slug 集合差 5 条，全部是非 API 内容（大陆多三条 changelog
和两份协议页，国际多一条登录安全页和一份 changelog）。

**结论：一份契约、两个 host。**这与 MiniMax 的结论一致，处理方式也一致 —— base
URL 是运营者在连接表单里改的一栏，不是第二组 profile。把一份契约拆成两行会造出
两个真相，其中一个先过期。

两点必须写进 UI 文案，因为它们是运营者会踩的坑：

- **Key 不通用。**`docs/api/errors` 明确写：`platform.kimi.com` 与
  `platform.kimi.ai` 的账户、余额、API Key 完全独立，混用返回 401。
- **价格不同，币种也不同。**见 1.4。

### 1.3 无凭据实测（2026-09-01，本节是唯一有实测证据的一节）

对两个 host 各发三个不带 key 的请求：

| host | 路径 | HTTP | body |
| --- | --- | --- | --- |
| api.moonshot.cn | `GET /v1/models` | 401 | `{"error":{"message":"Incorrect API key provided","type":"incorrect_api_key_error"}}` |
| api.moonshot.cn | `POST /v1/chat/completions` | 401 | 同上 |
| api.moonshot.cn | `POST /anthropic/v1/messages` | 401 | 同上 |
| api.moonshot.ai | 同三条 | 401 | 同上 |

两条可用的结论：

1. **两个 host 都真实存在、都用 Bearer 鉴权、都在无 key 时 401**，`/anthropic`
   路由不是文档里的规划而是已上线的路由。
2. **Anthropic 面的错误体是 OpenAI 形状，不是 Anthropic 形状。**Anthropic 的错误
   是 `{"type":"error","error":{...}}`；这里 401 返回的是 `{"error":{...}}`。
   OpenAPI 也确认了这一点：`/anthropic/v1/messages` 的 400 与 500 声明为
   `MessagesErrorResponse`（Anthropic 形状），401 声明为 `ErrorResponse`
   （OpenAI 形状）。**同一个端点上错误体形状按状态码不一致**，见 3.6。

没有真实 key，所以本方案中除本节外的一切都是文档读数，不是实测。

### 1.4 模型、上下文与价格

在售模型（`docs/models`，两地相同的四个精确标识符）：

| 模型 | 上下文 | 说明 |
| --- | --- | --- |
| `kimi-k3` | 1,048,576 | 旗舰；原生视觉；**始终推理**，不可关闭 |
| `kimi-k2.7-code` | 262,144 | 编程模型；**始终推理**，Preserved Thinking 固定开启 |
| `kimi-k2.7-code-highspeed` | 262,144 | 与上一条同模型同参数，仅输出速度不同 |
| `kimi-k2.6` | 262,144 | 通用；思考可开可关 |

`kimi-k3` 的输出上限：默认 131,072，最大可设 1,048,576（`max_completion_tokens`
描述）。K2.x 的输出上限文档未给，**按 MiniMax M2.x 的同一条保守规则处理：不记录**
—— 错的上限会被预算和路由当真执行，缺失的上限只少一层保护。

k3 的输出上限与它的上下文窗口同为 1,048,576，看起来像 MiniMax M2.x 那种「上限等于
窗口 = 缺了一行」，但**这里不是**：Kimi 明确写了「此值为期望返回的 Token 长度，而
非输入加输出的总长度」。两个数字相等是巧合，不是文档缺项，所以照记。

实际后果要一并说明：`filterTokenCapabilities`
（`internal/gateway/service.go:2653-2664`）按 `input + output > MaxContextTokens`
过滤，所以只要输入非空，这个输出上限就永远达不到。记它仍然是对的 —— 它是模型的
事实，被另一条更紧的约束盖住是路由的正常工作，不是记错。

已下线（不得进入模型目录，且是 404 而不是报错 400）：`kimi-k2.5`、`moonshot-v1`
全系、`kimi-k2` 全系、`kimi-latest`、`kimi-thinking-preview`。

价格（每 1M tokens，缓存命中价 / 未命中价 / 输出价）：

| 模型 | 大陆（¥） | 国际（$） |
| --- | --- | --- |
| kimi-k3 | 2.00 / 20.00 / 100.00 | 0.30 / 3.00 / 15.00 |
| kimi-k2.7-code | 1.30 / 6.50 / 27.00 | 0.19 / 0.95 / 4.00 |
| kimi-k2.7-code-highspeed | 2.60 / 13.00 / 54.00 | 0.38 / 1.90 / 8.00 |
| kimi-k2.6 | 1.10 / 6.50 / 27.00 | 0.16 / 0.95 / 4.00 |

价格随地区变，因此**定价不能随模型目录内建**：模型目录记的是「谁存在、能做什么」，
价格是连接（host）的属性。这与 1.2 的结论并不冲突 —— 契约同一，计费不同。

限速（`docs/pricing/limits`，大陆表；国际站有 IP 白名单的额外说法）：Tier0 并发 1
/ RPM 3 / TPM 500k / TPD 1.5M，逐级到 Tier5 并发 1000 / RPM 10000 / TPM 5M。
Tier0 的并发 1、RPM 3 对**连接测试和能力探测**有直接后果，见 3.12。

### 1.5 三条 Access Surface

| wire | base_url | 路径 | 可用模型 |
| --- | --- | --- | --- |
| OpenAI Chat Completions | `<host>/v1` | `/chat/completions` | 四个全部 |
| OpenAI Responses | `<host>/v1` | `/responses` | **仅 `kimi-k3`** |
| Anthropic Messages | `<host>/anthropic` | `/v1/messages` | **仅 `kimi-k3`** |

鉴权三条都是 `Authorization: Bearer $MOONSHOT_API_KEY`。**Anthropic 面用 Bearer，
不是 `x-api-key`**，也没有任何文档提到 `anthropic-version` 头 —— 与 MiniMax 相同。

其余 OpenAI 形状端点（同一个 key、同一个 `/v1` 前缀）：`GET /v1/models`、
`POST /v1/tokenizers/estimate-token-count`、`GET /v1/users/me/balance`、
`/v1/files*`、`/v1/batches*`。本方案只接入生成，其余见第 6 节。

### 1.6 逐 surface 的字段差异

以下全部来自 OpenAPI Schema 的属性集合，加上散文页的取值约束。

**Chat Completions（`ChatRequestCommon` / `ChatRequestBase` + 按 model 判别的三个子
schema）**

接受：`model`、`messages`、`max_tokens`（已弃用）、`max_completion_tokens`、
`response_format`、`stop`（≤5 条，每条 ≤32 字节）、`stream`、`stream_options`、
`tools`、`tool_choice`、`logprobs`、`top_logprobs`、`prediction`、
`prompt_cache_key`、`safety_identifier`；`kimi-k3` 额外接受顶层
`reasoning_effort`；`kimi-k2.6` / `kimi-k2.7-code` 额外接受 `thinking`。

**schema 里根本没有 `temperature`、`top_p`、`n`、`presence_penalty`、
`frequency_penalty`、`seed`、`user`、`parallel_tool_calls`。**散文页
（`docs/api/models-overview`）进一步说明前五个是「固定值，传入其他值会报错，建议
不要显式传入」：`temperature` 固定 1.0（`kimi-k2.6` 非思考模式 0.6）、`top_p`
固定 0.95、`n` 固定 1、两条 penalty 固定 0。

**这是 Kimi 与此前所有已接入平台的最大结构性差异**，见 3.1。

`response_format` 三态：`{"type":"text"}`、`{"type":"json_object"}`、
`{"type":"json_schema", ...}`（Structured Output）—— **两个 JSON 半边都文档化了**，
不像 MiniMax 需要实测才敢声明。

`tool_choice`：`kimi-k3` 支持 `auto` / `none` / `required` 以及指定函数对象；
`kimi-k2.6` 与 `kimi-k2.7-code` **不支持 `required`**，传入报错。

多模态 content block：`text`、`image_url`、`video_url`。`image_url` / `video_url`
的 `url` 只接受两种取值 —— `data:<mime>;base64,...` 或 `ms://<file_id>`。
**没有 http(s) 地址**，见 3.5。

**Responses（`ResponsesRequest`）**

接受：`model`（仅 `kimi-k3`）、`input`、`instructions`、`stream`、
`max_output_tokens`、`reasoning.effort`（`low`/`high`/`max`，默认 `max`）、
`text.format`（仅 `json_schema`，带 `name` / `strict`）、`tools`、`tool_choice`、
`prompt_cache_key`、`safety_identifier`。

没有 `temperature`、`top_p`、`store`、`previous_response_id`、
`parallel_tool_calls`、`user`。工具有两类：`function` 与 Kimi 自有的 `namespace`
（把一组函数收进命名空间），后者是方言。

**Messages（`MessagesRequest`）**

必填 `model`（仅 `kimi-k3`）、`messages`、`max_tokens`。可选 `system`、`stream`、
`stop_sequences`（≤5）、`tools`、`tool_choice`、`metadata.user_id`、
`output_config`。

`output_config` 有两个成员，**恰好就是 Halro 的 Anthropic 门面已经建模的两个**：
`effort`（`low`/`high`/`max`，默认 `max`）与 `format`（`{"type":"json_schema",
"schema":{...}}`）。

没有 `temperature`、`top_p`、`top_k`、`thinking`、`service_tier`、`container`、
`mcp_servers`。`tool_choice` 只有 `auto` / `any` / `none` —— **没有 Anthropic 的
指定工具形态 `{"type":"tool","name":...}`**。工具的 `input_schema` 要求符合
MFJS（Moonshot Flavored JSON Schema）规范，见 3.7。

content block 类型：请求侧 `text` / `image` / `thinking` / `tool_use` /
`tool_result`；响应侧 `thinking` / `text` / `tool_use`，顺序固定。
image 的 `source.type` 只有 `base64` 与 `url`，且 `url` 只接受 `ms://<file_id>`。
**没有 `document`、`search_result`，也没有 MiniMax 那种 `video` 扩展块。**

流事件六个：`message_start`、`content_block_start`、`content_block_delta`、
`content_block_stop`、`message_delta`、`message_stop`。**没有 `ping`，也没有
`error` 事件。**

`stop_reason` 四态：`end_turn` / `max_tokens` / `tool_use` / `refusal`。

### 1.7 用量口径 —— 三条面互不一致

这一节是整份方案里对结算正确性影响最大的部分。Halro 的
`semantic.Usage` 约定 `InputTokens` **含**缓存，且
`CachedInputTokens + CacheWriteInputTokens > InputTokens` 时 `Validate()` 直接拒绝
（`internal/semantic/result.go`）。三条面各自是：

| 面 | 输入 | 缓存读 | 缓存写 | 推理 token |
| --- | --- | --- | --- | --- |
| Chat | `usage.prompt_tokens` | `usage.cached_tokens`（**顶层**） | 无 | **无** |
| Responses | `usage.input_tokens`（**含**缓存） | `usage.input_tokens_details.cached_tokens` | `usage.input_tokens_details.cache_write_tokens` | `usage.output_tokens_details.reasoning_tokens` |
| Messages | `usage.input_tokens`（**不含**缓存） | `usage.cache_read_input_tokens` | `usage.cache_creation_input_tokens` | `usage.output_tokens_details.thinking_tokens` |

三处与现有解码器对不上，全部是会静默算错钱的那一类：

1. **Chat 的 `cached_tokens` 在 `usage` 顶层**，不在 OpenAI 的
   `prompt_tokens_details.cached_tokens` 里。`openaiapi.Usage.CachedPromptTokens()`
   现在只认 OpenAI 的嵌套形态和 DeepSeek 的 `prompt_cache_hit_tokens`，读 Kimi 会
   得到 0 —— 命中的那段按未命中价结算。大陆 k3 两档相差十倍。
2. **Chat 的 `prompt_tokens` 是否含 `cached_tokens`，文档没说。**这是必须靠实测
   关闭的假设，见第 7 节。
3. **Messages 的 `thinking_tokens` 在 `output_tokens_details` 里**，而
   `anthropicapi.Usage` 读的是 `usage.thinking_tokens`（顶层）。直接复用会把推理
   token 读成 0。

Messages 面「input 不含缓存」的口径与 Anthropic 原生一致，
`internal/provider/anthropic/adapter.go` 的 `portableUsage` / `semanticUsage` 已经
做了加回处理，这一半可以原样复用 —— 但只有这一半。

**第四条，是一个决定而不是一处修补：Responses 面的 `cache_write_tokens` 不映射。**

Kimi 的价目表只有两档 —— 缓存命中价与未命中价，**没有缓存写入价**。而 ADR 0022
的现状是：Halro 的价格版本没有 cache-write 费率，`CacheWriteInputTokens` 是
`InputTokens` 的子集，`UncachedInputTokens()`
（`internal/semantic/result.go:44`）会把它从按输入价计费的那一段里**减掉**，同时
`recordUsageTiers` 给这条 attempt 打上 `CostEstimated`。

也就是说，照标准做法把 `input_tokens_details.cache_write_tokens` 映到
`CacheWriteInputTokens`，结果是：那段 token 一分钱不收，并且留下一行以后要重算的
记录 —— 而 Kimi 根本就是按未命中价收这段钱的。

**决定：Kimi 的 Responses 面不填 `CacheWriteInputTokens`**，让这些 token 留在
未缓存段按输入价结算，这与 Kimi 实际怎么收钱一致。这不是忽略一个上游字段，是拒绝
把一个 Halro 有、Kimi 没有的计费层次强加到 Kimi 的账上。Anthropic 面的
`cache_creation_input_tokens` 同理，**同样不填**。

一旦 Halro 有了 cache-write 费率，这个决定要重新看一遍 —— 但那时要看的是 Kimi 有
没有开始分档收费，而不是字段在不在。

### 1.8 错误与限流

错误体 `{"error":{"type":..., "message":...}}`（Anthropic 面 401 除外，见 1.3）。

值得写进熔断/故障转移判断的几条：

- `429` 下有四种 `error.type`：`engine_overloaded_error`（服务端容量，
  文档明确说充值提 Tier 也消不掉，按 `Retry-After` 退避）、
  `rate_limit_reached_error`（组织级并发/RPM/TPM/TPD）、
  `exceeded_current_quota_error`（欠费/额度不足）。
  **前两种该重试，第三种不该** —— 重试只会把同一个失败重复计费路径走一遍。
- `499 client_closed_request`：客户端在返回前断开，流式被中间代理切断时常见。
- `504`：服务端 900 秒无响应，**网关返回 HTML 超时页**，不是 JSON。错误解码必须
  容忍非 JSON body，见 3.6。
- `400 content_filter`：内容安全审查，输入或输出触发都算。**不可重试**。

---

## 2. 落点

严格按 `docs/contracts/adding-a-platform.md` 的编号，只写 Kimi 特有的内容。

### 2.1 类型、Access Surface、凭据方案

- `internal/domain/models.go`：`ProviderKimi ProviderType = "kimi"`，
  并加入 `ProviderInstance.Validate` 的 switch（守卫：
  `TestEveryRegisteredProviderTypePassesInstanceValidation`）。
- `internal/domain/provider_profile.go`：`SurfaceKimi AccessSurface = "kimi-api"`。

  一个 surface 承载三条 wire，与 MiniMax、Bedrock Mantle 同一个理由：Access
  Surface 命名的是「一把凭据能到达的 API 面」，不是面上说的 wire 格式。Kimi 的一
  把 key 同时打开 `/v1/chat/completions`、`/v1/responses`、
  `/anthropic/v1/messages`，拆成三个 surface 会让运营者为一把 key 建三条连接。
- 凭据方案沿用 `CredentialBearerStatic`。**不新增方案**：Anthropic 面也是 Bearer。

### 2.2 profile 标识符与表行

```
ProfileKimiAnthropicMessages ProviderProfileID = "kimi.anthropic.messages.v1"
ProfileKimiChat              ProviderProfileID = "kimi.chat.v1"
ProfileKimiResponses         ProviderProfileID = "kimi.responses.v1"
```

三行同 surface、同 scheme，构成一个连接组。`BaseURLTemplate` 预填
`https://api.moonshot.ai`（国际站），大陆账号由运营者改成
`https://api.moonshot.cn` —— 与 MiniMax 完全同构，UI 上复用同一条区域提示的写法。

**默认 profile 选哪一条，与 MiniMax 的选择相反，理由要写清楚。**MiniMax 选了
Anthropic 面，因为只有它的用量口径是完整的。Kimi 的情况不同：

- Anthropic 面与 Responses 面**都只服务 `kimi-k3` 一个模型**；
- Chat 面服务全部四个模型；
- 三条面的用量都需要各自的修补（1.7），没有一条是「开箱正确」的。

因此**默认 profile 取 `ProfileKimiChat`**：它是唯一能让一条新连接触达全部四个模型
的面。这不是把结算正确性排在后面 —— 三条面都要改解码器，Chat 面并不因此更差。

**代价要写出来，不能只写理由。**Chat 面是三条里唯一**既不上报缓存写入档、又不上报
推理 token**的一面（1.7 与 3.4）。把它设为默认，等于让每一条新建的 Kimi 连接默认
落在可观测性最差的那一面上。这个代价可以接受，因为两项都不影响结算金额（缓存写入
本方案本来就不映射，推理 token 是展示口径），但运营者要知道：**只用 `kimi-k3`
时，Anthropic 面是更好的选择**，UI 的 profile 说明里应当这么讲。

同一处还要改 `providerTypeTable`（`internal/domain/provider_table.go:484-501`）：

```go
{ProviderKimi, ProfileKimiChat, kimiChatSet},
```

`LegacyDefaults` 取 `kimiChatSet` 本身。它是「只知道 provider type、还没选 profile」
的两个调用方（bootstrap、store 对无能力记录的补齐）拿到的集合，守卫
`TestTypeDefaultsWithinDefaultProfile` 只要求它**不宽于**默认 profile 的 Defaults，
取相等是最简单的满足方式。

能力集合（`Defaults == Ceiling`，不留 opt-in 缝隙，与 MiniMax 同）：

```go
// 三条面共同缺席的两位，每一处缺席都是对上游的断言：
//   - Embeddings：Kimi 不提供任何 embedding 端点。
//   - FetchedImage：图片只接受 base64 与 ms://<file_id>，没有 http 地址。
//     Halro 也绝不能替调用方去取那个地址 —— 那正是 SafeTransport 的
//     host 允许清单存在的原因。
//   - ProviderExecutedTools：Kimi 有官方联网搜索工具，打开它等于接受一条
//     不经过 SafeTransport 的上游出网，这是一次契约评审而不是一行表格。
kimiChatSet = ProviderCapabilities{
    Chat: true, Streaming: true, Tools: true, Vision: true,
    JSONObject: true, StructuredOutputs: true,
    Reasoning: true, StreamUsage: true,
}
kimiAnthropicSet = ProviderCapabilities{
    Chat: true, Streaming: true, Tools: true, Vision: true,
    StructuredOutputs: true,   // output_config.format
    Reasoning: true, StreamUsage: true,
}
// Responses 面不绑 stream primitive（与 MiniMax 同一处 Halro 侧取值），
// 因此没有 Streaming/StreamUsage；Reasoning 的缺席另有原因，见 3.3。
kimiResponsesSet = ProviderCapabilities{
    Chat: true, Tools: true, Vision: true,
    StructuredOutputs: true,
}
```

`DeveloperRole` 三条面全缺席：`developer` 角色在 Kimi 的任何 schema 里都不存在。

`JSONObject` 只有 Chat 面有：Responses 的 `text.format` 与 Messages 的
`output_config.format` 都只接受 `json_schema` 一种，**没有无 schema 的 JSON 模式**。

`MaxContextTokens` / `MaxOutputTokens` 不写在连接层：四个模型的窗口不同，这是逐模型
事实，落在模型目录（2.10）。

### 2.3 Operation → Primitive 绑定

`internal/provider/primitive.go` 新增五个常量，`internal/provider/profile_bindings.go`
绑定：

```
kimi.anthropic.messages         + .stream   → ProfileKimiAnthropicMessages（anthropicWire）
kimi.chat-completions           + .stream   → ProfileKimiChat（chatPair）
kimi.responses                              → ProfileKimiResponses（仅 OperationChat）
```

守卫：`TestCeilingWithinProfileManifestOperations`。

注意 `adding-a-platform.md` 已经写明：**这张表写错了没有任何守卫会红**。绑定必须
逐条对着适配器分支复核，不能靠测试。

**同一个文件里还有第二处，同样无守卫：`semanticGenerationPrimitives`**
（`internal/provider/primitive.go`）。漏登记不会让任何测试变红 —— `Resolve` 会落回
legacy Chat 路径，适配器的 Responses 分支照样打到同一个端点，看起来一切正常。代价
是请求多穿过一层 OpenAI Chat 中间表示并在那里吃掉损失，而**这层损失不会出现在字段
申报里**，因为走语义路径时它本来不发生。

逐条表态（照 MiniMax 的分法）：`kimi.responses` 登记为语义 primitive；Chat 与
Anthropic 的四个走各自适配器的既有路径。写方案时先看 `PrimitiveMiniMaxResponses`
那一行的处理，Kimi 与它同形。

### 2.4 字段申报（`internal/compatibility/provider_fields.go`）

三条 `register`，逐 profile。与 MiniMax 相比，Kimi 的申报要**多出 `temperature`
与 `top_p`**，这是全树第一次有 profile 申报这两个成员，见 3.1。

Chat 面（`ProfileKimiChat`）需要申报的：

- `temperature`、`top_p` —— 值相关：只有各模型的固定值可通过，其余报错。实现取值
  见 3.1。
- `n`（>1）、`seed`、`user`、`parallel_tool_calls`（=false）—— schema 里没有该成员。
- `messages[].content[].is_error` —— OpenAI 的 tool 消息只有文本。
- `messages[].content[].type=video` —— Halro 的语义模型没有视频块，Kimi 有；这是
  Halro 不跟随的地方，不是 Kimi 的缺失。
- `reasoning_effort` —— 值相关，见 3.2。
- `stop` **不申报**：Kimi 文档化了 `stop` 且说明匹配即停（与 MiniMax「接受后忽略」
  相反），可以直接携带；但 ≤5 条、每条 ≤32 字节的约束要在渲染器里校验，超限的请求
  在 provider I/O 之前拒绝，而不是让上游 400。

Anthropic 面（`ProfileKimiAnthropicMessages`）在直连 Anthropic profile 的损失之上
再加：

- `messages[].name`、`messages[].role=developer`、`n`、`seed`、`user`。
- `top_k`、`temperature`、`top_p` —— Kimi 的 Messages schema 里没有这三个成员。
- `tool_choice={"type":"tool"}` —— Kimi 只有 `auto`/`any`/`none`，见 3.7。
- `messages[].content[].type=document`、`type=search_result` —— Kimi 的块类型只有
  五种。
- `messages[].content[].cache_control` —— 见 3.9。
- `reasoning_effort`：**值相关，规则是确定的，算一遍就有**。

  `portableEffortLevels` = `anthropicapi.EffortLevels{low, medium, high, xhigh,
  max}` ∩ `openaiapi.ReasoningEffortLevels{none, minimal, low, medium, high,
  xhigh}` = `{low, medium, high, xhigh}`。Kimi 的梯子是 `{low, high, max}`。

  两边取交，再按「不四舍五入到邻近档」的既有规矩（DeepSeek 的同一条）：

  | 取值 | 这一面上 | 原因 |
  | --- | --- | --- |
  | `low` / `high` | 携带 | 两把梯子都有 |
  | `medium` / `xhigh` | **申报** | portable 有、Kimi 没有；不向下取整成 `low`/`high`，那会卖一个调用方没买的深度 |
  | `max` | **申报**（portable 路径） | Kimi 有，但 portable 走不到 —— 与 DeepSeek 的 `max` 同因。native 模式下可达 |
  | `none` | **申报** | k3 无法关闭思考，这一面又只服务 k3。「不要思考」在这里无法履行，无法履行就拒绝 |
  | 空 | 携带（不发 `output_config`） | 用 Kimi 自己的默认 `max` |

  规则一行：`add(request.ReasoningEffort != "" &&
  !slices.Contains(kimiAnthropicPortableEfforts, request.ReasoningEffort),
  "reasoning_effort")`，其中该常量 = `{low, high}`。

Responses 面（`ProfileKimiResponses`）：Chat 面的损失，加上 `messages[].name`、
`response_format` 的无 schema 半边、以及 3.3 说明的 reasoning。

守卫：`TestEveryProfileRegistersItsOwnFieldRules`。**漏掉这一步是静默的** ——
未注册的 profile 落回 legacy 规则，看起来像成功。

### 2.5 端点清单（`internal/compatibility/manifest.go`）

三个 profile 各自加入 `chatProfiles`，并在
`openai.chat-completions.v1` / `openai.responses.v1` / `anthropic.messages.v1`
三份 manifest 的 `ProfileCoverage` 里各加一行。`embedProfiles` **不加**。

守卫：`TestEveryChatProfileAppearsInAnEndpointManifest` 与
`TestTheManifestDeclaresEverythingTheRulesRefuse`（后者要求 2.4 拒绝的每个成员在
这里有声明）。

`docs/compatibility/endpoint-manifests.json` 是生成物，随之更新。

### 2.6 适配器构造（`internal/app/provider_adapters.go`）

三个 builder：

- `ProfileKimiAnthropicMessages`：Anthropic 适配器，`OperationPathPrefix`
  `/anthropic`（Kimi 的 Anthropic 路由是 `<host>/anthropic/v1/messages`，与 MiniMax
  相同的形状）。模型枚举走同 host 的 `/v1/models`（见 3.11）。
- `ProfileKimiChat` / `ProfileKimiResponses`：OpenAI 适配器的两条分支，
  base `<host>/v1`，无路径前缀。

守卫：`TestEveryReachableProfileBuildsAnAdapter`、
`TestEveryReachableProfileReachesTheNetworkWhenCalled`。另写
`TestKimiWiringAddressesOneRoutePerProfile`（对照 MiniMax 的同名测试）—— 它验证
适配器打到哪条路由，**不验证 2.3 的绑定表**，两件事不要混同。

### 2.7 方言渲染器 —— 清单上没有，但一定要写

`adding-a-platform.md` 的六步里没有这一处，因为它不是注册点。但 MiniMax 为它写了
两个文件（`internal/compatibility/minimax.go` 226 行、
`internal/provider/openai/minimax.go` 104 行），Kimi 同样躲不掉。**方案第一版漏了
它，这是最容易在实施时才发现的缺口。**

为什么躲不掉：`openaiapi.ChatCompletionRequest` 带 `Temperature`、`TopP`、`N`
（`internal/openaiapi/types.go:23-28`），OpenAI 适配器会把它们原样发出去。Kimi 的
schema 里没有这三个成员，散文页说传非固定值报错。**不写渲染器 = 每个带
temperature 的请求都在 provider I/O 之后 400，而那时预算已经预留。**

`internal/compatibility/kimi.go` 至少要承担：

1. **推理开关的两套互斥拼写。**k3 走顶层 `reasoning_effort`；`kimi-k2.6` /
   `kimi-k2.7-code` 走 `thinking`，且两者的合法取值集合还不同（3.2）。同一条
   `ProfileKimiChat` 上按模型分叉 —— 这是 Kimi 与 MiniMax 最大的实现差异，MiniMax
   的开关是连接级的一个状态，Kimi 的是模型级的两套语法。
2. **输出上限归一。**`VisibleOutputTokenLimit` 与 `CompletionTokenLimit` 都渲染到
   `max_completion_tokens`；两者同时出现时在字段层申报（3.10）。不发已弃用的
   `max_tokens`。
3. **`stop` 的长度校验。**≤5 条、每条 ≤32 字节，超限在 provider I/O 之前拒绝，而
   不是让上游 400。
4. **采样参数的处置**，取值见 3.1 的定论。

`internal/provider/openai/kimi.go` 承担传输层的两件事：

5. **非 JSON 错误体。**504 是 HTML 超时页（3.6），解码器不能把 body 内容带进错误
   消息 —— 那是把上游响应体写进日志的路径。
6. **HTTP 200 携带错误的探测。**MiniMax 的这一类是靠无凭据实测在 `/v1/embeddings`
   上发现的；Kimi 没有 embeddings，三条 chat 路由无凭据实测都返回了正规 401
   （1.3），所以**目前没有证据说 Kimi 有这个毛病**。这一处先不写代码，但拿到真实
   key 后要主动试一次（第 5 节）。

Anthropic 面还有第七件事，落在 `internal/provider/anthropic` 的用量翻译上：

7. **不填 `CacheWriteInputTokens`**（1.7 的第四条）。现有的 `portableUsage` /
   `semanticUsage` 把 `cache_creation_input_tokens` 填进去，对 Anthropic 正确，对
   Kimi 会少收那一段的钱并留下 `CostEstimated`。需要一条按 profile 分叉的翻译，
   **不能改共用路径** —— 直连 Anthropic 的行为必须原样不动。

### 2.8 第七步：无事可做

`internal/app/admin_providers.go` 已改读 `domain.IsRegisteredProviderType`。
**不要在那里重新引入清单。**守卫：`TestEveryOfferedProviderTypeIsAcceptedOnSave`。

### 2.9 Anthropic-wire 平台的三处额外注册

- `internal/compatibility/anthropic/native.go`：注册
  `ProfileKimiAnthropicMessages` 及其 payload 校验器。Kimi 的校验器要拒绝：
  `top_k`、`temperature`、`top_p`（Kimi 无此成员）、`thinking`（Kimi 用
  `output_config`）、`cache_control`（见 3.9）、`tool_choice={"type":"tool"}`。
  与 MiniMax 的校验器不同的是：**`stop_sequences` 放行**（Kimi 文档化了它会生效），
  **`output_config` 放行**（Kimi 接受它）。
- `internal/gateway/service.go` 的 `isNativeAnthropicProfile`：加入
  `(ProfileKimiAnthropicMessages, SurfaceKimi)`。
- `internal/domain/provider_profile.go` 的 `ProfileSendsAnthropicBetas`：
  **不加**。Kimi 没有任何文档提到接受 `anthropic-beta`，也没提 `anthropic-version`。

守卫：`TestNativeAnthropicListsAgree`（前两者必须完全一致，第三者是子集）、
`TestNoNativeProfileIsWithheld`。

还有一处：`internal/provider/capability_detection.go` 的 `reasoningProbeEffort`
必须排除 `ProfileKimiAnthropicMessages` —— 探测通过 portable Chat 映射读答案，而
Anthropic-wire 上游返回的是带签名的 thinking 块，读不出来，问一次等于白付钱。
守卫：`TestEveryAnthropicWireProfileIsExcludedFromTheReasoningProbe`。

### 2.10 清单之外

**模型目录**（`internal/modelcatalog/builtin.go`，新增 `kimiModels()`）。四个精确
标识符 × 它们各自可达的 profile：

| 模型 | Chat | Responses | Messages |
| --- | --- | --- | --- |
| kimi-k3 | ✅ | ✅ | ✅ |
| kimi-k2.7-code | ✅ | ✗ | ✗ |
| kimi-k2.7-code-highspeed | ✅ | ✗ | ✗ |
| kimi-k2.6 | ✅ | ✗ | ✗ |

「Responses 与 Messages 只服务 `kimi-k3`」是 OpenAPI 的 `model` 枚举写死的，不是
推断。这张表正是模型目录相对于连接层能力集合的价值所在。

逐模型要记的：`MaxContextTokens`（k3 1048576，其余 262144）、
`MaxOutputTokens`（**只有 k3 记 1048576**，K2.x 不记，见 1.4）、
`Reasoning`（四个都记 —— 但含义不同，见 3.2）、
`Vision`（四个都记；`kimi-k2.7-code` 与 `kimi-k2.6` 的介绍页明确写了支持图片与
视频输入，`kimi-k3` 原生视觉）。

**前端**：`web/src/types.ts` 的两个联合类型、`web/src/pages/ProvidersPage.tsx`
的类型清单与区域提示、`web/src/i18n/locales/{zh-CN,en-US}.ts` 的
`accessSurfaces["kimi-api"]`、`types.kimi`、`providers.kimiRegionHint`。
区域提示文案要同时说清两件事：地址不同、**key 不通用**。

**golden**：`HALRO_UPDATE_GOLDEN=1 go test ./internal/app/ -run
TestProviderProfilesGoldenMatchesConsoleFixture`，然后**读 diff** —— 那就是每一张
连接表单将要提供的内容，diff 本身就是评审。同步
`web/src/test/provider-profiles.golden.json`。

---

## 3. 实施前必须先定论的十四条

### 3.1 `temperature` / `top_p` —— 全树第一个申报它们的 profile（最高优先级）

现状：`internal/compatibility/provider_fields.go` 里没有任何 profile 申报过
`temperature` 或 `top_p`。所有已接入平台都接受这两个成员。

Kimi 不接受。schema 里没有这两个属性，散文页说「固定，传入其他值会报错，建议不要
显式传入」。

**先排除一个不能选的：静默丢弃。**不发上去、当没看见，直接违反能力过滤原则 ——
不支持的成员是拒绝，不是悄悄改掉。调用方付了钱，拿回一个采样参数被无声换掉的结果。

剩下两个，**关键在于它们落在哪一层，而不是哪个更宽松**：

**(A) 一律拒绝** —— `provider_fields.go` 里一条 `add(request.Temperature != nil,
"temperature")`。任何显式取值都在 provider I/O 之前把 Kimi 目标从路由里剔掉。

**(B) 值相关** —— 等于该模型的固定值时放行，否则拒绝。

方案第一版选了 (B)，并说「固定值从模型目录读」。**这一版收回那个说法：(B) 在字段
规则层做不出来。**

`generateFieldRules` 的注册签名是
`func(add fieldSink, request semantic.GenerateRequest)`
（`internal/compatibility/provider_fields.go:39`）—— 它按 profile 索引，**参数里
没有目标模型**。而 Kimi 的固定值恰恰随模型变（k3/k2.7-code 是 1.0，k2.6 思考 1.0
非思考 0.6）。字段规则拿不到「这次要打哪个模型」，就没法判断 1.0 该放还是该拒。

`adding-a-platform.md` 第 4 步已经写过这条分界，而且是从 Bedrock 的
`fetched_image` 上学来的：

> Declare a fact here only if it is a property of a request member. A property of
> the target is a capability instead.

「采样参数被上游钉死」是**目标的属性**，不是请求成员的属性。它该走能力位，落在
`filterTokenCapabilities`（`internal/gateway/service.go:2653`）同一层 —— 那一层
拿得到 `target.Capabilities`，也就拿得到逐模型的事实。

**因此定论分两段：**

1. **本次实施取 (A)**，一律拒绝。理由是它现在就能做对，而且做对的成本是一行。代价
   是挡掉一类无害请求（`temperature: 1.0` 对 k3 合法，某些 SDK 封装会默认填）——
   这个代价是**可见的拒绝**，调用方拿到的是「这个字段这条路由不支持」，不是一个
   被改过的结果。必须在 manifest 的 `DeclaredTransforms` 里写明原因，别只写字段名。
2. **(B) 作为后续的能力位提案，单独立项**，不塞进本次适配。它要新增一个
   `ProviderCapabilities` 成员（「采样参数固定」及其取值）、一条目标过滤、以及模型
   目录里逐模型的固定值 —— 那是一次影响所有平台的能力模型扩展，不是 Kimi 的一行。

同一条处置覆盖 `n`（固定 1）。`presence_penalty` / `frequency_penalty` 固定 0，但
Halro 的语义模型和 `openaiapi.ChatCompletionRequest` 都没有这两个成员，无事可做。

### 3.2 推理是模型的属性，不是 profile 的属性

Kimi 的推理开关按模型分成两套**互不通用**的拼写：

- `kimi-k3`：顶层 `reasoning_effort`，取 `low`/`high`/`max`，默认 `max`。
  **始终推理，无法关闭。**
- `kimi-k2.7-code`：`thinking`，且只接受 `{"type":"enabled","keep":"all"}`。
  **始终推理，无法关闭。**
- `kimi-k2.6`：`thinking`，接受 `enabled` / `disabled` / `enabled+keep:all`。
  **唯一能关的。**

后果，逐条：

1. **portable 的 `reasoning_effort: "none"` 在四个模型里只有一个能兑现。**
   DeepSeek 的做法是把 `none` 映到 `thinking.type=disabled`；Kimi 上这条只对
   `kimi-k2.6` 成立。对 k3 和 k2.7-code，「不要思考」这个请求**无法履行**，而
   Halro 的规矩是：不能履行就拒绝，不能默默履行成别的。
2. **`low`/`high`/`max` 只对 k3 成立**，K2.x 传 `reasoning_effort` 会报错。
3. `DeepSeekEffortLevels` 恰好就是 `{low, high, max}` —— Kimi K3 的梯子与它逐字
   相同，`internal/compatibility/deepseek.go` 的写法可以直接照搬结构。
   但 portable 侧仍受 `openaiapi.ReasoningEffortLevels`（`none`/`minimal`/`low`/
   `medium`/`high`/`xhigh`）限制，`max` 在 portable 上不可达 —— 与 DeepSeek 同。

**方案**：profile 级申报按最窄的公共集合写（`low`/`high` 可达，其余申报），逐模型
的差异落在模型目录：`kimi-k2.6` 记 Reasoning 可关，k3 与 k2.7-code 记
「Reasoning 不可关」。**后者是模型目录当前无法表达的事实** —— MiniMax 的方案在
同一处踩到过同一个坑（M2.x 无法关闭思考，只能写在注释里）。本方案沿用同样的处理：
写进目录注释，并在 UI 的模型说明里显示，不假装能力集合表达得了它。

### 3.3 Responses 面：不绑 stream，是 Halro 的取舍，不是上游限制

Kimi 的 `/v1/responses` 文档化了 `stream: true` 与完整的事件流。Halro 不绑 stream
primitive 的原因与 MiniMax 完全一样：OpenAI 适配器的 Responses 分支没有 stream
primitive，`CapabilityDependencies` 又要求 stream_usage ⊃ streaming ⊃ chat。要接
上得复用 Bedrock Mantle 的 Responses 适配器，而那一个焊死在那个 host 的端点、
project 头和凭据方案上。

**必须在 manifest 的 `DeclaredTransforms` 里写明这是 Halro 的范围决定**，否则运营
者会以为 Kimi 不支持。

Reasoning 在这一面的缺席则是另一个原因：canonical response mapper 保不住
reasoning item。Kimi 的 Responses 返回 `ResponsesOutputReasoningItem`，正是那个
mapper 会丢掉的形状 —— 与 `ProfileOpenAIResponses`、`ProfileMiniMaxResponses`
同因。

### 3.4 Chat 面不报推理 token

Chat 的 usage schema 只有四个成员：`prompt_tokens`、`completion_tokens`、
`total_tokens`、`cached_tokens`。**没有 `completion_tokens_details.reasoning_tokens`。**

而 k3 与 k2.7-code 始终推理、始终在 `reasoning_content` 里返回推理内容 —— 也就是
说，Chat 面上会有一段调用方付费、Halro 记不到 `ReasoningTokens` 的输出。它并不
丢钱（`completion_tokens` 应当已经含它），但会让用量看板上「推理占比」这一列在
Kimi Chat 上恒为零。

**不修，但要记录。**推理 token 是展示口径而不是结算口径，捏造一个数比缺一个数糟。
第 7 节把「`completion_tokens` 是否含推理」列为待实测。

> **本节整节已被实测推翻，见 §10.2 第 2 条。**Chat 面确实上报
> `completion_tokens_details.reasoning_tokens`（实测 48 个 completion token 里 45 个
> 是推理）。Kimi 的 OpenAPI 在 usage schema 里漏了这个成员，本节据它推断，推错了。
> Halro 的解码器读的就是 OpenAI 的标准嵌套，所以无需改代码。

### 3.5 图片只接受 base64 与 `ms://` —— `FetchedImage` 缺席

三条面一致：`image_url` / `image.source.url` 只接受 `data:` 与 `ms://<file_id>`。
`ms://` 指向的是通过 `/v1/files` 上传的文件，属于上游侧资源。

因此 `FetchedImage` 在三个能力集合里全部缺席，**并且 Halro 不得代取**：让网关去
拉一个调用方给的 URL 正是 SafeTransport 的 host 允许清单要防的请求伪造。这与
Bedrock Mantle 的处理一字不差。

`ms://` 本身**不建模**：它要求先走 `/v1/files` 上传，而文件接口不在本次范围内
（第 6 节）。一个带 `ms://` 的请求会被 Anthropic 面的解码器当作非法 URL 拒绝，
这是正确行为。

`Vision` 在场而 `FetchedImage` 缺席时，一个带 http 图片地址的请求走哪条路要说清楚，
否则读方案的人会以为它「能看图所以能过」：`fetched_image` 依赖 `vision`
（`CapabilityDependencies()`，`internal/domain/provider_table.go:586`），路由在
**目标过滤**层就把三条 Kimi profile 全部剔除，调用方拿到的是「没有路由支持这个
请求」而不是某个字段名。这与 Bedrock Mantle 一致，也正是那条能力从字段申报升格成
能力位的原因。带 base64 图片的请求不受影响。

### 3.6 错误体形状：Anthropic 面按状态码变，504 是 HTML

1.3 实测确认：`/anthropic/v1/messages` 的 401 是 OpenAI 形状，而 OpenAPI 声明 400
与 500 是 Anthropic 形状。Anthropic 适配器的错误解码必须**两种都容忍**，任何一种
解不出时退化为「按状态码分类 + 不泄露 body」。

`504` 更极端：文档写明 900 秒无响应时网关返回 **HTML 超时页**。解码器读到非 JSON
不能 panic、不能把 HTML 片段塞进错误消息（那是把上游响应体带进日志的路径，违反
「不记录响应体」的不变量），只能按状态码归类。

熔断与故障转移的分类建议：

| 上游 | Halro 分类 | 可重试 |
| --- | --- | --- |
| 400 `content_filter` | BadRequest | 否 |
| 400 `invalid_request_error` | BadRequest | 否 |
| 401 / 403 | Unauthorized / Forbidden | 否 |
| 404 `resource_not_found_error` | BadRequest（模型不存在） | 否 |
| 429 `engine_overloaded_error` | Overloaded | 是，按 `Retry-After` |
| 429 `rate_limit_reached_error` | RateLimited | 是，退避 |
| 429 `exceeded_current_quota_error` | **Unauthorized 类** | **否** |
| 499 | ClientClosed | 否 |
| 500 / 503 | Upstream | 是 |
| 504（HTML） | Upstream timeout | 是（非流式） |

`exceeded_current_quota_error` 与另外两种 429 同码不同义，**是本节唯一一条会被
默认实现搞错的**：把欠费当限流重试，只是把同一个失败重复走一遍。

### 3.7 Messages 面的两处方言：`tool_choice` 少一态，`input_schema` 要 MFJS

- Anthropic 的 `tool_choice` 有四态：`auto` / `any` / `tool`（指定名字）/ `none`。
  **Kimi 只有前两态与 `none`。**指定工具的请求必须在字段层拒绝，native 校验器也要
  拒 —— 否则字节转发上去会被上游 400，而那时预算已经预留。
- `MessagesTool.input_schema` 要求符合 MFJS（Moonshot Flavored JSON Schema）。
  规范在 `github.com/MoonshotAI/walle` 仓库，不在平台文档里。

  **本方案不实现 MFJS 校验。**理由：它是 JSON Schema 的一个子集/方言，Halro 侧
  照搬一份规范就是复制一份会过期的真相。做法是让上游拒绝，并把 400 原样分类为
  BadRequest。**但这必须在 manifest 的 `DocumentedDeviations` 里写明**，否则一个
  在 Anthropic 上能跑的工具定义在 Kimi 上 400，运营者无从判断是谁的问题。

### 3.8 `namespace` 工具与「动态加载工具」不建模

Kimi 有两处工具方言：Responses 面的 `namespace` 工具（把一组函数收进命名空间）、
Chat 面 k3 的动态工具加载（在 `messages` 中间插入
`{"role":"system","tools":[...]}` 且该消息无 `content`）。

两者都不在 Halro 的语义模型里，也都不打算加。后者尤其要注意：**它是一条没有
`content` 的 system 消息** —— Halro 的消息校验大概率会拒绝空 content 的 system
消息，这是正确行为，但错误消息应当可读。

### 3.9 缓存是自动的，没有 `cache_control`

Kimi 的 Context Caching 全自动：不创建、不引用 ID、不管 TTL，
`prompt_tokens > 256` 才可能命中。因此 Anthropic 面上的 `cache_control` 标记
**没有对应物**，native 路径也要拒绝它 —— 一个不存在的折扣被标出来，等于在计费上
撒谎。这与 MiniMax 的处理同因同解。

`prompt_cache_key`（Chat 与 Responses 面）是 Kimi 用来提高命中率的会话标识。
Halro 的 `EndUserRef` 不该映到它 —— 语义不同：一个是终端用户身份，一个是会话
前缀键。`safety_identifier` 才是 `user` 的对应物，但它要求哈希后的值，而
`EndUserRef` 是调用方给的原文。**结论：`user` 申报为不支持**，不做重命名。

### 3.10 Messages 面 `max_tokens` 必填与两个输出上限

Anthropic Messages 要求 `max_tokens`，Kimi 照搬。Chat 面有两个成员：
`max_tokens`（已弃用）与 `max_completion_tokens`。

Kimi **没有**说明这两者语义不同（不像 MiniMax 的单一上限含推理）。文档只说
`max_tokens` 已弃用、请用 `max_completion_tokens`，两者描述指向同一件事。

因此：`VisibleOutputTokenLimit` 与 `CompletionTokenLimit` 都渲染到
`max_completion_tokens`；两者同时出现时申报为不支持（Halro 不替调用方在两个它自己
区分了的上限之间选一个）。**「`max_completion_tokens` 是否含推理 token」列入第 7
节待实测** —— k3 始终推理，这个答案决定 portable Messages 请求的上限是否被悄悄
放大。

### 3.11 模型枚举走 `GET /v1/models`，且它顺带给能力标志

CLAUDE.md 的规矩：适配器的沉默不是上游的答案。Kimi 服务
`GET /v1/models`，返回的每个模型对象带 `id`、`context_length`、
`supports_image_in`、`supports_video_in`、`supports_reasoning`。

**枚举**用这个端点，不要内建清单。Anthropic 面的 profile 也走同 host 的 `/v1`
路由取模型列表 —— 与 MiniMax 的适配器同一个做法。这一半没有争议。

**能力标志是另一回事，而且它撞上了一条既有的设计决定。**方案第一版把它写成「值得
作为一条独立切片」，这个说法太轻。`internal/provider/model_catalog.go:20-22` 的
注释是这样立的：

> Note what an entry does not carry: no context window, no output ceiling, no
> capability flags. A list of this shape answers who exists on the account and
> nothing about what they do, which is why a target built from it takes
> `MetadataSourceNone` and leaves capabilities to the model catalog.

这不是「还没做」，是**做过决定**：模型列表回答「谁存在」，能力永远来自 Halro 的
模型目录或运营者的声明，不来自「某个标识符出现在一份清单里」这个事实。

Kimi 的 `/v1/models` 恰好带了 `context_length` 与三个 `supports_*`，也就是恰好越过
了那条线。要用它，就是推翻上面那段注释，**这是 ADR 级的决定，不是一条切片**：它会
改变每一个 OpenAI 兼容上游的目标构建语义（那个解码器是共用的），并且引出一个新
问题 —— 上游对自己的声明算不算证据、算哪一档。

**本次范围内的处置：不读这些标志。**能力照旧来自 `internal/modelcatalog/builtin.go`
（2.10），证据档位 `declared`（`SourceBuiltin.MaxEvidence()` 本来就封在这一档）。
Kimi 的这四个字段作为**推翻那条设计决定的第一个具体理由**记在这里，等有第二个上游
也这么做时再一起立 ADR —— 一个上游的方言不足以改共用解码器。

一个已知的不一致要留意：`/v1/models` 会列出账号可访问的模型，而 Responses 与
Messages 只服务 `kimi-k3`。**枚举的结果不能直接当作某条 profile 的可用模型集合** ——
「谁存在」和「在这条面上能不能用」是两个问题。

### 3.12 Tier0 的并发 1 / RPM 3 对连接测试的影响

Tier0（零充值）账号：并发 1、RPM 3。一次连接测试如果同时做「枚举模型」和「探测
能力」，很容易自己撞自己的限流，然后把 429 报成「凭据无效」。

MiniMax 适配的第八处缺陷正是这一类（「能探测」和「能枚举」被绑在同一个开关上）。
那个开关在 `internal/app/admin_invocation_targets.go` 的 `discovery.CanEnumerate`
（:380 判分支、:555 决定是否缓存、:930 合并），Kimi 的实施要**盯着这里复核**，不要
只在方案里写一句「必须串行」。

**Kimi 的连接测试必须串行、且必须把 429 与 401 分开报。**Tier0 账号做一次「枚举 +
探测」就已经用掉 2 次调用，RPM 3 意味着第二次点击「测试连接」就可能撞 429 —— 而
429 被报成「凭据无效」，运营者会去换一把好的 key。

### 3.13 两地价格不同 —— 定价不能随模型目录内建

1.4 已给出两张价目表，币种都不同。Halro 的定价是运营者维护的数据，本方案
**不内建任何 Kimi 价格**。但区域提示文案里要提醒：换 host 等于换价目表。

### 3.14 `stop_options` 与流式用量

Chat 面通过 `stream_options: {"include_usage": true}` 在末个 chunk 之前附带
`usage`。而文档的流式示例里，**最后一个带 `finish_reason` 的 chunk 已经带了
`usage`**，没有提到 `stream_options`。两处说法不一致。

`StreamUsage` 能力已经声明，实现上按 OpenAI 惯例发送
`stream_options.include_usage=true`；**「不发这个选项时是否也返回 usage」列入第 7
节** —— 若上游无条件返回，多发这个选项无害；若必须发，不发就会丢掉整次调用的用量，
那是结算级别的问题。

---

## 4. 切片划分

| 片 | 内容 | 完成判据 |
| --- | --- | --- |
| 1 | 类型 + surface + `providerTypeTable` 一行 + 三行 profile 表 + 能力集合 | `go test ./internal/domain/` 绿；golden diff 已读 |
| 2 | primitive 常量 + 绑定表 + `semanticGenerationPrimitives` + 字段申报 + 端点清单 | 六步守卫全绿；`endpoint-manifests.json` 已更新 |
| 3 | **方言渲染器**（2.7 的 1–4、5）+ 三个适配器 builder + wiring 测试 | `TestEveryReachableProfileReachesTheNetworkWhenCalled` 绿；带 temperature 的请求在 provider I/O 之前被拒 |
| 4 | Anthropic-wire 三处注册 + native 校验器 + 推理探测排除 | `TestNativeAnthropicListsAgree` 绿 |
| 5 | 1.7 的三处用量解码修补 + 缓存写入档不映射（2.7 的第 7 件） | 每处一条针对性单测，用真实响应形状构造 |
| 6 | 模型目录 `kimiModels()` | `TestBuiltinCatalogValidates` 绿 |
| 7 | 前端类型 + 区域提示 + i18n + golden 前端夹具 + `provider-real-matrix.md` 加行 | `npm run typecheck && npm test` 绿；`internal/webui/dist` 已重建并提交 |
| 8 | 真实账号验证（见第 5 节） | 第 7 节的待验证项逐条关闭或改写 |

片 5 与片 1–4 独立，可以并行；片 8 需要真实 key，两地各一把。

**原来的「片 0：3.1 的定论」删掉了，因为它是个次序矛盾。**那一片要产出的定论依赖
第 5 节的第 4、5 条实测，而实测在片 8。3.1 现在已经在**没有实测的前提下**取了保守
的 (A)，正是为了不制造这个环 —— 实测回来只会让 (A) 变得更保守或更宽松，都不阻塞
实施。

---

## 5. 验证计划

**无凭据可做的**（已做的见 1.3）：

- 两个 host 的三条路由在无 key 时都 401，且错误体形状如 1.3 所记。
- OpenAPI 结构比对（1.2）。

**必须有真实 key 才能关闭的**，两地各跑一遍（大陆一把、国际一把，因为 key 不通用）：

1. `GET /v1/models` 的真实返回：模型集合、`context_length`、三个 supports 标志。
2. Chat 面一次非流式 k3 调用，读 `usage` 的四个成员 —— 关闭「`prompt_tokens` 是否
   含 `cached_tokens`」（1.7 第 2 条）与「`completion_tokens` 是否含推理」（3.4）。
3. Chat 面一次流式调用，**不带** `stream_options`，看末 chunk 有没有 usage（3.14）。
4. Chat 面 `temperature: 0.5` —— 确认确实 400 而不是被**忽略**。若是忽略，3.1 的
   整个论证换一条更糟的理由（静默改结果）成立，结论 (A) 不变。
5. Chat 面 `temperature: 1.0` —— 确认放行。这条决定 3.1 的 (B) 提案值不值得立项，
   **不阻塞本次实施**。
5b. 三条路由各发一个必然失败的请求（超长输入、非法模型），确认返回的是非 200 而
   不是「200 里裹着错误」（2.7 的第 6 件）。这是 MiniMax 上最贵的一类缺陷，Kimi
   目前只被无凭据的 401 排除了一部分。
6. Messages 面一次带 `output_config.effort` 的调用，读
   `usage.output_tokens_details.thinking_tokens` 的真实嵌套（1.7 第 3 条）。
7. Messages 面 `tool_choice: {"type":"tool","name":...}` —— 确认 400（3.7）。
8. Messages 面 `max_tokens` 与推理的关系（3.10）。
9. K2.x 传 `reasoning_effort`、k3 传 `thinking` —— 确认各自报错（3.2）。

真实 Provider 冒烟按 `docs/verification/provider-real-matrix.md` 的规矩：可选、
计费、默认不在 CI/dev 跑。

---

## 6. 明确不做的

- **Embeddings**：Kimi 没有这个端点。
- **文件接口**（`/v1/files*`）与 `ms://` 文件引用。
- **Batch**（`/v1/batches*`）：异步批处理是另一个生命周期模型。
- **`/v1/tokenizers/estimate-token-count`** 与 **`/v1/users/me/balance`**。
- **官方联网搜索 / 官方工具**：`ProviderExecutedTools` 意味着接受一条不经过
  SafeTransport 的上游出网，是一次契约评审。
- **Partial Mode**（Chat 面 assistant 消息上的 `partial: true`）：Halro 的语义模型
  没有这个概念，不建模。

  Messages 面另说，而且要说清楚，不能一句「照常工作」带过。Kimi 在
  `MessagesRequest.messages` 的描述里明写：**末条为 assistant 消息时，模型从该消息
  内容之后继续生成**。这与 Anthropic 原生行为一致，所以 native 模式下字节转发不引入
  新风险。要确认的是 portable 路径：**Halro 的 Messages 门面在什么情况下会产出一个
  末条为 assistant 的 body**（多轮对话把上一轮回复追加进来，恰好是最常见的形状）。
  若会，那么一个普通的多轮请求在 Kimi 上的语义与在 Anthropic 上的语义相同 —— 这不是
  缺陷，但它是一条**行为依赖上游约定**的路径，要在 manifest 的
  `DocumentedDeviations` 里写明，而不是留给读代码的人自己发现。
- **Responses 面的流式**（3.3）与 `namespace` 工具、动态工具加载（3.8）。
- **MFJS 校验**（3.7）。
- **视频输入**：Kimi 有 `video_url` 块，Halro 的语义模型没有视频。

---

## 7. 目前没有证据的结论（逐条）

写在这里的每一条都是文档读数或推断，**没有一条被真实请求确认过**。第 5 节列出了
关闭它们的方法。

1. Chat 面 `prompt_tokens` 是否含 `cached_tokens`。若不含，1.7 的换算方向要反过来，
   否则 `semantic.Usage.Validate()` 会拒绝或结算算错。**影响结算，最高优先级。**
2. Chat 面 `completion_tokens` 是否含推理 token（3.4）。
3. 不带 `stream_options` 时流式是否仍返回 usage（3.14）。**影响结算。**
4. `temperature` 传固定值是否放行、传其他值是否确实 400（3.1）。整条 3.1 的取舍
   建立在这个答案上。
5. `max_completion_tokens` 是否含推理 token（3.10）。
6. Messages 面 `thinking_tokens` 的真实嵌套位置 —— OpenAPI 写在
   `output_tokens_details` 里，但 OpenAPI 与实际实现不一致的情况见过。
7. Messages 面**会不会拒绝** `anthropic-version` 头。方向与第一版写反了：
   `internal/provider/anthropic/adapter.go:174` 是**无条件发**
   `anthropic-version: 2023-06-01` 的，所以「Halro 不发」不会发生。未知的是 Kimi
   对这个未文档化的头的处理 —— 忽略（几乎肯定）还是拒绝。
8. 429 是否带 `Retry-After` 头（文档提到「按 `Retry-After` 提示等待」，但没给示例）。
9. `/v1/models` 返回的模型是否只是账号可访问的子集，以及它是否包含
   Responses/Messages 不服务的模型（3.11 的推断）。
10. k2.7-code-highspeed 是否在 `/v1/models` 里作为独立 id 出现，还是只在文档里。
11. 两地的模型集合是否真的相同 —— 文档相同，但可用性可能按账号地区不同。
12. `stop` 超过 5 条或单条超 32 字节时是 400 还是截断。
13. Anthropic 面在 429 / 503 上的错误体形状（1.3 只测到 401）。

---

## 8. 与 MiniMax 方案的差异摘要

供实施时快速对照 `docs/prd/minimax-adaptation-plan.zh-CN.md`：

| 项 | MiniMax | Kimi |
| --- | --- | --- |
| 三条 wire、一个 surface、一把 bearer | 是 | 是 |
| 两个 host 同契约 | 是 | 是（已用 OpenAPI 结构比对证明） |
| 默认 profile | Anthropic 面（用量口径最全） | **Chat 面**（唯一覆盖全部四个模型） |
| `temperature` / `top_p` | 接受 | **拒绝**（全树第一例） |
| JSON 模式 | 仅 Chat 面 json_object，且靠实测 | 三面都有 `json_schema`，Chat 面另有 `json_object`，均已文档化 |
| `stop` / `stop_sequences` | 接受后忽略 → 拒绝 | **生效** → 携带（有长度约束） |
| Anthropic 面 `output_config` | 不接受 | **接受**（effort + format） |
| Anthropic 面块类型扩展 | 有 video / mid_conv_system | 无扩展，比 Anthropic 更少 |
| `tool_choice` 指定工具 | —— | **不支持**，须拒绝 |
| 缓存读的拼写 | Anthropic 面标准 | **三面三种拼写**，其中两种与现有解码器不符 |
| 缓存写档 | 按上游报，`CostEstimated` 兜底 | **不映射**：Kimi 没有缓存写价，映射反而少收钱（1.7 第四条） |
| 方言渲染器 | `compatibility/minimax.go` + `provider/openai/minimax.go` | 同样两个文件，但分叉的维度不同：MiniMax 按连接分，**Kimi 按模型分**（k3 与 K2.x 两套推理语法） |
| HTTP 200 携带错误 | 有（最高级缺陷），在 `/v1/embeddings` 上实测到 | **已部分排除**：Kimi 没有 embeddings 端点，三条 chat 路由无凭据实测都返回正规 401（1.3）。剩下的要靠真实 key 试一次（第 5 节 5b） |
| 推理开关 | 一个 on 状态，无深度梯 | **按模型分成两套互不通用的拼写**，两个模型无法关闭 |

---

## 9. 实施记录（2026-09-01，无真实密钥）

方案实施完毕，落在 `feat/kimi-adaptation-plan` 分支。本节记录**实施推翻方案的地方**
和**与方案不同的决定**，其余照方案执行。

### 9.1 方案写错、被实现推翻的一处 —— Anthropic 面没有落地

方案的 §2.2、§2.9 都把 `ProfileKimiAnthropicMessages` 当作三行之一。**它没有注册，
这一条是错的。**

问题不在注册步骤，在两端同时不通：

- **响应端。**`kimi-k3` 始终推理且 Preserved Thinking 始终开启，Messages 响应的
  content 块顺序文档写明是 `thinking → text → tool_use`，也就是**每一次响应都带
  thinking 块**。而 `internal/compatibility/anthropic/mapping.go` 的
  `DecodeResult` 遇到 `thinking` / `redacted_thinking` 直接返回
  `signed thinking response requires native mode`。portable 路径读不了 Kimi 的任何
  一次 Messages 回复。
- **请求端。**同一个文件的 portable mapper 在 `ReasoningEffort == ""` 时会写
  `Thinking: {"type":"disabled"}`。Kimi 的 `MessagesRequest` **没有 `thinking`
  成员**（它的推理开关是 `output_config.effort`），所以这个成员大概率会被 400。
  MiniMax 能用是因为它的拼写恰好就是 `thinking`，Kimi 不是。

于是这条路由只在 native 模式下可用。而 **Halro 表达不了「只走 native 的 profile」**：
portable 路由不排除 native profile（`internal/gateway/service.go` 只在 native 方向
做过滤），注册这一行等于往每个 portable 候选集里放一个**预算预留之后才失败**的目标 ——
正是这个仓库最贵的那类错误。

处置：不注册。`kimi.anthropic.messages.v1` 这个标识符保留、不得他用，理由写在
`internal/domain/provider_profile.go` 的常量注释里。要重新offer 它，需要先解决其中
一件事：portable 路径能携带带签名的 thinking 块，或者代码里出现「native-only
profile」这个概念。两件都不是 Kimi 适配的范围。

连带删掉的：`kimiAnthropicSet`、两个 Anthropic primitive 常量、
`anthropicWire` 绑定、`native.go` 与 `isNativeAnthropicProfile` 的注册、
`reasoningProbeEffort` 的排除项。方案 §2.9 整节因此**不适用**。

### 9.2 与方案不同的三处决定

**(1) `response_format` 在 Responses 面不做字段申报。**方案 §2.4 要求申报无 schema
的那一半。实现时两条既有守卫直接冲突：`TestTheManifestDeclaresEverythingTheRulesRefuse`
用合成的语义请求驱动，会看到 json_object 被拒而要求申报；
`TestResponsesCoverageDeclaresEveryOutputBudgetAndFormatLoss` 用真实的 portable 请求
驱动，那条路径上 `output_config.format` 永远是 schema，于是要求**不要**申报。

两条都对，冲突的是那条规则本身。正确答案是：**这件事该由能力位说，不该由字段规则说**
—— `kimiResponsesSet` 本来就没有 `JSONObject`，能力过滤会在字段规则之前把目标drop 掉。
申报是多余的第二个真相，删掉。

**(2) 缓存写入档不需要「不映射」的代码。**方案 §1.7 第四条要求「不填
`CacheWriteInputTokens`」。实现时发现 `openaiapi.ResponseTokenDetails` 本来就没有
`cache_write_tokens` 字段，所以 Kimi 的这个计数根本不会被解码，那段 token 自然留在
未缓存段按输入价结算 —— 正是方案想要的结果。**没有加代码，也没有加字段。**如果哪天
有人给这个结构体补上这个字段，就会无声地变成少收费，这一段记在这里就是为了那一天。

**(3) 探测器不加 200-携带错误的守卫。**方案 §2.7 第 6 件把它列为「先不写代码，拿到
key 后主动试」。实现照做了，并把理由写进 `internal/provider/openai/adapter.go` 的
`kimi` 字段注释：MiniMax 那条守卫是有实测支撑的，Kimi 这边三条路由的无凭据探测都返回
了正规 401，凭猜测加守卫是在昂贵的方向上猜。

### 9.3 照方案实施的部分

- 类型 `kimi`、surface `kimi-api`、凭据 `bearer.static`；`providerTypeTable` 一行，
  `LegacyDefaults` 取 `kimiChatSet`（§2.2 的补充落实）。
- 两行 profile 表，`Defaults == Ceiling`，默认 profile 是 Chat 面。
- 三个 primitive 常量、两条绑定、`kimi.responses` 登记为语义 primitive（§2.3 的
  无守卫一步）。
- `internal/compatibility/kimi.go`：采样参数拒绝、两套推理拼写按**精确模型标识符**
  分叉（跟随上游 OpenAPI 自己的 discriminator，不做前缀推断）、`tool_choice=required`
  的逐模型限制、`stop` 的 5 条/32 字节校验、两个输出上限归一、response_format 三态。
- `provider_fields.go` 两条规则；三份端点清单各两行 coverage；
  `docs/compatibility/endpoint-manifests.json` 重新生成。
- `provider_adapters.go` 两个 builder，Bearer only（Kimi 只文档化了这一种）。
- `internal/modelcatalog/builtin.go` 的 `kimiModels()`：五条 entry（k3 两面 + 三个
  K2.x 只在 Chat 面），k3 记输出上限、K2.x 不记。
- 前端：`types.ts` 两个联合类型、`ProvidersPage.tsx` 的类型清单与
  `regionHintKey()`、zh-CN/en-US 各三条文案；`internal/webui/dist` 已重建。
- `web/src/test/provider-profiles.golden.json` 与
  `docs/compatibility/endpoint-manifests.json` 两份 golden 已用
  `HALRO_UPDATE_GOLDEN=1` 重生成，diff 只含 Kimi 行。

### 9.4 新增的测试

- `internal/compatibility/kimi_test.go`：九条。其中
  `TestKimiChatFieldRulesAgreeWithTheRenderer` 是最有价值的一条 —— 它拿同一批请求
  同时驱动字段规则和渲染器，要求两者对「这个请求能不能走这条 profile」给出同一个答案。
  两者写在不同文件里，分歧的形状正是「路由放行、渲染器拒绝」，也就是预算预留之后
  才失败。
- `internal/app/kimi_wiring_test.go`：路由与 Bearer 头各一条，加上
  `TestKimiChatWiringRendersTheKimiDialect`。
- `manifest_derivable_coverage_test.go` 的探针电池加了 temperature / top_p 两条 ——
  这条轴此前从没被驱动过，因为 Kimi 之前没有任何上游拒绝它们。

### 9.5 反向验证

把 `internal/provider/openai/adapter.go` 的 `kimi` 开关改成常量 `false`（确认改动
真的生效：`grep` 到了 `kimi:                false`），`-count=1` 跑
`TestKimiChatWiringRendersTheKimiDialect`，四条断言全红：

```
kimi_wiring_test.go:141: a request naming temperature reached Kimi; it has no member for one
kimi_wiring_test.go:144: the refused request was still sent upstream
kimi_wiring_test.go:162: kimi-k2.6 was sent the top-level reasoning_effort, which it does not read
kimi_wiring_test.go:165: kimi-k2.6 was sent thinking , want {"type":"enabled"}
```

随后还原，重新跑绿。

### 9.6 现在的状态

代码可编译、全树测试绿、前端 500 条测试绿、embedded bundle 已重建。**没有一次请求
到过真实的 Kimi 账号**，第 5 节的验证计划一条都没关闭，第 7 节的 13 条假设一条都没
消掉。§9.1 的 Anthropic 面是唯一一处功能缺口，它是决定不是遗漏。

---

## 10. 实测记录（2026-09-01，大陆站真实账号）

`platform.kimi.com` / `https://api.moonshot.cn`，Tier 未知（余额 ¥25，其中赠送 ¥15）。
密钥从未进入仓库、日志或本文件。三轮探针共约 30 次调用，总花费不足 ¥1。

### 10.1 三条推翻了已写下的判断

**(1) §9.1 是错的：Anthropic 面可用，已恢复。**

§9.1 从 Kimi 的 OpenAPI 推出两条，两条都被实测证否：

```
POST /anthropic/v1/messages  {"thinking":{"type":"disabled"}, ...}
  -> 200, content: [{"type":"text","text":"OK"}]      ← 没有 thinking 块
```

`thinking` 不在 Kimi 的 `MessagesRequest` schema 里，但它**被接受**，而且正是
Halro 的 portable mapper 对「没要求推理」的请求所发的那个成员；返回体里也没有
portable 解码器拒绝的 thinking 块。**文档的沉默不是上游的拒绝** —— CLAUDE.md 里
那条规矩，这次是自己撞上的。

对照组（不发 thinking，让 k3 用默认）：

```
POST /anthropic/v1/messages  {...}
  -> 200, content: [{"type":"thinking",...},{"type":"text","text":"OK"}]
```

所以 §9.1 判断里唯一正确的那半保留了下来，并变成一条字段规则：**portable 路径不得
在这条 face 上申请任何推理深度**（`reasoning_effort` 全值申报不支持），否则回来的
thinking 块会在上游已收费之后被解码器拒掉。native 模式不受影响，能力位保持
`Reasoning: true`。

**(2) 方案 §3.10 是错的：Kimi 的单一输出上限计推理 token。**

```
max_completion_tokens: 48, model kimi-k3
  -> completion_tokens 48，其中 reasoning_tokens 45，content 为空，finish_reason=length
max_tokens: 48（同模型同提示）
  -> 完全相同的形状
```

方案说「Kimi 没有说明这两者语义不同……都渲染到 `max_completion_tokens`」。实测
表明：两个成员确实是同一个量，但**那个量包含推理**。于是 Halro 的
`VisibleOutputTokenLimit`（只约束答案）**任何时候都不是同一个量** —— Kimi 每个模型
默认都推理。原实现把它无条件搬进 `max_completion_tokens`，等于把调用方的答案预算
悄悄改成「答案+推理」的预算；上面那次请求付了 48 个 token，一个字答案都没拿到。

已改成 MiniMax 那条规则的更强版本：只要请求带 `VisibleOutputTokenLimit` 就在
provider I/O 之前路由绕开，Chat 与 Responses 两条 profile 都是。代价写在清单里：
Anthropic 门面（`max_tokens` 必填）因此不再落到 Kimi 的 Chat/Responses profile，
而是落到 Kimi 自己的 Anthropic profile —— 那条 face 上推理是关的，两个量才重合。

**(3) Anthropic 面的推理 token 藏在嵌套里，原来读不到。**

```
"usage":{"input_tokens":88,"cache_read_input_tokens":0,"output_tokens":58,
         "output_tokens_details":{"thinking_tokens":42}}
```

`anthropicapi.Usage` 读的是顶层 `thinking_tokens`，Kimi 放在
`output_tokens_details` 里 —— 每一次 Kimi 推理都会被记成 0。已加
`Usage.ReasoningTokens()` 解析两种拼写，四处调用点全部改为走它。反向验证：把
`compatibility/anthropic/mapping.go` 的那一行改回顶层字段，
`TestKimiAnthropicWiringReadsTheNestedThinkingTokens` 报
`reasoning tokens read as 0, want 42`。

### 10.2 关掉的假设（第 7 节逐条）

| # | 假设 | 实测结论 |
| --- | --- | --- |
| 1 | Chat 的 `prompt_tokens` 是否含 `cached_tokens` | **含**。同一请求两次：`prompt_tokens` 都是 1384，第二次 `cached_tokens` 1280。而且 Kimi **同时**发 `prompt_tokens_details.cached_tokens`（OpenAI 标准嵌套），所以既有解码器本来就读得到；顶层那个是冗余的第二处拼写 |
| 2 | Chat 的 `completion_tokens` 是否含推理 | **含**，而且 Chat 面**确实上报** `completion_tokens_details.reasoning_tokens` —— 方案 §3.4「Chat 面不报推理 token」是错的，OpenAPI 的 usage schema 漏了这个成员 |
| 3 | 不带 `stream_options` 时流式是否仍报 usage | **仍报**，但位置非标准：在 `choices[0].usage` 里。**带** `include_usage` 时另发一个 `choices:[]` 的收尾 chunk，usage 在顶层 —— 那是 OpenAI 标准形状，也正是 Halro 一直在发的选项，所以**没有缺陷**，无需改代码 |
| 4 | `temperature` 传非固定值是否报错 | **报错**：`400 invalid temperature: only 1 is allowed for this model`。`top_p` 与 `n` 同形（`only 0.95` / `only 1`） |
| 5 | 传固定值是否放行 | **放行**（`temperature: 1.0` → 200）。所以 §3.1 的 (B) 提案是有真实收益的，但仍单独立项，不塞进本次 |
| 6 | Messages 的 `thinking_tokens` 嵌套位置 | 见 10.1(3)，OpenAPI 是对的，实现读错了 |
| 7 | Messages 是否拒绝 `anthropic-version` | 未单独验证，但适配器一直无条件发该头，三次 Messages 调用全部 200 —— **等价于已排除** |
| 8 | 429 是否带 `Retry-After` | **带**，`retry-after: 1`，另有 Kimi 自己的 `x-retry-after: 1`。~~没有~~ —— 这一格第一版写的是「没有」，那是**没有证据的断言**：大陆那轮的 429 全部出现在第一遍并发探针里，而那一遍根本没抓响应头。国际站故意打穿限速后抓到了头，见 §10.6 |
| 9 | `/v1/models` 是否只是子集 | 返回四个在售模型，与文档一致；额外带 `context_length`、`supports_image_in`、`supports_video_in`、`supports_reasoning`、`supports_dynamic_tools`，k3 还带 `think_efforts` / `reasoning_efforts`（`["low","high","max"]`，默认 `max`）与 `supports_thinking_type:"only"` |
| 10 | highspeed 是否作为独立 id 出现 | **是**，`kimi-k2.7-code-highspeed` 在列表里 |
| 11 | 两地模型集合是否相同 | **相同**，逐字段。见 §10.6 |
| 12 | `stop` 超限是 400 还是截断 | **400**：`stop array too long. Expected an array with maximum length 5, but got 6`。渲染器提前拦，省一次往返 |
| 13 | Anthropic 面 429/503 的错误体形状 | **429 关闭**（OpenAI 形状），**503 仍未关闭**且造不出来。见 §10.6 与 §10.7 |

### 10.3 文档说错、实测纠正的四处

- **K2.x 不接受 `reasoning_effort`** —— 假。`kimi-k2.6` + `reasoning_effort:"high"`
  返回 200 并推理了。**k3 也接受 `thinking`**，返回 200。两者都没有证据表明被
  *honour*，所以渲染器仍按各自文档化的拼写发 —— 被忽略的成员比被拒绝的成员更贵。
- **K2.x 不支持 `tool_choice:"required"`** —— 说法不准。真实错误是
  `tool_choice 'required' is incompatible with thinking enabled`：它跟思考开关冲突，
  不是跟模型冲突。Anthropic 面的具名工具同理
  （`tool_choice 'specified' is incompatible with thinking enabled`），而**关掉思考后
  具名工具正常工作**，返回了 `tool_use` 块 —— 所以 portable 路径（永远关思考）能用
  具名工具，没有申报它。
- **Responses 与 Messages 只服务 `kimi-k3`** —— 假。`kimi-k2.6` 在两条 face 上都
  返回 200。模型目录已按实测写，不按 schema 的 `enum` 写。
- **Messages 面无 `temperature`** —— schema 里确实没有，但 `temperature: 1.0`
  返回 200。其他取值未测；按 fail-closed 处理，portable 与 native 两条路径都拒。

### 10.4 确认无误、未动代码的部分

- Messages 的 `input_tokens` **不含**缓存（`input_tokens:0` + `cache_read:88` +
  `prompt_tokens:88`），Anthropic 惯例，`PromptTokens()` 的加回逻辑正确。
- Messages 流式事件就是 Anthropic 的六个，usage 在 `message_delta` 上，字段名是
  Anthropic 的。
- Responses 面返回 `type:"reasoning"` 输出项（带 `summary`）—— 正是 canonical
  mapper 丢弃的形状，该 profile 不声明 Reasoning 是对的。
- Responses 的 `input_tokens_details.cache_write_tokens` 存在，而
  `openaiapi.ResponseTokenDetails` 没有这个字段 —— 那段 token 自然落在未缓存段按
  输入价计，正是 §9.2(2) 想要的结果。
- `stop_sequences` 在 Anthropic 面**生效**（`hello STOPHERE world` → 输出 `hello `），
  所以照 MiniMax 那样拒绝它是错的，Kimi 这边携带。
- `output_config.format` 的 json_schema 生效，返回了合模式的 JSON。
- 退役模型 404 `resource_not_found_error`。
- **没有发现「HTTP 200 里裹错误」** —— 所有失败都是正规状态码。

### 10.5 一件不该忘的事：Tier 限速直接打穿了第一轮

第一轮 26 条探针并发发出，**20 条拿到 429**，`rate_limit_reached_error`。
方案 §3.12 预测过这件事，但那是写给连接测试的；实测证明它对**任何**批量动作都成立。
第二、三轮改成串行 + 每 22 秒一次才跑通。

对实现的含义没有变化（连接测试本来就要串行、429 与 401 要分开报），但对运维文档有：
`docs/verification/provider-real-matrix.md` 里记了这条，任何 Kimi 冒烟脚本都必须配速。

---

## 11. 国际站实测与两条收尾（2026-09-01）

`platform.kimi.ai` / `https://api.moonshot.ai`，另一把密钥（两地不通用）。7 次常规
调用 + 6 次故意打穿限速。密钥同样已从磁盘删除。

> 本节编号接在第 10 节之后，§10.2 表格里被本节推翻的两格已就地改正并标出原文。

### 11.1 两地确实是同一份契约（假设 11 关闭）

`GET /v1/models` 两地逐字段比对（剔除 `created`、`permission`、`root`、`parent`
这些非契约字段）：

- 模型 id 集合相同：`kimi-k3`、`kimi-k2.6`、`kimi-k2.7-code`、
  `kimi-k2.7-code-highspeed`。
- **每个模型的每个字段都相同** —— `context_length`、`supports_image_in`、
  `supports_video_in`、`supports_reasoning`、`supports_dynamic_tools`，以及 k3 的
  `think_efforts` / `reasoning_efforts` / `supports_thinking_type`。比对脚本逐 key
  找差异，一条都没打印。

其余四条也一致：

| 探针 | 大陆 | 国际 |
| --- | --- | --- |
| Chat k3 usage | `prompt 88 / completion 16 / reasoning 13` | 逐字段相同 |
| `temperature: 0.5` | 400 `invalid temperature: only 1 is allowed for this model` | 同一句 |
| Anthropic 面 + `thinking:disabled` | 200，无 thinking 块 | 相同 |
| Anthropic 面 `kimi-k2.6` | 200 | 相同 |
| Responses k3 usage | 带 `input_tokens_details.cache_write_tokens` | 相同 |

所以 §1.2 用 OpenAPI 结构比对得出的「一份契约、两个 host」结论，在**运行时**也成立。
`BaseURLTemplate` 用一栏文案而不是两组 profile 的决定是对的。

### 11.2 Anthropic 面的 429（假设 13 的一半关闭）

在 `/anthropic/v1/messages` 上并发发 6 次，5 次 429。**经运营者同意后故意触发** ——
429 不计费，代价是账号被限流几秒。

```
HTTP/2 429
content-type: application/json; charset=utf-8
retry-after: 1
x-retry-after: 1
x-msh-trace-id: 99f3afaa...

{"error":{"message":"Organization Rate limit exceeded, please try again after 1 seconds",
          "type":"rate_limit_reached_error"}}
```

两条结论：

1. **形状是 OpenAI 的，不是 Anthropic 的。**于是这条 face 的分布是：400 用 Anthropic
   形状，401 和 429 用 OpenAI 形状。同一个端点按状态码换形状，这不是猜测了，是三个
   状态码各自量到的。解码器两种都吃，`envelope.Error.Type` 在两种形状下都解得出，所以
   `rate_limit_reached_error` 和那句 message 都留住了。
2. **`retry-after: 1` 是有的**，另有 Kimi 自己的 `x-retry-after`。
   `decodeHTTPError` 已经在读 `parseRetryAfter(response.Header)`，所以退避信息本来就
   到得了熔断器，无需改代码。

**§10.2 第 8 格原来写「没有 Retry-After」，那是错的，而且错得有教训**：大陆那轮的
429 全部出现在第一遍并发探针里，而第一遍根本没加 `-D` 抓头。「没抓到」被我写成了
「没有」。这正是 CLAUDE.md 那条 —— 验证不了就说验证不了，别把假设写成结论。

### 11.3 503 关不掉，改为验我们自己的容错

503 要上游真的不可用才会出现，造不出来也等不来。硬要「关闭」它只有两条路，两条都是
把猜测当证据：拿 400 的形状去推，或者喂个假 body 然后声称量到了 Kimi 的形状。

所以 503 **保持未关闭**，另加一条测试，名字写明它验的是哪一边：
`TestAnthropicErrorDecodingToleratesShapesItHasNotSeen`
（`internal/provider/anthropic/kimi_error_tolerance_test.go`）。它钉的是无论形状如何都
必须成立的性质：

- 分类只看状态码，解不出 body 也不会失败；
- **不把上游响应字节带进错误消息** —— 这是不变量，不是体面问题。用例包括 Kimi 文档
  里那种 504 HTML 超时页和一个陌生形状的 JSON，断言 `upstream timed out`、`abc`
  这些 body 片段不出现在 `err.Error()` 里；
- 已量到的三种真实 body（401 的 OpenAI 形状、400 的 Anthropic 形状、429 的 OpenAI
  形状）各一条用例，message 与分类都钉住。

### 11.4 现在还开着的

只剩 503 一条，且**没有办法主动关闭**。等它真的发生时，`x-msh-trace-id` 会在响应头里，
届时把 body 贴进 §11.2 的表即可。

---

## 12. 运行中暴露的缺陷：推理内容渲染不出去（2026-09-01）

启用 Kimi 后，运营者看到的是这两条，一前一后：

```
502  provider_error       provider response cannot be rendered safely
503  provider_unavailable no healthy deployment is available for this model
```

**第二条是第一条的后果**，不是新问题：连续的 provider 错误把 deployment 标成不健康，
`ResolveCandidatesFor` 之后返回空，就落到 `service.go:250` 那条 503。

### 12.1 根因

Kimi **每个模型默认都推理，调用方问不问都一样**。这是第 10 节量到的事实，但当时只
用它改了输出上限那条规则，没有想到它对**响应渲染**意味着什么。

```
Kimi 返回 reasoning_content（Chat 面）／reasoning 输出项（Responses 面）
  → Halro 解成 semantic 的 ContentReasoning
  → 北向渲染:
      POST /v1/responses -> "content kind cannot be represented by Responses"
      POST /v1/messages  -> "provider result contains non-portable content"
  → gateway 502
```

复现方式是拿第 10 节量到的三条真实响应体，走一遍 decode→render，两条北向路径都炸。
Chat Completions 北向没事 —— 那条 wire 有 `reasoning_content` 这个成员。

**最贵的部分是时机**：上游调用已经成功、已经计费，是在往回渲染时才失败。调用方付了
钱，一个字也没拿到。

### 12.2 为什么之前没被挡住

Messages 北向其实**是**被挡住的，但挡住它的是别的理由 —— §10.1(2) 那条
`max_tokens` 规则（Messages 必带该成员，Kimi 的单一上限计推理，所以路由绕开）。
**为了正确的结果、出于不相干的原因**被挡住，这种挡法经不起以后有人动那条规则。

Responses 北向没有任何东西挡：一个普通的 `/v1/responses` 请求可以不带任何输出上限，
Kimi Chat 的字段规则一条都不触发，于是必炸。

### 12.3 处置

字段规则里加两条，按**北向 profile** 判定 —— 因为决定这件事的正是它：同一个 Kimi
目标在 `/v1/chat/completions` 上完全正常。

```go
add(request.Source.ProfileID == string(ProfileOpenAIResponses), "reasoning")
add(request.Source.ProfileID == string(ProfileAnthropicMessages), "reasoning_effort")
```

`ProfileKimiChat` 与 `ProfileKimiResponses` 各一份。Messages 北向那条因此从「被别的
规则顺带挡住」变成「明说」。`ProfileKimiAnthropicMessages` 不受影响 —— 那条 face 上
portable mapper 会关掉思考，回来没有推理内容可渲染，Messages 北向仍然有 Kimi 路由。

顺带补了一处清单失真：`openai.responses.stateless.v1` 的 `RequestFields` 里没有
`reasoning`，而 `responses.go` 一直在读 `request.Reasoning.Effort`。已加上。

**这是今天能写出的诚实形状，不是正确的那个。**正确的是一个能力位 ——「这个目标总是
推理」—— 与每个北向端点能承载什么做过滤，也就是 `adding-a-platform.md` 第 4 步那条
分层（目标的属性是能力，不是字段申报），和 §3.1 里为固定采样参数推迟掉的是同一条。
在那个能力位存在之前，只能写一条点名端点的规则。

### 12.4 探针电池当时看不见这条规则

`coverageProbes()` 构造的请求 `Source` 是空的，所以任何按北向判定的规则它都看不到 ——
拒绝和申报会**同时**隐形，而这正是那份测试存在的理由。已改成
`coverageProbes(manifest.ID)`，每条探针带上它所属端点的身份。

### 12.5 反向验证

把那两条规则从两个 profile 里删掉（`grep` 确认删干净：命中数 0），跑
`TestKimiTargetsAreRefusedByEndpointsThatCannotCarryReasoning`，四条断言全红：

```
kimi.chat.v1      is offered to openai.responses.stateless.v1
kimi.chat.v1      is offered to anthropic.messages.2023-06-01
kimi.responses.v1 is offered to openai.responses.stateless.v1
kimi.responses.v1 is offered to anthropic.messages.2023-06-01
```

还原后全绿。

### 12.6 一条值得花一次请求去问的问题（已问，答案见第 13 节）

现在的处置是把 Kimi 的两条 OpenAI-wire profile 从两个北向端点上拿掉。**如果 Kimi 的
Chat 面接受 `thinking:{"type":"disabled"}`，那就完全不必这样** —— 像 MiniMax 和
DeepSeek 那样，在调用方没要求推理时主动关掉，推理内容根本不会回来，三个北向端点全部
可用。

有一条强提示：`kimi-k3` 在 **Anthropic 面**上接受 `{"type":"disabled"}` 并且真的不
推理（§10.1(1) 实测）。但那是另一条 face，「同一个模型在另一条 face 上如此」正是这
份方案已经栽过一次的那种推断，所以不据此改实现。

**一次请求就能定论**：`POST /v1/chat/completions` 带
`{"model":"kimi-k3","thinking":{"type":"disabled"},...}`，看 `reasoning_content` 是
不是空、`reasoning_tokens` 是不是 0。是的话，§12.3 那两条规则应当换成渲染器里的一个
关闭开关。

---

## 13. §12 的修复被自己的实测推翻（2026-09-01）

§12.6 说那一条要花一次请求去问。问了，答案让 §12.3 整个作废。

### 13.1 实测：kimi-k3 能关，文档第五处错

国际站，四次调用：

| 模型 | 发 `thinking:{"type":"disabled"}` | content | reasoning_content | reasoning_tokens | finish |
| --- | --- | --- | --- | --- | --- |
| `kimi-k3` | **200** | `OK` | 空 | 无 | stop |
| `kimi-k3`（对照，不发该成员） | 200 | **空** | 一大段 | **59** | **length** |
| `kimi-k2.6` | 200 | `OK` | 空 | 1 | stop |
| `kimi-k2.7-code` | **400** | — | — | — | `invalid thinking: only type=enabled is allowed for this model` |

**「K3 始终进行推理」是错的**，`/v1/models` 里那个 `supports_thinking_type:"only"` 也
不表示不能关。这是这份适配里 Kimi 文档第五处被实测推翻。

那两行对照就是运营者遇到的故障本身：同一模型、同一提示、同样 64 token 预算 ——
不发开关是「预算全烧在推理上、答案为空、finish=length」，发了就是干净的 `OK`。

`kimi-k2.7-code` 与 `-highspeed` 是真关不掉，这次文档是对的。

### 13.2 §12.3 错在哪

按北向 profile 把 Kimi 的两条 OpenAI-wire profile 从 Responses 与 Messages 端点上
拿掉，有两个问题：

1. **范围下错了一档。**这件事是**逐模型**的 —— 四个模型里两个能关。按 profile 一刀切
   连带把 k3 和 k2.6 也拿掉了。
2. **它违背了网关自己的承诺。**README 与 CLAUDE.md 都写着应用只看到 Gateway Key 与
   公开模型别名。运营者把别名指向 Kimi，应用就得改对接方式 —— 这正是网关该消掉的那类
   差异，不是该制造的。运营者的原话：「开始使用的是 Responses API，后来项目调整使用
   kimi，那对接的 API 就不能用了」。

而**房规早就有**，只是我在 Kimi 上偏离了它：
`docs/prd/deepseek-adaptation-plan.zh-CN.md` §9 记着 DeepSeek 在 2026-08-18 撞过一模
一样的链条，§9.2 定的修法是「**未指定即关，与 Anthropic 同规则**」，理由写得比我这里
清楚：

> 除了修掉这个失败，它还消掉一处与调用方无关的差异：在此之前，同一个 portable 请求，
> 路由到 Anthropic 就不思考、路由到 DeepSeek 就思考并多计一段输出费。**网关存在的
> 意义之一就是消掉这种差异。**

我当初偏离它的理由是「k3 文档说关不掉，没有 off 可发」。前提是假的。

### 13.3 改了什么

**(1) 渲染器：未指定即关。**`applyKimiReasoning` 在 `effort == ""` 时，对有开关的模型
发 `thinking:{"type":"disabled"}`。`kimiReasoningSpelling` 里 `kimi-k3` 的 `CanDisable`
按实测改成 true。没有开关的（k2.7-code 系列）与未知标识符仍然不发 —— 猜一个拼写正是
让「什么都没要求的请求」被计推理费的方式。

**(2) §12.3 那两条按北向 profile 的规则整个删掉。**三个北向端点上 Kimi 全部可用。

**(3) `none` 变成可达。**既然未指定即关，再把显式的「不要思考」路由绕开就是自相矛盾：
说了被拒，不说反而如愿。`KimiPortableEfforts` 现在是 `{none, low, high}`。代价写明：
`kimi-k2.7-code` 上 `none` 会被渲染器拒 —— **预算预留之后**，这是这条 profile 唯一的
一处，接受它是因为另一条路（整条 profile 拒绝 `none`）对四个模型里两个是错的。

**(4) `max_tokens` 规则收窄到「确实会推理时」。**Kimi 的单一上限计推理，但推理关掉时
它和答案预算就是同一批 token。原来那条无条件申报把**每一个带 `max_tokens` 的普通
Chat 请求**都挡在 Kimi 之外 —— 那是 OpenAI 客户端最常见的写法。现在只在
`ReasoningEffort != ""` 或两个上限同时出现时申报。同样的 k2.7-code 残留：那个模型上
渲染器会拒。

保留下来的两处（它们是独立的修正，与本节无关）：`openai.responses.stateless.v1` 的
`RequestFields` 补上 `reasoning`；`coverageProbes(manifest.ID)` 让探针带上所属端点身份。

### 13.4 端到端复核

渲染器发出的请求体，以及 Kimi 对它的真实回答，走一遍北向 Responses：

```
request Halro sends: {"model":"kimi-k3","messages":[...],"thinking":{"type":"disabled"}}
responses status=completed output items=1
```

之前这条路径是 502。

### 13.5 反向验证

把「未指定即关」那三行删掉（`grep` 确认命中数 0），
`TestKimiSwitchesReasoningOffWhenNobodyAskedForIt` 报：

```
kimi-k3   with nothing asked sent thinking "", want "{\"type\":\"disabled\"}"
kimi-k2.6 with nothing asked sent thinking "", want "{\"type\":\"disabled\"}"
```

还原后全绿。

### 13.6 剩下的：kimi-k2.7-code

两个 k2.7-code 标识符真的关不掉，于是在它们上面仍有两处**预算预留之后**的拒绝：
显式 `none`，以及带 `max_tokens` 的请求。这是这次适配唯一没有被推到 provider I/O
之前的失败。

它是**目标的属性**（「这个目标总是推理」），该走能力位 + 目标级过滤，也就是
`adding-a-platform.md` 第 4 步那条分层 —— 和 §3.1 为固定采样参数推迟掉的是同一条。
现在有两个具体理由要它，不再是一条抽象的架构偏好。**单独立项**：它会动到
`ProviderCapabilities`、每个 profile、console 与两份 golden，不该混在 Kimi 适配里。

在那之前，运营者要知道的是：**`kimi-k2.7-code` 与 `kimi-k2.7-code-highspeed` 在
Responses 与 Messages 北向上仍可能出现「上游成功但渲染失败」的 502**，其余两个模型
不会。

> **§14 更正**：上面这句「其余两个模型不会」只对 Chat 与 Anthropic 两条 provider
> profile 成立。`kimi.responses.v1` 上它对**所有**模型都不成立，而且是每次调用都
> 发生；该 profile 已因此收起，见 §14。

## 14. §13 漏掉的第三面：`kimi.responses.v1` 被收起（2026-09-02）

§12 与 §13 修的是 Chat 面与 Anthropic 面。多角色评审复查这条分支时发现，同一条链
在 **Responses 面上一次都没有修过**，而且它在那里不是「某些模型会」，是每一次调用
都必然发生。

### 14.1 链路

三段逻辑相乘，每一段单独看都合理：

1. `provider_fields.go` 对 `ProfileKimiResponses` 全值拒 `reasoning_effort` ——
   理由是 canonical response mapper 保不住 reasoning item，写在 §3.3。
2. `RenderProviderResponseRequest` 只在 `ReasoningEffort != ""` 时才写 `reasoning`
   成员。上一条保证它永远是空，所以请求体**永远不带关闭开关**。
3. Kimi 的 `/v1/responses` 默认 `reasoning.effort = max`（§1.6），于是每次都推理，
   返回 `type:"reasoning"` 输出项；`DecodeProviderResponse` 的 `switch` 没有这个
   case，落到 `default:` **返回错误**。

结果是 `ErrorMalformed, Ambiguous: true` → 502 → 按估算值保守结算。也就是 §12 那次
生产事故的同一形状，在第三个面上。

### 14.2 前提里的那个动词

`provider_table.go` 与本文 §3.3、§10.4 都写着 canonical mapper 会把 reasoning item
「丢弃 / drops」。**它不丢弃，它报错。** 「该 profile 不声明 Reasoning 就安全」这个
结论，整个建立在这个错误的动词上。§10.4 把「Responses 面返回 reasoning 输出项」记为
「确认无误、未动代码的部分」，实际上那一行正是缺陷的证据。

### 14.3 为什么这次不是「让上游不产出」

`docs/contracts/adding-a-northbound-endpoint.md` 的推论 2 说：优先让上游不产出，而
不是拒绝路由。Chat 面与 Anthropic 面都是这么修的（送 `thinking:{"type":"disabled"}`）。

Responses 面暂时做不到：它的 `reasoning.effort` 阶梯是 `low`/`high`/`max`，**没有
关闭档**（§1.6）。Chat 面能关靠的是 `thinking`，Messages 面能关靠的是文档里没有、
§13.1 实测出来的 `thinking` —— 这一面有没有对应的未文档化开关，**没有测过**。
猜一个成员发出去，代价是同一笔预留后的 400。

### 14.4 处置：`Withheld: true`

用仓库现成的机制收起这条 profile：实现保留、走 profile 表的不变量测试照跑、服务矩阵
不列、所有写路径拒绝。

代价是有界的，这是选它而不是选别的原因：**没有任何北向端点因此失去 Kimi**。Chat 面
服务全部四个模型，Anthropic 面服务两个，`/v1/chat/completions`、`/v1/responses`、
`/v1/messages` 三个北向端点都仍然可以路由到 Kimi。别名可重指的承诺没有被动到 ——
§12.3 走错的那一步正是动了它。

这也是第一次从一个**已开放的连接组中间**收起单条 profile（Bedrock 那五条是整组收起）。
两处因此要跟着改，都在这次一并做了：`summaryOf` 原本没有复制 `Withheld`，所以连接组
里读到的永远是 false；控制台的 `combines_with` 原本会宣称一个 Kimi 凭据也覆盖这条
profile，而保存时会被拒 —— 正是这个端点存在的目的所要防的那种「表单比服务端宽」。

### 14.5 解除条件：已测，答案是没有（2026-09-02，大陆站真实账号）

这一节原本写的是一个待测的条件。已经测了，三条都跑在 `kimi-k3` 上。

**对照组**，不带任何推理成员：

```
{"model":"kimi-k3","input":"Reply with OK.","max_output_tokens":64}
  -> 200, status "incomplete", incomplete_details.reason "max_output_tokens"
     output: [ {"type":"reasoning", summary:[...]} ]        ← 只有这一项
     usage: output_tokens 64, output_tokens_details.reasoning_tokens 61
```

**没有任何 message 项**。64 个输出 token 里 61 个花在推理上，调用方一个字答案都没
拿到。§12 的原始形状在这一面上直接复现——而且比 Chat 面更彻底，因为这里连一段可以
渲染的文本都没有。

**写法一**，OpenAI 阶梯上的 `none`：

```
{"...","reasoning":{"effort":"none"}}
  -> 400 invalid_request_error: reasoning.effort value "none" is not supported
```

文档这次是对的：阶梯就是 `low`/`high`/`max`，没有关闭档。

**写法二**，Messages 面上那个未文档化、实测有效的 `thinking` 成员：

```
{"...","thinking":{"type":"disabled"}}
  -> 200, output 里仍是 reasoning 项, reasoning_tokens 61
```

**被接受了，并且被忽略了。** 这是三种结果里最坏的一种，也是这次非测不可的原因：
同一个 key、同一个模型、同一家上游，`thinking` 在 Messages 面上真的关掉了推理，在
Responses 面上收下之后当没看见。要是按 §13.1 的经验直接外推把它加上去，得到的会是
一个 200、一份账单、和一个以为自己关掉了推理的调用方——比被拒绝贵得多。

**结论：`kimi.responses.v1` 保持收起，而且理由从「没测过所以保守」变成了「测过，
这一面关不掉」。** 重新开放需要下面任一件事发生，两件都不在 Halro 这边：

- Kimi 给 `/v1/responses` 加一个真正生效的关闭写法；
- 或者 §13.6 那个「这个目标总是推理」的能力位落地并进入路由，让北向端点在预留之前
  就把这类目标绕开——那时这条 profile 可以带着「不可关闭」的标记重新开放，而不是靠
  关掉推理。

`TestNoEndpointIsServedByATargetThatReasonsUnasked` 跳过 withheld 的 profile，所以
在那之前谁把 `Withheld` 删掉，都会在那个守卫上失败。

### 14.5.1 顺带落实的两件事

- **Responses 面不拒绝未知成员。** `thinking` 不在它的 schema 里，它回了 200。这答掉
  了评审里悬着的一条：`RenderProviderResponseRequest` 无条件发 `store:false`，而 Kimi
  的 Responses schema 没有 `store` 成员——如果它严格拒绝未知成员，那每次调用都会是预留
  之后的 400。它不严格拒绝。（响应体里也回显了 `"store":false`。）
- **采样值与缓存口径确认。** 响应回显 `temperature:1, top_p:0.95, presence_penalty:0,
  frequency_penalty:0`，与「每个模型钉死一组值」一致；`input_tokens:89` 且
  `input_tokens_details.cached_tokens:89`，证实这一面的 `input_tokens` **含**缓存，
  与 §10.4 记的 Messages 面（不含）相反。

### 14.6 这一轮同时修掉的

- `applyKimiToolChoice` 原本按模型判断（k3 可以 required、K2.x 不行），与 §10.3 自己
  记的实测相反 —— 真实冲突是「forced tool call vs thinking enabled」。双向都错：
  k3 带深度 + required 被放行到上游 400，k2.6 不带深度的普通请求被误拒。改为按
  `kimiThinkingWillBeOn` 判断。
- 能力探测对 Kimi 发 `reasoning_effort:"minimal"`，不在 Kimi 阶梯上，探测在进程内
  必然失败、Reasoning 永远拿不到验证证据 —— DeepSeek 当初的同一个坑。补了 case，并把
  那个硬编码四 profile 的回归测试改成走 profile 表，下一个短阶梯平台会被指名。
- `stop` 的上界（≤5 条、≤32 字节）原本只在渲染器里查，即预留之后。搬到字段规则；
  「这一层查不了长度」的旧理由不成立，`Stop` 就是 `[]string`。
- `max_tokens` 规则把 `reasoning_effort:"none"` 当成「要思考」，于是把最省钱的组合
  （显式不推理 + 答案预算）路由绕开。改用与渲染器共享的 `KimiEffortAsksForDepth`。
- 四处与代码矛盾的注释（`kimi.go` 两处、`builtin.go`、`provider_table.go`）以及
  manifest 里那句「unlike MiniMax there is no reasoning-counting difference」——
  最后这句会原样发布进 `docs/compatibility/endpoint-manifests.json`。

## 15. 评审 P2：把三处「没有守卫」补上两处（2026-09-02）

§14 处理的是缺陷本身。这一轮处理的是「为什么它三次都没被测试拦下」，对象是
`docs/contracts/adding-a-northbound-endpoint.md` 自己点名的三处无守卫。该文档的
「Steps with no mechanical guard」一节已按结果改写，这里只记与 Kimi 相关的部分。

### 15.1 端点渲染不了什么 × 上游不问自答什么

`TestNoEndpointIsServedByATargetThatReasonsUnasked`（`internal/app/`）。这是
DeepSeek 2026-08-18、Kimi §12、以及 §14 三次事故的共同形状，此前没有任何测试把两半
配起来。

- **端点这一半由执行得出**，不是手写清单：把一个带 reasoning part 的结果交给端点真正
  的渲染器，再交一个只带 text 的作对照。第一版没有对照组，于是把唯一能渲染 reasoning
  的 Chat 面也报成不能——那不是端点的答案，是我的 fixture 无效。
- **上游这一半是 `modelcatalog.Entry.ReasonsUnasked`**，**刻意不放进
  `ProviderCapabilities`**：那个结构里每个成员回答的都是「这项能不能开」，对运营者是
  复选框，还要被连接 ceiling 包含。「不管谁问都会来」不属于这三者中的任何一个，放进去
  会把包含关系反过来，并多出一个语义与其余全部相反的复选框。它是 per (profile, model)
  的，因为这条事实同时依赖两者：Kimi 的 Responses 面对所有模型都成立，Chat 面只对
  k2.7-code 那条线成立。

两条明确的限制：它**还没有进路由**（要进路由得把这条事实穿过 deployment 能力快照，那是
durable state，另立一件事）；已经错了的配对以 residue 列在测试里、附各自的实测依据，
守卫在这个清单**变长**或其中一条**不再成立**时都会失败。今天的 residue 是
`kimi-k2.7-code` 与 `kimi-k2.7-code-highspeed` 在 `/v1/responses` 与 `/v1/messages`
上——两者都没有关闭开关，每次调用都是上游计费 + 502。

**withheld 的 profile 被跳过**，这正是把「收起」和「收起的原因」绑在一起的那根线：
不找到关闭开关就把 `kimi.responses.v1` 放出来，会在这个守卫上失败，而不是走到运营者
面前。反向验证确认了这一点（第一次没确认改动是否真的落到文件上，测试因此假绿——正是
CLAUDE.md 警告的那种失效搜索串）。

### 15.2 网关路由表

`TestEveryGatewayRouteIsADeclaredNorthboundMethod` 与
`TestEveryGatewayRouteSitsInsideItsGuardedGroup`。admin 路由早就有这个守卫，网关路由
没有。第二个测试用中间件计数守住 step 6 那句警告：注册在裸 router 上的北向路由只带 2 层
中间件，注册在守卫组内的带 5 层。

写它需要 `compatibility.AllNorthboundProfiles` —— 那张表此前是
`BuiltinNorthboundProfile` 函数体里的私有 map，也就是一份没人能走的清单，正是 provider
profile 表当初被修掉的同一个形状。

### 15.3 native 校验器

`cache_control` 原本是对原始字节做 `bytes.Contains`。`"cache_control"` 能绕过它
——Go 的解码器和 Kimi 的解码器都把它读成同一个成员，而字节扫描两个都看不见，于是标记
会随一个 Halro 声明为「干净」的请求发到上游。这与 `rejectDuplicateMembers` 存在的理由
是同一条：Halro 检查的文档和 provider 收到的文档不能不同。改为遍历解码后的文档找
**键**；值里出现同名字符串不算成员。MiniMax 的校验器共用同一个实现。

Kimi 的 native 校验器此前零测试（MiniMax 有 4 个），现在补了，包括一条专门守住
「registry 的 switch 里 case 写错、Kimi 拿到别人的校验器」——三个 profile 的区别在于
各自**拒绝什么**，所以断言的就是那个。顺带把重复解码去掉：provider 校验器不再把 payload
解第二遍。

### 15.4 `ProviderCode` 收敛

`internal/provider/anthropic/adapter.go` 是五个适配器里唯一把上游选定的标识符原样带进
`provider.Error.ProviderCode` 的，而 `logProviderFailure` 会把它写成结构化日志属性。它
恰好又是服务 Kimi 那条「400 用 Anthropic 信封、401/429 用 OpenAI 信封」路由的适配器。
两处都改了：适配器源头（与另外四个一致），以及日志写入点（不变量真正绑定的地方，一次
覆盖所有适配器，包括以后新增的）。收敛发生在判定 refusal **之前**——一个 type 是长篇
散文的 400 不是上游在指认拒绝原因。

`kimi_error_tolerance_test.go` 原来的泄漏断言是空转的：`provider.Error.Error()` 只返回
`Message`，看不到 `ProviderCode`，而两条带 `leaked` 的用例解出的 `ProviderCode` 恰好都
是空。现在按字段名断言。

## 16. 「总是推理」进路由（2026-09-02）

§13.6 提出、§14/§15 反复提到的那个能力位接上了。`kimi-k2.7-code` 与
`kimi-k2.7-code-highspeed` 在 `/v1/responses` 与 `/v1/messages` 上不再是「上游计费 +
502」，而是在**预留之前**被路由绕开。

### 16.1 它没有走 durable 状态，这是最省的一处

上一轮估计要把这条事实穿过 deployment 的能力快照——那是 bbolt 里的持久状态，要动格式
版本、要说明需不需要重新初始化。实际不用：注册表构建时**目录已经在手边**
（`prepareProviderRegistryActivation` 本来就收 `*modelcatalog.Catalog`），而
`Catalog.Lookup` 自己会补默认 TargetKind 并做 region 回退。所以 `provider.Target` 上加
一个字段、构建时查一次目录就够了，**没有任何 durable schema 变化，不需要重新初始化**。

而且这样更对：快照存在的意义是钉住「运营者当初同意了什么」，而这条是「上游会怎么做」。
一条只会**减少**路由、从不放宽路由的事实，不该等到每个 deployment 被重新保存一遍才生效
——升级 Halro 学到的新事实应当立刻起作用。

目录未覆盖的模型答 false。这是唯一可行的方向：把未知的一律绕开会拒掉每一个
`operator_declared` 部署。

### 16.2 两处丢失，不是一处

`compatibility.ReasoningAnswerSurvives` 把两个半边配起来，而它们不可互换：

- **上游 profile 自己的解码器拒绝**（Responses 形状的五个 profile）——根本到不了北向
  渲染器，所以**每个端点都完**；
- **北向端点的渲染器承载不了**（`/v1/responses` 与 `/v1/messages`）——只有那个端点完，
  同一个目标在别处照常服务。

这个区分是 §15 测 MiniMax 时才浮出来的：守卫第一版只建模了后者，会说
`/v1/chat/completions` 在 `minimax.responses.v1` 上没事——那个端点确实渲染得了
reasoning，但根本轮不到它。

两张表都**只列丢失的**，没列的读作「能承载」——那是猜错方向的一侧，所以不留给猜：
`TestReasoningReachabilityTablesMatchTheRealDecodersAndRenderers` 驱动真实的解码器和真实
的渲染器，要求两张表与它们的实际行为一致。

### 16.3 residue 清单没了

那份「已知还坏着」的清单是上一轮的产物：当时只能记录、不能修。现在那些组合在预留之前
就被绕开，清单随之消失。守卫的问题也从「还有哪些坏的」变成「每个被标记的目标 × 每个端点，
声明是否与真实行为一致」。

`kimi.responses.v1` 仍然收起。它那一面的解码器对所有模型都拒绝，就算放出来也会每个请求
都被绕开——一个永远答不出东西的连接不该让运营者建得出来。
`TestAWithheldProfileThatReasonsUnaskedStaysUnservable` 钉住这一点。

### 16.4 三段链路，三个测试

反向验证时发现中间那一段没人守：把注册表读目录那一步断掉，两端的测试全绿。
`TestTheRegistryReadsReasonsUnaskedFromTheCatalogue` 补的就是它——这正是本轮反复遇到的
同一种形状，只不过这次是在我自己刚写的代码上。
