# HA 移植到当前 `main` 的设计

- 状态：owner 已确认 §7 的四项建议（2026-10-21），按 §5 分阶段实施。0a 已合入（#445）；0b–2 的实现见 §8（#446）；阶段 3 见 §9
- 依据：[`halro-ha-architecture.zh-CN.md`](halro-ha-architecture.zh-CN.md)（下称"HA 设计"，代码在 `ha` 分支）、
  [`memory-bounded-redesign.zh-CN.md`](memory-bounded-redesign.zh-CN.md)（下称"内存有界设计"，已随 v0.9.0 发布）
- 范围：把 `ha` 分支上已实现、已在本地三节点演练通过的 HA，搬到 v0.9.2 之后的 `main` 上。
  不新增 HA 能力，不改 HA 的不变量。

## 1. 现状

`ha` 分支就是 HA 被移走之前的 `main`（`098a3214`，2026-10-01），之后只合入了 #439（撤销确认失败时的
内存鉴权快照刷新，以及让 CI 重新通过的依赖升级）。#404 用 11 个回退提交把 HA 与 metadata journal
从 `main` 整体移走，理由写在 PR 里：生产跑的 v0.8.5 不含 HA，内存问题要在不带 HA 的 `main` 上先修，
"HA then continues on top of the fixed main"。内存问题已由 P0–P3 与热状态上限修好并发布（v0.9.0–v0.9.2）。
本文就是那句话的下一步。

2026-10-21 在 `ha` 上的核对：`make full-check` 通过；本地三进程演练通过（复制、单/双 Replica 断开、
stepdown、promote 与 fencing、未确认尾部的 reseed、Replica 维护与备份、三份日志逐字节一致）。
两个发现：(1) 撤销返回 503 后内存鉴权快照不刷新——#439 已修；(2) HA 成员上 `ledger verify` /
`audit verify` 被 `GuardUnavailableRuntime` 拒绝，且提示文案过时——随阶段 3 修复（§9）。

## 2. 移植不改变的东西

HA 设计的不变量（§3）、复制模型（§6）、角色与 term（§8）、提升与 fencing、播种与回退（§11）
全部原样保留。本文只回答一个问题：**`main` 在 #404 之后新增的状态与行为，在复制模型里各归哪一类，
以及 HA 代码里哪些假设已经不成立。**

## 3. `main` 在 #404 之后的变化盘点

只列与 HA 相交的部分。事实均对照代码核实（`dc704030..51586ee7`）。

### 3.1 `halro.db`：基本没变

- schema：`ha` 为 40，`main` 为 41。38–40 号迁移两边逐字相同；41 号 `drop_usage_derivatives`
  只在 `main`。**无编号冲突。**
- bucket 与 `meta` key：`main` 没有新增任何一个。41 号迁移删掉的 4 项
  （`meta/usage_checkpoint`、`meta/usage_rollup_state`、`usage_daily_rollup`、`usage_checkpoint_segments`）
  在 HA 设计 §5.2 里都是 **C 类（节点派生，不进 journal）**，删掉只会让 C 类表变短。
  `ha` 独有的 3 个 key（`applied_journal_sequence`、`metadata_hmac_envelope`、`metadata_journal_epoch`）
  都是 journal 自己的，随 journal 一起回来。
- #404 之后改过的 store 文件只有 4 个：`pricing_backup.go`、`store_pricing.go`（#434 定价时钟漂移修复，
  改的是已有事务的内容，没有新写入点）、`store.go`（41 号迁移）、`store_usage.go`（整体删除）。
  **没有新的 `db.Update` 写入点需要重新分类。**
- `meta/governance_checkpoint` 与 `governance_checkpoint_segments`：`main` 启动时清空、不再读
  （P3 删掉了读路径），仍是 C 类，不受影响。

### 3.2 Ledger / Governance 帧：格式没变

`internal/ledger/log.go` 自 #404 起只改了 2 行；`event.go` 的改动全在内存状态一侧（工作单元归档、
pin 提交失败的结果字符串），帧编码、事件种类编号、feature epoch 都没有变。Governance journal 帧同样
没变（删的是 `SnapshotVersion`，即已废弃的 checkpoint）。**物理复制帧、逐字节相同的前提仍成立。**

### 3.3 新增的持久状态：都在 `halro.db` 之外

| 状态 | 路径 | 性质（内存有界设计） |
| --- | --- | --- |
| 用量历史索引 | `data/history/history.db` | Ledger 的派生物，可随时清空重建 |
| 归档 | `data/history/archive.db` | 已离开内存状态的 Work Unit（含 Run、成本、Work Unit 级幂等记录）与 Outcome。**不是**自由派生物：绑定写入它时的 Ledger 链头与 Governance journal 哈希，对不上就从日志一起重建 |
| 账务状态快照 | `data/ledger/state.snap` | Ledger 内存状态的快照，绑定链头，不可信即从 Ledger 重建 |

### 3.4 新增的运行时行为

| 行为 | 位置 | 与 HA 的关系 |
| --- | --- | --- |
| 热状态上限 | `budget/hot_state.go` | 只在**准入**时拒绝新请求/新 Work Unit/新 Run；Ledger 应用帧的路径上没有检查。Replica 应用复制帧不受影响 |
| 启动路径：快照 + 归档 + 重放后缀 | `app/ledger_state.go`、`runtime.go` | 取代了"`ledger.NewState()` 从头重放"。见 §4.1 |
| 封存触发点移入快照循环 | `runLedgerStateSnapshots` → `sealLedgerGeneration` | 见 §4.2 |
| 压缩门槛改用 Parquet 清单 ∧ `history.db` 水位 | `ledgerArchivedThrough` | 见 §4.3 |
| `history.db` / `archive.db` 每小时 reopen | `boltreopen` | 纯节点本地，无冲突 |
| 用量查询从 `history.db` 读 | `usage/history_read.go` | 每个节点都有自己的索引，见 §4.4 |

## 4. 冲突点与处理

### 4.1 Replica 的账务状态构建：`ha` 走的是 v0.8.5 那条会 OOM 的路

`ha` 的 `replication_runtime.go:187-212`：Replica 启动时 `ledger.NewState()` 然后对**整个** Ledger 逐条
`Apply`，并在内存里维护用量聚合（`restoreUsageAggregate`）。这正是 v0.8.5 生产上 OOM 重启循环的路径
（2026-10-19 事故，启动重放 >2 GiB）。`main` 用"状态快照 + `archive.db` + 重放后缀"与 `history.db`
取代了它。

**处理：** Replica 的状态构建改用 `main` 的打开路径，不搬 `ha` 的这一段：

- Replica 打开时与 Standalone 相同：读 `state.snap`（可信则用）、对照 `archive.db` 的绑定位置、
  重放后缀；不可信则从本地 Ledger 重建。
- 复制帧应用到 Ledger 后，同一条记录喂给内存状态与 `history.db` 收集器，与 Primary 上一条请求结算
  走的是同一个 `Apply`。
- Replica 也跑快照循环（写 `state.snap`、写 `archive.db`、`forgetArchived`），否则它的内存状态只增
  不减，跑久了就是 v0.8.5 的问题；提升时也要从头重放。**但快照与归档只推进到 confirmed index**
  （HA 设计 §6.2 对 C 类的同一规则），因为未确认的尾部可能在提升或 reseed 时被截掉，而归档一旦写入，
  内存状态就已经把它忘了。

### 4.2 封存：Replica 绝不能自己滚

`main` 把 `sealLedgerGeneration()` 放在快照循环里调用。HA 设计里封存是 Primary 决定、经
`BeforeRoll`/`AfterRoll` 钩子作为结构事件进流、Replica 用 `ApplyReplicatedRoll` 应用的（`ha` 的
`ledger_seal.go`、`seal.go:98`、`native_sink.go:74`）。

**处理：** 快照循环按角色拆开——Primary 照旧"先封存、再判断要不要快照"，Replica 只做快照、不调
`sealLedgerGeneration`。`Roll` 的钩子与 `ApplyReplicatedRoll` 从 `ha` 搬回。加一个测试钉住
"Replica 运行时从不调用 `Log.Roll`"。

### 4.3 压缩门槛：`ha` 读的 usage checkpoint 已被 41 号迁移删除

`ha` 的 `compactLedgerSegments` 以 `meta/usage_checkpoint` 为门槛；`main` 已经没有这个 key，
门槛是 `min(Parquet 清单, history.db 水位)`。而 HA 设计规定**只有 Primary 导出 Parquet**，
所以 Replica 上 Parquet 清单不存在或停滞，按 `main` 的门槛 Replica 永远不会压缩。

**处理：** 门槛按角色取：Primary 用 `main` 现有的 `min(清单, 历史水位)`；Replica 用
`min(历史水位, confirmed index)`。压缩形态本来就是节点本地的（HA 设计 §6.2.3），两边不必一致。

### 4.4 用量数据：切换后 Parquet 分裂，但 `history.db` 不受影响

HA 设计：Usage 分区不复制，只有 Primary 导出 Parquet。切换后新 Primary 的 `usage/` 从它自己的位置
继续导出，Parquet 历史分散在两个节点上。这在 `ha` 上就成立，不是本次引入的。

变化在于：`main` 的用量查询已经改为读 `history.db`，而每个节点都从自己的、逐字节相同的 Ledger
建 `history.db`。所以**切换后控制台与 Admin API 的用量查询是完整的**，只有 Parquet 导出文件分在两处。
本文不改变"只有 Primary 导出"，只在运维文档里写清 Parquet 归档要从两个节点收。

### 4.5 `state.snap` 在 `ledger/` 目录里

`state.snap` 放在 `data/ledger/` 下，与 WAL 段同目录。HA 的复制按帧走、不按目录同步，所以复制本身
不受影响；但**播种**（HA 设计 §11.1）与 Replica 备份若按目录拷贝，会把 Primary 的快照带过去。快照绑定
链头、自带 MAC，带过去在被验证为可信时可以直接用，否则会被拒绝并重建。

**处理：** 不搬家（搬家是格式变更，要 bump 并说明需重新初始化）；在播种代码里显式决定：带上并依赖
它的绑定校验，还是排除它让 Replica 自己重建。倾向带上——播种的目的就是让 Replica 快速可用，
而绑定校验已经是"不可信就重建"的失败关闭。实现时用一个测试钉住"篡改或错位的 `state.snap` 在 Replica
上被拒绝并重建"。

### 4.6 `archive.db` 在 Replica 上：从日志派生，但不能超前

`archive.db` 的内容完全由 Ledger 与 Governance journal 决定，Replica 的这两份日志与 Primary 逐字节
相同，所以 Replica 自己写出的 `archive.db` 在内容上等价，**不需要复制**，归 C 类。唯一的约束就是
§4.1 的：只推进到 confirmed index。调用方幂等（`provider_resources`，A 类）不在 `archive.db` 里，
#378 钉住的"调用方幂等随 journal 复制"不受影响。

### 4.7 热状态上限与提升

Replica 不接请求，热状态上限不起作用；它的内存状态大小与 Primary 同源。提升后的新 Primary 从
`openRequests = 0` 开始准入，与重启一致。无需改动，但演练要覆盖"Primary 处于上限附近时切换"。

## 5. 移植方式

不在 `main` 上一次性 `git revert` 那 11 个回退提交：冲突集中在 `app/runtime.go`、usage 与 ledger
的状态层，一次解决会把"机械恢复"和"按 §4 重新设计"混在一个无法审查的 diff 里。

按 HA 原本的阶段顺序分 PR，每个 PR 从回退对应的那个 revert 提交开始，再按 §4 改：

| 阶段 | 内容 | 对应的回退提交 | 主要工作 |
| --- | --- | --- | --- |
| 0a | metadata journal：`halro.db` 成为 journal 的投影 | `aee27487`、`e8ada230`、`bd3c8887`、`cfa39627` | 基本机械：§3.1 已证明没有新写入点；冲突在 #434 改过的两个定价文件与 41 号迁移（迁移函数签名要从 `*bbolt.Tx` 改回 `*Tx`） |
| 0b–2 | 复制连接、Primary/Replica 运行时、`halro cluster`、提升、备份 | `a5e21480`、`52c45f42`、`202a1419` | §4.1–§4.6 全部在这里；运行时重写 Replica 的状态构建 |
| 3 | 集群状态、计划内交接、HA 健康系统 | `687f8d6a`、`716ac18c` | 基本机械；顺带修 §1 的发现 (2) |
| — | `ha` 上 #439 的修复 | `2227daf8` | 并入阶段 0b–2 |

阶段 0a 单独合入 `main` 后，Standalone 部署的行为不变（HA 设计 §4："没有 replication 块就是
Standalone，部署契约一个字节都不变"），但 `halro.db` 的写路径整体换成经 journal 的投影。这是
**数据目录格式变化**：新增 `metadata.journal` 与 3 个 `meta` key。

## 6. 验证

每个阶段：

- `make full-check`（Node 22），以及 Linux 上的 `go test -race`（HA 有 Linux-only 测试，CI 覆盖）。
- 0a：从 v0.9.2 写出的真实数据目录升级、重启、备份恢复、回滚到 v0.9.2 的二进制（应被拒绝或明确
  可回滚——由 0a 的格式决定写清楚）；v0.9.2 生产规模（约 123 万条 Ledger 记录）下的启动时间与堆，
  对照 v0.9.2 基线，确认 journal 没有把内存有界的成果吃回去。
- 0b–2：在移植后的代码上重跑 2026-10-21 的本地三节点演练全套，外加：
  - Replica 运行 24 小时的堆曲线（§4.1：Replica 的内存状态必须有界）；
  - 提升前后 `history.db` 用量查询结果一致（§4.4）；
  - Replica 从不封存、压缩只推进到 confirmed index（§4.2、§4.3）；
  - 篡改的 `state.snap` 在播种后被拒绝并重建（§4.5）；
  - 热状态上限附近切换（§4.7）。
- 3：HA 健康系统的本地验收重跑。

HA 设计 §1.3 的进入条件（kind、Linux、相邻版本、生产演练）仍未满足，移植完成**不等于**可以启用 HA。

## 7. owner 决定（2026-10-21，均按建议）

1. **阶段 0a 是否单独发布。** 单独发布能让 journal 在 Standalone 生产上先跑一段，但它是数据目录格式
   变化，且对不用 HA 的部署没有直接收益。另一种做法是 0a–2 在一个长期分支上做完再一起合入。
   建议：0a 单独合入 `main` 但**不单独发版**，与 0b–2 一起进下一个 minor。
2. **回滚边界。** 0a 之后的数据目录能否被 v0.9.2 打开。建议：用 schema 版本做围栏，v0.9.2 拒绝打开
   （与 v0.9.0 的做法一致），回滚走备份恢复。
3. **`state.snap` 播种时带不带**（§4.5）。建议带上，依赖绑定校验。
4. **Parquet 导出在切换后的归属**（§4.4）。建议维持"只有 Primary 导出"，文档说明归档要从两个节点收。

## 8. 阶段 0b–2 的实现记录

按 §5，从 `a5e21480`、`52c45f42` 与 `202a1419` 的 shutdown_test 部分的回退开始，再并入 `ha` 上 #439
（`2227daf8`）。冲突的处理：

- **§4.1** Replica 打开时走 `openBoundedLedgerState`：与 Standalone 相同的快照 + 归档 + 后缀，只重放到
  ordering journal 给出的已应用 Ledger 序号；快照越过已应用前缀即弃用重建。投影应用的每条记录同时喂
  给 `ledger.State`、已应用链头与用量收集器（`history.db`）。Replica 跑快照循环与 `Close` 时写快照。
- **快照绑定哪个链头。** 设计里写的是"只推进到 confirmed index"。Replica 的内存状态本来就只含已应用
  前缀，但 `WriteStateSnapshot` 与 `FlushArchive` 绑定的是 `Log.ChainHead()`——在 Replica 上它可能落在
  durable 而未确认的尾部。所以 Replica 给它们一个 `appliedLedgerHead`：最后一条已应用记录的链头。
  为此 `ledger.Record.Hash` 改为对每个链式帧都填（帧 MAC 的哈希，即下一帧的 `PreviousHash`），
  与 `PreviousHash` 同样"由读到它的那次遍历认证"；此前只有带校验器的遍历才填，`Log.Replay` 的活动代
  拿不到。唯一的旧调用方（`ledger.ReplicationHeadAt`）另外要求 `ChainVerified`，结果不变。
- **§4.2** `sealLedgerGeneration` 在 Replica 上直接返回（Ledger 在 Replica 模式下本来也拒绝本地 `Roll`）。
- **§4.3** Replica 的压缩门槛为 `min(history.db 水位, 已应用前缀)`。
- **§4.5** 播种照旧拷整个 `ledger/` 目录，`state.snap` 随之带过去；被改动的快照 MAC 失败、重建。
  `history/` 不在播种清单里：不要求也不校验，带过去的 `history.db`/`archive.db` 在 Replica 打开时按它
  自己的 Ledger 校验，对不上即重建。备份本来就不含这三个派生文件。
- **§4.6** `archive.db` 每个节点自己写，Replica 的写入绑定已应用链头。

- **#385 提前到本阶段。** 不带 `687f8d6a`（#385：集群状态页、计划内交接加固）时，Primary 从不更新成员状态
  里的 projection；stepdown 之后老 Primary 以 Replica 打开即报 "Replica metadata projection does not match
  authenticated member state"。用 `ha`（`2227daf8`）与本分支的二进制跑同一脚本（establish → 播种 → 一次元数据
  写 → stepdown → 重启老 Primary）确认：`ha` 上老 Primary 是 term 2、projection index 13，能打开；本分支不带 #385
  时是 term 1、projection index 0，打不开；带上 #385 后与 `ha` 一致。所以 #385 并入本阶段，阶段 3 只剩
  `716ac18c`（#386 HA 健康系统）与 §1 的发现 (2)。

测试：`TestReplicaLedgerState*`、`TestReplicaCompactionGateIsTheHistoryAndTheAppliedPrefix`、
`TestReplicaRuntimeNeverRollsItsLedger`、`TestReplayedRecordHashIsTheChainHeadAfterIt`，以及集成测试
`TestPrimaryAndReplicaRuntimesReplicateAndApplyAnAuditFrame` 增加的"Replica 关闭写出快照"。§6 的
24 小时堆曲线仍未跑。

本地三节点演练（2026-10-21，darwin，本分支二进制）：

| 项 | 结果 |
| --- | --- |
| establish、两个 Replica 从索引 0 播种、复制收敛 | 通过 |
| Primary 重启从快照恢复；Replica 运行中写 `state.snap`；Replica 重启从快照恢复（head 1438） | 通过 |
| stepdown n0→n1：n1 以 Primary 身份从它作为 Replica 写的快照恢复 | 通过 |
| 老 Primary 带过时 projection（旧二进制写的）→ 从 n1 reseed：带过去的 `state.snap` 直接被使用 | 通过 |
| stepdown n1→n2（n2 始终是 Replica）：用量汇总与请求列表切换前后逐字段一致（只差 `generated_at`） | 通过 |
| 停机后三个节点的 Ledger、Audit、Governance、metadata journal 逐字节一致；三个节点 `doctor` 通过 | 通过 |
| Replica 维护模式 → `backup create --replica` → `backup verify` → 退出维护 | 通过 |
| Primary 封存、Replica 应用封存与压缩 | **未跑**：封存阈值下限 16 MiB，本地失败请求经路由挂起后只能写约 0.3 MB/分钟；由单元测试覆盖，Replica 应用 Roll 的代码与 `ha` 相同 |
| 热状态上限附近切换（§4.7） | 未跑 |

演练中再次遇到 `route suspensions` 在 HA 成员上的过时提示（"this build has no replication runtime"），即 §1 的发现 (2)，留在阶段 3。

## 9. 阶段 3 的实现记录

从 `716ac18c`（#386 HA 健康系统）的回退开始。冲突只有三处：#439 的元数据确认失败上报保留，并按 `ha`
的写法经 `waitRequiredConfirmation` 计数；`runtimeFieldBudget` 79 → 81；CI 的 govulncheck 保持 main 的
go1.26.10。`ha` 上后来把 ha-health 镜像升到 golang:1.26.10，一并带上。控制台 bundle 重建，不手工合并。

**§1 的发现 (2)。** `GuardUnavailableRuntime` 的名字和文案都停留在"运行时还没写出来"的时候；它现在的
职责是拒绝在 HA 成员上走 Standalone 路径的离线写。改名为 `GuardStandaloneAccess`，文案改为"refusing X on
an HA member: it would mutate member state … outside the replication order"。不追加任何帧的命令
（HA 设计 §12 对"只读"的定义）改走新的 `GuardMemberRead`：`ledger verify`、`audit verify`、
`audit verify-anchor`、`route suspensions`（`usage verify` 本来就放行）。放行之前先在演练数据的副本上
用真实二进制跑过：

- `ledger verify`、`route suspensions` 在 Primary 与两个 Replica 上都通过——Ledger 的 checkpoint 检查
  本来就容许 checkpoint 落后。
- `audit verify` 在 Primary 上通过、在两个 Replica 上失败（"audit log does not match its trusted
  checkpoint"）：它要求 checkpoint 与链尾完全相等，而 `audit_checkpoint` 是 C 类、Replica 从不推进。
  成员上改为要求 checkpoint 是已认证链上的一点（不越过链尾，同一位置哈希与长度相同）；Standalone
  仍是完全相等。
- `audit verify-anchor` 原来用可写的 `audit.Open`，会就地截掉半帧尾部；在成员上那是排序权威的事。
  改用新的只读 `audit.InspectWithVisitor`：认证完整记录、报告但不截掉半帧，判定与原来相同
  （`TestVerifyAuditAnchorsDetectsARewrittenChainWithARecomputedTail` 现在还断言文件字节不变，退回旧
  实现时失败）。

修复后在同一份副本上：四个命令在三个成员上都通过，三份 `audit.log` 字节不变，`ledger seal` 仍被拒绝
且文案已更正；之后三个成员照常启动并收敛。

**HA 健康系统的本地验收**（§6）没有按 2026-09-28 的 kind 规模重跑（那是一整天的人工长跑与 72 小时采样）。
本阶段做的是仓库门禁（含 `cmd/halro-ha-health` 的测试与 Prometheus 规则测试）和一次主机进程 smoke，
结果见 PR。
