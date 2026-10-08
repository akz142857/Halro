# Usage 日期归档回收：实现契约与源码缺口

> 2026-10-03 原始prune baseline授权P1已复现并本地修复：新增[独立策略激活凭据](../contracts/usage-date-policy-authority.md)，绑定精确旧/新根及原始native物理位置/链hash，重开校验并拒绝未签名旧义务。见[原始复现](../verification/performance/2026-10-03-usage-date-data-reclaim/README.md)与[修复门禁](../verification/performance/2026-10-03-usage-date-policy-authority/README.md)。后续义务完成必须保留仍有效的策略凭据，CLI/默认Runtime和生产接受仍未完成。

2026-10-03。本文依据源码明确实现与验收条件。[有限候选构建与 schema 10 消费者](../contracts/usage-date-catalog-candidate.md)、[相对于 fresh 投影的有限候选行集对账](../contracts/usage-date-catalog-verification.md) 已实现；后续 [Standalone 逻辑激活与屏障恢复](../contracts/usage-date-catalog-activation.md) 已加入独立 App 来源/Metadata 最终复核、一次性比较凭据和固定旧根义务。完整物理回收、原生认证/构造续传、CLI/默认 Runtime/HA 激活仍未实现，不能作为默认磁盘切换、删除权限或 D-03 批准。顶层任务仍为 I-07/I-08 进行中。

## 根格式必须表达的事实

前置核对时的 schema 9 根只保存 `last_sequence`、`complete_prefix` 和分页目录引用。
当时的 `publishArchiveCatalogRootWithProof` 固定输出这个结构；有限 publisher 按原生序列追加全部新 attempts。后续 schema 10 现已加入显式日期/清理 baseline，publisher 保留它们，当前 reader/精确 verifier 也核对描述的范围；默认根没有自动升级，候选构建 API 不激活新根，独立 Standalone 激活 API 才可发布逻辑覆盖。
日期清理则可能删除序列中间的旧日期行。它必须保留原生处理 H，同时明确表达允许遗漏的日期范围。
不能继续把 `complete_prefix=true` 解释成所有历史行仍存在，也不能把 H 降到第一条剩余文件之前。

新格式必须使当前不理解日期覆盖的程序显式拒绝。仅给 schema 9 增加未知 JSON 字段不够：当前 root reader 会跳过未知字段，旧 publisher 还会把它们丢掉。
日期策略由调用方独立提供；根中的保留日期、空目录和已保存进度都不能自行授权删除。
追加、迁移、验证和窗口权限必须共同处理新格式，不能出现日期根被追加路径悄悄写回全历史语义的情况。

## 四阶段及发布边界

| 阶段 | 有限持久进展 | 成功条件 |
| --- | --- | --- |
| 构建 | 固定旧根身份/摘要/H、调用方日期策略、输入页引用与页内位置、输出 head/counters | 每步有页/行/字节/时长上限；完整旧根尚可读；只生成可核对的不可变候选页 |
| 对账 | 固定 native/角色允许前缀、候选根与精确行集比较进展 | 必须证明整个应保留集合存在、所有列出行精确匹配、日期/路径/行时间一致；不是只核 page hash 或 H |
| 发布 | 已认证候选的实际根字节/临时 inode、旧根见证、固定回收义务 | 实际 file/目录屏障和原子 rename；发布前失效旧游标；rename 后失败保留可重试义务，不能报告完整回收 |
| 回收 | 有限旧页/文件续读位置及一个待完成对象的所有权信息 | 实际读者退出、当前引用安全、身份/内容匹配、unlink 与目录同步；完成实际清理后才退账或报告完成 |

根发布和物理 unlink 是两个不同完成点。只能保留固定数量的活动回收义务，不能每次失败追加一份历史删除列表。
正常追加不应因旧文件尚未删完而丢失或停用永久维护；但不能因此让旧义务删除新根重新引用的路径。

## 进度不是恢复信任

content-addressed 页的 SHA 只证明 bytes 与引用相符，不证明其中的行属于原生事实或覆盖完整保留集合。
因此不能恢复一个保存的 output head/cursor 后直接发布根。重开必须重新建立 native/成员/Metadata 的独立权威，并验证候选内容与进度的对应关系。
原有精确 verifier 仍有整个 job 的固定上限。新的候选对账 step 已按调用推进归档文件索引与 `(0,H]` 原生投影页，保存有限 checkpoint；重开后重新建立私有磁盘比较索引、分步重算已存前缀并比较全部确定状态，不继承保存的 Ready 或索引。比较索引使用固定 160 B 节点与最多 256 层 radix 分支，不保留历史堆 map/freelist；独立 native 来源认证/构造仍是整 job deadline，超限单分片的兼容重分片与 HA 角色授权仍须交付，不能以局部比较代替这些条件。

Standalone 激活现在先持久化不超过 128 KiB 的旧/新根 intent，根 rename 前复核其实际位置与候选/来源，再失效旧游标并发布 schema 10，最后持久化 derived published phase。保存 phase 不能授权重试；重开须相对于新的独立原生参考有限对账当前根、证明候选 head 在当前页链中，重复实际 file/目录屏障，保留后续追加的根。激活本身保留物理文件，报告明确回收 pending；不同根对不能覆盖未完成的唯一旧义务。

[周期参考与策略交接内核](../contracts/usage-periodic-reference-and-succession.md) 已实现固定双槽交接：下一 intent 的 MAC 认证绑定上一 active 完成记录的实际 bytes digest；新根发布期间保留旧 active。新的实际 data job 成功见证、旧 signed 页进度与上一完成记录一致时，才可替换页进度；新原始页链完成后才替换 active，不递归保存历史策略。裁剪后的实时查询库另用独立原生全历史双磁盘参考完成日期对账/回收，关闭取消并真实等待回调与清理退出，再清钥/关闭来源。定时维护控制、实时窗口权限重新绑定和默认启动选择尚未接线；整历史 IO、共享费用和生产接受仍待验收。

跨步骤读取必须继续保持输入的冻结 H 与旧根见证；生产 append、角色变化、native/Metadata 变化或路径替换不能悄悄成为新的来源。
取消、关闭和真实 syscall 退出的边界沿用 [归档生命周期](../contracts/usage-archive-lifetime.md)。错误不得泄露成功 scope；已提交的派生修改则不能伪称已回滚。

## 当前引用和路径所有权

已有有限 publisher 的路径包含真实 `MinSequence/MaxSequence`，旧版 publisher 使用另一种文件名前缀。
但当前日期校验只绑定 canonical UTC 日期分区路径，没有验证文件名中的数字与 descriptor 一致；旧格式还允许其他合法文件名。
因此不能仅凭“新 append 的 sequence 更大”推断旧删除列表的 path 永远不会被新根引用。

实现必须给出可验证的当前引用排除机制：例如同一根 generation 的持久引用协议，或按冻结当前根构建并核对的有限查询索引。
若使用文件名/范围作为排除证明，必须先实现并验证该命名契约，覆盖已有历史格式及中断恢复；不能把当前 publisher 的习惯当作所有根的约束。
unlink 前还必须核对实际文件身份与内容；仅比较 path 字符串或“文件仍存在”不足以区分旧对象和重建的新对象。

[有限当前根引用索引](../contracts/usage-date-catalog-references.md) 现已实现这个机制的 named-path 部分：按有限当前页建立私有固定节点磁盘索引，分别记录数据路径与目录页路径，绑定实际当前根字节/身份、日期策略与目录身份。固定 checkpoint 只记录续读位置/计数；重开或保存 bytes 变化后分批重算已存前缀，即使保存 Ready 也重建全部当前引用，才能执行有界点查。索引不解码数据分片，故不等于行集/native 对账或物理 inode/父路径所有权。实际 unlink、独立权限与内容身份复核、别名/父路径约束、可恢复清理和旧义务完成仍须实现；不能以索引缺少一个字符串路径直接授权删除。

[有限旧目录页回收](../contracts/usage-date-page-reclamation.md) 已加入不可逆清理专用签名进度：只能从实际完成并Close成功的数据job见证启动，绑定原始策略及数据遍历摘要，不能签普通saved Ready。先读取并Close页，再durable下一跳intent，实际当前页引用/身份/checksum排除后unlink并签名推进；重开可在旧链头已经消失时继续。当前共享页和正常追加后新引用受到保护，策略凭据及旧义务仍保留。页Ready不完成孤儿清理或旧义务，也不证明物理磁盘费用归还。见[本地门禁证据](../verification/performance/2026-10-03-usage-date-page-reclaim/README.md)。

## 必须直接覆盖的回归

1. 旧日期、新日期、迟到旧日期交错；保留行不能因序列空洞遗漏，真实 H 不回退。
2. 每步最小工作量、最大页/行/字节限制；大目录跨步骤完成且工作集不随历史长度增长。
3. 候选保存后进程重开；篡改进度、output head、页内容或根不得获得发布权限。
4. 根 rename 前后、file/目录同步、unlink、进度发布及所有实际 Close 的失败/取消/进程退出。
5. 旧游标在清理后显式失败；真实读者阻塞时回收 Busy，取消不提前放行。
6. 回收未完成时继续追加及重开；旧路径被新根引用或 inode 被替换时不得误删。
7. 主节点 durable、副本 applied、成员 term/Metadata/native 最终证据变化；不授予 confirmation、晋升或 native trim 权限。
8. 清理后旧格式 reader/publisher 显式拒绝；当前 publisher、日期校验、窗口权限、离线命令和默认切换保持一致。

新增[单个过期数据分片回收](../contracts/usage-date-file-reclamation.md)：实际 unlink 前结合 fresh 原生精确对账、当前根引用排除、显式日期与原 prune baseline、实际父路径/文件身份和内容复核，使用 pinned parent handle 移除 leaf 并同步目录。离线 Standalone App 在操作前后独立复核 native/Metadata/成员证据，关闭真实 owner 后才交付 scope；HA 与持久成员配置降级拒绝。缺失对象须 fresh 授权后返回零行/零字节，实际 Close 错误不能被缺失状态掩盖；unlink 后失败撤回报告并保留旧义务。后续[有限旧根数据清理](../contracts/usage-date-data-reclamation.md) 已加入固定 8 KiB cursor/计数/prefix hash、每步 descriptor/页/文件限制及整步累计行数/read/native/index 预算。重开从旧 head 分批重算已存前缀，已处理的 eligible 路径必须实际缺失；保存 Ready 同样重算全部旧 descriptor。重算恰到工作上限时继续由同 fresh owner 下一有限块执行新工作，避免离线重开永远重复前缀。Data Ready 仍保留旧页与义务；当前根或来源变化后的自动 rebase、目录页和孤儿回收、旧义务完成、原生构造续传、共享费用及 CLI/默认 Runtime/HA 接线继续未完成。

这些条件须有代码及实际回归证据后才关闭任务。单对象本地入口不能代表完整日期回收已交付，也没有用本机门禁代替 Linux 或生产接受。
