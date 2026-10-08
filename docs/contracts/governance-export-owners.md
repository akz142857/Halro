# 固定事实前缀、账务 join 与导出发布

2026-10-03；I-04/I-10 候选。`Manager.WithOutcomeExport` 与 `Manager.WriteExportFromOwners` 位于 Governance package，`WriteExportStream.BeforePublish` 提供文件发布前门禁。**公共 Admin handler 已接入所选 owner：磁盘账务走三来源有限导出；默认 State 仅保留 Ledger 全量快照兼容路径，Gov/Metadata 均使用固定有限来源。默认 Runtime 磁盘 generation 尚未发布。**

## Outcome 来源

capture 在 Manager 锁内取得已完整应用的 native Summary 和 ephemeral fence，并绑定该 Manager/Log/State或Projection 身份。native 每条记录产生一个唯一 Outcome，故 captured `Records` 是完整行数 witness。不可变事实支持 State 的过渡有限候选读取，也支持 fresh authenticated WAL 重建的磁盘 owner；没有新增全量 snapshot/slice。

`ReadPage(ctx, rows, bytes)` 内部维护连续游标，不让调用者跳过旧行；最多 200 个输出项、1MiB 行 JSON。磁盘页最多扫描 200 physical rows/1MiB raw Record，单 raw Record≤192KiB，均解码前检查。State 保留最多 Rows+1 候选，**仍扫描完整历史并保留既有历史驻留**。cut 后事实过滤，可返回 empty/Done=false；future prefix 仍增加总 CPU/IO，但各磁盘页有限并检查 context。达到 captured count 后 Done，避免无休止追赶新增后缀。

一页结束即释放 Manager 锁及 DB reader；visitor/编码/文件 IO 不跨事务。native 与 backend head 不一致、owner 身份变化、fence/key 失效、位置缩小/同位置 hash 改变、源损坏或取消锁存失败，返回零页；字节不足可在同位置重试，不推进 count/cursor。创建 context 不能被子调用 Background 绕过。即使空账本也须读取一页，之后才能 Complete。

Complete 再次核对 owner/fence/context、全部事实已读取且 count 相等，然后 seal，清 Manager/Log/State/Projection/context 引用。自动退出同样清理并串行等待已经进入的读取；后续方法返回 closed，重复 Complete 不修改成功状态。未完成却 callback 返回 nil，外层返回 incomplete。caller 必须持有 Governance 资源直到整个 callback/发布结束；[Manager Retire](governance-manager-lifetime.md) 已通过 ticket join 整个回调，仍不关闭共享资源或发布业务 generation。

计数是进程内一致性 witness，不授予 D-03 的内容/key-set/absence 信任；达到计数后未扫描伪造的旧 cut 后缀、协调篡改仍未认证。ephemeral fence 不是跨重启 DB 信任。锁/文件 IO 不可强制中断，context 检查不构成完整时长或共享读者准入验收。

## 联合来源与成熟度

`WriteExportFromOwners` 从 Report 使用的同一 accounting dependency 取得 `AccountingExportReader`；State 不支持 immutable lifecycle cut 时明确失败，无旧 State fallback。先 capture Gov facts，再 capture Ledger，让已存在的事实引用可在后续账务 cut 找到。两个水位独立，不声称跨 journal 原子视图。

WorkUnit/Run 使用[固定账务来源](accounting-export-cut.md)的最小有限页，按现有 schema 投影，不重复 committed/reserved。每个 Outcome 页按完整 ProjectID/WorkUnitID 建最多 200 项的有限去重 map/引用；在首次 maturity 查询前去重，才能将合法 source-byte overflow 分成不相交的待重试子集。observations 预分配，拆分深度最多 8，不用递归 append 或全局 WU join map。

每个 maturity 子批仍固定同一 Ledger watermark，完整核对返回数量、原 refs、Found 和有效 status。缺失、跨项目、不一致或源错误不能变成 false provisional；开放或 in-flight 为 provisional，其余关闭结果为成熟。合法 finalized-pending/late settlement 的成熟度取原 cut。8MiB 是单次读取原始源限额；失败父批会被子批重新读取，单个 Outcome 页累计 IO/解码可超过 8MiB，须另作费用/性能校准。

Metadata 的真实全局有限来源见 [定义固定前缀契约](metadata-definition-export.md)。新 `WriteExportWithDefinitionOwner` 在 Gov/Ledger cut 后捕获 Metadata，并将其 callback 保持至发布退出。旧 `ExportDefinitionSource{Read,Complete}` 仍兼容，缺任一函数拒绝；显式注入不替调用者证明完整性/固定 cut/事务释放，也不能自行保证捕获顺序。

## 发布与生命周期

所有 producers 返回、sink seal/join、四 datasets flush/Sync/rename 和 directory Sync、临时 manifest Sync 完成后，正式 manifest rename 前只调用一次 BeforePublish：Metadata Complete→Gov Complete→Ledger Complete，全部成功才能发布。任一读取/join/许可/取消失败均返回零 manifest，清除仅本调用 owned directory。hook 返回后发布前再检查 context，因而成功 Complete 后立即取消也不能获得未开始的发布；进入正式发布后的 durability barriers 沿用[文件层契约](governance-export-stream.md)。

Ledger Budget operation ticket 覆盖整个联合回调和外部文件发布；即使在最后门禁中 Retire 超时，新调用拒绝，旧 export 可以完成已接受读取和发布，回调退出才解除 owner。Governance Manager ticket 也覆盖外层回调与发布，Retire 永久撤销并 join 后清自己的 owner 引用；Metadata 资源保留仍由 caller 保证。公共 handler 的 Runtime export lease 另覆盖文件发布、成功审计及响应返回；这些 join 不替代 Runtime generation 发布。

三角色复核和真实 native 双 owner、宽字段/拆批、追加事实及失败门禁证据见 [governance-export-owners evidence](../verification/performance/2026-10-03-governance-export-owners/README.md)，公共接线与资源 join 见 [handler evidence](../verification/performance/2026-10-03-governance-export-handler/README.md)。shared physical RAM/磁盘/并发费用、Metadata 通用生命周期、默认选择/generation/可信恢复、Linux/RSS/cgroup、完整故障和生产接受仍未闭环。

## Metadata 来源后续

已实现 [Metadata 全局定义固定前缀](metadata-definition-export.md) 及 `WriteExportWithDefinitionOwner`：Gov→Ledger→Metadata capture，Metadata callback 保持到三来源 Complete 和正式 manifest 发布；实际双 disk 与 Metadata global source fixture 保留全部项目、历史版本及晚插入排除。公共 handler 已使用此 factory；真实磁盘端点组合验证不等于默认 Runtime generation 已发布。

## 公共 handler 与 State 过渡路径

Admin 路由继续执行管理员权限、同源及 CSRF 检查。每个 Runtime 同时只接受一个导出，无内部等待队列；busy/retired 返回 503。业务排空在 HTTP shutdown 前永久撤销新导出，失败或取消不重新打开入口；重试等待同一完成信号。成功排空先 join 导出，再停止后台、追赶 Usage 和退休账务；直接 Close 也先 join，再关闭 native/store/audit/vault。

仅 `ledger.ErrGovernanceExportBackend` 表示所选 State 缺少固定账务来源，进入明确兼容路径。它通过 `WithLegacyGovernanceSnapshot` 借用当前 Budget owner 的 State 和 operation ticket，按 Gov→Ledger snapshot→Metadata 捕获；不读取 `Runtime.state`。Gov/Metadata 连续有限页和发布前 Complete 保持，Ledger snapshot 与 WorkUnit join map 仍随历史增长。磁盘关闭、退休、损坏及 Gov/Metadata 错误失败封闭，不回退旧 State。

Runtime lease 覆盖文件、审计和 HTTP 响应返回；阻塞响应可延迟 Close 的真实 join。此限制只证明导出操作数量/依赖生命周期，不证明所有 HTTP goroutine、物理 RAM、磁盘费用或 IO 时长有界。
