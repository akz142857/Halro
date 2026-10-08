# memory-state 合成实验工具

为方案 D-02/D-04 提供可复现的本地基线。工具不读取 Halro 配置、生产目录或 provider；结果不能替代生产取证、正确性故障矩阵或容量接受。

## 构建与运行

从仓库根目录执行；每个样本启动新进程，不复用 Go heap：

```sh
mkdir -p /private/tmp/halro-memory-tools
GOCACHE=/tmp/halro-go-cache go build -o /private/tmp/halro-memory-tools/memory-state ./tools/memory-state
/private/tmp/halro-memory-tools/memory-state -mode ledger-usage -fixture priced -requests 100000
/private/tmp/halro-memory-tools/memory-state -mode usage-snapshot -fixture priced -requests 100000
/private/tmp/halro-memory-tools/memory-state -mode ledger -fixture legacy -requests 10000
GOCACHE=/tmp/halro-go-cache go test -count=1 ./tools/memory-state -run '^$' -bench BenchmarkUsageWatermark -benchmem -benchtime=100x
```

可通过 `-ldflags '-X main.sourceCommit=<实际 HEAD>'` 记录构建基线；JSON 同时记录工作树是否修改。正式保存证据时另外记录源文件指纹、二进制 SHA256、Go/OS/架构、运行限制与完整命令。未设置 `GOGC/GOMEMLIMIT` 时 JSON 对应字段为空，使用 Go 默认值。

## 状态 fixture 与测量含义

`request-lifecycle-v1` 每个请求顺序生成 accepted、reserved、started、settled、finalized 五条事件，逐请求应用，不先创建完整历史数组。`legacy` 采用无价格快照的历史事件；`priced` 固定一个部署的 metered 价格、独立克隆的价格证据，7 input/3 output tokens，每次最终费用 13 微美元。请求 ID 独立，期间从接受时间导出。该 fixture 仅覆盖成功、单 attempt 生命周期；不含并发调用、fallback、Run/WorkUnit、高 pending、治理、价格历史、真实 Ledger WAL 或 Usage checkpoint/导出。

- `ledger` 保持当前 Ledger State 存活；`ledger-usage` 同时保持 Usage Aggregate 存活。
- `usage-snapshot` 在相同活跃引用上再保留一份全量 Snapshot；`snapshot_live_heap_delta_bytes` 为其 GC 后额外 heap。
- `baseline_after_gc` 是建状态前，`final_after_gc` 是状态仍被引用时；差值只表示这套 fixture 的存活 heap，不表示 RSS/cgroup 已释放。
- struct 大小不包含指针引用、字符串、slice backing array、map 和价格证据。
- `-heap-profile <新路径>` 仅支持状态模式，0600 创建并拒绝覆盖；profile 保存在仓库外。layout 模式拒绝该参数，避免把关闭 DB 后的 profile 当作存活工作集。

20 ms 采样的 `sampled_peak` 各字段是各自的最大值，可能发生在不同时间，也可能漏掉短峰值；这不是连续时间曲线。Linux RSS 来自 `/proc/self/status`。明确指定 `-cgroup-dir /sys/fs/cgroup` 时，读取 cgroup v2 memory.current/peak/max/events；`memory.peak` 是该 cgroup 生命周期的硬高水位，需每个样本创建新 cgroup/container。统计包含该 cgroup 的页缓存等内存，不只 Go heap。工具不修改 GC、cgroup 或生产限制。

## 存储布局微基准

```sh
mkdir -p /private/tmp/halro-layout-evidence
/private/tmp/halro-memory-tools/memory-state -mode layout-same -requests 10000 -batch 100 -metadata-writes 1000 -payload-bytes 1024 -dir /private/tmp/halro-layout-evidence
/private/tmp/halro-memory-tools/memory-state -mode layout-separate -requests 10000 -batch 100 -metadata-writes 1000 -payload-bytes 1024 -dir /private/tmp/halro-layout-evidence
```

使用仓库现有 bbolt 版本和 map freelist；未关闭同步提交。两个 writer 同时开始：一方更新 16 个 metadata slot，另一方按 batch 写 history 值、SHA256 digest 和事务内 sequence。同库共享一个写锁，独立库使用两个写锁。该微基准没有权威 WAL、跨库原子提交、实际配置/价格操作或 HA 确认，所以仅比较此工作负载下的竞争与存储形状。

工具在指定的现存父目录下创建独占临时子目录，只清理该子目录。写入后关闭并重开全部 DB，逐项验证 sequence、数量、value digest、水位，以及 metadata 各 slot 的最终值；失败返回非零退出码。报告原始 bbolt stats、文件总大小、重开和完整扫描时间。`p95_upper_bound_nanos/p99_upper_bound_nanos` 是固定 log2 histogram 的桶上界，并非精确分位数；原始桶保留。

## Standalone 有界投影冷构造样本

`bounded-projections` 走真实的 native 写入和磁盘投影 API：多个独立请求 writer 各自保持五事件顺序并发追加到认证 Ledger；随后固定 native head，用 `BuildProjectionFromCursorWithLimit` 对全历史做认证并新建受限 Ledger projection；最后从同一 native history 认证重放，构建受限 `DiskProjection` Usage derivative。输出核对 native/Ledger/Usage 水位、成功请求/attempt 数与费用，并报告三个数据文件尺寸、native group commit 统计及内存采样。此模式不构造 `ledger.State` 或 `usage.Aggregate`，所有 DB 保持打开直到最终 GC/采样结束。

冷启动的 Usage 归约使用 `ApplyReplayBatchContext`：每批最多 512 条、JSON 编码不超过 8 MiB，在一笔 bbolt 写事务中应用；逐条仍从固定 reader 验证位置、finalization 和归属证据。Runtime Standalone、Primary、固定 cut reference rebuild 与此 harness 使用相同行数/字节上限；在线 Collector 仍逐条应用。Ledger 私有冷投影同样最多 512 条/8 MiB；公开 `ApplyBatch` 的在线/短屏障仍保持 64 条上限。任一行验证失败，整批不前进水位。

运行必须给出新建临时目录的父目录，以及 native 临时 WAL、Ledger 和 Usage 三个正数上限；derived bbolt 文件由 `Options.MaxSize` 限制。Native safety ceiling 由工具每 64 个请求检查一次，单个 fixture 最多有 4 个 append worker，因此只用于限制实验临时数据规模，不是 native writer 的硬分配门禁。实验结束只删除本次 `MkdirTemp` 创建的子目录。

```sh
mkdir -p /private/tmp/halro-bounded-evidence
/private/tmp/halro-memory-tools/memory-state -mode bounded-projections -fixture priced -requests 1000 -workers 4 -native-max-bytes 67108864 -ledger-max-bytes 268435456 -usage-max-bytes 268435456 -dir /private/tmp/halro-bounded-evidence
```

该样本覆盖 Standalone 单 generation 的完整认证冷构造与两个 fresh 派生 owner，不覆盖 roll/segment、治理、HA 角色 cut/确认、恢复中断、Runtime 其余消费者或可信 saved-state 启动。要比较历史规模，应在同一平台、镜像、cgroup、GOMAXPROCS 和三个 ceiling 下分别重启容器运行；Darwin 数据不能替代 Linux 结果。它也不能单独满足目标生产容量验收。

同一 Linux/arm64、2 GiB cgroup、4 CPU、GOMAXPROCS=4、1000 请求/5000 事件、64 writer 的前后单次对照见实验 README：批量归约后 Usage 投影阶段从约 5.91s 降至 1.04s，整条冷构造从约 6.82s 降至 2.08s；两次水位、请求数、attempt 数和费用相同。样本数只有一对，不能据此宣称稳定性能倍率或容量验收。

`layout.open_databases_after_scan_gc` 在 DB 重开且全扫描之后、关闭之前测量，是存活 DB 的工作集。外层 `final_after_gc/live_heap_delta_bytes` 是 DB 关闭后的状态，JSON 的 `final_measurement_phase` 明确区分；不得用其选择布局。全扫描故意触及映射页，不能把只测 Open 的低 RSS 当作大库随机读取的容量证明。

布局决策还需补齐预填充/churn、长 reader、随机读、更多 batch/并发点、实际锁序、备份及 HA/schema 生命周期。在目标 Linux 磁盘和实际 fsync 条件下复测；本机 OrbStack bind mount 结果不能推导生产 NVMe 延迟。D-04 的 100 万条历史、持续流量/曲线、硬门槛与故障接受仍未完成。

## 最近证据

见 [2026-10-02 实验记录](../../docs/verification/performance/2026-10-02-memory-state/README.md)。

## D-03 隔离认证页原型

[authroot](authroot/README.md) 包提供实验页编解码、认证根、点查/范围与空根测试；不是生产 Seal 或容量证明，不由上述 CLI 模式调用。

D-02最新[混合churn证据](../../docs/verification/performance/2026-10-04-layout-mixed-churn/README.md)来自Store包新增 BenchmarkMemoryLayoutMixedChurn：同库/独立库、真实metadata journal、原子delete/append及有限随机点查。每项三次Linux volume样本，进程IO与两库合计PageAlloc不互换为设备写放大；两布局100000行cgroup峰值均约223MiB，物理容量仍需独立验收。

D-03 的 [认证 Ledger 类型化点读证据](../../docs/verification/performance/2026-10-04-authenticated-point-read/README.md)补齐同 pin 业务查询和实际 Metadata price-pin reader 兼容性；64MiB整次解码/验证费用与32MiB查询费用分开记账。默认Runtime尚未切换，saved-state启动及物理容量仍未验收。

D-03 [容量与同cut账务查询](../../docs/verification/performance/2026-10-04-authenticated-accounting-read/README.md)覆盖认证候选的capacity/WorkUnit/pending三个reader契约，固定逐行认证index及同根source join；32个900KiB实际Run允许完整返回。逻辑workspace/有限返回和物理容量分别验收，项目active/open计数IO仍随选中数量增长。

D-03 [完整准入reader证据](../../docs/verification/performance/2026-10-04-authenticated-admission-reader/README.md)补齐live认证候选的列表/maturity/cohort方法集：固定pin与有限range/Floor、原生推进期间旧cut稳定，cohort关闭后的visitor费用仍持有到回调退出。完整reader不等于Runtime MutationOwner或saved-root授权，物理容量仍未接受。

D-03 [真实账务写入适配器](../../docs/verification/performance/2026-10-04-authenticated-accounting-writer/README.md)提供显式Manager构造与同owner原生事实更新：独占维护下绑定source path/key和完整head/hash，单槽等待真实读取/writer退出且保留费用，更新先于observer与必要ACK。默认Runtime、两级恢复接线、可信增量启动及生产接受仍开放。

D-03 [认证两级恢复](../../docs/verification/performance/2026-10-04-authenticated-two-level-recovery/README.md)从固定原始request/attempt输入生成原生终结事实，当前认证settlement证明解除，保留既有终结和legacy真实origin；App显式适配器复用role/term/full ordering确认与撤权。真实mTLS Primary/Replica及fresh中断重试已有本地验证，默认backend/整job Recovery储备接线、Seal/增量初始化及生产接受仍开放。

D-03 [认证恢复整job owner](../../docs/verification/performance/2026-10-04-authenticated-recovery-resource-owner/README.md)验证同Runtime资源池/共享零队列槽、整job Recovery workspace、native实际fsync退出前取消不退费、fresh retry及持久Disk直到Discard退账。注入的同池RAM+Disk仅为测试；生产配置Disk=0和认证normal类在Unknown/Pressured不可用均保留为显式阻断，不据此切默认backend。

D-03 后续[限定生命周期的 Recovery RAM 授权](../../docs/verification/performance/2026-10-04-recovery-scope/README.md)已使显式认证维护 job 在 Unknown/Pressured 使用同 Runtime 池的 Recovery 临时费用，join 最终子 owner 后退账；普通业务、持久 Disk/Unconfirmed 和逃逸 context 不获得授权。Disk=0、冷构造及默认启动/增量恢复仍待实施与接受，先前整job报告记录的是该接线前状态。

I-11 已开始[认证 Ledger 组件重开](../../docs/verification/performance/2026-10-04-authenticated-reopen/README.md)：真实跨进程/新Manager持久owner、独立native/许可边界及认证保存position、同reducer只应用delta、关闭join与真实后继删除receipt接管。全native审计仍解码历史；端到端增量Open、完整Seal/Metadata基线/默认Runtime/物理及生产接受仍未完成，M3未启用。

后续已新增[认证备份 Metadata 内容基线](../../docs/verification/performance/2026-10-04-metadata-backup-baseline/README.md)，通过显式库接口从完整验证的加密备份构造全分类内容根，不接受 live DB 来源。内容来源路径的进展不替代 journal epoch/header/trim/resume、整文件轮换/发布、完整 Seal、默认 Runtime 和生产接受；本工具 CLI 未自动接受基线。

再后续已有[Metadata epoch/file revision 关联](../../docs/verification/performance/2026-10-04-metadata-epoch-binding/README.md)：私有根 bookkeeping 原子归一、真实 root 与 format2 header、trim 保留完整 binding/revision 和固定缓冲。当前只接显式库接口，普通和旧 HA reader 拒绝 format2。域 delta、trim cut root/resume、完整发布/资源/角色接受仍待完成，CLI/默认 Runtime 未切换。

后续的[Metadata 认证范围删除](../../docs/verification/performance/2026-10-04-metadata-range-delete/README.md)补齐结构性桶删除原语；完整 native reducer/单帧接受契约尚未接入，CLI/默认 Runtime 未切换。

再后续[Metadata 整帧 delta](../../docs/verification/performance/2026-10-04-metadata-frame-delta/README.md)已接显式库接口，1024-op 整帧原子归约、先准备后 native durable/root publish 和全源认证下显式前缀恢复已有本地证据。CLI/默认 Runtime 与生产格式迁移/完整 Seal/角色选择仍未切换。


后续[Metadata 独立保存根重开](../../docs/verification/performance/2026-10-04-metadata-reopen/README.md)新增实际跨进程新 Manager 验证：独立保存完整 root identity、原生完整/保存/允许 head 与 backup/epoch binding，完整来源与排除尾部资格检查在候选归约之前；不匹配完整根先于 orphan repair 拒绝，partial native tail 保留。只归约许可 delta，native 已落盘而根未发布可补应用且重试不重复；同 Manager 两份持久费用借用、整个 factory IO 费用与真实关闭、成功后继清理 receipt 和失败后继拒绝已有本地证据。当前只接受未 trim epoch/revision1；trim cut 根/resume、完整 Seal/角色-term/密钥资格、持久 inventory/生产接入、所有原生日志增量初始化、历史审计和 Linux/生产接受仍开放。I-11 保持进行中，21 项未闭环，M3 未启用。


后续[Metadata trim cut-state 与物理 revision 关联](../../docs/verification/performance/2026-10-04-metadata-trim-state/README.md)已补齐：只裁已应用认证根的前缀，完整 native/域认证后将原 root identity、旧 frame start/end/hash/revision 绑定到新 anchor；内容根保持不变，保留未应用尾部，新 revision 的后续 delta 使用新物理位置。完整根与实际 anchor/独立 TrimState 一致才可重开，缺关联和落后于已删前缀拒绝；连续 trim/全裁后追加、实际 publisher 子进程退出后新 Manager 重开、真实 replacement 创建 receipt/成功后继接管、post-rename 取消与panic及实际Close join/整rewrite Disk费用已有本地证据。完整 Seal/角色-term/密钥资格、持久 inventory和跨进程生产删除、整文件发布/格式迁移、所有原生日志增量初始化/历史审计、完整故障与Linux/生产接受仍开放。I-11保持进行中，21项未闭环，M3未启用。

## 正确性与故障矩阵

`verify_fault_matrix.py` 对照 `docs/contracts/memory-bounded-fault-matrix.json`，逐包运行 §9.1 的具名本地夹具，不调用真实计费 provider。输出目录必须不存在。

```bash
python3 tools/memory-state/verify_fault_matrix.py --output /tmp/halro-memory-matrix-new-run --race --require-complete
python3 -m unittest discover -s tools/memory-state -p test_verify_fault_matrix.py
```

记录完整 Go JSON 流、命令、退出码、环境和当前 cmd/internal/tools Go 源码前后 SHA256；源码变化、测试未执行、skip、包失败或超时都不能得到通过。结果区分 `verified`、`partial` 与 `failed`；`--require-complete` 在已实现测试通过但有缺口时返回 2，失败返回 1，完整验证才返回 0。不得删除 remaining 字段来让未实现路径通过。该矩阵不替代 Linux 容量曲线或生产接受。

## 独占 host backing 的 ext4 容量 runner

`ext4_runtime_capacity_linux.py` 默认仍使用 disposable container 内的 backing file。Docker writable layer 空间不足时，可显式选择 host `/tmp` 下的新文件；工具不接受已有镜像或设备，不挂载整个 host 目录。

```sh
python3 tools/memory-state/ext4_runtime_capacity_linux.py \
  --binary /tmp/frozen-runtime-capacity-linux.test --output /tmp/new-ext4-evidence \
  --requests 100000 --host-backing-directory /tmp --image-mib 32768
```

Host 模式必须实际 DIO=1。创建前核对真实 host 可用空间至少为镜像大小加 256MiB；使用独占临时目录、`O_EXCL` 文件、物理预分配、原 FD/父目录/inode 核验及 `st_blocks * 512 >= size`，随后通过已验证 loop 设备格式化，避免 shared-file zero-range 或 discard 打洞。证据保留 host filesystem 与真实 ext4 两层空间、原始 Docker 命令、实际 loop、cgroup 与失败日志。seed 与 fresh restart 仍使用相同 2GiB cgroup、1.5GiB peak 门槛、真实 OS sampler、4h 每阶段上限；缩小 `--image-mib` 不改变这些门槛。

清理分两阶段。内层 runner 持有实际 backing 与 loop FD，核对真实 backing dev/inode、loop device、卸载及 detach，关闭 reader 后只交付待完成的 join。外层始终保留 host 原 creator FD，确认原进程退出并移除自己的 stopped container，再用独立新容器按实际 backing dev/inode 查询所有 loop；相同文本路径的 foreign loop 不授予权限，也不会被 detach。只有实际引用清零、独立 reader 关闭，且 host 原 creator 核验与 Close 成功，工具才删除自己的文件。缺少或未知 cleanup、foreign 替换、预分配失败会保留 backing owner 与 `host-backing.json`；`docker rm` 和普通进程退出不授予删除资格。

```sh
PYTHONPYCACHEPREFIX=/tmp/halro-capacity-python-cache python3 -m unittest discover \
  -s tools/memory-state -p test_host_backing.py
PYTHONPYCACHEPREFIX=/tmp/halro-capacity-python-cache python3 -m unittest discover \
  -s tools/memory-state -p test_ext4_runtime_capacity_cli.py
HALRO_HOST_BACKING_INTEGRATION=1 PYTHONPYCACHEPREFIX=/tmp/halro-capacity-python-cache \
  python3 -m unittest discover -s tools/memory-state -p test_ext4_host_fixture.py
```

最后一项只创建独占 512MiB 镜像，在实际 2GiB cgroup 内通过两个新 shell 进程校验少量 fixture IO、DIO、ext4 与退休；不能用它声称 Runtime 或 100k/1M 容量通过。
