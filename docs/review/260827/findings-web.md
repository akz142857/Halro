# 阶段 2 · Admin 控制台一致性评审（findings-web）

- 范围：`git diff v0.3.0(abfc05c)..HEAD(8bb4847) -- web/src`，43 文件 +4028/-831。
- 方法：按方案 §6 的五个靶子逐项对照代码与 v0.3.0 原文件（`git show v0.3.0:web/src/pages/DeploymentsPage.tsx`），后端仅为核对契约两侧读了 `internal/store/bolt/store.go`（迁移 32）与 `internal/app/capability_drift.go`（复核状态计算）。
- 可运行验证：`npx vitest run src/pages/deploymentCondition.test.ts src/i18n/i18n.test.tsx src/design-system.test.ts`（41 通过）与 `npx vitest run src/pages/DeploymentsPage.test.tsx`（84 通过），均于本机实际运行。
- 分级依照方案 §3 基准 B1–B10；阻塞类按 `docs/verification/release-assessment.md` §5。

## 发现

### W1 【肯定】词表拆分在控制台是两件独立能力，且由服务端词表驱动

- `web/src/types.ts:362-374`、`web/src/pages/DeploymentsPage.tsx:1209`、`web/src/pages/ProvidersPage.tsx:848`
- 基准：B9。是否阻塞发布：否。
- `json_object` 与 `structured_outputs` 是两个独立勾选框（连接表单与部署表单均如此），两份文案各自命名（zh-CN.ts:729 "JSON 对象模式"/"结构化输出"；en-US.ts:723 "JSON object"/"Structured outputs"），探测结果按能力逐项渲染（`DeploymentsPage.tsx:2372-2390` 每个能力有独立的不可用原因与探针结局）。不是一个开关换名。
- 防漂移机制齐备：能力键来自 `GET /provider-profiles`（`useProviderProfiles.ts:18-25`，无本地回退）；前端 fixture 由 Go 侧金样测试锁定（`internal/app/admin_provider_profiles_golden_test.go` ↔ `web/src/test/provider-profiles.golden.json:105-127`）；`i18n.test.tsx:18,29` 强制中英键位逐一对齐、且"服务端提供的每个能力两种语言都必须有文案、不许有多余文案"；`DeploymentsPage.tsx:1215` 的分组表也被测试对照端点真实输出。

### W2 【肯定】`provider_executed_tools` 默认关闭、显式开启、后果就地说明

- `web/src/pages/ProvidersPage.tsx:631-632,651-653,848,854-861`
- 基准：B4（A3 的前端对侧）。是否阻塞发布：否。
- 新连接的能力初值取服务端 `connection_defaults`，金样中 `openai.responses.v1` 与 `anthropic.messages.2023-06-01` 均为 default:false / ceiling:true——默认不勾，开启是显式动作。
- 勾选框旁常驻标签（zh `providers.capabilityEgressTag` "会放行上游自行联网"，zh-CN.ts:1016；en-US.ts:1011），勾上后另出警示横幅，两种语言均写明三件事：流量不经过 Halro（即不经 SafeTransport 的出站管控）、不受出站主机限制、不出现在审计记录（zh-CN.ts:1018；en-US.ts:1013）。哪些能力需要警示由服务端 `capability_opt_in_warnings` 决定（`useProviderProfiles.ts:208-213`），不是前端硬编码。

### W3 【肯定】卡片化后不可用/被扣留的部署更醒目，且排序有依据

- `web/src/pages/deploymentCondition.ts:67-118`、`web/src/styles.css:507-513`
- 基准：B2（可行动性一面）。是否阻塞发布：否。
- 每张卡固定渲染一行"当前最坏状态"（drifted > 探测不健康 > 价格隔离 > 缺价格 > …），blocked 级为红点加红字，被压制的其余状态计数显示（`+N`）而不是丢弃（`DeploymentsPage.tsx:411-425`）。梯子是纯函数，`deploymentCondition.test.ts`（188 行）覆盖排序；"需要关注"过滤器读同一梯子（`deploymentCondition.ts:150-161`），修掉了 v0.3.0 里过滤器与行内标记各说各话的问题。相比 v0.3.0（drifted 仅在状态列一个小字标记），这是可见性的净提升。

### W4 【肯定】卡片化后信息基本不丢，两处净增

- `web/src/pages/DeploymentsPage.tsx:474-626` 对照 `git show v0.3.0:web/src/pages/DeploymentsPage.tsx`（330-400 行区间）
- 基准：无（对照 §6 第三条）。是否阻塞发布：否。
- v0.3.0 行/展开区的字段逐一核对：名称、服务商、上游目标、并发、路由依赖、价格四费率与调度、区域、上下文/输出上限、状态、复核通知、隔离警示、价格时间线、能力列表、访问面——全部仍在卡片或抽屉里。测试 `DeploymentsPage.test.tsx:1402`（"keeps every fact the readiness strip held"）守住这一点。
- 两处净增：价格时间线从"只显示未生效版本"改为含现行与被替代版本（`DeploymentsPage.tsx:515-540` 注释明说旧缺陷）；证据从一条汇总改为逐能力标注（`DeploymentsPage.tsx:557-568`）。例外见 W8。

### W5 【肯定】409 提示重载，未见静默覆盖

- `web/src/i18n/errors.ts:122`、`web/src/i18n/locales/zh-CN.ts:74`
- 基准：无（§6 常设项）。是否阻塞发布：否。
- 409/412/428 统一落为"数据已被其他操作修改，请刷新后重试"；范围内改动的所有写路径均带存储的 revision 提交（如 `DeploymentsPage.tsx:262-272`），失败经通知渠道呈现（`DeploymentsPage.tsx:361-366`），没有任何"重取 revision 自动重发"的模式。部署表单对解析结果的 409 要求显式确认后才能继续（测试 `DeploymentsPage.test.tsx:309`）。`errors.ts:101-104` 还专门把"刷新救不了"的 409（`capability_expansion_requires_revalidation`）从通用文案里摘出来单独说明。

### W6 【肯定】secret 驻留与生产 bundle 扫描仍然成立

- `web/scripts/check-artifacts.mjs`、`web/package.json:8`
- 基准：B3。是否阻塞发布：否。
- canary 扫描仍在 `npm run build` 链尾对 `internal/webui/dist` 全量执行：禁 `localStorage`/`sessionStorage`/`indexedDB` 字符串、禁 sourcemap、禁 `gw_plaintext-canary`/`csrf-canary`/`password-canary` 等一次性明文标记。`web/src` 中除 `theme.ts` 的"永不写入"注释外无任何浏览器存储调用。本轮 diff 里大量输入框补了 `autoComplete="off"`（AdminUsersSection、DeveloperPage、MFASettings、Providers/Projects/Policies/Routes/Usage 等），进一步减少浏览器代管的敏感输入残留。

### W7 【肯定】探测/拒绝渲染只透标识符，不透上游原文

- `web/src/pages/DeploymentsPage.tsx:2279-2284,2309-2315`
- 基准：B3。是否阻塞发布：否。
- 上游拒绝某能力时，界面只渲染 HTTP 状态与错误码标识（注释原话："the upstream's own sentence never reaches this cell"），配 zh-CN.ts:843 的说明文案；测试 `DeploymentsPage.test.tsx:427` 覆盖。六种探针结局各有独立文案与下一步指引（`DeploymentsPage.tsx:1655-1688`），"预算未轮到"（`detectionProbeBudget`）单独成句——H3 若成立，运维在界面上是能看到"没轮到"而非"不支持"的。

### W8 【建议】抽屉删掉了 Deployment ID / profile_id / binding_id，代码注释与现状矛盾

- `web/src/pages/DeploymentsPage.tsx:605-623` 对照 v0.3.0 的 technical-details 区（`deploymentID`/`bindingID`/`profile` 三个 i18n 键已随之删除）
- 基准：无（对照 §6 第三条，"改前字段改后仍能找到"的例外）。是否阻塞发布：否。
- 正向关联仍通：UsagePage 以 `?q=<deployment_id>` 跳转（`UsagePage.tsx:127`），列表过滤器匹配 `deployment.id`（`DeploymentsPage.tsx:62`）。但反向没了：运维无法再从控制台抄下某个部署的 ID 去检索服务端日志/审计。605 行注释仍写着标识符是"运维对着日志行打开抽屉要找的那个东西"，而该区块实际已不含任何 ID——注释描述的用途与渲染内容矛盾。建议在 Connection 区补回 Deployment ID（profile/binding 可议）。

### W9 【建议】部署级能力编辑器没有 `provider_executed_tools` 的出网提示

- `web/src/pages/DeploymentsPage.tsx:2371-2390`、`web/src/pages/DeploymentsPage.tsx:2581-2589`（CapabilitySubsetEditor）
- 基准：B4（A3 前端对侧）的完整性。是否阻塞发布：否。
- `capabilityNeedsOptInWarning` 只在 ProvidersPage 使用。连接开启后，部署表单里 `provider_executed_tools` 是一个与"流式"同权重的普通勾选框，旁边没有 egress 标签或警示。知情开启的闸门在连接层成立（W2），但逐部署开启的人未必是当初开连接的人。建议部署编辑器同样消费 `capability_opt_in_warnings` 渲染标签。

### W10 【建议】两个 JSON 能力的勾选框缺一句区分文案

- `web/src/i18n/locales/zh-CN.ts:729`、`web/src/i18n/locales/en-US.ts:723`
- 基准：无（疑问性质的建议）。是否阻塞发布：否。
- "JSON 对象模式"与"结构化输出"对开发者语境足够，但对运维，"哪个是只保证能解析、哪个是上游强制 schema"只写在 `types.ts:362-368` 的代码注释里，界面上没有任何 hint。拆分的动机（B9：一个开关不能承诺两件事）值得用一句话呈现给做勾选决定的人。

### W11 【问题】迁移 32"两半皆关"的存量部署，控制台看不出被关了、更看不出为什么

- 前端：`web/src/pages/DeploymentsPage.tsx:278-280,557-568`（能力列表只渲染 enabled=true 的项）、`web/src/pages/deploymentCondition.ts:114-116,150-161`（review_available 为最低档 quiet，且不入"需要关注"过滤器）；后端对侧：`internal/store/bolt/store.go:906-955`（两半置 false、证据置 unsupported）、`internal/app/capability_drift.go:210-218`
- 基准：B2 的可行动性一面（方案 §6 第一条、假设 H1 的前端半边）。是否阻塞发布：**本体不阻塞**（fail-closed 方向正确，不属 §5 七类），但它把 H1 的"发布说明必须写明"从建议升为必要——CHANGELOG/发布说明若不写，升级后的能力回退对运维完全不可见（方案 §9 第一条已要求）。
- 实际可见面分三种情形：
  1. **目录覆盖的模型**：读取时 `capability_drift.go` 会算出 review_available，把两半放进 `available_for_review`，卡片出现 quiet 级"有可复核的新能力"，抽屉里列出名字。但措辞是"目录提供了更多能力"（`reviewReasons.catalog_revision_advanced` 等），没有任何一句提到词表拆分或迁移——运维看到的是"多了可开的"，不是"你原有的被关了"。
  2. **目录不覆盖 / 运维自声明的模型**：能力列表里两半直接消失，`capability_evidence` 里写好的 `unsupported`（zh"不再受支持"）永远渲染不到——抽屉证据列只遍历 enabled 的能力。零信号，首个 JSON 请求被拒绝时才会发现。
  3. 两种情形都不入"需要关注"过滤器（quiet 不计入）。
- 界面层若要补：一个可行的最小修是把 `capability_evidence` 为 `unsupported` 但能力为 off 的项也列进抽屉能力区（数据已经在了，见 store.go:947），这恰好能把"被关了"和既有的"不再受支持"文案接上。"为什么"则仍只能靠发布说明。

## 疑似BUG

无。范围内未发现有代码证据的前端缺陷；`deploymentCondition.ts` 与卡片渲染的边界（价格未知 vs 缺失、测试中覆盖一切、suppressed 计数）均有测试且与实现一致。

## 契约漂移候选（待阶段 1 完成后交叉核对）

- **D1（对应 W11 / H1）**：迁移 32 的控制台可见性依赖读取期 `capability_drift.go` 的 review 计算，且只对目录覆盖的模型部分成立。请 A7/R3 核对：迁移后真实库里这两类部署各自的 `capability_review` 状态与 `capability_evidence` 值，是否与上文三种情形的推断一致；发布说明与 CHANGELOG 是否已按 §9 写明能力回退。
- **D2**：`store.go:861` 在迁移中把 `json_mode` 从 `operator_disabled` 删除但不写入两个后继，`capability_drift.go:211-217` 因此会把运维当初**主动关掉**的能力当作"可复核的新能力"（而非"已由管理员关闭"）重新兜售。这是运维决定在迁移中的语义变化，前端只是如实渲染——是否有意，请 A7 判定。
- **D3**：`capability_opt_in_warnings` 服务端只声明了词表，未区分"哪一层的表单需要警示"；连接层前端消费了，部署层没有（W9）。请 A3 核对服务端在部署写路径上是否有对应的准入约束（若部署级开启本就要求连接级先开且服务端二次校验，W9 降级为纯 UI 一致性问题）。
- **D4**：部署表单的能力分组 `deploymentCapabilityGroups`（`DeploymentsPage.tsx:1206-1211`）是前端硬编码，靠测试对照端点输出防漂移；A6 核对端点清单时如新增能力，注意这张表与 `capability_modalities`/`non_modal_capabilities` 三处要同步。

## 计数

| 分级 | 条数 |
|---|---|
| 肯定 | 7（W1–W7） |
| 建议 | 3（W8–W10） |
| 问题 | 1（W11） |
| 疑似BUG | 0 |

阻塞候选：无（W11 不属 release-assessment.md §5 七类阻塞，但绑定 §9 的发布说明硬要求）。
