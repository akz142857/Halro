# Heimdall 使用手册

本文面向本地体验、管理员配置和应用接入。生产部署、升级、备份恢复与安全加固请同时阅读 [Operator Guide](operator-guide.md)。

英文版见 [user-guide.md](user-guide.md)。

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

首次启动只需要：

```bash
make start
```

该命令会按需安装前端依赖、构建 `bin/heimdall`、创建只监听本机回环地址的
`config.yaml`、初始化本地加密存储并启动服务。React 静态资源嵌入二进制，
运行时不需要单独启动前端服务。

### 2.2 在页面中完成首次初始化

终端会显示 Admin 地址：

```text
Admin: http://127.0.0.1:8081/admin/setup
```

打开页面并设置管理员用户名和至少 8 个字符的密码，建议使用易记的长密码短语。密码只以 Argon2id 哈希
保存在本机元数据中，不会写入 `config.yaml`。成功后初始化入口永久关闭，
浏览器会自动创建安全会话并进入控制台。

兼容性说明：管理员密码下限由旧规则的 12 个 UTF-8 字节调整为 8 个
Unicode 码点。这是明确的产品策略变更，不是等价重构：纯 ASCII 密码的
最低字符数从 12 降为 8，而中文等多字节密码不再因编码字节数获得更低的
字符要求。生产环境仍建议使用明显长于最低限制的密码短语。

自动初始化会生成：

- `master.key`：本机 Master Key，权限为 `0600`；
- `data/heimdall.db`：元数据；
- `data/ledger/`：权威用量账本；
- `data/audit/`：审计链；
- 后续生成的 Usage checkpoint 与 Parquet 数据。

Master Key 必须与数据目录分开备份。丢失 Master Key 后，Provider Credential 无法恢复。
重复执行 `make start` 不会覆盖配置、Master Key 或数据。如果只剩 Master Key 或
只剩元数据等残缺状态，Heimdall 会拒绝自动修复并要求人工恢复匹配的文件。

如果 Admin 通过 TLS 监听非回环地址，启动终端还会显示一次性 Setup Token，
页面必须同时提交该 Token。它只保存在当前进程内，重启后自动轮换。

### 2.3 后续启动

以后仍然使用同一条命令：

```bash
make start
```

系统检测到管理员已经存在后会显示正常登录页。停止服务按 `Ctrl+C`。

### 2.4 外观与界面语言

首次初始化页和登录页右上角可以直接选择语言。Admin 当前完整支持简体中文与
English，切换后无需刷新页面。

登录后进入 **设置 → 通用**，可以配置当前管理员的 **外观** 与 **界面语言**：

- **外观**：选择浅色或深色，选择后立即应用；保存失败会恢复服务端已确认的值并提供重试；
- **我的语言**：保存到当前管理员账号，在其他浏览器登录同一账号时继续生效；
- **实例默认语言**：用于未登录页面，以及选择“跟随实例默认语言”的管理员。

外观默认深色，只提供浅色与深色两项。它和管理员语言偏好一起保存到服务端，在新
浏览器或新 Session 登录同一账号后恢复；退出登录、登录页和首次初始化页始终使用
深色。外观不写入 `localStorage`、`sessionStorage`、Cookie 或 IndexedDB。

管理员个人偏好与实例默认语言分别保存，因此某一个请求失败不会造成另一个设置显示
为失败。语言解析顺序为管理员偏好、实例默认语言、浏览器语言、内置简体中文。实例
默认语言属于全局设置，修改会写入 Audit；管理员偏好也有独立 Revision，并使用 `If-Match` 防止
多个页面相互覆盖。Gateway API、Provider 模型名称、错误码和审计枚举等协议字段
不会在协议层被翻译。语言偏好只保存在服务端元数据中，不写入浏览器持久化存储。

### 2.5 Headless 与自动化部署

```bash
./bin/heimdall init --config ./configs/config.example.yaml
printf '%s' "$ADMIN_PASSWORD" | ./bin/heimdall admin bootstrap \
  --config ./configs/config.example.yaml --username admin
./bin/heimdall serve --config ./configs/config.example.yaml
```

这些离线命令继续用于无浏览器服务器、CI、自动化部署与紧急密码恢复，运行时
持有数据目录独占锁，因此必须在服务停止时执行。

默认地址：

| 服务 | 地址 | 用途 |
|---|---|---|
| Admin | `http://127.0.0.1:8081/admin` | 管理后台 |
| Gateway | `http://127.0.0.1:8080` | OpenAI Compatible API |
| Metrics | `http://127.0.0.1:9090/metrics` | Prometheus 指标 |

登录 Admin 使用首次初始化页面中设置的用户名和密码。

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
  --billing-mode metered \
  --input-micros-per-million "$INPUT_MICROS_PER_MILLION" \
  --output-micros-per-million "$OUTPUT_MICROS_PER_MILLION" \
  --daily-budget-micros-usd 5000000
unset OPENAI_API_KEY
```

`5000000` micro-USD 等于每日 5 USD。命令返回的 `gateway_key` 只显示一次，必须立即保存到 Secret Manager，不要放入聊天、源码、日志或浏览器持久存储。

Bootstrap 不会把两个零价字段自动解释为免费。只有确认该 Deployment 永久或按合同免费时才使用 `--billing-mode free`；计费模型必须传入经过核对的 micro-USD 单价。Bootstrap 会把这些条款原子保存为 Price Version 1。

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
| Bedrock Runtime | `https://bedrock-runtime.<region>.amazonaws.com` | Beta，Converse 文本，显式静态 AWS Credential |
| Bedrock Mantle | `https://bedrock-mantle.<region>.api.aws` | Beta，可选择 OpenAI Chat、无状态 Responses 或 Anthropic Messages |

Bedrock Credential 是一个 JSON Secret：

```json
{"access_key_id":"...","secret_access_key":"...","session_token":"...","region":"us-east-1"}
```

`session_token` 可省略，`region` 必须与 endpoint 一致。系统不会访问 IMDS，也不会读取宿主机的默认 AWS Credential Chain。

Mantle Credential 直接保存 Bedrock API Key，不使用上述 JSON。创建凭据时选择 Mantle 访问面，
再为所需协议分别创建 Provider；一个 Provider 只绑定一个 Profile。Mantle Responses 始终以
`store:false` 调用 AWS，不创建 Heimdall 无法管理的 30 天存储状态。Runtime 与 Mantle 的凭据、
并发上限和能力证据相互隔离。

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

Bedrock Runtime 的 `bedrock.runtime.invoke.titan-embed-text-v2.v1` Profile 固定使用
`amazon.titan-embed-text-v2:0`，当前只接受单个字符串、Float 输出和 256/512/1024 维。
数组、Token 数组、`base64`、`user` 与其他维度会在访问 AWS 前拒绝；需要批量时请由客户端逐条
调用并自行控制并发，不要假设 Heimdall 会做隐藏 Fan-out。

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

Deployment 与价格时间线分开管理。至少创建一个已经生效的 Price Version；输入、输出价格单位为 `USD / 1M tokens`，固定价格单位为 `USD / request`。历史 Attempt 会保存当时的完整价格快照，后续调价不会重算旧消费。没有有效价格默认返回 `409 price_unavailable`，不会再显示成已知 `$0.00`；只有显式 `free` 版本才表示已知零成本。

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
- **Settings**：浅色/深色外观、界面语言、可热更新的运行参数和管理员密码；
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

完整指标说明见 [Metrics reference](../contracts/metrics-reference.md)。指标标签刻意排除 Project、Key、Request ID、原始模型和 IP 等高基数或敏感数据。

生产环境应配置 `metrics.credential_file`，使用以下命令独立轮换和吊销指标凭据，而不旋转 Master Key：

```bash
./bin/heimdall metrics rotate --config ./config.yaml --overlap 10m
./bin/heimdall metrics list --config ./config.yaml
./bin/heimdall metrics revoke --config ./config.yaml --version 1
```

`rotate` 只在标准输出显示一次新 Token，应直接写入 Secret 文件，不能进入 shell history、环境变量或日志。非回环 Metrics listener 还必须配置 `metrics.tls` 双向 TLS 和 Client CA。

## 9. Key 生命周期

- Provider Secret 只通过 Credential 管理，密文使用 audience-bound AEAD；
- Gateway Key 以 `gw_` 开头，只存储 SHA-256 表示；
- Gateway Key 创建后只显示一次；
- 怀疑泄露时立即在 Projects 页面禁用旧 Key 并创建新 Key；
- 不要在停用前删除仍被业务使用的唯一 Key；
- Admin 修改密码后会轮换 Session/CSRF 并使旧 Session 失效。
- 在“设置 → 安全”中可以添加多个相互独立的身份验证器，兼容 Microsoft Authenticator、Google Authenticator、1Password 等标准 TOTP 应用。每个验证器可以单独撤销，任意一个有效验证码都能完成登录。
- 首次启用时会生成 10 个只展示一次的恢复码。恢复码使用后立即失效，请离线保存，不要与管理员密码存放在一起。
- 验证码依赖准确时间；如果持续失败，请检查服务器和手机的自动时间同步。

服务停止时，也可以使用离线命令创建或禁用内部 Key：

```bash
./bin/heimdall key create --config ./configs/config.example.yaml \
  --project-id prj_... --name team-a

./bin/heimdall key disable --config ./configs/config.example.yaml \
  --key-id key_...
```

## 10. Phase 2 媒体与资源接口

管理端可为 OpenAI 选择独立的“媒体与资源”Profile，并为 Bedrock 分别选择 Titan Image、
Cohere Rerank 或 Nova Reel Async Profile。不要把多个协议能力合并到同一个 Provider。
Files、Batches 与 Async 创建请求必须携带 `Idempotency-Key`；上传文件还必须携带
`Heimdall-Route`。资源 ID 只在创建它的 Project 内可见，查询和删除始终回到原 Provider、
Deployment、Profile 与 Region。Bedrock 异步任务当前不能取消；接口会明确返回
`provider_cancel_unsupported`，而不是显示虚假的成功状态。

Price Version 的“每请求固定 USD”用于媒体、重排和资源操作；请按上游价格配置。价格未知时
默认在 Provider I/O 前拒绝，而不是写入最小保守占位。文件内容保存在数据目录的私有对象目录，并随删除或 TTL 回收清理。

## 11. 备份与恢复

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

## 12. 诊断和常见问题

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
| `409 price_unavailable` | Deployment 是否存在已生效 Price Version；unknown 是否被预算或成本型 Token Guard 禁止 |
| `429` | Project RPM/TPM/并发、Provider/Deployment 并发、Token Guard 或上游限流 |
| `502 provider_error` | Provider endpoint、Credential、模型名称、网络和连接测试 |
| Dashboard 出现 `EST.` | Provider Usage 缺失或调用结果不明确；不是已确认的真实消耗 |
| Cost 为 `$0.00` | Price Version 是否明确配置为 `free`；未知价格不会计入已知成本合计 |
| 数据目录 locked | 另一个 Heimdall 或离线命令正在持有数据目录；不要手工删除锁文件绕过 |
| Readiness 失败 | 先检查 Accounting、pricing quarantine、WAL append error、磁盘空间和 Usage lag |

### 12.1 价格版本、历史成本与价格建议

- Deployment 的价格采用不可变版本和生效时间；修改当前价格会创建新版本，不会重算旧请求。
- 每次 Provider Attempt 都在调用上游前绑定价格快照。Usage 中的原始成本、调整额和最终成本可展开查看；未知成本显示为空而不是 `$0`。
- 历史错价通过追加“成本调整”纠正。原始 Settlement 不会被修改，并同时保留服务期与调整入账期口径。
- “价格建议”是独立的待评审 Proposal。LLM 或导入工具只能提交带来源 digest、模型/地区匹配、警告和到期时间的建议，不能自动改价。
- 管理员核验来源后，通过当前密码（已启用 MFA 时还需 TOTP）重新认证并显式采纳；系统随后创建新的不可变 Price Version 并写入 Audit。歧义或已过期建议不能采纳。
- 恢复旧备份后若某个 scheduled 价格已经越过生效时间，Deployment 会进入 pricing quarantine；管理员复核并确认前不会恢复流量。

## 13. 安全注意事项

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
- [Usage storage](../contracts/usage-storage.md)
- [Metrics reference](../contracts/metrics-reference.md)
- [Webhook payloads](../contracts/webhook-payloads.md)
- [Token Guard EWMA](../architecture/token-guard-ewma.md)
- [Threat model](../architecture/threat-model.md)
