# OpenAI 平台摸底

凭据形态：Bearer 静态密钥（`domain.CredentialBearerStatic`），端点 `https://api.openai.com`。

## 一、Provider Profile

OpenAI 被拆成两个互不相干的 Profile（`internal/provider/profile.go:358-363`、`:411-416`）：

| Profile | Operations | 南向 Primitive |
|---|---|---|
| `openai.chat-embeddings.v1` | chat、chat_stream、embeddings | `openai.chat-completions`(+stream)、`openai.embeddings` |
| `openai.media-resources.v1` | moderations、images、transcriptions、speech、files、batches | 六个对应的 `openai.*` primitive |

拆分是对的：媒体与资源类操作的失败模式、计费方式、状态语义都和对话无关，把它们绑在同一个 Profile 上会让一个凭据的能力上限失去意义。代价是操作者要建两个 Provider 才能同时用对话和图片。

## 二、逐模态

### 文本对话【肯定】

`/v1/chat/completions`、`/v1/responses`、`/v1/messages`（portable）三个北向端点都能落到这个 Profile。`provider_fields.go:92-94` 明确把它归入"直接使用 OpenAI 线协议表示"的一档——**不拒绝任何可选字段**，是九个 Profile 里能力面最完整的。

模型目录可枚举（`internal/provider/openai/adapter.go:128-141`，`CanEnumerate/CanDescribe/CanVerify` 全开），目标种类 `model_id`。

### `/v1/responses` 的实际边界【肯定】

manifest 记录得很清楚，值得原样列出：

- 只有 POST create；**没有**检索、删除、取消、`input_items`、Conversations、后台模式、webhooks
- `store` 默认 false，`store=true` 直接拒绝
- hosted tools、strict function tools、reasoning output、流式 function call 全部拒绝
- 请求里的 instructions、工具定义、tool_choice、结构化 schema 在响应里返回为保守的 null/空/默认值——因为原始 Responses 对象没有走出站脱敏路径

最后一条是安全取舍：宁可回声不完整，也不把未脱敏的指令原样吐回去。这个取舍应该让接入方知道，否则他们会以为是 bug。

### 嵌入【肯定】

无字段限制（`provider_fields.go:154`）。

### 图片生成【问题：能力面窄】

`provider.ImageCall` 只携带 prompt、n、quality、size、response_format、style（`internal/provider/inference_resources.go:18-21`）。

缺的：
- **`user` 字段**被实验层显式拒绝（manifest `openai.images.generations.v1` 的偏差条目）
- **图片编辑与变体**（`/v1/images/edits`、`/v1/images/variations`）没有北向端点
- 没有 `background`、`output_format`、`moderation` 等 gpt-image 系列的新参数

### 语音【问题：缺一个端点】

有 `/v1/audio/speech`（合成）与 `/v1/audio/transcriptions`（转写），**没有 `/v1/audio/translations`**（转写并翻译为英文）。OpenAI 有这个端点，Halro 没适配。

`SpeechCall` 携带 voice、input、response_format、speed（`inference_resources.go:42-45`）；`TranscriptionCall` 携带 filename、content_type、language、prompt、response_format、temperature、data（`:32-36`）。参数面够用，缺的是端点本身。

另外，转写是 multipart 上传，`/v1/audio/transcriptions` 的请求体大小受 `server.max_request_bytes`（默认 10 MiB）约束——OpenAI 自身允许 25 MB 音频。**【未验证】** 超过 10 MiB 的音频在 Halro 上会得到 413，这个默认值对语音场景偏小，但是否构成实际问题取决于使用方式。

### 文件与批处理【问题：缺 list】

- 文件：create / get / content / delete —— **没有 list**
- 批处理：create / get / cancel —— **没有 list**

资源标识符是 Halro 自己的、按 Project 隔离的不透明 ID（manifest 偏差条目），不是 OpenAI 的原始 ID。这是正确的隔离，但也意味着调用方无法用 OpenAI 控制台里看到的 ID 直接寻址。没有 list 端点时，调用方必须自己记住创建时拿到的 ID，否则资源就找不回来了——对批处理这种长时间异步作业来说，这是个实际的可用性缺口。

### 内容审核

`/v1/moderations`，仅 OpenAI 可服务。参数面未细查。

## 三、验证证据

### 【问题】GA 门禁的 OpenAI 冒烟对现代模型是坏的 —— 已修复并验证

2026-08-13 第一次用真实凭据跑 `internal/provider/openai/real_smoke_test.go`，非流式对话即失败：

```
non-stream chat failed: bad_request http=400 code=unsupported_parameter:max_tokens
```

冒烟对所有 profile 一律发 `max_tokens`，而 OpenAI 当前的模型只收 `max_completion_tokens`。也就是说**这个 GA 发布门禁的 cell 对任何现代模型都不可能通过**。旁边的能力探测路径早已用的是 `max_completion_tokens`（`internal/provider/capability_detection.go:113`），只有这个文件停在旧参数上——它已经很久没有被真实账号跑过。

修复：按 profile 选参数——`openai`/`azure_openai` 发 `max_completion_tokens`，`deepseek`/`openai_compatible` 保持 `max_tokens`（两者同时发会被所有上游拒绝）。修复后 2026-08-13 实跑通过（`gpt-5.4` + `text-embedding-3-small`，7.17s，覆盖非流式对话、语义 SSE、嵌入）。

**这不是产品缺陷**：调用方把 `max_tokens` 发给一个只收 `max_completion_tokens` 的模型，Halro 原样转发、上游拒绝——直连 OpenAI 是同样的 400。不静默改写调用方的请求是有意的取舍。

### 【问题】上游错误码此前被整个丢弃 —— 已修复

诊断上面那条时发现的：`limitedErrorMessage` 只取 `error.message`，把 `code`、`type`、`param` 全丢了，`classifyHTTPError` 也从不设 `ProviderCode`（`internal/provider/openai/adapter.go`）。

影响面不止冒烟：**OpenAI、Azure、DeepSeek、OpenAI 兼容端点的连接测试与网关失败日志，`provider_code` 永远是空的**。Anthropic 适配器一直填这个字段，所以只有 OpenAI 系这条链是瞎的——摸底初稿没发现它，因为它只在真实上游返回结构化错误时才显形。

修复：提取 `code`（空则回退 `type`），并把 `param` 以 `:` 相接（两段都是标识符，符合 `provider_code` 既有的字符形状）。上游的 message 仍只留在 error 内部，不进日志、不进控制台。

### 其余证据

**最完整的一个平台。**

- GA 真实账号矩阵包含 `openai`，要求 `BASE_URL`/`API_KEY`/`MODEL`/`EMBEDDING_MODEL`，覆盖非流式对话、语义 SSE、嵌入、以及受限的能力探测（`tests/provider-matrix/main.go:53`）。**2026-08-13 实跑通过**
- 适配器级真实冒烟：`internal/provider/openai/real_smoke_test.go`
- SDK 黑盒兼容套件覆盖 Go/Node/Python 三种官方 SDK（`tests/compatibility/`）

但要注意：矩阵覆盖的是 `openai.chat-embeddings.v1`。**`openai.media-resources.v1` 的六个操作不在任何真实账号回归里**，manifest 也承认"official SDK black-box matrix is not yet validated"。图片、语音、文件、批处理目前只有传输层 fixture 证据。

## 四、小结

对话与嵌入这条主链路是三个平台里最扎实的：能力面完整、无字段拒绝、有 GA 门禁、有 SDK 黑盒验证。

媒体与资源这条链路则相反：端点齐了六类，但缺 list、缺 translations、缺图片编辑，且完全没有真实账号证据。如果这条链路要往前推，第一步应该是给 `openai.media-resources.v1` 补一个真实冒烟，而不是继续加端点。
