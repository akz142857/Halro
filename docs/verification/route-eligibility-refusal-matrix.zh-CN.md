# 上游拒绝形态取证矩阵（#319）

Route eligibility（[设计](../todo/route-eligibility-design.zh-CN.md)，Epic #318）押在
`FailureReason` 准确上。今天它从一小把字符串码和状态码推出来（`internal/gateway/failure.go:126-150`），
而**全仓对"厂商如何表达额度/权益问题"只有一条厂商级依据**：Kimi Code 的 402
（`internal/provider/openai/adapter.go:1034-1044`，且该 profile 是 withheld 的）。

本文是这条空白的记录处。它回答四个问题，逐家逐格填：

1. 额度耗尽长什么样（状态码、`code`/`type` 字段、有无 `Retry-After`）；
2. **它与普通限流是否同码**——这一条决定 Phase 2 要不要把 `FailureReason` 引进路由路径。
   若每家都用不同状态码，状态码本身就够判，设计可以更便宜；**只要有一家把两者压在同一个码上，
   基于 reason 的路由就是必需的**。**这一条已有答案，见 §3.0：九家里六家同码，答案是必需**；
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

### 3.0 核心问题已有答案：**耗尽与限流普遍同码，reason 必须进路由路径**

2026-09-20 扫官方文档得到的结论，等级 **B**：**九个入口里有六个把额度耗尽与普通限流压在
HTTP 429 上**，只能靠响应体里的 `code`/`type` 区分。Kimi 的文档把这件事说得最直白——
"429 is not a single cause. Check the `error.type` in the response first"。

这直接回答了 §0 的问题 2，答案是**不能只看状态码**，§8 阶段 2 选的 reason-based routing
是必需的而不是可选的更贵方案。

**对 Halro 的直接后果**：`internal/gateway/failure.go:145` 把 429 无条件映射成
`rate_limited`。对下面五家（OpenAI、Anthropic、Gemini、Kimi、BigModel），额度耗尽因此会拿到
`rate_limited` 策略（`internal/routegate/policy.go:97`：1 s 起、翻倍至 60 s、半开探针反复再撞），
**而那是一个不会在 60 秒内自愈的状态**。Anthropic 的文档还专门点出这一种 429
"has no `retry-after` header and keeps failing until access resumes"。

### 3.1 矩阵

`—` = 未取证。当前所有非空格子均为等级 **B（官方文档）**，无一为 A；C 的来源单独标注。

| 厂商 | A 额度耗尽 | B 普通限流 | **A/B 同码？** | C 凭证失效 | `Retry-After` |
|---|---|---|---|---|---|
| **OpenAI** | `429` `credit_balance_exhausted`；另有 `organization_spend_limit_exceeded` / `project_spend_limit_exceeded` | `429` 速率类 code | **是** | `401` | 限流类给；计费类给了也无用（充值前不会好） |
| **Anthropic** | `402` `billing_error`（付款问题）**与** `429` `rate_limit_error`（月度消费上限） | `429` `rate_limit_error` | **是**（消费上限与限流同为 429 同 type） | `401` `authentication_error`；`403` `permission_error` | 限流给；**消费上限那种明确不给**，且会持续失败 |
| **Azure OpenAI** | — | `429` | — （文档未区分配额耗尽与 TPM/RPM 限流） | `401` | `retry-after` / `retry-after-ms` |
| **DeepSeek** | **`402`** Insufficient Balance | **`429`** Rate Limit Reached | **否** | `401` | 文档未提 |
| **MiniMax** | 体内 `1008`；HTTP 状态**未实测**（等级 C 的三份报告指向 `500`） | 体内 `1002` | 体内码不同；**HTTP 状态未知** | **A**：`401` + `{"type":"error"}`，两面一致 | 文档不写 |
| **Kimi / Moonshot** | `429` `exceeded_current_quota_error`（余额不足/欠费/代金券过期） | `429` `rate_limit_reached_error`；另有 `429` `engine_overloaded_error` | **是**（三种语义同压 429） | `401` `invalid_authentication_error` | 仅 `engine_overloaded_error` 提到按 `Retry-After` 等 |
| **BigModel** | `429` + 业务码 `1113`（账户已欠费） | `429` + `1302`（速率）／`1305`（模型过载） | **是** | `401` + `1000`/`1001`/`1003` | 文档未提 |
| **Gemini** | `429` `quota_exceeded`（日配额） | `429` `rate_limit_exceeded`（每分钟/每秒） | **是** | `401` `authentication`；`403` `permission_denied` | 文档未提 |
| **Bedrock Mantle** | — | — | — | — | — |

两处必须说清楚的边界：

- **Bedrock Mantle 那一行仍然是空的，不能用 AWS 的 Converse 文档去填。** Converse 的
  `ThrottlingException`（`429`，措辞是 "exceeding the account quotas"，同样把限流与账户配额
  压在一个异常里）属于 **withheld 的 Runtime profile**；Mantle 走的是另一张面
  （`internal/app/provider_adapters.go:419-434`，OpenAI 形状或自有 Responses 适配器），
  错误信封大概率不是 AWS 形状。拿 Runtime 的证据去填 Mantle，就是"适配器的沉默不是上游的答案"
  那条规则的反向版本。
  （顺带：AWS 自己的 CommonErrors 页把 `ThrottlingException` 记作 `400`，Converse 页记作 `429`，
  两页不一致——这也是为什么它只能算线索。）
- **Azure 的耗尽那一格是真的没查到**，不是"没有"。它的文档只讲 429 与重试，未把配额耗尽与
  TPM/RPM 限流分开描述，甚至专门有一节解释"用量低于配额也可能收到 429"。

来源：[OpenAI](https://developers.openai.com/api/docs/guides/error-codes) ·
[Anthropic](https://platform.claude.com/docs/en/api/errors) ·
[DeepSeek](https://api-docs.deepseek.com/quick_start/error_codes) ·
[Gemini](https://ai.google.dev/gemini-api/docs/api-errors) ·
[Kimi](https://www.kimi.ai/help/kimi-api/api-troubleshooting) ·
[BigModel](https://docs.bigmodel.cn/cn/api/api-code) ·
[Azure](https://learn.microsoft.com/en-us/azure/ai-foundry/openai/quotas-limits) ·
[Bedrock Converse](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html)

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

§3.0 已经解锁了其中一件不必再等的事：**429 需要按响应体的 `code`/`type` 分流**，
这不是从某一家的实测推出来的，而是六家文档一致的结论。剩下仍然要等真实响应的是**窗口数字**
（`Retry-After` 在额度场景下给不给、值可不可信）与 **scope 宽窄**（per-credential 还是
per-model），这两项文档都不回答，只有打出来才知道。

分流要认的 code 至少包括（全部等级 B，拼写以各家文档为准）：

| 厂商 | 判为 `subscription_quota_exhausted` 的 code |
|---|---|
| OpenAI | `credit_balance_exhausted`、`organization_spend_limit_exceeded`、`project_spend_limit_exceeded` |
| Kimi | `exceeded_current_quota_error` |
| Gemini | `quota_exceeded` |
| BigModel | 业务码 `1113` |
| MiniMax | 业务码 `1008`（另有 `2056` usage limit exceeded，是 Token Plan 的 5 小时窗口） |
| Anthropic | 402 `billing_error`；429 的消费上限那种**无 code 可认**，只能靠"429 且无 `retry-after`"这个弱信号，**不要据此下判断** |

最后一行是这张表里唯一不该照做的一格：拿"没有某个头"当分类依据，是把一次瞬时限流误判成
无限期挂起的做法，而那个方向的错误代价最大（`RecoverOnCredentialRevision` 意味着要人来清）。
Anthropic 这一格留给真实响应。
