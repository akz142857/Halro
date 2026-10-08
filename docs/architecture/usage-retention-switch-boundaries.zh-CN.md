# Usage 默认磁盘切换：保留与清理边界

2026-10-04 更新：普通 Standalone Open 已选择 fresh 磁盘 Usage，并在 readiness 前完成原生恢复追赶、归档核对及存储窗口重应用；周期任务已接完整参考、签名策略交接和实时窗口权限重新绑定。备份/恢复及 Doctor 的分页目录兼容已接线，定向验证和未完成条件见 [当前证据](../verification/performance/2026-10-04-usage-default-runtime/README.md)。下文是此前分阶段边界记录；其中“默认仍 Aggregate”“周期尚未接线”已被本段取代。HA、全历史 IO、共享费用、完整迁移/备份及 Linux/生产验收仍未完成，不宣称整体切换可发布。

2026-10-03 源码核对记录。本文记录必须验证的边界；有限窗口 step、清理版本与进程内归档权限入口已实现，日期清理及默认磁盘切换仍未完成。顶层进度仍以 [最终方案 §8](memory-bounded-state-design.zh-CN.md) 为准。

## 当前实现事实

- `internal/app/usage.go` 的磁盘 `pruneUsageWindow` 已接入同 owner/root 的真实归档权限，每 tick 有限推进；没有权限或出错时保留记录，HA 不借用 Standalone 权限。磁盘 `pruneUsageArchive` 仍直接返回。fresh启动辅助已接归档重新核对、已发布日期策略独立原生认证及有限窗口重应用，但普通 Open 尚未选择该辅助流程。默认 Usage 仍为 Aggregate；现在切换仍会让周期归档保留配置停止生效。已裁查询库不能作为完整日期回收参考，需接完整参考来源及下一策略交接。
- `Exporter.PruneVerifiedWindowStep` 使用完整原生对账或从零开始的真实 publisher 前缀生成的私有权限，不接收裸水位。连续有限发布仅在旧根权限匹配、全部新前缀实际发布及清理成功后延续；rename 后未确认根保留一份固定重试凭据，匹配身份/摘要并重复屏障后才能恢复。重开不继承，不替代日期 coverage 或可信重启。
- `internal/usage/retention.go` 按账本顺序分别裁掉 attempts 与 summaries 的连续前缀。遇到第一条未导出或时间未过期的记录便停止；两类首条存活记录共同决定 floor。它没有承诺删除前缀之后所有日期过期的记录。
- `internal/usage/parquet.go` 的 `Exporter.PruneBefore` 按 UTC 分区日期筛选所有 descriptor，先发布剩余 manifest，再删除文件。日期与账本序列不单调，因此归档裁剪可能形成序列空洞，不能套用内存窗口的单一 floor。
- `VerifyDiskArchiveAfter` 只证明调用方明确给出的 `(after, H]` 序列区间，默认从 genesis 开始；独立的 `VerifyDiskArchiveRetainedDates` 已增加显式 UTC 日期谓词，仍从零遍历完整原生归档前缀。所有应保留行必须存在，仍列出的过期行也必须与native精确匹配；缺失旧日期计入报告，不生成完整窗口权限。不能通过第一条剩余文件的序列推导 floor。
- `disk_query.go`、`disk_failures.go`、`disk_detail.go` 的续页游标固定 owner generation、查询和 H，当前已增加同一事务的清理 epoch，删除后拒绝旧续页。`DiskProjection.read` 在一次调用中保持读锁，因此排他的写操作不能穿插该调用；不同分页调用之间仍可能发生清理。HTTP token 的 MAC 只认证游标内容，并不证明游标引用的数据尚未删除。
- `disk_detail.go` 保留全部 request summary observations；`disk_summary.go` 使用跨短事务的归约。只清理 attempts/summaries 主表无法完成磁盘回收，也不能据此宣布全部历史索引有界。

## 窗口清理必须守住的契约

1. 清理依据来自同一已选 owner 与经过校验的归档发布位置，不能把目录存在、未知水位或空目录当作导出成功。未导出的记录保持可查询。
2. 分别推进 attempt、summary 前缀，保留第一条未过期记录及其后缀，包括迟到的旧日期记录；floor 取真实存活记录的最小序列。单批行数、读写字节和锁内工作有硬上限，实际提交后才报告进展。
3. 主记录、对应查询索引、清理位置和清理版本在同一事务变化。正常 Append 不使旧的固定 H 游标失效；删除它可能引用的记录必须让续页显式失效，或持有有限、可回收的 pin。禁止返回成功的短页／空页掩盖删除。
4. HTTP token、failure 的未完成关联、detail 的选定 summary version 和内部 archive source cursor 都必须纳入同一清理规则。长归约与响应所有权的实际退出边界也须覆盖，不能仅等待一次 DB View 结束就回收依赖。
5. 清理不回退 Usage 处理水位、不改变累计账务／daily rollup，也不删除 native Ledger、active request、started attempt 或恢复需要的首次归属证据。迟后 settlement 仍按原 finalized 事实结算，不生成第二份请求摘要。
6. request versions、质量／延迟索引、小时／Dashboard 数据及 rollup 历史版本分别定义保留语义和有限清理路径。哪些累计数据继续保留必须明确；不能一律跟随明细窗口删除。

## 日期归档保留必须守住的契约

日期范围与序列范围是两个维度。新校验接口必须由调用方明确提供允许丢弃的日期范围／策略证据，逐条核对完整 native 源中仍应保留的 attempts。根中的日期、缺失分区和首条剩余 sequence 均不能自行授予删除许可。保留范围内的漏行、重复行、字段不符仍失败；报告明确披露所校验的日期与序列范围。

分页目录重写需要有限持久进展、旧根见证、完整新根原子发布和失败重试。旧 cursor／验证／export 可能仍引用旧页与数据文件，因此新根发布与物理 unlink 是两个阶段；回收须证明实际读者已退出，且不能让取消提前释放 pin 或让旧 owner 删除新 owner 的文件。格式升级必须让旧 reader 显式拒绝，禁止旧程序把保留后的根当作空历史覆盖。

归档读者现已接入独立 lifetime 与四个零队列读许可；目录、整 checksum job 和旧版 Verify/Reconcile 在实际文件关闭后才退许可。私有回收门禁同时取得写入 `mu` 和排他的读者 lifetime；续页版本 1 绑定 exporter key、epoch、旧 page ref 与 H，append 保留旧 H，清理版本推进后拒绝旧 token（包括空/EOF token）。Runtime 和 offline 关闭实际 join 读者后才释放依赖及目录锁，契约和证据见 [归档生命周期](../contracts/usage-archive-lifetime.md)。返回的 descriptor 不保护调用返回后另行打开的文件；门禁也不提供日期/native 删除权限。有限根重写与物理回收仍要保护新根重新引用的路径，不能仅按旧根删除列表 unlink；完整持久化进展/重试协议尚未实现。

当前 `VerifyDiskArchiveAfter` 不提供这些日期语义；新增[日期校验接口与离线命令](../contracts/usage-archive-dates.md) 已提供明确策略的完整原生对账。[有限候选与 schema 10 消费者](../contracts/usage-date-catalog-candidate.md) 后续已实现有限保存/重开前缀重算及明确覆盖字段；候选 Ready 不授权激活/删除，完整发布/物理回收协议仍未实现，相关清理继续拒绝。本文没有选定新的持久认证格式，也没有替代 D-03。

2026-10-03 核对发现 `validateCatalogEntry` 不核对 descriptor 的 `Date`，旧 `PruneBefore` 仅按它删除。后续已在精确 verifier 增加 canonical 日期／路径及每行 UTC 完成日期绑定，有限 publisher 也在创建分片前拒绝混合日期和无效日期；未放宽旧 schema 的字段比较。新回归保留真实行及文件摘要，只伪造日期／目录描述，证明完整和日期两类校验均拒绝。它修复了日期权限的前置条件，尚未使旧 `PruneBefore` 成为新的有限清理协议。

## 必须先通过的回归向量

| 情景 | 预期结果 |
| --- | --- |
| attempt 序列 10 为旧日期、20 为新日期、30 为迟到旧日期，全部已导出 | 窗口前缀只删除 10；归档日期策略可排除 10、30，但必须继续证明 20 存在 |
| attempt 首条存活为 20，summary 清理到 40 | floor 不得高于 20；重启后 20 保持可查询 |
| 日期过期但 sequence 高于已发布导出 H | 窗口不得删除；下次导出仍可取得它 |
| 首次分页后清理，续页没有剩余候选 | 旧游标明确失效或其 pin 保住完整旧范围，不得成功返回 Done 掩盖删除 |
| failure 在跨页关联中、detail 已选 immutable summary version | 清理不能让它们改用另一条关联／版本，或将缺失解释为未找到 |
| 日期策略允许排除旧行，但保留日期内删掉文件或目录 descriptor | 独立 native 对账失败；不能提升 sequence floor 规避 |
| 发布新根后 unlink 失败／取消／进程退出 | 已发布覆盖范围不回退，未清完文件保留可重试义务；报告不冒充完整物理回收 |
| 清理后追加迟后 settlement、重复执行清理、重新构建 owner | 累计汇总不重复、不缩小，首次 finalized 摘要保持，恢复职责不丢失 |

[窗口 step 实现](../contracts/usage-disk-window.md) 已覆盖两类前缀、epoch、有限版本回收、真实取消 join 和迟后 settlement；[当前验证](../verification/performance/2026-10-03-usage-disk-window/README.md) 记录源码 cut 与门禁。日期归档的周期控制/实时窗口权限重新绑定、普通 Open 默认选择和其余回收向量仍待实现；启动辅助及已发布日期根的窗口权限、fresh重建后窗口重应用已有本地证据。逻辑删除、物理空间回收、Linux 容量和生产接受分别验收，不能互相替代。

下一阶段的根格式、四阶段持久进展、重开信任和当前引用排除条件已按源码核对写入 [日期回收实现契约](usage-date-reclamation-protocol.zh-CN.md)；该协议仍未完整接线，不据该记录开启默认切换。

后续 [有限候选行集对账](../contracts/usage-date-catalog-verification.md) 已将候选比较拆成有限归档文件索引与完整 `(0,H]` 来源页，使用私有固定节点磁盘 radix 索引和 checkpoint 前缀重算。它不从保存的 Ready、候选页 SHA 或空目录获得删除权限；仍须独立认证来源并通过 native/member/Metadata 最终复核后才能进入激活。原生参考构造的整 job deadline、超限旧分片重分片、根原子激活/旧根义务/当前引用安全 unlink、默认 Runtime 和生产容量继续未完成。

[周期参考与策略交接内核](../contracts/usage-periodic-reference-and-succession.md) 已实现并有定向 race/vet 证据；完整历史参考的权限不自动授予实时查询 owner。当前进度选择、派生 hint 交接、权限重新绑定和定时接线完成前，仍不能开启默认切换。
