# Amazon Bedrock Mantle 接入评估

状态：评估，待裁决
日期：2026-08-12
范围：`internal/provider/bedrockmantle`、`internal/provider/anthropic`、`internal/domain/provider_profile.go`、
`internal/app/providers.go`、`internal/compatibility`、`docs/verification/provider-real-matrix.md`

> 输入是 AWS 控制台 Bedrock Mantle "Getting started" 的四张截图（SDK 选择、环境配置、
> 快速测试、App integration）。**截图证明的是控制台今天提供什么，不证明我们这边的实现对不对**——
> 后者的每一条结论都标了 `file:line`，是本仓当前代码的事实。

---

## 0. 结论先说

**Mantle 不是"从零接入"，三个 profile 已经在仓库里并且接好了线。** 真正的缺口是四条，
其中两条会让控制台上最常见的那条路径**根本走不通**，一条是运维层面的定时炸弹，
还有一条是"整套东西从未对着真实服务发过一个请求"。

按成本从低到高：**先做 §3.1 的取证**（一次真实调用就能定死三个未知数），
再定 §3.2 的产品取舍（IAM 认证要不要支持），最后才是写代码。
**在取证之前写代码是在猜。**

---

## 1. 仓库现状（已核对）

三个 Mantle profile 都已注册、都有适配器、都有能力天花板：

| Profile ID | 适配器 | 认证头 | 能力天花板（`domain/models.go:523-530`） |
|---|---|---|---|
| `bedrock.mantle.openai.chat.v1` | `openai` 适配器 | `Authorization: Bearer` | Chat/Streaming/Tools/Vision/JSONMode/DeveloperRole/Reasoning/StreamUsage |
| `bedrock.mantle.openai.responses.v1` | `bedrockmantle.ResponsesAdapter` | 同上 | 同上但**无 Reasoning**（映射器保不住 reasoning item，有注释说明） |
| `bedrock.mantle.anthropic.messages.v1` | `anthropic` 适配器，`MessagesPath: "anthropic/v1/messages"` | `x-api-key` | Chat/Streaming/Tools/Vision/Reasoning/StreamUsage |

线在 `internal/app/providers.go:611-634`。截图里 `AnthropicBedrockMantle` +
`client.messages.create/stream` 对应的正是第三行。

其余已就位的部分：

- **端点形状被强校验**：`bedrockmantle.ValidateEndpoint`（`adapter.go:39-58`）要求 HTTPS 源、
  无 path/query/fragment/user-info、非默认端口一律拒绝，host 必须
  `bedrock-mantle.<region>.api.aws`，region 只允许 `[a-z0-9-]`。
  Admin 侧在保存 Provider 前也跑同一函数（`admin_providers.go:813,902`）。
- **凭据方案与访问面绑死**：`SurfaceBedrockMantle` 只解析出 `CredentialBedrockAPIKey`
  （`provider_profile.go:112`），且有测试钉住 **SigV4 在这个访问面上必须解析失败**
  （`provider_profile_test.go:9-14`）。
- **兼容性口径已分类**：Mantle 的三个 profile 在 `compatibility/provider_fields.go:41,49,55`
  分别按 Anthropic / Responses / Chat 三种线型处理，不是漏网的默认分支。
- **Beta 天花板是编译期保证**：数据面用的不是 `binding.Capabilities`——V3 已独立复核过，
  越界请求在任何 Provider I/O 之前被 400 `unsupported_feature` 拒绝，不预留额度、不建连。

---

## 2. 四条缺口

### 2.1 workspace 概念在本仓完全不存在（阻塞）

截图三、四里 SDK 显式设 `default_headers={"anthropic-workspace-id": "default"}`，
控制台面包屑也是 `default / Getting started`——workspace 是 Mantle 的一级概念。

**全仓 `grep -ri workspace internal/` 零命中。** `anthropic.Options`
（`internal/provider/anthropic/adapter.go:27-35`）也没有任何自定义头的入口，
所以今天连"手工塞一个头"都做不到。

两种可能，**必须先取证再决定改法**：

- 若该头**必填**：现在这条路径对着真实服务是 100% 失败，只是没人试过。
- 若**可选**（缺省落到 `default` workspace）：单 workspace 账户能用，
  多 workspace 账户无法寻址——对一个把"凭据托管 + 项目隔离"当卖点的网关来说，
  这等于把租户维度丢在门外。

改法的形状（无论哪种）：workspace 是 **binding 级**而不是 Provider 级的属性——
同一套凭据下不同 workspace 应当是不同的调用目标。这会牵动
`domain.ProviderBinding`、Admin 表单、以及 `anthropic.Options` 的自定义头通道。

### 2.2 控制台默认的 IAM 认证，本仓按设计不支持（需产品裁决）

截图一的"身份验证"默认选中 **IAM 凭证**（"使用您的 AWS 配置文件或附加角色"），
截图二写明走 **默认凭据链**：环境变量 → `~/.aws/credentials` → 附加角色（EC2/ECS/Lambda）。
API 密钥是第二选项。

本仓对 Mantle 访问面**只接受 API key**，且这不是遗漏而是有测试钉住的取舍
（`provider_profile_test.go:13`）。

**这条不能当配置项加。** `docs/verification/security-review-v1.md` 的 IMDS 裁决明写：
默认凭据链只在 **Key Slot 模式**下被接受，且只接受三种 workload-identity 源；
"新增静态源、另一个 metadata 源、或让 Admin 输入控制凭据端点选择，需要另一次评审"。
把默认链引入**数据面 Provider** 正好落在这句话里——**它是一次新的威胁评审的触发条件，
不是一个开关**。

裁决三选一：

1. **维持只支持 API key**，并在文档里写清"控制台默认选 IAM，Halro 走第二个选项"，
   避免运维照着控制台默认值配到一半发现不通；
2. **支持 SigV4 显式会话凭据**（复用已有的 `CredentialAWSSigV4Explicit`，
   Bedrock Runtime 已经在用），不碰默认链——这是安全代价最小的扩展；
3. **支持默认凭据链**——需要一次完整的威胁评审，且与 §2.3 的托管模型冲突
   （见下）。

### 2.3 API key 会过期，而 `domain.Credential` 没有过期概念（运维定时炸弹）

截图一写明：短期密钥**有效期长达 12 小时**，长期密钥有可配置的到期时间。

`domain.Credential`（`internal/domain/models.go:96-108`）只有
`CreatedAt/UpdatedAt/Revision`，**没有任何过期字段**。后果链条：

- 12 小时后所有请求 401，Halro 侧表现为 `ErrorAuthentication`；
- 没有任何指标/告警说"凭据过期了"，运维看到的是上游认证失败；
- 轮换必须人工，且要走 `PUT /credentials/{id}`（现在还要 step-up）。

这与项目其他地方的纪律不一致：Gateway Key 有 `ExpiresAt`，Master Key 有代次与指纹，
唯独 Provider 凭据没有寿命概念。

最小可行改法：`Credential` 增加可选 `ExpiresAt`，`doctor` 与控制台在临近过期时告警，
`halro_provider_credential_expiry_seconds` 一个 gauge。**这条与 Mantle 无关地有价值**——
Azure、Anthropic 的密钥同样会被轮换。

### 2.4 整条路径从未对真实服务发过一个请求（最应该先做的事）

`docs/verification/provider-real-matrix.md` **零处提及 mantle**。也就是说：

- host 形状 `bedrock-mantle.<region>.api.aws`、
- path `anthropic/v1/messages`（`providers.go:633`）、
- `x-api-key` 而不是 `Authorization: Bearer`、
- 错误信封与 `x-amzn-requestid` 头名（`bedrockmantle/adapter.go:324-331`）、
- 流式事件序列，

**全部来自文档阅读，没有一条被真实响应验证过。** 按 CLAUDE.md 的"Verify, never assume"，
这些现在都只是假设。截图给了三个可直接取证的事实（region `us-east-2`、
model id `anthropic.claude-haiku-4-5`、workspace 头），但没给 host 与 path。

---

## 3. 建议顺序

### 3.1 先取证（半天，需要一个真实账户，billable 但极小）

按截图三的快速测试跑一次，**抓下真实请求**（`anthropic[bedrock]` SDK 走 HTTP 代理，
或直接读 SDK 源码里的 endpoint/path 常量），回答四个问题：

| # | 问题 | 定死什么 |
|---|---|---|
| Q1 | 实际 host 与 path 是什么？ | `ValidateEndpoint` 与 `MessagesPath` 对不对 |
| Q2 | `anthropic-workspace-id` 是必填还是可选？ | §2.1 是阻塞还是增强 |
| Q3 | IAM 与 API key 两条路的头分别长什么样？ | §2.2 的选项 2 是否可行 |
| Q4 | 短期 key 的实际有效期与过期后的错误码/信封？ | §2.3 的告警阈值 |

证据按 `docs/verification/provider-real-matrix.md` 的格式归档；**不要把密钥、账号 ID
或原始响应体提交进仓库**。

### 3.2 再裁决（判断题，无工程量）

- §2.2 的三选一（建议：**选项 1 或 2**，不碰默认链）；
- workspace 建模在 binding 级还是 Provider 级；
- 凭据过期是否随 1.0.0 做（建议：**不随**，它是独立于 Mantle 的改进，
  且会动 `domain.Credential` 的持久结构）。

### 3.3 最后才写代码

按取证结果，工作量大致：

| 项 | 触及 | 粗估 |
|---|---|---|
| workspace 头通道（binding 级属性 + Admin 表单 + i18n + 适配器自定义头） | `domain`、`app`、`provider/anthropic`、`web` | 1~2 天 |
| SigV4 显式会话凭据支持 Mantle（若选 2.2-选项2） | `provider_profile.go` + 认证器复用 + 负面测试 | 半天 |
| 凭据过期字段 + doctor/告警/控制台 | `domain`、`store`、`app`、`web`、observability | 1~2 天（**需要 data dir 重建**，见下） |
| 真实 Provider 矩阵条目 + 兼容性 golden 更新 | `docs/verification`、`compatibility` | 半天 |

---

## 4. 不要做的事

- **不要为了 Mantle 放宽 Beta 能力天花板。** CLAUDE.md 明列它是需要专门契约评审的一条，
  且天花板不可注入目前是**类型层面**的保证（`gemini.Options`/`bedrock.Options` 结构里
  没有 `Capabilities` 字段），把它改成可配置就是把编译期保证降级成运行期校验。
- **不要把默认凭据链当配置开关加进数据面。** 见 §2.2，那是一次威胁评审的触发条件。
- **不要在没有 §3.1 取证的情况下改 `ValidateEndpoint` 或 `MessagesPath`。**
  当前值可能是对的；在没有真实响应的前提下改动它们，只是把一个未验证的假设
  换成另一个未验证的假设。
- **不要新增 profile ID 复用旧编号。** 三个 Mantle profile ID 已注册，
  契约上不得重用（CLAUDE.md：event kind / frame epoch / migration name 同理）。

---

## 5. 与 1.0.0 的关系

**这项工作不阻塞 1.0.0，也不应该塞进 1.0.0。** 理由：

- 三个 profile 已经在 1.0.0 里，且已在已知限制里标注为 **Beta**；
- §2.3 的凭据过期字段会改动持久化结构，属于"需要重建数据目录"的变更；
- §3.1 的取证需要真实 AWS 账户，与 S1（真实 Provider 证据）是同一类需要单独授权的动作。

若 §3.1 取证发现 workspace 头**必填**，则应把"Mantle Anthropic Messages profile
在 1.0.0 中未经真实服务验证"这一句补进发布说明的已知限制——
这与 §2.4 的事实一致，且不需要改代码。
