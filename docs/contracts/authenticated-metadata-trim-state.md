# Metadata trim cut 的内容根与物理来源关联

`Baseline.TrimAtApplied` 是 I-11 的显式维护接口。只裁剪当前认证内容根已应用的前缀，保留未应用 durable 尾部。它不决定角色、确认或 HA 允许位置，不接默认 Runtime，也不回收内容树旧 pages。

## 认证关联

trim 不重签普通 DB，不改写内容根，也不把新偏移套到旧 root cut 上。新的 format2 trim anchor 携带固定 `TrimStateBinding`：完整 RootCommitment（Scope/Cut/Digest/ID/Revision/Rows），已应用 sequence、旧 source file revision、该帧实际 start/end 和 hash。原 initial epoch BaselineBinding 仍保留来源含义。

维护 owner 从认证根读取真实 Metadata schema/instance/reader/feature/epoch/applied rows，完整认证 native 并检查全部 domain facts，包括未应用尾部。root cut 必须由旧 revision 与实际帧 start/hash 推导，native rewrite 再独立核对实际 end/hash。候选大小在创建之前受现有 native disk ceiling 限制。不能裁剪过 root 的 applied sequence，不能从 durable 推导已应用。

新 anchor 的 physical revision 加一，逻辑链头保持原 cut hash；保留帧的原始 bytes/hash 不变。anchor 的实际物理 end 决定新 applied head。内容根及全部行不变；同 cut/零 cut 不重写，但仍完整认证 source、域资格和内容 bookkeeping。

## trim 后重开与增量

`OpenOptions.TrimState` 须由独立接受提供，并与实际 MAC 有效的 anchor 完全匹配。保存位置不能早于 TrimmedThrough。保存根就在 trim cut 时，必须匹配完整 cut-state root identity；其 cut 用旧 source provenance 核对。保存位置晚于 trim cut 时，用当前 revision 中的真实保存帧核对 delta cut。新 revision 的后续归约生成新的位置/内容 cut，不猜测旧物理偏移。

缺少或改变 state、旧 revision、保存位置落后于已删前缀、根/来源不匹配都在 root/orphan repair 之前拒绝。裸 `metadatajournal.Trim` 仍保留旧维护兼容行为；其没有 cut-state 关联的 format2 文件不能进入此合格重开路径。format1 不接受 state，旧 format2 reader 会拒绝它不认识的新 state 字段。

## 实际文件、费用与失败

native append mutex 覆盖完整 rewrite。候选以独占创建取得实际 inode receipt，验证来源与候选后在同目录 replaceCaptured，目录 Sync。关闭旧 descriptor 后才能结束 rewrite 临时磁盘费用；新 descriptor 必须匹配实际创建 receipt。原生日志保留新 inode 的创建资格，不能沿用旧 inode 删除权。

post-rename 取消、错误或 panic 时仍保留实际新 inode 的 receipt，停止 append，关闭真实旧/new descriptors 后才能退临时费用。外来源/候选/新路径保留并拒绝。域候选失败则停止查询，返回零成功 receipt；内容根保持实际原值，不回滚已发布 journal。

同 Manager 的 IO workspace 固定64MiB，rewrite 额外预留整个 native ceiling Disk；现有 root/native 持久费用同时保留。资源前置拒绝保持原候选可用。Close 取消并 join 整个 job；归还临时费用只说明逻辑 owner 退役，不证明磁盘 blocks 已释放或 Linux RSS 门槛。

成功完整关闭的合格后继可把实际 replacement receipt 交回原 publisher；借用重开本身不制造 native 删除权。原 publisher 经 root 与 native receipt 更新后才能按真实 inode 清理。没有实际 replacement creation receipt 的跨进程新 owner 仍不能凭文件存在获得删除权限，持久 inventory/reconciliation 另行完成。

## 仍未接受

完整 active Seal、多组件一致发布、角色/term/confirmed/key-unlock 资格、生产费用重建/跨进程删除、格式迁移、schema/restore/compact/rotation、原生日志的增量初始化与历史审计时限、完整故障矩阵、Linux 和生产接受仍开放。当前 source/rewrite/reopen 保持 O(历史字节) 全认证扫描，不能声明整个正常启动已增量。M3 默认启动未启用。

[原始验证与源码快照](../verification/performance/2026-10-04-metadata-trim-state/README.md)。
