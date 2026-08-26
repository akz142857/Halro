# Halro 应用接入指南

本文面向**要通过 Halro 调用大模型的应用团队**。你不需要了解 Halro 的内部架构、
管理后台或运维流程；那些内容在 [使用手册](user-guide.zh-CN.md) 与
[Operator Guide](operator-guide.md) 中，由 Halro 管理员负责。

一句话概括：Halro 对外是一个 **OpenAI 兼容 / Anthropic 兼容的 HTTP API**。
把 SDK 的 `base_url` 指向 Halro，把 API Key 换成管理员发给你的 Gateway Key，
`model` 填公开别名，即完成接入。

## 1. 接入前你会拿到什么

向 Halro 管理员申请接入后，你会收到以下信息（见文末"接入信息表"模板）：

| 项目 | 示例 | 说明 |
|---|---|---|
| Base URL | `https://halro.example.internal` | 网关地址，所有 API 路径挂在其下 |
| Gateway Key | `gw_...` | 你的应用专属密钥，等价于 OpenAI 的 `api_key` |
| 模型别名 | `chat`、`embedding` | 请求里 `model` 字段填的值，**不是**上游真实模型名 |
| 限额 | 预算 / RPM / TPM / 并发 | 你的 Project 上配置的额度，超出会收到 403/429 |

你的应用**只接触 Gateway Key 和公开别名**：不接触上游 Provider 的 API Key，
也不需要知道别名背后路由到的真实模型。上游切换、路由、计费均由管理员在网关侧完成，
对你透明。

## 2. 认证

所有请求携带：

```
Authorization: Bearer gw_xxxxxxxx
```

Anthropic 兼容端点（`/v1/messages`）额外接受 Anthropic SDK 的习惯写法
`x-api-key: gw_xxxxxxxx`；两个头同时出现且值不一致会被拒绝。

Gateway Key 的保管规则：

- 只放在服务端环境变量或密钥管理系统中，**不写入代码、Git、日志、URL query、前端页面**；
- 一个应用（或一个环境）一把 Key，不要多个系统共用；
- Key 可能被管理员轮换或吊销，收到 `401 invalid_api_key` 时先与管理员确认 Key 状态。

## 3. 端点清单

以下端点已发布为 **compatible**（有版本化兼容性契约与 SDK 黑盒验证）：

| 端点 | 协议 |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/embeddings` | OpenAI Embeddings |
| `POST /v1/responses` | OpenAI Responses（仅无状态层，见 3.1） |
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1/messages/count_tokens` | Anthropic Token 计数 |

以下端点为 **experimental**，接入前请与管理员单独确认是否对你的 Project 开放、
以及当前的能力边界：`/v1/moderations`、`/v1/images/generations`、
`/v1/audio/transcriptions`、`/v1/audio/speech`、`/v1/files*`、`/v1/batches*`、
`/v1/rerank`、`/v1/async/invocations*`。其中 `/v1/rerank` 与
`/v1/async/invocations` 是 Halro 扩展接口，不是 OpenAI 端点，OpenAI SDK 没有
对应方法，需要直接发 HTTP 请求。

两条重要语义，和"尽力兼容"的网关不同：

- **不支持的字段会被拒绝，而不是被静默丢弃。** 请求里带了当前路由能力之外的
  字段会得到明确错误。收到这类错误时不要盲目重试，先精简请求。
- **能力跟着路由走。** 同一个端点，不同别名背后的 Deployment 能力不同
  （比如是否支持工具调用、视觉输入、某个 embedding 维度）。别名支持什么以
  管理员提供的接入信息为准。

### 3.1 Responses 端点是无状态的

`POST /v1/responses` 只提供显式无状态层：省略 `store` 视为 `store: false`，
所有有状态 / 资源引用字段（如 `previous_response_id`）会在请求阶段被拒绝。
需要多轮对话时由应用自行携带完整上下文。

## 4. 快速开始

以下示例假设 Base URL 为 `https://halro.example.internal`，别名为 `chat`。
建议总是显式设置 `max_completion_tokens`：它既限制上游输出，也是网络结果不明确时
预算保守估算的上界，不设会按更大的上界预留预算。

### curl

```bash
curl https://halro.example.internal/v1/chat/completions \
  -H "Authorization: Bearer $HALRO_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chat",
    "max_completion_tokens": 256,
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

### Python（OpenAI SDK）

```python
import os
from openai import OpenAI

client = OpenAI(
    api_key=os.environ["HALRO_GATEWAY_KEY"],
    base_url="https://halro.example.internal/v1",
    max_retries=0,
)

response = client.chat.completions.create(
    model="chat",
    max_completion_tokens=256,
    messages=[{"role": "user", "content": "你好"}],
)
print(response.choices[0].message.content)
```

### Node.js（OpenAI SDK）

```javascript
import OpenAI from "openai";

const client = new OpenAI({
  apiKey: process.env.HALRO_GATEWAY_KEY,
  baseURL: "https://halro.example.internal/v1",
  maxRetries: 0,
});

const response = await client.chat.completions.create({
  model: "chat",
  max_completion_tokens: 256,
  messages: [{ role: "user", content: "你好" }],
});
console.log(response.choices[0].message.content);
```

### Python（Anthropic SDK，走 /v1/messages）

```python
import os
from anthropic import Anthropic

client = Anthropic(
    api_key=os.environ["HALRO_GATEWAY_KEY"],
    base_url="https://halro.example.internal",
)

message = client.messages.create(
    model="chat",
    max_tokens=256,
    messages=[{"role": "user", "content": "你好"}],
)
print(message.content[0].text)
```

### Node.js（Anthropic SDK，走 /v1/messages）

```javascript
import Anthropic from "@anthropic-ai/sdk";

const client = new Anthropic({
  apiKey: process.env.HALRO_GATEWAY_KEY,
  baseURL: "https://halro.example.internal",
});

const message = await client.messages.create({
  model: "chat",
  max_tokens: 256,
  messages: [{ role: "user", content: "你好" }],
});
console.log(message.content[0].text);
```

注意两个 SDK 的 `base_url` 不同：OpenAI SDK 需要带 `/v1` 后缀，Anthropic SDK
只填根地址（SDK 自己拼 `/v1/messages`）。`anthropic-version` 请求头按 SDK
默认值发送即可。同一把 Gateway Key、同一个别名，两种协议都能用；选哪个 SDK
取决于你的应用已有的依赖，不影响背后路由到哪个上游。

### Embeddings

```bash
curl https://halro.example.internal/v1/embeddings \
  -H "Authorization: Bearer $HALRO_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model": "embedding", "input": "Halro Gateway"}'
```

Embedding 使用独立别名（背后是声明了 embeddings 能力的独立 Deployment）。
部分上游（如 Bedrock Titan V2）只接受单个字符串输入和固定维度；批量输入时
由应用逐条调用并自行控制并发，不要假设网关会做隐藏 fan-out。

## 5. 流式（SSE）约定

在请求中设置 `"stream": true`。流为标准 SSE，以 `data: [DONE]` 结束。
需要用量统计时加：

```json
"stream_options": {"include_usage": true}
```

语义保证：

- 首个内容字节发出**之前**，网关可能安全地重试或 fallback 到备用上游；
- 首个内容字节发出**之后**，绝不会切换 Provider——你收到的流一定来自同一个上游，
  不会出现两份回答拼接；
- 流中断按保守口径计费，不会因为中断而免费。

客户端请使用支持 SSE 的 HTTP 库读取（OpenAI / Anthropic SDK 的 stream 模式即可），
不要给流式请求设置过短的整体超时。

## 6. 限额、错误码与重试策略

你的 Project 受这些约束：每日预算、RPM、TPM、最大并发、允许的模型别名、
来源 IP CIDR，以及可能的内容策略（Token Guard / 脱敏）。触发时的返回：

| 状态码 / 错误 | 含义 | 应用侧处理 |
|---|---|---|
| `401 invalid_api_key` | Key 不完整、被禁用、过期或已轮换 | 不重试；核对 Key，联系管理员 |
| `403 model_not_allowed` | 别名不在 Project 的允许列表 | 不重试；核对别名，联系管理员 |
| `403 budget_exceeded` | 当日预算耗尽（或本次请求的保守估算超过余额） | 不重试；等预算日重置或申请调额 |
| `409 price_unavailable` | 网关侧价格未配置，拒绝产生未知成本 | 不重试；联系管理员 |
| `429` | RPM / TPM / 并发 / 上游限流 | 可退避重试；遵守 `Retry-After`（如有），用指数退避 + 抖动 |
| `502 provider_error` | 上游 Provider 出错 | 网关已做过有界重试和 fallback；应用侧最多再做少量退避重试 |

重试的总原则：**网关内部已经做了有界重试、fallback、熔断与记账**。应用侧
不要叠加无界的 SDK 自动重试（示例里 `max_retries=0` 就是这个原因）——
每次重试都是真实计费的请求，无界重试会放大你自己的预算消耗。确有需要时，
只对 `429` 和 `5xx` 做有上限（如 2–3 次）的指数退避重试；`4xx` 一律不重试。

另外注意：预算按**保守估算预留**。请求开始前会按 `max_completion_tokens`
上界预留额度，结算后释放差额；因此临近预算上限时，可能出现"看似还有余额
却被 403"的情况，这是预期行为。

## 7. Phase 2 资源接口的额外要求

如果你的 Project 被开放了 Files / Batches / Async 类接口：

- 创建类请求（上传文件、创建 batch、创建 async 任务）**必须**携带
  `Idempotency-Key` 请求头；上传文件还必须携带 `Halro-Route` 头指明目标别名；
- 资源 ID 只在创建它的 Project 内可见，跨 Project 不可见也不可猜测；
- Bedrock 异步任务当前不支持取消，取消接口会返回
  `provider_cancel_unsupported`，而不是伪装成功。

## 8. 禁止事项（会被网关拒绝或属于违规使用）

- 不要把 Gateway Key 暴露给浏览器、移动端或任何最终用户可触达的位置；
- 不要在 URL query 里传 Key；
- 不要绕过别名直接猜测 / 填写上游真实模型名——`model` 只接受公开别名；
- 不要用同一把 Key 服务多个互相隔离的业务（预算与审计将无法区分）；
- 不要依赖 experimental 端点的字段形状长期不变。

## 9. 接入信息表（由 Halro 管理员填写后随本文档发放）

```text
环境:            ☐ 测试  ☐ 生产
Base URL:        ______________________________
Gateway Key:     通过密钥管理系统单独交付，不随本文档传递
可用别名:
  - 别名: ________  用途: ________  能力: ☐ 流式 ☐ 工具调用 ☐ 视觉 ☐ JSON 对象模式 ☐ 结构化输出
  - 别名: ________  用途: ________  能力: ______________________________
限额:            预算 ____ USD/日   RPM ____   TPM ____   并发 ____
来源 IP 限制:    ______________________________
开放的 experimental 端点（如有）: ______________________________
联系人 / 告警渠道: ______________________________
```

## 参考

- [使用手册（管理员视角）](user-guide.zh-CN.md)
- [端点兼容性契约](../compatibility/README.md) 与
  [endpoint-manifests.json](../compatibility/endpoint-manifests.json)（机器可读的字段级契约）
- [ADR 0005：无状态 Responses Facade](../adr/0005-stateless-responses-facade.md)
