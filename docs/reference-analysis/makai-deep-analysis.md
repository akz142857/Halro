# Makai 深度分析报告

> 分析对象：`github-project/makai`
> 上游仓库：`https://github.com/lsm/makai.git`
> 分析基线：`f27e940db21c9f604f3b966a8d9e3e8f131baa55`
> 分析日期：2026-07-31
> 分析目的：评估 Makai 对 Halro 轻量、自托管 LLM Gateway V1 的参考价值。

## 1. 结论摘要

Makai 是 Zig-first 的流式 AI Runtime，并配有 TypeScript SDK。它的核心不是 HTTP Gateway，而是多 Provider 流式抽象、Provider/Auth/Agent/Tool 分布式协议、stdio/WebSocket 等传输，以及本地 Agent/TUI。

它对 Halro 的最大价值集中在 Gateway Core 的下半部分：

1. Provider Adapter 统一接口；
2. Provider 能力差异建模；
3. SSE/流式事件标准化；
4. 请求/响应结构转换；
5. Retry-After、指数退避和取消；
6. 模型引用和模型发现；
7. 凭证解析、OAuth 和安全落盘；
8. 单二进制构建与跨平台发布。

但 Makai 不能直接作为 Halro 基座，原因是：

1. 它没有 OpenAI-compatible HTTP Server；
2. 没有 Project、API Key、预算、RPM/TPM/并发配额；
3. 没有 Router Alias、Fallback Chain 或 Circuit Breaker；
4. 没有持久化 Usage 日志、Dashboard、Admin API 或 Prometheus；
5. Bedrock Adapter 仍明确返回 `error.NotImplemented`；
6. Provider Retry 代码存在重复，若直接移植会放大维护成本；
7. 项目版本仍为 `0.0.1`，部分真实 Provider E2E 在 CI 中被禁用或允许失败。

综合判断：Makai 是优秀的 Provider/Streaming 实现样本，但不是成品 Gateway。Halro 应借鉴其边界和协议语义，用 Go 重新实现精简版本，不建议把 Zig Runtime 作为子进程嵌入。

## 2. 项目画像

| 维度 | Makai |
|---|---|
| 产品定位 | 流式 AI Runtime + Agent Runtime + TypeScript SDK |
| 核心语言 | Zig 0.16 |
| SDK | TypeScript/CommonJS |
| 入口 | `makai --stdio`、`makai --tui`、Auth CLI |
| 传输 | in-process、stdio、SSE、WebSocket |
| Provider | Anthropic、OpenAI Completions/Responses、Azure、Google、Ollama 等 |
| Agent | Agent Loop、Tool Protocol、本地工具和 MCP Bridge |
| 存储 | 本地 Auth JSON / macOS Keychain、TUI Session Store |
| 版本 | 0.0.1 |
| 规模 | 约 318 个非 `.git` 文件，工作区约 15 MB |
| 测试资产 | 约 47 个测试相关文件、约 205 个 TS/Zig test/it/describe 调用 |

仓库以 239 个 Zig 文件为主体，TypeScript SDK 很薄。Zig 构建除 vendored `zigzag` 外不依赖外部 Zig 包，符合轻量二进制方向。

## 3. 架构拆解

`DESIGN.md` 将系统分为四层：

```text
Streaming Core
  └─ Provider Layer
      └─ Protocol Layer
          └─ Agent Layer
```

更具体地说：

```text
TypeScript SDK / TUI / CLI
  │
  ▼
Transport
  ├─ stdio
  ├─ websocket
  └─ in-process
  │
  ▼
Protocol Runtime
  ├─ Auth Protocol
  ├─ Provider Protocol
  ├─ Agent Protocol
  └─ Tool Protocol
  │
  ▼
Provider Registry
  │
  ├─ Anthropic
  ├─ OpenAI Completions
  ├─ OpenAI Responses
  ├─ Azure OpenAI
  ├─ Google
  └─ Ollama
```

Makai 强调：

- Session/Stream 级序列号；
- 多会话 Multiplexing；
- 消息 Envelope；
- 显式内存所有权；
- Transport 与 Protocol 解耦；
- Provider 与 Agent 解耦；
- Auth 生命周期单独建模。

这种边界清晰度值得 Halro 学习，但 Halro 的外部协议已经确定为 HTTP/OpenAI-compatible，不需要复制 Makai 的四套分布式协议。

## 4. Provider 抽象

### 4.1 Registry

`ApiRegistry` 的核心接口是：

```text
api name
stream(model, context, options, allocator)
stream_simple(...)
optional auth provider / refresh / api-key resolver
optional auth-failure detector
```

内置 API 包括：

- `anthropic-messages`；
- `openai-completions`；
- `openai-responses`；
- `azure-openai-responses`；
- `openai-codex-responses`；
- `google-generative-ai`；
- `google-gemini-cli`；
- `ollama`。

优点：

- Provider 注册和调用方解耦；
- API 类型与 Provider 实例分开；
- Auth 刷新可由 Provider 自己定义；
- 可按 `source_id` 注销扩展 Provider。

风险：

- 相同 `api` 注册会静默覆盖旧实现；
- Registry 按 API 类型索引，不直接表达 Halro 的 Provider Instance；
- 健康状态、权重、超时和熔断状态不在 Registry；
- 没有显式能力验证。

Halro 建议使用两层模型：

```text
Adapter Type:
  openai / azure_openai / gemini / bedrock

Provider Instance:
  id / type / endpoint / encrypted secret / region /
  timeout / enabled / health / concurrency
```

Adapter 是代码级单例，Provider Instance 是配置和运行状态，不能混为一个 Registry。

### 4.2 Provider 能力矩阵

`ProviderCapabilities` 显式描述：

- streaming；
- extended thinking；
- prompt caching；
- vision；
- tool calling；
- reasoning effort；
- developer role；
- max token 字段；
- thinking 格式；
- Mistral tool ID；
- tool result 结构兼容性。

并通过 URL 判断 OpenAI Native、Anthropic、Mistral、Groq、Cerebras、OpenRouter、Qwen、DeepSeek、Google、Bedrock、Azure、Ollama。

这是 Makai 最值得 Halro 借鉴的设计之一。OpenAI-compatible 并不意味着语义完全兼容；同一请求需要按 Provider/Model 能力进行预转换或拒绝。

Halro 可精简为：

```go
type Capabilities struct {
    Chat             bool
    Embeddings       bool
    Streaming        bool
    Tools            bool
    Vision           bool
    Reasoning        bool
    UsageInStream    bool
    MaxTokenField    string
}
```

V1 不需要覆盖 Agent 特有的 thinking/tool 细节，但必须避免仅凭 endpoint 声称完全兼容。

### 4.3 请求转换

各 Adapter 分别处理：

- System/Developer/User/Assistant/Tool 消息映射；
- Tools JSON Schema；
- Thinking/Reasoning；
- Prompt Cache；
- Provider Header；
- `max_tokens` 字段差异；
- SSE Event；
- Usage；
- Stop Reason；
- Provider 特殊 ID。

Makai 将这些差异留在 Provider 文件中是正确的。Halro 应保持统一内部 DTO：

```text
OpenAI Request
  -> Canonical Request
  -> Route Resolution
  -> Provider Request

Provider Stream
  -> Canonical Events
  -> OpenAI SSE
```

避免 OpenAI Handler 直接拼装各 Provider HTTP 请求。

## 5. Streaming Core

### 5.1 统一事件模型

`AssistantMessageEvent` 包括：

- start；
- text_start/delta/end；
- thinking_start/delta/end；
- toolcall_start/delta/end；
- done；
- error；
- keepalive。

统一事件模型使调用方无需理解不同 Provider 的 SSE 格式。

对 Halro，V1 可以更小：

```text
ResponseStart
TextDelta
ToolCallDelta
Usage
ResponseEnd
Error
```

然后由 OpenAI Encoder 生成 `chat.completion.chunk` 和 `[DONE]`。

### 5.2 EventStream

Makai 的 `EventStream` 使用固定环形缓冲、原子 head/tail、published flag 和 futex 唤醒。提供：

- 非阻塞 push；
- QueueFull；
- blocking push；
- poll/pollBatch/wait；
- complete/completeWithError；
- thread join；
- owned/borrowed event 模式。

设计亮点：

- 有界缓冲，不会无限增长；
- 顺序型流可选择阻塞，避免丢 chunk；
- 终止结果与流事件分离；
- 显式唤醒；
- 对高频 delta 做了低层优化。

风险：

- 所有权规则复杂；
- 注释显示 generic stream、provider stream 和 protocol copy 路径存在 borrowed/owned 两种模式；
- 错误使用可能 double-free、use-after-free 或泄漏；
- 对 Go 实现没有必要复制该低层复杂度。

Halro 使用 Go channel 即可，但应保留这些语义：

- channel 有界；
- 不允许丢失文本 delta；
- client disconnect 能取消 upstream；
- producer 终止后只发一个 terminal state；
- usage 即使在流末尾到达也能记录；
- slow client 必须有写超时。

## 6. Retry、取消与错误处理

Makai Provider 实现包含：

- 网络错误重试；
- HTTP retryable status；
- 错误正文判断；
- 指数退避 + jitter；
- 最大延迟；
- `Retry-After` Header；
- 响应体中的 retry delay；
- cancel token；
- 分阶段取消检查。

这是很有价值的工程实现样本。

但目前 Retry 逻辑在 Anthropic、Google、OpenAI 等 Adapter 中有明显重复。Halro 应将重试分成两层：

```text
HTTP Attempt Policy
  ├─ network error
  ├─ 408 / 409 / 429 / 5xx
  ├─ Retry-After
  └─ backoff + jitter

Route Fallback Policy
  ├─ 哪些错误允许切下一个 Provider
  ├─ 是否已向客户端发送响应字节
  ├─ 幂等性
  └─ 总 deadline / attempt budget
```

特别要注意：流式响应一旦已经向客户端发出 delta，通常不能透明 fallback，否则会产生重复或拼接内容。Makai 的 Provider Retry 不等于 Halro 的 Route Fallback。

## 7. 模型标识与发现

Makai 定义规范化模型引用：

```text
provider_id/api@model_id
```

并对 model ID 做 percent encoding，严格检查分隔符、UTF-8 和歧义。

它还提供 Model Catalog：

- provider；
- api；
- model；
- auth status；
- cache age；
- resolve/list。

对 Halro 的启示：

- 内部模型标识应稳定、无歧义；
- 外部 Alias 与内部目标分开；
- 用户传入 `chat`，路由后应记录原始模型与 resolved model；
- Admin API 返回稳定 route/provider ID，不应依赖 display name。

建议 Halro 内部目标结构：

```text
Target {
  route_id
  provider_id
  upstream_model
}
```

而不是把三者编码成业务字符串后到处解析。Makai 的 model_ref 很适合跨进程协议，Halro 单进程内使用结构体更简单。

## 8. Auth 与 Secret

### 8.1 凭证解析

`resolveApiKey` 采用优先级：

1. 显式 API Key；
2. Provider ID 对应的本地存储；
3. OAuth access token；
4. 否则 `AuthRequired`。

Auth 与 Provider 调用解耦，Provider Protocol 使用结构化错误码驱动 SDK 登录/重试。

这种错误分类值得借鉴。Halro 的 Provider 错误至少应归一化为：

- auth；
- permission；
- rate_limit；
- timeout；
- unavailable；
- invalid_request；
- content_policy；
- upstream_4xx；
- upstream_5xx；
- cancelled。

这些分类决定重试、熔断、Fallback、告警和 HTTP 映射。

### 8.2 本地凭证存储

Makai 在 macOS 优先使用 Keychain，其他情况或不可用时使用 `~/.makai/auth.json`：

- 文件权限 0600；
- 同目录临时文件；
- fsync；
- 原子 rename；
- 清理陈旧临时文件；
- 内存释放前 secure zero；
- 可导入 Codex CLI Keychain Auth。

这是本地 CLI 凭证管理的成熟实现。

但它与 Halro 服务端 Secret 存储需求不同：

- JSON fallback 中保存的是明文 token/API Key，只靠 0600；
- Halro PRD 要求 AES-GCM 密文；
- Server 需要 master key、轮换、备份和多 Provider Secret 管理；
- macOS Keychain 不适合 Linux server 的通用部署模型。

可借鉴原子写、权限检查和 secure zero；不可直接复用其存储格式。

## 9. Usage 与成本

Makai 的 `Usage` 支持：

- input；
- output；
- cache read；
- cache write；
- total token；
- cost；
- 基于 `Model.cost` 计算。

优点是 Provider 完成解析后就产生统一 Usage，天然适合 Gateway 收集。

不足：

- 价格绑定在 Model 上；
- 没有生效时间和价格版本；
- 没有持久化；
- 没有 Project 归属；
- 没有预算账本；
- 没有失败请求成本口径；
- 没有聚合和查询。

Halro 可直接借鉴统一 Usage 结构，但价格表应独立，最终 UsageRecord 应同时记录：

- Provider 报告 token；
- Gateway 估算 token（若 Provider 缺失）；
- 价格版本；
- 计算币种；
- cost；
- cost source/quality。

## 10. Bedrock 状态

`bedrock_converse_stream_api.zig` 明确写明：

> Native Bedrock API provider placeholder. Full AWS event-stream implementation pending.

其 `streamBedrockConverseStream` 和 simple 版本均返回 `error.NotImplemented`，而且 `registerBuiltInApiProviders` 没有注册 Bedrock。

虽然仓库存在：

- `aws_sigv4.zig`；
- Bedrock 文件；
- Bedrock Provider Type；

但不能据此判断 Bedrock 已受支持。

这对 Halro 很重要：Bedrock 是 PRD 的核心 Provider，不能从 Makai 直接移植成品。需要自行实现并重点验证：

- AWS Credential Chain；
- SigV4；
- Region/Service；
- Converse/ConverseStream；
- AWS EventStream framing；
- CRC；
- throttling/error mapping；
- usage；
- model ID/ARN；
- request cancellation。

## 11. Transport 与协议

Makai 抽象了：

- Sender/Receiver；
- AsyncSender/AsyncReceiver；
- in-process pipe；
- stdio；
- SSE；
- WebSocket；
- ack/nack/ping/pong/sync；
- per-stream sequence；
- multi-session multiplexing。

这对分布式 Runtime 很合理，但 Halro V1 是单进程 HTTP Gateway，不需要将内部调用序列化为 Envelope。

建议仅吸收：

- Request ID / Attempt ID；
- 每个流严格有序；
- 明确 terminal event；
- cancel propagation；
- bounded buffer；
- reconnect 不属于 V1 server-to-provider 主链路。

不要吸收：

- 内部 JSON Envelope；
- ack/nack；
- sync snapshot；
- Agent/Auth/Tool 四协议；
- stdio 子进程桥接。

否则会增加延迟、故障面和调试成本。

## 12. Agent 与 Tool 子系统

Makai 包含 Agent Loop、Tool Protocol、本地 Tool Runtime、Shell/File/Edit/Search/Workspace、MCP Bridge 和 TUI。

这些实现本身较完整，但与 Halro V1 的“不做 Agent Trace、插件、工作流、MCP Server”直接冲突。

唯一应吸收的思想是：Provider 调用能力与 Agent 编排能力是不同层。Halro 应保持 Gateway 中立，不应让 Agent/Tool 类型污染基础 Chat/Embeddings 路径。

## 13. CI、测试与成熟度

Makai CI 有：

- Zig pattern guardrail；
- 分组 Unit Test matrix；
- TypeScript SDK E2E；
- Mock Protocol E2E；
- Anthropic/OpenAI/Google/Ollama E2E；
- Provider Protocol Fullstack；
- Distributed Fullstack；
- Release Binary workflow。

正面信号：

- 模块测试较细；
- 协议负向测试和多会话测试被明确列为规范；
- TS SDK 会真实启动 Makai binary；
- 单二进制发布路径已建立。

风险信号：

- 版本仍为 0.0.1；
- Anthropic/OpenAI 等 E2E 使用 `continue-on-error: true`；
- Azure E2E 被 `if: false` 禁用；
- GitHub Copilot 相关 E2E 因 quota/credential 被禁用；
- WebSocket 在设计文档中仍标注需要 hardening；
- Bedrock 未实现；
- 部分架构文档描述目标状态，不能全部视为已交付状态。

因此 Makai 适合作为设计与源码样本，不应被当作已验证的生产 Gateway 依赖。

## 14. 与 Halro PRD 的逐项适配

| Halro 能力 | Makai 参考度 | 判断 |
|---|---:|---|
| Provider Adapter | 高 | 最强参考点 |
| Provider 管理 CRUD | 低 | Registry 不是持久化 Admin 管理 |
| Project/API Key | 无 | 不存在 |
| OpenAI-compatible Server | 低 | 有 OpenAI client/DTO，但无兼容 HTTP server |
| Model Alias/Route | 中 | ModelRef/Catalog 可参考，无业务路由 |
| Token/Cost | 中高 | 有统一 Usage 和 Cost，无持久化与价格版本 |
| 每日预算 | 无 | 不存在 |
| RPM/TPM/并发 | 无 | 不存在 |
| Retry | 高 | Provider attempt retry 很有价值 |
| Fallback | 低 | RoutingPreferences 不是完整 fallback engine |
| Circuit Breaker | 无 | 不存在 |
| 异常检测/Webhook | 无 | 不存在 |
| Dashboard/Web UI | 无 | 有 TUI/demo，不是 Admin UI |
| Parquet/DuckDB | 无 | 不存在 |
| Prometheus | 无 | 不存在 |
| Secret | 中 | 本地安全写入值得借鉴，服务端加密不适用 |
| 单二进制 | 高 | Zig 构建和 release 值得参考 |

## 15. 建议吸收的设计

### P0：Gateway Core 必须吸收

1. Canonical Request/Response/Event；
2. Adapter Type 与 Provider Instance 分离；
3. Provider Capability Matrix；
4. Provider 错误归一化；
5. Retry-After + exponential backoff + jitter；
6. 流式取消和单一 terminal state；
7. 原始模型与 resolved model 同时记录；
8. Usage 在 Adapter 边界统一输出。

### P1：实现时参考

1. OpenAI Completions/Responses 的 SSE parser；
2. Anthropic Messages 的 Tool/Thinking/Cache Usage；
3. Gemini usageMetadata；
4. Azure endpoint/header 差异；
5. 原子 Secret 文件写入；
6. Provider mock server 和协议 golden tests；
7. 真实 Provider E2E 通过环境变量按条件运行。

### P2：后续能力

1. OAuth Provider 登录；
2. 更完整的模型发现；
3. WebSocket 或分布式 Provider Runtime；
4. Agent/Tool 协议。

## 16. 不建议吸收的部分

- 用 Zig Runtime 子进程承载 Go Gateway 的 Provider 调用；
- stdio JSON 协议；
- Auth/Agent/Tool 分布式 Envelope；
- TUI 和本地工具；
- borrowed/owned 手工内存模型；
- URL 启发式作为唯一能力来源；
- 将模型价格直接绑定在代码模型对象；
- Provider 内重复实现 Retry；
- 静默覆盖 Registry 项；
- 明文 `auth.json` 作为 Server Secret Store。

## 17. 建议的 Halro Provider 接口

结合 Makai 的优点和 Go 的语言特性，建议接口近似：

```go
type Adapter interface {
    Type() ProviderType
    Capabilities(model string) Capabilities
    Chat(ctx context.Context, req CanonicalChatRequest) (*CanonicalChatResponse, error)
    ChatStream(ctx context.Context, req CanonicalChatRequest) (EventStream, error)
    Embeddings(ctx context.Context, req CanonicalEmbeddingRequest) (*CanonicalEmbeddingResponse, error)
    Test(ctx context.Context, cfg ProviderConfig) error
    ClassifyError(err error) GatewayError
}

type EventStream interface {
    Recv() (CanonicalEvent, error)
    Close() error
}
```

公共 HTTP Client 层负责：

- deadline；
- connection pool；
- retry policy；
- body size limit；
- metrics；
- tracing；
- cancellation。

Adapter 只负责：

- endpoint/header；
- request transform；
- response/SSE parse；
- usage；
- provider error decode。

这能避免 Makai 当前 Provider 文件中 Retry 和网络循环重复的问题。

## 18. 对当前 PRD 的修订建议

Makai 分析暴露出 PRD 的五个实现缺口：

1. **Provider capability 未定义**：Chat、Embeddings、Streaming、Tools 等能力需要显式矩阵；
2. **流式 fallback 语义未定义**：首字节前和首字节后失败行为不同；
3. **错误分类未定义**：哪些错误 retry、fallback、熔断、告警必须统一；
4. **总时间预算未定义**：多次 retry/fallback 不能无限叠加 Provider timeout；
5. **Bedrock 复杂度被低估**：AWS EventStream 和 Credential Chain 应独立里程碑。

建议为 Route 增加：

```yaml
routes:
  - alias: chat
    timeout: 60s
    max_attempts: 3
    retry:
      max_attempts_per_target: 2
      retry_on: [rate_limit, timeout, unavailable]
    targets:
      - provider: bedrock-sg
        model: anthropic.claude-...
      - provider: openai-primary
        model: gpt-5
```

并规定：

- 所有 attempt 共用一个 deadline；
- 已发送下游响应字节后不跨 Provider fallback；
- 4xx invalid request 不 retry；
- auth failure 默认不 fallback，除非明确配置；
- 429/5xx/timeout 影响 circuit；
- client cancel 不记 Provider failure。

## 19. 最终评价

| 维度 | 评分 | 说明 |
|---|---:|---|
| Provider Adapter 参考价值 | 5/5 | 深度高、Provider 差异覆盖广 |
| Streaming 参考价值 | 5/5 | 事件、背压、取消和终止语义完整 |
| Gateway 控制面参考价值 | 1/5 | Project/Budget/Admin 均不存在 |
| 路由与韧性参考价值 | 3/5 | Retry 强，Fallback/Circuit 不完整 |
| 单二进制参考价值 | 4/5 | 构建与 release 方向一致 |
| 生产成熟度 | 2.5/5 | 0.0.1、Bedrock stub、部分 E2E 非强制 |
| 代码直接复用适合度 | 2/5 | 语言不同，且引入子进程不划算 |
| 设计思想复用适合度 | 5/5 | 很适合转译为 Go Adapter 设计 |

最终建议：把 Makai 当作 Halro 的“Provider Adapter 与 Streaming 参考实现”，不要把它当作部署依赖或完整 Gateway。

## 20. 主要源码依据

- `DESIGN.md`
- `CLAUDE.md`
- `CHANGELOG.md`
- `zig/build.zig`
- `zig/build.zig.zon`
- `zig/src/ai_types.zig`
- `zig/src/api_registry.zig`
- `zig/src/register_builtins.zig`
- `zig/src/event_stream.zig`
- `zig/src/transport.zig`
- `zig/src/transports`
- `zig/src/providers`
- `zig/src/utils/provider_caps.zig`
- `zig/src/utils/retry.zig`
- `zig/src/utils/auth_resolver.zig`
- `zig/src/utils/oauth/storage.zig`
- `zig/src/protocol/model_ref.zig`
- `zig/src/protocol/provider`
- `zig/src/protocol/auth`
- `zig/src/protocol/agent`
- `zig/src/protocol/tool`
- `typescript/README.md`
- `typescript/src`
- `.github/workflows/ci.yml`
- `.github/workflows/release-binaries.yml`

## 21. 许可证与复用提醒

根 `package.json` 声明 TypeScript 包使用 ISC，但本地仓库顶层未发现独立 `LICENSE` 文件。Zig 源码的适用许可证需要在直接复用前向上游确认，不能仅依据 npm package metadata 推定整个仓库均可按 ISC 复制。

建议 Halro 仅借鉴设计并独立实现；若要复制任何源文件或较大代码片段，应先完成许可证确认和 NOTICE 处理。

## 22. 分析限制

本报告基于本地仓库静态源码、配置、测试和 Git 历史分析。当前环境未发现 Zig 工具链，且仓库未安装 Node 依赖，因此未执行 Zig build、SDK 测试或真实 Provider E2E；Provider 可用性判断以当前源码和 CI 配置为准。
