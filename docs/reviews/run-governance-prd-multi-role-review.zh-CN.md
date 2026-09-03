# Run Governance PRD 多角色评审报告

- 日期：2026-09-04
- 分支：`codex/run-governance-prd-review`
- 对象：[Run 治理、业务结果关联与单位结果成本 PRD](/Users/ziy/Code/ClayCosmos/Halro/docs/prd/prd-run-governance-and-business-outcomes.zh-CN.md)
- 代码基线：`381743f6613607dc256828f4776b52af8bdd232c`
- 结论：**修订后可以进入 S0 契约与容量验证；不能跳过 S0 直接修改持久格式。**

## 1. 评审角色与方法

本轮按三种职责分别检查同一份文档：

| 角色 | 检查重点 |
| --- | --- |
| 架构边界 | Halro 核心边界、故障域、模块耦合、兼容和阶段交付 |
| 预算与账本 | 双预算原子性、pending/reservation、恢复、跨账期、事件格式和第二权威风险 |
| 产品/API/FinOps | 用户闭环、Outcome 信任、权限、幂等、统计口径、UI 和业务试点 |

三个独立只读评审任务都运行了源码检查，但任务后端把完成状态返回为空正文，因此空结果不被视为“评审通过”或可引用证据。主任务随后使用三套相同检查表逐项重审，并把源码事实、问题、裁决和文档改动记录在本报告中。

严重度：

- P0：会造成不可接受的数据损坏、安全绕过或生产失控，方案不能继续；
- P1：进入开发前必须修正的架构或契约缺陷；
- P2：必须在 S0 冻结或补充的实现完整性问题；
- P3：文案、命名或后续增强。

本轮未发现 P0。初稿发现 4 项 P1、5 项 P2、1 项 P3，均已在 PRD 中作出裁决；容量数值等需要实验的数据继续作为 S0 开放项保留。

## 2. P1 发现与裁决

### P1-01 · Outcome 与 accounting Ledger 共用故障域

**初稿问题**

初稿把 Work Unit、Run 和 Outcome 全部写入现有 authenticated Ledger，同时又要求 Outcome 写入和报表故障不得影响普通推理。两项要求不能同时成立。

当前 `budget.Manager.appendApplyRecord` 一旦 apply 失败会把状态机标为终止失败，之后拒绝继续 append；这是正确的计费 fail-closed 语义。若非计费 Outcome 也进入同一状态机，它的格式或应用错误就可能停止后续 accounting 事件。现有 `ledger.State` 还永久保存 EventID digest 和余额状态，高基数 Outcome 会扩大计费热状态。

**影响**

- 业务结果写入故障可能拖垮模型调用；
- Outcome 洪峰与 Attempt reservation 竞争同一日志 writer/fsync；
- 报表扩展增加核心计费状态机的内存与恢复负担。

**裁决**

- Work Unit、Run 生命周期及 Attempt 归属保留在 Accounting Ledger；
- Outcome 进入独立 authenticated Governance Journal；
- Governance Journal 只对结果声明负责，不保存或结算金额；
- 两者以 WorkUnitID 和双 watermarks 连接，分别验证、备份和恢复；
- Governance Journal 故障只关闭 Outcome 写入和治理查询，不改变无 Run 推理 readiness。

**状态：已修正。**见 PRD D9、§9.1、§10.2、§14。

### P1-02 · `exhausted` 不是稳定的 Run 生命周期状态

**初稿问题**

初稿把“不能准入最小价格调用”定义为不可逆 `exhausted`。系统没有跨模型稳定的“最小调用价格”：一个昂贵请求被拒绝后，更便宜的请求可能可以进入；已预留 Attempt 按更低实际费用结算后，余额也可能重新释放。

**影响**

- 一次拒绝可能错误地永久关闭仍有可用额度的 Run；
- 价格变化会让状态含义漂移；
- 应用把 `run_not_active` 和本次金额不足混为一谈。

**裁决**

Run 生命周期只保留 `active / closed / expired`。预算另返回实时 `budget_state=available|fully_reserved|depleted`；某次准入失败返回 `run_budget_exceeded`，不写永久状态事件。

**状态：已修正。**见 PRD §7.5、§8.3、§19.2。

### P1-03 · Deferred 与异步资源没有冻结 Run 归属和预算时点

**初稿问题**

初稿只说所有协议接受 `X-Halro-Run-ID`，没有说明提交后延迟执行的 ProviderResource 如何保存归属，也没有说明预算是在排队时还是实际执行时预留。

当前 Deferred Response 会先持久保存资源，由后台 worker 后续执行；仅从提交 Header 获取 RunID 会在重启或后台执行时丢失。若提交时长期持有 reservation，现有“未开始 reservation 在恢复时释放”的规则又会与仍在队列中的资源冲突。

**裁决**

- Deferred 提交时验证并持久化 RunID/WorkUnitID，worker 执行前重新验证并准入；
- queued 状态不长期占用 reservation，接收成功不承诺未来一定有预算；
- Provider async invoke 在提交即触达上游，必须在提交前 reservation；
- cancel、poll、restart 都从资源记录恢复归属，不依赖新的 Header。

**状态：已修正。**见 PRD §8.1。

### P1-04 · 未完成 Work Unit 会扭曲单位结果成本

**初稿问题**

初稿使用全部 eligible Work Units 的成本除以成功数，同时允许 open Work Unit 上报 Outcome 和继续创建 Runs。这会把仍在执行的成本提前放入分子，把 provisional 成功放入分母，指标随正在进行的工作大幅波动。

**裁决**

- 增加 `matured_units`：Work Unit 已关闭且所有关联 Runs 没有 pending/inflight Attempt；
- open Work Unit 的 Outcome 标为 provisional；
- success rate 和 cost per success 只使用 matured units；
- in-progress cost 单独返回；coverage 仍以全部 eligible units 为分母，保留漏报可见性。

**状态：已修正。**见 PRD §7.4、§7.6、§11.2–11.3、§12.4。

## 3. P2 发现与裁决

### P2-01 · 幂等 bucket 不能成为隐藏权威

初稿只说新增 Governance idempotency bucket。如果 bucket 丢失后相同重试创建第二个 Work Unit/Run，幂等语义实际由可损坏派生库决定。

裁决：创建和 Outcome 事件写入 operation、Key hash、规范 fingerprint；bbolt 仅保存可重建索引，首版不做改变历史重放语义的 TTL 淘汰。

**状态：已修正。**见 PRD §8.1、§10.1、§19.1。

### P2-02 · Definition 禁用会不会截断在途验收没有定义

裁决：禁用只阻止新 Work Unit 引用；已经声明该不可变版本的 Work Unit 仍可上报和修订。物理删除仍不允许。

**状态：已修正。**见 PRD §7.3、§8.2。

### P2-03 · Project 与 Run 双预算必须共享同一临界区

当前实现按 Project 使用一把 admission mutex，并用 pending 覆盖“已准入但尚未进入 Ledger balance”的窗口。若新增独立 Run 锁，再依次获取 Project/Run 锁，会引入锁顺序和两边 pending 不一致。

裁决：在现有 `projectAdmission` mutex 下同时维护 Project period pending 与 Run pending；一次检查和增减两个 pending，fsync 继续在锁外。Accounting `State.Apply` 同时更新 Project 与 Run balance。

**状态：已修正。**见 PRD §9.2。

### P2-04 · Run/Work Unit 归属需要 apply 级因果校验

只在 Handler 验证归属不足以保护 replay。Settlement 必须与 Reservation 冻结的 RunID/WorkUnitID 完全相同，RunCreated 必须引用已存在且未关闭的 Work Unit，同一 Request 的 Attempts 不能换 Run。

**状态：已修正。**见 PRD §9.2 与 §19.1–19.3。

### P2-05 · 两个日志的导出、备份和查询快照必须成对表达

拆分 Outcome 故障域后，如果 Summary 只返回 accounting watermark，就无法解释结果声明使用到哪里；备份漏掉空或非空 Governance Journal 也会产生费用存在但结果历史缺失的半恢复。

裁决：Summary 与 export 返回双 watermarks；backup manifest、verify、staging restore 和 full replay 分别覆盖两个日志，再验证连接状态。

**状态：已修正。**见 PRD §10.4–10.5、§11.3、§19.5。

## 4. P3 发现与裁决

### P3-01 · `budgeted_success` 命名会被理解为“花费在预算内”

该字段实际表达“成功 Work Unit 是否出现过 Run budget rejection”，并不证明供应商最终账单在预算内。

裁决：改为 `success_without_run_budget_rejection`，并保留 Run cap 不是供应商发票绝对上限的说明。

**状态：已修正。**见 PRD D3、§11.2。

## 5. 三个角色的通过项

架构边界通过项：

- Halro 不运行 Agent、工具、CI、Judge 或人工标注；
- Project 仍是上级权限与成本责任边界；
- Outcome 模块与普通推理热路径隔离后，扩展仍属于治理、身份、记账和证据原语；
- S1 可以作为内部工程里程碑，但与 S2 一起构成首个用户可见版本。

预算与账本通过项：

- Reservation 必须在 Provider I/O 前持久；
- 同时计算 committed、reserved 和 pending；
- 已开始但结果不明的 Attempt 继续保守结算；
- Run cap 跨 Project 账期累计，但每个 Attempt 保留其受理账期；
- Run cap 明确不是供应商最终发票的绝对保证。

产品/API/FinOps 通过项：

- Outcome 是具来源的外部声明，Halro 不显示为自身判决；
- Work Unit 解决失败重跑只计算一个业务结果的问题；
- success rate 与 coverage 同屏，未知费用显式 partial；
- cost per success 包含成熟失败工作的浪费成本，同时排除尚未成熟工作；
- 完整 TCO、收入和任意多维分析继续由外部数据系统承担。

## 6. 仍需 S0 用测量裁决的事项

以下不是评审遗漏，不能靠评审主观给出数值：

1. active Runs、open Work Units、Outcome revisions 和 TTL 硬上限；
2. 控制面 RPM、body size、最大 cohort 范围与查询延迟；
3. Governance Journal segment、RTO/RPO、chain key rotation 和损坏隔离门槛；
4. Accounting/Governance checkpoint 的分段和连接方式；
5. Parquet/NDJSON 导出形状；
6. Outcome 修订窗口；
7. 首个真实业务试点定义、验收方和决策用途。

在这些数据出来前，文档中的 1,000 active Runs、30 天 TTL 等数字只能叫候选限制，不能成为产品容量承诺。

## 7. 最终判断

修订后的方案保持了 Halro 的正确扩展方向：核心负责身份、权限、调用归因、预算准入、可验证费用和受控结果声明；业务系统负责执行 Agent 和作出验收判断；外部分析平台负责完整 FinOps 与跨系统 TCO。

方案可以进入 S0，但实现团队必须先完成双预算 prototype、无 Run 热路径 benchmark、两个日志的故障隔离验证和最大规模恢复实验。任何一项证明隔离或容量不可接受，都应缩小 Outcome 内置范围，而不是降低 accounting fail-closed 语义。
