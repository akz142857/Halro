# 上游拒绝形态取证矩阵（#319）

Route eligibility（[设计](../todo/route-eligibility-design.zh-CN.md)，Epic #318）押在
`FailureReason` 准确上。今天它从一小把字符串码和状态码推出来（`internal/gateway/failure.go:126-150`），
而**全仓对"厂商如何表达额度/权益问题"只有一条厂商级依据**：Kimi Code 的 402
（`internal/provider/openai/adapter.go:1034-1044`，且该 profile 是 withheld 的）。

本文是这条空白的记录处。它回答四个问题，逐家逐格填：

1. 额度耗尽长什么样（状态码、`code`/`type` 字段、有无 `Retry-After`）；
2. **它与普通限流是否同码**——这一条决定 Phase 2 要不要把 `FailureReason` 引进路由路径。
   若每家都用不同状态码，状态码本身就够判，设计可以更便宜；**只要有一家把两者压在同一个码上，
   基于 reason 的路由就是必需的**；
3. 额度是 per-credential 还是 per-model（决定 scope 宽窄，取不到证据按窄的来）；
4. `Retry-After` 给不给、给的值可不可信（假值比不给更糟）。

与 [`provider-real-matrix.md`](provider-real-matrix.md) 的分工：那条通道回答"这个 profile 能不能
正常服务"，是发版门禁；本文回答"它拒绝的时候说了什么"，是 Phase 2 定参数的依据。两者共用真实账号，
但本文的调用多数是**故意打失败**，不进发版证据包。

## 证据等级

| 等级 | 含义 |
|---|---|
| **A** | 本仓库对真实账号实测，记录在案（文件:行号 或本文 §3 的执行记录） |
| **B** | 上游官方文档 |
| **C** | 第三方报告（用户 issue、社区帖），只作线索，不作结论 |
| **—** | 无任何依据。**"Halro 没枚举过"是关于 Halro 的事实，不是上游的答案** |

## 1. 要打的上游

Bedrock Runtime 五行 withheld，不在内；Kimi Code（`api.kimi.com`）同样 withheld，
而仓库唯一那条 402 依据恰恰来自它——**已提供的 Kimi 面（`api.moonshot.ai`）一条证据都没有**。

| # | 厂商 / profile | Base URL | 路径 | 认证头 |
|---|---|---|---|---|
| 1 | OpenAI chat / responses | `https://api.openai.com` | `/v1/chat/completions`、`/v1/responses` | `Authorization: Bearer` |
| 2 | Anthropic messages | `https://api.anthropic.com` | `/v1/messages` | `x-api-key` + `anthropic-version: 2023-06-01` |
| 3 | Azure OpenAI | `https://<resource>.openai.azure.com` | `/openai/deployments/<dep>/chat/completions?api-version=<pinned>` | `api-key` |
| 4 | DeepSeek | `https://api.deepseek.com` | `/v1/chat/completions` | `Authorization: Bearer` |
| 5 | MiniMax（三面同 host） | `https://api.minimax.io` | `/v1/chat/completions`、`/v1/responses`、`/anthropic/v1/messages` | `Bearer`（Anthropic 面另收 `x-api-key`） |
| 6 | Kimi / Moonshot | `https://api.moonshot.ai` | `/v1/chat/completions`、`/anthropic/v1/messages` | `Bearer` 仅此一种 |
| 7 | BigModel 国内 / 全球 | `https://open.bigmodel.cn`、`https://api.z.ai` | `/api/paas/v4/chat/completions`；Coding Plan 走 `/api/coding/paas/v4/...` | `Authorization: Bearer` |
| 8 | Gemini | `https://generativelanguage.googleapis.com` | `/v1beta/models/<model>:generateContent` | `x-goog-api-key` |
| 9 | Bedrock Mantle | `https://bedrock-mantle.<region>.api.aws` | 三条 Mantle 路由（`internal/app/providers.go:883` 的前缀） | `Authorization: Bearer` |

## 2. 每家要打的四种情形

| 情形 | 怎么造 | 记什么 |
|---|---|---|
| **A 额度耗尽** | 零余额/未充值的 key；或已用完套餐窗口的 key | 状态码、`code`/`type`、`Retry-After` |
| **B 普通限流** | 正常 key 并发猛打同一模型 | 同上，**与 A 对比是否同码** |
| **C 凭证失效** | 把 key 改掉一位 | 401 还是 403 |
| **D 订阅未开通** | 有 key 但未买该模型/套餐 | 402？403？（Kimi Code 的 402 属此类） |

A 命中后再叠两问确定 scope 宽窄：**同 key 换一个模型**再打（per-credential 还是 per-model）、
**换一个 key 打同模型**（是否账号级）。

统一模板，响应头必须留（`Retry-After` 在头里）：

```bash
curl -sS -D - -o body.json -w '\n%{http_code} %{time_total}s\n' \
  -X POST https://api.deepseek.com/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}'
```

**不要记进本文**：API key、请求体与响应体原文、request id。只记状态码、`code`/`type` 的枚举值、
`Retry-After` 的有无与量级。这与 CLAUDE.md 的"日志/错误/指标/审计里不留上游响应字节"是同一条约束。

跑之前先看一眼 `halro_provider_failure_reason_total{reason, provider_status}`：
`{reason="unclassified", provider_status="500"}` 这类组合就是发现信号——Halro 没读懂的拒绝，
以及它当时看到的状态码。

## 3. 记录

### 3.1 矩阵

`—` = 未取证。填入时标证据等级。

| 厂商 | A 额度耗尽 | B 普通限流 | A/B 同码？ | C 凭证失效 | D 订阅未开通 | `Retry-After` | scope |
|---|---|---|---|---|---|---|---|
| OpenAI | — | — | — | — | — | — | — |
| Anthropic | — | — | — | — | — | — | — |
| Azure OpenAI | — | — | — | — | — | — | — |
| DeepSeek | — | — | — | — | — | — | — |
| MiniMax | **C**：`500` + `{"type":"error","error":{"type":"api_error","message":"insufficient balance (1008)"}}`（Anthropic 面）<br>**B**：码表 1008 = insufficient balance，2056 = usage limit exceeded | **B**：码表 1002 = rate limit | 否（按 C/B：1008 与 1002 是不同码），但**两者的 HTTP 状态均未实测** | **A**：`401` + `{"type":"error"}`，两张兼容面一致 | — | **B**：官方码表不写 HTTP 状态，也不写 `Retry-After` | — |
| Kimi / Moonshot | — | — | — | — | — | — | — |
| BigModel | — | — | — | — | — | — | — |
| Gemini | — | — | — | — | — | — | — |
| Bedrock Mantle | — | — | — | — | — | — | — |

### 3.2 MiniMax：已知的三条，以及它们今天在 Halro 里的下场

**已被证伪的一条**（2026-08-31 实测，`docs/prd/minimax-adaptation-plan.zh-CN.md` §1.3、
`docs/review/260901/adversarial-verdicts.md` V4）：两张兼容面对 1004 回的是规矩的 **401 +
`{"type":"error"}`**，`200` + `base_resp` 只出现在 `/v1/embeddings`，而 Halro 不声明 MiniMax 的
embeddings 能力。**"MiniMax 用 200 包错误"对 Halro 实际会调的路由不成立。**

**未实测的一条（等级 C，需要一次真实 1008 来定级）**：三份独立公开报告（2025-12 至 2026-01，
全是 Anthropic 面）给出同一形态 `HTTP 500` + `insufficient balance (1008)`。顺代码走一遍：

| 环节 | 结果 |
|---|---|
| `internal/provider/anthropic/adapter.go:797-803` | `status >= 500` → `Provider5xx`、`Ambiguous`，`FailureReason` 为空 |
| `internal/routegate/gate.go:129-140` | 无 reason → `availabilityPolicy`，deployment 级，**连败 5 次**才挂 30 s |
| `internal/gateway/service.go:2690-2700` | `walkOn` 对 `Ambiguous` 返回 false → **不切下一家候选** |
| 计费 | 按估算保守提交 |

即 MiniMax 没钱了会被当成"上游偶发 500"：逐个请求 500 回去、逐个估算计费、不回退。
指标上表现为 `{reason="unclassified", provider_status="500"}`。

**码表里 `classifyMiniMaxStatus`（`internal/provider/openai/minimax.go:67-106`）没列的码**
（等级 B）：`2056` usage limit exceeded（Token Plan 的 5 小时窗口，是**第二种**额度语义）、
`2045` rate growth limit、`1041` conn limit、`2049` invalid API Key。现在全落 default → 5xx
ambiguous。OpenAI 面的 1008 形态无任何证据。

来源：[官方错误码表](https://platform.minimax.io/docs/api-reference/errorcode)、
[MiniMax-M2 #62](https://github.com/MiniMax-AI/MiniMax-M2/issues/62)、
[kilocode #5047](https://github.com/Kilo-Org/kilocode/issues/5047)。

### 3.3 429 今天被无条件判成限流

`internal/gateway/failure.go:145` 把 429 无条件映射到 `rate_limited`，`insufficient_quota`
在全仓零命中。任何用 429 承载额度耗尽的厂商，现在会按 `rate_limited` 策略
（`internal/routegate/policy.go:97`：credential×model、1 s 起、翻倍至 60 s）处理，
**永远不会升级为 `subscription_quota_exhausted`，也不会触发 `HalroProviderQuotaExhausted`**。
OpenAI 是否如此，正是 §3.1 第一行 A/B 两格要回答的。

（#324 已把 `internal/circuit` 从 gateway 移走，`availabilityFailure` 不复存在；
早期描述中"429 把断路器按住不开"的那半句已随之过时，分类这半句仍然成立。）

## 4. 取证之后

把观察到的矩阵并回 reason 分类表，**然后才**冻结设计 §4.3 的窗口与阈值——
`DefaultPolicies()` 里的数字自带注释说明它们是起点而非结论。
