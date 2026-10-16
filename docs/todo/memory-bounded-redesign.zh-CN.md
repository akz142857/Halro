# 内存有界的历史存储：重新设计

- 状态：P0 完成（#406）；P1 完成（#407、#408），结果见 [P0 基线](../verification/memory-bounded-baseline.zh-CN.md) §5；P2 实施细则见 §12，待确认。
- 取代：[内存有界状态与历史持久化改造方案（第一版，已归档）](../prd/memory-bounded-state-design-v1.zh-CN.md)。第一版的实现保留在 `memory-bounded-state` 分支，不合并。
- 适用范围：单机（Standalone）。HA 代码已移到 `ha` 分支（#404），本方案完成后 HA 基于新的 `main` 继续，见 §9。
- 基线：`main` @ `dc704030`，即 v0.8.5 加上之后的非 HA 修复。生产运行的正是 v0.8.5（GHCR 镜像 digest 与 `v0.8.5` 一致）。

## 1. 问题

### 1.1 生产上看到的

v0.8.5 在 1 GiB 内存限制下多个任务以 137 退出（被 SIGKILL），改成 2 GiB 后稳定运行，读数 1.188 GiB / 2 GiB，CPU 0.05%。没有 heap profile，所以**不知道生产上哪一项占得最多**。请求速率也不知道。

### 1.2 代码里确认的原因

以下各条已在 `main` @ `dc704030` 上核对（不是沿用第一版的分析）：

1. **Ledger 启动时从第一帧全量回放进内存。** `internal/app/runtime.go:355` 用 `ledger.NewState()` 和 `ledgerLog.Replay(ledger.Watermark{}, …)` 重建。`ledger.State`（`internal/ledger/event.go:491`）的 14 个 map 里，`settled`、`leaseRecords`、`eventDigests`、`workUnits`、`runs`、`requestRuns`、`settledByWorkUnit`、`governanceIdempotency` 随历史只增不减。重启不能缩小。
2. **Usage 在内存里保存控制台窗口内的全部明细。** 默认窗口 30 天（`config.DefaultConsoleWindowDays`）。按 1 请求/秒算，30 天约 259 万条明细；第一版测得结构体本身约 888 B/条，仅元素空间就约 2.14 GiB。**按天数限制，不能限制内存**，内存随请求速率增长。
3. **Governance 启动时全量回放**（`runtime.go:1033` 起）。
4. **部署价格时间线全量加载后缓存**（`store_pricing.go` 的 `auditedDeploymentTimeline`）。
5. **为读一个水位复制全部明细**：`loadUsageCheckpoint`、`admin_usage_summary` 调 `Snapshot`；导出时多份数组同时存活。
6. **WAL segment 目录全量常驻**（`segments.json` 整体读入）；开启封存后随段数增长。
7. **请求结束前崩溃会残留活跃状态**：`requestRuns`/`activeByWorkUnit` 只在 `RequestFinalized` 时清理。

第一版的本地合成实验（Ledger + Usage，不含价格快照）：1 万请求约 36 MiB，10 万约 379 MiB，近似线性。

## 2. 目标与非目标

**目标**

- **内存与历史规模无关，也与请求速率无关。** 只随在途请求数、项目数和配置的缓存大小变化。
- **数据默认永久保留**，保留期可以设置。
- **控制台默认查看最近 30 天**，查询速度不明显变慢。
- **吞吐和每请求的 fsync 次数不比现在差。**
- **启动时间与历史规模无关。**

**非目标**

- 不做 HA。HA 在 `ha` 分支，§9 说明怎么接回来。
- 不自建存储引擎，不自建带认证的 B 树。
- 不删除 Ledger WAL 的历史（它是账务权威，见 D6）。

## 3. 第一版的教训

第一版用了 15 天，新增约 15.4 万行生产代码，最终 selected 引擎比原引擎慢 6.6 倍（并发 1）到 17 倍以上（并发 16），2 GiB 下的容量探针没有通过。本版把下面几条作为约束：

1. **先定预算，再定方案。** 第一版没有吞吐和写放大的预算，到第 12 天才第一次端到端测性能。本版的预算写在 §6，开工前确认。
2. **先做端到端原型。** 第一周就在真实请求路径上量预算，不先逐层追求正确性证明。
3. **不自建认证存储。** 第一版每个请求约 33 次 fsync、约 2 万次页读取，根源在于要求派生数据也有密码学级的内容认证。本版的认证决定见 D3。
4. **写入路径不加同步工作。** 派生数据由后台从 WAL 追赶，不在请求路径上同步写。
5. **限定范围。** 只做单机；每个阶段有可以测量的退出条件，不满足就停下来裁定，不继续加机制。

## 4. 关键决定

### D1 数据分四层

| 层 | 内容 | 位置 | 是否权威 |
| --- | --- | --- | --- |
| 权威日志 | Ledger WAL、Governance journal、Audit log | 现有文件，不变 | 是 |
| 热状态 | 账务判断要用的最小集合：进行中的请求与预留、未关闭期间的余额、活跃的 WorkUnit/Run、近期事件摘要 | 内存，有上限；定期整体落盘为快照 | 否，可由 WAL 重建 |
| 历史索引 | 已结算明细、请求摘要、已关闭的 WorkUnit/Run、全部事件摘要、治理幂等、小时汇总 | 磁盘上独立的 bbolt 文件 `history.db` | 否，可由 WAL 重建 |
| 归档 | Parquet 分区 | 现有，不变 | 否 |

### D2 内存里放什么（问题 A 的决定）

**内存不按天数保留数据。** "30 天"只是控制台默认的查询范围，由磁盘上的历史索引提供；30 天之外的数据同样在索引里，查询方式相同。

内存只放两类东西：

1. **热状态**：大小只随在途请求数、项目数和未关闭的期间数变化，与历史无关。
2. **固定大小的缓存**：例如最近若干小时的汇总、最近查询过的页。上限由配置给出，超出就淘汰。

理由：生产请求速率未知，任何"按时间保留在内存"的规则都会让内存随速率增长，这正是现在 Usage 的问题。bbolt 通过 mmap 读取，热点页留在操作系统页缓存里，30 天的查询在实际使用中仍然很快；页缓存在内存紧张时可以回收，不会像 Go heap 那样导致 OOM。

### D3 派生数据怎么防篡改（问题 B 的决定）

**决定：不对派生数据做密码学级的内容认证（不建 Merkle 树）；改用"快照整体 MAC + 行级 MAC + 与权威合计对账 + 随时可从 WAL 重建"。**

依据是现有威胁模型（`docs/architecture/threat-model.md`、ADR 0016）划定的边界：

- 主机 root 被信任；持有 `master.key` 的人可以重算任何 MAC，本来就防不住。
- 要防的是**能写数据目录、但没有主密钥的人**：离线修改文件、截断、换入别处的文件、被攻破的备份任务。
- Ledger WAL 已经有 HMAC 和哈希链（ADR 0016），是唯一的账务权威。

在这条边界下，各类派生数据的处理：

| 数据 | 被篡改的后果 | 保护方式 | 成本 |
| --- | --- | --- | --- |
| 热状态快照（余额、在途预留等） | 预算可被绕过，后果严重 | 整个快照算一个 HMAC，并记录对应的 WAL 位置；启动时先验证 MAC，再从 WAL 回放快照之后的部分 | 快照大小有上限（D2），整体算 MAC 很便宜 |
| 历史索引的每一行 | 报表和导出数字错误 | 每行带 HMAC，覆盖键、内容和 WAL 序号，防止修改和跨行替换 | 每行一次 HMAC，可忽略 |
| 历史索引中被删除的行 | 报表少算 | 按项目、按期间的合计记在热状态快照里（受 MAC 保护），查询汇总时与索引对账，不一致就报错 | 每期间一条合计 |
| 整个索引被换成旧版本 | 数据落后 | 索引记录自己追到的 WAL 位置；启动时与 WAL 头部比较，落后就从 WAL 追赶。回滚只会导致重新补齐 | 无 |
| 任何怀疑 | — | `halro history verify` 流式扫描 WAL 与索引逐行核对，内存有界；`halro history rebuild` 从 WAL 重建 | 离线命令 |

这样做能发现修改、删除、替换和回滚，防护等级和 Ledger WAL 本身一致。做不到的只有一件事：**不重放 WAL，就无法在启动时证明索引里的每一行都齐全**。这一点由合计对账和离线 `verify` 补上。同样的弱点在第一版里要用每请求约 33 次 fsync 的代价来换，不值得。

### D4 写入路径：请求只写 WAL

- 请求路径与现在相同：追加 WAL（fsync），然后更新内存热状态。**不增加 fsync。**
- 历史索引由一个后台任务从 WAL 读取，**按批写入**（一个 bbolt 事务写一批），追赶延迟目标为秒级。
- 控制台显示索引已追到的位置；查询结果最多落后几秒。
- 热状态快照定期写出（例如每分钟一次，或累积一定量的 WAL 后），写出的是一个有上限的小文件，**不暂停业务**：先在锁内复制热状态，再在锁外编码、计算 MAC 和写盘。

### D5 启动

1. 读取最新的热状态快照，验证 MAC，取得它对应的 WAL 位置。
2. 只回放该位置之后的 WAL。回放时间与快照间隔内的写入量成正比，与历史无关。
3. 快照缺失或验证失败时，从 WAL 全量重建热状态。这是恢复路径，不是常规路径；重建只保留热状态，内存依然有上限（已结算的事件只更新合计，不进内存）。
4. 历史索引在后台追赶，**不阻塞 ready**。追赶期间控制台提示"历史数据追赶中"。

### D6 数据保留

| 数据 | 默认 | 可设置 |
| --- | --- | --- |
| Ledger WAL | 永久 | 不可删除。已有的 WAL 封存（`internal/ledger/seal.go`）可以滚动和压缩段文件，但不丢事件 |
| 历史索引（明细） | 永久 | `history.retention`；到期的明细从索引删除，汇总保留 |
| 小时 / 日汇总 | 永久 | 不删除，体积只随维度增长 |
| Parquet 归档 | 维持现有 `usage.retention_days` | 维持现有 |
| 控制台默认查看范围 | 30 天 | 维持现有设置中心的 `console_window_days`；它只决定默认查询范围，不再决定内存 |

如果以后需要删除 Ledger WAL 的历史，要另行设计链的截断锚点和审计方式，不在本方案内。

### D7 存储选型

历史索引用一个**独立的 bbolt 文件**（`history.db`），理由：

- bbolt 已经是现有依赖，团队熟悉，有备份和校验的先例。
- 和 `halro.db`（元数据）分开，避免后台批量写入和管理写操作争同一把写锁。
- 只追加、按 WAL 顺序批量写，正好适合 bbolt 的单写者模型。

这个选择在 P1 的原型里用 §6 的预算验证；不达标就换（例如按时间分段的 Parquet 加小索引），换之前先停下来裁定。

## 5. 各项内存消费者的去向

| 现在的消费者 | 留在热状态 | 移到历史索引 |
| --- | --- | --- |
| `balances` | 未关闭期间的余额 | 已关闭期间的余额 |
| `reservations`、`leaseRecords` | 进行中的预留与对应记录 | 已结算的记录（价格快照恢复、备份价格校验改为流式读索引） |
| `settled` | 无 | 全部 |
| `eventDigests` | 在途实体和最近一段 WAL 的摘要（用于崩溃后重复事件的识别） | 全部摘要；`Apply` 先查热集合，再查索引 |
| `workUnits`、`runs` | 活跃的 | 已关闭的 |
| `requestRuns`、`activeByWorkUnit`、`pendingByWorkUnit` | 在途的；同时修复 §1.2 第 7 条的残留 | 无 |
| `settledByWorkUnit`、`governanceIdempotency` | 无 | 全部（治理查询改为按索引分页） |
| Usage `attempts`、`summaries` | 无 | 全部；控制台查询与导出改读索引 |
| Usage 小时汇总 | 最近若干小时的缓存 | 全部 |
| Governance State | 活跃的 Outcome 与修订 | 历史 Outcome 与索引 |
| 部署价格时间线 | 当前生效的版本与有限缓存 | 按时间点查询 |
| WAL segment 目录 | 当前段与有限页 | 有序索引 |

## 6. 预算（2026-10-15 已确认，P0 后填入实测基线）

| 项 | 预算 | 怎么测 |
| --- | --- | --- |
| Go heap | 历史从 1 万增加到 100 万请求，GC 后 heap 增量不超过 **32 MiB** | 同一配置，三个历史规模，进程内测量 |
| 热状态上限 | 默认配置下不超过 **128 MiB**；超出时拒绝新请求并告警，不 OOM | 压测：项目数和在途数取配置上限 |
| 吞吐 | 同一环境下不低于 P0 基线的 **95%**（并发 1、16、64） | 同一探针、同一容器环境 |
| p99 延迟 | 不超过 P0 基线的 **110%** | 同上 |
| 每请求 fsync | 不多于 P0 基线 | `strace -c` |
| 启动时间 | 与历史无关：100 万请求的历史下，有快照时不超过 **10 秒** | 冷启动计时 |
| 历史索引追赶延迟 | 稳态不超过 **5 秒** | 并发 64 下观测 |
| 改动规模 | 新增生产代码目标不超过 **1 万行** | `git diff --numstat`；超出就停下来复核设计 |

这些数字已于 2026-10-15 由用户按建议值确认。P0 时写入 `docs/verification/` 作为固定门槛，不在运行后放宽。

P0 实测基线见 [P0 基线](../verification/memory-bounded-baseline.zh-CN.md) §4。

## 7. 实施阶段

每个阶段结束时跑一次门禁、记录预算实测值；任何一项不达标，就停下来记录原因并裁定，不靠增加机制硬过。

| 阶段 | 内容 | 退出条件 | 估计 |
| --- | --- | --- | --- |
| P0 基线 | 合入第一版的 I-01 诊断（§10，`main` 上目前没有 pprof）；在 `main` 上测 heap 随历史的增长、吞吐、p99、fsync 次数、启动时间；用 pprof 找出最大的消费者 | §6 各项的基线值写入文档 | 1–2 天 |
| P1 原型：Usage | `history.db` 与后台追赶；Usage 明细和小时汇总移入索引；控制台查询和导出改读索引；行级 MAC | Usage 部分满足 §6 的 heap、吞吐、追赶延迟预算 | 约 1 周 |
| P2 Ledger | 热状态快照与后缀回放；已结算历史、事件摘要、治理索引移入索引；修复残留活跃状态 | §6 全部预算 | 1–1.5 周 |
| P3 其余 | Governance 历史、价格时间线、segment 目录 | 对应项满足 heap 预算 | 约 1 周 |
| P4 收尾 | `history.retention`、`history verify`/`rebuild` 命令、备份与恢复、运维文档 | 完整门禁 + 一次 Linux 2 GiB 容量运行 | 3–5 天 |

合计约 4–5 周。P1 结束后就能判断方案是否可行；如果 P1 不达标，最多损失约 1.5 周。P1、P2 完成后都可以单独发布。

**数据格式变更：** 新增 `history.db` 和热状态快照文件，首次启动时从现有 WAL 构建，不需要重新初始化。旧版本二进制不会读这两个文件，也不受影响。

## 8. 风险与未决问题

1. **生产的主要消费者。** P0 已合入 I-01（`HALRO_PPROF_LISTEN`），在合成历史上测出 Ledger 约占 55%、Usage 约占 42%（[P0 基线](../verification/memory-bounded-baseline.zh-CN.md) §3），与 §1.2 一致。生产流量的维度分布不同，如果能在生产上采一次 heap profile，可以据此修正 P1–P3 的先后顺序。
2. **bbolt 文件只增不缩。** 删除过期明细后空间会复用，但文件不会变小。需要在 P4 决定是否提供离线压缩命令。
3. **首次构建 `history.db` 的时间。** 首次启动要从现有 WAL 构建索引；它在后台进行，不阻塞 ready，但大型实例上可能要很久。P0 没有索引可测，只有替代数据：100 万请求、3 GB 的 WAL，现在启动时全量回放要 205～231 s，其中大半是 GC 扫描常驻 heap，不属于索引构建本身。真实的构建速度在 P1 测量。
4. **治理查询的语义。** 现在部分治理查询在内存里做全量过滤，移到索引后要按分页重写，返回结果的顺序和完整性要保持一致。

## 9. 和 HA 的关系

HA 代码在 `ha` 分支（`098a3214`）。本方案落地后，HA 基于新的 `main` 继续，需要在评审 `ha` 分支时决定：

- metadata journal（#360）是否仍按原设计接入；
- 复制只需要传输权威日志（Ledger WAL、Governance journal、Audit、metadata journal），`history.db` 和热状态快照是派生数据，每个节点各自从日志构建，不参与复制。这一点让 HA 比第一版的设计简单：第一版要复制七个认证根。

## 10. 可以从第一版复用的部分

`memory-bounded-state` 分支里与存储方案无关的几项，可以单独挑出来评审后合入：

- I-01：pprof 与运行时内存诊断（`internal/diagnostics`），P0 就需要。
- I-02：去掉只为读水位而做的全量 `Snapshot`（对应 §1.2 第 5 条）。

其余的认证存储、七根发布、共享资源预算和物理终结储备都不沿用。

## 11. P1 实施细则（Usage）

2026-10-15 写于开工前，依据是对 `main` @ `2f13dbb5` 上 Usage 全部读取方的盘点。

### 11.1 `history.db` 的内容

独立的 bbolt 文件 `data/history.db`，一次写事务同时写入一批记录和索引水位，所以索引内容和它声明的 WAL 位置永远一致。

| bucket | 键 | 值 | 用途 |
| --- | --- | --- | --- |
| `meta` | 固定键 | 格式版本、WAL 水位、MAC 密钥校验值 | 启动时判断能否沿用 |
| `attempts` | 结算序号（8 字节大端） | 尝试明细 + 行 MAC | 尝试列表、导出 |
| `requests` | 结束序号 | 请求摘要 + 行 MAC | 失败列表、请求详情 |
| `request_index` | 请求 ID | 结束序号 | 按 ID 找请求 |
| `request_attempts` | 请求 ID + 结算序号 | 空 | 请求详情、失败上下文 |
| `failed` | 结束序号 | 空 | 失败列表只扫失败的请求 |
| `anomalies` | 结算序号 | 空 | 仪表盘"近期异常" |
| `project_attempts` | 项目 ID + 结算序号 | 空 | 按项目过滤尝试列表 |
| `hourly` | 小时（Unix 秒） | 小时汇总 + 请求延迟直方图 + 行 MAC | 仪表盘近 7 天曲线 |
| `rollups` | 现有 `RollupKey` 编码 | 现有日汇总 + 行 MAC | 汇总接口、仪表盘今日数据 |
| `pending` | 请求 ID / 尝试 ID | 在途请求的累加器、尝试开始时间 | 重启后从水位精确续上 |
| `deployments` | 部署 ID | 最近一次成功请求的时间 | 引导页 |

日汇总从 `halro.db` 移到这里，因为它和明细本来就要在同一个事务里推进；`halro.db` 里原来的 usage 检查点和日汇总 bucket 删除。

### 11.2 写入

- 现有的 `Collector` 继续从 Ledger 接收已提交的记录，写入目标从内存聚合换成 `history.Writer`。
- `Writer` 每 250 毫秒或每 2000 条写一个事务，二者先到先写。这条路径不在请求路径上，请求路径不增加 fsync。
- 通道满了不阻塞请求：和现在一样，记下缺口，之后从索引水位回放 WAL 补齐。
- 行 MAC 用 HMAC-SHA256，密钥由主密钥经 HKDF 派生，标签为 `halro history row v1`。MAC 覆盖 bucket 名、键和值。读到 MAC 不对的行就报错并计数，不跳过。

### 11.3 留在内存里的

- Prometheus 计数器；
- 在途请求的累加器、尝试开始时间；
- 最近 4096 个事件 ID 的去重窗口；
- 当前水位。

除计数器外，这些都只随在途请求数变化。计数器保存在 `meta` 里，随批次一起写入。

### 11.4 启动

1. 打开 `history.db`，检查格式版本和 MAC 密钥校验值。
2. 读出水位并与 Ledger 头部比较：水位超过头部、版本不对或校验值不对，就整个重建。
3. 载入 `pending`，从水位开始回放 WAL。只回放后缀，不阻塞 ready；追赶期间控制台显示索引的水位。
4. 第一次启动时没有 `history.db`，在后台从头构建。

### 11.5 查询

| 接口 | 改成 | 行为变化 |
| --- | --- | --- |
| 尝试列表 | 从游标往前倒序扫 `attempts`；有项目过滤时扫 `project_attempts`，有请求 ID 时扫 `request_attempts`。每页最多检查 20000 行，不够一页就带着游标返回 | 过滤条件很少命中时，可能返回不满一页，但仍有下一页 |
| 失败列表 | 倒序扫 `failed`，再按请求取最后一次失败的尝试 | 同上，每页同样有检查上限 |
| 请求详情 | `request_index` 加 `request_attempts`；在途的请求从内存取 | 无 |
| 仪表盘·今日 | 读当天的日汇总；日汇总补上 `offering` 维度，`RollupVersion` 升到 3 | 无 |
| 仪表盘·近 7 天 | 读 168 个 `hourly` 行 | 每小时请求延迟 p50/p95 从精确值改为直方图近似值，与汇总接口的算法一致 |
| 仪表盘·近期异常 | 倒序扫 `anomalies` 中今日的部分 | 无 |
| 汇总接口 | 只读 `rollups`，不再合并内存里未落盘的增量 | 数据最多落后一个写批次 |
| 引导页 | `deployments` 表和最近一条请求 | 无 |
| Parquet 导出 | 从 `attempts` 按序号区间流式读取 | 无 |

### 11.6 删除

- `usage.Aggregate` 里的 `attempts`、`summaries`、`hourly`、`rollupDelta`，以及窗口裁剪；
- usage 检查点（head 和 segments）、它的定时保存，以及相关指标。

`console_window_days` 不再决定内存，只决定控制台默认的查询范围。

### 11.7 本阶段不做

- **合计对账（D3 第三行）。** 要等 P2 的热状态快照。P1 能发现行被修改，发现不了行被删除。二级索引（request、project、failed、anomalies）的条目本身不带 MAC：读取时用它指向的已认证行核对条目是否成立，加出来或改过的条目会让读取报错；被删掉的条目和被删掉的行一样，要到 P2 的对账才能发现。
- **`halro history verify` / `rebuild`、`history.retention`。** 放在 P4。
- **离线备份校验。** 它会在内存里重建整份聚合，P1 先保持原样，P4 改成流式校验。

### 11.8 退出时测量

用同一个探针测量。Ledger 仍在内存里，所以 heap 总量还达不到 §6 的预算。P1 看三项：

1. heap profile 里不再出现 Usage 的消费者；
2. 吞吐和 p99 不低于基线；
3. 并发 64 下索引落后不超过 5 秒。

## 12. P2 实施细则（Ledger）

2026-10-16 写于开工前，依据是对 `main` @ `1911e018` 上 `ledger.State` 全部读取方的盘点。带 ★ 的是与 §5 / §7 不同、需要确认的决定。

### 12.1 现状（代码核对）

| 字段 | 1M 历史时 | 何时删除 | 生产代码里的读取方 |
| --- | --- | --- | --- |
| `leaseRecords` | 0.90 GB | **从不**（结算后仍保留） | `RecoverDeploymentPricePins`（每次启动核对**全部**已提交的价格 pin）、备份时的 `ValidateDeploymentPriceReferences` |
| `settled` | 0.86 GB | 从不 | 备份校验（遍历全部）；`GovernanceSnapshot` 经 `settledByWorkUnit` 取尝试，唯一的用处是 `admin_outcomes.go:243` 按 WorkUnit 累加已知成本、估算成本和未知次数 |
| `eventDigests` | 0.44 GB | 从不 | 只有 `Apply`：同一 EventID 再次出现时比对内容 |
| `balances` | 小 | 从不 | 预算判断和控制台，只读当前期间。期间按天划分，条数 = 项目数 × 天数，**随天数增长，不随请求数增长** |
| `reservations`、`requestRuns`、`activeByWorkUnit`、`pendingByWorkUnit` | 小 | 结算 / 结束时删除 | 预算、恢复、指标 |
| `workUnits`、`runs`、`runsByWorkUnit`、`governanceIdempotency` | 基线里为 0（探针不建 WorkUnit） | 从不 | 治理接口、预算。`EventRunCreated` 为了数一个 WorkUnit 下的 Run，会扫描**全部** Run |

启动时间的另一半在 `ledger.OpenWithOptions`：它把活动 WAL 文件完整读两遍，一遍校验哈希链，一遍算滚动摘要（1M 历史约 51 s）。封存默认关闭，默认段大小 8 GiB，所以 1M 历史的 3 GB WAL 就是一个活动文件。**只做快照达不到 10 秒的启动预算。**

价格 pin 在 `halro.db` 里，已提交的 pin 也从不删除，每个按版本定价的尝试一条。

### 12.2 热状态

| 保留 | 上限由什么决定 |
| --- | --- |
| `reservations` 和对应的 `leaseRecords`（结算时一并删除） | 在途尝试数。例外：因价格 pin 提交失败而结算（`pin_commit_failed`）的尝试保留租约，因为它的 pin 还是 prepared，启动时要靠这条租约把它提交；否则 pin 被删掉，它锁定的价格就能被取消。只有元数据写失败时才会出现，数量很少 |
| `requestRuns`、`activeByWorkUnit`、`pendingByWorkUnit` | 在途请求数 |
| `balances` ★ | 项目数 × 天数。每条约 150 字节，100 个项目一年约 5 MB。§5 原计划把已关闭期间移入索引；这里先全部留在内存，P4 再按需要移 |
| `workUnits`、`runs`、`runsByWorkUnit`、`governanceIdempotency` ★ | 治理操作数，与请求数无关。§7 原计划在 P2 移入索引；这里留到 P3，与 Governance 历史一起处理。P2 只修掉 `EventRunCreated` 扫全部 Run 的问题，改用 `runsByWorkUnit` 计数 |
| 每个 WorkUnit 的成本合计：已知成本、估算成本、未知次数（新增） | WorkUnit 数。替代 `settled` 和 `settledByWorkUnit`，`admin_outcomes` 直接读合计 |
| 最近 65536 个事件的摘要（环形，约 6 MB） | 固定 |

移出内存：`settled`、`settledByWorkUnit`、结算后的 `leaseRecords`、超出窗口的 `eventDigests`。这三项在基线里合计 2.2 GB，结算后的明细已经在 P1 的 `history.db` 里，不需要新的索引。

热状态的大小作为指标导出；超过 128 MiB 时告警（§6）。在 P3 处理治理字段之前，拒绝新请求只针对在途数超限，不针对治理字段。

### 12.3 `Apply` 的校验怎么变

| 现在的校验 | 之后 |
| --- | --- |
| EventID 已出现过，内容不同 → 拒绝；内容相同 → 跳过 | 只在最近 65536 个事件内检查。会重复写入的只有确定性的恢复事件（`evt_recovery_…`），两份副本都在同一次启动的恢复过程里，相隔很近。恢复在快照任务启动之前完成，所以两份不会被快照隔开 |
| 为已结算的尝试再建预留 → 拒绝 | 只对窗口内的尝试有效。尝试 ID 是 128 位随机数，WAL 又有 HMAC 链，旧 ID 被重用只可能是伪造，而伪造过不了链校验 |
| 结算没有预留的尝试 → 拒绝 | 不变 |

### 12.4 快照

- 文件 `data/ledger/state.snap`，写临时文件、fsync、rename，再用 `durable` 对目录 fsync。
- 内容：格式版本（2）、对应的 WAL 位置（generation、offset、sequence）、写快照时 Ledger 的链头（sequence 和链哈希，在复制热状态之后读取，所以不早于热状态的位置），以及 §12.2 的全部字段。整个文件一个 HMAC，密钥从 Ledger 密钥经 HKDF 派生，标签 `halro:ledger-hot-state-mac-key:v1`。
- 为什么要记链哈希：第一版只记位置，启动时核对位置上有记录。同一密钥下的两份 WAL 副本（例如复制出去后各自继续写）可以长度完全相同、每帧 offset 都对得上，而内容不同；只看位置会把一份副本的热状态接到另一份的后缀上。链哈希覆盖之前的全部帧，两份历史只要有一帧不同，链哈希就不同。这个缺口是 PR #421 的自动审查指出的，已用测试 `TestLedgerStateSnapshotFromAForkIsSetAside` 在修复前复现。
- 每 60 秒，或 WAL 每增长 64 MiB 写一次。在读锁内复制热状态，在锁外编码、计算 MAC 和写盘。请求路径不增加 fsync。
- 格式版本不对或 MAC 不对：记录告警，走全量重建（§12.5 第 4 步）。

### 12.5 启动

1. 打开 Ledger。★ **封存默认开启，`ledger.seal.max_active_bytes` 默认从 8 GiB 降到 256 MiB**，这样活动文件最多 256 MiB，Open 的两遍读取有了上限。封存、校验、压缩都是现有功能，只改默认值。**已有实例的 `config.yaml` 里显式写着 `enabled: false` 和 8 GiB，显式值优先于默认值，所以升级后要手动改成 `enabled: true`、`max_active_bytes: 268435456`**；改完后第一次维护周期就会滚动一次。
2. 读快照，验证 MAC，检查它的位置不超过 WAL 头部。快照记录的链头必须属于这份 WAL：链头正好是 WAL 头部时，两个链哈希必须相等；否则回放时链头之后那一帧记录的前一哈希必须等于它（活动文件在 Open 时已整体认证过，已封存段在第 3 步从头认证）。
3. 快照在已封存的 generation 里时，从该段**开头**校验哈希链，快照位置上的记录必须正好结束在快照记录的 offset，之后的记录才应用。从段中间开始会跳过已封存帧的认证，所以不这样做。快照在活动 generation 里时，Open 已经从头校验过整个活动文件，直接从快照的 offset 继续。读量最多是两个段（约 512 MiB），与历史长度无关。任何一步对不上，就丢弃快照，走第 4 步。
4. 没有快照或校验失败时，从第一帧重建，但只保留热状态，内存仍有上限。这是升级后第一次启动和恢复时才走的路，耗时仍随历史增长。
5. 价格 pin 恢复 ★：处理未提交的 pin；已提交的 pin 中，租约在快照位置之后的，用本次回放读到的租约核对，找不到就拒绝启动。租约在快照位置之前、尝试已结算的 pin 不再在启动时核对：它们提交时记录的就是自己那条租约写入后返回的序号，备份校验会流式回放 WAL 再核对一遍。全量重建时同理，只核对在途尝试的 pin。

### 12.6 读取方改造

| 读取方 | 改成 |
| --- | --- |
| `admin_outcomes` 的成本汇总 | 读每个 WorkUnit 的合计，不再遍历尝试 |
| `GovernanceSnapshot(IncludeAttempts)` | 删除这个选项，唯一的调用方已改读合计 |
| `RecoverDeploymentPricePins` | 按 §12.5 第 5 步 |
| `ValidateDeploymentPriceReferences`（备份创建和恢复，离线） | 流式读 WAL，逐条检查结算事件和 pin，不在内存里建全量状态 |
| `SettledAttempt`、`SettledAttempts`、`AccountingLease`（已结算部分） | 删除 |

### 12.7 合计对账（D3 第三行，P1 留下的）

每小时（随 Parquet 导出的维护周期）一次：先让历史索引追到 Ledger 头部，再把 `history.db` 里每天的**合计行**，与内存里同一天所有项目 `balances` 之和比对已知成本、provider token 数和未知成本次数。用合计行而不用项目行，是因为项目维度有键数上限，超出的项目会并入其他行。

只比对不会再变的日子：48 小时之前、并且没有任何未结算租约的那天。更近的日子两边可能还在各自追加结算。

不一致时计数 `halro_usage_history_reconcile_mismatches_total`，按天写错误日志并提示用 `usage rebuild-summary` 重建；`halro_usage_history_reconciled_periods` 是上一次比对的天数。它能发现被删掉的明细、汇总行和索引条目。控制台上的提示留到 P4。

### 12.8 本阶段不做

- 治理字段移入索引（P3）。
- 已关闭期间的 `balances` 移入索引（P4，视实测增长决定）。
- `halro history verify` / `rebuild`（P4）。

### 12.9 退出时测量

用 P0 的探针，在同一 Linux 容器里测量，对照 §6 全部预算：

1. heap：1 万、10 万、100 万历史下 GC 后的 heap，增量不超过 32 MiB；
2. 有快照时，100 万历史冷启动不超过 10 秒；没有快照时的全量重建耗时照实记录；
3. 吞吐不低于基线的 95%，p99 不超过基线的 110%（并发 1、16、64）；
4. 每请求 fsync 不多于基线；
5. 热状态在项目数和在途数取配置上限时不超过 128 MiB；
6. 新增生产代码行数（预计 2500–3500 行，上限 1 万行）。

任何一项不达标，停下来记录原因并裁定。

## 13. P3 实施细则（其余消费者）

2026-10-16 起草。P2 合入 `main`（#421，`b54a821b`）之后，§7 列给 P3 的是四项：Ledger 里的治理字段、Governance State、部署价格时间线、WAL segment 目录。下面先按代码和实测给每一项定性，再只做真正随流量增长的部分。

### 13.1 现状（代码核对与实测）

用一次性进程内测量（GC 后 `HeapAlloc` 差值，各 2 万个实体，ID 用 `id.New` 生成的真实长度）：

| 消费者 | 由什么驱动增长 | 实测或估算 | 结论 |
| --- | --- | --- | --- |
| Ledger `workUnits`、`runs`、`runsByWorkUnit`、`workUnitCosts`、`governanceIdempotency` | 调用方通过 Run Governance API 创建的 WorkUnit 和 Run（`internal/app/run_governance.go`），关闭后永不删除 | 一个关闭的 WorkUnit 带一个关闭的 Run（含四条幂等记录）约 **2.2 KB**；普通请求在摘要窗口填满后不再增长 | **要做** |
| Governance State（`internal/governance/manager.go` 的 `outcomes` 及五个索引 map、`idempotency`、`intents`） | 调用方报告的 Outcome，永不删除；启动时从 bbolt 里的整份检查点恢复再回放 journal | 每个 Outcome 约 **1.5 KB** | **要做** |
| 部署价格时间线（`store/bolt/store_pricing.go` 的 `auditedDeploymentTimeline`） | 只有管理员创建价格版本（唯一写入点是 `admin_prices.go`），按部署缓存 | 每个版本几百字节；100 个部署各 100 个版本约 5 MB，与请求数无关 | 不做 |
| WAL segment 目录（`segments.json`） | 每封存一次 256 MiB 加一条，每条约 400 字节 | 100 万请求约 3.2 GB WAL、13 条；每秒 1 请求跑一年约 400 条、约 160 KB | 不做 |

规模感：如果调用方每 10 个请求开一个 WorkUnit、报一个 Outcome，100 万请求就是 10 万个，约 370 MB，远超 §6 的 32 MiB 增量预算；如果生产上不用 Run Governance，这两项都是零。

另外两处与大小无关、但随历史变慢的代码：`budget.Manager` 创建 WorkUnit 时遍历项目的**全部** WorkUnit 数未关闭的（`manager.go:287`），创建 Run 时遍历项目的全部 Run 数活跃的（`manager.go:408`），每次都复制整份列表。

### 13.2 热状态的边界（2026-10-16 用户确认后细化）

沿用 D2：内存里不放按天数划定的窗口，只放有上限的东西。

**Ledger 一侧。** WorkUnit 在两个条件同时满足时离开热状态（"归档"）：

1. 它已关闭；它的每个 Run 已关闭，或已过 `ExpiresAt`（以正在应用的事件的 `OccurredAt` 为时钟，回放时结果相同）；
2. 没有在途请求或未结算尝试归属于它（`activeByWorkUnit`、`pendingByWorkUnit` 为空）。

只看"已关闭"不够：关闭时可能还有尝试在途，结算会在关闭之后继续记到 Run 和 `workUnitCosts` 上；关闭的 WorkUnit 下的 Run 也可能还活着、还在接请求（WorkUnit 关闭不会关闭 Run）。归档时它和它的全部 Run、成本合计、指向它们的治理幂等记录一起移出。

热状态因此只有：未关闭的 WorkUnit（每项目 ≤ 1000）及其 Run（每个 ≤ 32）；已关闭、但还有活着的 Run（每项目活跃 Run ≤ 1000）或在途请求的 WorkUnit；以及**归档队列**——已归档、还没写进 `archive.db` 的条目。

**归档队列与持久化顺序。** 快照写入流程改为：在状态锁内同时复制热状态和队列；把队列里的条目写入 `archive.db`（一个事务，同时写"归档水位"=快照位置）；成功后才写快照文件，然后从队列删掉已写入的条目。崩溃发生在任一步之间，都从上一份快照恢复并重放后缀，归档会在同样的位置再次发生，再次写入（写入是幂等的覆盖）。队列超过 4096 条时提前触发一次快照写入，所以队列有上限。全量重建时每应用一段就写一次队列。

**查找顺序**：热状态 → 归档队列 → `archive.db`。由于条目先进队列、写入 `archive.db` 之后才离开队列，查找不会落空，不需要"追赶中"的 503。

**启动时的一致性**：`archive.db` 记录归档水位。快照位置超过归档水位（例如 `archive.db` 因格式版本或密钥变化被清空），说明快照声称已归档的条目在 `archive.db` 里不存在，就丢弃快照，从第一帧重建（重建会重新产生全部归档）。

**Governance 一侧。** Outcome 一写入就进归档队列（与 Ledger 相同的持久化顺序，挂在 Governance 检查点上）。热状态只保留**最近 4096 个**修订头和最近 4096 条幂等记录（按 journal 序号的固定容量环）。`Apply` 检查修订连续性时，头在环里就核对；不在环里就接受——这条事件在写入前已由 Manager 对照完整的头（环 → 队列 → `archive.db`）校验过，journal 有链式 MAC。环的内容只取决于 journal 前缀，所以从检查点回放和从头重建结果相同。

### 13.3 读取与校验

- **`Apply` 的唯一性检查**：`WorkUnitCreated`、`RunCreated` 只对热状态和队列检查 ID 是否已存在；已写入 `archive.db` 的不查。ID 由服务端随机生成，WAL 有链式 MAC，重复只能来自篡改。API 路径上创建前照旧生成新 ID，不受影响。
- **幂等重放**：Manager 查幂等记录按热状态 → 队列 → `archive.db` 的顺序。
- **项目内计数**：每项目维护未关闭 WorkUnit 集合和未关闭 Run 集合（都在热状态里，有上限），替代 §13.1 的两处全量遍历。
- **管理端列表**（`run-governance/work-units`、`runs`、`governance/outcomes`）：按 ID 游标分页，合并热状态、队列和 `archive.db` 的结果，顺序与现在一致（按 ID）。
- **治理汇总与导出**：汇总仍有 100,000 个 WorkUnit 的上限；导出现在一次读入全部 WorkUnit、Run 和 Outcome，P3 保持这个语义（读的来源变成热状态 + `archive.db`），**导出时的瞬时内存仍与历史成正比**，改成流式导出放到 P4。

### 13.4 归档文件 `archive.db`

实现时没有放进 `history.db`，而是单独一个文件 `data/history/archive.db`（`internal/archive`）。原因：`history.db` 是 Usage 的派生数据，运行中会被 `Reset` 清空重建（`runtime.go` 的 `openUsageHistory`、`usage.go`）；而写进归档的条目已经从内存里删掉，归档被单独清空就等于丢数据。归档只和依赖它的快照一起失效，见下面的水位与链绑定。行级 MAC 的密钥从 Ledger 密钥派生（`vault.DeriveArchiveHMACKey`）。

| bucket | 键 | 值 |
| --- | --- | --- |
| `work_units` | WorkUnit ID | WorkUnit、成本合计、Run ID 列表；行级 MAC |
| `work_unit_index` | 项目 + ID | 空 |
| `runs` | Run ID | 归档时的 Run；行级 MAC |
| `run_index` | 项目 + Run ID | 空 |
| `idempotency` | 项目 + 操作 + 键哈希 | `GovernanceIdempotency`；行级 MAC |
| `outcomes` | Outcome ID | Outcome 与写入它的幂等键、指纹；行级 MAC |
| `outcome_project_index`、`outcome_work_unit_index` | 项目或 WorkUnit + ID | 空 |
| `outcome_heads` | 项目 + WorkUnit + 定义 + 版本 | 当前头的 Outcome ID 与修订号；行级 MAC，读取时与 Outcome 行核对 |
| `outcome_idempotency` | 项目 + 键哈希 | 指纹与 Outcome ID；行级 MAC |
| `meta` | `ledger_through`、`ledger_chain`、`governance_through`、`governance_hash` | 两条日志各自已归档到的位置，以及该位置对应的链头哈希或 journal 记录哈希 |

索引条目没有 MAC，读取时与它指向的带 MAC 的行核对（与 #408 相同），不一致就报错，不跳过。WorkUnit 行只记 Run ID，读取时再取 Run 行并核对归属；写入的页面填充到 90%（bbolt 默认 50%，归档几乎只追加）。

### 13.5 快照、检查点与绑定

- **Ledger 快照**（P2）格式不变，内容只剩 §13.2 的热部分。启动时要求归档的 `ledger_through` 不小于快照位置，并且归档记录的链头与快照的链头一样，在同一次回放里对照 Ledger 核对（下一帧的前一哈希，或 WAL 头部的哈希）；任一不符就走全量重建。全量重建前先清空归档里的 WorkUnit 部分，重建过程中每满 4096 条写一次。这样另一份数据目录副本留下的归档不会被拿来用（与 #421 的快照链绑定同一个问题）。
- **Governance 检查点**：代码核对发现，生产代码从未写入过它（`SaveGovernanceCheckpoint` 只在测试里调用，自 #272 引入起如此），每次启动都从头回放整个 journal。它的内容是整份状态，与有界的设计矛盾，所以读取路径删除；残留的旧检查点直接丢弃，不解析。
- **Governance 归档**：每次启动仍从头回放 journal，归档位置之前的记录只更新环、不再写入；回放到归档记录的位置时核对 journal 记录哈希，不符就清空归档的 Outcome 部分再回放一次。运行时每次报告在 journal 追加后立即写入归档；写入失败则 Governance 进入不可用，下次启动从 journal 补回。
- **升级后首次启动**：没有 `archive.db`，归档位置为 0，Ledger 快照被拒，两份状态都从日志重建，归档边重建边写，不在内存里累积。之后每次启动从快照恢复。

### 13.6 本阶段不做

- 部署价格时间线、WAL segment 目录（§13.1，不随请求增长）。
- 已关闭期间的 `balances`（P4）。
- `halro history verify` / `rebuild`（P4）。

### 13.7 退出时测量

1. 进程内：分别创建并关闭 1 万、10 万个 WorkUnit（各带 1 个 Run、1 个 Outcome），GC 后 heap 增量不超过 32 MiB；
2. P0 探针在同一 Linux 容器里复测普通请求的吞吐、p99、fsync，对照 §6，确认不退化；
3. 治理 API 的创建/关闭/报告延迟照实记录（会多一次 `history.db` 读）；
4. 新增生产代码行数（预计 2000–3000 行；P1 新增 2324、删除 2356 行，P2 新增 1052、删除 137 行，三阶段新增合计仍在 1 万行以内）。

任何一项不达标，停下来记录原因并裁定。

**结果**（[P3 结果](../verification/memory-bounded-baseline.zh-CN.md) §7）：治理实体 1 万 → 10 万 heap 15.1 → 15.2 MiB；普通请求除并发 64 吞吐为基线的 93.8%（差 1.2 个百分点，记录与裁定见结果页 §7.2）外均达标；`archive.db` 每个 WorkUnit 约 5.3 KB。

## 14. 热状态上限的执行（拒绝与告警）

2026-10-17 起草。P3 合入后测了 §6 第二项（[基线 §8](../verification/memory-bounded-baseline.zh-CN.md)）：正常流量远低于 128 MiB，但没有任何机制保证不超过，§6 要求的"超出时拒绝新请求并告警"没有实现。这一节补上。

### 14.1 量什么

不读真实 heap：`runtime.ReadMemStats` 会暂停整个进程，不能放在请求路径上；heap 也包含热状态以外的一切。改为在 `ledger.State` 里按条目数估算：

    估算字节 = Σ 各 map 的条目数 × 该类条目的单价 + 固定部分（事件摘要环、归档队列上限）

单价是常量，取 §8.2 探针的实测值并向上取整；探针改为同时检查估算与实测 heap 的偏差（每类不超过 ±25%），单价过时会让它失败。估算在 `State` 的读锁下只读 `len()`，是 O(1)。

计入的：在途请求、预留与租约、余额、WorkUnit、Run、治理幂等记录、归档队列。

### 14.2 拒绝什么

估算超过上限时，**只拒绝会让热状态变大的新入口**：

| 入口 | 超限时 |
| --- | --- |
| `BeginRequest*`（新请求进入） | 拒绝，503 `accounting_capacity_exhausted`，带 `Retry-After` |
| `CreateWorkUnit*`、`CreateRun` | 拒绝，503，同一错误码 |
| 已进入的请求再预留（重试、fallback） | 不拒绝：每个请求的尝试数已由 `max_total_attempts` 限住，半途拒绝只会把已开始的请求变成失败 |
| 结算、结束、关闭 WorkUnit/Run、恢复 | 不拒绝：它们让热状态变小 |

余额行在当天第一次预留时创建，随天数增长、不随请求增长，不单独拒绝，但计入估算。

不是 fail-open：超限时拒绝的是新工作，已有的账务照常结算，不丢任何记录。

### 14.3 配置

`ledger.max_hot_state_bytes`，默认 128 MiB（§6），允许 16 MiB～4 GiB。项目很多、又开了 Run Governance 的部署（§8.3：约 53 个项目到上限）可以调高，代价是内存。

### 14.4 指标与告警

- `halro_ledger_hot_state_estimated_bytes`（gauge）、`halro_ledger_hot_state_limit_bytes`（gauge）。
- `halro_ledger_hot_state_rejections_total{operation}`（counter；`operation` 只有 `request`、`work_unit`、`run` 三个值）。
- 告警 `HalroLedgerHotStateHigh`：估算超过上限的 80% 持续 10 分钟，warning。
- 告警 `HalroLedgerHotStateRejecting`：10 分钟内有拒绝，critical。
- 两条都写进 `rule-tests.yml` 和运维手册（`runbook_links_test` 要求有锚点）。

`halro_ledger_state_snapshot_bytes`（快照文件大小）保留：它量的是磁盘上的文件，不是同一件事。

### 14.5 测试

- 确定性：把上限设得很小，确认新请求、新 WorkUnit、新 Run 被拒为 503，已进入请求的重试预留和结算照常完成，估算回落后恢复接受。
- 估算的单价由 §8.2 探针校准（见 14.1）。
- 不需要 Linux 容器复测：不涉及 I/O。

### 14.6 规模

预计生产代码 200～300 行（估算、四个入口的检查、配置、指标），加告警规则、手册与测试。

### 14.7 实现记录（2026-10-17）

按上面的设计实现，偏差两处：

- **预留的单价按真实事件校准。** §8.2 的探针用最小字段的事件，预留 2074 B；按网关实际填写的字段（路由、部署、价格快照、Token Guard 摘要）测得 2847 B，单价取 3072 B。校准测试 `TestHotStateEstimateTracksTheHeap` 覆盖计量价格和未知价格两种预留。
- **新请求的检查只在 `BeginRequest*`。** 设计里写的"四个入口"是新请求、新 WorkUnit、新 Run，加上已有请求的再预留；后者按 14.2 不检查，所以实际是三处。

各类单价与实测见[基线 §8.4](../verification/memory-bounded-baseline.zh-CN.md)。控制台的错误说明新增 `accounting_capacity_exhausted`（中英文）。

请求路径上的开销：`HotStateBytes` 在 8 核并发争用下每次 80 ns（单核 3.7 ns）。并发 64 时一个请求生命周期约 560 µs，占比约 0.01%。`BenchmarkRequestLifecycle`（8 项目、并发 64、2 万次，与 `main` 交替各 5 轮）：`main` 1741～1853、本分支（检查生效）1666～1854 lifecycles/s，区间重叠，差异在噪声内。

### 14.8 评审修正（#423）

评审指出两处，核实后都成立，按下面修正：

1. **一批请求同时进入时会冲过上限。** 普通请求在 `RequestAccepted` 时不往状态里加任何条目，要等预留时才计入；检查只看当时的估算，所以同时到达的一批都能通过。修正：预算管理器记录本进程已接收、尚未结束的请求（集合，按请求 ID；同一请求可能从两条路径结束，只有第一次释放）；接收时在同一把锁下先计入再检查，估算里在途部分取 `max(在途请求数, 预留数)` × 预留单价。`TestHotStateLimitCountsABurstBeforeItReserves`：64 个并发请求、上限只够 10 个时恰好接收 10 个，估算不超过上限；退回旧检查时 64 个全部接收。接收加释放在 8 核争用下每次约 210 ns。
2. **删除条目不释放 map 的槽位。** 实测：12 万个预留进出之后，预留和租约两个 map 已经没有条目，仍各占 6.4 MiB，而估算为零。修正：这些 map 降到峰值的四分之一以下时（峰值至少 1024）按现有条目数重建。重建必须新建 map 逐条复制：`maps.Clone` 保留源 map 的表结构，用它重建等于没重建（第一版就是这样，实测无效）。`TestASettledBurstGivesItsMemoryBack`：4 万个预留进出之后剩 0.05 MiB；换回 `maps.Clone` 时剩 3.34 MiB，测试失败。

顺带两处调整：

- **事件摘要环按写满计价。** 原来按已写入的比例计价，每个事件在检查之后让估算长 192 B；环在前 65536 个事件内写满后就不再变，所以从一开始就按 12 MiB 计。
- **测 heap 时被测对象必须仍可达。** 诊断中有几次读数比基线还低 11～14 MiB，原因是读数时整个状态已经没有引用、被回收了；读数之后要 `runtime.KeepAlive`。

剩下一处已知的越界：某个项目当天的第一个请求会在检查之后新建一行余额（224 B），每个项目每天一次。
