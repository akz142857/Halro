# 内存有界状态：交付计划与进度看板

更新：2026-10-08。此表从当前仓库证据出发，替代此前只描述执行顺序的计划。计划写成不代表实现完成；当前没有后台执行承诺或上线日期。

## 1. 当前基线与目标

- 原任务 24 项：5 项完成，19 项开放。其中 17 项属于本地工作；D-01 生产取证、R-01 发布及生产接受沿用此前暂缓范围。
- 原 2 GiB 是逻辑 RAM 总预算；实际 Linux cgroup 限制另行验证。原 Recovery reserve 为 1387 MiB，normal 剩余 661 MiB。补齐常驻费用后，未共享必要 normal 为1046.053 MiB/70 claims。P1-A2已接入三个后台消费者的共享池，实际减少256.667 MiB；当前必要 normal 为789.386 MiB/54 claims，仍缺128.386 MiB。这仍不是完整阶段峰值；旧385.053/318.526 MiB缺口保留历史范围。
- 六处生命周期修复已完成对应验证；私有运行使用逻辑 4 GiB / 实际 cgroup 2 GiB，不能算原目标通过。十万请求冷迁移仍在 1800 秒门槛失败。
- 用户提出 1 GiB 的可能性。本计划把它列入可行性比较，尚未把原验收目标改成 1 GiB。任何 1 GiB 承诺必须同时覆盖逻辑预算、实际 cgroup、数据规模、单次大小、并发和吞吐；不能靠隐瞒更大的预留预算成立。

当前依据：[本轮裁定](../verification/performance/2026-10-08-ha-resource-vector/README.zh-CN.md)、[实际费用表](../verification/performance/2026-10-08-ha-resource-vector/work-vector.json)、[原剩余条件](../verification/performance/2026-10-08-ha-resource-vector/remaining-task-register.json)。

## 2. 按交付物推进

下面的时间是到下一次书面裁定的有效工作上限，包含该阶段启动的检查等待；不是完工估计，也不是定时自动运行。阶段进入时记录开始时间、固定输入和范围。到上限仍未通过，必须记录失败或缺失证据、实际改动和下一步选择，不能自动追加同类试跑。必要的资源清理和退出确认仍须完成。

| 阶段 | 当前状态 | 到裁定的工作上限 | 必须交付 | 进入下一阶段的条件 |
| --- | --- | --- | --- | --- |
| P0 基线和看板 | 本计划已建立；原源码冻结记录沿用 | 已完成计划工作 | 原任务数量、已有成果、明确失败、阶段与原任务映射 | 基线事实和验收条件可查；不新增任务完成声明 |
| P1 预算可行性 | 预算补漏、维护顺序和三后台池共享已交付；仍缺128.386 MiB，完整峰值未接受 | 2 小时 | 覆盖启动、审计、复制、请求、checkpoint、维护、角色切换、Close 的 owner/并发/释放表；2 GiB 与 1 GiB 对比；唯一首选改动或路线不成立裁定 | 区分已证明可共享和待证明；给出改动后可复算预算。表不完整就标未知，不声称可行 |
| P2 固定预算运行闭环 | 未开始新方案验证 | 4 小时，每次裁定最多 2 个有不同依据的候选 | 首选资源交接/共享实现，配置预检，受影响生命周期测试，实际固定预算的双角色运行报告 | 在同一源码/配置下，实际 TLS、成功/取消请求、精确双端终结、checkpoint、两次重启、Close 和资源退休通过；公共入口仍按完整模式接受范围开放 |
| P3 冷迁移成本闭环 | 旧候选已拒绝，新算法未开始 | 4 小时，每次裁定最多 2 个新算法候选 | 阶段成本归因、候选算法说明、同期对照、逐级规模结果 | 1k 筛选通过后才跑 10k，再通过才跑 100k；每级有业务/认证差分和真实退出结果 |
| P4 原任务逐项接受 | I-03、I-04 已交付所选子项；I-05 跨页/超新增预算/连续崩溃恢复已验证，下一批 I-06 角色 generation 切换 | 每批最多 2 小时；一次只选 1～3 个原任务，批次数据缺项裁定后确定 | 每项的条件→实现→测试→证据→残缺项表；故障、备份、晋升及吞吐等分别验证 | 原条件全部有证据才关闭该项。独立项可以提前接受，不强制等全部 P2/P3 完成 |
| P5 本地最终交付 | 依赖前述接受 | 2 小时裁定窗口；必要检查超过窗口则说明当前运行和剩余项 | 适用完整 gate、最终源码/配置身份、用户可操作的迁移与回滚说明、实际关闭清单 | 17 项本地条件全部满足才能称本地范围完成；生产两项单列 |

用户已确认转入原 19 项开放问题的实际开发，并在讨论 I-03 下一批故障测试后确认推进独立的 **I-04 Ledger 投影**。三批行一致性子项见 §10～§12；最新 I-05 遗留恢复固定输入始于 2026-10-08 18:35:46（Asia/Singapore），约 22 分钟内交付，见 §13。现有恢复算法通过新增跨页、超新增 Active 预算及连续 SIGKILL 验证，不需要为了本批修改生产算法；修正了普通测试 writer 的零链摘要夹具。下一批转到 I-06 角色允许前缀缩小、generation 替换及失败回滚。I-03 的 HA 物理 ENOSPC 批次暂缓，原接受条件保留。P1-A2 三后台共享接线已交付；P1-A3 全阶段峰值仍是 D-04/I-03 的残缺条件。P2 仍须遵守原容量接受门槛，独立原任务条件可以直接实现和核验。

现在不排一个没有依据的“几天全部完成”日期。按实际缺项调整工程估计和批次数，任何调整需写明原因，保留原预测。后续围绕原任务具体未满足条件排批次，不把同一容量失败的重复运行算作开发进展。

## 3. 首个检查点具体交付

P1 [首次裁定与 owner/阶段表](memory-bounded-budget-feasibility.zh-CN.md)已提交，修正后的真实对账见该文。完整峰值和安全共享仍有明确未知项，P1 不标完整通过。预算裁定至少包含：

1. 各工作 owner 的实际申请点、RAM/Disk/claim、持有区间、同时活跃关系、释放/join 点和源码位置。
2. 原必要下界与完整费用的差别；原恢复预留中多个候选、session、queue/read owner 的真实并发证据。
3. 每个共享候选的排他机制、嵌套调用、取消、已接受工作终结及 Close 义务。只有证明生命周期互斥后才可从预算中消除重复项。
4. 对固定单次大小、并发和各角色模式给出 2 GiB / 1 GiB 的结果：可进入实现验证、不成立、或缺哪条证据。逻辑可容纳还不等于 cgroup 实测通过。
5. 首选改动的预计节约字节、修改范围、受影响检查；若只能通过减少能力成立，明确能力损失，作为产品范围选择提交，不默认削减 HA 或账务契约。

缺口不能靠任意降低 Recovery reserve、提高总额、放宽认证或手工注入 Healthy 消除。不能仅凭测试串行就认定实际 owner 可以共享。

## 4. 冷迁移分级门槛

沿用已有接受门槛，不重跑无变化的失败候选，不延长失败用例期限：

| 级别 | 门槛 | 失败后的动作 |
| --- | --- | --- |
| 1,000 请求筛选 | 同期 v8/候选各至少 3 次完整生命周期；迁移中位数改善至少 30%；峰值不超过原 1.5 GiB 筛选上限；无 OOM、业务或认证差分 | 拒绝该候选，记录成本归因；没有新的算法依据不进入更大规模 |
| 10,000 请求 | 原 300 秒迁移期限；激活、业务核对、独立进程重开、退出与资源退休通过 | 保留原失败，定位阶段；不因 1k 改善宣布通过 |
| 100,000 请求 | 原 1800 秒迁移期限；完整生命周期、实际空间、RAM 与业务/认证正确性通过 | 判定目标规模未接受，转入算法或范围裁定 |

1.5 GiB 是原 2 GiB 路线的筛选门槛，不能用于接受 1 GiB 部署。若确定改为 1 GiB，必须在运行前固定该路线的 cgroup、余量和吞吐条件，重新验证；原结果仅作对照。每次记录阶段 IO、分配、耗时和峰值，而非只记总时长。

## 5. 17 项本地任务如何关闭

下表用于安排证据，不取代原完整条件。现在这些项全部保持开放；相关阶段通过也不自动整组关闭。

| 原任务 | 主要阶段 | 关闭时必须单独核对 |
| --- | --- | --- |
| D-02 存储布局 | P3/P4 | 布局对照、锁序、写放大、mmap、备份/HA 影响及明确选型 |
| D-03 认证协议 | P4 | 内容/键集合/位置/版本/发布/删除检测及角色恢复协议 |
| D-04 容量门槛 | P1/P2/P3/P4 | 固定输入的 RAM、磁盘、并发、churn、延迟和启动门槛；暂缓的生产证据依赖须保持标识 |
| I-03 预算与终结储备 | P1/P2/P4 | 所有权交接、压力迟滞、有限等待、终结额度、ENOSPC |
| I-04 Ledger 投影 | P4 | 历史/幂等/余额、原子水位、价格/治理/备份 cursor、锁序和失败封闭 |
| I-05 两级恢复 | P2/P4 | 全 pending/unfinalized 枚举、迟后 settlement、Usage 联动、幂等与孤儿清理 |
| I-06 HA 窗口/角色 | P2/P4 | 有限帧、未确认准入、前缀重建、原子 generation 切换和恢复确认 |
| I-07 Usage 分页/汇总 | P4 | 明细 cursor、统计语义、原子处理位置和无全量驻留 |
| I-08 归档/manifest | P4 | 行/字节/编码上限、完整前缀、孤儿重试、流式验证及 reader 生命周期 |
| I-09 价格历史 | P4 | 时间 cursor、有限缓存、流式审计、单部署增长基准 |
| I-10 Governance | P4 | Outcome/修订/幂等索引、有限 cohort、结果完整性 |
| I-11 快照/增量恢复 | P2/P4 | 原 §4.1.1、原生日志初始化认证、篡改/部分发布/旧快照/增量故障 |
| I-12 segment 目录 | P3/P4 | 原 §4.3.1、迁移/cursor、Roll/Compact 原子性、缺失检测、HA/备份兼容与增长 |
| O-01 迁移命令/探针 | P2/P3/P4 | 准备→维护复制→恢复确认→ready，schema、进展、空间和中断预演 |
| O-02 备份/回滚/升级 | P4 | 兼容矩阵、保留新事实的回滚、Replica 和晋升 |
| V-01 故障矩阵 | P4 | 原 §9.1 每项，包括 SIGKILL、ENOSPC、确认前提交与重试 |
| V-02 容量/性能 | P2/P3/P4 | 原 §9.2 完整曲线、硬阈值、安全余量、吞吐和恢复时长 |

当前还不能给 P4 一个可信的总完工时长：必须先列清每项尚缺哪些接受用例，复用有效既有证据，不重复已成功且未受变化影响的测试。D-01/R-01 的暂缓不允许伪造其证据或静默删除本地项对它们的依赖；本地结果、生产资格分别报告。

## 6. 每次汇报和失败后的规则

每个检查点更新此表并提交一段面向用户的结果：

- 本次主目标、修改的行为、源码/配置身份和实际用时。
- 交付物及证据链接；预算减少了多少字节、性能改善了多少、哪个接受条件通过。
- 仍失败的条件及原因；未知保持未知。
- 原任务关闭数量和阶段交付数量分别报告；不从 5/24 推算工作量百分比。
- 下一步唯一主目标、工作上限、成功判据；需要改变内存目标或产品能力时明确提出选择。

达到工作上限没有结论，记录缺口并转入路线裁定；同一候选无源码/配置/环境相关变化不重跑。新的运行错误只修复当前交付必须经过的路径，其他问题登记后排期，避免重新扩成无限集成修复。

检查遵守 AGENTS.md：开发时受影响定向检查，实际并发/生命周期变更才运行对应 race；发布前执行适用完整 gate 一次。所有运行保留原退出码，完成 owned 宿主资源退休。公共 HA opening/promotion/rotation 的接受边界和账务/认证契约保持。

## 7. 进度日志

| 日期 | 检查点 | 实际结果 | 原任务数量变化 | 下一步 |
| --- | --- | --- | --- | --- |
| 2026-10-08 | P0 计划重建 | 建立阶段交付、裁定时限、17 项映射、失败规则；仅文档修改 | 5 完成 / 19 开放，未变化 | P1 完整预算可行性裁定，2 小时工作上限 |
| 2026-10-08 | P1 首次裁定 | 新增常驻费用 66.526 MiB/7 claims；必要缺口 385.053 MiB；实际预检修正与 Manager 对账；两个单独共享方案不足，整体共享只是待证明候选 | 5 完成 / 19 开放，未变化；实际节约 0 B | 先补齐跨组件嵌套/等待/退出协议与全阶段费用；未进入 P2，原 2 小时裁定上限保持 |
| 2026-10-08 | P1-A1 在线维护顺序 | 已把 Metadata join、collector join、coordinator 捕获/peer wait、reserve/receiver/applier 屏障与 RAM 借用分开；重放回调单次/过期/实际 join 有界；最终检查见预算裁定 | 5 完成 / 19 开放；维护前置子项交付，真实费用节约 0 B | 下一唯一主目标 P1-A2 跨组件子借用与 foundation 最终释放；本会话 13:37:11 开始、15:37:11 裁定上限，P2 仍未进入 |
| 2026-10-08 | P1-A2 三后台池共享 | 三个真实消费者/两角色接线，固定8成员票据、单根FIFO、真实嵌套与最后owner释放；六组回归和最终定向/全resourcebudget-config race及真实TLS/ext4通过；实际减费256.667 MiB，当前必要789.386 MiB/54 claims，原预算仍缺128.386 MiB | 5完成/19开放；共享接线子项交付，P1整体/P2未通过 | P1-A3：剩余缺口与全阶段峰值裁定，2小时上限；先给可复算证据和唯一后续路线，不重跑不变的容量失败 |
| 2026-10-08 | 原 I-03 有限等待/备份 ENOSPC | 实际排队点加入每阶段30秒上限，保留已进入操作上下文与费用；新增密文真实ENOSPC精确清理，最终定向race和真实TLS/checkpoint通过；详见§9及checks.json | 5完成/19开放；条件级交付，整项接受仍缺完整HA故障/目标容量 | 下一批原I-03的HA物理ENOSPC与接受终结保护，最多2小时/两个不同故障时点；容量阻断后转独立原I-04条件 |
| 2026-10-08 | 原 I-04 账务行一致性 | 修复损坏结算金额/摘要/物理位置被分页接受，以及损坏预留金额继续结算并推进水位；新增 61 个故障场景通过；Linux Ledger 410 个顶层用例通过、8 个跳过，Budget/备份/恢复定向回归通过 | 5完成/19开放；两类真实缺陷已修复，I-04 整项接受条件见 §10 | I-04 余额/幂等索引与业务读取错误传播；保留 D-02/I-03 容量依赖 |
| 2026-10-08 | 原 I-04 余额/幂等索引 | 拒绝缺失/null/负余额及损坏治理 alias，首次位置按之前的 cut 校验，SHA-256 索引拒绝 31/33 字节静默补齐/截断；58 个 Ledger 故障场景及 2 个 Manager 拒绝场景通过，最终 Linux Ledger 414 通过/8 跳过，四个消费者定向回归通过；快照时序夹具改为实际锁屏障并通过定向 race | 5完成/19开放；本批校验及错误传播子项交付，§11 列出具体剩余入口 | I-04 租约/请求点读身份与物理位置校验 |
| 2026-10-08 | 原 I-04 租约/请求读取 | 普通/认证点读、恢复/价格租约读取、恢复分页及归约依赖统一身份与物理 cut 校验；116 个故障场景和 8 种真实旧版到达顺序通过，Linux Ledger 419 通过/8 跳过，四个消费者回归通过 | 5完成/19开放；本轮选定读取缺口已补齐，整项残缺见 §12 | I-05 超现行准入上限的遗留恢复与中断重试；复用已通过的小输入回归 |
| 2026-10-08 | 原 I-05 跨页/中断恢复 | 固定新增请求容量仅 4 个并占满，129 个既有请求的两集合完整恢复；26 个新场景包含每轮 12 次真实 SIGKILL，四个场景连续中断两次；原摘要、账务与治理/Usage 清零及同 MAC cut 验证通过；Linux Budget 整包 110 通过/1 子进程 helper 跳过 | 5完成/19开放；所选恢复功能条件通过，生产算法未改；完整 HA/依赖/资源/增长仍开放 | I-06 角色允许前缀缩小、generation 替换与失败回滚，复用已有重开和组件证据 |

## 8. docs 文件盘点与整理计划

2026-10-08 实际盘点：docs 共 7,740 个文件，逻辑文件大小约 462.74 MiB；Git 跟踪路径 489 个。此为整个 docs 目录，不能把所有文件归因于本次任务。计数不表示进度或代码复杂度。

| 类别 | 实际数量/大小 | 处理依据 |
| --- | --- | --- |
| docs/verification/performance | 6,695 个文件，约 436.91 MiB | 占用主要来源；原始实验、夹具、报告和源码快照混放；当前已有 .gitignore 整目录忽略 |
| docs/architecture/evidence | 496 个文件，约 18.16 MiB | 作为实验证据逐项核对引用与独立失败记录；不一律当成当前设计 |
| 所有 .gz 文件 | 29 个，约 208.57 MiB | 主要为每轮完整源码归档；内容很接近不代表相同，冻结失败版本仍有复现用途 |
| 完全相同的非空文件 | 232 组，441 个额外副本，约 15.53 MiB | 仅是内容去重候选；不同运行的输入/前后指纹可能承担不同证明位置，不直接删除 |

这里按文件大小汇总，不是文件系统实际块占用；扫描未评价每个文件的必要性。性能目录在 .gitignore 中，因此“本地存在”也不代表提交后其他人能复现；正式交付必须明确证据归档位置。

整理分为三类：

1. 当前入口：主设计、此进度看板、最新裁定和机器任务清单。后续更新这些入口，避免每轮重复生成多份状态总结。稳定的契约/ADR/用户指南继续按原用途保留。
2. 历史证据：成功和失败原始日志、固定输入、源码身份、资源退休记录、实验夹具。保留可复现信息，采用可校验归档和索引后再从日常阅读目录移出；原始失败不因成功覆盖。
3. 清理候选：相同内容副本、无引用临时输出、已被替代且可复现的中间文件。先检查仓库引用、脚本使用、证据 manifest 与哈希，再制定具体路径清单；若需改引用，在同一整理中修正并校验。

当前尚未完成逐文件引用/用途审计，也未移动或删除历史文件。不得把“无 Markdown 链接”当成无用途，Go 夹具、脚本和机器指纹也可能消费它。后续整理作为独立一批工作，先提供路径、归档索引、哈希/大小和预计收益；不能批量删除整个 evidence 或 performance。

新运行的原始输出集中在一个声明过的实验目录；相同输入按内容身份复用，保留运行记录引用。每个候选最多一份用户可读裁定，进度在本表更新。整理文件和新增报告不计为原功能项完成。

## 9. 原 I-03：本批实现和未满足条件

本批直接修改共享 RAM 根借用、账务 append、Member 读取权限、Ledger 业务读/维护、Metadata 业务 facade、恢复 writer、Usage view 和 collector 的排队路径。无 deadline 的调用也有每个等待阶段 30 秒上限；调用方较短的 deadline 优先。Collector 的后台 slot 和 reducer 锁是两个阶段，最坏合计 60 秒。队列超时返回 `ErrWorkQueueWait`，撤掉自身等待票据；进入操作后计时器停止，实际 IO、确认、物理借用和 Close join 保留原生命周期，不能靠超时提前退掉接受费用。

新增回归使用虚拟时钟核对：超时没有进入 native 回调，只退自身票据，原生 head/认证 root 不变，同一 owner 可以重试，已进入的操作超过等待时限仍可完成。新 Linux 用例走实际备份命令路径，在输出逻辑预留成功后填满隔离的 16 MiB tmpfs：密文写入真实 ENOSPC，失败输出没有发布，已确认关闭的临时 inode 先清理，再恢复空间让原 source 关闭；无关文件和原生账本保持，留盘费用不因失败消失。该用例使用逻辑 4 GiB 小数据夹具，不能接受原容量目标。

| 原 I-03 条件 | 本地已验证范围 | 整条条件尚缺什么 |
| --- | --- | --- |
| 所有权移交 | 三后台共享、实际嵌套/退出及备份 source/stage/output 同一 Manager；本批超时只退等待者费用 | 全部持久化消费者和命令交接覆盖，生产启动磁盘身份 reconciliation |
| 压力迟滞 | DataDir/临时盘 bytes/inodes、未知信号拒绝和迟滞；备份 ENOSPC 在真实预检后发生 | 目标容量下持续压力/恢复，动态输出盘完整接受 |
| 有限等待 | 上述排队点硬上限，定向 race 核对超时/重试/已接纳工作保留 | 其余 owner/命令覆盖审计；不响应取消的物理 IO 继续记未退休 |
| 终结额度 | 原 Completion 池及超时后保留实际接受费用；终结、Close 有组件及实际 TLS 检查。2026-10-09：原 2 GiB 在 Recovery 1259 MiB 下可行（§18），满压力下终结关闭不再申请新准入且在真实 ENOSPC 下完成（§21） | 三节点（两个 peer 同时在线）下的压力终结；目标容量下的持续压力 |
| ENOSPC | 新增命令备份密文实际失败与精确清理，既有八类 native/projection/Runtime 场景保留 | HA 储备保护、归档多文件发布、迁移故障及重启双端确认矩阵 |

当前必要 normal 为 **827729920 B（789.386 MiB）/54 claims**；原 Recovery reserve 1387 MiB 后 normal 仅 661 MiB，仍差 **134621184 B（128.386 MiB）**。本批没有新增减费。I-03 整项仍开放，原任务计数保持 **5 完成 / 19 开放**；不按文档或子测试数量报完成率。

最终源码的定向 race、受影响包检查、Linux 实际备份 ENOSPC、双端 TLS/checkpoint 常规与 race 记录集中在 [I-03 检查记录](../verification/performance/2026-10-08-i03-acceptance/checks.json)。后台 collector 超时后保留原有固定唤醒；新增回归证明无需另一个 native 事件即可继续追账，补充后重新验证实际 TLS 与 ENOSPC。

五个受影响包整包通过；Usage 整包在默认 10 分钟总期限超时，当时归档损坏父用例仅运行约 2 秒，`missing_middle` 子用例处于普通原生日志夹具构造。单独重跑该用例的全部九个场景、Collector 全部定向及完整 typed/ordinary reducer 对照约 7.5 秒通过；最终 collector/view race 约 7.4 秒通过。保留 Usage 整包超时与栈，不把局部通过改写为整包成功。

TLS 是小数据私有夹具，不替代原 reserve/容量及公共 HA opening/promotion/rotation 接受。成功、失败迭代日志保留，不新增本轮状态说明文档。本批 15:50:48 开始，16:21 左右完成裁定，未达到 2 小时上限。

原定下一批的 **I-03 HA 物理 ENOSPC 与已接受终结保护**已按用户确认暂缓，转入 §10 的 I-04；这些用例仍是 I-03 的未满足条件。恢复该批次时，成功判据保持为：拒绝新工作、保留接受事实及费用、精确清理自有临时文件、双端重启恢复和确认。原容量配置无法承载时记录容量阻断及缺失用例，不重复不变的容量失败。

## 10. 原 I-04：账务行一致性修复

本批发现并修复两类实际缺陷。普通结算点读及普通/认证分页未统一校验结算摘要、已知成本标志和金额；结算位置为零或超出固定物理 cut 也可能被接受。更直接的账务问题是：把 pending 投影里的预留金额从 5 改为 4，旧普通 Apply 和认证 native advance 都会成功，推进水位并留下错误的预留余额。

现在统一按 reducer 的原始事件校验结算行与 pending 预留行，核对原始 lease、金额、余额归属和合法物理位置。普通/认证的 pending、request recovery、pricing/backup 分页采用同一校验。失败页不返回先前读出的有效前缀或 cursor；回调忽略错误也不能完成认证 pin。ApplyBatch 的后续依赖损坏会使整批回滚，认证 advance 不发布后继 root；已持久化的 native 事实仍保留。新增读取的结算位置字节计入分页上限，免费/未知成本、旧 generation 较大 offset 和空 Roll 的现有语义保留。

| 原 I-04 条件 | 本批与复用的有效证据 | 尚未满足的整项条件 |
| --- | --- | --- |
| 历史/幂等/余额索引 | 既有真实事件 differential、免费/未知成本和迟后归属；本批拒绝损坏预留金额/余额 key/lease，失败不泄漏 event-id 索引 | 其余余额/幂等读取及跨索引失败传播仍须逐入口核对；完整增长/目标容量接受未完成 |
| 原子水位 | 普通 Apply/整批回滚和认证 advance 的失败 root 保持；原生日志已提交事实保留；既有首位置/物理 duplicate/角色前缀用例通过 | 全角色启动、物理切换与完整故障矩阵沿用原残缺项 |
| 价格/治理/备份 cursor | 结算摘要、金额与 cut 统一校验；request recovery/pending 同步检查；既有 price 双流完整性、Governance idempotency/maturity 与备份 pricing 回归通过 | 未在本批接受全部公共入口迁移及生产/增长性能；受 D-02/I-03 依赖约束 |
| 锁序和失败封闭 | 新增 36 个结算故障、24 个预留故障和 1 个失败页前缀用例；既有 callback/Close join、owner 费用及取消检查通过 | 本批只改纯校验逻辑，没有改变锁或生命周期；全阶段空间/并发接受仍开放 |

检查记录集中在 [I-04 checks.json](../verification/performance/2026-10-08-i04-settlement-integrity/checks.json)：保留修复前两个失败日志、本批七个 Go 文件身份与四个修复前源码快照；最终 Linux Ledger 包 410 个顶层测试通过、8 个辅助/显式 opt-in 用例跳过，实际容器退出 0、无 OOM 并已删除。Budget、store/bolt、app 的受影响恢复/备份/Primary 定向回归通过。跳过的 ENOSPC/容量 profile 不算本批通过；没有运行全仓库 gate，纯校验修改不重复 race。

两类真实缺陷已闭环，但 **I-04 整项仍开放**，原计数保持 5 完成 / 19 开放。当前容量缺口仍为 128.386 MiB，本批节约 0 B。此处原定的余额/幂等索引批次已按用户确认执行，结果见 §11；原容量和全角色故障残缺项仍保留。

## 11. 原 I-04：余额/幂等索引与业务错误传播

本批直接补齐读取与写入前的同一校验。余额投影必须含全部五个非 null、非负整数；新账户的真实缺失仍可返回零余额，已有行的 `{}` 或 null 字段返回损坏错误。旧实现虽然点读拒绝负值，归约却会加载负余额并继续记账，甚至把 -1 加成看似合理的正数后推进水位；现在加载阶段拒绝。治理幂等索引的项目、操作、键、fingerprint 和目标 ID 校验也接入归约，损坏的 alias 不再静默跳过业务效果并推进 root。

首次事件位置的普通/认证读取都核对当前物理 cut；普通失败结果清为零且 found=false。归约中的既有首次位置必须属于**处理前**的 cut，后续重复事件不能把超前位置变合法。事件摘要行要求完整 32 字节数组，拒绝 Go JSON 解码原本会静默补齐的 31 字节数组和截断的 33 字节数组。失败不发布派生进展，已持久化 native 事实保留。没有改变原生格式、存储 schema、锁序、业务能力或资源配额。

| 子条件 | 结果与证据 | 明确范围 |
| --- | --- | --- |
| 余额行解码及归约 | 7 种损坏 × 普通/认证读取与 apply/advance，共 28 个场景通过 | 缺字段、null、五类负计数；拒绝、零结果、费用保留、不推进 cut/root |
| 治理幂等归约 | 6 种损坏 × 两后端，共 12 个场景通过 | 项目/操作/键/fingerprint/WorkUnit/Run 身份；保留已提交事实 |
| 首次事件位置与摘要 | 12 个读取场景和 6 个重复事件 advance 场景通过 | 当前 cut、半索引、摘要长度及处理前 cut；合法 duplicate/Roll/历史 differential 继续通过 |
| 业务错误传播 | 两个使用磁盘 Manager 的接口故障场景及既有 17 个准入错误/4 个已提交事实重试场景通过 | Manager 接口注入 Ledger 的 typed 损坏错误，验证拒绝预留/幂等重放，不新增 native 事实或保留 in-flight 金额；实际磁盘行损坏由上述 Ledger 场景验证 |

最终 [checks.json](../verification/performance/2026-10-08-i04-index-integrity/checks.json)记录：Ledger 定向通过，Linux Ledger 整包 **414 个顶层测试通过、8 个跳过**；Budget、Usage、store/bolt、app 受影响的业务/恢复/备份回归通过。首次整包保留 413 通过/1 失败/8 跳过：旧快照测试依赖 10ms 等待，快照开始过晚时返回全部 105 条的合法 cut，不能满足夹具要求的中间 cut。本批将夹具改为 Snapshot 实际写锁屏障，99 条后缀实际排入队列后复制，断言收紧为精确第 6 条；该测试定向 race 通过，再编译的整包通过。生产快照实现没有改动。跳过的 opt-in ENOSPC/容量 profile 不算本批接受；自有容器状态已记录并删除，没有全仓库/生产接受声明。

本批完成上述余额/幂等校验及所选业务拒绝条件；整项仍为 **5 完成 / 19 开放**，容量缺口没有变化。这里原定的租约/请求读取批次已按用户确认执行，结果见 §12。D-02/I-03、全角色物理切换、增长和目标容量接受保持独立残缺项。

## 12. 原 I-04：租约/请求读取与恢复依赖

本批修复普通 ReadLease/ReadRequest 只解码、认证及恢复租约主要核对逻辑 sequence 的缺口。点读和请求恢复分页现在共用身份、事件合法性及 generation/offset/sequence 校验；普通读取的行和校验水位来自同一 Bolt 事务，认证读取沿用同一 pin。现代请求的终结事实还必须匹配原接受项目、period/timezone version 和 WorkUnit/Run，物理与逻辑位置均在接受事实之后。损坏的原始请求不能在迟后 settlement 归约时推进水位或发布后继 root，已持久化 native 事实保留。

校验保留真实旧版顺序：lease-first、finalization-first、稍后 accept，以及只有 finalization 的行；不伪造 Accepted，也不把遗留 attempt 的跨 period 归属强改成现代请求归属。旧 generation 的较大 offset 和空 Roll 的有效 cut 仍保留。

| 子条件 | 本批结果 | 范围 |
| --- | --- | --- |
| 租约身份与物理位置 | 9 种损坏 × 6 个点读/分页入口，54 个场景通过 | 普通、认证、价格、恢复点读及两后端 request-attempt 页；错误点读返回零值/found=false，回调忽略错误仍拒绝完成 |
| 请求原始/终结事实 | 10 种损坏 × 5 个入口，50 个场景通过 | 普通/认证请求与终结读取、恢复点读；拒绝超前位置和现代归属变化 |
| 恢复分页与归约 | 6 个超前原始请求页、6 个迟后结算依赖故障通过 | 错误页无有效前缀/cursor，普通水位与认证 active.root 不推进，native 已提交结算保留 |
| 旧版兼容与消费者 | 8 种真实 native 到达顺序、既有 12 种遗留恢复/重启场景通过；Budget、Usage、store/bolt、app 定向回归通过 | 实际损坏行在 Ledger 入口验证；Usage 错误传播沿用接口故障用例，合法顺序/跨 period/迟后结算另有真实 Ledger 消费回归 |

共新增 **116 个故障场景与 8 个旧版正向场景**。修复前 76 个故障子用例失败的日志保留；最终源码对应的 Linux Ledger 整包 **419 个顶层测试通过、8 个跳过**，退出 0、无 OOM，自有容器已删除。8 个辅助/显式 opt-in 用例未执行，不算容量或 ENOSPC 接受。检查、源码身份和失败记录集中在 [本批 checks.json](../verification/performance/2026-10-08-i04-request-integrity/checks.json)，未新增状态说明 Markdown。纯行校验和私有读 helper 修改没有改变锁、生命周期、schema、原生格式或配额，按 AGENTS.md 不重复 race/全仓库 gate。

I-04 的三批选定行一致性条件已交付：结算/pending、余额/幂等、租约/请求及所选消费者失败传播。**I-04 整项尚缺 D-02/I-03 依赖、全角色物理切换/完整故障矩阵、全部公共入口迁移接受和增长/目标容量接受**；机器任务清单已移除本批解决的读取缺口。原计数保持 **5 完成 / 19 开放**，本批减费 0 B，必要 normal 仍为 789.386 MiB/54 claims，原容量仍缺 128.386 MiB。

这里原定的 **I-05 超新增准入预算的遗留恢复与中断重试**批次已按用户确认执行，结果见 §13。原要求保持为两个独立集合完整分页、持久化后重建重试、不丢失/重复释放/终结及 Usage 原摘要/迟后归属/无孤儿活跃额度；没有把这批替代十万冷迁移或完整 HA 接受。

## 13. 原 I-05：跨页遗留恢复与连续崩溃重试

本批核对现有两级恢复的缺失证据，没有发现需要修改生产恢复算法的缺陷。新增固定夹具含 **129 个既有 MAC 时代请求：77 个 pending attempts、104 个未 finalized requests，以及 25 个已 finalized 但仍待结算的请求**。包括 accepted-before-lease、未开始、已开始、已 settled 未 finalized、迟后 settlement；有 Run/无 Run、普通/认证后端和每页 1/64 行分别验证。新 Active 上限固定 128 MiB，每个新增请求逻辑收费 32 MiB，仅容纳 4 个；四个费用实际占满，第五个被拒绝。恢复两个独立集合仍完整完成，未申请新的 Active 费用。

| 原 I-05 条件 | 本批通过的具体范围 | 完整接受尚缺 |
| --- | --- | --- |
| 两个独立输入集合 | 8 个跨页/归属场景完整结清 77 attempts、终结 104 requests；包括只有接受事实的请求及已终结父请求的 pending attempt | 更大规模、原资源目标和完整公共角色启动 |
| 迟后归属及摘要保持 | 25 份既有 Usage 终结摘要逐份保持；Run 原归属金额正确；原 12 种真实旧版 lease-first/finalization-first/跨 period 恢复用例在整包继续通过 | 完整 HA 允许前缀、checksum-era 独立基线和生产接受 |
| 幂等及中断重试 | 10 个持久化/应用/确认错误场景，加 8 个真实 SIGKILL 场景；终结类场景先在结算时杀进程，重建后在终结时第二次杀进程 | 全部公共 Runtime 角色、物理切换、压力与增长矩阵 |
| 无孤儿活跃额度 | 最终 pending、账务/Run 预留、WorkUnit in-flight 及 Usage 活跃请求均为零；已提交金额精确 3850 微美元 | 完整角色 generation 生命周期及 I-04/I-06 依赖 |
| Usage 必需联动 | 增量与 fresh 磁盘重放均保持原终结摘要；129 个请求统计、账务金额及 native MAC cut 一致，未重建迟后活跃累加器 | 公共 HA 多组件追赶/发布及目标增长/资源接受 |

共 **26 个新增场景**。每轮完整验证执行 12 次真实 SIGKILL，其中四个场景连续杀进程两次；父进程核对实际 SIGKILL 信号、已 Sync 的边界标记及 native head/hash，区分已持久化未应用与已应用未返回。重试只从重新认证的 native 事实 fresh 构建，不使用被杀进程留下的候选 DB 授权。最终原生日志恰有 103 个 settlement、129 个 finalized，无重复终结事实；合法旧版顺序的既有证据继续保留。

保留两个夹具失败及修正：初始总逻辑预算 256 MiB 没有覆盖占满的 128 MiB Active 与嵌套读取工作区，认证恢复在初始确认前拒绝；夹具总额修正为 384 MiB，Active 上限未改。真实崩溃检查还发现旧普通测试 writer 使用非认证 active Replay 取回记录，导致 Record.Hash 为零；现在直接使用精确 AppendRecordWithReceipt，并核对实际 native head/hash。新 Usage 夹具也走 authenticated replay。**生产 writer/恢复算法未修改，夹具修正不是生产缺陷修复，也不改变原 2 GiB/1 GiB 目标。**

最终 Darwin 编译/default vet、Linux 定向和 Budget 整包通过；整包 **110 个顶层测试通过、1 个子进程 helper 在父进程跳过**，退出 0、无 OOM。helper 已在真正子进程中执行；自有运行容器已删除。检查、源码身份、失败日志和资源状态集中在 [I-05 checks.json](../verification/performance/2026-10-08-i05-legacy-recovery/checks.json)，没有新增状态说明 Markdown。仅修改三个测试文件，未重跑不受影响的 Ledger/Usage/app 整包、race 或全仓库 gate。

这里的 129 个请求超过固定的新增请求逻辑容量，不表示实际历史对象已超过 128 MiB；逻辑 384 MiB/256 MiB Disk 夹具及实际 2 GiB 隔离容器也不接受完整容量或峰值。I-05 已移除本批解决的跨页/超新增预算及所选中断缺项，仍保留公共 HA/依赖/资源/增长接受。原计数 **5 完成 / 19 开放**，本批减费 **0 B**，原必要 normal 仍缺 **128.386 MiB**。

下一批唯一主目标：**I-06 角色允许前缀缩小、Runtime generation 替换与失败回滚**，最多 2 小时。复用已有离线缩小前缀的 Runtime 重开及组件验证，核对未完成的完整切换路径：新视图只读取角色允许的认证前缀，不修改或接纳排除的 durable 尾部；旧 generation 撤销并 join 后才发布新视图；构建/发布失败不暴露混合 generation 或 ready，原 native 事实可重建重试。发现真实接线缺口直接修复；不重复运行不变的容量失败，不提前接受完整公共 HA。

## 14. 2026-10-08 用户裁定与路线调整

卡点复核：19 项开放的共同原因有三个。① 原 2 GiB 预检仍缺 128.386 MiB，凡固定 2 GiB 启动 HA 的任务（I-03、I-06、I-11、O-01、V-01、V-02）都被挡住，连功能夹具也会先被预检拒绝；② v8 十万冷迁移的写放大（一次 Ledger 构建读 10–11 GB、写 5.2 GB，净落盘约 83 MB）没有被逐事实随机改写认证树模型内的调参解决；③ D-04 依赖暂缓的 D-01，依赖链上的任务按定义无法关闭。

用户裁定：

| # | 裁定 | 对计划的影响 |
| --- | --- | --- |
| 1 | D-04 拆分为 **D-04a 本地目标 Linux 容量门槛**与 **D-04b 生产校准** | D-04a 不再依赖 D-01，可按本地固定输入关闭；D-04b 随 D-01 单列暂缓。依赖 D-04 的本地任务改依赖 D-04a |
| 2 | 允许**路线 A**：Ledger 请求池与 Metadata 接收窗口并入 foundation 共享执行池，池内为已接受工作的终结保留优先槽 | 独立终结保证的契约不变，实现由“独立池”改为“共享池内的终结优先预留”；必须证明终结不被后台/新工作饿死、取消与 Close 的 join 顺序不变 |
| 3 | 十万请求 / 1800 秒是真实目标，归入 **M3** | 冷构建算法改造（外部排序 + 按键折叠 + 自底向上建认证树）移到 M3；M1/M2 不以十万冷迁移为前置 |

调整后的顺序：

| 阶段 | 主目标 | 完成标志 |
| --- | --- | --- |
| S0 稳住工作树 | 全量 `go test ./...`/vet 恢复绿色；工作按模块拆成可审查提交；数字来源统一 | 全量测试通过并记录；文档只保留一份当前数字 |
| S1 预算闭合（路线 A 优先，路线 B 并行审计） | A：两池并入共享池并设终结优先槽，合集上限约 +0.27 MiB，乐观节约约 288 MiB；B：逐项核对 1387 MiB Recovery reserve 的同时活跃集合 | 原逻辑 2 GiB 预检通过，真实 TLS 双角色在实际 2 GiB cgroup 下完成启动、请求、checkpoint、两次重启和 Close |
| S2 逐项接受 | I-03 → I-06 → I-04/I-05 HA 部分 → I-11 → I-12 → I-07～I-10 → O-01/O-02 → V-01 → V-02（不含十万） | 每项原条件逐条有证据 |
| S3 (M3) 冷构建 | 先梳理 Ledger 归约的跨键依赖，再实现外部排序/自底向上建树；分级 1k → 10k → 100k | 1k 改善 ≥30%，10k ≤300 秒，100k ≤1800 秒 |

S0 首项已完成：I-06 夹具 `TestReplicaGenerationPublicOpenFailureRetriesPreserveNativeFacts` 原先在预检阶段就被拒绝（夹具的 Recovery 公式未计入 metadata window/inventory/归档读者）。改为显式私有 Recovery 余量后，6 个子用例全部命中预期的具体错误类型并通过。这是夹具修正，不构成容量接受，原计数仍为 **5 完成 / 19 开放**。

## 15. S1 第一批：请求终结并入共享 RAM 池

| 项 | 结果 |
| --- | --- |
| 共享池 | 新增优先成员（已接受请求）：排在已排队的后台 root 之前，同层 FIFO，不抢占正在持有的 root；请求的终结 Disk 作为私有 reserved claim，随成员退休释放 |
| 私有优先 | 私有终结池放得下时请求保留私有池（终结回调持有工作区等待 `WaitConfirmed`，私有池不会拖住共享池）；只有 `ErrExhausted` 才退回共享池，探测用 `Manager.fit`，不计入拒绝数 |
| 预检 | `HAPhysicalSharedProfiles` 加入“one Ledger request”；Metadata 窗口保持独立。真实 Linux 运行发现 Audit/Governance 原生物理批编码器各常驻 64 MiB IO 而向量未计，已补入常驻列表 |
| 数字 | 三后台共享 789.386 MiB → 请求并入后 597.044 MiB → 补齐两个常驻编码器后 **725.044 MiB**；原 661 MiB normal 下仍缺 **64.047 MiB** |
| 验证 | resourcebudget/config 包及 race 通过；两项反向验证（关闭优先插队、回退计入拒绝）均使新测试失败；默认 896 MiB 夹具下真实 TLS 双角色两测试在实际 2 GiB cgroup 通过（峰值 216608768 B，无 OOM）；原 1387 MiB 下 Replica 仍在 Metadata 接收窗口被 normal 总额拒绝 |

原计数 **5 完成 / 20 开放**，本批实际减费 192.341 MiB，但同时发现 128 MiB 未计常驻费用，原配置仍不成立。证据见 `docs/verification/performance/2026-10-09-s1-request-shared-lane/checks.json`（本地，目录被忽略）。

登记的开放项：

1. 剩余 64.047 MiB：候选为 Audit/Governance 批编码器并入共享池（需先核对它们与请求终结/后台的真实并发），或核算 64 MiB 批编码器的实际必要大小。
2. 启动失败后关闭时 Usage 报告“实际 IO 退休不确定”并留下共享成员：需判定是 fail-closed 设计还是失败路径清理缺陷。
3. 请求退回共享池时，HA 终结确认等待会串行化；只在私有池放不下时发生。
4. S0 遗留：`budget` 包单独运行 813.8 秒（3 个 I-05 遗留恢复测试占 776 秒），`ledger` 单独 361 秒但全量并行时超过 10 分钟，`usage`/`app` 同样超时；CI 的 `go test -count=1 ./...` 无 `-timeout`，会失败。

## 16. S1 第二批与 S0 测试时限

**S0 测试时限。** 默认 `go test` 运行 I-05 三个遗留恢复矩阵的代表性子集（每个维度至少覆盖一次），完整矩阵在 `HALRO_STRESS=1` 下运行；`budget` 包 814 秒 → 412 秒（后台另有负载）。SIGKILL 子进程期限 30 秒在并行负载下会被超时杀死并误判，放宽到 2 分钟，超时仍由 `childCtx.Err` 区分。Makefile `test`、CI 普通测试步骤及 CLAUDE.md 门禁命令加 `-timeout=20m`，与 `race` 一致。

**批编码器（路线 B）。** 最大 HA 批次（512 事件、约 8 MiB）在编码器内总分配 75.2 MiB，超过原 64 MiB 费用：`durableFrames` 无预分配扩容 42.3 MiB、逐帧分配 8 MiB、给复制回调的整批复制 8 MiB。改为按帧总长精确预分配、帧直接编码进缓冲、回调共享只读缓冲后，Governance 24.9 MiB、Audit 24.5 MiB（关闭 GC 的总分配上界）。费用降为 32 MiB，新测试以费用为阈值，撤回预分配即失败（实测 62 MiB）。audit/governance 整包通过。

**批编码器并入共享池（路线 A）未做。** Governance `AppendBatch` 会在请求处理路径中被调用；若在请求持有共享池时嵌套进入，合集需再增加一个槽，节约为零，且并发证据未取得。

| 必要 normal | MiB |
| --- | ---: |
| 第一批结束（含两个编码器 64 MiB） | 725.044 |
| 编码器降为 32 MiB | **661.046**（缺口 48128 B） |

**真实 Linux（原 1387 MiB，实际 2 GiB cgroup）新进展。** Replica 已越过 Metadata 接收窗口。两个测试同时运行时 online 测试报“物理分配不可用”，单独运行时消失：是前一失败测试未清理的残留占用同一 ext4，属测试隔离问题。两个测试现在卡在同一处：运行阶段读取 Replica Metadata 时，normal 已用 614.587 / 661 MiB，各类别与预检向量吻合，剩余不足一次普通读取工作区。

结论：预检向量只覆盖必要 owner，不含运行时普通读取；在原 1387 MiB Recovery 下，必要 owner 占满后没有正常读写余量。继续沿“逐项抠字节”推进不会得到有安全余量的配置，需要路线裁定（见汇报）。

仍开放：启动失败后关闭时 Usage 报告 `ownedfile.ErrRetirement` 且留下共享成员——`usage` 包内 45 处可能来源，需在 Linux 上复现失败启动后逐层追踪，未定位。原计数 **5 完成 / 20 开放**。

## 17. `internal/app` 分片与本轮暂停点

`internal/app` 空闲单跑 1924.5 秒，长尾为主，改为固定分片（`scripts/go-test-shard.sh`，`check` 模式证明分片不重不漏）：CI 与发布普通测试 4 片、race 8 片并行，Makefile 依次运行。9 个崩溃时点矩阵默认跑代表时点，发布 `stress` 任务以 `HALRO_STRESS=1` 运行全部。修复：上一批改坏的发布契约测试、两个未登记的 `authenticatedstate` fuzz 目标、`runtime_repack` 中 `ReadStats` 遇 `ErrDiskUsageBusy` 的时序竞争（改前 6/6 失败、改后 6/6 通过；改前提交同样复现，非本轮引入）。race 最重分片未实测，完整门禁未跑。

本轮在此暂停，续作见[续作交接](memory-bounded-resume.zh-CN.md)。拓扑问题已由用户裁定为三节点，见 §18。

## 18. Recovery reserve 论证：离线备份工作区不属于服务进程

用户裁定目标拓扑为**三节点**，3 组会话（2 peer + 1 替换）保留。逐项分解原 2 GiB 配置下 1453961408 B 的 `ValidateReplicationCandidates` 下限：会话 3×(96.02+64.02+96.02)=768.2 MiB、projection 工作区 192、accounting 128、**backup 工作区 128**、Metadata 映射 96、pricing IO 与归档读取各 32、其余约 10.5 MiB。

backup/restore 工作区只由离线命令获取，每个命令经 `commandBudgetContext` 自建管理器；没有命令作用域时 `acquireBackupProjectionBudget` 直接报错，无法记入服务进程的管理器。以 `OpenWithOptions`、`WaitForMemberMaintenance` 与全部 `*Runtime` 方法为根的 VTA 调用图（对 `Monitor.WithSampling` 按调用点传入的闭包做上下文敏感处理；`start`/`serve` 都经 `runRuntime` 进入）到达不了任何 backup、backup projection、staging、output、restore 所有者。离线命令进程不持有复制会话，其峰值仍由 `Validate` 的单机启动 Recovery 求和覆盖。

提交 `ddca5b72` 把 backup 工作区 RAM 与 backup/restore 两个 claim 移出 HA 公式。原 2 GiB 生产预算：

| | 候选公式下限 | 物理工作向量上限 | 2 GiB 可行区间 |
| --- | --- | --- | --- |
| 改前 | 1453961408 B（1386.606 MiB） | 约 1386.95 MiB | 实际为空（1387 被 normal 下限拒绝） |
| 改后 | 1319742656 B（1258.605 MiB） | 约 1386.95 MiB | 约 128 MiB |

新增 `TestOriginal2GiBHABudgetLeavesARecoveryWindow` 固定上下界；改写的 backup/restore 断言与新测试在改前公式下均失败（已确认回退确实生效）。

真实 Linux（逻辑 2 GiB、Recovery 1259 MiB、实际 2 GiB cgroup、3 GiB ext4）：peer credit 与 online checkpoint 两个双角色测试通过（原 1387 MiB 下两者均失败）。旧二进制在 1259 MiB 下同样通过：这两个夹具走 `validateSelectedHASessionBudget`，不经过配置门禁。因此运行时结论只说明“1259 MiB Recovery 加相应增大的 normal 足够”，配置门禁结论单独由上表证明。证据在被忽略目录 `docs/verification/performance/2026-10-09-recovery-offline-workspaces/`。

仍开放：这两个测试不覆盖启动、请求、checkpoint、两次重启与 Close 的完整生命周期接受；其余各项（accounting/projection/Metadata 映射与会话的真实重叠）未再缩减。若需更大余量，可在此基础上继续逐项论证。race 最重分片（2/8）实测 550.6 秒通过（有并发负载），不需增加片数。

## 19. 完整生命周期接受与失败启动后的重启

**生命周期**（`6663b8a2`）。`TestHAPhysicalLifecycleSurvivesTwoRestarts`：双角色三代（原始 + 两次重启），每代用新 Manager 从磁盘打开，复制 3 条事实、两次在线 checkpoint、完整终结与 Close；重启后的 Replica 必须读到此前所有事实，且 Seal 不回退。真实 Linux（逻辑 2 GiB、Recovery 1259 MiB、实际 2 GiB cgroup、3 GiB ext4、`GOMEMLIMIT=768MiB`）通过，cgroup 峰值 224350208 B，无 OOM；每代 ext4 可用空间仅减少约 40 KB。

**§3.3-1 结论：失败启动的清理不是缺陷，是 fail-closed 设计。** 跳过终结序列直接 Close 时，`closePhysicalLocked` 与 Usage `closePhysicalUsageLocked` 只关闭描述符，保留名称、extent 与费用供下次启动回放，并返回 `ErrRecovery`；Usage 加上 `ownedfile.ErrRetirement` 表示“保留 credit”，所以共享成员不归零；基础 Close 在该错误处提前返回（Lease、Publisher 等都保留）。同进程再次打开会因进程级 `sealPublishers` 登记返回 `ErrBusy`，这是防止复用被保留目录的守卫。生产路径不会同进程重开：`runRuntime` 只在 `ErrMaintenanceRequested` 且 Close 无错时循环，其余一律退出进程。§3.3-2 的“同一 ext4 残留”因此只是测试隔离问题。

**但下一个进程确实无法启动，查出两处问题：**

1. **缺陷，已修复（`9feaf834`）。** 失败启动保留的 cache target 仍绑定在 Publisher 上，下次启动走 `RestorePhysicalCandidate`；它在 Directory catch-up workspace 内用调用方 ctx 申请 4096 B 控制 claim，被路由成借款子项，随后 `RetainIndependentWorkspace` 正确拒绝，表现为 `HA physical common targets: invalid resource budget input or ownership`。Prepare 与 Restore 改为经 `WithoutRAMWorkspace` 正常准入。ext4 回归测试在修复前失败；resourcebudget/audit/governance/authenticatedstate/app 的 Physical 测试在修复前后结果逐项相同。
2. **容量契约缺口，未修复，待裁定。** 失败启动后 Replica 数据目录实际占用 1177169920 B（约 1.10 GiB，主要是 projection 物理 bank），启动 inventory 按设计把它计入，超过 `persistent_inventory_disk_bytes` 的 512 MiB，于是拒绝启动。配置校验目前不要求这个上限覆盖“稳态数据 + 一次失败启动保留的物理向量”。把夹具上限设为 1.5 GiB 后，`TestHAPhysicalFailedStartupIsReplayedByTheNextStart`（失败启动在子进程中执行并退出）通过：下次进程把可用空间从 1.83 GB 收回到 3.005 GB，后续两代之间相差约 33 KB。

**环境说明。** 名称含 ENOSPC 的测试要求小 ext4（≤512 MiB 或 ≤1536 MiB），在 3 GiB 镜像上必然失败；按对应尺寸重跑后，resourcebudget/audit/governance/authenticatedstate 全部通过。`internal/app` 中 `TestHASelectedPhysicalCommonCachesActualENOSPCAndFreshBothRoles`（压力下 join 报 `resource budget exhausted`）与 `TestHASelectedPhysicalBusinessOriginalWindowAtENOSPCActualTLS`（文件系统观测断言）在对应尺寸下修复前后都失败，属于既有问题，已登记待查。

**完整门禁。** `ddca5b72` 上 `make full-check` 失败：`ledger`、`usage` 包在并发负载下总时长超过 20 分钟（超时时刻正在运行的测试只跑了 3～5 秒，没有挂起），另有 `TestProjectedCollectorRetirementJoinsAdmittedWaiterAndBlockedIO` 单独运行时 138/200 失败。后者是测试断言了瞬时状态（等待者被撤回唤醒后释放 slot 本属正确），`3ea5e334` 改为确定性断言，500 次 0 失败、race 200 次通过；它随原工作树拆分提交引入，`main` 上没有。门禁需在空闲机器上重跑。

## 20. inventory 下限、门禁分片与 race 分配测试

**inventory 下限（用户裁定：由配置推导；`af7f2112`）。** 真实 Linux 失败启动后 Replica 保留的文件逐项落在所属域上限内：Ledger/Usage 投影 pages 各 512 MiB（上限 576）、directory pages 32 MiB（段索引 48）、Audit/Governance 日志各 16 MiB（64）、原生 WAL 32 MiB，合计 1177169920 B。单个 pack 由 `allocatePhysicalLocked` 限制在存储上限内，repack 目标在恢复时被限制在上限内，repack 期间新旧两个 pack 槽可以并存。`Validate` 现在要求启用的 `persistent_inventory_disk_bytes` ≥ 各派生磁盘上限之和的两倍（溢出时饱和到不可满足）；原生 Ledger 历史由运维在下限之上另行预留。原 2 GiB 生产预算改为 3586 MiB（下限 3074 + 原生 512），RAM 不变。已启用 inventory 且低于下限的配置会在校验时被拒，需要调大；不需要重新初始化数据目录。边界测试和稀疏文件超限测试都做了反向验证；真实 Linux 在线 / 生命周期 / 失败启动回放三个测试在推导值下通过，峰值 220 MiB，无 OOM。证据：`docs/verification/performance/2026-10-09-inventory-floor/`（被忽略）。

**门禁分片（`f06af594`）。** 第二次完整门禁在空闲机器上仍失败：`ledger`、`usage` 超过 20 分钟，`budget` 1051 秒。三者单独运行分别为 381 / 529 / 约 412 秒，测试都是顺序执行；ledger 在 381 秒里只用了 38 秒 user + 57 秒 sys CPU，所以与其他包并行时变慢几倍，是 fsync 争用而不是 CPU。`go-test-shard.sh` 改为按包分片（`shard PKG INDEX TOTAL`、`check PKG TOTAL`、`check-all`、`all`），`rest` 正好排除脚本中的唯一清单：app 4、budget 2、ledger 2、usage 3，race 翻倍。CI 与发布每片一个 matrix 项；发布契约测试从脚本清单推导两套 matrix 并逐项比较（删掉一项即失败，已验证）。第三次门禁中 `make test` 全部通过，各分片 46～495 秒。

**race 分配测试（`2e92187e`）。** 同一次门禁的 race 阶段，Audit/Governance 最大批次分配测试失败：race 下分配 38160016 B，超过 33554432 B 编码器费用。race 插桩会把栈分配移到堆上，普通构建实际为 25740920 / 26096792 B（约 22% 余量）。按 `scratchstate` 的先例，费用检查只在非 race 构建中运行。

## 21. 两个既有 ENOSPC 失败

两者在 2026-10-07 的证据中通过、在本分支第一个可编译提交 `c1550229` 上已失败；拆分前的历史没有提交，无法 bisect，改为在 `ErrExhausted` / `ErrPressure` 处临时打印调用栈直接定位（插桩已移除）。

1. **产品缺陷，已修复（`0ee44582`）。** `closeHAPhysicalNative` 先经 `withSelectedCut` 确认没有 peer 持有 Ordering 承诺，这次读取申请一个新的 4096 B IO 准入，满压力下被拒，导致终结关闭本身走不完（`TestHASelectedPhysicalBusinessOriginalWindowAtENOSPCActualTLS`）。检查改在 Directory catch-up workspace 内执行，与原生关闭和 `finishHAPhysicalBusiness` 用同一组已付费通道；拿不到该池时仍走正常准入，检查从不跳过，原生关闭仍在截面回调之外。同一树改前改后、同一 1536 MiB 镜像对照：改前失败，改后通过。
2. **测试过时，已修正（`fc537e71`）。** `TestHASelectedPhysicalCommonCachesActualENOSPCAndFreshBothRoles` 在预付 work 的借款内关闭 foundation；共享 RAM 池引入后该 work 是池成员，而 Close 最后退役的共享池不能在成员自己的借款内退役（`SharedCompletableWorkspace.Close` 返回 `ErrExhausted`）。改为先退还成员，再在满压力下关闭，保留原测试要守的性质：整个关闭不需要新的正常准入。512 MiB 镜像下通过。

修复后 `internal/app` 全部 Physical Linux 测试：3 GiB 镜像下除这三个需要小镜像的 ENOSPC 测试（填充 helper 拒绝大文件系统）外全部通过；三者在 512 / 1536 MiB 镜像下全部通过。证据：`docs/verification/performance/2026-10-09-enospc-regression/`（被忽略）。
