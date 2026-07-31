# LiteLLM 深度分析报告

> 分析对象：`github-project/litellm`
> 上游仓库：`https://github.com/BerriAI/litellm.git`
> 分析基线：`bf1a8fe40329eb018ef420057766ce95a43baaa3`
> 上游版本：`1.96.0`
> 分析日期：2026-07-31
> 分析方式：静态源码、配置、Schema、测试与部署资产分析
> 分析目的：评估 LiteLLM 对 Heimdall 轻量、单二进制、自托管 LLM Gateway V1 的参考价值。

## 1. 结论摘要

LiteLLM 与 Heimdall 的产品能力高度重叠。它不只是 Provider SDK，也包含一个成熟的 OpenAI-compatible AI Gateway，已经覆盖：

- 虚拟 API Key；
- Organization、Team、Project、User、Key 多级治理；
- 模型别名和部署路由；
- 多种负载均衡策略；
- Retry、Fallback、Cooldown；
- RPM、TPM、并发限制；
- 预算、成本和使用量跟踪；
- Provider 凭证管理；
- 管理后台；
- Prometheus、健康检查和告警；
- PostgreSQL、Redis、Docker、Helm 和云部署。

因此，LiteLLM 是两类参考的结合体：

1. **产品语义参考**：它充分暴露了企业 AI Gateway 真正需要处理的边界条件；
2. **复杂度反例**：它为多租户、横向扩展和超宽功能面承担了 Heimdall V1 不应承担的复杂度。

对 Heimdall 最有价值的设计包括：

1. 外部模型名与内部 Provider Deployment 分离；
2. 公共请求模型、Provider Transformation、共享 HTTP 执行器分层；
3. 预算在请求前预留、请求后按真实成本对账；
4. 限流按 Key、Project、Model 等维度组合，而不是只做全局 RPM；
5. Fallback 按错误类型区分，避免所有失败都盲目切换 Provider；
6. Cooldown 状态、失败阈值和恢复时间显式建模；
7. 使用量事件与聚合统计分离；
8. Provider Credential 与 Model Deployment 分离；
9. Key 只保存哈希，Provider Secret 使用带认证加密；
10. 健康检查分为进程存活、服务依赖和真实模型调用。

不应照搬的部分包括：

1. PostgreSQL-only 的管理平面；
2. Redis 驱动的跨实例限流、预算、Cooldown 和缓存；
3. Organization → Team → Project → User → Key 的完整多租户层级；
4. 数十种路由策略和大量特殊请求类型；
5. MCP、Agent、Guardrail、Prompt、Evaluation 等 V1 范围外能力；
6. Python、Node、Prisma、Rust Bridge 和前端构建链组合；
7. 超大核心文件和大量运行时分支。

最终判断：**LiteLLM 应作为 Heimdall 的行为规范和风险清单，而不是代码基座。** Heimdall 应用 Go 重新实现一个受控子集，保留单进程下的强一致闭环，把多节点协调能力明确推迟到 V2。

## 2. 项目画像

| 维度 | LiteLLM |
|---|---|
| 产品定位 | LLM SDK + AI Gateway |
| 核心语言 | Python |
| Gateway HTTP | FastAPI，Uvicorn/Granian/Gunicorn |
| 管理 UI | Next.js 16 + React 18 + Ant Design |
| Provider | 大量商业与开源 Provider |
| OpenAI Compatibility | Chat、Embeddings、Responses、Images、Audio、Batches 等 |
| 管理存储 | PostgreSQL + Prisma |
| 分布式状态 | Redis / DualCache |
| 部署 | Docker、Docker Compose、Helm、Terraform |
| 监控 | Prometheus + 多种日志/可观测性集成 |
| OSS 许可证 | `enterprise/` 外 MIT |
| 企业版 | `enterprise/` 使用独立许可证 |
| Python 版本 | `>=3.10,<3.15` |
| 当前版本 | `1.96.0` |

仓库规模非常大：

- 约 5,084 个 Python 文件；
- 约 1,185 个 TSX 文件；
- 约 404 个 TypeScript 文件；
- 约 110 个 Rust 文件；
- 约 2,324 个 `test_*.py`；
- Python、TypeScript、TSX、Rust、Prisma 等主要源码约 58 万行；
- 贡献者数量达到千人级。

仅五个关键文件就达到：

| 文件 | 行数 |
|---|---:|
| `litellm/proxy/proxy_server.py` | 16,781 |
| `litellm/router.py` | 11,788 |
| `litellm/llms/custom_httpx/llm_http_handler.py` | 13,140 |
| `litellm/integrations/prometheus.py` | 4,255 |
| `schema.prisma` | 1,463 |

这说明 LiteLLM 的成熟度和覆盖面很高，也说明它已经越过了 Heimdall “Simple First” 所允许的复杂度边界。

## 3. 总体架构

LiteLLM 实际包含两个产品：

```text
Python SDK
  └─ litellm.completion / embedding / responses / ...

AI Gateway
  └─ FastAPI Proxy
      └─ LiteLLM SDK
          └─ Provider Transformation
              └─ Shared HTTP Handler / Provider SDK
```

Gateway 请求主链可以概括为：

```text
OpenAI-compatible Request
  │
  ▼
Authentication / Virtual Key
  │
  ├─ allowed routes
  ├─ allowed models
  ├─ team / project / user context
  ├─ budget
  └─ rpm / tpm / concurrency
  │
  ▼
Request Processing
  │
  ├─ metadata injection
  ├─ policy / guardrail hooks
  ├─ model alias resolution
  └─ request normalization
  │
  ▼
Router
  │
  ├─ deployment selection
  ├─ pre-call checks
  ├─ retry
  ├─ fallback
  └─ cooldown
  │
  ▼
LiteLLM SDK
  │
  ▼
Provider Transformation + HTTP Handler
  │
  ▼
OpenAI-compatible Response / SSE
  │
  ▼
Spend Queue / Metrics / Alerts / Logs
```

这种分层整体正确。问题在于，长期扩展形成了两个“上帝对象”：

- `proxy_server.py` 同时承担路由注册、配置、生命周期和大量端点协调；
- `router.py` 同时承担部署选择、重试、Fallback、Cooldown、缓存和多种策略。

Heimdall 应保留请求管线，不应保留文件组织方式。

## 4. OpenAI-compatible API

Proxy 明确提供：

- `POST /v1/chat/completions`；
- `POST /chat/completions`；
- `POST /v1/embeddings`；
- `POST /embeddings`。

此外还有 Azure 风格路径、Responses、Images、Audio、Files、Batches、Rerank、Realtime、A2A、MCP、Agents 等大量接口。

Chat 请求会：

1. 解析请求体；
2. 从认证上下文注入 user、team、organization、project、agent 等信息；
3. 交给统一请求处理器；
4. 执行路由和 Provider 调用；
5. 对流式响应返回 SSE；
6. 通过 callback/hook 系统记录成功或失败。

对 Heimdall 的结论：

- V1 只实现 PRD 中的 Chat Completions 和 Embeddings；
- 路径同时接受带 `/v1` 和不带 `/v1` 的形式价值不高，可只保留标准路径；
- 请求结构应尽量透传未知字段，但 Provider Adapter 必须决定支持、转换或拒绝；
- OpenAI-compatible 应定义为“协议兼容”，不能承诺每个 Provider 的所有语义完全一致；
- 必须为流式和非流式建立同一套 Usage、错误和完成状态。

## 5. Provider 抽象

### 5.1 分层模型

LiteLLM Provider 层的核心模式是：

```text
Common Request
  │
  ▼
Provider Config / Transformation
  ├─ validate_environment
  ├─ map parameters
  ├─ transform_request
  ├─ build URL and headers
  ├─ transform_response
  └─ map exceptions
  │
  ▼
Shared HTTP Handler
```

基础 Chat Transformation 定义请求转换和响应转换契约。具体 Provider 继承或组合该能力。

例如：

- DeepSeek 继承 OpenAI GPT Config，只覆盖差异；
- Azure OpenAI 处理 deployment、api-version、Azure URL 和认证；
- Gemini AI Studio 复用 Vertex Gemini 语义；
- Bedrock Converse 有独立的大型转换器；
- OpenAI-compatible Provider 大量复用 OpenAI 转换。

这个设计比在 Handler 中按 Provider 写巨大 `switch` 更可维护。

### 5.2 Provider 差异不是一个 API Base

LiteLLM 源码证明，即使都支持聊天，差异仍包括：

- system/developer role；
- tool call 与 tool result 格式；
- reasoning/thinking 字段；
- JSON schema；
- multimodal content；
- token 参数命名；
- endpoint 与认证；
- usage 字段；
- stop reason；
- 流式增量格式；
- 错误码和重试提示；
- 模型能力和上下文窗口；
- Bedrock Converse 与 Invoke 的差异。

Heimdall V1 的 Adapter 不能只包含 `BaseURL + APIKey`。

建议接口：

```go
type Adapter interface {
    Type() ProviderType
    Validate(ctx context.Context, cfg ProviderConfig) error
    Capabilities(model string) Capabilities
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
    ChatStream(ctx context.Context, req ChatRequest) (EventStream, error)
    Embeddings(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error)
    ListModels(ctx context.Context) ([]ProviderModel, error)
    ClassifyError(error) ProviderError
}
```

Provider Instance 另行保存：

```text
id
type
endpoint
region
encrypted credential reference
timeout
enabled
health state
max concurrency
```

### 5.3 应限制 V1 的公共模型

LiteLLM 公共数据模型极宽，导致共享 HTTP Handler 超过 1.3 万行。

Heimdall V1 建议只标准化：

- Chat message；
- text/image input 的最小集合；
- tool definitions 和 tool calls；
- temperature、top_p、max_tokens；
- stream；
- response_format 的有限子集；
- embedding input；
- usage；
- finish reason；
- Provider request ID。

不支持的参数必须有明确策略：

```text
strict: 返回 400 unsupported_parameter
drop: 删除字段并记录 warning
passthrough: 仅对同协议 Provider 透传
```

默认应为 `strict` 或显式配置，不能静默改变用户请求。

## 6. 模型、部署与 Alias

LiteLLM 的 `model_list` 把对外 `model_name` 与内部 `litellm_params.model` 分开。

一个公共模型组可以映射多个 Deployment：

```yaml
model_list:
  - model_name: chat
    litellm_params:
      model: openai/gpt-5
  - model_name: chat
    litellm_params:
      model: bedrock/anthropic.claude-...
```

Router 的解析路径大致包括：

1. 显式 API Key/API Base；
2. 批量或逗号分隔模型；
3. 特殊 API 类型；
4. Team model mapping；
5. 精确 model group 或 deployment ID；
6. alias；
7. 默认模型、通配模型或 deployment name；
8. 找不到时返回错误。

它还会清理内部 mock/testing 参数，避免调用方触发测试行为。这是一个重要的安全提醒：内部控制字段不能与公共请求参数共享命名空间。

Heimdall 建议把概念固定为三层：

```text
Requested Model
  "chat"
     │ alias
     ▼
Route
  ordered targets + policy
     │
     ▼
Deployment
  provider instance + provider model
```

不要把 Provider、Model、Alias、Route 混成一张表。

建议稳定 ID：

- `provider_id`：凭证和 endpoint 实例；
- `deployment_id`：某 Provider 上的具体模型；
- `route_id`：对外暴露的逻辑模型；
- `alias`：可选的人类友好名称。

## 7. Router、Retry、Fallback 与 Cooldown

### 7.1 路由策略

LiteLLM Router 支持或扩展出：

- simple shuffle；
- least busy；
- usage-based；
- latency-based；
- cost-based；
- tag-based；
- complexity-based；
- adaptive、quality-based 等高级策略；
- routing groups；
- deployment affinity；
- weighted failover；
- health-check routing。

Heimdall V1 不需要这些策略的全集。

建议只保留：

1. `ordered`：按顺序选择健康目标；
2. `round_robin`：同级 Deployment 轮询；
3. `weighted`：可选，若实现成本可控；
4. `least_inflight`：可选，适合单进程。

成本、延迟、质量动态路由依赖足够样本、窗口统计和防抖，否则容易产生振荡。

### 7.2 Retry

LiteLLM 能按异常类型配置重试策略，例如：

- AuthenticationError；
- Timeout；
- RateLimit；
- ContentPolicy；
- BadRequest。

正确语义不是“失败就重试”。

Heimdall 建议：

| 错误 | 同 Deployment Retry | 切换 Fallback |
|---|---:|---:|
| 网络断开/连接超时 | 是，有限次数 | 是 |
| 429 | 遵守 Retry-After | 是 |
| 5xx | 是，指数退避 | 是 |
| 401/403 Provider Auth | 否 | 是，并告警 |
| 参数错误 | 否 | 通常否 |
| 上下文超限 | 否 | 仅切到更大上下文模型 |
| 内容策略拒绝 | 否 | 仅显式配置时切换 |
| Project 预算/RPM | 否 | 否 |
| 客户端取消 | 否 | 否 |

Retry 必须受总请求 deadline 约束，并使用 jitter，不能让每个 Fallback 重新获得完整超时。

### 7.3 Fallback

LiteLLM 区分：

- 普通 Fallback；
- Context Window Fallback；
- Content Policy Fallback；
- default fallback；
- max fallback 次数。

这是值得吸收的语义。Fallback Chain 不应只有字符串数组，而应允许错误条件：

```yaml
routes:
  - id: chat
    targets:
      - deployment: claude-primary
      - deployment: gpt-backup
        on: [timeout, rate_limit, provider_5xx]
      - deployment: nova-backup
        on: [timeout, rate_limit, provider_5xx]
```

### 7.4 流式 Fallback

LiteLLM 为流式调用提供 Fallback 包装并记录 Fallback 元数据。这类能力实现复杂，尤其是已经把部分 token 发给客户端后：

- 切换模型会造成文本重复或语义断裂；
- usage 很难合并；
- OpenAI SDK 不理解“中途换模型”的语义；
- 客户端可能已经执行 tool call；
- 两个 Provider 都会产生费用。

Heimdall V1 应采用明确规则：

> 只有在尚未向客户端发送第一个下游 payload 时允许 Fallback；首个 payload 发出后，任何失败都终止当前 SSE。

这条约束可显著降低错误恢复复杂度。

### 7.5 Cooldown 与 Circuit Breaker

LiteLLM 会根据 429、401、408、404、5xx、失败比例、最小请求数等条件把 Deployment 放入 Cooldown Cache，并在 TTL 后恢复。

它本质上接近 Circuit Breaker，但名称和实现更偏缓存状态。

Heimdall 建议显式三态：

```text
Closed
  └─ 连续失败/窗口失败率超阈值 → Open

Open
  └─ cooldown 到期 → HalfOpen

HalfOpen
  ├─ probe 成功 → Closed
  └─ probe 失败 → Open
```

单进程可用内存原子状态实现，不需要 Redis。状态快照可持久化，但不应让一次异常退出永久锁死 Provider。

## 8. Virtual Key 与认证

LiteLLM 的 Virtual Key 默认生成 `sk-` 前缀随机 token，并在数据库中保存 SHA-256 哈希。认证时对输入 token 做相同哈希后查询。

Schema 中 Key 支持：

- name/alias；
- expiry；
- allowed models；
- user/team/project/organization；
- permissions；
- max parallel；
- TPM/RPM；
- max budget 和 reset；
- allowed routes；
- policies/access groups；
- blocked；
- key rotation 和旧 Key 宽限期。

这套设计远超 Heimdall V1，但基本安全语义正确。

Heimdall 建议：

- Key 格式使用 `gw_`；
- 至少 256 bit CSPRNG 随机数；
- 只在创建响应中返回一次明文；
- 持久层只保存 `SHA-256(key)`；
- 比较使用固定时间比较；
- 日志中只记录 key ID 或哈希前缀；
- 提供 revoke，不在 V1 做复杂 rotation grace period；
- Key 归属 Project，不引入 Team/User/Organization；
- 管理员也不能找回明文 Key，只能重建。

额外建议：哈希前保存版本信息，如 `v1:<sha256>`，便于未来升级方案。

## 9. Provider Credential 安全

LiteLLM 将 Credential 与 Model 分开，`LiteLLM_CredentialsTable` 保存 credential values 和 info。

其加密工具支持：

- AES-256-GCM；
- `v2:gcm:` 版本前缀；
- 12-byte nonce；
- ciphertext + 16-byte tag；
- legacy XSalsa20-Poly1305 兼容；
- 从 `LITELLM_SALT_KEY` 或 master key 派生加密 Key。

值得借鉴：

1. 加密格式必须版本化；
2. nonce 每次随机；
3. 使用 AEAD，而非只有加密没有完整性；
4. 解密需要兼容旧格式；
5. Credential 独立于 Model，便于复用和轮换。

需要警惕：

- 当前 Key 派生是单次 SHA-256，源码也承认不等同于 HKDF/PBKDF2；
- AES-GCM 未利用 AAD 绑定记录身份；
- “数据库字段已加密”不代表日志、异常和 UI 不会泄漏 Secret。

Heimdall 推荐：

```text
master.key: 32 random bytes, chmod 0600
record key: HKDF-SHA256(master, salt, "provider-secret:v1")
AEAD: AES-256-GCM
AAD: provider_id + field_name + format_version
wire format: v1:<base64(nonce|ciphertext|tag)>
```

配置和 Admin API 中的敏感字段必须支持：

- 输出始终脱敏；
- 更新时空值表示“不变”，不能误清空；
- Secret 永不进入 structured log；
- 连接测试错误先经过脱敏。

## 10. Project 与治理模型

LiteLLM 的实体层级是：

```text
Organization
  └─ Team
      └─ Project
          └─ Virtual Key

User / Membership / End User / Agent / Tag
  └─ 可分别挂预算与使用量
```

`LiteLLM_ProjectTable` 位于 Team 与 Key 之间，包含：

- alias 和 description；
- allowed models；
- spend / model spend；
- per-model RPM/TPM；
- blocked；
- budget relation；
- object permission。

这个 Project 与 Heimdall PRD 的 Project 很接近。

Heimdall V1 应刻意砍平为：

```text
Project
  ├─ one or more API Keys
  ├─ allowed routes/models
  ├─ daily budget
  ├─ RPM
  ├─ TPM
  ├─ max concurrency
  └─ enabled
```

虽然 PRD 写的是单个 API Key，但建议数据模型从一开始支持一个 Project 多个 Key。这样可以安全轮换 Key，而不复制预算和限流配置。

不需要引入 Organization、Team、User Membership。Admin 本身也不应成为一个“租户”。

## 11. Budget

### 11.1 LiteLLM 的预算模型

`LiteLLM_BudgetTable` 支持：

- hard max budget；
- soft budget；
- max parallel；
- TPM/RPM；
- per-model budget；
- duration 和 reset time；
- allowed models；
- 关联 Organization、Project、Key、End User、Tag 和 Membership。

Key 和 Team 等实体还保留部分直接预算字段，体现了长期兼容和演化成本。

### 11.2 预算不是简单的 `today_cost >= limit`

并发请求会产生经典竞态：

```text
当前消费 $49，预算 $50

请求 A 检查：未超
请求 B 检查：未超
A 花费 $2
B 花费 $2
最终 $53
```

LiteLLM 已有 Budget Reservation、原子计数器、数据库 reseed 和异步 Spend 更新，说明生产预算的难点在一致性，而不是查询报表。

Heimdall 单进程可做得更简单，但仍应正确：

1. 请求前估算最大/合理成本；
2. 在 Project 内存账本中原子预留；
3. 超预算立即返回 403；
4. 请求结束按真实成本 reconciliation；
5. 请求失败释放未消费预留；
6. 每次变更写入 append-only usage/WAL；
7. 启动时从当日持久记录重建；
8. 日界线使用配置时区，不默认依赖机器本地时区。

建议把状态分开：

```text
committed_spend
reserved_spend
available = limit - committed - reserved
```

对于无法在请求前可靠计算的输入，可按 token 估算加安全系数；如果 Provider 不返回 Usage，必须使用本地 token estimator 并标记 `estimated=true`。

PRD 的 “Disable Key” 建议改为 Project 状态或预算闸门，不要永久修改 Key enabled。第二天预算重置时应自动恢复。

## 12. RPM、TPM 与并发限制

LiteLLM 有多代 limiter hook，并按 Key、Team、User、End User、Project、Model 等维度维护分钟计数；DualCache 可使用内存或 Redis，在多实例时依赖 Redis 共享状态。

还存在动态限流器，用活跃 Project 和模型容量计算份额。这是大规模平台能力，不适合 V1。

Heimdall 单实例建议：

- RPM：token bucket；
- TPM：token bucket；
- concurrency：weighted 或普通 semaphore；
- 每个 Project 独立状态；
- 可选 Provider/Deployment 全局并发保护；
- 请求前扣除估算输入 token；
- 请求后用真实 total token 补扣或返还；
- 限流拒绝返回 429，并带 `Retry-After`；
- 预算拒绝返回 403；
- allowed model 拒绝返回 403 或 OpenAI-compatible 404，需统一约定。

需要定义清楚 TPM 的口径：

- 只算 input，还是 input + output；
- streaming 时何时扣 output；
- Fallback 尝试是否计入；
- Provider 未返回 Usage 时如何估算。

建议使用 `input + output`，每次真实 Provider 尝试都计入内部 usage，但 Project 对外账单是否计算失败请求需由成本事实决定。

## 13. Usage、成本与存储

### 13.1 LiteLLM 记录模型

`LiteLLM_SpendLogs` 是 per-request 事实表，包含：

- request ID；
- call type；
- hashed API key；
- spend；
- total/input/output token；
- start/end/duration；
- first completion time；
- model、model ID、model group；
- Provider、API base；
- user、team、organization、end user；
- cache；
- tags；
- IP；
- messages；
- response；
- session、status、agent；
- proxy request。

另有：

- ErrorLogs；
- 按 User、Team、Organization、Tag、End User、Agent 的日聚合表；
- Audit Logs；
- Spend 更新队列；
- 冷存储和清理逻辑。

### 13.2 写入路径

LiteLLM 将请求事实交给异步 Spend Queue，再更新 request log 和各层级聚合。队列有上限，满载时尝试合并更新，而不是简单丢弃。

这说明高吞吐下不能让每个请求同步更新十几张表。

Heimdall 的 Parquet 方向适合分析，但 Parquet 不是好的实时事务日志：

- 追加与崩溃恢复不如 WAL；
- 每次请求直接改 Parquet 成本高；
- 当天文件损坏会影响预算重建；
- Dashboard 实时查询不应每次扫描 Parquet。

建议 V1 使用三层：

```text
Request Path
  └─ bounded usage channel
      ├─ append-only WAL / JSONL segment
      ├─ in-memory aggregates
      └─ daily Parquet compaction

Dashboard
  └─ in-memory aggregates + recent ring buffer

Historical Query
  └─ Parquet, optional DuckDB
```

若坚持最终只有 `data/`，WAL 也可放在其中，不构成外部依赖。

### 13.3 成本计算

LiteLLM 的成本计算覆盖：

- input/output token 差异价格；
- cache read/write token；
- reasoning token；
- 按秒或按请求计价；
- service tier；
- region；
- Provider 特殊规则；
- discount/margin；
- 自定义单价；
- 图像、音频、工具等非文本计价。

Heimdall V1 只需：

```yaml
prices:
  - provider: openai
    model: gpt-5
    input_per_million: ...
    output_per_million: ...
    effective_from: ...
```

但价格表必须：

- 带版本或生效时间；
- 每条 Usage 保存当时使用的价格；
- 支持人工覆盖；
- 未知价格记录 token 但 `cost=null`，不能假装为 0；
- Dashboard 区分真实成本、估算成本和未知成本。

### 13.4 日志隐私

LiteLLM Schema 允许保存 messages、response、proxy request 和 requester IP。能力强，但隐私风险高。

Heimdall 默认 Usage Log 不应保存 Prompt/Response 正文，只保存：

- request ID；
- project/key ID；
- route/deployment/provider/model；
- token；
- cost；
- latency/TTFT；
- status/error class；
- timestamps；
- retry/fallback count；
- estimated flags。

正文日志必须是显式 opt-in，并具备脱敏、保留期和访问审计。

## 14. Prometheus、健康检查与告警

### 14.1 Prometheus

LiteLLM 的 Prometheus 集成远超 PRD 的六个指标，覆盖请求、token、费用、延迟、TTFT、失败、预算、缓存、部署、用户维度等，并包含 label 管理和 series 清理逻辑。

这暴露了一个常见风险：Project、User、Model、API Key 等高基数字段会造成 Prometheus series 爆炸。

Heimdall V1 建议低基数指标：

```text
gateway_requests_total{route,provider,status_class}
gateway_tokens_total{route,provider,direction}
gateway_cost_usd_total{route,provider}
gateway_request_duration_seconds{route,provider}
gateway_time_to_first_token_seconds{route,provider}
gateway_provider_up{provider}
gateway_active_requests{provider}
gateway_rate_limit_rejections_total{scope,reason}
gateway_budget_rejections_total{project}
gateway_fallback_total{from_provider,to_provider,reason}
```

其中 `project` 可能高基数。默认不应把 key hash、request ID、完整 model ID 放入 label；Project 维度可通过配置开关或仅在内部 Dashboard 使用。

Prometheus 延迟应使用 seconds histogram，而不是名为 `latency_ms` 的无类型值。

### 14.2 健康检查

LiteLLM 区分：

- liveliness；
- readiness；
- Provider/model 健康；
- callback/service 健康；
- database/cache 健康；
- in-flight 和 graceful shutdown 状态。

Heimdall 建议：

- `/health/live`：进程事件循环/HTTP Server 存活；
- `/health/ready`：配置已加载、master key 可用、存储可写；
- Admin “测试连接”：发最小 Provider 请求；
- 后台 Provider probe：低频、带独立预算和 timeout；
- Passive health：真实流量结果更新 Circuit Breaker；
- `/metrics` 可单独配置是否需要认证或仅监听内网地址。

连接测试不能只验证 TCP/TLS；必须至少验证认证和目标模型权限。

### 14.3 告警

LiteLLM AlertType 包括：

- LLM exception；
- slow/hanging request；
- budget；
- spend report；
- failed spend tracking；
- DB exception；
- cooldown；
- outage/region outage；
- fallback report；
- Key、Team 等管理事件。

其 Slack Alerting 也能向结构化 Webhook 发送事件，并通过缓存窗口做 outage 聚合。

Heimdall 不需要为飞书、企业微信、Slack、Discord 各写业务逻辑。建议：

```text
Alert Event
  └─ Generic Webhook Delivery
      ├─ template: generic
      ├─ template: slack
      ├─ template: feishu
      ├─ template: wecom
      └─ template: discord
```

告警系统必须有：

- dedup key；
- rolling window；
- cooldown；
- severity；
- delivery retry；
- 有界队列；
- 超时；
- SSRF 防护；
- Secret header 加密；
- 测试告警；
- 失败计数。

“Error Rate >20%” 必须同时配置最小样本数，否则 1 次请求失败就会告警。

## 15. Admin UI

LiteLLM Dashboard 使用：

- Next.js 16；
- React 18；
- Ant Design；
- TanStack Query/Table；
- Recharts；
- Tailwind；
- Zod；
- OpenAPI 类型生成；
- 独立 Node 构建。

构建后静态产物嵌入/打包到 Proxy 镜像中。它支持比 Heimdall PRD 更广的 Key、Team、Budget、Model、Credential、日志、SSO、Guardrail 等管理功能。

这与 Heimdall 的 HTMX 选择形成鲜明对比：

| 方向 | LiteLLM | Heimdall |
|---|---|---|
| UI 架构 | SPA/Next.js | Server-rendered HTMX |
| 构建链 | Node + React | Go template + static |
| 状态管理 | Client query/cache | Server authoritative |
| 适用规模 | 复杂管理平台 | 轻量单机控制台 |

Heimdall 应坚持 HTMX。需要借鉴的是信息架构，不是技术栈：

- Dashboard；
- Projects/Keys；
- Providers/Credentials；
- Routes/Models；
- Usage/Logs；
- Alerts；
- Settings。

敏感操作应使用 POST/PUT/DELETE、CSRF、防重复提交和审计日志。Key 创建页应提供一次性复制界面，刷新后不可恢复。

## 16. 配置与运行时状态

LiteLLM 支持 YAML、环境变量、数据库模型配置、管理 API 和大量 general/router settings。灵活性高，但配置来源多，优先级复杂。

Heimdall 应提前定义唯一优先级：

```text
CLI flags
  > environment overrides
  > config.yaml
  > built-in defaults
```

Provider、Project、Route 如果允许 UI 修改，就不能只存在 YAML 中。建议：

- `config.yaml`：启动、TLS、Admin、master key 路径、数据目录、全局默认；
- `secrets.enc`：Provider Secret；
- `data/state.*`：UI 管理的 Provider metadata、Project、Route、Key hash；
- `data/usage/`：WAL 与 Parquet；
- 环境变量只覆盖启动级配置，不用来动态管理业务实体。

必须支持：

- 配置 schema version；
- 启动时严格校验；
- 原子写入；
- 自动备份上一版本；
- 变更审计；
- 热更新失败回滚；
- Secret 与普通配置分离。

## 17. 数据库与部署依赖

LiteLLM Prisma datasource 明确指定 PostgreSQL。官方 Compose 至少启动：

- LiteLLM；
- PostgreSQL 16；
- Prometheus。

大规模部署文档进一步使用 Redis/ElastiCache/Memorystore、负载均衡器、云数据库和多实例。

其 Docker 构建还涉及：

- Python 环境；
- Node 构建 Admin UI；
- Prisma CLI 和 engines；
- Rust/maturin 组件；
- enterprise 源码；
- migration entrypoint。

这与 Heimdall 的目标完全不同：

```text
LiteLLM production:
  container + postgres + optional/required redis + migrations + UI build

Heimdall V1:
  one Go binary + config + master key + data directory
```

因此，不要尝试“裁剪 LiteLLM 部署”来实现 Heimdall。裁剪后的维护成本仍然来自上游架构，而收益会不断被新增功能侵蚀。

## 18. 安全审视

LiteLLM 展示了 Gateway 必须防御的攻击面：

- 公共请求注入内部控制参数；
- 管理端点越权；
- Key 泄漏；
- Provider Secret 泄漏；
- SSRF 到任意 API Base/Webhook；
- 环境变量引用被请求参数利用；
- 日志保存 Prompt/Response；
- SSO redirect/open redirect；
- 高基数指标导致资源耗尽；
- 无限流 streaming；
- 后台队列积压；
- 不安全的健康检查参数；
- Secret 出现在异常文本。

源码中健康检查会拒绝来自请求参数的 `os.environ/...` 引用，路由也清理内部 mock 参数。这些都是经过真实攻击面演化后的防护。

Heimdall V1 至少需要：

1. Admin 与 Gateway API 分离 middleware；
2. Admin session 使用 Secure、HttpOnly、SameSite Cookie；
3. CSRF；
4. 登录失败限流；
5. API Key 固定时间校验；
6. Provider/Webhook URL SSRF 校验；
7. 禁止链接本地 metadata 和私网地址，或明确 allowlist；
8. HTTP client 禁止自动跟随到不可信网段；
9. Request body、response body、SSE event 大小限制；
10. Header allowlist，不能把客户端 Authorization 透传给 Provider；
11. 日志脱敏；
12. master key 权限检查；
13. TLS 配置；
14. graceful shutdown 和 in-flight drain；
15. 管理变更审计。

## 19. 测试、成熟度与维护性

LiteLLM 的测试资产很丰富，约有 2,324 个 `test_*.py`，还包括：

- Proxy unit tests；
- Provider tests；
- pass-through tests；
- UI Vitest；
- Docker tests；
- load tests；
- CI workflows；
- 安全扫描和规则；
- stable image 的长时间负载测试流程。

这说明它是成熟、活跃、生产导向的项目。

同时，成熟不等于结构简单：

- 大量兼容分支；
- 多代 limiter 并存；
- Schema 中存在新旧预算字段；
- OSS 与 Enterprise 分支；
- 同时支持 SDK 和 Proxy；
- 同时支持许多 API 形态；
- 核心文件过大；
- callback/hook 对请求生命周期有隐式影响。

Heimdall 应把测试预算集中在不变量：

1. Key 明文永不持久化；
2. 预算并发不超卖；
3. RPM/TPM/并发准确；
4. Fallback 不重复输出流；
5. 客户端取消能传播到 Provider；
6. Provider 错误映射稳定；
7. Usage 在崩溃后可恢复；
8. Secret 不进入日志；
9. 配置更新原子；
10. Circuit Breaker 状态转换正确；
11. 时区和日预算 reset 正确；
12. OpenAI SDK 兼容回归。

## 20. 与 Heimdall PRD 的逐项对照

| Heimdall PRD | LiteLLM 现状 | 对 Heimdall 的建议 |
|---|---|---|
| Provider 管理 | Credential + Proxy Model + UI/API | 分离 Credential、Provider Instance、Deployment |
| AES-GCM | 支持版本化 AES-256-GCM | 用 HKDF + AAD 强化 |
| Project | 完整 Project，位于 Team 和 Key 之间 | 直接采用扁平 Project |
| Key hash | SHA-256 保存 | 采用，前缀改为 `gw_` |
| OpenAI API | 覆盖远超 Chat/Embedding | V1 严格限两个端点 |
| Alias | model group、alias、mapping 多层 | 明确 Route/Alias/Deployment 三层 |
| Usage | PostgreSQL 事实表 + 聚合表 | WAL + 内存聚合 + 日 Parquet |
| Daily Budget | 多实体预算和 reservation | 单 Project 原子 reservation |
| RPM/TPM/并发 | 多维 DualCache/Redis limiter | 单进程 token bucket + semaphore |
| Fallback | 类型化 fallback | 保留错误分类，简化策略 |
| Circuit Breaker | Cooldown Cache | 实现显式 Closed/Open/HalfOpen |
| 异常检测 | 丰富 AlertType 与窗口缓存 | 最小样本 + 窗口 + dedup |
| Webhook | Slack/通用 webhook 等 | 一个事件模型，多种模板 |
| Dashboard | Next.js 完整后台 | HTMX 保持轻量 |
| Parquet | 主路径为 PostgreSQL | Parquet 只做历史分析 |
| DuckDB | 非核心 | 可选 CLI/查询层，不进请求路径 |
| Prometheus | 非常丰富且处理高基数 | 少量稳定低基数指标 |
| 单二进制 | 不符合 | Go embed UI，坚持目标 |
| 零外部依赖 | 不符合生产形态 | 单节点约束必须写进文档 |

## 21. 建议吸收的设计

### P0：V1 必须吸收

1. Requested Model、Route、Deployment、Provider Instance 分层；
2. Provider Transformation 与 HTTP Transport 分离；
3. 统一 ProviderError 分类；
4. Key 只保存 SHA-256；
5. Provider Secret 使用版本化 AEAD；
6. Project 多 Key；
7. 预算预留与真实成本对账；
8. RPM、TPM、并发三种独立限制；
9. Fallback 按错误分类；
10. 首个流式 payload 后禁止 Fallback；
11. 显式 Circuit Breaker；
12. Usage 事实与 Dashboard 聚合分离；
13. 未知价格不能记为 0；
14. 默认不记录 Prompt/Response；
15. live、ready、Provider probe 分离。

### P1：V1 可选或紧随其后

1. per-deployment concurrency；
2. Retry-After；
3. TTFT；
4. 配置热加载和回滚；
5. Key rotation；
6. Provider Credential 复用；
7. 告警 dedup/cooldown；
8. Usage WAL compaction；
9. 价格表版本；
10. graceful shutdown。

### P2：后续版本

1. latency/cost routing；
2. multi-instance coordination；
3. Organization/Team；
4. SSO/RBAC；
5. distributed rate limiting；
6. external DB；
7. guardrails；
8. advanced audit；
9. prompt/response opt-in storage；
10. regional routing。

## 22. 不建议吸收的部分

1. 直接依赖 LiteLLM Python SDK；
2. 为兼容所有 Provider 建立超宽请求对象；
3. 将 Proxy endpoint 和生命周期集中到单文件；
4. 将 Router、Retry、Fallback、Cooldown、缓存集中到单类；
5. V1 引入 Redis；
6. V1 引入 PostgreSQL/Prisma；
7. V1 实现 Organization/Team/User/End User；
8. V1 实现数十类 OpenAI/Anthropic API；
9. V1 实现 Guardrail、MCP、Agent、Prompt 和 Evaluation；
10. 为 Dashboard 引入完整 SPA；
11. 在 Prometheus label 中暴露高基数 ID；
12. 默认保存完整请求与响应；
13. 流式输出开始后跨 Provider 续写；
14. 运行时允许任意 API Base 或 Webhook URL；
15. 多个配置来源缺少明确优先级。

## 23. 建议的 Heimdall 内部架构

```text
cmd/heimdall
  │
  ▼
internal/http
  ├─ gatewayapi
  ├─ adminapi
  └─ web
  │
  ▼
internal/gateway
  ├─ authenticate
  ├─ authorize model
  ├─ reserve limits/budget
  ├─ resolve route
  ├─ execute attempts
  ├─ stream response
  └─ reconcile usage
  │
  ├───────────────┬────────────────┬────────────────┐
  ▼               ▼                ▼                ▼
router          limiter          budget          circuit
  │
  ▼
provider
  ├─ openai
  ├─ azureopenai
  ├─ gemini
  └─ bedrock
  │
  ▼
usage
  ├─ WAL
  ├─ live aggregate
  └─ parquet compactor
```

建议核心请求阶段：

```text
Received
  → Authenticated
  → Authorized
  → CapacityReserved
  → BudgetReserved
  → Routed
  → ProviderStarted
  → FirstByteSent
  → Completed | Failed | Cancelled
  → UsageCommitted
```

把阶段写入 request context，可避免 limiter、budget、usage 和 alert hook 各自猜测请求状态。

## 24. 对当前 PRD 的修订建议

### 24.1 Project Key

把：

> Project 包含 API Key

改为：

> Project 可包含多个 API Key；Key 明文只显示一次，支持撤销和重建。

### 24.2 Budget

补充：

- 预算采用预留 + 对账；
- 以配置时区重置；
- 未知价格时的策略；
- 超预算只关闭预算闸门，不永久 disable Key；
- 并发安全和崩溃恢复。

### 24.3 Fallback

补充：

- Fallback 条件；
- 最大 attempt；
- 全局 deadline；
- Retry-After；
- 首个 SSE payload 后禁止 Fallback；
- 每次 attempt 都记录 Usage 和错误。

### 24.4 Storage

把：

> 默认 Parquet

细化为：

> 请求先写可恢复 WAL；实时 Dashboard 使用内存聚合；每日压缩为 Parquet；DuckDB 仅用于历史分析。

### 24.5 Provider

增加三层：

- Credential；
- Provider Instance；
- Model Deployment。

### 24.6 Security

增加：

- HKDF 和 AAD；
- CSRF；
- SSRF 防护；
- Secret 日志脱敏；
- 管理审计；
- 请求/响应大小限制；
- master key 权限校验；
- webhook header 加密。

### 24.7 Observability

增加：

- TTFT；
- attempt/fallback count；
- estimated usage/cost；
- unknown cost；
- rejected requests；
- bounded metric labels；
- usage queue backlog/drop 指标。

## 25. 最终评价

| 评价维度 | 分数 | 说明 |
|---|---:|---|
| 产品能力参考 | 10/10 | 与 Heimdall 目标高度重叠 |
| Provider 实现参考 | 9/10 | 转换层覆盖极广 |
| 路由语义参考 | 9/10 | Retry/Fallback/Cooldown 边界丰富 |
| 预算与限流参考 | 9/10 | 展示了生产一致性的真实复杂度 |
| 安全参考 | 8/10 | 有大量演化后的防护，也有可改进点 |
| 可观测性参考 | 9/10 | 指标、日志、告警覆盖完整 |
| 轻量化参考 | 3/10 | 技术栈与依赖明显偏重 |
| 直接复用价值 | 4/10 | 能力过宽，裁剪成本高 |
| Heimdall V1 适配度 | 7/10 | 适合借鉴语义，不适合作为基座 |

LiteLLM 回答的是：

> 一个横向扩展、多租户、覆盖大量 Provider 和 API 类型的企业 AI Gateway，最终会需要什么？

Heimdall 要回答的是：

> 在单节点、单二进制、零外部依赖的约束下，哪些能力构成可靠的最小企业网关闭环？

两者不是竞争关系。LiteLLM 提供完整问题空间，Heimdall 应通过严格取舍提供更小的运维面。

## 26. 主要源码依据

- `README.md`
- `pyproject.toml`
- `LICENSE`
- `enterprise/LICENSE.md`
- `Dockerfile`
- `docker-compose.yml`
- `schema.prisma`
- `litellm/proxy/proxy_server.py`
- `litellm/proxy/route_llm_request.py`
- `litellm/proxy/auth/user_api_key_auth.py`
- `litellm/proxy/auth/auth_checks.py`
- `litellm/proxy/auth/budget_throttle.py`
- `litellm/proxy/spend_tracking/budget_reservation.py`
- `litellm/proxy/db/db_spend_update_writer.py`
- `litellm/proxy/db/db_transaction_queue/spend_update_queue.py`
- `litellm/proxy/hooks/parallel_request_limiter.py`
- `litellm/proxy/hooks/dynamic_rate_limiter.py`
- `litellm/proxy/common_utils/encrypt_decrypt_utils.py`
- `litellm/proxy/health_endpoints/_health_endpoints.py`
- `litellm/router.py`
- `litellm/router_strategy/`
- `litellm/llms/base_llm/chat/transformation.py`
- `litellm/llms/custom_httpx/llm_http_handler.py`
- `litellm/llms/openai/`
- `litellm/llms/azure/`
- `litellm/llms/bedrock/`
- `litellm/llms/gemini/`
- `litellm/llms/deepseek/`
- `litellm/integrations/prometheus.py`
- `litellm/integrations/SlackAlerting/slack_alerting.py`
- `litellm/types/integrations/slack_alerting.py`
- `ui/litellm-dashboard/package.json`

## 27. 许可证与复用提醒

仓库根许可证说明：

- `enterprise/` 目录之外为 MIT；
- `enterprise/` 使用独立许可证。

如果 Heimdall 只参考设计思想并用 Go 独立实现，许可证风险较低。

如果复制具体实现：

1. 必须确认文件不位于 `enterprise/`；
2. 保留 MIT 版权和许可证声明；
3. 不要从构建产物反向复制企业功能；
4. 对混合 OSS/Enterprise import 路径做单独审查；
5. Provider 特殊协议还需遵守相应平台条款。

## 28. 分析限制

本报告基于指定 commit 的本地静态分析，没有：

- 启动完整 PostgreSQL/Redis/Proxy 环境；
- 执行全部测试；
- 对真实 Provider 发起计费请求；
- 对 Enterprise 功能做许可证覆盖外的运行验证；
- 进行动态压力测试或安全渗透测试。

因此：

- 能力判断以源码、Schema、配置和测试资产为依据；
- 性能、稳定性和具体 UI 行为未做运行时背书；
- 上游迭代很快，实施 Heimdall 时应以自身不变量为准，不能依赖 LiteLLM 当前内部细节。
