# Heimdall 使用手册

本文面向本地体验、管理员配置和应用接入。生产部署、升级、备份恢复与安全加固请同时阅读 [Operator Guide](operator-guide.md)。

## 1. 系统中的几个核心对象

一次请求从内部 Gateway Key 到真实模型的关系如下：

```text
Application
  └─ Gateway Key
      └─ Project（预算、RPM、TPM、并发、允许模型、Policy）
          └─ Route（公开模型别名，例如 chat）
              └─ Deployment（上游模型、价格、能力、并发）
                  └─ Provider（上游地址、类型）
                      └─ Credential（加密保存的平台 Secret）
```

应用只接触 `gw_...` Gateway Key 和公开模型别名，不接触 Provider API Key，也不需要知道上游真实模型名称。

## 2. 本地快速启动

### 2.1 环境要求

- Go 1.26.5 或更高版本；
- Node.js 与 npm，仅在从源码构建 React 管理后台时需要；
- macOS、Linux，或项目支持的其他 Go 目标平台。

获取源码并进入项目目录：

```bash
git clone https://github.com/akz142857/Heimdall.git
cd Heimdall
```

构建前端和单二进制：

```bash
make build
```

构建结果为 `bin/heimdall`。React 静态资源已经嵌入二进制，运行时不需要 Node.js，也不需要单独启动前端服务。

### 2.2 初始化数据目录

示例配置默认只监听本机回环地址：

```bash
./bin/heimdall config check --config ./configs/config.example.yaml
./bin/heimdall init --config ./configs/config.example.yaml
```

初始化会生成：

- `master.key`：本机 Master Key，权限为 `0600`；
- `data/heimdall.db`：元数据；
- `data/ledger/`：权威用量账本；
- `data/audit/`：审计链；
- 后续生成的 Usage checkpoint 与 Parquet 数据。

Master Key 必须与数据目录分开备份。丢失 Master Key 后，Provider Credential 无法恢复。

### 2.3 创建管理员

管理员密码至少 12 字节。使用标准输入传递，不要把密码直接写进命令参数：

```bash
read -r -s ADMIN_PASSWORD
printf '\n'
printf '%s' "$ADMIN_PASSWORD" | ./bin/heimdall admin bootstrap \
  --config ./configs/config.example.yaml \
  --username admin
unset ADMIN_PASSWORD
```

### 2.4 启动服务

```bash
./bin/heimdall serve --config ./configs/config.example.yaml
```

默认地址：

| 服务 | 地址 | 用途 |
|---|---|---|
| Admin | `http://127.0.0.1:8081/admin` | 管理后台 |
| Gateway | `http://127.0.0.1:8080` | OpenAI Compatible API |
| Metrics | `http://127.0.0.1:9090/metrics` | Prometheus 指标 |

登录 Admin 使用用户名 `admin` 和上一步设置的密码。停止服务按 `Ctrl+C`。

## 3. 配置第一个模型

有两种方式。快速体验可使用离线 Bootstrap；长期维护建议使用 Web UI。

### 3.1 一条命令创建完整 OpenAI 链路

此命令必须在服务停止时执行。它会原子创建 Credential、Provider、Deployment、Route、Project 和一个 Gateway Key：

```bash
read -r -s OPENAI_API_KEY
printf '\n'
printf '%s' "$OPENAI_API_KEY" | ./bin/heimdall bootstrap \
  --config ./configs/config.example.yaml \
  --provider-type openai \
  --provider-base-url https://api.openai.com \
  --provider-model gpt-5-mini \
  --public-model chat \
  --daily-budget-micros-usd 5000000
unset OPENAI_API_KEY
```

`5000000` micro-USD 等于每日 5 USD。命令返回的 `gateway_key` 只显示一次，必须立即保存到 Secret Manager，不要放入聊天、源码、日志或浏览器持久存储。

完成后重新执行 `serve`。

### 3.2 使用 Web UI 完整配置

按照以下顺序操作：

1. **Providers → 凭据**：选择 Provider 类型并录入 API Key。Secret 使用 AES-GCM 加密，之后只显示“已配置”，不会回显明文。
2. **Providers → Provider**：选择类型、Base URL、Credential 和能力上限，然后执行连接测试。
3. **Deployments**：填写真实上游模型名称、能力子集、并发上限和每百万 Token 的输入/输出美元价格。
4. **Routes**：创建公开模型别名，例如 `chat`，绑定 Deployment，并选择 ordered fallback 或 round robin。
5. **Projects**：配置允许的模型别名、RPM、TPM、最大并发、每日预算、CIDR 和安全 Policy。
6. **Projects → Keys**：为应用创建 Gateway Key。Key 只显示一次；确认已经安全保存后关闭弹窗。

Provider 能力是上限，Deployment 能力只能是 Provider 能力的子集。Route 中使用的是公开别名；SDK 请求不应使用真实 Provider 模型名称。

### 3.3 Provider 基础参数

| 类型 | Base URL 示例 | 说明 |
|---|---|---|
| OpenAI | `https://api.openai.com` | GA；chat、stream、embeddings |
| Azure OpenAI | Azure resource endpoint | 必须显式配置 API Version |
| DeepSeek | `https://api.deepseek.com` | GA；默认不声明 embeddings |
| OpenAI Compatible | 已审核的 HTTPS 地址 | 按实际平台声明能力 |
| Gemini | `https://generativelanguage.googleapis.com` | Beta，原生适配器 |
| Bedrock | `https://bedrock-runtime.<region>.amazonaws.com` | Beta，显式静态 AWS Credential |

Bedrock Credential 是一个 JSON Secret：

```json
{"access_key_id":"...","secret_access_key":"...","session_token":"...","region":"us-east-1"}
```

`session_token` 可省略，`region` 必须与 endpoint 一致。系统不会访问 IMDS，也不会读取宿主机的默认 AWS Credential Chain。

## 4. 调用 Gateway

### 4.1 安全设置 Gateway Key

不要把 Key 直接写在 shell history 中：

```bash
read -r -s HEIMDALL_GATEWAY_KEY
printf '\n'
export HEIMDALL_GATEWAY_KEY
```

使用结束后清除：

```bash
unset HEIMDALL_GATEWAY_KEY
```

### 4.2 curl 非流式请求

建议显式指定 `max_completion_tokens`。它既限制上游输出，也限制网络结果不明确时的保守预算上界：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $HEIMDALL_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chat",
    "max_completion_tokens": 256,
    "messages": [
      {"role": "user", "content": "你好，请用一句话介绍 Heimdall"}
    ]
  }'
```

### 4.3 curl 流式请求

```bash
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $HEIMDALL_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chat",
    "stream": true,
    "stream_options": {"include_usage": true},
    "max_completion_tokens": 256,
    "messages": [
      {"role": "user", "content": "列出三个 LLM Gateway 的安全能力"}
    ]
  }'
```

流使用标准 SSE，并以 `data: [DONE]` 结束。首个内容事件发出前允许安全重试或 fallback；开始向客户端发送内容后不会切换到另一个 Provider，以免拼接两份回答。

### 4.4 Embeddings

需要 Route 对应的 Deployment 声明 embeddings 能力，并使用正确的 embedding 上游模型：

```bash
curl http://127.0.0.1:8080/v1/embeddings \
  -H "Authorization: Bearer $HEIMDALL_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"embedding","input":"Heimdall Gateway"}'
```

通常应为 Chat 与 Embedding 创建不同 Deployment 和公开 Route。

### 4.5 Python OpenAI SDK

```python
import os
from openai import OpenAI

client = OpenAI(
    api_key=os.environ["HEIMDALL_GATEWAY_KEY"],
    base_url="http://127.0.0.1:8080/v1",
    max_retries=0,
)

response = client.chat.completions.create(
    model="chat",
    max_completion_tokens=256,
    messages=[{"role": "user", "content": "你好"}],
)
print(response.choices[0].message.content)
```

### 4.6 Node.js OpenAI SDK

```javascript
import OpenAI from "openai";

const client = new OpenAI({
  apiKey: process.env.HEIMDALL_GATEWAY_KEY,
  baseURL: "http://127.0.0.1:8080/v1",
  maxRetries: 0,
});

const response = await client.chat.completions.create({
  model: "chat",
  max_completion_tokens: 256,
  messages: [{ role: "user", content: "你好" }],
});
console.log(response.choices[0].message.content);
```

是否让 SDK 自动重试应由应用明确决定。Gateway 自身已经执行有界重试、Fallback、熔断和记账；叠加无界 SDK 重试可能放大成本。

## 5. Project、预算与限流

每个 Project 可以独立设置：

- `Allowed Models`：允许访问的公开 Route 别名；
- `Daily Budget`：按照 `usage.timezone` 的自然日执行；
- `RPM`：每分钟请求数；
- `TPM`：每分钟 Token；
- `Max Concurrency`：项目并发请求；
- `Allowed CIDRs`：来源网络边界；
- Token Guard Policy；
- Redaction Policy。

Deployment 中必须填写正确价格，否则成本会显示 `$0.00`，预算也无法体现真实美元成本。Web UI 的价格单位是 `USD / 1M tokens`。

网络超时或连接中断可能导致“Provider 是否已经处理请求”无法确定。Heimdall 会按请求允许的最大输出 Token 做保守结算，并标记为 `estimated`。Dashboard 主 Token 数只显示 Provider 报告量，估算上界单独显示；Usage 页面使用 `EST.` 标记。应用应合理设置 `max_completion_tokens` 和 Project 最大输出限制。

## 6. Token Guard 与脱敏

### 6.1 Token Guard

在 **Policies** 页面创建策略，再绑定到 Project。建议部署顺序：

1. 使用 `observe` 或 `alert` 收集正常基线；
2. 配置单请求 Token、每分钟 Token、成本、并发、错误率和来源 IP 硬阈值；
3. 确认误报可接受后再使用 `temporary_block`；
4. EWMA 只用于相对异常检测和告警，不会自动封禁。

临时封禁可在 Projects 页面由管理员手动解除。固定 RPM、TPM、预算和并发限制始终优先于实验性检测。

### 6.2 Redaction

Redaction Policy 支持内置 PII/Secret、RE2 规则和字典。策略可以：

- 拒绝包含敏感内容的请求；
- 对进入 Provider 的内容脱敏；
- 对 Provider 返回内容脱敏；
- 在流式输出中跨 chunk 检测敏感模式。

上线前应使用真实业务样本执行 Policy 测试，分别检查漏检和误杀。不要把生产 Secret 粘贴到聊天、Issue、测试快照或浏览器存储中。

## 7. Dashboard、Usage 与 Operations

- **Dashboard**：今日请求、Provider Attempt、Provider 报告 Token、成本、错误率、延迟和最近七天趋势；
- **Usage**：每次 Provider Attempt 的状态、Token、成本、延迟以及是否为保守估算；
- **Operations**：Alert 和 HMAC Audit Chain；
- **Settings**：可热更新的运行参数和管理员密码；
- **System Status**：账本、Usage watermark、队列与运行健康度。

一个客户端请求可能产生多个 Provider Attempt，例如重试或 fallback。请求数与 Attempt 数不同是正常现象；成本、Token 和错误率按 Attempt 记录。

## 8. Metrics

默认要求 Bearer Token。令牌由 Master Key 派生，不保存在 YAML：

```bash
./bin/heimdall metrics token --config ./configs/config.example.yaml
```

调用：

```bash
read -r -s HEIMDALL_METRICS_TOKEN
printf '\n'
curl http://127.0.0.1:9090/metrics \
  -H "Authorization: Bearer $HEIMDALL_METRICS_TOKEN"
unset HEIMDALL_METRICS_TOKEN
```

完整指标说明见 [Metrics reference](metrics-reference.md)。指标标签刻意排除 Project、Key、Request ID、原始模型和 IP 等高基数或敏感数据。

## 9. Key 生命周期

- Provider Secret 只通过 Credential 管理，密文使用 audience-bound AEAD；
- Gateway Key 以 `gw_` 开头，只存储 SHA-256 表示；
- Gateway Key 创建后只显示一次；
- 怀疑泄露时立即在 Projects 页面禁用旧 Key 并创建新 Key；
- 不要在停用前删除仍被业务使用的唯一 Key；
- Admin 修改密码后会轮换 Session/CSRF 并使旧 Session 失效。

服务停止时，也可以使用离线命令创建或禁用内部 Key：

```bash
./bin/heimdall key create --config ./configs/config.example.yaml \
  --project-id prj_... --name team-a

./bin/heimdall key disable --config ./configs/config.example.yaml \
  --key-id key_...
```

## 10. 备份与恢复

离线创建加密备份：

```bash
umask 077
openssl rand 32 > backup.key
./bin/heimdall backup create \
  --config ./configs/config.example.yaml \
  --output ./heimdall.hmbk \
  --key-file ./backup.key
```

验证：

```bash
./bin/heimdall backup verify \
  --file ./heimdall.hmbk \
  --key-file ./backup.key
```

备份不包含 Master Key。必须分别保管：

1. 加密备份文件；
2. Backup Key；
3. 创建该备份时使用的 Master Key。

恢复前必须停止服务，并严格按照 [Backup and restore](backup-restore.md) 操作。

## 11. 诊断和常见问题

服务停止后执行只读诊断：

```bash
./bin/heimdall doctor --config ./configs/config.example.yaml
```

常见错误：

| 现象 | 优先检查 |
|---|---|
| `401 invalid_api_key` | Gateway Key 是否完整、启用、过期或已轮换 |
| `403 model_not_allowed` | Project 的 Allowed Models 是否包含公开 Route 别名 |
| `403 budget_exceeded` | 每日预算、Deployment 价格和保守估算上界 |
| `429` | Project RPM/TPM/并发、Provider/Deployment 并发、Token Guard 或上游限流 |
| `502 provider_error` | Provider endpoint、Credential、模型名称、网络和连接测试 |
| Dashboard 出现 `EST.` | Provider Usage 缺失或调用结果不明确；不是已确认的真实消耗 |
| Cost 始终为 `$0.00` | Deployment 输入/输出价格尚未配置 |
| 数据目录 locked | 另一个 Heimdall 或离线命令正在持有数据目录；不要手工删除锁文件绕过 |
| Readiness 失败 | 先检查 Accounting、WAL append error、磁盘空间和 Usage lag |

## 12. 安全注意事项

- 示例配置仅监听 `127.0.0.1`；公网监听必须配置 TLS 和经过审核的反向代理边界；
- Admin 和 Metrics 不允许通过公网明文暴露；
- 不要在 URL query、命令参数、Git、日志或聊天中传递任何 Secret；
- Provider 自定义 endpoint 和 Webhook 必须使用 HTTPS，并受 SafeTransport/SSRF 策略约束；
- 不要同时运行两个进程操作同一个数据目录；
- 修改 Master Key、恢复备份和离线 Key 命令前先停止服务；
- 定期验证 Audit Chain、备份、磁盘空间、WAL/Usage watermark 和告警投递。

更多运维资料：

- [Operator Guide](operator-guide.md)
- [Backup and restore](backup-restore.md)
- [Usage storage](usage-storage.md)
- [Metrics reference](metrics-reference.md)
- [Webhook payloads](webhook-payloads.md)
- [Token Guard EWMA](token-guard-ewma.md)
- [Threat model](threat-model.md)
