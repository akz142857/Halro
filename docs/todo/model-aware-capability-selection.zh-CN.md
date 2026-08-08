# 基于服务商与模型的能力选择升级方案

- 状态：Proposed / 待评审
- 生产就绪：否
- 范围：Provider 模型目录、Deployment 能力快照、Profile Binding 自动映射、Admin 创建流程、测试与迁移
- 当前实现：仍由管理员先选择内部“能力接口”，再填写模型 ID；本文描述的模型级能力目录与自动映射尚未实现
- 相关契约：[Provider capability contract](../contracts/provider-capabilities.md)、[ADR 0009](../adr/0009-phase2-resource-ownership.md)、[Compatibility manifests](../compatibility/README.md)

## 0. 决策摘要

Halro 应把能力归属从“Provider Profile 的可用上限”收敛到“具体服务商实例与具体模型目标的已知能力”。普通管理员的目标流程调整为：

```text
选择服务商 → 选择模型 → 查看该模型支持的能力 → 按需关闭能力 → 保存并验证
```

内部 `ProviderProfileBinding` 继续保留，用于 Adapter、协议、凭据方案、健康测试和运行时调度，但默认不再作为“能力接口”要求用户直接选择。系统根据模型能力自动选择内部 Binding；只有模型信息未知或一个模型确实横跨多个不兼容协议时，才进入明确的高级流程。

核心不变量：

```text
effective capabilities
  = provider/profile ceiling
  ∩ versioned model capability snapshot
  ∩ operator-retained subset
```

- 管理员只能收窄，不能超出模型目录或 Provider Profile 扩张能力。
- 未知能力默认不启用，不能从模型名称进行宽松猜测。
- 模型目录刷新不能静默扩张或改变已启用 Deployment。
- 请求路径只读取已经验证并持久化的不可变 Deployment 能力快照，不实时依赖外部模型目录。

本文中的 `gpt-5.4-mini`、`gpt-image-2` 仅作为交互示例，不构成模型能力事实；实际能力必须由版本化目录和证据决定。

## 1. 问题

### 1.1 当前流程暴露了内部实现

当前 Deployment 创建流程要求管理员先选择类似以下内部 Profile Binding：

```text
对话 · 流式 · 向量嵌入 — openai.chat-embeddings.v1
内容审核 · 图像 · 音频转写 — openai.media-resources.v1
```

这些选项是协议和 Adapter 边界，不是用户心智中的模型能力。用户真正知道的是“选了哪个服务商、哪个模型、准备用它做什么”。把内部 Profile 放在模型之前会产生三个问题：

1. 用户必须理解 Halro 的内部协议拆分。
2. 选择 Profile 后默认继承上限，容易把具体模型不支持的能力错误声明为支持。
3. 同一模型如果需要多个内部 Profile，现有单一 `binding_id` 无法表达。

### 1.2 当前模型发现没有能力信息

`GET /admin/api/v1/providers/{id}/models` 当前只返回：

```json
{
  "id": "model-id",
  "owned_by": "provider"
}
```

模型列表缓存按 `provider_id + binding_id` 隔离，TTL 为 5 分钟；它能证明服务商返回了一个模型标识，但不能证明该模型支持对话、Embedding、审核、图像、音频或任何增强特性。

许多服务商的模型列表 API 本身也不返回完整能力矩阵。因此不能把“列表中存在”解释为“支持 Provider Profile 的所有能力”。

### 1.3 Provider 上限不等于模型能力

当前 `ProviderInstance` 汇总多个 Binding 的能力上限，Deployment 允许保存其子集。这保证了 Deployment 不能超过 Provider，但无法保证它没有超过具体模型。

例如：

- 一个 Provider 可以同时支持 Chat、Embeddings、Moderations 和 Images；
- 一个具体模型通常只支持其中一个核心操作族及若干增强特性；
- 把 Provider 的全部能力默认复制给模型，会把运行时错误推迟到真实请求阶段。

这与 Halro “在 Provider I/O 之前拒绝不支持语义”的 fail-closed 目标不一致。

## 2. 目标与非目标

### 2.1 目标

1. 普通创建流程只要求选择服务商和模型，不暴露内部 Profile ID。
2. 已知模型只展示目录声明为支持的能力；不支持能力不显示、不可提交。
3. 管理员可以关闭模型支持的能力，但不能手动扩张。
4. 未知模型可被接入，但必须经过明确的手动声明与验证流程，默认零能力。
5. Deployment 持久化模型能力快照、来源、版本和证据，请求路径不依赖目录实时状态。
6. 模型能力变化必须经过显式复核、重新测试与重新启用，不能自动影响在线流量。
7. 内部 Binding 仍保持协议明确、凭据明确、Adapter 明确和 fail-closed。
8. 引导深链与页面“新增”复用同一查询、同一表单和同一能力决策。

### 2.2 非目标

- 不把模型名称前缀或营销名称当作可信能力证据。
- 不通过发送昂贵、产生持久资源或具有业务副作用的请求自动探测全部能力。
- 不在请求热路径实时访问服务商模型目录。
- 不自动为既有 Deployment 开启新能力。
- 不承诺第三方 OpenAI-compatible 服务一定提供可靠模型目录。
- 不把模型能力目录扩展成完整模型市场、Benchmark 或推荐系统。

## 3. 术语与能力分层

### 3.1 核心操作能力

核心操作决定请求使用哪个北向语义和哪个 Provider Adapter：

| 能力 | 示例语义 |
| --- | --- |
| `chat` | 对话或文本生成 |
| `embeddings` | 向量嵌入 |
| `moderations` | 内容审核 |
| `images` | 图像生成 |
| `transcriptions` | 音频转写 |
| `speech` | 语音合成 |
| `files` | 文件资源 |
| `batches` | 批处理资源 |
| `rerank` | 文档重排 |
| `async_generate` | 异步媒体生成 |

### 3.2 增强特性

增强特性依附于核心操作，不能单独成为可路由能力：

- `streaming`；
- `tools`；
- `vision`，明确表示“视觉输入”，不能与图像生成混称；
- `json_mode`；
- `developer_role`；
- `reasoning`；
- `stream_usage`。

现有依赖关系继续成立，例如 Streaming、Tools、Vision、JSON、Reasoning 必须依附 Chat，Stream Usage 必须依附 Streaming。

### 3.3 规格限制

以下字段是模型规格，不是功能能力：

- `max_context_tokens`；
- `max_output_tokens`；
- 区域、目标类型和服务商部署名；
- 价格、并发和路由权重。

UI 和 API 必须继续区分“支持什么操作”和“该操作的容量/规格”。

## 4. 权威能力模型

### 4.1 能力来源

每项有效能力必须带来源和证据：

| 来源 | 含义 | 是否可扩张 Deployment |
| --- | --- | --- |
| `builtin_catalog` | Halro 内置、版本化且经过评审的模型目录 | 是，但仅在创建或显式复核时 |
| `provider_metadata` | 服务商 API 返回的结构化能力元数据 | 是，前提是 Adapter 对字段语义有明确契约 |
| `verified_probe` | 明确的无副作用或用户触发验证结果 | 是，受探测覆盖范围限制 |
| `operator_declared` | 管理员对未知模型的显式声明 | 是，但证据保持 Declared，不能伪装成 Verified |
| `legacy_snapshot` | 迁移前 Deployment 的既有声明 | 仅保持现状，不能自动扩张 |
| `unsupported` | 已知不支持或无法证明 | 否 |

证据等级沿用现有 `verified / declared / legacy / unsupported`，但增加来源与目录版本，避免只看一个布尔值。

### 4.2 有效能力计算

对每个布尔能力：

```text
effective = provider_ceiling
         && model_snapshot_supports
         && operator_retains
```

对数值限制：

```text
effective_limit = minimum(all non-zero limits declared by provider, model, and operator)
```

`0` 继续表示“该层没有声明上限”，不能用于擦除上游已经声明的非零限制。

### 4.3 不可变快照

Deployment 创建时保存能力快照。目录刷新只产生比较结果：

- 新增能力：标记 `available_for_review`，不自动开启；
- 删除能力或限制收紧：标记 `capability_drift`，阻止新启用，并要求重新验证；
- 仅名称、owner 等非语义元数据变化：不影响 Deployment revision；
- 已启用 Deployment 不因一次目录请求失败而被静默修改。

任何接受后的能力变更都必须：

1. 产生新的 Deployment revision；
2. 使既有健康测试变为 stale；
3. 重新执行 Deployment 测试；
4. 经显式启用后才进入路由候选；
5. 写入审计事件。

## 5. 数据模型提案

### 5.1 模型目录描述

扩展当前 `provider.ModelDescriptor` 的 Admin 视图，不直接信任上游返回值：

```go
type ProviderModelDescriptor struct {
    ID              string
    OwnedBy         string
    Status          ModelCapabilityStatus // known | partial | unknown | conflicting
    Capabilities    ProviderCapabilities
    Evidence        CapabilityEvidenceSet
    CapabilitySource string
    CatalogRevision string
    ProfileCandidates []ModelProfileCandidate
}

type ModelProfileCandidate struct {
    BindingID    string
    ProfileID    ProviderProfileID
    Capabilities ProviderCapabilities
}
```

约束：

- 上游 `/models` 只提供模型存在性，不自动产生能力布尔值；
- 能力由 Adapter 的目录解析器合并；
- 同一能力出现冲突时状态为 `conflicting`，该能力 fail-closed；
- 响应上限继续保持 10,000 项和 5 分钟缓存，但能力目录版本必须进入缓存键或响应元数据；
- 刷新只访问 Provider 已绑定、已验证的模型目录地址，不接受用户控制的任意 Admin URL。

### 5.2 Deployment 能力快照

在现有 `Deployment` 上增加：

```go
type ModelCapabilitySnapshot struct {
    ProviderModel     string
    CatalogRevision   string
    Source            string
    CapturedAt        time.Time
    Capabilities      ProviderCapabilities
    Evidence          CapabilityEvidenceSet
    OperationBindings map[string]string // operation -> provider binding id
}

type Deployment struct {
    // existing fields...
    ModelCapabilitySnapshot ModelCapabilitySnapshot
    OperatorDisabled        []string
    CapabilityReviewState   string // current | review_available | drifted | legacy
}
```

持久化时必须验证：

- `ProviderModel` 与 Snapshot 中的模型一致；
- Effective capabilities 是 Snapshot 的子集；
- 每个启用的核心操作都有一个兼容且已启用的 Binding；
- 增强特性映射到其依赖的核心操作 Binding；
- Operation Binding 的 Profile、Access Surface、Credential Scheme 与 Provider 相容；
- Evidence 不得高于来源允许的等级。

### 5.3 单 Binding 与多 Binding

大多数已知模型只需要一个核心操作 Profile。第一阶段继续使用现有 `binding_id`，但由系统自动选择。

目标模型允许同一 Provider + Model 在确有证据时跨多个内部 Profile 暴露能力。此时使用 `operation_bindings` 映射，而不是让用户重复创建逻辑上相同的模型：

```json
{
  "chat": "binding_chat",
  "embeddings": "binding_chat",
  "images": "binding_media"
}
```

多 Binding 进入运行时前必须完成单独设计门禁：

- Router 按请求的核心操作选择 Adapter；
- 每个 Operation Binding 独立健康测试，Deployment 只有在所有已启用操作均通过时才可整体标记健康；
- 价格选择必须按操作种类明确，不能把 token 价格用于固定请求资源；
- 文件、批处理和异步资源继续遵守 ADR 0009 的 owner pinning；
- 不清楚上游是否已经产生副作用时保持 UNKNOWN/fail-closed，不跨 Binding 重试。

在这些门禁完成前，跨 Profile 模型必须拆成多个 Deployment；UI 不能伪装成已经支持一个多能力 Deployment。

## 6. 模型能力目录

### 6.1 目录组成

目录由三部分合并：

1. **服务商模型列表**：证明当前账户/区域能看到哪些模型 ID。
2. **Halro 内置能力清单**：版本化的精确模型/别名与能力声明。
3. **Adapter 元数据或验证证据**：在协议明确且无安全降级时补充或验证。

模型目录键至少包含：

```text
provider type
+ access surface/profile
+ provider model or provider deployment target
+ region when applicable
```

不能只按模型名称做全局缓存。Azure Deployment、Bedrock 区域/Inference Profile 和 OpenAI-compatible 自定义模型都可能让同名模型具有不同能力。

### 6.2 匹配规则

- 优先精确模型 ID；
- dated alias 只有在服务商命名规则稳定且有测试时才能映射到已知 family；
- 不允许宽泛前缀把未知未来模型自动提升为已知能力；
- 模型别名变化不改变已持久化 Deployment Snapshot；
- Provider 返回重复、空、超长或异常模型 ID 时沿用现有规范化与数量上限。

### 6.3 未知模型

未知模型的普通流程：

1. 显示“能力未知”；
2. 不默认勾选任何核心操作；
3. 提供“声明模型用途”的高级入口；
4. 管理员先选择一个核心操作，再选择依附特性；
5. 保存为停用，Evidence 为 Declared；
6. 完成对应 Operation 的真实测试后才能启用；
7. 测试只能验证被执行的语义，不能把未测试能力一并升级为 Verified。

## 7. Admin 交互方案

### 7.1 普通创建流程

#### 步骤一：选择服务商

- 普通模型部署页显示已启用的服务商，并明确标记当前健康测试是否有效；健康状态无效时允许准备停用 Deployment，但不能启用；
- 首次引导优先展示健康测试有效的服务商；若选择未验证或测试已过期的服务商，必须先返回服务商步骤完成验证，不能误报“模型服务已就绪”；
- 服务商选择后加载聚合模型目录；
- 不展示 `binding_id`、Profile ID 或“能力接口”。

#### 步骤二：选择模型

- 搜索和选择服务商返回的模型；
- 每个模型显示核心用途徽标，例如“对话”“Embedding”“图像生成”；
- `known` 模型可直接进入下一步；
- `partial/unknown/conflicting` 显示明确状态和处理入口。

#### 步骤三：确认能力

- 只渲染模型目录支持的能力；
- 默认选中已知核心操作和已知增强特性；
- 管理员可以关闭，但不能添加目录外能力；
- 精确区分“视觉输入”与“图像生成”；
- 显示能力证据与目录版本，但 Profile ID 只放在高级详情中。

#### 步骤四：规格与发布

- 展示目录中的上下文与输出限制；
- 管理员只能进一步收紧；
- 新 Deployment 仍保存为停用；
- 执行与所选能力一致的测试后才能启用；
- 首次引导根据权威 readiness 自动推进。

### 7.2 模型变化

Provider、模型目标、区域或核心 Operation 发生变化时，不允许在现有 Deployment 上就地修改；沿用“创建替代 → 测试 → 切换路由”的流程。

### 7.3 错误与加载状态

- 模型目录加载前不挂载依赖目录数据的表单状态，避免深链竞态；
- 目录失败时保留手动模型 ID 入口，但进入未知模型 fail-closed 流程；
- 禁用操作必须显示具体原因，不允许下拉框视觉上有值而内部状态为空；
- 深链与页面“新增”必须共享相同的 query key、初始化函数和组件；
- 只读管理员可查看能力与来源，但不能创建或调整。

## 8. Admin API 提案

### 8.1 聚合模型目录

保留现有路径：

```http
GET /admin/api/v1/providers/{provider_id}/models
```

默认返回该 Provider 所有启用 Binding 的聚合模型目录；`binding_id` 仅作为高级诊断过滤条件。响应示例：

```json
{
  "items": [
    {
      "id": "example-model",
      "owned_by": "provider",
      "status": "known",
      "capabilities": {
        "chat": true,
        "streaming": true,
        "tools": true,
        "images": false
      },
      "capability_evidence": {
        "chat": "declared",
        "streaming": "declared",
        "tools": "declared",
        "images": "unsupported"
      },
      "capability_source": "builtin_catalog",
      "catalog_revision": "sha256:...",
      "profile_candidates": [
        {
          "binding_id": "...",
          "profile_id": "openai.chat-embeddings.v1",
          "capabilities": { "chat": true, "streaming": true, "tools": true }
        }
      ]
    }
  ],
  "catalog_revision": "sha256:...",
  "fetched_at": "...",
  "expires_at": "...",
  "cached": false
}
```

### 8.2 Deployment 创建

普通客户端提交：

```json
{
  "provider_id": "...",
  "provider_model": "example-model",
  "catalog_revision": "sha256:...",
  "retained_capabilities": {
    "chat": true,
    "streaming": true,
    "tools": false
  }
}
```

后端重新解析目录并生成 Binding、Profile、Access Surface、Evidence 和完整 Snapshot。客户端提交的 Profile/Binding 不能作为权威值。

如果目录 revision 已变化，返回稳定冲突：

```text
409 model_capability_catalog_changed
```

客户端刷新模型能力后由管理员重新确认，不能静默接受新目录。

### 8.3 手动声明

手动声明使用单独的显式字段或端点，不能复用普通请求并偷偷省略目录：

```json
{
  "mode": "operator_declared",
  "provider_model": "custom-model",
  "core_operation": "chat",
  "retained_capabilities": { "chat": true, "streaming": true }
}
```

审计必须记录声明人、声明内容、Provider revision、目录失败原因和后续验证结果。

## 9. 后端实现切片

### Phase 0：契约冻结与模型目录种子

- 明确核心操作、增强特性和规格限制；
- 定义目录来源、证据等级、冲突和未知状态；
- 为 OpenAI、Azure OpenAI、Anthropic、DeepSeek、Gemini、Bedrock 各建立最小可信目录策略；
- 给模型目录生成稳定 revision/digest；
- 更新 `docs/contracts/provider-capabilities.md` 和兼容性 manifest 规则；
- 不改变运行时。

门禁：不能依赖宽泛名称猜测；每个目录条目有来源、状态和测试。

### Phase 1：模型目录能力化与单 Binding 自动选择

- 扩展 `provider.ModelDescriptor` 的 Admin 投影；
- 聚合多个 Provider Binding 的模型目录；
- 增加能力解析器和缓存版本；
- Deployment 创建由后端自动选择唯一兼容 Binding；
- 普通 UI 移除“能力接口”，改为服务商 → 模型 → 能力；
- 未知模型进入显式手动声明流程；
- 首次引导和普通新增共用初始化路径。

门禁：已知单 Binding 模型可完整创建、测试、定价、启用、路由；未知模型不能获得隐式能力。

### Phase 2：能力快照、漂移与迁移

- 给 Deployment 持久化 Model Capability Snapshot；
- 增加 catalog revision 冲突检查；
- 增加 `current / review_available / drifted / legacy` 状态；
- 目录变化触发复核，而非修改在线 Deployment；
- 任何能力变更推进 Deployment revision 并使测试 stale；
- Admin 增加能力来源、差异和复核界面；
- Backup/Restore、审计和导出包含快照字段。

门禁：目录刷新、进程重启和备份恢复后，在线能力不发生静默变化。

### Phase 3：多 Operation Binding Deployment

- 引入 `operation_bindings`；
- Registry/Router 根据请求核心操作选择内部 Adapter；
- 各 Operation 独立健康、能力证据和错误归类；
- 价格与 Usage 按 Operation 选择正确维度；
- 资源型操作继续 owner pinning；
- 更新路由、readiness、测试和审计。

门禁：同一 Provider + Model 只有在目录明确支持时才能跨 Profile；任一 Operation 未通过验证时不得宣称整体 Ready。

## 10. 迁移方案

### 10.1 既有 Provider

- 保留现有 Profile Bindings，不重新解释凭据或协议；
- 新目录在现有 Binding 上构建；
- Provider revision 不因只读目录刷新自动增加；
- 修改 Binding 仍按现有拓扑 mutation 和热加载规则执行。

### 10.2 既有 Deployment

下一次 schema 迁移为每个 Deployment 生成：

- `source = legacy_snapshot`；
- `catalog_revision = ""`；
- Snapshot capabilities = 当前 Deployment capabilities；
- Operation Binding 从现有 `binding_id` 推导；
- `review_state = legacy`。

迁移不自动增加或删除能力，不自动启用/停用 Deployment。管理员后续显式“采用目录能力”时创建新 revision、重新测试并重新启用。

如果项目在正式发布前决定允许重置数据，可简化迁移实现，但测试仍需覆盖旧结构拒绝被误读的情况；不能让未识别字段悄悄降级成 Provider 上限。

### 10.3 API 兼容

- 读取响应在过渡期保留 `binding_id/profile_id` 作为诊断字段；
- 普通创建请求逐步从客户端指定 Binding 迁移为服务器解析；
- 旧客户端请求必须显式进入 legacy 路径或被拒绝，不能与新目录解析混合后产生不同结果；
- 契约冻结后再决定是否需要版本化 Admin API；Gateway 北向协议不因本升级改变。

## 11. 安全与正确性

### 11.1 Fail-closed

- 未知、冲突、过期且无法复核的能力不进入新 Deployment；
- 模型目录不可用不回退到 Provider 全能力；
- 请求需要的能力必须同时存在于 Deployment Snapshot 和当前运行时 Adapter；
- 无匹配 Adapter 时在 Provider I/O 前返回稳定 `unsupported_feature`；
- 可能已经执行的 Provider 操作不能因另一个 Binding 可用而重试。

### 11.2 SSRF 与凭据

- 模型目录继续使用已绑定 Provider Adapter 和安全 Transport；
- Admin 不能提交任意目录 URL；
- 浏览器不接收或持久化 Provider 密钥；
- 目录错误不得回显上游敏感响应体；
- 缓存键不能包含凭据或其派生片段。

### 11.3 审计

至少记录：

- `deployment.capability_snapshot.created`；
- `deployment.capability_snapshot.reviewed`；
- `deployment.capability_drift.detected`；
- `deployment.operator_capabilities.declared`；
- `deployment.operation_bindings.changed`。

事件包含 Deployment、Provider、模型、目录 revision、变更前后摘要和管理员身份，不包含凭据。

## 12. 测试与验收

### 12.1 单元测试

- Provider ceiling、模型能力和管理员收窄的交集；
- 能力依赖关系；
- 数值限制收紧；
- 证据等级不能伪造提升；
- 精确/别名/未知/冲突模型匹配；
- 模型目录 digest 的确定性；
- Operation → Binding 自动解析。

### 12.2 存储与迁移测试

- 旧 Deployment 迁移为 Legacy Snapshot；
- 目录刷新不改变持久化能力；
- 能力复核推进 revision；
- stale ETag/目录 revision 被拒绝；
- Backup/Restore 保留快照、来源和 Operation Bindings；
- schema kill-point 不产生半迁移状态。

### 12.3 Admin API 测试

- 聚合模型目录不把 `/models` 存在性当作能力证据；
- 已知模型自动选 Binding；
- 未知模型普通创建被拒绝；
- 手动声明保持 Declared；
- 超出模型目录或 Provider 上限被拒绝；
- catalog revision 变化返回稳定 409；
- 目录失败不回退到全能力。

### 12.4 Runtime 测试

- 请求能力在 Provider I/O 前验证；
- Operation 选择正确 Adapter；
- 多 Binding 不跨操作盲重试；
- 健康、价格、Usage 和资源 owner 使用相同 Operation Binding；
- 模型能力快照在热加载和进程重启后保持一致。

### 12.5 前端测试

- 选择服务商后才加载模型；
- 选择模型后只显示支持能力；
- 不支持能力不渲染；
- 管理员只能取消，不能添加；
- 未知/冲突/目录失败有明确路径；
- 深链和普通新增表单状态一致；
- URL/request race 不把旧服务商目录应用到新选择；
- 只读角色、键盘、焦点、窄屏和长模型列表可用。

### 12.6 真实服务商门禁

自动测试通过不代表目录事实有效。每个正式支持的 Provider 需要记录：

- 真实模型列表响应；
- 目录匹配结果；
- 每个核心 Operation 的显式烟雾测试；
- 不支持能力在 Provider I/O 前被拒绝；
- 目录与实际行为冲突时的处置记录。

## 13. 可观测性

建议增加低基数指标：

- 模型目录刷新成功/失败次数，按 Provider type、Profile、结果分类；
- `known / partial / unknown / conflicting` 模型数量；
- capability drift 检测次数；
- 手动声明 Deployment 数量；
- catalog revision 冲突次数；
- Operation Binding 测试成功/失败次数。

禁止把模型 ID、Provider ID、Deployment ID 或凭据相关值直接作为 Prometheus label。具体对象只进入审计和受控日志。

## 14. 风险与缓解

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| 服务商模型能力变化快 | 内置目录过时 | 版本化目录、显式 revision、真实门禁；不自动扩张 |
| `/models` 不含能力 | 无法自动识别 | 目录 enrichment；未知模型显式声明 |
| 名称规则误匹配未来模型 | 错误开启能力 | 精确匹配优先，宽泛前缀禁止 |
| 同名模型因区域/账户不同而变化 | 缓存污染 | 键包含 Provider、Profile、目标和区域 |
| 多 Binding 增加调度复杂度 | 错 Adapter、错价格、错重试 | Phase 3 独立门禁，未完成前不伪装支持 |
| 目录更新影响在线流量 | 非预期行为变化 | 不可变 Snapshot、显式复核、重新测试 |
| UI 隐藏 Profile 后难以诊断 | 运维信息不足 | 高级详情显示 Profile、Binding、来源和证据 |
| 手动声明被当作已验证 | 能力过度承诺 | Evidence 保持 Declared；每项验证单独升级 |

## 15. 发布门禁

全部满足后才能宣布完成：

- [ ] 普通 Deployment 创建不要求理解或选择内部 Profile。
- [ ] 已知模型只显示目录支持能力，管理员只能收窄。
- [ ] 未知模型默认零能力并进入显式声明流程。
- [ ] Deployment 保存模型能力快照、目录 revision、来源和证据。
- [ ] 目录刷新不会静默改变在线 Deployment。
- [ ] 能力变化使健康测试 stale，并要求重新验证。
- [ ] Provider ceiling、模型目录和管理员收窄在后端权威校验。
- [ ] 深链与普通新增没有初始化差异或请求竞态。
- [ ] Backup/Restore、审计、热加载和重启保持能力快照。
- [ ] 多 Binding 若未完成全部运行时门禁，则明确保持不可用。
- [ ] 完整 Go、Race、Vet、前端测试、生产构建和浏览器验收通过。
- [ ] 真实 Provider 能力证据已进入 `docs/verification/provider-real-matrix.md`。

## 16. 建议实施顺序

优先执行 Phase 0、Phase 1 和 Phase 2。它们已经解决当前最重要的问题：模型能力不再由 Provider 上限默认推导，普通用户不再直接选择内部 Profile，在线 Deployment 不受目录漂移静默影响。

Phase 3 只在真实模型确实需要同一 Provider + Model 跨多个内部 Profile 时进入开发。不能为了界面上的“全选”提前扩大运行时；也不能因为 Phase 3 尚未完成，就继续让单模型错误继承 Provider 的全部能力。
