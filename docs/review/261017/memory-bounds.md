# X18：全局内存有界性

> 对象：冻结 `main` `2e749db1`。逐个列出随运行时间或流量可能增长的内存结构，写明上界与依据。"有界"指存在不随历史长度增长的上界；上界是否足够小另行说明。E05/E06 的实测见 `runtime-evidence.md`。

## 内存

| 结构 | 位置 | 上界 | 依据 | 结论 |
| --- | --- | --- | --- | --- |
| Ledger 热状态（预留、保留租约、请求归属、打开的 Work Unit/Run、余额、治理幂等、归档队列） | `internal/ledger/event.go` `State` | `ledger.max_hot_state_bytes` 估算值（默认 128 MiB）；超过即拒绝新请求/Work Unit/Run | `internal/budget/hot_state.go:75`、`internal/ledger/hot_state.go`；估算对各类条目与实测堆误差 ≤25%（`TestHotStateEstimateTracksTheHeap`） | 有界；但快照瞬时堆约 6×（V-003） |
| 事件摘要去重窗口 | `State.eventDigests`/`eventOrder` | 65,536 条，按满额 12 MiB 计价 | `recentEventDigests`；`hotDigestWindowBytes` | 有界。窗口外的重复只可能来自确定性恢复事件的相邻重复追加（`internal/budget/manager.go:1294`），窗口足够 |
| 已准入未结束请求集合 | `budget.openRequests` | 在途请求数；计入热状态估算 | `internal/budget/hot_state.go`；7 个请求入口均 `defer run.close()` | 有界 |
| map 槽位 | Go map 删除后不归还槽位 | 降到峰值 1/4 以下重建 | `internal/ledger/hot_state.go` `shrinkMaps`（#424） | 有界 |
| 状态快照的瞬时副本 | `marshalSnapshotWithQueue` | 约 6.3× 状态实际堆（实测 60.4 MiB 状态 → 峰值 380.7 MiB） | V-003 | 有界但远大于配置值 |
| 用量历史待写批次 | `usage.Aggregate` `batchAttempts`/`batchSummaries`/hourly/rollup/pending | 正常：2,000 条或 250 ms 写一次。**写入持续失败时无上界** | V-001（已复现，重启不恢复 hourly/rollup 行损坏） | **无界** |
| 用量 rollup 维度 | `usage` rollup | 每维度键数封顶 | `TestUsageRollupBoundsKeysPerDimension` | 有界 |
| 在途请求的用量摘要 | `usage` pending/started | 在途请求数 | `internal/usage/history.go` | 有界 |
| Collector 队列 | `usage.Collector.queue` | channel 容量（`usage.wal_queue_capacity`）；满则丢弃并标记 lagging，之后从 Ledger 补 | `internal/usage/collector.go:70-77` | 有界 |
| 治理 Outcome 头与幂等 | `governance.Manager` `heads`/`idempotency` | 各 4,096 | #422 | 有界 |
| 治理日志去重索引 | `governance.Log.recordsByEventID` | 4,096 | `internal/governance/log.go:37,157` | 有界 |
| 认证后固定速率上限 | `keylimit.Limiter` | `maxTrackedKeys`/`maxTrackedProjects`，超出进 overflow 桶；键只来自已认证的 Key/Project ID | `internal/keylimit/limiter.go` | 有界 |
| 认证前来源限流 | `sourcelimit` | `gateway.source_rate_limit.max_tracked_sources`（默认 16,384） | 配置 | 有界 |
| 路由挂起 | `routegate.Gate.scopes` | 由配置对象（凭据、deployment、Provider、凭据×配置的上游模型）决定 | `internal/routegate/gate.go:92` | 有界（随配置，不随流量） |
| 上游拒绝原因计数 | `failureReasonCounters` | `maxTrackedFailureReasons`，超出计 overflow | `internal/gateway/failure_reason_metrics.go:66-74` | 有界 |

## 磁盘（不是内存，但随历史线性增长，容量规划需要）

| 文件 | 增长 | 保留 | 实测（E01） |
| --- | --- | --- | --- |
| Ledger WAL（含封存代） | 每请求约 5.9 KB（失败请求 5 帧） | 永久，按设计（账务权威，只封存/压缩，不删除） | 10.2 万请求 606 MB |
| `data/history/history.db` | 每请求约 5.75 KB | **无保留期**：`usage.retention_days` 只管 Parquet（`internal/config/config.go:258-261`），历史行永久保存 | 10.3 万请求 591 MB，与 WAL 同量级 |
| `data/history/archive.db` | 每个已结束 Work Unit 约 5 KB（#422 实测） | 永久 | 1,080 个 Work Unit 8 MB（bbolt 预分配为主） |
| `state.snap` | 与热状态成比例 | 覆盖写 | 11 MB |

`history.db` 让磁盘占用相对 v0.8.5 约翻倍且无上限。这是设计决定（"内存只保留有界热状态，历史在盘上"），但发布说明与运维指南都没有给出每请求的磁盘成本或增长提示；见 `review-report.md` 的接受项建议。

## 结论

除 V-001（用量历史写入失败时的待写批次）外，所有随流量或历史增长的内存结构都有不随历史增长的上界；热状态上限按估算生效，但快照时的进程堆约为配置值的 6 倍（V-003）。
