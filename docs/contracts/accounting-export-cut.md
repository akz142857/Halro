# 固定账务前缀的 Governance 导出源

2026-10-03；I-04/I-10 的磁盘读取候选契约。接口位于 `internal/ledger/governance_export.go`，Budget Manager 通过 `WithGovernanceExport` 持当前 owner 的 operation ticket。**公共 Admin handler 已接入所选磁盘 owner；默认 Runtime/Replica 仍保留完整 State，使用明确的 Ledger 快照过渡路径。**

## 固定前缀与最小输出

一次 capture 在同一短事务读取 Ledger watermark、hash、projection generation 和 WorkUnit/Run 创建计数。回调在事务及 projection 锁外执行；普通同 generation 追加允许继续读取，generation/同序号位置或 hash 改变、位置退后、关闭、损坏及取消使来源失败，不返回部分页。

WorkUnit/Run 的创建与关闭 sequence 随领域行、索引、计数及处理位置同事务提交。幂等别名或重复事件不产生新的对象/关闭位置，也不重复计数。WorkUnit maturity 使用该 cut 内既有 cohort version；cut 后创建不可见，cut 后关闭恢复原 open/active 状态。已 finalized 但仍有 pending attempt 的 WorkUnit 保持 in-flight，迟后 settlement 不改变旧 cut。

输出仅包含既有 schema-1 导出字段；Run 的 budget 是不可变创建值，不带当前 committed/reserved，WorkUnit 不携带全历史 attempt/cost。Watermark 是独立 Ledger 前缀，不声称与 Governance 或 Metadata 跨 journal 原子一致。

## 有限读取与重试

- WorkUnit/Run 页限制为 1～200 个物理索引行、1～8MiB 原始来源；future-created 过滤项仍消耗扫描/字节额度。position 最多 256B，领域行/cohort version 最多各 3MiB，在解码前检查。输出及临时解码对象属于调用方的有限页；页返回后事务释放，不能跨文件 IO 持有 reader。
- `After` 必须等于该 view 上次返回的 `NextID`；不能跳过或重读前缀。空页可有 continuation。达到 capture 时的创建计数后结束，不追赶新创建后缀；即使空列表，也须实际读取一页以验证 owner。
- maturity 每批最多 200 refs、1MiB 身份和 8MiB 原始来源；同事务有限去重，保留输入顺序及重复次数。不存在或跨项目返回 `Found=false`，源错误不伪装为不存在。
- 合法 maturity 批次遇到来源字节超限返回零页，保存最多该批 200 个唯一 refs/1MiB 身份。字符串复制避免持有调用方 substring 的大 backing allocation。只允许剩余集合的子集在同 cut 重试，成功子集解除义务；未完成整个集合不能 Complete，期间不能改查无关引用。参数本身的身份/数量超限锁存失败，调用方须在提交查询前分割到合法上限。

上述上限是逻辑来源/对象界限，不能换算为物理 RAM 费用。map/解码/堆复制、mmap、并发总量和 Linux RSS/cgroup 尚未接受；普通查询和导出仍需共享预算/reader 数量/时长门禁。

## 封闭与发布顺序

调用方应按以下顺序执行：

1. 在 `WithGovernanceExport` 回调内读取有限 WorkUnit/Run 页和所需 maturity 批次，完成其它来源及事务外文件输出。
2. 确认所有 join/来源结果完整，在发布 manifest **之前**调用 `Complete(ctx)`。它重新检查当前 owner/固定 cut、原 operation context 与当前调用 context、两类列表 EOF 及待重试 maturity 集合。
3. Complete 成功后在同一回调内发布文件 manifest。Budget operation ticket 直到回调返回才释放；Retire 即使在 Complete 之后也等待外部发布退出。

原 operation context 的取消不能通过子调用 `context.Background()` 绕过；不可恢复源/取消错误锁存，不能忽略后完成。Complete 封闭 view 并清 projection/context/pending 引用，回调退出也执行清理并 join 已进入的短读取。后续读/重复 Complete 返回 closed；成功封闭状态不被重复调用改写。回调返回 nil 而未成功 Complete，外层返回 incomplete。

Complete 是发布前许可。成功封闭后不在 manifest 已发布时追加一次新水位检查，将后续合法变化或取消追溯为导出失败。`WriteExportStream.BeforePublish` 和 [联合来源](governance-export-owners.md) 已集成这一顺序；公共 handler 和真实全局 Metadata 固定来源已接入。

State 缺少 immutable as-of 生命周期位置，因此此 API 没有 State fallback；旧 State Manager 返回 backend error，retired Manager 返回 accounting-retired，均不调用来源回调。公共 handler 仅对 backend error 调用 `WithLegacyGovernanceSnapshot`，借用当前 Budget State owner 与 ticket 至发布退出；快照和 WorkUnit map 尚未有界。其它错误不回退，Runtime 旧 alias 不参与导出。默认磁盘 owner 选择/完整 generation 发布仍待实施。

## 信任与验收边界

创建计数与物理索引是独立一致性 witness，可检测缺失前缀、缺 position/领域行、已扫描段计数异常等。达到计数后不扫描额外后缀，不能认证其中伪造的旧 cut 索引项；协调改写 counter/index/内容或缺失的不可变字段也未获认证。水位/hash/generation 检查仅约束进程内已认证来源和 owner，不替代 D-03 的内容、键集合、absence 及 generation 发布协议。不存在 reopen/trust-existing-DB API；候选仍由完整 authenticated native WAL 新建重建。

本轮回归及已终止检查见 [accounting-export-cut evidence](../verification/performance/2026-10-03-accounting-export-cut/README.md)，后续 Governance 固定页、有限 join 与发布门禁见 [联合来源证据](../verification/performance/2026-10-03-governance-export-owners/README.md)，第三来源见 [Metadata 全局定义固定前缀](metadata-definition-export.md)，公共接线见 [handler evidence](../verification/performance/2026-10-03-governance-export-handler/README.md)。默认 Runtime owner、完整生命周期/共享资源费用、磁盘增长/备份/迁移/回滚、Linux/故障及生产接受仍未闭环。
