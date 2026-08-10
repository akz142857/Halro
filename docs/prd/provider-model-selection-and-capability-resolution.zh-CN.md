# 跨服务商模型选择与能力自动解析方案

状态：**已实施（Phase 1–4）；动态签名目录为 1.1.0 可选能力且默认关闭**  
更新日期：2026-08-10  
范围：Deployment 创建流程、Provider 模型目录、能力证据、动态目录更新  
不包含：现有 Deployment 的自动改写、新 Provider 协议实现、签名私钥托管或 Provider 侧授权

## 0. 决策摘要

当前把“模型主要用于什么”交给管理员选择，再用这个答案反推内部 Binding 和能力，是错误的责任划分。管理员选择的是一个上游调用目标；Halro 应根据 Provider、目标类型、模型身份、协议接口和可信证据解析可部署能力。

新的普通流程固定为：

```text
选择服务商
  → 选择调用目标（模型 ID、Azure Deployment、Bedrock ARN 等）
  → Halro 自动解析模型身份、调用接口和能力
  → 查看简短能力摘要
  → 保存为停用 Deployment
  → 测试精确 Deployment revision
  → 显式启用
```

普通用户不再选择 17 项技术能力，也不再回答“这个模型主要用于什么？”。

系统能够可信确定能力时自动配置；系统不能确定时只显示：

```text
Halro 尚未取得这个调用目标的可信能力信息。
[验证模型并继续]  [高级接入]
```

“高级接入”是例外通道，选择的是实际调用接口/能力配置，不是模型用途；逐项能力只放在默认折叠的技术详情中。

### 0.1 外部评审处理结论

2026-08-10 的安全、领域、Provider、UX 与发布评审意见已吸收。本次冻结以下修订：

- 动态目录是 1.1.0 的可选增强，不阻塞 1.0.0 的模型选择简化；1.0.0 只预留 `signed_catalog` Claim source。
- 动态目录默认关闭，启用后仍必须经过 SafeTransport、编译期固定 host allowlist、内容签名与 last-known-good。
- Admin API 在 pre-1.0.0 阶段一次性从 models 改为 invocation targets，不保留双读或 legacy URL。
- 第一阶段一次只创建一个 Deployment Variant；多 Variant 批量创建与 Route 按操作组合另立方案，不在本方案内暗含事务语义。
- Claim/cache/目录是可重建派生数据；已保存 Deployment snapshot 才是运行时权威，Claim TTL 永不使在线 Deployment 自动失效。
- Azure Deployment 默认手工输入并显式映射底层模型；ARM 控制面枚举只是可选增强，不要求网关持有管理面身份。
- Gemini 的 `supportedGenerationMethods` 只建立操作可用性，不推断视觉、工具或 JSON 等协议特性。
- Anthropic “Models API 只返回身份”的评审意见已过期：截至 2026-08-10，官方 API 已返回结构化 `capabilities` 和 token 限制；Halro 仍只 allowlist 映射当前能力字典已定义且经过 Adapter 评审的字段。
- 模型下拉框只渲染和搜索可见名称；状态、owner 与内部 Binding 不进入普通列表。

## 1. 对两个产品问题的明确回答

### 1.1 服务商新增模型后是否自动适配、无需重新发版

不能给出无条件的“是”。需要分别承诺“自动发现”和“自动开放能力”。

| 场景 | 自动出现在模型列表 | 自动解析能力 | 是否发布 Halro 新版本 |
| --- | --- | --- | --- |
| 现有 Adapter 的模型列表出现新 ID，但只返回 ID/owner | 是 | 否，能力保持 unknown | 否 |
| 现有 Adapter 返回经过评审的结构化能力字段，且字段可映射到现有能力字典 | 是 | 是，证据记为 `provider_metadata` | 否 |
| 已启用的 Halro 签名动态目录新增该精确模型，协议与能力字典均已存在 | 是；但标记“可用性未验证” | 是，证据记为 `signed_catalog` | 否 |
| 现有安全检测器能在已知协议上验证该模型 | 是或允许手工输入 | 验证成功的能力自动解析 | 否 |
| Provider 新增模型，但使用全新的请求/响应协议或认证方式 | 视 Adapter 而定 | 否 | 是 |
| Provider 新增 Halro 尚未定义的能力，如新的 Realtime 生命周期 | 可发现 | 否；不能塞进旧布尔字段 | 是 |
| 新增一个 Halro 尚未实现的 Provider | 否 | 否 | 是 |

免发版自动适配的严格公式：

```text
自动适配
  = 已有 Provider Adapter
  ∩ 已有调用协议/Profile
  ∩ 已有 Capability Dictionary
  ∩ 可信的结构化元数据、签名目录或安全验证证据
```

因此产品承诺应写成：

> 在已支持的服务商、协议和能力范围内，新模型可以通过服务商目录、安全验证或管理员已启用的 Halro 动态目录自动接入，无需重新发布 Halro；新协议和新能力仍需要版本升级。

### 1.2 用户是否还需要复杂选择模型能力

不需要。能力是 Halro 的路由事实和安全声明，不应由普通用户猜测。

- 普通流程：只选服务商和调用目标；能力摘要只读。
- 已确认能力：Halro 默认启用同一能力接口内可信建立的能力。
- 用户想收窄：在“高级设置”中关闭不需要的能力；不能打开证据未建立的能力。
- 未知模型：显式执行一次受控验证；仍无法确认时才进入高级接入。
- 手工声明：选择一个真实调用接口/能力配置，再由该接口给出能力上限；不展示六个等价的“用途”按钮。
- 多个接口：Halro 展示将生成的“可部署能力单元”，而不是要求用户理解 Binding/Profile。

## 2. 当前实现为什么无法满足这两个目标

### 2.1 上游模型信息在 Adapter 边界被截断

当前 `provider.ModelDescriptor` 只有：

```go
type ModelDescriptor struct {
    ID      string
    OwnedBy string
}
```

即使 AWS Bedrock、Anthropic 或 Gemini 的服务端目录返回输入/输出模态、能力、支持的方法、生命周期或 token 限制，进入 App 层前也只剩 ID 和 owner。App 无法区分“上游没有声明”和“Halro 丢弃了声明”。其中 `internal/provider/bedrock/models.go` 已解析 `outputModalities`、`inferenceTypesSupported` 和 `modelLifecycle`，但返回 `provider.ModelDescriptor` 时只保留 Model ID 与 Provider name，是当前最直接的截断证据。

### 2.2 内置目录与二进制版本绑定

模型能力依赖 `internal/modelcatalog/builtin.go` 的精确条目。新模型未写入当前二进制时会成为 unknown；例如新增一个 `gpt-5.x` 精确 ID，不会因为已有 `gpt-5` 条目而自动继承能力。这里不应改成按名字前缀猜测，而应引入可校验、可回滚的动态目录。

### 2.3 前端写死了哪些 Provider 可以列模型

Deployment 表单当前按 Provider type 硬编码模型目录能力，导致拥有结构化 Models API 的 Provider 也可能被排除。是否能列目标、能描述目标、能验证目标应来自 Adapter 能力，而不是 React 里的 Provider 名单。

### 2.4 “主要用途”混淆三个不同概念

当前交互把以下问题合并成一个按钮选择：

1. 用户打算用模型完成什么业务；
2. 上游模型实际支持什么；
3. Halro 应通过哪个 Profile/Binding 调用。

业务用途属于 Route 或业务配置；模型能力来自证据；调用接口来自 Adapter/Profile。三者不能互相代替。

### 2.5 单一 Deployment 与多 Binding 的关系没有产品化

一条 Deployment 只能绑定一个 Binding，这是正确的安全边界。但一个上游目标可能通过多个接口暴露不同能力。当前实现把这个内部歧义转嫁给用户；正确做法是后端解析一个或多个可部署能力单元，并保持每个单元只绑定一个 Binding。

## 3. 统一领域模型

### 3.1 Platform / Provider Instance

表示 OpenAI、Anthropic、DeepSeek、Gemini、Azure OpenAI、AWS Bedrock 或 OpenAI-compatible 服务实例，以及其凭据、地址、区域和启用的 Profile Bindings。

Provider 只声明“Halro 的 Adapter 能通过哪些协议与它通信”，不能直接证明任意模型都支持这些能力。

### 3.2 Invocation Target

用户真正选择的是调用目标，而不是全局统一的“模型 ID”。

| 平台 | Invocation Target |
| --- | --- |
| OpenAI / Anthropic / DeepSeek / Gemini | Provider model ID |
| Azure OpenAI | Deployment name；它再映射到模型与版本 |
| AWS Bedrock | Foundation model ID、Inference Profile ARN 或 Provisioned Throughput ARN |
| OpenAI-compatible | 服务商模型 ID、别名或自定义端点目标 |

建议新增归一化描述：

```go
type InvocationTargetDescriptor struct {
    TargetID          string
    TargetKind        DeploymentTargetKind
    DisplayName       string
    OwnedBy           string
    CanonicalModelRef string
    Region            string
    Lifecycle         TargetLifecycle
    Metadata          NormalizedModelMetadata
    MetadataSource    MetadataSource
    Availability      AvailabilityState
    ScopeKey          InvocationTargetScopeKey
    FetchedAt         time.Time
}
```

`TargetID` 是实际发给 Provider 的调用身份；`CanonicalModelRef` 是可选的能力目录身份。两者不得混写。Azure Deployment 或自定义别名即使映射到已知模型，也必须保留真实调用目标。

### 3.3 Capability Definition

能力字典仍由 Halro 版本化管理。动态目录只能为现有能力提供事实，不能远程发明新的路由语义。

每项定义至少包含：

- 稳定 capability ID；
- 所属操作、输入输出模态、协议特性或托管资源层；
- 依赖关系；
- 可由哪些 Profile 承载；
- 允许的证据来源；
- 是否可安全自动验证；
- 是否产生费用、持久资源或外部副作用。

### 3.4 Capability Claim

能力不能继续只有模型级 `source` 加一组布尔值。每项能力需要自己的结论与证据：

```go
type CapabilityClaim struct {
    CapabilityID string
    Status       ClaimStatus // supported, unsupported, unknown, conflicting
    Evidence     CapabilityEvidence
    Source       ClaimSource // builtin_catalog, provider_metadata, signed_catalog, verified_probe, operator_declared
    Scope        ClaimScope
    ObservedAt   time.Time
    ExpiresAt    *time.Time
    Revision     string
}
```

关键语义：

- Provider 列表中出现目标，只建立 `available`，不建立任何能力。
- 签名目录中的候选只建立模型身份；若当前 Provider 凭据未枚举到它，availability 为 `unverified`，不能伪装成“当前账户可用”。
- 未提供某字段表示 unknown，不表示 unsupported。
- Provider 元数据只有在 Adapter 对字段语义做过评审和映射后才能成为 `provider_metadata`。
- 两个可信来源冲突时状态为 `conflicting`，不自动启用。
- Profile ceiling 只表示协议能承载什么，不能单独建立模型能力。
- token 上下文、价格、配额、区域与生命周期是规格或可用性，不是布尔能力。
- `signed_catalog` 从 1.0.0 起是合法但暂时无生产者的 source 值，1.1.0 动态目录无需再改变 Claim 结构。

`ClaimScope` 不是一个松散 region 字符串。其键至少是：

```text
(provider instance, target kind, exact target ID, profile/binding,
 target 自身的 location/region semantics)
```

Bedrock ARN 或 cross-region inference profile 的身份本身就是 scope；不得把同一 ARN 的事实按普通 region 字段拆散，也不得把不同 ARN 因调用 region 相同而合并。

### 3.5 Deployment Variant

Resolver 的输出不是一个模糊 Binding ID，而是一组可部署能力单元：

```go
type DeploymentVariant struct {
    BindingID        string
    ProfileID        ProviderProfileID
    Target           InvocationTargetDescriptor
    Capabilities     ProviderCapabilities
    CapabilityClaims []CapabilityClaim
    ResolutionState  ResolutionState
}
```

不变量：

- 一个 Variant 精确绑定一个 Binding；
- Variant 能力必须同时被模型证据建立并被 Binding ceiling 承载；
- 跨 Binding 的能力不得合并到一条 Deployment；
- 1.0.0 创建流程一次只允许选择并保存一个 Variant；不做隐式批量创建；
- 用户界面展示“对话与多模态”“向量”等产品语言，技术详情才展示 Profile/Binding。

一次创建多条 Deployment 会引入名称派生、原子提交、审计、revision 和 Route 按 operation 组合语义。这些问题在独立方案冻结前不得加入本流程。

## 4. Adapter 契约

不再由前端按 Provider 名称判断功能。Adapter 使用可选接口声明能力：

```go
type InvocationTargetLister interface {
    ListInvocationTargets(context.Context, TargetQuery) ([]InvocationTargetDescriptor, error)
}

type InvocationTargetDescriber interface {
    DescribeInvocationTarget(context.Context, InvocationTargetRef) (InvocationTargetDescriptor, error)
}

type ProviderMetadataMapper interface {
    MapCapabilityClaims(InvocationTargetDescriptor) []CapabilityClaim
}

type InvocationTargetDiscoveryReporter interface {
    InvocationTargetDiscovery() InvocationTargetDiscoveryCapabilities
}
```

实现原则：

- OpenAI/DeepSeek 若目录只给 ID/owner，则只建立可用性，不猜能力。
- Anthropic 当前 Models API 提供结构化 capabilities；Adapter 逐字段 allowlist 映射，不能因为上游增加未知字段就自动开放 Halro 能力。
- Gemini 的 `supportedGenerationMethods` 只可映射 `generateContent`、`embedContent` 等操作可用性；不得据此推断 vision、tools、JSON mode。token limits 保留为规格。
- Bedrock 保留控制面返回的输入/输出模态、推理类型、生命周期和模型身份；不同 target/ARN/region semantics 分开作用域。现有 ACTIVE 与 ON_DEMAND/INFERENCE_PROFILE 可用性过滤必须保留，并补齐尚未解析的 input modalities。
- Azure 以 Deployment name 为调用目标；普通路径为手工输入 Deployment name + 显式映射底层模型。只有另行配置 ARM 身份且权限充足时才枚举/描述；数据面 key 不承担管理面发现。
- OpenAI-compatible 默认只信标准化后被评审的字段；任意扩展 JSON 不自动提升为可信证据。

`InvocationTargetDiscoveryCapabilities` 至少说明支持哪些 target kinds、能否枚举、能否描述、能否验证，以及是否需要额外管理面身份。它并入 Provider Admin 响应，前端据此渲染，不再维护 Provider type 名单。

## 5. 能力解析管线

### 5.1 解析顺序

```text
读取 Provider Adapter 能力
  → 枚举/描述 Invocation Target
  → 确定可选 Bindings 与 Profile ceilings
  → 精确匹配本地或签名动态目录
  → 合并经过评审的 Provider metadata claims
  → 必要时读取新鲜 verified probe
  → 检测冲突和依赖关系
  → 对每个 Binding 求交集
  → 输出 0..N 个 Deployment Variants
```

核心公式：

```text
Variant capabilities
  = established model claims
  ∩ binding/profile ceiling
  ∩ capability dependency closure
  ∩ operator-retained subset
```

### 5.2 来源处理

来源不是简单“后者覆盖前者”。

| 来源 | 能建立 supported | 能建立 unsupported | 默认过期 |
| --- | --- | --- | --- |
| 本地随包目录 | 是 | 仅精确、显式条目 | 随版本 |
| Halro 签名动态目录 | 是 | 仅精确、显式条目 | 按签名清单 |
| 经过评审的 Provider metadata | 是 | 仅 Adapter 契约明确允许时 | 短 TTL |
| verified probe | 是 | 仅匹配明确协议错误时 | 短 TTL |
| operator declaration | 是，最高仅 `declared` | 否 | 随 Deployment snapshot |

任何来源的缺失都保持 unknown。认证失败、限流、超时、配额不足、区域不可用和模糊 Provider 错误不能变成 unsupported。

TTL 只作用于尚未保存的 resolution/cache 是否可复用。已保存 Deployment 的 Model Capability Snapshot 不引用在线 Claim，也不因 provider metadata、verified probe 或目录缓存到期而失效。TTL 到期最多产生“建议重新确认”的复核提示；仅“证据过期/目录暂不可用”不得降级能力、摘除路由或阻断流量。

### 5.3 模型变化与并发

- Provider、目标 ID、目标类型、区域、Binding 或凭据 revision 改变时，旧解析结果不可应用。
- API 必须回显并校验 selection revision/target fingerprint；前端也必须比较当前 revision 后再写入表单。
- 晚到响应只能进入历史/缓存，不能覆盖用户已经选择的新模型。
- 保存时服务端重新校验目标、目录 revision、Claim revision 和 Binding revision。

任何 revision/fingerprint 不匹配都返回 `409 resolution_changed`，响应包含机器可读的 `mismatches[]` 和新的 resolution payload。前端显示“模型信息已更新，请复核后再保存”；服务端禁止静默重解析并按用户未见过的结果保存。

## 6. 动态签名目录

### 6.1 目标

解决“现有协议中的新模型必须等 Halro 二进制发版”问题，同时不把运行安全交给一个实时外部服务。

### 6.2 分发模型

- Halro 二进制继续携带一份可离线工作的内置目录快照。
- 1.1.0 中远程目录更新默认关闭；离线安装不会产生任何目录网络请求。管理员显式启用后才调度后台刷新。
- 后台任务可下载增量或完整目录清单；请求热路径绝不访问远程目录。
- 下载必须使用 SafeTransport；目录 host 是编译期常量 allowlist，不读环境代理、不跟随重定向，并执行 HTTPS、DNS/IP 校验和 pinned dialing。
- 响应体与解码后大小均有硬上限；若使用压缩格式，限制压缩比、条目数与展开总量，拒绝压缩炸弹。
- 清单包含 schema version、catalog revision、生成时间、过期时间、能力字典版本和签名。
- 使用固定信任根验证签名；不接受 TLS 成功替代内容签名。
- 验证成功后原子切换本地 last-known-good；失败继续使用旧目录并记录降级状态。
- Reader 声明明确的可读 schema 区间 `[min_readable, max_readable]`；目录 schema 更老或更新时降级到 last-known-good 并显示状态，不拒绝 Halro 启动或 Gateway 服务。
- 拒绝 revision 回退、过期清单、超出可读区间和能力字典不兼容的清单。
- 管理员可关闭远程更新、固定 revision、查看来源和手动刷新。
- 下载内容不含凭据、Provider 实例信息或用户模型列表。

签名私钥不进入仓库、构建产物或普通 CI secret。目录发布由隔离的 KMS/HSM 或离线签名环境完成，签名前需要内容审核与双人批准。二进制只携带公钥信任根。轮换采用新旧公钥重叠窗口：先发布同时信任两根的 Halro 版本，再切换签名键；旧根至少保留到该 Halro 版本已发布且最长有效目录全部过期，之后才能删除。

### 6.3 动态目录能改什么

允许：

- 为现有 Provider/Profile/target kind 增加精确模型身份；
- 为现有能力字典增加或收窄 Capability Claims；
- 更新模型规格、生命周期与别名映射；
- 标记条目撤销或需要复核。

禁止：

- 增加 Halro 不认识的新 capability ID；
- 改变 Provider 请求/响应映射；
- 增加认证方式或放宽 SSRF/allowed-host 策略；
- 静默修改已保存 Deployment 的能力；
- 远程启用 Provider、Deployment 或 Route。

### 6.4 已保存 Deployment 的稳定性

Deployment 继续保存不可变 Model Capability Snapshot。目录更新只产生：

- `current`：仍一致；
- `review_available`：发现更多能力，但不自动扩宽；
- `drifted`：已保存声明超出当前可信事实，按既有 fail-closed 规则处理；
- `catalog_unavailable`：对现有三态枚举的就地扩展；无法刷新时仍使用 last-known-good，不把 absence 当 drift，且该状态继续允许现有流量。

任何目录更新都不能替代精确 Deployment revision 的测试，也不能自动启用 Deployment。

## 7. Admin UX

### 7.1 普通创建流程

表单只保留必要选择：

1. 部署名称；
2. 服务商；
3. 调用目标类型（仅 Provider 确有多种目标时显示）；
4. 模型/调用目标；
5. 可选 region、并发和价格等部署配置；
6. 只读能力摘要；
7. 保存。

能力摘要示例：

```text
已识别
可用于：对话、图像理解、工具调用、流式输出
已由服务商信息与 Halro 目录确认
[技术详情]
```

不显示：

- “这个模型主要用于什么？”；
- 17 个默认展开的勾选项；
- Binding、Profile、verified_probe、inconclusive 等实现词汇；
- 把“接口能够承载”写成“模型确定支持”。

模型 ID 下拉框也必须收敛为单列选择列表：

- 每一项只显示模型/调用目标名称；
- 删除“能力未知”及其他能力状态列；
- 删除 `system`、`owned_by` 等 owner 列；
- 能力状态在选中后的能力摘要区显示，不在下拉列表中重复；
- owner 若未来确实用于区分同名目标，只能作为消歧副文本按需出现，不能恢复成常驻列。
- 搜索默认只匹配模型/调用目标名称；删除当前对不可见 `owned_by` 的匹配，避免出现“搜到了但看不出为什么”的结果。

下拉框的责任只是帮助用户搜索和选择调用目标。列表中的“能力未知”会让每一行重复同一条无法执行的信息，`system` 则是上游目录元数据而非用户的选择条件；两者都会降低扫描效率。

### 7.2 多个可部署能力单元

若解析出多个 Variant，普通界面显示 Resolver 产生的高层选项，而不是技术歧义：

```text
这个模型可以通过 2 种接口部署，请选择一种：
( ) 对话与多模态
( ) 向量嵌入
[查看技术详情]
```

1.0.0 明确只保存一个 Variant。选项必须来自 Resolver 输出，不能由前端从 Provider ceiling 临时推导。多选、批量创建和 Route 自动组合不属于本方案。

解析为 0 个 Variant 时不能进入“未知模型”验证：

```text
当前服务商尚未启用可承载此模型的调用接口。
请先在服务商中启用所需接口，然后返回继续。
[前往服务商设置]
```

### 7.3 未知目标

普通界面只有两个动作：

- `验证模型并继续`：明确触发受控验证，显示调用次数/可能费用/不会创建持久资源；
- `高级接入`：管理员明确选择一个已配置的调用接口，由 Halro 展示该接口的声明上限。

高级接入中的默认选择粒度是“能力接口”，例如“OpenAI 对话接口”“嵌入接口”，不是六个用途按钮。逐项能力默认从接口模板带出，管理员可收窄；扩大到模板外仍被后端拒绝。

验证结束但仍为不确定、未授权或不可用时，摘要区保留固定行结构并显示“未知”；说明行给出原因和下一步。`inconclusive` 是本次验证的终态，不显示立即重试 CTA，直接提供“高级接入”。认证/权限问题只引导修复 Provider，冷却结束前不提供重复验证。

面向用户的复核状态使用结果语言：

| 内部状态 | 用户文案 |
| --- | --- |
| `current` | 能力信息为最新 |
| `review_available` | 发现新的可用能力，复核后可启用 |
| `drifted` | 能力信息发生冲突，已暂停参与路由 |
| `catalog_unavailable` | 暂时无法更新能力信息，现有部署继续运行 |

### 7.4 编辑现有 Deployment

- 模型身份和调用目标继续锁定；需要改变时创建替代 Deployment。
- 能力只允许在 snapshot 内收窄；发现新能力时走复核、保存、重测、启用流程。
- 切换模型/Provider 必须清除旧 Binding、旧能力、旧解析结果和旧确认状态。
- “返回识别”必须重建解析状态，不能保留手工声明产生的 Binding 或能力。

## 8. API 与存储演进

### 8.1 Admin API

建议把当前 Provider models 响应演进为目标目录：

```text
GET /admin/api/v1/providers/{id}/invocation-targets
GET /admin/api/v1/providers/{id}/invocation-targets/{target}/resolution
POST /admin/api/v1/providers/{id}/model-capability-detections
```

项目仍处于 pre-1.0.0，直接删除旧 `/providers/{id}/models` 路由并同步修改前端、测试和文档；不保留 alias、双读或 legacy 响应。若存储 schema 同步改变，明确要求重新初始化数据目录。

响应包含：

- 目标身份、类型、region 和生命周期；
- availability 与 capability 分离状态；
- 0..N 个 Deployment Variants；
- 每项 Claim 的来源、scope、revision 和 freshness；
- catalog/adapter/profile revisions；
- 用户友好摘要和默认折叠的技术详情。
- Provider 实例的 target discovery capabilities，供前端决定 target kind、枚举、描述与验证入口。

### 8.2 存储

权威边界：已保存 Deployment 及其不可变 Model Capability Snapshot 是运行时权威；Invocation Target cache、Claim 集合、resolution 记录和目录 last-known-good 都是可重建派生数据。删除全部派生 cache 后，已保存 Deployment 的能力与路由行为不得改变，只允许发现/解析变慢或暂时降级。

优先复用现有不可变 Snapshot 和 Deployment revision 机制；新增：

- 动态目录 last-known-good 与 revision；
- Invocation Target cache；
- 按能力存储的 Claim 集合或可重建的解析记录；
- Deployment Variant 的解析 revision；
- 目录更新/解析冲突/高级声明审计事件。

不要把原始 Provider 响应、凭据、认证头、任意模型输出或 Provider 错误正文持久化到目录/Claim。

### 8.3 审计与可观察性

审计事件至少覆盖：目录更新启用/禁用、下载成功/失败、签名或版本拒绝、last-known-good 降级/恢复、解析冲突、0 Variant、高级声明和 resolution revision 冲突。审计 metadata 记录有界状态、Provider ID 与 revision，不记录凭据、原始响应和模型输出。

指标至少覆盖：

- 目录刷新次数、状态、降级持续时间与恢复；
- resolution 结果（resolved/unknown/conflicting/no_variant）；
- probe 任务终态、预算耗尽与并发拒绝；
- 409 resolution mismatch 计数。

Prometheus label 只能使用有界枚举，例如 provider type、target kind、source、status、error class；禁止 model ID、target ID、ARN、Deployment ID、Claim ID 或 detection ID 等高基数值。

## 9. 实施阶段

### Phase 0：冻结契约（1.0.0）

- [x] 用 ADR 固定 Invocation Target、Capability Claim、Deployment Variant 三个概念。
- [x] 冻结“发现不等于能力证据”和“普通用户不选能力”的产品契约。
- [x] 在 Claim source 中预留 `signed_catalog`，但 1.0.0 不实现远程目录下载。
- [x] 明确本方案与现有自动检测、能力字典文档的替代/保留关系。

门禁：Go domain table tests 覆盖数据来源、scope、冲突、失效和 Variant 不变量；ADR review checklist 固定免发版边界。

### Phase 1：Adapter 与目标目录（1.0.0）

- [x] 用 `InvocationTargetDescriptor` 替代只含 ID/owner 的信息边界。
- [x] 前端删除 Provider type 硬编码，改读 Adapter 暴露的 target capabilities。
- [x] OpenAI、DeepSeek、Anthropic、Gemini、Bedrock 分别实现其真实支持的目标枚举/描述契约；Anthropic/Gemini 当前缺失的目标列表实现已经补齐。
- [x] Azure 默认手工输入和底层模型映射；ARM 枚举作为额外身份启用后的可选增强。
- [x] Bedrock/Anthropic/Gemini 保留并 allowlist 映射已评审的结构化元数据；Gemini 方法字段只映射操作，字段缺失保持 unknown。
- [x] Bedrock 保留现有 ACTIVE/inference type 可用性过滤并补 input modalities。

门禁：各 Adapter fixture/contract tests 证明新增模型即使能力未知也能免发版出现在列表；反向测试证明删除某 Provider 映射后不会从名称或其他 Provider 字段猜测。

### Phase 2：Capability Resolver（1.0.0）

- [x] 引入逐能力 Claim、精确 scope、TTL 与冲突状态。
- [x] 服务端按 Binding 输出 0..N 个 Deployment Variants，1.0.0 每次只允许保存一个。
- [x] 合并内置目录、Provider metadata 和 verified probe 保存路径；`signed_catalog` 暂无生产者；缺失永不变成 unsupported。
- [x] 保存时绑定 target/catalog/profile/claim revisions，并继续生成不可变 snapshot。
- [x] 实现 `409 resolution_changed` 与 `mismatches[]` + 新 resolution payload。

门禁：Go app/API tests 覆盖同一目标跨 Binding 不泄漏、0/N Variant、revision 冲突、Claim TTL 不影响 snapshot、晚到结果不可保存。

### Phase 3：简化创建界面（1.0.0）

- [x] 删除普通流程的“主要用途”和 17 项能力选择。
- [x] 模型 ID 下拉框改为只显示模型/调用目标名称，移除“能力未知”状态列和 `system`/owner 列，并停止按隐藏 owner 搜索。
- [x] 增加结构稳定的只读能力摘要、未知目标双 CTA、验证失败出口、0 Variant 提示和默认折叠技术详情。
- [x] 多 Variant 只允许选择并保存其中一个，不提供批量创建。
- [x] 高级接入只允许选择真实接口模板并收窄能力。
- [x] 删除 React Provider type 名单、前端 Binding 推断和用途按钮相关死代码/测试。
- [x] Phase 完成时删除旧 TODO 中被替代的普通流程正文，只保留指向本方案的一行链接。

门禁：Vitest 覆盖无用途/逐项选择、单列下拉、仅按名称搜索、0/1/N Variant、inconclusive 进入高级接入、selection revision 防晚到覆盖；浏览器 RC checklist 验收键盘、屏幕阅读器、窄屏与真实 Provider 状态。

2026-08-10 已完成基于生产 bundle 与 fixture Provider 响应的本地浏览器 RC：未知目标双 CTA、可访问结构、键盘选择以及 390 × 844 窄屏无横向溢出均通过，证据边界记录于 [`docs/verification/provider-real-matrix.md`](../verification/provider-real-matrix.md#local-fixture-browser-rc-evidence-2026-08-10)。真实 Provider 账号状态仍是精确 RC commit 的外部门禁，未以 fixture 结果冒充通过。

### Phase 4：动态签名目录（1.1.0，可选增强）

- [x] 提取内置目录为同一 schema 的 bundled snapshot。
- [x] 实现默认关闭开关、SafeTransport 下载、编译期 host allowlist、双重大小上限和压缩防护。
- [x] 实现签名验证、可读 schema 区间、原子更新、last-known-good、固定 revision、降级与恢复状态。
- [x] 建立隔离 KMS/HSM 或离线签名、双人审核、发布和信任根轮换流程。
- [x] 确保离线、更新失败、目录过期和未来 schema 不会中断 Gateway 请求路径。

实现证据：[`ADR 0020`](../adr/0020-dynamic-signed-model-catalog.md) 固定读取与失败语义；[`model-catalog-publishing`](../runbooks/model-catalog-publishing.md) 固定私钥隔离、双人审核、发布与轮换步骤；`model-catalog-publish.yml` 只接收外部签名产物，经生产公钥验证和受保护环境批准后创建 CODEOWNERS PR。实际启用前仍必须在仓库设置中配置 `model-catalog-production` 的 required reviewers、严格保持最新的分支保护或 merge queue、公开的 `MODEL_CATALOG_TRUST_ROOTS`、`CATALOG_PUBLISHER_LOGIN` 以及受保护的发布 bot 凭据；这是生产激活门禁，不由源码伪造为已配置。

门禁：Go security/integration tests 覆盖 SafeTransport、重定向、环境代理、私网/DNS、大小/压缩、篡改、回退、过期、未来 schema、新旧公钥窗口、降级恢复；RC 离线测试证明零目录出站和 last-known-good 不影响现有流量。

### 各阶段就地收口

- [x] 保留旧 Deployment snapshot，不批量重算或静默扩宽。
- [x] 对旧 `operator_declared` 记录保持可解释来源；编辑时再进入新流程。
- [x] 每个 1.0.0 Phase 同步更新 API 契约、Provider 能力文档、能力字典方案和真实 Provider RC 矩阵，不把清理集中拖到最后。
- [x] 本次没有 durable schema 变更；API 在 pre-1.0.0 阶段硬切换且不保留错误兼容层。

## 10. 必测场景

| 场景 | 预期 |
| --- | --- |
| OpenAI 列出新的 `gpt-5.x`，本地无条目且无元数据 | 立即可见；能力 unknown；不按名字继承 |
| 动态签名目录随后增加该精确模型 | 无需发版即可解析；现有 Deployment 不自动扩宽 |
| Anthropic/Gemini 新模型返回现有 Adapter 已评审的结构化字段 | 无需发版生成对应 Claims |
| Anthropic 新模型增加一个 Halro 不认识的 capability 字段 | 忽略未知字段；不会自动开放新能力 |
| Gemini 返回 `generateContent` 但没有其他能力证据 | 只建立对话操作，不推断 vision/tools/JSON |
| Bedrock cross-region inference profile 从不同调用 region 使用 | Claims 按 exact ARN 与 target region semantics 作用，不错误拆分或合并 |
| Azure Deployment 名为 `prod-chat`，底层为已知模型 | 调用身份保持 `prod-chat`；能力身份来自明确映射 |
| Azure 数据面凭据无 ARM 枚举权限 | 正常回落到手工输入 Deployment name + 底层模型映射 |
| DeepSeek/兼容服务只返回 ID | 列表可用，能力保持 unknown |
| Provider metadata 与签名目录冲突 | 标记 conflicting，不自动启用 |
| 切换模型时旧检测晚到 | 不覆盖新模型、Binding 或能力 |
| 一个模型解析为对话和嵌入两个 Bindings | 输出两个 Variant，不合并成非法 Deployment |
| 动态目录被篡改或回退 | 拒绝更新，继续未过期 last-known-good 并显示降级 |
| last-known-good 已过期 | 新解析回退内置目录并显示降级；既有不可变 Deployment 继续运行 |
| 动态目录新增未知 capability ID | 拒绝清单，要求 Halro 版本升级 |
| 模型 ID 下拉框包含能力未知、known 或 owner=`system` 的模型 | 每行只显示模型/调用目标名称；状态移到选中后的摘要区 |
| Probe 达到调用预算或 Provider 并发上限 | 停止新调用，返回明确终态；不自动重试，不把未完成能力记为 unsupported |
| 签名密钥轮换窗口内分别使用新旧合法签名 | 两根均可验证；窗口结束后旧根被拒绝且不影响 last-known-good |
| 目录刷新降级后恢复成功 | 清除降级状态，原子切换到新 revision，不残留告警 |
| Resolver 输出 0 个 Variant | 引导启用 Provider 调用接口，不诱导验证模型 |
| 已保存 Deployment 的 provider metadata/probe TTL 到期 | 现有 snapshot、路由和流量不变，只产生复核提示 |
| 保存时任一 resolution revision 改变 | 返回 409 + mismatch + 新 resolution；不静默保存 |

## 11. 发布门禁

| 门禁 | 验证手段 |
| --- | --- |
| 自动发现与能力开放是独立状态；目录/元数据缺失永不变成 unsupported | Go domain/app table tests |
| 普通流程没有“主要用途”和默认展开的逐项能力选择 | Vitest DOM assertions |
| 下拉框只渲染模型名且只按模型名搜索 | Vitest listbox/search assertions |
| 前端不含 Provider type 目录名单，不从排序/第一个 Binding 选接口 | source guard + Vitest + Go API tests |
| 每个 Provider metadata 映射字段都有来源说明和正/反 fixture | Adapter contract tests |
| 可信来源冲突不自动开放；每个 Variant 只绑定一个 Binding | Go resolver tests |
| 0/1/N Variant、409 mismatch 和晚到结果均有确定行为 | Go API tests + Vitest |
| 已保存 snapshot 不受 Claim TTL、目录不可用或 cache 删除影响 | Go restart/drift tests |
| 能力扩大仍需保存、测试精确 revision、显式启用 | Go lifecycle tests + browser RC |
| 1.0.0 的 OpenAI、Anthropic、DeepSeek、Gemini、Azure、Bedrock 行为符合各自真实发现边界 | Adapter tests + opt-in RC matrix |
| 1.1.0 动态目录默认零出站且不在请求热路径 | network trap integration test + RC offline checklist |
| 1.1.0 SafeTransport、签名、schema 区间、过期、回退、轮换、降级恢复全部成立 | Go security/integration tests |

## 12. 明确不接受的捷径

- 不按 `gpt-*`、`claude-*` 或其他名字前缀猜能力。
- 不把 Provider/Profile ceiling 当成模型能力。
- 不因为 `/models` 返回了 ID 就自动勾选 Chat。
- 不把签名目录候选当成当前 Provider 凭据已经获权调用的模型；未被 Provider 枚举时必须标记可用性未验证。
- 不由前端按第一个 Binding、排序或用途按钮决定调用接口。
- 不让远程目录定义新协议、新认证、新能力语义或修改安全策略。
- 不让目录更新静默改变已启用 Deployment。
- 不把超时、认证失败、限流、配额不足和区域不可用记成 unsupported。
- 不记录原始模型输出或 Provider 错误正文作为可持久化能力证据。

## 13. 与现有文档的关系

- [模型能力自动识别方案](model-capability-auto-detection.zh-CN.md) 保留安全检测、证据、预算和不可重试 UNKNOWN 等契约；其“主要用途/Binding 消歧”产品交互由本方案替代。
- [模型能力字典演进与四层展示](../todo/model-capability-dictionary-evolution.zh-CN.md) 继续负责能力字典版本与新增能力语义；本方案负责如何跨 Provider 为这些能力取得事实。
- [基于服务商与模型的能力选择升级方案](model-aware-capability-selection.zh-CN.md) 和 [1.1.0 路由归属方案](../todo/model-aware-capability-selection.v1.1.0.zh-CN.md) 中“用户直接选择能力/用途”的普通流程，以本方案为后续目标；已保存 snapshot、单 Deployment 单 Binding 和 Route 组合原则继续有效。
- Phase 3 落地时直接删除上述旧文档中已被替代的普通创建流程正文，仅保留迁移说明和指向本文档的链接；不长期维护两套互相冲突的产品流程。

## 14. Provider 事实来源（核对日期：2026-08-10）

- [Anthropic List Models](https://platform.claude.com/docs/en/api/models/list)：当前响应包含 model identity、结构化 capabilities 与 token limits；具体字段仍需 Adapter allowlist。
- [Gemini Models API](https://ai.google.dev/api/models)：`supportedGenerationMethods` 是 API method 列表；token limits 是规格，不能借此推断其他协议特性。
- [Azure AI Services Deployments List](https://learn.microsoft.com/en-us/rest/api/aiservices/accountmanagement/deployments/list)：部署枚举属于 ARM 管理面，并返回 deployment model；普通数据面 key 不应被假设拥有该权限。
