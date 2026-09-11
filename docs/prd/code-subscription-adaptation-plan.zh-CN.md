# Kimi、MiniMax、DeepSeek Code 订阅适配实施方案

状态：**实现完成；MiniMax OpenAI 已开放，Kimi Code 与 MiniMax Anthropic 按证据门槛保持 withheld，DeepSeek 未注册虚构订阅产品**

最近复核：2026-09-10

关联方案：

- [Provider Offering / 订阅访问总体方案](./provider-offering-subscription-access-plan.zh-CN.md)
- [BigModel 适配方案](./bigmodel-adaptation-plan.zh-CN.md)

## 1. 目标

在不增加厂商特例 UI 的前提下，让 Kimi、MiniMax 的 Code 订阅像 BigModel Coding Plan 一样成为
独立的上游产品：凭据创建时选择产品与账号地域，连接选择具体协议实现，部署只选择上游可请求的
模型 ID，用量、失败与审计记录都能追溯到对应 Offering/Profile/账号地域。

DeepSeek 采用同一设计，但只有在官方发布可由第三方工具调用的独立订阅凭据契约后才激活。当前官方
DeepSeek API 仍是 API Key + 充值/赠送余额、按 token 扣费；不能仅因为它可用于 Codex/编程任务，
就把按量 API 命名为“Code 订阅”。

本方案延续 BigModel 已实现的通用层，不引入 `is_coding_plan`、`is_subscription_key` 等厂商布尔字段。

## 2. 核心产品模型

```text
Provider Type
  └─ Offering（买了什么）
       └─ Region / Surface（账号和额度在哪个边界）
            └─ Connection Group（哪些 Profile 组合或互斥）
                 └─ Profile（使用哪种协议实现）
                      └─ Deployment（调用哪个真实模型 ID）
```

“订阅”属于 Offering，不属于模型 ID。模型下拉框不得出现“Kimi Code”“Token Plan”等产品名称；否则
同一个模型 ID 会同时承担产品选择、路由和能力证据三种职责，无法保证凭据和额度不串用。

统一规则：

1. 订阅和按量 API 使用不同 Offering、Access Surface 与 Credential Scheme。
2. 已被一手契约证明为独立额度边界的国内和海外账号不能共用凭据；地域固定在 surface 上，或由
   官方 endpoint 明确推导。证据不足时使用单一 surface + `RegionNone`，不靠产品需求猜地域。
3. 一份凭据创建后不能通过轮换改变 Offering、region、surface 或 scheme；换产品必须新建凭据。
4. 官方已知 Host 与所选固定地域矛盾时拒绝保存；无法识别的企业代理地址允许保存。固定 surface
   仍保留操作者选择的产品地域，同时单独显示“自定义端点，地域未验证”，不能把产品地域改成 unknown。
5. 上游模型枚举与能力证据分离：模型列表回答“谁存在”，内建目录只回答“已知能做什么”。
6. 订阅价格未知时沿用 BigModel 的 unknown-cost 语义，不把调用记为零成本。
7. 受限产品需要地域化说明、明确确认和带 policy revision 的审计记录；服务端只能记录操作者真正
   提交并与当前版本匹配的 revision。
8. `account_region_id` 是独立持久化维度，不复用部署/cloud `region`；Target、Ledger、failure capture、
   Parquet 和查询结果都快照它，确保跨产品 fallback 与 by-endpoint 产品可以按账号地域归因。

## 3. 当前证据与实施结论

| 厂商 | 按量产品 | Code 订阅产品 | 当前结论 |
| --- | --- | --- | --- |
| Kimi | Kimi Open Platform | Kimi Code 会员权益 | 通用产品与协议机制可实现；地域清单及 Halro 身份仍需证据，相关 Profile 先 withheld |
| MiniMax | MiniMax API Platform | MiniMax Token Plan | 国内外均公开专用 Key 和 endpoint，可以实施 |
| DeepSeek | DeepSeek API Platform | 尚无官方独立产品 | 只设计激活门槛，不注册可达 Offering/Profile |

### 3.1 Kimi Code

官方当前公开：

- Kimi Code Key 来自 Kimi Code 控制台，与 Kimi Open Platform API Key 不可互换；
- OpenAI-compatible Base URL：`https://api.kimi.com/coding/v1`；
- Anthropic-compatible Base URL：`https://api.kimi.com/coding/`；
- 当前公开模型包括 `k3`、`k3-256k`、`kimi-for-coding`、
  `kimi-for-coding-highspeed`；模型清单会演进；
- 官方要求第三方工具保留真实身份标识，不能伪造受支持工具的 User-Agent；
- 订阅存在周额度、月额度和滚动频率限制，并可能启用 Extra Usage。

产品需求提出国内/海外两类账号，但当前中英文公开资料给出的是同一个 `api.kimi.com` Coding Host。
两把不同账号的 Key 都能在该 Host 使用，只能证明 Key 有效，不能证明它们属于两个额度 surface。
正式开放双地域前必须从一手账号信息验证 OAuth issuer、控制台账号地域、条款与额度归属；地域即便
成立，也只能是 operator-declared，不能从调用 Host 在线推导。

阶段 A 必须在下面两个互斥清单中作出选择，不能同时注册：

- **双地域清单**：只有一手证据证明两个独立账号/额度边界后，才注册 `kimi-code-cn` 与
  `kimi-code-global`，并让四个候选 Profile 继续接受 Halro 身份和 Thinking 门槛；
- **单地域降级清单**：证据仍不足时，只注册 `kimi-code`（`RegionNone`）和 OpenAI/Anthropic 两个
  Profile。它们仍因 Halro 身份/Thinking 门槛保持 withheld，但不得预注册、下发或展示虚构的国内/
  海外选项。

本次实现采用**单地域降级清单**：领域表中只有 `kimi-code`（`RegionNone`）及两个 withheld Profile；
Admin 元数据和 UI 均不显示 Kimi Code，直到阶段 A 的身份与协议门槛完成。MiniMax 则按已公开的两个
固定地域 surface 实现。

### 3.2 MiniMax Token Plan

官方当前公开：

- Token Plan/Subscription Key 与按量 API Key 不可互换；同一 Subscription Key 还可能消费 Token Plan
  席位或已购 Credits，甚至可以在尚无可用资源时提前存在；
- 国内 Anthropic endpoint：`https://api.minimax.cn/anthropic`；
- 国内 OpenAI-compatible endpoint：`https://api.minimax.cn/v1`；
- 海外 Anthropic endpoint：`https://api.minimax.io/anthropic`；
- 海外 OpenAI-compatible endpoint：`https://api.minimax.io/v1`；
- Token Plan 的模型、请求额度和多模态权益会随套餐更新，不能长期依赖静态全量模型列表。

MiniMax 国内与海外可由官方 Host 区分，但仍使用两个固定 surface，以保持与 BigModel 一致的凭据身份
和审计语义。`RegionForProviderEndpoint` 用于拒绝明确的跨区 Host，不参与选择产品。由于 Subscription
Key 是 Token Plan + Credits 的混合资源入口，Offering 的展示名称和成本治理不能暗示每次调用必然从
周期订阅额度扣减。

### 3.3 DeepSeek

截至复核日期，官方 API 文档声明：

- 使用 DeepSeek Platform 创建的 API Key；
- 费用按输入/输出 token 从充值余额或赠送余额扣减；
- Codex、Claude Code 等页面只是说明如何把 DeepSeek API 接入编程工具，并未定义独立订阅 Key、
  订阅 endpoint 或周期额度。

因此当前只保留候选设计，不在 `providerOfferingTable`、`surfaceTable`、Profile 表、Admin 元数据或 UI
中注册 `deepseek.code-subscription`。第三方经销商的“DeepSeek Coding Plan”也不能登记为 DeepSeek
官方产品；它应作为该经销商自己的 Provider/Offering 适配。

## 4. 标识与表结构设计

### 4.1 Offering

新增：

```go
OfferingKimiCode                  ProviderOfferingID = "kimi.code"
OfferingMiniMaxSubscriptionAccess ProviderOfferingID = "minimax.subscription-access"
OfferingKindEntitlement           ProviderOfferingKind = "entitlement"
```

`kimi.code` 为 `OfferingKindSubscription`；MiniMax 使用新的通用 `OfferingKindEntitlement`，表示同一产品
Key 可以承载 Token Plan 席位或 Credits，不能断言某次调用实际扣了哪一种内部余额。二者都设置
`RequiresUsageWarning=true`。Admin/Web 的 kind 枚举和中英文标签同步加入 `entitlement`，但路由不按 kind
分支，行为仍由 Profile 决定。

候选但暂不注册：

```go
// 只在 §12 的激活条件全部满足后加入表。
OfferingDeepSeekCodeSubscription ProviderOfferingID = "deepseek.code-subscription"
```

现有 `kimi.open-platform`、`minimax.api-platform`、`deepseek.api-platform` 不改名、不迁移，避免把既有
凭据和用量重新归类。

### 4.2 Access Surface

MiniMax 两条 surface 无条件进入目标清单；Kimi 仅在 §3.1 的双地域证据门槛通过后采用下表的两条
fixed surface：

| Surface | Offering | Region | Scope | 官方 Host |
| --- | --- | --- | --- | --- |
| `kimi-code-cn` | `kimi.code` | `cn` | `fixed` | `api.kimi.com` |
| `kimi-code-global` | `kimi.code` | `global` | `fixed` | `api.kimi.com` |
| `minimax-cn-subscription-access` | `minimax.subscription-access` | `cn` | `fixed` | `api.minimax.cn` |
| `minimax-global-subscription-access` | `minimax.subscription-access` | `global` | `fixed` | `api.minimax.io` |

Kimi 两条 surface 共享 Host 是合法状态。`RegionForProviderEndpoint(ProviderKimi, "api.kimi.com")` 必须
返回 unknown，而不是任选一个地域；地域由 surface 决定。MiniMax Host 则必须能识别并拒绝跨区绑定。

若 Kimi 双地域门槛未通过，则用 `kimi-code | kimi.code | none | none | —` 替换表中的两条 Kimi 记录；
`api.kimi.com` 只保留在 Profile 的 Base URL 中。Offering 的所有 surface 必须来自同一个清单，禁止
同时出现 `RegionNone` 和 fixed 地域。
固定 surface 不需要向 Admin 输出 `region_hosts` 供 UI 二次推导；若兼容旧契约必须保留该字段，则按
`(region, host)` 去重，且 Web 对 fixed surface 只使用 surface region，绝不能按裸 Host 折叠 Kimi 两地。

每条受限 surface 都声明独立 `DocumentationURL` 和稳定 `UsagePolicyRevision`。revision 更新表示 Halro
提示或所依据的官方契约发生了实质变化，不跟随每次文字润色递增。

### 4.3 Credential Scheme

新增：

```go
CredentialKimiCodeKey             CredentialScheme = "kimi.code-key"
CredentialMiniMaxSubscriptionKey CredentialScheme = "minimax.subscription-key"
```

两者仍使用静态 Bearer 传输，但 scheme 不能复用 `bearer.static`。Scheme 表示密钥来源和产品边界，
不是 HTTP Header 的形状；独立 scheme 能阻止订阅 Key 与按量 Key 交叉绑定。

DeepSeek 候选 scheme `deepseek.code-subscription-key` 只记录在方案里，不提前进入枚举。

### 4.4 Provider Profile

双地域清单为每个地域提供两个协议实现，不声明 Responses API：

| Profile ID | Connection Group ID | Base URL | Wire | 首次状态 |
| --- | --- | --- | --- | --- |
| `kimi.code.cn.openai.chat.v1` | `kimi-code-cn-openai` | `https://api.kimi.com/coding/v1` | OpenAI Chat Completions | withheld，待地域/身份验证 |
| `kimi.code.cn.anthropic.messages.v1` | `kimi-code-cn-anthropic` | `https://api.kimi.com/coding` | Anthropic Messages | withheld，另受 Thinking 门槛约束 |
| `kimi.code.global.openai.chat.v1` | `kimi-code-global-openai` | `https://api.kimi.com/coding/v1` | OpenAI Chat Completions | withheld，待地域/身份验证 |
| `kimi.code.global.anthropic.messages.v1` | `kimi-code-global-anthropic` | `https://api.kimi.com/coding` | Anthropic Messages | withheld，另受 Thinking 门槛约束 |
| `minimax.cn.subscription.openai.chat.v1` | `minimax-cn-subscription-openai` | `https://api.minimax.cn/v1` | OpenAI Chat Completions | offered |
| `minimax.cn.subscription.anthropic.messages.v1` | `minimax-cn-subscription-anthropic` | `https://api.minimax.cn/anthropic` | Anthropic Messages | withheld，待 Thinking 验证 |
| `minimax.global.subscription.openai.chat.v1` | `minimax-global-subscription-openai` | `https://api.minimax.io/v1` | OpenAI Chat Completions | offered |
| `minimax.global.subscription.anthropic.messages.v1` | `minimax-global-subscription-anthropic` | `https://api.minimax.io/anthropic` | Anthropic Messages | withheld，待 Thinking 验证 |

若采用 Kimi 单地域降级清单，则以 `kimi.code.openai.chat.v1`、
`kimi.code.anthropic.messages.v1` 及各自唯一的 Connection Group 替换四条地域 Profile；MiniMax 四条不变。

同一地域的两个 Profile 共享 surface 和 scheme，所以一把订阅凭据可以创建两个协议连接；但它们不是
一个自动组合的连接。当前 `ConnectionProfiles` 和 Web 都按 `(type, surface, scheme)` 合并，无法表达
这种协议二选一。实施前先在 `profileRow` 与 Admin/Web 元数据增加永久的 `ConnectionGroupID`：

- `ConnectionProfiles` 只组合相同 `ConnectionGroupID` 的 Profile；
- OpenAI 与 Anthropic 协议使用不同 group，因此在连接表单都是独立选项，选中后只绑定 anchor；
- 现有可自动组合的 Chat/Media companion Profile 共用 group，行为不变；
- `RoutePartitioned` 继续只表示“模型只在组内某条路由存在”，不得借它显示协议选项；
- 不变量确保同 group 的 Profile 具有相同 type/surface/scheme，且 group ID 永不被重新指向别的语义。

所有既有 Profile 也必须补上显式、非空的 Connection Group ID：能自动组合的旧 companion 使用同一
稳定 group，原本独立的连接使用不同 group。Admin API 不提供按旧三元组临时推导的双重语义，升级前后
对同一既有 Profile 的分组结果必须一致。

Credential identity 仍按 `(surface, scheme)` 去重；表顺序中的 primary profile 只用于解析凭据默认值，
不代表操作者选择了该协议，也不得写入凭据创建审计。

适配器优先复用现有 Kimi/MiniMax OpenAI 与 Anthropic renderer/parser，但必须创建新的 profile-scoped
adapter binding。不能让订阅 profile 继承按量 profile 的 path、模型目录或能力证据。

Credential audience 继续只绑定规范化 origin（scheme + host + port + provider type），不包含
`/coding/v1`、`/coding`、`/v1` 或 `/anthropic`。这样同一凭据才能安全供同 Host 的两个协议 Profile
使用；Provider Connection 的 Base URL 和 Profile 决定实际路径。

## 5. 模型枚举与能力证据

### 5.1 先读真实响应

每个产品、地域、协议都保存脱敏 fixture：

1. 使用对应订阅 Key 调用官方已公布的模型枚举路由；未公布时才探查候选路由；
2. 保存 HTTP 状态、响应头和脱敏 body；
3. 验证订阅 entitlement、返回 shape 和实际 identifier；
4. 有上游列表时，上游列表是“谁存在”的唯一来源，并在控制台显示“刷新”；
5. 只有确认不存在枚举路由时，才使用带复核日期和来源的最小 seed。

不得用无 Key 的 401 证明路由存在，也不得把现有按量 surface 的模型列表当作订阅列表。官方已经
公布的路由直接写入 Profile 契约；真实 smoke 验证权限和实际返回值，不重新猜“路由是否存在”。

### 5.2 Kimi 初始目录

如果真实验证确认没有模型枚举路由，首期 seed 仅使用官方模型配置页当前列出的：

- `k3`
- `k3-256k`
- `kimi-for-coding`
- `kimi-for-coding-highspeed`

能力以官方声明记为 `declared`；真实 smoke 只把实际请求验证到的字段升级为 `verified`。模型别名或
后端自动升级不能把一个模型的验证结果归到另一个 ID。

### 5.3 MiniMax 初始目录

MiniMax 已公布两种模型枚举契约：

- OpenAI Profile：`GET /v1/models`，`Authorization: Bearer`，OpenAI list shape；
- Anthropic Profile：`GET /anthropic/v1/models`，`x-api-key`，Anthropic list shape。

实现直接复用各自 Profile 的 catalogue decoder，并使用上游 identifier；真实 Subscription Key smoke
验证 entitlement 和响应中的实际列表。内建目录只为返回的 ID 补能力。不得把 Token Plan 介绍页的
套餐权益表直接转换成 Chat 能力，因为其中可能包含语音、图像、视频或音乐模型。

首期连接只开放 Chat、Streaming、Tools、Reasoning 中有官方证据或真实响应证据的能力。Vision、
Structured Outputs、Files、Speech、Image、Video 等必须按独立 operation/profile 实现，不能因为
Token Plan 套餐“包含多模态”就在文本 profile 上自动开启。

### 5.4 Anthropic portable/native 开放门槛

现有 `anthropicWire` 同时绑定 portable Chat 与原生 Messages；portable decoder 无法把未请求的
`thinking` / `redacted_thinking` 安全转换成普通 Chat。Kimi 当前模型默认或固定 Thinking，MiniMax
官方示例也返回 thinking block，因此 Anthropic Profile 不能因“协议兼容”直接 offered。

每个模型、地域必须验证：

1. portable 非流式 Chat 未请求 reasoning 时，`thinking:{type:"disabled"}` 被接受且响应不含 thinking；
2. portable 流式 Chat 同样不产生 thinking/redacted_thinking 事件；
3. 显式请求 reasoning 时，非流式/流式均能按 Halro canonical contract 转换；
4. 被拒绝或仍强制返回 thinking 时，不得让已发生上游计费的请求最终变成无说明的 502。

若无法关闭 Thinking，首期只开放原生 Messages。为此需要新增真实的 `native_only` primitive/能力表达，
或保持该 Anthropic Profile withheld；不能复用 `route_partitioned` 或假称 portable Chat 可用。

### 5.5 替换与回显保护

Kimi 固定别名和 MiniMax 可能出现的模型路由都必须进入 substitution guard：只有上游响应明确回显
请求模型，或官方契约明确声明别名映射时，探测结果才可写回该模型。否则结果保持 unknown，并记录
`model_substituted` 或 `provider_model_unconfirmed`。

## 6. UI/UX

凭据表单保持 BigModel 的统一流程：

```text
服务商类型 → 上游产品（Offering + 地域） → 地址绑定 → 服务商密钥
```

预期选项（Kimi 的两种清单互斥）：

- Kimi Open Platform · 中国大陆/海外（现有）
- Kimi Code（订阅）· 中国大陆（满足 §3.1 地域与身份门槛后显示）
- Kimi Code（订阅）· 海外（满足 §3.1 地域与身份门槛后显示）
- Kimi Code（订阅）（仅单地域降级清单且身份门槛通过后显示，不附会地域）
- MiniMax API Platform · 中国大陆/海外（现有）
- MiniMax Token Plan / Credits · 中国大陆
- MiniMax Token Plan / Credits · 海外
- DeepSeek API Platform（现有；不展示虚构的订阅项）

选择产品时自动填充该 profile 的默认 Base URL 和 scheme。受限条款沿用紧凑折叠组件：默认收起，
持续显示待确认/已确认；未确认提交时自动展开并聚焦确认框。固定 surface 使用企业代理时仍显示
产品地域，旁边增加“自定义端点，地域未验证”，而不是把产品地域显示为 unknown。

每份 documentation 元数据改为 `{region, url, policy_revision}`。创建/改绑请求提交
`acknowledged_policy_revision`，不得只提交一个无法证明所读版本的布尔值。服务端必须将它与当前
surface revision 精确比较：一致才保存并将该提交值写入审计；受限 surface 缺失该字段时返回
HTTP 422 + `usage_policy_acknowledgement_required`，版本不一致时返回 HTTP 409 +
`usage_policy_revision_mismatch`，并在响应中返回当前 documentation 元数据。HTTP 412 继续只用于现有
ETag/`If-Match` 并发控制，不复用为条款错误。控制台收到 409 后显示“条款已更新”，重新加载文档并
清除确认。创建凭据、创建连接以及更换 Credential/Profile 都适用；只改名称等未改变产品绑定的编辑
不要求重复确认。

连接表单显示协议 Profile，例如“Token Plan · 海外 · Anthropic Messages”。部署表单仍只显示真实模型
ID，不再重复展示订阅产品选择；连接标签带 Offering/地域，操作者能看出额度来源。

UI/API 回归矩阵至少覆盖：

| 维度 | 必测状态 |
| --- | --- |
| 清单 | MiniMax 国内/海外；Kimi 双地域清单和单地域降级清单分别构造元数据 |
| 协议 | OpenAI offered；Anthropic 在 Thinking 门槛前 withheld、通过后 offered |
| 凭据 | 正确 Key、按量 Key、另一 Offering Key、MiniMax 跨地域 Key |
| 表单切换 | Offering、地域、Profile 改变后清空旧模型、探测结果、binding 和能力声明 |
| 条款 | 首次确认、缺失 revision、旧 revision、重新加载后确认、只改名称 |
| 地址 | 官方 Host、明确跨区 Host、企业代理；产品地域与“端点地域未验证”分开显示 |
| 可见性 | withheld Profile 不进入元数据/选择器，解除门槛后标签、默认 URL 与 scheme 正确 |

## 7. 鉴权与请求行为

- Kimi Code：`Authorization: Bearer <kimi-code-key>`；发送真实 `User-Agent: Halro/<version>`，不得透传
  任意客户端值或伪造 Claude Code、Codex 等工具身份。必须用真实 Key 验证 Halro 身份被接受；否则
  对应 surface 保持 withheld，直到官方明确允许。
- MiniMax Subscription Access 使用同一产品 scheme，但 Authorizer 按 Profile 区分：OpenAI 路由发送
  `Authorization: Bearer <key>`；Anthropic 路由发送 `x-api-key: <key>`。两者都先删除入站的冲突认证
  Header，不能把现有按量 adapter 的 Bearer 兼容性当成订阅契约。
- 所有 adapter 的 path 从 Profile 决定，不从模型 ID、Key 前缀或 Host 猜测。
- Header allowlist、Beta header、tool schema 转换沿用对应 wire 的现有安全边界。
- 连接测试若会发起推理，必须标注“可能消耗订阅额度”，且默认不自动运行。

## 8. 错误、额度与 fallback

保留现有 `provider.ErrorClass`，新增有限枚举 `provider_failure_reason`，不能把展示用短语直接当作
ErrorClass。只有上游稳定的结构化 `code/type` 可以产生 reason；不得从可变英文/中文 message 推导额度
或能力结论。每个 Profile 的 fixture 必须形成下面的精确映射：

| 可观测条件 | ErrorClass | canonical reason | Retryable | Ambiguous | Route 行为 |
| --- | --- | --- | --- | --- | --- |
| 非 Kimi 的 HTTP 401，或稳定 code 明确 Key 无效 | `authentication` | `invalid_credential` | false | false | 不回退；提示检查 Offering/地域/Key |
| Kimi bare HTTP 401 | `authentication` | 空 | false | false | 官方同时用 401 表示无模型权益、未知模型等，不武断归因为坏 Key |
| Kimi HTTP 402，会员校验暂时不可用 | `unknown` | `entitlement_verification_unavailable` | 由 adapter 的执行证据决定 | 由 adapter 的执行证据决定 | 按真实 Retryable/Ambiguous 与显式 Route 决定 |
| 稳定 code 明确订阅未生效/过期 | `authentication` | `subscription_inactive` | false | false | 不自动切按量 |
| 稳定 code 明确周期额度耗尽 | `rate_limit` | `subscription_quota_exhausted` | 保留 adapter 值 | 保留 adapter 值 | 只允许 Route 中显式配置且满足执行语义的 fallback |
| HTTP 403 但无稳定结构化 code | `authentication` | 空 | 保留 adapter 值 | 保留 adapter 值 | 不从 message 猜额度，按执行语义决定是否回退 |
| HTTP 429 且无更具体稳定 code | `rate_limit` | `rate_limited` | 保留 adapter 值 | 保留 adapter 值 | 遵守 `Retry-After` 和现有策略 |
| 上游 5xx/连接失败 | 现有分类 | 空或稳定 reason | 按现有规则 | 按现有规则 | 按 Route 策略 |

Kimi 官方公开 402/403 场景，但同一状态可能代表不同原因；MiniMax 仍需真实订阅 fixture 固定业务码和
envelope。若实际响应没有稳定 code，就保持表中的保守分支，不能通过全文匹配错误句子制造可重试或
额度耗尽结论。

`provider_failure_reason` 只归一“为什么失败”；`Retryable` 与 `Ambiguous` 描述请求是否可安全重试、
上游是否可能已执行或计费，必须保留 adapter/transport 的执行证据。不得因为 reason 更具体或更模糊而
改写这两个字段，否则回退与退款决策会和 Ledger 记录互相矛盾。

`provider_failure_reason`、原始 bounded `provider_code`、retryability 和 ambiguity 必须贯穿 attempt、
Ledger、failure capture、日志、Parquet 与查询 API；如推进 schema version，保留旧分区空值读取测试。
同时携带 `offering_id`、`profile_id`、`account_region_id`，让订阅耗尽后显式回退到按量产生的费用能够
按产品和账号地域归因。普通响应、SSE、`Retry-After`、跨路由 fallback 都使用同一映射测试。

## 9. 计价与成本治理

订阅调用不等于免费调用：

- 有官方等效单价时，创建正常 price version；
- 没有等效单价时，成本为 unknown，操作者必须显式接受该项目不由金额预算拦截；
- 本期 Dashboard 只展示“Halro 观测到的订阅请求数/Token 数”和成本 unknown，不把本地计数称为上游
  剩余额度，也不把 unknown 聚合成 `$0`；
- Extra Usage、Credits 等自动补充机制如果由上游在同一 Key 下扣减，记录仍属于订阅 Offering，并在
  产品说明中告知可能产生额外费用；Halro 不凭响应猜测一次请求用了哪种内部余额。

上游剩余额度不在本期范围。未来若加入，必须为每个厂商另行设计 quota connector、授权范围、账户级
聚合、缓存与新鲜度、失败/不可用状态、Admin API 和 Dashboard 标识；额度查询值不能从本地请求数推算。

## 10. 数据兼容与迁移

- 不自动迁移现有 Kimi、MiniMax、DeepSeek 凭据；它们继续属于按量 Offering。
- 新 ID 只追加，不重命名已有 Offering/Surface/Profile，因为这些 ID 已进入审计和用量数据。
- 旧 Ledger/Parquet 分区读取新增产品字段为空或已有按量值；不重写历史记录。
- 新增 `account_region_id` 与 `provider_failure_reason` 并推进 Parquet schema version；旧版本读取为空，
  新写入在 durable boundary 校验 Offering/Profile/account region 一致。
- 已有连接不会自动获得订阅能力；操作者新建订阅凭据和连接后，再显式加入 Route fallback。
- by-endpoint 凭据轮换必须比较旧、新 endpoint 解析出的账号地域：已知跨区拒绝；已知与 unknown 之间
  的变化也 fail closed，要求新建凭据。这样同一 Credential ID 不会跨越账号/余额边界。
- 凭据使用条款审计只记录 Offering、access surface、account region、文档 URL 和操作者提交且匹配的
  revision，不记录表顺序推导出的 primary Profile；连接创建/改绑才记录真实 Profile。
- 滚动升级期间，只有所有运行实例都能解析新 ID/schema 后才允许创建新订阅记录；回滚到旧版本前
  停止新建并保留数据备份，不能让旧实例静默丢弃未知 surface/profile。

## 11. 实施阶段

### 阶段 A：证据夹具

> 本次实现未获得专用订阅测试凭据，且仓库策略禁止默认执行会消耗额度的真实 smoke；因此下列外部
> 准入项保持未完成，对应 Profile 也按设计继续 withheld，而不是用按量账号或无鉴权响应替代证据。

- [ ] 获取 Kimi Code 国内/海外、MiniMax Token Plan 国内/海外专用测试账号。
- [ ] 每个地域分别捕获模型枚举、portable/native 非流式、流式、工具调用、Thinking 和额度错误的
      脱敏响应；不得执行耗尽整个套餐的测试。
- [ ] 验证 Kimi 两类账号的 issuer、控制台地域、条款、额度归属及 `User-Agent: Halro/<version>`；这
      是解除 Kimi Profile withheld 的硬门槛，不能用“两个 Key 都可调用同一 Host”代替。
- [ ] 验证 MiniMax 两地 Subscription Key entitlement 和实际模型列表；官方两种枚举路由与 shape
      直接作为契约，不复用按量 adapter 的猜测。
- [ ] 对每个 Anthropic Profile 验证 `thinking.disabled` 的 portable 流式/非流式；失败则 native-only
      或继续 withheld。

### 阶段 B：领域模型（已完成）

- [x] 新增 2 个 Offering、`entitlement` kind、2 个 credential scheme；MiniMax 固定新增 2 个 surface /
      4 个 Profile，Kimi 按 §3.1 互斥选择 2 个 fixed surface / 4 个 Profile 或 1 个 `RegionNone` surface /
      2 个 Profile。Kimi 与未通过 Thinking 门槛的 Profile 初始 `withheld=true`。
- [x] 增加永久 `ConnectionGroupID`，让同凭据的 OpenAI/Anthropic Profile 成为两个协议选项而不是
      自动 companion；为所有既有 Profile 回填稳定 group，保持现有 companion 与 route-partitioned
      语义不变。
- [x] 加入 Offering/region/surface/scheme/connection-group 唯一性和闭包不变量测试。
- [x] 验证 Kimi 共享 Host 返回 region unknown，MiniMax 跨区 Host 被写路径拒绝。
- [x] 验证 fixed 与 by-endpoint 凭据轮换都不能改变产品或账号地域。

### 阶段 C：Adapter 与目录（代码完成，真实证据门槛见阶段 A）

- [x] 按选定清单注册 6 或 8 个 profile-scoped adapter/binding，并严格应用各自的 offered/withheld 状态。
- [x] MiniMax OpenAI 使用 Bearer，Anthropic 使用 `x-api-key`；Kimi 使用 Bearer + 真实 Halro User-Agent。
- [ ] 根据真实响应实现模型枚举；无路由时才使用最小 seed。
- [x] 为每个模型填写独立能力证据和 substitution guard。
- [x] 增加请求 path、认证 Header 清理、portable/native Thinking、流式事件和 tool-call 转换测试。

### 阶段 D：Admin 与 UI（已完成）

- [x] 元数据下发新的 Offering/kind、region、profile、connection group，以及
      `{region,url,policy_revision}` 文档契约；fixed surface 不通过 Host 推导产品地域。
- [x] 凭据表单显示 MiniMax 国内外订阅选项；Kimi 按选定清单显示国内外或无地域单项。产品选择自动
      更新默认地址。
- [x] MiniMax 显示 2 地域 × 2 协议；Kimi 双地域门槛通过后也显示 2 地域 × 2 协议，否则只按单地域
      降级清单显示无地域产品。所有 offered 连接都是独立选项；withheld 项不进入元数据或 UI；部署
      表单只显示上游可请求模型。
- [x] 提交并校验 `acknowledged_policy_revision`；版本漂移清除确认并返回具名错误。
- [x] 复用条款折叠 UI，增加中英文文案及所有新错误码本地化。

### 阶段 E：用量、失败和发布（已完成；真实 smoke 未获授权）

- [x] 为 `account_region_id`、`provider_failure_reason` 推进持久化 schema，覆盖订阅 → 按量 Route
      fallback 的 Offering/Profile/账号地域归因。
- [x] 覆盖 unknown price、预算关闭提示、Halro 本地观测计数和 Dashboard 非零成本/剩余额度误导防护。
- [x] 更新 compatibility manifest、provider real matrix、operator guide 和嵌入式 Web bundle。
- [x] 更新 `web/src/test/provider-profiles.golden.json` 和 endpoint manifest golden；完成针对性测试后，
      推送前运行一次前端 typecheck/全测/production build 与 `go test -count=1 ./...`，并执行
      `git diff --exit-code -- internal/webui/dist` 检查已生成 bundle 无未纳入的漂移。
- [x] 真实 Provider smoke 默认 skip，只在操作者显式提供专用测试凭据时运行。

### 本次实现复核与验证

实现完成后按领域边界、适配器、Admin/UI、持久化与兼容声明重新复核，并修正了以下问题：

- withheld 的 Kimi Code 与 MiniMax Anthropic Profile 不再被原生 Anthropic endpoint manifest 误报为
  当前可服务；解除 withheld 时必须同时补齐 native schema、路由和兼容性证据；
- 订阅 403 在没有稳定结构化 code 时统一保持 canonical reason 为空，不从上游 message 猜测“额度耗尽”；
  是否可重试及是否 ambiguous 保留适配器基于请求执行阶段给出的证据（完整 HTTP 403 当前为不可重试且
  不 ambiguous）；
- by-endpoint 凭据轮换增加账号地域不可变校验：已知地域不能跨区，也不能在已知与未知地域之间切换；
- 补齐 MiniMax 订阅 surface 与独立协议选项的中英文名称，删除 withheld Kimi 产品的不可达 UI 文案；
- fixed-region 产品使用企业代理时，凭据和连接表单都显示“产品地域保持、端点地域未验证”；识别出另一
  官方地域时则就地阻止提交；
- 存量 withheld 连接与凭据保持可查看、可删除但不可重新启用、编辑、测试或轮换，启动加载也不会为其
  创建 adapter；
- `FailureSemanticsRecorded` 区分“明确不可重试/未执行”与旧记录“未采集”，并贯通 Ledger、失败捕获、
  Usage API、Parquet schema 8 和控制台；schema 7 回归使用物理上确实缺少新列的历史行；
- policy revision 冲突会展开条款、等待最新目录并重新聚焦确认框；刷新失败时保持 fail-closed；
- MiniMax Subscription 与 Kimi Code 的字段规则、兼容性文案和成熟度不再继承按量产品证据；永久
  Profile → ConnectionGroup、Surface → Offering/RegionScope/Region/Host 映射由迁移测试固定；
- Kimi Code `k3` 不再静态声明只对高档套餐成立的 1M 上下文窗口；MiniMax entitlement 部署同样显示
  订阅成本治理提示；
- Ledger 保持旧 fixed-region 事件可回放，但新 append 必须携带准确账号地域；前端与服务端统一按
  hostname（含非默认端口场景）识别官方跨区端点；Provider 连接也覆盖条款 revision 刷新成功与失败。

验证结果：前端 typecheck、44 个测试文件（583 项测试）、production build、bundle/artifact 检查通过。
Go 完整门禁中除更新后的 `provider-profiles` golden 漂移外所有包通过；审阅并更新 golden 后，该生成测试
以更新模式和普通模式各通过一次。按仓库“只运行能看到变化的测试”规则未重复已成功的耗时包。新增的
withheld 启动隔离、schema 7 真实缺列、失败语义、地域与兼容性测试均通过。未运行任何真实或计费
Provider smoke。

## 12. DeepSeek 激活门槛

只有以下条件全部满足，才能把 DeepSeek 候选变成可达产品：

1. DeepSeek 一手资料明确产品名称和订阅计费边界；
2. 存在与按量 API Key 不同的官方凭据，或官方明确同一 Key 如何选择订阅额度；
3. 公布第三方工具可调用的 Base URL、协议和允许用途；
4. 明确国内/海外是独立账号边界还是同一产品；
5. 真实账号验证最小调用、模型枚举、额度耗尽、过期和产品不匹配响应；
6. 法务/使用条款允许 Halro 这类网关，而不要求伪造特定客户端身份。

满足后复用本方案的 Offering → Surface → Scheme → Profile 模板；不满足时 UI 只显示
`deepseek.api-platform`。文档监控可以记录候选状态，但不能预注册常量或 disabled 假选项。

## 13. 验收标准

1. MiniMax 显示按量 + Subscription Access × 国内/海外；Kimi 只有在 Halro 身份及对应协议门槛满足
   后才显示 Code 订阅项。双地域证据不足时只能显示无地域的单 surface 产品，不得显示国内/海外；
   身份门槛不足时整个 Kimi Code Offering/Profile 不进入 Admin 元数据或 UI。
2. MiniMax 及通过门槛后的 Kimi，每个 offered 地域 × 协议选项都有正确标签、Base URL、surface、
   scheme、ConnectionGroupID 和保存结果；OpenAI/Anthropic 都可独立选择，不被合并或误标
   route-partitioned。单地域降级清单另覆盖 `RegionNone × 2 协议`，但 withheld 时不展示。
3. 选择任一订阅项后保存 exact surface/scheme，不能被 provider type 默认 Profile 覆盖；一把同 origin
   凭据可以建立两个协议连接，但每个连接只绑定选择的协议 group。
4. 通用 API Key、订阅 Key、错误产品 Key、MiniMax 跨地域 Key 均有回归测试；订阅 Key 无法绑定按量
   连接，按量 Key 无法绑定订阅连接。
5. MiniMax 国内 surface + `api.minimax.io`、海外 surface + `api.minimax.cn` 均被拒绝；fixed surface
   使用企业代理仍保留产品地域和对应条款，只把 endpoint 标成“地域未验证”。
6. 采用 Kimi 双地域清单时，同 Host 的两个地域不会在 Domain/Admin/Web 被折叠或推导成唯一地域；
   凭据、连接、用量和审计仍能区分 `account_region_id`。采用单地域清单时只存在 `RegionNone`，不得
   生成伪地域。
7. 切换 Offering、地域或 Profile 后，旧模型选择、能力检测结果、binding 和能力表单状态全部失效并
   重置，不能带到新的协议或产品。
8. 模型列表来自对应订阅 endpoint；按量模型不会自动进入订阅目录。MiniMax 两种枚举 route/header/
   shape 和 Kimi seed/枚举决策均有 fixture 测试。
9. Chat、Streaming、Tools、Reasoning 只有在该模型自己的证据支持时才可开启；每个 Anthropic Profile
   覆盖 portable/native、流式/非流式、Thinking disabled/显式 reasoning。
10. 条款区域默认收起，未确认提交会展开并聚焦；文档包含地域、URL、revision。旧 revision 提交被
    拒绝并要求重新确认。凭据审计不含虚构 Profile，连接审计包含真实 Profile。
11. 401/402/403/429、稳定 provider code、无 code、普通响应/SSE 和 `Retry-After` 都命中 §8 的同一
    canonical mapping；只有被明确标记 retryable 的失败触发配置好的 Route fallback。
12. 订阅额度耗尽不会在同一连接内静默切换按量 Key；显式 fallback 正确归因 Offering、Profile 和
    account region。MiniMax/Kimi 在同一产品 Key 内使用 Credits/Extra Usage 时不伪造具体余额来源。
13. Dashboard 把本地观测请求数与上游剩余额度明确区分，unknown 成本不显示为 `$0`。
14. DeepSeek 在激活门槛未满足前不出现订阅选项，也不被描述为“待 UI 开关”的已实现功能。

## 14. 预计修改范围

- `internal/domain/provider_offering.go`
- `internal/domain/provider_connection.go`
- `internal/domain/provider_profile.go`
- `internal/domain/provider_table.go`
- `internal/provider/profile_bindings.go`
- `internal/app/provider_adapters.go`
- `internal/app/admin_audit_intent.go`
- `internal/modelcatalog/builtin.go` 或新的订阅目录文件
- `internal/app/admin_provider_profiles.go`
- `internal/app/admin_providers.go`
- `internal/app/admin_deployments.go`
- `internal/gateway/`、`internal/ledger/`、`internal/usage/`
- `internal/failurecapture/failurecapture.go`
- `internal/compatibility/manifest.go`、`provider_fields.go`
- `web/src/hooks/useProviderProfiles.ts`
- `web/src/pages/ProvidersPage.tsx`
- `web/src/pages/DeploymentsPage.tsx`
- `web/src/pages/UsagePage.tsx`
- `web/src/types.ts`
- `web/src/i18n/locales/zh-CN.ts`、`en-US.ts`
- `web/src/i18n/errors.ts`
- `web/src/test/provider-profiles.golden.json`
- 上述 Domain、Admin、Gateway、Usage 与 Web 页面对应的测试文件
- `docs/compatibility/endpoint-manifests.json`
- `docs/verification/provider-real-matrix.md`
- `internal/webui/dist/`

## 15. 官方资料

以下资料均于 2026-09-10 复核：

- [Kimi Code 产品概览](https://www.kimi.com/code/docs/)
- [Kimi Code 模型配置](https://www.kimi.com/code/docs/kimi-code/models.html)
- [Kimi Code 常见问题](https://www.kimi.com/code/docs/kimi-code/faq.html)
- [Kimi Code Error Reference](https://www.kimi.com/code/docs/en/kimi-code/error-reference.html)
- [Kimi Code Server API](https://www.kimi.com/code/docs/en/kimi-code-cli/reference/server-api.html)
- [MiniMax 国内 Token Plan 概要](https://platform.minimaxi.com/docs/token-plan/intro)
- [MiniMax 国内 Token Plan 快速接入](https://platform.minimaxi.com/docs/token-plan/quickstart)
- [MiniMax 国内 OpenAI 模型枚举](https://platform.minimaxi.com/docs/api-reference/models/openai/list-models)
- [MiniMax 国内 Anthropic 模型枚举](https://platform.minimaxi.com/docs/api-reference/models/anthropic/list-models)
- [MiniMax 海外 Token Plan Overview](https://platform.minimax.io/docs/token-plan/intro)
- [MiniMax 海外 Token Plan Quick Start](https://platform.minimax.io/docs/token-plan/quickstart)
- [MiniMax 海外 OpenAI 模型枚举](https://platform.minimax.io/docs/api-reference/models/openai/list-models)
- [MiniMax 海外 Anthropic 模型枚举](https://platform.minimax.io/docs/api-reference/models/anthropic/list-models)
- [DeepSeek 模型与计费](https://api-docs.deepseek.com/quick_start/pricing)
- [DeepSeek Codex 接入](https://api-docs.deepseek.com/quick_start/agent_integrations/codex/)
