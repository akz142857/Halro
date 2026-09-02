# Usage 数据保留与裁剪：设计与实施方案

- 状态：提案，尚未实施
- 日期：2026-09-02
- 文档语言：中文
- 适用范围：Usage Aggregate、Usage checkpoint、Parquet 导出、日 rollup、Ledger WAL、控制台「用量与调用」、设置中心
- 数据迁移：第一阶段不需要重新初始化；checkpoint 结构不变，只是内容变少

## 一、问题

控制台「最终失败」和「调用明细」两个页面读的是内存里的 Usage Aggregate。**它只 append，从不裁剪**——这个数据目录有史以来的每一次 Provider 尝试都留在内存里，并且随 checkpoint 整体写进 bbolt。

这不是「留太久」的口味问题，是一条随运行时间线性恶化的资源曲线。

### 1.1 实测代价

一条已结算 `AttemptEvent` 在 checkpoint 里 **1149 字节**（`internal/usage/aggregate.go` 的结构，含价格快照指针，JSON 编码实测）。`TakeCheckpoint` 把**全部历史**重新 marshal 一次，默认 **每分钟一次**（`internal/config/default.go` 的 `CheckpointInterval`）。

| 吞吐 | checkpoint 日增 | 90 天累计 |
| --- | --- | --- |
| 1 次/秒 | 95 MB | 8.3 GB |
| 10 次/秒 | 947 MB | 83 GB |
| 100 次/秒 | 9.5 GB | 832 GB |

三重代价，都随历史长度增长：

1. **常驻内存**保存全部历史；
2. **每分钟一次 O(全部历史) 的 JSON 序列化**，写进 bbolt 单个事务——checkpoint 的耗时与实例寿命成正比；
3. **崩溃后重建**要重放整条 WAL，而 WAL 自身也没有截断机制。

`QueryAttempts` 与 `QueryFailedRequests` 还是从切片尾部**线性倒扫**过滤（`internal/usage/query.go`、`internal/usage/failures.go`）。游标分页在前，但筛选命中率低时会扫过整个切片。

### 1.2 压缩解决不了这件事，而且会让它更糟

checkpoint 今天是**裸 JSON 存进 bbolt，没有任何压缩**（`internal/app/runtime.go` 的 `PutUsageCheckpoint`，值就是 `json.Marshal` 的输出）。这个数据极度可压缩——结构完全重复，只有 ID 和时间戳在变。实测一天（10 次/秒，864000 条）：

| | 大小 | 耗时 |
| --- | --- | --- |
| `json.Marshal` 原始输出 | 942.5 MB | 1.48 s |
| gzip 最快档 | 23.6 MB（**39.9 倍**） | +0.77 s |
| gzip 默认档 | 17.1 MB（55 倍） | +1.53 s |

40 倍不是小数目，磁盘问题确实能压下去。但压缩**不触及真正的成本**，还会加重它：

1. **CPU 是主要瓶颈，压缩只会往上加。** 序列化耗时随历史线性增长：一天的历史每次 checkpoint 花 1.48 s，30 天就是 **约 44 秒**——而 checkpoint 间隔是 **60 秒**。再叠加压缩，30 天规模上一次 checkpoint 要花掉一分钟里的大半，90 天就根本追不上自己的节拍。即便只有 1 次/秒，90 天也意味着每分钟拿约 13 秒 CPU 反复序列化同一段历史。
2. **内存完全不受益。** attempts 是内存里的 Go 结构体，压缩的是写出去的那一份。940 MB/天的 JSON 对应的常驻内存是同一量级，压不掉。
3. **bbolt 单事务里塞一个几百 MB 到几十 GB 的值**，本身就是页面翻搅问题，与压缩无关。

所以压缩的定位是：**窗口裁剪之后的锦上添花，不是它的替代**。窗口把历史压到 30 天以内之后，checkpoint 只有几十到几百 MB，那时再压缩是把一个已经可接受的数字变得更好；而在没有窗口的前提下压缩，只是把「磁盘爆掉」换成「CPU 追不上」。

顺带一提，Parquet 侧用的是 `parquet.NewGenericWriter` 的默认设置，没有显式指定压缩编码——那是独立于本方案的一个可优化点，但 Parquet 是列式存储且本来就有归档窗口，优先级远低于此处。

### 1.3 已有的 `usage.retention_days` 管不到它

`internal/config/config.go` 有 `usage.retention_days`，默认 90。但它**只**被 `halro usage prune` 使用，而那是一个手动、离线（要停机取数据目录锁）的 CLI 命令，只裁剪 **Parquet 分区**——控制台不读 Parquet。

所以现状是：设置里的 90 天，和界面上看到的记录，是两回事。

### 1.4 一处必须订正的文档

`docs/guides/operator-guide.md` 与最终失败列表页面说「可见窗口等于 Usage 保留窗口」。**这句话是错的**：可见窗口是无限的，`retention_days` 与之无关。本方案实施前应先订正，或与第一阶段同时订正。

## 二、当前实现证据

| 结构 | 位置 | 增长 | 谁读它 |
| --- | --- | --- | --- |
| `Aggregate.attempts` | `usage/aggregate.go` | **无界**，每条 ~1149B | 调用明细、最终失败、请求详情、Dashboard 分解、Parquet 导出 |
| `Aggregate.summaries` | 同上 | **无界** | 最终失败列表、Dashboard 请求质量 |
| `Aggregate.hourly` | 同上 | 无界但极小（~200B/小时） | Dashboard 近 7 天 |
| `Aggregate.requests`（在途） | 同上 | 有界（在途数） | 活跃请求数 |
| 日 rollup | `domain/usage_rollup.go` | 每天 ≤ 200 键 × 5 维度，**天数无界** | **汇总页** |
| Parquet 分区 | `usage/parquet.go` | 无界，可被 `usage prune` 手动裁剪 | 离线导出、对账 |
| Ledger WAL | `ledger/` | **无界，无截断** | 账务权威，重建来源 |

三条对设计有决定性影响的事实，都已对代码核实：

- **汇总页读日 rollup，不读 attempts**（`internal/app/admin_usage_summary.go`）。所以裁剪 attempts **不会**破坏汇总数字。
- **Parquet 导出按 `attempt.Sequence > manifest.LastSequence` 选取**（`parquet.go:194`）。裁剪任何尚未导出的 attempt = 永久丢失。
- **`Reconcile` 只统计落在 Parquet 保留区间 `[firstRetained, LastSequence]` 内的 attempts**（`parquet.go:335`）。Aggregate 窗口短于 Parquet 窗口时，两者会对不上。

### 1.5 最终档案是 Ledger WAL —— 而它从不被删除

链条走到最后，真正长期留存的是哪一层？答案是 **Ledger WAL，因为没有任何东西会从它里面删掉任何记录**。它不是被选为归档的，它是**默认成为归档**的。

实测本仓库真实数据目录：`ledger.wal` 157304 字节 / 155 帧 = **每事件 1015 字节**。一次请求生命周期是 5 个事件（受理、预留、开始、结算、终态，见 ADR 0018），即约 **5 KB/请求**。

| 吞吐 | WAL 日增 | 年增 | 是否会被清理 |
| --- | --- | --- | --- |
| 1 次/秒 | 0.41 GB | 0.15 TB | **否** |
| 10 次/秒 | 4.08 GB | 1.46 TB | **否** |
| 100 次/秒 | 40.8 GB | 14.6 TB | **否** |

四层数据的真实保留情况：

| 层 | 会被清理吗 | 谁清理 |
| --- | --- | --- |
| Aggregate | 否（本方案要加） | — |
| Parquet 分区 | 是，但**手动 + 离线** | `halro usage prune`，默认 90 天 |
| 日 rollup | **否** | — |
| **Ledger WAL** | **否** | — |

Parquet 侧的量级作为对照：本仓库真实分区每天 16–76 KB（列式 + 每尝试一行，而非每请求五个事件）。它既小又可裁，不是问题所在。

**必须承认的一点：本方案把控制台的增长封住了，却没有碰最大的那一层。** 实施完第一阶段之后，10 次/秒的实例仍然每天涨 4 GB，永远不停——只不过涨在 WAL 上，而不再是每分钟重新序列化一次的 checkpoint 上。两者性质不同（一个是线性磁盘占用，一个是随寿命增长的 CPU 与内存），但前者也终将撑爆磁盘。

### 1.6 WAL 截断：格式已经预留，实现没有

`ledger.Watermark` 有一个 `Generation` 字段，而全代码库里它**恒为 1**，`Replay` 明确拒绝 1 以外的值（`internal/ledger/log.go:421`）。这正是「截断后新开一个文件、代号 generation 2」所需要的形状——**格式预留了，逻辑没写**。按 CLAUDE.md 的说法，这类为 1.0.0 之后演进保留的机制是能力而非债务。

WAL 截断为什么不能顺手做，以及真要做时的形状：

- 它是账务权威。删掉前缀就等于放弃「从 WAL 重建一切」这条底线，所以只能是**先封存再截断**：把前缀密封成不可变、带校验和、链可验证的归档文件，确认 Parquet 与 checkpoint 已完整覆盖该区间之后，才让活动 WAL 从下一个 generation 重新开始。
- 要同时回答：ADR 0016 的帧 MAC 链跨 generation 如何续接、`halro ledger verify` 如何验证一条被截断的链、备份与恢复如何处理多个 generation、以及「重放确定性」不变量在跨代重放时是否仍然成立。
- 这些没有一条能塞进本方案。**WAL 截断应当是一份独立的设计文档**，优先级不低于本方案——本方案解决的是「CPU 与内存随寿命恶化」，它解决的是「磁盘随流量无限增长」。

## 三、术语与口径

### 3.1 三个不同的窗口

它们回答三个不同的问题，**不应该绑成一个值**：

| 窗口 | 回答的问题 | 承载 |
| --- | --- | --- |
| **控制台窗口** | 界面能往回翻多久 | Aggregate |
| **归档窗口** | 合规要求留多久 | Parquet |
| **账务历史** | 账能重建到哪 | Ledger WAL |

把控制台窗口和归档窗口绑成一个值，就会有人为了满足合规而把 180 天塞进内存——那正是本方案要消除的曲线。

### 3.2 必须保持的不变量

1. **Ledger WAL 是账务权威**，本方案不删它的任何一条记录。裁剪只作用于派生数据。
2. **裁剪的上界是已导出水位**：`min(cutoff, manifest.LastSequence)`。未导出的 attempt 永远不裁。
3. **汇总数字不因裁剪而改变**：它来自 rollup，rollup 有自己的窗口（见 D4）。
4. **控制台窗口 ≥ 7 天**：Dashboard 的近 7 天曲线读 `hourly` 与 `summaries`，更短的窗口会打断它。
5. **裁剪后 `halro doctor` 仍然通过**：对账口径要么随之调整，要么裁剪与 Parquet 保持同步（见 D5）。

## 四、目标与非目标

### 4.1 目标

1. 消除 Aggregate 的无界增长，让 checkpoint 大小与耗时由窗口决定而非由实例寿命决定；
2. 控制台窗口可配置，默认值保守；
3. 裁剪在线自动执行，不需要停机、不需要记得跑 CLI；
4. 第二阶段：窗口成为设置中心里可选的运行时设置（30 / 60 / 90 / 180 天）；
5. 订正现有文档中关于可见窗口的错误陈述。

### 4.2 非目标

- 不删 Ledger WAL 的任何记录，也不在本方案中设计 WAL 截断——见 1.5 与 1.6，那是本方案封不住的一层，需要独立设计；
- 不改变汇总页的数据来源；
- 不为控制台提供超出其窗口的历史查询（要看更早的用 Parquet 导出）；
- 不在第一阶段引入运行时设置机制。

## 五、设计决策

### D1 · 裁剪谁

**采用：裁剪 `attempts` 与 `summaries`，保留 `hourly` 与 rollup。**

前两者是每请求/每尝试一条、体量随流量线性增长的明细。后两者是按小时/按天聚合的，体量与流量无关，且是 Dashboard 与汇总页的来源。

`hourly` 也应有窗口，但它的量级（~1.7 MB/年）不构成问题，可以放在第二阶段一并处理，或干脆按 Dashboard 需要的 7 天裁剪。

### D2 · 裁剪的边界条件

裁剪游标是 **`min(按时间的 cutoff, Parquet manifest.LastSequence)`**。

这条是硬约束而不是优化：导出按序列号水位选取待导出记录，裁掉尚未导出的 attempt 会让它永远不进归档。所以裁剪必须排在导出**之后**，并以导出水位为上界。

推论：**如果 Parquet 导出坏了或被关掉，裁剪必须停下来**，而不是继续裁。宁可让 Aggregate 涨，也不能静默丢历史。这一点必须有告警或至少一条 WARN。

### D3 · 压缩

**第一阶段不做，但在窗口落地后单独评估。**

理由见 1.2：压缩把 checkpoint 缩小约 40 倍，却让本已是瓶颈的序列化成本进一步上升，且对常驻内存毫无帮助。先有窗口，checkpoint 才小到「压缩是优化」而不是「压缩是续命」。

窗口落地后若仍要压缩，正确形状是 gzip 最快档（40 倍中的 39.9 倍已经拿到，多花一倍 CPU 只多换 1.4 倍），并且要同时处理 `RestoreCheckpoint` 的解压与「旧的未压缩 checkpoint 仍能读」——按 pre-1.0.0 规则，这里是就地改格式并提升版本号，让旧 checkpoint 被拒绝并重建，而不是留两条读路径。

### D4 · rollup 与 Parquet 的窗口

第一阶段**不动**这两者：

- rollup 体量小，且是汇总页的唯一来源，裁它等于让汇总数字凭空缩水；
- Parquet 已有 `retention_days` 与 `usage prune`，第一阶段只把它从「手动离线」改成「在线定时」是另一个改动面，应独立评审。

但要在文档里说清楚：**`usage.retention_days` 管的是归档，不是界面**。

### D5 · 对账口径

`Reconcile` 目前用 Aggregate 的 attempts 与 Parquet 比对。Aggregate 一旦有更短的窗口，两者必然对不上，而 `halro doctor` 会因此报错——这是必须先解决的，不能等到实施后发现。

两个方向：

- **A：让 Reconcile 知道 Aggregate 的窗口**，只在 `[max(firstRetained, aggregateFloor), LastSequence]` 区间比对。改动小，但对账覆盖面随之缩小。
- **B：对账改为 Parquet ↔ Ledger 重放**，不经过 Aggregate。覆盖面最完整，但要在离线路径上重放 WAL，成本高。

**倾向 A**，因为对账的目的是「导出没有丢/没有重复」，而不是「Aggregate 完整」；Aggregate 本来就是可重建的派生物。B 更正确但应作为独立议题。

### D6 · 配置形状（第一阶段）

```yaml
usage:
  # 归档保留多久，管的是 Parquet 分区，不是控制台可见范围
  retention_days: 90
  # 控制台「调用明细」「最终失败」能往回翻多久
  console_window_days: 30
```

分成两个键，而不是复用一个——正是 3.1 的理由。校验：`console_window_days` 至少 7（不变量 4），至多 `retention_days`（界面不该承诺归档都没有的东西）。

### D7 · 第二阶段：设置中心

`console_window_days` 从 `config.yaml` 提升为存储在 bbolt 的运行时设置，走与账期时区同一套机制：revision 校验、审计记录、热更新、控制台下拉选择 30 / 60 / 90 / 180。

这一步比第一阶段大得多——它涉及 store schema、Admin API、审计动作、以及「改小窗口会立刻丢数据」这个确认语义。**不应与第一阶段合并**。

## 六、实施切片

### S0 · 先把口径钉住

- 断言 Aggregate 在裁剪前后，汇总页数字不变（证明它读 rollup）；
- 断言裁剪不会裁掉 `Sequence > manifest.LastSequence` 的记录；
- 断言窗口 < 7 天时配置被拒；
- 记录当前 checkpoint 大小随 attempt 数的增长曲线，作为改造前后的对照。

### S1 · Aggregate 裁剪

- `Aggregate.PruneBefore(cutoff time.Time, exportedThrough uint64)`，同时维护 `attemptIndex` / `summaryIndex`；
- 在 `runUsageMaintenance` 的 Parquet tick 上，**导出成功之后**调用；
- 导出失败或未启用时不裁，并记录一次 WARN；
- checkpoint 版本不变（结构没变，只是内容少了）。

### S2 · 配置与文档

- 新增 `usage.console_window_days`，默认 30，校验区间 `[7, retention_days]`；
- 订正 Operator Guide 与最终失败列表页面关于「可见窗口」的表述；
- `configs/config.example.yaml` 补双语注释，说清两个窗口的分工。

### S3 · 对账口径

- 按 D5-A 让 `Reconcile` 知道 Aggregate 的下界；
- `halro doctor` 的 parquet 检查随之调整；
- 断言裁剪后 doctor 仍然通过。

### S4 · 观测

- checkpoint 大小、attempt 数、裁剪条数作为 Prometheus 指标；
- 裁剪被导出水位卡住时可告警——这是「归档坏了」的早期信号。

### S5 · 设置中心（独立评审）

见 D7。

## 七、验证计划

- `internal/usage`：裁剪的边界、索引一致性、checkpoint round-trip、裁剪后重放重建结果一致；
- `internal/app`：导出失败时不裁；doctor 在裁剪后通过；
- 对真实数据目录副本跑一次完整启动 + 裁剪 + `doctor` + `usage verify`；
- 改造前后各测一次 checkpoint 耗时与大小，作为方案有效性的证据，而不是断言。

## 八、验收标准

1. checkpoint 大小与耗时由 `console_window_days` 决定，不再随实例寿命增长；
2. 汇总页数字在裁剪前后完全一致；
3. 尚未导出的记录永不被裁，且导出停摆时裁剪也停摆并有 WARN；
4. Dashboard 近 7 天曲线不受影响；
5. `halro doctor` 与 `halro usage verify` 在裁剪后通过；
6. 文档不再声称可见窗口等于 `retention_days`；
7. 现有数据目录直接升级，无需重新初始化。
