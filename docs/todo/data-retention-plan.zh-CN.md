# 数据保留与压缩：设计与实施方案

- 状态：第一、二阶段已实施（S0–S6）；第三、四阶段待实施
- 日期：2026-09-02
- 文档语言：中文
- 适用范围：Ledger WAL、Usage Aggregate 与 checkpoint、Parquet 导出、日 rollup、控制台「用量与调用」、设置中心
- 数据迁移：第一、二阶段不需要重新初始化。第三阶段同样不需要：段清单在第一次滚动
  时才出现，未开启封存的目录一个字节都不变；bbolt 迁移 34 给链 checkpoint 补上
  `generation: 1`（封存前的每一条 checkpoint 描述的都正是第 1 代）

## 一、问题

**这个实例里没有任何一层数据会被自动清理。**

四层派生与权威数据各自增长，只有一层有清理机制，而那一层需要停机手动执行，并且不是控制台读的那一层。

| 层 | 内容 | 会被清理吗 | 谁清理 |
| --- | --- | --- | --- |
| Ledger WAL | 账务权威 | **否** | — |
| Usage Aggregate + checkpoint | 控制台工作集 | **否** | — |
| Parquet 分区 | 查询归档 | 是，但**手动 + 离线** | `halro usage prune`，默认 90 天 |
| 日 rollup | 汇总页唯一来源 | **否** | — |

两条不同性质的曲线，必须分开处理：

- **越跑越慢**：Aggregate 无界增长，而 `TakeCheckpoint` 每分钟把全部历史重新序列化一次，所以 checkpoint 的耗时与实例**寿命**成正比，与负载无关。这条先致命。
- **越跑越满**：WAL 从不截断，磁盘占用与**流量**成正比，永不回收。这条最终也致命。

### 1.1 越跑越慢：checkpoint 的成本随寿命上升

控制台「最终失败」和「调用明细」读的是内存里的 Usage Aggregate（`internal/usage/query.go`、`internal/usage/failures.go`）。`Aggregate.attempts` 与 `Aggregate.summaries` 只 append，从不裁剪，并随 checkpoint 整体写进 bbolt。

`TakeCheckpoint`（`internal/usage/aggregate.go`）把全部历史 `json.Marshal` 一次，默认间隔 **60 秒**（`internal/config/default.go`）。

| 历史长度（10 次/秒） | 序列化耗时 | 与 60 秒间隔的关系 |
| --- | --- | --- |
| 1 天 | 约 1.5 s | 2.5% |
| 30 天 | 约 45 s | **75%** |
| 90 天 | 约 135 s | **追不上自己的节拍** |

即便只有 1 次/秒，90 天也意味着每分钟拿约 13 秒 CPU 反复序列化同一段历史。同时：

- **常驻内存**保存全部历史，压缩帮不上忙；
- **bbolt 单事务**里塞一个几十 GB 的值，本身就是页面翻搅问题；
- **崩溃后重建**要重放整条 WAL，而 WAL 自身也无截断。

`QueryAttempts` 与 `QueryFailedRequests` 还是从切片尾部线性倒扫过滤。游标分页在前，但筛选命中率低时会扫过整个切片。

### 1.2 越跑越满：WAL 是默认的最终档案

链条走到最后，真正长期留存的是 Ledger WAL——它不是被选为归档的，而是**因为没有任何东西删它**而成为归档。

实测本仓库真实数据目录：`ledger.wal` 157304 字节 / 155 帧 = **每事件 1015 字节**。一次请求生命周期是 5 个事件（ADR 0018），即约 **5 KB/请求**。

| 吞吐 | WAL 日增 | 年增 |
| --- | --- | --- |
| 1 次/秒 | 0.41 GB | 0.15 TB |
| 10 次/秒 | 4.08 GB | 1.46 TB |
| 100 次/秒 | 40.8 GB | 14.6 TB |

作为对照，Parquet 侧真实分区每天只有 16–76 KB（列式，且每尝试一行，而非每请求五个事件）。它既小又可裁，不是问题所在。

### 1.3 现有 `usage.retention_days` 管不到界面

`internal/config/config.go` 有 `usage.retention_days`，默认 90。它**只**被 `halro usage prune` 使用——一个手动、离线（要停机取数据目录锁）的 CLI 命令，只裁剪 Parquet 分区，而控制台不读 Parquet。

设置里的 90 天，和界面上看到的记录，是两回事。

### 1.4 一处必须订正的文档（已订正）

旧方案 `request-failure-diagnostics-plan.zh-CN.md` 说「可见窗口等于 Usage 保留窗口」。**这句话当时是错的**：可见窗口是无限的，`retention_days` 与之无关。该文档已就地加订正说明；控制台上的对应措辞早先已随横幅一起移除。

## 二、实测数据

所有数字来自本仓库真实数据目录，或对真实结构体的直接测量。方法记在这里，以便日后重测。

### 2.1 单位大小

| 对象 | 大小 | 来源 |
| --- | --- | --- |
| checkpoint 里一条已结算 `AttemptEvent` | 1149 B | `json.Marshal` 实测 |
| WAL 一帧 | 1015 B | 真实 `ledger.wal` 157304 B / 155 帧 |
| 一次请求的 WAL 占用 | 约 5074 B | 5 个事件 |
| Parquet 一天分区 | 16–76 KB | 真实 `data/usage/date=*` |

### 2.2 压缩率（含一处对早期结论的纠正）

**早期用合成的同质数据测出 40 倍，那个数字不可用。** 真实分布下的重测：

| 对象 | 原始 | gzip 最快档 | 压缩比 | 耗时 |
| --- | --- | --- | --- | --- |
| 真实 `ledger.wal` | 157 KB | 28 KB | **5.6×** | 1 ms |
| 真实 `ledger.wal`（默认档） | 157 KB | 25 KB | 6.3× | 1 ms |
| checkpoint，一天 10 次/秒、模型/项目/成本/状态混合 | 926 MB | 126 MB | **7.4×** | 2.1 s |

WAL 比 checkpoint 压得差，是因为每帧带 MAC 与校验和——那部分是高熵的，压不动。

结论：**真实压缩比在 5.6–7.4 倍之间**，不是一个数量级的改变。它能把磁盘曲线的斜率压下来，但压不掉曲线本身，也压不掉 CPU 与内存。

## 三、最终形态：五层存储与压缩

| 层 | 内容 | 格式 | 压缩 | 保留 | 10 次/秒的量级 |
| --- | --- | --- | --- | --- | --- |
| **0 · 活动 WAL** | 账务权威，热路径 | 帧 + MAC 链（不变） | **不压** | 按封存触发，非按天 | 封存前 ≤ 设定上限 |
| **1 · 封存 WAL 段** | 最终档案，可完整重建 | 密封只读段，新 generation | **gzip 最快档** | 长期 / 可外移 | 4.08 → **约 0.73 GB/天** |
| **2 · Parquet 分区** | 查询归档 | 列式，每尝试一行 | **显式 zstd** | `retention_days`，默认 90，改为在线自动 | 16–76 KB/天 |
| **3 · Aggregate + checkpoint** | 控制台工作集 | JSON in bbolt | **不压，已量并否决（S6）** | `console_window_days`，默认 30，高吞吐需调小 | 27.1 GB |
| **4 · 日 rollup** | 汇总页唯一来源 | bbolt 行 | 不需要 | 长于 Parquet | 约 1000 行/天 |

### 3.1 为什么活动 WAL 不压缩

它在 fsync durability barrier 上。压缩会直接加到写路径延迟里，而这一层的量本来就该由**封存与截断**控制，不该由压缩控制。用压缩去救一个从不截断的日志，是把磁盘问题换成延迟问题。

### 3.2 为什么封存段才是压缩的主战场

它是唯一能把「最终档案」的曲线掰下来的地方：4.08 GB/天 → 约 0.73 GB/天，1.46 TB/年 → 约 260 GB/年。段是只读、一次成型的，压缩发生在封存那一刻，不在任何请求路径上。

### 3.3 checkpoint 不压缩 —— 窗口落地后重量的结论

原本预期「窗口之后压缩是锦上添花」。窗口落地后重算，**结论翻转为不做**：30 天窗口、10 次/秒下序列化已占 45 秒，压缩再加约 63 秒，一个 60 秒周期根本跑不完。省 23 GB 磁盘换彻底追不上节拍，不是取舍，是退步。

同一组数字也说明窗口本身在高吞吐下不够，真正的下一级杠杆是增量 checkpoint（S9）。详见第六章 S6。

## 四、术语与不变量

### 4.1 三个不同的窗口

它们回答三个不同的问题，**不应该绑成一个值**：

| 窗口 | 回答的问题 | 承载 |
| --- | --- | --- |
| **控制台窗口** | 界面能往回翻多久 | Aggregate |
| **归档窗口** | 合规要求留多久 | Parquet |
| **账务历史** | 账能重建到哪 | WAL（活动段 + 封存段） |

把控制台窗口和归档窗口绑成一个值，就会有人为了满足合规把 180 天塞进内存——那正是本方案要消除的曲线。

### 4.2 必须保持的不变量

1. **账务可重建性不下降。** 封存段 + 活动 WAL 合起来，必须能重建出与今天等价的账务状态。删除只发生在「已封存且已校验」之后。
2. **裁剪 Aggregate 的上界是已导出水位**：`min(cutoff, manifest.LastSequence)`。未导出的 attempt 永远不裁——导出按 `Sequence > manifest.LastSequence` 选取（`parquet.go:194`），裁掉未导出的等于永久丢失。
3. **导出停摆则裁剪停摆。** 宁可让 Aggregate 涨，也不能静默丢历史。必须有 WARN，最好有指标。
4. **汇总数字不因裁剪而改变。** 它来自日 rollup（`internal/app/admin_usage_summary.go`），不来自 attempts。
5. **控制台窗口 ≥ 7 天。** Dashboard 的近 7 天曲线读 `hourly` 与 `summaries`。
6. **裁剪后 `halro doctor` 与 `halro usage verify` 仍然通过。**
7. **封存不改写历史。** 段一旦密封即只读；重放跨代必须与未截断时得到相同结果（「重放确定性」不变量）。

## 五、设计决策

### D1 · 裁剪 Aggregate 的哪些结构

**裁 `attempts` 与 `summaries`，保留 `hourly` 与 rollup。**

前两者每请求/每尝试一条，随流量线性增长。后两者按小时/按天聚合，体量与流量无关，且分别是 Dashboard 与汇总页的来源。

`hourly` 也应有窗口，但量级只有约 1.7 MB/年，可与 Dashboard 需要的 7 天一并处理，优先级低。

### D2 · 裁剪的边界与顺序

裁剪游标是 `min(按时间的 cutoff, Parquet manifest.LastSequence)`，且必须排在导出**之后**执行。这是不变量 2 与 3 的直接后果，不是优化。

### D3 · rollup 的窗口

第一阶段不动。它是汇总页的唯一来源，裁它等于让汇总数字凭空缩水；且体量小（每天 ≤ 200 键 × 5 维度）。若要设窗口，必须**长于** Parquet 归档窗口，否则会出现「归档里有、汇总里没有」。

### D4 · 对账口径

`Reconcile` 目前用 Aggregate 的 attempts 与 Parquet 比对（`parquet.go:335`）。Aggregate 一旦有更短的窗口，两者必然对不上，`halro doctor` 会因此报错——**必须在实施前解决，不能事后发现**。

- **A：让 Reconcile 知道 Aggregate 的下界**，只在 `[max(firstRetained, aggregateFloor), LastSequence]` 区间比对。改动小，覆盖面随之缩小。
- **B：对账改为 Parquet ↔ Ledger 重放**，不经过 Aggregate。覆盖面最完整，成本高。

**采用 A。** 对账的目的是「导出没有丢/没有重复」，而不是「Aggregate 完整」——Aggregate 本来就是可重建的派生物。B 更彻底，应作为独立议题。

### D5 · 压缩的取舍

见第三章。三条决定：活动 WAL 不压；封存段压（gzip 最快档，5.6×，1 ms 级）；checkpoint 的压缩排在窗口之后（7.4×，2.1 s/天量级）。

选最快档而非默认档：真实数据上默认档只多换 12%（5.6× → 6.3×），却要多花一倍 CPU。

checkpoint 若要压缩，按 pre-1.0.0 规则**就地改格式并提升版本号**，让旧 checkpoint 被拒绝并从 Ledger 重建，而不是留两条读路径。

### D6 · WAL 封存与截断的形状

`ledger.Watermark` 有 `Generation` 字段，全代码库**恒为 1**，`Replay` 明确拒绝 1 以外的值（`internal/ledger/log.go:421`）。这正是「截断后新开一个文件、代号 generation N+1」所需要的形状——**格式预留了，逻辑没写**。

设计骨架：

1. **触发**：活动 WAL 超过设定大小，**且**其前缀覆盖的所有账期都已满足封存前置条件。按大小而非按天，因为磁盘压力来自字节。
2. **封存前置条件**（缺一不可，任一不满足则不封存并告警）：
   - 该前缀内所有 attempt 已导出进 Parquet 且 `Verify` 通过；
   - usage checkpoint 与 rollup 的水位已越过该前缀；
   - 该前缀的帧链已通过 `ledger verify`。
3. **封存动作**：把前缀密封成只读段（`ledger-<generation>.seg.gz`），记录段的起止 sequence、起止 offset、链头哈希与整段校验和进一个段清单；确认段可读、校验通过之后，活动 WAL 从 generation+1 重新开始。
4. **必须回答的四个问题**（这也是它不能塞进第一阶段的原因）：
   - ADR 0016 的帧 MAC 链**跨 generation 如何续接**——新代的第一帧要锚定上一代的链头；
   - `halro ledger verify` 如何验证一条被截断的链——它要能沿段清单回溯，而不是只看活动文件；
   - **备份与恢复**如何处理多个 generation——`.hmbk` 目前按名字暂存单个 `ledger.wal`；
   - **重放确定性**在跨代重放时是否仍成立——`Replay(from Watermark)` 的语义要扩展到「从某代某偏移开始」。

### D7 · 配置形状

第一、二阶段：

```yaml
usage:
  # 归档保留多久，管的是 Parquet 分区，不是控制台可见范围
  retention_days: 90
  # 控制台「调用明细」「最终失败」能往回翻多久
  console_window_days: 30
```

校验：`console_window_days` 至少 7（不变量 5），至多 `retention_days`（界面不该承诺归档都没有的东西）。

第三阶段追加：

```yaml
ledger:
  seal:
    enabled: false          # 默认关闭，开启是一次明确决定
    max_active_bytes: 8GiB  # 活动 WAL 超过此值触发封存
    compress: true
```

### D8 · 设置中心（最后阶段）

`console_window_days` 从 `config.yaml` 提升为存储在 bbolt 的运行时设置，走与账期时区同一套机制：revision 校验、审计记录、热更新、控制台下拉选择 30 / 60 / 90 / 180 天。

它涉及 store schema、Admin API、审计动作，以及「改小窗口会立刻丢数据」这个确认语义。**不应与前面任何阶段合并。**

## 六、实施切片

### 第一阶段 · 封住「越跑越慢」

**S0 · 先把口径钉住**
- 断言裁剪前后汇总页数字不变（证明它读 rollup）；
- 断言裁剪不会裁掉 `Sequence > manifest.LastSequence` 的记录；
- 断言窗口 < 7 天时配置被拒；
- 记录改造前的 checkpoint 大小与耗时随 attempt 数的曲线，作为对照。

**S1 · Aggregate 裁剪**
- `Aggregate.PruneBefore(cutoff time.Time, exportedThrough uint64)`，同时维护 `attemptIndex` / `summaryIndex`；
- 在 `runUsageMaintenance` 的 Parquet tick 上，导出成功**之后**调用；
- 导出失败或未启用时不裁，记录一次 WARN；
- checkpoint 版本不变（结构没变，只是内容变少）。

**S2 · 配置与文档**
- 新增 `usage.console_window_days`，默认 30，校验区间 `[7, retention_days]`；
- 订正 Operator Guide 与最终失败列表页面关于「可见窗口」的表述；
- `configs/config.example.yaml` 补双语注释，说清两个窗口的分工。

**S3 · 对账口径**
- 按 D4-A 让 `Reconcile` 知道 Aggregate 的下界；
- `halro doctor` 的 parquet 检查随之调整；
- 断言裁剪后 doctor 通过。

**S4 · 观测**
- checkpoint 大小、attempt 数、裁剪条数、上次裁剪时间作为 Prometheus 指标；
- 裁剪被导出水位卡住时可告警——这是「归档坏了」的早期信号。

### 第二阶段 · 归档自动化与压缩

**S5 · Parquet 在线自动裁剪 + 显式 zstd**
- 把 `usage prune` 的逻辑接到维护循环上，不再要求停机；
- 给 `parquet.NewGenericWriter` 显式指定压缩编码；
- 旧分区不重写，只对新分区生效。

**S6 · checkpoint 压缩 —— 已量，否决**

窗口落地后重算，结论是不做，而且这次的理由比第一次更硬。30 天窗口、10 次/秒：

| | 值 |
| --- | --- |
| checkpoint 原始大小 | 27.1 GB |
| gzip 最快档之后 | 3.67 GB |
| `json.Marshal` 耗时 | 约 45 s |
| gzip 追加耗时 | 约 63 s |
| checkpoint 间隔 | **60 s** |

序列化已经占掉一分钟的四分之三，压缩再加一分钟——**加上压缩之后它连一个周期都跑不完**。省下的 23 GB 磁盘换来的是彻底追不上节拍。

### S6 暴露的更大问题：窗口本身在高吞吐下不够

同一组数字说明 30 天默认值只在低吞吐下成立：

| 吞吐 | 30 天窗口的 checkpoint | 每次序列化耗时 | 占 60 s 间隔 |
| --- | --- | --- | --- |
| 0.1 次/秒 | 0.28 GB | 约 0.5 s | 1% |
| 1 次/秒 | 2.8 GB | 约 4.5 s | 7% |
| 10 次/秒 | 27.1 GB | 约 45 s | **75%** |
| 100 次/秒 | 271 GB | 约 450 s | **追不上** |

窗口把曲线从「随寿命增长」压成了「随吞吐 × 窗口长度」——这是本质改善，实例不再因为跑得久而变慢。但在 10 次/秒以上，**唯一的调节手段是把窗口调短**，而那是在拿可见历史换 CPU。

真正的下一级杠杆是**增量 checkpoint**：只序列化上次之后新增的部分，而不是每次重写全量。那会把每次 checkpoint 的成本从 O(窗口) 降到 O(增量)，与窗口长度脱钩，压缩届时才重新变得有意义。它需要改 checkpoint 的格式与恢复路径，属于独立设计。

**S9 · 增量 checkpoint（独立评审）** —— 见上。在它落地之前，高吞吐实例应按上表调小 `console_window_days`，文档必须给出这张表而不是只给一个默认值。

### 第三阶段 · 封住「越跑越满」

**S7 · WAL 封存与截断** —— 已完成（2026-09-02）。

**与 D6 骨架的一处偏离，以及为什么。** D6 写的是「把前缀密封成只读段，活动 WAL
从 generation+1 重新开始」。真按前缀切，切点之后剩下的帧就要被搬进新文件、偏移
量整体前移——而 usage checkpoint 存的正是 `{generation, offset, sequence}` 这种
水位。偏移一旦重编号，一个封存前写下的水位就会指向另一条记录，而且没有任何东西
会报错。改成**整代滚动**：整个活动文件成段，新文件从空开始，任何一帧的偏移终生
不变，跨代重放因此是平凡正确的。代价是封存点只能落在滚动那一刻，而不是操作者指
定的任意位置——这个代价换的是「不需要证明偏移重编号是安全的」。

**不删除任何东西。** 第三章表格里封存段的保留策略本来就是「长期 / 可外移」：账
务要能从完整历史重建，所以段是永久归档，只是小 5.4 倍且可以被搬走。因此 D6 的三
条封存前置条件不再是滚动的前置条件（滚动不丢任何东西），而是**压缩**的前置条件
——一个已经完全进入 Parquet 归档并越过 usage checkpoint 的代，才是操作者可以安全
搬离本机的代，让「已压缩」和「可搬走」是同一件事。

**四个必答问题的答案**（`internal/ledger/seal_test.go` 各一节，
`internal/app/ledger_seal_contract_test.go` 覆盖备份与 verify 命令）：

1. **跨代链续接**——新代第一帧的 previous-hash 就是上一代的链头，记在段清单的
   `end_hash` 里；打开时用它作为校验器的锚点。把活动文件首帧的 previous-hash 改
   成全零（即「把新代从历史上摘下来」）会被 `ErrTampered` 拒绝。
2. **跨代 verify**——`VerifySegments` 沿段清单逐代校验：锚点必须接得上前一代的
   链头、存储校验和必须匹配、扫描必须停在清单声称的长度与序号上。段被改一个
   字节会被发现，段被删除返回 `ErrSegmentMissing`。
3. **跨代备份恢复**——`Snapshot` 之外新增 `StageSegments`，把每个段与段清单一起
   放进 `.hmbk`；恢复后逐条重放与源实例完全一致，`ledger verify` 报出
   `sealed_generations` 与其中的帧数。
4. **跨代重放确定性**——封存前后、压缩前后，`Replay(Watermark{})` 与
   「从一个封存前取得的水位续放」都返回逐条相同的结果。

**顺带查出并修掉的真实缺陷。** `halro doctor` 与 `halro usage export` 都通过
`ledger.InspectReplay` 重建状态，而它只读活动文件——封存之后 doctor 把真实数据目
录报成 `committed sequence 0 at offset 0; chain authenticated (0 frames)`，即一个
空账本，且状态仍是 `pass`。这是「校验范围随封存缩小到零、却仍然报通过」的那类
失败。`Inspect`/`InspectReplay` 已改为跨代，doctor 的链校验也改为先校验封存段。

**真实数据目录副本上的完整验证**（155 帧、157,304 字节的 `ledger.wal`）：

| 步骤 | 结果 |
| --- | --- |
| `ledger seal` | 157,304 → 28,949 字节，**5.43×**（方案预估 5.6×） |
| `ledger verify` | 155 帧全部认证，链头哈希与封存前逐字节相同 |
| `doctor` | healthy，`committed sequence 155`，「155 frames across 1 sealed generations」 |
| `usage compact` 重放重建 | 生成的 manifest 与未封存副本**逐字节相同** |
| `usage verify` | ledger 31 / parquet 31，无缺失、无重复 |
| `halro start` | 在已封存目录上正常启动并绑定监听 |

### 第四阶段 · 设置中心

**S8 · 把窗口交给设置中心** —— 已完成（2026-09-02）。

`console_window_days` 从 `config.yaml` 提升为 bbolt 中的运行时设置，走与记账时区
同一套机制：`SeedInstanceUsageSettings` 在首次启动时以配置值为初值写入，此后配置
文件不再有发言权；修改经 `PUT /admin/api/v1/settings/usage`，带 revision 校验、
`settings.usage.update` 审计记录与热更新，裁剪读的是存储值而不是配置值。

**确认语义只在缩短时出现。** 加长不丢任何东西，问一次只会训练操作者习惯性点确定；
缩短会在下一个导出周期把窗口外的调用记录裁出内存，所以服务端要求
`acknowledge_trim`，否则返回 `console_window_trim_unacknowledged`，控制台据此弹出
说明后果的确认框：记录本身仍在 Parquet 归档里、可用 halro usage 导出，但把窗口改
回来不会把它们找回来——那需要从账本重放重建。

另外两条边界仍在服务端：低于 7 天被拒（运行总览读 7 天），超过
`usage.retention_days` 被拒（界面不该承诺归档已经没有的历史）——后者同时决定下拉
框里显示哪些预设，一个只会被服务端拒绝的选项不如不给。

**测试**：`internal/app/admin_usage_settings_test.go` 覆盖未确认的缩短被拒、超出
归档被拒、低于下限被拒、revision 冲突、审计落盘，以及「改完重启后配置文件不会把
它改回去」；`web/src/pages/UsageWindowForm.test.tsx` 覆盖缩短弹确认、加长直接保存、
不提供超出归档的预设、配置文件失效提示。

## 七、验证计划

- `internal/usage`：裁剪边界、索引一致性、checkpoint round-trip、裁剪后重放重建结果一致；
- `internal/app`：导出失败时不裁；doctor 在裁剪后通过；
- `internal/ledger`（第三阶段）：跨代链续接、跨代 verify、跨代 `Replay` 确定性、
  滚动中途崩溃在改名两侧各自的恢复方向；
- 对真实数据目录副本跑完整启动 + 裁剪 + `doctor` + `usage verify`；
- **改造前后各测一次 checkpoint 耗时与大小、WAL 日增与封存后占用**，作为方案有效性的证据，而不是断言。

## 八、验收标准

1. checkpoint 大小与耗时由 `console_window_days` 决定，不再随实例寿命增长；
2. 汇总页数字在裁剪前后完全一致；
3. 尚未导出的记录永不被裁；导出停摆时裁剪也停摆并有 WARN 与指标；
4. Dashboard 近 7 天曲线不受影响；
5. `halro doctor` 与 `halro usage verify` 在裁剪后通过；
6. Parquet 裁剪不再需要停机；
7. 文档不再声称可见窗口等于 `retention_days`；
   ✅ 且不再声称它由 `config.yaml` 决定——第四阶段之后配置文件只是初值；
8. （第三阶段）封存后账务可从「封存段 + 活动 WAL」完整重建，结果与未封存时逐条一致；
   ✅ 已在真实数据目录副本上验证（重放重建出的 manifest 逐字节相同）；
9. （第三阶段）**活动** WAL 不再随运行时间单调上升，归档缩小到约 1/5.4 且可外移；
   历史本身仍然全量保留，因为账务必须能从它重建；
10. 现有数据目录直接升级，前两阶段无需重新初始化。
