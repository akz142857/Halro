# R4 Provider / 兼容性独立评审报告

## 结论

**结论：NO-GO。** 在 `main` 的 `222d08f84f61493fc9a273d351cc728528d6e30c` 上确认 1 个 P1
问题：BigModel general profile 对一批默认或强制推理模型接受 `reasoning_effort=none`，但在上游
请求中完全省略 `thinking`。上游的默认值是开启推理；对强制推理模型，`none` 本来还应在 Provider
I/O 前拒绝。这既静默改变了调用者意图，也会令 answer-only token limit 和北向端点可渲染性判断建立在
错误的“不会推理”假设上。现有单测明确把该错误行为断言成通过。

除该问题外，BigModel 四象限的 host / product surface / credential scheme / path prefix 隔离、MiniMax
枚举与能力证据分离、模型替换保护，以及 Python / Node / Go SDK 对本机兼容性假服务的非流式、流式、
usage 和失败语义测试均通过。按任务约束，本次未访问任何真实 Provider，也未修改产品代码。

## 评审边界与方法

- 基线：`main`，HEAD `222d08f84f61493fc9a273d351cc728528d6e30c`；主要比较范围为
  `v0.7.1..HEAD`。
- 对应计划：R4，覆盖 INV-03、INV-04、INV-05、INV-06、INV-07、INV-15。
- 检查面：Provider profile / offering / surface / connection group、adapter 构造、URL/path/auth、模型
  枚举、能力证据来源、请求渲染、响应/流解码、usage、错误语义、SDK 兼容性锁。
- 证据优先级：源码和自动化测试；BigModel 契约仅使用一方官方文档；Provider 行为证据仅复用仓库中
  已保存且注明来源/日期的 fixture，没有发起真实 Provider 请求。
- 未读取其他 role 的评审草稿；未改动任何产品代码。

## Profile × endpoint × region 矩阵

### BigModel 四象限

| Profile | 产品面 / 地区 | Base URL | operation path | credential scheme | 结果 |
|---|---|---|---|---|---|
| `bigmodel.cn.chat-embeddings.v1` | General API / CN | `https://open.bigmodel.cn` | `/api/paas/v4/{chat/completions,embeddings,models}` | `bigmodel.api-key` | host/path/scheme 和独立 connection group 均通过 |
| `bigmodel.global.chat.v1` | General API / Global | `https://api.z.ai` | `/api/paas/v4/{chat/completions,models}` | `bigmodel.api-key` | host/path/scheme 和独立 connection group 均通过 |
| `bigmodel.cn.coding.chat.v1` | Coding Plan / CN | `https://open.bigmodel.cn` | `/api/coding/paas/v4/{chat/completions,models}` | `bigmodel.coding-plan-key` | 与同 host 的 general surface 按 path 和 key 隔离；通过 |
| `bigmodel.global.coding.chat.v1` | Coding Plan / Global | `https://api.z.ai` | `/api/coding/paas/v4/{chat/completions,models}` | `bigmodel.coding-plan-key` | 与同 host 的 general surface 按 path 和 key 隔离；静态契约通过，未做真实订阅验证 |

`bigModelPathPrefix` 只按精确 profile ID 选择 `api/paas/v4` 或 `api/coding/paas/v4`，没有通过 host、
model 名称或 fallback 猜测产品面。四个 profile 的 connection group 均不同，避免 CN/Global、General/
Coding 共用 credential 或连接配置。官方 Z.AI Coding Plan 快速开始也分别列出 OpenAI Chat
`https://api.z.ai/api/coding/paas/v4` 和普通 API 路径，且要求 Coding Plan 专用 key：
[Z.AI Coding Plan Quick Start](https://docs.z.ai/devpack/quick-start)。

### MiniMax 枚举与协议面

| 产品面 | 地区 | OpenAI-shaped profile | Anthropic-shaped profile | 枚举来源 | 结果 |
|---|---|---|---|---|---|
| API Platform | Global（同 surface 允许 CN host） | `minimax.chat.v1` / `minimax.responses.v1` | `minimax.anthropic-messages.v1` | 同 host 的 OpenAI-shaped `/v1/models` | 保存的真实响应 fixture、拒绝 key、空/非 JSON/畸形响应测试通过 |
| Subscription Access | CN | `minimax.cn.subscription.openai-chat.v1` | `minimax.cn.subscription.anthropic-messages.v1` | CN surface 的 `/v1/models` | profile/binding 静态覆盖通过；未使用真实订阅 key |
| Subscription Access | Global | `minimax.global.subscription.openai-chat.v1` | `minimax.global.subscription.anthropic-messages.v1` | Global surface 的 `/v1/models` | profile/binding 静态覆盖通过；未使用真实订阅 key |

Anthropic-shaped profile 没有因为自身 decoder 不识别 OpenAI `/models` 形状而退回硬编码目录；adapter
使用同 host、同 bearer key 的 OpenAI-shaped catalogue decoder。枚举仅决定“谁存在”，builtin catalogue
与探测/声明才决定“能做什么”。`resolveInvocationTargetWithCatalog` 也只在 metadata source 确为
`provider_metadata` 时运行 metadata mapper，未知或手填 model 不会凭 endpoint 的存在被自动授予 chat、
streaming 等能力。

### SDK compatibility 矩阵

| SDK | 锁定版本 | 本机假服务覆盖 | 结果 |
|---|---|---|---|
| Python OpenAI / Anthropic | `openai==3.8.0`, `anthropic==1.4.0` | sync/async、chat/messages、stream、usage、错误 | PASS（Python 3.12.7） |
| Node OpenAI / Anthropic | `openai@7.10.0`, `@anthropic-ai/sdk@0.124.0` | chat/messages、stream、usage、错误 | PASS（Node 22.22.2） |
| Go OpenAI / Anthropic | `github.com/openai/openai-go/v3@v3.56.0`, `github.com/anthropics/anthropic-sdk-go@v1.71.0` | chat/messages、stream、usage、错误 | PASS |

这些 SDK 测试证明的是 Halro 北向 facade 与锁定客户端的兼容性，不是任何真实 Provider 的上游兼容性。

## 已确认问题

### R4-P1-001：BigModel `none` 被静默丢弃，默认/强制推理模型被误判为“不推理”

**严重性：P1，发布阻断。** 违反 INV-07 的“精确保持调用者意图”，并影响 INV-05 的北向兼容性
和预留前 fail-closed 保证。

**可达入口**

任一 OpenAI Chat Completions 北向请求，只要路由至 BigModel CN/Global general profile，并选中例如
catalogue 已内置的 `glm-4.7`，即可到达该路径。显式发送 `reasoning_effort=none` 不需要特殊权限或探测
状态。

**确定性最小复现**

```go
request := bigModelBaseRequest("glm-4.7")
request.ReasoningEffort = "none"
body, err := RenderBigModelChatRequest(request, "")
// 当前：err == nil，body.Thinking == nil
```

仓库自己的 `TestBigModelChatReasoningIsExactPerModel` 在
`internal/compatibility/bigmodel_test.go:53-88` 中明确写入
`{"glm-4.7", "none", "", false}`，因此测试通过反而证明错误契约已经被固化。

**根因链**

1. `internal/compatibility/bigmodel.go:52-60` 只把 `glm-5.3` / `glm-5.3-flash` 归类为
   always reasoning，把 `glm-5.2` 归类为 optional；其余所有模型一律归为 no reasoning。
2. `internal/compatibility/bigmodel.go:63-76` 因而接受这些模型的 `none`。
3. `internal/compatibility/bigmodel.go:172-186` 没有为 no-reasoning 分支写入任何 `thinking` 字段，最终
   wire request 省略该成员。
4. BigModel/Z.AI 官方契约中 `thinking.type` 默认是 `enabled`；支持动态开关的模型需要显式
   `disabled` 才能关闭，而强制推理模型不应接受 `none`。当前 Z.AI 文档还明确将 GLM-4.7、
   GLM-4.5V 列为强制推理模型：
   [Z.AI Chat Completion](https://docs.z.ai/api-reference/llm/chat-completion)、
   [智谱 thinking 能力说明](https://docs.bigmodel.cn/cn/guide/capabilities/thinking)。
5. 这与 renderer 自己在 `internal/compatibility/bigmodel.go:101-103` 声明的原则冲突：只有在语义
   等价时才允许省略 unsupported member。

**影响面**

- 调用者要求 `none`，上游却仍可能/必然推理，造成额外延迟、token/订阅额度消耗和 reasoning
  content；这是成功响应中的静默语义漂移，不是显式错误。
- `bigModelThinkingWillBeOn`（`internal/compatibility/bigmodel.go:79-84`）也对这些请求返回 false，
  从而可能把 answer-only `max_tokens` 当作可无损映射；实际推理开启时，该 token bound 的语义不再
  成立。
- catalogue 只为 `glm-5.3` 和 `glm-5.3-flash` 设置 `ReasonsUnasked=true`
  （`internal/modelcatalog/builtin.go:659-680,682-698`）。`glm-4.7` 等官方已说明会默认/强制推理的
  可达模型没有标记。
- `filterUnrenderableReasoning` 只看 `target.ReasonsUnasked`
  （`internal/gateway/service.go:2885-2907`）。漏标时，请求不会在额度预留前被路由 away，可能在
  Provider 已计费后才因北向 Responses/Anthropic 端点无法承载 reasoning answer 而失败。
- `TestEveryTargetThatReasonsUnaskedIsPairedWithEveryEndpoint` 只遍历“已经被标记”的 entry
  （`internal/app/northbound_reasoning_contract_test.go:77-107,138-148`），所以无法发现 catalogue
  漏标。

**修复与回归建议（本报告未修改代码）**

- 以 `(profile, exact model)` 为键维护可审计的 reasoning wire policy，至少区分 always/forced、
  default-on-but-disableable、optional/off、unknown；unknown 必须 fail closed，不应降级为 no reasoning。
- 显式 `none`：可关闭模型发送 `thinking.type=disabled`；强制/always 模型在 Provider I/O 和额度预留前
  拒绝或路由至可精确满足的 target。
- 空 effort：若 renderer 按计划不强制发送 disabled，则所有已知会默认/强制返回 reasoning 的 entry
  必须标记 `ReasonsUnasked=true`；unknown 不能被当作已知不会推理。
- 增加至少四类回归：`glm-4.7/none` fail-closed、一个 dynamic model 的 `none -> disabled`、空 effort
  的 `ReasonsUnasked` catalogue 覆盖、不能渲染 reasoning 的北向端点在预留前过滤。

## 候选问题 / 发布风险

以下项目缺少本次约束内可取得的真实 Provider 证据，因此不记为已确认缺陷，但应由发布负责人明确
接受或补证。

### R4-C2-001：Global Coding Plan 的真实行为矩阵尚未闭合

Global Coding Plan 的 host/path/key 静态契约正确，但仓库注释也说明其模型集合来自一方文档而非真实
订阅。实际 enumeration、旧 model alias/substitution、`stream_options.include_usage`、finish/error
envelope 和 quota/expired 语义未在真实订阅上验证。建议补一组不计费或最低成本的 release smoke，并
保存去敏原始 response fixture。

### R4-C2-002：MiniMax Subscription Access 的 CN/Global 真实凭据证据缺失

四个 subscription profile 的 wiring 和 fake 测试存在，但未见本发布基线内用真实 entitlement key
分别验证 `/models`、chat、stream final usage、无效/过期/额度耗尽错误的证据。尤其不能仅凭 API
Platform 的同协议行为推导 Subscription Access 的 entitlement/failure semantics。

### R4-C2-003：BigModel Coding Plan 的可用模型集合可能随官方产品漂移

CN catalogue 中 Coding Plan 的两条 capability seed 来自 2026-09-09 实测；当前官方产品页展示的
计划模型/alias 可能继续变化。动态 `/models` 枚举与 substitution guard 已避免“隐藏新 model”或把
alias 探测证据错记给 alias，但新 target 会保持 unknown，直到声明/探测，不会自动获得能力。此行为
安全但可能造成升级后暂时不可用；需在 release note/运维检查表中明确。

### R4-C2-004：订阅额度错误的 canonical mapping 未经真实样本验证

通用的 401/403/429 与畸形响应路径有自动化测试，但 `subscription_inactive`、
`subscription_quota_exhausted` 等业务分类如果依赖 Provider 的具体 body/code，仍需要真实的去敏错误
样本。不要从 HTTP status 单独推导 subscription state。

## 负面证据（未发现问题的部分）

- **四象限隔离：** `TestBigModelWiringKeepsEachProductOnItsOwnHostSurfaceAndPath`、connection-group、
  surface/offering/region stability 相关测试通过。CN/Global 和 General/Coding 均不能交叉绑定。
- **URL/path/auth：** BigModel fixture 验证 Bearer auth、general/coding path prefix、Chat/Embeddings/
  Models 相对路径；adapter 没有通过 host 猜 path。MiniMax 两种 wire face 均保持同 host bearer key，
  catalogue endpoint 使用 OpenAI shape。
- **枚举与能力分离：** BigModel/MiniMax `/models` 只提供 model identity；未知 model 不因能列出或手填
  而获得 chat/streaming 能力。metadata mapper 仅消费 upstream 实际描述的 metadata source。
- **替换保护：** Coding Plan 返回与请求不同的 response model 时，substitution guard 不把探测结果
  归因给原 alias；依赖 chat probe 的 stream 能力也不会绕过该保护。
- **stream/usage/failure：** 聚焦 provider/compatibility 测试覆盖正常 SSE、terminal usage、finish、
  malformed/ambiguous body、空列表、拒绝 key、非 JSON 和错误映射；本机 SDK 假服务验证三种语言客户
  端可消费这些语义。
- **MiniMax 真实响应 fixture：** 仓库保存的 2026-09-01 `/v1/models` 原始形状包含 8 个 chat model，
  decoder 测试针对该实际 OpenAI-shaped body，而不是从文档臆造 response schema。
- **能力边界：** BigModel CN Coding 的已测两 target 分别按 profile/model 赋能，未从 general profile
  继承 vision 等能力；Global Coding 没有把未测 StreamUsage 直接标 true。

## 执行命令与结果

### 源码与聚焦测试

```bash
git rev-parse HEAD
git diff --stat v0.7.1..HEAD -- internal/domain internal/provider internal/compatibility internal/modelcatalog internal/app tests/compatibility

go test -count=1 ./internal/domain ./internal/compatibility ./internal/provider/... ./internal/modelcatalog ./internal/app \
  -run 'Test(BigModel|MiniMax|ProviderBindingsCannotCrossConnectionGroups|ConnectionGroupsAreExplicit|ProfileConnectionGroupsAreStable|SurfaceOfferingAndRegionScopeAreStable|EveryProfile|AllProfiles|Offering|UsagePolicy|AdapterConstruction|CeilingWithinProfileManifest|Manifest|Capability|ModelCatalog|AdminProvider)'
# PASS

go test -count=1 -v ./internal/compatibility -run '^TestBigModelChatReasoningIsExactPerModel$'
# PASS，但 glm-4.7/none 的错误期望被测试固化，见 R4-P1-001
```

### 本机兼容性假服务（未访问 Provider）

```bash
go build -o /private/tmp/halro-r4-compat-server ./tests/compatibility/server
/private/tmp/halro-r4-compat-server --listen 127.0.0.1:18088

npm test --prefix tests/compatibility/node
# PASS, Node v22.22.2

go -C tests/compatibility/go test -count=1 ./...
# PASS

python3 -m venv /private/tmp/halro-r4-python-sdk-venv
uv pip install --python /private/tmp/halro-r4-python-sdk-venv/bin/python \
  --index-url https://pypi.org/simple -r tests/compatibility/python/requirements.txt
/private/tmp/halro-r4-python-sdk-venv/bin/python tests/compatibility/python/test_sdk.py
# PASS, Python 3.12.7
```

沙箱内首次监听 loopback 和客户端连接返回 `EPERM`，属于执行环境网络隔离；同一二进制和同一测试在
获准的本机环境重跑后全部通过。Python 初始环境没有锁定依赖，使用临时 venv 安装 requirements 后
通过；没有改动仓库或全局 Python 环境。

## 未验证项

- 未使用或读取任何真实 Provider credential；未向 BigModel/Z.AI/MiniMax API 发请求。
- 未验证 Provider 账户地区、套餐 entitlement、余额/额度、限流窗口和账单实际扣减。
- 未验证真实网络下的 DNS/TLS/proxy、长连接中断、上游重试和跨区延迟。
- 未捕获 Global Coding Plan、MiniMax CN/Global Subscription Access 的真实 list/chat/stream/error body。
- 未执行 billable smoke、破坏性测试或负载测试。
- SDK 假服务覆盖的是锁定版本在当前 fixtures 上的 API 行为，不覆盖未来 SDK 版本，也不等同于真实
  Provider SDK 直连。

## R4 发布判定

在 R4-P1-001 修复并补上上述 targeted regression 前，R4 不签字，v0.8.0 不应进入顺序合并/发布。
候选问题不必全部变成代码改动，但 Global Coding Plan 与 MiniMax Subscription Access 的真实行为缺口
应由 release owner 明确登记为已接受风险，或在发布前补齐去敏 smoke 证据。
