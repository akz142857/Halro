# Metadata 认证备份内容基线

`internal/metadatastate.BuildFromBackup` 是离线内容根构造器。来源必须是已经完整验证的 `backup.VerifiedArchive`，不接收 live Metadata DB 路径。它不授予 Runtime readiness、恢复角色、term、确认前缀或当前 epoch 的可信启动资格。

## 来源与身份

- `OpenVerifiedFile` 在同一个 pinned archive FD 上重新消费完整 ciphertext，核对所有 payload 的大小与 SHA256、manifest 结束位置、完整文件数量、原生 AEAD ending 及首次完整验证的 ciphertext digest。只落盘 `data/metadata.db`，没有提前暴露选中文件或集合式 seen-name 分配。
- 选中文件须在调用者保持排他的私有目录中独占创建。回调借用真实 read-only FD；不能 Close、逃逸或留下尚未关闭的 duplicate/mmap。来源 owner 在回调前后核对原 inode、完整文件摘要与目录身份。read-only FD 本身不禁止另一个进程写同一文件；目录/文件排他必须维持整个构建期，前后摘要不能替代这个条件。
- bbolt 只通过 `OpenFile` 取得该 FD 的 read-only duplicate，不重新从 pathname 选择输入；read-only、无迁移、不预加载 freelist。实际 View、DB Close 和来源后验验证均完成后，候选才可返回。
- component Identity、InstanceID、KeyGeneration 与稳定 Ledger key 由调用者独立提供。实际 Metadata `instance_id` 必须匹配预期，schema 必须与当前 reader 一致；TxID、reader/feature gate、journal epoch/sequence 与加密备份 manifest 的 MetadataInfo 必须逐字段相同。位置整数采用严格 8-byte 解码；Info 中忽略 decode 错误不能成为可信位置。
- 稳定 key 使用独立 HKDF purpose；scope 绑定 codec、独立身份、实例 ID、key generation、当前 schema 和真实 journal 分类表摘要。cut 绑定备份 ID、完整 ciphertext digest、manifest digest、文件摘要/大小、MetadataInfo 与分类范围。行数由内容树的认证 commitment 绑定。

备份内容的真实性是该备份 cut 的来源证据，不证明备份创建前 live DB 的历史真实性，也不证明备份是最新状态。它不替代首次迁移的接受决策、外部 anchor 或 HA 仲裁。

## 完整内容与读取

所有实际 top-level/nested bucket 和全部 raw values 都入根，包括 A/B/C/D/E/journal bookkeeping；空桶和桶 sequence 也入根。实际分类表为清单来源，未知桶、未知 meta key、meta 嵌套桶、深度/编码键/值超限均明确拒绝，没有默认分类、遗漏或静默截断。

固定 32 层 cursor 栈逐项消费。codec 用零字节转义与路径 terminator 保留原始字节顺序，区分桶 marker 与 value；不把 `/` 或字符串拼接当作键身份。认证树的点查、缺失证明及完整有限范围读取沿用 `authenticatedstate`，不再信任普通 bbolt Cursor 的完整性。

包含 C/E 是覆盖原始内容，不授予 Replica 使用源节点派生数据的资格；sessions 的晋升/恢复失效规则也不被这个内容根取消。后续角色适配必须明确重建/失效规则。

## 生命周期与费用

文件大小限制及同一 Manager 的 Disk/RAM 准入在创建前检查。选中文件的整个逻辑 Disk 费用保持至实际消费者退出、FD Close、owned inode 删除和目录 Sync；Close 本身不退款。Close 会取消并 join 正在执行的消费者。替换文件/目录保留，未完成物理清理不退费；清理失败返回非 nil、不可查询的 cleanup-only owner，调用者必须检查 result 与 error 并重试 Discard。

构建期同时持有选中文件的 4MiB IO envelope、Metadata 解码/游标 32MiB 加完整源文件大小的 RAM envelope，以及认证树自身工作/持久 owner。完整源文件的 RAM 费用是对 mmap 驻留的保守逻辑记账；未作 Linux RSS/cgroup 校准。大历史源可因有限资源上限明确拒绝，不能把本路径声称为任意历史规模的生产有界迁移。

返回前实际关闭 bbolt、重新校验源、清理本轮源文件并同步目录。任何失败不返回 ready 候选；已构建根也回收本轮 owner。已关闭内容根保留持久费用，Discard 才删除与退账；源错误不能退款其他 owner。生产 Manager 的 Disk 配置仍为 0，没有自动创建“已对账”持久费用。

## 尚未授予的资格

本路径没有将 baseline ID/root 关联到 MetadataJournal epoch/header、trim file revision 或 resume token，没有应用 journal suffix。schema/restore/compact/rotation 整文件发布、Master Key 与 credential/MFA/envelope 兼容、跨组件 Seal、外部身份与当前角色许可仍待实现。默认 Runtime 不选择该根，M3 不启用。

定向证据见 [Metadata 基线验证](../verification/performance/2026-10-04-metadata-backup-baseline/README.md)。
