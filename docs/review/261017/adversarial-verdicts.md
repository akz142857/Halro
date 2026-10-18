# 对抗性复核

> 规则（方案 §5.3）：P0/P1 定稿前由另一视角独立尝试推翻。本轮唯一的 P1 是 V-001 初稿，复核推翻了其核心主张；另外对影响最大的三条 P2（V-002、V-003、V-005）也做了同样的尝试。

| Finding | 初稿 | 推翻尝试 | 结论 |
| --- | --- | --- | --- |
| V-001 | P1："用量历史写入持续失败时，内存中的待写批次无上限增长" | 见下 | **核心主张被推翻，降为 P2**；剩余缺陷（历史与 Parquet 永久停更、重启不恢复、无告警、指标误导）成立 |
| V-002 | P2：延迟层对热状态拒绝不放回队列 | 查找延迟层/资源面是否另有热状态预检：`internal/gateway/deferred*.go`、`inference_resources*.go` 中无任何 `HotState`/`accounting_capacity_exhausted` 引用；拒绝确实在 `appendApply` 之前（`internal/budget/manager.go:893`）。第二次独立复核（2026-10-18）：`deferredAdmissionRefusal` 的注释把可放回的条件写成"在写 Ledger 之前拒绝"，`ErrHotStateFull` 由 `admitRequest` 在 `RequestAccepted` 追加前返回（`manager.go:893-905`），映射在 `service.go:447`——条件满足而码不在列表 | **维持 P2** |
| V-003 | P2：快照瞬时堆约 6× | 质疑点：一次性测试的 `HeapAlloc` 含未回收垃圾，真实进程可能因 GC 并发而峰值更低。用 E06 在真实二进制、热状态贴近上限时以 200 ms 采样 `/debug/runtime` 的 `HeapAlloc` 复核 | **维持 P2**（见下） |
| V-005 | P2：doctor 升级前报不健康 + 不检查新文件 | 查 v0.8.5 的 `OpenReadOnly` 与历次 CHANGELOG | **维持 P2，现象一改写**（见下） |

## V-001 的复核过程

1. **初稿依据**：`Aggregate` 层一次性测试中，手工调用 `Flush`，每轮写入 1,000 个请求，`PendingRecords` 线性增长（2,000 → 10,000）；代码上 `Collector.flush` 失败时不置 lagging（`internal/usage/collector.go:106-110`）。
2. **推翻尝试一（真实二进制，E04c）**：在运行的候选二进制上复现：`pending_records` 始终为 0，`flush_failures_total` 快速增长，堆没有趋势性增长。原因是损坏在启动时已存在，开机追赶失败，Collector 保持 lagging、不应用实时记录。——说明"启动时已损坏"这一情形不增长。
3. **推翻尝试二（运行中损坏，Collector 层）**：追赶成功、非 lagging 的 Collector 在运行中损坏最新 hourly 行后继续 `Observe`。结果：每应用一条记录就同步重试一次失败的提交，吞吐塌缩；`applied` 只前进到 2,382，队列（容量 8,192）满后 `Observe` 丢弃并置 `lagging=true`，此后不再应用。——**内存增长止于约一个队列的记录，有界**。初稿的线性增长是手工 `Flush` 绕过了 Run 循环的自限所致。
4. **复核后仍成立的部分**：历史停更（两种情形都发生）、Parquet 导出停止、hourly/rollup 行损坏时重启不恢复、失败提交持续空转、`pending_records` 因在途批次不计数而显示 0、无告警。这些都由 E04c 的真实二进制输出直接证明。
5. **级别**：不再是"可被触发的资源无界增长"；属于派生物故障的静默停更与可观测性缺口，账务权威不受影响 → P2。

## V-003 的复核结果

真实二进制（E06，Linux 容器，候选 linux/arm64）：热状态估算压到 16 MiB 上限（16,777,984 B）后，70 s 内以 200 ms 采样 `HeapAlloc`：最低 26.8 MiB，**峰值 104.4 MiB**，期间快照 15.0 MB、无失败。峰值约为上限的 6.2 倍，与一次性测试的 6.3 倍一致——GC 并发没有把峰值压低。

仍未测的部分：采样的是 `HeapAlloc`，不是 RSS；RSS 的峰值不会低于堆目标，但具体数字没有测。结论不变：上限有界，但 128 MiB 不能当作进程内存预算，按它设容器内存限制会在快照时 OOM → **P2**。

## V-005 的复核结果

**现象一（升级前 doctor 报不健康）不是本范围引入的**：v0.8.5 的 `OpenReadOnly` 已是同样的精确相等检查（`internal/store/bolt/store.go:1493`，schema 38），历次需要迁移的版本都在 CHANGELOG 里写明"首次启动前 doctor 报 `metadata: fail … does not match required version`，意思是还没升级，不是损坏"（`CHANGELOG.md` 0.8.x 节）。本范围把 schema 从 38 推到 41，**`main` 的 `[Unreleased]` 却没有这句说明**（只在 #426 里补了）——缺的是发布说明，归入 V-007 一类；代码层面的"旧 schema 应报 warn 而非 fail"是既有设计的改进建议，不是回退。

**现象二（新文件不在任何离线检查中）成立**：`doctor.go` 中含 `add("` 的行数在 v0.8.5 与冻结 SHA 上相同（均为 76）；它认识封存代（v0.8.5 前已有），但不读 `history.db`、`archive.db`、`state.snap`。级别维持 P2。

