# ADR 0026: Business outcome evidence and cohort reporting

- Status: Accepted for S3 implementation
- Date: 2026-09-04
- Builds on: [ADR 0017](0017-usage-export-format.md)
- Evidence: [Run Governance S0](../verification/performance/2026-09-04-run-governance-s0/README.md)

## Context

“一次模型调用成功”只说明 Provider Attempt 的技术状态，不能说明工单解决、代码验收或
业务流程成功。业务系统需要把外部验收结果和模型成本连接起来，但 Halro 不执行评价器，
也不能把外部声明包装成自己的事实。

Outcome 通常迟到、可修订、可能突发写入。若它与 Accounting Ledger 共用 writer/apply
失败状态，治理日志的一条坏记录会停止普通推理；若只放 bbolt，又失去追加历史、完整性
验证和可重建幂等语义。

## Decision

Outcome Event 写入独立 authenticated Governance Journal。它有独立 magic/version、
chain-key derivation domain、sequence、watermark、writer、apply state 和 checkpoint。
Accounting Ledger 仍是费用与 Run/Work Unit 归属唯一权威；Governance Journal 是外部
结果声明和修订历史唯一权威。

Outcome Definition 是管理员创建的不可变版本。第一版只支持 BOOLEAN 和有界
CATEGORICAL。事件保存鉴权得到的 reporter key、value、observed/ingested time、可选
SHA-256 和受限 `evidence_ref`。它不保存证据正文、自由文本 comment/reasoning、Prompt
或 Response，也不访问引用地址。

修订是 append-only。第一次 revision 为 1；后续事件必须引用 current head，否则返回
409。Governance sequence 决定 head，调用方时钟不决定顺序。每个 Work Unit/Definition
最多 20 revisions。

基础报表固定使用 `work_unit_cohort`：按 Work Unit 创建时间选 cohort，读取每个
Definition 的 current Outcome，再汇总这些 Work Units 下全部 Runs 的 settled Attempt
cost。只有 closed 且没有 pending/inflight Run 的 Work Unit 才 matured；此前 Outcome
显示 provisional。缺失 Outcome、未知成本或无 Definition 都是显式 partial/unknown。

查询捕获并返回 accounting/governance 两个 watermarks。两日志不共享全局 sequence，
系统不声称全局原子快照。Governance not-ready 只影响 Outcome 写入和业务报表，不进入
普通 inference readiness。

Governance checkpoint 使用小 head 加不可变 4 MiB segments，current-head、幂等索引和
低基数 rollup 可从 Journal 重建。它不与 Accounting checkpoint 共用 transaction。

Export 采用四个规范化 NDJSON 数据集：Work Units、Runs、Outcomes、Outcome Definitions。
Usage Attempt 数据集只增加 nullable `run_id`/`work_unit_id`。统一 manifest 对每个文件
记录 dataset、schema、format、checksum、sequence range 和 count，并记录双 watermarks。
Attempt cost 不复制到治理数据集。

## Why

S0 的固定帧下界 probe 验证 100 万条事件（90 万 current heads）可完整认证重放，参考机
用时约 0.68 秒、重放后 heap delta 约 65 MiB。单字节篡改会拒绝重放；同一测试中损坏
Governance Journal 后，独立 Accounting Ledger 仍能追加。数据支持故障域拆分和重建
索引方向，但不是最终生产 SLA。

现有 Usage checkpoint 已是 4 MiB 不可变 segments，实测约 1,120 bytes/attempt；把
Outcome head 混入高基数 Attempt segment 会耦合两个保留窗口和故障域。独立分段保留
相同的 O(delta) checkpoint 性质。

NDJSON 与 ADR 0017 已有格式、原子发布和 mixed-manifest 语义一致。多数据集保留实体
关系和 schema 演进，避免统一宽行中的大量 nullable 字段，也便于外部 FinOps/BI 连接。

## Rejected alternatives

### Outcome 进入 Accounting Ledger

拒绝。业务写入洪峰、坏 revision 或 apply failure 会毒化预算状态机和普通推理。

### Outcome 只存 bbolt current row

拒绝。历史修订、调用方归属和幂等重建无法独立验证，索引会变成第二权威。

### Halro 主动抓取 evidence_ref 或运行 evaluator

拒绝。它扩大 SSRF、Secret、PII 与执行面，也把 Halro 从小型网关内核变成 Agent 工作流。

### Outcome 自动改变预算或路由

拒绝。迟到和外部声明质量无法满足实时 fail-closed 决策；后续需独立离线评估和授权。

### 一个统一 governance NDJSON 流

拒绝。Definition、Work Unit、Run 和 Outcome 生命周期不同；统一流削弱 schema、主键和
完整性校验，外部使用者仍需重新拆表。

## Consequences

- 备份 manifest 和 staging restore 必须覆盖双 journal、双 checkpoint 与双 watermarks；
- Summary 必须标注 generated time、basis、coverage、unknown/estimated 和两份水位；
- 内置指标只做低基数 cohort；Project/Run/WorkUnit/Key/Definition ID 不作 metrics label；
- 第一版不提供权威事件逐条物理删除，关闭资源也不表示删除；
- 外部系统负责证据正文、Evaluator、工具成本、收入和全 TCO/ROI。

## Required verification

- Journal truncated/tampered/wrong-key/revision-gap/reused-id 全部 fail closed；
- 同 head 并发修订只有一个成功，幂等索引删除后 replay 仍返回原对象；
- Outcome append/apply/checkpoint 故障时无 Run 推理和 Accounting append 可继续；
- 增量 projector 与全量 replay 逐字段相等；
- 迟到、修订、missing、unknown 和 provisional/matured cohort golden tests；
- 四数据集 NDJSON round-trip、checksum/range/count、缺失/重复和双 watermark restore；
- 最大内置查询不扫描完整 Ledger，所有列表最多 200 items/page。
