# Heimdall Redis-like HA 架构提案

- 状态：Proposed / Future
- 生产就绪：否
- 适用范围：Standalone 向 Primary/Replica HA 的演进
- 当前实现：仅 Standalone；本文出现的 `cluster` 配置和命令均未实现
- 相关决策：[ADR 0001](adr/0001-single-process-architecture.md)、[ADR 0004](adr/0004-distributed-evolution.md)、[Distributed State Ownership](distributed-state-ownership.md)

## 1. 目标与设计 DNA

Heimdall 保持与 Redis 相近的产品和运维形态：

- 单二进制；
- 本地持久化；
- Standalone 默认可用；
- HA 由多个 Heimdall 进程组成；
- 不强制依赖 PostgreSQL、Redis、S3、KMS、Etcd 或 Consul；
- 外部存储和 KMS 只能是可选增强。

Redis-like 对齐的是单二进制、本地持久化和 Primary/Replica 运维体验，
不是照搬 Redis 异步复制的数据丢失语义。Heimdall 会在调用外部 Provider
前产生预算和费用副作用，因此权威状态必须使用多数派提交。

HA 只提高可用性，不增加单个 Project 的权威写吞吐。多分片 Cluster、
active-active、多 Primary 和按 Project 横向扩容不属于本文范围。

## 2. 非目标

本文不承诺：

- Provider exactly-once；
- 活跃 HTTP/SSE 连接跨节点迁移；
- 丢失多数派后继续产生新的 Provider 副作用；
- 共享 PVC 上的多写者；
- 跨集群双向复制；
- 在线动态增加或移除 voting member；
- 通过增加 Replica 提升单 Project 吞吐；
- 当前版本已经实现本文配置、命令或协议。

## 3. 术语

| 术语 | 定义 |
|---|---|
| Primary | 当前唯一允许提交权威 mutation 和启动 Provider 副作用的节点 |
| Replica | 复制并回放权威日志、可作为候选节点的 Heimdall 进程 |
| Leader | 共识协议内部名称；在用户层等同于 Primary |
| term | 一次 Primary 任期；每次合法选举单调增加 |
| index | 复制日志中的单调位置 |
| durable index | 已在本节点稳定存储并完成 `fsync` 的最高 index |
| commit index | 已被多数 voting members 持久化、不可被合法新 Primary 覆盖的最高 index |
| applied index | 已确定性应用到本节点本地状态的最高 committed index |
| ownership epoch | 对 Project 权威所有权的 fencing 代数；首版与 term 绑定 |
| write lease | 由多数派支持、具有明确期限的 Primary 写权限 |
| cluster incarnation | 一次集群生命期的唯一标识；灾备恢复时必须更换 |

## 4. 运行模式和进程模型

### 4.1 Standalone

Standalone 的外部部署契约保持不变：

```text
一个 Heimdall 进程
一个 data_dir
一个 master.key 或 Key Slot descriptor
一个本地 Ledger
无集群端口
无 Replica ACK 等待
```

```yaml
cluster:
  mode: standalone
```

引入 HA 基础后，Standalone 内部允许经过兼容迁移复用统一 mutation 和
snapshot 结构，但不得增加必需外部服务或默认网络监听。

### 4.2 HA

HA 使用三个独立 Heimdall 进程。Kubernetes 中每个 Pod 只运行一个
Heimdall 进程，每个 Pod 使用独立的 `ReadWriteOnce` PVC：

```text
heimdall-0 → PVC 0
heimdall-1 → PVC 1
heimdall-2 → PVC 2
```

不能在同一 Pod 中启动多个 Heimdall 进程冒充 HA，也不能让多个 Pod
共同写入同一个数据目录。

用户配置表达集群成员身份，而不是永久 Primary/Replica 角色：

```yaml
cluster:
  mode: ha
  node_id: heimdall-0
  cluster_id: production-a
  listen: 0.0.0.0:9910
  members:
    - heimdall-0.heimdall-internal:9910
    - heimdall-1.heimdall-internal:9910
    - heimdall-2.heimdall-internal:9910
```

Primary/Replica 是运行时角色，不能固定绑定到 StatefulSet ordinal。
首版只支持固定三 voting members；在线成员变更在单独协议完成前禁止。

## 5. 安全与一致性不变量

实现和发布测试必须持续证明：

1. 任意时刻至多一个节点可以提交新的权威 mutation。
2. 在经过证明的 lease 安全裕量内，至多一个节点有资格启动新的 Provider
   副作用；检查到系统调用之间的残余竞态必须由 fencing token、Provider
   幂等键和 `provider_unknown_outcome` 协议收敛。
3. Budget Reservation 和 `attempt_started` 在 Provider I/O 前完成多数派提交。
4. 已向客户端或管理员确认成功的权威 mutation 不会在合法故障切换后丢失。
5. 只有日志足够新、完成当前任期领导权确认、追平已知 `commit index`，且本地
   状态完整的节点可以晋升并接收写流量。
6. 同一个 mutation ID 不能对应不同内容。
7. 回放相同 committed mutation 必须产生相同权威状态。
8. Provider 结果不明确时不能自动重放，必须保守结算。
9. 已提交对象元数据不能引用合法候选节点缺失的对象。
10. Backup manifest 中的所有状态必须对应同一 `applied index`。
11. 不明确的角色、term、lease、schema、密钥或存储状态一律 fail closed。
12. 活跃流在 Owner 丢失后终止，不宣称透明迁移。

### 5.1 不变量兑现矩阵

| 不变量 | 主要机制 | 必须覆盖的验证 |
|---|---|---|
| 单一权威 writer、已确认 mutation 不丢失 | §7–§9 term/vote、多数派 durable commit、当前任期提交限制 | §19 Raft Figure 8、旧 Primary 回归、响应前崩溃 |
| Provider 副作用收敛 | §8–§9 write lease、安全裕量、内部 fencing token、幂等键、保守未知终态 | §19 分区、进程暂停、lease 到期边界、发送前崩溃 |
| 候选状态完整 | §10–§12 apply 水位、对象 receipt、按需补对象、Master Key/schema gate | §19 缺对象、损坏、落后节点竞选、全量 re-seed |
| 备份单点一致 | §13 snapshot barrier、不可变 staging、统一 applied index、状态根 | §19 并发写入、GC/轮换、staging 崩溃、隔离恢复演练 |

## 6. 权威状态和本地状态

HA 模式下，replicated mutation log 是权威修改顺序。现有持久状态按以下
方式处理：

| 状态 | HA 权威来源 | 本地形态 | 故障切换要求 |
|---|---|---|---|
| Credential、Provider、Deployment、Route | committed mutation | bbolt projection | 必须应用到 commit index |
| Project、Gateway Key、Policy | committed mutation | bbolt projection/cache | 必须应用到 commit index |
| Budget Reservation、Attempt、Settlement | committed Ledger mutation | Ledger WAL + bbolt intent | Provider I/O 前提交 |
| Admin、MFA、Session | committed mutation | bbolt projection | 恢复时失效敏感会话 |
| Pricing 和调整事件 | committed mutation | bbolt + Ledger projection | 不允许时间线回退 |
| Audit | committed mutation 的确定性投影 | 本地 HMAC chain | 绑定 applied index |
| Provider object manifest | committed mutation | bbolt projection | 对象 durability receipt 前置 |
| Provider object bytes | 对象复制协议 | 本地不可变文件 | 候选节点必须完整拥有 |
| Usage checkpoint、Parquet | Ledger 派生 | 本地可重建文件 | 允许从 Ledger 重建 |
| Circuit breaker、连接池 | 节点本地 | 内存 | 不复制 |
| Metrics | 节点本地派生 | 内存 | 聚合展示 |

权威 mutation 不能与 Ledger、bbolt 或 Audit 进行没有恢复协议的普通双写。
节点必须保存每个本地 projection 的 `last_applied_term/index`。应用流程必须：

- 使用唯一 mutation ID；
- 确定性执行；
- 重放幂等；
- 在崩溃后从最后完整 applied index 继续；
- 不在回放期间重新生成时间、随机 ID 或 Provider 结果。

时间、随机 ID、定价证据和外部返回值必须作为 mutation fact 写入规范化
payload。现有 `internal/authority.Mutation` 是演进起点，但需新增 term、index、
cluster identity 和 commit/apply 语义，并通过新的 ADR 冻结格式。

## 7. 复制日志与多数派提交

### 7.1 日志记录

每条记录至少包含：

```json
{
  "cluster_id": "production-a",
  "cluster_incarnation": "inc_...",
  "term": 7,
  "index": 10241,
  "previous_term": 7,
  "mutation_id": "mut_...",
  "schema_version": 1,
  "scope": "project",
  "project_id": "prj_...",
  "operation": "budget.reserve",
  "payload": {},
  "checksum": "sha256:..."
}
```

Checksum 用于检测损坏，不替代节点身份认证和传输完整性。

### 7.2 提交规则

固定三 voting members 时，Primary 本地 `fsync` 加任意一个 Replica 的
相同 `term/index` 持久 ACK 构成多数派。安全语义使用多数派公式，而不是
把 `minimum_replicas_to_write: 1` 当成通用规则：

```text
required durable members = floor(voting_members / 2) + 1
```

首版配置固定 `voting_members = 3`，因此提交要求固定为 2；保留通用公式只是为了
明确多数派安全语义，不表示首版支持其他成员数或动态 membership。

Primary 只能在 mutation 达到 `commit index` 后向调用方确认成功。
Replica ACK 必须表示记录已经写入稳定存储，而不是仅收到或写入 OS cache。

Primary 不能仅凭多数派复制来直接提交旧任期记录。它必须先通过多数派复制一条
**当前任期**记录，再由该记录的提交连带确认此前的完整 prefix；这是防止新
Primary 错误提交或覆盖历史记录的必要限制。每届新 Primary 在接受业务写入前，
必须提交当前任期的 `leadership_established` no-op，并完成本地 apply。

所有权威 mutation 都使用相同的多数派提交规则，包括配置、Credential、
Route、Project、Gateway Key、Admin/MFA、Pricing、Policy、Reservation、
Attempt 和 Settlement。只有可重建派生数据、缓存和 telemetry 可以异步。

节点分别暴露和持久化：

```text
received_index
durable_index
commit_index
applied_index
```

候选节点必须同时满足：

- 日志不旧于投票者；
- 获得固定成员多数派选票，并提交当前任期的领导权确认记录；
- 在接收写流量前，`applied_index` 追平其已知 `commit_index`；
- 所有引用的 Provider objects 完整；
- Master Key generation 和 mutation schema 兼容；
- 本地完整性检查通过。

首版不支持在线扩缩 voting members，避免在 joint-consensus 协议完成前破坏
多数派交集。

## 8. Provider 请求和外部副作用

每个 Provider Attempt 必须遵循：

```text
多数派提交 Budget Reservation
        ↓
多数派提交 attempt_started(request_id, attempt_id, owner term/epoch)
        ↓
重新验证 Primary write lease 和 fencing token
        ↓
调用 Provider
        ↓
多数派提交 Settlement 或 provider_unknown_outcome
        ↓
向客户端完成响应
```

Provider 支持幂等键时，Adapter 必须使用稳定的 `attempt_id`。不支持幂等键时，
故障切换后不得自动重放处于 `attempt_started` 且没有可信终态的调用。

`attempt_started` 必须携带由 `cluster_incarnation + term + lease_epoch` 派生的内部
fencing token。Adapter/SafeTransport 必须在建立连接或写出请求字节的最后可控点
再次校验该 token；Provider 支持幂等键时，还必须把稳定 `attempt_id` 映射为其
幂等键。第三方 Provider 通常不会理解 Heimdall 的 fencing token，因此该 token
只能封锁 Heimdall 内部旧 Owner，不能物理撤回已发到第三方的请求。

若发生以下情况：

```text
Provider 可能已接受请求
→ Primary 未取得可信结果或 Settlement 未提交
→ Primary 故障
```

新 Primary 必须把 Attempt 视为 `provider_unknown_outcome`，保留保守费用，不得假设
Provider 未执行。后续通过超时、Provider 账单对账或人工调整完成处理。

`provider_unknown_outcome` 只表示外部调用是否执行或执行结果不可信；它与
Pricing 中“价格未知”的状态不同，二者不得复用枚举、指标或自动处置逻辑。

客户端重试只有在携带稳定 idempotency key 且其记录仍在保留期内时，才可
确定性返回已有状态。没有幂等键的客户端重试可能形成新的 Provider 调用，
这属于明确公开的边界。

Settlement 无法形成多数派时，不能静默释放 Reservation。活跃 SSE 在 Owner
丢失后终止，客户端可以按幂等契约重连，但 Heimdall 不宣称迁移连接或
Provider exactly-once。

关键失败分支必须确定性落到以下状态：

| 失败点 | 已提交事实 | 恢复处置 |
|---|---|---|
| Reservation 提交前 | 无 | 不调用 Provider，安全重试 |
| Reservation 后、Attempt 前 | reservation | 超时释放或由 committed mutation 关闭 |
| `attempt_started` 后、写出请求前 | reservation + attempt | 证明未写出后可按同一 attempt 恢复；不能证明则标记未知 |
| Provider 可能已接收、Settlement 前 | reservation + attempt | `provider_unknown_outcome`，禁止自动重放 |
| Settlement 已提交、响应前 | 完整终态 | 通过客户端幂等键返回已有终态 |

## 9. Primary 选举、租约与 fencing

### 9.1 共识状态

每个 voting member 必须持久化：

- `current_term`；
- `voted_for`；
- membership；
- log term/index；
- `commit_index`；
- 当前 cluster incarnation。

候选节点只有在日志不旧于投票者时才能获得选票。新 Primary 不得覆盖任何
committed prefix；任何在先任期已提交的记录都必须存在于后续 Primary 的日志中，
这是 Leader Completeness 的硬性选举门禁。

### 9.2 写租约

Primary 必须持有多数派支持的有期限 write lease。实现必须使用单调时钟计算
本地有效期，并设置安全裕量；不得通过比较节点 wall clock 判断租约。

实现和部署必须给出可测量的时间约束，而不是只声明“租约不重叠”：

```text
heartbeat_interval < renewal_deadline < lease_duration
election_min_timeout >= lease_duration + 2 * clock_drift_bound + pause_margin
provider_fence_margin >= clock_drift_bound + scheduler_pause_bound
```

在环境无法给出可信的 `clock_drift_bound` 和 `scheduler_pause_bound` 时，自动 write
lease 必须禁用，只能使用带外人工 fencing 完成切换。这些参数必须来自故障测试，
并作为发布门禁和运行时配置校验的一部分。

新 Primary 的 lease 生效前必须保证旧 lease 不再有效。失去多数派、租约续约
失败、进程长暂停或本地单调时钟有效期到达时，节点必须在期限内：

- 从业务流量入口摘除；
- 拒绝新 Reservation 和 Attempt；
- 不启动新的 Provider I/O；
- 对不能确认的在途调用记录 `provider_unknown_outcome`；
- readiness 进入不可写状态。

每个 Provider I/O 前必须重新验证 lease 和 fencing token，而不是依赖较早的
Replica ACK。校验与实际 socket connect/write 之间仍存在不可消除的竞态窗口；
进程暂停也可能跨过租约边界。因此架构承诺的是在经过证明的时间界限内停止新
副作用，并以内部 fencing、Provider 幂等键和 `provider_unknown_outcome` 收敛，
而不是声称能绝对保证第三方只看到一个发起者。已经进入第三方 Provider 的连接
无法被 Heimdall epoch 物理撤销。

### 9.3 人工切换

第一阶段只支持 operator-managed failover。示例命令属于未来接口：

```bash
heimdall cluster promote \
  --cluster-id production-a \
  --confirm-term 7 \
  --confirm-commit-index 10241 \
  --fencing-proof OLD_PRIMARY_ISOLATED
```

提升前必须通过 STONITH 或等价手段证明旧 Primary 已经终止、断电或被网络
隔离。`--confirm-sequence` 或人工口头确认不能代替 fencing。落后或不完整节点
默认拒绝提升。任何允许数据丢失的强制恢复必须使用独立高危流程，显示预计
丢失范围并写入 Audit。

### 9.4 自动切换

自动切换只有在持久化 term/vote、日志新鲜度、多数派 commit、量化 lease 边界、
内部 fencing、故障矩阵和发布门禁全部通过后才能启用。三个 Heimdall 进程可以在单二进制内
完成投票，不要求部署独立 Sentinel、Etcd 或 Consul。

## 10. Provider object 复制

Provider object bytes 不进入权威 mutation log，避免大对象阻塞共识日志。
对象使用独立、经过认证的分块复制通道。

创建协议：

```text
Primary 写临时文件
→ fsync 文件
→ 计算 size + SHA-256
→ 原子 rename
→ fsync 父目录
→ 向 voting members 发送 object_prepare
→ Replica 校验、fsync、rename、fsync 父目录
→ Replica 返回 durability receipt(node_id, digest, size, term)
→ durability set 满足多数派交集
→ 多数派提交 object manifest mutation
→ 对外返回成功
```

最简单的首版规则是对象在多数 voting members 上持久化后才提交 manifest。
缺少 committed manifest 引用对象的节点不得晋升到对应 index。

对象删除必须先提交 tombstone。只有在以下条件全部满足后才能 GC：

- 所有已知且仍具增量追赶资格的 voting members 都越过 tombstone 安全水位；
- 没有全量同步依赖该对象；
- 没有 Backup pin；
- 保留期和审计要求满足。

长期离线成员不能仅因“不再是合法候选”就从 GC 水位计算中消失。系统必须先以
committed mutation 将其标记为 `requires_full_reseed`，再允许 GC 忽略它的旧水位；
未来若支持成员驱逐，则驱逐本身必须是经过共识提交的 membership change。

应用 committed object manifest 时若本地缺少对象，节点不得推进对应
`applied_index`。它应按 digest 从 Primary 或其他持有 durability receipt 的成员
按需拉取，完成 checksum、`fsync`、原子 rename 和父目录 `fsync` 后再继续 apply；
若对象在所有合格来源都已丢失，则节点 fail closed，并进入全量 re-seed 或灾备
恢复，不能创建空占位文件或跳过该 mutation。

断线续传、重复分片、对象大小上限、digest 不一致、临时文件清理和 orphan
清理必须 fail closed，且不能让元数据引用缺失对象。

## 11. File Master Key

HA 不强制 KMS。File 模式继续是一等运行模式，但所有仍具增量追赶和晋升资格的
voting members 必须持有相同的已激活 Master Key generation，并通过真实 Vault
Key Check 验证。

Kubernetes projected Secret 是只读的，不能沿用“进程原地覆盖 Secret 文件”
的轮换方式。HA File-mode 使用版本化 keyring：

```text
Operator 分发 next key generation
→ 节点通过认证通道报告 Vault Key Check 结果
→ quorum ready
→ committed activate(new generation)
→ 重加密/迁移
→ 创建并演练新备份
→ committed retire(old generation)
```

Secret 使用不同名称或挂载路径保存 `current` 与 `next` generation。离线 Replica
不能永久阻止轮换；它必须先被 committed 标记为 `requires_full_reseed`，并在重新
加入时完成全量同步。备份创建时必须登记 `key_generation_backup_pin`；旧 Key 必须
保留到所有数据、增量合格成员以及仍在保留期内的备份 pin 都不再依赖它。

Master Key 不通过普通复制日志传输。节点只通过已认证通道交换 generation 和
验证结果，不能把明文 Key、完整 fingerprint 或 Secret 放入日志、Metrics 标签
或普通 Audit payload。Secret 仍需独立离线备份和最小 RBAC；KMS 保持可选。

## 12. Replica 全量同步和追赶

Replica 加入或落后超过增量日志保留范围时：

1. Primary 在 committed index `N` 建立一致 snapshot；
2. 固定对象 manifest、Ledger/Audit prefix 和必要日志段；
3. Replica 下载到私有 staging；
4. 验证 cluster identity、schema、Master Key、checksum 和状态根；
5. 原子发布本地数据目录；
6. 从 `N+1` 增量追赶；
7. 完整 applied/object 检查通过后才获得候选资格。

增量追赶期间遇到缺失对象时使用第 10 节的按 digest 补对象协议；如果所需日志、
对象或 Key generation 已越过安全保留水位，Replica 必须放弃增量追赶并重新执行
完整 re-seed，不能靠跳过缺口恢复候选资格。

旧 Primary 重新加入时必须作为 Replica，丢弃未提交的冲突 suffix。日志只有在
所有必要恢复水位、快照和备份 pin 允许后才能裁剪。快照安装中断不得改变当前
可用数据目录，重试必须幂等。

## 13. Replica 在线一致性备份

日常备份优先由健康 Replica 完成，但不是“暂停回放后复制整个目录”。备份由
运行中的 Replica 进程内部协调：

```text
从 Primary 捕获 committed target index N
→ Replica 等待 applied_index >= N
→ 获取本地 snapshot barrier
→ 固定 bbolt、Ledger、Audit、Usage 和对象 manifest 水位
→ 对日志段、Usage 文件和对象建立 pin
→ 在同一文件系统生成不可变 staging snapshot
→ 释放 barrier 并立即恢复 apply
→ 后台从 staging 加密、验证和发布 .hmbk
→ 成功或失败后释放 pin
```

staging 不能通过对仍会变化的源文件创建硬链接来“快照”。bbolt 必须从一致读事务
导出副本；Ledger 和 Audit 必须复制已固定的 prefix；Usage、对象和封闭日志段只有
在已经不可变并受 pin 保护时才可直接复用。可验证具有 copy-on-write 隔离语义的
reflink 可以作为优化，但必须在不支持或验证失败时回退到真实复制。释放 barrier
前应完成 staging 文件及目录的 `fsync`；之后源文件的写入、rename、truncate 或
GC 都不得改变 staging 内容。

整个加密过程不得持续暂停 Replica apply。备份调度器只能选择：

- 健康且完整的 Replica；
- 不是当前唯一 durable ACK 来源的 Replica；
- staging 空间、CPU、I/O 和 lag 预算足够的 Replica。

备份期间若另一 Replica 故障，Primary 根据 quorum 和 ACK freshness 继续或
fail closed。在线备份不要求停止 Primary，但可能影响复制冗余和延迟，不能
宣称业务完全不受影响。

未来接口必须明确它调用运行中 Replica 的管理 API，而不是让离线 CLI 抢占
`data_dir` 锁：

```bash
heimdall backup create-online \
  --replica https://heimdall-1:8081 \
  --output /secure-backups/heimdall.hmbk \
  --key-file /secure-secrets/backup.key
```

HA backup manifest 必须保留当前 Standalone manifest 的全部字段和语义，包括：
`format_version`、`backup_id`、`created_at`、`encrypted`、`metadata`、
`ledger_watermark`、`checkpoint_watermark`、`usage_manifest_version`、
`adjustment_manifest_version`、`adjustment_manifest_watermark`、
`ledger_feature_epoch`、`minimum_ledger_reader_version`、`pricing_state_sha256`、
`pending_intent_sha256`、`pending_intents`、`master_key_fingerprint`、
`key_slot_descriptor_sha256`、`restore_drill_verified`、`build` 和 `files`。HA 不能用
一份更短的新 manifest 替换这些已有恢复证据，只能在兼容版本中增加以下字段：

```json
{
  "cluster_id": "production-a",
  "cluster_incarnation": "inc_...",
  "source_node_id": "heimdall-1",
  "term": 7,
  "commit_index": 10241,
  "applied_index": 10241,
  "mutation_schema_version": 1,
  "minimum_ha_reader_version": "...",
  "state_root_sha256": "sha256:...",
  "object_manifest_sha256": "sha256:...",
  "master_key_generation": 3,
  "audit_checkpoint": {
    "applied_index": 10241,
    "chain_head_sha256": "sha256:..."
  }
}
```

`state_root_sha256` 的规范化输入 schema 必须由 ADR 冻结，至少覆盖 bbolt metadata、
Ledger/Checkpoint 水位、Usage/Adjustment manifest、Pricing 与 Pending Intent 摘要、
Audit checkpoint、Provider object manifest、Key generation 和 mutation schema。
上述每个组成部分都必须显式绑定同一个 `applied_index`，不能只依赖文件创建时间
推断一致性。`restore_drill_verified` 只能由隔离环境中的真实恢复、完整校验和结果
回写流程置为 true，创建 archive 或仅执行 checksum 验证不得自动置位。

`.hmbk` 必须复制到该节点/PVC以外的故障域，Backup Key 继续与 archive 和
Master Key 分开保管；不限定必须使用 S3。

## 14. 恢复模型

### 14.1 Replica re-seed

单个 Replica 或 PVC 丢失时，从健康 Primary 的一致 snapshot 重新播种，再追赶
增量日志。不能从旧 `.hmbk` 直接加入并假设其成员身份仍有效。

### 14.2 全集群灾备恢复

多数派和全部可用成员丢失时：

1. 隔离旧集群网络和流量；
2. 验证 `.hmbk`、Backup Key 和 Master Key generation；
3. 使用新的 `cluster_incarnation` 恢复一个初始节点；
4. 清除或重新签发旧 lease、vote 和成员运行身份；
5. 从初始节点全量同步两个新 Replica；
6. 完整验证 DB、Ledger、Audit、Usage 和 Provider objects；
7. 完成恢复演练和审批后切换业务流量；
8. 旧 Primary 即使重新上线也不得加入或写入新 incarnation。

恢复到 Standalone 也必须显式选择新身份，不能携带旧 Primary lease 自动启动。

## 15. Kubernetes 拓扑与流量入口

基础部署使用：

- 一个三副本 StatefulSet；
- 每 Pod 一个 RWO PVC；
- 一个 Headless Service 用于成员发现和复制；
- 一个客户端 Service 用于 Gateway/Admin 流量；
- Pod anti-affinity 和 topology spread；
- PodDisruptionBudget 保持多数派；
- PVC retention policy 防止 scale down 或卸载误删；
- NetworkPolicy 只允许成员访问复制端口。

普通 Kubernetes Service 不知道哪个 Pod 是 Primary。为了避免依赖外部
controller，推荐所有节点都可接收客户端连接：

- Primary 本地处理；
- Replica 通过认证的内部通道代理，但每次转发前必须实时取得当前 term、Primary
  identity 和有效 lease 证明；缓存的角色标签或地址不能单独授权转发；
- 目标 Primary 在提交 mutation 和发起 Provider I/O 前，仍必须独立复核 term、
  lease 与 fencing token，不能信任 Replica 的代理判断；
- 角色不明确、lease 无效或 Primary 不可达时 fail closed；
- 请求 body 一旦可能转发，不得由 Replica 自动重试到另一个 Primary；
- SSE 连接不迁移，承载连接的 Pod 故障时客户端重连。

如果未来采用 Primary-only EndpointSlice/role label，则必须单独证明 endpoint
更新延迟、旧连接、kube-proxy 缓存和旧 Primary fencing 的安全性，不能仅依赖
selector 声称完成切换。

探针必须区分：

| 探针/状态 | Primary | Replica |
|---|---|---|
| process liveness | 进程和本地存储可运行 | 进程和本地存储可运行 |
| startup | 恢复完成、密钥有效 | snapshot/apply 初始化完成 |
| client readiness | lease/quorum 可写 | 可安全代理到有效 Primary |
| replication readiness | 多数派和 ACK freshness 正常 | 复制通道正常 |
| backup eligibility | false | applied/object/空间均满足 |

慢速全量同步不能被 liveness 反复重启。节点 SIGTERM 后先撤销 client readiness，
再 drain 在途请求；计划内退出 Primary 前先转移角色。超过 deadline 的 Provider
调用进入 `provider_unknown_outcome`。

## 16. 集群通信安全

复制、对象、投票、ACK、快照和内部代理协议必须：

- 强制加密和双向节点身份认证，不提供明文 fallback；
- 绑定 cluster ID、cluster incarnation 和 node ID；
- 校验成员 allowlist；
- 每帧绑定协议版本、term、index、nonce 和完整性；
- 拒绝跨集群消息、旧 term、重放 ACK、重放选票和重放 object receipt；
- 设置连接、帧、对象、并发、速率和 deadline 上限；
- 支持双信任窗口下的证书或集群密钥轮换；
- 不在复制错误、日志或 Metrics 中泄露 Credential、对象内容或密钥。

可以使用本地 Secret 提供集群 CA/节点身份，不要求外部 PKI 服务。认证材料
轮换、成员加入和移除必须有独立 runbook 和故障测试。

## 17. 滚动升级

节点握手必须声明支持的 binary、replication protocol、mutation schema、snapshot
和 Ledger reader feature 范围。不兼容节点不得进入 voting quorum。

升级顺序：

```text
升级一个 Replica
→ 等待 applied/object 完整并重新获得候选资格
→ 升级第二个 Replica
→ 确认多数成员支持新 feature
→ 转移 Primary
→ 升级旧 Primary
→ 单独提交 feature activation mutation
```

二进制 rollout 与新 mutation feature activation 必须分离。StatefulSet 使用受控
partition 或 `OnDelete` 策略，不能让默认滚动更新首先杀死当前 Primary。已经迁移
且不可降级的数据不能由旧二进制原地打开；失败回滚使用升级前完整备份。

## 18. 可观测性与目标

至少提供以下有限基数指标和告警：

- runtime role、term、ownership epoch；
- received/durable/commit/applied index；
- Replica lag records/bytes/seconds；
- ACK 与 `fsync` latency；
- quorum、lease 和 write availability；
- election、promotion、fencing 和 role change；
- object missing/corrupt/sync backlog；
- full sync、snapshot、backup 状态和时长；
- WAL/PVC/staging 使用量和 pin 水位；
- key generation 或节点身份不一致。

Secret、完整 fingerprint、证书主体、Project ID 和高基数对象 ID 不得成为
Metrics label。

正式发布前必须冻结并测试以下目标；未验证的数字只能标记为目标，不能作为
产品承诺：

| 场景 | RPO | RTO/行为 |
|---|---|---|
| 单节点故障，仍有多数派 | 已提交权威 mutation 为 0 | 目标由故障测试确定 |
| Provider 已执行但结果不明 | 不承诺结果 RPO | `provider_unknown_outcome`，禁止自动重放 |
| 丢失多数派 | 在已验证的 lease/fence 边界内停止新副作用 | 等待成员恢复或人工灾备 |
| 全集群从 `.hmbk` 恢复 | 备份 commit index 之后的数据不保证 | 目标由数据规模演练确定 |

## 19. 发布门禁与故障矩阵

HA 不得只通过普通单元测试宣称完成。发布门禁至少覆盖：

- Primary 在本地 append、`fsync`、Replica ACK、commit advance、apply 和响应
  客户端各点崩溃；
- 覆盖 Raft Figure 8 类场景：旧任期记录即使已复制到多数派，也不能在缺少当前
  任期 committed entry 时被直接认定提交；
- Reservation、Attempt、Provider 接收、首个流 token、Settlement 各点崩溃；
- 双向和单向网络分区、长进程暂停、DNS 黑洞、时钟跳变；
- 落后 Replica 竞选、旧 Primary 重新加入、冲突 suffix 截断；
- 对象临时写、rename、目录 `fsync`、receipt、manifest commit 和 GC 各点故障；
- 长期离线成员只有在 committed `requires_full_reseed` 后才能移出 GC/Key
  安全水位，并验证其不能以增量方式重新获得候选资格；
- 磁盘满、只读、慢盘、日志损坏和 checksum 不一致；
- Replica backup 与持续流量、GC、日志轮转、Key rotation 并发；
- Backup staging、加密、发布和 unpin 各点崩溃；
- HA manifest 保留全部 Standalone 恢复字段，且 Ledger、Audit、对象、Pricing、
  Pending Intent 和 Key generation 均能验证绑定同一 `applied_index`；
- 对可变 bbolt/WAL/Audit 源文件执行写入、rename 和 truncate，证明硬链接 staging
  被拒绝，reflink 具备 CoW 隔离，复制回退不会随源文件变化；
- 错误 Master Key generation、节点证书和跨集群重放；
- Pod eviction、SIGTERM、SSE drain、PDB 和节点故障；
- 相邻版本混合复制、角色转移、备份和恢复；
- 单 PVC 丢失 re-seed，以及全部 PVC 丢失后新 incarnation 灾备恢复。

测试必须证明：

- 在已声明的 lease/暂停/时钟边界内至多一个节点有资格启动新的 Provider
  副作用，边界外残余竞态能被 fencing、幂等与保守未知终态收敛；
- 已确认的权威 mutation 不丢失；
- 不明确 Attempt 不会自动重复执行；
- 合法候选节点不存在已提交元数据引用的缺失对象；
- 任意成功发布的备份都能在隔离环境完整恢复。

## 20. 实施阶段

### Phase 0：规范与 Standalone 基础

1. 冻结 mutation、term/index、commit/apply 和状态分类 ADR。
2. 保持 Standalone 部署契约与默认配置不变。
3. 让权威 mutation 可确定性重放并完成 crash-point 测试。
4. 定义内部认证协议、版本协商和 cluster identity。

### Phase 1：固定成员复制

1. 实现三节点固定成员复制日志。
2. 实现多数派 durable commit。
3. 实现全量 snapshot、增量追赶和冲突 suffix 恢复。
4. 实现对象 prepare/receipt/manifest 协议。
5. 完成状态根一致性和故障注入测试。

这一阶段没有安全自动选举时，只能称为 replicated warm standby。人工提升必须
完成外部 fencing；如果尚未实现多数派提交，必须明确可能的数据丢失，不能
宣称零 RPO。

### Phase 2：人工 HA 与在线备份

1. 实现 term/commit 验证和安全人工 promote。
2. 实现客户端代理或经过证明的 Primary Service 路由。
3. 实现 Replica 进程内 snapshot barrier 和在线备份。
4. 实现 re-seed、灾备恢复和新 cluster incarnation。
5. 完成 Key generation 两阶段轮换。

### Phase 3：自动故障切换

1. 实现持久化投票、日志新鲜度和多数派选举。
2. 实现具有量化时钟/暂停边界的 write lease 和旧 Primary 内部 fencing。
3. 完成网络分区、进程暂停、Provider 幂等和 `provider_unknown_outcome` 门禁。
4. 达到并发布经过验证的 RPO/RTO。

## 21. 最终边界

Heimdall HA 是多个独立 Heimdall 进程；Kubernetes 中是多个 Pod、每 Pod
一个进程和一个独立 PVC。整体采用 Redis 风格的 Primary/Replica 运维模型，
但权威费用和安全状态使用多数派提交、term/lease fencing 和确定性回放。

Standalone 继续保持单二进制、本地文件和无必需外部依赖。S3、KMS 和外部
数据库只能是可选增强，不能成为基础 HA 的前置条件。
