# 内存有界状态的 Runtime 维护契约

已显式完成 `metadata migrate --activate-runtime` 的 Standalone 使用同一个选定七根世代处理真实 Metadata、账务、Usage、Governance 和备份。没有完成迁移的目录使用过渡 reader；存在迁移候选而没有 bootstrap 时，默认启动拒绝，避免改变正在迁移的来源。

## 在线 checkpoint 与页文件回收

`Runtime.CheckpointMemoryState` 暂停新的业务 HTTP 准入，返回 503 和 `Retry-After`，保留 health 调用；等待已接受的完整 handler 和终结清理退出后，暂停 deferred worker、Metadata 与域 writer，取得真实原生维护租约，再发布同一 cut 的七根。取消等待不会发布 root，也不会撤销健康 writer；正常完成继续使用原来的业务 Manager 和 listener。关闭与 checkpoint 共用 job 锁，物理 owner 退出前不归还费用。

五个内容树达到文件上限的一半后，维护可把完整可达行写入另一个固定 pack。先发布当前七根，再对候选完整认证、同步并发布 compact 七根，最后关闭与退休旧 pack；没有独立 common selection，`Commit` 拒绝。准备失败且旧 seal 仍被选中时回滚；新 seal 可能已持久化而退出不明时保留文件和资源，撤销业务可用性。

候选 pack、固定控制文件以及四个最早中断保留槽均由同一个 Manager 的额外 Disk reservation 覆盖。该额度是逻辑文件费用，不能保证操作系统已经预留物理块。启动 inventory、磁盘/inode 停止线及实际 ENOSPC 是独立边界。最早创建留下的无认证空页文件与不完整 intent 只作 opaque 保留，不因此获得内容或删除权限；陌生大文件、控制根、外来 inode 或槽耗尽拒绝继续回收。

定向证据包含真实 Runtime DAO churn → common checkpoint → 旧页文件消失 → compact root 重开，以及四轮实际 Runtime slot 连续交替、pin 拒绝、取消、实际 SIGKILL 后按共同 seal 选择完整 pack。测试以固定小规模证明协议；没有据此承诺大历史下的维护暂停时长。

Ledger 原生关闭先撤回入队，再加入实际生命周期、writer、mutation 与启动历史 reader。调用方 deadline 到期且退出未知时，原 owner 保留 FD、密钥和未决费用并锁存 recovery；普通重试不清除此债务。满队列及真实 durable callback/历史 FD 回归见 [原所有者关闭证据](../architecture/evidence/native-close-join-deadline-v1-2026-10-07/README.zh-CN.md)。这些等待可取消，不授予强制中断同步内核 Close/Sync 的能力。

Standalone 的物理 Usage 查询窗口在已认证归档 witness 下借用原始维护租约与实际冻结 source，独立分配在全部终结债务之上的 COW 候选；不会拿原生帧的未来根做裁剪。带历史请求的启动、过期窗口裁剪、取消／闭合租约拒绝、原生累计事实不变、共同发布及重开见 [物理窗口回归](../architecture/evidence/physical-usage-window-v1-2026-10-07/README.zh-CN.md)。真实 Linux race／ext4／2 GiB cgroup 通过；functional sampler 与 offline Runtime 不替代目标容量或普通 worker 并发接受。HA 的派生裁剪仍需要独立角色协议。

## Seal 之后的 Ledger Roll

已完成的 Roll 后，selected 启动从原签名 anchor 验证实际新 sealed 文件与 active suffix，内存只留固定 reader、decoder 和至多 63 行的目录事务页。原选定历史 descriptor 的语义字段须保持精确一致；已选原始 WAL 改为规范 gzip 名称时，仅物理文件名、压缩标志及 stored 长度/hash 可以变化，必须流式复核原签名明文长度/hash、序号、链及 epoch。新增 descriptor 的 sequence、前后链、原始与 gzip 明文 checksum 都要匹配实际文件，认证失败不得顺手修复 active partial tail。新增 sealed 文件的完整物理 checksum 需要流式读取其所有字节；内存有界不代表跳过这部分 IO。

七根重开、真实账务/Usage 和共同发布测试覆盖两个完成 Roll；另有 70 次 Roll 与首个 63 行未发布目录页后的重开。在线目录刷新现在也按每页最多 63 行处理，最后才更新状态标量与共同 seal；中途失败撤销该目录读取/发布，并要求默认 Runtime 恢复。实际 70 次业务轮转、70 段压缩、两次 common checkpoint、重开及 Usage/物理 locator 核对通过普通与 race 回归。空认证 Ledger 的预设 epoch 只可由实际首帧支持，非零 anchor 不借用此例外。reader 实际 Close 未决时，Log 保留 descriptor、密钥及来源 IO permit 并撤销后续读取，不以再次 Close 消除债务。

pending Roll 仅在原签名目录、完整实际链和独立 saved anchor 同时通过后恢复。未 rename 的 intent 只撤销未完成发布；已经 rename 的实际文件须完整匹配，后继只允许不存在或为空，不得截断非空 successor。修复前重查 common selection、目录与文件 inode 及取消；新增 successor、manifest 与 locator 仍使用同一 Manager 的实际 Disk owner。11 个原始 Roll SIGKILL、10 个恢复 SIGKILL、篡改及撤回权限回归，以及默认七根 Runtime 三种布局的实际账务/Usage 重开通过；见 [pending Roll 证据](../verification/performance/2026-10-06-memory-bounded-resume/selected-pending-roll/README.zh-CN.md)。恢复按固定 workspace 流式认证完整历史，其 IO 随历史增长；HA 仍拒绝借用此 Standalone 修复权限。

## 首次迁移

先在固定 `memory-state.preparing` 写完独立来源 receipt、同步并核对 inode，再排他发布 `memory-state`。显式 `--restart-preparation` 重新验证相同完整备份、独立 native keys、原生日志、目录锁和 inventory；认证的未发布世代最多保留四槽，未认证 staging 最多保留四槽。opaque 内容不被读取、认证或删除。已有已发布目录篡改仍拒绝。

首次创建至发布有九个真实 SIGKILL 时点，重启保留有六个时点；最早中断还覆盖默认启动拒绝且 native 字节不变、完整激活、实际业务及二次重开。旧实现已发布而没有完整 receipt 的未知目录仍须显式离线归档；新协议不会提前发布该名称。

离线 Ledger、Usage 和 Governance 初始建根使用新的私有有界磁盘 reducer，再按固定 ordinal namespace 流式生成最终认证树，避免把每条事实的 COW 中间垃圾留在初始 pack。该 scratch 必须不存在，从独立许可的完整原生事实重建；已有 DB 不可作为此入口的来源。完整原生校验、同池 decoder/mmap workspace、额外 scratch Disk、精确 cut 和实际 Close/Discard 屏障均保留；正常运行的增量 writer 仍使用原 typed reducer。

## HA 共同权限与回退

真实 Replica 在构造新 generation 之前校验认证 MemberState、完整 ordering 和四源允许位置。Metadata projection cursor 必须属于同一 applied ordering cut；各文件单独合法仍不足以授予 readiness。Primary 的本地持久 cut 可以领先已确认 cut，但最终恢复与启动仍由真实确认门禁完成。较小许可前缀重建撤销旧 owner，后继确认才能推进。

Metadata format2 的实际复制组件已接通独立 `BoundReplicationAuthority`、NativeSource、Replica fsync、OrderingJournal fsync、ACK 和 Primary 确认。Runtime Hello 按真实 source 格式协商；reader 数量和 actual Close 未决费用继续有界。legacy seed 与 Replica backup 拒绝未被 manifest 覆盖的 memory-state 命名空间，先验证真实 MemberState 配置再允许原生修复。详见 [format2 复制证据](../verification/performance/2026-10-06-memory-bounded-resume/metadata-format2-replication/README.zh-CN.md)。

同组 Metadata format2 的停机 Primary producer、独立签名 carrier、固定五文件安装和 replication-only maintenance 已实际接通；原 N 的四源/Member/ordering 资格通过后，以同 incarnation/term 的 N+1 boundary fsync、应用、ACK 与确认，不重置排序位置。安装过程有五个实际 SIGKILL 时点；显式 restart 先核对同 Manager inventory，最多四槽保留 opaque staging，再从调用方独立 carrier 重新复制或完整验证已发布候选；不读取/删除 opaque 内容。真实 TLS、Primary 未确认边界及 Replica 已应用边界的 SIGKILL 恢复也已验证。这些维护入口不提供业务 HTTP/readiness 或晋升授权。显式激活后的默认 Primary/Replica 业务 Runtime 已接七根选择、真实 TLS 确认与增量恢复，Seed、晋升后再次 Seed 和原 opaque 控制/history 归档已有本地固定源码证据。HA 完整物理信用与在线续期仍在接线，生产 physical opening guard 保持关闭；不得仅放宽 Standalone guards 或以 quorum confirmed 替代全部实际 Replica 的终态应用。两轮原所有者重新准备、七域 received/applied 观察及真实 loop/FD 退出见 [HA 续补接受](../architecture/evidence/ha-physical-renew-close-v1-2026-10-07/README.zh-CN.md)，该组件结果不授予 Default opening 或完整容量接受。

最新状态的独立目录回退命令为 `metadata export-legacy`，见 [完整契约](selected-legacy-export.zh-CN.md)。它验证当前构建的 legacy reader，目标可运行、可按既有协议轮换，原 selected 目录保持可重开。离线验证保留真实启动/恢复/关闭及压力监控，同时禁止 provider、deferred 和外部告警 worker 启动。selected 原目录的 File/KMS DEK 轮换已有本地功能和故障证据，包含实际同组 TLS 及轮换后再次 Seed；完整物理同组、多维护周期及任意旧二进制兼容继续开放。

## 验证与剩余接受

Usage 离线 replay 的 future-finalization overlay 仅保留已核验的序号和归属字段，不保留大 EventID/价格快照。96 个各带 900 KiB EventID 的真实原生日志回归覆盖 heap 保留上限、迟后归属检查和事务中途失败后的原子回滚；该 GC 后存活值不替代完整 RSS/cgroup peak。

本轮具名检查加入 [冻结故障矩阵](memory-bounded-fault-matrix.json)。完整 Go、定向 race、Linux 物理容量分别记录结果，不借用先前源码的通过记录。四个历史审计并发验证同一 common Seal 时，读取在实际 IO 前取得有限共享 claim，再串行真实文件读取；Ledger 启动来源作同样的已计费等待，原维护 callback 排他门禁继续拒绝重入。独立竞争复现、费用上限与 actual audit descriptor 回归保留修前失败，避免把另一个合法 reader 的短暂占锁当作权限缺失。

默认 selected Runtime 的 [真实 OS 压力和两轮维护回归](../architecture/evidence/runtime-memory-load-v1-2026-10-07/README.zh-CN.md) 在固定 2 GiB cgroup 下通过：每轮提交 64 个 handler 调用，实际 provider 同时活跃峰值 1，有限准入拒绝、已接纳终结、费用/FD 退出及独立新进程重开均核对。该用例不授予 64 路业务容量、网络 listener 或大历史吞吐承诺。

Linux runner 的 `--mode selected` 实际迁移 seed，再由独立进程重开七根，检查原生位置、余额、请求终结及 Usage；`--storage tmpfs` 提供有限 inode 信号，并记录 tmpfs 页和 warm filesystem cache 的实验边界。初始普通 volume 的 inode 信号不可核验时，真实压力门禁拒绝，未改为放行。每个样本执行前校验完整固定预算配置。磁盘 profile 和物理 RAM 接受门槛分别记录，超过文件 ceiling 是容量拒绝，不能重签成内存通过。seed 和 restart 的实际 cgroup peaks 都纳入验证；warm 页可能仍记在前一个 cgroup，不能只看 restart 的 current/peak。固定 2 GiB 配置下 100、1,000、10,000 请求各三次实际迁移/重开通过，seed peak 最大 541.01 MiB；[固定快照证据](../verification/performance/2026-10-06-memory-bounded-resume/linux-selected-2g-real/README.zh-CN.md) 同时报告 seed 和 restart。小规模样本不关闭 V-02。

仍开放：HA 物理未来信用、公开 Default opening 和在线暂停/续期/恢复；完整角色组合 ENOSPC 与确认停滞故障；大遗留恢复逐批故障及三节点完整切换；目标规模、并发和多维护周期的 Linux 2 GiB 验收；selected 物理同组多文件轮换及旧 reader 兼容。D-01/R-01 生产取证、发布和生产观察按用户要求暂缓。

selected COW Metadata 的完整 disk ceiling 与真实旧 bbolt 文件可分别配置；`legacy_metadata_file_disk_bytes` 正值只在 `memory_state_required` 下使用，实际旧文件与导出均强制执行该上限并收取 ceiling + 32 MiB mapping RAM。它不缩减 COW 物理储备，零保持旧行为。配置、真实 bootstrap/超限拒绝及轮换接受见 [独立旧文件上限证据](../architecture/evidence/legacy-metadata-file-ceiling-v1-2026-10-07/README.zh-CN.md)。
