# 共享资源预算基础模块

对应 [主方案](../../docs/architecture/memory-bounded-state-design.zh-CN.md) 的 I-03。共享逻辑预算已接入 HTTP/deferred/failure-capture 生命周期、部分 HA workspace/队列/Source 读者、可选 Primary 未确认 append admission，以及 Ledger/Usage/Governance 投影、Audit/Governance/Metadata/Ordering 原生日志和 segment index 的可选磁盘 ceiling 与持久 owner。实现仍不完整：未覆盖的持久化/临时消费者、启动时全目录身份与占用 reconciliation、动态 inode 文件系统策略、物理块预留和目标容量接受仍开放。它不替代计费 `internal/budget`；产品额度默认保持关闭，I-03 未完成。

## 准入与隔离

`Cost` 同时记录 RAM、Disk、Unconfirmed 逻辑字节。一个 Manager 负责总量、固定类别上限、独立恢复储备和有限 claim 槽；不创建等待队列、每 claim goroutine 或历史注册 map。使用 fail-fast `TryAcquire`，超大/超限对象拒绝，不会永久排队。所有维度和控制开销一起检查，失败不留下部分额度。

`TryAcquireWork` 一次取得普通工作类别与 Completion 两份 claim；终结额度在工作接受前已计费，普通工作不能消耗它。HTTP 新工作把信用持有到 handler 返回；deferred worker 在读取持久输入前同时取得 Background 与 Completion，并持有到终结写入退出。其它调用方可用 `ActivateCompletion` 撤销旧信用句柄并把费用交给终结所有者，不重新准入。压力、健康信号过期或关闭准入都不会阻断已申请信用。它不证明 request 已接受或账本已结算。

`WithRAMWorkspace` 在实际回调前取得最多八个固定 IO/Background RAM 槽。`TryAcquireContext` 只借同一 Manager、匹配类别与大小的空闲槽；压力或关闭新准入不重新收费，实际 retained/transferred 子所有者全部退出后才复用。正常完成/取消 join 子所有者，不确定退出保存 `WorkspaceRetirement` 的实际 owner，最后一个物理子所有者退出后才退费。`RetireRAMWorkspace` 允许发布后的阶段在观察者运行前退休，已退出上下文不能继续借用。它不提供 Disk/Unconfirmed、Recovery 权限或跨整个 request 的 Completion 接受。认证 Ledger 已在原生 append 前使用此保护，容量不足不产生原生事实；I-03 仍开放。

恢复只使用 `RecoveryReserve` 与 `RecoveryClaims`，不能借普通工作额度。正常准入从总量及槽数中扣除这些储备；Control 类容量必须覆盖全部 claim 的逻辑元数据。因此恢复不会因为普通工作占尽 claim 槽而失去已配置的储备。恢复单对象也受限，必须按原方案流式/分批枚举，不能一次加载全部 pending。

类别为 Configuration、Active、Cache、IO、Background、Completion、Recovery、Control。外部消费者不能通过普通 TryAcquire 自选 Completion/Recovery/Control 绕过规则；恢复 API 只供受信任恢复路径使用。类别上限可重叠，但共享总量始终检查；不是各模块独立总额度。

## 所有权与资源退还

Permit 是值句柄，普通 copy 仍是同一所有者。`Transfer` 增加固定槽的 generation，原句柄的全部 copies 即失效；不增加 claim/控制开销。`Retain` 显式创建另一消费者，受配置上限和固定 16 槽限制；最后引用释放后才退还资源。共享同一对象可 Retain，复制 payload 必须另外申请费用。

`ReleasePart` 只供唯一、已激活的所有者退还某些维度：清除 RAM 引用后可退 RAM，再把剩余磁盘/确认费用转交持久化所有者。未确认字节须在要求的确认完成后退还；发送成功不等于确认。磁盘字节须在实际空间退还后处理，KV 逻辑删除或事务结束不是文件空间已释放。

`Merge` 把同 manager、class、储备池的两个唯一活跃 owner 聚合成一个；两个旧句柄均失效，数据费用保持，只退还一个 claim 的控制 RAM、槽和 owner。持久文件/确认窗口可以聚合有限工作集，而不是保留每个历史事件的预算 claim。跨类别、共享 claim、尚未激活信用均拒绝；不逃逸类别上限。

最后消费者必须先清除实际资源引用，再调用 Release；旧/已释放句柄的 copies 也须丢弃。每 claim 计入 1KiB 逻辑控制 RAM（含固定槽和 live handle 估算），不是精确 Go allocator/OS 配额。Manager 等固定对象需在 runtime 的固定对象预算中计入。Release/Merge 只退还逻辑费用，GC 滞后及继续持有旧对象仍由外部采样/压力门禁约束。

## 压力边界

初始 Unknown 拒绝普通准入。`NewSampler(dataDir)` 同步只读采样，不创建 goroutine 或修改内核控制：runtime 的 total−released、Linux cgroup v2 当前可见 leaf/祖先的 current/max/high，以及数据目录 statfs 可用字节/inode。不同内存作用域分别判断，不能相加。祖先可能包含 sibling，因此祖先只检查 finite high/max 余量；leaf 同时检查绝对用量。非 Linux、cgroup v1、读数缺失、超长/溢出、迁移/重挂载、取消均保守关闭普通准入。

采样 proc 文本各限 64KiB、scalar 各限 128B、层级固定最多 32 个 GroupMemory；每层三次文件读取，前后验证可见 membership、mountinfo、选中挂载 ID/dev 和目录 dev/inode。ID 不包含无关挂载；同路径替换也重置冷却。可见边界外的 namespace 祖先无法验证，前后核对不是原子内核快照。OS read/statfs 不能强制取消；`At` 在采集开始记录，慢读不会延长采样租约。

`PressurePolicy` 的阈值、有效期、冷却均需显式校准，不提供生产默认。达到进入线立即 Pressured；恢复须全部读数持续达到退出线并经过 Cooldown。未知/过期、采样间断、源替换、时钟回退及重复读数会重置恢复过程。Manager 在 TryAcquire/Snapshot 检查 Healthy 到期，采样停止不会永久放行。压力及未知信号保留独立恢复路径和已申请终结信用。

启动采样循环前只调用一次 `BindController`。完整 `Decision` 私有、accessor 只读，带该 Controller 的递增序号；`ApplyPressure` 拒绝旧序号、重放、其它来源，未知/压力也消费序号，因此延迟的 Healthy 不能覆盖较新关闭决策。Healthy 租约截至原采样起点＋控制器有效期，并受 Manager 的 MaxPressureAge 限制。绑定后 `SetPressure` 无法绕过这些规则；未绑定的手动 API 仅供受信边界/测试。控制器更换或热重绑定协议尚未实现。

`CloseAdmission` 永久停止全部新 claim，包括恢复；只用于终止新准入，不是普通过载处理。压力用 ApplyPressure 收缩普通工作，不能误用 CloseAdmission 阻断新恢复。

本地 btrfs 卷实际 statfs `Files=0/Ffree=0`：无 inode 容量证据，返回 DiskUnavailable，不能解释为已耗尽或无限。已知 Files>0、Ffree=0 才是 inode 停止线。btrfs 等动态 inode 文件系统的安全策略仍需设计/校准；当前不能直接在这些文件系统开启生产准入。Linux tmpfs 上真实触页 64MiB 验证关闭普通工作而保留终结/恢复；这是 fixture 验证，未填满生产磁盘。

`Monitor` 持有固定最新 Sample/Decision，一次运行、ticker 不排队；采样和 transition 日志回调在状态锁外，Snapshot 不等待 OS IO。取消后即使采样仍在同步 OS 调用中，Snapshot 立即 Unknown；owner 返回之前 runtime 的 backgroundWait 不宣称已 join。

`AttachManager` 在 Run 前一次性绑定同一 Controller/采样 owner，两个 Monitor 无法共享一个 Manager authority。每个样本刷新 Manager，而非只在 transition 时刷新；Manager 自身检查 owner context，阻塞采样时取消也立即关闭普通准入。短暂的 Observe→Apply 调度/锁等待导致租约失效时，发布 Unknown、重置冷却并继续采样；authority/序号错误仍停止 owner 并记录异常。压力或停止不退还在途 permit，也不阻断独立恢复和既有终结信用。

可选 `resource_budget` 需要已校准的 `resource_pressure`，各固定类别共享一个 RAM/Disk/未确认字节 Manager。可选 unconfirmed 配置已接 Primary 的 Ledger、Metadata、Audit、Governance 和 provider-object 写前准入；固定 65,536 项 release 表及启动恢复后缀先取 Recovery owner，live append permit 汇成单个 Primary owner，并在确认状态持久化后按前缀分维释放。默认关闭。既有恢复后缀超过配置条数或字节时，仍以有界表/peer window 追赶并关闭新 append 准入，直至后缀重新落入限额且费用补齐；超过固定表硬上限仍 fail closed。Primary 预算/使用/恢复 debt/拒绝/进展指标已接入且无身份标签；完整原生写入拒绝故障矩阵尚未完成。历史文件空间、动态 inode 策略和物理磁盘预留仍不能由此推断。每 HTTP 新工作在读取正文前取得完整 `request_ram_bytes`，并原子保留配置的 Completion RAM/Disk 信用；不信任 Content-Length 或按小正文打折。handler 完全返回后退账，覆盖同步 decode/调用/终结/捕获编码/输出，包括 panic unwind。deferred worker 在解析身份/读取密封输入/标记 in_progress 前原子取得 Background envelope 和 Completion 信用；额度不足保持 queued，由原有 dispatcher backoff 重试，不写终结/调用上游。HTTP、deferred 与 capture 同时受同一总额、control 开销和恢复储备隔离约束。failure capture 在完整编码前另取 Background envelope，涵盖截断前 JSON、截断副本、plaintext、密封 bytes 及队列对象；packet/Store 调用/writer 显式共享 owner，取消返回不提前退还实际 writer 费用。Store.Close 在 vault/目录锁关闭前 join 已接受的 puts 和 sealed writer，实际文件 IO 可能超过 queue 取消时限。

HA 的 Source coordinator 构造逐帧验证和发送窗口每次重建，在读源前取得 Recovery envelope；每 peer 最多持有一个 byte-blocked 候选，重复 ACK/Retry 复用，清除候选引用后退费。队列同步复制仍受 NativeSource revision guard，显式成功/失败 repair、truncate、close 或 lease release 撤销旧候选；不替代热追加前缀的信任边界。单会话预算拒绝不 poison 持久化事实，全局源/状态故障清理全部候选和 lease。HA 配置 replication_candidate_ram_bytes 至少为最大 wire 帧六倍，仍须 native JSON/codec 实测校准；复制 Runtime 的独立发送队列另在构造 channel/复制前预留 replication_queue_ram_bytes；容量含活动 writer record 和固定队列元数据，队列与显式 replication_read_ram_bytes 在 stream 读取前原子合为一个 session Recovery claim，持续到关闭清队列、socket Close 完成且实际 reader/handler、writer 全部退出。入站 wire 借给同步 handler，返回前有效；域事实长期保留需要独立 owner。预算池最多三个 live/retiring session，不因 Close 释放同步等待者或写出而提前复用槽/退费；未启用预算的 ConnectionManager 也限 configured peers+1 authenticated session，control-only 同样占槽。Send/Close 串行队列生命周期，网络写不持该锁；Close 不等待 generation handler，避免 registry 退役自等待。Recovery 类、储备和专用槽须容纳三个候选/重建、三个 session queue/read owner 和一份 Source 固定读者预留（七个 claim），还需 claim 控制费用；Queue envelope 下限不替代 allocator 校准。TLS 内部/握手、其余原生 reader/实际 IO 费用与进展时限、PendingFrom 页及其重建、前置 reconcile 和 live RecordDurable 编码未包含，额外重建槽只是容量预留。

NativeSource 固定读者另在 owned key copy/默认游标构造前申请一份 replication_reader_ram_bytes Recovery lifetime envelope。聚合默认与两个 peer 三组，覆盖各组固定游标/密钥及可能保留的 Ledger 解压流/descriptor 元数据；空闲/重连保持同份预留，Close join 全部读取/同步发送、关流/清引用后再退费，旧 lease release 不影响替换组。输出和临时 decoder 仍需 candidate/read owner；全量 Cursor/reconcile/域投影不因此有界。Recovery 总量为 3*(candidate+queue+read)+reader，加七个 claim 控制费用；下限须实测校准，不保证 allocator 或 IO 进展时限。[读者预算验证](../../docs/verification/performance/2026-10-02-native-reader-budget/README.md)。

Provider object 的最终分块读取、已有 live 对象重传与恢复 source spool 已改为固定 64KiB 流式 SHA/复制，不再另建整文件 ReadFile 副本；摘要认证在 fsync/发布前完成，metadata 长度和命名 identity 要验证。其临时费用仍依赖上述 candidate/read envelope 或原调用 owner；原始上传 sealed slice、其它长期 reader/IO 与磁盘/确认 owner 未迁移。完整认证 IO 与对象大小仍线性增长，不声称已经具备进展时限。部分 spool crash tail 在已授权重传边界只修当前未完整 chunk；已有 live 对象重试重复文件/目录屏障，不以名称存在当作持久化成功。[流式对象验证](../../docs/verification/performance/2026-10-02-provider-object-stream/README.md)。

RAM envelope 是经峰值校准的逻辑估算，不能拦截实际 allocator，也不能自动计入持久域状态或跨 HTTP 的独立副本。failure capture 的显式 capture_ram_bytes 至少覆盖 request_ram_bytes、三个保留侧字节下限，并需要至少 3 个 owner；这些下限不替代真实峰值校准。deferred 的全量 pending 枚举尚须迁移有界 cursor。当前不会声称配置总额覆盖所有消费者，或已经保证生产终结的 RAM/磁盘空间。

可选 `resource_pressure` 配置全部显式校准，默认 nil。Standalone、Primary 和 Replica 使用各自 runtime 的单一监控；Standalone/Primary readiness 与 15 个新工作路由读取同一状态，在正文读取前拒绝。已有工作查询/取消/删除/close/outcome 及已通过门禁的请求不重新准入；Replica readiness 仍是复制候选语义，peer/恢复未安装业务压力门禁。SIGHUP 不热替换策略，需重启。

固定 state/reason 指标和 transition 日志暴露未知/压力；未取得、过期、时钟无效、取消或停止的物理读数不按零或当前值发布。两份配置参考的零值为无效占位，不能照抄启用。

尚未实现：其余真实消费者接线、生产峰值/突增余量校准、启动现有磁盘/未确认占用对账、动态 inode 文件系统策略、文件增长聚合 owner、实际 ENOSPC 与 HA 确认。逻辑预算不拦截 Go 分配、不预分配文件系统块，也不替代认证/账务事实验证。采样不强制 GC，退逻辑费用不证明 OS 已回收。

## 接入规则

1. 请求体读入、解码和上游调用前取得足够的工作/终结费用，估算包含副本、重试、流式输出和编码放大。
2. 可靠结束前保留终结 owner；持久化失败或确认未完成时转交有界恢复所有者，不能因 HTTP 返回就 Release。
3. RAM、磁盘和未确认窗口按真实生命周期分维度退还；唯一日志/备份不得为了退预算删除。
4. 新增磁盘/确认费用在实际写入前取得；持久所有者聚合已有费用。重启须从权威状态和实际文件空间重建占用，不能以空 Manager 认定已有空间可用。
5. 接入配置、指标和用户可见拒绝语义；目标环境校准额度、采样有效期和控制开销。多个 subsystem 必须共用 Manager。

[逻辑预算基础历史证据](../../docs/verification/performance/2026-10-02-resource-budget/README.md)、[压力采样历史验证](../../docs/verification/performance/2026-10-02-resource-pressure/README.md) 与 [runtime 接入验证](../../docs/verification/performance/2026-10-02-resource-runtime/README.md) 与 [failure capture 所有权验证](../../docs/verification/performance/2026-10-02-resource-capture/README.md)。这些规则仍须在各实际调用路径和故障矩阵中证明；局部门禁通过不等于 I-03 完成。
