# V-012 — P2：在线流量期间的 `CatchUp` 让用量索引永久丢记录

- 基线：冻结 `main` `2e749db1`（已存在）；`5e1b7676`（#430 之后）明显加重
- 段/功能：D 段 / P1 用量历史（`internal/usage/collector.go`）
- 分类：【问题】，真实二进制现象（E05b 预演）+ 确定性单元复现
- 发现方式：E05b 预演（假上游，全部 200）收尾的 `usage verify` 失败
- 状态：已修复，#433（合并 `17972e41`）

## 现象

E05b 预演收尾时 `halro usage verify` 报 `usage reconciliation content mismatch`：121,635 个 attempt 中有一个的 `started_at` 被替换成"完成时间减延迟"的兜底值。队列没有丢弃（drops 为 0），也没有任何日志或指标报告。

## 原因

`Collector.Run` 先从队列取出一条记录，然后才等 `applyMu`。若此时一个持锁的 `CatchUp`（维护定时器、Admin 用量读取）把排在它**后面**的记录都应用了，这条被持有的记录再到 `Aggregate.Apply` 时已落在水位之后，被拒为 `usage record sequence is not monotonic`。重放从水位继续，所以它永久丢失。加了插桩后每轮约 22 次拒绝，相当于索引少了 11 个请求。冻结 SHA 上 3/10 轮复现，#430 之后 10/10：#430 在 `CatchUp` 开头先写回持有的批次，持锁时间更长。

## 影响

用量索引（`history.db`、汇总）是 Ledger 的派生物，余额与账务权威不受影响；`usage verify` 能发现，`usage rebuild` 能恢复。但运维看到的用量会少于实际，且没有任何信号提示。

## 修复

`Observe` 通过单槽 channel 唤醒 `Run`；`Run` 与 `CatchUp` 一样在 `applyMu` 下排空队列，每次最多 `historyFlushRecords` 条（剩余则再唤醒），记录不会在锁外被持有。`TestCatchUpDuringLiveTrafficLosesNothing`：修复后 `-count=10` 通过，旧 `collector.go` 上 10/10 失败（例如索引 560 个请求，Ledger 568 个）。真实二进制在修复分支上重做预演：`ledger/usage/audit verify` exit 0，`doctor` healthy。
