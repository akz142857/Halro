# 分时价位（按日时段折扣）— 独立评审提案

状态：**已实施**（2026-08-18，采纳方案 B）。落地记录见
[ADR 0023](../adr/0023-time-of-day-pricing.md)，本文保留为评审过程与议题清单。
建立日期：2026-08-18
最后核对：2026-08-18 —— §2 的「已经有的」、§5.4 的定论、§6 的影响面清单
均已对照 main 上的真实代码核实（行号即当时的 main）

实施时对 §5 的开放议题给出的答案，逐条见 [ADR 0023](../adr/0023-time-of-day-pricing.md)：

| §5 议题 | 结论 |
|---|---|
| 1 快照记录档位、预留时定档 | 照做。`PriceSnapshot.schedule_tier` 记录档位，四项费率本身即该档；定档在 `NewVersionedPriceSnapshot` 一处发生 |
| 2 时区权威属供应商 | `PriceSchedule.timezone` 存 IANA 名，与会计时区共用校验规则但互不为默认值 |
| 3 判不出档位取贵档 | 逐项取 base 与所有时段的最大值，保证任意 token 组合下都不少记；`halro doctor` 增加 `pricing_schedule_zone` 检查 |
| 4 规则挂在价格版本内 | 照做（本就由代码定死） |
| 5 持久结构变更范围 | 三个结构各加一个可空字段。**不需要迁移，不需要重新初始化数据目录**——空值即"全天一档"，恰好是旧记录本来的含义 |
| 6 管理界面表达 | 价格表单增加时段表编辑器，部署面板、价格时间线、用量明细均显示档位 |

一个 §6 未列出、实施时才发现的连带改动：`CalculateUSDTokensV1` 增加了时刻入参。
"这笔多少钱"在有分时的价格下没有不带时刻的答案，与其让某个调用方静默用 base 费率，
不如让签名在每个调用点上把这个问题问出来。
触发来源：[DeepSeek 适配方案 §2.5 / §5 / §7.4](../todo/deepseek-adaptation-plan.zh-CN.md)
范围：`internal/domain`（价格版本、价格快照、计价公式）、`internal/ledger`、`internal/budget`、
`internal/app/admin_prices.go`、Admin Console 的价格编辑——**逐处影响面见 §6**，
它比"模型部署页的价格设置"要宽
相关：[ADR 0022 缓存读取输入计价](../adr/0022-cache-read-input-pricing.md)、
[版本化模型定价 PRD](prd-versioned-model-pricing.zh-CN.md)、
[时区治理 PRD](prd-timezone-governance.zh-CN.md)

## 1. 要决定的是一件事

**价格版本是否要能表达「同一天内按时段重复变化的费率」。** 今天不能——它只能表达
「从某个时刻起整体换一套费率」。这两件事看着相邻，实际是两种结构：前者是周期性规则，
后者是一条时间线。

这不是 DeepSeek 的问题，是所有供应商共用的计价结构问题。DeepSeek 只是第一个把它变成
**当下就在发生的金额偏差**的供应商。

## 2. 现状：哪些已经有了，缺的到底是什么

已经有的（这几条常被误记为缺口，写在最前面避免评审重走一遍）：

- **时间线有。** `DeploymentPriceVersion.EffectiveFrom`（`internal/domain/pricing.go:202`）
  一直在，价格版本可以在任意时刻整体切换，`resolvePriceEffectiveFrom`
  （`internal/app/admin_prices.go:541`）是它唯一的生效规则。
- **分档计价有。** `CalculateUSDTokensV1`（`internal/domain/pricing_cost.go`）已经按
  `(input − cached) × input_rate + cached × cached_rate + output × output_rate + fixed` 拆档，
  缓存档位由 [ADR 0022](../adr/0022-cache-read-input-pricing.md) 于 2026-08-17 落地。
- **会计时区受治理。** 周期身份自描述、`usage.timezone` 版本化，见
  [时区治理 PRD](prd-timezone-governance.zh-CN.md)。
- **预留时定档的机制有。** `PrepareDeploymentPricePin`
  （`internal/store/bolt/store_pricing.go:688`）已经在预留时刻取价并把 pin 持久化，
  且带 `selectedAt` 高水位单调性与时钟前跳隔离（`wall_clock_forward_jump` → quarantine）。
  分时会让金额依赖当天的钟点，从而抬高对时钟正确性的依赖——但这层防护无需为分时新造。
- **IANA 时区基建有。** `internal/timezone/database.go` 治理时区库来源（env / system /
  `time/tzdata`），`cmd/halro/main.go:26` 导入 `_ "time/tzdata"` 保证裸容器里也能解析，
  `ValidateAccountingTimezone`（`internal/domain/accounting_settings.go:77`）只接受 IANA
  区名。供应商时区只是"再加一个受治理的 IANA 名字"，夏令时由 tzdata 处理，不是新概念。

缺的只有一样：**一条「每天 X 点到 Y 点用另一套费率」的周期性规则，以及它在快照里的落点。**

## 3. 不做的代价（为什么值得单独排期）

DeepSeek 的分时折扣**已经在生效**：高峰为北京时间 9:00–12:00 与 14:00–18:00，
非高峰按官方文档减半（见适配方案 §7.4）。操作者今天只能填一个固定价，于是：

- 填高峰价 → 每天非高峰的那段时间**记贵一倍**；
- 填非高峰价 → 高峰时段**记少一半**，预算与配额按偏低的成本放行；
- 填加权平均价 → 每一笔都不对，长期趋近正确，**但预算是按笔执行的**，
  不是按月对账的。

方向由操作者填哪个价决定，这本身就是问题：**同一个部署，账的偏差方向取决于一个没有语义的输入。**

其它供应商同样有分时或阶梯定价的可能，这条规则一旦确定，是所有供应商共用的。

## 4. 三个候选

| # | 方案 | 代价 | 结论 |
|---|---|---|---|
| A | 维持现状，文档写清「填加权平均价」 | 每笔都错，预算按笔执行；无工程成本 | 今天的默认，不是终点 |
| B | 价格版本表达时段规则，结算时按时段选档 | 持久结构变更、快照契约变更、管理界面变更 | **推荐**；承载形状已由 §5.4 定死（只能内嵌在价格版本内），其余边界要一次定清 |
| C | 用现有 `EffectiveFrom` + 外部调度，每天切两次价格版本 | 零结构变更；但每天产生 2 个价格版本，一年 700+ 条，且**调度失败即长时间错价**，失败还不显眼 | 不推荐：把正确性押在一个进程外的定时器上，与 fail-closed 冲突 |

C 之所以要写下来，是因为它看起来是「用已有能力解决」，评审时很容易被当作省事的选择。
它的失败模式是静默的——切换没跑，账继续按上一个档记，而没有任何一层会报错。

## 5. 若选 B，必须先定的议题

下面六条里，**第 4 条已由现有代码定死**（见该条），其余五条是评审的议题清单，不是实现步骤。
前三条是硬约束，写错会直接损坏既有不变量。

1. **快照必须记录被选中的档位，且在预留时刻定档。**
   这是最硬的一条：结算事件的价格快照必须与预留时的**逐字节一致**，
   `samePriceSnapshot`（`internal/ledger/event.go:492`，校验在 `event.go:358-361`）用 JSON
   相等判断，不一致的结算会被 Ledger 直接拒绝。所以一个跨越 12:00 的请求，
   **不能**在结算时重新按当时时刻选档——那会让它的结算被拒。定档时刻只能是预留时刻，
   而预留本来就必须先于 Provider 调用持久化。
   连带要求：`PriceSnapshot`（`internal/domain/pricing_snapshot.go:17`）要能把
   「哪一档、依据哪条规则」写进去，否则重放时无法复现同一个金额。

2. **时区权威是供应商的，不是实例的。**
   DeepSeek 的高峰按北京时间划分，与实例的 `usage.timezone`（会计周期时区）无关，也与
   管理员的显示时区无关。三者必须显式分开，否则一个把会计时区设成 UTC 的实例会按错误的
   小时判断折扣。夏令时地区的供应商要一并考虑：规则存的是带时区标识的本地时段，
   不是 UTC 偏移量。基建见 §2 最后一条，本条要定的是**语义分离**，不是能力有无。

3. **判不出档位时按贵的那一档。**
   规则缺失、时区数据不可用、时刻落在规则未覆盖的空隙——都必须落到**较高**的费率，
   与 `CLAUDE.md` 的 fail-closed 一致。少记的账没有第二次机会被发现。
   时段表本身的校验（重叠、跨午夜、覆盖空隙）要落在
   `DeploymentPriceVersion.Validate()` 里，不能只靠管理界面拦。

4. **规则挂在哪一层——已定：只能内嵌在价格版本内。**
   这条原本列为开放议题（版本内嵌 vs. 部署上的独立可变对象 + 版本引用），但现有代码
   已经排除了后者。`validateSnapshotAgainstPrice`（`internal/store/bolt/pricing_backup.go:80`）
   在备份校验时用 `NewVersionedPriceSnapshot(price, snapshot.PricingSelectedAt)`
   **重新推导**快照，并要求 digest 与存档的快照逐字节相等。也就是说全系统已经假设：

   > 快照 = f(价格版本, selectedAt)，一个纯函数。

   档位规则内嵌在不可变的价格版本里，`f` 只是多用了 `selectedAt` 的一个维度（供应商本地
   钟点），纯函数性不变——第 1 条的 Ledger 相等校验、重放、备份校验全部自动成立，不需要
   新机制。而独立可变对象会**直接打破**这条：被引用的对象一旦被改，历史快照就再也推导
   不出来，备份校验会开始拒绝真实数据。
   代价照付：每次改折扣都要新建一个价格版本，规则表不能跨版本复用。

5. **持久结构变更的范围与代价。** `DeploymentPriceVersion` / `DeploymentPriceProposal` /
   `PriceSnapshot` 三处都要动，格式版本要 bump，**要明说是否需要重新初始化数据目录**
   （pre-1.0.0 就地改，不并存旧结构，见 `CLAUDE.md`）。ADR 0022 是同一形状的先例，
   可以照它的路径走。逐处清单见 §6。

6. **管理界面怎么表达。** 操作者要填的是「时段 + 费率」，不是一个数。价格提案、
   生效时刻、审计记录都要能显示它，否则规则会变成一个只有后端知道的隐藏状态。
   逐处清单见 §6——它不止价格表单一处。

## 6. 影响面清单（按代码血缘核实，2026-08-18）

这一节回答「是不是只改模型部署页的价格设置」。**不是。** 价格表单是最显眼的一处，
但连带五类。其中**两处是接口/展示语义的变更，不是加字段**，最容易被漏，已加粗标出。

### 6.1 前端（`web/`）

| 位置 | 要做什么 |
|---|---|
| `PriceVersionForm`（`web/src/pages/DeploymentsPage.tsx:477`） | 主改动：从 4 个数字输入变成「基础费率 + 一张时段表 + 供应商时区」 |
| 折叠行与展开面板（同文件 `250-277`、`320-327`） | 现在 `DeploymentFact` 把 input / cached / output 各显示成**一个数**；有时段后这个前提不成立，要表达「当前落在哪一档、今天共几档」 |
| 价格时间线与排期列表（同文件 `347-357`） | 现在只按 `effective_from` 列版本，规则表必须可见，否则就是只有后端知道的隐藏状态（§5.6） |
| **`web/src/pages/UsagePage.tsx:162`** | 每笔尝试现在显示 `price_version_id · v{n}`。**必须补上「billed 在哪一档」**——否则对账时无法解释两笔相同用量为何金额不同 |

外加 `web/src/types.ts`、i18n 两份 locale（zh-CN / en-US），以及重建并提交
`internal/webui/dist`。

### 6.2 Admin API

- `GET`/`POST /admin/api/v1/deployments/{id}/prices`（`internal/app/runtime.go:1439-1440`）：
  加字段，直接。
- **`POST /prices/preview`（`internal/app/admin_prices.go:211`）：语义变更，不是加字段。**
  它现在算「给定 token 数的成本」，而 `CalculateUSDTokensV1(input, cached, output, price)`
  **没有时刻入参**。加时段后预览必须回答「哪个时刻的成本」——要么加时刻入参，要么一次
  返回全部档位。评审要定这个形状。
- 价格提案 4 个 endpoint（`runtime.go:1444-1447`）与 `DeploymentPriceProposal`
  （`internal/domain/pricing_proposal.go:30`）：提案镜像价格版本的费率字段，必须同步加，
  否则「提案 → 采纳」会静默丢掉规则。注意**提案目前没有前端界面**（`web/src/api.ts:412-418`
  有客户端方法，但没有任何页面调用），所以这块是纯后端一致性，不产生新 UI 工作量。

### 6.3 计价与会计核心

`CalculateUSDTokensV1`（`internal/domain/pricing_cost.go`，签名要能拿到选中档位）、
`PriceSnapshot` 与 `NewVersionedPriceSnapshot`（`internal/domain/pricing_snapshot.go:17`/`:101`）、
`DeploymentPriceVersion.Validate()`（时段表校验，见 §5.3）、
`PrepareDeploymentPricePin`（`internal/store/bolt/store_pricing.go:688`，定档实际发生的地方）。

Ledger **不用改代码**——`samePriceSnapshot` 是 JSON 全等比较，新字段自动纳入——但要补
测试证明跨时段边界的请求结算被接受（§8 第一条）。

### 6.4 持久层

- bbolt 迁移：ADR 0022 落在 migration 30（`internal/store/bolt/store.go:742`
  `deployment_price_cached_input_rate`），这次是下一个编号，编号不得复用。
- `validateSnapshotAgainstPrice`（`internal/store/bolt/pricing_backup.go:80`）代码不用改，
  但备份校验会自动开始检查新字段的可推导性——这正是 §8 的那条判据。
- **Parquet 不用动。** `PriceSnapshotJSON`（`internal/usage/parquet.go:68`）是整块 JSON 列，
  新字段随之落盘，不需要新列，`parquetSchemaVersion` 不必从 4 升到 5。

### 6.5 运维面

- `internal/app/doctor.go` 的 `pricing_clock`（:260）与 `pricing_readiness`（:373-388）：
  时钟正确性从「影响版本切换时刻」升级为「影响每天每笔的档位」，值得加一条供应商时区库
  可用性检查。
- `internal/app/admin_onboarding.go:103-116` 的 `PriceReady` 判定逻辑不变（有无生效价格），
  但要确认带规则的价格仍算 ready。
- `PricingAuditIntent`（`internal/domain/pricing_audit.go:9`）现在只记 price_version /
  effective_from / source，不记费率数值，可能不必加字段——但要确认 `ChangeSummary`
  是否足以让审计看见规则变化（§5.6）。

## 7. 不在本文范围

- **DeepSeek 的 Anthropic 兼容端点**与**原生 Responses 端点**：各自是独立评审，
  理由与已核实的事实记在[适配方案 §5](../todo/deepseek-adaptation-plan.zh-CN.md)。
  前者会把认不出的模型名静默兜底成 `deepseek-v4-flash`，与 Halro 的成本归属前提冲突，
  那是它自己评审的第一个议题。
- **阶梯计价（按用量分段）与承诺折扣**：形状不同，不要顺手合并进来。理由不只是"范围太大"，
  是**形状不兼容**：阶梯的费率取决于周期内的用量累计，不是 §5.4 的 f(价格版本, selectedAt)
  的纯函数——快照无法从价格版本重新推导，`validateSnapshotAgainstPrice` 会开始拒绝真实
  存档，重放也不再收敛。分时能扩展进现有结构，阶梯不能，它需要自己的评审。

## 8. 验收判据与落实位置

- 一次跨越时段边界的请求，其结算金额等于**预留时刻**所在档位算出的金额，且 Ledger 接受该结算
- 同一笔用量在重放时得到同一个金额（快照自带档位，不重新按时钟判断）
- 带时段规则的快照能被 `NewVersionedPriceSnapshot(price, PricingSelectedAt)` 原样推导出来，
  `validateSnapshotAgainstPrice` 对它的 digest 校验通过（§5.4 的纯函数性，用真实备份验，
  不用自造 fixture）
- 规则不可判定时算出的金额 ≥ 任一可选档位算出的金额
- 时段表的重叠 / 跨午夜 / 覆盖空隙由 `DeploymentPriceVersion.Validate()` 拦下，不依赖管理界面
- 会计时区改动不改变折扣判断
- 价格预览（§6.2）对同一组 token 数能显式回答"哪个时刻的成本"，不再返回一个无时刻语义的数
- 用量明细（§6.1 的 `UsagePage`）能说出每笔尝试 billed 在哪一档，两笔同用量不同金额可被解释
- 现有的固定价部署行为不变（没有时段规则 = 全天一个档）

每条对应的测试（均已通过）：

| 判据 | 测试 |
|---|---|
| 跨边界结算取预留档位 | `TestPriceScheduleSnapshotPinsTheReservationTier` |
| 重放金额稳定、快照可原样推导 | 同上（digest 比对）+ `TestScheduledPricePinReDerivesThroughTheBackupValidator`（走真实 bbolt 与真实 pin 路径，非自造 fixture） |
| 判不出档位时 ≥ 任一档 | `TestPriceScheduleFallsBackToTheDearestTierWhenTheZoneIsUnknown`（对四种 token 组合逐档比对） |
| 时段表非法形态被 `Validate()` 拦下 | `TestPriceScheduleValidationRejectsMalformedTables`（11 种）、前端 `scheduleDraftProblem` 用例 |
| 会计时区不影响折扣判断 | `TestPriceScheduleIgnoresTheAccountingTimezone` |
| 预览能回答"哪个时刻的成本" | `TestScheduledPriceRoundTripsThroughTheAdminAPIAndPricesEveryTier` |
| 用量明细说得出 billed 在哪一档 | 前端 `billedTierLabel` 用例 |
| 固定价行为不变 | `TestFixedPriceSnapshotCarriesNoScheduleTier`、`TestRecordsWrittenBeforeSchedulesDecodeAndHashUnchanged`（旧记录解码后编码逐字节一致，摘要不变） |
