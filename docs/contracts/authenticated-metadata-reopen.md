# Metadata 保存内容根的独立资格重开

`metadatastate.OpenFromRoot` 是 I-11 的显式库接口。输入来自独立接受的备份来源、保存根身份、保存/完整/允许 head 和同一 Manager 已重建的两份持久 Disk reservation；不能从待打开的文件自己推导这些输入，再把自洽当作资格。调用者须在整个 factory 和结果 owner 生命周期保持独占维护准入。接口不构造 active Seal，不决定 HA 身份/角色/term，不证明生产密钥解锁，不接默认 Runtime。

## 资格与实际文件

从独立 identity/instance/key generation 和实际 Metadata catalog 重建 scope，使用稳定 Ledger key 的两个独立 HKDF purpose，分别认证内容根和 native journal。来源证据与 epoch initial binding 的 schema、catalog、scope、完整初始行数及由备份 cut 归一的新 epoch cut 必须完全一致。

`OpenBoundExactContext` 完整认证实际文件，要求完整 head/binding/file revision 精确一致，并核对 actual FD、named inode、大小及真实文件类型。它拒绝 partial tail，永不截断。旧 `OpenBoundContext` 的已资格 tail repair 行为保留；format1 普通 reader 仍不接受 format2。

`InspectBoundContext` 在 native 排他下先完整认证，再访问包括 initial control 的全部 frame，最终复核实际 source。factory 对包含排除尾部在内的全部 operations 检查 domain/schema 资格，以一个上一帧 scalar receipt 和下一帧的实际 start 得到物理 end。保存及允许 head 必须各自是实际帧边界，不能猜测 JSON 长度或仅比较 sequence。保存根的 cut 须对应实际保存帧；sequence0 根须与初始 binding 的 cut/digest/ID/完整行数一致。

内容根通过 `StoreOptions.ExpectedRoot` 核对独立保存的 Scope/Cut/Digest/ID/Revision/Rows。该检查先于 pending root 或 orphan pack tail 的修复。head/binding/边界/完整根身份不匹配时，不修改 native、根或 orphan pages。资格已成立后，底层可按既有规则清理合法未发布 pages；随后从同一认证 pin 检查实际 instance/schema/reader/feature 和 epoch/applied sequence。内容 MAC 不使错误的 bookkeeping 成为合法位置。

## 恢复与可见性

只对 `(saved, permitted]` 的完整 frame 复用[同一整帧归约器](authenticated-metadata-frame-delta.md)，每帧原子推进内容和 bookkeeping。完整来源再次认证，排除尾部不进入内容根。durable head 不自动变成确认许可；保存位置领先于允许位置或允许位置不在真实链上均拒绝。保存根和完整 native 相等时应用零帧，保持内容和 root revision。

如果 native 已 Sync 但根尚未发布，独立接受旧根和允许的新 native frame 后，重开清理合法孤儿 pages，只补应用这一帧；同 cut 重试不重复应用。任何失败或取消都返回 nil candidate 和零 receipt，先关闭真实 native/store owner 再释放本 job 费用；不存在半完成查询对象。

`ReopenReceipt` 区分完整扫描的 SourceFrames 与实际 AppliedFrames。当前 native Open/inspection/replay 多遍扫描仍为 O(历史字节)，不是整条启动路径已增量的证明。

## 费用及关闭

factory 与 replay 共用一份实际 64MiB IO workspace，通过同 Manager 的 permit retain 覆盖整个工作；归约另有 64MiB Update workspace，另计现有 owner/claims。不能先释放 factory 费用再让未结束的 IO 继续，也不能退费两次。

借用的内容根和 journal 持久 reservation 在实际 Close 完成后释放借用引用，原持久 owner 保持。format2 journal 关闭时清除 owned key。借用候选无 Discard 权限；它不能通过已存在路径取得删除资格。原 publisher 可 `DiskReservations` 保留关闭后的费用，并在成功、完整关闭的合格后继上 `AdoptClosedSuccessor` 更新 root 删除 receipt。原 publisher 与后继必须同 Manager、namespace、key tag、initial binding；失败或取消的后继不能接管清理 receipt。跨进程由调用者独立 reconciliation 重建持久费用，不能重用退出进程的 Permit。

## 仍未接受

[trim cut-state 关联](authenticated-metadata-trim-state.md)已扩展合格重开：独立接受的完整 cut-state root、旧 source provenance 和实际新 anchor/revision 必须一致；没有该关联的裸 trim 文件仍拒绝。不能把旧 initial root 直接作为 trim 起点。全组件 active Seal、生产角色/term/key qualification、持久 inventory 与物理清理接管、format1→2 迁移、整文件 schema/restore/compact/rotation、原生日志有资格的增量初始化和历史审计时限、完整故障矩阵、Linux/生产接受仍开放。M3 默认启动仍禁用。

[原始验证与源码快照](../verification/performance/2026-10-04-metadata-reopen/README.md)。
