# Heimdall 版本化模型定价与历史成本可追溯升级 PRD

- 状态：Implemented — Goal 0–6 已实现并通过自动化验收
- 目标版本：metadata schema v14 / Ledger WAL v2 / backup format v2
- 日期：2026-08-04
- 文档语言：中文
- 适用范围：Deployment 定价、Gateway 预算预留、Provider Attempt 结算、Usage、Dashboard、Audit、备份恢复

## 1. 文档定位

本 PRD 定义 Heimdall 从“Deployment 可原位修改单价”升级为“不可变价格版本 + Attempt 价格快照 + 追加式成本纠正”的产品与工程要求。

本升级不改变 Provider 返回 Token Usage 优先的计量原则，也不把 Heimdall 变成服务商账单系统。它解决的是以下问题：

1. 模型价格会随时间、地区、服务等级和合同变化；
2. 历史消费必须保持当时结算结果，不能用当前单价重算；
3. 审计人员必须能从单次 Provider Attempt 独立还原当时使用的单价、公式和来源；
4. 错价修正必须追加证据，不能覆盖历史账本；
5. 预算预留和最终结算必须绑定同一个价格版本，避免长请求跨越调价时刻后使用两套价格。

相关现有边界：

- Deployment 当前直接保存 `input_micros_per_million`、`output_micros_per_million` 和 `fixed_request_micros_usd`；
- 迁移前创建、尚未绑定 Deployment 的 legacy Route 仍可能直接保存输入/输出价格并进入 Runtime；
- Gateway 在请求前按估算 Token 预留预算，在 Attempt 完成后按 Provider Usage 或保守估算结算；
- Ledger 的 `EventAttemptSettled` 当前保存 Token、最终成本和估算标志，但不保存价格版本与单价；
- Usage 与 Dashboard 汇总 Ledger 已提交金额，不使用当前 Deployment 单价重新计算历史消费。

## 2. 问题陈述

### 2.1 历史金额不可独立验证

当前历史成本不会因 Deployment 后续调价而变化，这是正确的。但 Ledger 只保存最终金额，不能回答：

- 该 Attempt 当时使用了哪个价格版本；
- 输入、输出和固定价格分别是多少；
- 价格何时生效、由谁录入、来源是什么；
- 金额使用哪个舍入规则计算；
- 一次重试或 fallback 是否切换了价格不同的 Deployment。

因此历史金额具有持久性，却缺少完整可重演证据。

### 2.2 原位修改价格抹去配置历史

当前管理员更新 Deployment 时可以直接覆盖价格字段。Revision 和 Audit 能证明 Deployment 被修改过，但不能形成可查询的价格时间线，也不能可靠处理未来生效价格。

### 2.3 `0` 同时表示未知价格和免费

当前三个价格字段为 `0` 时，Dashboard 显示 `$0.00`。系统无法区分：

- 管理员尚未配置价格；
- 模型确实免费；
- 服务商按其他维度收费，而当前 Deployment 未表达；
- 历史迁移无法确定价格。

未知价格被表现为零成本会低估预算风险。

### 2.4 错价缺少追加式纠正路径

管理员延迟录入调价或错误填写单价后，当前系统既不应重写 Ledger，也没有正式的差额调整事件。直接修数据库会破坏审计与账本完整性。

## 3. 目标用户与核心场景

### 3.1 目标用户

- 为多个 Provider 和模型维护成本的 Heimdall 管理员；
- 负责预算、成本归集和内部对账的平台或 FinOps 团队；
- 审计历史调用和配置变化的安全、财务及合规人员；
- 负责升级、备份恢复、事故处理和账本验证的 SRE。

### 3.2 核心用户故事

1. 作为管理员，我可以为一个 Deployment 创建立即或未来生效的价格版本，而不覆盖历史价格。
2. 作为平台负责人，我可以确认每次 Provider Attempt 使用了哪一个不可变价格版本。
3. 作为审计人员，我可以使用 Ledger 中的 Token、单价快照和公式版本独立重算成本。
4. 作为 FinOps 人员，我可以区分免费、已定价、未知价格和历史未版本化消费。
5. 作为管理员，我可以通过追加调整事件纠正错价，不修改原始 Attempt 结算事件。
6. 作为 SRE，我可以升级旧数据库并保留全部历史金额、Ledger WAL sequence/CRC、Audit chain、Usage checkpoint 和备份可恢复性。

## 4. 目标

1. 引入 Deployment 级不可变价格版本，价格变化只创建新版本。
2. 价格版本支持立即生效和未来 UTC 时间生效。
3. 每次 Attempt 在 Provider I/O 前选择并持久化价格快照。
4. 预算预留、成功结算、估算结算和异常恢复使用同一快照。
5. Ledger 保存足够的价格证据，使单次成本可以离线重算。
6. Usage、Dashboard 和导出接口区分已知成本、未知成本、估算成本和调整金额。
7. 提供追加式成本调整事件，不覆盖原始结算。
8. 旧版本升级后历史金额保持逐 micro-USD 不变。
9. 价格缺失时 fail closed 或显式标记，禁止静默解释为免费。
10. 保持整数 fixed-point 计算、保守向上取整和现有按 Attempt 记账原则。
11. 在免费、未知价格和付费三种模式下，都先创建可恢复的 durable Accounting Lease，再执行 Provider I/O。
12. 对调价时间、历史调整归属时间和调整入账时间使用互不混淆的稳定字段。
13. 检测时钟回拨、旧备份恢复和跨存储部分提交，禁止价格时间线静默倒退。

## 5. 非目标

第一版不包含：

- 充当 Provider 最终发票、税务或应付账款系统；
- 自动保证公开价与企业合同价一致；
- 汇率换算、多币种会计、税费、信用额度或预付余额；
- 按缓存输入、Batch、长上下文、Priority Tier、地域阶梯量、峰谷时段分别计价；
- 根据生成内容质量、延迟或业务价值动态定价；
- 修改 Provider 返回的 Token 数；
- 自动抓取网页后无需人工确认直接激活价格；
- 使用 LLM 回答直接覆盖生产价格；
- 重写或删除既有 Ledger 事件；
- 对旧版历史 Attempt 猜测并补造不存在的价格证据；
- 在本阶段实现组织级审批角色和四眼审批工作流。

第一版虽不实现 RBAC 和四眼审批，但价格调整、显式免费和超过阈值的调价仍必须执行 recent re-auth、额度限制、二次确认和完整 Audit。缺少职责分离属于明确剩余风险，不得解释为普通低风险 Admin 修改。

缓存 Token、Batch 折扣等后续能力必须扩展价格公式与 Usage 语义，不能复用普通输入 Token 字段伪装实现。

## 6. 核心产品决策

### 6.1 成本权威仍然是每次 Provider Attempt

一次客户端请求可能发生重试或 fallback，并产生多个 Provider Attempt。每个 Attempt 可能使用不同 Deployment、模型和价格，因此必须独立计算成本。

```text
Client Request
  ├─ Attempt 1 → Deployment A → Price A/v3 → Cost 1
  └─ Attempt 2 → Deployment B → Price B/v7 → Cost 2

Request Cost = Cost 1 + Cost 2
```

Dashboard 的日、项目、Provider 和模型成本都是已结算 Attempt 成本与调整事件的聚合，不得使用聚合 Token 乘当前价格反推。

### 6.2 价格条款不可变

价格版本一旦创建，其以下字段永不修改：

- Deployment；
- billing mode；
- currency；
- 输入、输出和固定单价；
- 公式版本；
- 生效时间；
- 来源元数据和内容 digest；
- 创建人和创建时间。

录入错误时创建后继版本。仅尚未生效且从未被 Attempt 引用的未来版本允许取消；取消只改变生命周期状态，不修改价格条款。

### 6.3 时间只决定选择，Attempt 快照决定结算

每个 Attempt 在查价前只调用一次服务端时钟，得到不可变的 `pricing_selected_at`。系统使用该时间选择价格、计算预算，并把 `pricing_selected_at`、Accounting Lease 和完整 PriceSnapshot 原子写入 Ledger。Ledger `occurred_at` 仍表示事件实际 append 时间，不能反过来作为价格选择时间。

随后即使发生以下情况，也继续使用已绑定快照：

- 价格版本在请求处理中切换；
- Deployment 被修改、停用或删除；
- Runtime 热加载新配置；
- Provider 响应延迟跨越生效时刻；
- 客户端断开但后台继续完成结算。

同一客户端请求中的后续重试或 fallback 是新的 Attempt，应分别捕获自己的 `pricing_selected_at`，并选择目标 Deployment 当时有效的价格版本。

价格选择不得采用“先查价格、再在 Budget Manager 内重新读取时间”的循环定义。边界测试和离线审计一律以 Ledger 中的 `pricing_selected_at` 为准。

### 6.4 价格生效时间使用 UTC

- `effective_from` 必须是带时区的 RFC 3339 时间，服务端规范化为 UTC；
- 选择规则为同一 Deployment 中 `effective_from <= pricing_selected_at` 的最新非取消版本；
- 同一 Deployment 不允许两个版本具有相同 `effective_from`；
- 正常 Admin API 禁止创建已经早于服务端当前时间的版本；
- 未来版本可以取消，但生效后不能取消或删除；
- 价格时间线不使用 `usage.timezone`，避免 DST 和部署时区变化影响选择；
- Project 日预算仍按既有 `usage.timezone` 划分自然日，两者不得混用。

服务端 wall clock 不是天然可信时间。每个 Deployment 必须持久化 `last_observed_price_version_id`、`last_observed_effective_from` 和 `last_observed_at` 高水位：

- 已经被任何 durable Accounting Lease 使用的价格版本不得因时钟回拨重新退回旧版本；
- 检测到 wall clock 早于持久化高水位超过允许偏差时，相关 Deployment fail closed，并降低 readiness；
- 时钟大幅前跳时不得静默提前激活 scheduled 价格，必须进入 pricing quarantine 等待时钟恢复或管理员复核；
- 部署基线必须使用 NTP/chrony 或等价可信时间同步，并配置偏差、回拨和同步失效告警；
- 最大允许偏差由配置给出，生产默认建议不超过 30 秒；关闭该检查必须产生启动警告和 Audit；
- 旧备份恢复后的高水位处理遵循第 14.5 节的 pricing quarantine，不允许仅以备份内旧时钟状态恢复流量。

### 6.5 未知价格不等于免费

价格相关状态拆成三个正交维度，不允许 API、Ledger、Parquet 或 UI 使用一个枚举混合表达：

| 维度 | 稳定值 | 含义 |
|---|---|---|
| `billing_mode` | `metered \| free` | 当前价格条款如何计费；unknown/legacy 没有 billing mode |
| `price_evidence_status` | `versioned \| unknown \| legacy_unversioned` | 是否存在可验证价格版本证据 |
| `cost_value_status` | `known \| unknown` | micro-USD 成本值是否存在；unknown 时不能序列化为整数零 |

组合语义：

| 组合 | 含义 | 成本表现 |
|---|---|---|
| `versioned + metered + known` | 按 Token 和/或固定请求价格计费 | 按公式结算 |
| `versioned + free + known` | 管理员明确声明当前价格为零 | 已知成本 `$0` |
| `unknown + 无 billing mode + unknown` | 没有有效价格或当前公式无法表达 | 成本为 `null`，不能显示为免费 |
| `legacy_unversioned + 无 billing mode + known` | 升级前已有金额但缺少价格证据 | 保留既有金额并显示证据不完整 |

规则：

- `free` 必须通过显式 billing mode 创建价格版本，不能由全部价格为零自动推断；
- `metered` 至少一个价格分量大于零；
- `unknown` 不创建伪造的零价版本，Price Version ID、单价和成本值均为 nullable；
- Project 设置了每日预算，或绑定了成本维度 Token Guard 时，`unknown` Deployment 必须在 Provider I/O 前返回稳定错误；
- 未启用成本治理的 Project 可以由实例策略选择“拒绝”或“允许但标记未知”，默认拒绝；
- 未知 Attempt 的成本字段必须为 `null/unknown` 语义，不能进入已知成本合计；
- 所有持久化模型必须提供必填/nullable 验证矩阵，禁止使用 Go/JSON 零值推断 unknown；
- Token 是否估算继续由 `token_estimated` 表达，不能与上述三个价格维度合并。

### 6.6 价格来源是证据，不是运行时网络依赖

第一版支持来源类型：

- `manual`：管理员依据合同或内部价格录入；
- `official_url`：记录公开官方价格页；
- `provider_api`：未来受支持的官方机器接口结果；
- `import`：受控离线清单导入；
- `migration`：由旧 Deployment 当前价格迁移生成。

请求热路径不访问价格网页、LLM 或 Provider 控制面。来源 URL 只作为审计元数据；第一版服务端不主动抓取该 URL。

来源另有独立保证等级：

| `source_assurance` | 含义 |
|---|---|
| `asserted` | 管理员声明，Heimdall 未验证材料真实性 |
| `verified_api` | 由受支持的 Provider 官方 API 获取并由服务端记录接收时间 |
| `signed_import` | 来自通过签名、schema version 和签名者 allowlist 验证的价格清单 |

`manual`、`official_url` 和 `migration` 默认只能是 `asserted`。UI 不得仅因为 URL 域名看似官方就显示“已验证”。敏感合同不保存正文时，必须记录外部 WORM/文档系统中的不可变证据 ID、版本、custody owner 和 digest；只有 URL + 管理员输入 digest 的记录应准确称为“来源声明/指纹”，不能宣称完成独立取证。

来源字段按类型定义必填矩阵，并统一限制长度、字符集和数量。`uri`、`reference`、`note`、URL path 和外部证据 ID 都必须通过 Secret Scanner；控制台外链使用安全转义与 `noopener noreferrer`，禁止自动预取。未来如启用抓取，只能复用 SafeTransport、重定向逐跳重校验和 egress allowlist。

### 6.7 LLM 只能生成价格建议

未来如增加“获取价格建议”，其输出必须保存为独立 Proposal，不能直接成为 Price Version。Proposal 必须显示来源、抓取时间、模型/地区匹配信息和警告，经管理员确认后才能创建不可变价格版本。

Proposal 最小 schema 包含：Provider、模型/Deployment 身份、Region、服务等级/Tier、候选价格项、来源引用与 assurance、服务端抓取时间、解析警告、匹配置信状态、过期时间和 Proposal digest。Proposal 永不进入 Gateway 热路径，到期后不能确认；管理员确认只会以 Proposal 为输入新建 Price Version，并再次执行全部价格与来源验证。

禁止：

- 仅凭 LLM 训练知识生成生产价格；
- 无官方或合同来源时自动激活；
- 定时任务静默覆盖当前价格；
- 把价格抓取失败降级为零价格。

## 7. 价格数据模型

### 7.1 `DeploymentPriceVersion`

建议新增独立 bbolt bucket，概念模型如下：

```json
{
  "id": "price_...",
  "deployment_id": "dep_...",
  "version": 3,
  "billing_mode": "metered",
  "currency": "USD",
  "formula_version": "usd_token_v1",
  "input_micros_per_million": 400000,
  "output_micros_per_million": 1600000,
  "fixed_request_micros_usd": 0,
  "effective_from": "2026-09-01T00:00:00Z",
  "source": {
    "type": "official_url",
    "assurance": "asserted",
    "uri": "https://provider.example/pricing",
    "published_at": "2026-08-20T00:00:00Z",
    "retrieved_at": "2026-08-21T03:00:00Z",
    "received_at": "2026-08-21T03:05:00Z",
    "content_sha256": "sha256:...",
    "reference": "Public standard tier",
    "note": "Region-independent public list price"
  },
  "created_by": "admin_...",
  "created_at": "2026-08-21T03:05:00Z",
  "cancelled_by": "",
  "cancelled_at": null,
  "revision": 1
}
```

约束：

- `id` 全局唯一，使用现有安全 ID 生成器；
- `version` 在 Deployment 内单调递增且不复用；
- `version` 只表示创建序列，不表示生效时间顺序；API/UI 必须始终按 `effective_from` 排序；
- 第一版为降低操作歧义，新版本的 `effective_from` 必须晚于该 Deployment 所有未取消版本；需要插入历史时间线时必须走 Correction Price Evidence，不能走正常版本 API；
- `currency` 第一版固定为 `USD`；
- 金额使用非负 `int64` micro-USD；
- `formula_version` 第一版固定为 `usd_token_v1`；
- `source.uri` 只允许无 userinfo、无 query、无 fragment 的 HTTPS URL；
- `source` 不得包含 API Key、合同正文、个人信息或其他 Secret；
- `content_sha256` 只证明管理员所依据材料的 digest，不要求 Heimdall 保存受版权或合同保护的原文；
- `received_at` 由服务端写入且不可由客户端覆盖；`published_at/retrieved_at` 在 asserted 来源中只是管理员声明；
- `revision` 只用于取消 scheduled 版本等生命周期操作，不允许借此修改价格条款。

### 7.2 生命周期状态

```text
scheduled ──到达 effective_from──► active ──后继版本生效──► superseded
    │
    └──生效前且未被引用──► cancelled
```

- `scheduled`：未来版本，尚不参与请求；
- `active`：当前选择结果；
- `superseded`：被后继版本取代，继续供历史查询；
- `cancelled`：生效前取消，不参与选择，永久保留审计元数据。

只有 `cancelled_at/cancelled_by` 是持久化生命周期事实。`scheduled`、`active` 和 `superseded` 必须根据非取消时间线、`pricing_selected_at` 和持久化激活高水位派生，禁止同时保存可漂移的 status 权威。系统不能依赖后台定时任务恰好在生效秒执行状态切换。

bbolt 索引必须使用 canonical 二进制键，例如 `deployment_id + big-endian UTC nanos + price_id`。`next_version`、生效时间唯一性和价格记录必须在同一 bbolt 事务内更新，禁止使用格式不唯一的 RFC 3339 字符串作为排序权威。

### 7.3 Deployment 关系

升级后 Deployment 继续拥有模型能力和容量，但不再直接拥有可写价格字段：

```text
Deployment
  ├─ capabilities
  ├─ max_concurrency
  └─ price timeline
       ├─ price v1 · superseded
       ├─ price v2 · active
       └─ price v3 · scheduled
```

为兼容 Admin 展示，Deployment 响应可以提供只读派生字段：

```json
{
  "price_evidence_status": "versioned",
  "active_billing_mode": "metered",
  "active_price_version": {"id": "price_...", "version": 2},
  "next_price_effective_at": "2026-09-01T00:00:00Z"
}
```

旧的三个价格字段在一个明确的 Admin API 兼容窗口内可以只读返回当前有效值，但创建和更新 Deployment 不再接受其作为价格写入口。兼容窗口结束后删除这些字段，避免出现双写权威。

### 7.4 PriceSnapshot nullable 矩阵

| 字段 | metered | free | unknown | legacy_unversioned |
|---|---:|---:|---:|---:|
| `price_version_id/version` | 必填 | 必填 | `null` | `null` |
| `billing_mode` | `metered` | `free` | `null` | `null` |
| 单价字段 | 必填、至少一项大于零 | 必填且全零 | `null` | `null` |
| `cost_micros_usd` | 整数 | `0` | `null` | 既有整数 |
| `source_assurance/digest` | 按来源矩阵 | 按来源矩阵 | 可选 unknown reason | 不补造 |
| `pricing_selected_at` | 必填 | 必填 | 必填 | 旧事件不存在 |

Go 领域模型必须使用显式 tagged union、指针或 option 类型表达 nullable，禁止让 `int64(0)` 同时代表免费、未知和字段缺失。

## 8. Ledger 价格快照

### 8.1 Durable Accounting Lease

现有“reservation 金额必须大于零”的单一模型升级为带类型的 Accounting Lease。所有 Attempt，包括免费和允许放行的未知价格 Attempt，都必须先持久化 Lease，再执行 Provider I/O：

| `lease_mode` | 金额语义 | PriceSnapshot | Budget 行为 |
|---|---|---|---|
| `metered` | `reservation_micros_usd > 0` | versioned metered | 计入 reserved balance |
| `free` | `reservation_micros_usd = 0` | versioned free | 不占用金额预算，但保留 Attempt 生命周期 |
| `unknown_allowed` | reservation amount 为 `null` | unknown tagged snapshot + policy evidence | 仅允许在无成本治理且显式 opt-in 时使用，不进入已知余额 |

要求：

- 不得只把现有校验从 `> 0` 改成 `>= 0`；Ledger Validate、Balance、replay、pending recovery 和 Usage 都必须按 `lease_mode` 验证；
- `free` 的零金额是已知事实，`unknown_allowed` 的 null 是未知事实，两者不能共享整数零表示；
- unknown 放行必须记录实例策略版本、Project ID、Token Guard 状态和拒绝成本治理的原因；
- 即使金额为零或未知，Lease 仍拥有 Attempt ID、生命周期、prepared Token 上界和恢复状态；
- metered/free/unknown 三种 Lease 均参与并发、RPM/TPM、Token Guard 的非成本维度和 Provider Attempt 统计；
- Budget State 分别维护 known reserved/committed 和 unknown Attempt 计数，不能把 unknown 加入已知余额。

### 8.2 `PriceSnapshot`

每次 Attempt reservation 必须持久化以下快照：

```json
{
  "pricing_selected_at": "2026-09-01T00:00:01Z",
  "price_evidence_status": "versioned",
  "cost_value_status": "known",
  "price_version_id": "price_...",
  "price_version": 3,
  "billing_mode": "metered",
  "currency": "USD",
  "formula_version": "usd_token_v1",
  "input_micros_per_million": 400000,
  "output_micros_per_million": 1600000,
  "fixed_request_micros_usd": 0,
  "effective_from": "2026-09-01T00:00:00Z",
  "source_type": "official_url",
  "source_assurance": "asserted",
  "source_content_sha256": "sha256:..."
}
```

Ledger 必须自包含足够字段完成成本复算，不能要求历史查询依赖仍然存在的 bbolt Price Version。`price_version_id` 用于导航和一致性校验，不能替代单价快照。free 和 unknown 使用第 7.4 节 nullable 矩阵定义的 tagged variant，不能强行填充示例中的 metered 字段。

### 8.3 持久化时序

必须遵循：

```text
捕获一次 pricing_selected_at
        ↓
选择 Deployment 与有效 Price Version
        ↓
计算最大 Token 与预算预留
        ↓
持久化 Accounting Lease + PriceSnapshot + prepared Token 上界
        ↓
持久化 AttemptStarted
        ↓
开始 Provider I/O
        ↓
获得 Provider Usage 或保守估算
        ↓
使用同一 PriceSnapshot 结算
        ↓
持久化 AttemptSettled
```

Accounting Lease、PriceSnapshot 和 `AttemptStarted` 尚未 durable 时禁止发起 Provider I/O。这样即使进程在请求发送后崩溃，恢复逻辑也能使用当时价格完成保守结算。

### 8.4 Ledger 事件扩展

`EventReservationCreated` 和 `EventAttemptSettled` 至少新增：

- `lease_mode`；
- `pricing_selected_at`；
- `price_evidence_status`；
- `cost_value_status`；
- `price_version_id`；
- `price_version`；
- `billing_mode`；
- `currency`；
- `price_formula_version`；
- `input_micros_per_million`；
- `output_micros_per_million`；
- `fixed_request_micros_usd`；
- `input_cost_micros_usd`；
- `output_cost_micros_usd`；
- `fixed_cost_micros_usd`；
- `price_effective_from`；
- `source_type`；
- `source_assurance`；
- `source_content_sha256`；
- 现有 `committed_micros_usd`。

`EventReservationCreated` 还必须保存：

- `prepared_input_tokens`；
- `prepared_output_tokens`；
- reservation 上界；
- unknown allow policy evidence；
- 恢复幂等标识。

`EventAttemptStarted` 必须在任何可能把请求字节交给 Provider 的操作前 durable。适配器和 SafeTransport 必须提供清晰的“尚未开始外部 I/O/可能已经到达 Provider”边界；若无法证明未发送，一律按可能到达处理。

结算事件中的快照必须与 Accounting Lease 一致。任何 ID、单价或公式不一致都属于 Ledger invariant violation，必须 fail closed、使 readiness 失败并产生告警。

### 8.5 成本公式

`usd_token_v1`：

```text
input_cost_micro_usd  = ceil(input_tokens  × input_micro_usd_per_million  / 1,000,000)
output_cost_micro_usd = ceil(output_tokens × output_micro_usd_per_million / 1,000,000)
total_micro_usd       = input_cost + output_cost + fixed_request_micro_usd
```

要求：

- 全程使用 checked integer arithmetic；中间乘积使用 `bits.Mul64`/`bits.Div64`、`big.Int` 或等价宽整数，不能因为中间 `int64` 乘法溢出而拒绝最终本可表示的结果；
- 输入和输出分量分别向上取整到 1 micro-USD；
- 不允许二进制浮点参与 Ledger 权威计算；
- 前端十进制 USD 必须严格转换为 micro-USD，超过 6 位小数时拒绝而不是静默四舍五入；
- `free` 版本三个金额必须全部为零，但仍记录已知价格状态和版本 ID；
- `CostEstimated` 继续表示 Token 或调用结果不确定导致的保守成本，不表示单价来源可信度；
- 价格证据状态与 Token/成本估算状态必须分开表达。

### 8.6 Pending Lease 崩溃恢复状态机

Runtime 启动必须在 readiness 通过前 replay Ledger 并枚举全部 pending Accounting Lease：

```text
ReservationCreated，未见 AttemptStarted
  → 可以证明未进入 Provider I/O
  → 追加 start_failed/released Settlement，成本为 0

ReservationCreated + AttemptStarted，未见 AttemptSettled
  → Provider 结果未知
  → 使用 prepared input/output 上界与原 PriceSnapshot 保守结算

已有 AttemptSettled
  → 不得重复结算
```

恢复要求：

- recovery Settlement event ID 或 recovery idempotency key 必须从 `attempt_id + recovery schema version` 确定性派生，或在首次 intent 中持久化；
- 启动恢复本身再次崩溃后，重复 replay 只能得到同一结果；
- pending 状态必须保存完整 Attempt 元数据、Lease mode、prepared Token、PriceSnapshot 和 Started 事实，不能只保留 project/period/amount；
- recovery 先追加 Ledger，再更新 checkpoint；checkpoint 永远不是恢复权威；
- unknown_allowed 恢复后成本仍为 unknown，不得改写为零；
- 恢复过程中发现快照缺失、状态冲突或 overflow 时保持 listeners 关闭并降低 readiness；
- Metrics 和 `doctor` 显示 pending 数量、最老年龄和恢复结果，但不暴露模型价格来源的敏感自由文本；
- kill-point 测试覆盖 Reservation append 前后、Started append 前后、socket write 前后、Settlement append 前后及恢复 Settlement 期间再次崩溃。

### 8.7 价格取消与 Lease Pin 一致性

价格版本存于 bbolt，Accounting Lease 存于 Ledger WAL，两者不能依靠单个本地事务原子提交。当前单实例/单写者模式必须为每个 Deployment 提供串行化 gate：

1. Gateway 在 gate 内捕获 `pricing_selected_at`、选择版本并创建 durable pin intent；
2. Accounting Lease 成功 append 后，pin 标记为 committed；
3. 取消 scheduled 版本必须在同一 gate 内检查 `now < effective_from`，且没有 pending/committed pin；
4. pin intent 与取消动作任一侧崩溃时，启动恢复根据 intent 和 WAL 是否存在完成或回滚；
5. 锁顺序固定为 Deployment pricing gate → bbolt pricing tx → Ledger project lock，禁止反向获取；
6. 取消成功后已经 durable 的 Lease 仍使用其快照；未 durable 的选择不得开始 Provider I/O。

如实现选择 outbox 而非 pin intent，必须通过 ADR 证明相同的崩溃与 TOCTOU 不变量。未来多写者模式需要重新设计，不能把进程内锁当作集群协议。

## 9. Token、价格与结算语义

### 9.1 Token 权威不变

- Provider 返回有效 Usage 时，使用 Provider Input/Output Token；
- Provider 未返回 Usage、流结束缺失 Usage 或结果不明确时，沿用现有保守估算；
- `TokenEstimated` 和 `PreparedOutputTokens` 语义保持不变；
- 价格版本不影响 Token 数，也不能借价格表纠正 Provider Token。

### 9.2 请求前预留

预算预留使用：

- 脱敏后请求的输入 Token 估算；
- 请求允许的最大输出 Token；
- 当前 Attempt 绑定的 PriceSnapshot；
- 固定请求价格。

调价不能改变已存在 reservation。新 Attempt 使用新价格可能因预算不足而被拒绝，即使同一个客户端请求的前一个 Attempt 已经开始。

Token Guard 当前在具体 Attempt 之前计算候选 Deployment 的最大成本。升级后必须在请求准入时使用同一个 `pricing_selected_at` 捕获候选价格视图，并记录 `token_guard_pricing_view_digest`。真正创建 Attempt 时如价格时间线已经前进：

- Token Guard 必须使用新 Attempt 价格重新检查成本维度；
- 非成本维度 Lease 可以复用；
- 新成本超过策略时在 Provider I/O 前拒绝；
- 不允许继续使用请求开始时已经过期的 Deployment 单价低估风险。

free/unknown 的 Token Guard 行为：free 的估算成本为已知零；unknown 只能在策略没有成本维度且实例明确允许时通过，否则返回 `price_unavailable`。

### 9.3 最终结算

- 正常成功：Provider Usage × PriceSnapshot；
- 成功但无 Usage：保守 Token × PriceSnapshot，并标记 EST；
- ambiguous failure：按现有最大风险 Token × PriceSnapshot 保守结算；
- 明确未到达 Provider 的失败：按现有语义释放 reservation，不产生伪造 Token 成本；
- Provider 实际发票与本地价格不一致不直接重写 Attempt，应通过对账和调整事件处理。

## 10. 追加式成本调整

### 10.1 原则

原始 reservation、AttemptSettled、Token 和价格快照不可修改。错价修正新增 `EventCostAdjusted`。正常 Price Version API 禁止 backdate，因此迟到的历史价格通过不可参与实时选择的 `CorrectionPriceSnapshot` 表达，不能为了纠价向生产价格时间线插入过去版本。

```json
{
  "event_id": "evt_...",
  "kind": "cost_adjusted",
  "request_id": "req_...",
  "attempt_id": "attempt_...",
  "original_settlement_event_id": "evt_...",
  "original_settlement_sha256": "sha256:...",
  "project_id": "prj_...",
  "service_period_id": "2026-06-01",
  "original_completed_at": "2026-06-01T09:00:00Z",
  "posted_period_id": "2026-08-04",
  "posted_at": "2026-08-04T08:00:00Z",
  "base_settlement_cost_micros_usd": 204,
  "net_cost_before_micros_usd": 204,
  "delta_micros_usd": 102,
  "net_cost_after_micros_usd": 306,
  "adjustment_sequence": 1,
  "idempotency_key_sha256": "sha256:...",
  "mode": "reprice",
  "correction_price_snapshot": {
    "billing_mode": "metered",
    "currency": "USD",
    "formula_version": "usd_token_v1",
    "input_micros_per_million": 600000,
    "output_micros_per_million": 2400000,
    "fixed_request_micros_usd": 0,
    "input_tokens": 30,
    "output_tokens": 120,
    "input_cost_micros_usd": 18,
    "output_cost_micros_usd": 288,
    "fixed_cost_micros_usd": 0,
    "source_assurance": "asserted",
    "source_content_sha256": "sha256:..."
  },
  "reason_code": "late_provider_price_update",
  "reason": "Provider public price effective date was entered late",
  "created_by": "admin_...",
  "occurred_at": "2026-08-04T08:00:00Z"
}
```

Adjustment mode：

- `reprice`：使用完整 CorrectionPriceSnapshot 和原 Attempt Token 重新计算；
- `explicit_delta`：仅用于现有公式无法表达的 Provider 账单差额，必须给出 signed delta、固定 reason code 和更高保证等级的外部证据；
- 错误 Adjustment 只能追加反向或后继事件纠正，不能删除、取消或覆盖。

### 10.2 调整规则

- delta 可以为正或负，但单个 Attempt 的累计净成本不得小于零；
- 每个调整引用原结算事件 digest 和证据来源；
- `reprice` 必须保留原 Token，不允许同时修改 Token；
- Adjustment 事件自包含完整 CorrectionPriceSnapshot；任何 Price Version ID 只能导航，不能作为复算权威；
- `base_settlement_cost` 永远是原 Settlement，`net_cost_before/after` 是该 Attempt 调整链前后的净额，并满足 `after = before + delta`；
- `adjustment_sequence` 在 Attempt 内从 1 单调递增，创建时要求 `If-Match` 或 `expected_net_cost/expected_sequence`；
- Ledger 在同一 Project lock 内原子验证 sequence、净额非负和恒等式；
- 幂等键的 digest 必须进入权威事件和 replay state；same key + same payload 返回原结果，same key + different payload fail closed；
- 调整写入 Ledger 和 Admin Audit；
- 客户端只能提交 Attempt ID、mode、候选价格/显式 delta、reason 和来源；request/project/period、原 Token、原成本及所有分量由服务端权威数据派生；
- 账务真实性高于预算 admission：Adjustment 不能因为正向 delta 会超预算而被拒绝；当前周期正向调整允许产生 over-budget 状态并阻断后续 reservation；
- 当前周期负向调整默认释放额度，但必须产生 Audit、Metric 并与 Project Balance 原子一致；
- 已关闭历史服务期的调整不占用今天预算；
- Dashboard 同时展示原始已提交成本、调整净额和调整后成本；
- 导出必须保留每条调整，不得只导出覆盖后的最终数字。

第一版 UI 可以只支持单 Attempt 调整。批量回填必须通过后续受控工具实现，要求 dry-run、影响摘要、幂等键和逐事件审计。

### 10.3 服务期与入账期双时间轴

每个 Adjustment 同时保存：

- `service_period_id` 与 `original_completed_at`：成本归属的原 Attempt 时间；
- `posted_period_id` 与 `posted_at`：纠正操作实际写入 Ledger 的时间。

查询必须提供两种明确口径：

1. `service_period_restated`：把 delta 归入原 Attempt 服务期，用于重述后的项目、模型和 Provider 成本；
2. `adjustment_posted`：把 delta 归入实际入账日，用于当日 FinOps 操作量、审计和 reconciliation。

Dashboard 默认成本趋势使用 `service_period_restated`，并显示“历史已因调整变化”标记；“今日调整”卡片使用 `adjustment_posted`。API、CSV/Parquet 导出必须带 `reporting_basis`，禁止同一无标签字段有时按服务期、有时按入账期。任何时间范围都满足：

```text
restated_service_cost = original_settlements_in_service_period + adjustments_attributed_to_service_period
posted_adjustment_total = adjustments_whose_posted_at_is_in_query_period
```

### 10.4 Adjustment Parquet 数据集

现有一行一个 Settlement Attempt 的 Parquet partition 不得因后续调整被覆盖。新增独立 append-only `cost_adjustments` 数据集，至少保存：

- Ledger event ID、sequence、idempotency digest；
- request/attempt/project/deployment/provider 身份；
- service 与 posted 双时间轴；
- base/before/delta/after；
- CorrectionPriceSnapshot 与证据 digest；
- reason code、调整序号和创建者 ID。

Settlement 与 Adjustment 使用独立 watermark/manifest。查询层按 Attempt ID reduce/join；已封存 partition 永不原位修改。checkpoint 必须保存 settlement-by-attempt、累计 delta、adjustment sequence、幂等 digest 和双时间桶；旧 checkpoint 缺少这些状态时直接丢弃并从 Ledger 全量重建，不能把缺失字段解释为零。

## 11. Admin API 草案

以下路径均位于现有 `/admin/api/v1`，要求 Admin Session、CSRF、同源校验、稳定错误响应和统一请求体大小限制。创建 `free` 版本、超过实例阈值的调价和任何成本调整还要求 5 分钟内 recent re-auth；已启用 MFA 时必须包含 TOTP，未启用时至少重新验证当前密码。

实例必须限制每个 Deployment 的 scheduled/历史版本数量、版本创建速率、自由文本长度和幂等记录保留期。默认建议：scheduled 不超过 16 个、reference 不超过 256 字符、note/reason 不超过 1024 字符；最终值由配置模型与容量测试确认。

### 11.1 查询价格时间线

```http
GET /deployments/{deployment_id}/prices
```

返回 active、scheduled、superseded 和 cancelled 版本，支持 cursor 分页。

### 11.2 创建价格版本

```http
POST /deployments/{deployment_id}/prices
Content-Type: application/json
Idempotency-Key: ...
```

```json
{
  "billing_mode": "metered",
  "currency": "USD",
  "input_usd_per_million": "0.40",
  "output_usd_per_million": "1.60",
  "fixed_request_usd": "0",
  "effective_from": "2026-09-01T00:00:00Z",
  "source": {
    "type": "official_url",
    "uri": "https://provider.example/pricing",
    "published_at": "2026-08-20T00:00:00Z",
    "retrieved_at": "2026-08-21T03:00:00Z",
    "content_sha256": "sha256:...",
    "reference": "Standard tier"
  }
}
```

API 金额使用十进制字符串，避免 JSON number/JavaScript 浮点产生不可见误差。服务端返回 canonical micro-USD 整数与标准化十进制显示值。

按 source type 的最小要求：

| 类型 | 必填字段 |
|---|---|
| `manual` | reference、note、外部证据 ID 或明确 `asserted_without_archive` |
| `official_url` | HTTPS URI、retrieved_at 声明、reference；默认 assurance=asserted |
| `provider_api` | Adapter、服务端 received_at、响应 digest、provider request ID |
| `import` | manifest ID、schema version、签名者、签名 digest；assurance=`signed_import` |
| `migration` | migration version、原资源 ID、原 revision |

free 版本同样必须有来源声明，不能以“金额为零”为由省略证据。

### 11.3 取消未来版本

```http
POST /deployments/{deployment_id}/prices/{price_id}/cancel
If-Match: "revision"
```

只允许 scheduled 且未被任何 durable pin 或 Accounting Lease 引用的版本。

### 11.4 价格预览

```http
POST /deployments/{deployment_id}/prices/preview
```

输入 Token、输出 Token、候选价格和生效时间，返回公式分量与预算影响；不写入任何状态。

### 11.5 创建成本调整

```http
POST /usage/attempts/{attempt_id}/cost-adjustments
Idempotency-Key: ...
If-Match: "adjustment-sequence-or-net-revision"
```

客户端只提交 `reprice` 的 Correction Price 条款，或受限 `explicit_delta`、标准 reason code、文字说明和来源证据。request/project/period、Token、base/before/after 和公式分量由服务端派生。API 必须先提供 preview，显示服务期、入账期、预算影响、调整前后净额和证据保证等级，再要求二次确认。

第一版为单次和 24 小时累计调整配置软/硬阈值。超过硬阈值时没有四眼审批能力的实例必须拒绝并提示使用受控离线流程；不得由普通确认弹窗绕过。

### 11.6 稳定错误码

| HTTP | 错误码 | 场景 |
|---|---|---|
| `400` | `invalid_price` | 负数、精度过高、metered 全零、free 非零 |
| `400` | `invalid_effective_time` | 无时区、正常接口回填过去时间 |
| `404` | `price_version_not_found` | 版本不存在或不属于 Deployment |
| `409` | `price_timeline_conflict` | 相同生效时间、并发创建冲突 |
| `409` | `price_version_in_use` | 尝试取消已生效或已引用版本 |
| `412` | `revision_mismatch` | `If-Match` 失败 |
| `422` | `price_source_invalid` | 来源 URL 或 digest 不合法 |
| `409` | `adjustment_conflict` | sequence、expected net 或幂等 payload 冲突 |
| `422` | `adjustment_evidence_required` | 调整缺少符合 mode 的证据 |
| `409` | `price_unavailable` | 成本治理要求已知价格但无有效版本；Gateway 配置冲突，不可重试，不返回通用 Provider failure |

## 12. Admin Console 需求

### 12.1 Deployment 页面

“容量与成本”升级为“容量与价格”：

- 并发上限继续属于 Deployment；
- 当前价格以只读卡片显示，不再直接编辑；
- 显示状态：已定价、免费、未知、即将调价；
- 显示 Price Version、输入/输出/固定价格、生效时间和来源；
- 提供“新建价格版本”操作；
- scheduled 版本显示倒计时和取消操作；
- 历史时间线显示 superseded 版本，但不提供修改或删除；
- Deployment 启用但价格未知时显示高优先级警告；
- 保存普通 Deployment 属性不得隐式创建价格版本。

### 12.2 新建价格版本流程

1. 选择 `metered` 或 `free`；
2. 输入价格和生效时间；
3. 填写来源类型、URL/digest/说明；
4. 展示相对当前价格的百分比变化；
5. 使用示例 Token 预览单次成本；
6. 明确提示版本生效后不可修改或删除；
7. 管理员二次确认后创建。

如果价格下降或上涨超过实例可配置阈值，UI 必须强调显示，但第一版不要求第二管理员审批。

创建 free 版本或超过阈值的价格必须在确认前完成 recent re-auth。来源为 asserted 时显示“管理员声明，未经 Heimdall 独立验证”，不得使用“官方已验证”徽标。

### 12.3 Usage 页面

每个 Attempt 展示：

- Provider 报告或估算 Token；
- Price Version ID 和版本号；
- 当时输入/输出/固定价格；
- 公式版本和成本分量；
- 原始成本、调整净额、最终成本；
- `EST.`、`FREE`、`UNKNOWN`、`LEGACY`、`ADJUSTED` 标签；
- 来源摘要和 Audit 跳转。

存在 Adjustment 时同时展示服务期和入账期，并允许切换“重述成本”和“入账调整”口径。

### 12.4 Dashboard

- 已知成本合计只汇总 `versioned+metered`、`versioned+free` 和 `legacy_unversioned+known` committed cost；
- 未知价格 Attempt 单独计数，不能并入 `$0.00`；
- 展示成本调整净额；
- Top 5 和趋势使用 Attempt 已提交/调整后的成本，不按当前价格重算；
- 历史未版本化金额保留在总额中，并显示证据完整度警告；
- 小额成本可以继续以高精度 tooltip 展示，避免两位小数掩盖有效计价。
- 成本趋势默认使用 `service_period_restated`；另设“今日入账调整”指标使用 `adjustment_posted`；
- pricing quarantine、时钟异常、unknown 放行和 over-budget adjustment 必须显示独立治理状态，不能只折叠成普通错误率。

## 13. Audit、Metrics 与告警

### 13.1 Audit 事件

至少增加：

- `deployment_price.create`；
- `deployment_price.cancel`；
- `deployment_price.activate_observed`；
- `cost_adjustment.create`；
- `pricing_migration.complete`；
- `pricing_import.preview`；
- `pricing_import.apply`。

Audit 记录资源 ID、Deployment ID、版本号、生效时间、来源类型、digest、变更前后摘要和操作者，不记录合同正文、Secret 或带 query 的 URL。

价格版本到点生效是确定性时间选择，不要求每台 Runtime 在生效秒写 Audit。`activate_observed` 只在系统首次观察到新版本生效时幂等写入，不能成为选择正确性的前提。

Ledger 与 Admin Audit 是两个独立持久化边界。价格创建、取消和成本调整必须使用 durable mutation intent：

1. bbolt 事务写入 intent、幂等 digest、目标 Ledger/Audit event ID；
2. 按固定顺序追加权威 Ledger 或价格记录；
3. 追加 Admin Audit，记录 success/rejected/failed；
4. 标记 intent delivered；
5. 启动时重放未完成 intent，同一 event ID 重试不得产生重复事件。

实现 ADR 必须列出每个 tx commit、WAL append/fsync、Audit append/fsync 和 delivered 标记之间的 kill point。不得允许“Ledger 已调整但 Audit 永久缺失”或“Audit 宣称成功但权威价格/成本未写入”。

### 13.2 Metrics

建议新增低基数指标：

- `heimdall_pricing_unknown_attempts_total`；
- `heimdall_pricing_version_created_total{billing_mode}`；
- `heimdall_pricing_version_cancelled_total`；
- `heimdall_cost_adjustments_total{direction}`；
- `heimdall_cost_adjustment_micros_usd_total{direction}`；
- `heimdall_pricing_invariant_failures_total`；
- `heimdall_deployments_without_active_price`。
- `heimdall_pricing_clock_offset_seconds`；
- `heimdall_pricing_clock_rollback_total`；
- `heimdall_pricing_quarantine_deployments`；
- `heimdall_pricing_recovery_pending_intents`；
- `heimdall_pricing_migration_failures_total`。

Metrics 标签不得包含 Price ID、Deployment ID、Provider model、来源 URL 或管理员身份，避免高基数和信息泄露。

### 13.3 告警

- 已启用 Deployment 没有有效价格；
- 未来 7 天存在 scheduled 价格但来源证据缺失；
- PriceSnapshot 与结算事件不一致；
- unknown Attempt 数量增加；
- 成本调整金额或频率异常；
- 价格时间线读取失败或出现重复生效时刻；
- Usage checkpoint 无法处理新 Ledger schema。
- 时钟偏差、回拨或时间同步失效；
- restore pricing quarantine 未解除；
- Ledger/Audit mutation intent 长时间未交付；
- migration readiness gate 未通过；
- 价格版本数量、scheduled 数或存储增长超过容量阈值。

每条生产告警必须在 Metrics Reference 中给出阈值、for duration、严重度和 Runbook。价格配置错误使用不可重试 Gateway 配置冲突错误并进行服务端限速/告警抑制，避免客户端重试放大。

## 14. 升级与数据迁移

### 14.1 Schema 迁移

升级事务至少完成：

1. 创建 `deployment_price_versions` bucket 和 Deployment 时间线索引；
2. 枚举全部 Deployment 和仍无 `deployment_id` 的 legacy Route；
3. legacy Route 必须在同一迁移中创建等价 Deployment、保留 Provider/Binding/模型/能力/优先级，并把 Route 原子改为引用新 Deployment；
4. legacy Route 自带价格迁移为新 Deployment 的 `migration` Price Version；共享 Provider 的多个 Route 不得因为名称相同错误合并 Deployment；
5. 如果无法无歧义解析 Provider Binding、Access Surface、模型身份或价格，dry-run 报告硬阻断，禁止 schema commit；
6. 为每个现有 Deployment 检查当前价格字段；
7. 三个价格全部为零时，不猜测免费，标记为待决 `unknown`；
8. 任一价格非零时创建 `migration` 来源的初始 Price Version；
9. 初始版本 `effective_from` 使用迁移提交时间，不伪造 Deployment 创建时间；
10. 将 Deployment 与 Route 价格写入口切换到 Price Version，消除第二套价格权威；
11. 升级 Ledger/Usage/Parquet schema reader，使其同时理解旧事件和新事件；
12. 原子提交 schema version，失败时保持旧状态完整可重试。

迁移 dry-run 必须输出：

- legacy Route 数量和拟创建 Deployment 映射；
- 非零价、零价、歧义和无法迁移资源；
- 每个受影响 Project、Route、Gateway 公开模型及是否会被 price enforcement 阻断；
- 需要管理员明确选择 `free` 或录入 metered 价格的资源；
- 预计 bucket、Ledger reader、checkpoint 和 Parquet schema 变化；
- 所需备份空间、迁移临时空间和预计停机时间。

无人值守升级遇到任何启用 Route 可到达的 unresolved unknown 或 legacy 歧义时默认失败，不得成功提交 schema 后才让生产请求 fail closed。管理员必须在旧版本或迁移 staging 流程中明确解决。Bootstrap 同步增加必选 billing mode：metered 必须提供价格，free 必须显式声明。

离线升级工具必须提供：

```text
heimdall pricing migrate --dry-run --report <path>
heimdall pricing migrate --resolution-file <path> --apply
```

Resolution file 使用版本化 schema，逐 Deployment/legacy Route 声明 `metered`、`free` 或“保持停用”，并带来源证据；文件 digest、操作者和 apply 结果进入 Audit。工具先在 staging copy 完成全部迁移与校验，再原子发布数据目录，不能在 live metadata 上边询问边修改。无人值守 apply 要求报告 digest 与 resolution file 针对同一原始 metadata revision，防止检查后配置漂移。

### 14.2 旧历史数据

升级前 Ledger 事件：

- `committed_micros_usd`、Token 和时间保持不变；
- 不补写 `price_version_id` 或猜测单价；
- 查询层标记 `legacy_unversioned`；
- Dashboard 继续将已有 committed cost 纳入历史合计；
- Usage 明确显示“金额已持久化，但缺少价格版本证据”；
- 既有 WAL 字节、frame CRC、sequence、event ID 和备份不得因展示升级被重写。Ledger 当前不宣称跨记录 hash chain；防篡改 Audit chain 是另一条边界，不能混称。

### 14.3 回滚

写入新 Price Version 或新 Ledger 事件后，不支持旧二进制直接打开数据目录。发布必须：

- 在迁移前创建并验证备份；
- 在隔离目录使用目标版本二进制完成真实 restore drill，而不只执行 archive verify；
- 使用 schema version 阻止旧二进制启动；
- 提供升级 dry-run，列出 unknown Deployment、将创建的初始版本和预计 schema 变化；
- 回滚通过恢复迁移前完整备份完成，禁止部分 bucket 手工降级；
- Operator Guide 明确恢复会丢弃升级后产生的新请求和价格事件。

Ledger 新事件不能只在现有 EventKind 枚举尾部追加后让旧 reader 报 `corrupt`。必须引入 WAL frame/payload schema version 或 feature epoch：

- 新二进制向后读取旧事件；
- 旧二进制遇到新 schema 返回稳定 `ErrUnsupportedVersion`，不能误报损坏并尝试修复；
- backup manifest 记录最小 reader version 和启用的 feature epoch；
- schema gate 在任何新格式 append 前 durable；
- 新 reader 对 unknown future 字段/事件按版本策略 fail closed。

恢复演练至少验证 Master Key、bbolt、Ledger WAL、Audit、checkpoint rebuild、Parquet、Price Version 选择、Token/成本总额和 Gateway readiness，并记录 Backup ID、schema version、二进制版本、操作者、RPO/RTO、磁盘空间和结果。生产切换必须采用原子数据目录发布，禁止在 live 目录上部分回滚。

### 14.4 Backup、Restore 与 Parquet

- Price Version bucket、索引和调整事件必须进入备份；
- 非 quarantine 状态下，restore 后相同可信 `pricing_selected_at` 必须选择相同 Price Version；
- Usage checkpoint version 前进，并可从 Ledger 全量重建；
- Settlement Parquet 新增价格状态、版本 ID、公式和单价分量；Adjustment 使用第 10.4 节独立数据集；
- 旧 Parquet partition 保持只读兼容或通过确定性迁移生成新 partition；
- 导出验证必须对比 Token、原始成本、调整净额和最终成本。

bbolt metadata snapshot 与 Ledger snapshot 必须形成可解释的一致截面。实现可以使用全局 pricing/accounting backup barrier，或持久化 backup epoch/high-watermark；无论选择哪种方式，都必须验证：

- Ledger 引用的 Price Version 存在于 metadata snapshot；或
- 引用明确标记为允许的 orphan，且 PriceSnapshot 自包含、导航不可用不会影响复算；
- 备份期间并发创建价格、取消版本和创建 Lease 不会产生无法恢复的中间状态。

### 14.5 Restore Pricing Quarantine

恢复旧备份后，以下情况必须进入 pricing quarantine，相关 Deployment 在人工复核前 fail closed：

- 备份时 scheduled、恢复时 wall clock 已超过 effective_from；
- 备份后可能发生过取消或后继版本创建；
- 恢复的 `last_observed_effective_from` 晚于或无法与当前可信时间协调；
- Price Version、Ledger pin、Audit intent 或 backup watermark 不一致。

管理员必须查看待生效版本、来源、备份时间和当前 Provider 价格，执行 recent re-auth 后追加 `deployment_price.restore_confirm` 或创建正确后继版本。演练覆盖“创建未来价格 → 备份 → 原环境取消 → 越过生效时间 → 恢复旧备份”，确认旧 scheduled 版本不会自动复活为流量价格。

## 15. 安全与正确性要求

1. Accounting Lease、PriceSnapshot 和 AttemptStarted 必须在 Provider I/O 前 durable。
2. 价格选择使用服务端捕获的 UTC wall clock 与持久化价格高水位，不接受客户端声称的“当前时间”；偏差、回拨和前跳遵循第 6.4 节 fail-closed 规则。
3. Admin 正常接口不允许 backdate，避免事后改变应选价格。
4. 来源 URL 禁止 userinfo、query 和 fragment；path、reference、note、reason 和外部证据 ID 同样执行长度限制、转义和 Secret Scanner。
5. 第一版不主动访问来源 URL，避免新增 SSRF 面。
6. Price Version 金额、currency、formula 和 effective time 一经创建不可修改。
7. 所有金额计算检查乘法、加法和 signed adjustment overflow。
8. 调整后 Attempt 净成本不得为负。
9. bbolt、Ledger、checkpoint、Parquet 和 Dashboard 的汇总必须逐 micro-USD 一致。
10. Price Version 删除不得用于 GDPR/Secret 清除；价格元数据不是 Secret，按财务审计保留策略长期保存。
11. 未知价格不能被序列化为已知 `$0`。
12. 任何 invariant failure 必须停止相关请求并降低 readiness，不能用当前 Deployment 价格兜底。
13. 创建 free 版本、超过阈值调价和所有 Adjustment 要求 recent re-auth；没有四眼审批仍是明确剩余风险。
14. Admin 写入统一执行请求体、频率、版本数量和幂等记录容量限制，防止 bbolt、Ledger 与 Audit 膨胀。
15. Adjustment 的身份、Token、项目、周期和派生成本不能信任客户端字段。
16. Audit 提供篡改证据，不对同时掌握主机 root 和 Master Key 的攻击者提供不可否认性；外部 rollback anchor 仍是独立未来能力。

## 16. 兼容性与边界行为

### 16.1 价格边界时刻

- `pricing_selected_at` 早于新版本生效：使用旧版本，即使响应在生效后返回；
- `pricing_selected_at` 等于新版本 `effective_from`：使用新版本；
- 第一次 Attempt 使用旧版本，生效后发生 retry：retry 可以使用新版本；
- fallback 到另一个 Deployment：使用目标 Deployment 自己的当前版本。

### 16.2 Deployment 生命周期

- Deployment 停用不删除价格历史；
- Deployment 删除采用现有软删除语义，价格版本永久可供历史查询；
- 有 active Route 的 Deployment 价格变更不要求重新执行 Provider 连接测试，因为价格不改变上游目标；
- 价格未知且成本治理要求已知时，连接测试仍可执行，但真实 Gateway 请求 fail closed；
- Provider、Binding 或模型目标变化时应创建新的 Deployment，不能让旧价格时间线跨越不同计费对象。

### 16.3 明确免费模型

免费版本必须由管理员显式选择 `billing_mode=free`，记录来源和生效时间。后续从免费变为付费时创建 metered 后继版本。历史免费 Attempt 显示 `$0` 和 `FREE`，与 unknown 明确区分。

## 17. 测试计划

### 17.1 领域与存储测试

- 版本号单调、相同 effective time 冲突；
- metered/free/unknown 验证规则；
- immutable 字段不能更新；
- scheduled 取消与已引用拒绝；
- bbolt migration 原子性和重复执行幂等；
- 并发创建价格版本只有一个成功；
- Deployment 软删除后历史价格仍可读取。
- legacy Route 有价、零价、共享 Provider、多 Binding、歧义阻断和迁移回滚；
- bbolt timeline key 使用 UTC nanos 稳定排序，version++ 与 unique effective time 同事务；
- metered/free/unknown Accounting Lease tagged validation 矩阵。

### 17.2 价格选择测试

- 生效前一纳秒、生效时刻和后一纳秒；
- 多 scheduled 版本；
- cancelled 版本被跳过；
- Runtime 热加载和进程重启选择一致；
- restore 后选择一致；
- UTC/DST/usage timezone 不影响价格时间线。
- `pricing_selected_at` 与稍后 `occurred_at` 跨越生效边界；
- wall clock 前跳、后跳、NTP 失效、重启和高水位不回退；
- scheduled 取消与 Lease pin 并发的所有排序；
- restore pricing quarantine 和人工确认。

### 17.3 Gateway 与预算测试

- Provider I/O 前已持久化 PriceSnapshot；
- 非流式、流式、Responses、Anthropic Messages 和 Embeddings 使用同一规则；
- Provider Usage、无 Usage、ambiguous failure 和客户端断开；
- retry/fallback 跨价格版本；
- 价格上涨后新 Attempt 预算拒绝，旧 Attempt 继续结算；
- unknown/free 行为；
- Token Guard 请求级价格视图与 Attempt 级重新检查；
- input/output 分量分别向上取整；
- 最大 int64 边界和 overflow fail closed；
- 中间乘积超出 int64 但最终除法结果可表示；
- 当前期 Adjustment 造成 over-budget 后阻断新 reservation，负向调整释放额度。

### 17.4 Ledger 与恢复测试

- Reservation 和 Settlement 快照完全一致；
- 每个 Provider I/O 前后的确定性 kill point；
- 崩溃后使用原 PriceSnapshot 保守结算；
- 未 Started Lease 释放、已 Started Lease 保守结算、恢复期间二次崩溃；
- 确定性 recovery event ID 和 pending 元数据完整性；
- Ledger replay、checkpoint rebuild 和 Parquet compaction；
- 旧 Ledger 事件读取和 legacy 标签；
- WAL sequence/CRC、Audit chain、备份验证和恢复回归；
- 新 WAL schema 被旧 reader 识别为 unsupported 而非 corrupt；
- bbolt/WAL/Audit mutation intent 的全部双写 kill point；
- 并发创建/取消价格、请求和备份的一致截面。

### 17.5 调整测试

- 正负 delta、累计不能低于零；
- 幂等键重复提交；
- same idempotency key + different payload fail closed；
- 并发 Adjustment sequence/If-Match 冲突；
- reprice CorrectionPriceSnapshot 离线复算和 explicit_delta 证据验证；
- 当前周期预算同步；
- 历史周期只更新历史报表；
- service_period_restated 与 adjustment_posted 两种口径；
- 独立 Adjustment Parquet watermark、join/reduce、已导出 Settlement 后再调整；
- checkpoint 丢弃重建和完整调整链离线重算；
- 原始 Settlement 永不改变。

### 17.6 Admin Console 测试

- 价格历史、scheduled、free、unknown、legacy 展示；
- 十进制输入精度和本地化；
- 来源 URL 安全验证；
- source assurance 与 asserted/verified 展示；
- 生效不可修改提示；
- Usage 公式展开与调整展示；
- Dashboard unknown 不显示为零；
- recent re-auth、调整 preview、额度阈值和 over-budget/quarantine 状态；
- 中英文、键盘操作、窄屏和可访问性。

## 18. 验收标准

功能只有在以下条件全部满足时才算完成：

1. 同一 Deployment 至少三个历史/当前/未来价格版本可以稳定查询，旧版本不可修改。
2. 在价格生效边界前后发起的 Attempt 分别绑定正确版本。
3. 长请求跨越生效时刻后仍使用 `pricing_selected_at` 与 Accounting Lease 绑定的版本结算。
4. Retry 和 fallback 可以在同一客户端请求中产生不同价格版本的独立成本。
5. Ledger 单条 Attempt 可仅使用自身 Token、PriceSnapshot 和公式离线重算，结果逐 micro-USD 相等。
6. Dashboard、Usage、Ledger replay、checkpoint 和 Parquet 的成本总额完全一致。
7. unknown、free、legacy_unversioned 和 estimated 在 API/UI 中可明确区分。
8. 旧版本升级后历史 Token 与 committed cost 零变化，既有 Ledger WAL 字节/sequence/CRC 和 Audit chain 不被重写。
9. 成本调整只追加事件，原始 Settlement 保持不变，并可导出完整调整链。
10. 在所有 Provider I/O 前，Accounting Lease、PriceSnapshot 与 AttemptStarted 已经 durable。
11. 每个关键 kill point 恢复后没有重复计费、价格漂移、负余额或无法释放的 Accounting Lease。
12. Backup/restore、Usage rebuild、Master Key rotation、Admin Audit 和 Metrics 完成回归。
13. 全量 Go test、Race、Vet、Web typecheck/test/build 和文档链接检查通过。
14. Operator Guide、User Guide、Threat Model、Implementation Status、Metrics Reference 和 Release Notes 同步更新。
15. metered、free 和显式允许的 unknown Attempt 都能在 Provider I/O 前创建语义正确的 durable Accounting Lease，且 unknown 从未被解释为零成本。
16. pending Lease 在每个 kill point 后可幂等恢复：未 Started 释放，已 Started 保守结算，无永久预算占用。
17. 迟到历史调价使用自包含 CorrectionPriceSnapshot；无需向实时价格时间线 backdate 即可离线复算完整调整链。
18. 服务期重述和入账期调整两种报表口径都有明确等式，Ledger、checkpoint、两个 Parquet 数据集与 API 逐 micro-USD 一致。
19. 全部 legacy Route 在迁移中转换为 Deployment 或成为 schema commit 硬阻断；零价启用资源未决时无人值守升级失败而不是上线后断流。
20. 时钟前跳、回拨、旧备份恢复和 scheduled 取消竞态不会让已观察价格时间线倒退或复活已取消价格。
21. 生产升级前完成隔离 restore drill，并验证 pricing quarantine、WAL reader gate 和 Ledger/Audit intent 恢复。

## 19. 建议实施顺序

### Phase 1：价格领域模型与迁移

- `DeploymentPriceVersion`、状态与验证；
- bbolt bucket、canonical timeline 索引、legacy Route migration 和 pre-upgrade readiness gate；
- Admin 查询/创建/取消 API；
- Deployment 价格写入口拆分；
- Audit 和单元测试。

### Phase 2：Gateway 绑定与 Ledger 快照

- `pricing_selected_at`、时钟高水位和版本选择器；
- metered/free/unknown Accounting Lease、PriceSnapshot 和 durable pin；
- Provider I/O 前持久化 Lease 与 AttemptStarted；
- pending Lease 崩溃恢复状态机；
- Token Guard Attempt 级价格重检；
- Settlement 公式分量；
- Ledger schema、replay、kill-point 和恢复测试。

### Phase 3：Usage、Dashboard 与 Console

- checkpoint/Parquet schema；
- unknown/free/legacy/estimated 查询语义；
- Deployment 价格时间线 UI；
- Usage 成本证据与 Dashboard 汇总；
- Metrics、告警和可访问性测试。

### Phase 4：追加式调整与运维闭环

- `EventCostAdjusted`；
- CorrectionPriceSnapshot、并发 sequence 和双时间轴；
- 单 Attempt 调整 API/UI；
- 独立 Adjustment Parquet、导出、对账和 Ledger/Audit intent；
- 备份恢复与操作 Runbook；
- 生产升级演练。

### Phase 5：价格建议与自动化（后续独立验收）

- 官方来源 Adapter 或受控导入；
- LLM 只做结构化 Proposal；
- 人工确认和变化提醒；
- 来源内容 digest 与漂移检测；
- 不得阻塞前四个 Phase 的正确性上线。

## 20. 实现前需形成的 ADR

以下四份前置 ADR 已于 2026-08-04 接受，构成实现与验收的约束：

1. [ADR 0011：Accounting Lease 与崩溃恢复](adr/0011-accounting-lease-crash-recovery.md)：metered/free/unknown tagged lease、PriceSnapshot 编码、AttemptStarted I/O 边界、pending recovery、deterministic event ID 和 Token Guard 重检。
2. [ADR 0012：价格选择与跨存储一致性](adr/0012-pricing-selection-cross-store-consistency.md)：`pricing_selected_at`、时钟高水位、bbolt timeline、durable pin intent、scheduled 取消、Ledger/Audit mutation intent 和锁顺序。
3. [ADR 0013：Adjustment 会计与读模型](adr/0013-adjustment-accounting-read-models.md)：CorrectionPriceSnapshot、signed delta、sequence/idempotency、服务期/入账期双时间轴、Budget Balance、checkpoint 和独立 Adjustment Parquet。
4. [ADR 0014：WAL/备份兼容](adr/0014-ledger-wal-backup-compatibility.md)：frame/payload schema version、旧 reader `ErrUnsupportedVersion`、bbolt/WAL 一致截面、离线 backup barrier、restore pricing quarantine 和 orphan reference 规则。

以下演进项不阻塞 Phase 1 开工，但必须在对应功能合入前作出版本化决定：

5. Admin API 旧价格字段的兼容窗口长度和删除版本。
6. 未设置 Daily Budget 的 Project 遇到 unknown 价格时，实例级策略是否允许显式 opt-in 放行；默认必须拒绝。
7. 未来缓存 Token、Batch 和服务等级价格应扩展 `formula_version` 还是引入通用 line-item 模型。

## 21. 最终决策

本升级采用以下不可退让的产品结论：

1. 价格变化创建不可变版本，不覆盖历史条款。
2. 每次 Provider Attempt 独立选择并持久化价格快照。
3. 价格快照必须在 Provider I/O 前随 typed Accounting Lease 和 AttemptStarted durable。
4. 最终成本按 Attempt 写入 Ledger，Dashboard 只做聚合，不使用当前价格重算历史。
5. 未知价格、明确免费、估算成本和旧版未版本化历史必须分别表达。
6. 历史错价通过追加调整事件纠正，禁止改写原始结算。
7. 自动化和 LLM 只能提出价格建议，未经可信来源和管理员确认不得进入生产价格时间线。
8. metered、free 和 unknown_allowed 都必须使用显式 tagged Accounting Lease；免费零值与未知 null 永不混用。
9. `pricing_selected_at` 是价格边界权威，时钟高水位禁止时间线倒退；异常恢复进入 pricing quarantine。
10. 历史纠价使用不参与实时选择的 CorrectionPriceSnapshot，并同时保留服务期和入账期。
11. Adjustment 使用独立 append-only Parquet 数据集；已导出的 Settlement 永不覆盖。
12. legacy Route 和零价启用资源必须在 schema commit 前解决，禁止升级后才暴露业务中断。
13. 价格、Ledger、Audit 和备份跨存储操作必须有 durable intent/barrier 与确定性崩溃恢复协议。
