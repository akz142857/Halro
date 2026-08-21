# Amazon Bedrock Mantle 接入评估与开发计划

状态：Phase 0～2 与验收补齐已实现并经代码核实；Phase 3 的 harness 已交付，真实调用未执行
日期：2026-08-12（2026-08-13 归档时按当时代码复核）
范围：`internal/domain`、`internal/provider/{bedrock,bedrockmantle,openai,anthropic}`、
`internal/app`、`internal/gateway`、`internal/compatibility`、`internal/store`、`web`、
`docs/adr/0007-bedrock-mantle-profiles.md`、`docs/verification/provider-real-matrix.md`

> 位置：本文原在 `docs/todo/`，Phase 0～2 与验收清单全部关闭后迁到这里。它不再是待办，
> 而是这条接入面为什么长成现在这样的记录——改动 Mantle 的 wire contract、能力上限或
> Project 寻址时应当同步更新本文。
>
> 归档时仍未关闭、且**不由本文继续跟踪**的两项：
>
> - **Phase 3 的真实 Mantle 调用**未执行，需要真实 AWS 账户与一次明确的计费授权。
>   它的归属是 [`verification/provider-real-matrix.md`](../verification/provider-real-matrix.md)——
>   那里写明三个 profile 在任何 commit 上都没有真实证据，以及获授权后怎么跑、证据要记哪些字段。
> - **§7「后续独立任务」的四项**从一开始就声明为独立立项，不属于本文范围。其中通用
>   Credential expiry 已部分落地，见 §4.3 的归档订正。

> 本文评估的是 **Amazon Bedrock 的 `bedrock-mantle` 访问面**，不是 Claude Platform on AWS。
> 初始输入截图包含了属于另一产品线的 `anthropic-workspace-id` 信息，不能继续作为
> Bedrock Mantle wire contract 的依据。本文以 AWS 当前官方文档和本仓代码为准。
>
> 本轮没有执行真实 Provider 调用。真实调用可能计费，必须由用户显式授权后再运行。

---

## 0. 结论与建议裁决

Mantle 不是从零接入：三个 profile 已注册并接入适配器。但原评估的两个核心前提错误：

1. `anthropic-workspace-id` 属于 Claude Platform on AWS；Bedrock Mantle 使用
   `anthropic-workspace`（Messages）和 `OpenAI-Project`（Chat/Responses）。Bedrock
   账户已有 default project/workspace，省略资源头的请求归入 default。因此当前路径不是
   “可能 100% 不可用”，准确口径是：**default project 可用但未经真实账户验证，非 default
   project 无法显式寻址**。
2. Mantle 默认能力 ceiling 目前不是编译期保证。Admin 可以声明超出默认 ceiling 的
   binding 能力，适配器又直接使用 `binding.Capabilities`。这是现有 fail-closed 契约缺口，
   应先于任何 Mantle 增强修复。

建议裁决如下：

- **当前三个 v1 profile 继续只支持 Bedrock API key**，不把默认 AWS 凭据链塞进数据面。
- **当前版本只承诺账户 default project**；在 Operator Guide 与发布说明中写清限制。
  （Phase 2 之后此条放宽为：Provider 可显式指定一个 Bedrock Project，仍无请求级 project。）
- 若要支持显式非 default project，第一版采用可选的 **Provider 级 `BedrockProjectID`**：一个
  Provider 对应一个 Bedrock Project，多 Provider 可以复用同一 Credential。暂不做
  Deployment 级多 Project。
- **SigV4、自动刷新短期凭据、通用 Credential expiry** 分别立项，不作为 BedrockProjectID
  支持的顺手改动。
- 如果 1.0.0 继续携带 Mantle Beta profile，必须在发布前修复能力 ceiling；否则应禁用
  这些 profile。显式 Project、SigV4 和自动刷新不阻塞 1.0.0。
  （**该修复已完成，见 §7 Phase 0。**）

---

## 1. 产品边界与官方契约

### 1.1 不要混淆两条 AWS 产品线

| 项 | Amazon Bedrock Mantle | Claude Platform on AWS |
|---|---|---|
| 服务端点 | `bedrock-mantle.<region>.api.aws` | `aws-external-anthropic.<region>.api.aws` |
| 运营方 | AWS / Amazon Bedrock | Anthropic on AWS |
| Anthropic 资源头 | `anthropic-workspace` | `anthropic-workspace-id`（必填） |
| OpenAI 资源头 | `OpenAI-Project` | 不适用 |
| SigV4 service | `bedrock-mantle` | `aws-external-anthropic` |

参考：

- [Amazon Bedrock Workspaces (Anthropic-compatible)](https://docs.aws.amazon.com/bedrock/latest/userguide/workspaces.html)
- [Amazon Bedrock Projects (OpenAI-compatible)](https://docs.aws.amazon.com/bedrock/latest/userguide/projects.html)
- [Amazon Bedrock Mantle Responses API](https://docs.aws.amazon.com/bedrock/latest/userguide/bedrock-mantle.html)
- [Amazon Bedrock Messages API](https://docs.aws.amazon.com/bedrock/latest/userguide/inference-messages-api.html)
- [Claude Platform on AWS Workspaces](https://docs.aws.amazon.com/claude-platform/latest/userguide/workspaces.html)

上表的两个资源头名、default 归属行为、三条请求路径，以及 Claude Platform 的
`anthropic-workspace-id` 必填要求，均已于 2026-08-12 对上述文档页逐条核对。
本节其余推断若与后续文档更新冲突，以文档为准并回改本文。

### 1.2 Bedrock Mantle 已能由官方文档确定的事实

| Profile | 请求路径 | API-key 认证 | 显式资源头 |
|---|---|---|---|
| `bedrock.mantle.chat.v1` | `/v1/chat/completions` | `Authorization: Bearer` | `OpenAI-Project` |
| `bedrock.mantle.openai.chat.v1` | `/openai/v1/chat/completions` | `Authorization: Bearer` | `OpenAI-Project` |
| `bedrock.mantle.responses.v1` | `/v1/responses` | `Authorization: Bearer` | `OpenAI-Project` |
| `bedrock.mantle.openai.responses.v1` | `/openai/v1/responses` | `Authorization: Bearer` | `OpenAI-Project` |
| `bedrock.mantle.anthropic.messages.v1` | `/anthropic/v1/messages` | `x-api-key` | `anthropic-workspace` |

Workspaces 与 Projects 是同一种 Bedrock Project 资源在不同协议中的名称。每个账户有
default project/workspace；省略资源头时，请求关联到 default。因此上述 host、path、API-key
头名和 default 行为不再列为“必须付费抓包才能裁决”的未知数。真实账户 smoke 仍要验证
Halro 的具体实现是否符合契约，但不能用单次 smoke 代替协议设计。

### 1.3 官方文档还确定了这些，都影响设计

以下事实同样来自上述 AWS 文档页，取得成本为零，但会改变 Phase 2 的形状：

- **Project ID 有可校验的形状**：Bedrock project ID 为 `proj_` 前缀加字母数字，另有字面量
  `default`；Claude Platform on AWS 的 workspace ID 是 `wrkspc_` 前缀。二者不可互换。
- **Project 是 region 作用域资源**：project ARN 形如
  `arn:aws:bedrock-mantle:<region>:<account>:project/proj_...`。project ID 与 endpoint region
  绑定，不一致必然失败——这是 §5.3 Region 一致性的直接依据。
- **归档 project 不能用于新推理**：官方原文为 archived project “cannot be used for new
  inference requests”，历史数据保留 30 天。失败发生在**请求时**而非保存时。
- **长期 API key 默认策略只允许 get/list projects**，创建/更新/归档需要额外 IAM 策略。
  这限制了任何“保存时在线校验 project 是否存在”的方案。
- **长期 key 就是 IAM 的 service-specific credential**：
  `aws iam create-service-specific-credential --service-name bedrock.amazonaws.com`，
  权限等于该 IAM 用户已附加的策略。另有独立的 IAM action `bedrock-mantle:CallWithBearerToken`
  控制“能否用 bearer token 走 Mantle 端点”，与推理权限是两回事——运维排障时要分开看。
- **控制面与数据面同 host**：project 管理走同一个 `bedrock-mantle.<region>.api.aws` 上的
  `/v1/organization/projects`。Halro 只使用数据面路径，控制面路径不得被网关暴露或代理。
- 每账户最多 1000 个 project。

---

## 2. 仓库现状

### 2.1 已接通的部分

三个 Mantle profile 均已注册：

| Profile ID | 适配器 | 默认能力 ceiling（`internal/domain/models.go:523-530`） |
|---|---|---|
| `bedrock.mantle.chat.v1` | `openai` | Chat/Streaming/Tools/Vision/JSONMode/DeveloperRole/Reasoning/StreamUsage |
| `bedrock.mantle.openai.chat.v1` | `openai`（`OperationPathPrefix=openai/v1`） | 同上 |
| `bedrock.mantle.responses.v1` | `bedrockmantle.ResponsesAdapter` | 同上但无 Reasoning |
| `bedrock.mantle.openai.responses.v1` | `bedrockmantle.ResponsesAdapter`（`OperationPathPrefix=openai/v1`） | 同上但无 Reasoning |
| `bedrock.mantle.anthropic.messages.v1` | `anthropic` | Chat/Streaming/Tools/Vision/Reasoning/StreamUsage |

已有边界：

- `bedrockmantle.ValidateEndpoint` 只接受 HTTPS origin
  `bedrock-mantle.<region>.api.aws`，拒绝 path/query/fragment/user-info 和非默认端口；
- `SurfaceBedrockMantle` 当前只接受 `CredentialBedrockAPIKey`；
- 三个 profile 在 compatibility 层分别按 Chat、Responses、Anthropic Messages 分类；
- Responses 显式发送 `store:false`，不导入 AWS 默认的 30 天 stateful response 所有权；
- API key 绑定精确 Mantle endpoint audience，不能附着到 Bedrock Runtime Provider。

### 2.2 必须先修复：immutable capability ceiling 实际可被放宽

原文关于“数据面不用 `binding.Capabilities`、ceiling 是编译期保证”的结论不成立：

- `internal/app/admin_providers.go:918-950` 只对 `isStrictOperationProfile` 返回 true 的
  profile 校验默认上限；`isStrictOperationProfile`（`:984-990`）没有三个 Mantle profile；
- `web/src/pages/ProvidersPage.tsx:639-647` 的固定能力 profile 列表也没有 Mantle；
- `internal/app/providers.go:580` 直接把 `binding.Capabilities` 转成适配器能力，并在
  `:617`、`:625`、`:633` 传给三个 Mantle 适配器；
- capability detection 又从适配器能力生成可能计费的探测计划。

因此 Admin/API 可以保存超出 `DefaultProviderCapabilitiesForProfile` 的 Mantle 能力，随后影响
请求预检和能力探测。这条修复是 Phase 0：

1. 三个 Mantle profile 加入后端 strict profile 和前端 fixed profile；
2. 不把安全约束只留在 Admin：在 `ProviderProfileBinding.Validate` /
   `ProviderInstance.Validate` 强制 immutable profile 能力是默认 ceiling 的子集；
3. 适配器构造时再次与默认 ceiling 求交或拒绝越界旧记录，防迁移/非 Admin 写入绕过；
4. 覆盖 Admin API 越界、旧记录激活、capability detection 计划和数据面 preflight 测试。

**第 3 条的失败粒度必须写死：越界的旧 binding 走既有的 withholding 路径**——像
`providers.go` 处理 `withheldBindingUnavailable` 那样把该 deployment 摘出候选并计入
`report.Dangling`，其余路由照常加载。**不是让进程起不来。** fail-closed 指的是这条路径
不再被流量使用，不是把一条陈旧记录升级成全局启动失败；后者会让一个 Mantle 配置错误
带走所有其他 Provider 的路由。Admin 写入路径则相反，必须直接拒绝保存。

---

## 3. Project / Workspace 寻址设计

### 3.1 当前精确能力（Phase 2 之后）

Provider 可选填 `BedrockProjectID`：留空不发资源头、归入账户 default project；填写则按
协议渲染 `OpenAI-Project` 或 `anthropic-workspace`。`anthropic-workspace-id` 仍然
不得出现在任何 Mantle 请求上——它属于另一条产品线，代码里只用于删除。

仍然成立的限制：两者都没有真实账户证据；project 是 Provider 级而非请求级。

### 3.2 推荐的第一版：Provider 级 `BedrockProjectID`

`ProviderProfileBinding` 在同一 Provider 下不能重复 profile，不能充当可重复的 Project 维度：

- `internal/domain/models.go:431` 禁止 profile 重复；
- binding ID 是 `providerID + ":" + profileID`；
- `matchingBindingID` 遇到同 profile 多 binding 会失败关闭。

为避免与 Halro 自身的 `Project` 混淆，推荐增加可选、类型化的
`ProviderInstance.BedrockProjectID`：

- 空值：显式表示使用 AWS default project，不发送资源头；
- 非空：OpenAI Chat/Responses 渲染 `OpenAI-Project`，Anthropic Messages 渲染
  `anthropic-workspace`；
- 一个 Provider 只指向一个 Project；需要多个 Project 时创建多个 Provider，并复用同一
  Credential；Provider 各自保留并发、熔断、证据和运维状态；
- `BedrockProjectID` 作为不透明标识符处理，不写入日志、错误、Metrics 或真实证据；
  Admin list/detail 是否展示脱敏值需单独定义；
- 校验规则按 §1.3 的形状事实收紧：只接受 `proj_` 前缀加字母数字，或字面量 `default`
  （后者等价于留空，建议直接规范化为留空）。**显式拒绝 `wrkspc_` 前缀**——那是
  Claude Platform on AWS 的 workspace ID，粘错产品线的 ID 是这一整节最可能的人为错误，
  而前缀检查是最便宜的一道闸；
- **不做保存时在线校验**：按 §1.3，长期 key 的默认策略只允许 get/list projects，
  在线校验会让 Provider 保存路径依赖一次上游调用，且权限结果因 key 类型而异。
  project 是否存在、是否已归档，一律在请求时暴露。

该方案不是“零 schema 改动”：涉及 domain、Admin API、Provider 表单、i18n、三个适配器
和文档。**但它不需要 bump format version，也不需要重建数据目录**——这一条是实现时纠正的：
新字段缺失即"账户 default project"，而这正是该字段存在之前每一条记录的真实含义，没有任何
已存字节改变解释。pre-1.0 的版本号规则针对的是"陈旧状态会被误读"，这里不会。

### 3.3 请求头落点

Project 是资源寻址，不是凭据。不要把它塞进 `StaticHeaderAuthorizer`，也不要引入
`map[string]string` 自由头。

实现应使用类型化请求元数据/装饰逻辑，并由 profile 映射成正确 wire header：

1. 构造请求并设置 Content-Type、Project 等协议头；
2. 最后执行认证；未来若使用 SigV4，签名器才能看到真实 payload 和需要签名的头；
3. 明确删除与当前 profile 冲突的资源头及认证头，防调用方注入覆盖；
4. Chat、Responses、Messages 的 stream/non-stream 和 Probe 均走同一规则。

### 3.4 暂不选择 Deployment 级

只有产品明确要求“同一 Provider 在请求级跨多个 Bedrock Project 路由”时才把 BedrockProjectID 放到
Deployment/Target。该方案必须把 BedrockProjectID 穿过 semantic operation 与适配器调用接口，且要
重新定义 Project 与 Route、Halro Project、预算、成本归属和 fallback 隔离语义。当前不需要为
这个未确认需求支付复杂度。

---

## 4. 认证与凭据生命周期

### 4.1 当前裁决：v1 profile 保持 API-key only

当前 `SurfaceBedrockMantle` 只接受 `CredentialBedrockAPIKey`，且已有负面测试拒绝 SigV4。
这与 ADR 0007 的 Phase 1C 决策一致。Operator Guide 和 UI 必须说明：AWS 控制台若默认展示
IAM，Halro 当前应选择 API key 路径。

### 4.2 SigV4 必须另立 profile/安全评审

不能只复用现有 `CredentialAWSSigV4Explicit` 或把 `CredentialScheme` 从单值改成集合：

- 现有 Bedrock signer 只接受 Runtime/Agent Runtime host 和对应 signing service；
- Mantle signing service 是 `bedrock-mantle`，authority 与 action/resource 规则不同；
- OpenAI、Responses、Anthropic 当前 POST 路径均以 `Authorize(request, nil)` 调用认证器，
  且在认证后才设置 Content-Type；现有 signer 会因此签名空 payload；
- 默认凭据链会引入环境变量、shared credentials、容器/实例 metadata 与刷新端点选择，触发
  新的威胁评审。

若后续支持 SigV4，必须使用新的 profile revision/ID，并交付：

- Mantle host、region、service scope 与 IAM least-privilege 规则；
- 显式会话凭据和 session token 生命周期；
- 真实 payload hash、签名前 header 顺序和 canonical request 测试；
- wrong region、expired token、AccessDenied、clock skew、refresh failure 测试；
- 对默认链、IMDS/ECS endpoint、代理、重定向和 SSRF 边界的独立安全裁决。

### 4.3 `Credential.ExpiresAt` 不是短期 key 自动刷新

Bedrock 短期 API key 的有效期已于 2026-08-12 对官方 `api-keys.html` 核实：取
**12 小时**与**生成它的 IAM 会话时长**两者中更短者，且只在生成它的 region 有效，
权限继承自生成它的 IAM principal。AWS 推荐生产负载使用短期凭据。当前
`StaticHeaderAuthorizer` 在 topology 构造时固定秘密，因此 `ExpiresAt` 只能支持提示和人工轮换，
不能把 Halro 变成生产级自动刷新客户端。

通用 Credential expiry 应独立立项并先定义：

- expiry 是 operator-declared 还是 Provider-verifiable；未知 expiry 的语义；
- warning/critical/expired 阈值、时钟偏差和过期后是告警还是拒绝流量；
- 更新 Credential 后 authorizer 的原子替换与旧秘密清零；
- schema version、backup/restore、doctor、Admin/API、Audit 和 runbook；
- Metrics 使用有界 `expiry_state` 聚合。`credential_id` 仍是无界集合，不能声称它避免高基数；
  具体 ID 只在 Admin/doctor 的授权输出中展示。

〔2026-08-13 归档订正〕上面这段写的是"应独立立项"，但其中前两条已在 `237a1e9` 落地，
本文归档时必须说清落到哪儿了，否则读者会以为整块都还没动：

- `domain.Credential.ExpiresAt`（`internal/domain/models.go:112`，可选指针）已存在，
  裁决取 **operator-declared + 纯提示**：注释明写"the gateway does not refuse traffic on it,
  because it is a typed-in date rather than anything the upstream told us"。这同时答掉了
  第一条（operator-declared，不做 Provider-verifiable）和第二条的后半（过期后不拒绝流量）。
  Admin 侧有 `internal/app/admin_credential_expiry_test.go` 守护。
- **仍未做**：warning/critical 阈值与时钟偏差、doctor、Audit、runbook，以及有界
  `expiry_state` 指标——`grep -rn 'expiry_state' internal/` 零命中，
  `internal/app/doctor.go` 与 `internal/audit/` 也不认识 `ExpiresAt`。

因此"通用 Provider Credential expiry/rotation"这个独立立项仍然成立，只是范围缩小为
可观测性与生命周期那一半；字段本身与它的提示语义已经定案，不必重开。

---

## 5. 已由代码定案的行为

### 5.1 401/403 不会触发 fallback

三个适配器把 401/403 映成 `provider.ErrorAuthentication`，且 `Retryable` 为 false。
`gateway.retryable` 只重试明确 retryable、malformed 或 provider 5xx，并对认证错误立即终止。
客户端最终收到 502 `provider_authentication_error`，但 **502 映射不是 fallback 决策依据**。

因此原文“401 是否会静默切到备用 Deployment”无需真实账户调查，结论是不会。仍需补回归测试：

- Chat、Responses、Messages，各自 stream/non-stream；
- 401 与 403；
- 只产生一次 attempt、备用 Deployment 调用数为零；
- 客户端错误稳定为 `provider_authentication_error`；
- Usage/Attempt 记录 `error_class=authentication`，不保存 Provider 错误正文。

### 5.2 capability detection 不更新 Deployment connection-test 状态

Capability detection 会把认证错误分类为 `ProbeUnauthorized`，但不会设置
`Deployment.LastTestStatus`；现有测试明确要求 detection 创建的 Deployment 仍保持空状态。
文档和 UI 不得把 detection 结果描述成 connection-test 留痕。

### 5.3 Region 必须与 Mantle endpoint 一致

`deploymentRegion`（`internal/app/admin_deployments.go:745-749`）在显式 `Region` 非空时直接返回它，
与 `providerRegion` 从 endpoint host 派生出的值没有任何交叉校验。

这不只是标签不一致：按 §1.3，Bedrock project 是 region 作用域资源，其 ARN 内含 region，
project ID 与 endpoint region 不一致必然失败。模型可用性、能力证据和配额同样具有 region 语义。
因此必须二选一：

- Mantle Deployment 的 Region 只读并始终从 Provider endpoint 派生；或
- 保存时强校验显式 Region 与 endpoint region 完全一致。

create、update、restore、catalog lookup 和 capability evidence 都必须执行同一规则。

---

## 6. 真实 Provider 证据计划

### 6.1 静态契约测试优先

先依据官方文档和本地 fake server 固定以下契约，不产生费用：

- 三个路径、两种 API-key 头和两种 Project 头；
- default（省略头）、显式 Project A/B、非法/无权 Project；
- Project 头不能覆盖认证头，错误协议头必须被清除；
- non-stream、stream、usage、工具、视觉、JSON、reasoning 的请求渲染边界；
- 2xx、401、403、429 + Retry-After、5xx、timeout、malformed/oversize、request ID；
- Responses 的 completed、incomplete、failed/error、缺失终态以及有无 `[DONE]`；
- Anthropic Probe 实际发送 `max_tokens=1`，UI/文档标记其可能计费；OpenAI/Responses 的
  `/v1/models` Probe 单独验证。

### 6.2 真实 smoke 的证据粒度

一次调用不能证明三个 profile 的全部能力。执行身份必须至少绑定：

`exact commit × region × profile × exact model × authentication × project mode`

并分别验证：

- profile 与模型的 API compatibility；
- non-stream 与 stream；
- profile 声明的 tools、vision、JSON/developer role、reasoning、stream usage；
- default project 和显式 Project（若该功能已实现）；
- 结构化稳定错误与 retry/request-ID 行为。

某个模型不支持某能力不能直接证明整个 profile 不支持；单个模型支持也不能证明所有 Mantle
模型支持。能力结果只能进入对应 model/profile/region 的 Deployment evidence。

### 6.3 安全执行要求

真实 smoke 只能在用户显式授权后执行，并遵循：

- 专用、最小权限、限额凭据和明确调用上限；
- 优先进程内受控 Transport，不用会记录敏感头和 body 的第三方 HTTP 代理；
- 原始 trace 只存本机临时 0600 文件、限时保留，不提交仓库；
- canary 测试证明清洗器会移除 API key、account ID、Project ID、原始 model ID、endpoint、
  request ID、prompt、response 和错误正文；
- 为遵守现有 real-matrix 的安全证据契约，共享证据不保存原始 model ID；使用绑定执行身份的
  target digest/受限 custody record 保留可复核性；
- 安全证据只归档 exact commit、profile、region、target digest、操作、计数和归一化结论；
- 临时/限流/5xx 不得记录为 unsupported。

`docs/verification/provider-real-matrix.md` 和 runner/adapter smoke 必须先扩展到 Mantle，不能用
手工截图替代可复现证据。

---

## 7. 开发拆分与验收

### Phase 0：修复能力 ceiling（发布前必须完成）—— 已实现

触及：`domain`、`app`、`web`。

落地形状：

- `domain.IsImmutableCapabilityProfile` 是这份名单唯一的拼写，domain、Admin 与
  registry loader 共用；三个 Mantle profile 已加入；
- `ProviderProfileBinding.Validate` 与 `ProviderInstance.Validate`（无显式 binding 的
  legacy 投影分支）强制 binding 能力是默认 ceiling 的子集。这是 `PutProvider` 必经的
  边界，因此 Admin API 与任何直接 store 写入一起被挡住；
- registry loader 在构造适配器之前检查同一条不变量，越界的陈旧 binding 计入
  `capability_ceiling_exceeded` 并被摘出候选——**不是让进程起不来**；
- `app/admin_providers.go` 里重复的 `isStrictOperationProfile` 已删除，不留第二份名单；
- 前端 `isStrictCapabilityProfile` 补齐三个 profile，表单不再渲染可勾选的能力网格。

验收（均有测试，且已反向验证：撤掉修复后对应测试失败）：

- `internal/domain/capability_ceiling_test.go`：三个 Mantle profile 越界被拒、
  等于 ceiling 与收窄被接受、legacy 投影越界被拒、名单成员资格；
- `internal/app/bedrock_mantle_capability_ceiling_test.go`：Admin API 越界返回 400、
  ceiling 本身仍可保存；直接改写 bbolt 记录制造的陈旧越界 binding 被 withheld 而进程
  正常启动；
- `web/src/pages/ProvidersPage.test.tsx`：三个 Mantle profile 显示固定能力且无勾选框，
  Converse text 仍可勾选。

越界能力既进不了 store，也进不了 registry，因此不会出现在 detection 计划里，也不会
触发 Provider I/O。

### Phase 1：default project 口径与非计费验证 —— 已实现

触及：`internal/app`、三个 provider 包、`internal/gateway`、ADR 0007、
Operator Guide、release notes、provider real matrix。

代码：

- Region 一致性落地为 `validateBedrockMantleRegion`（`app/admin_deployments.go`）——
  显式 Region 与 endpoint 派生 Region 不一致直接拒绝，留空仍从 endpoint 推导。
  选的是等值校验而不是只读字段，运维仍可显式写出 region，只是不能写成另一个。

契约测试（全部对 fake server，不产生费用）：

- 三个 profile 的路径与认证头，外加**任何 project/workspace 头都不发**的断言
  （`provider/openai`、`provider/bedrockmantle`、`provider/anthropic` 各自的 Mantle 用例）；
- Probe 计费语义：OpenAI 形态读单个模型元数据，Anthropic Messages 发 `max_tokens=1`
  的真实推理调用——两者对"测试连接要不要花钱"的答案不同，各自钉住；
- 401/403 归 `ErrorAuthentication` 且不可重试，网关层不 fallback、备用 deployment
  调用数为零、客户端稳定收到 `provider_authentication_error`
  （`gateway/service_test.go`）；
- Region 一致性：不匹配被拒、匹配与留空不被这条规则拦下（`app`）。

文档口径（无条件，不依赖真实账户结果）：

- ADR 0007 增补修订节：ceiling 此前并非真的不可变、Workspaces/Projects 是同一种资源
  且头名按协议不同、`anthropic-workspace-id` 属于另一条产品线、Region 一致性；
- release notes：Provider 表新增 Mantle 行；已知限制新增"Mantle 无真实账户证据"，
  并改写原第 7 条（它写的"Admin 可以存超出上限的声明"在 Phase 0 之后不再成立）；
- `docs/verification/provider-real-matrix.md`：明写三个 profile 在任何 commit 上都
  没有真实证据，并列出被 fixture 钉住的是哪些事实；
- `docs/guides/aws-surface-selection.md`：新增「当前边界」节——API key only、
  default project only、Region 必须一致、能力集编译期固定。

未做（留给后续任务，不属于本阶段）：真实 smoke harness 与 `tests/provider-matrix`
的 Mantle 扩展，见 Phase 3。

### Phase 2：Provider 级显式 Project —— 已实现

裁决：采用文档推荐的 Provider 级 `BedrockProjectID`，不做 Deployment 级。

落地形状：

- `domain.ProviderInstance.BedrockProjectID`，可选。空值即账户 default project，
  这也正是该字段存在之前所有记录的含义——**因此不需要 migration，也不需要重建数据目录**
  （原 §3.2 写的"必须 bump format version"在这里不成立：没有任何已存字节改变含义）；
- 校验 `domain.ValidateBedrockProjectID`：`proj_` + 字母数字、长度上限；字面量 `default`
  由 `NormalizeBedrockProjectID` 归一为空；`wrkspc_` 前缀按名拒绝；非 Mantle 访问面拒绝；
- 渲染由 `provider.ApplyBedrockProject` 单点负责：OpenAI 形态发 `OpenAI-Project`，
  Messages 发 `anthropic-workspace`，并在设置前清掉它认识的全部资源头（含
  `anthropic-workspace-id`——它在这份名单里只为被删除）。**不是自由 header map，
  也不走 credential authorizer**；
- 三个适配器均改为"协议头与资源寻址在先、认证在后"，即 §3.3 的顺序要求；
- Admin API、Provider 表单、i18n（zh-CN/en-US）、类型定义同步。

验收（含反向验证）：

- `domain`：ID 形状、`wrkspc_` 具名拒绝、`default` 归一、非 Mantle 面拒绝；
- `provider`：只发本协议的头、清掉全部已知资源头、永不发 `anthropic-workspace-id`、
  伪造的 `Authorization` 仍被 authorizer 清除；
- 三个适配器各自的 project 渲染（有值/空值）与凭据头不受影响；
- `app`：两个 Provider 共用一条 Credential、各自的 project 不串且重启后仍然如此；
  Admin 拒绝不可用 ID；Runtime 面拒绝该字段；
- **组合根的接线本身有测试**：`newProviderBindingAdapterWithClient` 从
  `newProviderBindingAdapter` 中拆出，可以对着假 transport 跑真实接线。拆分的直接原因是
  反向验证发现：把三处 `BedrockProjectID` 全删掉，当时**没有任何测试失败**——功能会存下
  project 却从不发送。

### Phase 3：真实 Mantle smoke —— harness 已实现，未执行

**没有发生任何真实调用。** 需要真实 AWS 账户与一次明确的计费授权。

已交付：

- `internal/provider/bedrockmantle/real_smoke_test.go`：三种 wire profile 各自跑
  非流式 + 流式，两者都必须报出 usage（Halro 记不了账的一次调用不算证据）；
  端点先过 `ValidateEndpoint`，所以 smoke 不可能在产品拒绝保存的 host 上通过；
  能力上限取 profile 的 ceiling，smoke 不会碰产品不声明的能力；
- `tests/provider-matrix`：新增 `-include-beta` 与独立的 `betaProfiles`。Beta 结果
  带 `tier: "beta"`，**永远不参与 GA 发布门禁**；不加该 flag 时仍输出
  `status: "not_run"` 行，避免"沉默被读成已覆盖"；
- 证据按 §6.2 绑定到 `region × wire_profile × authentication × project_mode` 与一个
  `target_digest`（`sha256(region, wire profile, auth, project mode, 精确模型)`）——
  可复核两次运行是否同一目标，但共享证据里不出现账户的模型权限；
- `docs/verification/provider-real-matrix.md` 写明运行方式、证据字段与"一次运行只证明
  一个格子"的口径。

未做：真实执行、以及执行后回填 real matrix 的证据行。

### 验收补齐（Phase 0～2 的清单剩余项）—— 已完成

前面几个 Phase 落地时，§2.2、§5.1、§6.1、§5.3、§3.2 的验收清单还有若干条没做完，
现已逐条补上：

- **§2.2 第 4 条**：`internal/provider` 断言三个 Mantle profile 的 detection plan 不会
  包含 ceiling 之外的探测（反向验证：让适配器声明 embeddings —— 即 Phase 0 之前的
  缺陷形状 —— 三个 profile 全部失败）；`internal/gateway` 断言越界请求在**任何
  Provider I/O 之前**被 400 `unsupported_feature` 拒绝，且适配器调用数为零。
  两条拒绝路径分别钉住：Responses 的 `reasoning_effort` 由 profile 字段清单拒绝，
  图片输入由能力过滤器拒绝。
- **§5.1**：Responses 与 Messages 两个适配器各自的 401/403 分类（此前只有 openai
  形态有）；三条协议的 **stream 路径**认证失败均为终态、不可重试、`Ambiguous=false`
  且未发出任何事件；**账本记录 `error_class=authentication` 且不含 Provider 错误正文**
  （用 `budget.Manager.AddObserver` 捕获 ledger 记录后断言）。
- **§6.1**：Mantle 状态矩阵（400/401/403/408/429/500/502/503/504）含 `Retry-After`
  与 `x-amzn-requestid` 透传、错误正文不进 message；**非法/无权/已归档 project** 的
  403 归入认证类；malformed body 与 `readLimited` 的 oversize 边界；
  **Probe 计费语义写进控制台**（选中 Messages 实现时给出计费提示，另两个实现不提示，
  有前端测试）与 Operator Guide。
- **§5.3**：`create`/`update` 走同一条 `deploymentFromInput` 校验；**restore 与
  endpoint 迁移**这条路补上 loader 侧兜底——region 不一致的 deployment 计入
  `region_mismatched` 并被摘出候选，进程照常启动（反向验证过）。
- **§3.2**：断言 `BedrockProjectID` 不进日志、错误正文、Prometheus、Audit；
  并把"Admin detail 返回明文"这个决定写进测试注释——脱敏值无法与 AWS 控制台核对，
  而读者已经是本实例的管理员。

### 后续独立任务

- Mantle SigV4 新 profile 与威胁评审；
- 可刷新短期凭据/workload identity；
- 通用 Provider Credential expiry/rotation——**部分已落地**：字段与"仅提示、不拒绝流量"的
  裁决已定案，剩余的阈值、doctor、Audit、runbook 与有界 `expiry_state` 指标仍未开工，
  见 §4.3 的归档订正；
- 若确有需求，再设计 Deployment 级多 Project。

这些任务在安全与生命周期设计冻结前不估实现工期。

---

## 8. 测试与发布门禁

迭代期间按 `AGENTS.md` 运行变更能影响的最小集合，并使用 `-count=1`：

- `internal/domain`：profile ceiling、schema validation；
- `internal/app`：Admin 写入、activation、Region、backup/restore；
- `internal/provider/{openai,anthropic,bedrockmantle,bedrock}`：请求头、签名、stream/error；
- `internal/gateway`：401/403 no-fallback 与 Usage error class；
- `web`：fixed capability UI、Project 表单与 i18n；
- 仅并发/刷新逻辑改动才对受影响包运行 `-race`。

最终 push 前只运行一次完整 gate；没有用户显式授权时绝不运行真实 Provider smoke。

1.0.0 发布门禁：

1. Mantle ceiling 修复通过；否则禁用三个 Mantle profile；
2. `docs/adr/0007-bedrock-mantle-profiles.md` 与最终范围一致；
3. `docs/milestones/release-notes-v1.0.0.md` 无条件披露 Mantle Beta 尚无真实账户证据；
4. 已知限制明确 API-key only、Bedrock Project 为 Provider 级（无请求级 project）、
   手工凭据轮换；
5. 不把 fixture、SDK 文档或截图冒充真实账户证据。

---

## 9. 明确禁止

- 不把 `anthropic-workspace-id` 加到 Bedrock Mantle；
- 不接受 `wrkspc_` 前缀的值作为 `BedrockProjectID`；
- 不暴露或代理 Mantle 控制面路径（`/v1/organization/projects`）——Halro 只使用数据面；
- 不在 Provider 保存路径上发起 project 存在性的在线校验；
- 不用自由 header map 实现 Project；
- 不把 Project 资源寻址混入 Credential authorizer；
- 不在旧 Mantle v1 profile 上静默增加 SigV4；
- 不引入未经威胁评审的默认 AWS 凭据链或 metadata endpoint；
- 不把 `Credential.ExpiresAt` 宣称为短期凭据自动刷新；
- 不使用 `credential_id` 等无界 Metrics label 并声称其低基数；
- 不从单模型/单区域 smoke 外推 profile 全局能力；
- 不在未获明确授权时运行计费真实 Provider 调用；
- 不提交真实凭据、account/Project ID、原始请求响应或 Provider evidence。
