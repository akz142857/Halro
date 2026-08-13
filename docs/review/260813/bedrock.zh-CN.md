# AWS Bedrock 摸底

README 将 Bedrock 与 Bedrock Mantle 都标注为 **Beta**（`README.md:177-178`）。两者是**两条完全独立的接入路径**，凭据形态、端点、Profile、能力上限都不同，本文分开讨论。

截图中的两份凭据都是 Mantle 形态（`https://bedrock-mantle.us-east-{1,2}.api.aws`，`AWS Bedrock（测试版）`），走的是 API Key 而非 SigV4。

## 一、路径 A：Bedrock Runtime / Agent Runtime（SigV4）

凭据：`domain.CredentialAWSSigV4Explicit`。五个 Profile，每个绑定一种 Access Surface（`internal/provider/profile.go:397-410`、`:417-419`）：

| Profile | Operations | 模型 | 说明 |
|---|---|---|---|
| `bedrock.runtime.converse.text.v1` | chat、chat_stream | 可枚举 | 唯一的通用对话 Profile |
| `bedrock.runtime.invoke.titan-embed-text-v2.v1` | embeddings | **写死** `amazon.titan-embed-text-v2:0` | |
| `bedrock.runtime.invoke.titan-image-v2.v1` | images | **写死** `amazon.titan-image-generator-v2:0` | |
| `bedrock.agent-runtime.rerank.cohere-v3-5.v1` | rerank | **写死** `cohere.rerank-v3-5:0` | Agent Runtime 是另一个 Surface |
| `bedrock.runtime.async.nova-reel-v1.v1` | async_invoke | **写死** `amazon.nova-reel-v1:0` | 异步视频生成 |

### 【问题】Converse Profile 拒绝工具调用

能力上限硬编码在适配器里（`internal/provider/bedrock/adapter.go:296`）：

```go
return provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true}
```

没有 `Tools`、没有 `Vision`、没有 `JSONMode`、没有 `Reasoning`。能力过滤层与之一致（`internal/compatibility/provider_fields.go:44-53`），会拒绝这些字段：

`messages[].name`、`n>1`、`seed`、`tools`、`tool_choice`、`parallel_tool_calls`、`response_format`、`reasoning_effort`、`user`

也就是说：**Halro 通过 Converse 只能做纯文本对话**。而 Bedrock Converse API 本身是支持 `toolConfig` 与图片输入块的——这是 Halro 侧的适配缺口，不是上游限制。

后果不只是"少个功能"：路由层会因为能力不匹配而**绕开** Bedrock，一个同时配了 OpenAI 与 Bedrock 的 Project，只要请求里带了 `tools`，就永远不会落到 Bedrock。这个行为是对的（不静默丢弃），但它让 Bedrock 在带工具的工作负载里等于不存在。

**定位文档已经存在**——这是摸底初稿的一处漏读。[`docs/guides/aws-surface-selection.md`](../../guides/aws-surface-selection.md) 的「能力面的差别」一节写明 Converse 被"刻意钉死在纯文本对话"，并列出八个 Profile 的能力对照表；「都做不到的事」一节还记录了 Bedrock 侧没有批量推理与护栏。缺的不是说明，是功能本身。

已记入 [`docs/todo/provider-adaptation-gaps.zh-CN.md`](../../todo/provider-adaptation-gaps.zh-CN.md) §1。现实阻塞：Converse 走 Runtime（SigV4），而当前手头两份 AWS 凭据都是 Mantle（API Key），改了也无法用真实账号验证。

### 【肯定】四个固定模型 Profile 的取舍

`ValidateProfileModel` 强制模型 ID 与 pin 一致（`internal/provider/bedrock/invoke_titan_embedding.go:65-77`），枚举也直接从 pin 返回单条而不去问上游。注释解释了原因（`models.go:42-51`）：一个只接受一个模型的 Profile 去列一百个基础模型，等于给操作者九十九个建不出来的选项。

代价是新模型必须改代码。对 Beta 阶段可接受，但要记在账上：Bedrock 每次上新模型，Halro 就需要一次发版。

### 【肯定】控制面枚举的边界处理

Converse 的模型清单在 Bedrock 控制面，与运行时端点不是同一个 host。适配器要求它同样通过 Provider 的 allowed-hosts 策略；不通过时枚举失败、控制台回退到手工输入模型，而不是静默调用一个操作者没批准的主机（`internal/provider/bedrock/models.go:52-57`）。

### 嵌入的字段限制

Titan Embed V2 只接受字符串输入（不接受 token 数组）、`encoding_format` 只接受 float、`dimensions` 只接受 256/512/1024（`internal/compatibility/provider_fields.go:163-178`）。这三条与 Titan 的实际能力一致。

## 二、路径 B：Bedrock Mantle（API Key）

凭据：`domain.CredentialBedrockAPIKey`，Access Surface `bedrock-mantle`。三个 Profile，区别在于**南向使用哪种线协议**（`internal/provider/profile.go:420-437`）：

| Profile | 南向线协议 | Operations |
|---|---|---|
| `bedrock.mantle.openai.chat.v1` | OpenAI Chat Completions | chat、chat_stream |
| `bedrock.mantle.openai.responses.v1` | OpenAI Responses（无状态） | chat、chat_stream |
| `bedrock.mantle.anthropic.messages.v1` | Anthropic Messages | chat、chat_stream、messages、messages_stream |

三者共享端点校验（`bedrockmantleprovider.ValidateEndpoint`）与凭据形态，但适配器实现不同：OpenAI Chat 复用 OpenAI 适配器，Responses 用独立的 `NewResponses`，Anthropic Messages 复用 Anthropic 适配器并指定 `MessagesPath: "anthropic/v1/messages"`（`internal/app/providers.go:666-683`）。

### 能力上限的三种形态【问题：不对称】

- **OpenAI Chat**：归入"直接使用 OpenAI 线协议"一档，不拒绝任何可选字段（`provider_fields.go:92`）
- **OpenAI Responses**：拒绝 `n>1`、`stop`、`seed`、**流式下的 `tools`**、`reasoning_effort`（`:87-91`）
- **Anthropic Messages**：拒绝 `messages[].name`、developer 角色、`n>1`、`seed`、`response_format`、`reasoning_effort`（`:81-86`）

第三条带有一条明确的注释：这个 Profile 的线协议**能**承载 `output_config`，但 Beta 的能力上限由构建固定，放宽它是一次独立的契约评审。也就是说这是有意的保守，不是遗漏。

代价是同一个 Mantle 凭据，选不同的 wire profile 会得到差异很大的能力面，而这个差异对操作者不可见——控制台上它们都叫"AWS Bedrock（测试版）"。**建议**在 Deployment 选择 wire profile 时把能力差异显示出来。

### 【肯定】一次验证只证明一个 cell

矩阵注释（`tests/provider-matrix/main.go:41-43`）：一次 Mantle 运行只证明 commit × region × wire profile × 认证方式 × project 模式这一个组合；三种 wire profile 是三个 cell，不是"Mantle 通过了"。

截图里两份 Mantle 凭据分属 us-east-1 与 us-east-2，其中 `aws-wahool-mantle-365` 显示"被 0 个服务商使用"——它还没有绑定到任何 Provider，因此还没有被验证过。

## 三、验证证据

| 路径 | GA 门禁 | Beta 矩阵 | 适配器冒烟 |
|---|---|---|---|
| Runtime / Agent Runtime | 不参与（Beta） | ❌ 不在 `betaProfiles` | ✅ `internal/provider/bedrock/real_smoke_test.go` |
| Mantle | 不参与（Beta） | ✅ `tests/provider-matrix/main.go:45-49` | ✅ `internal/provider/bedrockmantle/real_smoke_test.go` |

Beta 不进也不阻塞 GA 发布门禁（`docs/verification/provider-real-matrix.md:45-47`），这是明确的设计。

## 四、小结

Mantle 这条路更完整：三种 wire profile、Beta 矩阵覆盖、适配器冒烟俱全，代价是三者能力面不对称且对操作者不可见。

Runtime 这条路则是"一个残缺的通用 Profile + 四个单模型 Profile"。Converse 缺工具调用是最实质的缺口；四个固定模型 Profile 各自能用，但每个都是单点，且新模型需要发版。

两条路都不在 GA 门禁内，这与 Beta 定位一致。
