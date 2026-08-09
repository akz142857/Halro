# 模型能力自动识别与验证方案

- 状态：**Implemented — 待用户验收；精确 RC 的真实账号证据仍是外部发布门禁**（2026-08-09）
- 日期：2026-08-09
- 目标版本：待验收后定版
- 基线：[基于服务商与模型的能力选择升级方案](model-aware-capability-selection.zh-CN.md)
- 相关方案：[模型能力选择 1.1.0：能力组合归属路由层](model-aware-capability-selection.v1.1.0.zh-CN.md)
- 范围：Admin 创建模型部署、能力检测任务、Adapter 检测契约、检测证据、缓存与限频、审计与测试
- 当前阶段：仓库实现、自动化测试、生产构建与浏览器交互验收已完成；发布仍受 §16 的精确 RC 真实账号门禁约束。

## 评审结论（2026-08-09 多角色评审）

安全与威胁模型、架构与不变量、契约一致性、产品与前端、交付与可验证性五个角色对照真实代码评审了本文。共同结论：**方案的安全判断经得起推敲，但它现在不该做，且当前文本不能作为契约冻结的输入。**

### 为什么推迟

三条已核实的事实，按分量排序：

1. **“内置目录追不上服务商”是假设，不是已观测的事实。** `internal/modelcatalog/builtin.go` 今天只有约 24 条：OpenAI 21、DeepSeek 2、Bedrock 4，**Anthropic 零条目、Gemini 零条目**。Anthropic 是 GA Provider。也就是说 §1.1 描述的痛点在 Anthropic 上 100% 存在，而消除它的代价是往 `builtin.go` 里加二三十行，用的是已经建好、已被构建期门禁保护的机制。目录还没被填过就宣布它不够用，是拿心智模型当证据（CLAUDE.md「Verify, never assume」）。

2. **本方案需要的持久化后台任务子系统，仓库里完全不存在。** `internal/idempotency` 只剩 29 行且只有 `ValidateKey`，其包注释明写 durable request-lifecycle store 已被删除，因为“留着会读起来像同一件事的第二个实现”。`internal/alert/dispatcher.go` 是纯内存；`internal/app/runtime.go` 的后台 goroutine 全是无持久化的 ticker 循环。§7.3 的 single-flight、§7.4 的幂等存储、§6 的状态机、§9 的取消与 TTL、重启 `running → interrupted`，全部要从零建。

3. **§16 的最后两条门禁，就是已经卡住 1.0.0 的那两条。** 基线 §17.4 的 F1（真实 Provider 能力证据）与 F2（浏览器验收）状态均为“未开始”，F1 卡在“需要授权与凭据：计费、opt-in，且矩阵运行器要求精确的 RC commit”。本文 §14.5 与 §16 末两条是它们的加量版。把新方案的完成度绑在一个从未成功执行过的外部依赖上，不是排期问题。

加上 1.1.0 已经装着 HA 史诗（`halro-ha-architecture.zh-CN.md` 自称“§19 的门禁本身就是数月量级的验证工程”），1.1.0 塞不下本方案。

### 解除 Deferred 的判据

判据不是日期，是数据。1.0.0 在生产运行一段时间后，统计 `deployment.operator_capabilities.declared` 审计事件（`internal/app/capability_audit.go:20` 今天已经在发，`:39` 已记 `source`）：

- 手工声明占全部 Deployment 创建的比例；
- 按 Provider 与 `target_kind` 的分布。

据此分流：

| 观测结果 | 动作 |
| --- | --- |
| 集中在 `azure_deployment` / `custom_endpoint_model` | 做下面的 S2，本方案继续 Deferred |
| 集中在 Anthropic / Gemini 等目录空白的 Provider | 做下面的 S1，本方案继续 Deferred |
| S1 + S2 之后手工声明占比仍高，且分散在多个 Provider | 本方案挣到了它的成本，按“恢复前必须先解决”修订后重新评审 |

### 先做的更小替代

这两项各自独立、互不依赖，都不需要新 schema、新 API、新持久任务，也不会引入计费型控制面调用。

| 编号 | 内容 | 落点 | 粗估 |
| --- | --- | --- | --- |
| S1 | 补齐内置目录：Anthropic、Gemini、Bedrock Converse Text、经评审的 OpenAI-compatible 常见模型 | `internal/modelcatalog/builtin.go` 的 `builtinEntry` + `Entry.Validate`，构建期门禁已存在 | ~100–150 行 Go + 目录测试 |
| S2 | Azure Deployment 与自定义端点增加“这个目标背后是哪个已知模型”选择器，选中后套用该目录条目的能力，证据仍为 `declared` | `internal/app/admin_deployments.go` 的 `resolveDeploymentTarget`；前端 `web/src/pages/DeploymentsPage.tsx` 的 `DeploymentForm` | ~40 行 Go + ~60 行前端 + 4 个测试 |

S2 覆盖的是目录**天然**命不中的场景：`azure_deployment` 与 `custom_endpoint_model` 的目标是运维自取的名字，模型 ID 目录永远对不上。但这个问题的答案运维自己知道——那个 Deployment 是他在 Azure 门户里亲手建的。一个下拉框就够，不需要 8 次计费探测。

S1 与 S2 合计覆盖本方案价值的大部分，而复杂度低一个数量级：不需要 schema 24、durable 任务实体、幂等键存储、single-flight、TTL、冷却、取消、重启恢复、6 个指标、7 个审计事件。它们放不下的只有“一次自动勾选 10 项能力”——而那一件事恰好是全部风险的来源。

### S1 / S2 实施状态（2026-08-09）

- [x] **S1 已完成。** 内置目录补入精确的 Anthropic、Gemini、Bedrock Converse Text 与经 OpenAI-compatible Profile 收窄的常见模型条目；目录继续只产生 `declared` 证据，并由 `Entry.Validate`、精确 ID 查询、Profile ceiling 与目录 revision 测试守住边界。模型与版本依据采用 Anthropic Model IDs / Models Overview、Gemini Models API / Models guide、AWS Bedrock API compatibility 官方资料；未加入 `latest`、preview 或 experimental 别名。
- [x] **S2 已完成。** 现有 `GET /admin/api/v1/providers/{id}/models` 响应增加 `capability_models` 投影（不是新 API）；Azure Deployment 与 `custom_endpoint_model` 创建表单增加“底层已知模型”选择器。请求同时保留 `provider_model`（真实调用目标）与可选 `capability_model`（能力上限来源），后端按目标类型限制可映射目录、校验 model revision，并把最终快照来源记为 `operator_declared`。未选择时仍保留原有手工声明逃生通道。
- [x] **S2 回归门禁已补。** 覆盖 Azure 映射、自定义端点映射、协议目录隔离、过期 revision 拒绝、前端自动勾选与请求字段；选择模型、输入、搜索、hover、blur 均不产生 Provider 能力调用。
- [x] **通用自动检测已解除 Deferred 并实现。** schema 24、持久任务、幂等/single-flight、TTL/冷却、受控探测、取消/恢复、Deployment 快照、指标/审计与普通 UI 闭环均按后文契约落地；真实账号证据仍只在精确 RC 上 opt-in 执行。

### 恢复本方案前必须先解决（评审已核实，逐条给出证据）

以下五条不是实现细节，是与已落地并被测试锁住的能力模型正面冲突。**任何重启本方案的尝试都必须先处理它们**，否则会重复本次评审。

1. **§0 推翻了一条已发布契约，而 §18 的同步清单漏掉了它。**
   本文 §0“仅 `builtin_catalog` 和成功的 `verified_probe` 可以自动勾选”，对上：
   - `docs/contracts/provider-capabilities.md:63` —— “**Only the builtin catalog pre-selects.**”
   - `internal/modelcatalog/catalog.go:61` —— `func (s Source) PreselectsCapabilities() bool { return s == SourceBuiltin }`（已用代码固化，经 `internal/app/admin_provider_models.go:311` 驱动控制台勾选）
   - 基线文档同一规则出现在四处：`:158`、`:378`、`:676`、`:737`

   §18 列了基线六个章节，唯独漏了这个契约文件与基线 §4.1 / §12.5——恰好是本方案最核心的行为改变。按 pre-1.0.0“错误构造不得与替代者并存”，这必须是一次显式的契约修订，不能让新旧两套预选规则并存。

2. **检测产物构造不出一条合法的能力快照。** 两处独立的硬失败：
   - `internal/domain/models.go:674` —— 快照必填 `ModelRevision`，§9 的 `ModelCapabilityDetection` 没有任何字段能提供它；
   - `internal/domain/provider_profile.go:208` `capabilityLimitSubset(0, 128000) == false`。§9 规定“数值限制不从通用探测推断”，即 `Recommended.MaxContextTokens = 0`；于是只要 Binding 声明了非零 token 上限，普通路径就创建不出 Deployment，报错还是 `admin_deployments.go:481` 那句“能力超出 Provider 能力”这种完全误导的措辞。

3. **`verified_probe` 快照会被漂移引擎静默摘除路由。** `internal/app/capability_drift.go:140` 的豁免只给了 `operator_declared`；不满足即 `CapabilityReviewDrifted`，随后被 `internal/app/providers.go:215` 排除出路由注册表。场景很现实：今天靠检测建起 `chat+streaming+tools`，下个版本 Halro 补一条只写 `chat` 的目录条目，这条部署在**下次重启后被扣留**——而它的能力是 Halro 自己实测出来的。基线 §17.1 记录过同一个故障对 `operator_declared` 发生过一次，并留有反向验证测试 `TestCatalogGrowingUnderADeclarationIsReviewableNotDrift`。

4. **这会是第一条绕开 ledger 的计费 Provider 调用路径。** 今天控制面的 Provider 调用按构造不计费——`internal/provider/openai/adapter_test.go:239` 这个测试的名字就叫 `TestConnectionProbeUsesNonBillableEndpoint`；Deployment 测试走 `Prober.Probe`，OpenAI 侧是一次 `GET /v1/models/{model}`。而 §5.2 的十项探测全是生成式计费请求，§11 却让它们不进 ledger、不进 limiter、不进预算。这与两条 CLAUDE.md 硬不变量正面冲突：“Ledger WAL 是记账权威”、“预留必须在 Provider 请求前持久化”。§9 也没说 `ProviderCalls` 何时写盘——崩溃后无法区分这条检测是 0 次调用还是 7 次，`max_provider_calls` 在崩溃循环下形同虚设。仓库里已有正确形状可抄：`internal/gateway/inference_resources_store.go` 的 reserve → in-flight → unknown 三态，以及 `domain.ProviderResource`（`internal/domain/models.go:31-77`）几乎同形的字段集。

5. **任务级状态机从未定义。** §6 那张表是**逐能力**的；整体状态散落成六处互不相同的枚举：§3.1 七态、§8.1 `queued|running`、§8.2 `completed`、§9 `failed`、§9.1 `interrupted/inconclusive`、§14.3 `interrupted`。`interrupted` 既不在任何 API 枚举里，也没有对应审计事件——崩溃后只留一条 `started`，没有终态记录。前端侧是同一个洞：§6 定义七种能力状态，§3.4 只画了三个符号，`unsupported` / `unavailable` / `unauthorized` / `canceled` 在 UI 上无归宿，而这四种的下一步动作完全不同（接受 / 重试 / 去改凭据 / 重点一次）。Phase 0 承诺的交付物正是“冻结状态机”，它目前不存在。

上述五项均已在实现前关闭：预选契约同步到基线与能力契约；检测实体携带模型 revision，并由服务端继承数值限制；`verified_probe` 不因后续目录增长被静默漂移；每次可能计费的调用在发出前持久化为 `reserved → running`，模糊结果落为 `unknown` 且重启不重放；任务级与逐能力状态机、UI 文案及终态审计均已冻结并测试。

### 其余已核实的问题（恢复时一并处理）

| 位置 | 问题 | 证据 |
| --- | --- | --- |
| §8.4 | `retained_capabilities` 会被 400 拒绝 | 真实字段是 `capabilities`（`internal/app/admin_deployments.go:31`），且 `internal/app/admin_session.go:547` 开了 `DisallowUnknownFields()` |
| §7.1 | `credential_generation/fingerprint` 在代码中不存在，危害是双向的 | 全仓库仅出现在本文。若实现成哈希凭据材料并经 §8.2 返回给浏览器，等于给出离线确认预言机；若不实现，则换凭据不推进 `ProviderInstance.Revision`（`domain.Credential` 只有 `Revision`/`KeyVersion`），新鲜期内会**跨凭据复用**检测结果 |
| §7.1、§8.2 | 目标指纹不应出现在 API 响应与审计 metadata 中 | 其余十一个输入对调用方全部已知；GET 走 `requireAdmin`，只读管理员可见 |
| §11 | 超时预算不自洽 | `AttemptResponseHeaderTimeout` 默认 60s（`internal/config/default.go:65`），而单次检测总时长 90s，两次接近上限的探测即击穿 |
| §7.3 | “多标签页不能绕过 Provider 限制”其机制不成立 | 冷却与 single-flight 均以指纹为 key，而 `provider_model` 是无界自由文本，换个模型 ID 就是全新的桶。仓库今天没有通用 Admin mutation 限流（只覆盖登录与 step-up） |
| §8.2 | 响应示例 `binding_id: "binding_..."` 格式错误 | 真实格式是 `providerID:profileID`（`internal/domain/models.go:265`），如 `prv_xxx:openai.chat-embeddings.v1` |
| §8.1、§8.4 | 八个新错误码会在控制台退化成通用文案 | `web/src/i18n/errors.ts` 全文 29 行，只有一个按 code 的分支；四个 409 会全部显示为同一句 `errors.conflict` |
| §13.1、§13.2 | 审计事件与指标命名同既有约定有偏差 | `deployment.created_from_capability_detection` 与既有 `deployment.capability_snapshot.created` 职责重叠（`internal/app/capability_audit.go:10-13` 已写明分工原则）；指标 label `result` 在本仓库无先例，同类一律用 `status`；“禁止 Provider ID 作为 label”与 `docs/contracts/metrics-reference.md:174` 的“Provider/Deployment IDs are bounded managed identifiers”冲突 |
| §7.2 | 配置同步清单不完整 | 除 `configs/config.example.yaml` 外还须同步 `internal/config/config.go`、`internal/config/default.go`、`internal/config/default.yaml`，三者一致性由 `TestDefaultTemplateMatchesDefault` 守着 |
| §16 | 十七条中至少五条不可证伪 | “多标签页”“不信任模型自述”“临时故障不会被误记”“契约完整”，以及挂在 F1 上的真实 Provider 门禁。恢复时须逐条改写为有限、可枚举的断言 |
| §3 全节 | 示例文案用的是 ADR 词汇，相对已上线控制台是倒退 | 仓库已完成过一轮翻译：Binding→能力接口、Profile→能力配置、capability_source→保存的答案来自哪里（`web/src/i18n/locales/zh-CN.ts`），并确立了“实现词汇只能放进 `technical-details` 折叠区”的分寸。`verified_probe` / `inconclusive` / `not_probed` / `Binding` / `Profile` 一个都不应出现在主界面 |
| §3.2 | 目录命中路径加确认按钮是净损失 | 该路径今天是零点击：`web/src/pages/DeploymentsPage.tsx:737` 在选中目录模型时当场完成预选与接口绑定。一个“点了没有任何后果”的确认按钮会训练用户对真正花钱的那个按钮同样不假思索 |

### 本次评审确认无误的部分

记在这里，避免下次重新争论：

- **十七项能力名与 `domain.ProviderCapabilities` 的 JSON tag 逐字相同**，没有发明的能力名，也没有遗漏。
- **§5.4 的依赖关系与 `internal/modelcatalog/catalog.go:439` 的 `ValidateDependencies` 完全一致。**
- **schema 24 号正确且无冲突**（当前 `internal/store/bolt/store.go:24` 为 23，仓库内无第二处主张 24）；备份侧无需改动，`internal/app/backup.go:528` 是整库快照，`internal/backup/archive.go:273` 已是范围判定而非等值判定。
- **§1.3 关于 `provider.Prober` 的陈述属实**：它是连通性验证，OpenAI 非 Azure 路径确实只对模型目录发 GET，证明不了任何生成语义。
- **§8.4 的双路径不违反 pre-1.0.0 原则**：`operator_declared` 是逃生通道而非被替代的错误构造，它与检测是两种真实不同的证据来源。真正违规的是 `retained_capabilities` 这个第二种能力表示法（见上表）。
- **`endpoint-manifests.json` 不需要同步**：其中十九条全是北向 Gateway 端点，模型目录条目不构成兼容性主张。
- **§7.4 的幂等键不是过度设计**：前端有现成先例（`DeploymentsPage.tsx:403` 的 `useRef(crypto.randomUUID())` + `api.ts:358` 的 `Idempotency-Key` 头），成本约五行。
- 安全姿态整体成立：拒绝“问模型你支持什么”、把 `/models` 存在性降级为非证据、临时故障绝不写成 `unsupported`、检测与 Deployment revision 测试分离且不预填 `LastTestRevision`、C 级持久副作用能力默认不探测、明确 CTA 而非 `onChange` 触发，以及 §17 那份“不接受的捷径”清单。

### 对基线文档的影响

已执行 §18 的显式契约修订：`docs/contracts/provider-capabilities.md` 与基线
§2.1、§4.1、§6.3、§7.1、§8.3、§9、§12、§15 统一为“内置目录或成功的
`verified_probe` 可预选；未知模型普通路径显式检测，高级路径手动声明”。

---

## 0. 结论

> 以下正文是解除 Deferred 后的实现契约；历史评审问题以上方修订结论和
> 实际代码/测试为准。

保留模型能力模型，但普通创建流程不再要求管理员替 Halro 判断模型支持哪些技术能力。

新的普通流程是：

```text
选择服务商
  → 选择或填写模型 ID
  → 明确点击“确认模型并识别能力”
  → Halro 先查可信目录与新鲜缓存
  → 未命中时执行受控、低成本、无持久副作用的真实协议检测
  → 自动勾选该单一能力接口上检测为 supported 的能力
  → 管理员只能收窄
  → 保存为停用 Deployment
  → 对已保存的精确 Deployment revision 执行既有测试
  → 显式启用
```

本方案刻意不做“向 LLM 发送一条自我介绍 Prompt，并相信它声称自己支持什么”。模型自述不是能力证据；一次 Chat 响应也无法证明 Embeddings、SSE、工具调用、视觉或媒体端点可用。

核心不变量：

```text
用户选择的是模型和用途，不是替系统背书能力事实。

自动勾选能力
  = 当前 Provider/Profile 上限
  ∩ 可信目录声明，或针对当前目标实际验证成功的能力
  ∩ 同一个 Binding 可以承载的能力

effective capabilities
  = capability snapshot
  ∩ operator-retained subset
```

- 仅 `builtin_catalog` 和成功的 `verified_probe` 可以自动勾选。
- `provider_metadata` 若未来出现，仍按基线文档约束：可展示但默认待确认，除非该 Adapter 对字段语义建立了独立、经过评审的强契约。
- 超时、限流、认证失败、权限不足、配额不足和响应不明确都不能写成“不支持”。
- 检测不发生在 Gateway 请求热路径，也不改变在线 Deployment。
- 一条 Deployment 仍只绑定一个内部 Binding；跨 Binding 的组合仍归 Route 层。
- 能力检测不能替代对已保存 Deployment revision 的既有测试，也不能预填 `LastTestRevision`。

## 1. 为什么需要这项改动

### 1.1 当前系统把信息缺口转嫁给管理员

服务商 `/models` 返回模型 ID，只能证明当前账户或区域看到了这个目标，不能证明它支持哪些操作。内置目录没有收录精确模型 ID 时，当前控制台展示全部可声明能力并要求管理员手动选择。

这在安全上是 fail-closed 的，但产品责任分配不合理：管理员通常知道“我要用这个模型”，却不应该记住 `stream_usage`、`developer_role`、工具调用、视觉输入和内部 Profile 的协议差异。

### 1.2 单纯扩大内置目录不能根治

补充精确目录条目仍然必要，但目录随二进制发布，天然落后于服务商的新模型、私有模型、Azure Deployment 和 OpenAI-compatible 自定义目标。人工声明必须保留为逃生通道，却不应是普通路径。

> **本节是本方案的承重论据，评审认为它尚未成立。** 目录今天只有约 24 条，Anthropic 与 Gemini 是零条目，也就是说“目录追不上”这一点从未被观测过——被观测到的只是“目录还没被填”。本节唯一经得起检验的部分是 Azure Deployment 与自定义端点：那里目录**天然**命不中，但它对应的是评审结论里的 S2（一个“背后是哪个已知模型”的下拉框），不是通用检测。

### 1.3 现有 `provider.Prober` 不是能力检测器

当前 `provider.Prober.Probe(ctx, model)` 用于连接或目标可达性验证。例如 OpenAI 非 Azure 路径通过模型目录的 GET 请求验证目标，不能证明 Chat、Streaming、Tools、JSON、Vision 或 Embeddings 语义。

因此本方案新增独立的 Adapter 能力检测契约；禁止通过修改 `Prober` 的含义，把连接测试和多次计费语义请求混在同一个方法里。

## 2. 目标与非目标

### 2.1 目标

1. 普通管理员选择模型后无需手工判断技术能力。
2. 只有模型被明确确认后才允许触发 Provider 请求，输入、搜索、高亮和鼠标经过不触发。
3. 已知模型零调用完成能力填充；未知模型使用受控检测。
4. 自动检测只承认实际协议行为，不承认模型自述。
5. 自动勾选所有在同一个 Binding 上验证成功且依赖关系完整的能力，管理员仍可关闭任意能力。
6. 检测具备幂等、single-flight、缓存、冷却、取消、超时和预算上限。
7. 检测失败分类准确；临时故障不得污染模型能力事实。
8. 检测结果由服务端持有并绑定目标指纹，客户端不能伪造 `verified_probe`。
9. 检测记录可审计、可过期，不记录凭据、原始 Provider 响应或任意模型输出。
10. 已保存 Deployment 继续使用不可变能力快照，目录或检测缓存刷新不静默改变它。

### 2.2 非目标

- 不根据模型名称、前缀、营销名称或 LLM 自我描述推断能力。
- 不在选择框每次输入、过滤或焦点变化时调用 Provider。
- 不自动探测会创建文件、批处理、异步任务或其他持久资源的能力。
- 不在一次检测中跨多个 Binding 合成一条 Deployment。
- 不因一次检测成功而跳过 Deployment 创建后的 revision 测试。
- 不把检测做成模型 Benchmark、质量评测或推荐系统。
- 不保证所有 Provider、所有能力都能被通用探测；无法可靠证明时保持 `inconclusive` 或 `not_probed`。
- 不在本方案中引入远程签名目录；它可作为后续独立方案，不能阻塞本方案。

## 3. 产品交互

### 3.1 模型确认边界

模型输入分为四个状态：

```text
editing → confirmed → detecting → detected
                     ↘ failed / canceled / expired
```

只有以下显式动作产生 `confirmed`：

- 从模型列表选择一项后，点击“确认模型并识别能力”；
- 键盘选中一项后，点击同一按钮；
- 手动输入完整模型 ID 后，点击同一按钮。

以下行为一律不发送能力检测请求：

- `onChange`；
- debounce 到期；
- 下拉项高亮变化；
- 鼠标 hover；
- 普通 blur；
- 模型目录刷新；
- 返回上一步后重新展示已有选择。

不采用“选择项一落下立即请求”的原因是列表点击仍可能只是比较候选。明确 CTA 把可能计费的操作做成一个可理解的提交边界，也让手输和下拉复用同一行为。

### 3.2 检测前提示

目录或新鲜缓存命中时：

```text
已从 Halro 能力目录识别该模型；不会调用服务商。
[确认模型]
```

需要真实检测时：

```text
该模型尚未收录。Halro 将执行最多 8 个低成本验证请求，
每次请求均限制输入、输出和超时；可能产生少量服务商费用。
不会创建文件、批处理或异步资源。

[确认模型并识别能力]  [高级手动声明]
```

若当前尚无版本化价格，不能显示虚假的美元估算，应显示“费用未知”，同时展示确定的请求数、最大输入、最大输出和总超时上限。

### 3.3 检测中

显示按能力推进的状态，不展示模型原始输出：

```text
正在识别能力 3 / 6

✓ 对话
✓ 流式
○ 工具调用
○ JSON 模式

[取消检测]
```

取消表示：停止尚未发出的探测并丢弃晚到结果。已经进入 Provider I/O 的请求可能仍然产生费用；UI 必须明确说明这一点。

### 3.4 检测完成

自动勾选 `supported` 能力，并展示证据：

```text
已识别 6 项能力 · 刚刚验证

✓ 对话          已验证
✓ 流式          已验证
✓ 工具调用      已验证
✓ JSON 模式     已验证
? 推理          无法可靠确认
— 图像生成      未执行付费检测
```

管理员可以关闭已勾选能力。普通流程不能勾选 `unsupported`、`inconclusive` 或 `not_probed`；需要超出检测结论时进入现有高级 `operator_declared` 流程，证据仍是 `declared`，不能伪装为 `verified`。

### 3.5 模型或目标变化

以下任一字段改变，当前检测结果立即从表单失效：

- Provider；
- 模型 ID；
- Binding 手动覆盖；
- Region；
- Target Kind；
- Azure Deployment 名称或 Bedrock ARN；
- 影响 Adapter 身份的 Provider revision。

旧请求可以继续在后台收尾，但其结果不得写入新选择的表单状态。前端以 `selection_revision` 比较，服务端以目标指纹比较，两层都必须存在。

## 4. 能力来源与处理顺序

后端固定按以下顺序解析：

```text
1. builtin_catalog 精确命中
2. 相同目标指纹的新鲜 verified_probe 缓存
3. Adapter 能力检测
4. 高级 operator_declared 兜底
```

### 4.1 内置目录命中

- 返回 `completed`，`provider_calls = 0`；
- 来源保持 `builtin_catalog`；
- 自动勾选目录能力；
- 不为了把 `declared` 提升为 `verified` 而默认增加费用；管理员可以显式选择“重新验证”。

### 4.2 新鲜验证缓存命中

- 目标指纹完全相同才复用；
- 返回原 `detection_id` 或派生只读引用；
- UI 显示验证时间与“重新验证”；
- 复用不产生 Provider 请求；
- 过期结果只供审计查看，不可用于新的 Deployment 快照。

### 4.3 Adapter 检测

- 只探测该 Binding/Profile 明确允许的能力；
- 每项能力使用 Adapter 自己的稳定 wire contract；
- App 层不得拼接 Provider URL、认证头或供应商私有 JSON；
- 只接受结构、状态机和显式 Provider 错误作为证据，不解析模型自然语言来决定能力。

### 4.4 人工声明

人工声明保留在“高级”中，用于私有模型、权限受限目标或当前无法可靠检测的能力。它不会被普通流程自动选择，也不会产生 `verified_probe`。

## 5. 检测风险分级与能力矩阵

### 5.1 A 级：零 Provider 调用

| 来源 | 自动应用 | 证据 |
| --- | --- | --- |
| 内置目录精确条目 | 是 | `declared` / `builtin_catalog` |
| 新鲜的同指纹验证缓存 | 是 | 复用原 `verified_probe` |
| `/models` 中存在 | 否 | 只证明 ID 存在 |
| 模型自然语言自述 | 否 | 不作为能力证据 |

### 5.2 B 级：默认允许的低成本、无持久副作用探测

| 能力 | 探测方法 | 成功判据 | 备注 |
| --- | --- | --- | --- |
| `chat` | 最小非流式生成 | wire 响应完整、含可接受文本/内容结构 | 输出严格限长 |
| `streaming` | 最小流式生成 | 语义事件顺序完整且正常结束 | 不能只看 HTTP 200 |
| `stream_usage` | 流式探测附带 usage 请求 | 正常结束时出现合法 usage | 依赖 `streaming` |
| `tools` | 声明一个无副作用虚拟工具 | 返回合法 tool call | 工具永不执行 |
| `json_mode` | 要求固定小 JSON 结构 | 解析及约束校验成功 | 自然语言“我支持”无效 |
| `developer_role` | 最小 developer-role 请求 | 请求被接受且响应完整 | 依赖 `chat` |
| `vision` | 使用仓库内置的极小、无敏感信息图片 | 多模态请求被接受并返回完整响应 | 不要求模型回答特定自然语言词 |
| `embeddings` | 单个固定短文本 | 返回有限、非空、数值合法的向量 | 使用 Embeddings primitive |
| `moderations` | 固定无害文本 | 返回合法审核结果结构 | 只在 Profile 支持时 |
| `rerank` | 两段固定无害文本 | 返回合法、有界排序结果 | 只在专用 Profile 支持时 |

默认检测预算：最多 8 次 Provider 调用；具体 Adapter 可以用一次请求同时证明依赖能力，但每个结论必须有独立的判据。

### 5.3 C 级：不能自动宣称 verified

| 能力 | 默认结果 | 原因 |
| --- | --- | --- |
| `reasoning` | `inconclusive`，除非协议返回经过评审的明确 reasoning 结构 | 普通回答无法证明使用了推理模式 |
| `files` | `not_probed` | 账户级持久资源，不是模型 ID 自身能力 |
| `batches` | `not_probed` | 会创建持久任务，且常为账户级资源 |
| `images` | `not_probed` | 费用较高并生成媒体 |
| `transcriptions` | `not_probed` | 需要上传音频并可能计费 |
| `speech` | `not_probed` | 会生成二进制媒体并计费 |
| `async_generate` | `not_probed` | 会创建异步任务或外部资源 |

这些能力优先依赖内置目录。未来若提供显式“付费扩展检测”，必须单独设计成本确认、资源清理和 UNKNOWN 结果处理，不能暗中并入普通按钮。

### 5.4 依赖关系

- `streaming`、`tools`、`vision`、`json_mode`、`developer_role`、`reasoning` 依赖 `chat`；
- `stream_usage` 依赖 `streaming`；
- 父能力未成功时，子能力不得为 `supported`；
- 某次组合探测失败时，不得把其中所有能力都写成 `unsupported`，应退化为独立探测或 `inconclusive`。

## 6. 结果状态与失败分类

每项能力使用以下状态：

| 状态 | 含义 | 可自动勾选 |
| --- | --- | --- |
| `supported` | 当前目标通过明确协议验证 | 是 |
| `unsupported` | Adapter 识别出明确且稳定的“不支持该模型/参数”响应 | 否 |
| `inconclusive` | 请求完成但无法证明能力，或错误语义不够明确 | 否 |
| `unavailable` | 超时、连接、5xx、限流或暂时不可用 | 否 |
| `unauthorized` | 凭据、权限或 opt-in 不足 | 否 |
| `not_probed` | 风险级别、预算或 Profile 决定不发请求 | 否 |
| `canceled` | 尚未完成即被取消 | 否 |

只有 Adapter 可以把 Provider 错误映射为 `unsupported`。App 层看到普通 400、404 或错误字符串时不得自行猜测。

对已经可能到达 Provider 的 `UNKNOWN` 请求：

- 不自动重试；
- 记录 `inconclusive` 或 `unavailable`；
- 由管理员显式重新验证；
- 同一个幂等键不得重新执行已经可能发生的调用。

任务级状态机冻结为：`queued → running → completed|failed|canceled`；进程启动
把遗留的 `queued|running` 直接改为终态 `interrupted`。`completed` 的过期由
`expires_at` 即时派生，不新增可写状态；所有终态都不可重新进入运行态，重试
必须使用新的明确确认与幂等键。

## 7. 目标指纹、缓存与并发控制

### 7.1 目标指纹

检测指纹是以下规范化字段的 SHA-256：

```text
provider_id
+ provider_revision
+ credential_revision
+ credential_key_version
+ binding_id
+ profile_id
+ access_surface
+ provider_model
+ target_kind
+ canonical_target
+ region
+ detector_contract_version
+ requested_risk_tier
```

任一字段变化均不得复用旧结果。

### 7.2 缓存生命周期

建议默认值：

- `completed` 结果用于新建 Deployment 的新鲜期：24 小时；
- 检测记录审计保留期：30 天；
- 同指纹强制重新检测冷却：5 分钟；
- 目录结果仍按现有目录规则，不受检测 TTL 影响。

这些值进入经过校验的 `admin.model_capability_detection` 配置，不允许通过 Admin 请求任意放大，并已同步默认模板、示例配置、有效配置摘要与操作指南。

### 7.3 Single-flight

- 同一目标指纹同时只有一个 `queued/running` 检测；
- 第二个请求返回同一 `detection_id`，不创建新 Provider 调用；
- 全局默认最多 4 个运行任务；
- 同一 Provider 默认最多 1 个运行任务；
- 同一管理员不能通过多个标签页绕过 Provider 限制。
- 每位管理员默认每分钟最多启动 6 个新检测；目录、缓存和幂等复用不计入，换模型 ID 也不能绕过该上限。

### 7.4 幂等

创建检测必须携带 `Idempotency-Key`：

- 同 key、同规范化请求：返回原任务；
- 同 key、不同请求：`409 idempotency_conflict`；
- 服务端只保存 key 的摘要；
- 前端一次明确确认生成一个 key，模型或目标变化生成新 key；
- 网络结果不明确时只允许用原 key 查询/重放同一语义，不得生成新 key自动重试。

## 8. Admin API 契约

### 8.1 创建或复用检测

```http
POST /admin/api/v1/providers/{provider_id}/model-capability-detections
Idempotency-Key: <opaque>
```

```json
{
  "provider_model": "gpt-5.4-mini",
  "target_kind": "model_id",
  "region": "",
  "binding_id": "",
  "risk_tier": "safe_automatic",
  "selection_revision": "client-opaque-revision"
}
```

响应：

- 目录或缓存命中：`200 completed`；
- 新任务：`202 queued|running`；
- 同指纹已有任务：`200/202` 返回该任务；
- Provider 已变化：`409 provider_revision_changed`；
- 冷却中强制刷新：`429 capability_detection_cooldown`；
- 当前没有可检测 Binding：`400 no_detectable_binding`。

### 8.2 查询

```http
GET /admin/api/v1/model-capability-detections/{detection_id}
```

返回示例：

```json
{
  "id": "mcd_...",
  "status": "completed",
  "provider_id": "prv_...",
  "provider_model": "gpt-5.4-mini",
  "binding_id": "prv_...:openai.chat-embeddings.v1",
  "profile_id": "openai.chat-embeddings.v1",
  "source": "verified_probe",
  "provider_calls": 6,
  "max_provider_calls": 8,
  "started_at": "...",
  "completed_at": "...",
  "expires_at": "...",
  "capabilities": {
    "chat": {"status": "supported", "evidence": "verified"},
    "streaming": {"status": "supported", "evidence": "verified"},
    "tools": {"status": "supported", "evidence": "verified"},
    "reasoning": {"status": "inconclusive"},
    "images": {"status": "not_probed"}
  },
  "recommended_capabilities": {
    "chat": true,
    "streaming": true,
    "tools": true
  },
  "selection_revision": "client-opaque-revision",
  "revision": 3
}
```

禁止返回：

- Provider 凭据或认证头；
- 原始请求体；
- 原始 Provider 响应；
- 模型生成文本；
- Provider 错误正文；
- 内部目标指纹、Credential revision/key version、幂等摘要、调用状态记录与创建人；
- 能被模型 ID、Deployment ID 等高基数值污染的指标标签。

### 8.3 取消

```http
DELETE /admin/api/v1/model-capability-detections/{detection_id}
If-Match: "<revision>"
```

- 设置 `cancel_requested_at` 并推进 revision；
- 停止尚未发送的能力探测；
- 取消可取消的上下文；
- 丢弃晚到的能力结论；
- 已可能执行的 Provider 调用不重试、不回滚，也不声称没有费用。

### 8.4 Deployment 创建引用检测结果

普通未知模型创建请求不再发送 `mode=operator_declared`，而是发送：

```json
{
  "provider_id": "prv_...",
  "provider_model": "gpt-5.4-mini",
  "capability_detection_id": "mcd_...",
  "capability_detection_revision": 3,
  "capabilities": {
    "chat": true,
    "streaming": true,
    "tools": false
  }
}
```

后端必须重新校验：

- 检测属于同一 Provider；
- 指纹与当前 Provider/Binding/模型/区域完全一致；
- 状态为 `completed` 且未过期；
- revision 未变化；
- retained 是 `recommended_capabilities` 的子集；
- 所有 retained 能力来自同一个 Binding；
- 当前 Profile 上限仍覆盖 retained。

不满足时 fail-closed：

- `409 capability_detection_stale`；
- `409 capability_detection_changed`；
- `400 capabilities_exceed_detection`；
- `400 capability_detection_target_mismatch`。

高级人工声明继续使用具名 `mode=operator_declared`，但从普通 UI 隐藏。

## 9. Domain 与存储模型

新增独立实体，不在尚未创建的 Deployment 上暂存检测状态：

```go
type ModelCapabilityDetection struct {
    ID                  string
    ProviderID          string
    ProviderRevision    uint64
    CredentialRevision  uint64
    CredentialKeyVersion uint16
    ProviderModel       string
    ModelRevision       string
    BindingID           string
    ProfileID           ProviderProfileID
    AccessSurface       AccessSurface
    TargetKind          DeploymentTargetKind
    CanonicalTarget     string
    Region              string
    TargetFingerprint   string
    DetectorVersion     string
    RiskTier            string
    Status              string
    Results             map[string]CapabilityProbeResult
    Recommended         ProviderCapabilities
    ProviderCalls       int
    MaxProviderCalls    int
    Calls               []DetectionProviderCall
    StartedAt           *time.Time
    CompletedAt         *time.Time
    ExpiresAt           *time.Time
    CancelRequestedAt   *time.Time
    CreatedBy           string
    IdempotencyKeyHash  [32]byte
    Revision            uint64
    CreatedAt           time.Time
    UpdatedAt           time.Time
}
```

```go
type CapabilityProbeResult struct {
    Status      string
    Evidence    CapabilityEvidence
    ErrorClass  string
    BindingID   string
    ProbeKind   string
    StartedAt   *time.Time
    CompletedAt *time.Time
}
```

约束：

- `Recommended` 必须恰好等于同一 Binding 上所有 `supported` 布尔能力经过依赖校验后的集合；
- 数值限制不从通用探测推断，除非 Provider 协议返回有强契约的结构化限制；
- `supported` 必须带 `EvidenceVerified`；其余状态不能带 verified；
- 检测记录不能包含任意原始 Provider 内容；
- 每次可能计费的调用在 Provider I/O 前以 `reserved → running` 持久化；进程恢复把两种未决状态改为 `unknown` 并中断任务，不自动重放；
- `completed` 至少要有一个核心操作 `supported`，否则整体状态为 `failed`，但保留逐项结果；
- 过期是读取时派生还是后台清理可以分开：可用性按 `ExpiresAt` 即时判断，物理删除按保留策略执行。

### 9.1 Schema

建议新增 schema 24：

```text
model_capability_detections
model_capability_detection_idempotency
model_capability_detection_fingerprint_index
```

这是可前向创建空 bucket 的加法迁移，不需要重建既有 Deployment，也不改变现有能力快照。迁移必须验证：

- schema 23 数据可前向打开；
- 新 bucket 缺失时不能在 schema 24 下静默运行；
- Backup/Restore 保留未过期检测与审计需要的历史记录；
- 恢复后的 `running` 检测转为 `interrupted/inconclusive`，绝不自动重放 Provider 请求。

## 10. Adapter 契约

新增可选接口，不能扩展现有 `Prober`：

```go
type CapabilityDetector interface {
    CapabilityDetectionPlan(ModelCapabilityDetectionTarget) (CapabilityDetectionPlan, error)
    DetectCapability(context.Context, ModelCapabilityDetectionTarget, CapabilityProbe) CapabilityProbeResult
}
```

`CapabilityDetectionPlan` 必须先于任何 Provider I/O 固定：

- 候选能力；
- probe kind；
- 依赖关系；
- 最大调用次数；
- 每次最大输入/输出；
- 是否可能计费；
- 是否会产生持久副作用；
- 是否允许进入 `safe_automatic`。

App 层验证计划总预算，并逐项调度。Adapter 负责：

- 固定路径与请求 schema；
- 凭据与 SSRF 边界；
- Provider 错误分类；
- 响应大小限制；
- 语义成功判据；
- 取消传播；
- 敏感内容清理。

初始实现优先级：

1. OpenAI 与 Azure OpenAI；
2. DeepSeek 与经过显式审查的 OpenAI-compatible；
3. Anthropic、Gemini；
4. Bedrock 各 Profile 单独实现，禁止用一个通用 Bedrock detector 横跨 Converse、Invoke、Agent Runtime 和 Mantle。

某 Adapter 未实现 `CapabilityDetector` 时，UI 显示“当前接口不支持自动识别”，进入目录或高级声明路径，不能回退到模型自述。

## 11. 调度、超时与预算

默认安全上限建议：

| 项目 | 上限 |
| --- | --- |
| 单次检测 Provider 调用 | 8 |
| 单次探测输出 | 128 tokens 或 Provider 等价上限 |
| 单项响应体 | 现有 Provider 安全上限以内，并可进一步收窄 |
| 单项响应头超时 | 复用并不超过 `AttemptResponseHeaderTimeout` |
| 单次检测总时长 | 90 秒 |
| 全局并发检测 | 4 |
| 单 Provider 并发检测 | 1 |

检测不计入用户 Gateway Usage 与项目预算，因为它由管理员在控制面触发；但必须单独记录 Provider usage（若协议返回）、调用次数和审计事件。未来若加入检测成本核算，应使用独立 control-plane cost 类别，不能伪装成某个 Project 的业务流量。

没有版本化价格时，UI 只能承诺请求和 token 上限，不能承诺金额。检测开始前的确认不可省略。

## 12. 安全与正确性

### 12.1 SSRF 与凭据

- 只能通过当前已绑定、已启用的 Provider Binding 与 Adapter 发起；
- Admin 请求不得提交 URL、Host、Header 或凭据；
- 目标地址沿用 Provider 规范化、allowlist、DNS rebinding 和凭据 audience 约束；
- 检测结果与错误中不回显凭据或上游正文。

### 12.2 Prompt 与模型输出

- 探测输入全部由 Halro 内置，不能拼接管理员自由文本；
- 工具探测只声明虚拟工具，绝不执行工具；
- 模型输出视为不可信输入，只用于受限解析器验证 wire 结构；
- 不把模型自然语言写入日志、审计、检测记录或前端；
- 不允许模型通过输出要求发起后续 URL、工具或资源请求。

### 12.3 Fail-closed

- 未完成或过期检测不能创建普通未知模型 Deployment；
- 检测结果与当前目标不一致时必须重新检测或高级声明；
- 临时故障不变成 unsupported；
- `supported` 之外的能力不自动进入快照；
- Profile 收窄后即使检测仍新鲜，创建时也要用当前 Profile 再校验；
- 目录、缓存和检测都不可静默扩张已存在 Deployment。

### 12.4 检测与 Deployment 测试的边界

能力检测回答：

> 在某时刻，某 Provider/Binding/目标是否实际接受并正确完成某项协议语义？

Deployment 测试回答：

> 已持久化的这个精确 Deployment revision 当前是否通过其启用门禁？

前者发生在创建前，后者绑定 `Deployment.Revision`。因此检测成功后仍然：

1. 创建停用 Deployment；
2. 保存来自检测的不可变能力快照；
3. 对已保存 revision 执行既有 `/deployments/{id}/test`；
4. 测试成功后显式启用。

前端可以把“保存 → 测试”串成一个可见流程，但后端不能预填 `LastTestRevision`。

## 13. 审计与可观测性

### 13.1 审计事件

- `model_capability_detection.started`；
- `model_capability_detection.cache_reused`；
- `model_capability_detection.completed`；
- `model_capability_detection.failed`；
- `model_capability_detection.cancel_requested`；
- `model_capability_detection.expired`；
- Deployment 创建继续使用既有 `deployment.capability_snapshot.created`，以 `source=verified_probe` 区分，避免重复职责的审计动作。

检测 ID 由审计资源 ID 承载。metadata 只记录 Provider ID、Binding ID、模型 ID 的安全摘要、状态计数、调用次数、风险级别、revision 与错误分类。内部目标指纹、原始请求/响应、模型输出、Provider 错误正文和凭据均禁止记录。

### 13.2 指标

建议新增低基数指标：

- `halro_model_capability_detection_total{provider_type,status,source}` Counter；
- `halro_model_capability_probe_total{provider_type,capability,status}` Counter；
- `halro_model_capability_detection_inflight{provider_type}` Gauge；
- `halro_model_capability_detection_cache_total{status}` Counter；
- `halro_model_capability_detection_provider_calls_total{provider_type}` Counter；
- `halro_model_capability_detection_duration_seconds{provider_type,status,source}` Histogram。

禁止使用模型 ID、Provider ID、Binding ID、Detection ID 或管理员 ID 作为 label。新增指标必须同步 `docs/contracts/metrics-reference.md`。

## 14. 测试方案

### 14.1 Domain

- 只有 `supported` 产生 verified evidence；
- Recommended 只包含 supported 且依赖完整的能力；
- 子能力不能脱离父能力；
- 单次结果不能跨 Binding 合并；
- 指纹确定性与字段敏感性；
- 过期、取消、revision 和状态机转换；
- 原始 Provider 内容无法进入持久化结构。

### 14.2 Adapter

每个实现 detector 的 Adapter 必须有 fake-server wire 测试：

- Chat、Streaming、Tools、JSON、Vision、Embeddings 等成功路径；
- 工具永不执行；
- SSE 截断、缺终止事件和非法 usage 不算 supported；
- 400/404 只有匹配经过评审的 Provider 错误契约才是 unsupported；
- 401/403、429、5xx、超时和连接失败分类准确；
- 响应体上限、恶意 JSON、未知字段和错误正文不泄露；
- 取消能停止后续探测；
- UNKNOWN 不自动重试。

### 14.3 App/API

- 输入、搜索、高亮、hover、blur 不触发检测；
- 只有显式 CTA 触发一次；
- 同幂等键同请求返回原任务，不同请求 409；
- 同指纹并发请求 single-flight；
- 缓存命中零 Provider 调用；
- Provider revision、凭据 generation、Binding、模型、区域变化使结果失效；
- 取消后晚到结果不落为 supported；
- 重启把 running 转为 interrupted，不自动重放；
- 只读管理员可查看但不能创建/取消检测；
- 普通创建只能使用当前、未过期、同目标的检测；
- 客户端伪造 verified capabilities 被拒绝；
- 检测成功不设置 Deployment `LastTestRevision`；
- Backup/Restore 保留所需记录并保持 fail-closed。

### 14.4 Frontend

- 下拉选择后仍需显式确认；
- 手输与下拉共用同一确认函数和 query/mutation key；
- 双击、重复点击和多标签页不增加 Provider 调用；
- 切换 Provider/模型时旧结果不能覆盖新表单；
- 检测进度、取消、费用未知、缓存时间和失败分类可见；
- 完成后只自动勾选 recommended；
- 管理员可关闭但不能普通开启未验证能力；
- 目录命中不显示计费检测确认；
- detector 不可用时进入明确的高级声明路径；
- 键盘、焦点、窄屏和长模型 ID 可用。

### 14.5 真实 Provider 门禁

在 `docs/verification/provider-real-matrix.md` 的精确 RC commit 上增加 opt-in 能力检测证据：

- 每个 GA Provider 至少一个已知支持模型；
- 支持能力被检测为 supported；
- 至少一个明确不支持的能力被正确分类，或记录为何没有稳定负例；
- 临时错误不得被写成 unsupported；
- 调用次数不超过计划；
- 证据文件不含凭据、Prompt、模型输出或 Provider 错误正文。

真实检测产生费用，继续保持 opt-in，不得进入普通 CI。

## 15. 实施切片

### Phase 0：契约冻结

- 冻结本文 API、状态机、指纹、风险分级和失败分类；
- 为 OpenAI/Azure 写出逐能力 wire probe 表；
- 决定检测配置路径、TTL 和保留期；
- 更新基线文档中“未知模型普通流程”：普通路径改为 detection，高级路径才是 operator declaration；
- 明确哪些模型级能力排除 files/batches。

门禁：没有任何能力依赖自然语言自述；每个自动探测都有稳定成功判据、最大调用数和副作用结论。

### Phase 1：Domain、schema 24 与任务 API

- 增加 Detection 实体、状态机、索引和迁移；
- 实现幂等、single-flight、TTL、冷却、取消和重启恢复；
- 增加 POST/GET/DELETE API；
- 先用 fake detector 完成端到端测试；
- 加入审计与低基数指标契约。

门禁：尚无真实 Adapter 时也能证明重复请求不执行两次、取消不接受晚到结果、重启不重放。

### Phase 2：OpenAI/Azure 与普通创建闭环

- 实现首批 safe automatic probes；
- 控制台增加明确确认、进度和结果应用；
- Deployment 创建引用检测 ID/revision；
- 未知模型普通路径不再发送 operator declaration；
- 保存后继续要求精确 revision 测试。

门禁：一个未收录 OpenAI/Azure 模型可在不手勾技术能力的情况下完成“识别 → 自动选择 → 保存停用 → 测试”。

### Phase 3：其他 Provider

- DeepSeek、审查过的 OpenAI-compatible；
- Anthropic、Gemini；
- Bedrock 按 Profile 独立实现；
- 未实现 detector 的 Provider 保持目录/高级声明，不做通用猜测。

门禁：Provider 之间不共享未经证明的错误字符串匹配或 wire 假设。

### Phase 4：生产证据与文档收口

- 真实 Provider opt-in 矩阵；
- 浏览器验收；
- 操作指南、费用说明、Backup/Restore 和指标文档；
- 评估是否需要后续“签名远程能力目录”方案。

门禁：本文 §16 全部满足后才可宣告完成。

## 16. 发布门禁

- [x] 模型输入、搜索、hover、blur 不产生 Provider 能力请求。
- [x] 只有明确确认 CTA 触发检测，并展示最大调用数与费用边界。
- [x] 内置目录或新鲜缓存命中时 Provider 调用为零。
- [x] 同幂等键、同指纹并发和多标签页均不会重复执行。
- [x] 检测只通过 Adapter 固定协议，不信任模型自述。
- [x] 低风险能力有逐项成功判据、依赖校验和响应上限。
- [x] 高成本或持久副作用能力默认不探测。
- [x] 临时故障、认证、权限、配额、限流和 UNKNOWN 不会被误记为 unsupported。
- [x] 只有 supported 能力被自动勾选；管理员只能普通收窄。
- [x] 检测结果绑定完整目标指纹、revision、TTL 和单一 Binding。
- [x] 客户端无法伪造 verified evidence 或把旧检测应用到新目标。
- [x] 取消丢弃晚到结论，重启不自动重放可能计费请求。
- [x] 检测成功不会预填 Deployment `LastTestRevision`。
- [x] 已创建 Deployment 的能力仍是不可变快照，刷新检测不改变在线流量。
- [x] Audit、Backup/Restore、指标与低基数 label 契约完整。
- [x] 完整 Go、Race、Vet、前端、生产构建和浏览器验收通过。
- [-] 精确 RC commit 的真实 Provider opt-in 能力检测证据完成。运行器与无敏感信息证据格式已实现；该项需要发布候选 commit、计费授权和真实 Provider 凭据，因此保留为外部发布门禁，不计为仓库实现欠项。

## 17. 明确不接受的捷径

- 选择框 `onChange` 或 debounce 自动调用 LLM；
- 询问模型“你支持什么”并据此全选；
- 用 `/models` 存在性证明能力；
- 用模型名称前缀复制相似模型能力；
- 把 Provider Profile 上限全部复制给未知模型；
- 把普通 400/404 一律当成 unsupported；
- 取消后接受晚到成功结果；
- 失败后自动换幂等键重试；
- 在一个 Deployment 中合并多个 Binding 的检测结果；
- 用创建前检测替代创建后的 Deployment revision 测试；
- 为降低实现成本记录原始 Prompt、模型输出或 Provider 错误正文。

## 18. 与现有方案的关系

本方案不推翻基线能力模型，而是调整未知模型的普通交互和证据取得方式：

- 已知模型：继续由内置目录自动预选；
- 未知模型普通路径：由“管理员声明能力”改为“明确确认后自动检测并预选”；
- 未知模型高级路径：保留 `operator_declared`；
- 能力快照、漂移、Route 预检、Profile 收窄、审计与运行时 fail-closed 继续有效；
- 一个 Deployment 一个 Binding、跨能力组合归 Route 层的结论不变；
- 现有 `provider.Prober` 与 Deployment revision 测试不改变语义。

实施前必须同步修订基线文档 §6.3、§7.1、§8.3、§9、§12 与 §15，避免同时存在“未知模型普通路径要求手工声明”和“未知模型普通路径自动检测”两套互相冲突的产品契约。

> **本方案 Deferred 期间，本节的修订一律不执行。** 基线文档与 `docs/contracts/provider-capabilities.md` 保持现状。
>
> 另外，本节这份清单本身不完整——评审发现它漏了基线 §2.1(4)、§4.1、§12.5、§14、§17.2 A2、§17.4，以及 `docs/contracts/provider-capabilities.md:63` 那条被代码固化的“Only the builtin catalog pre-selects”。恢复本方案时须先补全清单，并注意基线 §15 有两处硬编码的门禁计数需要一起数。
