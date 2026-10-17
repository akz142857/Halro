# V-001 — P1：用量历史写入持续失败时，内存中的待写批次无上限增长，无告警，重启也不恢复

- 基线：冻结 `main` `2e749db1`
- 段/功能：D 段 / M01（#407）；交叉点 X18
- 分类：【问题】，已复现
- 对抗复核：见 `../adversarial-verdicts.md`，维持 P1

## 现象

`history.db` 的一次提交失败后，批次被原样放回内存，后续记录继续追加；只要失败持续，批次就随流量线性增长，直到内存耗尽。期间只有一个计数器上涨，没有告警、没有降级、没有自动重建。若损坏的是 hourly 或 rollup 行，**重启也不能恢复**：启动时不读这些行，恢复成功，第一次提交再次失败，增长重新开始；只有手工删除 `history.db` 才能脱离。

## 代码路径

1. `Collector.Run`（`internal/usage/collector.go:79-103`）每收到一条 Ledger 记录就 `Apply` 进聚合，批次满 2,000 条或每 250 ms 调用 `flush`。
2. `flush`（`internal/usage/collector.go:106-110`）失败时只做 `flushFail.Add(1)`；不设置 `lagging`，不停止 `Apply`。
3. `Aggregate.Flush`（`internal/usage/aggregate.go:550-565`）失败时调用 `returnBatch`（`:627` 起），把整批 attempts、summaries、hourly、rollup、pending 放回内存最前面。
4. `HistoryStore.commit`（`internal/usage/history.go:334-436`）在同一事务里**读取并认证**已有的 hourly、rollup、successes 和 meta 状态行（`mergeHourly`、`applyRollupDelta`、`getSealed(historyMeta, …)` `:428`）。这些行中任何一行认证失败（磁盘位翻转、手工编辑、部分写），之后的**每一次**提交都失败。
5. 唯一的外部信号是 `halro_usage_history_flush_failures_total` 与 `halro_usage_history_pending_records`（`internal/app/metrics.go:193-201`）；`deploy/observability/prometheus/alert-rules.yml` 中没有任何引用 `halro_usage_history_*` 的告警，运行手册也没有对应条目。

热状态上限（`ledger.max_hot_state_bytes`，#423）只计价 Ledger 状态，不覆盖用量聚合，所以它挡不住这条增长。

## 复现

一次性测试（只在冻结 SHA 的 scratch worktree 中运行，未提交）：落盘一个请求后，翻转 meta 状态行的一个字节，然后每轮写入 1,000 个请求并 `Flush`：

```
round 1: flush err=true pending=2000
round 2: flush err=true pending=4000
round 3: flush err=true pending=6000
round 4: flush err=true pending=8000
round 5: flush err=true pending=10000
```

第二个一次性测试把 meta 行换成 hourly 行：损坏后用 `RestoreAggregate` 模拟重启，结果 `restore after damage: err=<nil>`，随后 `first flush after restart: err=write usage history: usage history row failed authentication`。meta 行损坏时则不同：启动读到它，`internal/app/runtime.go:1174-1189` 会 `Reset` 并从 Ledger 重建。

测试主体：`newTestAggregate` → `Apply` 一对 `AttemptSettled`/`RequestFinalized` → `Flush` → `bbolt.Update` 中对 `historyMeta`/`historyKeyState` 的值做 `raw[0] ^= 0x01` → 循环 `Apply` 与 `Flush`，断言 `PendingRecords()` 单调增长。

## 影响

- 本次发布的核心承诺是"内存有界"。这条路径在一个派生文件出错时把内存与流量挂钩；hourly/rollup 行损坏时每次重启后都会重新开始；
- 触发不需要外部调用方，但也不需要整盘故障：单行损坏、`history.db` 单独只读、bbolt 写入错误都足够；整盘满时 WAL 同时失败、请求被拒，增长会停，这是唯一自然止损的情形；
- 运维看不到：无告警，`halro doctor` 也不检查 `history.db`（V-005）。

## 建议

- 提交失败时把 Collector 置为 lagging 并丢弃内存批次——用量历史是派生物，`CatchUp`/重建会从 Ledger 补回；或为批次设硬上限，超过即丢弃并标记需要重建；
- 对认证失败的行走"重建 `history.db`"路径，而不是无限重试同一事务；
- 为 `flush_failures_total` 增长、`pending_records` 持续高位、`row_auth_failures_total` 增长加告警与 runbook（与 V-004 一并处理）。
