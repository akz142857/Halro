# Metadata 定义固定前缀导出

继续 I-10/I-04。本契约覆盖 Bolt Metadata 的全局 OutcomeDefinition 固定来源及联合导出，已接入公共 Admin handler 的磁盘与 State 过渡分支；D-03、共享费用和生产接受仍未完成。

## 定义前缀与原子写入

schema 41 新增 `outcome_definition_export` / `outcome_definition_export_meta`。正常 Put 在同一 bbolt 事务提交不可变定义版本、单调 ordinal 索引、计数和摘要链；两个桶与定义一样进入 authoritative Metadata journal，Replica 按确认前缀原子应用。索引写失败会回滚原定义和审计 intent。摘要链绑定上一摘要、ordinal、身份键和原行 SHA-256。

初始迁移后的版本按 ID/version 建立前缀，之后按实际提交顺序追加。有限导出按 ordinal 返回，保留全部项目、全部历史版本及没有 Outcome 引用的定义，不把 latest/enabled 列表当作完整历史。文件 schema 不要求定义行以 ID 排序。

新增索引及 head 操作同样占用 Metadata 整个事务的 8MiB journal payload；单定义 raw ceiling 不是最大可接受 Put 大小的保证。Put 与 version=0 的最新定义读取已改为有限源页核对，见下一节；Admin 的兼容全量 List 仍未分页。

## 写入核对与最新定义读取

Put 不再在一个写事务中扫描全历史并构建 `latestByID` map。它持有一份整调用
Store operation ticket 和固定的全局 definition 写入 mutex，按原生 ID/version
顺序有限读取源（64行/1MiB，保留单 raw8MiB+key128B），reader 退出后完整验证
领域规则、revision=version 及身份键。只保留当前 ID 组、目标最新定义和活跃
计数，跨页保留一个组，不为每个历史 ID 分配永久对象或 cache。

每页及完成核对同一 Metadata TxID，写入前核对下一 TxID 和捕获的导出 head。
后者检测已合并进同一个写事务的先行定义操作，不能只靠 TxID。来源变化返回
RevisionConflict；无关 Metadata 写入也会保守打断，尚未采用定义专属 revision。
随后短写事务保持项目 revision、版本/名称/项目身份、活跃64项上限、重新启用
上限及审计 intent 与定义/导出索引的原子提交。两个并发新建不能同时占用最后
一个活跃槽；Store Close 撤销后等待实际归约/写入退出。

Get(version=0) 复用同一有限核对；显式版本继续点读。核对保留页外损坏拒绝，
并加强原始身份及领域一致性检查。全源扫描、累计分配及源写竞争仍随历史增长，
没有可信索引快路径；其共享 RAM/物理费用、排队准入、Admin 全量 List、Linux
容量接受和 D-03 仍开放。证据见
[定义写入有限工作集](../verification/performance/2026-10-04-definition-write-bounded/README.md)。

## 认证恢复先于建立前缀

Open 时 schema 41 只保留空索引及持久 `pending_epoch`，不把尚未恢复的 DB 当作新基线。Attach 获得 HMAC key 后，先认证旧 native journal 并 reconcile 已 durable、未 apply 的尾部，再在独占、未发布的 Store 所有权下按有限事务建立完整定义前缀。旧尾部每个完整原生 frame 与 applied sequence 同事务提交；失败只回滚当前 frame，重开从已提交位置继续。

持久 pending 门禁覆盖清理和全部构建批次。源页最多64行/1MiB，退出 reader 后验证再写有限批次；保留既有单定义 raw≤8MiB、key≤128B 的宽行兼容。完成后以短事务核对完整前缀并提交 prepared witness，绑定旧 epoch/sequence/hash 及前缀 count/hash。随后发布新 epoch 文件，再同事务写 DB epoch/sequence 和清 pending。pending 源不能导出，Replica 拒绝未发布迁移的 seed。

- schema 提交、尾部恢复、前缀准备后的重启均继续持久 pending 流程，不依赖内存 `migrated` 标记。
- epoch 文件已发布、DB 位置未写的重启，须认证新 journal，并精确匹配新 header 的 PreviousEpoch/PreviousChainHead、零操作位置、旧 DB 位置和 prepared 前缀，才进入完整原始定义源的有限重建；重新计算的 count/hash 必须等于 prepared witness，随后才完成位置提交。不能只信任最后一项索引或跨进程保存的中间进度。
- phase 2 表示准备时存在并认证过旧 journal，其后丢失会拒绝。phase 3 单独保留首次初始化及既有 whole-file restore 不携带 journal 的契约；这种恢复以已发布投影为基线，不宣称恢复了缺失文件中的未知事实。
- 错误密钥、坏完整 native frame、错误前任链或位置不覆盖原 journal，不清 pending。

清理和建立索引采用有限事务，不保留整个历史的 dirty-page 集；中断后的已提交索引仍被 pending 门禁隐藏，重试从完整事实重新构建。每个 native frame 保持整体原子性；大 DeleteBucket 或旧 overflow value 回收的物理成本不能从8MiB编码限额推出，逐 frame fsync 也可能降低恢复吞吐。旧 schema 迁移、完整认证扫描、共享费用及空间预检仍开放。维护窗口及物理 RAM/IO 须由 O-01/D-04/Linux 接受完成，不能凭批次限额认定整个启动已内存有界。已有 DB 内容和键集合的可信性仍属 D-03；本 SHA 链仅提供捕获后读取的一致性 witness。

## 有限读取与完成

`Store.WithOutcomeDefinitionExport` 捕获同一短 View 中的 Metadata epoch/applied sequence、定义 count/hash，调用方在 callback 内使用 view：

- 每页最多 200 行、16MiB 原始源，计入 ordinal、索引和定义；单索引≤192B、单定义≤native 8MiB，原始长度先于 SHA/解码检查。16MiB 最大页容纳合法宽行与索引开销。
- 按连续 ordinal 点读至捕获 count，不追新增后缀，新增低 ID 或后续版本不混入旧前缀。每项验证原行 hash、摘要链、ID/version 身份键及合法领域字段；完整前缀终点须等于捕获摘要。
- Source bytes 超小返回限额错误且不推进，可增大限额重试。页内后行超预算返回已通过的有限页，下一页继续；只读取未纳入页的候选长度，不解码它。累计 IO 和 mmap/物理 RAM 不等同于该逻辑页限额。
- 原 operation context 和每次子 context 均检查，Background 不能绕过根取消；不可恢复错误锁存，不返回部分错误页，也不能忽略错误后 Complete。
- Complete 再验证位置不回退、epoch 不变及捕获前缀 witness，要求计数和摘要完整。成功封闭 view 并清 DB reader/context；重复 Complete、late read 拒绝，callback 退出 seal/join 已进入调用。
- DB handle 与 Store mutable field 分离，无跨页/编码事务。调用方必须完整 join Metadata 生命周期至 callback 和文件发布退出；该 view 不是 Runtime 通用退休协议。

## 三来源候选接线

`Manager.WriteExportWithDefinitionOwner` 按 **Gov → Ledger → Metadata** 捕获来源，确保被前两者引用的不可变定义已存在于后取的 Metadata 前缀；水位仍独立，不声称跨 journal 原子 cut。Metadata owner factory 同步调用一次 visitor，并持有/等待其整个 callback；callback 包含文件写出、Metadata→Gov→Ledger Complete 和正式 manifest 发布。旧 `WriteExportFromOwners` 保留显式 source 的兼容入口，其 caller 仍负责 source cut 和生命周期。

真实 App fixture 使用正常 Metadata 定义写入、完整 authenticated 重建的双 disk Ledger/Gov owner、实际 Metadata global view。包括十个 900KiB 原生 WorkUnit、60 个修订、跨两个项目的 17 个定义版本以及 1MiB+1 creator；读出期间追加更低 ID、新版本、Finalize/Report，输出保留三个来源的固定前缀。公共 handler 已在 Gov/Ledger 捕获之后取得实际 Metadata view，短 View 返回后再编码/文件 IO，不再调用全量 Definitions list。Runtime 导出许可在排空时撤销、Close 前 join，包含最终发布、审计与响应；该许可不提供 Metadata 通用退休或默认磁盘 generation 发布。

检查证据及尚未接受边界见 [Metadata 定义前缀验证](../verification/performance/2026-10-03-metadata-definition-export/README.md) 和 [公共 handler 验证](../verification/performance/2026-10-03-governance-export-handler/README.md)。

本轮有限事务、真实 SIGKILL、恢复位置与关闭 join 的证据见 [Metadata 恢复分块验证](../verification/performance/2026-10-04-metadata-recovery-blocks/README.md)。

Replica 的 `ApplyReplicaMetadataThrough` 同样按完整 frame 原子提交。应用前在同一 native append lock 下完整认证包括目标外 durable 尾部，之后才逐笔提交；相同位置重试也不能绕过坏尾部。失败返回已提交原生位置，确认目标外事务不应用；整个目标不再提供单个 DB 事务的原子性，成员 AppliedIndex 仍只在完整 ApplyConfirmed 成功后发布。验证见 [Replica Metadata 分块](../verification/performance/2026-10-04-replica-metadata-blocks/README.md)。
