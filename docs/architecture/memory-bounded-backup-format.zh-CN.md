# 内存有界备份：分页目录格式候选

2026-10-04 起实施。format 5 已接入 App summary 创建、CLI 创建/verify/完整输出、默认 Restore 和 Replica 报告；统一 reader 接受 formats 1–5。返回完整 Files slice 的 collecting 兼容 API 保留 formats 1–4，受 1 MiB manifest 上限约束。本文记录当前实现契约；不替代最终方案的共享预算、原生数据验证、恢复/回滚/HA 升级和生产接受条件。

## 格式与完整性

保留 HMBK envelope version 1、现有分块 AES-GCM 和认证 final record。解密后的 tar 包含文件 payload、目录页及最后一个 `manifest.json`。目录页在保留命名空间 `__halro_backup_catalog__/` 下，文件名为 20 位十进制连续页号加 `.json`，从 1 开始。页最多 64 行、编码最多 64 KiB，包含 `version=1`、`number` 和 `files`。每行完整保存 `path`、真实字节数 `size`、真实内容 SHA-256；页内及跨页路径严格按字典序递增。页只能声明此前已出现的 payload，payload 与页可以交错。

根记录最多 64 KiB，`format_version=5`、`compatibility_version` 为 2/3/4，保留原有 Metadata、Ledger watermark/chain、Governance、价格、Key Slot descriptor 及 HA 字段；Files 不接受数组，writer 输出 `files:null`，完整集合通过目录访问。`file_catalog` 包含版本、页数、文件数和 SHA-256。摘要输入为 domain `halro:backup-file-catalog:v1\0`，随后按页顺序追加大端 uint64 页号、大端 uint64 原始页长度与原始 JSON 字节。所有这些记录仍由完整 AEAD 流认证。

Reader 拒绝断号、换序、页号不一致、重复路径、目录内遗漏/额外/错误事实以及根计数/摘要不一致。实际 payload 与声明目录分别建立私有 MAC 磁盘事实索引，以逐项匹配和独立原生计数证明双射，不能只比摘要或信任可修改的 consumed flag。完整 tar 结束和 AEAD final record/尾部验证后才返回 handle。页 JSON 在追加第 65 行前拒绝；页和文件字段拒绝重复/未知字段与尾部 JSON。根 JSON 在类型解码前拒绝各层重复字段，深度最多 16、单对象最多 128 字段、单数组最多 128 项；这些是 format 5 控制元信息解析约束，不是总文件数限制。

## 角色与兼容

`compatibility_version=2` 是旧 Standalone 账务门禁，3 增加 Governance，4 是 Replica 前缀门禁。version 5 本身不表示 HA。Standalone 显式 `source_role=standalone` 并拒绝 cluster/per-store 前缀字段；Replica 必须满足旧 version 4 的完整身份、任期和四个 store head 门禁。各角色必要文件在 writer 发布前和 reader 验证后分别核对。价格、密钥描述符和 native chain 等元信息继续沿用原有验证；备份库不替代 App 的 staged native/domain 验证。

`Create`/`Verify`/`Extract` 的旧收集 API 继续使用 formats 1–4；Create 遵守 1 MiB manifest 上限。`CreatePaged` 写入 format 5；`OpenPaged` 只接受 5。`OpenVerified` 和 App 默认 Restore 接受 formats 1–5，并以逐项接口读取文件目录，不以空 Files 表示成功。尚未运行旧二进制或跨版本升级/回滚验收。

OpenVerified 现已统一读取 1–5：envelope 仍是 version 1，先保留所有实际文件事实并有限检查页候选，完整 ending 后才由 root 选择语义。旧包中碰巧使用保留路径的合法用户文件保持原样；legacy ArchiveSummary 的 Catalog.Version=0，仅表示操作文件数。默认 Restore 已接入同持有者确认、提取、关闭及 compatibility version 的域/HA判断。CreateBackupSummary 与实际 CLI create 已选择 version5；verify/完整报告使用统一reader并逐项生成私有spool，archive/key/index退休后固定32KiB交付。Replica报告按compatibility version校验peer/confirmed前缀并把固定根摘要记录入ordering后缀，旧Manifest摘要算法保留。collecting兼容API仍明确使用旧格式；真实format5 HA恢复、旧二进制/升级回滚及全部资源/故障/生产门禁仍未验收。

## 持有者与发布

SourceCatalog 仍按私有磁盘排序目录逐项供给 writer，writer 只持有一页描述符和固定摘要；根无需回填历史 Files。所有文件和页、根、AEAD ending 完成，临时归档 Sync/Close 和源目录实际 Close/owned 清理完成后，沿用排他 link 与目录同步发布。

VerifiedPagedArchive 持有首次完整验证的源 FD、ciphertext SHA-256、只读操作事实和 32 字节 key copy；调用串行，回调不可重入。Summary 返回根与独立前缀 map，FilesEach 按私有 digest-key 顺序逐项返回事实，不承诺字典序。Extract 只写空私有 staging，对同 FD 再读完整密文、逐项核对 payload 并校验完整第二遍摘要，因此 inode/大小/mtime 都未变化的内容改写也不能获得成功。提取失败可能留下 staging 前缀，由调用方清理；调用方必须 Close handle 并完成 native/domain 门禁后才发布 live 数据。Close 实际关闭 FD/DB、清钥并仅移除自有 inode，不能因取消而提前归还仍在 IO 的资源。

## 接受条件仍开放

本地容器格式证据见[分页备份格式报告](../verification/performance/2026-10-04-paged-backup-format/README.md)。剩余collected域结果、真实format5HA恢复/升级回滚、真实角色/native 数据、旧版本拒绝/升级/回滚、SIGKILL/ENOSPC、空间预检与终结储备、全部费用/并发/孤儿回收及目标 Linux 容量仍需完成。有限页不证明 tar/PAX、加密库、bbolt freelist/mmap/page cache 或全进程峰值已经纳入预算；该格式的控制元信息上限不授予清理历史、可信重启或 HA 晋升权限。
