# 内存有界状态：存储布局与认证快照决策记录

2026-10-07 集成复核：已修复启动时 Usage 与历史审计的 native 读取竞争，连续 12 次启动验证及 App 等七个受影响核心包的完整检查通过，物理用例另经实际 ext4 验证。**固定 2 GiB HA 配置仍在空库启动的 Governance 原始 completion-pool 申请被拒绝：normal 剩余 46.824 MiB，本笔需要 128.346 MiB，最低缺口 81.521 MiB，尚不含后续组件。**十万冷迁移失败保持；完整 `go test ./...` 的原失败记录也保留，包级复核不冒充全仓库绿色或目标接受。公共 HA 全物理 guard 未打开，原 19 项条件未关闭。具体修复、逐包结果、冻结输入和实际资源退休见[集成裁定](../verification/performance/2026-10-07-memory-state-authentication-reuse/integration-decision.zh-CN.md)。

2026-10-07 最新：分页认证复用三组中位数 16.811 秒，同期 v8 12.372 秒，仍慢 35.88%；现有 512B v8 的单项认证复用三组中位数 9.930 秒，同期 v8 11.539 秒，改善 13.95%，未达 30%。两个候选均拒绝启用、不扩规模。原 Roll 失败已复核为夹具先发布完成目录再人为回退的错误，合法三布局修正后通过，已提交三布局拒绝及定向 App race 通过。主方案已分离历史与当前状态，19 项原条件保持，见[本轮完整记录](../verification/performance/2026-10-07-memory-state-authentication-reuse/README.zh-CN.md)。下述各轮保持当时源码边界。
状态：2026-10-07，本地实施选择已固定为各组件独立认证 COW 根与共同 Seal；D-02/D-03 的完整角色、容量和生产接受仍进行中。后续段落保留各轮候选实验的当时结论，不覆盖本节当前实施选择。D-03 展开见 [认证根候选草案](memory-state-authenticated-root-protocol.zh-CN.md)，隔离页原型验证见 [本地记录](../verification/performance/2026-10-02-memory-state-authroot/README.md)。主方案见 [内存有界状态设计](memory-bounded-state-design.zh-CN.md)，原始实验见 [本地实验记录](../verification/performance/2026-10-02-memory-state/README.md) 与 [真实 journal/churn 实验](../verification/performance/2026-10-02-memory-state-churn/README.md)。

## 2026-10-07 成本复核后的执行决定

[本轮真实成本裁定](memory-state-cost-decision.zh-CN.md)保留各组件认证根、共同 Seal 与增量恢复作为运行/重开候选；**fresh scratch 冷构建仍未取得目标历史规模的成本接受**。一千请求分段诊断确认 Usage 与重复 Ledger 构建是优先成本问题，复合键优化的组件加速不足以批准整条迁移。下一轮先调整私有归约布局或重复构建，在相同账务/认证/费用职责下通过分级门槛，再扩展容量与剩余角色。此决定不将裸 bbolt、跳过 Sync/认证或放宽 HA guard 作为捷径；下文历史“继续实现”不构成当前成本批准。

后续已纳入同预算的有限批内归约：一千请求三组迁移中位数 57.802 → 38.732 秒，改善 32.99%；512 槽 / 4 KiB 行上限，宽行仍走原认证树。该候选一万 300 秒门禁失败。随后在原 64 MiB scratch 费用内将固定块 bank 从 2 调至 32 MiB，新三组一千中位数 54.948 → 26.549 秒（51.68%），同一万 300 秒复测再次失败，未进入激活/重开；峰值约 236 MiB，观测可用磁盘仍超过 3.7 GB。本轮不批准扩规模，缓存调参收口，先减少同一备份 job 的重复参考，再评估冷归约布局。目标历史规模、业务吞吐和全部物理模式仍未接受；失败及后续结果见成本裁定入口。

随后已实现同一 bounded 备份/Restore job 的一次私有 Ledger 参考复用，保留独立源、完整价格/Usage 验证与实际退出义务。新三组原始 baseline/整体候选中位数 47.141 → 20.407 秒（56.71%）；一万相同 300 秒门槛仍失败于 activation Ledger，备份两个构建已完成，目标成本接受仍未成立；下一轮转入私有冷归约布局原型。详见[参考复用证据](../verification/performance/2026-10-07-memory-state-reference-reuse/README.zh-CN.md)。

此前有限排序批认证 treap union 使三组一千中位数 **47.478 → 11.569 秒**，一万首次在 **295.347 秒**内迁移并重开/对账。随后同一冻结 v8 在 **28 GiB** 完全分配 backing、实际空间资格通过后，十万仍未通过原 **1800 秒**门槛：首段 backup Ledger **1783.513 秒**后超时，实际读写约 **208.50 GB /72.30 GB**，没有进入激活或重开。最高观测 cgroup peak **776.1 MiB**，空间充足、无 ENOSPC 日志，容器非 OOM 退出；完整自有 backing 已真实退休。当前私有冷归约布局不获十万接受，原失败与一万单次通过均保留。见[十万证据](../verification/performance/2026-10-07-memory-state-100k/README.zh-CN.md)。

后续已实现独立fresh分页归约包，4KiB/11条目固定页，保留完整键集合/祖先范围/子hash-count认证与实际FD/Sync/owner义务；仅在独立3077输入冻结候选中接既有领域归约。三个领域23项定向、最终分页包完整race与vet通过；App备份用例通过，但补充中断Roll与未修改v8同样失败，均保留。三组全新一千整体中位数 **12.561 → 23.386秒**，慢 **86.17%**，读写及分配均增加；六轮对账、重开与实际退休通过不改变性能拒绝。**直接分页替换原型不选用，生产调用保持v8，不升级一万/十万。**逐行Flush与4KiB blob共同变化，不能将全部差异归因于高扇出；下一项先隔离固定批内共享认证路径和紧凑blob成本，不调大缓存/时限，也不改变最终七根。见[分页原型完整证据](../verification/performance/2026-10-07-memory-state-paged-reducer/README.zh-CN.md)。

最新因素隔离已执行：4KiB节点保留，共享完整原值/缺失校验与512B blob作为两个开关单独及组合比较；五版本各三次完整一千生命周期，本轮v8 **11.446秒**、组合 **17.749秒**，仍慢 **55.06%**。两个单开关候选也不合格，三种新候选均不接生产、不升级规模；完整包/race/vet、领域定向和部分App备份用例通过不改变成本拒绝，既有Roll失败继续开放。15次激活/精确对账/重开与实际自有资源退休均完成，峰值215.44MiB、Open≤0.331秒。独立CPU诊断另已完成，原19项开放条件保持；下一项先裁定事务内重复页认证复用的安全职责与成本，批量修改作为第二备选，本轮尚无认证receipt实现或接受。详见[本轮证据](../verification/performance/2026-10-07-memory-state-reducer-factors/README.zh-CN.md)及[下一轮计划](../verification/performance/2026-10-07-memory-state-reducer-factors/next-round.zh-CN.md)。文件字节上限仍不等于物理预留，原费用、实际FD/Sync/owner和最终七根均保持。

## 当前实施选择（2026-10-07）

根据用户授权自主决定开发方案，继续实现以下固定布局。它给开发、故障验证与迁移提供具体协议；容量门槛和最终生产格式接受仍按主方案执行。

| 部分 | 选择 | 跨进程信任与生命周期 |
| --- | --- | --- |
| 权威事实 | Ledger、Metadata journal、Governance、Audit、HA Ordering 保持原生事实源 | native 完整认证、独立身份/密钥、Standalone 或真实 MemberState/ordering 许可前缀决定权限 |
| 派生业务状态 | 各组件独立认证 COW 页树 | Ledger、Metadata、Governance projection、Usage、Ledger Directory，以及 Governance/Audit native cache 共七根；查询认证键集合、值及范围，不从普通 DB 内容授信 |
| 跨组件发布 | 有界 common Seal v2 | 实际 source freeze 和各 owner 的确切位置/完整 root commitment 共同发布；不假设跨库事务，未发布后继页保留并从旧公共根恢复 |
| 普通启动 | 显式迁移后选择完整七根及许可 native delta | 元数据/目录逐页，业务关键读取逐路径认证；完整旧字节后台 audit 失败撤销同一 generation。HA 完整七根接入仍在实现 |
| 旧库与冷重建 | 旧 bbolt 只作认证基线输入；fresh 私有 scratch 作归约/格式转换 | 旧 DB 不能直接作为重开权限；所有 mapping、workspace、temporary files 与 Close/retirement 必须收费。百万历史的 cold scratch 资源接受仍开放 |
| 回收 | common selection + captured inode + 全 reader join | 固定候选/opaque 槽、整文件 ceiling 和实际 inventory；未知 Close/发布屏障保持费用，不能以路径不存在或逻辑计数推断物理退款 |

独立组件 writer 避免把历史派生更新放进 Metadata 权威事务。各组件位置只在对应 native 持久化与既有确认屏障后推进；账务与 Usage 的共同发布需要真实排空及 maintenance lease。认证树的 immutable 页与有限工作集用于最终增量读取；bbolt 保留在未迁移兼容路径及 fresh 私有转换中，后者不据此取得跨进程权限。

已实现的 Standalone、HA Metadata 独立来源/安装与多页维护验证见 [Runtime 维护契约](../contracts/memory-bounded-maintenance.zh-CN.md) 和 [所选维护资格证据](../verification/performance/2026-10-06-memory-bounded-resume/selected-maintenance-qualification/README.zh-CN.md)。这些局部结果不能代替真实目标 Linux 的百万历史、并发 HTTP、多个维护周期及完整 HA 角色转换接受；D-02/D-03/D-04 的剩余门禁继续保留。

## 1. 已知条件

2026-10-03 增加[UTC日期范围的精确归档校验](../contracts/usage-archive-dates.md)及实际 `verify --retain-from` 离线入口：完整原生前缀不因过期谓词跳过，日期内必需行与仍列出的旧行均精确匹配；分区日期/路径绑定实际完成日期。Standalone/Primary durable/Replica applied继续使用独立认证fresh参考和最终物理清理屏障。它提供后续日期保留的对账前置条件，尚未改变根格式或实现有限unlink/pin/默认Runtime，也不批准D-02/D-03/D-04；最终受影响门禁见[日期校验证据](../verification/performance/2026-10-03-usage-archive-dates/README.md)。

2026-10-03 后续实现增加[有限归档发布的窗口权限延续](../contracts/usage-window-authority.md)：固定的同owner原生参考权限与publisher实际根摘要/文件身份绑定，全部发布屏障和物理清理完成后才延续；rename后失败保留一份不可用于清理的pending凭据，精确匹配并重复屏障后才能恢复。缺失根从完整原生零位置开始；未知根、范围校验与重开不继承。定向race已验证真实trim后续传、独立全native行集对账、清理路径替换及实际Close join，最终受影响门禁见[发布验证](../verification/performance/2026-10-03-usage-window-publication/README.md)。它不批准默认Runtime切换、日期coverage/物理回收或D-02/D-03/D-04，顶层任务状态不变。

- Ledger/metadata journal/治理日志继续保存权威事实；派生库不可成为新的事实来源。
- 当前工作树 metadata 使用 bbolt schema 45（2026-10-04 复核），其写事务入口包含日志记录、分类及确认契约。新增派生 buckets 要显式分类为 node-derived，不能经普通原生 Update 绕过权威写入协议。见 `internal/store/bolt/store.go`、`journal_class.go`、`journal_tx.go`。
- 默认 Standalone/Primary/Replica Ledger 和 Usage 已选择 fresh 磁盘 owner；Standalone/Primary Governance 也已选择 fresh 磁盘 Manager。Primary 在恢复和真实排序确认完成后发布 ready，Replica 使用角色允许的 applied cut。详见主方案 §8 的对应证据；这些是过渡的完整认证重建路径，可信快照增量启动、共享物理费用、完整迁移和容量接受仍未完成。I-02 单独只减少 watermark 读路径峰值。
- 本地 Linux 容器的 priced 生命周期 fixture 从 1 万到 10 万请求保持线性 heap 增长。布局微基准证明同库两个 writer 存在竞争，并显示小 heap 不能说明低 cgroup。具体数值、限制与重复样本在实验记录中。
- 当前 Ledger Open 扫描活跃文件、加载 segment 目录；部分原生日志 Open 仍扫描完整内容。只减少 State Replay 不能独立满足最终增量启动目标。

本节上述列表按 2026-10-04 工作树更新。后文各轮局部实现记录保留其当时的证据切面；其中“默认 Runtime 尚未切换”等描述不能作为当前选择路径的结论。

## 2. D-02 候选与当前判断

| 维度 | 同 metadata DB | 独立派生 DB |
| --- | --- | --- |
| 事务 | 同一投影批的行与水位可原子提交；权威 metadata 写事务仍遵守既有协议 | 同一派生库可原子提交行与水位；不得假设跨库原子性 |
| 竞争 | 历史投影、配置与价格共用 writer；长 reader 也影响同一库维护 | 隔离 writer，但磁盘、CPU、mmap/page cache 仍共享 |
| HA/schema | 新 buckets 分类及 exact-schema 准备进入 metadata 生命周期 | 派生 schema 独立，但本节点重建、generation/ordering 边界必须显式管理 |
| 备份 | 一份 DB 的一致事务视图；日志位置与其他 store 一致性仍须协议 | 每库记录位置与 generation；只能在认证共同前缀恢复，不依赖文件复制时间相同 |
| 空间与维护 | 历史数据会改变 metadata DB 大小、freelist 和迁移成本 | 可隔离派生库重建、替换与分区，仍须计入多 generation 的最坏空间 |

目前优先验证“独立派生 DB”候选，以减少历史投影对权威 metadata writer 的干扰；这不是最终选择。每批继续遵循权威日志已持久化 → 派生事务提交 → 既有按序应用/确认屏障，不为隔离 writer 引入无界异步队列。价格 pin、Usage rollup 等跨库读取/更新必须列出恢复规则，不能靠锁覆盖跨库崩溃。

真实 journal 与预填充覆盖 churn 实验已接入；持有长 reader 的部分样本中出现接近 300 ms 的最大写等待，p99 可能因样本中的单次异常而没有体现。这要求读事务逐页释放，并把一致备份的 reader 生命周期与维护门禁单独处理。实验定时关闭的是未遍历数据的空闲 pinned reader，不是线上并发中止读事务的实现建议。

I-04 的真实 Ledger 域状态已加入[独立派生库过渡候选](../verification/performance/2026-10-02-ledger-projection/README.md)：复用原事件 reducer 的单事件依赖工作集，64 行/8MiB 原子批次，完整认证日志新文件重建，接受/终结分离及有界分页。100/10000 请求的磁盘 GC 存活采样约 0.26/0.42MB，旧 State 约 0.27/25.03MB；候选在 10000 请求下累计分配约 1.21GB、同步构建约 10.5s。这不是相同职责的端到端对比，支持继续域实现，不能据此批准存储布局。后续已接入默认 Standalone Runtime 的过渡路径（见 [owner 契约](../contracts/standalone-ledger-runtime.md)），每次从完整认证 WAL 重建，不信任跨重启派生文件；Primary/Replica 仍保留 State。内容/键集合认证、减少重复装载/编码和行重写、生产 mmap/页缓存/吞吐及跨库边界仍待验证，D-02 最终布局未批准。

离线 backup/restore 的价格引用校验现从冻结原生 WAL 新建临时派生库，删除后才发布，见[备份验证](../verification/performance/2026-10-03-backup-pricing-projection/README.md)。为保持原生已接受宽身份的兼容性，单身份键与组合键的每个组件使用固定 SHA-256；完整身份保留于域行/关联值，事件摘要绑定完整事件。分页按内部键顺序，查找哈希不承担内容/键集合认证，仍禁止信任旧派生 DB 或据此批准存储布局。

2026-10-04补充[追加/删除和随机读的三轮Linux比较](../verification/performance/2026-10-04-layout-mixed-churn/README.md)：10000/100000行、两布局、每项三次、1000个真实metadata事务与1000个delete/append事务、100000次同Tx位置绑定点查，共12样本。新增进程storage IO与两库合计PageAlloc避免旧指标职责不同的比较误用。独立库吞吐中位数较好，但100000行两布局cgroup峰值均约223MiB，打开/full-scan后heap仅约1.5MiB；两布局都有约449–451ms最大metadata等待。因此当前证据支持隔离writer的过渡选择，不能证明物理内存有界或替代生产引擎决定。原始journal/全部history及digest/位置和metadata原值在关闭重开后再次核对；全部样本无max/OOM，24次本次容器/卷清理成功。负载不是price pin/实际计费准入，也不是cold-cache/持续查询；RSS/mmap/file归因、百万规模和目标生产磁盘仍未完成。

2026-10-05补充[真实 Store price pin 并发实验](../verification/performance/2026-10-04-layout-mixed-churn/README.md)：`PrepareDeploymentPricePin`/`CommitDeploymentPricePin` 与同一套投影 churn、随机点查在 Darwin 和 Debian 13 Linux/arm64 运行；Linux 2GiB cgroup 下 10000/100000 行、两布局各三次，共12样本，全部通过并无 OOM。100000 行 cgroup peak 约 222–224MiB、Go heap 约 1.1MiB。该路径直接调用 Store API，不覆盖 Gateway 准入、账本结算、HA 确认；短样本不代表吞吐或布局批准。D-02 仍缺实际准入链、锁序/阻塞归因、目标生产磁盘、RSS/mmap/cache 拆分及恢复/备份/HA 空间条件。

2026-10-05新增[Gateway Chat/账务准入 Linux 基准](../verification/performance/2026-10-04-layout-mixed-churn/README.md)：真实 `Service.Chat` 使用真实 bbolt 定价 pin、Ledger-backed request/attempt/settlement 与本地 fake provider，Linux/arm64 1/8 worker各三次、每样本300个请求，全部成功。吞吐中位数47.40/248.0 request/s；8-worker范围235.8–255.9。它补上 Gateway 核心调用路径但不经过真实 HTTP listener/上游/HA，也未与 projection churn 合并；不作容量承诺或布局批准。D-02 仍需锁序及阻塞归因、运行期/HA与恢复/备份资源条件、目标生产磁盘和物理工作集拆分。

随后新增[Gateway+独立projection联合负载](../verification/performance/2026-10-04-layout-mixed-churn/README.md)：真实 Chat/pin/Ledger 与 10k/100k 行独立历史库的 delete/append 事务、随机点读并发；每个 300 请求样本都验证全部请求、账务 reservation 与投影完整 live range。Linux/arm64 共12样本全部通过且无 OOM。吞吐中位数在10k/100k规模的1 worker为50.81/49.28 request/s，8 workers为159.9/166.2。高并发下吞吐低于 Gateway-only 基准，但本实验未分解CPU、锁和设备IO，也未采集cgroup peak；不归因于单一锁或确认容量。投影只用独立DB；同库 Gateway 组合、HA/备份恢复、生产 listener/介质和阻塞profile仍缺，D-02不关闭。

D-02 通过前补齐：

1. 实际 Store.update/batch、真实 Store price pin、Gateway Chat/账本结算及其与独立 projection churn 并发均有本地 Linux 样本；仍需同库组合路径、真实 HTTP/Runtime/HA工作流、锁序及阻塞归因和目标生产环境对比，不在 DB 事务里等待 peer 或嵌套写。
2. 固定规模预填充、覆盖 churn、长 reader、追加/删除 churn、随机读、price pin 竞争及多轮重复已有本地/Linux样本；仍需在代表性历史规模分解 freelist、mmap/RSS/cgroup/cache、设备 IO 与 p95/p99，控制 reader 生命周期与 DB 打开数量。
3. 比较 batch/并发范围，确认恢复、备份、exact-schema、全量重建空间和中断语义。独立 DB 候选若不能满足正确性与资源门槛，则继续同库或历史分区候选。
4. 在目标 Linux 文件系统重测并固定参数。OrbStack bind mount 不代表生产持久化延迟；单库全扫描 cgroup 接近文件规模也说明需要验证缓存/扫描节流和历史分区，不能只加 Go heap 限额。

## 3. D-03 认证与启动成本的选择

| 候选 | 能证明的信任 | 启动代价 / 剩余问题 | 当前定位 |
| --- | --- | --- | --- |
| 每次从认证基线＋日志重建 | 结果由权威事实重新生成；metadata journal 不独立含 epoch 初始整库，必须有完整认证基线 | 读取/写入 O(历史)，分批能控制内存，维护窗口与空间必须验收 | 过渡路径；不能作为最终普通启动 |
| 认证整个快照文件并启动时完整校验 | 完整校验后可确认字节未被替换，仍需协议保证索引业务正确 | 每次启动读取 O(历史)；验证后不能使用已改变的未经认证 DB 跳过后续日志 | 可作为过渡/离线备份校验；不能单独兑现增量普通启动 |
| 认证根 + 可验证的有序索引/数据块，按读取验证 | 根绑定内容和键集合；每次查询提供 membership/non-membership/range 完整性证据 | 需要实际格式/实现与评审；不能把普通 bbolt Get 的 nil 当成认证不存在，也不能让索引遍历漏行 | 最终增量启动的研究候选，尚未选定实现 |

以上是系统契约比较，不提出自制密码算法。最终候选必须使用经过审阅的标准认证原语及有明确证据的格式/实现；若当前引擎不能提供有界、安全的读取与键集合证明，应评估认证块层或其他索引布局，而不能缩减删除检测要求。

## 4. 最终候选必须闭合的协议

- 认证头至少绑定投影 schema/算法版本、instance/cluster/incarnation、generation、角色/term、每个权威 store 的 cursor/链 anchor、ordering 关联、完整索引根与必要参数。密钥由既有密钥生命周期提供，认证用途隔离、轮转与旧快照读取规则须单独评审；不直接复用普通 checksum 或签名 watermark。
- 树/块包括完整键集合、排序/范围界限与引用摘要。查不存在、枚举 pending、分页、cohort 和幂等查询同样需要完整性验证；任意辅助索引都不能以未认证路径决定余额为零或事件未出现。
- 普通启动先验证认证头、身份和角色允许位置，再从受信任根读取有限节点/活跃状态、认证并应用增量。尚未读到的历史内容不得影响决定；读取它时验证后才使用。缺块、错误引用、解码边界或证明失败必须失败封闭，不能回退为“未找到”。后台全审计与按需验证分开记录，不能把延后发现损坏等同于启动时已全面验证。保留启动时全部权威历史文件存在/长度的有界目录检查，O(segment 数) 元数据成本单独报告；任何安全关键缺失/损坏使全局业务未就绪，后台审计期限与最低进展预算须在 D-04 决定。
- 运行投影发生变化后，每批内容、完整性根与 cursor 的发布原子性必须成立。若采用“不可变基线＋增量层”，增量层自身必须有硬上限，fold/新 generation 不能形成无界 backlog；不能只验证旧快照后相信任意已修改 DB。
- 构建输出、认证根、文件/目录同步和活动 generation 发布必须支持中断与重试；reader 绑定 generation，旧 reader 退出后才回收。旧认证快照只可作为经权威日志/允许前缀校验的较早起点，完整回放后续事实；不可用于撤销后来已持久化的事实。
- 原生日志初始化须提供安全的认证起点与增量路径，覆盖 Ledger、Metadata、Governance、Audit、HA Ordering 及其 cursor 重建路径，而非只优化 State。不能仅以快照 cursor 跳过日志原有认证；旧 checksum-only epoch、目录缺失、活跃尾部、角色前缀缩小须有明确处理规则。
- 存储引擎的 Open、freelist、遍历、映射和损坏解码自身纳入资源限制；认证正确不能代替 RAM/cgroup 有界。若认证格式要求完整重写历史，生成/维护的 I/O 和空间仍需独立验收。

## 5. 实施顺序与未决项

先完成布局的真实路径/工作集实验，再把候选认证格式写成可验证的读写与崩溃状态机。D-03 通过内容/缺失/范围证明、根发布、增量与角色/备份评审前，不把普通持久化投影当作跨重启可信起点；I-04 的过渡实现即使先进入开发，也只用完整有界认证重建，并受主方案的维护窗口门禁约束。

独立三角色复核指出 metadata 认证整库基线、首次接受及整文件/密钥发布的生产 P1；HA Ordering 增量遗漏已补入候选，协议/实现证明仍未完成。隔离页原型的测试与解码 fuzz 通过，仅支持继续原型开发。

未决：具体认证索引实现与格式、根发布/密钥接口、所有原生日志的认证初始化起点、历史分区策略、目标生产 I/O/内存数据与预算参数。本文不宣称这些问题已解决，D-02/D-03/D-04 均未完成。

D-03 后续隔离磁盘原型：[单 pack COW/根发布证据](../verification/performance/2026-10-02-memory-state-authroot-disk/README.md)。codec 2 加入认证准确树高，验证小规模 churn、重开、错误/进程 SIGKILL；尚无 GC/全局预算或百万历史接受条件，不能据此批准存储后端。

D-03 大历史实验：[流式构建/批次及 10m 证据](../verification/performance/2026-10-02-memory-state-authroot-build/README.md)。KV 原型常驻 Go heap 不随本次历史样本线性增加，但未启用缓存提示的单 pack 10m 扫描/构建使 file cache 占主要 cgroup 记账，触及 256MiB 限额并出现 max/回收事件；no OOM 不计容量通过。[后续有限 FD 策略对照](../verification/performance/2026-10-02-memory-state-authroot-cache/README.md) 的第二轮 10m peak=18.613MiB，无 max/OOM、内容与位置验证通过，扫描约慢 8.1%。它支持继续研究增量写提示及固定窗口读取策略，非强制 advice 不能授予缓存所有权或强内存预算。生产布局仍须域数据、随机/并发尾延迟、长时 churn、GC/pack 维护及全磁盘预算，不凭 Go heap 或单次缓存实验批准 append-only pack，也不把 GOMEMLIMIT 当作文件缓存预算。

## 6. 默认 Usage 接线的实际剩余边界（2026-10-03）

[公共磁盘查询](../contracts/usage-public-disk-queries.md) 已支持所选 owner 的明细、失败列表、完整详情、Summary及Dashboard/onboarding/metrics/system status；默认 Runtime 仍选 Aggregate。下一步应按真实消费者闭合这些边界，再一次选择完整 disk owner，不能仅替换 Collector 而保留同历史规模的兼容 Snapshot：

| 实际路径 | 当前依赖 | 必需迁移条件 |
| --- | --- | --- |
| `runtime.go` 的 Usage 初始化、恢复、Collector 和终结追赶 | `restoreUsageAggregateWithReader`、历史 checkpoint、Aggregate Apply | fresh DB 从同一认证 Ledger owner 完整重建；两级恢复产生的新事实通过同一 derivative/Collector 追赶后再发布；失败及关闭 join 后释放物理 owner，保留配置/原生文件身份保护 |
| `admin_usage.go` Dashboard、`metrics.go` | Aggregate Dashboard/Metrics/Windowed/Checkpointed | 所选disk已接同owner errorful观测及独立响应join；global首次成功与Dashboard同cut，指标区分投影逻辑extent/旧内存窗口并在读失败时省略数值；默认启动及物理费用仍待闭合 |
| `admin_onboarding.go` | Snapshot 请求/attempt 全扫描和成功 request-ID map | 所选disk已接成功finalized请求→成功attempt关联、同cut最新上下文与配置更新时间/修订比较；固定工作上限超限拒绝而不截断，后续索引效率/容量及Metadata配置列表仍待闭合 |
| `admin_usage_summary.go`、checkpoint 维护 | Metadata 已存 rollup＋Aggregate 未存增量、Drain/Commit/Return | 所选disk Summary已接同cut归约及固定exclusive scratch，未合并旧Metadata增量；默认选择、孤儿协调与checkpoint仍待迁移，不借旧 checkpoint 名义信任 fresh DB；原生 Ledger 外部 chain anchor 现已独立于Usage CatchUp/checkpoint成功推进，Replica durable尾部拒绝；最终可信增量仍待D-03 |
| `exportUsageParquet`、`usage.go` 窗口/归档保留 | Snapshot、完整 pending slice/date map、Manifest.Files slice、`PruneBefore` | 有限源页和整批输入/编码/输出费用；按完整事实前缀发布分区/manifest，崩溃孤儿可重试；成功归档后才能推进 trim，保留游标引用及 Summary/详情版本，不以磁盘常驻代替保留语义 |

`internal/usage/parquet.go` 的现有 Export 先复制全部未归档 attempts、排序、按日期聚合，再累计全部 Manifest.Files 后发布。仅把 Snapshot 改为多次小 Snapshot 仍保留整段 pending、日期及文件目录增长，也不能证明“按日期写完一个文件”已覆盖完整 sequence 前缀。[固定H的有限归档源](../contracts/usage-archive-source.md) 已开始I-08实现：原生sequence顺序、有限页、DB事务外sink、完整source receipt及Close join；分批归档仍需要改变发布与目录协议，不把此producer当作Export/manifest整体完成。

[有限分片 publisher 后续接线](../contracts/usage-disk-export.md) 已使所选 disk Runtime 无需 Snapshot/pending/date map：完整冻结前缀的有限 Parquet/NDJSON 输出成功后才原子发布 H；旧 catalog 启动/位置/发布采用逐值流式遍历。日期交错和同名孤儿的内容核对、file Sync/目录屏障、全部物理生命周期已局部验证。JSON 兼容迁移仍需全目录扫描/重写，版本化分页索引、有限完整前缀的大积压调度、精确磁盘校验/孤儿清理和可恢复保留尚待实现；该轮默认 Usage 仍 Aggregate，旧 offline Verify/Reconcile/Export/Prune 仍有全量数组/map；后续 Verify 接线见本节末。本项不改布局/容量/可信快照的未决状态。

后续 [分页 catalog v1](../contracts/usage-paged-catalog.md) 已接所选 disk 发布：schema-9 固定根与有限不可变页，保留行 schema 3–8；旧目录流式迁移，后续仅追加新页/根，位置点读不扫描历史。目录构造、冻结签名 cursor、结构/hash/身份与缺页校验、旧接口拒绝及默认 Aggregate 启动格式门禁已局部验证。认证根/可信增量恢复仍未完成，不能把页 SHA 当作原生内容或键集合认证；大积压调度、精确行集校验、可恢复迁移/保留/孤儿清理与共享费用仍是默认切换前置条件。

[精确归档对账后续](../contracts/usage-exact-archive-verification.md) 已实现所选 fresh disk owner 的显式 native 区间比较：分页目录/摘要/实际行校验与独立 EventID、sequence、path 临时磁盘索引，默认从 genesis 验证，不由首文件或空目录推导保留起点；历史 schema 3–8 缺列及 Snappy/Zstd 读取、私有摘要验证 spool、footer/page/解压预检和整 job 回读/物理索引上限已有定向验证。失败/取消/退休/清理失败均撤销报告，实际 source H 与归档前缀分别记录。该轮大历史可恢复校验、日期保留的迟后事实 coverage、CLI/默认 Runtime 接线、孤儿清理、共享费用与可信恢复仍未完成；不推进 D-02/D-03/D-04 决策批准。

[有限完整前缀步进](../contracts/usage-archive-steps.md) 已接所选 disk Runtime 的真实维护入口：一次固定页/行/输入输出工作量，只发布明确的 ThroughSequence，保留真实 source Head 为独立 fence；下一次或新 Exporter 从已提交根续传。严格全段 Export/校验仍拒绝超限，不将 sink/write/work/time/取消/退休错误转成部分成功。259 条真实账务 attempt 的实际 Runtime 回归先发布 256 条，再覆盖全部事实，两个范围精确对账，无 Aggregate fallback 与 native trim 授权；两种格式重开续传、源预算边界、失败根保持/同 inode 孤儿复用和 Close join 已定向验证。完整历史可恢复校验、旧目录迁移/保留恢复、迟后事实日期 coverage、孤儿清理、共享费用/可信恢复及默认启动仍未完成。

[离线校验前置](../verification/performance/2026-10-03-usage-offline-prerequisites/README.md) 已补只读 ordering 完整认证扫描和无需迁移的旧 JSON catalog 精确对账。前者保留 index-zero baseline 的 heads，将角色选择前缀与实际物理 tail 分开，并拒绝坏后缀、partial、身份/位置错误、取消和观察到的文件变化，不修尾/Sync/写盘；后者先校验完整根，再从同 descriptor 逐项读取，继承缺省 entry schema，复用同一行集/预算/解码/生命周期边界且保持既有文件。该证据 cut 中它们仍是实际 CLI 接线的前置：HA 原生 baseline head 与 ordering frame digest 的绑定、角色 policy、独立外部 anchor/member/Metadata 的最终复核及 fresh 双投影整操作清理尚待实现；不能把 cursor 相同当成 native 身份证明。默认 Usage、D-02/D-03/D-04 和发布验收的状态不因此改变。

[实际离线 Verify 接线](../contracts/usage-offline-verification.md) 现已从完整认证 native 新建独占 Ledger/Usage 磁盘参考，默认 genesis、可显式选择 `--after-sequence`；旧 JSON/分页目录与 Parquet/NDJSON 复用精确对账。HA 将 seeded Ledger head、全部 Ledger range/roll 的真实 metadata/frame digest 与只读 ordering 绑定，Primary 选择 signed durable，Replica 选择 applied 并核对 Metadata；完整 excluded tail 仍认证，独立 historical checkpoint/native/member/Metadata 最终复核及 DB 清理错误撤销报告。报告明确 archive H、真实 Usage source H、role 允许物理 cut 和完整 native tail，不能据 durable 对账授予 confirmation/晋升/删除权。该 Verify 证据 cut 的默认 Runtime Usage、compact/prune/rebuild-summary 仍为 resident 路径；完整历史可恢复校验、日期保留 coverage、迁移/孤儿清理、共享物理费用、I-12 目录索引和可信增量恢复仍待实现。受影响 Ledger/Replication/Usage 全包 race、App 定向 race、CLI 与 vet 通过，原始源码 cut/命令/实际 exit 见[离线 Verify 证据](../verification/performance/2026-10-03-usage-offline-disk-verification/README.md)。临时双 DB 和重复完整认证没有证明 Linux 容量或启动窗口；D-02/D-03/D-04 不据此批准。

[实际离线 Compact 接线](../contracts/usage-offline-compaction.md) 已共用认证 fresh 双投影：写入前精确验证既有根、多个有限 step 覆盖全部冻结目标才成功，最后完整对账与 native/Metadata/物理清理后ctx屏障；错误撤销报告，已发布完整前缀和孤儿可在验证后重试。为保持默认Aggregate启动的格式兼容，missing/legacy保持schema8 streaming JSON，已有分页根继续分页；旧schema继承、bounded值/固定buffer、actual root write/work费用及atomic根身份/digest/barriers已实现，仍承担旧目录完整scan/rewrite。默认Runtime、prune/rebuild-summary、retention coverage、可恢复全历史校验、共享物理费用/容量和可信恢复仍待闭合；最终适用Usage全包196项race、App33项定向race、CLI50项和三包vet通过；App清理后ctx修复的两个文件增量/重跑及未变Usage依赖复用、最终指纹与原始日志见[compact证据](../verification/performance/2026-10-03-usage-offline-compaction/README.md)，不据局部实现批准 D-02/D-03/D-04。

Standalone 的过渡 owner 仍从完整 native 认证源重建，D-03 不因此通过。HA 必须另按 applied/confirmed 允许前缀选择同 generation 的 Ledger/Usage/Governance，不能复用 Standalone 的 native durable head。以上是当前接线门槛，最终布局、共享费用、Linux 物理容量及生产接受继续待证据。

[实际离线 Summary 重建](../contracts/usage-summary-disk-rebuild.md) 现用认证 fresh 磁盘参考及全标签有限页，将汇总写入两个固定的本节点派生槽；完整结束后才原子发布位置和不可恢复 checkpoint 标记。原始事实、Ledger anchor、Metadata epoch/sequence不被改写；实际FD身份/原位Metadata交接和最终native/清理屏障已通过真实回归。旧Aggregate正常启动明确拒绝标记并完整重建，避免将旧汇总加到全量replay上；这不是可信快照或默认Runtime选择。有限写事务与固定槽数量消除了CLI整历史Aggregate/rollup map/day set，但bbolt mmap/freelist、inactive空间和共享物理费用/同库运行时竞争仍待接受，不据此批准D-02/D-03/D-04。最终受影响Usage 199、Store 197、App 805项全包race、CLI 51项与四包vet均实际exit 0；App 12个顶层及1个子测试skip另列原始原因，不作Linux/真实AWS接受。1147个Go/module前后指纹及16份源码快照一致；首轮App包级10分钟超时栈保留，同一源码单独以30分钟上限重跑通过。完整门禁及源码cut见[Summary验证记录](../verification/performance/2026-10-03-usage-summary-disk-rebuild/README.md)。默认Usage切换的窗口、日期coverage与跨页清理条件见[保留边界记录](usage-retention-switch-boundaries.zh-CN.md)，窗口 step 的后续实现见下段。

[磁盘 Usage 窗口 step](../contracts/usage-disk-window.md) 现已实现有限事务中的独立 attempt/summary 前缀、最小存活 floor、原子计数/epoch、查询索引和过期 terminal 的有限版本回收。窗口 epoch 绑定本地及 v2 HTTP 游标；普通 Append 保持旧 H，删除后旧续页明确失败。长归约/归档/detail 与普通物理读者实际退出前，零队列维护返回 Busy；取消不提前释放实际工作。内部归档读取单独核 attempt 删除 cut，不能用共享 floor 推断完整来源。Usage 全包208项 race、App 25项定向 race、CLI 51项及三包 vet 均实际 exit 0，1149个Go/module与9份源码快照前后一致；原始证据见[窗口验证](../verification/performance/2026-10-03-usage-disk-window/README.md)。该 API 仍由调用方独立证明完整归档发布权限，未接入默认 Runtime；日期 coverage、重建后窗口发布、其余历史索引/物理空间及共享费用、HA/D-03/Linux 接受仍未完成，不据本地删除次数批准布局或容量。

[归档读者生命周期](../contracts/usage-archive-lifetime.md) 增加四个零队列读许可、实际 FD 关闭后的退许可、私有排他回收门禁及 epoch 绑定归档续页。普通 append 保留旧 H，清理推进拒绝旧续页（包括空/EOF）；旧版 prune 使用同一门禁但仍保留历史数组和完整根重写。Exporter Close join 实际读写并清权限，Runtime 两角色分支和 fresh/legacy offline 在依赖及目录锁之前 join。新 Usage 五项与 App 两项真实生命周期定向 race 已通过，当前最终全包门禁终态见[验证记录](../verification/performance/2026-10-03-usage-archive-lifetime/README.md)。descriptor 返回后另行打开的文件不受保护；门禁不提供日期/native 删除权限。有限持久化根重写、保护当前引用的 unlink/恢复、默认磁盘 Usage 和共享费用仍待完成；D-02/D-03/D-04 不据此批准。

[有限日期候选与 schema 10](../contracts/usage-date-catalog-candidate.md) 现已实现分页目录的有限持久构建与重开前缀重算，固定 checkpoint/缓存不保存历史数组；旧根或显式策略变化拒绝复用。schema 10 将日期 coverage 与处理 H 分开，追加保留清理 baseline，当前目录/原生精确对账和离线命令消费该格式，默认 Aggregate 仍明确拒绝。实际页/状态 rename 与取消 Close、两种容器的迟到旧日期追加、完整数据也不 mint 窗口权限、默认拒绝/显式 scope 已定向验证；最终受影响门禁与源码 cut 见[候选证据](../verification/performance/2026-10-03-usage-date-candidate/README.md)。Ready 只证明 descriptor 转换结束，尚不激活 live 根或 unlink；native 激活、大历史对账续传、当前引用保护/旧根义务、孤儿空间/物理费用仍待完成。不据本机候选或新派生格式批准 D-02/D-03/D-04。

[有限候选行集对账](../contracts/usage-date-catalog-verification.md) 后续实现按调用推进的归档文件/来源序列比较、有限 checkpoint 重算和私有磁盘 radix 索引。索引只保留三个根、extent/offset 和临时 MAC key，160 B 节点/最多 256 层路径，不建立随历史增长的堆 cache/freelist；磁盘量仍随行/文件增长并受显式空间 cap。保存的索引不被继承，保存的 Ready 重算后才返回比较结果，实际 scratch Close/清理在 receipt 前完成。实际 App fresh 来源/角色及最终权限复核的回归与本地门禁见[比较证据](../verification/performance/2026-10-03-usage-date-comparison/README.md)。这不是权威事实布局选择、D-03 认证快照或 Linux/RSS 接受；native 构造 deadline、激活/unlink/孤儿恢复与共享费用仍未闭合，D-02/D-03/D-04 不据此批准。
