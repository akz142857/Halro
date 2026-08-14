# Anthropic 平台摸底

凭据形态：`x-api-key`（`domain.CredentialAnthropicAPIKey`），端点 `https://api.anthropic.com`。

Anthropic 是三个平台里适配最深、机制最多的一个——也是**验证证据最少**的一个。

## 一、Provider Profile

只有一个：`anthropic.messages.2023-06-01`（`internal/provider/profile.go:364-369`）。

Operations：chat、chat_stream、messages、messages_stream。前两个是 portable 路径（北向 OpenAI 形状），后两个是原生路径（北向 Anthropic 形状）。同一个 Profile 承载两种执行模式，这是它与其他 Profile 的根本区别。

另有 `bedrock.mantle.anthropic.messages.v1` 共用同一份适配器代码，但 Profile、凭据、能力上限都是独立的，见 [bedrock.zh-CN.md](bedrock.zh-CN.md)。

## 二、原生 vs portable 双模【肯定】

这是本平台适配的核心设计，也是最值得肯定的部分。

- **模式选择**：`Halro-Route-Mode` 头（`internal/gatewayapi/handler.go:397`）。原生模式要求直连 Anthropic 或 Mantle Anthropic Profile。
- **原生模式**：请求体逐字转发，只替换上游模型标识符；工具与内容块原样传递，因此 `cache_control`、按工具的配置都能原封不动到达上游。
- **portable 模式**：请求经由规范模型重新编写，因此它会**拒绝**自己无法承载的成员，而不是静默丢弃。

`anthropic-beta` 只在原生模式下转发，且只转发该连接被配置为接受的令牌（`AllowedAnthropicBetas`，`internal/provider/provider.go:246-250`）。portable 模式直接拒绝 beta 令牌——理由很硬：beta 令牌描述的是"请求按什么方式书写"，而 portable 路径会重写请求，令牌因此失去意义。

工具按**执行位点**分类：客户端执行的 Anthropic 内建工具在任何日期后缀下都接受；供应商执行的工具要求连接显式声明 `provider_executed_tools`，因为上游会发起 SafeTransport 主机允许列表之外的网络调用。族匹配锚定到 `<family>_<YYYYMMDD>`，所以 `bash_code_execution_*` 这种"以已知族名开头的更长名字"按自己的规则分类，不会被当成它像的那个客户端工具。

`mcp_servers`、`container`、`fallbacks` 被拒绝，且 manifest 明确说这是**有意的边界**而非未建模字段：前两个把出网或代码执行委托给上游，第三个把模型选择移出 Halro 的路由与成本归属。

## 三、portable 模式的能力面

`internal/compatibility/provider_fields.go:59-79` 声明的不支持字段：

| 字段 | 原因 |
|---|---|
| `messages[].name` | Anthropic 无对应 |
| `messages[].role=developer` | 同上 |
| `n>1` | Anthropic 单候选 |
| `seed` | 无对应 |
| `response_format`（json_object） | Anthropic 只有 schema 模式，没有无 schema 的 JSON 模式 |
| `response_format`（非 strict 的 json_schema） | Anthropic 给了 schema 就会强制执行，没有宽松模式 |
| `reasoning_effort=max` | **受限于 portable 表示而非 Anthropic**：portable 请求都经过 OpenAI 线形式，其 `reasoning_effort` 止于 `xhigh` |
| `user` | 无对应 |

最后一条的注释值得注意：曾经把 Anthropic 自己的 `max` 声明为可路由，结果请求通过了能力过滤、在渲染阶段失败——**在预算预留之后**，报的还是调用方没发过的字段名。现在的取交集写法（`provider_fields.go:18`）是那次的修复。

## 四、模态覆盖

| 能力 | 状态 |
|---|---|
| 文本对话（流式/非流式） | ✅ 双模 |
| 视觉输入 | ✅ portable 支持 URL 源图片块 |
| 工具调用 | ✅ 原生完整；portable 支持客户端工具 |
| 结构化输出 | ⚠️ 仅 strict json_schema |
| 扩展思考 | ⚠️ 见下 |
| Token 计数 | ✅ `/v1/messages/count_tokens`，**仅**直连 Profile |
| 嵌入 / 图片 / 语音 | — 平台本身没有 |
| **Message Batches** | ❌ **平台有，Halro 未适配** |
| **Files API** | ❌ **平台有，Halro 未适配** |
| Models API | ⚠️ 仅内部使用（目录枚举与连接测试），无北向端点 |
| Skills / Agents / Sessions / Admin API | ❌ 未适配 |

### 【问题】Batches 与 Files 的不对称

Halro 的 `/v1/batches` 与 `/v1/files` 只能落到 `openai.media-resources.v1`。Anthropic 有对应的 Message Batches API（50% 成本折扣）与 Files API，但没有被适配。

对一个多平台网关来说，这意味着"批处理"这个能力在换平台时会消失，而调用方无法从 Halro 的端点形状上看出这一点——他们只会得到路由失败。

### 扩展思考（thinking）

当前这代 Claude 模型默认开启 adaptive thinking。portable 路径无处安放带签名的 thinking 块（签名必须在下一轮原样交回，OpenAI 形状的响应没有位置存它），因此 portable 请求现在显式发送 `thinking: {"type":"disabled"}`（本次会话修复，见 commit `0e0a4ef`）。

**要用扩展思考，必须走原生 `/v1/messages`。** 这是设计边界，不是缺陷，但需要在面向接入方的文档里写清楚——否则 portable 用户会得到一个"能力凭空消失"的观感。

调用方显式请求 `reasoning_effort` 的 portable 请求不做覆盖：安静地给一个更浅的答案是更坏的失败，因为它不可见。这类请求在 adaptive 模型上仍然会失败，正确的做法是改走原生端点。

## 五、验证证据【摸底时是最大的空洞，已补】

| 证据类型 | 摸底时 | 现在 |
|---|---|---|
| GA 真实账号发布门禁 | ❌ 不在 `gaProfiles` | ✅ 已加入（`tests/provider-matrix/main.go:54-57`） |
| 适配器级真实冒烟 | ❌ 无 `real_smoke_test.go` | ✅ `internal/provider/anthropic/real_smoke_test.go` |
| SDK 黑盒兼容套件 | ⚠️ `tests/compatibility/server` 有 Messages 的假服务端，但打的是假上游 | 未变 |
| 单元/契约测试 | ✅ 充分 | 未变 |

摸底时，三个平台里**只有 Anthropic 同时缺席 GA 门禁、Beta 矩阵和适配器冒烟**——OpenAI 三者俱全，Bedrock 与 Mantle 各有冒烟、Mantle 还在 Beta 矩阵内。下面这段是当时的判断依据，保留它是因为它解释了为什么这条排在第一位。

**已用真实账号验证**（2026-08-13，模型 `claude-opus-5`，直连 `https://api.anthropic.com`）：七项在同一次运行内全部通过，17.165s。

过程中冒烟自己被修过一次：首跑 6/7，`count_tokens` 失败，因为冒烟的 payload 复用了 Messages 的 body 而带上了 `max_tokens`——Anthropic 的 count_tokens 没有这个参数，答以 `max_tokens: Extra inputs are not permitted`。这是冒烟自身的缺陷，不是 Halro 的；修正后整跑通过。

这次真实运行证实的，恰好是今天修的两个缺陷在上游成立、而不只是在 fixture 里成立：

- `portable_chat` 通过，说明 adaptive thinking 确实被禁用了——不禁用的话这个模型必然返回带签名的 thinking 块，portable 路径必然 502
- 同一项里 `finish_reason` 落在 OpenAI 枚举内，说明 termination 词汇表修对了——修之前这里会是 `end_turn`
- `model_catalog` 有条目同时报出两个 token 上限，说明 `max_tokens` 字段名改对了——读错时这里恒为 0
- `probe_without_a_deployment_model` 通过，说明空模型走 `/v1/models` 的回退在真实账号上成立

而 `count_tokens` 那次失败本身也是一个结论：Halro 只改写上游模型标识符、其余成员原样转发，所以调用方发了这个端点不接受的成员，拿到的是上游自己的拒绝，而不是一个被悄悄改写过的请求。这条取舍此前只有代码注释，现在有了真实上游的验证。

而 Anthropic 承担的适配复杂度是最高的：双执行模式、beta 令牌按连接过滤、工具按执行位点分类、独占的 count_tokens 端点、以及 Models API 的两种内部用途。复杂度最高 × 真实证据最少，是本次摸底里最需要处理的组合。

这个判断有直接的经验支撑：仅在 2026-08-13 这一天的排查中，就在 Anthropic 链路上连续发现四个缺陷，全部是单元测试无法发现、只有真实请求才会暴露的类型：

1. 连接测试对空模型构造 Messages 请求，被本地校验拒绝，从未发出（commit `d724e1f`）
2. Models API 的输出上限字段名读错（`max_output_tokens` vs 实际的 `max_tokens`），能力键有一半在真实响应里不存在（同上）
3. adaptive thinking 默认开启导致 portable 路径必然失败（commit `0e0a4ef`）
4. termination 词汇表用错，`finish_reason` 漏出 `end_turn`（同上）

第 2 项尤其说明问题：它的测试 fixture 是照着实现者的假设写的，因此测试和代码一起错，全绿。这正是真实账号回归存在的理由。

## 六、小结

设计层面，Anthropic 的适配是三个平台里最讲究的：双模执行、按执行位点分类工具、按连接过滤 beta 令牌、明确拒绝会绕开 SafeTransport 的能力。这些边界都不是随手划的。

问题不在设计，在证据。**已修复**：`internal/provider/anthropic/real_smoke_test.go` 覆盖原生非流式、原生流式、portable 非流式、portable 流式、count_tokens、模型目录枚举、以及空模型的目录探测；`tests/provider-matrix/main.go` 的 `gaProfiles` 加入 `anthropic`（前缀 `HALRO_MATRIX_ANTHROPIC_`，需要 `BASE_URL`/`API_KEY`/`MODEL`）；`docs/verification/provider-real-matrix.md` 同步更新。

其中两条断言是照着今天踩过的坑写的：portable 路径的 `finish_reason` 必须落在 OpenAI 枚举内，模型目录必须至少有一条同时报出两个 token 上限——后者正是读错字段名时会退化成的形状。

功能层面，Message Batches 与 Files API 是两个明确的、平台已有而 Halro 未适配的缺口，已记入 [`docs/prd/provider-adaptation-gaps.zh-CN.md`](../../prd/provider-adaptation-gaps.zh-CN.md) §3，动手前需要先定北向端点的形状——两家的批处理输入语义不同（OpenAI 是已上传文件的 ID，Anthropic 是内联请求数组）。
