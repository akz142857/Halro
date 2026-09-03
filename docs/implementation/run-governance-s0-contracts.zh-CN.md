# Run Governance S0 契约测试清单

- 状态：S0 冻结
- 来源：[Run Governance 与业务结果 PRD](../prd/prd-run-governance-and-business-outcomes.zh-CN.md)
- 证据：[S0 容量与契约验证](../verification/performance/2026-09-04-run-governance-s0/README.md)

本清单把 PRD 的 MUST、不得和失败语义转成后续实现必须能执行的验收项。
“S0 已证”只代表 test-only probe 已证实设计形状；“S1–S4”表示该项必须在
对应生产实现阶段转为正式契约测试，不能把 probe 当成交付代码。

## A. 身份、租户和授权

| ID | 冻结契约 | 可执行证明 | 阶段 |
| --- | --- | --- | --- |
| RG-A01 | Work Unit、Run ID 由服务端生成；调用方外部 ID 不进入权威 ID | 拒绝客户端指定 ID；碰撞/长度/字符集测试 | S1 |
| RG-A02 | Work Unit、Run、Definition、Outcome 必须属于同一 Project | 每个读写路由做 cross-project 矩阵，统一返回不可枚举错误 | S1/S3 |
| RG-A03 | 知道 Run ID 不构成授权；推理 Key 必须具有 `run:attach` | 无 scope、错误 scope、旧 Key 和正确 scope 矩阵 | S1 |
| RG-A04 | 旧 Key migration 后只能得到 `inference`；新 Key 默认相同 | migration before/after 与 runtime snapshot 测试 | S1 |
| RG-A05 | 创建 Run 不自动授予 `outcome:write` | scope 组合测试 | S1/S3 |
| RG-A06 | Gateway scope 不替代 Admin Session、CSRF 与 Step-up | Admin route auth matrix | S1/S4 |
| RG-A07 | 所有公共 POST 必须检查 Key enabled/expiry、Project、CIDR 和独立写限流 | handler table test，429/401/403 语义固定 | S1/S3 |

## B. Project 与 Run 预算权威

| ID | 冻结契约 | 可执行证明 | 阶段 |
| --- | --- | --- | --- |
| RG-B01 | Project 是上级边界；Run 不得扩张预算、模型、RPM/TPM、并发、CIDR、Token Guard 或脱敏权限 | 每项 Project 拒绝条件加带 Run/不带 Run 对照 | S2 |
| RG-B02 | 一次 Provider 消费只在 Accounting Ledger 结算一次；Run/Work Unit 只是归属 | Ledger replay 与 export reconciliation 不出现第二成本列 | S1/S3 |
| RG-B03 | 准入同时检查 Project 与 Run 的 committed + reserved + pending + new reservation | `TestS0DualAdmissionNeverOverAdmitsSameRun`、正式 property/concurrency test | S0 已证/S2 |
| RG-B04 | 多个 Runs 共享并服从同一 Project cap | `TestS0DualAdmissionManyRunsShareProjectCap` | S0 已证/S2 |
| RG-B05 | 整数加法溢出必须拒绝，不能 wrap 为可用额度 | MaxInt64 边界与 fuzz | S2 |
| RG-B06 | pending 在释放 Project lock 前增加，在 durable Apply 后才删除；WAL fsync 不在 Project lock 内 | kill-point、lock instrumentation 和 benchmark | S2 |
| RG-B07 | Reservation 持久化失败或 Run 权威状态不明时，Provider I/O 必须为零 | injected append/apply failure + fake Provider call count | S2 |
| RG-B08 | 已开始后崩溃的 Attempt 同时保守计入 Project 与 Run，不因 close/expiry/restart 退款 | lifecycle recovery matrix | S2 |
| RG-B09 | settlement 低于 reservation 可恢复 Project 与 Run headroom | `TestS0DualAdmissionSettlementCanRestoreHeadroom` | S0 已证/S2 |
| RG-B10 | Project 日预算为零只表示无日金额 cap；Run cap 仍生效 | zero Project cap + finite Run cap | S2 |
| RG-B11 | Run cap 是 Halro 准入上限，UI/API 不得称为 Provider 发票绝对上限 | API description/i18n snapshot | S4 |

## C. 生命周期和并发顺序

| ID | 冻结契约 | 可执行证明 | 阶段 |
| --- | --- | --- | --- |
| RG-C01 | Run cap 创建后普通应用不可修改；首版续跑创建新 Run | API route absence + immutable field test | S1/S2 |
| RG-C02 | Run cap 跨 Project 日账期累计；Request 保留受理时账期 | midnight/cross-period replay matrix | S2 |
| RG-C03 | expiry/close 只阻止新调用；已准入 Attempt 必须完成结算 | close/expiry 与 reservation barrier test | S2 |
| RG-C04 | `RunClosed` 与 reservation 在同一 Project lock 序列化 | `TestS0RunCloseSerializesWithAdmission` + 正式 barrier test | S0 已证/S2 |
| RG-C05 | `WorkUnitClosed` 与 `RunCreated` 序列化；已有 Run 不被强制关闭 | barrier test | S1 |
| RG-C06 | budget rejection 不写永久 exhausted 状态；便宜请求可在剩余额度内继续 | mixed reservation size test | S2 |
| RG-C07 | close 是幂等操作；重放不得生成第二事件或改变第一次结果 | Idempotency-Key/replay/restart test | S1 |
| RG-C08 | Work Unit 只有 closed 且所有 Runs 无 pending/inflight 才 matured | full state matrix | S3 |
| RG-C09 | Halro 只拒绝后续模型调用，不声称杀死 Agent 或撤销工具副作用 | API error and documentation assertions | S4 |

## D. Outcome 声明和修订

| ID | 冻结契约 | 可执行证明 | 阶段 |
| --- | --- | --- | --- |
| RG-D01 | Outcome 是鉴权调用方声明，不是 Halro 判决；保存 reporter key 和两种时间 | event round-trip + UI wording snapshot | S3/S4 |
| RG-D02 | Outcome 不得修改 Attempt、预算余额、reservation 或 Provider 调用事实 | before/after accounting snapshot equality | S3 |
| RG-D03 | Definition 使用后不可修改；变化创建新 version；disable 不阻断已声明 Work Unit | definition lifecycle matrix | S3 |
| RG-D04 | 首版只接受 BOOLEAN 和有界 CATEGORICAL；拒绝自由文本和任意 float | domain/API boundary tests | S3 |
| RG-D05 | 首次 Outcome 无 supersedes；修订必须引用 current head；旧 head 返回 409 | concurrent revision barrier test | S3 |
| RG-D06 | 同幂等 Key + 同 fingerprint 返回原对象；不同 fingerprint 冲突；重启后相同 | journal rebuild + handler test | S3 |
| RG-D07 | 修订顺序只由 Governance sequence 决定，`observed_at` 不决定 head | out-of-order clock test | S3 |
| RG-D08 | open Work Unit 的 Outcome 是 provisional；matured 后才进入成功率/单位成本 | cohort state matrix | S3 |
| RG-D09 | 每个 Work Unit/Definition 最多 20 个修订；Work Unit close 后最多写 30 天，超过后只读历史 | revisions 1..21、close+30d±1 boundary test | S3 |
| RG-D10 | `evidence_ref` 最多 128 字符，拒绝 URL、控制字符、换行和疑似 Secret；Halro 不抓取 | validator table/fuzz + zero outbound request assertion | S3 |
| RG-D11 | 不持久化 comment、reasoning、原始业务结果、Prompt 或 Response | JSON unknown-field rejection + secret canary scan | S3 |

## E. HTTP 和资源边界

| ID | 冻结契约 | 可执行证明 | 阶段 |
| --- | --- | --- | --- |
| RG-E01 | 所有 POST 强制 `Idempotency-Key`，作用域为 Project + operation + key hash | same/different Project/key/operation matrix | S1/S3 |
| RG-E02 | JSON body hard max 16 KiB，Content-Type 必须正确，未知/重复字段拒绝 | 16KiB±1、content-type、duplicate/unknown field tests | S1 |
| RG-E03 | 响应必须 `Cache-Control: no-store` 与 `X-Request-ID` | middleware contract | S1 |
| RG-E04 | 首次创建 201；完全相同重放 200 且对象 ID 相同 | endpoint test | S1/S3 |
| RG-E05 | active Runs/open Work Units 每 Project 各 hard max 1,000 | 999/1000/1001 与并发创建 | S1 |
| RG-E06 | 默认 TTL 24h，最大 30d；TTL 只用于派生状态 | boundary + fake clock | S1/S2 |
| RG-E07 | 每 Work Unit 最多 8 Definitions、32 Runs；每 Project 64 active Definitions | 每个上限 ±1 和并发创建 | S1/S3 |
| RG-E08 | 写入限流 Key 120 RPM、Project 1,000 RPM；读取 600/5,000；Summary 60/Project | fake clock/token bucket tests；配置不可高于 hard max | S1/S3 |
| RG-E09 | 列表必须 cursor 分页且最多 200 items；不得返回无界 payload | 201 items、cursor 稳定性和响应大小 test | S1/S3 |

## F. 日志、checkpoint、恢复和兼容

| ID | 冻结契约 | 可执行证明 | 阶段 |
| --- | --- | --- | --- |
| RG-F01 | Work Unit/Run 生命周期和归属进入 Accounting Ledger；Outcome 只进入独立 Governance Journal | event-kind destination test | S1/S3 |
| RG-F02 | Governance 使用独立 magic/version、chain-key domain、sequence、writer 和 apply state；换 key 开新 generation 并链接上代 terminal digest | open/replay/key-domain/generation rotation test | S3 |
| RG-F03 | Governance 损坏、拥塞或 apply failure 不得影响无 Run 推理和 Accounting append | `TestS0GovernanceFailureDoesNotPoisonAccountingLog` + production fault injection | S0 已证/S3 |
| RG-F04 | 两日志均拒绝截断、篡改和 sequence/revision 断链 | `TestS0GovernanceJournalRejectsTampering` + corruption matrix | S0 已证/S3 |
| RG-F05 | bbolt 中的幂等索引、head 和 checkpoint 均可删除重建，不是权威 | drop-bucket/restart/full-replay equality | S1/S3 |
| RG-F06 | checkpoint version 不匹配时整体丢弃派生版本；watermark 不得领先对应日志 | old/ahead/missing segment tests | S1/S3 |
| RG-F07 | Governance rollup 与 head 在同一 bbolt transaction 前进；一次 tick 只重写 head + open tail | kill-point + bounded-write test | S3 |
| RG-F08 | 增量投影与双日志全量重放逐字段相同 | randomized event stream differential test | S3 |
| RG-F09 | 新 reader 单向升级；旧 reader 遇新 Ledger epoch/Governance format 必须拒绝 | reader gate fixtures | S1/S3 |
| RG-F10 | 迁移或首次新格式写入后只能从升级前备份回退 | upgrade/rollback integration test + runbook | S4 |
| RG-F11 | Restore 在 staging 中验证全部文件、双 chain、双 replay 和 active resources 后原子切换；任一步失败不得部分启用 | kill-point restore matrix | S3/S4 |
| RG-F12 | 第一版不得按 Run/Work Unit/Outcome 物理删除权威事件 | API absence + retention test | S3 |

## G. 查询、报表和导出

| ID | 冻结契约 | 可执行证明 | 阶段 |
| --- | --- | --- | --- |
| RG-G01 | cohort 以 Work Unit 创建时间选择，汇总其全部 Runs 的 settled Attempt cost | fixed fixture hand calculation | S3 |
| RG-G02 | 缺 Outcome、未知成本、无 Definition 必须显式 partial/unknown，不得显示为零或失败 | complete state matrix + API/UI snapshot | S3/S4 |
| RG-G03 | Outcome 晚到/修订可以重述旧 cohort；返回 `generated_at` 和双 watermarks | before/after query test | S3 |
| RG-G04 | 跨日志只按捕获的一对 watermarks 连接，不声称全局原子顺序 | concurrent append/query barrier test | S3 |
| RG-G05 | `CostMicrosUSD` 已包含 estimated 子集，二者不得重复相加；除法用 checked integer arithmetic 和固定舍入 | overflow/rounding/golden test | S3 |
| RG-G06 | Summary 最多 90 天/100,000 Work Units，服务端 2 秒；不扫描完整 Ledger，超限要求 export | 100k fixture benchmark + scan counter + timeout/limit test | S0 已量/S3 |
| RG-G07 | Usage Attempt 成本只导出一份；governance 数据集只能引用其 ID | reconciliation duplicate-cost test | S3 |
| RG-G08 | Governance export 是四个规范化 NDJSON 数据集；manifest 记录 schema/format/checksum/range/count 和双 watermarks | round-trip/tamper/missing/duplicate test | S3 |
| RG-G09 | 旧 export partition 不改写；mixed formats 逐文件验证 | ADR 0017 mixed-manifest test 扩展 | S3 |
| RG-G10 | Prometheus label 不得包含 Project/Run/WorkUnit/Key/Definition/evidence_ref | metric descriptor allowlist test | S2/S3 |

## H. 默认路径和发布边界

| ID | 冻结契约 | 可执行证明 | 阶段 |
| --- | --- | --- | --- |
| RG-H01 | 默认 `run_governance_enabled=false`；无 Run 请求不读新增状态、不新增持久写 | 当前 HEAD baseline + instrumented no-Run test | S0 已录/S1/S2 |
| RG-H02 | Outcome 子系统 not-ready 不进入普通 inference readiness | readiness matrix | S3 |
| RG-H03 | 首版 Outcome 不自动改变预算、路由、模型或调用策略 | dependency/route audit + absence test | S3 |
| RG-H04 | 桌面与窄屏都能显示 partial、来源、双 watermark 与下钻链 | browser acceptance matrix | S4 |
| RG-H05 | 未经真实 Provider、设备和生产数据副本验证的结果必须标为未验收 | release checklist | S4 |

## S0 完成判定

- 双预算 prototype 的普通、64-worker 并发和 race 测试通过；
- 无 Run 的当前生产 HEAD benchmark 已保存，后续实现有同机对照；
- 两日志故障隔离和治理日志篡改检测通过；
- 10k/100k/1m 恢复结果已保存，并明确只是紧凑下界；
- active/revision/TTL/body/RPM/page 上限、checkpoint 和 export 形状已冻结；
- ADR、threat model 和 data-flow 与本清单相互引用。
