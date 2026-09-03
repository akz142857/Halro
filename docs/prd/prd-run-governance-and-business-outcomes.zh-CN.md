# PRD：Run 治理、业务结果关联与单位结果成本

- 状态：**Proposed / 未实现**
- 日期：2026-09-04
- 代码基线：`381743f6613607dc256828f4776b52af8bdd232c`
- 当前格式基线：metadata schema v35、Ledger authenticated epoch v4、Usage checkpoint v12、Usage export schema v5、backup manifest v2
- 目标版本：待进入实现时基于最新 `main` 顺延；本文中的“下一版本”不得在未核对当前常量时直接写死
- 适用范围：Project、Gateway Key、Gateway HTTP、Budget/Ledger、Usage 派生读模型、Admin API、Admin Console、备份恢复与导出
- 相关调研：[AgentPlane、FinOps 与 TokenOps](/Users/ziy/Code/ClayCosmos/Halro/docs/reference-analysis/agentplane-finops-tokenops.zh-CN.md)、[业务结果与扩展适配分析](/Users/ziy/Code/ClayCosmos/Halro/docs/reference-analysis/halro-outcomes-and-run-governance-assessment.zh-CN.md)
- 评审记录：[Run Governance PRD 多角色评审报告](/Users/ziy/Code/ClayCosmos/Halro/docs/reviews/run-governance-prd-multi-role-review.zh-CN.md)

## 一、结论与交付边界

Halro 增加三个可组合能力：

1. **Work Unit（业务工作单位）**：一个最终只应计算一次业务结果的对象，例如一张工单、一次代码修复或一份文档处理任务；
2. **Run（任务执行）**：完成 Work Unit 的一次执行，可跨多个 Agent、进程、Gateway Key、模型请求、重试和 fallback，但第一版必须属于一个 Project；
3. **Outcome（业务结果声明）**：受信调用方依据一个版本化指标定义，对 Work Unit 上报的结构化结果。

Halro 据此提供：

- Request / Attempt → Run → Work Unit 的费用归因；
- 单个 Run 的累计费用上限，与 Project 日预算同时、原子地执行；
- Outcome 的受控上报、修订、覆盖率及基础单位结果成本；
- 供外部数据平台进一步分析的明细导出。

Halro **接收和保存结果声明，不执行任意业务验收**。业务系统、CI、人工质检或独立评价器负责判定。第一版不保存完整聊天、模型输出、代码、测试日志或工具输出，只保存有限的结构化结论与证据摘要。

这个边界允许用户只部署 Halro 完成基础成本治理，也允许成熟用户把同一数据导入自己的评测与 FinOps 系统。它不把 Agent 编排、记忆、工具执行或通用实验平台引入网关。

## 二、问题与用户价值

### 2.1 当前可以回答的问题

Halro 已能回答：

- 某个 Project 今天用了多少预算；
- 一个 Gateway Request 产生了哪些 Provider Attempts；
- 每个 Attempt 的 Provider、Deployment、Token、价格快照、成本、重试/fallback 和不确定性；
- 请求为什么被预算、并发或 Token Guard 拒绝。

当前不能原生回答：

- 一个跨十次模型请求的 Agent 任务累计花了多少；
- 多个进程或下游 Agent 是否在共享同一个任务额度；
- 第一次失败、第二次重跑成功时，完成这一个业务结果总共花了多少；
- 降低平均请求成本后，业务验收率是否下降；
- 有多少任务至今没有任何可信结果，报表覆盖率是多少。

### 2.2 目标用户

- 使用 Halro 统一承载 Agent 或复合 AI 应用模型流量的平台团队；
- 需要限制单个自动化任务失控费用的 Project 管理员；
- 需要比较模型、提示词或流程方案单位结果成本的产品与 FinOps 人员；
- 需要解释预算拒绝、重试费用和结果来源的审计与运维人员。

### 2.3 核心用户故事

1. 应用为一件业务工作创建 Work Unit，并为每次执行创建 Run。
2. 分布在多个进程的 Agent 使用同一个 `run_id` 调用 Halro，费用进入同一个 Run。
3. Run 的全部并发调用共享一笔额度，任何一次新调用都不能只看调用后的累计费用。
4. 一个 Run 失败后，应用可在同一 Work Unit 下创建新 Run；最终只计算一个业务成功单位，费用包含两次 Run。
5. 验收器可延迟上报结果，也可用新事件修订原结果；历史费用不被改写。
6. 管理员能看到结果覆盖率、预算内验收率和每个通过结果的模型费用，并能下钻到 Run、Request 和 Attempt。
7. 未使用这些能力的现有客户端继续按当前协议和性能工作。

## 三、术语和身份关系

| 术语 | 含义 | 身份来源 |
| --- | --- | --- |
| Project | 预算、模型权限、Gateway Key 和安全策略的责任边界 | 管理员配置；Gateway Key 绑定 |
| Work Unit | 只计算一次最终业务结果的工作对象 | Halro 生成 `wku_...` |
| Run | 一次执行过程，可含多个请求和 Agent | Halro 生成 `run_...` |
| Request | 一次进入 Halro 的模型请求 | Halro 生成 `req_...` |
| Attempt | Request 对某个 Provider/Deployment 的一次实际尝试 | Halro 生成 `att_...` |
| Outcome Definition | 规定结果名称、值域、成功值和计量单位的不可变版本 | 管理员创建 `odef_...` |
| Outcome Event | 某上报者对 Work Unit 作出的一次结果声明或修订 | Halro 生成 `out_...`，来源 Key 由鉴权确定 |

关系约束：

```text
Project 1 ── N Work Units
Project 1 ── N Runs
Work Unit 1 ── N Runs
Run 1 ── N Requests
Request 1 ── N Attempts
Work Unit 1 ── N Outcome Definitions ── N revision events
```

- 第一版 Run 与 Work Unit 不得跨 Project；
- 一个 Request 最多属于一个 Run，一个 Run 最多属于一个 Work Unit；
- Run 可以不关联 Work Unit，用于只需要任务预算的场景；
- Work Unit 创建时声明预期 Outcome Definitions，未声明的结果不能后来写入；
- Work Unit ID、Run ID 和 Outcome ID 都是关联标识，不是凭据；权限始终来自 Gateway Key 与 Project。

## 四、目标与非目标

### 4.1 目标

1. 可信、可追溯地传播 Run 与 Work Unit 归属，不影响 OpenAI/Anthropic 请求体兼容性；
2. 在 Provider I/O 前同时执行 Project 日预算和 Run 全程预算；
3. 在并发、断流、进程崩溃和未知 Provider 结果下维持保守、不超发的准入语义；
4. Outcome 支持延迟、重复、乱序和显式修订，不把缺失结果解释为失败或成功；
5. 成本报表区分已知、估算和未知费用，显示结果覆盖率；
6. 所有权威事件可验证、可恢复，派生查询可从对应的受认证日志重建；
7. 旧客户端不传 Run ID 时，其行为与当前版本一致；
8. 数据基数、请求体、生命周期与权限都有显式上限。

### 4.2 非目标

- 通用 Agent 编排、A2A 调度、任务队列或 Agent 停止协议；
- 记忆、RAG、工具执行、完整 trace、提示词或模型输出存储；
- 在 Halro 内运行任意 CI、业务规则、LLM-as-a-Judge 或人工标注系统；
- 根据 Outcome 自动切换模型、自动追加预算或动态修改生产策略；
- 工具、沙箱、GPU、人工、采购、税务、发票和收入的完整 TCO/ROI；
- 跨 Project 或跨 Halro 实例的一笔共享 Run 预算；
- 用现有每日 Usage 边际 rollup 直接承载高基数 Run 明细；
- 以“Agent 自称完成”作为默认业务验收。

## 五、不可破坏的不变量

1. **Project 仍是上级责任边界。**Run 额度只能收紧单个任务，不能扩张 Project 日预算、模型权限、RPM、TPM、并发、CIDR、Token Guard 或脱敏权限。
2. **一份 Provider 消费只结算一次。**Run 与 Work Unit 是既有 Attempt 成本的归属索引，不建立第二份费用账本。
3. **预算准入必须包含在途预留。**在同一 Project 临界区内检查：

   ```text
   project_committed + project_reserved + project_pending + new_reservation <= project_daily_cap
   run_committed     + run_reserved     + run_pending     + new_reservation <= run_lifetime_cap
   ```

4. **Reservation 在 Provider I/O 前持久。**失败写入或无法确认 Run 权威状态时，带 Run 的调用不得到达 Provider。
5. **未知执行结果保守结算。**已开始且崩溃的 Attempt 继承当前恢复规则，同时计入 Project 与 Run；不得因 Run 关闭、过期或进程重启静默退款。
6. **Outcome 不改写费用。**结果修订只改变结果派生读模型；它不能修改 Attempt、释放预算或改变 Provider 是否被调用过的事实。
7. **缺失不是零。**无 Outcome、未知成本和无验收定义均返回显式 partial/unknown 状态，不显示为零成本或失败。
8. **业务结果故障不影响普通推理。**Outcome 写入和业务报表不可用时，只影响该控制面请求；未携带 Run 的模型调用保持现有路径。
9. **派生状态永远不是预算权威。**Usage checkpoint、Run 报表和 Outcome rollup 可删除重建，不能改变 Ledger 余额。
10. **原始业务内容不进入新增存储。**Evidence 只允许摘要与受限引用；Halro 不根据引用抓取网络内容。
11. **业务结果写入不得毒化计费状态机。**Outcome 使用独立的受认证 Governance Journal；其损坏、应用失败或写入拥塞不能让 accounting Ledger 停止接受普通推理的预算事件。

## 六、关键设计裁决

### D1 · Work Unit 与 Run 均由 Halro 生成

采用服务端生成 ID。业务系统在自己一侧保存外部业务 ID → Work Unit ID 的映射。

原因：调用方自造任意高基数字符串会带来碰撞、跨租户枚举、PII、长度和存储放大风险；服务端 ID 也能明确“创建一笔新 Run”是一项有权限、受限流的动作。

### D2 · Run 第一版只能属于一个 Project

同一 Project 的多个 Gateway Key、Agent 和进程可以使用一个 Run；跨 Project 任务在上层拆成多个 Run，外部分析系统可再汇总。

原因：现有预算权威、锁和账期都以 Project 为边界。跨 Project 原子事务会引入锁排序、部分失败与权限合并问题，第一版没有足够收益支撑这项复杂度。

### D3 · Run 额度固定且跨账期累计

Run 创建后预算上限不可由普通应用修改。管理员如确有必要，可在后续阶段通过带 revision、Step-up 和 Audit 的操作追加额度；第一版续跑通过在同一 Work Unit 下创建新 Run 完成。

Run 的额度不随 Project 账期切换重置。每个 Request 仍只计入其受理时确定的 Project 日账期。

这里的“额度上限”约束的是 Halro 基于冻结价格和 prepared token bounds 所做的 Provider 调用准入。若 Provider 最终报告的用量超过准备上界、价格配置遗漏了供应商的额外收费项，或供应商账单口径与 Halro 不一致，实际外部账单仍可能高于 Run cap。Halro 必须记录实际或保守结算值、将 Run 标为耗尽并停止后续调用，界面和文档不得把它表述为供应商发票的绝对硬上限。

### D4 · Run 过期只阻止新调用

到达 `expires_at` 后，新的附带调用返回 `run_not_active`。已准入的 Attempt 继续正常结算。过期不退款、不删除历史、不改变 Outcome 上报权限。

### D5 · Halro 只能停止后续模型调用

Run 用尽或关闭后，Halro 拒绝后续附带该 Run 的模型调用。它不杀死 Agent 进程、不撤销工具副作用，也不承诺取消已经被 Provider 接受的调用。应用通过明确错误码决定结束、降级或创建新 Run。

### D6 · Outcome Definition 由管理员版本化

每个定义包括：Project、稳定名称、版本、数据类型、允许值、哪些值算成功、单位及说明。第一版只支持 `BOOLEAN` 与受限 `CATEGORICAL`；不支持自由文本结果和任意浮点分数。

定义一旦被 Work Unit 使用便不可修改；变化创建新版本。这样两个时期的“通过”不会因后台改配置而被悄悄混算。

### D7 · Outcome 是外部声明，不是 Halro 判决

Outcome Event 保存鉴权得出的 `reporter_key_id`、definition、value、ingested_at、调用方提供的 observed_at、证据摘要和被修订事件。界面措辞使用“上报结果”“判定来源”，不显示成“Halro 验证通过”。

### D8 · Outcome 修订采用追加事件

第一次上报不带 `supersedes_outcome_id`。修订必须引用当前最新事件；引用旧版本返回 `409 outcome_revision_conflict`。同一 Idempotency-Key 与同一规范请求重放返回原结果；相同 Key 对应不同请求返回冲突。

报表使用 `(work_unit_id, definition_id)` 当前最新的有效事件。历史事件保留，用于解释指标为什么变化。

### D9 · 计费事实与业务声明使用两个受认证日志

Work Unit、Run 生命周期以及 Attempt 的 `run_id` / `work_unit_id` 进入现有 accounting Ledger，因为它们直接参与准入、费用归属和崩溃恢复，必须和 Reservation/Settlement 共享顺序与应用事务。

Outcome Event 进入独立的 authenticated Governance Journal。它只对“哪个受信 Key 在何时上报了什么结果”负责，不保存、修改或结算费用。这样 Outcome 写入洪峰、格式错误或 journal apply failure 不会毒化现有 `budget.Manager` 的终止性 accounting apply 状态。

Accounting Ledger 仍是费用唯一权威，Governance Journal 是结果声明唯一权威；二者通过服务端 Work Unit ID 和一对 watermark 在查询时连接，不复制可加总成本。bbolt 只保存 Outcome Definition、Project/Key 配置及可从两个日志重建的幂等索引和 checkpoint。两个日志都必须进入备份、完整性验证和导出，但故障域、writer 和 apply 状态相互独立。

### D10 · 不把 Run 塞进现有每日边际 rollup

当前 rollup 每个维度最多保留 200 个键，并且没有交叉项。Run/Work Unit 需要专用明细索引和低基数结果 rollup：按 Work Unit 创建账期、Project、Definition 版本和结果值汇总，不以每个 Run ID 作为聚合键。

### D11 · 单位结果指标按 Work Unit cohort 计算

查询明确使用 `basis=work_unit_cohort`：选择某段时间内创建的 Work Units，读取当前已上报结果，并汇总这些 Work Units 下所有 Runs 的已结算模型成本。返回 `generated_at` 与 Ledger watermark。

这与“某日发生的模型费用”是两个不同报表。Outcome 晚到或修订会重述旧 cohort 的结果指标；界面必须显示“截至当前”的含义。

### D12 · 基础分析可以内置，复杂分析使用导出

内置报表提供覆盖率、成功率、预算内成功率、已知/估算成本、未知 Attempt 数和每个成功 Work Unit 的模型费用。多维任意切片、全 TCO、收入、人工成本和长期快照交给外部系统。

### D13 · 普通推理默认不启用新增语义

Project 默认 `run_governance_enabled=false`；所有旧 Gateway Keys 解码为 `inference` scope。请求没有 `X-Halro-Run-ID` 时，不产生 Work Unit/Run 状态读取，也不增加新的持久写入。

### D14 · 第一版不做 Outcome 驱动的实时策略

Outcome 通常迟到，且质量由外部定义。自动追加/减少预算、改变路由或切换模型需要独立离线效果评估、误判分析和操作授权，不与本 PRD 一起上线。

## 七、领域模型

以下为逻辑字段，不要求最终 Go 类型逐字一致。

### 7.1 Project 增量配置

```go
type RunGovernanceConfig struct {
    Enabled                    bool
    DefaultRunBudgetMicrosUSD  int64
    MaxRunBudgetMicrosUSD      int64
    DefaultRunTTLSeconds       int64
    MaxRunTTLSeconds           int64
    MaxActiveRuns              int64
    MaxOpenWorkUnits           int64
}
```

规则：

- `Enabled=false` 时其他字段必须为零；
- 启用时默认预算必须大于零且不超过最大预算；
- 所有以美元表示的 Run 预算要求调用目标具有有效价格；
- Project 日预算为零表示没有日金额上限，但 Run 上限仍正常生效；
- S0 冻结限制：默认 TTL 24 小时、最大 TTL 30 天、每 Project 最多 1,000 个 active Runs 和 1,000 个 open Work Units。依据与证据见 [S0 容量与契约验证](../verification/performance/2026-09-04-run-governance-s0/README.md)；这些是首版 hard limits，不是生产吞吐承诺。

### 7.2 Gateway Key scopes

```text
inference
work_unit:create
run:create
run:attach
governance:read
outcome:write
```

- 旧记录没有 scopes 时严格解释为 `[inference]`；
- 新 Key 默认仍只有 `inference`；管理员显式选择其他 scopes；
- `run:attach` 允许该 Key 把模型调用计入同 Project 的 Run；知道 Run ID 本身不授权；
- 创建 Run 的 Key 不自动拥有 `outcome:write`；
- 管理员 API 保持独立 Session/CSRF/Step-up 体系，不复用 Gateway Key scope。

### 7.3 Outcome Definition

```json
{
  "id": "odef_...",
  "project_id": "project_support",
  "name": "resolved_without_handoff",
  "version": 1,
  "data_type": "CATEGORICAL",
  "allowed_values": ["accepted", "rejected", "unknown"],
  "success_values": ["accepted"],
  "unit": "ticket",
  "description": "工单在观察窗口内未重开且无人工接管",
  "enabled": true,
  "revision": 1
}
```

约束建议：每 Project 最多 64 个 active Definitions；名称最多 64 个 ASCII 小写字符；枚举 2–16 项，每项最多 32 字符；描述最多 256 个 Unicode 字符。description 只显示给管理员，不进入模型上下文。

Definition 的 `enabled=false` 只阻止新的 Work Unit 引用。已经声明该不可变版本的 Work Unit 仍可上报、修订和查询 Outcome，否则管理员禁用定义会让在途业务单位永远无法完成验收。

### 7.4 Work Unit

```json
{
  "id": "wku_...",
  "project_id": "project_support",
  "outcome_definition_ids": ["odef_..."],
  "status": "open",
  "created_by_key_id": "gk_...",
  "created_at": "2026-09-04T08:00:00Z",
  "period_id": "2026-09-04",
  "period_timezone_version": 3
}
```

第一版状态为 `open` / `closed`。关闭阻止创建新 Run，不阻止已存在 Run 结算或 Outcome 延迟上报。只有 `closed` 且所有关联 Runs 均无 pending/inflight Attempt 时，Work Unit 才是报表中的 `matured`。每个 Work Unit 最多声明 8 个 Definitions、关联 32 个 Runs。关闭是幂等操作。

### 7.5 Run

```json
{
  "id": "run_...",
  "project_id": "project_support",
  "work_unit_id": "wku_...",
  "budget_micros_usd": 2000000,
  "committed_micros_usd": 680000,
  "reserved_micros_usd": 120000,
  "unknown_attempts": 0,
  "status": "active",
  "created_by_key_id": "gk_...",
  "created_at": "2026-09-04T08:00:03Z",
  "expires_at": "2026-09-05T08:00:03Z",
  "closed_at": null,
  "close_reason": null
}
```

Run 的生命周期状态由权威事件和时间推导：

```text
active ── close ──▶ closed
active ── expires_at reached ──▶ expired
```

预算不足不是稳定生命周期状态：某个昂贵请求无法准入时，更便宜的请求仍可能准入；已预留 Attempt 以低于 reservation 的金额结算后，可用额度也会回升。因此第一版只返回实时 `budget_state=available|fully_reserved|depleted`、余额和该次 `run_budget_exceeded`，不写 `RunExhausted` 事件，也不把一次拒绝永久转换为非 active。`depleted` 仅表示 `committed + reserved >= cap` 的当前派生结果。第一版不提供应用侧 resume 或修改预算。

### 7.6 Outcome Event

```json
{
  "id": "out_...",
  "work_unit_id": "wku_...",
  "definition_id": "odef_...",
  "definition_version": 1,
  "value": "accepted",
  "reporter_key_id": "gk_...",
  "evidence_sha256": "<64 lowercase hex>",
  "evidence_ref": "support_validation_973",
  "observed_at": "2026-09-04T08:15:00Z",
  "ingested_at": "2026-09-04T08:15:03Z",
  "supersedes_outcome_id": null,
  "revision": 1
}
```

- `observed_at` 允许合理时钟偏差和晚到，不用于决定修订顺序；Governance Journal sequence 决定顺序；
- `evidence_ref` 是不超过 128 字符的不可执行引用，拒绝 URL、控制字符、换行和疑似 Secret；Halro 不访问它；
- `evidence_sha256` 可省略；提供时必须是调用方对外部证据规范字节计算的 SHA-256；
- 不接收自由文本 comment、评价 reasoning 或原始产物；
- Work Unit 仍为 open 时允许先写 Outcome，但查询将其标为 `provisional`；Work Unit matured 后同一 current head 才参与成功率和单位结果成本；
- 同一 Work Unit/Definition 最多保留 20 次修订，超过后拒绝；Work Unit close 后最多继续上报或修订 30 天，随后只读历史。两项均为 S0 冻结 hard limits。

## 八、HTTP 契约

### 8.1 公共控制面路由

新增独立于 OpenAI/Anthropic 兼容面的路由：

| 方法 | 路径 | Scope | 用途 |
| --- | --- | --- | --- |
| POST | `/halro/v1/work-units` | `work_unit:create` | 创建 Work Unit |
| GET | `/halro/v1/work-units/{id}` | `governance:read` | 查询 Work Unit、费用和当前 Outcome |
| POST | `/halro/v1/work-units/{id}/close` | `work_unit:create` | 阻止新 Run |
| POST | `/halro/v1/runs` | `run:create` | 创建 Run |
| GET | `/halro/v1/runs/{id}` | `governance:read` | 查询余额、状态与请求汇总 |
| POST | `/halro/v1/runs/{id}/close` | `run:create` | 阻止新调用 |
| POST | `/halro/v1/work-units/{id}/outcomes` | `outcome:write` | 上报或修订 Outcome |

所有 POST：

- Bearer Gateway Key 必须启用、未过期、Project 可用且来源 IP 符合 Project CIDR；
- 强制 `Idempotency-Key`，长度、字符集、摘要与请求 fingerprint 沿用已有受控异步资源规则；其作用域固定为 `(Project, operation, key_hash)`；
- `Content-Type: application/json`，请求体 hard max 16 KiB；未知字段拒绝；
- 返回 `Cache-Control: no-store` 和 `X-Request-ID`；
- 成功创建返回 201；同一幂等请求重放返回同一对象和 200；
- 控制面设置独立写桶：每 Key 120 RPM、每 Project 1,000 RPM；读桶为每 Key 600 RPM、每 Project 5,000 RPM，Summary 另限每 Project 60 RPM。管理员可下调，不能配置超过 hard max；不能靠无限创建 Run 消耗本地存储或绕开 Run cap。

创建/上报事件必须携带 operation、Idempotency-Key hash 和规范请求 fingerprint。bbolt 只保存可重建查找索引；索引丢失后必须从相应受认证日志恢复，不能因为索引缺失而把重试当成新创建。索引至少与其权威事件保留同样长，首版不做会改变重复提交语义的 TTL 淘汰。

创建 Work Unit：

```http
POST /halro/v1/work-units
Authorization: Bearer gw_...
Idempotency-Key: ticket-4821-create
Content-Type: application/json

{
  "outcome_definition_ids": ["odef_..."]
}
```

创建 Run：

```http
POST /halro/v1/runs
Authorization: Bearer gw_...
Idempotency-Key: ticket-4821-attempt-1
Content-Type: application/json

{
  "work_unit_id": "wku_...",
  "budget_micros_usd": 2000000,
  "ttl_seconds": 86400
}
```

调用模型：

```http
POST /v1/chat/completions
Authorization: Bearer gw_...
X-Halro-Run-ID: run_...
Content-Type: application/json

{ "model": "chat", "messages": [{"role": "user", "content": "..."}] }
```

`X-Halro-Run-ID` 适用于所有由 Halro 计费的兼容面，包括非流式、流式、原生 Messages、Deferred Responses 及实验性资源端点。每条协议路径都必须通过同一个请求封装入口传播，禁止逐 Handler 零散实现。

延迟执行必须冻结归属而不能只依赖提交请求的瞬时 Header：

- Deferred Response 提交时验证并把 RunID/WorkUnitID 写入 `ProviderResource`；worker 真正开始 Provider Attempt 时重新验证 Project、scope、Run active/TTL，并在当时执行双预算准入；队列接收成功不承诺未来一定有预算；
- queued Deferred 被取消或过期且尚未开始时不产生 Run reservation；worker 因 Run 关闭、过期或预算不足而不能执行时，资源进入明确 terminal failure 并保存受控错误码；
- Provider async invoke 在提交动作本身就会触达 Provider，因此在提交前完成 reservation；后续 poll/delete 继承资源归属但不重复计费；
- 任何持久资源恢复后都从资源记录恢复 RunID，不能从新的请求 Header 猜测或丢掉归属。

上报 Outcome：

```http
POST /halro/v1/work-units/wku_.../outcomes
Authorization: Bearer gw_...
Idempotency-Key: support-validation-973
Content-Type: application/json

{
  "definition_id": "odef_...",
  "value": "accepted",
  "observed_at": "2026-09-04T08:15:00Z",
  "evidence_ref": "support_validation_973",
  "evidence_sha256": "<64 lowercase hex>",
  "supersedes_outcome_id": null
}
```

### 8.2 Admin 路由

| 方法 | 路径 | 约束 |
| --- | --- | --- |
| GET/POST | `/admin/api/v1/projects/{id}/outcome-definitions` | 创建需 `requireAdminMutation`、If-Match Project revision、Audit intent |
| POST | `/admin/api/v1/projects/{id}/outcome-definitions/{definitionID}/versions` | 创建下一不可变版本；旧版保留 |
| PUT | `/admin/api/v1/projects/{id}/run-governance` | Project revision + Audit；关闭前检查 active Runs |
| GET | `/admin/api/v1/governance/runs` | 分页、单筛选或专用组合索引 |
| GET | `/admin/api/v1/governance/work-units` | 分页、结果状态及覆盖率 |
| GET | `/admin/api/v1/governance/outcomes` | 查看来源与修订链 |
| GET | `/admin/api/v1/governance/summary` | cohort 基础指标 |

删除已使用 Definition 不允许物理删除，只能禁用新 Work Unit 引用。关闭 Run Governance 时，如果存在 active Runs，返回 409 并列出计数；管理员可选择先等其完成或逐个关闭，不能让配置消失后调用默认放行。

### 8.3 主要错误码

| HTTP | code | 语义 |
| ---: | --- | --- |
| 400 | `invalid_run_id` | Run ID 格式非法 |
| 400 | `invalid_outcome` | Definition、值域、证据字段或 observed_at 非法 |
| 401 | `invalid_api_key` | Gateway Key 缺失或无效 |
| 403 | `gateway_key_scope_denied` | Key 缺少所需 scope |
| 403 | `run_budget_exceeded` | 新 Attempt 的保守预留不能进入 Run 剩余额度 |
| 403 | `budget_exceeded` | 现有 Project 日预算拒绝 |
| 404 | `run_not_found` / `work_unit_not_found` | 资源不存在或不属于当前 Project；两者不区分以防枚举 |
| 409 | `run_not_active` | Run 已关闭或过期 |
| 409 | `work_unit_closed` | 关闭的 Work Unit 不能创建新 Run |
| 409 | `outcome_revision_conflict` | 修订引用的不是最新 Outcome |
| 409 | `price_unavailable` | 有金额预算但目标没有有效价格 |
| 409 | `idempotency_conflict` | 同一 Idempotency-Key 对应不同请求 |
| 429 | `governance_rate_limited` | 控制面创建或上报过快 |
| 503 | `run_governance_unavailable` | 带 Run 的调用无法读取/持久化权威状态；Provider 未被调用 |

当 Project 与 Run 都会拒绝时，固定先检查 Project 的权限/静态限制，再检查 Run 状态，再进入目标选择和价格预估，最后在一次 Project 临界区完成双预算准入。错误优先级需要在契约测试中冻结，避免通过错误码泄漏跨 Project 资源状态。

## 九、Budget、Accounting Ledger 与 Governance Journal

### 9.1 事件归属

Accounting Ledger 新增：

```text
WorkUnitCreated
WorkUnitClosed
RunCreated
RunClosed
```

已有 `RequestAccepted`、`ReservationCreated`、`AttemptStarted`、`AttemptSettled`、`RequestFinalized` 增加可选 `run_id` 与 `work_unit_id`。一旦 Request 受理，两个值固定并由下游事件继承；不能在 fallback 或重试过程中改变。

Governance Journal 新增 `OutcomeReported`。事件包含独立 sequence、server ingested time、WorkUnitID、Definition version、reporter Key、规范值、evidence 摘要、被修订事件和 Idempotency-Key hash/fingerprint。它不包含 Attempt 金额，也不提供余额 API。

`ledger.Event.Validate` 从当前通用必填校验调整为按 EventKind 的穷尽 switch：

- Request/Attempt 事件继续要求 RequestID、ProjectID 和账期；
- Run/Work Unit 事件要求各自的 server ID、ProjectID、OccurredAt 和事件特有字段；Governance Journal 对 Outcome 使用独立的穷尽校验；
- `WorkUnitCreated` 使用服务器当前时间调用现有账期计算逻辑，冻结 `period_id` 与时区版本；客户端不能提交或覆盖 cohort 账期；
- Accounting 治理事件的 `OccurredAt` 与 Outcome 的 `ingested_at` 均由服务器写入；Outcome 的 `observed_at` 只是受限业务观察时间，不能替代日志顺序或账期；
- 任何字段组合不符合 kind 均拒绝，避免一个半初始化事件在回放时被不同版本解释成两种状态；
- EventKind 只能追加，旧编号永不复用。

### 9.2 双预算准入

`budget.Manager` 的 Project lock 保持唯一准入临界区。新增：

```go
type RunBalance struct {
    ReservedMicrosUSD  int64
    CommittedMicrosUSD int64
    UnknownAttempts    int64
}

type RunAdmission struct {
    PendingMicrosUSD int64
}
```

实际实现应把 Run pending 放在现有 `projectAdmission` 的同一把 mutex 下，例如 `runPending map[runID]int64`；不能再为每个 Run 建一套独立锁后尝试锁排序。这样 Project 与 Run 判断、两个 pending 增减构成一次临界区，WAL fsync 仍在锁外。

流程：

```text
解析并鉴权 Run，确认 Project/状态/TTL/scope
→ 选择目标并冻结价格快照，计算保守 reservation
→ 获取 Project lock
→ 检查 Project Balance + project pending + reservation
→ 检查 Run Balance + run pending + reservation
→ 两个 pending 同时增加
→ 释放锁
→ 追加 ReservationCreated（含 run_id）并 fsync/apply
→ 获取锁，同时释放两个 pending
→ Provider I/O
```

任一检查失败时两个 pending 都不增加。Ledger append 失败时两个 pending 都回滚，Provider 不被调用。Apply 后到 pending 释放前可能双重计数，只允许保守拒绝，不允许出现两处都未计数的窗口。

项目日预算、Run 预算和 Token Guard 的成本判断必须使用同一已选目标、同一价格快照和同一 prepared token bounds；不允许三处分别查询可能变化的价格。

Accounting apply 还必须拒绝以下因果破坏：引用不存在或不同 Project 的 Run；带 WorkUnitID 却没有 RunID 的 Attempt；Settlement 改变 Reservation 冻结的 RunID/WorkUnitID；RunCreated 引用不存在或已关闭的 Work Unit；同一个 Request 的不同 Attempts 改变 Run 归属。Run 生命周期余额与 Project balance 在同一 `State.Apply(record)` 临界区更新，不能先更新一边再让 observer 补另一边。

### 9.3 结算与恢复

`AttemptSettled` 一次原子应用同时完成：

- Project reservation 释放与 committed 增加；
- Run reservation 释放与 committed 增加；
- Provider Token、成本、Attempt 状态和估算标记记录；
- Usage/Run observers 获得同一 Ledger sequence。

这里的 Attempt 状态描述供应商调用是否成功、失败或结果不明；它不是第七节定义的业务 Outcome。二者不得复用同一字段、枚举或 UI 文案。

启动时 `RecoverPendingLeases` 从 Reservation 事件恢复 RunID：

- 未写 `AttemptStarted`：Project 与 Run reservation 同时释放；
- 已开始：用冻结价格和 prepared bounds 同时保守结算到 Project 与 Run；
- Run 已过期或 Work Unit 已关闭也不能改变恢复结果；
- Run 元数据缺失、Project 不匹配或余额应用失败时启动失败，不允许丢掉 Run 维度继续提供服务。

### 9.4 生命周期并发

- `RunClosed` 与新 reservation 使用同一个 Project lock；close 先应用则后续 reservation 拒绝，reservation 先准入则允许该 Attempt 完成；
- `WorkUnitClosed` 与 `RunCreated` 同样序列化；关闭前已经存在的 Run 不被强制关闭；
- Outcome 上报不获取预算准入锁；同一个 Work Unit/Definition 的并发修订在 Governance Journal writer 前使用短临界区，只有一个引用当前 head 的事件成功；
- 时钟只用于 TTL 派生；各日志内部由自己的 sequence 决定因果顺序，跨日志查询只使用已记录的一对 watermarks，不虚构全局原子顺序。

## 十、存储、格式升级与兼容

### 10.1 metadata

实现时将当前 metadata schema v35 顺延到下一可用版本，创建并登记：

- Outcome Definitions bucket；
- 可从受认证事件重建的 Governance idempotency index bucket；
- Outcome/Run 专用 checkpoint 或索引 bucket；
- 所需的按 Project、状态和时间索引。

GatewayKey scopes 新字段通过 migration 显式回填 `[inference]`，不能仅依赖 nil 的运行时特殊解释；迁移须有 before/after kill-point tests，并加入 `requiredBuckets()`。

### 10.2 Accounting Ledger 与 Governance Journal

新增 EventKind 与受认证字段需要新的 Ledger feature epoch。新 reader 接受 v4 → 新 epoch 的单向转换，并在第一次新 epoch 后拒绝回到旧 epoch。旧 reader 遇到新 epoch 必须拒绝打开，不能跳过未知事件。

Governance Journal 使用独立 magic/version、chain key derivation domain、sequence 和 watermark；不得复用 accounting frame 的裸字节定义后靠 EventKind 猜日志类型。Outcome 修订 head 与幂等索引从 Governance Journal 重建，索引损坏不能改写历史。

升级说明必须写明：metadata migration、第一次写入新 Ledger epoch或创建 Governance Journal 后，旧二进制不能直接打开数据目录；回退只能恢复升级前备份。

### 10.3 Usage checkpoint 与读模型

当前 Usage checkpoint v12 需要顺延，Attempt/Request 投影增加 RunID/WorkUnitID，并新增可分段的 WorkUnit、Run、Outcome current-head 投影。恢复时：

- 版本不匹配丢弃整个派生 checkpoint，从 Accounting Ledger 与 Governance Journal 各自重建后再连接；
- checkpoint watermark 不得领先 Ledger；
- 新 Outcome rollup 与明细 head 必须在同一个 bbolt transaction 前进；
- 增量处理与全量重放必须逐字段相等；
- 删除派生 bucket 后重启得到相同的 Run 余额、Work Unit 成本和 Outcome 指标。

### 10.4 Usage export

现有 export schema v5 顺延，Attempt row 增加 nullable `run_id` / `work_unit_id`；新增独立的 `work_units`、`runs`、`outcomes` 和 `outcome_definitions` 数据集，或提供一个规范化 NDJSON governance export。导出 manifest 同时记录 accounting 与 governance watermarks。最终选择必须满足：

- 旧分区不改写；manifest 对每个文件记录 schema；
- attempt 成本只存在一份，其他数据集引用而不复制为可加总金额；
- checksum、sequence range、record count 和完整性验证覆盖新文件；
- 未知与估算状态在导出中保留；
- evidence_ref 不扩展为证据内容。

### 10.5 Backup / Restore

backup manifest 顺延并记录 metadata schema、Ledger feature epoch、Governance Journal version/head、Outcome/Run checkpoint 版本及新导出 manifest。Restore 必须在 staging 目录完成：

1. 解密并验证所有文件；
2. 打开并迁移 metadata；
3. 分别验证 Accounting Ledger chain、Governance Journal chain 与新事件；
4. 验证或丢弃派生 checkpoint；
5. 独立全量回放后核对 Project/Run 余额、Outcome heads 及两个 watermark 的连接状态；
6. 确认 restored active Runs、Work Units 和 scoped Gateway Keys；
7. 原子切换数据目录。

任何一步失败均不得部分启用恢复目录。

### 10.6 数据保留与删除

第一版不提供按 Run、Work Unit 或 Outcome 物理删除权威事件的接口。Accounting Ledger 与 Governance Journal 生命周期内分别保留费用/归属事实和 Outcome 修订链；关闭资源只改变可执行状态，不代表删除。这样才能保持链校验、费用重放和历史指标可解释。

派生索引、checkpoint 和查询缓存可以丢弃并从对应受认证日志重建；它们的查询保留窗口不得被描述为权威数据已经删除。若部署方需要缩短保存期，必须先通过容量测量确定数据目录生命周期，并使用已有的备份、归档或整目录轮换机制处理。

未来引入租户级保留或隐私删除前，必须另行设计 Ledger 可验证裁剪、导出证明、Outcome 证据最小化和跨数据集一致删除。本 PRD 不以直接删除 bbolt bucket 或跳过 Ledger 事件作为实现捷径。

## 十一、查询、指标与口径

### 11.1 Work Unit 直接模型成本

```text
work_unit_model_cost = sum(all settled Attempt cost where Attempt.run belongs to Work Unit)
```

- 包含失败 Run、重试、fallback 和已开始后恢复的保守结算；
- `CostMicrosUSD` 已包含估算成本，`EstimatedCostMicrosUSD` 是其子集，不得相加两遍；
- 未知成本 Attempt 单独计数；只要未知数大于零，返回 `cost_completeness=partial`；
- 未经过 Halro 或没有 Run 归属的费用不进入 Work Unit 数字；
- 工具、沙箱、人工和其他系统成本不进入本指标，UI 名称固定为“模型调用成本”。

### 11.2 结果指标

对于选定 Definition/version 和 Work Unit cohort：

```text
eligible_units       = cohort 中声明该 Definition 的 Work Units
matured_units        = 已关闭且所有 Runs 无 pending/inflight Attempt 的 eligible_units
evaluated_units      = 有当前有效 Outcome 的 matured_units
successful_units     = 当前 Outcome value 属于 success_values 的 evaluated_units
outcome_coverage     = evaluated_units / eligible_units
success_rate         = successful_units / evaluated_units
success_without_run_budget_rejection = successful_units 中从未出现 run-budget rejection 的单位
cost_per_success     = matured_units 全部相关模型成本 / successful_units
```

- 分母为零时返回 `null` 与 reason，不返回 0；
- `cost_per_success` 分子包括成熟但未成功或没有有效 Outcome 的 Work Units 已发生费用；仍在执行的 Work Units 成本单列为 `in_progress_cost`，避免尚未完成的 cohort 提前扭曲单位结果成本；
- success_rate 必须和 coverage 同屏，防止只上报成功样本；
- 同一 Work Unit 多次 Run 最终只计一个 successful unit；
- Work Unit 关闭后已有 Run 可以结算，直到其没有 pending/inflight Attempt 才进入 matured；关闭前上报的 Outcome 显示为 provisional，不进入 evaluated/successful；
- Definition 版本不同默认不合并；管理员可以显式选择多个版本，但 API 返回分版本行；
- 本指标不命名为 ROI、节省金额或总成本。

### 11.3 Summary API 返回形状

```json
{
  "basis": "work_unit_cohort",
  "cohort_start": "2026-09-01",
  "cohort_end": "2026-09-30",
  "definition_id": "odef_...",
  "definition_version": 1,
  "generated_at": "2026-10-07T08:00:00Z",
  "accounting_watermark": {"sequence": 123456, "offset": 9876543},
  "governance_watermark": {"sequence": 725, "offset": 44120},
  "eligible_units": 100,
  "matured_units": 92,
  "evaluated_units": 90,
  "successful_units": 72,
  "outcome_coverage": 0.9,
  "success_rate": 0.8,
  "known_cost_micros_usd": 14000000,
  "in_progress_cost_micros_usd": 800000,
  "estimated_cost_micros_usd": 500000,
  "unknown_attempts": 2,
  "cost_completeness": "partial",
  "cost_per_success_micros_usd": 194444
}
```

金额除法使用 checked integer arithmetic 和明确舍入规则；内部 micros 不能先转 float。`generated_at` 和两份 watermark 说明晚到或修订 Outcome 会改变同一 cohort 的当前值，也允许外部导出判断两次结果使用了哪些输入快照。

### 11.4 明细与分页

- Run、Work Unit、Outcome 列表使用稳定复合游标，不使用 offset；
- 默认与最大 page size 沿用现有 Usage 的 50/100 风格；
- 允许一个组合查询必须有对应的持久索引；没有索引的任意多条件查询返回 400，而非全表扫描；
- ID 精确查找必须验证 Project 后统一返回 404；
- 单 Work Unit 下钻最大展示 32 Runs，单 Run Request/Attempt 继续使用分页；
- 报表明确标注账期时区与 Work Unit cohort 口径。

## 十二、Admin Console

### 12.1 导航

不新增顶级侧栏项目。在“用量与调用”中增加：

```text
汇总 | 任务与结果 | 调用明细
```

原因：三者依次回答“总体花了多少”“哪些任务产生结果”“具体哪次调用发生了什么”，并能保持侧栏数量稳定。

### 12.2 Project 配置

Project 页面新增折叠区“任务费用治理”：

- 默认关闭；
- 默认/最大 Run 预算；
- 默认/最大 TTL；
- active Run 与 open Work Unit 上限；
- Outcome Definitions 列表及新版本创建；
- 明确提示启用后新的 scoped Key 才能创建/附加 Run 或上报结果。

关闭治理前展示 active 资源计数与影响。不能用一个确认框隐式关闭所有 Runs。

### 12.3 Gateway Key

创建/编辑 Key 时展示 scopes。`inference` 默认选中；其他权限逐项说明。Key 值仍只显示一次，列表绝不返回 hash。旧 Key 在 UI 明确显示“仅推理”。

### 12.4 任务与结果页

顶部卡片：

- Work Units；
- matured / in-progress Work Units；
- Outcome 覆盖率；
- 验收成功率；
- 模型调用成本；
- 每个成功 Work Unit 的模型调用成本；
- Run 预算拒绝数；
- 未知成本 Attempts。

表格按 Work Unit 展示状态、Definitions、当前 Outcome、Run 数、Request/Attempt 数、已知/估算成本和未知数。抽屉中展示：

```text
Work Unit
  ├─ Outcome revision chain（上报者 Key、值、时间、证据摘要）
  └─ Runs
       └─ Requests
            └─ Provider Attempts
```

UI 禁止：

- 将 `outcome=accepted` 渲染成“Halro 验证通过”；
- 将无 Outcome 显示成 0 分或失败；
- 在 unknown cost > 0 时显示一个无说明的精确总额；
- 将 Outcome 上报时间当作模型费用发生时间；
- 将 open Work Unit 的 provisional Outcome 混入正式成功率或单位结果成本；
- 提供自由文本 evidence 输入。

响应式布局需在桌面与窄屏验证；高密度层级默认只展开一层，避免把完整树一次渲染。

## 十三、安全与滥用分析

| 威胁 | 控制 |
| --- | --- |
| 不断创建新 Run 绕开单 Run cap | Project 日预算始终生效；创建需 scope、速率与 active 上限；报表统计 budget rejection |
| 把别的 Project Run ID 填入请求 | 鉴权后核对 `run.ProjectID == principal.Project.ID`，不匹配统一 404 |
| 未授权 Key 消耗别人的 Run | 模型调用同时要求 `inference` 与 `run:attach` |
| 伪造成功结果 | `outcome:write` 独立 scope；服务端记录 reporter Key；Definition 限定值域；UI 展示来源 |
| 只上报成功样本 | 报表同时显示 eligible、evaluated 与 coverage |
| Outcome 重放或乱序覆盖 | Idempotency-Key + request fingerprint；修订必须引用当前 head |
| ID/evidence 注入 PII、Secret 或 URL | server IDs；evidence 字段长度/字符集/secret scan；不允许 URL、不主动访问 |
| 高基数与存储耗尽 | Project active 上限、Definitions/Run/revision 数量上限、控制面限流、请求体上限 |
| Metrics 标签爆炸 | Prometheus 只用 outcome/status/reason 等有限枚举，不暴露 Run/Work Unit ID 标签 |
| 通过过期时钟绕过 | `expires_at` 由服务器根据受限 TTL 计算；客户端 observed_at 不参与 Run 状态 |
| 结果系统故障拖垮推理 | 独立 Handler、限流和写路径；无 Run 推理不依赖 Outcome store |
| Outcome 内容进入模型 | 数据结构与推理请求完全分离；禁止任何隐式上下文注入 |

Project 禁用或 Gateway Key 撤销后：已有 Run 保留历史但该 Key 不能继续附加/查询/上报；其他仍有效、具相应 scope 的同 Project Key按正常权限继续。

## 十四、故障语义

| 故障 | 模型调用 | 控制面请求 | 记录语义 |
| --- | --- | --- | --- |
| 请求不带 Run | 保持当前行为 | 不适用 | 只记 Project/Request/Attempt |
| 带 Run但治理状态无法读取 | Provider 前 503 | 不适用 | 不创建 reservation |
| Run reservation WAL 写失败 | Provider 前 503 | 不适用 | pending 回滚，无费用事实 |
| Provider 结果不明 | 沿用保守响应 | 不适用 | Project 与 Run 同时保守结算 |
| Outcome 写失败 | 不影响其他推理 | 503，可同幂等键重试 | 不推进 Outcome head |
| Outcome 派生 checkpoint 损坏 | 普通推理继续；治理投影独立重建 | Outcome 查询返回 503，治理子系统状态为 not ready | accounting readiness 与预算权威不从 Outcome 状态读取 |
| Summary 超时 | 推理不受影响 | 503，不返回部分总额 | 指标不伪装完整 |
| Run 过期时仍有 Attempt | Attempt 可完成 | 查询显示 expired + inflight | 结算后余额继续更新 |
| 磁盘满 | 依现有 Ledger/metadata fail-closed | 创建/上报失败 | 不产生未持久的成功响应 |

## 十五、指标、日志与告警

### 15.1 Prometheus

建议新增有限基数指标：

```text
halro_governance_runs_created_total{outcome}
halro_governance_runs_active
halro_governance_work_units_open
halro_governance_outcomes_reported_total{value_class,outcome}
halro_governance_run_rejections_total{reason}
halro_governance_control_requests_total{operation,outcome}
halro_governance_control_latency_seconds{operation}
halro_governance_checkpoint_sequence
halro_governance_rebuild_total{outcome}
halro_governance_rebuild_duration_seconds
```

不得用 ProjectID、RunID、WorkUnitID、KeyID、DefinitionID 或 evidence_ref 作为 Prometheus label。按 Project 的分析通过 Admin API 完成。

### 15.2 日志

结构化日志可带本次请求的 RunID/WorkUnitID，但必须经过安全标识校验；不记录 Outcome evidence_ref、外部业务 ID、Gateway Key、提示词或模型输出。拒绝日志包含 reason、request_id、project_id 和受控资源 ID。

### 15.3 告警

- Run budget rejections 在短窗口内突增；
- active Runs / open Work Units 接近配置上限；
- Outcome ingest 连续失败；
- governance checkpoint 长时间落后 Ledger head；
- Outcome coverage 长期低于阈值只作为产品数据质量信号，不进入 Halro readiness。

## 十六、性能预算

S0 必须测出基线后再冻结门槛，至少包括：

| 路径 | 基线/对照 | 要证明的性质 |
| --- | --- | --- |
| 无 Run 的普通请求 | 当前 HEAD | 未启用功能时无额外持久写，延迟/吞吐无实质回退 |
| 同 Project、同 Run 并发准入 | 1/8/64 workers | 严格预算，无 over-admission；锁不跨 WAL fsync |
| 同 Project、多 Runs | 1/8/64 workers | 不产生全局 Run 锁瓶颈或数据 race |
| 创建/关闭/过期并发 | 与模型请求交错 | 事件因果和状态可解释 |
| Outcome 上报 | 多 Work Units 与同一 revision head 竞争 | 幂等、冲突、受控吞吐 |
| 启动恢复 | 10k/100k/1m Runs/Outcomes 候选集 | 明确内存、重放时间和可接受容量 |
| Summary | 最大内置查询范围 | 不扫描整个 Ledger，不返回无界 payload |

如果专用 Run/Outcome 状态导致无 Run 热路径读取新锁或写入新状态，设计退回修改，不以“开销很小”接受未经测量的全局影响。

## 十七、实施阶段与文件边界

### S0 · 契约、威胁模型与容量 spike

交付：

- 将本文所有 MUST/不得转换为契约测试清单；
- 建立双预算 admission prototype 与 benchmark；
- 测量 Accounting Ledger State、Governance Journal、Usage Aggregate、checkpoint 和 Admin 查询的基数成本；
- 裁决候选上限、Outcome export 形状和 checkpoint 分段策略；
- 更新 threat model 与 data-flow diagram；
- 不修改生产格式、不新增用户可见开关。

完成门：无 Run 热路径基线已记录；并发预算无法 over-admit；1m 候选恢复数据给出可接受/不可接受结论；未决项有明确裁决。

### S1 · Work Unit / Run 归因与只读成本（内部里程碑）

交付：

- metadata migration、Gateway Key scopes、Project 开关；
- Ledger 新 epoch 与 WorkUnit/Run lifecycle；
- `/halro/v1/work-units`、`/runs`、`X-Halro-Run-ID`；
- Request/Attempt 明细及 export 的 Run/Work Unit 字段；
- Admin 只读下钻；
- 尚不启用 Run 金额拒绝和 Outcome。

完成门：所有计费协议面归因一致；旧客户端行为不变；跨 Project/scope 绕过失败；备份、恢复、重建和旧 reader 拒绝行为通过。S1 可以供内部测试和数据副本验证，不作为面向应用团队的长期独立版本发布。

### S2 · Run 预算

交付：

- Project Run governance 配置；
- Project + Run 双预算原子准入；
- close/expiry 生命周期与实时 budget state；
- 恢复、错误码、指标和 Admin 配置 UI。

完成门：并发/崩溃/跨日矩阵通过；Run 预算拒绝发生在 Provider I/O 前；Project 既有限制全部保持优先或同时生效；affected budget package 通过 race。S1 与 S2 作为首个面向用户的 Run Governance 发布单元一同交付，避免长期暴露“能归因但不能治理”的中间产品。

### S3 · Outcome 接收与基础指标

交付：

- Outcome Definition Admin；
- scoped Outcome API、revision/idempotency；
- Outcome Governance Journal events、checkpoint、低基数 rollup；
- cohort summary、coverage、success、cost-per-success；
- governance export。

完成门：迟到/重复/修订/缺失/未知成本矩阵通过；增量与全量重建等价；结果系统不可用不影响推理。

### S4 · Console 与操作交接

交付：

- “任务与结果”标签、Project/Key 配置、下钻与 partial 状态；
- 中英文文案、用户指南、操作指南、OpenAPI/示例；
- upgrade/rollback、backup/restore、容量与数据保留说明；
- 数据副本上的真实历史回放验证。

完成门：桌面和窄屏完整路径通过；用户能从单位成本定位到 Outcome → Run → Request → Attempt；所有外部验收仍被标为上报来源而非 Halro 判决。

### S5 · 后续评估项，不在首轮承诺

- 管理员追加 Run 预算与 resume；
- 跨 Project 或跨 Halro 集群额度；
- 工具成本 ingestion；
- FOCUS/OTel 映射增强；
- 离线 Outcome 驱动的预算/模型建议；
- 外部评价器插件或 webhook。

## 十八、建议代码落点

| 区域 | 预计变更 |
| --- | --- |
| `internal/domain/` | Project 配置、Gateway Key scopes、Outcome Definition 输入与校验 |
| `internal/ledger/` | 新 EventKinds/fields、epoch、Validate/Apply/回放状态、chain tests |
| `internal/governance/` | 独立 Outcome Journal、修订 head、幂等重建、chain/checkpoint tests |
| `internal/budget/` | Run balance、pending admission、双预算、恢复与并发测试 |
| `internal/gateway/` | 统一 request envelope 接收 Run context，所有协议共用 |
| `internal/gatewayapi/` | `/halro/v1` handlers、header/JSON/错误契约 |
| `internal/auth/` | scope 写入 snapshot 与无锁鉴权 |
| `internal/store/bolt/` | migration、Definitions、idempotency、checkpoint/index |
| `internal/usage/` | Run/WorkUnit/Outcome 投影、cohort rollup、查询、export |
| `internal/app/` | Admin routes、Runtime wiring、backup/restore、metrics |
| `web/src/` | Project、Key、Usage tabs、详情抽屉、i18n 与 API types |
| `docs/` | ADR、协议、用户/运维指南、迁移和发布说明 |

S0 结束后至少补充两份 ADR：

1. `Run budget authority and dual admission`：冻结 Project/Run 两级准入、恢复、关闭竞态和 Ledger 事件；
2. `Business outcome evidence and cohort reporting`：冻结 Outcome 信任边界、修订语义、报表 cohort 与 partial 状态。

## 十九、测试矩阵

### 19.1 Domain / HTTP

- 各 ID、scope、TTL、预算、Definition、枚举和 evidence 边界值；
- JSON unknown fields、重复字段、null/零值、超大 body、错误 content type；
- Idempotency-Key 同请求重放、不同请求冲突、重启后重放；
- 删除 idempotency index 后从对应日志重建，原请求仍返回同一资源或 Outcome；
- 旧 Key migration 后只有 inference；新 scope 不进入 Admin JSON hash；
- 资源存在/不存在/跨 Project 的响应不可枚举；
- OpenAI、Anthropic、Gemini/Bedrock 等现有面均传播同一 Run context；
- 不传 Run header 的 golden contract 不变。

### 19.2 Run 状态全矩阵

| 状态 | 新调用 | 已准入结算 | 新 Run | Outcome |
| --- | --- | --- | --- | --- |
| active Work Unit + active Run | 允许（预算允许时） | 正常 | 允许（上限内） | 允许 |
| Run active、该请求预算不足 | 本次拒绝；更便宜请求可再尝试 | 正常 | 同 Work Unit 可新建 | 允许；关闭前为 provisional |
| Run closed | 拒绝 | 正常 | 同 Work Unit 可新建 | 允许 |
| Run expired | 拒绝 | 正常 | 同 Work Unit 可新建 | 允许 |
| Work Unit closed | 现有 active Run 依约继续 | 正常 | 拒绝 | 允许迟到/修订 |
| Project disabled | 拒绝 | 已开始按现有规则结算 | 拒绝 | 拒绝新上报 |

### 19.3 Budget / concurrency

- Project 足、Run 不足；Project 不足、Run 足；两者刚好相等；
- 64 并发在同 Run 临界余额，成功 reservations 总额不超过 cap；
- 两个 Runs 同 Project 不互相花掉 Run cap，但共同受 Project cap；
- Ledger append 前/后、Apply 前/后、pending 释放前 kill points；
- 关闭/过期与 reservation 同时发生；
- streaming 客户端断开、Provider 超时报错、usage 缺失、Provider 报告超出 prepared bound；
- fallback 多 Attempts 与 ambiguous outcome；
- 午夜前受理、午夜后结算；Run 跨多个账期；时区版本切换；
- `go test -race` 仅对受影响的 budget/ledger/gateway package 运行。

### 19.4 Outcome

- 第一次上报、相同事件重放、并发修订只有一个成功；
- observed_at 乱序不改变 Governance Journal sequence head；
- accepted → rejected → accepted 修订链和 rollup delta；
- 未声明/禁用/错误版本 Definition；
- 无 Outcome、unknown value、结果覆盖率不足；
- Work Unit 多 Runs、失败重跑成功只算一个结果；
- open Work Unit 的 Outcome 为 provisional；关闭但仍有 inflight Attempt 时不 mature；最终结算后才进入正式指标；
- Outcome 写失败、checkpoint 损坏与全量重建；
- evidence_ref 的 URL、Secret、控制字符和长度拒绝；
- reporter Key 禁用后的历史来源仍可解释。

### 19.5 报表、导出与恢复

- cost 总额与 Attempt 明细精确相等，estimated 是子集；
- unknown cost 保持 partial；分母为零返回 null；
- cohort 与 expense period 的跨日反例；
- Outcome 晚到后旧 cohort 指标更新且 generated_at/watermark 变化；
- accounting/governance 各自的 increment、checkpoint restore、full replay 逐字段等价，并能在指定双 watermark 下连接；
- NDJSON/Parquet（按最终裁决）schema、manifest、checksum 与旧分区混合读取；
- 新备份 round-trip；缺少任一日志时拒绝治理恢复；升级前备份回退；旧 reader 对新 schema/epoch 明确拒绝；
- 截断、篡改、缺失新数据集和 checkpoint ahead-of-ledger 均失败关闭。

### 19.6 Frontend

- Key scope 与 Project 配置行为测试；
- Summary partial/unknown/null/zero 的不同渲染；
- Outcome 来源措辞、revision chain 与下钻链接；
- 筛选、URL 恢复、分页、窄屏层级和无障碍；
- TypeScript 类型检查、直接受影响测试、生产 build；
- `web/` 改动后重建并核对 `internal/webui/dist`，生成包与 source 同一提交。

## 二十、完整验收标准

功能验收：

- [ ] 一件 Work Unit 的两次 Runs、多个 Requests 与 fallback Attempts 能完整归因且不重复加总；
- [ ] 同 Run 的跨进程并发调用共享严格额度，临界并发不 over-admit；
- [ ] Project 日预算和所有既有限制仍生效，换 Run ID 不能绕过；
- [ ] close、expiry、崩溃恢复和跨日的余额与错误码符合契约；
- [ ] Outcome 可迟到、幂等和修订，来源与 definition version 可追踪；
- [ ] Outcome Journal 写入、重建或完整性故障不会毒化 accounting apply，也不会让无 Run 普通推理停止；
- [ ] 报表同时展示 coverage、success rate、known/estimated/unknown 和 cost per success；
- [ ] 无 Outcome/未知成本/零成功分母均显式 partial/null；
- [ ] 可以从 Work Unit 下钻到 Run、Request 和 Attempt。

安全与可靠性验收：

- [ ] Gateway Key scopes、Project ownership、CIDR、禁用/过期全部覆盖新路由；
- [ ] 新路径不存储原始业务产物、提示词、结果或 Gateway Key；
- [ ] Run 状态不可用时带 Run 请求在 Provider I/O 前失败；
- [ ] Outcome/报表故障不影响无 Run 普通推理；
- [ ] Ledger、Audit、checkpoint、export、backup/restore 的格式升级和回退说明完整；
- [ ] 破坏派生状态后可从对应日志等价重建；Accounting Ledger 损坏时推理 readiness 失败，Governance Journal 损坏时治理子系统失败但普通推理保持可用；
- [ ] Prometheus 无高基数业务 ID 标签。

交付验收：

- [ ] 直接受影响 Go tests 使用 `-count=1`，并发修改通过受影响 package 的 `-race`；
- [ ] Frontend typecheck、tests、production build 通过；
- [ ] 最终 push 前只运行一次完整 Go 与 Frontend gate，并核对 embedded bundle 无漂移；
- [ ] 没有未经用户明确要求的真实 Provider 付费 smoke；
- [ ] 用户指南提供最小 curl/SDK 例子、错误处理和多 Agent 传播说明；
- [ ] 运维指南说明容量、保留、备份、恢复、升级和不可直接降级边界；
- [ ] 实现状态文档逐阶段标明已实现、部分实现与外部验收项。

## 二十一、上线与回滚

1. 发布前先备份并验证；记录 metadata schema、Ledger head/epoch、Audit head 与 Usage checkpoint。
2. 升级二进制后先保持所有 Project 的 Run Governance 关闭，验证旧流量、Usage 与备份。
3. 在隔离 Project 创建新的 scoped Key，小流量启用 S1 归因；核对 Attempt 总额前后相等。
4. 启用 Run budget，观察拒绝、unknown/estimated、checkpoint lag 和恢复演练。
5. 接入一个 Outcome Definition，先验证 coverage 与 cohort 口径，再开放更多场景。
6. 格式升级后的直接二进制回滚不支持；恢复升级前备份才是回滚路径。业务开关可以关闭新创建，但不能删除已经写入的历史事件。

若出现下列任一条件，停止扩大范围：

- 无 Run 热路径出现可测的持续回退；
- 同 Run 并发存在 over-admission；
- 任何协议面丢失 Run 归因或绕过 Project 限制；
- Outcome/报表故障影响普通推理 readiness；
- checkpoint/full replay 不等价；
- 数据量使启动恢复或备份超过 S0 确定的运行边界；
- 结果覆盖率不足以支持所展示的单位结果指标。

## 二十二、S0 裁决

2026-09-04 的 S0 已冻结以下实现输入，详细测量、限制和证据边界见
[S0 容量与契约验证](../verification/performance/2026-09-04-run-governance-s0/README.md)：

1. 每 Project 1,000 active Runs、1,000 open Work Units；默认/最大 TTL 24 小时/30 天；每个 Outcome head 最多 20 revisions；
2. 控制面写入 Key/Project 为 120/1,000 RPM，读取为 600/5,000 RPM，Summary 为 60 RPM/Project；JSON body hard max 16 KiB；
3. Accounting 沿用 4 MiB immutable-segment Usage checkpoint 并增加 nullable 归属字段；Governance 使用独立的 4 MiB segmented checkpoint；两个 checkpoint 不共享 transaction；
4. Governance export 使用规范化的 `work_units`、`runs`、`outcomes`、`outcome_definitions` 四个 NDJSON 数据集，统一 manifest 记录双 watermarks；
5. 内置 Unit Summary 最多 90 天且最多 100,000 Work Units，服务端延迟门槛 2 秒；超限使用 export；
6. Work Unit close 后 Outcome 上报/修订窗口为 30 天，之后只读；
7. Governance Journal 采用独立 domain-derived chain key、4 MiB segments；换 key 时开启新 generation 并把上代 terminal digest 写入新 header；acknowledged local writes 的目标 RPO 为 0，1m events 冷恢复门槛为 30 秒；任意 header/chain/revision 失败使 Governance not ready，但不影响 Accounting readiness；
8. 首个真实业务试点不是持久格式输入。S1/S2 可按冻结契约实施；进入 S3 前必须由业务负责人提供 Work Unit、Definition、验收方、观察窗口和决策用途，并在数据副本上验收。没有试点不得发布 Outcome 产品能力。

本次 S0 只增加 test-only probes 和文档，没有修改生产格式或增加用户可见开关。
