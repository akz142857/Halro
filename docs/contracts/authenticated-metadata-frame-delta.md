# Metadata 整帧归约、先准备后持久化与显式前缀恢复

本契约是 I-11 私有 format2 组件。默认 Runtime、format1 writer、HA 协商、跨进程 Seal/角色选择未切换。

## 整帧写入

`Baseline.AppendFrame` 只操作已经从完整认证备份导入并建立新 epoch 的 private owner。先检查实际分类、格式/字节/路径限制与 applied/durable head 一致，再取得同一 Manager 的 64MiB IO 工作费用。`AppendPreparedContext` 持有 native 排他，规范编码并独立解码自有 facts，计算本帧实际 hash/位置，将其交给 `Store.UpdateNativeFrame`。

内容树先完整归约一帧、写候选 pages 并 Sync，才执行 native Write/Sync 屏障；屏障成功后按既有 root/staging/rename/目录 Sync 发布。语义和内容树逻辑/append ceiling 拒绝发生在 native append 前，不能拆成多次 root 提交。物理日志/根发布 IO 仍可能失败；native 已落盘而 root 发布未确认时保留实际 head 和文件，永久撤销候选查询，不能回滚或虚构成功。该机制不是磁盘预分配或 ENOSPC 全部接受。

普通 `Store.Update` 仍为 64 mutations、8MiB 累计输入/返回、4096 read pages、256 read calls。显式 NativeFrame profile 固定为最多 32770 mutations（1024 operations × 32 ancestors + 2 bookkeeping）、512MiB 累计输入、4456720 read pages、131080 read calls、32MiB 累计返回。输入/返回为累计工作界限，不是保留该规模的驻留集合；单键/value、fanout/height、实际 append/disk ceiling 和 64MiB Update 工作费用不变。该 profile 不授权任意更多 native ops：format2 仍限 1024、32 层及 8MiB frame。外层 IO 与内层 Update 同时需要至少 128MiB 工作费用，另有现存 owner/claims；尚未完成 Linux RSS/延迟校准。

## 四类实际操作

源码现有完整操作集是 Put/Delete/CreateBucket/DeleteBucket，没有 Sequence 修改操作。

- CreateBucket 与旧 replay 一样逐层 CreateIfNotExists；已有 bucket sequence 必须为 8 bytes 并保持原值，新 marker sequence 为零。创建拒绝父 bucket 中的同名 value。
- Put 需要所有祖先 bucket 存在；拒绝同名 child bucket。空 value 是真实空值。
- Delete 对缺失 bucket/key 幂等；存在同名 child bucket 时拒绝，不能把 bucket 当普通 value 删除。
- DeleteBucket 对缺失 bucket 幂等；存在时采用[认证范围剪裁](authenticated-metadata-range-delete.md)，连同全部后代/sequence 一次删除。同帧重建从新的零 sequence 开始，旧后代不复活。

使用实际 Metadata catalog，只接受 A/B/D。C/E、journal bookkeeping、未知 bucket/meta key 都在准备前拒绝。meta 桶结构整体变更与 schema/instance/reader/feature gate 的变化需要独立整文件/epoch 发布，不通过此 delta 路径。有效编码路径必须是 UTF-8，二进制 key 仍保留原 bytes；无法由既有 JSON string path 表达的桶名必须显式拒绝，不能经 JSON 替换字符后签名另一条路径。导入根仍覆盖源 DB 的全部二进制桶名。

成功归约在同一个 root transaction 中最后写入真实 epoch 与 sequence。新 cut 同时绑定初始 baseline cut/root digest、file revision、实际 frame epoch/sequence/offset/hash。原备份 `BaselineEvidence` 和 initial native binding 保持来源语义，不改成当前 projection digest。

## 显式前缀恢复

`ReplayTo(expectedHead, through)` 要求调用者显式给出当前完整 head 和允许 sequence。它不从 durable head 推导确认或角色权限。先认证整个冻结 native 文件，再对全部 facts（包括排除尾部）检查静态分类/schema/格式资格，之后只归约 selected prefix；每帧原子推进内容与 bookkeeping。再认证取得所选 frame 的真实物理 end，不能猜测 JSON 长度。实际源 named inode/FD/size、完整 head/hash/binding/revision 在扫描前后核对。

排除尾部的坏 MAC 或不合格 domain facts 在任何 selected root 修改之前拒绝。后续读取/应用/最终源复核失败则 candidate 停止查询；已实际提交的私有根保留到物理清理或独立恢复。当前仍需 O(native 历史字节) 的完整扫描，不是增量初始化或其延迟接受。

Close 先取消再 join 完整工作与真实 native/root IO，不能在 callback 返回时提前退费。native durable closure 是 callback-scoped、零队列、最多实际写一次；逃逸之后拒绝，正在进行的实际 Sync 必须 join。拒绝改变准备 facts、漏调用/重复调用屏障，以及 durable 后发布失败继续 append。外来 native inode 保留，不向已被路径替换的 FD 追加。物理 Disk owner 的删除资格与目录 Sync 规则保持。

## 尚未完成

整帧归约与 1024/64 冲突已在显式库接口闭合；format1→2 的生产兼容/迁移仍未接受。[独立保存根重开](authenticated-metadata-reopen.md)已有未 trim epoch/revision1 的显式接口；生产跨进程 root/位置/密钥资格选择、完整 Seal/角色/term/允许前缀、[trim cut-state 与 revision 关联](authenticated-metadata-trim-state.md)已有显式接口；完整生产 resume、整文件 schema/restore/compact/rotation、持久费用重建、历史审计时限、完整故障矩阵和 Linux/生产接受仍待完成。当前组件不证明实际生产 Vault/Master Key 解锁或所有 key envelope 的业务兼容，也不给 Replica 源节点 C/E 或晋升后 session 的资格。

[本轮原始验证](../verification/performance/2026-10-04-metadata-frame-delta/README.md)。
