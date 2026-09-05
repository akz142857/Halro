# ADR 0025: Run budget authority and dual admission

- Status: Accepted for S1/S2 implementation
- Date: 2026-09-04
- Builds on: [ADR 0018](0018-project-admission-and-the-accounting-write-path.md)
- Evidence: [Run Governance S0](../verification/performance/2026-09-04-run-governance-s0/README.md)

## Context

Halro 的 Project 日预算已经通过 Accounting Ledger、durable reservation 和
`pendingAdmitted` 实现 fail-closed 准入。Run 需要一个跨 Project 账期的 lifetime
cap，但不能产生第二份费用账本，也不能让普通请求重新进入慢锁或多一次持久写。

若分别检查 Project 与 Run，两个并发请求可能都看到余额充足并共同越限。若在 Project
lock 内等待 WAL fsync，又会恢复 ADR 0018 已消除的同 Project 串行瓶颈。

## Decision

Run/Work Unit 生命周期、reservation 归属和 settlement 继续进入 Accounting Ledger。
Ledger 是 Project 和 Run 金额的唯一权威；Usage、checkpoint 与 Summary 都是可重建
读模型。

带 Run 的准入在一个 Project 临界区内完成：

```text
project_used = project_committed + project_reserved + project_pending
run_used     = run_committed + run_reserved + run_pending

accept only if:
  project_cap == 0 or project_used + reservation <= project_cap
  and run_used + reservation <= run_cap
```

两个 pending 同时增加后释放锁。Accounting append/apply 在锁外完成；apply 成功或
append 失败后，再在同一锁下移除 pending。任何整数溢出、Run 状态未知、Project 不匹配
或 append/apply 失败都拒绝 Provider I/O。

Run close、Work Unit close 和创建/准入使用相同的 Project lock 建立顺序。先准入的
Attempt 可以完成；先 close/expiry 的 Run 拒绝新 reservation。expiry 是当前时间上的
派生判断，不写永久 exhausted 事件。预算不足也不是生命周期状态，因为更便宜的请求或
低于 reservation 的 settlement 可能恢复可用额度。

无 `X-Halro-Run-ID` 的请求保持 ADR 0018 路径：不读取 Run/Work Unit 状态、不维护
Run pending、不产生新增事件或持久写。`run_governance_enabled` 默认 false。

首版每 Project 最多 1,000 active Runs 和 1,000 open Work Units；默认 TTL 24 小时，
最大 30 天；每 Work Unit 最多 32 Runs。这些是 hard limits，不是容量宣传值。

## Why

S0 probe 在同一锁内更新双 pending，64 个并发准入没有 over-admission，race 检查通过；
1/8/64 workers 的临界区开销保持亚微秒级。该 probe 比生产 Manager 简单，所以只证明
锁形状可行，不能声称最终实现更快。

把 Run 写入 Accounting Ledger 使崩溃恢复、未知 Attempt、close/expiry 后结算和
Project/Run 双投影共享一条因果顺序。把它写进 Governance Journal 会让业务结果故障
参与预算权威，无法 fail closed。

## Rejected alternatives

### 独立 Run 费用账本

拒绝。一次 Provider 消费会有两个可加总来源，恢复和 reconciliation 可能产生不同余额。

### 先查 Project，再查 Run

拒绝。两个检查之间存在 over-admission 窗口，也无法定义 close 与 reservation 的顺序。

### 在 Project lock 内 fsync

拒绝。它重建 ADR 0018 的性能瓶颈；锁只保护 read-modify-decide 和 pending 转移。

### 把一次 budget rejection 写成 exhausted

拒绝。reservation 大小不同且 settlement 可恢复 headroom，永久状态会错误拒绝后续请求。

## Consequences

- Accounting Ledger 下一 feature epoch 必须携带 Work Unit/Run 生命周期与归属字段；
- `ledger.State` 增加可重建 Run balance，但费用仍只结算一次；
- S2 必须覆盖并发、整数溢出、跨日、crash/unknown、close/expiry 与 Provider-zero-call；
- 最终实现后必须在同主机重跑无 Run `BenchmarkRequestLifecycle`，有实质回退则退回设计；
- Run cap 约束 Halro 准入，不得在 API/UI 中称作 Provider 发票绝对上限。

## Required verification

- Project + Run over-admission property test，1/8/64 workers 和 affected package `-race`；
- append/apply 每个 kill point 后 pending、Ledger 和 Provider call count 一致；
- close/expiry/reservation 的确定性 barrier tests；
- 同一 Attempt 在 replay、checkpoint 和 export 中费用仅一份；
- 无 Run 请求没有新增状态读取与写入，benchmark 对比 S0 baseline。
