# Metadata 内容根与 epoch/file revision 绑定

本契约是 I-11 的私有离线维护组件；未接默认 Runtime、HA 协商或整文件生产发布。

## 实际关联

`Baseline.StartNewEpoch` 只消费已经由完整认证备份构造的 private candidate。要求同一个稳定 Ledger key、严格晚于来源 epoch 的新 epoch、私有目录中尚不存在的 native 文件，以及同一 Manager 的工作/持久费用。

构造顺序为：取得独立 64MiB 工作用途和整个 native ceiling Disk owner → 暂停候选查询 → 认证读取当前根身份 → 在一个 COW transaction 中写入新的 `metadata_journal_epoch` 和 `applied_journal_sequence=0` → 捕获实际新 root → 独占创建、同步 format2 epoch header 及目录 → 检查取消 → 返回 fixed head/binding receipt。

`Store.RootIdentity` 在持有实际 Store owner、独立 4KiB IO 费用、pack/header 与当前 root 字节检查后返回固定 Scope/Cut/Digest/RootID/Revision/Rows。它不是 resume、角色或确认许可。原始 `BaselineEvidence` 始终描述备份来源，不能被改成新的 journal 位置。

Header 的 `BaselineBinding` 经 native MAC 绑定 version、epoch、独立 identity/key generation、schema、真实 class catalog、实际 root scope/cut/digest/ID/完整行数。新 root 的 cut 同时绑定原 cut 与新 epoch；journal key 使用独立稳定 HKDF purpose。没有从 live DB 推导身份或签名旧普通 DB。

## 格式与 reader

新的绑定使用 frame format2。旧 reader 的 frame version 检查会拒绝它；未增加“同一个 format1 payload 但旧 reader 忽略新字段”的兼容漏洞。默认生产 writer/HA advertised format 仍为 1。

format2 control 与 operations 均使用严格规范 JSON。拒绝未知/重复字段、不同编码、额外内容、payload/绑定 epoch 不一致、无 root/revision、未知组件值、reserved header bytes 与文件内混合 frame version。Ops 解码在扩展 slice 之前限制 1024 个操作、32 层 path，限制 key/bucket 名 32768 bytes、value 3MiB 和整体编码。writer 在 Marshal 前执行相同界限；源 frame 本身仍有 8MiB 上限。

普通 Open/OpenReplica、旧 replication cursor/head/range/export/truncate/repair 及 HA callback 不授予 format2 资格。显式 `OpenBoundContext` 要求独立预计的完整 binding、当前 file revision 和 disk ceiling；这些条件在任何 torn-tail repair 之前检查。尚无完整 control frame 的文件不能被任何普通 Open 截断成“空 journal”。

## Trim

`TrimContext` 在已打开的同一源 FD 上完整认证第一遍，再以固定 64KiB copy buffer 和单 frame 解码工作集认证/复制保留后缀，逐 frame 核对实际复制 bytes 的 SHA256。源第二遍 head 必须与第一遍完全相同；temporary Sync 后再次完整认证候选。最终核对源/temporary inode、源大小/mtime、取消，再 rename 与目录 Sync。未知/替换路径保留，不以 pathname 重开 source，也没有随后缀增长的 retained slice。

format2 trim anchor 保留完整 baseline binding，增加 file revision。零 cut 与相同 cut 是真实 no-op，不增加 revision；全部 operations 被 trim 后 binding 仍保留，之后 Append 仍写 format2。旧 revision 拒绝。Trim 位置不足以跳过丢失事实：root 的 applied position 低于 TrimmedThrough 时不可回放，后续 Runtime 必须拒绝，不能把旧 baseline 当作 trim cut 状态。

固定 cursor 不等于完成空间/RSS 接受。Trim 的 rewrite Disk 预检/owner 聚合仍是调用者义务，没有空间预分配保证；原 private publisher 的 inode 已被替换后不具备新 journal 删除权。[cut-state rewrite](authenticated-metadata-trim-state.md)现有实际创建 receipt 的接管和合格重开；持久费用重建及跨进程生产删除资格仍待实现。

## 生命周期与失败

非法 key/epoch、路径、压力和资源准入在 root mutation 前拒绝，保持原候选可用。开始修改之后任何错误/取消都永久撤销该候选查询资格；不回滚后来 root 或虚构成功 receipt。Close 先取消整 factory，再 join 实际回调/native IO 和其费用，最后关闭 native 与内容根。普通 Close 保留两个持久 Disk owner。

Discard 必须先实际关闭并删除自己创建的 native journal、同步目录并释放其费用，再删除引用的 root。外来 native replacement 不删，同时保留唯一引用根及两份费用。任何物理清理失败需保留 owner 并重试；不能以取消或同路径新文件的存在退款。

## 剩余开发条件

- [四类整帧归约与先准备后持久化](authenticated-metadata-frame-delta.md)已有显式库接口证据，包含桶删除重建/sequence、真实 1024-op 与 32 层帧和同帧 bookkeeping。普通 root transaction 的 64-mutation 上限未放宽；独立整帧 profile 解决原子接受冲突。format1→2 的生产迁移/兼容接受仍待完成。
- [独立保存根资格重开](authenticated-metadata-reopen.md)已有未 trim epoch/revision1 的显式库接口及跨进程新 Manager 验证；生产完整 Seal/角色/term/key qualification、持久 Disk inventory/reconciliation、[trim cut-state/旧来源位置/新 revision 关联与实际 rewrite owner 接管](authenticated-metadata-trim-state.md)已有显式接口证据；完整生产选择/增量 resume、历史审计时限仍待完成。
- schema/restore/compact/rotation 的整文件多阶段发布，以及外部 active Seal、故障矩阵、Linux 资源/性能和实际生产接受。

format2 只由显式库接口写入私有文件。上述条件未闭合前，默认 Runtime 与 M3 不选择这条路径。证据见 [本轮验证](../verification/performance/2026-10-04-metadata-epoch-binding/README.md)。
