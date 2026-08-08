# 基于服务商与模型的能力选择升级方案

- 状态：Reviewed / 待实施（评审意见已并入正文，不单列评审章节）
- 生产就绪：否
- 范围：Provider 模型目录、Deployment 能力快照、Profile Binding 自动映射、Admin 创建流程、测试与重新初始化
- 当前实现：仍由管理员先选择内部“能力接口”，再填写模型 ID；本文描述的模型级能力目录与自动映射尚未实现
- 数据影响：**需要重新初始化数据目录**。项目处于 pre-1.0.0，无已发布实例需要兼容，因此不提供迁移路径、不保留 legacy 能力来源与 legacy 创建路径（见 §10）
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
- 只有 Halro 内置目录可以让能力默认处于勾选状态；服务商返回的元数据一律需要管理员显式确认（见 §4.1）。
- 目录与上游元数据都不得放宽 Beta Profile（Gemini、Bedrock、Bedrock Mantle）被刻意钉死的能力上限，即使上游声称支持。

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

| 来源 | 含义 | 是否可扩张 Deployment | 是否可默认勾选 |
| --- | --- | --- | --- |
| `builtin_catalog` | Halro 内置、版本化且经过评审的模型目录 | 是，但仅在创建或显式复核时 | 是 |
| `provider_metadata` | 服务商 API 返回的结构化能力元数据 | 是，前提是 Adapter 对字段语义有明确契约 | 否 |
| `verified_probe` | 明确的无副作用或用户触发验证结果 | 是，受探测覆盖范围限制 | 否 |
| `operator_declared` | 管理员对未知模型的显式声明 | 是，但证据保持 Declared，不能伪装成 Verified | 否 |
| `unsupported` | 已知不支持或无法证明 | 否 | 否 |

`provider_metadata` 是**外部输入**：上游返回什么，Halro 就会据此展示什么。如果它既能扩张能力又默认勾选，等于把“默认启用哪些能力”的决定权交给上游。因此它只能渲染为“可用 · 待确认”，必须由管理员显式勾选，且写入 Snapshot 的证据等级不得高于 `declared`。只有 `builtin_catalog` 允许默认勾选。

证据等级沿用现有 `verified / declared / unsupported`（见 `internal/domain/provider_profile.go`）。既有的 `legacy` 等级随本方案一并删除，不保留为占位；`internal/gateway/service.go` 中的 `LegacyUnprofiled` 分支需在实施时一并评估去留，避免出现两套互不相同的 legacy 语义。除布尔值外，Snapshot 还必须记录来源与目录版本。

### 4.2 有效能力计算

对每个布尔能力：

```text
effective = provider_ceiling
         && model_snapshot_supports
         && operator_retains
```

数值限制**不做静默钳制，而是拒绝**。现有实现（`internal/domain/provider_profile.go` 的 `capabilityLimitSubset`）的语义是：

```text
上游 available == 0  → 该层未声明上限，候选取任意非负值
上游 available >  0  → 候选必须 > 0 且 <= available，否则拒绝保存
```

`0` 表示“该层没有声明上限”，不能用于擦除上游已经声明的非零限制。模型目录层沿用同一条规则：管理员填写的限制超出模型目录或 Provider 上限时返回校验错误，**不要改写成 `min()` 自动取小** —— 静默钳制会把管理员的越界输入藏起来，事后无法区分“他就想要这个值”和“系统替他改小了”。

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

第 2 步复用既有机制，不要新造状态：`Deployment` 已有 `LastTestRevision`，与 `Revision` 比较即可判定测试是否过期（见 `internal/domain/models.go`）。再引入一个独立的 stale 标志会产生两套判定，且必然在某个路径上不一致。

### 4.4 Profile 收窄的启动期核对

能力漂移不只来自上游目录，也来自 Halro 自身：二进制升级可能收窄某个 Provider Profile 的上限，而已持久化的 Snapshot 仍声称支持。若只按 §11.1 在请求路径拦截，这类问题会以“线上每个请求返回 400”的形式被发现，而不是在启动时。

因此启动与热加载时必须对所有已启用 Deployment 执行一次核对：

- Snapshot 能力 ⊄ 当前 Profile 上限时，置为 `drifted` 并阻止其进入路由候选；
- 结果计入 `halro doctor` 输出与审计，不是只写一行日志；
- 核对失败本身不得阻止进程启动，但受影响 Deployment 一律 fail-closed。

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
    ModelRevision    string // (provider type, profile, model, region) 的单模型 digest
    CatalogRevision  string // 整个目录的 digest，仅用于诊断与缓存观察
    ProfileCandidates []ModelProfileCandidate
}

type ModelProfileCandidate struct {
    BindingID    string
    ProfileID    ProviderProfileID
    Capabilities ProviderCapabilities
}
```

约束：

- 上游 `/models` 只提供模型存在性，不自动产生能力布尔值（现有 `provider.ModelLister` 的注释已经写明这一点，本方案不改变它）；
- 能力由 Adapter 的目录解析器合并；
- 同一能力出现冲突时状态为 `conflicting`，该能力 fail-closed；
- 响应上限继续保持 10,000 项和 5 分钟缓存（见 `internal/app/admin_provider_models.go`），但能力目录版本必须进入缓存键或响应元数据；
- **冲突检查以 `ModelRevision` 为准，不用整个目录的 digest**。目录级 digest 会因任何一个无关模型上下线而 rotate，叠加 5 分钟 TTL 后，管理员会在完全没有触碰自己那个模型的情况下反复收到 409；
- 刷新只访问 Provider 已绑定、已验证的模型目录地址，不接受用户控制的任意 Admin URL。

### 5.2 Deployment 能力快照

在现有 `Deployment` 上增加：

```go
type ModelCapabilitySnapshot struct {
    ProviderModel     string
    ModelRevision     string // 冲突检查与漂移比较的基准
    CatalogRevision   string // 诊断用
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
    CapabilityReviewState   string // current | review_available | drifted
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
- **仅 `builtin_catalog` 来源的核心操作与增强特性默认选中**；来自 `provider_metadata` 或 `verified_probe` 的能力渲染为“可用 · 待确认”，默认不选中，需管理员逐项勾选；
- 管理员可以关闭，但不能添加目录外能力；
- 精确区分“视觉输入”与“图像生成”；
- 显示能力证据与目录版本，但 Profile ID 只放在高级详情中。

#### 步骤四：规格与发布

- 展示目录中的上下文与输出限制；
- 管理员只能进一步收紧；
- 新 Deployment 仍保存为停用；
- 执行与所选能力一致的测试后才能启用；
- 首次引导根据权威 readiness 自动推进。

### 7.2 模型变化与编辑态

Provider、模型目标、区域或核心 Operation 发生变化时，不允许在现有 Deployment 上就地修改；沿用“创建替代 → 测试 → 切换路由”的流程。

增强特性的编辑按方向区分，不能一概而论：

| 操作 | 是否允许就地修改 | 后续要求 |
| --- | --- | --- |
| 关闭一项已启用能力（收窄） | 是 | 推进 Deployment revision、写审计；不要求重新测试，可保持启用 |
| 开启一项目录支持但此前未启用的能力（扩张） | 是 | 推进 revision、使测试 stale、重新测试、显式重新启用 |
| 修改核心 Operation、Provider、模型目标、区域 | 否 | 创建替代 Deployment |

**收窄能力必须先做路由预检。** `internal/gateway/service.go` 的 `filterSemanticCapabilities` 直接按 Deployment 能力过滤候选，候选集为空时请求返回 `400 unsupported_feature`。因此“关闭能力”与“采用目录能力后能力集变化”这两个动作，在提交前必须返回受影响的 Route 列表，明确指出哪些 Route 会因此失去唯一候选，由管理员确认后才执行。

### 7.3 错误与加载状态

- 模型目录加载前不挂载依赖目录数据的表单状态，避免深链竞态；
- 目录失败时保留手动模型 ID 入口，但进入未知模型 fail-closed 流程。这条兜底是本方案的 kill-switch：**目录解析整体不可用时，管理员仍必须能够只凭模型 ID 完成部署**，不允许把能力目录做成创建流程的唯一入口；
- Profile ID 与 Binding ID 收进“高级详情”是**永久设计**，不是过渡安排。它们是运维诊断所需的稳定字段，读接口必须长期提供（与 §10.2 一致）；
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
      "model_revision": "sha256:...",
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
  "degraded_bindings": [
    { "binding_id": "...", "error_class": "provider_unavailable" }
  ],
  "fetched_at": "...",
  "expires_at": "...",
  "cached": false
}
```

聚合的失败与延迟语义必须显式定义，不能沿用单 Binding 的隐含行为：

- **并发获取，不串行。** 现状是单 Binding 一次上游调用，超时取 `AttemptResponseHeaderTimeout`（缺省 15s）。聚合 N 个 Binding 若串行，最坏耗时 N×15s，会把 Admin 页面拖死。需要设定并发上限与单 Binding 超时。
- **部分失败降级，不整体 502。** 任一 Binding 失败时，返回其余 Binding 的模型并在 `degraded_bindings` 中标明缺失来源与错误分类，UI 明确提示“该服务商部分能力接口未返回目录”。只有全部 Binding 都失败才整体报错。
- **缓存仍按 Binding 粒度存储**（沿用现有 `provider_id + binding_id` 缓存键），聚合在读取时完成，避免一个 Binding 的刷新使整份聚合结果失效。
- 降级结果**不得**被当作“该能力不存在”的证据：缺失来源只产生 `unknown`，不产生 `unsupported`。

### 8.2 Deployment 创建

普通客户端提交：

```json
{
  "provider_id": "...",
  "provider_model": "example-model",
  "model_revision": "sha256:...",
  "retained_capabilities": {
    "chat": true,
    "streaming": true,
    "tools": false
  }
}
```

后端重新解析目录并生成 Binding、Profile、Access Surface、Evidence 和完整 Snapshot。客户端提交的 Profile/Binding 不能作为权威值。

如果**所选模型的** `model_revision` 已变化，返回稳定冲突：

```text
409 model_capability_changed
```

冲突判定只比较所选模型的 digest。目录中其他模型的增减、排序或 `owned_by` 变化不构成冲突 —— 否则在 5 分钟 TTL 下，服务商每上线一个无关模型都会让正在创建的 Deployment 收到 409，管理员会迅速学会无脑重试，冲突检查就此失去意义。

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
- 给模型目录生成稳定的单模型 digest 与目录 digest；
- **确定目录的载体与发布节奏**：内置目录随二进制发布，意味着服务商上线新模型后，管理员必须等 Halro 发版才能获得“已知能力”。Phase 0 必须回答：目录以 Go 源码还是嵌入式 JSON 形式存在、如何评审、发版节奏，以及是否与既有的定价目录（`halro pricing migrate` 那一套）合并为同一套“目录发布”流程。若不合并，需说明理由 —— 两套版本化目录各自演进是长期维护成本；
- 更新 `docs/contracts/provider-capabilities.md` 和兼容性 manifest 规则；
- 不改变运行时。

门禁：不能依赖宽泛名称猜测；每个目录条目有来源、状态和测试；目录发布方式已确定并写入契约文档。

### Phase 1：模型目录能力化与单 Binding 自动选择

- 扩展 `provider.ModelDescriptor` 的 Admin 投影；
- 聚合多个 Provider Binding 的模型目录；
- 增加能力解析器和缓存版本；
- Deployment 创建由后端自动选择唯一兼容 Binding；
- 普通 UI 移除“能力接口”，改为服务商 → 模型 → 能力；
- 未知模型进入显式手动声明流程；
- 首次引导和普通新增共用初始化路径。

门禁：已知单 Binding 模型可完整创建、测试、定价、启用、路由；未知模型不能获得隐式能力。

### Phase 2：能力快照与漂移

- 给 Deployment 持久化 Model Capability Snapshot；
- 增加单模型 revision 冲突检查；
- 增加 `current / review_available / drifted` 状态；
- 目录变化触发复核，而非修改在线 Deployment；
- 任何能力变更推进 Deployment revision，并通过 `LastTestRevision` 使测试 stale；
- 增加启动与热加载时的 Profile 收窄核对（§4.4），结果进入 `halro doctor`；
- 能力收窄前返回受影响 Route 预检结果（§7.2）；
- Admin 增加能力来源、差异和复核界面；
- Backup/Restore、审计和导出包含快照字段。

门禁：目录刷新、进程重启和备份恢复后，在线能力不发生静默变化；Profile 收窄在启动时被发现，而不是在请求路径上。

### Phase 3：多 Operation Binding Deployment

- 引入 `operation_bindings`；
- Registry/Router 根据请求核心操作选择内部 Adapter；
- 各 Operation 独立健康、能力证据和错误归类；
- 价格与 Usage 按 Operation 选择正确维度；
- 资源型操作继续 owner pinning；
- 更新路由、readiness、测试和审计。

门禁：同一 Provider + Model 只有在目录明确支持时才能跨 Profile；任一 Operation 未通过验证时不得宣称整体 Ready。

## 10. 数据影响与重新初始化

项目处于 pre-1.0.0：没有已发布版本，没有需要保持兼容的在外部署，操作员自行重新初始化实例。CLAUDE.md 对这一阶段的要求是**就地修正，不积累兼容层** —— 错误的构造不得与其替代品并存。因此本方案**不提供迁移路径**。

### 10.1 直接修改，不保留旧构造

- Deployment 的能力结构直接改为“Provider 上限 ∩ 模型 Snapshot ∩ 管理员保留”，不保留旧的“从 Provider 上限继承”读路径；
- 不引入 `legacy_snapshot` 能力来源，不引入 `review_state = legacy`；
- 一并删除 `CapabilityEvidence` 中的 `legacy` 等级，并评估 `internal/gateway/service.go` 中 `LegacyUnprofiled` 分支的去留 —— 保留它就等于保留了第二套 legacy 语义；
- 不保留“旧客户端 legacy 创建路径”。Admin API 的创建请求只有一种解析方式：服务端按目录解析，客户端提交的 Profile/Binding 一律不作为权威值。

### 10.2 需要写明的操作影响

- 本变更**需要重新初始化数据目录**（`make reset CONFIRM=RESET` 或重新 `init`）。这一点必须写进变更说明与发布说明，以便操作员提前安排；
- 持久化结构版本号必须递增，使旧结构被**明确拒绝并重建**，而不是被静默误读为“未声明即继承 Provider 上限”；
- 读接口长期提供 `binding_id / profile_id` 作为诊断字段。这不是过渡安排：UI 把它们收进高级详情之后，它们是排障时唯一能定位内部 Adapter 的线索（与 §7.3 一致）。

### 10.3 既有 Provider 拓扑

- 保留现有 Profile Bindings，不重新解释凭据或协议；
- 新目录在现有 Binding 上构建；
- Provider revision 不因只读目录刷新自动增加；
- 修改 Binding 仍按现有拓扑 mutation 和热加载规则执行；
- Gateway 北向协议不因本升级改变。

## 11. 安全与正确性

### 11.1 Fail-closed

- 未知、冲突、过期且无法复核的能力不进入新 Deployment；
- 模型目录不可用不回退到 Provider 全能力；聚合目录部分失败时缺失来源只产生 `unknown`，不产生 `unsupported`；
- 请求需要的能力必须同时存在于 Deployment Snapshot 和当前运行时 Adapter；
- Snapshot 超出当前 Profile 上限时在**启动期**即被标记 `drifted` 并移出路由候选（§4.4），不把发现时机留到请求路径；
- 无匹配 Adapter 时在 Provider I/O 前返回稳定 `unsupported_feature`；
- 可能已经执行的 Provider 操作不能因另一个 Binding 可用而重试；
- **目录、上游元数据与管理员声明都不得放宽 Beta Profile 的能力上限。** Gemini、Bedrock、Bedrock Mantle 的能力边界是被刻意钉死的，放宽属于需要独立契约评审的决定，不能作为本方案的副作用发生。

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

### 12.2 存储与重新初始化测试

- 旧结构被**明确拒绝**，不会被误读为“未声明即继承 Provider 上限”；
- 持久化结构版本号未递增时拒绝启动，而不是降级读取；
- 目录刷新不改变持久化能力；
- 能力复核推进 revision，并使 `LastTestRevision` 判定为 stale；
- stale ETag / 单模型 revision 被拒绝；
- Backup/Restore 保留快照、来源和 Operation Bindings。

### 12.3 Admin API 测试

- 聚合模型目录不把 `/models` 存在性当作能力证据；
- 已知模型自动选 Binding；
- 未知模型普通创建被拒绝；
- 手动声明保持 Declared；
- 超出模型目录或 Provider 上限**被拒绝**，而不是被静默钳制到上限；
- 所选模型的 revision 变化返回稳定 409；目录中**无关模型**的增减不触发 409；
- 部分 Binding 失败时返回其余模型并在 `degraded_bindings` 标注，不整体 502；全部失败才报错；
- 聚合调用受并发上限与单 Binding 超时约束，不随 Binding 数量线性放大延迟；
- 目录失败不回退到全能力。

### 12.4 Runtime 测试

- 请求能力在 Provider I/O 前验证；
- Operation 选择正确 Adapter；
- 多 Binding 不跨操作盲重试；
- 健康、价格、Usage 和资源 owner 使用相同 Operation Binding；
- 模型能力快照在热加载和进程重启后保持一致；
- **Profile 收窄后启动**：Snapshot 超出新上限的已启用 Deployment 在启动核对中被置为 `drifted` 且不进入候选，`halro doctor` 能看到；核对本身不阻止进程启动。

### 12.5 前端测试

- 选择服务商后才加载模型；
- 选择模型后只显示支持能力；
- 不支持能力不渲染；
- 管理员只能取消，不能添加；
- 仅 `builtin_catalog` 能力默认勾选；`provider_metadata` 能力渲染为待确认且默认未勾选；
- 收窄能力时展示受影响 Route 预检结果，管理员确认后才提交；
- 目录整体不可用时手动输入模型 ID 的通道仍然可用；
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

- 模型目录刷新成功/失败次数（Counter），按 Provider type、Profile、结果分类；
- 聚合目录部分降级次数（Counter），按 Provider type、错误分类；
- `known / partial / unknown / conflicting` 模型数量（Gauge）；
- capability drift 检测次数（Counter），区分“目录漂移”与“Profile 收窄”两个来源；
- 手动声明 Deployment 数量（Gauge）；
- 单模型 revision 冲突次数（Counter）；
- Operation Binding 测试成功/失败次数（Counter）。

Counter 与 Gauge 必须在设计阶段就定死，不能实现时临时决定 —— 两者的告警写法完全不同。新增指标需同步既有契约文档 `docs/contracts/metrics-reference.md`，否则指标契约与实现分叉。

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
| UI 隐藏 Profile 后难以诊断 | 运维信息不足 | 高级详情显示 Profile、Binding、来源和证据（永久字段，非过渡） |
| 手动声明被当作已验证 | 能力过度承诺 | Evidence 保持 Declared；每项验证单独升级 |
| 目录 digest 粒度过粗 | 409 频繁误伤，管理员学会无脑重试 | 冲突检查按单模型 digest，无关模型变化不触发 |
| 内置目录随二进制发布 | 新模型要等发版才能“已知” | Phase 0 定死载体与发布节奏；手动声明通道始终可用 |
| 聚合多 Binding 目录 | Admin 页面延迟放大、单点失败拖垮整页 | 并发上限、单 Binding 超时、部分失败降级为 `degraded_bindings` |
| 上游元数据默认启用能力 | 上游变相决定 Halro 的启用项 | 仅内置目录可默认勾选；元数据一律待确认且证据不超过 Declared |
| 二进制升级收窄 Profile | 线上按请求报 400 才被发现 | 启动与热加载核对，置 `drifted` 并进入 `halro doctor` |

## 15. 发布门禁

全部满足后才能宣布完成。状态核对于 2026-08-09，逐条对照代码验证，依据见 §17。

- [ ] 普通 Deployment 创建不要求理解或选择内部 Profile。**未达成** —— 创建表单仍渲染「能力接口」选择器。
- [ ] 已知模型只显示目录支持能力，管理员只能收窄。**部分** —— 后端拒绝超集，但 UI 的勾选框来自 Profile 上限而非目录。
- [x] 未知模型默认零能力并进入显式声明流程。
- [ ] 仅内置目录能力默认勾选，上游元数据能力默认待确认。**部分** —— 前半达成；`provider_metadata` 来源无生产调用点，后半无从演示。
- [ ] Deployment 保存模型能力快照、单模型 revision、来源和证据。**部分** —— §5.2 要求的 `Evidence`、`OperatorDisabled`、`OperationBindings` 三个字段均未实现。
- [x] 目录刷新不会静默改变在线 Deployment；无关模型变化不产生 409。
- [x] 能力变化通过 `LastTestRevision` 使健康测试 stale，并要求重新验证。
- [x] Provider ceiling、模型目录和管理员收窄在后端权威校验；越界值被拒绝而非钳制。
- [x] Profile 收窄在启动核对中被发现，`halro doctor` 可见。
- [x] 能力收窄前给出受影响 Route 预检。
- [x] 目录整体不可用时，手动输入模型 ID 的通道仍可完成部署。
- [x] 深链与普通新增没有初始化差异或请求竞态。
- [x] Backup/Restore、审计、热加载和重启保持能力快照。
- [ ] 代码中不存在 legacy 能力来源、legacy 复核状态或 legacy 创建路径。**基本达成，有一处残留** —— 见 §17.2 D4。
- [ ] 变更说明与发布说明已写明需要重新初始化数据目录。**部分** —— schema 21、22 已写明，schema 20（能力快照本身）从未被单独宣告。
- [x] 多 Binding 若未完成全部运行时门禁，则以**可断言的方式**保持不可用：提交 `operation_bindings` 的请求返回 400，并有对应测试用例；仅靠 UI 不暴露入口不算数。
- [x] 新增指标已同步 `docs/contracts/metrics-reference.md`。
- [ ] 完整 Go、Race、Vet、前端测试、生产构建和浏览器验收通过。**部分** —— 自动化部分通过；浏览器验收未做。
- [ ] 真实 Provider 能力证据已进入 `docs/verification/provider-real-matrix.md`。**未达成**。

计数：11 达成、6 部分、2 未达成。

## 16. 建议实施顺序

优先执行 Phase 0、Phase 1 和 Phase 2。它们已经解决当前最重要的问题：模型能力不再由 Provider 上限默认推导，普通用户不再直接选择内部 Profile，在线 Deployment 不受目录漂移与 Profile 收窄的静默影响。

由于不做迁移（§10），Phase 1 与 Phase 2 之间不存在“新旧结构并存”的中间态：结构版本号递增之后，旧数据目录被明确拒绝并重建。这也意味着这两个 Phase 应当在同一个可发布区间内完成，不要让主干长期停在“已改结构、未落快照”的状态。

Phase 3 只在真实模型确实需要同一 Provider + Model 跨多个内部 Profile 时进入开发。不能为了界面上的“全选”提前扩大运行时；也不能因为 Phase 3 尚未完成，就继续让单模型错误继承 Provider 的全部能力。

## 17. 实施现状与剩余工作（2026-08-09）

本节记录截至 2026-08-09 的落地情况和尚未完成的工作。所有结论都逐条对照代码验证过，行号为验证时的位置。

### 17.1 已落地

Phase 0、Phase 1、Phase 2 的**执行机制**已经完成并有测试覆盖，对应 PR #114–#130：

| 切片 | PR |
| --- | --- |
| Phase 0 目录种子与契约冻结 | #114 |
| Phase 1 聚合发现、后端解析 Binding、控制台流程 | #115、#116 |
| Phase 2 快照持久化（schema 20） | #117、#119 |
| Phase 2 漂移在加载期核对、进入 `halro doctor` | #121、#122 |
| Phase 2 复核状态派生化、Route 预检 | #124、#126 |
| Phase 2 漂移改为扣留而非拒绝启动、能力审计事件、扩张需重新验证 | #125、#126 |
| §13 可观测性指标 | #127 |
| 移除 legacy 能力证据与 unprofiled adapter（schema 21） | #128 |
| 路由必须指向部署（schema 22） | #129 |
| `operation_bindings` 不可用断言、只读角色门禁、备份保留快照断言 | #130 |

最强的部分是漂移核对、Route 预检、审计、指标，以及三道 fail-closed 迁移（schema 20/21/22）。这些都对真实数据目录验证过，不只是 fixture。

### 17.2 剩余工作

按建议顺序排列。A 组是本方案存在的理由，应当优先。

#### A. 用户流程 —— 本方案的核心承诺尚未兑现

§1.1 开篇陈述的问题是「当前流程暴露了内部实现」，而这一点目前仍然成立。

- **A1. 创建表单仍要求选择「能力接口」。** `web/src/pages/DeploymentsPage.tsx:790` 渲染 `Field label={t("deployments.binding")}`，中文标签即「能力接口」，提示语为「选择该模型实际使用的能力接口和协议」。只要服务商有多于一个启用的 Binding 就会显示，而这是 OpenAI 的常态。按 §7.3，Profile ID 与 Binding ID 应当收进「高级详情」，不出现在普通创建路径上。
- **A2. 能力勾选框来自 Profile 上限而非模型目录。** `DeploymentsPage.tsx:658` 的 `configurableCapabilityNames` 由 `capabilityCeiling` 推导。§7.1 步骤三要求只渲染模型目录支持的能力；目前不支持的能力仍会渲染且可勾选，管理员要到保存时才被服务端拒绝。
- **A3. 控制台始终发送 `binding_id`，架空了后端的聚合。** §8.1 规定 `binding_id` 仅作为高级诊断过滤条件，但 `DeploymentsPage.tsx:668` 无条件带上 `selectedBinding?.id`，于是「正常情况」和「诊断情况」颠倒了。
- **A4. 内置目录与模型选择器没有交集。** 内置目录只有 4 条，全部是 Bedrock（`internal/modelcatalog/builtin.go:27-30`）；而模型选择器只对 `openai / deepseek / openai_compatible` 启用（`DeploymentsPage.tsx:665`）。因此**没有任何目录覆盖的模型能在控制台里被选到**，所有路径都走 §6.3 的未知模型兜底。这使门禁 2、4 目前是「空真」而非被证明。需要决定：给这三类服务商补种子，还是把 Bedrock 纳入选择器，或两者都做。

#### B. §5.2 缺失的快照字段

- **B1. `Evidence`** —— 快照未保存证据等级。当前证据取自 Binding 而非目录条目来源，其上界靠 `admin_deployments.go` 中一次无条件的 verified→declared 降级实现，而不是 `entry.Source.MaxEvidence()`。
- **B2. `OperatorDisabled`** —— 未实现。只存保留集的后果是：「管理员主动关掉」与「目录本来就没有」无法区分，而这正是该字段存在的理由。
- **B3. `OperationBindings`** —— 属于 Phase 3，见 E。

#### C. 与文档措辞的偏差（已实现但形态不同，需确认是否接受）

- **C1. 复核状态改为派生，不持久化。** §5.2 将其列为 `Deployment` 字段。改动理由记录在 `internal/app/capability_drift.go` 顶部：比较的两侧都会在记录不被改写的情况下变化，存下来只能记录上次写入时的状态。**建议接受并回改文档。**
- **C2. §13 第 3 项指标统计的是 Deployment 而非 Model**，第 7 项统计的是 Deployment 测试而非 Operation Binding 测试（后者属 Phase 3）。**建议接受并回改文档。**
- **C3. `Clamp`/`Merge` 对数值上限取 `min()`**，而 §4.2 写的是「不要改写成 `min()`」。代码的区分是「派生层之间取窄」与「管理员输入越界则拒绝」，后者确实是拒绝而非钳制。**建议澄清文档措辞。**

#### D. 欠账

- **D1. CHANGELOG 未宣告 schema 20。** `[Unreleased]` 有 schema 21、22 的重置说明，但能力快照本身——本方案的主题——从未被单独宣告为需要重新初始化的原因。§10.2 要求写进变更说明。
- **D2. Phase 0 遗留两问未答**：目录是否与定价目录（`halro pricing migrate`）合并为同一套发布流程、若不合并的理由；以及兼容性 manifest 规则（`docs/compatibility/`）未更新。
- **D3. §12 缺失的测试**：聚合的并发上限与单 Binding 超时无任何测试；全部 Binding 失败时的 502 路径无测试；「无关模型增减不触发 409」只测了正向；§6.2 的日期别名映射既无实现也无测试。
- **D4. legacy 残留一处**：`internal/app/providers.go` 中，当部署未声明任何操作时仍会退回 Adapter 全量上限。该分支不可达（`Deployment.Validate` 会拒绝这种记录），但它正是 §10.1 要求消除的「未声明即继承 Provider 上限」形态。

#### E. Phase 3：多 Operation Binding Deployment

整体未开始，与 §16 一致。按 2026-08-09 的决定，**纳入 1.0.0 范围**。

#### F. 发布前人工项

- **F1. 真实 Provider 能力证据**进入 `docs/verification/provider-real-matrix.md`（§12.6，计费、opt-in）。
- **F2. 浏览器验收**（门禁 18 的最后一项）。

### 17.3 超出本方案范围的改动

诚实记录：以下改动在实施期间完成，但并非本文档要求，评审时应按独立变更看待。

- **路由必须指向部署（#129，schema 22）。** 本文档只谈 Deployment 的创建路径，未提及 legacy Route。该改动源于实施中发现的真实缺陷：不带 `deployment_id` 的路由会绕过版本化定价、健康探测、能力快照与部署级并发限制。同时修正了一个既有缺陷 —— 迁移 3 在同一未提交事务内合成部署，而迁移 20 的守卫读 `Stats().KeyN`，看不见同事务写入，导致 schema-2 目录可以一路迁移到当前版本并留下一个 `Deployment.Validate()` 拒绝的记录。
- **`Registry.Register` 硬拒绝非 `ProfiledAdapter`（#128）。** §4.1/§10.1 只要求「评估 `LegacyUnprofiled` 的去留」，实际做法更强：让该状态不可表示，因为原先的 fail-closed 是靠注册之后的一个分支擦除可选语义实现的，分支没覆盖到的需求会静默变成 fail-open。
- **只读角色对「测试/启用/创建替代」的门禁（#130）。** §7.3 要求只读管理员可查看但不能创建或调整，这一项算在范围内，但修正面比文档所述更广。
- **系统配置面板改版（#118、#120）** 与本方案无关。
