# Halro HA 架构设计

- 状态：Proposed（2026-09-20）。**Phase 0a 已实现并合入 `main`**（2026-09-25，
  [#360](https://github.com/akz142857/Halro/pull/360)，`d1633269`）——metadata journal 已经在代码里。
  **Phase 0b 的格式与配置契约已于 2026-09-26 完成仓库侧实现**：`replication` 块已有 fail-closed
  校验，帧、ACK/commit notice、握手、ordering journal 与 `state.json` 有版本化 codec/MAC/golden fixture；`halro cluster`
  子命令、复制连接、Primary/Replica 运行时与提升仍不存在
- 进度：见 [§18.0](#180-进度一览)
- 适用范围：Standalone 向 Primary/Replica 的演进
- 目标版本：不绑定。进入条件是 §1.3 列出的证据，不是某个 tag
- 追踪：[#105](https://github.com/akz142857/Halro/issues/105) Epic、
  [#315](https://github.com/akz142857/Halro/issues/315) Phase 0a、
  [#106](https://github.com/akz142857/Halro/issues/106) Phase 0b、
  [#107](https://github.com/akz142857/Halro/issues/107) Phase 1、
  [#108](https://github.com/akz142857/Halro/issues/108) Phase 2、
  [#109](https://github.com/akz142857/Halro/issues/109) §19 的未决问题、
  [#12](https://github.com/akz142857/Halro/issues/12) 调用方幂等
- 由来与本文引用的全部实测事实：见[附录 A](#附录-a这份设计的由来) 与[附录 B](#附录-b核实过的事实索引)

---

## 1. 这是什么

### 1.1 一句话

三个独立的 Halro 进程，各自独占一个数据目录。Primary 把各权威存储**已经写下的字节**按持久顺序
同步给 Replica；一次请求里只有两个点等待副本确认；Primary 故障时由运维提升一个 Replica，而协议
保证不会有第二个节点确认写。

```
                          客户端 / SDK
                               │
                    ┌──────────┴──────────┐
                    │   客户端 Service     │  全部 Ready 成员进 endpoints
                    └──┬────────┬─────────┘
          ┌────────────┘        └────────────┐
          ▼                                  ▼
┌─────────────────────────┐        ┌─────────────────────┐
│  Primary   halro-0      │        │ Replica  halro-1/2  │
│                         │        │                     │
│ Gateway→准入→账务→Provider│       │ 写请求一律           │
│                         │        │ 503 not_primary     │
│ 权威存储（已成帧的字节）  │        │ + Retry-After       │
│  ledger.wal             │        │                     │
│  audit.log              │ 帧流   │ 落盘 → fsync        │
│  governance.journal     │ +index │ → ack(index, term)  │
│  metadata.journal ──────┼───────►│ → 异步 apply        │
│         └→ halro.db     │  mTLS  │    ↳ halro.db 投影  │
│            （投影）      │        │    ↳ ledger.State   │
└─────────────────────────┘        └─────────────────────┘
     独占 PVC + 目录锁                  各自独占 PVC
```

### 1.2 什么时候**不该**上 HA

放在最前面，因为它决定要不要读下去。Standalone 加一份可验证的离线备份在这些情况下就够了：

1. **你的故障场景不是磁盘丢失。** Standalone 今天对进程崩溃、OOM、`kill -9`、主机断电的 RPO
   **已经是 0**：`Log.Append` 排队后等的是 `writeFull` 加 `Sync()` 都成功才返回
   （`internal/ledger/log.go:558-562, 880-898`），`usage.durability` 的 `strict` 与 `balanced`
   只差合批，不差"是否 fsync 后才返回"（`internal/config/default.yaml:104-105` 的注释亦如此）。
   HA 买的是**盘丢了之后还有一份**。盘在云上已多副本（EBS、Ceph）时，这一项价值有限。
2. **没有人值班。** RTO = 人工响应 + apply 追平 + 提升耗时。无人响应时 RTO 无界，HA 的可用性
   收益归零，只剩 RPO 收益。
3. **你只有两台机器。** 见 §6.4：2 节点不提供故障切换后的可用性。
4. **你不需要 per-Project 的账在节点丢失后仍然完整。** 这是 HA 在本产品里唯一不可替代的收益：
   Credential/Provider/Deployment/Route 都是**无 `ProjectID` 的全局资源**，
   `Registry.ResolveCandidatesFor`（`internal/provider/provider.go:733`）不接受 Project 参数，
   多 Project 共用一套上游凭证是**唯一模式**；上游按凭证出账，收到零个 Halro Project 标识。
   **per-Project 归因只存在于 Halro 自己的 Ledger 里，丢了没有上游可以对账回来。**
   如果 chargeback 只是内部观测而非合同义务，这一项也可以不买。

四条都不成立时——本地 NVMe、有 oncall、三台机器、chargeback 对外——HA 值这个工程。

### 1.3 进入条件

**Phase 0a（metadata journal）是一个独立的 Standalone 变更**，不受下列条件约束：它改的是
Standalone 的写路径，由 Standalone 自己的门禁验证；实测不需要 schema bump，也不要求重新初始化
数据目录。把它拆出来，是为了
避免"先验证 Standalone、再改 Standalone 写路径、再验证"的咬尾。

**Phase 0b 及之后**开工前必须拿到：

1. Standalone 已按[生产验证方案](../verification/production-validation-plan.zh-CN.md)走完 G0–G7
   至少一次。2026-09-18 的执行只完成 E1–E3，4 项 BLOCKED、3 项 PARTIAL
   （[记录](../verification/production-validation-run-260918.zh-CN.md)）。2026-09-25 用干净提交重跑了 G0
   （[记录](../verification/production-validation-run-260925-g0.zh-CN.md)），但 G0 本身仍是 `CONDITIONAL PASS`，
   G1–G7 一项未动，所以本条**仍未满足**。HA 的门禁要证明"已确认的
   账务 mutation 在切换后不丢"，判据来自 Standalone 已建立的账务不变量；没被生产验证过的
   Standalone 做 HA 故障注入，只能证明副本彼此一致，不能证明它们一致地保持了正确的账。
2. 一段生产窗口内的五类数据：`halro_wal_sync_seconds` 分布与 `halro stats` 吞吐；峰值并发与在飞数；
   Project 数与每 Project 速率分布；崩溃后 `RecoverPendingLeases` 的真实触发率与保守结算比例；
   **CPU 余量与 `ledger.State.Apply` 速率**（Linux 基线表明瓶颈在 apply 而非 fsync，而 Replica 侧
   apply 与 Primary 争同一种资源）。
3. 决定是否需要自动故障切换的三个数字：实际需要切换几次、每次人工响应多久、损失是否大于工程成本
   与新增失败模式（见 §19）。

### 1.4 非目标

- Provider exactly-once；
- 活跃 HTTP/SSE 连接跨节点迁移；
- 通过增加 Replica 提升单 Project 或整体写吞吐——HA 会**降低**它；
- 共享 PVC 多写者；跨集群双向复制；多 Primary / active-active；按 Project 分片的 Cluster 模式
  （ADR 0004 保留该名，本文不设计也不占用）；
- 在线增减成员；
- 不停机的 Master Key 代数轮换。

---

## 2. 术语

| 术语 | 定义 |
|---|---|
| Primary | 当前唯一允许确认权威写、发起 Provider 副作用的进程 |
| Replica | 按持久顺序接收并落盘 Primary 写下的字节、随时可被提升的进程 |
| term | 一次 Primary 任期；每次提升（含 `stepdown` 的接收方）单调 +1，持久化在每个节点 |
| promised_term | 节点在 prepare 阶段承诺过的最高 term；承诺后拒绝 ack 任何更低 term 的帧 |
| cluster incarnation | 一次 Master-Key tenure 内的唯一标识；从备份恢复、灾备重建或 Master Key 轮换时必须更换 |
| replication index | Primary 为每批已持久化的存储帧分配的全局单调序号 |
| durable index | 本节点存储帧**与 ordering 记录**都已 `fsync` 的最高 index |
| confirmed index | 已满足提交规则、Primary 可据以向调用方确认的最高 index |
| applied index | Replica 已把帧落到文件、把 metadata 帧应用到 bbolt、推进 `ledger.State` 的最高 index |
| 投影 | `halro.db` 中由 metadata journal 决定的那部分；可从 journal 重建 |
| journal epoch | metadata journal 的一段连续历史；每次离线整文件发布开新 epoch |

---

## 3. 不变量

实现与门禁必须持续证明：

1. **任意时刻至多一个进程确认权威写。** 由 term + promise 保证：Primary 只有在本 term 收到 ack
   才能确认，而承诺了更高 term 的 Replica 永远不 ack 旧 term。
   "至多一个进程**写入**本地存储"是过程保证而非协议保证——物理复制没有存储层 fencing，被隔离的
   旧 Primary 可以在本地写下永不被确认的帧，它们按 §11.2 截断。
2. **至多一个进程发起新的 Provider 副作用。** 检查与 socket 写出之间的残余竞态**不由上游幂等键
   收敛**——Halro 不向任何 Provider 发送幂等键（`internal/provider/` 无 `Idempotency-Key`），
   也不假设哪个 Provider 的推理端点接受它。残余只靠 `recovered_started_unknown_result` 与保守结算
   收敛，其余靠 Provider 账单对账与人工调整。**这是公开的边界，不是待补的缺口。**
3. **只有两类写必须在生效前达到 confirmed**（§6.3）：
   - (a) `ReservationCreated` 与 `AttemptStarted`，在 Provider I/O 之前；
   - (b) **吊销与降权类元数据写**：Gateway Key 撤销、Credential 删除或轮换、Project 停用、Admin
     权限下调、MFA 吊销。这类写的丢失方向是 fail **open**（已撤销的凭据在切换后复活），与账务写的
     保守多计方向相反，不能异步。

   其余权威写本地持久后即向调用方确认，异步复制。
4. **异步帧的丢失只能导致保守多计的可归因行，不能导致行缺失或权限恢复。** Provider I/O 之后丢失的
   Settlement/Finalize 帧，其 reservation 与 attempt 已 confirmed，新 Primary 的
   `RecoverPendingLeases` 会把它们恢复成带 Project、带金额上界、带机器可读终态的
   `recovered_started_unknown_result` 行（ADR 0011）。
5. 已确认的权威 mutation 在合法切换后不丢失，无例外。
6. **帧级前缀**：Replica 上每个追加式权威存储（Ledger、Audit、Governance、metadata journal）的
   明文帧序列，在任意时刻都是 Primary 曾经经历过的某个持久前缀；`halro.db` 是 journal 前缀的投影
   加节点本地键（§5.2）。因此提升等价于"Primary 在那一刻崩溃后重启"——对账务恢复严格成立，对派生
   存储与投影由 §11.2 的回退协议补齐。
7. 同一 replication index 不能对应不同字节；同一 Ledger/Audit sequence 不能对应不同帧。
8. Provider 结果不明确时不自动重放，保守结算。`recovered_started_unknown_result` 只表示外部调用
   是否执行或结果不可信；它与 Pricing 的"价格未知"状态不同，二者不得复用枚举、指标或自动处置逻辑。
9. 已提交的对象元数据不能引用合法候选节点缺失的对象（§9）。
10. 备份 manifest 中的所有存储对应同一 applied index（§12）。
11. 角色、term、incarnation、schema、密钥或存储状态不明确时一律 fail closed。
12. 活跃流在 Primary 丢失后终止；不宣称透明迁移。
13. 复制不得成为泄露渠道：Credential 密文、对象字节、密钥材料、Audit payload 只以既有封装形态在
    成员间流动，复制层不解密、不记录、不进 Metrics label。播种是例外，按高危动作对待（§11.1）。
14. **协议假设成员诚实。** ack、durable/applied index、receipt 都是成员的声明，Primary 无法验证
    远端 fsync。成员诚实的保证来自 §14 的身份与主机边界，不来自协议——这与第 2 条拿掉幂等键是
    同一种诚实。

---

## 4. 拓扑与配置

2 或 3 个节点。3 节点时"本地持久 + 1 个 Replica 持久 ack"等于多数派；2 节点时等于全部，且**不提供
故障切换后的可用性**（§6.4）。

Kubernetes 中每 Pod 一个进程、一个 `ReadWriteOncePod` PVC。不能在同一 Pod 里跑多个进程冒充 HA，
也不能让多个 Pod 写同一个数据目录。

```yaml
# 没有 replication 块就是 Standalone，部署契约一个字节都不变
replication:
  cluster_id: production-a
  node_id: halro-0
  listen: 0.0.0.0:9910        # 复制与集群管理端口；仅成员可达
  peers:
    - name: halro-1
      address: halro-1.halro-internal:9910
      spki_sha256: "…"        # 每成员公钥 pin：单成员证书泄露可改配置吊销而不换 CA
    - name: halro-2
      address: halro-2.halro-internal:9910
      spki_sha256: "…"
  tls:                        # 集群 CA + 本节点证书；无明文 fallback
    ca_file: /run/secrets/halro-cluster/ca.crt
    cert_file: /run/secrets/halro-cluster/tls.crt
    key_file: /run/secrets/halro-cluster/tls.key
```

块名是 `replication` 而不是 `cluster`：它描述的就是复制；ADR 0004 的 "Cluster" 专指多 HA 分片。
不设置只有一个合法取值的键。`replication` 块不存在时 `config check` 拒绝任何 `halro cluster`
子命令——Standalone 不得悄悄多出一个监听端口。2 节点配置输出一条 warning（§6.4），不是 error。

---

## 5. 复制什么，不复制什么

### 5.1 数据目录

```text
data/
├── .halro.lock            节点本地
├── halro.db               bbolt：投影（A/B/D 类键）+ 节点本地键（C/E 类），见 §5.2
├── metadata.journal       新增：bbolt 投影的 write-ahead 日志，权威，复制
├── cluster/               新增，节点本地：state.json、ordering.journal、totp-watermark、maintenance
├── ledger/                Ledger WAL + 封存代 segments.json
├── audit/                 HMAC 链 Audit
├── governance/            Governance journal
├── provider-objects/      调用方要求 Halro 保管的对象（ADR 0024）
├── usage/                 Parquet/NDJSON 分区（Ledger 派生；checkpoint 在 halro.db 的 meta 里）
├── logs/                  进程日志
└── failures/              failurecapture（默认关闭，仅诊断）
```

### 5.2 `halro.db` 按 `(bucket, key)` 分类

`internal/store/bolt/store.go:59-121` 静态枚举 **41 个 bucket 与 `meta` 下 23 个 key**；另有两个在
别处：`bucketPricingMigrationResolutions`（`pricing_migration.go:19`，A 类）与
`keyShutdownTruncatedAttempts`（`operational_counters.go:11`，E 类）。**分类粒度必须到 key**，因为
`meta` 同时装着四类。Phase 0a 把这张表落成代码里的一张静态表，journal 入口按它决定记不记，并
**拒绝一个事务同时写复制集与本地集**。

| 类 | 内容 | 处理 |
|---|---|---|
| **A 权威元数据** | `credentials` `projects` `gateway_keys` `gateway_key_hash` `providers` `provider_egress_proxies` `deployments` `routes` `redaction_policies` `token_guard_policies` `alert_webhooks` `admin_users` `admin_mfa_*` `provider_resources` `deployment_price_*`（含嵌套 `deployment_price_timeline/<id>`）`deployment_price_pin_intents` `pricing_*` `admin_audit_intents` `cost_adjustment_intents` `model_capability_*` `outcome_definitions` `run_governance_*` `migration_history` `pricing_migration_resolutions`；`meta` 中 `schema_version` `runtime_settings` `instance_*_settings` `instance_id` `admin_bootstrap_completion` `*_audit_intent` `minimum_ledger_reader_version` `ledger_feature_epoch` | 经 journal 复制 |
| **B 会话** | `admin_sessions` | 经 journal 复制；提升时全部失效。刷新至多每会话每分钟一次（`internal/adminauth/session.go:118-128`） |
| **C 节点派生** | `meta/usage_checkpoint` `meta/usage_rollup_state` `usage_checkpoint_segments` `usage_daily_rollup` `meta/token_guard_checkpoint` `meta/audit_checkpoint` `meta/ledger_chain_checkpoint` `meta/governance_checkpoint` `governance_checkpoint_segments` `meta/governance_journal_anchor` `audit_anchors` | **不进 journal**；每节点从自己的 Ledger/Audit/Governance 推进，Replica 只推进到 confirmed index（§6.2）。`token_guard_checkpoint` 含 `BlockedUntil` 封禁状态：它是派生的，但 Replica 上必须禁用 `runUsageMaintenance` 对它的写，否则提升后会被空 manager 覆盖 |
| **D 密钥信封与格式门** | `meta/vault_keyring` `key_slot_descriptor` `audit_hmac_envelope` `ledger_hmac_envelope` `vault_key_check` | 经 journal 复制（Replica 打开 Ledger/Audit MAC 需要它们）；Master Key 本身带外（§10）；轮换 bridge 见 §6.1.4 |
| **E 节点本地运营计数** | `meta/shutdown_truncated_attempts_total` | **不进 journal**。它是本节点 telemetry 的持久化背板（`internal/app/metrics.go:281`），不是账也不派生自任何日志 |
| **C 节点派生（续）** | `route_suspensions` | **不进 journal、不复制**。准入挂起派生自本节点的流量：Replica 没有流量可学，提升后每个 target 用一次失败请求重新学到，代价是一次请求。复制它反而会把 Primary 观测到的拒绝当成 Replica 的事实。清除挂起是管理动作、与 `admin_audit_intents`（A 类）同事务提交，recorder 按"记录 A 类那一半、直通 C 类那一半"处理 |

> **实测更正（Phase 0a 落地，2026-09-24）。** 上一行原文写的是"**这是唯一一处跨类同事务写**"。
> 不是。把检查改成只报告不拒绝、跑完整套测试扫了一遍，生产代码里有**两处**：
>
> 1. 清除路由挂起 + 它的 `admin_audit_intents`（上一行描述的那处）；
> 2. **Key Slot 初始化与 Master Key 改写**：五个 D 类密钥信封与 `meta/audit_checkpoint`（C 类）
>    在同一事务里发布。理由与第一处同源——一个节点持有 Audit HMAC 信封却没有配套 checkpoint，
>    就是拿着一把钥匙对着一条没有可信头的链；而 checkpoint 是每个节点自己的，Replica 从自己的
>    Audit 日志推进它，发布时它本来就是每个节点都从零开始的那一个。
>
> 两处都落成 `internal/store/bolt/journal_class.go` 里 `crossClassWrites` 的显式条目：每条列出
> **一个**本地写和它被允许同事务的**全部**复制写，其余一律拒绝。清单是量出来的，不是读代码读出来的。
>
> **这张清单有两道守护**（2026-09-25 补）。每条规则都是对 fail-closed 混写拒绝的一次豁免，也就是
> 一句"未被记录的那一半在 Replica 上无所谓"的承诺，所以它不能悄悄变长，也不能名不副实：
> `TestTheCrossClassAllowlistDoesNotGrowByItself` 把条目数钉成 2——加第三条必须同时把这个数字改掉，
> 理由写进提交；`TestEveryCrossClassRuleIsShapedLikeItsName` 检查形状——`local` 必须真的不复制，
> 每个 `with` 必须真的复制，否则这条规则要么豁免了一个根本不需要豁免的事务（真正的混写就藏在别处），
> 要么把两个本地写叫成了跨类事务。

> **A 类的鉴权成员也钉住了**（2026-09-25 补）。C/E 类一直有 `TestDerivedAndNodeLocalStateIsNotJournalled`、
> D 类有 `TestKeyEnvelopesAreJournalled`，而 A 类——最大的那一类——此前没有，这是反的：一个被误标为
> 派生的 bucket 不只是"在别处重建"，它是**它的撤销永远到不了另一个节点**。Halro 对"这个请求允许吗"
> 的回答是存下来的、不是算出来的：Gateway Key 由记录上的标志位停用，管理员同理，MFA 恢复码靠被消费
> 而作废，Provider 凭据靠删除而撤回。每一条都是一次**撤销**，而不复制的撤销就是提升后的 Replica
> 放行了 Primary 已经收回的权限——这正是本项目在别处处处拒绝的那种 fail-open。
> `TestWithdrawnAuthorityCannotSurviveAPromotion` 守住它。
>
> 另外两处数字更正：§6.1.1 写 `db.Update` 94 处，实测 **96** 处（另 `db.Batch` 1 处）；
> `meta` 的分类必须按 `meta/<key>` 作标识去做跨类判断，按 bucket 名判断会把**每一个**
> 写 `meta` 的事务都判成混写——这是第一版实现真实犯过的错。

### 5.3 其它状态

| 状态 | HA 处理 |
|---|---|
| Ledger / Audit / Governance / metadata journal 帧 | 物理复制，明文帧相同 |
| Ledger 封存代滚动 | 作为结构事件进流，携带整条 `Segment`；压缩形态节点本地（§6.2.3） |
| Provider object 字节 | 独立对象通道 + receipt，manifest 提交前置（§9） |
| Master Key / Key Slot descriptor | 不经复制流；所有节点持同一密钥（§10） |
| Usage 分区 | 不复制；只有 Primary 导出 Parquet |
| failurecapture | 节点本地；切换后不承诺可读 |
| 进程日志、Metrics、连接池、告警队列 | 节点本地；只有 Primary 发告警与 Audit 锚点 |
| 准入挂起（`internal/routegate`） | 短时的（探针、可用性窗口）只在内存，重启即忘；长时的（凭证失效、额度用尽）落 `route_suspensions`，节点本地不复制，见 §5.2 |
| 活跃 HTTP/SSE | 不复制；Primary 丢失即终止 |
| 限流窗口、并发计数、Token Guard EWMA | 不复制；提升后从零开始，与重启一致 |
| `cluster/*` | 节点本地；`state.json` 与 `ordering.journal` 带 MAC（§8.1） |

---

## 6. 复制模型

### 6.1 metadata journal：让 bbolt 成为投影

#### 6.1.1 为什么需要

bbolt 没有日志，无法直接复制。`internal/store/bolt` 里的写事务全部在包内（实测 `db.Update`
**96** 处 + `db.Batch` 1 处，包外无调用），Phase 0a 把它们收敛到**一个**会记录操作的事务入口。
这条路径在 **Standalone 也启用**——只在 HA 里存在的写路径永远得不到 Standalone 每天的测试覆盖。

> **实测更正（2026-09-24）：不需要重新初始化，也不需要 schema bump。** 本节原文预判两者都要。
> 落地后都不要：已有数据目录没有 journal，attach 就以"当前这个 bbolt 文件"作为 epoch 1 的起始
> 投影发布一份新的，并把派生出来的密钥封进 `meta/metadata_hmac_envelope`。拿真实数据目录的
> 副本跑过：启动干净、`metadata journal epoch published epoch=1`、`halro doctor` 报
> `epoch 1 authenticated through sequence 1`、重启续用同一 epoch。
>
> 这里没有沿用 Audit/Ledger 密钥那条"轮换过就拒绝派生"的门禁，是因为两者的失败形状不同：
> 那两条链轮换后派生会得到一把从未签过名的密钥，于是每一帧历史都报成被篡改——信封丢失读起来
> 像一次攻击。journal 打开时整条链当场验签，错密钥在第一帧就是 `ErrCorrupt`；而一个早于 journal
> 的实例根本没有历史帧可以作废。

#### 6.1.2 事务入口

bbolt 只有 post-commit 钩子（`Tx.OnCommit`），没有 pre-commit 钩子；但 `DB.Begin(true)` 与
`Tx.Commit()` 是公开 API，`DB.Update` 只是两者加一个回调。入口自己持有事务：

```text
Begin(true)
→ 回调在 recorder 上写（Put / Delete / CreateBucketIfNotExists / DeleteBucket；bucket 以路径表示）
   recorder 按 §5.2 分类：C/E 类直通不记录；A/B/D 类记录；同一事务混写两类 → 拒绝
   例外一处：`route_suspensions`（C）的清除与 `admin_audit_intents`（A）必须同事务，
   见 §5.2 该行——recorder 记录 A 类那一半，C 类那一半直通
→ 回调全部成功
→ journal 追加一帧 {epoch, sequence, prev_hash, ops, MAC} 并 fsync
→ 同一事务内 Put meta/applied_journal_sequence
→ Commit()
```

打开时若 journal 尾部超过 bbolt 的 `applied_journal_sequence`，重放缺失事务（put/delete/建删 bucket
天然幂等）；若 bbolt 领先 journal，fail closed（唯一例外见 §6.1.4）。

**`db.Batch` 被替换。** `db.Batch` 内部调用 `db.Update`，回调可能跑多次、失败时整批回滚再逐个重跑，
"回调后、commit 前"的位置在它里面不存在。替代是入口之上的自建合并层：排队 → 一个事务顺序跑每个
回调 → 任一失败：Rollback、剔除它、幸存者在新事务重跑 → 全部成功：journal 一帧 + fsync →
`applied_journal_sequence` → Commit。失败尝试的 ops 从未到达 journal。

ADR 0012 Amendment 的三条重跑前提（期望结果不当 error 返回、每次进入重置捕获变量、回调幂等）
原样保留；`TestPricePinPreparationSurvivesBatchSiblingFailures`、`halro_metadata_*` 的 batch 指标、
`BenchmarkMetadataBatchDelay` 的 250 µs 调参迁到新层。

**成本**：bbolt 一次 commit 已是两次 `fdatasync`（脏页、meta），加 journal 是三次。

实测（`BenchmarkDeploymentPricePinCeiling`，同一台 darwin 参考机，前后两次构建对跑，100x；
darwin 的 `Sync` 走 `F_FULLFSYNC`，所以这是悲观上界，不能外推到 Linux NVMe）：

| 并发 | 改动前 attempts/s | 改动后 attempts/s | 变化 |
|---|---|---|---|
| 1 | 52.10 | 34.26 | −34% |
| 8 | 369.0 | 248.6 | −33% |
| 64 | 1210 | 1218 | 持平 |

形状与预期一致：单写者多付一次 fsync，64 并发下合并层把这一帧摊掉，所以代价消失。
§16.3 要的那个"含 bbolt 写的端到端基线"分母，bbolt 那一半就是这条基准（现已包含 journal），
另一半是 `internal/budget` 的 `BenchmarkRequestLifecycle`。

**`metadataBatchDelay` 的 250 µs 已对着多一次 fsync 重新扫过**（2026-09-25，同一台 darwin/M4
参考机，`-benchtime 200x -count=3` 取中位数；改动前是 `8343abad` 的工作树，那一侧走裸
`bbolt.DB.Batch`，改动后是 `main` 的合并层加 journal 帧）：

| delay | 改动前 w=1 | 改动后 w=1 | 改动前 w=8 | 改动后 w=8 |
|---|---|---|---|---|
| 0 | 114.3 | 71.6 | 115.8 | 78.6 |
| **250 µs** | 104.6 | **72.3** | 766.5 | **561.8** |
| 500 µs | 106.1 | 68.6 | 800.0 | 506.8 |
| 1 ms | 93.7 | 67.9 | 733.2 | 540.6 |
| 2 ms | 87.6 | 59.6 | 680.3 | 490.0 |
| 10 ms | 47.7 | 38.5 | 372.5 | 300.2 |

**结论：250 µs 不动。** 它在改动后的两个方向上都是最优——8 并发最高（561.8），单写者也是非零
delay 里最高（72.3，甚至压过 delay=0 的 71.6）。

值得记下的是**代价的形状变了**。`BenchmarkMetadataBatchDelay` 的注释警告"过于慷慨的窗口会让一次
无争用的写比 `db.Update` 还慢"，改动前这在 250 µs 上就已经成立（114.3 → 104.6，−8.5%）；改动后
不成立了（71.6 → 72.3，落在噪声里），因为 journal 那次 fsync 在单写者上约 14 ms/op，把 250 µs
的窗口整个盖住。那个权衡没有消失，只是**拐点右移到 500 µs**（68.6 < 71.6）。所以注释仍然正确，
而下一个调这个常数的人应该知道他调的是一条形状不同的曲线。

#### 6.1.3 谁在写 bbolt

不是"只有 Admin"。每个带 Deployment 的 Attempt 在上游调用前有两次 bbolt 写——
`PrepareDeploymentPricePin` 与 `CommitDeploymentPricePin`（`internal/gateway/service.go:599,693`，
走 `s.batch`，`internal/store/bolt/store.go:1380`）。`provider_resources` 在 Files/batch/deferred 的
请求路径上每资源一次；C 类每分钟一次。

所以 metadata journal 在 HA 下是**请求路径**上的帧流。但因为 §6.3 只让两类写等 ACK，pin 帧不在
等待之列，**price pin 保持在 bbolt A 类即可，ADR 0012 不必修订**。若将来 journal 的字节量成为
问题，"pin 归本地集、取消判断改查已复制的 Ledger 状态"是一个独立的收缩选项，不该藏在 HA 文档里。

#### 6.1.4 journal epoch、密钥域、保留

- **每次离线整文件发布开新 epoch。** File 模式 Master Key 轮换把"用旧 Key 加密的新 Key"（bridge）
  写在离线快照副本里、整文件 rename 发布、再两次 compaction 抹残留
  （`internal/app/key_rotation.go`）；`restore` 与 `CompactSnapshot` 同样是整文件发布。发布之后旧
  journal 不再是投影。规则：发布的 `halro.db` 成为新 epoch 起点（`applied_journal_sequence` 归零、
  epoch +1 写入 `meta`），旧 epoch 文件删除并目录 fsync，删除给出与 `CompactSnapshot` 同等的残留
  保证。**这一条同时封死"旧 Master Key + journal ⇒ 新 Key"的路径：旧 epoch 不存在。**
- **MAC 密钥域** `halro:metadata:v1`，信封在 D 类。帧本身不加密——bbolt 今天也是明文元数据，
  journal 的数据分类与 bbolt 相同（Credential 是 AEAD 密文、口令是 Argon2id 哈希、无调用方正文）。
  **journal 不是第四个存放调用方正文的地方。**
- **保留与裁剪**：只保留 `≥ 上一次投影快照点` 的尾巴（§11.2 路径 A），或在 re-seed 一律走 bbolt
  快照时只保留 `≥ 所有成员 applied_index` 的尾巴，加一个带 MAC 的裁剪锚点记录"裁到哪、链头是什么"。
  Standalone 下的裁剪触发点与 Ledger seal 相同（字节阈值），否则每次 Admin 写都让一个永不收缩的
  文件长大。
- **备份**：journal 的当前 epoch 进 `.hmbk` 的 `files`（§12）。
- **离线 CLI 直写**（`admin reset-password|reset-mfa`、`key rotate|rewrap`、`usage rebuild-summary`）
  全部经过同一入口；不存在绕过 journal 的 bbolt 写。
  **这一条由 `TestTheRecorderIsTheOnlyWayToWriteMetadata` 守着**（2026-09-25 补）。在此之前它只是
  一次性转换 96 个调用点的结果加上 `journal_entry.go` 里的一句注释——在包里任何别的文件写
  `s.db.Update(func(tx *bbolt.Tx) error { … })` 都能编译、能过全部测试，写出的状态没有任何 journal
  帧描述。门禁按 AST 扫包内每个非测试文件，除三个具名豁免（入口层、journal 自身的挂载/回放路径、
  recorder 类型本身）外出现裸 `*bbolt.Tx` 即失败，且豁免过时也失败。它防的是漂移不是规避：豁免文件里
  定义、别处调用的 handler 能过——要拦的是有人顺手写出那个显而易见的形状，而不知道这条路已经关了。

### 6.2 复制流

#### 6.2.1 index 分配与 ordering journal

Primary 为每个权威存储完成 `fsync` 的每一批帧分配 replication index。**分配点在各存储的提交路径
内部同步完成**（Ledger 在 `l.mu` 下、`respondBatch` 之前；Audit/Governance 在 `AppendBatch` 返回前；
metadata journal 在入口 fsync 后 Commit 前），因此帧之间的依赖（pin commit 帧引用它之前的 Ledger
reservation 帧；checkpoint 引用 Ledger 序号）在 index 顺序里保持。

`(index, term, confirmed_index, store, store_generation, first_seq, last_seq, control_metadata, digest)`
写入 `cluster/ordering.journal`。其中不保存 payload，但会保存精确重建原帧 envelope 所需的确认水位与
有界、非敏感控制 metadata。**ordering 记录的 fsync
是 durable index 定义的一部分**：存储 fsync 与 ordering fsync 都完成，index 才算本地 durable，才能
被计入 confirmed。ordering fsync 可与下一批存储写合并，但确认要等它。这样 Primary 崩溃后重启，
任何被确认过的 index 在本地都有记录，不需要"补分配"。

header 还带四个存储在 **index 0** 时的 `(generation, sequence, authenticated_head)`，并与 cluster
identity 一起受 MAC 保护。cursor 区分合法基线与 source-fsync 成功但 ordering 失败的尾巴；head 则绑定
原生日志基线内容（metadata 即使是 `epoch/N, sequence/0` 也必须带 epoch-header head），防止另一份同样
结束在 Ledger sequence 88151、但内容不同且自身验签合法的历史被接到同一 incarnation。bbolt 起始投影
不是追加日志，另由 §11.1 的种子 manifest 文件 digest 绑定；实现播种时必须把该 digest 与 ordering
header 作为同一份已认证种子元数据发布，不能把 metadata journal 的 head 冒充为 bbolt 内容证明。

**为什么不是一条统一日志。** dqlite 式的做法是所有内容进同一条物理日志，全序天然存在，一次 fsync
就够——本文付的第二次 fsync（ordering）正是四个存储换来的。否决它的理由不是工程量：统一日志要求
Ledger/Audit/Governance 的盘上格式变成它的视图，而"复制各存储今天已经在写的那些字节"（§6.2.2）
最大的省钱项恰恰是 ADR 0014/0016 的帧契约与其崩溃测试**一个字节都不用改**。用一次可与下一批合并的
fsync，换掉三套格式的重写与重新验证，是划算的。

#### 6.2.2 帧

```json
{"index": 10241, "term": 7, "incarnation": "inc_…", "store": "ledger", "store_generation": 6,
 "store_sequence_first": 88120, "store_sequence_last": 88151,
 "structural": null, "frames": "<原始字节>", "digest": "sha256:…"}
```

`store ∈ {ledger, audit, governance, metadata}`。`payload` 是各存储**今天已经在写的那些字节**
——Ledger 的 epoch-4/5 帧（自带 CRC、MAC、哈希链）、Audit 与 Governance 的 HMAC 链帧、metadata
journal 帧。格式一个字节都不改，ADR 0014/0016 的帧契约与其崩溃测试原样有效。Replica 用共享密钥
校验后才落盘。任意 index 前缀都是 Primary 各存储在某一时刻共同处于的持久状态。

**因此不需要跨存储的 commit marker。** SQLite WAL 帧末位带提交标记，dqlite 的 follower 只应用到
提交边界；本文没有跨存储的原子单元，也不需要——"应用了 Ledger 的 reservation 帧、还没应用引用它的
pin commit 帧"是 Primary 自己也经过的状态（不变量 6）。单个 metadata 事务仍是单帧原子的（§6.1.2）。

#### 6.2.3 结构事件

Ledger 封存代滚动（Roll）是结构事件：`structural` 携带**整条 `Segment`**（`generation`、
`first/last_sequence`、`length`、`start_hash`/`end_hash`、`end_epoch`、`plain_checksum`、`sealed_at`），
Replica 校验本地 `offset == length`、`plain digest == plain_checksum`、链头 `== end_hash` 后再执行
rename 与 manifest 写入。`compressed`、`stored_length`、`stored_checksum`、文件名后缀是**节点本地**
形态：压缩留在各节点自己的维护 tick 上（gzip 输出不跨 Go 版本稳定），Replica 应用 Roll 时忽略它们。
不变量 6 因此是**帧级**前缀，不是文件字节级。

**但 Replica 上压缩的门槛要重定义，否则上面这句话在 Replica 上是空的。**
`compactLedgerSegments` 今天的门是 `ledgerArchivedThrough = min(Parquet manifest LastSequence,
usage checkpoint Sequence)`（`internal/app/ledger_seal.go:86,119-138`），而 §6.2.4 在 Replica 上
禁用 `exportUsageParquet`——manifest 读不到就 `return 0, false`，**Replica 会永远不压缩任何一代**。
那道门的目的是"这一代可以搬下机器了"（同文件注释），不是正确性，所以 Replica 侧去掉 Parquet 项、
只留 usage checkpoint（C 类，只推进到 confirmed index）。压缩只改节点本地的 `segments.json` 与
文件名，不写 A/B/D 类键、不产生权威帧，因此不威胁帧级前缀。不重定义的代价是 Replica 的封存归档
保持未压缩、约为 Primary 的 5 倍（压缩"takes the archive to roughly a fifth"，`ledger_seal.go:32`）。

**Roll 结构事件只在该代最后一帧 `≤ confirmed_index` 之后发出**，使 Roll 永远不落在未确认后缀里
——否则 §11.2 的截断会需要"撤销 Roll"，而实测表明截断点跨过一次 Roll 时 Ledger 直接拒绝打开
（附录 B 第 12 条）。Replica 自己的 seal tick 按角色禁用。

#### 6.2.4 Replica 的落盘、ACK 与 apply

- 按 index 严格顺序落盘，缺口即停止并请求重传；
- 每个存储追加到与 Primary 相同路径、相同偏移的文件；
- 每批落盘并 `fsync` 后回 `ack(index, term)`，ACK 表示稳定存储不是收到；**ACK 在 apply 之前**，
  apply 异步于 ACK；
- apply：Ledger 帧推进本地 `ledger.State` 与内存 Usage 聚合；metadata 帧**批量**应用到 bbolt（与
  Primary 的合并层同宽，否则逐帧 `db.Update` 约 830 tx/s 会比 Primary 慢一个数量级），且**只应用
  `index ≤ confirmed_index` 的帧**（Primary 在帧流里携带当前 confirmed）；
- Replica 周期性向 Primary 报告 `applied_index`；`durable − applied` 超阈值即 `not_candidate` 并告警；
- Primary 的 commit notice 与数据帧分队列，并保留到**每个已配置 Replica**都报告对应
  `applied_index`；某个 Replica 断线前漏掉最后一条 notice，重连后仍能重发；
- Replica 上**禁用**：seal tick（compact **不**禁用，但门槛按 §6.2.3 重定义）、
  `exportUsageParquet`、Audit 锚点发送、告警投递、
  `runUsageMaintenance` 对 `token_guard_checkpoint` 的写；C 类 checkpoint 只推进到 confirmed index。
  实测 Replica apply 链（帧校验 189k/s、`State.Apply` 602k/s、Usage 聚合 4.2M/s，darwin）高于
  Primary 生成速率，稳态 lag 有界；bbolt 侧的界由批量应用保证。

**为什么 Replica 要派生。** dqlite 式的 follower 什么都不派生，回退因此只是截断文件。本文让 Replica
推进 `ledger.State`、内存 Usage 聚合与 C 类 checkpoint，代价就是 §11.2 的投影回退与
`token_guard_checkpoint` 这类坑。买到的是 RTO：一个不派生的 Replica 在提升那一刻要付满一次冷重放，
而那正是 §16.3 的提升耗时目标要跑赢的数（10 GiB WAL、近 1 MiB 帧 profile：68.578 s）。

### 6.3 提交规则：谁等 ACK

```text
confirmed_index = max{ i : 本地 durable_index ≥ i
                          且 至少 1 个 Replica 回了 ack(≥ i, 当前 term) }
```

所有帧都会被复制、都会推进 `confirmed_index`。差别在**谁要等它**。

#### 6.3.1 必须等

| 帧 | 为什么 |
|---|---|
| `ReservationCreated` | 日预算与 Run cap 的凭据。丢了，新 Primary 的余额偏小，贴着上限的 Project 会被多放行 |
| `AttemptStarted` | "这次调用可能已经花了钱"的唯一记录。丢了，新 Primary 连保守结算的对象都不知道存在——这是**行缺失**，不是金额偏差 |
| 吊销、降权与约束收紧类元数据帧 | Gateway Key 撤销、Credential 删除/轮换、Project 停用或限额/CIDR 收紧、Token Guard/Redaction 收紧、Admin 权限下调或密码/会话代际轮换、MFA 吊销/恢复码消费。丢失方向是 fail **open**：已撤销的凭据或旧密码在切换后复活，或旧节点继续执行更宽的准入规则（墓碑写与 `gateway_key_hash` 都走同一条流）。这是安全事件，不是账务缺口 |

#### 6.3.2 不必等

其余全部：`AttemptSettled`、`RequestFinalized`、price pin 的 prepare/commit、非吊销类 Admin 写、
Audit 帧、Governance 帧。本地 fsync 后即向调用方确认，异步复制。

**为什么安全**：Provider I/O 之后丢失的帧，其 reservation 与 attempt 已经 confirmed，所以新 Primary
的 `RecoverPendingLeases` 会把该 Attempt 恢复成一条**带 Project、带金额上界、带机器可读终态**的
`recovered_started_unknown_result` 行。方向是保守多计而非行缺失——与仓库既有的全部不确定性构造
一致（这也是为什么"记一条空白行"式的方案不可接受：Ledger 的 `Validate` 无条件要求 `ProjectID` 与
`PeriodID`，`internal/ledger/event.go:147-152`，而能说出受影响 Project 的正是丢掉的那些字节）。

#### 6.3.3 其余规则

- 等待发生在本地 `State.Apply` 之后、响应之前，不阻塞其它请求的 append/apply（ADR 0018 的锁外
  append 结构不变）。
- Ledger 已有的群提交（`usage.wal_max_batch` / `wal_flush_interval`）天然成为复制的批次。由于只有
  两个事件等待，一次 Attempt 的额外延迟是 2 次往返 + 远端 fsync。
- **新任期的第一帧是 `leadership_established` no-op**（Ledger structural 帧，带 term）。它被 ack
  之前 `confirmed_index` 在新 term 内未定义，Primary 不确认任何需要等待的写；它是 §11.2 截断判据
  的锚点。
- **ACK 超时不是每请求错误。** 超时让 Primary 进入可恢复的 `ReplicationUnavailable` 状态（需要等待
  的写在 admission 处返回 503，不必等待的写继续；已 append 的帧保留，等 ACK 或截断），不是
  `MarkUnavailable` 的终态——否则帧 N 已持久未 apply、帧 N+1 在 `applyCond` 上永远等待。

### 6.4 Replica 丢失时

"Replica 丢失"只指**不可达**；"可达但因 term 拒绝 ACK"永远不算丢失（它意味着有人在提升，§8.4）。
**只有一种语义**：不静默继续，也不降级为单写。

| 节点数 | Replica 停机（备份/升级/重启） | Primary 重启（Replica 全在） | Replica 全部不可达 |
|---|---|---|---|
| 3 | 另一个 Replica 继续 ACK，无影响 | §8.2 行 3，自动恢复 | §6.3.1 的写不可用：admission 处 503 `replication_unavailable`；readiness **保持就绪**（客户端拿到 503 而不是拒连）；§6.3.2 的写继续 |
| 2 | **§6.3.1 的写停机**；用 `stepdown`（§8.5）或维护哨兵（§12.3）把窗口缩到最短 | 若 Replica 不可达 → §8.2 行 4 等待运维 | 同上 |

#### 2 节点部署的真实语义

**2 节点在 Primary 死亡后，提升出的实例在第二个节点回来之前不可写。** 提升时 `--no-peer-promise`
拿不到任何 promise（唯一的 peer 就是死掉的那个），新 Primary 也就没有任何 Replica 可以 ACK，
§6.3.1 的写全部 503。恢复路径：起一个新节点 → 从幸存者播种（§11.1）→ 可写。

所以 **2 节点不是一个提供故障切换可用性的部署**。它提供的是 RPO=0 的热副本、盘丢失时的快速重建、
以及可以在副本上做备份。要切换后立刻可写，需要 3 个节点。`config check` 对 2 节点输出 warning
说明这一点（不是 error——2 节点合法且有用，只是它买到的东西和运维的直觉不同）。

---

## 7. Provider 副作用与切换

```text
PricePin prepare（metadata 帧）        → 本地持久即可
ReservationCreated 帧                  → **等 confirmed**
AttemptStarted 帧                      → **等 confirmed**
PricePin commit（metadata 帧）         → 本地持久即可
再次核对：本进程仍是 Primary、term 未变、没有收到更高 term / promise
Provider I/O
AttemptSettled / recovered_started_unknown_result 帧 → 本地持久即可
RequestFinalized 帧 → 本地持久即可 → 向客户端完成
```

六个持久事件里只有两个等待。"再次核对"是一个**本地布尔检查**，发生在 SafeTransport 建立连接前的
最后可控点：它挡住的是本进程已经知道自己被取代的情况；**分区期间它永远通过**。挡不住的是"核对
通过后进程被暂停、随后其它节点被提升"——这个窗口由 §8.3 的 promise（旧 Primary 收到 propose 即
停止确认）与运维断言共同关闭，窗口外的残余只落到 `recovered_started_unknown_result`。

切换后新 Primary 启动时执行的正是 Standalone 的 `RecoverPendingLeases`：所有处于 `attempt_started`
且无可信终态的 Attempt 标记为 `recovered_started_unknown_result`，保留保守费用，不自动重放。
recovery event ID 由 `attempt_id + outcome` 派生，旧 Primary 若也曾做过保守结算而未确认，其后缀被
截断而不是冲突。

| 失败点 | 已确认的事实 | 新 Primary 的处置 |
|---|---|---|
| Reservation 确认前 | 无 | 未调用 Provider；客户端可安全重试 |
| Reservation 后、Attempt 前 | reservation | 超时释放 |
| `attempt_started` 后、写出前 | reservation + attempt | 无法证明未写出 → 标记未知 |
| Provider 可能已收到、Settlement 前 | reservation + attempt | `recovered_started_unknown_result`，保守结算 |
| Settlement 已持久但未复制、响应前 | reservation + attempt | 新 Primary 看不到 Settlement → 保守行；**保守多计，不是行缺失** |
| Settlement 已复制、响应前 | 完整终态 | 携带幂等键的客户端重试被 `409 idempotency_completed` 拒绝，不发起第二次 Provider 调用（依赖 #12） |

最后一行由已关闭的 [#12](https://github.com/akz142857/Halro/issues/12) 提供：Chat/Embeddings 已接受
`Idempotency-Key`，生命周期记录挂在 `ProviderResource` 上（A 类，随 journal 复制）。Halro 不保存
同步推理的响应体，因此不会重放既有终态；完成后的重试返回 `409 idempotency_completed`，在明确告诉
调用方结果未被保存的同时阻止第二次 Provider 调用。

---

## 8. 角色、term 与提升

### 8.1 持久化的集群状态

`cluster/state.json`：`cluster_id`、`incarnation`、`term`、`promised_term`、`role`（每次角色变更时
写并 fsync，不是关闭时写）、`durable_index`、`applied_index`、`node_id`、`peers` 快照、投影快照点。
文件带 MAC（Vault 域密钥 `halro:cluster:v1`），`cluster/ordering.journal` 同样带 MAC；文件写权限
攻击者（ADR 0016 定义的对手）不能伪造角色或 term。

### 8.2 启动裁决：没有节点自动成为 Primary

节点启动后先联系全部 peers，得到每个 peer 的 `(term, promised_term, role, incarnation, durable_index)`：

| # | 本地 | peers 报告 | 结果 |
|---|---|---|---|
| 1 | incarnation 与任一可达 peer 不同 | — | fail closed，等待运维（灾备恢复后的旧节点） |
| 2 | 任意 | 任一 peer 的 `term` **或 `promised_term`** 高于本地 term | 降为 Replica；接入时按 §11.2 截断 |
| 3 | `role=primary` | 全部 peers 可达、term 与 promised_term 都等于本地、无 peer 自称 Primary | **恢复 Primary**：干净重启，没有人被提升过（promise 会留下脚印） |
| 4 | `role=primary` | 任一 peer 不可达 | **停在不可写状态**等待；readiness 不就绪。运维用 `promote --self` 恢复（走 §8.3 的 prepare，term +1） |
| 5 | `role=replica` | 某 peer 是 Primary 且 term ≥ 本地 | 以 Replica 接入 |
| 6 | `role=replica` | 无 peer 自称 Primary | 停在 Replica 状态等待提升；readiness 就绪但不收客户端流量 |
| 7 | 任意 | 本地 term 高于所有可达 peer | 不自动成为 Primary；停在不可写状态，报告"term 领先"，等待运维 |
| 8 | 任意 | 两个 peer 同 term 且都自称 Primary | fail closed，告警 `HalroMultiplePrimaries`（协议上不应发生） |

**行 4 是可用性的代价，要写在 runbook 最前面**：Primary 重启时只要任一 Replica 也不可达，它不会
自己恢复服务。提升后直到旧 Primary 回归之前，新 Primary 的任何重启都落在这一行。

### 8.3 提升

```bash
halro cluster status
halro cluster promote --node halro-1 \
  --expect-term 7 --expect-index 10241 \
  --old-primary halro-0 --old-primary-fenced-by pod-deleted-pvc-retained
```

0. **prepare。** 目标选定 `T' = max(term, promised_term) + 1`，向**全部** peers（含被指名的旧
   Primary）发送 `propose(T')`。peer 把 `promised_term = T'` 写入 `state.json` 并 fsync 后才应答，
   应答携带自己的 durable/applied index 与当前角色。从应答起 peer (a) 拒绝 ack 任何 term < T' 的帧，
   (b) 在 §8.2 中把 `promised_term` 当作自己的 term 报告，(c) 拒绝任何 ≤ T' 的后续 propose。发起
   节点自身从此停止 ack 旧 term。需要 ≥ 1 个 promise 才能继续；**任一应答者自称 Primary → 拒绝**。
   旧 Primary 若可达，收到 propose 的那一刻就按 §8.4 退位。
1. 校验 `--expect-term/--expect-index` 与本地一致。
2. **fencing 断言。** `--old-primary-fenced-by` 只接受两种值：`pod-deleted-pvc-retained`（Pod 已删除
   且其 PVC 不会被自动重新挂到新 Pod）与 `node-isolated`（节点断网/断电）。**"进程已 kill"不是
   fencing**：kubelet 会拉起它。断言连同 actor、探测结果写入 Audit（§8.7）。协议内的 promise 已经
   保证旧 Primary 不能再**确认**任何写；断言负责的是它不再向 Provider **发出**新请求。
3. **候选判据比 `(last_frame_term, applied_index)` 字典序，不比裸 index。** 目标的
   `(last_frame_term, applied_index)` 不得小于任一应答 peer 的 `(last_frame_term, durable_index)`；
   若小于，先等本地 apply 追平（时间计入 RTO）或改提别的节点。
   > 缺了这条 up-to-date 规则，可以提升出一个位置更大但属于**旧分叉**的节点，静默丢弃另一支已确认
   > 写。promise 保证的是"没有第二个 Primary"，不是"选中的这个最新"。
4. term = T'，写入 `state.json` 并 fsync。**从这一步起提升不可撤销**：之后失败/崩溃的节点重启时落在
   §8.2 行 7，运维再次 promote 即可继续。
5. 开始接受 Replica 连接、主动连接全部 peers（含旧 Primary——它不会以 Replica 身份来连）通告新
   term，重试直到对方确认或运维显式放弃；让已接入的 Replica 追平到本地 durable index。
6. 写 `leadership_established` no-op 并等它 confirmed。
7. 走 Standalone 启动恢复：Ledger 链校验、`RecoverPendingLeases`（恢复帧走提交规则）、Audit 链校验、
   Admin 会话失效。
8. readiness 就绪，开始收客户端流量。

**`promote --self` 就是对自己执行的 promote**（旧称 resume）：同样 prepare、同样 term +1、同样进
Audit。§8.2 行 4 的节点用它恢复。

**2 节点。** 旧 Primary 死亡时 prepare 永远拿不到 promise，promote 必须允许显式 `--no-peer-promise`；
它是纯粹的人工断言。这没有安全后果：新 Primary 没有 Replica 就确认不了 §6.3.1 的写，所以断言错误
的最坏结果是**两个都不可写**，不是两个都在写。代价见 §6.4。

### 8.4 运行期退位

任何携带 `term > 本地 term` 或 `promised_term > 本地 term` 的消息——propose、帧、ACK 拒绝、握手、
通告——到达运行中的 Primary，它立即：进入 `ReplicationUnavailable`、停止确认、关闭 Gateway/Admin
listener、以非零码退出；重启走 §8.2 行 2。在飞的 Provider 调用按不变量 12 终止，其结算落在未确认
后缀，由新 Primary 的 `RecoverPendingLeases` 保守结算。Ledger 打开态不可截断
（`internal/ledger/log.go:665`），退位只能是退出后截断。

### 8.5 计划内交接 `stepdown`

```bash
halro cluster stepdown --to halro-1
```

1. Primary 撤销客户端 readiness，drain 在飞请求到最后一帧 confirmed（超过 `shutdown_timeout` 的
   Provider 调用按不变量 12 终止并保守结算——但这是运维选的时刻，不是凌晨三点）；
2. 等目标 Replica `applied_index == confirmed_index`；
3. 目标执行 §8.3（prepare 在 Primary 自己参与下必然拿到 promise；旧 Primary 收到 propose 即退位）；
4. 旧 Primary 以 Replica 接入，没有需要截断的后缀。

**升级（§15）、2 节点的备份窗口（§12.3）、节点维护都用它，不走崩溃式切换。**

### 8.6 Replica 上的 Admin 鉴权

登录、MFA 完成、会话续期、step-up、其 Audit checkpoint 在 Standalone 全是 bbolt 写；Replica 不可写
A/B 类键。Replica 的 Admin 面因此：

- 会话存储换成**内存实现**（`adminauth.SessionStore` 是 5 个方法的接口，CSRF 不依赖 store）；
  口令与 TOTP secret 从复制来的 bbolt **只读**校验；
- TOTP 防重放水位写到节点本地文件 `cluster/totp-watermark`（不进 journal、不复制）；跨节点重放因此
  退化为"per-node、≤ 90 s、需口令 + 活码"，与 Standalone 单机的重放窗口同量级；
- `promote` / `stepdown` 属于 Admin 的"总是重新验证"类，不吃会话缓存的 step-up；
- Replica 上登录/step-up 的 Audit 事件不进 Replica 自己的 Audit 链（那是 Primary 的前缀）；它们作为
  promote 事件的一部分，由**新 Primary 在新 term 内**写入，错误的 promote 也因此留下不会被截断的证据。

Replica 只放行：`cluster status`、`promote`、`stepdown`（作为目标）、只读健康端点。其它 Admin 与
全部 Gateway 请求返回 503 `not_primary` + `Retry-After`（不带成员名，避免向 Gateway 客户端泄露拓扑）。

### 8.7 高危动作矩阵

| 动作 | 执行位置 | 鉴权 | Audit 事件（写入者 / term） | 可撤销性 |
|---|---|---|---|---|
| `promote` | 目标 Replica | 本地会话 + 总是重新验证 | `cluster.promote`（新 Primary，新 term；含 actor、expect_term/index、old_primary、fenced_by、promise 应答摘要） | 步 4 后不可撤销 |
| `promote --self` | 本节点 | 同上 | `cluster.promote`（self=true） | 同上 |
| `promote --no-peer-promise` | 目标 | 同上 | 同上 + `no_peer_promise=true` | 同上；2 节点专用 |
| `stepdown` | Primary | 总是重新验证 | `cluster.stepdown`（旧 Primary 在旧 term 写 requested；新 Primary 在新 term 写 completed） | 步 3 前可中止 |
| `leave --confirm` | 本节点，离线 | 目录锁 + 口令 | `cluster.leave`（写入该目录自己的 Audit 链尾，含 cluster_id/incarnation/term/applied_index） | 不可撤销 |
| 播种 | Primary 侧 | 集群身份 + 可选运维审批 | `cluster.seed.requested/served`（含 node_id、index、字节数） | — |
| `backup create --replica` | Replica，离线 | 目录锁 + Backup Key | 事实由 Primary 记：`cluster.backup.reported` | — |

事件名进 `docs/contracts/audit-integrity.md` 的固定 schema。没有 `state.json` 的 `replication` 目录
以 replicated 模式启动时视为首次接入，只允许 re-seed，不允许凭旧数据声称任何 index。

---

## 9. Provider object

对象字节不进帧流：

```text
Primary 写临时文件 → fsync → 计算 size + SHA-256 → rename → fsync 父目录
→ 向 Replica 发送 object_put(digest, size, bytes)
→ Replica 校验 digest、fsync、rename、fsync 父目录 → 回 receipt(node_id, digest)
→ 收到 ≥ 1 个 receipt
→ 对象的 ProviderResource 元数据写入 metadata journal（普通帧流）
→ 对外返回成功
```

**注意这里有一个等待**（receipt），它不在 §6.3.1 的两类之列，但它是对象协议自身的前置条件：元数据
提交前对象必须已在副本上。删除先写 tombstone，对象字节只在 tombstone 达到所有**仍具增量追赶资格**
成员的 applied index 后 GC。长期离线成员不因"不可达"而从水位里消失：Primary 先写一条
`member_requires_full_reseed(node_id)` 元数据帧，GC 才可忽略它的旧水位；它回来时只能 re-seed。

Replica 应用引用某对象的元数据帧时若本地缺该对象，不得推进 applied index，按 digest 向 Primary
拉取；拉不到则 fail closed 进入 re-seed，不创建占位文件。对象封装（ADR 0024 按资源与 Project 派生
的 AEAD）不变；复制层看到的是密文。

---

## 10. Master Key

**所有成员持有相同的 Master Key（File 模式）或指向相同 Key Slot 的描述符（KMS 模式），复制层不传输
密钥。** 握手用 Master Key 派生的域密钥做**挑战-响应**（HMAC 覆盖 mTLS 导出的 transcript +
`cluster_id/incarnation` + 双方 nonce），**不是交换指纹**——指纹是 `sha256(key)`，写在每份备份
manifest 里，可回显。校验失败的节点拒绝接入。

Replica 需要密钥做两件事：校验 Ledger epoch-4/5 帧与 Audit 帧的 MAC（不校验就落盘是把篡改复制到
全集群），以及在提升后解密 Credential。它不需要密钥来落盘 metadata journal 帧。

轮换沿用离线流程（[File 模式 runbook](../runbooks/file-master-key-rotation.md)）：停整个集群，在
Primary 的数据目录上 `halro key rotate`（整文件发布，journal 开新 epoch，§6.1.4），并在旧、新
Master Key 同时可用的 staging 阶段开启**新 incarnation + 新 ordering/state 文件**；随后分发新密钥、
启动 Primary，**Replica 全部 re-seed**。cluster key 由 Master Key 与 incarnation 派生，旧 incarnation
不能在轮换后复用，否则新 Key 无法认证旧 `state.json` / `ordering.journal`。这套 staged 发布属于
Phase 2；在它落地前，含 `replication` 的配置会拒绝 `key rotate` 及其它离线数据写命令。Kubernetes
projected Secret 是只读的，"分发新密钥"意味着换 Secret + 重启，不是进程原地覆盖文件。`retained_ciphertext_rotation` 对 `failures/`、
`provider-objects/` 非空时 fail closed 的规则不变。

KMS 模式下建议每节点独立的 KMS 身份（同一 Key Slot，不同调用者），使 KMS 审计能区分节点、被攻陷
节点可按身份吊销。`hostsecurity`（禁 core dump、`PR_SET_DUMPABLE`）在**每个节点**生效——每个节点
内存里都有 Master Key。

---

## 11. 播种、追赶与投影回退

### 11.1 播种（高危动作）

1. Primary 侧写 Audit `cluster.seed.requested`；可配置 `seed_requires_approval: true`，运维在
   Primary 上 `halro cluster seed approve <node_id>` 后才继续——**播种是一次绕过 Backup Key 的全量
   数据目录导出**，集群证书 + 这一步等于一把数据钥匙（§14.1）；
2. Primary 在 confirmed index `N` 处锁定快照：bbolt 一致读事务导出、Ledger/Audit/Governance/metadata
   journal 固定前缀、被元数据引用的全部对象；
3. Replica 下载到 `data_dir` 内的 `staging/`（0700，文件 0600，完成前不可读，失败即删除）；
4. 校验 cluster identity、schema、密钥挑战、每个文件的 digest；
5. 原子发布为本地数据目录，Primary 写 `cluster.seed.served`；
6. 从 `N+1` 增量追赶；applied 追平且对象完整后才成为提升候选。

### 11.2 截断与投影回退

一个曾任 Primary 而被取代的节点可能持有 `> 新 Primary confirmed_index` 或 term 更低的后缀：

1. **截断点 C** = 新 Primary 的 `leadership_established` 帧之前的 confirmed index，由新 Primary 在
   握手中给出；截断只在 `Log`/`audit.Log`/`governance.Log`/journal 全部关闭时执行。
2. **追加式存储**截到 C 的帧边界。Ledger 文件层接受任意截断；拦截截断的是 bbolt 里的派生 checkpoint
   （下一条）。截断点跨越 Roll 按 §6.2.3 不可能发生；实现上仍要检查并在发生时走 Ledger re-seed。
3. **bbolt 不截断，重建到 C**，二选一（Phase 0b 定）：
   - **路径 A′（推荐）**：只从新 Primary 拉一份 bbolt@C′（C′ ≥ C；本机 `halro.db` 524 KiB，与历史
     不是一个量级），然后从 C+1 正常追赶；
   - **路径 A**：节点周期性在某个已确认 index S 处做 bbolt 快照（`Store.Snapshot`）并记进
     `state.json`，journal 至少保留 `(S, …]`；回退时用快照 S 替换 `halro.db`、重放 journal `(S, C]`。

   两条路径对现有 fail-closed 门**改动为零**：重建出的就是 bbolt@C，而 checkpoint/pin 帧必然排在它
   引用的 Ledger 帧之后，`reconcileLedgerChainCheckpoint`（`internal/app/runtime.go:1647`）、
   `reconcileAuditCheckpoint`（`:1615`）、`restoreGovernanceState`、`RecoverDeploymentPricePins`
   逐条通过。**原地回退 checkpoint 的路径不可行**：需绕过 5 处 "cannot move backwards" 门
   （`store_audit.go:411`、`store_outcomes.go:222,315`、`store_usage.go:93`、`store_settings.go:206`），
   且不充分——被截断的 journal 帧里还有普通权威写（Route、Key、Credential），journal 没有 before-image。
4. **Replica 的 bbolt 应用上限是 confirmed index**（§6.2.4），所以 Replica 侧的截断退化为纯文件尾截断，
   只有曾任 Primary 的节点需要第 3 条。
5. Replica 上**没有绕过 journal 的本地 bbolt 写**（§6.2.4 禁用清单、§12.1 的 `--replica`）——这是
   路径 A/A′ 正确性的前提。
6. usage checkpoint 领先 Ledger 头时现有守门直接丢弃并全量重建，不需要新逻辑；这意味着"提升后不需要
   重建 Usage"只在**未截断**时成立。

Primary 只在所有仍具增量追赶资格成员的 applied index 越过后才裁剪 Ledger 日志与 journal。

---

## 12. 备份与恢复

### 12.1 在 Replica 上做备份

`halro backup create` 会向**被备份目录**追加两条 `backup.create` Audit 帧（归档前 `requested`、归档后
`success|failure`）并推进 bbolt 的 `audit_checkpoint`（`internal/app/backup.go:101,116,815`；真实二进制
实测每次 Audit 记录 +2、bbolt txid +3）。在 Replica 上原样跑会让它的 Audit 链与 Primary 分叉。因此：

```text
Replica 进程停止（§12.3 的序列）
→ 停止前 drain apply 到 durable（applied == durable），记录 applied_index
→ halro backup create --replica --config … --output …
     --replica：不追加 backup.create 帧、不推进 audit_checkpoint、不写任何 A/B/D 类键；
                由 cluster/state.json 的 role=replica 交叉校验，二进制版本必须与 Replica 相同
→ halro backup verify …
→ 启动 Replica，从 applied index 追赶
→ 在 Primary 上 halro cluster report-backup --backup-id … --node halro-1 --applied-index …
```

`--replica` 之外剩余的写（`.halro.lock` 的 pid、`boltstore.Open` 的空事务、`governance.Open` 的
0 字节文件、半帧尾修复、checkpoint 落后时追平）都不破坏帧级前缀。

> **"只读命令"在本文里定义为"不追加权威帧"。** `audit verify` / `ledger verify` 今天也走写锁并改
> `.halro.lock` 与 `halro.db` 字节；`doctor` 是唯一真正不写目录的命令。

归档的 `files` 在现有清单（config、metadata.db、ledger.wal、audit.log、governance.journal、sealed
segments、usage 分区、provider-objects）上增加 `metadata.journal`（当前 epoch）与 `cluster/state.json`、
`cluster/ordering.journal`（作为证据文件，恢复时不复用）。manifest 保留现有全部字段
（`format_version`、`backup_id`、`created_at`、`encrypted`、`metadata{schema_version,txid,…}`、
`ledger_watermark`、`checkpoint_watermark`、`usage_manifest_version`、`ledger_feature_epoch`、
`minimum_ledger_reader_version`、`ledger_chain_head_*`、`ledger_chain_verified`、`governance_*`、
`pricing_state_sha256`、`pending_intent*`、`master_key_fingerprint`、`key_slot_descriptor_sha256`、
`restore_drill_verified`、`build`、`files`），新增：

```json
{"cluster_id": "production-a", "cluster_incarnation": "inc_…",
 "source_node_id": "halro-1", "source_role": "replica", "term": 7, "applied_index": 10241,
 "metadata_journal_epoch": 3, "metadata_journal_sequence": 5120,
 "per_store_head": {"ledger": {"sequence": 88151, "offset": …}, "audit": {…}, "governance": {…}}}
```

`per_store_head` 让 `validateRestoreStage` 能把 `applied_index` 对到各存储的序号；没有它不变量 10 在
restore 时只能相信写入者。`restore_drill_verified` 的语义不变：只有隔离环境的真实恢复能置为 true；
HA 下"真实恢复"指恢复为新 incarnation、播种一个 Replica、并验证旧节点被拒。

### 12.2 恢复

- **单个 Replica 或其 PVC 丢失**：按 §11.1 从 Primary 重新播种。不从旧 `.hmbk` 直接加入。
- **整个集群丢失**：隔离旧网络与流量；校验 `.hmbk`、Backup Key、Master Key；`halro restore` 到一个
  节点——restore 是整文件发布，新 epoch 在暂存阶段就开好、与库同一次 rename 发布（归档里的 journal
  只用于校验 `applied_journal_sequence`，不复用）——并赋予**新的 incarnation**；从它播种其它节点；校验后切流量。
  旧节点即使上线也因 incarnation 不匹配被拒（§8.2 行 1）。
- **从 HA 退回 Standalone**：在选定节点上删除 `replication` 块并显式执行 `halro cluster leave --confirm`
  ——它写 `cluster.leave` Audit、清除 `cluster/`，目录作为普通 Standalone 目录打开；其它节点的目录
  **必须销毁或以新 incarnation 隔离**，scale-down 后 Retain 的 PVC 同样。两个"退回 Standalone"的
  节点各自继续写，就是两个持有全部 Credential 的 Halro。

### 12.3 Kubernetes 上停一个 Replica

删 Pod 会被秒级重建并抢锁，所以：

- **S1 维护哨兵（推荐，Phase 2 交付）**：`halro serve` 在 `replication` 块存在时若发现
  `cluster/maintenance` 文件（`kubectl exec halro-1 -- halro cluster maintenance on` 写入），保持进程
  存活、liveness 通过、**不取目录锁**、readiness 不就绪；备份用 `kubectl exec` 在同一 Pod 内跑；
  `maintenance off` 后取锁、以 Replica 接入追赶。它是"进程在、锁不在"，不引入新的复制状态。
- **S2 `OnDelete` + 模板 patch（不需要新代码）**：patch StatefulSet 模板 `args` 为维护态 → 只删
  `halro-1` → 它持 PVC 不取锁 → `kubectl exec` 跑备份 → patch 回 → 再删一次。**互斥**：模板是维护态
  期间任何别的 Pod 被删（包括为 failover 删 Primary）都会以维护态回来，runbook 必须写死。
- **S3 scale 最高序号**：只能备份最高序号，且须先确认它不是 Primary。

**备份窗口的代价**：3 节点下另一个 Replica 继续 ACK，Primary 写不受影响；2 节点下按 §6.4 写停机，
时长 = 备份 + verify + Replica 启动 + 追赶——运维在窗口前知道这一点，并可先 `stepdown` 让备份落在
非 Primary 上。

---

## 13. 流量入口与 Kubernetes

### 13.1 探针

Kubernetes 只有三种探针，一个 Pod 只有一个 `Ready` 条件：

| 探针 | Primary | Replica |
|---|---|---|
| liveness | 进程与本地存储可运行 | 同；**播种/追赶中不被 liveness 重启**（liveness 不看 lag） |
| startup | §8.2 裁决完成、密钥有效、恢复完成 | 播种/追赶初始化完成；预算必须容纳播种时长（10 GiB 级数据目录不是 180 s 能播完的，按数据规模给 `failureThreshold`） |
| readiness | 进程健康、账务可用、§8.2 裁决完成 | **同样就绪**（在同步且 lag 在阈值内） |

Headless Service 加 `publishNotReadyAddresses: true`（成员发现不依赖就绪）。PDB `minAvailable: 2`
（3 节点）按 Ready Pod 计数。

### 13.2 客户端流量归属

Ready 不表达角色，客户端 Service 要靠别的机制只把流量给 Primary。三种，Phase 2 定一种：

| 机制 | 做法 | 代价 |
|---|---|---|
| **(a) Replica 进 endpoints 并回 503**（默认） | 客户端 Service 选全部 Ready 成员；Replica 对写请求返回 503 `not_primary` + `Retry-After` | 不需要 RBAC/controller/label；稳态下 1/N 的请求先吃一个 503 再由 SDK 重试，p99 多一次往返 |
| (b) Primary 给自己的 Pod 打 label | Service selector 含 `halro.io/role=primary` | 需要 `pods/patch` RBAC——`deploy/kubernetes/README.md` 刻意不给 Halro 任何 API 权限，这是要重新做的决定 |
| (c) 外部 LB 主动健康检查 `/cluster/primary` | 云 LB 只把回 200 的当作健康 | 依赖部署环境；NGINX ingress 等无主动探测 |

**无论哪种，切换期间客户端经历的不是"503 重试"而是整个人工 RTO 内的失败**：Primary 节点失联时
keep-alive 连接没有 RST，客户端先等超时；SDK 的重试地平线（10–20 s）远小于人工 RTO。只有
`stepdown` 与「Replica 全部不可达」这两种情况下客户端才看到带 `Retry-After` 的 503。

### 13.3 拓扑

- StatefulSet，`replicas: 2` 或 `3`，`podManagementPolicy: Parallel`，`updateStrategy: OnDelete`；
- 每 Pod 一个 `ReadWriteOncePod` PVC，`persistentVolumeClaimRetentionPolicy: Retain`；
  **PVC 退役流程**：scale-down 后的 PVC 必须显式销毁或以新 incarnation 隔离（§14.1）；
  **容量**：Replica 的压缩落后于 Primary（一个 tick 一代，播种后还有一段未压缩积压），PVC 按积压
  定容而不是按 Primary 当前占用；若不做 §6.2.3 的门槛重定义，按 5 倍封存归档定容；
- Headless Service（`publishNotReadyAddresses: true`）供成员发现，客户端 Service 按 §13.2；
- anti-affinity / topology spread；PDB `minAvailable: 2`（3 节点）；2 节点不设 PDB；
- NetworkPolicy 只允许成员访问 `replication.listen`（今天的 Standalone 清单没有 NetworkPolicy 对象，
  这是新增承诺）；
- SIGTERM：先撤 readiness、再 drain、再退出；Primary 的计划内退出先 `stepdown`。

现有 `deploy/kubernetes/halro-aws-kms.yaml`（Deployment、`replicas: 1`、`Recreate`）是 Standalone 的
部署，不变；HA 提供一份独立的 StatefulSet 清单，`manifests_test.go` 同样覆盖它。

---

## 14. 集群通信安全

- mTLS，集群 CA 由本地 Secret 提供，不要求外部 PKI；无明文 fallback；`peers[].spki_sha256` 对每个
  成员 pin 公钥，单成员证书泄露可改配置吊销；**CA 轮换走双信任窗口**（新旧 CA 同时受信一个窗口，
  逐节点换证书，再撤旧 CA），有独立 runbook 与门禁用例；
- 每帧绑定 `cluster_id`、`incarnation`、`term`、`index`、digest；跨集群、旧 term、重放 ACK 与重放
  receipt 一律拒绝；
- 握手：mTLS 之上再做 Master Key 域密钥的挑战-响应（§10），证明持有而非回显指纹；
- 成员 allowlist 来自 `peers`，握手校验证书主体与 `node_id` 对应；
- 连接数、帧大小、对象大小、并发、速率、deadline 都有上限；
- 复制错误、日志、Metrics 不含 Credential 密文、对象字节、密钥指纹全文或 Project ID。

`replication.listen` 是 Halro 除 Gateway/Admin/Metrics 之外的第四个监听，只在 `replication` 块存在时
存在；SafeTransport 的出站规则不适用于它，但它遵守同一份 allowlist 思路：只接受 `peers` 里的主体。

### 14.1 威胁模型的变化

Standalone 的信任边界是"一台主机独占一个目录"。HA 把它变成"N 台主机各持同一 Master Key 与同一份
数据"。以下条目要写进 `docs/architecture/threat-model.md`：

| 条目 | Standalone | HA |
|---|---|---|
| *The host root account is trusted* | 一台主机的 root | N 台主机的 root、N 个 PVC 的存储管理员、能读集群证书 Secret 与 Master Key Secret 的每个 RBAC 主体 |
| *One active writer* | 由目录锁保证 | 由 term + promise 保证**确认**唯一；本地写入唯一是过程保证（不变量 1） |
| 数据导出需要 `.hmbk` + Backup Key 或主机文件访问 | — | **播种是第三条路**：集群证书 + 通过挑战-响应（即持有 Master Key）= 全量数据目录；因此播种是高危动作（§11.1） |
| 任一成员失陷 = 全集群失陷 | 不适用 | 成立：`data/` + `master.key` + `cluster leave` = 一个能解密全部 Credential 并调用 Provider 的 Standalone。缓解只有 §8.7 的 Audit 来历、PVC 退役流程、每节点 `hostsecurity` |
| 集群证书的价值 | 不存在 | ≈ 未加密备份钥匙的一半（另一半是 Master Key）；泄露补救 = SPKI pin 吊销或 CA 轮换 |

---

## 15. 升级

`boltstore.Open` 会在每个节点本地跑 schema 迁移（38 条，其中 7 处 `rewriteBucket` 改 value 编码），
所以"先升 Replica"会让新 Replica 本地迁到 N+1 后仍收旧 Primary 的 N 形态 put——既不是投影也不是
合法新库。把迁移的执行点对齐到流的 schema 边界：

- **Replica 不迁移。** `replication` 块存在且 `role=replica` 的节点以第三种模式打开 bbolt：不迁移、
  也不因 schema 落后于二进制而拒绝（今天 `Open` 必迁、`OpenReadOnly` 精确相等门拒绝，需新增）；
  它声明的是"能应用的最高 schema"。
- **迁移只由 Primary 执行**，在它以新二进制打开时，作为一个带 `schema_boundary(N→N+1)` 标记的
  metadata journal 事务（可跨多帧，op 词表含建删 bucket）发出；Replica 应用它。迁移是 bbolt 内容的
  纯函数（无 `time.Now/rand/os`），Primary 在同一前缀上跑，字节一致。
- **握手声明范围**：binary、复制协议、`能应用的最高 bbolt schema`、Ledger reader 范围、journal 版本。
  Primary 不得向声明范围低于自己**当前 schema** 的 Replica 发送 boundary 之后的帧。

```text
逐个升级 Replica 的二进制（以第三种模式打开，继续应用 schema N 的帧）
→ 确认全部 Replica 追平
→ stepdown 到一个已升级的 Replica
→ 它以 Primary 打开：迁移 → schema_boundary 事务 → 其余 Replica 应用
→ 升级旧 Primary 的二进制，它以 Replica 接入并应用 boundary
```

`OnDelete` 下每一步就是 `kubectl delete pod <name>` 一次。**回滚**：boundary 之前的 Replica（旧二进制、
schema N）是即时回滚目标——`stepdown` 回去即可；boundary 之后回滚用升级前的备份恢复为新 incarnation。

---

## 16. 可观测性、目标与损失

### 16.1 指标（全部低基数）

Standalone 已有的写路径指标（`halro_wal_sync_seconds`、`halro_wal_append_{records,batches}_total`、
`halro_accounting_project_lock_{wait,held}_seconds`、`halro_metadata_*`、`halro stats`）保留，复制层增加：

- `halro_cluster_role{role}`、`halro_cluster_term`、`halro_cluster_promised_term`、`halro_cluster_incarnation_info`；
- `halro_replication_index{kind=durable|confirmed|applied}`（Replica 自报 applied，Primary 汇总按 peer）；
- `halro_replication_lag_seconds` / `_frames` / `_bytes`、`halro_replication_apply_backlog_frames`；
  **lag 是运维能直接读的那个数**：在 RPO=0 下它不是"会丢多少"，而是"**提升时要等多久追平**"
  （§8.3 步 3 要求候选 applied 追平），也就是 RTO 里可被监控的那一段。告警阈值即"你愿意让切换多花
  多少时间"；
- `halro_replication_ack_seconds`、`halro_replication_batch_frames`、`halro_metadata_journal_bytes`；
- `halro_replication_state{state=replicating|unavailable}`、进入/退出 unavailable 的计数；
- 对象缺失/校验失败/待同步计数；播种与追赶状态与耗时；`halro_cluster_maintenance`；
- 密钥挑战失败、schema 不兼容、SPKI 不匹配的成员计数。

### 16.2 告警与 runbook（Phase 2 交付）

| 告警 | 条件 | runbook |
|---|---|---|
| `HalroNoPrimary` | 无成员 `role=primary` 超过 N s | §8.3 promote |
| `HalroMultiplePrimaries` | ≥ 2 成员同 incarnation 自称 Primary | §8.2 行 8；隔离、核对 Audit |
| `HalroAwaitingOperator` | 任一成员停在 §8.2 行 4/7 | `promote --self` / promote |
| `HalroReplicationUnavailable` | Primary `state=unavailable` | 查 Replica；§6.3.1 的写已停（503） |
| `HalroReplicaNotCandidate` | `durable − applied` 超阈值 | 查 apply backlog / bbolt |
| `HalroMemberIncompatible` | schema/协议/密钥不兼容 | §15 |
| 现有 `HalroTargetDown`、`AccountingLeaseStale` 等 | 改为 role 感知；recording rules 加 `instance`/`role` 维度 | — |

告警**只由 Primary 发**；提升瞬间新旧 Primary 的去重靠 `(cluster_id, term)` 标签；`alert_webhooks`
的内存队列在 Primary 丢失时丢弃（Standalone 今天就有的语义）。

**deadman**：探测**客户端 Service**（跟随 Primary）作为"服务在"的信号，加每节点 liveness；它是
§1.3 第 3 条 RTO 数据的来源。**Audit 外部锚点**（ADR 0015）增加 `node_id` 与 `term`，只有 Primary 发；
witness 从全部成员拉取并按 `(cluster_id, term)` 归并。分区期间两个节点用同一 `instance_id` 发出
`Records` 相同但 `LastHash` 不同的锚点，witness 会判为篡改——设计上承认它是一个 split-brain 探测器，
不是误报源。

### 16.3 目标

发布前必须冻结、测试并以"目标"而非"承诺"发布：

| 场景 | 目标 | 推导 |
|---|---|---|
| 单 Project 吞吐（3 节点，LAN，NVMe，64 在飞） | **Phase 1 实测后冻结，不预设数字** | 分母是**含 bbolt 写的端到端基线**（pin 路径 + lifecycle 的组合基准；`BenchmarkRequestLifecycle` 的 7051/s 不含 bbolt 写，不能单独当分母）。只有 2 个事件等待，真正的风险在 4 核参考机的 CPU 而非等待次数 |
| 每请求额外延迟 | **2** 次等待 ACK 的持久事件 × (RTT + 远端 fsync + ordering fsync 摊销)，LAN + NVMe 下估 **+0.5–1.5 ms** | 与 Provider 数百毫秒相比可忽略；仍需实测 p99 |
| 单节点故障、仍有 ≥ 1 Replica | RPO = 0；RTO = 人工响应 + 剩余 apply + 提升耗时 | 提升耗时目标：≤ 同规模 Standalone 冷启动（10 GiB WAL、近 1 MiB 帧 profile：68.578 s） |
| `stepdown` | RPO = 0；客户端可见中断 = drain 时长 + endpoints 翻转 | 实测 |
| Provider 已执行、结果不明 | 不承诺结果 RPO；`recovered_started_unknown_result` | ADR 0011 |
| Replica 全部丢失 | §6.3.1 的写不可用（503），其余写继续；RPO = 0 | §6.4 |
| 全集群从 `.hmbk` 恢复 | applied index 之后的数据不保证 | §12 |

### 16.4 会丢什么

**这一节必须进用户文档，不进附录。** RPO=0 是对"已确认的权威 mutation"说的，不是对一切说的：

| 场景 | 损失 |
|---|---|
| 进程崩溃、OOM、断电（盘还在） | **0**。与今天的 Standalone 相同 |
| 计划内 `stepdown` | **0** |
| Primary 节点故障 + 人工提升（3 节点） | 已确认的权威 mutation：**0**。未确认的异步帧按 §7 的失败点表恢复成可归因的保守行——**金额保守多计，不是行缺失** |
| 同上，2 节点 | 同上，但提升出的实例在第二个节点回来之前**不可写**（§6.4） |
| Provider 已执行、结果不明 | 不承诺结果 RPO。**这是 HA 也消除不了的**——Halro 不向任何 Provider 发幂等键（不变量 2） |
| 活跃 HTTP/SSE 连接 | 全部终止，不迁移 |
| 限流窗口、并发计数、Token Guard EWMA | 归零重算，与重启一致 |
| failurecapture 的诊断记录 | 节点本地，不复制；切换后不承诺可读 |
| 尚未复制的 Provider object 字节 | 元数据帧在 receipt 之后才提交（§9），所以**已提交的对象不会缺**；上传中的会失败并由调用方重试 |
| 全集群从 `.hmbk` 恢复 | 备份 applied index 之后的一切 |
| Replica 全部不可达期间 | §6.3.1 的写返回 503（拒绝，不是丢）；§6.3.2 的写继续且本地持久 |

---

## 17. 发布门禁

不得只靠单元测试宣称完成。每条带**可观测的 oracle**：

| 门禁 | oracle | 单机可做？ |
|---|---|---|
| Primary 在 append / fsync / ordering fsync / 发送 / 收 ACK / 推进 confirmed / 响应 各点崩溃；Replica 在收到 / 落盘 / fsync / ACK / apply 各点崩溃 | 重开后各追加文件帧级前缀关系成立；`confirmed` 单调 | 是：kill-point 钩子 + `WrapDurability`（需为 Audit/Governance/bbolt/journal 新增注入缝）+ 进程内双 Runtime + 可控传输（需新建） |
| Reservation / Attempt / Provider 接收 / 首 token / Settlement 各点 Primary 崩溃后提升 | 新 Primary 的账 = 保守结算规则；无重复 Provider 调用（httptest 上游计数） | 是：Attempt 路径需新增 step 钩子 |
| **已确认的 mutation 不丢** | **客户端确认历史记录器**：测试客户端记录每个 200 响应对应的 request_id，提升后逐一在新 Primary 查到终态 | 是（需新建记录器） |
| **至多一个进程确认权威写** | 双 Runtime 分区 + promote，任何时刻 `confirmed` 只在一个 term 内推进；§8.2 表每一行都有用例 | 是 |
| 提升到落后节点被拒绝（含 `(term, index)` 字典序反例：位置更大但 term 更旧的旧分叉节点必须被拒）；旧 Primary 回归被降级、后缀截断、投影重建 | 截断后重开：`reconcileLedgerChainCheckpoint`、`reconcileAuditCheckpoint`、`restoreGovernanceState`、`RecoverDeploymentPricePins` 全部通过；`halro doctor` / `ledger verify` / `audit verify` 与 Standalone 相同 | 是 |
| 吊销类写在切换后不复活 | 撤销一个 Gateway Key → 杀 Primary → 提升 → 该 Key 仍被拒 | 是 |
| **Replica 的本地维护不得触碰被复制的字节** | 在 Replica 上把每个维护 tick 强制触发一遍（seal、`exportUsageParquet`、Audit 锚点、告警投递、`runUsageMaintenance`），四个权威存储的明文帧与 Primary 同偏移前缀仍相同，`token_guard_checkpoint` 未被空 manager 覆盖 | 是 |
| 对象：临时写、rename、目录 fsync、receipt、元数据帧各点故障；缺对象节点不推进 applied | 无占位文件；applied 停在引用帧之前 | 是 |
| 磁盘满、只读、慢盘、帧损坏、MAC 不匹配、密钥挑战失败、SPKI 不匹配 | fail closed 且原因可见 | 部分：真实 ENOSPC/慢盘需目标环境 |
| Replica `--replica` 备份 + 启动追赶，与 Primary 持续写、Roll 并发 | 备份前后 Replica 各权威文件的明文帧与 Primary 同偏移前缀相同；`audit verify` 记录数不变；隔离环境完整恢复为新 incarnation | 恢复演练是人工 |
| 播种中断不改变现有目录，重试幂等；staging 权限 | — | 是 |
| Pod 删除、SIGTERM、SSE drain、PDB、节点故障、维护哨兵 | — | 否：需 kind 集群 |
| 相邻版本混合：§15 每一步；boundary 之前的 Replica 拒绝其后帧 | — | 需第二个二进制的 `tests/` harness |
| 单 PVC 丢失 re-seed；全部 PVC 丢失后新 incarnation 恢复；旧节点被拒 | — | 否：需 kind |
| §16.3 的吞吐与延迟 | 同主机 Standalone vs 3 节点对照 | Linux 参考机 |

不变量 7（index↔字节分叉检测）、10（manifest 跨存储一致，靠 `per_store_head`）、13（复制层不泄露：
复制连接上的字节做 secret canary 扫描）各需要新建观测手段，列入 Phase 1。

---

## 18. 实施阶段

### 18.0 进度一览

截至 2026-09-26，对当前代码实测得到（不是按文档声明抄的；未合入项不冒充 `main` 已交付）：

**这张表是进度的唯一来源。** 对应的 issue 是工作分解，不是第二份记录：两边计数单位不同——本节
按本文 §18 的粗粒度交付物数，issue 按可勾选的细粒度工作项数——所以数字不该互相对照着读。每个
issue 的正文首行都指回这里，冲突时听这里。

| 阶段 | 本文条目 | 完成 | Issue（细粒度工作项） | 备注 |
| --- | --- | --- | --- | --- |
| Phase 0a metadata journal | 5 | **5** | [#315](https://github.com/akz142857/Halro/issues/315) **CLOSED** | 已合入 `main`，3858 行含测试，无未完成项 |
| Phase 0b 复制格式 | 6 | **6（待合入）** | [#106](https://github.com/akz142857/Halro/issues/106) · 当前代码 8/8 | 格式、codec、配置边界完成；无运行时 |
| Phase 1 复制流与 Replica | 5 | 0（基础层进行中） | [#107](https://github.com/akz142857/Halro/issues/107) · 25 项 | 协议状态机与持久化原语已开始；尚无运行时接线 |
| Phase 2 提升、备份与部署 | 5 | 0 | [#108](https://github.com/akz142857/Halro/issues/108) · 20 项 | 未开工 |
| §1.3 进入条件 | 3 | 0 | [#105](https://github.com/akz142857/Halro/issues/105) · 7 项 | 全部未满足，见本节末 |
| §19 自动切换的未决问题 | — | — | [#109](https://github.com/akz142857/Halro/issues/109) · 4 问 | 必须在 Phase 2 之前回答 |

Phase 0b 的 #12 项是对既有 A 类 bucket 的核实；其它七项由 ADR 0027、`internal/replication` 的
版本化 frame/ACK/hello/ordering/state codec 与 `replication` 配置校验完成。格式实现不打开端口：带复制块的
构建在 Phase 1 运行时接入前明确拒绝启动为 Standalone，无复制块的路径保持原样。

**按条目是 11/21，按能力仍是 0。** Phase 0a 让 `halro.db` 成为 journal 的投影；Phase 0b 冻结怎样
标识、认证、排序和传输这些字节。两者都没有建立复制连接、落一份 Replica 数据或执行一次提升。

已落地的部分：

| 位置 | 行数（含测试） |
| --- | --- |
| `internal/metadatajournal/` | 1219 |
| `internal/store/bolt/journal_*.go` | 2206 |
| `internal/vault/metadata.go` | 35 |
| `internal/app/doctor_journal.go` + `metadata_journal_test.go` | 398 |
| 合计 | **3858** |

Phase 0a 留下的唯一一条尾巴——`metadataBatchDelay` 要对着多一次 fsync 重新扫——已于 2026-09-25
扫完，结论是 250 µs 不动，两侧对跑的表见 §6.1.2。**Phase 0a 现在没有未完成项。**

`doctor` 的 `metadata_journal` 只读检查在 `internal/app/doctor_journal.go`；`backup create` 把
journal 收进归档并在 manifest 的 `metadata.metadata_journal_epoch` / `_sequence` 记下它投影的是
哪一段前缀（`TestTheBackupManifestRecordsWhichPrefixItProjects` 钉住这一点）；`restore` 撤下归档
里的 journal 并在发布前于暂存目录开好新 epoch。

§1.3 的三个外部进入条件仍一个都没满足。2026-09-25 把 G0 推到了 `CONDITIONAL PASS`
（[记录](../verification/production-validation-run-260925-g0.zh-CN.md)），G1–G7 未动。2026-09-26 维护者
明确要求继续剩余仓库任务，因此 Phase 0b 的仓库侧实现向前推进；这不把生产证据改写成 PASS，也不允许
Phase 1/2 的本地门禁冒充生产验收。

#### 门外的加固（2026-09-25）

门关着，但 Phase 1 将要依赖的那些**现有**不变量可以现在就钉住——不冻结任何格式，不开任何阶段。
这一轮补了五道，全部当天即通过，这正是重点：每一道守的都是一处"改一个词就破、而且破了不会有
任何测试变红"的地方，而第一次显形的场合会是一次提升。

| 守护 | 守的是什么 | 位置 |
| --- | --- | --- |
| `TestCallerIdempotencyReplicatesWithTheJournal` | 调用方幂等的两个 bucket 必须是 A 类，否则提升后的 Replica 答不出重试的 `Idempotency-Key` | §5.2 / [#106](https://github.com/akz142857/Halro/issues/106) 第 5 项 |
| `TestWithdrawnAuthorityCannotSurviveAPromotion` | A 类里决定"允许不允许"的九个 bucket 必须复制，否则**撤销**到不了另一个节点 | §5.2 |
| `TestTheCrossClassAllowlistDoesNotGrowByItself` | 跨类豁免条目数钉成 2，加第三条必须自觉 | §5.2 |
| `TestEveryCrossClassRuleIsShapedLikeItsName` | 每条豁免的 `local` 真的不复制、`with` 真的复制 | §5.2 |
| `TestTheRecorderIsTheOnlyWayToWriteMetadata` | 包内除三个具名豁免外不得出现裸 `*bbolt.Tx` | §6.1.4 |

其中一条顺带把 #106 的第 5 项（#12 的调用方幂等契约落 A 类 bucket）**核实为已满足**——它不冻结
格式，只是对现状的核实，所以在门之外做完了。0b 的其余七项原样未动。

`metadataBatchDelay` 的重扫（见 §6.1.2）也在这一轮，它是 Phase 0a 留下的最后一条尾巴。


### Phase 0a：metadata journal（独立的 Standalone 变更，[#315](https://github.com/akz142857/Halro/issues/315)）· **已合入 `main` 2026-09-25**（[#360](https://github.com/akz142857/Halro/pull/360)，`d1633269`）

1. ✅ `(bucket, key)` 分类表落成代码（§5.2）；入口拒绝混合事务——`internal/store/bolt/journal_class.go`，
   未分类即拒绝，没有默认值；跨类白名单两条（见 §5.2 的实测更正）；
2. ✅ 事务入口 + 自建合并层替换 `db.Batch`（§6.1.2）——`journal_entry.go`；ADR 0012 的三条重跑前提
   原样保留，`BenchmarkMetadataWriteTransaction` / `BenchmarkMetadataBatchDelay` 迁到新层；
   `metadataBatchDelay` 的 250 µs **已于 2026-09-25 对着多一次 fsync 重新扫过，结论是不动**——
   两侧对跑的表见 §6.1.2；
3. ✅ journal 帧格式、MAC 域 `halro:metadata:v1`、epoch 规则（§6.1.4）、保留/裁剪（带签名的裁剪锚点，
   Standalone 按字节阈值触发，与 Ledger seal 同一次 tick）、崩溃恢复矩阵新增 8 行；
   **不需要 schema bump，也不需要重新初始化**（见 §6.1.1 的实测更正）；
4. ✅ `doctor` 新增 `metadata_journal` 检查（只读：验签整条链、与 bbolt 记录的位置对比、分歧只报不修）；
   `backup create` 把当前 epoch 收进归档、manifest 记 `metadata_journal_epoch` / `_sequence`；
   `restore` 撤下归档里的 journal，并**在发布之前**就在暂存目录里开好新 epoch，所以库和它的
   journal 是同一次 rename 发布的（§12.2）。原本打算留给首次启动去开——实测发现那样会留下一个
   窗口：目录记着 epoch N 而文件不存在，`halro doctor` 报分歧，而它报得没错。
   `config check` 无新增：`metadata.journal` 不由配置定位，路径是数据目录里的固定名字；
5. ✅ 端到端基线基准（§16.3 分母）——见 §6.1.2 的实测表。

### Phase 0b：复制格式（[#106](https://github.com/akz142857/Halro/issues/106)）

进入条件：§1.3 全部满足。**目前三项均未满足；仓库侧按 2026-09-26 的维护者指令继续，生产验收仍被门禁。**

> 唯一的例外是下面第 4 项的一半：它不冻结任何格式，只是对现状的核实，所以在门之外做完了。
> 2026-09-25 实测 `main`：调用方幂等（#12 的契约，Chat/Embeddings 由
> [#342](https://github.com/akz142857/Halro/pull/342) 落地）的生命周期记录写在
> `provider_resources` 与 `provider_resource_idempotency`，**两者都已是 A 类**，所以它们确实随
> journal 复制。`TestCallerIdempotencyReplicatesWithTheJournal` 把这条依赖钉住——分类表读起来只是
> 一列 bucket 名，把这两个之一改成 C 或 E 是一处看上去很局部的单行修改，能通过其它所有测试，却会
> 让重试的 Idempotency-Key 在一个节点上答得出、在另一个节点上答不出，而这件事第一次显形会是在
> 一次提升的时候。

1. ✅ [ADR 0027](../adr/0027-ha-replication-formats.md) 冻结复制帧、ordering journal、
   `cluster/state.json`（含 MAC）、term / promised_term / incarnation / index 与
   `leadership_established`；`internal/replication` 提供版本化 codec、严格 decoder 与 golden fixture；
2. ✅ ADR 0027 冻结 mTLS + SPKI pin + Master Key challenge-response + 全版本范围握手；
3. ✅ 投影回退选择路径 A′：拉取新 Primary 的认证 bbolt 快照，原子发布后把其它存储追到同一 confirmed
   index，再恢复增量；不新增本地快照保留系统；
4. ✅ #12 的 Chat/Embeddings 调用方幂等契约已落在 `provider_resources` 与
   `provider_resource_idempotency` 两个 A 类 bucket；
5. ✅ `config check` 校验 `replication` 的成员身份、1–2 个 peer、语法有效且非 unspecified 的 dial target、唯一 SPKI pin 与强制 mTLS；
   2 节点输出可用性 warning；无块时不产生角色、文件、命令或早期运行时分支；带块但运行时尚未接入时
   fail closed，拒绝悄悄作为 Standalone 启动；
6. ✅ ADR 0027 明确逐项取代 §20 的旧契约；ADR 0004 标为 Superseded in part，分布式状态文档标明
   HA 与未来 Cluster 分片的边界。

### Phase 1：复制流与 Replica（[#107](https://github.com/akz142857/Halro/issues/107)）

> 2026-09-26 已开始第一批**基础层**实现：`internal/replication` 具备持久 ordering journal（append/fsync、
> 部分尾截断、完整后缀缺失拒绝、故障注入缝）、原子 `state.json` 发布、mTLS/SPKI 校验、bounded wire、
> hello/proof、累计 ACK/commit notice、Primary index/待确认状态机和 Replica 顺序落盘状态机，并通过包级
> race 测试。四个权威存储的 fsync→返回区间已有可选 durable hook，bbolt 已有“不迁移、允许落后 schema”
> 的 Replica 打开模式。多角色审查后又补上：帧与 ordering record 显式携带 Ledger generation / metadata
> epoch；自确认帧被拒；ordering 保存确认水位与控制 metadata，使未确认 durable suffix 的精确 envelope
> 可重建，构造器会先验证完整 suffix 再产生网络副作用；header 用 cursor + authenticated head 认证四
> 存储的 index-0 播种基线（校验的是 baseline cursor 处的历史 head，不误拿当前尾部比较；metadata 的
> epoch header 也必须绑定），四个原生存储已有 cursor/read/truncate 适配，启动恢复会截掉同一 generation
> 内 source-fsync 成功但 ordering 未完成的数据帧尾巴；
> frame/commit notice 分队列且 notice 保留到所有
> 已配置 Replica 的 applied ACK；多个队列失败以最高失败 index 为屏障；metadata journal 在 write/fsync
> 不确定后 poison；cluster 与 ordering 文件的目录项屏障可安全重试；含复制块时，所有可能迁移、修尾或
> 写成员数据的离线命令（包括表面查询命令）统一拒绝。Ledger 的请求 receipt 已把
> ReservationCreated/AttemptStarted 的 index 关联回请求；元数据 recorder 把撤销/降权写（含 Project
> 限额/CIDR 收紧、Gateway Key scope/有效期收紧、Token Guard/Redaction 收紧、Admin 密码/会话代际
> 轮换）标成同步确认；recorder-aware cursor 也会记录并确认 MFA authenticator/recovery-code 批量删除，
> 并在 bbolt Commit **之后**等待。Replica 原生 sink 会先验证 Ledger/Audit/Governance/metadata 自身的
> MAC、链和连续性，再 write+fsync；同一批原生字节的精确重传幂等，重传同 cursor 的不同字节则拒绝；
> bbolt Replica 禁止普通 update，只批量 apply confirmed metadata
> 前缀，Ledger `State` 同样只推进 confirmed 前缀。Ledger Roll 已成为一个独立的全局结构事件：Primary
> 先等待该代最后一帧确认，再发布完整 `Segment` 元数据；Replica 在写任何结构状态前逐字段核对本地
> 活跃代，并复用 manifest 的原子发布路径。纯 Roll 不伪造 Ledger 语义记录，投影水位继续指向最后一条
> 已应用记录；Primary 若在 native Roll 发布后、ordering fsync 前崩溃，启动恢复从完整验签的 sealed
> manifest 补齐唯一缺失的结构 index；Replica 在同一窗口崩溃则从 ordering 重建逻辑 cursor，等待
> Primary 原始 Roll frame 重传并逐字段幂等核对，不在本地合成一个 digest 不同的替代 frame。
> 但 app 运行时尚未安装这些 hook，也没有 listener、真实连接管理或角色 HTTP 行为；播种、对象通道和
> 故障注入门禁也未完成。因此下面五个粗粒度交付物仍为 0/5，不能据此声称
> HA 可运行。

1. Primary 侧：提交路径内的 index 分配、ordering journal、批次发送、ACK 聚合、`confirmed_index`、
   `ReplicationUnavailable` 状态、§6.3.1/§6.3.2 的两类写；
2. Replica 侧：顺序落盘、校验、ACK、异步批量 apply（bbolt 上限 confirmed）、apply 自报、禁用清单
   与压缩门槛重定义（§6.2.3）；第三种 bbolt 打开模式（§15）；
3. 结构事件（整条 `Segment`）、Roll 只在 confirmed 后；
4. 播种（含审批、staging）、追赶、`member_requires_full_reseed`、对象通道（§9）；
5. §17 前七项门禁 + 可控传输 + 新增注入缝 + 客户端确认历史记录器。

### Phase 2：提升、备份与部署（[#108](https://github.com/akz142857/Halro/issues/108)）

1. §8.2 启动裁决（8 行）、§8.3 prepare/promise 提升、`promote --self`、§8.4 运行期退位、§8.5
   `stepdown`、§8.6 Replica Admin 鉴权、§8.7 矩阵与 Audit 事件 schema、`cluster status` 规定字段；
2. §12 `backup create --replica`、`report-backup`、归档清单与 manifest、restore 的 epoch 规则、
   `leave`、维护哨兵；
3. §13 StatefulSet 清单与客户端路由三选一；§15 升级顺序与 `schema_boundary` 事务；
4. §16 指标、告警规则与 runbook、deadman 目标、锚点字段；§17 其余门禁；发布目标表；
5. 集群 CA 轮换 runbook；`threat-model.md` 修订（§14.1）。

**Phase 2 完成即交付。**

---

## 19. 自动故障切换：一个未决的问题

**当前设计是人工提升。** 这一条是从 2026-08 的初版继承下来的，理由是：安全的自动选举需要写租约，
而租约需要量化的时钟漂移界与进程暂停界，那套验证是数月工程。

**但这个理由在本文的提交规则下可能已经不成立，这个问题必须在 Phase 2 之前回答。**

推理：租约要买的唯一东西是"至多一个节点有资格发起新的 Provider 调用"。而 §6.3.1 已经要求
`ReservationCreated` 与 `AttemptStarted` 达到 confirmed 才能进 Provider I/O，所以**一个被隔离的旧
Primary 拿不到任何 ACK ⇒ confirmed 不推进 ⇒ 它根本走不到 Provider I/O**。三节点下的分区：

| 分区 | 旧 Primary P | 另一侧 |
|---|---|---|
| `{P, R1} \| {R2}` | P+R1 = 多数派，可以继续确认 | R2 孤立，拿不到 promise，提升不了 |
| `{P} \| {R1, R2}` | P 孤立，确认不了，走不到 Provider | R1 拿 R2 的 promise = 多数派，安全提升 |

两边不可能同时确认——这不是新性质，是 `required_acks=1` 在三节点里本来就等于多数派。

**租约还能多买一样**：已经确认过 reservation、然后进程被暂停、醒来直接调 Provider 的那个残余，租约
会因过期而拦住。但这个残余在**人工切换里同样存在**（§7 已公开），所以它不是"手动 vs 自动"的差别。

若上述推理成立，自动切换需要的只有三样：失败检测器 + 迟滞（防抖，不是正确性机制）；promise 要求
显式写成**多数派**（三节点下恰好等价于现在的"≥1"，但五节点就错）；把 `promote` 自动调一次。

**开工前必须先证伪这四条**：
1. 三节点分区表是否穷尽（非对称分区、级联分区、成员数 ≠ 3）；
2. "隔离的 Primary 走不到 Provider I/O"在**已经在飞**的请求上覆盖多少；
3. 提升的抖动代价（会话作废、在飞 Attempt 保守结算、客户端断连）会不会让自动切换在实践中比人工更糟；
4. 自动化拿不到 `--old-primary-fenced-by` 断言，在"旧 Primary 还活着但只是网络慢"的场景下会发生什么。

扛住了就把它作为 `failover: manual | automatic` 的可选开关（默认 manual）放进 Phase 2.5；扛不住，
人工的结论是对的，但理由要重写成真的那个。

**失败检测放在哪个失败域**是同一个问题的另一半。仓库已有 `halro-deadman`——独立部署、刻意在 Halro
失败域之外。倾向把判定做在它里面，不往 Halro 二进制里塞选举。诚实的边界：deadman 今天只有
`http.MethodGet` 探测加一个 POST 事件（`internal/deadman/http.go:106,124`、`anchor.go:153`），
`Checker` 签名无动作位，无 Halro Admin 凭证，单实例、无 peer、无仲裁。所以这**不是**更便宜的选择，
而是一份尚未启动的独立设计：它要长出仲裁、要持有能调用 `promote` 的凭据、要变成多实例——每一条都
把它推向它今天刻意不是的那个东西。

---

## 20. 取代的既有契约

pre-1.0.0 规则是"错误的构造不得与替代品并存"。本文与以下已 Accepted 的文本冲突，Phase 0b 的格式
ADR 必须写明 `Supersedes`：

| 文档 | 被取代的部分 | 本文的替代 |
|---|---|---|
| ADR 0004 | *Phase 0 constraints*（versioned mutation、ownership epoch、replay 契约）；*Consequences* 第 2–4 条 | 物理复制；term/promised_term；无语义 mutation。ADR 0004 状态改为 Superseded-in-part；三种模式的命名保留 |
| ADR 0004 | "an old leader must be unable to commit mutations or start new Provider calls after a higher ownership epoch exists" | 前半句由 promise 保证（不变量 1）；后半句是运维断言 + `recovered_started_unknown_result`（不变量 2）——**这是实质偏离，本文承认它** |
| distributed-state-ownership.md | Invariants 1（ownership epoch 按 Project）、3（stale ownership token）、5（replaying canonical mutations） | term 是集群级不是 Project 级；promise 替代 token；帧级前缀替代回放等价 |
| ADR 0001 | "bbolt stores transactional metadata" | "bbolt is a projection of the metadata journal plus node-local derived keys" |
| threat-model.md | §14.1 表列的条目 | §14.1 |

**Standalone 的最终边界不变**：单二进制、本地文件、无必需外部依赖。S3、KMS、外部数据库只能是可选
增强，不能成为 HA 的前置条件。

---

## 附录 A：这份设计的由来

2026-08-16 的初版是"三节点自研 Raft + 语义 mutation 日志 + 写租约 + Replica 进程内在线备份"。
2026-09-19–20 期间它经过两轮多角色评审（第一轮 5 个发现型 + 6 个证伪型角色，第二轮与一份"单流
异步复制"并列方案做 6 角色取舍），最终形态是本文。三处结构性改变：

1. **物理复制替代语义复制。** Replica 不重跑业务逻辑，因此不需要确定性回放、不需要冻结一套语义
   mutation schema。
2. **同步等待从"每条账务事件"收缩到"Provider I/O 前的两个事件 + 吊销类写"。** 这是本文与初版在
   成本上的主要差别，也是 `degrade` 一类降级模式失去存在理由的原因。
3. **上游幂等键从安全论证中移除。** 它在代码里不存在，不该出现在论证里。

被否决的"单流异步 + 账本空白记录"方案的失败原因值得记在这里，因为它们是本文若干选择的反面论据：
bbolt `NoSync` 崩溃后没有可补齐的起点（bbolt 上游注释写明 `THIS IS UNSAFE`）；一条"这里有个洞"的
账务记录按现有 Ledger schema **写不出来**（`Validate` 无条件要求 `ProjectID`，而能说出受影响
Project 的正是丢掉的那些字节）；"分叉恢复 ≡ 崩溃恢复"经实测证伪。

2026-09-20 又做过一次与 dqlite（SQLite WAL 帧物理复制 + Raft）的对照评估。结论是本文的分层是对的
而且理由可以说得更准：物理复制在**自己拥有的格式**上便宜，在**别人的引擎**上昂贵——ledger/audit/
governance 是 Halro 自己的成帧日志所以走物理，bbolt 是唯一一个不是 Halro 写的存储，所以 metadata
journal 走 op 级。页级复制 bbolt 被否决的三条已核实理由：524 KiB 的库上每事务约 5 个脏页（页大小
= `os.Getpagesize()`，darwin/arm64 是 16 KiB），在 250 µs 合批下等于每秒复制整库上百遍；脏页集与
写出口在 bbolt 里都未导出，拿到 delta 要永久自养一个 fork；C/E 类节点本地键与 A/B/D 类同住一个
文件，页级复制无法容纳它们。这一轮的产出是 §6.2.1、§6.2.2、§6.2.4 的三段理由，§6.2.3 的压缩门槛
修正，以及 §17 那条"本地维护不得触碰被复制字节"的门禁——它正是 dqlite 的经典崩法（follower 自己
checkpoint）在本文里的对应物。

评审的方法记录：**一份新方案不会因为更短或设计取向更有吸引力，而继承前一份方案的评审成果。**
并列方案必须各自过同一道门。

---

## 附录 B：核实过的事实索引

本文的论证依赖这些事实。它们在 2026-09-19/20 于基线 `e59da1c4` 核实过，其中标「实测」的是运行
代码或真实二进制得到的。**任何一条在实施时都应重新验证，而不是引用本表。**

| # | 事实 | 证据 |
|---|---|---|
| 1 | `Log.Append` 等 `writeFull` + `Sync()` 都成功才返回；`strict`/`balanced` 只差合批 | `internal/ledger/log.go:558-562,880-898`；`internal/config/default.yaml:104-105` |
| 2 | Halro 不向任何 Provider 发送幂等键 | `internal/provider/` 无 `Idempotency-Key`（grep） |
| 3 | `halro.db` = 40 bucket + 23 meta key（`store.go:59-121`），另 2 个在别处 | `store.go:59-121`；`pricing_migration.go:19`；`operational_counters.go:11` |
| 4 | `store/bolt` 写事务 94 `db.Update` + 1 `db.Batch`，包外无调用 | grep；`store.go:1380` |
| 5 | bbolt 无 pre-commit 钩子（只有 `Tx.OnCommit`）；`Begin/Commit` 公开可用 | bbolt v1.5.0 `tx.go:163`、`db.go:767,905` |
| 6 | **实测**：自建合并层原型 16 并发 + 4 失败回调，bbolt 与 journal 各 12 key、2 帧、`applied == seq` | 会话内原型 |
| 7 | **实测**：bbolt 一次 commit = 2 次 fdatasync；加 journal 为 3 次；单写者 106.7 → 73.2 tx/s（darwin） | bbolt `tx.go:566,613`；原型基准 |
| 8 | **实测**：每 Attempt 2 次 bbolt 写（price pin），真实屏障 9 次（5 Ledger fsync + 4 bbolt fdatasync）；Audit/Governance **不在** Attempt 路径 | `internal/gateway/service.go:599,693`；grep |
| 9 | **实测**：同主机 darwin pin 路径 473 attempts/s vs Ledger 1049 lifecycles/s | `BenchmarkDeploymentPricePinCeiling` / `BenchmarkRequestLifecycle` |
| 10 | Linux/NVMe 基线 7051 lifecycles/s（64 在飞，**不含 bbolt 写**），raw append 116k events/s | `docs/verification/standalone-capacity-baseline.md:99-122` |
| 11 | 10 GiB WAL 冷启动 68.578 s（近 1 MiB 帧 profile，opt-in 测试） | `docs/verification/performance-baseline.md:79` |
| 12 | **实测**：截断点跨过一次 Roll 时 Ledger 拒绝打开：`ledger is corrupt: generation 1 is 744 bytes, manifest says 1488`，状态 `AccountingRecoveryRequired` | 会话内复现 |
| 13 | 5 处 checkpoint "cannot move backwards" 单向门 | `store_audit.go:411`；`store_outcomes.go:222,315`；`store_usage.go:93`；`store_settings.go:206` |
| 14 | 启动 reconcile 门 | `internal/app/runtime.go:1615`（audit）、`:1647`（ledger chain） |
| 15 | **实测**：`backup create` 向被备份目录追加 2 条 Audit 帧、bbolt txid +3，`ledger.wal` 不动 | `internal/app/backup.go:101,116,815`；真实二进制对 `halro init` 新目录 |
| 16 | Ledger `Validate` 无条件要求 `ProjectID` + `PeriodID` | `internal/ledger/event.go:147-152` |
| 17 | Credential/Provider/Deployment/Route 无 `ProjectID`；`ResolveCandidatesFor` 不接受 Project 参数 | `internal/provider/provider.go:733` |
| 18 | `ledger verify` / `usage verify` / `doctor` 对 Ledger 尾部空白全部 pass；`ledger seal` 假定连续 | `internal/app/ledger_verify.go:107-114`；`internal/ledger/seal.go:193` |
| 19 | ADR 0013 的 `CostAdjusted` 已于 `8868e85` 整体撤除 | git |
| 20 | **实测**：bbolt `NoSync` 下 `db.Sync()` 检查点（txid 2）在 200 事务后被覆盖（meta 页 201/202）；上游注释 `THIS IS UNSAFE` | bbolt v1.5.0 `db.go:44-55`；会话内复现 |
| 21 | **实测**：Replica apply 链 帧校验 189k/s、`State.Apply` 602k/s、Usage 聚合 4.2M/s（darwin） | `-overlay` 注入基准 |
| 22 | `boltstore.Open` 每次打开跑迁移（38 条，7 处 `rewriteBucket`）；`OpenReadOnly` 精确相等门 | `store.go:1493,1526-1542,1805-1833` |
| 23 | Ledger 打开态不可截断 | `internal/ledger/log.go:665` |
| 24 | Admin 会话刷新至多每会话每分钟一次 | `internal/adminauth/session.go:118-128` |
| 25 | **实测**：TOTP 节点本地水位把跨节点重放限在 ±1 步（+35 s 接受、+65 s 拒绝） | `internal/adminauth/totp.go`；会话内测试 |
| 26 | Master Key 轮换 bridge 写在离线快照副本、整文件 rename 发布、两次 compaction 抹残留 | `internal/app/key_rotation.go`；`vault_material.go:113-125` |
| 27 | deadman 只有 GET 探测 + 一个 POST 事件，无 Admin 凭证，单实例无仲裁 | `internal/deadman/http.go:106,124`；`anchor.go:153`；`cmd/halro-deadman/main.go` |
| 28 | 现有 K8s 清单是 Deployment / `replicas: 1` / `Recreate`，无 NetworkPolicy 对象 | `deploy/kubernetes/halro-aws-kms.yaml:9,14,16` |
| 29 | 观测门禁要求 runbook 链接指向 `/docs/` 下真实文件的 `### <AlertName>` 小节 | `deploy/observability/runbook_links_test.go:13-63` |
| 30 | Ledger 帧 epoch 4/5 带 MAC 与哈希链；`usage.wal_max_batch` 默认 128、`wal_flush_interval` 2 ms | `internal/ledger/log.go:30-42,97-99`；`internal/config/config.go:249-250` |
| 31 | seal 与 compact 是同一 tick 上的两次独立调用，可分别禁用 | `internal/app/runtime.go:1177-1178`；`ledger_seal.go:10-32` |
| 32 | `compactLedgerSegments` 的门是 `min(Parquet manifest LastSequence, usage checkpoint Sequence)`，manifest 读不到即 `return 0, false`；压缩把归档降到约五分之一 | `internal/app/ledger_seal.go:86,119-138,32` |
| 33 | bbolt 页大小 = `os.Getpagesize()`（`Options.PageSize` 全仓未设）；脏页集 `tx.pages` 与写出口 `db.ops.writeAt` 均未导出 | bbolt v1.5.0 `internal/common/types.go:34`、`tx.go:520-546`；`internal/store/bolt/` grep |
