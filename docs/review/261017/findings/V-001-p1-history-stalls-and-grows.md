# V-001 — P1：一行用量历史损坏会让 `history.db` 与 Parquet 导出永久停更，重启不恢复、无告警、指标显示正常

- 基线：冻结 `main` `2e749db1`
- 段/功能：D 段 / M01（#407）；交叉点 X13、X18
- 分类：【问题】，真实二进制复现（E04c）
- 级别变更：初稿为 P1（"待写批次无界增长"），对抗复核一度推翻"无界"降为 P2；#427 的评审意见指出那次复核只测了高吞吐，**按节奏到达的复现证实无界增长，恢复为 P1**（2026-10-18），见 `../adversarial-verdicts.md`

## 现象

`HistoryStore.commit`（`internal/usage/history.go:334-436`）在同一事务里读取并认证已有的 hourly、rollup、successes 和 meta 状态行。只要其中一行认证失败，而该行又是持续写入要读回的行（当前小时的 hourly 行、当天的 rollup 行、successes 行、meta 行），之后的每一次提交都失败：

1. **用量历史永久停更**：运行中出现时，Collector 每应用一条记录就重试一次失败的提交（`internal/usage/collector.go:92`），吞吐塌缩，队列满后丢弃并置 lagging；启动时已存在时，开机追赶失败、Collector 保持 lagging。两种情况下控制台的用量、失败请求、Dashboard 都停在故障时刻；
2. **Parquet 导出随之停止**：`usage parquet export skipped; history write failed`；
3. **重启不恢复**：损坏的是 hourly/rollup 行时，启动的 `RestoreAggregate` 不读这些行，判断可信，不重建（meta 行损坏时 `internal/app/runtime.go:1174-1189` 才会 Reset 重建）；
4. **持续空转**：失败的提交被不断重试，E04c 中约 45 次/秒；
5. **指标看起来正常**：`halro_usage_history_pending_records` 始终为 0（批次在提交途中被取出、不计数）；只有 `flush_failures_total` 与 `row_auth_failures_total` 在涨，而它们没有告警（V-004），`doctor` 也不检查 `history.db`（V-005）；
6. **与发布说明不符**：CHANGELOG 写的是"rebuilt from the Ledger whenever it cannot be trusted"；实际只在启动时、且只在 meta 行不可读或位置超前时重建。

唯一的出路是离线 `halro usage rebuild-summary`（会从 Ledger 重建并替换 `history.db`），但没有任何信号告诉运维需要执行它。

账务权威不受影响：Ledger、余额、热状态都与此无关。

## 证据

**真实二进制（E04c，Linux 容器，候选 linux/arm64）**：先写入当前小时，停机后用一次性工具翻转 `history.db` 中 hourly 桶**最后一行**（`hourly/000000006ad37150`）的首字节，再启动并以 16 线程持续请求：

```
t=10s  pending_records 0 flush_failures_total 312  row_auth_failures_total 312
t=60s  pending_records 0 flush_failures_total 2603 row_auth_failures_total 2604
t=120s pending_records 0 flush_failures_total 5080 row_auth_failures_total 5082
WARN usage history catch-up failed ... usage history row failed authentication
WARN usage parquet export skipped; history write failed
```

重启后：`usage history catch-up failed; it is retried on the next maintenance tick`，30 s 内 `flush_failures_total` 再涨 1,283。

**对照**：翻转 hourly 桶**第一行**（最早的小时）时，`flush_failures_total` 始终为 0——只有被持续写入读回的行才会触发。

**Collector 层一次性测试**（冻结 SHA 的 scratch worktree）：追赶成功、非 lagging 的 Collector，在运行中损坏最新 hourly 行后继续 `Observe`，`applied` 只缓慢前进，队列满（容量 8192）后 `lagging=true`、`applied` 停在 2,382、`dropped` 持续增长——内存有界，历史停更。

## 影响

运维看到的用量与失败请求停在某一时刻，而所有可见指标与 `doctor` 都显示正常；用量归档（Parquet）同时停止。由单行损坏、bbolt 写错误等派生文件故障触发，不需要外部调用方。

## 建议

- 提交因认证失败而失败时，丢弃内存增量、标记 lagging，并在后台从 Ledger 重建 `history.db`（与"cannot be trusted 即重建"的说明对齐），或至少在启动时对当前小时/当天的行做一次认证；
- `pending_records` 计入在途批次；为 `flush_failures_total`、`row_auth_failures_total` 的增长加告警，runbook 写明 `halro usage rebuild-summary`（与 V-004 一并处理）。

## 恢复为 P1 的证据（2026-10-18）

**按节奏到达**（冻结 SHA 的 scratch worktree，一次性测试）：运行中的 Collector 追赶完成、非 lagging，损坏最新 hourly 行后以 50 请求/秒（100 条记录/秒）持续 `Observe`：

```
t= 1s pending=100  flush_failures=4  lagging=false dropped=0 queue=0
t= 6s pending=600  flush_failures=25 lagging=false dropped=0 queue=0
t=12s pending=1200 flush_failures=50 lagging=false dropped=0 queue=0
```

每次失败的提交都把批次交还（`Aggregate.Flush`），`Collector.flush` 只计数（`internal/usage/collector.go:106-110`），队列从不满、`lagging` 从不置位，待写批次随到达线性增长，没有上界。此前的"止于一个队列"只在到达快于失败提交时成立。

**每请求堆成本**：失败提交下每 2 万请求测一次 GC 后的 `HeapAlloc`：10 万请求增长 117.7 MiB，**约 1.24 KB/请求，线性**。按 50 请求/秒约每天 5 GB，2 GiB 的容器一天内会 OOM。热状态上限不覆盖它（用量聚合不计入热状态估算）。

建议的修复因此是发布前必须做的：提交失败（尤其是认证失败）时不得把批次留在内存里无限累积——丢弃内存增量并置 lagging，由后台从 Ledger 重建 `history.db`。
