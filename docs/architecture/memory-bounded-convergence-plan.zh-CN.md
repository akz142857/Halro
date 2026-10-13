# 内存有界状态：交付计划与进度看板

更新：2026-10-08。此表从当前仓库证据出发，替代此前只描述执行顺序的计划。计划写成不代表实现完成；当前没有后台执行承诺或上线日期。

## 1. 当前基线与目标

- 原任务 25 项（D-04 于 2026-10-08 拆为 D-04a/D-04b）：5 项完成，20 项开放。其中 17 项属于本地工作；D-01 生产取证、D-04b 生产容量校准、R-01 发布及生产接受沿用此前暂缓范围。
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
| 压力迟滞 | DataDir/临时盘 bytes/inodes、未知信号拒绝和迟滞；备份 ENOSPC 在真实预检后发生。2026-10-10：固定容量配置下 24 轮真实内存/磁盘压力进出与磁盘迟滞区间连续两次通过（§32） | 动态输出盘完整接受；历史规模（D-04/P2）下的持续压力 |
| 有限等待 | 上述排队点硬上限，定向 race 核对超时/重试/已接纳工作保留 | 其余 owner/命令覆盖审计；不响应取消的物理 IO 继续记未退休 |
| 终结额度 | 原 Completion 池及超时后保留实际接受费用；终结、Close 有组件及实际 TLS 检查。2026-10-09：原 2 GiB 在 Recovery 1259 MiB 下可行（§18），满压力下终结关闭不再申请新准入且在真实 ENOSPC 下完成（§21）；三节点（1 Primary + 2 Replica）满压力终结与重启真实通过（§22）；2026-10-10：固定容量配置下 24 轮压力中每轮已接受终结完成、费用归零（§32）；HA 两节点（§33）与三节点（§34）严格传输、真实 ENOSPC 下 24 轮储备循环各连续两次通过；三节点真实采样器下磁盘/内存交替 24 轮连续两次通过（§35） | 历史规模（D-04/P2）下的持续压力 |
| ENOSPC | 新增命令备份密文实际失败与精确清理，既有八类 native/projection/Runtime 场景保留。2026-10-09：HA 双角色在真实 ENOSPC 下拒绝新复制写入、终结关闭无自有临时文件残留、重启后双端确认事实且无回收（§23） | 设计 §9.1 所列新旧投影共存、bbolt 扩页下的 ENOSPC 未单独注入（三节点真实 ENOSPC 变体 §27、归档多文件发布 §28、HA 激活迁移 §29、Standalone 迁移 §30、HA 储备保护 §31 已于 2026-10-10 通过） |

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

## 22. 三节点满压力终结与重启（I-03）

生命周期辅助函数推广为任意成员数：成员 0 为 Primary，其余为 Replica（各自的 bank 上限、接收窗口和必须就绪的 peer credit 窗口），每个事实必须到达每个 Replica；新增 `pressuredClose`，在所有成员满压力下执行终结与 Close。`TestHAPhysicalThreeMemberPressuredTerminalAndRestart`（`a7bef390`）基于三成员夹具，搭建目标三节点组（1 Primary + 2 Replica），走真实 TLS 物理 credit 复制事实并在线 checkpoint，三个成员全部在满压力下完成终结与关闭，重启后两个 Replica 都读到事实。

真实 Linux（逻辑 2 GiB、Recovery 1259 MiB、实际 2 GiB cgroup）。三个成员共用同一 ext4，4 GiB 镜像放不下两个 Replica 的完整 bank（第二个 Replica 接收窗口物理分配失败），改用 6 GiB；生产中每个节点各有磁盘。结果 5/5 通过，cgroup 峰值 122～169 MiB，无 OOM。未含 `0ee44582` 时，每次都失败在 `actual final native join: new resource admission stopped by pressure`。

**顺带修复（`75fc4012`）。** 三成员夹具约每 3 次失败 1 次：`SessionRegistry.Close` 把对端先关闭时的 `tls: failed to send closeNotify alert (but connection was closed anyway): ... connection reset by peer` 当作关闭失败。`crypto/tls` 只在自身 socket 关闭成功后才报这个告警，没有资源遗留；`PeerSession.Close` 原已对超时这样处理，现在把 ECONNRESET / EPIPE 一并视为无害（`retiredCloseNotifyError`），其他关闭错误照常返回。生产中 Close 出错会结束 Replica 维护循环（进程退出），所以这也是真实影响。单元测试已做反向验证。证据：`docs/verification/performance/2026-10-09-three-member/`（被忽略）。

## 23. 真实 ENOSPC 下的终结、拒绝与双端重启（I-03）

`TestHAPhysicalActualENOSPCTerminalAndBothRolesRestart`（`1f23dca5`）沿用双角色生命周期夹具：复制事实并在线 checkpoint 后，用外部文件把隔离 ext4 填到真实 ENOSPC 并置满压力；新的复制写入必须因 `ErrPressure` 被拒绝且 peer credit 释放；每个成员终结与关闭前都重新填满（不借用其他成员释放的块），关闭后不得残留自有临时文件；移除填充后下一次启动，Replica 必须读到全部事实，被拒事实在每个成员上都不存在，且启动不回收任何空间（完整关闭不保留回放向量）。生命周期辅助函数的两个布尔参数改为模式枚举。

**发现并修复的缺陷（`4363a46c`）。** 满压力下 Primary 申请 peer credit 会断开复制会话：Primary 的 reserve 在 Directory catch-up workspace 内读取截面，Replica 的应答却用 `withSelectedCut` 申请新的 4096 B IO 准入，被压力拒绝；负回执同样失败，handler 返回错误，会话结束。之后每次重连都失败（发送 Replica 窗口需要同一读取），Primary 的 `rpc` 只等调用方 ctx（期间持有 controller 的 entry 读锁），ticket 保持 live。调用栈经临时插桩确认（已移除）。修复：Replica 观察在接收读屏障内借用 catch-up workspace（与在线 checkpoint、confirmed-prefix 应用相同的加锁顺序），授予、负回执、窗口发送与构造都走已付费通道；写入本身仍在 Primary 上被压力拒绝。

**证据**（真实 Linux，逻辑 2 GiB、Recovery 1259 MiB、实际 2 GiB cgroup）。同一测试、同一 3 GiB 镜像：去掉修复（已确认回退生效）后在 reserve 的 30 秒上限处失败，加上后通过，另重复 3 次通过；新测试与在线 checkpoint、两次重启生命周期、失败启动回放、TLS peer credit 同批通过，cgroup 峰值 250 MB，无 OOM；1536 MiB 业务 ENOSPC、512 MiB caches/derived ENOSPC、6 GiB 三节点满压力测试全部通过；3 GiB 下全部 `Physical` 测试除三个小镜像用例外通过。证据：`docs/verification/performance/2026-10-09-enospc-restart/`（被忽略）。

**仍开放。**
1. 已关闭（§24）：会话在 `rpc` 等待期间断开导致 ticket 结果未知、挡到重启的问题。用户裁定采用原样重发，不采用窗口序号推断。
2. `TestSelectedRuntimeCompressionReopensOldCommonRootAndPublishesPhysicalLocator`（单机，不经过 HA 路径）在 3 GiB 全部 `Physical` 批次中偶发失败：修复后 7 次中 1 次（`disk Usage projection is retired` + `context canceled`），未修复 4 次中 0 次，单独运行 10/10 通过。未归因于本次修改，待查。

## 24. 回复丢失：原样重发与被放弃 ticket 的解决（I-03 有限等待）

**裁定。** 用户采用方案 B：会话断开导致 credit 回复丢失时，只以 Replica 对同一请求的回答为准，不用新窗口的 `TicketSerial` 推断“未持久化”。依据：Replica 上 Reserve 对同一 ticket 幂等（`old.Active` 且参数一致返回成功），Release 对已失效 ticket 幂等，无法授予的 Reserve 会持久记录拒绝并占用该序号（`s.Serial = serial`），之后同一 ticket 不可能再被接受；序号推断没有这种持久隔离。

**实现（`ee252ff0`）。** 一次 credit 交换上限 30 秒（含重发）；对端会话结束（`OnDataSessionClosed`）立即唤醒等待者，等 Replica 新会话的窗口到达后原样重发。`releaseUnexposed` 对结果未知的 Reserve 同样重发取得确定结果，不再直接拒绝。所有者出错返回时 ticket 标记为被放弃，下一次 reserve、准入暂停或关闭会先完成它；仍在所有者手中的 ticket 不动。此前 `peerPhysicalTerminalAdmission` 在 reserve 出错时直接丢下 ticket，没有任何代码再完成它。Replica credit handler 增加 context 观察者（沿用 `haPhysicalFinishObserverKey` 惯例，生产中不存在），供测试在指定点结束会话。

**顺带发现的既有缺陷（`a4fa6384`）。** reserve 在发送任何请求前先为每个 peer 记录本地镜像。三节点下若第一个 peer 拒绝或未回答，第二个 peer 的镜像已持久化但请求从未发出，本地释放后镜像已消耗该 peer 的下一个序号，Replica 仍期望这个序号，于是之后对该 peer 的每次 reserve 都被拒，直到换新窗口（在线 checkpoint）或重启。现在对“镜像已记录但未发出”的槽位先原样发送，让 Replica 以授予（随后释放）或持久拒绝消耗同一序号；镜像未记录的槽位照旧本地释放。由三节点回复丢失测试发现。

**测试（`f62769f1`）。** `TestHAPhysicalLostCreditRepliesResolveByResend`（两节点）与 `TestHAPhysicalThreeMemberLostCreditRepliesResolveByResend`（三节点）：Replica 在 Reserve 持久化之后、应用之前、以及每次尝试直到超过上限时结束会话；前两种必须在新会话上恰好重发一次解决（各约 0.3～0.5 秒），第三种留下被放弃的 ticket，由下一次 reserve 先完成；所有事实到达 Replica 并在重启后保留。三节点变体覆盖一个 peer 已回答、另一个未发出的混合状态。三节点夹具准备提取为 `prepareHAPhysicalThreeMemberFixture`。

**证据**（真实 Linux，逻辑 2 GiB、Recovery 1259 MiB、实际 2 GiB cgroup）。反向验证：去掉重发与被放弃 ticket 处理（只保留观察者，已确认回退生效），两节点测试在第一种情况卡到夹具 4 分钟期限后失败；去掉 `a4fa6384`，三节点测试在被放弃 ticket 之后的 reserve 失败。最终版本：3 GiB 下全部 `Physical` 测试 30 项通过（三个小镜像用例除外，它们在 512/1536 MiB 下通过）；6 GiB 三节点两项通过；两节点重发测试累计 5 次通过。首次在容器内用官方 go1.26.6 linux-arm64（sha256 已核对）构建 race 版：两节点/三节点重发、TLS peer credit、在线 checkpoint、两次重启生命周期、失败启动回放、真实 ENOSPC 重启、三节点满压力全部通过，无数据竞争，cgroup 峰值约 585 MB。证据：`docs/verification/performance/2026-10-09-lost-reply/`（被忽略）。

**仍开放。** 管理审计批次路径（`admin_audit_physical.go`）在 ticket 结果未知时仍把审计批次所有者标记为退休；ticket 现在会被后续操作解决，但该所有者本身的退休状态未改。会话旧连接的关闭通知若晚于新会话上的重发到达，会让这次重发被视为丢失，最坏情况是等到 30 秒上限，再由后续操作解决，不会产生错误结果。


## 25. 维护 head 校验遇锁忙不再锁死恢复

**起因。** 门禁第 3 轮在 race 分片失败：`TestSelectedRuntimeSeventyLiveRollsAndCompressionUseFiniteDirectoryPages` 报 “create selected accounting manager: verify maintenance Ledger prefix: authenticated snapshot reader busy”。`ValidateNativeWriter` 在维护 head 下校验 projection 与 native writer，这些校验只尝试加锁；读者或写者短暂持锁时返回 `ErrBusy`，`MaintenanceLease.WithHead` 把它当作校验失败并锁定 accounting recovery，选定运行时启动失败。

**修复（`deb9f257`，合入 `d237342c`）。** 只因锁忙被拒的校验什么也没读，不能证明前缀有问题，`WithHead` 不再为此锁定恢复；忙与损坏或来源不一致同时出现时照旧锁定。`ValidateNativeWriter` 对这种拒绝按退避重试，上限 30 秒，与其他准入等待阶段一致。

**测试。** `TestAuthenticatedWriterValidationWaitsOutAMomentarilyHeldLock` 持 projection 锁 50 ms：修复前复现与门禁完全相同的错误，修复后通过。

## 26. projection 页包的准入背压

**裁定。** 用户选择方案 1：在页包写满之前回收并削减新业务，而不是放大页包上限。

**问题。** 公共 checkpoint 的 repack 是回收过时页的唯一途径，但只按定时器运行。突发负载先写满 Usage 页包；满页包无法归约 checkpoint 必须发布的积压，于是此后每次 checkpoint 都失败；Ledger 页包写满会在 append 内拒绝 Apply 并锁定 accounting recovery。`authenticatedstate` 也只在 append 中途碰到上限时才发现，失败事务已写入页面，store 自我中毒，所有者随之退休。

**实现。**
1. `authenticatedstate`（`f676433d`）：剩余空间不足一个 append 上限时，整笔更新在写入任何页面前以 `ErrCapacity` 拒绝，store 保持健康且为最新；剩余空间放不下两次更新时 repack 即可进行；`PackUsage` 无等待报告页包用量。
2. `usage`（`290b0b93`）：归约遇 `ErrCapacity` 干净停止，之前的更新连同水位完整提交，下次追账从停止处继续；不再算作导致退休的变更，live 路径不再包装为 `ErrRecovery`。live 追账每笔事务归约至多 8 条记录、共享页路径重写；`PackAdmission` 报告单条记录最大追加字节与用量。
3. `ledger`（`2fad7085`）：同样记录并通过 `PackAdmission`/`PackUsage` 报告。
4. `app`（`5621ee6d`，`runtime_projection_pack.go`）：任一页包用掉上次 checkpoint 后剩余空间的一半即触发 checkpoint；页包剩余空间不足“观测到的单条最大字节 × 2 × 仍欠记录数”（Usage 未归约后缀加每个已准入请求 8 条）时，网关以 503 加 `Retry-After` 削减新业务（与 checkpoint 期间相同），并立即唤醒 checkpoint。已准入请求照常完成；没有观测记录的页包不削减。

**测试。** 800 个请求进 64 MiB Usage 页包、600 个请求进 16 MiB Ledger 页包，经同一 handler 票据全部落地，Usage 合计正确，Close 干净；无准入时 Usage 突发约在第 300 个请求写满页包，Close 失败。另有 1800 请求长突发通过。`authenticatedstate` 新增 `TestStoreRefusesUpdateWithoutAppendRoomAndRecoversAfterRepack`、`TestStoreRepackQualifiesWhenAppendRoomIsLow`。

**合入（`dcb6746a`）后发现的缺陷（`3db39877`）。** 补跑 race 时 `TestSelectedRuntimeLedgerBurstReclaimsPackBeforeItFills` 在第 384 个请求失败：“apply durable Ledger record: … authenticated snapshot reader busy”。`Store.PackUsage` 以 `TryLock` 取 store 锁读取用量，而 `update` 对同一把锁也只尝试加锁、失败即 `ErrBusy`；准入对每个请求轮询 `PackUsage`，于是轮询恰好持锁时，另一个请求 append 内的 Apply 被拒，accounting 进入恢复。现在 store 在持锁改动 extent、物理所有者或关闭状态处把用量发布到原子视图，`PackUsage` 不再加锁。新增 `TestPackUsageNeverHoldsTheStoreLockAgainstAnUpdate`：修复前 0.1 秒内以同一错误失败（已确认测试确实加入旧代码），修复后通过。

**门禁第 5 轮失败与测试修复（`9c88ee24`）。** `88bff47f` 上的第 5 轮在 app 第 4 分片失败：`TestDefaultRuntimeUsesActivatedSevenRootsAndReopensActualBusiness/refuse_damage_data_memory-audit-1.cache` 报 “damaged selected artifact opened business”。在 `c640bd40`、`d237342c` 上各 3 次通过，从 `f676433d`（本分支第一个提交）起每次失败。原因：repack 在放不下两次更新时也会进行，夹具的在线 checkpoint 因此多发布一次 revision（`liveRevisions` 2 → 3，最终 revision 13 → 14）；启动按 `revision % 2` 读取缓存槽位，最终 revision 为偶数时 1 号槽是不被读取的旧槽，测试写死损坏 1 号槽，Open 正确地忽略了它。产品的 fail-closed 行为未变：损坏所选的 0 号槽仍被拒绝。测试改为损坏最终 revision 对应的槽位，在本分支（0 号槽）与 `c640bd40`（1 号槽）上均通过。

**回归。** 见续作交接 §4-1；随后在测试修复后的提交上跑门禁第 6 轮。

**仍开放。** 单机压缩测试的偶发失败（`context canceled` 原因链，§23-2）仍未归因，与 §25 的锁忙失败不是同一现象。

## 27. 三节点真实 ENOSPC 下的终结与重启（I-03）

本批 2026-10-10 14:25 开始，固定输入为 `dca5324d` 加新测试，约 25 分钟完成，未到 2 小时上限。

`TestHAPhysicalThreeMemberActualENOSPCTerminalAndRestart`（`8fa01a1c`）组合 §22 的三成员夹具与 §23 的 ENOSPC 收尾模式：在线 checkpoint 并把事实复制到两个 Replica 后，用外部文件把共用 ext4 以 `fallocate` 填到 4 KiB 粒度的真实 ENOSPC 并置满压力；新写入需要两个 Replica 的 credit，必须因 `ErrPressure` 被拒且 credit 释放、不留 live ticket；每个成员终结前重新填满，关闭后不得残留自有临时文件；移除填充后下一次启动，两个 Replica 都读到全部事实，被拒事实在三个成员上都不存在，且启动不回收空间。没有修改生产代码。

**证据**（真实 Linux，逻辑 2 GiB、Recovery 1259 MiB、实际 2 GiB cgroup、6 GiB ext4）：4/4 通过，每次约 5.5 秒，cgroup 峰值 130～179 MB，无 OOM。反向验证：回退 `4363a46c`（已确认 revert 生效，`ha_peer_physical_controller.go` 41+/54−）后在 `peer credit at actual ENOSPC … context deadline exceeded` 失败，即 reserve 的 30 秒上限，与两节点测试一致。证据：`docs/verification/performance/2026-10-10-three-member-enospc/`（被忽略）。

**I-03 ENOSPC 条件仍缺：** HA 储备保护、归档多文件发布、迁移故障（对应设计 §9.1 最后一条“迁移、新旧投影共存、bbolt 扩页及归档暂存中的 ENOSPC 注入”）。“HA 储备保护”在文档中未单独定义，下一批先写明判据：到停止线时停止新增工作，且早于储备被侵占（设计 §6.1）。

## 28. 归档多文件发布的真实 ENOSPC（I-03）

本批 2026-10-10 14:47 开始，固定输入为 `08f63a47` 加新测试，约 10 分钟完成，未到 2 小时上限。

**测试。** `TestDiskExportMultiFilePublicationAtActualENOSPC`：导出按日期段逐个发布分区文件，再提交目录页与 manifest。先在同一 ext4 上做一份不受干扰的参考导出，再把文件系统以 `fallocate` 填满，每次尝试前只多释放一个 4 KiB 块，让 ENOSPC 依次落在根目录、日期目录、parquet 列缓冲、分区文件、条目暂存、目录页与 manifest。每次失败必须返回 ENOSPC、不发布前沿（无 manifest）、无 `.archive-*`/`.tmp` 残留、已发布的分区与参考导出逐字节一致；空间足够后同一 exporter 复用这些分区完成，位置与文件集合与参考导出完全一致并通过校验和验证。Parquet 与 NDJSON 两种格式。

**发现并修复的缺陷（`6d8cfb8d`）。** Parquet writer 从导出缓冲池以临时文件借用列页缓冲，成功时归还；写入失败（包括 ENOSPC）时丢下手中的缓冲，文件留在日期目录，池计数非零又附加 `ErrDiskUsageCorrupt`，把磁盘满报成损坏。每次在 ENOSPC 下重试导出都会再留下一批，恰在空间不足时继续占用空间。现在缓冲池记录每个缓冲及其创建身份，writer 关闭后对未归还的缓冲关闭并按身份核对后删除；只有 writer 成功却仍持有缓冲才报 `ErrDiskUsageCorrupt`。

**证据**（真实 Linux，512 MiB ext4，2 GiB cgroup）：修复后 3/3 通过，每次结果一致——Parquet 失败 91 次（其中 38 次发生在已有分区发布之后），NDJSON 失败 18 次（15 次）。修复前（同一测试，run-2）Parquet 子测试在残留的 `.archive-column-*.tmp` 处失败，NDJSON 通过。本地 `TestDiskExport|TestArchive` 全部通过。证据：`docs/verification/performance/2026-10-10-archive-enospc/`（被忽略）。

**登记的相关开放项（不在本批范围）。** 进程在写入中被 SIGKILL 时留下的 `.archive-*.tmp`（分区、列缓冲、条目暂存、目录页、root 等）没有任何启动或维护步骤回收；ENOSPC 路径现在会精确清理，崩溃路径仍会累积。

**I-03 ENOSPC 条件仍缺：** HA 储备保护、迁移故障。

## 29. HA 激活迁移的真实 ENOSPC（I-03）

本批 2026-10-10 14:58 开始，固定输入为 `3cd8780f` 加新测试，约 15 分钟完成，未到 2 小时上限。

**测试（`b4747a2b`）。** `TestHAMemoryActivationAtActualENOSPCResumes` 沿用 SIGKILL 恢复测试的六个激活阶段钩子（`sources-qualified`、`namespace-created`、`seven-roots-built`、`bootstrap-pending-synced`、`common-seal-published`、`bootstrap-activated`），在指定阶段把隔离 ext4 填到真实 ENOSPC。激活只允许在已持有空间内完成，或以该 ENOSPC 失败，不得出现其他错误，且不残留自有临时文件；释放后必须完成激活：与崩溃后相同，默认激活必须拒绝覆盖被中断的状态，`Restart` 才能恢复；结果须通过独立的七根与原生日志审计读取。没有修改生产代码。

**证据**（真实 Linux，3 GiB ext4，逻辑 2 GiB、Recovery 1259 MiB、实际 2 GiB cgroup）：4/4 通过，cgroup 峰值 204～253 MB，每次结果一致——`sources-qualified` 以 ENOSPC 失败、默认重试即完成；`namespace-created`、`seven-roots-built`、`bootstrap-pending-synced` 以 ENOSPC 失败、默认激活拒绝、`Restart` 恢复；`common-seal-published`、`bootstrap-activated` 之后的写入落在已持有空间内，激活完成。四个失败阶段都返回真实 ENOSPC，说明注入确实到达后续写入。证据：`docs/verification/performance/2026-10-10-migration-enospc/`（被忽略）。

**仍缺。** Standalone 迁移（`halro metadata migrate` → `ActivatePreparedMemoryRuntime`）没有阶段钩子，本批未覆盖其 ENOSPC；设计 §9.1 所列“新旧投影共存、bbolt 扩页”下的 ENOSPC 也未单独注入。I-03 ENOSPC 条件另缺 HA 储备保护。

## 30. Standalone 迁移的真实 ENOSPC（I-03）

本批 2026-10-10 16:57 开始，基于 `b03ebf1a`，约 10 分钟完成，未到 2 小时上限。

**生产代码只加观测点（`e3066371`）。** `ActivatePreparedMemoryRuntime` 之前没有阶段钩子。本批拆出不导出的 `activatePreparedMemoryRuntime`，在六个持久边界调用 `phase`：`sources-qualified`、`roots-built`、`bootstrap-pending-synced`、`startup-caches-persisted`、`common-seal-published`、`bootstrap-activated`。导出函数传 nil，行为不变。

**测试。** `TestStandaloneMigrationAtActualENOSPCRecovers` 按 `halro metadata migrate --activate-runtime` 的顺序执行三步：命名空间、从备份重建 Metadata、七根激活。共 11 个注入点：命名空间 4 个（`before-staging-create`、`intent-created`、`staging-synced`、`namespace-synced`）、Metadata 重建前 1 个、激活 6 个。在注入点用外部文件把隔离 ext4 填到真实 ENOSPC，判据如下：
- 迁移要么在已持有的空间内完成，要么以该 ENOSPC 失败，不得出现其他错误。
- 不残留自有临时文件。
- 原生 Metadata 和日志逐字节不变。
- 只要还有迁移名字存在，普通启动必须拒绝。
- 释放空间后按文档恢复：已有公共 seal 用 `--resume-activation`，否则用 `--restart-preparation` 从同一备份重来。
- 恢复后默认 Runtime 能打开，迁移前已结算的请求费用仍在。

每次尝试使用新的预算 manager，与真实失败进程退出后的情形一致。

**证据**（真实 Linux，3 GiB ext4，Recovery 1259 MiB）：
- 3/3 通过，cgroup 峰值 199～247 MB，每次结果一致。
- 前 9 个注入点都以真实 ENOSPC 失败，`--restart-preparation` 均能恢复。
- `common-seal-published` 和 `bootstrap-activated` 之后只剩硬链接和 unlink，落在已持有空间内，迁移照常完成。所以 `--resume-activation` 在 ENOSPC 下实际不可达，本批没有走到这条路径；它的中断恢复由既有 SIGKILL 测试覆盖。
- 反向验证：把 restart 保留清单里的 `memory-audit-1.cache` 去掉后，`startup-caches-persisted` 以 `file exists` 失败；还原后通过。
- macOS 上受影响的激活与 CLI 迁移回归通过。

证据目录：`docs/verification/performance/2026-10-10-standalone-migration-enospc/`（被忽略）。

**运维含义。** restart 会把被中断的世代原样保留在 `.interrupted-N` 和 `.unqualified-N` 里，不删除。因此 ENOSPC 之后，要释放的空间至少能再放下一份完整的迁移输出。连续失败最多保留四代，用尽后须先手动归档。

**仍缺。** I-03 ENOSPC 条件只剩 HA 储备保护；设计 §9.1 所列“新旧投影共存、bbolt 扩页”下的 ENOSPC 也还没有单独注入。

## 31. HA 储备保护：未观测到的 ENOSPC（I-03）

本批 2026-10-10 17:05 开始，基于 `9a0197b3`，约 15 分钟完成。

**判据（用户 2026-10-10 确认）。** 按设计 §6.1：到达停止线时 HA 两端拒绝新工作，且发生在终结储备被侵占之前；已接受的终结仍能用储备完成。

**为什么测“未观测到的” ENOSPC。** 物理终结储备在准入前就真实 fallocate（`physicalTerminalAdmission` 依次预留确认窗口、Metadata 和 Ledger 的原生/COW 终结范围），普通写入只能在未偿终结债务之上分配。所以储备被侵占只可能发生在采样延迟窗口：文件系统已满，压力状态仍是 Healthy。§21/§23 的测试都是手动置满压力，没覆盖这个窗口。

**测试（`d8a9131a`）。** `TestHAPhysicalReserveHoldsAtUnobservedENOSPC` 跑两节点真实 TLS，分 `one-accepted`、`two-accepted` 两个子测试。先接受请求并等确认前缀被 Replica 应用，再用外部文件把隔离 ext4 填满，**不切换压力**。判据：
- 新准入必须被拒绝。单请求子测试先做一次对照准入并关闭，证明逻辑预算还有余量，因此填满后的拒绝只能来自物理层。实测为 `physical completion allocation unavailable`。
- 两端在线 checkpoint（后台写入方）可以运行也可以拒绝，但不得迫使节点进入恢复。实测在有活动窗口时，它在写入前就以 `ErrPhysicalCredit` 拒绝，从 `withAdmissionPaused` 入口返回，不污染 controller。**（2026-10-10 更正，见 §33：这个夹具用的是普通传输，没有 peer credit controller，`withHAPhysicalAdmissionPaused` 在入口就返回 `ErrPhysicalCredit`，与是否有活动窗口无关；这一条只证明 checkpoint 没有写入，没有覆盖“有空间也能 checkpoint”。在生产形态的严格传输下，有活动窗口时 checkpoint 同样以 `ErrPhysicalCredit` 拒绝、窗口结束后有空间即可完成，由 §33 的诊断证实。）**
- 被拒绝的分配可能回退它短暂占用的块，所以终结前再填一次，确认可用块为 0。随后每个已接受请求都必须完成终结，且 Replica 应用了终结事实。
- 释放空间后，同一成员必须能再次准入，证明两种拒绝都不会遗留状态。
- 两端在重新填满的盘上完成公共发布和关闭，不残留自有临时文件。

与双请求业务窗口测试共用的启动代码抽为 `startHAPhysicalBusinessPair`，带一个可选的预算调整参数。

**夹具容量。** 固定的 HA 物理预算只容纳 1 个在途请求，原因有两个：
- 每个请求的完成工作区为 192 MiB，normal RAM 只放得下一份。
- Primary 的 80 MiB Ledger/Usage 投影只放得下一个窗口的最坏 COW 债务，再加一次普通追加。第二个请求的 `RequestAccepted` 以 `authenticated snapshot limit exceeded` 拒绝，发生在调用上游之前，属于失败封闭。

本测试为第二个请求补了逻辑 RAM，并把投影上限调到 Replica 夹具同值的 576 MiB。两个窗口之后，Metadata 终结预算在逻辑上就耗尽了，所以双请求子测试的拒绝来自逻辑预算，物理拒绝只由单请求子测试证明。这些调整不是容量接受。

**证据**（真实 Linux，3 GiB ext4，Recovery 1259 MiB）：
- 新测试和两个共用启动代码的既有业务测试同批 5/5 通过，cgroup 峰值 168～245 MB。共用代码的 ENOSPC 业务测试在 1536 MiB 镜像下通过。
- 第一版测试在 3 次里有 2 次失败，失败在测试自己的前置检查：被拒分配回退后多出 1 个空闲块。已改为终结前再填一次，失败记录保留在 `attempt-1/`。
- 反向验证（均已确认缺陷注入生效）：
  - 让终结的 COW 更新不使用预留窗口，两个子测试都在 `accepted terminal at ENOSPC` 失败。
  - 让原生终结帧额外申请原生范围，测试仍通过：原生文件已分配的范围足够覆盖，不会触到 ENOSPC。所以本测试不能发现这一类缺陷，这里如实记录。

证据目录：`docs/verification/performance/2026-10-10-ha-reserve/`（被忽略）。

**结论与仍缺。** 停止线早于储备侵占，这一点由准入前的物理预分配保证，不依赖压力采样。本批在采样延迟窗口内实测：新准入被物理层拒绝，后台 checkpoint 不争用储备，已接受终结在零空闲块下完成并复制到 Replica。仍缺：
- 三节点版本。
- 双请求下的物理拒绝。
- 导出、备份、重建等其他后台写入方在 ENOSPC 下的争用。本批只测了在线 checkpoint，它在有活动窗口时根本不进入写入。

I-03 剩余：见 §32 结论。

## 32. 固定容量配置下的持续压力（I-03）

本节第一部分在运行实验前提交，结果按设计 §9.2 写在后面，门槛事后不调宽。

**范围。** 本批只回答 I-03“压力迟滞”和“终结额度”两行尚缺的“持续压力/恢复”：在既有固定容量配置下反复进入、退出真实压力，看准入、终结、迟滞、费用和内存是否漂移。历史规模（十万/百万）与业务 SLO 属于 D-04/P2，仍受十万冷迁移超期阻断，本批不覆盖、不宣称。上游是本地免费的确定性夹具，物理终结储备未启用（与 2026-10-07 负载回归相同），HA 不在范围内。

**固定配置（运行前提交）。** `TestRuntimeSustainedPressureLinux`（opt-in `HALRO_RUNTIME_SUSTAINED_PRESSURE=1`）：
- 环境：ubuntu:24.04 arm64，容器内存 2 GiB、无 swap、2 CPU，`GOMAXPROCS=2`、`GOMEMLIMIT=768MiB`；隔离的 3 GiB ext4。
- 配置：`selectedCapacityBudget` / `selectedCapacityPressure` 原样不改。磁盘停止线 128 MiB、退出线 256 MiB；runtime 进入线 1 GiB、退出线 900 MiB；采样间隔 100 ms。
- 流程：24 轮，内存压力（触碰 1 GiB 缓冲）与磁盘压力（填到可用约 64 MiB）交替。每 3 轮做一次维护（Metadata 增长到 repack 阈值、Roll、公共 checkpoint，并核对旧 pack 退役），共 8 次。每轮依次为：
  - 32 个顺序请求，采集延迟；
  - 64 个并发请求的突发；
  - 持有 1 个已接受请求，进入真实压力；
  - 发 8 个新请求，必须全部拒绝且上游调用为 0；
  - 已接受请求必须完成；
  - 解除压力，回到 Healthy。

  磁盘轮先只释放到进入线与退出线之间，压力状态须保持 1 秒，然后再全部释放。
- 硬门槛：
  - cgroup 峰值 ≤ 1.5 GiB，且 `max`/`oom`/`oom_kill` 均为 0（每个采样点都检查）。
  - 进入压力 ≤ 5 秒，回到 Healthy ≤ 10 秒。
  - 迟滞区间内不离开 Pressured。
  - 恢复后的顺序请求全部成功，每轮突发至少成功 1 个。
  - 顺序请求 p95 ≤ 500 ms、p99 ≤ 1 s。这是本地夹具上的工程筛选值，不是生产 SLO。
  - 结束时 GC 后 live heap 不超过第一次维护后的基线 +32 MiB。
  - FD 不增加，Active/Completion 费用为零，`ActiveRequests` 为零。
  - Usage 与原生账务精确一致，新进程重开后状态一致。
- 吞吐、突发拒绝数、进入/退出耗时只记录，不设门槛。

**v1 结果：未通过自身门槛。** 6 次运行，前 5 次的失败都出在测试本身或负载形态，第 6 次碰到了门槛没有预料到的设计内行为：
1. 核对已接受请求的终结事实时，读取发生在仍处于压力的时刻。读取属于新工作，压力下被正确拒绝。已改为恢复 Healthy 后再核对，门槛不变。
2. 第 0 轮之后，持有请求被拒绝；但测试只等待“到达上游”，没有失败出口，挂到 18 分钟总时限。已加失败出口和逐轮日志。
3. 第 5 轮出现 `rate_limit_exceeded`：夹具 Project 的 RPM/TPM 是业务策略，不属于容量配置，实验中关闭（并发上限不变）。
4. 第 9 轮再次出现 `rate_limit_exceeded`：进程内调用全部同源，触发了按源地址的前置限流，实验中设为 0。这是代理后部署支持的配置。
5. 第 10 轮：已经回到 Healthy，顺序请求却得到 503 + `Retry-After: 1`，上游调用为 0。

   第 6 次运行带诊断复现，并对照代码确认是 §26 的投影 pack 回收准入：剩余空间不够已接受工作的欠账时，新工作以 503 让出，同时触发即时 checkpoint。这是设计内的有界让出，不是缺陷。但 v1 的“恢复后顺序请求全部成功”没有预料到它，按规则判 v1 未通过。

   前 9 轮的压力进入耗时为 22～182 ms，退出耗时为 115～205 ms，均远低于门槛，迟滞区间没有提前离开。

**v2 登记（运行前提交）。** 其余门槛全部不变，只增加一条：被 pack 回收拒绝的请求（以响应正文 `memory state page reclamation is in progress` 识别）可按 `Retry-After` 重试，每次让出必须在 5 秒内解除。让出次数和最长等待只记录，不设门槛。其他任何拒绝仍判失败。延迟只统计成功的那一次尝试。

**v2 结果：未通过。** 两次运行：
- 第一次在第 6 轮失败，原因是 `provider_unavailable: every provider for this model is suspended`，此时还没有挂起诊断。
- 第二次加了路由挂起快照，在第 10 轮遇到 `memory state checkpoint is in progress`（503 + `Retry-After: 1`）。这是在线 checkpoint 暂停期间的设计内让出，v2 只认了 pack 回收那一种。

上游挂起没有复现，原因未明，诊断保留在 v3 中。

**v3 登记（运行前提交）。** 对照代码列出 Standalone 业务路径上全部带 `Retry-After` 的设计内让出，只有在线 checkpoint 暂停和 pack 回收两种，其余属于 HA/集群。v3 把这两种都纳入可重试，每次让出须 5 秒内解除；其余门槛不变，其他任何拒绝仍判失败。

**v3 第一次运行：夹具缺陷。** 第 10 轮（约 60 秒）出现 `all_candidates_suspended`（`Retry-After: 29`）。挂起快照显示是 deployment 作用域，原因为空、代码 `unknown`、期限 30 秒，来自周期性主动部署探测。

测试的合成上游经 `LegacyAdapterBridge` 包装。bridge 总是实现 `Prober`；内部适配器没有 `Probe` 时，它返回 `provider profile does not support connection testing`，探测循环随即以阈值 1 挂起该 deployment 30 秒。v2 第一次运行的上游挂起也是这个原因。之前的负载回归只跑 4 秒，到不了 30 秒的探测周期，所以没有暴露。

已给合成上游补 `Probe`，返回健康。门槛不变，提交后重跑。

**顺带登记（不在本批修改）。** `health.go` 的探测循环注释写明“没有探测端点的适配器不得因此被禁用”，但经 bridge 包装后，这条保护对任何未实现 `Probe` 的适配器都失效。生产中的 OpenAI、Anthropic、Gemini、Bedrock、Mantle 适配器都实现了 `Probe`，所以当前没有生产影响，属于潜在的不一致。

**v3 第二次运行。** 24 轮全部跑完，Close 之前的门槛都通过，Close 之后的 FD 检查为 9→10。测试自己的磁盘填充文件是在 FD 基线之后打开、测试结束时才关闭的。现改为先关闭它再比较，关闭前记录全部 FD 的指向；失败时也列出指向，用来确认多出的那个是什么。门槛不变。

**v3 结果：连续两次通过（第 3、4 次运行）。** 两次运行的输入相同，只差新进程断言（`574ab8f4` 要求子进程输出包含 `--- PASS`）。

逐项对照门槛，两次结果分别为：
- cgroup 峰值 1434 / 1471 MB，不超过 1.5 GiB（1611 MB）；`max`/`oom`/`oom_kill` 均为 0。峰值里含 1 GiB 的压力缓冲。
- 进入压力最长 138 ms，回到 Healthy 最长 201 / 193 ms，门槛分别为 5 秒和 10 秒。
- 磁盘轮的迟滞区间里都没有离开 Pressured。
- 压力下 192 个新请求全部被拒，上游调用为 0；24 个已接受请求全部完成。
- 恢复后的顺序请求全部成功。
- 顺序请求 p50 / p95 / p99 为 262 / 433 / 449 ms，门槛为 p95 ≤ 500 ms、p99 ≤ 1 s。
- 设计内让出共 3 次，最长等待约 1.0 秒，门槛 5 秒。
- GC 后 live heap 为 13.8 MB，基线 14.5 MB，没有漂移。
- FD 9→9；Active/Completion 费用、`ActiveRequests` 均为零。
- Usage 与原生账务精确一致。
- 新进程重开通过（第 4 次运行的输出中有 `--- PASS: TestRuntimeMemoryLoadFreshProcessHelper`）。
- 总耗时 238 秒，完成 8 次维护。

**只记录的数据。** 成功 816 次，约 3.4 次/秒；突发拒绝 1512 次；上游同时活跃峰值为 **1**。也就是说，固定容量配置在这个夹具上每次只接纳 1 个在途请求，64 路突发中每轮只成功 1 个。这和 2026-10-07 负载回归的结论一致，是固定配置本身的准入上限，不是这次压力造成的。

证据目录：`docs/verification/performance/2026-10-10-sustained-pressure/`（被忽略），包括：
- `v1/`、`v2/` 两版失败记录；
- v3 的四次运行；
- 各次的基准提交（`base-*.txt`）；
- 关闭填充文件前的 FD 清单，证实 9→10 多出的是测试的填充文件。

**结论。** 在固定容量配置、单机 selected Runtime、本地合成上游下，I-03“压力迟滞”和“终结额度”两行的“持续压力/恢复”已有可复现的通过证据。整项 I-03 仍不关闭，还缺：
- 历史规模下的持续压力，受 D-04/P2 的十万冷迁移超期阻断；
- HA 物理终结储备下的持续压力；
- 动态输出盘的完整接受；
- “所有权移交”“有限等待”两行原有的缺口。

实验过程中修正了 4 个测试或夹具问题，登记了 1 个潜在不一致（bridge 使“没有探测端点不禁用”的保护失效），没有发现产品缺陷。本批 17:19 开始，约 50 分钟完成。

## 33. HA 物理终结储备下的持续压力（I-03）

本节第一部分在运行实验前提交，结果按设计 §9.2 写在后面，门槛事后不调宽。本批 2026-10-10 23:25 开始，基于 `9a861004`。

**范围。** 回答 §32 结论里“HA 物理终结储备下的持续压力”一条：把 §31 的单次储备循环（接受 → 真实 ENOSPC → 拒绝新准入 → 已接受终结用储备完成 → 恢复）在两节点真实 TLS 上重复多轮，看储备、费用、空间、内存是否漂移，以及两端能否在最后的 ENOSPC 下关闭并重新打开。不覆盖：三节点、双请求下的物理拒绝、其他后台写入方（导出、备份、重建）的争用、历史规模；压力状态由测试设置，不经过真实采样器（与 §21/§23/§31 相同），ENOSPC 是真实的。

**固定配置（运行前提交）。** `TestHAPhysicalReserveSustainedPressure`（opt-in `HALRO_HA_SUSTAINED_PRESSURE=1`，需隔离 ext4）：
- 环境：与 §31 相同的 runner，ubuntu:24.04 arm64，容器内存 2 GiB、无 swap、2 CPU，`GOMAXPROCS=2`、`GOMEMLIMIT=768MiB`，3 GiB ext4。
- 配置：`startHAPhysicalBusinessPair` 的固定物理配置，加 §31 单请求子测试同样的调整（第二份完成工作区的逻辑 RAM、Primary 投影 576 MiB），使每轮能做对照准入。不是容量接受。
- 流程：24 轮。偶数轮是“未观测”的 ENOSPC（压力保持 Healthy，即采样延迟窗口）；奇数轮在填满后把两端设为 Pressured（已观测的停止线）。每轮：
  - 接受 1 个请求，再做一次对照准入并关闭，证明逻辑预算有余量；等 Replica 应用；
  - 外部文件填满 ext4；奇数轮设压力，并核对普通 IO 被 `ErrPressure` 拒绝；
  - 新准入必须被拒绝；偶数轮的拒绝不得只是逻辑预算耗尽；
  - 两端在线 checkpoint 可以运行或拒绝，但不得让节点进入恢复；
  - 再填一次，确认空闲块为 0，然后完成已接受终结；Replica 应用终结事实；
  - 删除填充、解除压力，第一次新准入必须成功；
  - 两端 Active/Completion 费用与 `CompletionReserved` 为零。

  每 3 轮在有空间时做一次两端在线 checkpoint（共 8 次），Seal revision 必须恰好 +1 且不进入恢复。第一次 checkpoint 后记录基线：GC 后 heap、ext4 可用空间、FD 数、两端预算快照。
- 结束：两端在 ENOSPC 下（每端前重新填满）完成公共发布与原生关闭，不残留自有临时文件；删除填充后用新的 Manager 重新打开同一目录，两端都必须读到全部 24 个终结，新准入成功，再正常关闭。
- 硬门槛：
  - 每个已接受终结在 ENOSPC 下 ≤ 5 秒完成；每次确认 + Replica 应用 ≤ 5 秒。
  - 每轮新准入被拒绝，恢复后第一次准入成功。
  - 每轮结束两端准入费用为零；结束时两端 `Used`（RAM、Disk）与 normal claims 在 5 秒内回到不高于基线。
  - GC 后 heap ≤ 基线 + 32 MiB；ext4 可用空间 ≥ 基线 − 32 MiB；FD 数 ≤ 基线。
  - 两端 ENOSPC 关闭无自有临时文件；重开后两端读到全部终结。
  - cgroup 峰值 ≤ 1.5 GiB，`max`/`oom`/`oom_kill` 均为 0（由 runner 记录判读）。
- 只记录：每轮拒绝的错误、终结最长耗时、每轮可用空间、总耗时。

**v1 结果：未通过，判据本身写错。** 第 1 次运行在第 0 轮结束时的费用检查失败（cgroup 峰值 220 MB，无 OOM）。该轮其余步骤都已通过：新准入以 `physical completion allocation unavailable` 被物理层拒绝，已接受终结在零空闲块下完成并被 Replica 应用，恢复后第一次准入成功。失败时 Primary 的 Completion 类占用 270393344 B，`CompletionReserved` 为 230080160 B。“两端准入费用为零”照搬了 §32 的单机判据，但 HA 物理夹具的常驻所有者在启动时就持有 Completion RAM，零不是这个夹具的空闲值。按规则判 v1 未通过。（v2 的打开快照修正了这里的判断：常驻的只有 Completion 类 270393344 B，`CompletionReserved` 打开时为 0，v1 看到的 230 MB 是尚未退还的瞬时值，见 v2 结果。）

**v2 登记（运行前提交）。** 只改参照：每轮结束时两端的 `CompletionReserved` 与 Active/Completion 类占用，必须等于打开后、第 0 轮之前的空闲快照（日志 `HA_SUSTAINED_OPENED`），结束时的静默检查也以它为准。其余门槛全部不变。

**v2 结果：未通过。** 第 1 次运行（cgroup 峰值 194 MB，无 OOM）：
- 打开快照：两端 Completion 类 270393344 B，`CompletionReserved` 与 Active 为 0。
- 第 0 轮（未观测）全部通过：物理拒绝，终结 10.7 ms，费用回到打开快照。
- 第 1 轮（已观测）在 Replica 读取终结事实时失败：`new resource admission stopped by pressure`。读取属于新工作，压力下被正确拒绝，与 §32 v1 第 1 条相同，是测试把读取放在了解除压力之前。此前该轮的新准入以 `new resource admission stopped by pressure` 被拒，已接受终结完成，确认与 Replica 应用在 5 秒内完成。
- 中止后的清理中，两端 Close 报 `owned file actual IO retirement is uncertain`（v1 同样出现）。这是测试在终结发布前中止、直接 Close 的路径，不在本实验判据内，记录待查。

**v3 登记（运行前提交）。** 两处，均不调宽门槛：
- Replica 读取终结事实移到解除压力之后。
- 每轮的费用检查改为在已登记的 5 秒静默上限内等待回到打开快照（v1 的瞬时值说明退还是异步完成的）；超过 5 秒仍判失败。

**v3 结果：未通过。** 第 1 次运行（cgroup 峰值 192 MB，无 OOM）：第 0、1 轮全部通过（含已观测轮的 Replica 读取与费用回到快照），第 2 轮结束时有空间的在线 checkpoint 在 Primary 上返回 `original Replica physical IO promise unavailable or changed`（`ErrPhysicalCredit`），未进入恢复。对照代码：checkpoint 入口 `withAdmissionPaused` 经 `RequireNoPeerPhysicalPromises` 要求没有未了结的 Replica 物理承诺，否则拒绝；生产中定时 checkpoint（`runUsageMaintenance`）对任何错误只记 `memory state common checkpoint deferred`，下个周期再试。这是设计内的推迟，v3 只按一次严格调用判读，没有预料到它。

**v4 登记（运行前提交）。** 有空间的在线 checkpoint 若以 `ErrPhysicalCredit` 被拒，且节点未进入恢复，可每 50 ms 重试，须在 5 秒内成功；推迟次数与最长等待只记录。其他错误、进入恢复或超过 5 秒仍判失败。其余门槛不变。

**v4 结果：未通过。** 第 1 次运行（cgroup 峰值 206 MB，无 OOM）：第 0、1 轮通过，第 2 轮有空间的 checkpoint 在 5 秒内一直以 `ErrPhysicalCredit` 被拒，不是瞬时推迟。

**诊断（临时测试，未提交；证据 `diag-1`…`diag-4-*`）。**
- 在原夹具上，连一个请求都没有的情况下有空间的 checkpoint 也失败：`startHAPhysicalBusinessPair` 用 `prepareSelectedTransport`（普通传输），不安装 peer credit controller，`withHAPhysicalAdmissionPaused` 在入口就返回 `ErrPhysicalCredit`。这个夹具根本不能做在线 checkpoint；§31 对 checkpoint 拒绝原因的解读因此有误，已在 §31 更正。
- 生产在启用物理储备时用 `prepareSelectedTransportWithPhysicalCredit`（严格传输），准入走 `peerPhysicalTerminalAdmission`（peer credit + 本地物理储备）。给夹具加严格模式（沿用生命周期测试的 Metadata 576 MiB、inventory 下限 + 512 MiB、Replica 接收窗口，Metadata journal 上限须在激活前设定，否则 Seal 与独立基线不符）后：无请求、两个请求、ENOSPC 下终结三种变体中，有空间的 checkpoint 都在 66～303 ms 内成功；ENOSPC 下有活动窗口时 checkpoint 以 `ErrPhysicalCredit` 拒绝，不进入恢复。
- 严格路径上 Primary 的 credit controller 只有一个 live ticket（`q.live`），有请求在途时任何新准入都直接以 `ErrPhysicalCredit` 拒绝。原登记里的“对照准入”在这条路径上不可能成立，在途时的拒绝也证明不了物理停止线。

**v5 登记（运行前提交）。** 改用生产形态的路径，判据相应调整，数值门槛不变：
- 夹具：严格传输 + `peerPhysicalTerminalAdmission`，固定物理配置加上述严格模式上限，不再加第二份完成工作区和 576 MiB Primary 投影；同一时刻只有 1 个请求在途。
- 每轮：接受 1 个请求 → 填满（奇数轮设压力）→ 在途时新准入必须被拒（原因只记录）→ checkpoint 可拒不得进恢复 → 再填满、零空闲块下完成终结，Replica 应用，释放 ticket → **仍满盘、无在途时新准入必须被拒；偶数轮（未观测）必须是 `ErrPhysicalAllocation`** → 删除填充、解除压力 → Replica 读到终结、第一次准入成功 → 费用在 5 秒内回到打开快照。
- 每 3 轮的 checkpoint 之后等待 Replica 的新 credit 窗口（与生命周期测试相同）；v4 的推迟重试（5 秒）保留。
- 重开同样用严格模式。其余门槛（终结 ≤ 5 秒、确认与应用 ≤ 5 秒、heap/空间/FD/预算漂移、ENOSPC 关闭无临时文件、重开读到全部终结、cgroup）不变。

**v5 结果：未通过。** 第 1 次运行（cgroup 峰值 204 MB，`max`/`oom`/`oom_kill` 均为 0）：
- 打开快照：Primary Completion 类 270393344 B，Replica 371671040 B，`CompletionReserved` 均为 0。
- 第 0、1 轮通过。在途时的新准入在两轮都以 `ErrPhysicalCredit`（live ticket）被拒；无在途时，第 0 轮（未观测）以 `HA original Ledger terminal window: physical completion allocation unavailable` 被拒，第 1 轮（已观测）以压力被拒。
- 第 2 轮（未观测）无在途时的新准入**成功**。前提有误：已接受请求的储备释放后可能把预分配的块还给文件系统（§31 在终结发布前再填一次就是这个原因），此时已不是 ENOSPC；第 0 轮被拒只是那次释放没有还回足够的块。

**v6 登记（运行前提交）。** 释放已接受请求后再填一次，确认空闲块为 0，再做无在途准入的判定；其余不变。

**v6 结果：连续两次通过。** 两次运行的输入相同（`b7d3738c`）。逐项对照门槛，两次结果分别为：
- cgroup 峰值 221 / 188 MB，不超过 1.5 GiB；`max`/`oom`/`oom_kill` 均为 0。
- 24 轮全部完成。每个已接受终结都在零空闲块下完成，最长 31.6 / 32.4 ms（门槛 5 秒）；确认与 Replica 应用都在 5 秒内完成。
- 在途时的新准入 24 次全被拒，原因都是 live ticket（`ErrPhysicalCredit`）。无在途、满盘时的新准入 24 次全被拒：未观测的 12 轮都是 `HA original Metadata terminal window: physical completion allocation unavailable`（`ErrPhysicalAllocation`），已观测的 12 轮都是压力拒绝。
- 恢复后第一次准入每轮都成功；Replica 每轮都读到终结。
- 每轮的准入费用都回到打开快照；结束时两端 `Used` 与 normal claims 不高于基线。
- 8 次有空间的在线 checkpoint 都一次成功，没有推迟；最长 686 / 574 ms。Seal revision 每次恰好 +1，不进入恢复。
- GC 后 heap 20.76 MB / 20.74 MB，基线 20.66 / 20.73 MB；ext4 可用空间比基线少 13.3 MB（门槛 32 MiB，两次一致，只记录）；FD 不增加。
- 两端在 ENOSPC 下完成公共发布与原生关闭，无自有临时文件；严格模式重开后两端都读到 24 个终结，新准入成功，正常关闭。
- 测试耗时 17.0 / 13.4 秒。

**回归。** 共用改动后夹具代码的 4 个既有测试在同一二进制上通过：3 GiB 镜像下 `TestHAPhysicalReserveHoldsAtUnobservedENOSPC`（两个子测试）、`TestHASelectedPhysicalBusinessOriginalWindowActualTLS`、`TestHASelectedPhysicalReplicaSameGenerationDoesNotConsumeCheckpointWindows`；1536 MiB 镜像下 `TestHASelectedPhysicalBusinessOriginalWindowAtENOSPCActualTLS`。

**反向验证。** 把 Ledger 投影终结 COW 更新的预留窗口换成 `nil`（补丁存于证据目录 `reverse-1.patch`，已确认注入生效、构建后已还原），测试在第 0 轮以 `accepted terminal at ENOSPC: physical completion allocation unavailable` 失败。

**只记录。** 两次通过的日志末尾各有两条 Replica 侧 `HA selected replication session failed ... EOF`，按顺序对应两次 release（重开前、结束时）关闭 Primary 传输。v1～v5 中止后的清理里，未完成终结发布就直接 Close 时两端报 `owned file actual IO retirement is uncertain` 或 `authenticated store requires reopen`，这是 fail-closed 的中止路径，不在本实验判据内，未进一步核对。

证据目录：`docs/verification/performance/2026-10-10-ha-sustained/`（被忽略），包括：
- v1～v5 的失败记录；
- `diag-*` 诊断运行；
- v6 的两次通过；
- `existing-*` 回归；
- `reverse-1`；
- 各版基准提交 `base-*.txt` 与 runner。

**结论。** 在两节点、严格传输（生产形态的 peer credit + 本地物理储备）、固定物理配置、压力状态由测试设置、真实 ENOSPC 下，I-03“终结额度”一行的“HA 物理储备下的持续压力”已有可复现的通过证据：
- 24 轮里停止线在储备被侵占之前生效：未观测时由物理预分配拒绝，已观测时由压力拒绝。
- 已接受终结都用储备完成，Replica 都应用了终结事实。
- 费用、空间、heap、FD 都没有漂移，两端可以关闭并重开。

本批另外查明，§31 用的普通传输夹具不能做在线 checkpoint，§31 已更正。严格路径上 Primary 同一时刻只有 1 个在途请求，这是 live ticket 的设计，和固定配置“只容纳 1 个在途请求”一致，不是本批造成的。

I-03 仍不关闭，还缺：
- 历史规模下的持续压力（受 D-04/P2 阻断）；
- 三节点版本、真实压力采样器下的 HA 版本；
- 动态输出盘的完整接受；
- “所有权移交”“有限等待”两行原有的缺口；
- 其他后台写入方在 ENOSPC 下的争用（§31）。

实验过程中修正了 5 个测试或夹具问题，没有发现产品缺陷。本批 23:25 开始，23:41 完成，约 16 分钟。

## 34. 三节点下 HA 物理终结储备的持续压力（I-03）

本节第一部分在运行实验前提交，结果按设计 §9.2 写在后面，门槛事后不调宽。本批 2026-10-10 23:41 开始，基于 `369af07e`。

**范围。** §33 结论里“三节点版本”一条。目标拓扑是三节点（§2-6 用户裁定），每个请求要从两个 Replica 各取得 credit，两者都必须应用终结。其余不覆盖项与 §33 相同：压力状态由测试设置，不经过真实采样器；不含其他后台写入方的争用和历史规模。

**固定配置（运行前提交）。** `TestHAPhysicalThreeMemberReserveSustainedPressure`（opt-in `HALRO_HA_SUSTAINED_PRESSURE=1`，需隔离 ext4）：
- 与 §33 v6 共用同一测试主体 `exerciseHAPhysicalReserveSustainedPressure`，成员数为 3（1 Primary + 2 Replica）。夹具辅助函数从两成员推广到 N 成员（`openHAPhysicalBusinessGroup`）：每个 Replica 用 Replica 上限与接收窗口，Primary 等待两个 Replica 的 credit 窗口。三节点 fixture 沿用 §27 的 `prepareHAPhysicalThreeMemberFixture`（Metadata journal 上限在激活前设定）。
- 环境：容器内存 2 GiB、无 swap、2 CPU，`GOMAXPROCS=2`、`GOMEMLIMIT=768MiB`；ext4 镜像 6 GiB（与 §27 相同，两个 Replica 的完整 bank 需要至少 3800 MiB）。测试进程时限 10 分钟。
- 流程与 §33 v6 完全相同，只是“Replica 应用”“Replica 读到终结”“等待 credit 窗口”对两个 Replica 都要成立，压力设置、checkpoint、ENOSPC 关闭与重开对三个成员都要成立。
- 硬门槛与 §33 v6 相同，数值不变：终结 ≤ 5 秒；确认 + 两个 Replica 应用 ≤ 5 秒；每轮新准入被拒（未观测轮无在途时须为 `ErrPhysicalAllocation`），恢复后第一次准入成功；每轮费用 5 秒内回到打开快照；checkpoint 推迟须 5 秒内解除；heap ≤ 基线 + 32 MiB，ext4 可用空间 ≥ 基线 − 32 MiB，FD ≤ 基线，三端 `Used`/normal claims 不高于基线；三端 ENOSPC 关闭无自有临时文件；重开后三端读到全部 24 个终结；cgroup 峰值 ≤ 1.5 GiB，`max`/`oom`/`oom_kill` 为 0。
- 通过标准：连续两次通过；两成员版本在改动后的辅助函数上复跑一次通过；反向验证（同 §33 注入）须让三节点版本失败。

**v1 结果：未通过，失败在填盘辅助函数。** 第 1 次运行（cgroup 峰值 207 MB，`max`/`oom`/`oom_kill` 为 0）：
- 24 轮全部通过，结束时的漂移与静默检查也全部通过：8 次 checkpoint 无推迟，最长 325 ms；终结最长 37.9 ms；heap 26.85 MB，基线 26.84 MB；ext4 可用空间比基线少 19.9 MB；FD 不增加。拒绝原因与 §33 v6 相同：在途时都是 live ticket，无在途满盘时未观测轮是 `ErrPhysicalAllocation`，已观测轮是压力。
- 结束阶段：Primary 在 ENOSPC 下完成发布与关闭后，为成员 1 填盘时，`exhaustHAPhysicalFixture` 的探测写入没有遇到 ENOSPC。Primary 关闭后两个 Replica 的会话断开，它们异步退还接收窗口持有的块，块在 fallocate 与探测写入之间被释放。两节点版本只有一个 Replica，没有暴露。这是测试辅助函数的竞争，不是被测系统的问题。

**v2 登记（运行前提交）。** `exhaustHAPhysicalFixture` 的探测写入如果找到空间，就继续填，最多 8 遍，直到一次写入真实遇到 ENOSPC；最后的 `Bavail == 0` 检查保留。这个辅助函数也被 §21～§31 的测试使用，改动只让它在并发退还块时多填几遍，结束条件不变，所以对共用它的测试也做回归。其余门槛不变。

**v2 结果：连续两次通过。** 两次运行的输入相同（`b4a0d068`），6 GiB ext4。逐项对照门槛，两次结果分别为：
- cgroup 峰值 173 / 133 MB；`max`/`oom`/`oom_kill` 均为 0。
- 24 轮全部完成，终结最长 22.1 / 31.9 ms；确认与两个 Replica 应用都在 5 秒内。
- 拒绝原因与 §33 v6 相同，各 12 次：在途时是 live ticket；无在途满盘时，未观测轮是 `ErrPhysicalAllocation`，已观测轮是压力。恢复后第一次准入每轮都成功，两个 Replica 每轮都读到终结。
- 8 次 checkpoint 都一次成功，没有推迟，最长 352 / 312 ms。
- heap 26.85 / 26.89 MB，基线 26.76 / 26.81 MB；ext4 可用空间比基线少 19.9 / 19.8 MB（门槛 32 MiB，只记录；两节点为 13.3 MB，多出一个 Replica 的原生增长）；FD 不增加；三端预算回到基线。
- 三端在 ENOSPC 下关闭，无自有临时文件；重开后三端都读到 24 个终结。
- 两次都出现 1 次 `fill pass 1 found returned blocks`，即 v1 的竞争在两次运行中都真实发生，并由新的辅助函数处理。
- 测试耗时 17.3 / 16.3 秒。

**回归**（同一二进制）：
- 3 GiB 镜像下 7 个测试通过：两节点持续压力（§33）、`TestHAPhysicalReserveHoldsAtUnobservedENOSPC`、两个业务窗口测试、`TestHAPhysicalActualENOSPCTerminalAndBothRolesRestart`（§23）、`TestHAMemoryActivationAtActualENOSPCResumes`（§29，6 个阶段）、`TestStandaloneMigrationAtActualENOSPCRecovers`（§30，11 个注入点）。各注入点的结果与原记录一致。
- 1536 MiB 镜像下 `TestHASelectedPhysicalBusinessOriginalWindowAtENOSPCActualTLS` 通过；6 GiB 镜像下 `TestHAPhysicalThreeMemberActualENOSPCTerminalAndRestart`（§27）通过。
- 第一次回归误用了 6 GiB runner，同样全部通过，记录保留在 `3m-regress-6144-misrun/`，不计入证据。

**反向验证。** 同 §33 的注入（Ledger 投影终结 COW 不用预留窗口；已确认生效、构建后已还原），三节点版本在第 0 轮以 `accepted terminal at ENOSPC: physical completion allocation unavailable` 失败。

证据目录：`docs/verification/performance/2026-10-10-ha-sustained/` 下以 `3m-` 开头的条目（被忽略），以及 `base-3m-*.txt` 与 `runner-6144.py`。

**结论。** 目标拓扑（1 Primary + 2 Replica）在严格传输、固定物理配置、真实 ENOSPC 下，24 轮储备循环连续两次通过：停止线在储备被侵占之前生效，已接受终结都用储备完成并被两个 Replica 应用，费用、空间、heap、FD 没有漂移，三端可以关闭并重开。实验修正了 1 个测试辅助函数的竞争，没有发现产品缺陷。

I-03 仍不关闭，还缺：
- 历史规模下的持续压力（受 D-04/P2 阻断）；
- 真实压力采样器下的 HA 版本；
- 动态输出盘的完整接受；
- “所有权移交”“有限等待”两行原有的缺口；
- 其他后台写入方在 ENOSPC 下的争用（§31）。

本批 23:41 开始，23:48 完成，约 7 分钟。（§33、§34 初稿写的 01:00～02:00 和“约 1 小时 15 分”“约 45 分”是推算而非读钟，按提交时间更正。）

## 35. 真实采样器下三节点 HA 储备的持续压力（I-03）

本节第一部分在运行实验前提交，结果按设计 §9.2 写在后面，门槛事后不调宽。本批 2026-10-10 23:59 开始（worktree 分支创建时间），基于 `a56aa669`，在单独的 worktree（分支 `memory-bounded-ha-sampler`）中进行，同时第 9 轮门禁在主工作区运行；实验容器与门禁共用本机 CPU，时间类结果因此偏保守。

**范围。** §33/§34 结论里“真实压力采样器下的 HA 版本”一条。§21、§23、§31、§33、§34 的压力状态都是测试手动设置的；本批每个成员的 Manager 由其数据目录上的真实采样器持有，接法与 Runtime 相同（`NewSampler` → `NewMonitor` → `AttachManager` → `Run`），挂上后 `SetPressure` 被拒，测试无法改写压力。拓扑为目标的三节点。§33/§34 已证明的“未观测窗口内的物理拒绝”不再重复，本批回答：采样器能否及时进入、离开压力，迟滞区间是否保持，储备循环在采样器驱动下是否完整、无漂移。不覆盖：历史规模、其他后台写入方的争用、动态输出盘。

**固定配置（运行前提交）。** `TestHAPhysicalThreeMemberReserveSampledPressure`（opt-in `HALRO_HA_SUSTAINED_PRESSURE=1`，需隔离 ext4）：
- 环境：与 §34 相同，容器内存 2 GiB、无 swap、2 CPU，`GOMAXPROCS=2`、`GOMEMLIMIT=768MiB`，6 GiB ext4。
- 夹具：§34 的严格传输三节点，固定物理配置；`openHAPhysicalBusinessGroup` 新增 `sampled` 参数，为每个成员接真实采样器，等到首个 Healthy 再打开成员，成员关闭后才停采样器。
- 压力策略：§32 的 `selectedCapacityPressure` 原样不改：磁盘停止线 128 MiB、恢复线 256 MiB；runtime 进入 1 GiB、退出 900 MiB；cgroup 进入 1536 MiB、退出 1280 MiB；采样间隔 100 ms。
- 流程：24 轮，偶数轮磁盘压力（外部文件填到真实 ENOSPC），奇数轮内存压力（持有并触碰 1 GiB 缓冲）。每轮：
  - 接受 1 个请求，等两个 Replica 应用；
  - 施加压力，**三个成员的采样器都必须在 5 秒内进入 Pressured**；
  - 在途时新准入必须被拒（原因只记录）；三端 checkpoint 可拒但不得进入恢复；
  - 磁盘轮再填满并确认空闲块为 0；完成已接受终结，两个 Replica 应用，释放；
  - 无在途时新准入必须被拒，原因须为 `ErrPressure` 或 `ErrPhysicalAllocation`；
  - 磁盘轮先只释放到停止线与恢复线中间（192 MiB），三端须保持 Pressured 1 秒，再全部释放；内存轮释放缓冲并 GC；
  - **三个成员须在 10 秒内回到 Healthy**；两个 Replica 读到终结；第一次准入成功；费用 5 秒内回到打开快照。

  每 3 轮做一次三端有空间的 checkpoint（保留 §33 v4 的 5 秒推迟重试），之后等两个 Replica 的 credit 窗口。第一次 checkpoint 后记录基线。
- 结束：同 §34，三端 ENOSPC 下关闭、无自有临时文件；重开（同样接真实采样器）后三端读到全部 24 个终结，新准入成功，正常关闭。
- 硬门槛：
  - 进入压力 ≤ 5 秒，回到 Healthy ≤ 10 秒，迟滞区间内不离开 Pressured（§32 的数值）；
  - 终结 ≤ 5 秒；确认 + 两个 Replica 应用 ≤ 5 秒；
  - 每轮两次新准入都被拒，无在途时的拒绝原因如上；恢复后第一次准入成功；
  - 费用、heap（+32 MiB）、ext4 空间（−32 MiB）、FD、三端 `Used`/normal claims 漂移门槛同 §33；
  - ENOSPC 关闭无自有临时文件；重开读到全部终结；
  - cgroup 峰值 ≤ 1.5 GiB（含 1 GiB 缓冲），`max`/`oom`/`oom_kill` 为 0。
- 只记录：每轮进入、退出耗时，拒绝原因，checkpoint 耗时与推迟次数。
- 通过标准：连续两次通过；共用辅助函数的 §33、§34 测试复跑通过；反向验证须让本测试失败（注入待定：若同 §33 的 COW 注入，只证明储备路径；另做一个采样器侧注入，例如让 Monitor 不挂到 Manager，测试须在“5 秒内进入 Pressured”失败）。

**v1 结果：连续两次通过。** 两次运行的输入相同（`12e1a3c5`），均在第 9 轮门禁同时运行时进行。逐项对照门槛，两次结果分别为：
- cgroup 峰值 1185 / 1190 MB（含 1 GiB 缓冲），不超过 1.5 GiB；`max`/`oom`/`oom_kill` 均为 0。
- 24 轮全部完成，磁盘轮、内存轮各 12 轮。
- 进入压力：从施加压力起到三个采样器都报告 Pressured，最长 450 / 403 ms（内存轮含触碰 1 GiB 的时间），门槛 5 秒。
- 回到 Healthy：从解除压力起到三个采样器都报告 Healthy，最长 220 / 211 ms，门槛 10 秒。
- 磁盘轮在停止线与恢复线之间（192 MiB）保持 1 秒，三端都没有离开 Pressured。
- 拒绝原因各 12 次：在途时都是 live ticket（`ErrPhysicalCredit`）；无在途时磁盘轮和内存轮都是 `ErrPressure`。采样器在第二次填满前就已关闭准入，所以这里没有出现物理拒绝；物理拒绝已由 §33/§34 在未观测窗口证明。
- 终结最长 144 / 200 ms；确认与两个 Replica 应用都在 5 秒内；每轮两个 Replica 都读到终结，恢复后第一次准入都成功，费用都回到打开快照。
- 8 次有空间的 checkpoint 都没有推迟，最长 4.27 / 3.69 秒（§34 无门禁并行时最长 0.35 秒；checkpoint 耗时没有门槛，只记录）。
- heap 26.81 / 26.85 MB，基线 26.85 / 26.82 MB；ext4 可用空间比基线少 20.2 / 20.6 MB；FD 不增加；三端预算回到基线。
- 三端在 ENOSPC 下关闭无自有临时文件；接真实采样器重开后三端读到 24 个终结，新准入成功。
- 测试耗时 124 / 108 秒，比 §34 的 17 秒长，主要是每轮等采样器进入、退出压力，以及 1 GiB 缓冲在 `GOMEMLIMIT=768MiB` 下的 GC。

**回归**（同一二进制，门禁并行）：§33 两节点（3 GiB 镜像）与 §34 三节点（6 GiB 镜像）持续压力测试在接入 `sampled` 参数后的辅助函数上各通过一次；checkpoint 最长 3.5 / 8.1 秒，同样是 CPU 争用，只记录。

**反向验证**（均已确认注入生效、构建后已还原，补丁在证据目录）：
- `reverse-cow.patch`（同 §33：Ledger 投影终结 COW 不用预留窗口）：第 0 轮（磁盘轮）以 `accepted terminal under sampled pressure: physical completion allocation unavailable` 失败。
- `reverse-sampler.patch`（采样器侧：Pressured 决策不写入 Manager，即采样器漏报压力）：第 0 轮以“采样压力未在 5 秒内到达 Pressured”失败，三端状态均为 Unknown。Healthy 租约在 2 秒后过期，Manager 自行转为 Unknown 并停止准入，所以漏报不会变成放行；但测试判据要求的是采样器真正报告 Pressured，因此失败。登记时设想的注入是“Monitor 不挂到 Manager”，那样会在启动等待 Healthy 时就失败，到不了登记的判定点，所以改为这个更贴近真实风险的产品侧注入。

证据目录：worktree 内 `docs/verification/performance/2026-10-11-ha-sampled/`（被忽略），包括 `v1-run-1`、`v1-run-2`、`regress-2m`、`regress-3m`、`reverse-cow`、`reverse-sampler`、批处理脚本 `batch.sh` 与汇总 `SUMMARY.txt`、两个补丁、`base-v1.txt` 与 runner。合入主分支时一并复制到主工作区的同名目录。

**结论。** 三节点、严格传输、真实采样器（与 Runtime 相同的接法）、真实 ENOSPC 与真实内存压力下，24 轮储备循环连续两次通过，首版即通过，未修改任何门槛或夹具：三个成员的采样器都在 0.5 秒内关闭准入、0.25 秒内恢复，迟滞区间保持，已接受终结用储备完成并被两个 Replica 应用，费用、空间、heap、FD 没有漂移，三端可以关闭并重开。没有发现产品缺陷。

I-03 仍不关闭，还缺：
- 历史规模下的持续压力（受 D-04/P2 阻断）；
- 动态输出盘的完整接受；
- “所有权移交”“有限等待”两行原有的缺口；
- 其他后台写入方在 ENOSPC 下的争用（§31）。

本批 2026-10-10 23:59 开始，2026-10-11 00:13 完成，约 14 分钟（时间取自分支创建与提交记录）。

## 36. 后台写入方在真实 ENOSPC 下与终结储备的争用（I-03）

本节第一部分在运行实验前提交，结果按设计 §9.2 写在后面，门槛事后不调宽。本批 2026-10-11 12:29 开始，基于 `024515d0`（第 10 轮门禁通过并已推送）。

**范围。** §31 登记的“导出、备份、重建等其他后台写入方在 ENOSPC 下的争用”。设计 §6.1 规定“该储备不能被导出、备份或重建占用”，§6 规定“后台预算不足：延迟后台工作；不影响必要结算资源”。此前只测过在线 checkpoint。本批先做单机 selected Runtime（物理终结储备开启，`PhysicalTerminalReserve=true`），HA Primary + Replica 的版本读完 HA 夹具后另行登记，不在本次登记内。

服务运行时 Parquet 定时任务依次调用的写入方，以及本批的取舍（依据都在代码里）：

| 写入方 | 本批 | 依据 |
| --- | --- | --- |
| Usage 磁盘维护 `maintainUsageDiskCycle`（collector 追账、日期维护、导出、窗口裁剪） | 测 | — |
| Ledger 封存 `sealLedgerGeneration`（Roll + 封存后校验） | 测 | 生产下限 16 MiB；测试在 Runtime 打开后把阈值设为 1，使封存立即到期，走的仍是生产函数 |
| Governance 导出 `WriteExportWithDefinitionOwner`（管理端触发，写 `governance/export/<id>`） | 测 | 设计点名“导出” |
| 公共 checkpoint `CheckpointMemoryState` | 测 | 已有 ENOSPC 证据，放在同一轮里一起核对 |
| Ledger 段压缩 | 不测 | Usage 为磁盘模式时 `ledgerArchivedThrough` 直接返回 false，不进入写入；测试断言 `r.usage.disk != nil` |
| Metadata 日志截断 | 不测 | selected Metadata facade 返回 `ErrFacadeMaintenance`；测试断言截断被拒 |
| 失败记录过期清理 | 不测 | 只删除文件 |
| 备份 | 不测 | 离线命令，不与服务共存 |

**固定配置（运行前提交）。** `TestStandalonePhysicalBackgroundWritersAtENOSPC`（`internal/app/terminal_physical_background_linux_test.go`；需 `HALRO_TERMINAL_ENOSPC_ROOT`、`HALRO_TERMINAL_ENOSPC_FS=ext4`、`HALRO_TERMINAL_ACTUAL_OS_PRESSURE=1`）：
- 环境：512 MiB ext4、容器内存 2 GiB、无 swap、2 CPU、`GOMAXPROCS=2`、`GOMEMLIMIT=768MiB`；runner 为证据目录内的 `runner-512.py pattern`。
- 夹具：与 `TestStandalonePhysicalTerminalActualENOSPCCompletesAndReopens` 相同（`physicalTerminalBudgetFixture`、Ledger 投影 56 MiB、物理终结储备、真实 OS 采样器 `physicalTerminalActualOSPressurePolicy`）。定时循环先停掉，写入方由测试逐个直接调用。
- 流程：一个真实 HTTP 请求进入上游时（终结已预付，`PhysicalTerminalDebt=2`），外部文件写到真实 ENOSPC，等采样器进入压力；然后依次运行四个写入方，每个之前若有空闲块先重新填满。之后让上游返回，完成终结；再发一个新请求，应被拒。删除外部文件，等采样器回到 Healthy，四个写入方各再运行一次；再发一个新请求，应成功。关闭后跑离线 `VerifyLedger`、`VerifyUsage`，重开读两个终结。
- 硬门槛（ENOSPC 下每个写入方运行后）：
  - 记账状态保持 `AccountingHealthy`（不锁定恢复）；
  - `PhysicalTerminalDebt` 仍为 2（不动用已预付的储备）；
  - Ledger 链头与哈希不变（后台写入不追加账务事实）；
  - 自有临时文件集合与运行前相同（`haPhysicalOwnedTemporaries` 的模式）；
  - Governance 导出若失败，目标目录不存在。
  写入方本身可以成功也可以返回错误，结果只记录。
- 硬门槛（终结与恢复后）：
  - 已接受请求返回 200、上游只调用一次、债务归零、终结事实可读；ENOSPC 下新请求 503 且不到上游；
  - 空间恢复后：collector 追账、checkpoint 成功；封存真的换了活动 WAL inode 且状态仍健康；Usage 磁盘维护返回 nil；Governance 导出成功且 `VerifyExportContext` 通过；新请求 200；
  - 关闭后无自有临时文件；离线 `VerifyLedger`、`VerifyUsage` 通过；重开后两个终结都可读。
- 只记录：每个写入方在 ENOSPC 下返回的错误、外部填充字节数。
- 判定为无效（不算通过，修夹具后作为新版本重跑）：四个写入方在 ENOSPC 下都返回 nil 且没有任何可见动作，说明夹具没有让它们碰到磁盘。
- 通过标准：连续两次通过；基线 `TestStandalonePhysicalTerminalActualENOSPCCompletesAndReopens` 同一二进制复跑通过；反向验证须让本测试失败。注入在读完各写入方失败路径后确定并在结果中写明理由，每个注入须先确认生效、再确认测试在对应门槛处失败。

基线：本批开始时，`024515d0` 上的 `TestStandalonePhysicalTerminalActualENOSPCCompletesAndReopens`（真实 OS 采样器）在 512 MiB ext4 下通过，耗时 8 秒。

**v1：夹具错误，无效。** 四个写入方在 ENOSPC 下都通过了每项硬门槛：不锁定恢复、债务保持 2、链头不变、无自有临时文件。返回值如下：
- Usage 磁盘维护：`reserve Usage archive RAM ... new resource admission stopped by pressure`，在准入处被压力挡下；
- 封存：没有换 WAL（Roll 被拒，只记日志）；
- Governance 导出：`governance journal is unavailable`；
- checkpoint：`context deadline exceeded`。

checkpoint 一直等到测试的 4 分钟期限才返回，之后读终结事实时也因期限已过而失败。原因在夹具：`CheckpointMemoryState` 的注释写明不能在业务 HTTP 处理中调用，因为它要等待该处理持有的 ticket；v1 恰好在在途请求自己的上游钩子里同步调用它，必然自锁。生产中它由定时循环在单独的 goroutine 里运行。终结本身返回了 200、债务归零。基线测试同次通过。

**v2（运行前提交）。** 只改这一处：checkpoint 从钩子里以单独 goroutine 启动（60 秒期限），等终结释放 ticket 后在 ENOSPC 下完成；在拿到其结果后检查状态健康、无新增自有临时文件。其余门槛不变。

**v2：ENOSPC 部分通过；空间恢复后失败，查出一个与 ENOSPC 无关的产品缺陷。** ENOSPC 下四个写入方（包括与请求并行的 checkpoint）都通过了全部硬门槛；终结 200、债务归零，新请求 503。空间恢复后，封存成功换了 WAL，随后 Usage 磁盘维护失败（`authenticated store requires reopen and native eligibility check`）。

归因（诊断测试未提交，源码和输出在证据目录；同一夹具，不填盘、不发请求，逐步执行）：
- 物理终结储备开启时，**Roll（封存）之后、下一个请求之前做 checkpoint**，checkpoint 以 `align selected Ledger maintenance Roll: maintenance Roll reduction failed` 失败，Ledger 投影随即退役：之后的业务请求报 `ledger projection is not available`。Roll 之后先有一个请求，再 checkpoint、关闭、重开都正常。
- **Roll 之后直接关闭**：关闭时的 checkpoint 同样失败，**重开失败**：`open selected Ledger root: authenticated seal does not match independently validated startup authority`。
- 逻辑预算（不开物理储备）下同样的顺序全部正常。
- 机制：Roll 之后 checkpoint 的 `alignStandaloneMaintenanceRoll` 调用 `AdvanceFromNative` 写入新位置，走 `updateHAPhysicalRestoredDeltaLocked` 的 Standalone 分支直接 `store.Update`；物理模式下存储写入要求已绑定的物理分配（`update.go` 的 `physicalUpdate`），checkpoint 没有，写入以 `ErrPhysicalAllocation` 被拒，失败使投影永久退役。请求的持久 Apply 在已预付的终结工作区里写，所以先有请求就没事。
- 生产可达：`ledger.seal.enabled` 与 `resource_budget.physical_terminal_reserve` 都是运营者选项，配置校验不禁止同时开启。封存在 Parquet 定时任务末尾执行，空闲实例在下一个请求之前就会遇到 checkpoint 或关闭。
- 另一个现象（两种预算下都有）：Roll 之后的 Usage 磁盘维护申请 288 MiB RAM（之前 8 MiB），在本夹具预算下被拒；先有请求再维护时则报 `ErrRecovery`。未归因。
- 实验补丁 `experiment-defer-align.patch`（未提交）：物理模式下 Roll 尚未对齐时 checkpoint 推迟、不写投影。在线场景修好（推迟后下一请求对齐，checkpoint、关闭、重开正常）；但 **Roll 后直接关闭仍无法重开**，说明根因还在启动校验：启动不接受“已保存的 seal 在旧 generation、原生头是之后一次完整的物理 Roll（offset 0、序号/哈希/epoch 相同）”。修复要改启动时的 fail-closed 校验或 Roll 本身，属于账务权威路径的设计决定，暂停等用户裁定。

本批到此 2026-10-11 12:48，§36 暂停。

**方案 1 尝试（用户 13:19 选定；未提交，补丁 `experiment-option1-partial.patch`）。** 逐步追踪后发现，“投影位置等于原生头”这一不变式在多处 fail-closed 检查里成立，Roll 后未对齐的状态会依次撞上：
1. 运行中 checkpoint 的对齐（`alignStandaloneMaintenanceRoll`）：推迟补丁可解决。
2. 启动物理恢复：Roll 时物理回执已随所有权转到新原生文件（声明全部匹配），失败点是 COW 根按原生头的 cut 解码，而根里仍是 Roll 前的 cut；失败会毒化存储。改为按 Roll 前的 cut 恢复（并要求无债务）后，Ledger 根可以打开。
3. 启动恢复的前缀确认（`selectedRecoveryWriter.ConfirmRecoveryPrefix` 要求投影位置等于原生头）：仍失败，报 `confirm authenticated recovery prefix through 2: verify maintenance Ledger prefix`，并锁定恢复。
4. 后面还有公共选择校验（`checkPhysicalCommonSelectionLocked` 同样要求两者相等），尚未走到。

结论：方案 1 要么在这些点逐一接受“未对齐的 Roll”（放宽至少 4 处 fail-closed 检查），要么在启动时为对齐写入新建一条有预付槽位的物理写路径；两者都比预估的改动大。按目前了解，更稳妥的是方案 2：Roll 在持有原生锁、已验证无债务时，同时完成投影位置的对齐写入，使“投影位置等于原生头”始终成立，上述检查都不用改。13:27 暂停，等用户重新裁定。

**方案 2 修复（用户 13:52 选定）。** Standalone 物理模式下，Roll 在把物理所有权转到新原生文件之后、仍持有原生锁时，借一个预付的投影根槽位，把 Ledger 投影位置写成新文件的头（新 generation、offset 0，序号、哈希、epoch 不变），再归还槽位（`ledger.Log.alignPhysicalRollProjectionLocked`）。投影因此不会在 checkpoint、关闭或重启时停在已封存的 generation 上，§36 列出的四处检查都不用改。范围只限 Standalone：HA Primary、Replica 的 Roll 走各自的确认路径，本修复不碰，HA 是否有同类问题未验证。

槽位不能在 Roll 的持久点之前预留：Roll 开始时尾部回收会退掉旧回执，新回执要等所有权转移时才在新文件上打开，而开着窗口时打开回执会被拒（实测）。

验证（Linux 512 MiB ext4）：
- 新回归测试 `TestStandalonePhysicalRollAlignsProjection`：请求 → checkpoint → Roll → checkpoint → 请求 → Roll → 关闭 → 重开 → 请求 → checkpoint → 关闭 → 子进程 Roll 后立即 SIGKILL → 重开 → 请求 → checkpoint → 关闭。修复版通过；去掉修复的同一测试在“Roll 后 checkpoint”处失败（`maintenance Roll reduction failed`）。
- `internal/ledger`、`internal/authenticatedstate`、`internal/statecut` 整包通过（ledger 406 秒）。
- `^TestStandalonePhysical` 全组（不开真实 OS 采样器，按其历史跑法）在空闲机器上全部通过。与 Go 整包测试并行时，`PreparedActualBudgetRefusalCleanupAtENOSPC` 失败过一次；空闲时修复版、基线各复跑两次都通过，判定为负载下的时序敏感，不是回归。

**仍有的窗口（如实登记）：**
- Roll 内“重命名已持久、对齐尚未写入”之间崩溃，或这一步遇到 ENOSPC：重启后仍是未对齐状态，无法打开。原 Roll 在同一位置之后已经有分配（新文件的终结范围），失败同样锁定恢复，所以不是新增的失败类别，但窗口仍在。彻底关闭需要启动侧接受（方案 1 的工作）。
- 本批顺带发现的既有问题，与修复无关、均在基线上复现：离线 `VerifyLedger`（即 `halro ledger verify`）对物理储备的 selected 实例返回 `ErrRecovery`，即使从未 Roll；`TestStandalonePhysical*` 中几个子进程测试在 `HALRO_TERMINAL_ACTUAL_OS_PRESSURE=1` 下因默认压力阈值在 512 MiB 盘上失败，历史上本来不这么跑。

**§36 v3 测试调整（运行前提交）。** 删除关闭后的离线 `VerifyLedger`、`VerifyUsage`：前者对物理储备实例必然失败（上一条），不是本批要回答的问题。其余门槛不变。

**v3（修复 `2622a913` 后）：ENOSPC 部分再次全部通过；空间恢复后在 Usage 磁盘维护处失败，原因已从 Roll 缺陷转为容量。** ENOSPC 下四个写入方的结果与 v2 相同，全部通过硬门槛；空间恢复后 collector、checkpoint、封存（换了 WAL、状态健康）都成功。Usage 磁盘维护报 `reserve Usage archive RAM=301989888 startup=false: resource budget exhausted`：
- 288 MiB 是归档重新校验的工作区（`limits.FileBytes` 256 MiB + 32 MiB，`archive_date_verify.go`/`archive_verify.go`）。Roll 后 Usage 源 generation 变了，下一次维护必须重新校验归档，所以 Roll 之前只要 8 MiB、之后要 288 MiB。
- 失败时预算快照（诊断补丁未提交）：IO 类只占约 36 MiB（上限 512 MiB），所以不是 IO 类上限；normal 池已用 452 MiB、normal claims 64。是字节还是 claim 耗尽、是本夹具预算偏小还是生产配置同样会卡住，未定。
- 配置校验里没有为这 288 MiB 工作区留位置（`config/resource_budget.go` 无相应项）。如果某个合法配置装不下它，Roll 之后 Usage 磁盘维护会每轮只记一条警告、永远推进不了。这属于容量（D-04）问题，不是 ENOSPC。
- 逻辑预算（8 GiB 夹具）下 Roll 后的同一维护也报过同样的拒绝（§36 v2 归因记录）。

§36 结论待定：ENOSPC 下的争用门槛（本批要回答的问题）在 v2、v3 都通过，但完整测试因 Roll 后的容量问题无法走完恢复阶段，按登记不算通过。2026-10-11 14:13 暂停，等用户裁定。

**容量问题查清（用户 14:13 后选定“先查清”）。** 结论：与 Roll、ENOSPC 都无关。只要开了资源预算并已存在 Usage 归档，Usage 日期维护每轮必被预算拒绝，导出也跟着停（`maintainUsageDiskCycle` 先跑日期维护、后导出）。证据在 `docs/verification/performance/2026-10-11-background-enospc/`（`cap-*`、`v3e`～`v3g`、诊断补丁 `experiment-*.patch`，均未提交到代码）。
- 更正 v3 记录两处：拒绝时 IO 类已占 440 MiB，不是 36 MiB（36 MiB 是 Background 类）；触发条件是“已有归档”，不是 Roll。fixture 里 Roll 封存出第一个代后才有第一份归档，所以看起来像 Roll 引起。
- IO 内存缺口：reference 作业（`usage_reference_budget.go`）按“记账恢复工作区 + 两个私有 bbolt 投影的完整映射”持有 376 MiB；日期校验会话再要 288 MiB（256 MiB 文件 + 32 MiB 解码），另有 Governance typed 投影、物理 Usage 追赶两个常驻者各 32 MiB，合计约 728 MiB，超过 IO 512 MiB。
- IO 磁盘缺口：日期校验按默认值预留 4 GiB 索引 + 256 MiB 文件，引用扫描再保留 4 GiB 索引，两者同时保留；`archive_verify.go` 那条路径会按归档上限收紧，这两条没有。索引和文件都在归档树内、物理上受归档上限约束，4 GiB 只是账面预留。IO 内存放到 1 GiB、IO 磁盘放到 8 GiB 仍被拒（4 + 0.18 + 4.25 GiB）。
- 没被发现的原因：日期维护的测试不开预算；唯一开预算调用 Usage 维护的测试期望被拒；配置校验不检查 IO 能否容下这些作业。

**修复（用户“按建议执行”）。**
- `usage`：日期校验、引用扫描把文件、索引上限收紧到归档磁盘上限（`clampArchiveScratch`）；窗口校验改为先收紧、再按收紧后的文件大小预留解码内存。导出 `DateMaintenanceWorkspace(ceiling)` 作为日期维护峰值的唯一来源。fixture（64 MiB 归档上限）下峰值从 288 MiB / 8.25 GiB 降到 96 MiB / 192 MiB。
- `config`：新增 `UsageMaintenanceIOMinimum` 与 `ValidateStandaloneUsageMaintenance`：IO 内存、磁盘须容下 reference 作业 + 日期维护峰值 + IO 常驻者（Governance typed 投影总是计入，因为已激活实例不设 `memory_state_required` 也会打开它；物理 Usage 追赶在开物理储备时计入），且不超过 normal 池。装不下的配置在加载时拒绝。
- 用户裁定一：memory-state 引擎约定测试（2 GiB 内存 + 8 GiB 投影上限）如实改为“引擎检查通过、Usage 维护规则拒绝”，补足 IO 后再验其余负例。原因：reference 作业在 memory-state 下仍建私有 bbolt 并按映射记内存。
- 用户裁定二：规则本批只对 Standalone（无 replication 配置）生效。HA 物理剖面里 Replica 的 576 MiB 投影上限让这一下限约 1.5 GiB，超过整个 normal 池（1152 MiB）；promote 后 Primary 的 Usage 维护可能装不下。登记为 D-04 开放项：改 reference 作业的记账方式后再把规则扩到 HA。
- 受影响但未改：Linux 容量剖面 `selectedCapacityBudget`（2 GiB 内存 + 8 GiB 投影）现在会被拒绝，同受上一条 D-04 阻塞；它只在显式设置 `HALRO_RUNTIME_CAPACITY_ROOT` 时运行。
- fixture 调整：Standalone 物理 fixture IO 内存 512 → 576 MiB（需要 536 MiB）；备份 reference fixture、投影包突发测试用 `fundUsageMaintenanceIO` 补足 IO（补多少同时加多少 Total）。

**验证（17:44 前）。**
- `usage` 新增两条测试并反向验证：撤掉收紧后，日期维护测试实测 288 MiB / 8.25 GiB 而失败；撤掉重排后，窗口校验测试实测 288 MiB 而失败。`usage` 整包通过（473 s）。
- `config` 新增规则测试并反向验证（撤掉规则后两条测试失败）；整包通过。
- `internal/app` macOS 整包（规则当时还对 HA 也生效）失败 21 条，全部是这条新规则拒绝 fixture；改为 Standalone-only 并补足 3 个 Standalone fixture 后，这 21 条重跑全部通过。
- Linux 512 MiB ext4：`^TestStandalonePhysical` 组与 `TestSelectedPhysicalUsageHistoricalStartupPruneAndReopen` 全部通过（不开真实 OS 采样器，按历史跑法）。
- `go vet` 三包通过。全仓门禁未跑。

**§36 v4：容量拒绝消失，卡在新的独立缺陷上。** ENOSPC 下四个写入方结果与 v2、v3 相同，全部通过硬门槛；空间恢复后 collector、checkpoint、封存都成功；Usage 磁盘维护报 `authenticated store requires reopen and native eligibility check`（`ErrRecovery`）。拆步定位：
- 追赶、日期维护、导出单独调用都成功；失败的是窗口裁剪 `PruneVerifiedWindowStep`。不开磁盘压力、不做 Roll，物理模式下同样失败。
- 拒绝点是 `usage/terminal_physical_window.go` 的队列守卫 `q.failure != nil || q.applied != anchor || q.remaining != 0`：实测 `applied == anchor`，`remaining = 3`。`remaining` 是物理储备为下一个请求预付、尚未消耗的 COW 槽位；处理过请求后稳态下不为 0，所以物理 Standalone 的窗口裁剪在处理过请求的 Runtime 里永远被拒（重开后、首个请求前可以裁剪，这正是现有测试 `TestSelectedPhysicalUsageHistoricalStartupPruneAndReopen` 走的路径）。
- 后果（按代码推断，未验证）：控制台窗口永不裁剪，Usage 投影一路涨到其磁盘上限；每轮维护返回错误。储备与记账状态不受影响（守卫在 `PreparePhysicalUpdate` 之前返回）。
- 未改：`PreparePhysicalUpdate(ctx, nil)` 本身会现场分配新槽位，不要求没有预付窗口，守卫看起来比必要的严；但它在物理储备的 fail-closed 路径上，为什么要求 0 尚未核实，不凭推断放宽。

§36 结论仍待定：ENOSPC 门槛在 v2～v4 都通过；完整测试现在被窗口裁剪守卫挡住。2026-10-11 17:44 暂停，等用户裁定。

**窗口裁剪守卫：查清意图后修复（用户 17:44 后选定）。**
- 意图：契约 `docs/contracts/memory-bounded-maintenance.zh-CN.md` 与原证据 `evidence/physical-usage-window-v1-2026-10-07/` 都写明，物理窗口裁剪“在全部终结债务之上分配普通 COW 候选，不消费原生帧的未来窗口”，即设计上与预付的根共存。
- 根因一（回归）：`290b0b93`（2026-10-10，实时追赶改为 8 条一批）让一批多帧只做一次物理更新、只消费一个预付根。物理储备为每个原生帧各预付一个根，于是多余的根留在 `remaining` 里。按“两边都构建、同输入运行”归因：`usage` 的 Linux 物理测试（ext4，`Physical` 组）在 `290b0b93^` 上 43 条全部通过，在 `290b0b93` 和 HEAD 上同样 5 条失败（`TestUsagePhysicalSelectedEachFrameCatchupAndKnownRetirement` 等，断言追赶后 `remaining == 0`，实测 4）。这些测试需要 ext4 环境，门禁不跑，所以一直没被发现。
- 根因二（设计内的窗口）：Roll 为后继 generation 的第一帧预付一个根（`seal.go` 的 `PhysicalDerivedRoll, 1`，`33ee0c26` 起就有）。逐步诊断：开启 0 → 请求后 5 → 追赶后 0 → Roll 后 2 → 追赶后 1。这一个根要到下一个请求被追赶后才消费。定时任务每轮先跑 Usage 维护、最后封存，所以封存后的下一轮裁剪一定被拒；没有流量的实例会一直被拒。
- 修复：物理模式下实时追赶回到每帧一次更新（`typed_live_advance.go`，非物理仍 8 条一批）；窗口守卫去掉 `remaining != 0`，保留 `failure`、`applied == anchor == 原生 head`、冻结 source lane。修复一之后，追赶完成时剩下的只可能是为尚未写入的帧预付的根。`retirePhysicalCatchupAfterCommon` 的 `remaining == 0` 是退休操作的条件，不改。
- 我中途先只放宽守卫（未提交），随后查出根因一，判断那样会掩盖回归，于是撤回、改为两处一起修。如实记录。

**验证（18:10 前，Linux 512 MiB ext4）。**
- 新回归测试 `TestStandalonePhysicalUsageWindowPrunesBesideTerminalReserve`：两个请求、Roll、完整 Usage 维护（30 天截止）、以未来截止时间裁剪到完成，累计事实不变；再处理请求、checkpoint、关闭、重开并核对。两处都不修时在维护的裁剪处失败；只修根因一时同样失败；两处都修时通过。根因一由上面 5 条 `usage` 测试覆盖（修后 43/43）。
- `^TestStandalonePhysical` 组与 `TestSelectedPhysicalUsageHistoricalStartupPruneAndReopen` 21 条通过（不开真实 OS 采样器）。

**§36 v7：Usage 维护通过，卡在 Governance 导出的功能缺口上。** ENOSPC 下四个写入方结果与 v2～v4 相同；空间恢复后 collector、checkpoint、封存、Usage 维护都成功；Governance 导出报 `accounting export requires a disk owner`（`ledger.ErrGovernanceExportBackend`）。
- 测试修正（如实登记）：原夹具直接调 `WriteExportWithDefinitionOwner`，不是生产路径。管理端 handler 在该错误时回退到 legacy 快照导出。现把这两步提成 `Runtime.writeGovernanceExport`，handler 与测试共用，行为不变。
- 回退后仍然失败：`WithGovernanceExport` 只有 bbolt `LedgerProjection` 实现，selected 七根的认证 Ledger 投影没有；legacy 回退又要求旧 State。所以 selected（memory-state）实例上 Governance 导出没有任何可用路径。现有导出测试 `governance_export_owners_test.go` 用的是 bbolt 磁盘 owner，没覆盖 selected。这是功能缺口，不是本批引入，§36 登记时误以为它可用。

§36 结论仍待定：ENOSPC 门槛在 v2～v7 都通过；完整测试现在只差空间恢复后的 Governance 导出（及其后的 `VerifyExportContext`、新请求、重开）。2026-10-11 18:10 暂停，等用户裁定。

**§36 v8 登记（用户选定方案 1，运行前提交）。**
- 门槛变动（如实登记，不算调宽 ENOSPC 部分）：“空间恢复后 Governance 导出成功且 `VerifyExportContext` 通过”改为“空间恢复后 Governance 导出返回 `ledger.ErrGovernanceExportBackend`、状态仍 `AccountingHealthy`、目标目录不存在”。原因见 v7：selected 实例尚无导出源。其余门槛不变，包括新请求 200、关闭后无自有临时文件、重开后两个终结可读。
- 开放项：selected（memory-state）实例的 Governance 导出，需要给认证 Ledger 投影实现 `WithGovernanceExport`（或等价的有界导出源）；在此之前管理端导出在这类实例上返回 503。
- 通过标准不变：连续两次通过；同一二进制复跑基线 `TestStandalonePhysicalTerminalActualENOSPCCompletesAndReopens`；反向注入各自先确认生效、再确认测试在对应门槛失败。
- 注入（读完失败路径后确定）：
  - A：去掉 `verifyPhysicalRollNoDebtLocked` 的 `o.debt != 0 || o.active != 0`。ENOSPC 下封存不 Roll 正是靠它：在途请求持有终结储备时拒绝 Roll。注入后封存会带着储备 Roll，预期在“`PhysicalTerminalDebt` 仍为 2 / 链头不变 / 状态健康”中至少一条失败。生效判据：ENOSPC 下 ledger-seal 的结果不再是“deferred without rolling”。
  - B：`governance.Manager.WriteExportWithDefinitionOwner` 入口先 `MkdirAll` 目标目录。模拟失败导出留下半成品，预期在“导出失败目标目录不存在”处失败（ENOSPC 下建目录本身可能 ENOSPC，所以至少在空间恢复后的拒绝处失败）。生效判据：目标目录被创建。

**§36 v8 结果与 v9 登记。** v8 两次都在空间恢复后的新请求处失败（502 `provider_error`）。原因是夹具：上游钩子把第二次调用一律当作“容量耗尽后上游又被调用”报错，而登记的门槛本来就要求空间恢复后的新请求到达上游（`calls == 2`）。之前的版本从没走到这一步。ENOSPC 部分、Usage 维护、Governance 拒绝在 v8 两次中都通过；基线测试在同一二进制上通过。
v9 只改夹具：空间恢复后置位，恢复后的调用照常返回；恢复前的第二次调用仍然报错。门槛与注入不变。

**§36 v9 结果与 v10 登记。** v9 两次都通过了 ENOSPC 部分、空间恢复后的 Usage 维护、Governance 拒绝和新请求；随后分别在两处夹具缺陷上失败：
- 第 1 次：重开后按字面 ID `req_background_after_space` 读终结，读不到。服务端会自行签发 X-Request-ID（Roll 回归测试已遇到过同一问题）。
- 第 2 次：关闭后“自有临时文件”检查报出 8 个 `usage/.archive-date-*-v1.json`。这些是 usage 定义的固定名常量（`archiveDateVerifyName` 等），是日期维护可续做的步骤状态，按设计跨重开保留；共用助手 `haPhysicalOwnedTemporaries` 的 `.archive-` 前缀把它们误判为临时文件。第 1 次没报，是因为这次日期维护是否留下这些状态取决于运行时机。
v10 只改夹具：新请求改用响应里的 X-Request-ID；助手按精确模式 `^\.archive-date-[a-z-]+-v1\.json$` 排除这些固定名状态（带随机后缀的临时名仍算临时文件）。这个助手 HA 测试也在用，排除对它们同样生效。门槛与注入不变。

**§36 v10 结果（在 `8868ed94` 上构建，Linux 512 MiB ext4、真实 OS 采样器）。**
- 连续两次通过（2.95–3.11 s，cgroup 峰值 247–348 MB）；同一二进制复跑基线 `TestStandalonePhysicalTerminalActualENOSPCCompletesAndReopens` 通过。
- ENOSPC 下四个写入方：Usage 维护被压力延迟（`new resource admission stopped by pressure`），封存未 Roll，Governance 导出被拒（journal unavailable），公共 checkpoint 成功；每个之后状态健康、`PhysicalTerminalDebt` 仍为 2、链头不变、无新增自有临时文件。在途请求 200 且上游只调用一次，ENOSPC 下新请求 503。
- 空间恢复后：collector、checkpoint、封存（Roll）、Usage 维护（含日期维护、导出、窗口裁剪）成功；Governance 导出按 v8 门槛如实拒绝且无目标目录；新请求 200；关闭后无自有临时文件；重开后两个终结可读。
- 反向注入（补丁在证据目录 `inject-*.patch`）：
  - B（导出入口先建目标目录）：生效（空间恢复后目录被建出；ENOSPC 下建目录本身失败，与登记预期一致），测试在“被拒导出留下目标”处失败。满足登记。
  - A（去掉 `verifyPhysicalRollNoDebtLocked` 的债务条件）：未生效。诊断打印显示 ENOSPC 下 Roll 根本没走到这里，在 `rollLocked` 开头的同一债务检查（`seal.go`）就被拒。
  - A2（两处债务检查都去掉）：生效（打印 `Roll past debt guard debt=2`），但 Roll 随即被压力准入拒绝，测试通过。
  - A3（再让首次 Roll 的 64 KiB 校验内存绕过压力）：Roll 在后面另一处压力准入被拒，测试通过。
  - C（A2 + 预算管理器对普通准入完全忽略压力，直接违反设计 §6 的“后台延迟”）：生效（ENOSPC 下 Usage 维护真的跑完并返回 nil，导出走到后端，Roll 越过债务和压力），Roll 最终在物理分配处被拒（`physical completion allocation unavailable`），所有硬门槛仍然成立，测试通过。
  - 结论：ENOSPC 下终结储备至少由四层独立保护（`rollLocked` 债务检查、`verifyPhysicalRollNoDebtLocked`、多处压力准入、物理预分配）；单点或三层同时去掉都没能让储备被动用，所以登记要求的“每个注入在对应门槛失败”对储备类门槛（A 系列）没有达到。要让它失败，需要注入到最后一层，让 Roll 从终结储备已预分配的范围里分配，这已不是“后台写入方行为”层面的故障。
- 判定：按登记，B 满足；A 系列未满足“在对应门槛失败”。§36 是否接受由用户裁定。2026-10-11 18:42 暂停。

开放项更新：selected 实例 Governance 导出（v8 登记）；HA 成员的 Usage 维护 IO 下限与 reference 作业映射记账（D-04）；Linux 容量剖面 `selectedCapacityBudget` 被新下限拒绝（同 D-04）；`usage` 的 ext4 物理测试不在门禁内（`290b0b93` 的回归因此潜伏两天）。

**§36 判定：接受（用户 2026-10-11 20:30 裁定）。** 依据：v10 连续两次通过，同一二进制基线通过；注入 B 在对应门槛失败。A 系列不改为“满足”，而是记为结论：Standalone 物理模式下，后台写入方在 ENOSPC 下动用终结储备，需要同时越过 `rollLocked` 债务检查、`verifyPhysicalRollNoDebtLocked`、多处压力准入和物理预分配四层；去掉前三层仍被第四层拒绝。登记的“每个注入在对应门槛失败”对储备类门槛未达到，这一偏差保留在 v10 结果里，不追溯改写登记。§36 的 HA 一半仍未登记。

**下一项：Linux 专属测试进门禁。** 核实“usage ext4 测试不在门禁内”时发现缺口更大：13 个包共 85 个 `_linux_test.go`，macOS 上的 `make full-check` 全部不编译；CI 只在 `main` 推送和 PR 上运行，本分支没有 PR，所以这些测试在本分支上从未被任何门禁执行。`290b0b93` 的回归由此潜伏。

**Linux 专属测试进入门禁（2026-10-11 20:38）。** 新增 `scripts/linux-only-tests.sh` 与 `make linux-only-check`。名单按 GOOS=linux 与 GOOS=darwin 的测试文件集之差，再加上在非 Linux 或非 ext4 时跳过的可移植文件，目前为 15 个包、163 个测试。运行方式：交叉编译测试二进制，在 `ubuntu:24.04` 容器内新建 8 GiB ext4 回环盘作为 `TMPDIR`，以 nobody 身份在只读源码的包目录下运行；每个包报告通过、跳过、失败的数量和跳过原因。非 Linux 主机上，`make full-check` 会包含这一步。
- HEAD（`c0db1b56`）：47 个通过，99 个跳过，0 个失败，约 1 分钟。跳过的都是要求专用小文件系统或子进程角色的测试（`HALRO_*_ENOSPC_ROOT`、`HALRO_EXT4_ROOT` 等），与 CI 的普通 runner 相同。它们仍然只作为登记证据运行，不在门禁内。
- 反向验证：在独立 worktree 里恢复 `290b0b93` 的物理模式批量追账，并确认改动已生效；脚本以 exit 1 失败，失败的是 `TestUsagePhysicalSelectedEachFrameCatchupAndKnownRetirement`、`TestUsagePhysicalActualCollectorPrepaidWatermarksAtPressure`。当时记录的另外 3 个失败需要 `HALRO_EXT4_ROOT`，在门禁里会跳过。
- 仍未覆盖：上述 99 个专用夹具测试。要让它们进门禁，需要为每类夹具准备专用文件系统并控制运行时长，另行登记。

## 37. §36 的 HA 一半：HA 成员的后台写入方在真实 ENOSPC 下与终结储备的争用（I-03）

本节第一部分在运行实验前提交，结果写在后面，门槛事后不调宽。2026-10-11 23:50 登记，基于 `b2cfd5db`（第 11 轮门禁通过并已推送）。

**读代码得到的范围（用户 23:40 选定按 foundation 级登记）。**
- 公开 HA 物理启动仍关闭：`openHASelectedApplication` 遇到 `PhysicalTerminalReserve` 直接返回 “HA physical terminal vector is not prepared”（`ha_selected_application.go`）。§31～§35 都在 foundation 夹具上测，本节同样，结论只是 foundation 证据。
- HA selected Runtime 不启动 `runUsageMaintenance`（`startHASelectedPrimaryWorkers` 的列表里没有），Replica 不启动任何后台任务。所以单机 §36 的定时封存、Usage 磁盘维护、定时 checkpoint 在 HA 上都没有调用方；HA 上实际存在的写入方只有 Primary 的 Usage collector（`usageCollector.Run`）、管理端 Governance 导出、各成员的在线 checkpoint（`checkpointHAPhysicalOnline`，由 `CheckpointMemoryState` 的 HA 分支调用，但 HA 上目前没有定时调用它）。
- 新开放项：HA selected 没有维护循环（无封存、无 Usage 日期维护与窗口裁剪、无定时 checkpoint）。它对长期运行的影响本批未验证。

| 写入方 | ENOSPC 下 | 空间恢复后 | 依据 |
| --- | --- | --- | --- |
| Primary Usage collector `CatchUp` | 测 | 须返回 nil | 生产只在 Primary 运行 |
| Primary Ledger `Roll` | 测，须不换代 | 不测 | 无 HA 调用方；有债务时 `rollLocked` 应拒绝。空间恢复后真的 Roll 会走一条不可达且未对齐的路径（§36 的对齐修复只覆盖 Standalone），不作为本节门槛 |
| Governance 导出 `writeGovernanceExport`（管理端调用的同一函数，装上 HA 应用接入的 owner） | 测 | 须以 `ErrGovernanceExportBackend` 干净拒绝且不留目标 | 与单机 v8 同一功能缺口 |
| 各成员在线 checkpoint | 测（5 秒上限） | 须成功（`checkpointHASustained`，允许 §33 v4 的 `ErrPhysicalCredit` 推迟重试） | — |

**固定配置。** `TestHAPhysicalBackgroundWritersAtENOSPC`（两节点）与 `TestHAPhysicalThreeMemberBackgroundWritersAtENOSPC`（三节点），`internal/app/ha_terminal_physical_background_linux_test.go`；需 `HALRO_HA_SUSTAINED_PRESSURE=1`、`HALRO_EXT4_ROOT`。严格传输（`openHAPhysicalBusinessGroup(..., strict=true, &policy, nil)`），每个成员由真实采样器 `selectedCapacityPressure()` 驱动压力，测试不设置压力。runner 沿用 §35：两节点 `runner-3072.py`、三节点 `runner-6144.py`（2 GiB 内存、2 CPU、`GOMEMLIMIT=768MiB`、`HALRO_HA_RECOVERY_RESERVE_MIB=1259`）。

**流程。** 接受一个请求（`peerPhysicalTerminalAdmission` + `BeginRequestDetailed`），各 Replica 应用；外部文件写到真实 ENOSPC，等所有成员的采样器进入 Pressured；依次运行上表的写入方，每个之前若有空闲块先补满。然后在零空闲块下完成已接受的终结，Replica 应用；无在途请求时新准入须被拒（压力或物理停止线）。删除外部文件，等所有成员回到 Healthy，按上表“空间恢复后”运行；再接受并完成一个新请求。各成员收尾关闭，重开后每个成员都能读到两个终结。

**硬门槛（ENOSPC 下每个写入方运行后，对每个成员）。**
- 不进入恢复（`recovery` 为 false，原生 Ledger 状态 `AccountingHealthy`）；
- `PhysicalTerminalDebt` 与写入方运行前相同（Primary 运行前须非零，否则夹具无效）；
- Ledger 链头与哈希不变；
- 自有临时文件集合不变（`haPhysicalOwnedTemporaries`）；
- Governance 导出若失败，目标不存在；Roll 不得换代。
写入方本身成功或返回错误只记录。

**硬门槛（终结与恢复后）。** 已接受终结在 `haSustainedTerminalWithin`（5 秒）内完成，Replica 可读；压力进入 `haSampledEnterWithin`（5 秒）、退出 `haSampledExitWithin`（10 秒）内完成；空间恢复后上表各项满足、新请求完成；收尾后无自有临时文件；重开后所有成员读到两个终结。

**只记录。** 每个写入方的返回值、接受时各成员的债务、checkpoint 推迟次数与最长耗时。

**通过标准。** 两节点、三节点各连续两次通过；同一二进制复跑 §35 的 `TestHAPhysicalThreeMemberReserveSampledPressure` 通过。反向注入在读完写入方失败路径后确定，结果中写明理由；每个注入先确认生效，再确认测试在对应门槛处失败。

**v1：夹具错误，无效（两节点，2026-10-11 23:53）。** ENOSPC 下六个写入方都通过了每个成员的硬门槛（不进入恢复、债务不变、链头不变、无自有临时文件）。接受时债务：Primary 2，Replica 31。返回值：collector `nil`；Roll 被 `BeforeRoll` 拒绝（“original Replica physical IO promise unavailable or changed”），没有走到债务检查；Governance 导出 “governance journal is unavailable”；Primary checkpoint 与 Replica checkpoint 都因同一 Replica promise 拒绝。随后在终结前补满时失败：`exhaustHAPhysicalFixture` 只新建不追加，循环里已建过 `foreign-background-refill`。v2 只改夹具：循环中每次补满用按写入方命名的独立文件。门槛不变。

**v2 结果（在 `01396f2d` 上构建，Linux ext4、真实采样器；证据在已忽略的 `docs/verification/performance/2026-10-11-ha-background/`）。**
- 两节点两次、三节点两次全部通过；cgroup 峰值 209/168/131/133 MB，无 max/oom 事件。同一二进制复跑 §35 `TestHAPhysicalThreeMemberReserveSampledPressure` 通过（24 轮，峰值 1194 MB）。
- 接受时债务：Primary 2，每个 Replica 31。
- ENOSPC 下每个写入方的返回值（四次相同）：collector `nil`；Roll 被 `BeforeRoll` 拒绝（`ErrPhysicalCredit`）；Governance 导出 “governance journal is unavailable”；各成员 checkpoint 都以 `ErrPhysicalCredit` 拒绝。空间恢复后：collector `nil`，导出 `ErrGovernanceExportBackend` 且无目标，checkpoint 无推迟、最长约 0.3 秒，新请求完成，重开后所有成员读到两个终结。
- 新事实：HA 严格物理模式下 `BeforeRoll` 只要装了 peer credit 就返回 `ErrPhysicalCredit`（`replication_runtime.go`），所以 HA 物理 Primary 在任何情况下都不会 Roll。目前没有调用方，但将来给 HA 接维护循环时，活动 WAL 将无法封存。

**反向注入（两节点，各先确认补丁生效）。**
- B（与单机相同：导出入口先 `MkdirAll` 目标）：失败，但失败在空间恢复后的“拒绝的导出不留目标”，不是 ENOSPC 下的同类门槛。原因：ENOSPC 下 `MkdirAll` 本身也拿不到块，注入没有产生目标。按登记，B 的门槛被测试抓到，但 ENOSPC 一半这条门槛在本夹具下无法由此注入触发。
- D（`checkpointHAPhysicalOnline` 任何错误都锁存恢复）：在 ENOSPC 下 `checkpoint-0` 后的“不进入恢复”门槛失败。满足。
- A-HA（去掉 `BeforeRoll` 的 peer credit 拒绝和 `rollLocked` 的债务检查）：测试通过。Roll 改为被压力准入拒绝（“new resource admission stopped by pressure”），储备仍没有被动用。与单机 A 系列相同的结论：储备有多层保护，去掉前两层后还有压力准入；登记的“在对应门槛失败”对储备类门槛未达到，偏差保留，不改写登记。
- 判定：按登记，两节点、三节点各两次通过，基线通过；D 满足，B 部分满足，A-HA 未满足。§37 是否接受由用户裁定。

**§37 判定：接受（用户 2026-10-12 00:03 裁定）。** 依据：v2 两节点、三节点各连续两次通过，同一二进制 §35 基线通过；注入 D 在对应门槛失败。B 只在空间恢复后的同类门槛失败（ENOSPC 下注入本身拿不到块），A-HA 被压力准入挡下、测试通过，这两项偏差保留在 v2 结果里，不追溯改写登记。结论只是 foundation 证据：公开 HA 物理启动仍关闭。开放项：HA selected 没有维护循环；HA 物理 `BeforeRoll` 在装有 peer credit 时总是拒绝 Roll。

## 38. selected 实例的 Governance 导出（§36 v7 登记的功能缺口）

2026-10-12 用户选定。§36 v7 查明：`WithGovernanceExport` 只有 bbolt `LedgerProjection` 实现，legacy 回退要求旧 State，所以 selected（memory-state）实例、包括 HA selected，管理端 Governance 导出没有任何可用路径，固定返回 `ErrGovernanceExportBackend`。

**读代码得到的事实。** 认证 Ledger 投影与 bbolt 投影共用同一个 `applyProjectionRecord`，所以 `export_work_units`、`export_runs` 两个按对象记录创建/关闭序号的桶，以及 `export_work_unit_count`、`export_run_count` 两个计数，认证投影早已在写；缺的只是读这一侧。selected 与 HA selected 的 budget owner 都内嵌 `*ledger.AuthenticatedLedgerProjection`，方法实现在投影上即可被 `budget.Manager.WithGovernanceExport` 看到，budget、governance、app 三层都不用改。

**实现。**
- `ledger/governance_export.go`：导出视图不再直接持有 bbolt 事务，改为经 `governanceExportSource` 每页打开一次有界读取（行、当前位置、按键序的索引遍历）。截面判定、分页进度、源字节上限、as-of 还原、成熟度批次重试的逻辑原样保留，只把 `*bbolt.Tx` 换成通用的 `projectionRows`。bbolt 数据源保留原有的投影 generation 校验。
- `ledger/authenticated_governance_export.go`：认证数据源。每页是一个独立的 `authenticatedPointJob`（独立 pin、IO 费用当页释放），页与页之间、以及发布期间都不持有 pin，截面靠 as-of 位置还原；Roll 或同序号哈希变化撤销截面（与 bbolt 同一判定）。HA 业务读取的已确认位置限制由 `authenticatedReadJob` 照常施加。
- 锁忙：认证读取只尝试取锁，写入方短暂持锁或四个 pin 槽占满时立即返回 `ErrBusy`。导出要连续读很多页，任何一页的瞬时忙都会让整次导出作废，所以数据源按 `authenticated_writer.go` 的先例退避重试（1 ms 起、50 ms 封顶、至多 30 秒），且只在回调尚未进入时重试，避免把读了一半的页重复累加。
- 导出格式、manifest、`VerifyExport` 均不变。不需要重新初始化数据目录。

**验证。**
- `internal/ledger` 新测试：同一 WAL 构建的认证投影与 bbolt 参照投影，导出的 WorkUnit、Run、截面与成熟度在每页 1 行、2 行、最大行数下逐项相同；截面后追加 WorkUnit 关闭、Run 关闭与新 WorkUnit 并 `AdvanceFromNative`（导出期间独占推进能执行，说明页间不持锁），截面内仍是打开状态、新对象不出现；持有 owner 写锁 20 ms 期间读页能等到释放。反向：去掉重试，锁测试以 `authenticated snapshot reader busy` 失败。
- `internal/app` 新测试 `TestSelectedRuntimeGovernanceExportPublishesAccountingCut`：真实 selected Runtime，经管理端同一函数 `writeGovernanceExport` 导出，`VerifyExport` 通过，manifest 的 Ledger 水位等于原生链头，四个数据集计数正确，状态仍健康。反向：移走认证投影的实现，测试以 `accounting export requires a disk owner` 失败。
- 现有 bbolt 导出测试（ledger、budget、governance、app）不变通过。

**§36/§37 门槛恢复（如实登记）。** §36 v8 因功能缺口把“空间恢复后 Governance 导出成功且 `VerifyExportContext` 通过”临时改为“干净拒绝”，§37 照此登记。缺口已补，两个测试改回原门槛：空间恢复后导出成功、`VerifyExportContext` 通过且水位与 manifest 一致；ENOSPC 下的门槛（拒绝、无目标、不进入恢复）不变。同一二进制（`suite.test` sha256 前缀 `a4af2401c5014215`，基于 `4afe69e2` 加本节改动）在 Linux ext4 真实采样器下：单机 512 MiB 连续两次通过，HA 两节点 3 GiB、三节点 6 GiB 各连续两次通过；ENOSPC 下导出仍报 “governance journal is unavailable”，与 §36/§37 相同。证据在被忽略的 `docs/verification/performance/2026-10-12-selected-export/`。

**仍开放。** HA 上导出只在 Primary 有意义，Replica 的管理端行为未单独验证；公开 HA 物理启动仍关闭（§37），所以 HA 部分仍只是 foundation 证据。

## 39. I-03 收尾线（2026-10-12 用户裁定）

2026-10-12 12:37 记录。§23～§38 都是 I-03 的子项，做完后原任务计数仍是 5 完成 / 20 开放。为了让 I-03 有可以关闭的终点，用户按推荐裁定以下三项，并确认收尾线。收尾线之外的条件不再挂在 I-03 下，而是转入下面列出的原任务，不删除。

**三项裁定。**
1. 备份输出盘：采用“预检、不预占整块空间，失败时精确清理”的契约。原规模下的容量验收随 V-02 进行。
2. 命令与重启：E2 只覆盖 6 个会修改状态的命令（`backup create`、`backup restore`、`usage compact`、`usage prune`、`ledger seal`、元数据迁移）。每个命令各跑一次正常退出、一次 SIGKILL。判据：正常退出后只剩持久 claim；下次启动接手的清单与磁盘一致，不泄漏、不重复计费。
3. 外移项：历史规模转 D-04/M3；生产启动时的磁盘身份核对转 D-01/R-01；无法取消的物理 IO 登记为残余，不算退休。

**收尾工作项（做完即关闭 I-03）。**

| 项 | 内容 | 判据 |
| --- | --- | --- |
| E1 | 真实 ENOSPC 注入：新旧投影共存期间、bbolt 扩页期间 | 与 §23 相同：拒绝新工作；已接纳的事实与费用保留；只清理自己的临时文件；重启后恢复 |
| A1 | 全部等待点清单，写明每个上界的来源；配一个契约测试，出现清单外的等待点即失败 | 清单完整，契约测试的反向验证能失败 |
| E2 | 见裁定 2 | 见裁定 2 |
| F1 | 回收崩溃遗留的 `.archive-*.tmp`（§28），加回归测试 | 遗留文件在下次启动或维护时被回收，反向验证能失败 |

**转出 I-03。** Roll 的崩溃窗口转 I-12；HA 维护循环和 HA 物理 `BeforeRoll` 转 I-06/I-12；引用任务转 D-04。

**流程。** 四项做完后只跑一次门禁，通过即关闭 I-03（计数变为 6 完成 / 19 开放），然后转 I-06。每个实验先登记门槛、提交后再运行；事后不放宽门槛，并做反向验证。

## 40. E1 登记：新旧投影共存与 bbolt 扩页下的真实 ENOSPC（I-03 收尾）

2026-10-12 15:34 登记，基于 `82b8b821`。本节在运行任何实验前提交；结果另起小节记录，不回改本节的门槛。工作上限 2 小时。

**先查清 selected 路径上实际有哪些点。** 设计 §9.1 写的是“新旧投影共存、bbolt 扩页”，但 selected 实例的运行时主路径已经基本不用 bbolt：
- Usage 与 Governance 用 typed 认证页包（`OpenSelectedTypedUsageProjection`、`OpenSelectedTypedGovernanceProjection`），Ledger 用认证投影，Metadata 走 facade。
- `usage.RestoreDiskLedgerDerivative` 与 `governance.(*AuthenticatedGovernanceProjection).RestoreProjection`（恢复到新 bbolt）只有测试调用，不在生产路径上。
- Usage 固定重建的暂存是 `scratchstate` 分页文件，不是 bbolt；只在迁移激活时运行。

据此，selected 路径上剩下的点如下。

| 点 | 场景 | 新旧共存 / 扩页 | 现有真实 ENOSPC 证据 | 本批 |
| --- | --- | --- | --- | --- |
| 1 | 认证页包 repack：候选包写在当前选中包旁边 | 共存 | 只有 SIGKILL（`repack_crash_test.go`）与 headroom 测试 | 做（组件） |
| 2 | 非物理 selected Runtime 的在线 checkpoint（含到期 repack） | 共存 | §36/§37 只覆盖物理终结储备模式 | 做（Runtime） |
| 3 | 管理端 Usage 汇总的 bbolt 暂存 `summary.db` | bbolt 扩页 | 无 | 做 |
| 4 | selected 备份物化 Metadata 镜像（`MaterializeMetadata`，bbolt） | 共存 + 扩页 | 备份 ENOSPC 测试只覆盖密文输出 | 做 |
| — | 两个缓存槽轮换的 checkpoint | 共存 | §36/§37 | 引用 |
| — | 迁移激活（含 Usage 固定重建、Metadata 重建） | 共存 | §29/§30 | 引用 |
| — | scratchstate 块缓存 | 扩页 | `TestScratchBlockBankActualENOSPCPoisonsCandidateAndKeepsFees` | 引用 |
| — | 物理模式 repack | 共存 | 候选空间预先 fallocate，不会在写入时 ENOSPC；headroom 测试 | 引用 |

密钥轮换同样调用 `MaterializeMetadata`，函数本身由点 4 覆盖；轮换命令自己的失败清理不在本批，记为 I-03 之外的开放项，交用户裁定归属。

**统一做法。** 真实 Linux、隔离 ext4（`HALRO_EXT4_ROOT`），用外部文件把文件系统填到真实 ENOSPC；每个点连续通过两次才算接受。

**各点门槛（沿用 §23：拒绝新工作、保留已接纳的事实与费用、只清理自己的临时文件、重启后恢复）。**

点 1（`internal/authenticatedstate`，可用包内 `s.hook`）。注入在 `repack-pages-created`（候选页拷贝时撞上 ENOSPC）与 `repack-pages-synced`（写根时撞上 ENOSPC）两处：
- Repack 返回的错误包含 `syscall.ENOSPC`，没有别的错误。
- 当前选中包的 inode、字节与根不变；全部行与注入前逐项相同。
- 候选只可能残留在非选中槽；释放空间后，下一次 Repack 或重新打开会将其回收。预算 `Used` 回到注入前基线，不泄漏也不重复计费。
- 释放空间后 Repack 成功；重新打开后行不变。

点 2（`internal/app`，非物理 selected Standalone，真实采样器）。先累积足以触发 repack 的死页，再填满，然后调用 `CheckpointMemoryState`：
- checkpoint 失败，错误链里有 ENOSPC；Runtime 状态保持 `AccountingHealthy`，不进入恢复。
- 填满期间，新请求要么完整结算，要么被 503 拒绝；不存在已接纳却丢失的请求。注入前已结算的 Ledger 与 Usage 事实不变。
- 数据目录里不残留本次 checkpoint 自己的临时文件。
- 释放空间后 checkpoint 成功；Close 后重启，全部已结算事实都在。

点 3（`internal/app`，selected Runtime）。填满后发管理端 Usage 汇总请求：
- 请求失败（非 2xx，或 handler 返回错误），不返回部分结果。
- 暂存目录被移除；在线 Usage 读取不受影响，状态保持健康。
- 释放空间后同一查询成功，并与填满前的参照结果相同。

点 4（`internal/app`，selected 备份）。为 `snapshotSelectedBackupMetadata` 加一个不导出的阶段钩子，生产传 nil，与 §30 做法相同；注入在物化开始前：
- 备份以 ENOSPC 失败；镜像文件与暂存被移除。
- 在线 Metadata 逐字节不变，IO 费用释放。
- 释放空间后同一命令成功，`backup verify` 通过。

**反向验证（每点一项，必须失败）。**
- 点 1：去掉失败后对候选槽的回收。
- 点 2：把 ENOSPC 误判为 `ErrRecovery`。
- 点 3：去掉暂存清理。
- 点 4：去掉镜像 Discard。

**不在本批。** HA selected（I-06）；历史规模（D-04/M3）。

### 40.1 E1 结果（进行中，2026-10-12 15:42）

**点 1 接受。** `TestStoreRepackAtActualENOSPCKeepsSelectedPack`（`2b42510c`）在 512 MiB ext4 上连续两次通过。两个注入点都以真实 ENOSPC 失败；候选在失败返回前就被清理，目录里只剩选中包；预算与注入前相同；释放空间后 repack、重开得到的目录与未注入的对照运行一致。反向验证：去掉失败路径对候选 owner 的 Retire 后，`repack-pages-created` 残留 `repack.unqualified-0`，`repack-pages-synced` 的下一次 repack 被拒（“requires reopen”），两例都失败。证据在被忽略的 `docs/verification/performance/2026-10-12-e1/`。

**点 2 v1 未通过登记门槛。** `TestSelectedCheckpointAtActualENOSPCKeepsAcceptedFacts`：
- 第一次正式运行（`p2-run-1b`）：checkpoint 以 ENOSPC 失败（`directory/root.next.pending`，没有走到 repack），状态仍为健康；随后的请求在原生 Ledger 写入第 209 条记录后，认证 Ledger 投影写 `ledger/pages` 时撞上 ENOSPC，请求以错误结束，既不是完整结算，也不是 503。门槛不满足。（更早一次运行因夹具配置无效在打开前失败，没有进入实验。）

诊断运行（不计入验收，测试改为只记录）查明的事实：
- 结果取决于时机。采样器先察觉时，checkpoint 被压力干净拒绝（`new resource admission stopped by pressure`，三次）。请求赶在采样器之前到达时有两种情形：
  - WAL 追加就失败，状态变为 `AccountingUnavailable`，没有持久事实。
  - WAL 已写入、投影应用失败，状态变为 `AccountingRecoveryRequired`。
- 两种情形下，后续请求都以 `ledger projection is not available` 失败；释放空间后 checkpoint 仍然失败，进程内不会恢复；Close 返回错误（`checkpoint audit log: new resource admission stopped by pressure`）。
- 重启后恢复：状态健康，已写入 WAL 的那条事实被计入（Usage 103→104，Ledger 头 206→208），没有丢失；WAL 未写入的情形下没有新事实。

**解读。** 非物理模式（`physical_terminal_reserve` 默认关闭）在采样器察觉之前遇到的 ENOSPC，按失败封闭处理：锁定、拒绝后续工作、重启恢复，不丢已持久化的事实。§23 的“拒绝新工作而服务继续”只有物理终结储备才能保证（§31～§37）。所以登记的点 2 门槛用在非物理模式上并不成立。是改门槛（需要用户裁定，不能事后放宽），还是把进程内恢复作为缺陷来修，留待用户决定。

### 40.2 点 2 v2 登记（2026-10-12 16:43，用户裁定）

用户采纳 §40.1 的解读：非物理模式的点 2 按失败封闭判定，不当作缺陷修。依据是 `internal/ledger/status.go`：`RequireRecovery` 的注释写明，锁定在进程生命周期内不会降回，解除的办法是重启，让 WAL 重新扫描、重新认证；进程内“标记健康”就是失败开放的形状。“拒绝新工作而服务继续”只由物理终结储备保证（§31～§37），默认关闭的非物理模式不承诺这一点。

v1 门槛不回改，v1 的结论是“未通过”。本节在任何 v2 运行之前提交。

**测试结构的两处修正（不是放宽门槛，是 v1 诊断查清的测试缺陷）：**
- 测试 handler 不写响应码，所以 recorder 的 200 不能说明请求成功。v2 以业务错误为准：只有 503（准入拒绝）或业务错误为 nil 才算“未失败”。
- selected 的压力采样器在独立的 context 上运行（`startSelectedPressureMonitor`），不受 `stopBackgroundWork` 影响。v1 在删除填充文件后立刻 checkpoint、Close，采样器还在冷却期，所以 Close 中的审计检查点被压力拒绝。v2 删除填充文件后，等资源管理器的压力回到 Healthy（上限 2 分钟，超时算失败），再做后续步骤。

**v2 门槛（非物理 selected Standalone，真实采样器）。** 每次运行先按观察到的结果分到下面两支之一，再按该支的门槛判定：

A 支（未锁定：填满期间状态始终为 `AccountingHealthy`）：
- 填满后的 checkpoint 失败，错误链里有 `syscall.ENOSPC` 或 `resourcebudget.ErrPressure`。
- 填满期间每个请求要么完整结算，要么被 503 拒绝。
- 释放空间、压力回到 Healthy 后：checkpoint 成功，Close 返回 nil。
- 重启后 Usage 请求总数等于填满前基线加上完整结算数，Ledger 头不回退。

B 支（锁定：填满期间出现业务错误）：
- 第一个业务错误的错误链里有 `syscall.ENOSPC`，此时状态为 `AccountingUnavailable` 或 `AccountingRecoveryRequired`。
- 锁定之后的请求和 checkpoint 全部失败，没有任何一个结算成功，即失败封闭。
- 释放空间后状态不降回（锁定保持）。Close 的返回值只记录，不判定，见下面的 E1-2b。
- 重启后状态为 `AccountingHealthy`。Usage 请求总数在 [基线 + 完整结算数, 基线 + 完整结算数 + ENOSPC 业务错误数] 之内，即已写入 WAL 的事实可以被计入，但不会凭空多出。Ledger 头不回退。

两支共同的门槛：
- 填满期间，与填满前相比，数据目录里没有新增文件名。
- 两支各自至少观察到两次通过，任何一次运行失败都算失败。为控制时间，总运行次数上限为 10 次；到上限时某一支没满两次，就如实记为“该支证据不足”，不算通过。

**反向验证（替换 v1 的“把 ENOSPC 误判为 ErrRecovery”，那一项在 v2 下不再构成失败）：** 在反向构建中，让 `Status.MarkUnavailable` 和 `Status.RequireRecovery` 都不做任何事。B 支必须因“出现业务错误后状态仍为健康”而失败。反向运行到 B 支出现为止，上限 10 次。要确认替换确实生效。

**E1-2b（单列，在点 2 v2 之后处理）：锁定后 Close 返回错误。** 预先定下判定方法：在压力已回到 Healthy 的 B 支运行中，检查 Close 的错误。
- 如果只剩锁定导致的部分（`ledger projection is not available` 一类），就判定为失败封闭的正确报告，不修。
- 如果仍有压力拒绝或其他与锁定无关的错误，就作为缺陷单独修。

运行设置与点 1 相同：512 MiB ext4，`runner-512.py`，`HALRO_TERMINAL_ACTUAL_OS_PRESSURE=1`。

### 40.3 点 2 v2 结果与 v3 登记（2026-10-12 16:46）

**v2 未通过。** 第 1 次运行（`p2v2-run-1`）：
- 填满后，checkpoint 被压力拒绝。
- 第一个请求在 `BeginRequestDetailed` 处收到 `new resource admission stopped by pressure`，状态健康。
- v2 只把 503 或 nil 算作“未失败”，于是这次被判为“第一个业务错误不是 ENOSPC”。

**原因是测试的分类与生产不一致。** 生产网关在 Provider 调用之前就把 `BeginRequestDetailed` 的任何错误映射为 503 `accounting_unavailable`（`internal/gateway/service.go:496`），这一步还没有接纳任何工作。测试 handler 直接调用记账，没有这一层映射。v1 的“既不是完整结算也不是 503”也有同样的问题。v1 当时没有区分失败发生在 Begin 还是 Finalize，所以那次失败的阶段已无法确定。

v2 的结论保持为“未通过”。本节在任何 v3 运行之前提交。

**v3 分类（替换 v2 的“业务错误”判定，按失败阶段区分）：**
- **准入拒绝**：中间件返回 503，或 Begin 返回错误。生产中两者都是 Provider 调用之前的 503。
- **接纳后失败**：Begin 成功、Finalize 返回错误。
- **完整结算**：Begin 与 Finalize 都成功。

**v3 门槛。** 两支的划分不变（A：状态始终健康；B：出现锁定）。

A 支：
- 填满后的 checkpoint 失败，错误链里有 `syscall.ENOSPC` 或 `resourcebudget.ErrPressure`。
- 每个请求要么完整结算，要么被准入拒绝；Begin 返回的错误必须是 `ErrPressure`。不允许出现接纳后失败。
- 释放空间、压力回到 Healthy 后，checkpoint 成功，Close 返回 nil。
- 重启后 Usage 请求总数等于基线加完整结算数；Ledger 头不回退。

B 支：
- 第一个非压力失败（Begin 或 Finalize）的错误链里有 `syscall.ENOSPC`，此时状态为 `AccountingUnavailable` 或 `AccountingRecoveryRequired`。
- 锁定之后，没有任何请求完整结算，checkpoint 失败。
- 释放空间后状态不降回。Close 只记录，不判定（E1-2b）。
- 重启后状态健康。Usage 请求总数在 [基线 + 完整结算数, 基线 + 完整结算数 + ENOSPC 失败数] 之内；Ledger 头不回退。

共同门槛、次数（每支各 2 次，上限 10 次）、反向验证和 E1-2b 都与 §40.2 相同。

### 40.4 点 2 v3 结果：未通过，查到一个失败封闭缺陷（2026-10-12 16:49）

驱动按 §40.2 的规则运行，任何一次失败即停止。

**第 1 次（`p2v3-run-1`）：A 支，通过。**
- 填满后，checkpoint 失败（`bind selected Usage maintenance: ledger projection native source verification failed`，错误链里有 ENOSPC 或压力），状态健康。
- 3 个请求都在 Begin 处被压力拒绝。
- 释放空间后 checkpoint 成功，Close 返回 nil。
- 重启后 Usage 106→106，Ledger 头不变。

**第 2 次（`p2v3-run-2`）：A 支，未通过。**
- 填满后，checkpoint 在 `advance selected Ledger directory`（写 `directory/root.next.pending`）处撞上 ENOSPC。
- 同一个错误链里，`publishSelectedCheckpointLocked` 退出时的恢复步骤 `Usage.ActivateTypedLive` 也失败了，报 `disk Usage projection row or index is invalid` 与 `new resource admission stopped by pressure`。
- 状态仍为 `AccountingHealthy`（0）。3 个请求都被压力拒绝。
- 释放空间、压力回到 Healthy 后，checkpoint 失败：`disk Usage projection is retired`、`maintenance Ledger owner is unavailable`。

**缺陷（`internal/statecut/services_checkpoint.go` 中 `publishSelectedCheckpointLocked` 的 live 恢复）：**
- 在 `BindTypedMaintenance` 之后失败的 checkpoint，会在退出时调用 `ActivateTypedLive` 把 Usage 投影恢复为实时状态。这次恢复失败了，失败只被并入返回的错误，没有锁定 Ledger 状态。
- 结果是 Usage 实时投影已经不可用，之后每次 checkpoint 都失败，只有重启才能恢复，而 Runtime 却一直报告健康。
- 内存有界的设计依赖 checkpoint 推进，所以这是“状态健康却已不能推进”的失败开放形状，违反失败封闭。
- 次要问题：恢复读 `meta/state` 时，被压力拒绝的读取会与 `ErrDiskUsageCorrupt` 一起返回，错误文本写成“row or index is invalid”，把一次压力拒绝报成了损坏。
- 是什么把投影标记为退役，尚未定位到具体代码行。

**还没有做的：**
- 没有复现计数，只出现过 1 次。
- 没有生产修复。修复属于生产代码改动，范围交用户裁定。
- 点 2 门槛不变，结论“未通过”。

### 40.5 §40.4 缺陷的修复登记（2026-10-12 18:33，用户裁定“现在修”）

**修复内容（生产代码，两处）：**
1. **恢复失败时锁定状态。** 位置是 `internal/statecut/services_checkpoint.go` 的 `publishSelectedCheckpointLocked`。live checkpoint 在 `BindTypedMaintenance` 之后失败时，退出前会调用 `ActivateTypedLive` 把 Usage 投影恢复为实时状态。修复后，如果这次恢复也失败，就调用 `NativeLedger.Status().RequireRecovery()`。理由：此时 Usage 实时所有者仍绑在已关闭的维护租约上，只有重启能重新绑定，与 `finishPhysicalCommonSelectionWithWorkspaceLocked` 失败时的处理一致。
2. **被拒绝的读取不再报成损坏。** 位置是 `internal/usage/typed_rows.go` 中 `usageEngine.View` 的 COW 分支。读取失败时（`tx.failure`，例如压力拒绝），回调或前置检查因为读不到行而得出的“损坏”结论不可信。修复后，只要读取失败且失败本身不是 `ErrDiskUsageCorrupt`，就只返回读取失败本身，不再附加 `ErrDiskUsageCorrupt`。

**不在本次修复内（记为后续）：**
- 让恢复免受新工作准入的压力约束：恢复的是已有状态，理应不必申请新准入，但这属于设计改动。修复后，压力下的恢复失败会按失败封闭锁定，需要重启。
- `Update` 与其他读取路径的同类误判。

**确定性测试**（`internal/statecut`，跨平台，进入普通 `make test`）：
- 新增一个不导出的阶段钩子，生产为 nil，与 §30 做法相同。它在 `AdvanceFromNative` 之前被调用，返回的错误替代目录推进。
- **注入例**：钩子把资源管理器置为 `Pressured`，并返回注入错误。门槛：
  - checkpoint 返回的错误链里有注入错误。
  - Ledger 状态为 `AccountingRecoveryRequired`。
  - 错误链里有 `resourcebudget.ErrPressure`，没有 `usage.ErrDiskUsageCorrupt`。
- **对照例**：钩子只返回注入错误，压力保持 Healthy。门槛：
  - 恢复成功，状态保持 `AccountingHealthy`。
  - 去掉钩子后的下一次 checkpoint 成功。这说明锁定只在恢复失败时发生，没有扩大范围。

**反向验证（两项都必须失败，并确认替换确实生效）：**
- 去掉修复 1：注入例因状态仍健康而失败。
- 去掉修复 2：注入例因错误链里有 `ErrDiskUsageCorrupt` 而失败。

**修复之后：**
- 先把点 2 测试的分支判定改成按状态，与 §40.2 的定义一致。原来按“是否出现请求失败”判定，这是测试缺陷：锁定也可能来自 checkpoint 本身。
- 然后按 v3 门槛在 Linux 上重跑点 2：每支各 2 次，上限 10 次。
- 再做 §40.2 登记的反向验证。

### 40.6 §40.5 测试设计的修订（在修复代码之前提交）

按 §40.5 写的注入例，在修复前的代码上**并没有**复现缺陷。在目录推进时置为 `Pressured` 后，恢复失败在 `ActivateTypedLive` 的 `ValidateNativeWriter`（`verify maintenance Ledger prefix`），这条路径本来就会锁定状态。Linux 上的失败路径不同：`ValidateNativeWriter` 已经通过，压力出现在 `View` 拿到快照之后、读 `meta/state` 之前，单一的压力开关触发不到这个时序。因此把测试拆成两个，每个对应一处修复：

**`internal/statecut`：`TestSelectedLiveCheckpointLatchesWhenUsageRestoreFails`**
- 阶段钩子增加 `usage-restore` 点：在真实的 `ActivateTypedLive` 之后，把钩子返回的错误并入恢复错误。
- **restore-failed 例**：注入目录推进失败和恢复失败。门槛：错误链里两者都有，状态为 `AccountingRecoveryRequired`。
- **restored 例**：只注入目录推进失败。门槛：状态保持健康，下一次 checkpoint 成功。

**`internal/usage`：`TestTypedViewReportsRefusedReadNotCorruption`**
- `usageEngine` 增加一个只供测试的 `pinned` 钩子，生产为 nil，在 `View` 拿到快照之后调用。测试在这个钩子里置为 `Pressured`。
- 门槛：
  - 错误链里有 `ErrPressure`，没有 `ErrDiskUsageCorrupt`。
  - 恢复 Healthy 后 `ReadStats` 成功，投影没有退役。
- 修复前，这个测试得到的错误与 Linux 上 `p2v3-run-2` 的完全相同（`row or index is invalid` 加 `new resource admission stopped by pressure`）。

**修复前已经运行过（属于诊断，不计入验收）：**
- restore-failed 例失败（状态 0），restored 例通过。
- View 例失败，错误同上。

修复后：
- 两个测试都必须通过。
- 反向验证仍按 §40.5，分别去掉修复 1 和修复 2。
- §40.5 中的点 2 Linux 重跑计划不变。

### 40.7 修复结果与点 2 v4 登记（2026-10-12 18:38）

**修复（`450ed1e2`）：**
- **修复 1**：恢复失败时调用 `RequireRecovery`。
- **修复 2**：`View` 中被拒绝的读取只返回拒绝本身。

**修复后两个确定性测试都通过。**

**反向验证（都失败，替换确认生效）：**
- 去掉 `RequireRecovery`：restore-failed 例报“failed Usage restore left accounting unlatched 0”。
- 让 `usageReadRefused` 恒为 false：View 例报“refused read reported as corruption”。

**点 2 v4 登记。** 测试的分支判定改为按状态（v3 的测试是按请求失败判定的），并补上 v3 门槛没有覆盖的情形：锁定由 checkpoint 本身造成，例如修复 1 生效的那条路径。
- **B 支第一条门槛改为**：最先造成锁定的操作，错误链里必须有 `syscall.ENOSPC`，这个操作可以是 checkpoint，也可以是请求。在锁定之前失败的请求，必须在失败时就造成锁定。
- **其余门槛不变**，包括：
  - 锁定后没有请求完整结算。
  - 填满期间没有新增文件。
  - 每支各 2 次、上限 10 次。
  - 重启后的 Usage 范围。
- **反向验证仍按 §40.2**：让 `Status.MarkUnavailable` 与 `RequireRecovery` 不做任何事，B 支必须失败。

### 40.8 点 3 登记补充（2026-10-12 18:40，在任何点 3 运行之前提交）

**为什么要补。** 按 §40 的原登记，文件系统完全填满后，汇总请求会在 `os.Mkdir` 暂存目录这一步就因 ENOSPC 失败，根本碰不到 bbolt 的 `summary.db`。这样“去掉暂存清理”的反向验证不可能失败，按“反向验证不失败就不算证据”的规则，原登记做不出有效证据。

**补充做法（`TestSelectedUsageSummaryAtActualENOSPCRemovesScratch`）：**
- 先结算 40 个请求，取参照汇总：调用管理端 handler 所用的 `readDiskUsageSummary`（任何错误都映射为 503），按项目分组，然后编码。
- 依次扫描剩余空间 h ∈ {0, 1, 2, 3, 4, 6, 8, 12, 16, 24, 32, 48, 64} 个块。对每个 h：先填到真实 ENOSPC，再把填充文件截短 h 个块，然后发汇总请求。

**每个 h 的门槛：**
- 请求成功时，编码结果与参照逐字节相同；请求失败时，没有任何部分结果。
- 暂存目录 `usage-summary-scratch` 不存在。
- 状态保持 `AccountingHealthy`。

**整体门槛：**
- 至少有一个 h 的失败满足两个条件：错误链里有 `syscall.ENOSPC`，并且失败不在 `mkdir`。也就是暂存已经建立，之后才撞上 ENOSPC。如果做不到，就如实记为“未覆盖 bbolt 扩页”，算作未通过。
- 释放空间、压力回到 Healthy 后：同一查询与参照相同，`ReadStats` 成功，状态健康。

**对 §40 的一处改动：** 原登记要求“在线 Usage 读取不受影响”。真实采样器在压力下会按设计拒绝读取，所以这一项改为在压力回到 Healthy 后检查。

**次数与反向验证：**
- 连续通过两次。
- 反向验证：把 `summaryCleanup` 换成只关闭 DB、不删除文件和目录。必须因暂存残留而失败，并确认替换确实生效。

### 40.9 点 4 登记补充（在任何点 4 运行之前提交）

**阶段钩子。** 选用一个不导出的 context 键 `selectedBackupMetadataPhaseKey`，生产 context 从不携带它。它有两个阶段：
- `materialize`：在镜像费用申请之前。
- `materialize-done`：在本步骤的所有费用归还之后，并带上本步骤的错误。

**剩余空间扫描**，理由同 §40.8：完全填满时，ENOSPC 可能落在物化之前的步骤上。
- h ∈ {0, 1, 2, 4, 8, 16, 32, 64, 128, 256} 个块。
- 对每个 h：在 `materialize` 阶段填到真实 ENOSPC，再截短 h 个块，然后运行完整的 `CreateBackupWithOptions`。
- 备份输出与暂存目录都在 ext4 上；暂存目录建在输出目录旁边。

**每个 h 的门槛：**
- 备份结束后，输出目录中没有 `.halro-backup-*` 暂存或镜像残留。
- `memory-state/metadata-root` 下所有文件逐字节不变。
- 如果物化步骤失败，`materialize-done` 时预算 `Used` 等于 `materialize` 时的值，即本步骤的费用全部归还。
- 如果备份失败，没有发布任何归档；如果备份成功，`VerifyBackup` 通过。

**整体门槛：**
- 至少有一个 h 的物化步骤本身以 ENOSPC 失败，并且整个命令的错误链里也有 ENOSPC。做不到就记为未覆盖，算未通过。
- 释放空间后，同一命令成功，`VerifyBackup` 通过。

**次数与反向验证：**
- 连续通过两次。
- 反向验证：去掉 `MaterializeMetadata` 失败路径上的 `image.Discard()`。必须失败：费用未归还，或镜像残留。要确认替换确实生效。

### 40.10 点 2 v4 结果（2026-10-12 18:46）

运行 `p2v4-run-1`～`-10`，10 次全部通过：A 支 9 次，B 支 1 次（`p2v4-run-4`）。

按 §40.2 的次数规则，A 支接受，B 支记为“证据不足”，不算通过。

**B 支那一次的过程（`p2v4-run-4`）：**
- 填满后，checkpoint 在目录推进处撞上 ENOSPC。这次 Usage 恢复成功，状态保持健康。
- 第一个请求在原生 WAL 写入第 205 条记录后，投影写入撞上 ENOSPC，Begin 失败，状态变为 `AccountingRecoveryRequired`。生产中这一步会返回 503。之后两个请求都失败。
- 重启后 Usage 102→103，这条已写入 WAL 的事实被计入；Ledger 头 204→206。

**E1-2b 按 §40.2 事先定的规则判定：** 锁定后 Close 只报 `ledger projection is not available`（锁定所致），没有压力或其他错误。结论：这是失败封闭的正确报告，不修。

**观察：** 被 503 拒绝的请求，接纳记录已经持久化，重启后会作为孤儿请求处理并计入 Usage 请求数，但调用方收到的是 503。这与“不确定时保守记账”的不变量一致，记在这里供 D-01/R-01 参考。

**修复后的 Linux 复核：** 修复 1 针对的 `p2v3-run-2` 路径（恢复失败）在 v4 的 10 次运行中没有再出现，修复 1 的证据目前只来自 §40.6 的确定性测试。

**v5 登记（只增加次数，不改门槛）：** 门槛与 v4 相同，只为 B 支补证。另起最多 30 次运行，直到新增 2 次 B 支通过为止；A 支照常判定，任何一次失败都算失败。

### 40.11 点 3、点 4 结果与点 2 反向验证（2026-10-12 18:47）

**点 3 接受。** `p3-run-1`、`-2` 连续通过。
- 剩余 0 块：失败在 `mkdir`。
- 剩余 1～6 块：失败都发生在写 `summary.db` 的过程中（即 bbolt 暂存库已经建立），共 5 档。
- 剩余 8 块及以上：被压力拒绝。
- 每一档的暂存都被移除，状态保持健康；释放空间后，结果与参照逐字节相同。
- 反向验证（`p3-reverse-1`，`summaryCleanup` 不删除文件）失败：参照查询留下的暂存使下一次查询在 `mkdir` 处得到 `file exists`，测试报 “summary scratch survived the query”。

**点 2 反向验证失败，构成证据。** `p2v4-reverse-1` 中，状态锁定被改为不做任何事。checkpoint 在目录推进处撞上 ENOSPC，Usage 恢复失败，状态仍为 0；释放空间后 checkpoint 报 `disk Usage projection is retired`，测试失败。这正是 §40.4 缺陷的路径：修复 1 去掉后，真实环境中会再次出现。

**点 4 未通过。** `p4-run-1` 在剩余 0 块时就失败，查出两个问题：
1. **失败的镜像连同暂存目录一起被保留。** 物化写 `metadata.db` 时撞上 ENOSPC，错误被标为 `ownedfile.ErrRetirement`（“actual IO retirement is uncertain”）。`createBackupWithWriter` 的清理遇到 `ErrRetirement` 就跳过，于是 `.halro-backup-stage-*` 留在输出目录里。离线命令退出后，没有任何东西会回收它，每次失败的备份都会多留一个。
2. **备份命令在 ENOSPC 下写了在线 Metadata 根。** 同一次运行中出现 `checkpoint audit log: metadata native/projection boundary requires close and authenticated recovery`，以及写 `memory-state/metadata-root/root.pending` 时撞上 ENOSPC。也就是说，备份命令（审计检查点）会写 selected 的 Metadata 根，在 ENOSPC 下使 Metadata 所有者进入需要恢复的状态。这次运行在检查 Metadata 根是否逐字节不变之前就已失败，下一次打开能否恢复也还没有验证。

点 4 的反向运行（`p4-reverse-1`）在同一处失败。因为正向运行本身就没通过，这次反向运行不构成证据。

E1 登记的 2 小时工作上限早已超出。点 4 的两个问题都涉及生产代码，修不修、按什么范围修，交用户裁定。

### 40.12 点 2 v5 第 1 次未通过：一次压力拒绝就锁定了状态（2026-10-12 18:48）

`p2v5-run-1` 的过程：
- 填满后，checkpoint 在目录推进处被压力拒绝（错误链里没有 ENOSPC）。
- 退出时恢复 Usage，恢复在 `ValidateNativeWriter` 中报 `verify maintenance Ledger prefix: new resource admission stopped by pressure`。
- 状态变为 `AccountingRecoveryRequired`。v4 门槛要求“最先造成锁定的操作必须撞上 ENOSPC”，因此判为失败。按规则，v5 在此停止。

**原因：这个问题在修复之前就存在，不是修复 1 引入的。**
- 位置是 `internal/ledger/maintenance.go`（`verify maintenance Ledger prefix`）。验证失败时，除 `ErrProjectionDiskLimit` 与 busy 外一律调用 `RequireRecovery`，`resourcebudget.ErrPressure` 不在豁免之列。
- §40.6 中修复前的诊断（压力注入例得到状态 3）走的也是这条路径。

**影响：**
- 非物理模式下，只要压力出现在 checkpoint 绑定 Usage 之后、恢复完成之前，就会把一次可重试的拒绝变成必须重启。
- 这与 §23“拒绝新工作而服务继续”中由压力承担的那一半相冲突：压力本来是“拒绝但继续”的机制，在这里却触发了锁定。

**根因与修复 1 是同一个：** 恢复已有状态时，仍然要申请受压力约束的新准入（§40.5 中记为“后续”的那一项）。可选方向：
- **(a)** 恢复复用 checkpoint 已经持有的许可。
- **(b)** 让维护前缀验证把压力拒绝和容量拒绝一样豁免（`ErrProjectionDiskLimit` 已有先例）；恢复失败仍由修复 1 锁定，所以单独做 (b) 并不能避免重启。
- **(c)** 在绑定 Usage 之前，就先取得恢复所需的全部准入。

这是设计改动，交用户裁定。在裁定之前，点 2 保持“未通过”。

### 40.13 用户裁定后的修复登记：恢复先取得准入（方案 c），点 4 的修复与门槛修订（2026-10-12 19:32）

用户采纳方案 (c)，点 4 按建议处理。本节在任何验收运行之前提交；下面写到的诊断都不计入验收。

**A. 恢复先取得准入（`internal/statecut/services_checkpoint.go`）**
- live checkpoint 在 `BindTypedMaintenance` 之前，先用 `Budget.TryAcquireRecovery` 取得一个 Recovery 父许可，RAM 为 `resourcebudget.ClaimRAMBytes`。取不到就返回 `admit selected Usage restore`，此时 Usage 还没有被绑定，保持实时状态。
- 退出时的 `ActivateTypedLive` 在 `restore.WithRecoveryScope` 中运行：它派生的 IO/Background RAM 准入都从专用 Recovery 储备扣除，不受压力影响。父许可在退出时释放。修复 1（恢复失败就锁定）保留。
- **诊断实测（statecut 夹具）：** 恢复同时需要 7～8 个 Recovery claim、64～128 MiB 的 Recovery RAM。claim 为 4～6 时、RAM 为 64 MiB 时都失败，claim 为 8、RAM 为 128 MiB 时通过。
- **为什么生产配置能容纳：**
  - 配置校验要求 Recovery 储备不少于 `accounting_recovery_ram_bytes`（≥128 MiB）加上其他 Recovery 用途。
  - 运行时的账务恢复（`authenticated_recovery_resources.go`）要求至少 8 个 Recovery claim，储备不少于“恢复量 + 派生读取 + claim”。
  - checkpoint 只在状态健康时运行，不会与账务恢复同时进行，所以这次恢复落在同一份已核定的额度内。这是推理，由 Linux 实测验证。
- **测试夹具改动：** `publish_test.go` 的 Recovery 改为 256 MiB 和 16 个 claim（原为 8 MiB、4 个，容纳不下恢复）。只增加容量，其他 statecut 测试需要全包回归。
- **确定性测试新增 `restored-under-pressure` 例：** 在目录推进时置为 `Pressured`。门槛：恢复成功，状态保持健康；压力回到 Healthy 后，下一次 checkpoint 成功。restored 与 restore-failed 两例不变。
- **反向验证：** 恢复不再放进 `WithRecoveryScope`，`restored-under-pressure` 必须失败。
- **点 2 v6（Linux）：** 门槛与 v4 相同。A 支至少 2 次、B 支至少 2 次，上限 30 次，任何一次失败即停。B 支出现概率约为 1/10，所以上限定为 30。

**B. 点 4 问题 1：bbolt 打开失败后，确定句柄已释放的就正常清理（`internal/metadatastate/materialize.go`）**
- **现状：** `bbolt.Open` 在文件建好之后失败时，bbolt 会自行关闭文件，但忽略关闭的错误（`_ = db.close()`），所以 `MaterializeMetadata` 把失败标成 `ownedfile.ErrRetirement`。备份清理看到这个标记就跳过，暂存目录被保留。
- **修复：** 保存交给 bbolt 的 `*os.File`。`Open` 失败后，由我们再调用一次 `Close`：如果返回 `os.ErrClosed`（bbolt 已关闭）或 nil（由我们关闭），句柄就确定已释放，不再标记 `ErrRetirement`，按普通失败删除镜像、归还费用。其他关闭错误仍标记 `ErrRetirement`。
- **确定性测试：** 新增一个只供测试的创建函数替换点（包级变量，生产时就是 `ownedfile.Create`）。测试让它返回同一文件的只读句柄，bbolt 初始化写页会失败。门槛：错误不含 `ErrRetirement`，镜像不存在，费用归还。
- **反向验证：** 去掉修复，测试必须失败。

**C. 点 4 门槛修订（前提错误）**
- **诊断（mac，selected 夹具）：** 一次**成功**的备份会改 `metadata-root/active.root` 与 `pages`，因为备份命令会写审计检查点。所以 §40 的“在线 Metadata 逐字节不变”前提不成立，这条门槛撤销。
- **改为：** 每次失败的备份之后，selected Runtime 能重新打开且状态健康；填满前写入的探针项目内容不变；`VerifyAudit` 通过。这同时回答问题 2：在 ENOSPC 下写 Metadata 根后，重启能否恢复。
- **点 4 其余门槛与 §40.9 相同**：没有暂存或镜像残留、物化费用归还、没有发布失败的归档、成功的备份能通过校验、至少一档在物化中撞上 ENOSPC、连续两次通过。反向验证仍是去掉 `image.Discard()`。
- **问题 2 的判定，事先定下：**
  - 如果重启能恢复、内容不变、审计校验通过，就判定为失败封闭，不修。
  - 如果重启失败，就作为缺陷另行处理。

### 40.14 v6 / 点 4 第二轮结果与点 4 v3 登记（2026-10-12 19:47）

**回归（mac）。**
- metadatastate、usage、resourcebudget 通过。
- statecut 有一例失败：`TestCommonSelectedServicesBudgetRefusalClosesGenerationWithoutRecoveryWrites`。它按旧的 8 MiB Recovery 储备计算占用量，夹具改为 256 MiB 后超出了容量。改为由夹具常量 `commonFixtureRecoveryRAM` 推算，仍然只留 16 MiB 普通 RAM。改后 statecut 全包通过。

**点 2 v6 接受。** `p2v6-run-1`～`-9` 全部通过，A 支 7 次、B 支 2 次。
- **修复 A 在 Linux 上的直接证据：** `p2v6-run-4` 正是 v5 失败的路径，目录推进处被压力拒绝。这次恢复在 Recovery 作用域中成功，状态保持 0；释放空间后 checkpoint 成功，Close 返回 nil。
- **B 支两次**（run 8、9）：目录推进撞上 ENOSPC 后恢复成功、状态保持 0，随后请求在 WAL 写入第 209 条记录后，投影写入撞上 ENOSPC，造成锁定。
  - 重启后 Usage 104→105，Ledger 头 208→210。
  - Close 只报锁定导致的错误，按 E1-2b 判定为正确报告。
- 点 2 的反向验证见 §40.11 的 `p2v4-reverse-1`。该二进制不包含修复 A，但反向编辑的对象（状态锁定）与 v6 相同。

**点 4 第二轮（`p4b-run-1`）未通过：8 分钟超时，没有一档在物化中撞上 ENOSPC。**
- **超时的原因：** 每档之后都删除了填充文件，于是每档都要以 O_SYNC 重新写满约 500 MiB。
- **覆盖不到的原因：** 剩余 0～64 块的每一档，物化步骤都先被真实采样器的压力拒绝（`new resource admission stopped by pressure`）。物化开始前就要申请准入，采样器在填充期间已经察觉到磁盘满。
- 超时前完成的各档中，暂存清理、重开健康、探针项目、审计校验都没有报错。
- 反向运行 `p4b-reverse-1` 恰好赶在采样器之前，在剩余 0 块时就在物化中撞上 ENOSPC，并因“failed materialization kept fees”失败。这说明 ENOSPC 能否落进物化取决于时序，命令层面无法稳定覆盖。

**点 4 v3 登记（本节在任何 v3 运行之前提交）：**
1. **组件测试（新增，`internal/metadatastate`，Linux，ext4，手动压力保持 Healthy）：** 新增一个只供测试的包级阶段钩子，生产为 nil，在 `bbolt.Open` 成功后调用。分两种情形：
   - **(i) open 前填满：** 直接调用 `MaterializeMetadata`，`Open` 写首页时撞上 ENOSPC。这是修复 B 的真实路径。
   - **(ii) 打开后填满：** 钩子按剩余空间 h ∈ {0, 1, 2, 4, 8, 16} 块填充，写入事务时撞上 ENOSPC。
   - 每种情形的门槛：错误链里有 `syscall.ENOSPC`，没有 `ownedfile.ErrRetirement`；镜像文件不存在；费用 `Used` 与调用前相同；释放空间后同一调用成功。
   - (ii) 中至少一档在写入事务时撞上 ENOSPC。
   - 连续通过两次。
   - **反向验证：** 去掉失败路径上的 `image.Discard()`，必须失败；再去掉修复 B，情形 (i) 必须失败。
2. **命令层测试（保留，门槛修订）：**
   - 每档之后不再删除填充文件，只截短 64 MiB，留出重开所需的空间。
   - “至少一档在物化中撞上 ENOSPC”这一项移到组件测试，因为命令层面由真实采样器先拒绝，这是上面查明的事实。
   - 其余门槛不变：没有暂存或镜像残留；物化步骤失败时费用归还；没有发布失败的归档；失败后重开健康、探针项目不变、`VerifyAudit` 通过；释放空间后备份能通过校验。
   - 连续通过两次。

### 40.15 点 4 v3 结果；修复 B 撤销（2026-10-12 19:53）

**驱动缺陷（已修）。** `e1-driver.sh` 用 `--- PASS: 名字` 判定通过，子测试的 PASS 行也会被匹配上，所以两次组件反向运行在汇总里被误记为 PASS。现已改为只匹配顶层的 `^--- PASS: 名字 (`。逐个核对日志的结果：
- `p4c-run-1`、`-2`：顶层 PASS。
- `p4c-reverse-discard-1`、`p4c-reverse-b-1`：顶层 FAIL。

**组件测试（`ms.test`，包含修复 B）：**
- 两次都通过。
- 打开后填满时，剩余 0～8 块的 5 档都在写入事务时撞上 ENOSPC，镜像被删除、费用归还；16 块时成功。
- 反向验证 discard：每一档都报 “failed image file survived”。
- 反向验证 B：Open 前填满一例报 “ENOSPC image left retirement debt”。

**命令层（`p4d-run-1`）未通过。**
- 剩余 1、4、8 块时，物化在写 `metadata.db` 时撞上 ENOSPC，暂存被清理，重开健康、探针不变、审计校验通过。
- 剩余 0、2 块时被压力拒绝。
- 剩余 16 块时，物化成功，但下一步备份观察索引 `halro-backup-observation_*.db` 在 bbolt `Open` 时撞上 ENOSPC，被标为 `ownedfile.ErrRetirement`，暂存目录因此被保留。

**修复 B 撤销（`2a267299`）。** 查到第二处同样的“打开失败即退役债务”时，发现这是仓库在三处刻意采用的设计：`ownedfile.OpenExclusiveBolt`、`store/bolt/file_limit.go`、`backup/observation_index.go`。三处注释一致：bbolt `Open` 失败时会丢掉自己的关闭错误，“句柄已关闭”不能证明 mmap/freelist 已退役，所以即使额外的 Close 成功，也保留债务。修复 B 的前提（句柄关闭就足以证明）与这条刻意保守的不变量冲突，因此撤销。组件测试的 Open 前一例改为断言这一设计：镜像作为债务保留，文件存在，Discard 拒绝删除。

**于是点 4 的门槛与设计冲突。** 只要 ENOSPC 落在任何一个 bbolt 文件的 `Open` 上（镜像或观察索引），失败的离线备份就会按设计留下 `.halro-backup-stage-*`。仓库里没有任何东西回收它：进程退出后 mmap 必然已经解除，但目录仍留在输出目录中，每次失败多留一个。这需要用户裁定：
1. 接受保留，在备份运维文档中写明“失败后可删除的残留”。
2. 由下一次备份命令（在数据目录锁下）回收它能证明属于已退出进程的残留暂存，例如在暂存中写入属主标记。
3. 其他方案。

在裁定之前，点 4 保持“未通过”。修复 A（`6dd8a8a3`）和点 2 v6 的结论不受影响。

### 40.16 E1 收尾（用户裁定）

2026-10-12 23:40 记录。E1 超出 2 小时上限很多，原因是门槛按设想写、写前没有核对代码（点 2 v2 的分类、点 4 的“暂存不残留”与刻意保留的 bbolt 退役债务冲突），每次登记有误都要重跑一整轮多次运行。用户裁定按以下方式收尾，并改用更省的验证方式。

**E1 结论。**
- 点 1、点 3：接受。
- 点 2：v6 接受（§40.14）。修复 1、修复 2（`450ed1e2`）和修复 A（`6dd8a8a3`）均有确定性测试和反向验证，修复 A 另有 Linux 直接证据。
- 点 4：未通过，不放宽门槛。物化写入时撞上 ENOSPC 的路径通过；bbolt `Open` 撞上 ENOSPC 时，按设计留下 `.halro-backup-stage-*`，没有回收者。回收转入 F1。
- 不再为 E1 跑 Linux。

**F1 范围扩大。** F1 除 `.archive-*.tmp`（§28）外，也回收失败的离线备份留下的 `.halro-backup-stage-*`：只回收能证明属主进程已退出的残留。判据不变：遗留在下次启动、维护或备份时被回收，反向验证能失败。

**之后的验证方式（A1、E2、F1）。** 以确定性测试加一次反向验证为证据；需要真实 Linux 的只跑一次确认，不做多次统计。登记前先读被判定的代码与数据。门槛照旧先登记、事后不放宽；只改测试写法不重新登记。看板每项只记结论和证据位置。

**I-03 关闭之后、开始 I-06 之前**，用约半天审查剩余各项和状态模型：哪些是发布前必需的，每项能否写成具体可判定的完成条件，状态之间的交互能否合并简化。由用户据此决定后续范围。

### 40.17 F1 完成（2026-10-12 23:53）

**修复（`82f0ffce`）。**
- Usage 归档临时文件：运行时的 Usage Exporter 持有数据目录锁，打开时在库存统计之前，按精确的临时文件名（`CreateTemp`/`MkdirTemp` 生成的数字后缀）回收崩溃遗留。固定名的可续做状态（`.archive-date-*-v1.json`）与其他文件不动。本进程内另有 Exporter 在用同一根目录，或之前有 Exporter 关闭失败时，跳过回收。
- 失败备份的暂存：暂存目录先写属主记录（数据目录、进程标记）。下一次同一数据目录的备份在锁下回收其他进程留下的暂存；没有记录、属于其他数据目录或本进程的，一律保留。Linux 运行同时确认：退役债务会让命令一直持有数据目录锁直到进程退出，所以能拿到锁就说明属主已经退出，进程标记只是双重保险。

**证据。**
- `TestExporterReclaimsCrashTemporaries`、`TestBackupReclaimsStagingRetainedByAnExitedProcess` 通过。反向验证共 5 处，全部按预期失败：去掉回收调用、去掉进程内守卫、去掉备份回收调用、去掉进程标记判断、去掉属主记录写入。
- 点 4 命令测试改为：失败备份因退役债务留下的暂存，必须带本进程记录并保留数据目录锁；模拟进程退出后，下一次备份必须回收它。退役债务保留费用，这是 §40.15 已确认的设计，费用检查不再适用于这种情况。Linux 跑一次（`f1b-p4d-run-1`）通过：剩余 16 块时，观察索引撞上 ENOSPC，暂存被保留并带有记录，下一次备份回收了它，之后的备份校验通过。
- 点 4 由此关闭。

## 41. A1 登记：等待点清单与契约测试（I-03 收尾）

2026-10-12 23:55 登记，基于 `7a51035f`。运行前提交；门槛事后不放宽。

**范围（按代码实际查得）。** “等待点”按调用形状机械识别，只看 `internal/` 下的生产代码（不含 `_test.go`）：
1. 排队等待：调用 `resourcebudget.WorkQueueContext` 或 `resourcebudget.AcquireWorkLane`。当前 9 处，上界为 `MaxWorkQueueWait`（30 秒）或调用方更早的期限。
2. 汇合等待：无参数的 `.Wait()`（`sync.WaitGroup`、`sync.Cond`）。当前 32 处。
3. 带参数的等待：方法名以 `Wait` 开头（`WaitConfirmed`、`WaitStartupAudit`、`WaitRecovery*`、`WaitReady` 等）。当前 48 处。

不在范围内：channel 收发与 `select`、互斥锁、文件与网络 IO 本身。这些无法按调用形状可靠识别，本项不声称覆盖。

**交付。**
- 清单写在契约测试里，每一项注明：位置（文件、所在函数、调用表达式）、类别、上界来源（固定常量、调用方期限，或“等待已进入的工作结束”及该工作自身的上界）。
- 契约测试用 `go/parser` 扫描生产代码：出现清单外的等待点，或清单项已不存在，都判失败。

**门槛。**
- 扫描结果与清单完全一致。
- 每项都有非空的上界来源。凡是没有计时上界的，写明原因；不把它们写成有上界。
- 反向验证：在生产代码中加一个清单外的等待点，测试失败；删去清单中的一项，测试失败。
- 发现没有任何上界、又不属于“等待已进入的工作结束”的等待点时，记为发现，报给用户，不在本项里顺手修。

### 41.1 A1 结果

**交付。** `internal/resourcebudget/wait_points_test.go`：清单 85 项（合计 89 处调用），契约测试 `TestWaitPointInventory` 用 `go/parser` 扫描 `internal/` 的生产代码，清单外的等待点或已经不存在的清单项都判失败。

**清单概况。**
- 排队等待 9 处：上界都是 `MaxWorkQueueWait`（30 秒）或调用方更早的期限。
- 带参数的等待 48 处，分两类：
  - 复制确认与恢复目标（`WaitConfirmed`、`WaitRecovery*`）：上界为调用方 ctx，复制不可用时立即返回。关停时用的是 `RunWithReady` 的 shutdownCtx。
  - 启动审计（`WaitStartupAudit`、`Wait`、`WaitReady`）：上界为调用方 ctx，以及审计自身的期限（`startupaudit.MaxDeadline` 15 分钟，进度超时不超过 30 秒）。各 Close 先取消审计，再汇合。
- 汇合等待 32 处，全部没有自己的计时上界，由被汇合的、已经进入的工作结束来决定，其中可能包括不响应取消的物理 IO（与 §9 一致，记为未退役）。只有 `budget.(*Manager).applyDurableRecord` 例外：ctx 取消时经 `context.AfterFunc` 唤醒，上界为调用方 ctx。

**没有逐层核对的 3 项**（已在清单里注明 “not traced in A1”）：告警投递扇出、deadman 探测、管理端 catalog 拉取的汇合。它们的上界取决于各自出站调用的超时，本项没有逐层核实。三者都不在内存受限状态路径上。

**发现。** 没有发现既无上界、又不是“汇合已进入工作”的等待点。

**证据。**
- 契约测试通过。
- 反向验证两项都按预期失败：在生产代码里加一个清单外的 `Wait()`，报 unlisted；删去清单中 `usage/typed_rows.go` 一项，报 unlisted。
- `resourcebudget` 全包通过。

**未覆盖。** channel 收发、`select`、互斥锁、IO 本身（见 §41 的范围）。

## 42. E2 登记：6 个命令的正常退出与 SIGKILL（I-03 收尾）

2026-10-13 00:03 登记，基于 `b9cf3651`。运行前提交；门槛事后不放宽。

**判据在代码里的含义（登记前已核对）。**
- 命令的预算 Manager 只存在于进程内。重启后的“接手清单”就是 `diskinventory.Reconcile` 对整个数据目录的重新扫描（`preparePersistentInventory`），它不信任任何保存下来的计数。
- 持久费用只有磁盘费用。RAM 与未确认字节都不跨进程，所以“正常退出后只剩持久 claim”可以检查为：命令返回后，共享 Manager 的 `Used.RAM` 只剩这些 claim 自身的控制开销（claim 数 × `ClaimRAMBytes`），`Used.Unconfirmed` 为 0。
- `diskinventory` 文档写明：启动基线与各消费者另行预留的增长上限，可能保守地重复计算。这是设计如此，本项不测，也不算违例。

**做法。**
- `backup create`、`backup restore`、`usage compact`、`usage prune`、`ledger seal` 共用一个带预算、开启持久清单的离线 Standalone 夹具（`budgetedBackupOperationFixture` 一类）。元数据迁移（`halro metadata migrate`）沿用 `cmd/halro` 已有的真实 SIGKILL 夹具。
- SIGKILL 落点：子进程从命令的内部入口运行，用已有的观察回调（`createBackupWithWriter` 的 writer、`restoreBackupWithExtract` 的 extract、`compactUsageDisk` 的 afterStep、`sealLedger` 的 observe），在命令中途写标记文件后挂起，父进程随即 SIGKILL。没有回调的命令，在确认可用的落点之后再写进结果，不新增生产代码钩子。
- 每个命令都跑一次正常退出（子进程跑完），一次 SIGKILL 后在同一数据目录重跑该命令直到完成。每种只跑一次，不做多次统计。

**门槛（每个命令）。**
1. 正常退出：命令返回后，`Used.RAM` = claim 数 × `ClaimRAMBytes`，`Used.Unconfirmed` = 0。
2. 两条路径结束后，都对数据目录做一次持久清单 `Reconcile`。基线的 `ChargeBytes` 必须等于测试按同一规则（每个文件取逻辑大小与已分配字节的较大者，加目录已分配块）独立算出的值。
3. 不泄漏：SIGKILL 后重跑完成，数据目录与输出目录的文件集合，在把随机部分（数字串、长十六进制串）规范化之后，与正常退出路径的集合相同，不能多出文件。
4. 不重复计费：两条路径结束后 `VerifyUsage` 都没有缺行、多行或重复行，且行数相同。
5. 反向验证：把 F1 的回收去掉，至少一个会留下临时文件的命令必须在门槛 3 上失败。

发现违例时，记为发现并报给用户，不在本项里顺手修。

### 42.1 E2 结果（2026-10-13 00:15）

**门槛 1 的登记修正（如实记录）。** 登记时漏读了 `command_metadata_budget.go`：命令作用域给每个元数据文件路径保留一个 8 KiB 的描述 RAM，连同磁盘上限一起保留到作用域结束，注释写明是设计。第一次运行时 4 个 app 命令都只多出这一项，数值精确等于 8 KiB × 元数据文件 owner 数。门槛 1 因此改为精确等式：
- Recovery 类的 RAM 等于 8 KiB × 元数据文件 owner 数，且这些 owner 保留各自的磁盘上限；
- Control 类的 RAM 等于 claim 数 × 1 KiB；
- 其余类别的 RAM 与未确认字节都为 0。

没有引入容差。

**测试。** `internal/app/command_restart_e2_test.go`（5 个命令），`cmd/halro/metadata_migration_e2_test.go`（元数据迁移）。都只在 `HALRO_E2=1` 时运行：其中的发现尚未裁定，测试会失败，不能进常规门禁。

**SIGKILL 落点（每个只跑一次）。**
- `backup create`：writer 回调里，暂存已建好、尚未写归档。
- `backup restore`：归档已解出到暂存之后。
- `usage compact`：第一步提交之后。
- `usage prune`：第二个 Usage 引用目录出现时（进入回收阶段）。没有回调，由父进程监视 TMPDIR 触发。
- `ledger seal`：最终观察回调里，封存完成、关闭之前。
- 元数据迁移：`staging-synced` 边界，沿用已有的真实 SIGKILL 子进程。

**结果。**

| 命令 | 门槛 1 | 门槛 2 | 门槛 3 | 门槛 4 |
| --- | --- | --- | --- | --- |
| backup create | 通过 | 通过 | **未通过**：TMPDIR 留下 `halro-segment-directory-*.idx` | 通过 |
| backup restore | 通过 | 通过 | **未通过**：数据目录上一级留下 `.halro-restore-stage-*`，内含完整解出的数据副本（ledger、audit、governance、metadata、config），重跑 restore 不回收 | 通过 |
| usage compact | 通过 | 通过 | **未通过**：TMPDIR 留下 `halro-usage-verify-*`（`ledger.db`、`usage.db`） | 通过 |
| usage prune | 通过 | 通过 | **未通过**：同上，TMPDIR 留下 `halro-usage-verify-*` | 通过 |
| ledger seal | 通过 | 通过 | 通过 | 通过 |
| 元数据迁移 | **未通过**：完整命令正常退出后，Background 类仍有一个带 4 KiB RAM、48 MiB 磁盘的 owner。认证存储 Close 时本应释放这 4 KiB，根因未查实 | 通过 | **未通过**：数据目录内留下 `memory-state.interrupted-0/migration.intent`。这是迁移的既定设计（被打断的候选不当作权威，只保留不删除），没有回收者 | 不适用（夹具无 Usage） |

- 门槛 2 全部通过：6 个命令在两条路径结束后，启动清单的 `ChargeBytes` 都与独立复算相等。
- 门槛 5：去掉 F1 的备份暂存回收后，`backup create` 在门槛 3 上多报出整个 `.halro-backup-stage-*`。恢复 F1 后，只剩 TMPDIR 那一项。

**待用户裁定。** 以上发现按登记都不在本项里修。

### 42.2 E2 发现的处理（用户裁定：按建议执行）

2026-10-13 00:31 记录。

1. **restore 暂存（已修，`33371250`）。** restore 先解包、后拿数据目录锁，所以 F1 的“拿到锁即属主已退出”在这里不成立：并发的另一个 restore 可能正在写它自己的暂存。改为逐个暂存证明存活：
   - 属主对暂存里的 `owner.lock` 持有排他 flock，并写入所恢复的数据目录；暂存删除成功后才释放锁；如果暂存成了退役债务，就把锁一直持有到进程退出（文件句柄保持可达，避免被 GC 关闭）。
   - 下一次 restore 在检查剩余空间之前，删除同一数据目录下能拿到锁的暂存。

   确定性测试覆盖了已退出、存活、其他数据目录、无记录、退役债务 5 种情况。反向验证 3 处都按预期失败：去掉回收调用、去掉存活判断、去掉持锁记录。E2 复验 restore 通过。
2. **迁移的 4 KiB 不是缺陷。** `authenticatedstate.(*Store).Close` 的注释写明：创建者把固定的清理和控制 RAM 与持久回执一起保留，直到 Discard 退役命名空间；迁移成功后候选按设计保留供审阅。迁移测试的门槛 1 因此改为精确等式：Background 类恰好是 4 KiB，且保留存储的磁盘上限。
3. **TMPDIR 私有引用**（`halro-segment-directory-*.idx`、`halro-usage-verify-*`、`halro-usage-maintenance-*`）：写入 `docs/guides/backup-restore.md`（Interrupted commands and leftover files），回收转 D-01/R-01。
4. **被打断的迁移候选**（`memory-state.interrupted-N`、`.unqualified-N`）：接受，写入同一节。已核实这些名字在 bootstrap 提交前会让默认启动拒绝（`memoryMigrationNamespacePresent`），所以文档写明：只有迁移完成、激活后的服务器能启动之后，才可以离线归档或删除；每类只有 4 个槽位。

**测试进入常规测试集**（`cb11ce06`）。去掉 `HALRO_E2` 开关；只放行第 3、4 项明确列出的残留名字，其他任何多出的文件仍判失败。迁移测试还要求 SIGKILL 后确实留下了被打断的候选，以证明落点确实到达。6 个命令全部通过。

## 43. 门禁 17 失败：空 `root.pending` 导致永久拒绝打开（I-03 收尾）

2026-10-13 10:49 记录。

**现象。** 门禁 17（`900eb29f`，`exit=2`，1995 s）在 `make test` 中只有一个失败：`internal/app` 的 `TestHAMemoryPendingPrimaryActualSIGKILLReopensSavedSevenRoots`，重开报 `authenticated store requires reopen and native eligibility check` / `authenticated snapshot corrupt or unavailable`。单独 8 路并行跑 200 次全部通过，属于时序窗口。

**根因。** 普通提交（非 physical）先用 `ownedfile.Create` 建出空的 `root.pending`，之后才一次写入整个 root。两步之间被 SIGKILL，会留下长度为 0 的 `root.pending`；打开时 `readFixed` 因长度不符返回 `ErrCorrupt`，`store.go` 再并上 `ErrRecovery`，之后每次打开都拒绝。SIGKILL 矩阵原来没有落在这两步之间的阶段。这是本分支之前就有的缺陷，不是 E1/E2 引入的。

**修复（`3fc03fb7`）。**
- 打开时，仅当 `root.pending` 是长度为 0、权限私有的普通文件，且两个 physical 回执（`terminal.physical.0/1`）都不存在时，才删除它并同步目录。physical 槽位改名成 `root.pending` 后同样为空，它的去留由回执决定，所以有回执时仍拒绝。非空的残缺文件仍拒绝。只读打开不变。
- SIGKILL 矩阵新增 `root-created` 阶段。修复前它复现了与门禁 17 完全相同的报错。

**验证。**
- `root-created` 修复前失败、修复后通过。
- 新增 `TestStoreOpenReclaimsOnlyEmptyOrdinaryPendingRoot`：短文件拒绝、有 physical 回执拒绝、空文件回收且选中的 root 不变。反向验证：去掉回执检查后，该测试失败。
- mac 上 `authenticatedstate`、`metadatastate` 全包通过；`internal/app` 中 HA SIGKILL 相关测试跑 3 次通过。
- Linux（ext4，非 root）跑 `authenticatedstate` 全包：140 通过，12 跳过，0 失败。

**下一步。** 在 `3fc03fb7` 之上重跑完整门禁（门禁 18）。

## 44. 门禁 18 失败：HA 启动失败路径的数据竞争（I-03 收尾）

2026-10-13 13:53 记录。

**现象。** 门禁 18（`fe318b63`，`exit=2`，10848 s）中 `make test` 全部通过，在 race 阶段 `internal/app` 有一个失败：`TestDefaultHASelectedRuntimeUsesActualSevenRootsAndTLSReady`，报 `race detected during execution of test`。

**根因。** `finishHASelectedPrimaryStartupOwned` 在 defer 中不持 `f.mu` 就把失败并入 `f.applyErr`。同一时刻 `/ready` 走 `businessHealthError`，在 `f.mu` 下读 `applyErr`。另一个写入点（`ha_selected_transport.go` 的 apply 失败）本来就持锁。这段代码来自 `0ee6fd87`，是本分支之前就有的缺陷。

**修复（`d61124ab`）。** 新增 `retainApplyErr`，在 `f.mu` 下写入；启动失败路径改用它。启动 worker 在独立 goroutine 中运行，不持 `f.mu`，不会死锁。

**验证。**
- 新增 `TestHASelectedRetainedApplyErrorIsVisibleToConcurrentReadiness`（`-race`）通过。反向验证：去掉 `retainApplyErr` 里的锁（确认文件里已没有 `f.mu.Lock`），race 检测器报 `DATA RACE`。
- 门禁失败的测试单独加 `-race` 跑一次通过。

**未处理的同类写入。** 业务回调结束时（`ha_memory_state_foundation.go` 约 734 行）在 `f.lifecycle` 下而不是 `f.mu` 下写 `closeErr`，`/ready` 在 `f.mu` 下读它。只在 credit 释放失败时发生，门禁没有触发过。留待用户决定是否一并处理。

**下一步。** 在本修复之上重跑完整门禁（门禁 19）。

## 45. I-03 关闭；剩余范围审查（2026-10-13）

门禁 19（`17c3133d`，`exit=0`，8244 s）通过，按用户确认快进推送 `memory-bounded-state`。I-03 关闭，计数变为 **6 完成 / 19 开放**。§44 的同类写入（`closeErr` 在 `f.lifecycle` 下写）按用户裁定放入本次审查。

按 §40.16 的安排完成剩余范围审查，见[剩余范围审查](memory-bounded-scope-review.zh-CN.md)。它给出状态模型的简化建议、逐项的发布必需性和可判定完成条件，以及 5 个待用户裁定的问题。裁定前不开始 I-06。

### 45.1 用户裁定（2026-10-13）

对[剩余范围审查](memory-bounded-scope-review.zh-CN.md) §5 的 5 个问题：

1. 第一个发布只发 **Standalone selected**。HA selected 连同 I-06、I-05/I-11 的 HA 部分、HA 维护循环、审查 §4 的第 1、6、7 项后置。
2. **selected 成为唯一引擎**，删除原引擎的运行路径和“零表示维持原路径”的配置语义。
3. 现有数据**重新初始化**，不迁移。O-01 改为删除迁移命令，写明重新初始化的步骤（含离线保留旧数据目录和备份）。十万请求、1800 秒的冷迁移目标（M3）取消，冷构建只服务投影损坏和备份恢复，目标按登记的历史规模重定。O-02 去掉导回原引擎和旧二进制兼容。
4. **物理终结储备**移出发布范围，保留为实验开关；运维文档写明不开时真实 ENOSPC 下已接受请求的终结没有保证。审查 §4 的第 4、5 项随之后置。
5. 容量目标**维持 2 GiB**，运行前登记固定输入和门槛。

下一步：按审查 §3，把发布范围内每一项的完成条件写进看板，再开始实施。

## 46. R-1 登记：`halro init` 直接产生 selected 实例

2026-10-13 记录，在实现之前提交。依据[第一个发布的完成条件](memory-bounded-release-conditions.zh-CN.md) R-1，以及用户的修订裁定：复用已验证的激活路径。

**核对过的前提。**
- selected 实例目前只能由 `metadata migrate --activate-runtime` 从一份加密备份激活产生（`cmd/halro/metadata_migrate.go`）。`backup restore` 只还原交换格式（bbolt Metadata + 原生日志），密钥轮换也用 `metadatastate.BuildFromBackup` 从备份重建。所以建库代码和备份写入代码本来就要保留。
- 激活的输入：主密钥、由它派生的 Ledger/Metadata journal 密钥、身份（32 字节）、实例 ID、密钥代数、一份归档。实例 ID 原引擎在第一次启动时才写入（`runtime.go` 的 `SeedInstanceID`），所以 init 必须在打归档之前写入。
- 中断后重来（`PrepareMemoryRuntimeMigrationDirectory` 的 Restart）要求身份、实例 ID、密钥代数和归档密文摘要都与中断前相同，所以归档必须跨崩溃保留，身份必须可以重新算出。
- `CreateBackup` 拒绝写进数据目录，并自己取数据目录锁。

**设计。** `halro init` 在 Standalone 配置下按以下阶段运行，每个阶段可以从磁盘状态判断出来：
1. 空目录 → 现有 `Initialize` 写出交换格式，再写入实例 ID。
2. 在数据目录旁的固定路径（`<data_dir 的父目录>/.<data_dir 名>.init.hmbk`）创建内部归档，先写 `.pending` 再改名。归档密钥和身份都由主密钥经 HKDF 派生，不在磁盘上留新的密钥材料。
3. 取数据目录锁，等待压力监视器 Healthy，然后依次运行 `PrepareMemoryRuntimeMigrationDirectory`（已有 `memory-state` 命名空间时带 Restart）、`PrepareFormat2FromBackup`、`ActivatePreparedMemoryRuntime`。
4. 删除内部归档。

重跑时：已有 `memory-state.bootstrap.pending` 走 `ResumeMemoryRuntimeActivation`；已有 `memory-state.bootstrap` 只删除残留的内部归档，然后报告已初始化；阶段 1 中途留下的不完整状态，沿用现有规则拒绝，不自动修复。

**判据（事后不放宽）。**
1. 空目录上 `init` 后，Runtime 打开的是 selected 状态（`selectedState != nil`）。能提交一个请求并完成终结，关闭后重开能读到它。
2. 在每个阶段边界被 SIGKILL 后重跑 `init`，最终满足判据 1，且数据目录旁不留下内部归档。阶段边界包括：交换格式写完、归档 `.pending`、归档改名后、命名空间准备的各个钩子、激活的 6 个检查点、删除归档之前。
3. 数据目录非空（有不认识的文件）时拒绝；已初始化的实例重跑 `init --if-needed` 时不做改动。
4. 反向验证：去掉“已有 `bootstrap.pending` 走 resume”的分支后，判据 2 中 `bootstrap-pending-synced` 之后的阶段会失败。

**不在本项。** `halro start` 首次运行时的自动初始化（默认配置没有资源预算，selected 需要它），放到 R-2/R-4。HA 仍走原路径，到 R-3 再拒绝。

### 46.1 R-1 结果（2026-10-13 17:40）

实现见 `030f5b95`：`internal/app/init_selected.go`，`halro init` 在配置了资源预算时走它。

**对登记的两处更正（属于前提，不是放宽判据）。**
- 恢复分支的条件应为“已有 `active.seal`”，不是“已有 `bootstrap.pending`”。共同 seal 发布之前中断时，Restart 准备会把 `bootstrap.pending` 连同其他中断产物一起移入保留槽，现有迁移测试（`memory_state_activation_enospc_linux_test.go`）也是这样判断的。按登记的写法，`bootstrap-pending-synced` 处被杀后重跑会失败。
- 实例 ID 只能在内部归档创建之前写入。之后的任何 Metadata 写入都会移动归档所绑定的原生位置，导致重启检查失败。归档之后改为只读读取，为此给 `boltstore.Store` 加了只读的 `InstanceID()`。

**判据结果。**
1. 通过：`TestSelectedInitializationProducesAReopenableSelectedInstance`。init 后 `selectedState != nil`；请求终结后重开，Usage 读到费用 7；重跑 init 不做改动；数据目录旁不留归档。
2. 通过：`TestSelectedInitializationSIGKILLAtEveryBoundaryResumes` 覆盖 17 个阶段边界（`HALRO_STRESS=1` 全跑，普通运行跑其中 6 个代表）。每个边界被真实 SIGKILL 后重跑 init 都能完成，selected 实例能连续两次打开，不留归档。
3. 通过：`TestSelectedInitializationRefusesAForeignDataDirectory`。数据目录里有不认识的文件时拒绝，不写数据目录，不建归档和主密钥。
4. 通过（按上面更正后的前提）：把 `active.seal` 恢复分支改成永不进入（已确认改动生效），`common-seal-published`、`bootstrap-activated` 两个阶段失败；在 seal 之前的阶段走 Restart，不受影响。恢复后重新通过。

**真实二进制。** Linux arm64 构建，在 Docker 里新建的 3 GiB ext4 上（2 GiB cgroup）：`halro init` 成功，产出 `memory-state.bootstrap` 和 `active.seal`，不留内部归档；`init --if-needed` 报告已初始化；`halro serve` 先后两次在约 1.2 秒内 ready，日志里没有 ERROR。

**环境事实。** selected 引擎只能在 Linux 上、并且数据目录和 TMPDIR 位于能报告 inode 数的文件系统上运行。macOS 没有 cgroup，Docker Desktop 的 overlay 和绑定挂载不报告 inode 数，这两种情况下压力采样都按设计失败封闭（`DiskUnavailable` / `CgroupUnavailable`）。发布文档需要写明。

**其他检查。** `cmd/halro`、`internal/store/bolt` 全包通过；`internal/app` 中 Initialize/SelectedInit/Bootstrap 相关测试通过；init 测试的 race 通过；`go vet` 通过。

**留给 R-2/R-4。** `halro start` 首次运行时的自动初始化仍走原引擎，因为默认配置没有资源预算。

## 47. R-0 登记：macOS 的主机压力源（用户裁定 2026-10-13）

2026-10-13 记录，在实现之前提交。

**为什么。** R-2 删除原引擎后，Halro 只剩 selected 引擎，而它的压力采样在没有 cgroup 时按设计拒绝（`resourcebudget/sampler_darwin.go` 的 `verifyCgroupFS` 恒返回 `ErrInvalid`）。Halro 发布 darwin 二进制和 Homebrew macOS，删除后将完全不能在 macOS 上运行。用户裁定：给 macOS 加主机压力源，放在 R-2 之前。

**核对过的前提。**
- 本机真实采样只有 `CgroupUnavailable` 一个问题；磁盘和 inode 都有效（可用约 160 GB、inode 约 15.6 亿）。
- macOS 把空闲内存几乎都用作缓存（本机 `vm.page_free_count` 约 4800 页 × 16 KiB ≈ 78 MB / 24 GiB），所以“空闲页”不能当可用内存。内核自己的内存压力指标是 `kern.memorystatus_level`（可用内存百分比，本机 40）。
- 压力判定（`pressure.go`）把 `Groups[0].Current` 与 `CgroupEnter/Exit` 比较，并对每个有上限的组检查 `Max - Current` 与 `HeadroomEnter/Exit`。`CgroupID` 变化记为 `SourceChanged`。

**设计。** 仅 darwin：
- 用一个“主机组”填 `Groups[0]`：`Current` = 本进程的 Go 运行时内存（与 `RuntimeUsed` 同源，纯 Go 二进制的主体），`Max` = `Current` + 主机可用内存，`MaxLimited` = true；`GroupCount` = 1。主机可用内存 = `kern.memorystatus_level` / 100 × `hw.memsize`。含义：进程所在的“组”上限就是主机还能给它的内存，余量检查即主机余量检查。
- `CgroupID` = SHA-256(固定前缀, `hw.memsize`, `kern.boottime`)，同一次开机内不变，重启后变化。
- 读不到或数值无效（level 不在 0～100、memsize 为 0、溢出）时仍报 `CgroupUnavailable`，不替换成无限值。
- Linux 和其他平台不变。

**取舍（写进运维文档）。** macOS 上没有容器上限，只有主机级保护：进程自身内存对 `cgroup_enter_bytes` 判定，主机余量对 `headroom_*` 判定。

**判据（事后不放宽）。**
1. 换算函数的确定性测试：给定 level 与 memsize 得到正确的可用字节；level > 100、memsize = 0、溢出时报错。
2. 采样器测试（用桩替换主机读取，任何平台都能跑）：主机组填入 `Groups[0]`、`GroupCount` = 1、`CgroupID` 固定；主机读取失败时 `Issues` 含 `CgroupUnavailable`。
3. 真实 macOS：本机真实采样不再含 `CgroupUnavailable`；真实二进制 `halro init`（selected）成功，`halro serve` 先后两次 ready。
4. 反向验证：去掉 darwin 的主机组接线，判据 3 的真实采样重新出现 `CgroupUnavailable`。
5. Linux 不变：`resourcebudget` 的 Linux 采样测试在门禁的 Linux 容器中通过。

**后续顺序（同时更新）。** R-0 → R-4（默认配置可推导出完整预算，让 `halro start` 在默认配置下初始化 selected）→ R-2（删除原引擎；先在一个测试文件上试点测试迁移，量出耗时再估全部）→ R-3、R-5。

### 47.1 R-0 结果（2026-10-13 17:53）

实现见 `e4bc072e`。判据全部通过：
1. `TestDarwinHostAvailableIsTheKernelShareOfMemory`：换算正确（24 GiB × 40% = 10307921510 字节，按 floor(memsize × level / 100) 精确计算），level > 100、memsize = 0、超过 int64 时报 `ErrInvalid`；开机时间不同则身份不同。
2. `TestSamplerHostGroupStandsInForAMissingCgroup`（任何平台都跑）：主机组填入 `Groups[0]` = {Current 50, Max 450, MaxLimited}，`GroupCount` = 1，身份固定；读取失败、负值、溢出、缺身份、进程内存不可用时都报 `CgroupUnavailable`，组字段清零。原有 cgroup 测试的夹具显式置 `host = nil`，在所有平台上继续覆盖 cgroup 路径。
3. 真实 macOS：`TestDarwinRealSamplerReportsAHostGroup` 通过；真实二进制 `halro init` 产出 selected 实例，`init --if-needed` 不改动，`halro serve` 两次 ready，日志无 ERROR。
4. 反向验证：darwin 的 `platformHostMemory` 改为返回 nil（已确认改动生效）后，真实采样 `Issues` = 2（`CgroupUnavailable`），判据 3 的测试失败；恢复后通过。
5. Linux：`resourcebudget` 的采样与压力测试在 Linux 容器（ext4）中通过；Linux 的 `platformHostMemory` 返回 nil，路径不变。`wait_points_test` 在容器中因没有源码目录而失败，与本项无关，门禁会在源码树里跑它。

另外：`internal/config` 全包、`internal/app` 中配置文档与压力相关测试、前端 vitest（690 个）通过。配置说明（`default.yaml`、`config.example.yaml`）已写明 macOS 上阈值的含义。

**顺带观察。** selected 激活会在数据目录的父目录留下一个 `.halro-publication-<id>.lock`（macOS、Linux 两次真实运行都有）。是不是有意为之，还没有核对，记为待查。

## 48. R-4 起草时测得的成本（2026-10-13，R-4 暂停）

为 R-4 选默认值时，先核对 2 GiB 下能放多大的投影、每个请求占多少空间。用的是一次性探针（未提交），配置为 `selectedRuntimeBudgetFixture`（投影上限各 64 MiB）。请求只走账务路径（Begin→Reserve→Settle→Finalize），没有上游，单线程顺序执行。

**1. 2 GiB 下投影上限约 150 MiB（更正见 §49：把 IO 类别给到 960 MiB 时可到 320 MiB）。** `UsageMaintenanceIOMinimum` 把投影映射按 RAM 计费，约为两个投影上限之和再加 64 MiB（64 MiB → 映射 192 MiB；256 MiB → 576 MiB；1 GiB → 2112 MiB），另加账务恢复 128 MiB 等常驻项。在 2 GiB 总额、1 GiB Recovery 储备下，Standalone 的 Usage 维护周期只能容纳每个投影约 150 MiB。现有 2 GiB 容量剖面 `selectedCapacityBudget`（投影 8 GiB）需要 IO RAM 约 16.3 GiB，过不了 `ValidateStandaloneUsageMaintenance`（与看板 §36 的记录一致）。

**2. 投影写放大约 45 倍。** Linux ext4（Docker、2 GiB cgroup）上，每个请求原生 WAL 约 2.1 KB，Ledger 投影根增长约 93 KB；macOS 上约 90 KB。Usage 投影根先长到约 48 MB 后持平。按约 90 KB/请求计，64 MiB 的 Ledger 投影约 520 个请求就满（macOS 实测第 518 个请求报 `authenticated store pack needs reclamation before the next update`，这是 §26 设计的背压：拒绝新写入、等 checkpoint 回收），150 MiB 约 1600 个请求就要回收一次。

**3. 单请求顺序耗时：Linux ext4 约 54 ms，macOS 约 140 ms。** 即单线程约 19 请求/秒（Linux）。并发下能否合批提高吞吐，未测。

**4. 在线 checkpoint 期间业务返回 503。** `withMemoryCheckpointRequest` 在 checkpoint 暂停期间对新业务请求返回 `503 Retry-After: 1`（探针中约每 10 秒一次）。暂停时长未测。

**5. 顺带澄清。** 直接调用 `r.accounting` 的测试会在 checkpoint 暂停时得到 `accounting manager is retired`：`enterOperation` 在 `paused != nil` 时也返回 `ErrAccountingRetired`。生产请求经过上面的请求门，不受影响；但这个错误名会误导排查，记为待改。

**结论。** 这些数字直接决定 2 GiB、只用 selected 引擎的第一个发布能否成立：投影上限约 150 MiB、每约 1600 个请求回收一次、每次回收期间 503、单线程约 19 请求/秒。R-2 删除原引擎不可逆，建议在 R-2 之前先跑一次登记过的容量探针，回答并发吞吐、p99、checkpoint 暂停时长和频率，再决定是否继续这条路线。R-4 暂停，等用户裁定。

## 49. 容量探针登记（用户裁定 2026-10-13，在运行之前提交）

**目的。** R-2 删除原引擎之前，回答 selected 引擎在 2 GiB 下能否承受业务负载。

**对 §48 的更正。** 把 IO 类别 RAM 设为 960 MiB（normal 1 GiB 中的大部分），`Validate` 和 `ValidateStandaloneUsageMaintenance` 在投影上限 96～320 MiB 时都通过（已核对）。§48 说的约 150 MiB 是按 IO 512 MiB 算的。

**固定输入。**
- 测试：`TestSelectedCapacityProbe`（`internal/app/selected_capacity_probe_test.go`，需 `HALRO_SELECTED_CAPACITY_PROBE=1`）。
- 预算 `selectedCapacityProbeBudget`：总 RAM 2 GiB、Recovery 与 Recovery 储备各 1 GiB、IO 960 MiB、Ledger/Usage 投影上限各 256 MiB；运行前在测试里再次校验通过。压力 `selectedCapacityProbePressure`：runtime 1 GiB/900 MiB，cgroup 1.5 GiB/1.25 GiB，余量 64/128 MiB，磁盘 128/256 MiB，采样 100 ms；用真实采样器。
- 环境：ubuntu:24.04 arm64 容器，`--memory 2g --cpus 2`，数据目录和 TMPDIR 在新建的 8 GiB ext4 上。
- 负载：同一实例上依次跑并发 1（2000 个请求）、16（2 万）、64（2 万）。每个请求经过业务请求门（checkpoint 暂停时等待重试），再走 Begin→Reserve→Settle→Finalize，没有上游。设计内的反压（`authenticatedstate.ErrCapacity`、`resourcebudget.ErrPressure`）计为拒绝，请求按错误终结；其他任何错误计为错误。
- 延迟从进入请求门算起，含 checkpoint 等待。checkpoint 暂停由 1 ms 轮询 `liveCheckpoint.paused` 测得。

**门槛（用户选定的工程建议值，事后不放宽）。**
1. 各并发级别错误数为 0。
2. 各并发级别拒绝不超过 1%。
3. 单次 checkpoint 暂停不超过 2 秒。
4. 并发 64：吞吐不低于 100 请求/秒，p99 不超过 1 秒。
5. cgroup 峰值（`memory.peak`）不超过 1.5 GiB。

任一项不达标即判定路线未通过，记录归因后转入用户裁定，不重跑同一配置。只跑一次。

### 49.1 容量探针结果：未通过（2026-10-13 20:10）

在 `9b5cd925` 上按登记跑了一次。测试在 2 小时上限处超时（`exit=2`），日志与 goroutine 转储放在被忽略的 `docs/verification/performance/2026-10-13-selected-capacity-probe/`。

**结果。**
- 并发 1（2000 个请求）：全部完成，0 错误、0 拒绝；吞吐 5.05 请求/秒；p50 198 ms，p99 334 ms，最长 1.19 秒；checkpoint 暂停 15 次，最长 0.98 秒；Ledger 投影根 81 MB，原生 WAL 4.4 MB。
- 并发 16（2 万个请求）：约 1 小时 50 分钟内没有跑完，吞吐低于 3.1 请求/秒，比单线程还慢。
- 并发 64：没跑到。

**判定。** 门槛 4（并发 64 吞吐 ≥ 100 请求/秒）不达标，路线未通过。按登记不重跑。门槛 5（cgroup 峰值）没有测到；容器内存观察值一直在 45 MiB 以内。

**归因（代码 + 转储）。** 超时时 16 个工作 goroutine 里，14 个在 `resourcebudget.AcquireWorkLane` 等待，1 个在 Ledger 内执行。`budget/authenticated_owner.go` 的 `withOwnedAppendWorkspace` 让所有账务写入经过同一个容量为 1 的写入道（`appendGate.lane`），最多 16 个等待者，第 17 个直接得到 `ErrExhausted`，所以并发 64 还会产生错误。每个请求有 4 次写入（Begin、Reserve、Settle、Finalize），每次都在这条道里完成原生追加、认证 COW 应用和落盘，单次约 50 ms，没有跨请求合批。所以吞吐上限约为 1 / (4 × 单次写入时长)，并发只能排队，不能提速；并发 16 更慢，推测是 checkpoint 更频繁和 CPU 争用（2 核），未单独核实。

**影响。** “只保留 selected 引擎、2 GiB”的第一个发布在当前写入路径下不能承受业务负载。R-2（删除原引擎）不能进行。需要用户裁定路线。

## 50. 对照与拆解登记（用户裁定 2026-10-13，只测不改）

**目的。** 在决定“做合批提交”还是“换路线”之前，补两项数据。本节只记录数字，不设通过线，不改产品代码。

1. **原引擎对照。** 同一探针（`TestSelectedCapacityProbe`，`HALRO_SELECTED_CAPACITY_PROBE_ENGINE=legacy`）、同一容器环境（2 GiB、2 CPU、8 GiB ext4）、同一负载（并发 1/16/64，请求数 2000/2万/2万），跑原引擎一次。测试时限 100 分钟。
2. **selected 单次写入拆解。** selected 引擎、并发 1、300 个请求（`HALRO_SELECTED_CAPACITY_PROBE_PLAN=1:300`）：
   - 一次在 `strace -f -c` 下，统计每个请求的 fsync/fdatasync/写入次数和耗时；
   - 一次带 `-test.cpuprofile`，看 CPU 时间在认证计算（HMAC/SHA）、COW 页编码和其他部分的分布。

探针为此加了两个开关：引擎选择、请求计划。§49 的判定只针对默认计划下的 selected 引擎，开关打开时不做判定。

### 50.1 对照与拆解结果（2026-10-13 22:55）

环境同 §49（Docker、2 GiB、2 CPU、loop ext4）。原始日志、strace 汇总和 CPU profile 在被忽略的 `docs/verification/performance/2026-10-13-selected-capacity-probe/`。原引擎第一次运行中途 Mac 休眠、Docker 虚拟机被挂起（容器内进程只运行了约 4 分钟），作废，用 `caffeinate` 重跑。

**1. 原引擎对照（同一负载，0 错误，cgroup 峰值 424 MiB）。**

| 并发 | 原引擎 吞吐 / p50 / p99 | selected（§49） |
| --- | --- | --- |
| 1 | 33.4/s / 29 ms / 50 ms | 5.1/s / 198 ms / 334 ms |
| 16 | 51.9/s / 293 ms / 565 ms | < 3.1/s（未完成） |
| 64 | 46.9/s / 1174 ms / 4656 ms | 未跑到 |

原引擎在这个环境里同样达不到 §49 的门槛 4（≥ 100/s、p99 ≤ 1 s），所以门槛 4 的绝对值部分受环境（虚拟机里的 loop 设备、fsync 慢）限制。§49 的判定不改；有意义的是相对差距：并发 1 时 selected 慢约 6.6 倍，并发 16 时慢约 17 倍以上。

**2. selected 单次写入拆解（并发 1、300 个请求）。**
- strace（会把速度拖慢约 5 倍，看次数不看时间）：平均每个请求约 21000 次 `pread64`、2000 次 `pwrite64`、33 次 `fsync`、270 次打开文件、8 次改名（含初始化，量级可信）。
- 不带 strace：11.4 请求/秒，p50 87 ms。§49 并发 1 跑 2000 个请求时是 5.1/s、p50 198 ms，**单请求成本随历史增长**。
- CPU profile：采样只占墙钟的 36%，其余是 IO 与落盘等待。CPU 中 81% 在 Usage Collector 的追赶（`usage.(*Collector).catchUpLocked`），其中 61% 在 `ledger.(*authenticatedUsageSource).VerifyAnchors` → `replayAuthenticatedLocked` → `scanFromFrames`；按函数看 JSON 解码约 26%，SHA/HMAC 约 10%，GC/分配约 12%。

**3. 归因。**
- **重放验证随历史线性增长（缺陷）。** `VerifyAnchors`（`internal/ledger/authenticated_usage_source.go:236`）为确认两个锚点在认证链上，每次都从启动点（`startupSaved`，完整历史时从零）重放验证原生日志到当前头部。Collector 几乎每次结算都追赶一次，所以每个请求的验证量随启动以来的记录数线性增长，总成本是平方级。长期运行的实例会越来越慢。
- **单一写入道（设计）。** §49.1 已述：所有账务写入串行，并发不能提速。
- **每请求约 33 次 fsync、约 2 万次页读取。** 认证 COW 存储每次写入都要读取、验证并重写整条页路径，再分别落盘多个文件。

**建议（待用户裁定）。** 先单独修第一项：把锚点验证改为从上次已验证的位置向前增量验证，保持“锚点必须在认证链上”的保证不变。改动集中，属于正确性敏感的认证路径，需要先登记设计。修完后重测并发 1/16，看与原引擎的差距还剩多少，再决定是否值得做合批提交，或者换路线。

## 51. 登记：锚点验证改为增量（用户裁定 2026-10-13，在实现之前提交）

**问题（§50.1）。** `authenticatedUsageSource.VerifyAnchors` 每次都从可信起点（`startupSaved`，完整历史时从零）重放验证原生日志到当前头部，以证明调用方给出的两个锚点在认证链上。Usage Collector 几乎每次结算都调用它，所以单次成本随启动以来的记录数线性增长。

**核对过的前提。**
- 锚点来自消费方，注释明确“消费方提供的偏移和哈希从不作为证明”。所以增量的起点只能是本进程自己用认证重放验证到的位置，不能是消费方提供的值。
- `replayAuthenticatedLocked(from)` 支持从日志中间的锚点开始：校验器以 `from.Hash` 和序号为起点继续验证链，后续帧必须与之相连。
- 回调执行期间原生写入方被冻结（`WithFrozenUsageSource`），头部稳定。
- 压缩只作用于已封存的旧代，活动代不会被重写。

**设计。**
- `Log` 记录一个“已验证位置”：可信起点（`startupSaved` 或零）、是否完整历史模式、已验证到的锚点、所在代。只有 `VerifyAnchors` 在一次成功的认证重放后，才把它更新为本次重放到达的头部。
- 下一次 `VerifyAnchors`：可信起点和模式相同、记录的位置在当前活动代内，并且两个锚点都不早于它时，从这个位置开始重放；这个位置本身视为已见到。其他情况一律走原来的完整重放。
- 失效：任何重放错误、原生状态变为需要恢复、`Log` 关闭（缓存只在内存中、随对象消失）、代号变化（Roll 之后第一次回到完整重放）。

**保证。** 两个锚点仍必须在认证链上：要么由本次重放实际看到，要么等于本进程先前重放验证过的位置。消费方提供的值仍不作为证明。

**取舍（需用户知悉）。** 现在每次调用都会重新读一遍已验证的前缀，所以运行期间有人改动前缀字节，下一次追赶就会发现。改为增量后，同一进程内已验证过的前缀不再重复读取；这种篡改改由下一次启动的完整审计发现。数据目录由本进程独占加锁，这属于运行期间对已验证磁盘字节的篡改。

**判据（事后不放宽）。**
1. 确定性测试：连续追加并多次调用 `VerifyAnchors` 时，每次实际重放的记录数只等于上次调用以来新增的记录数（不随总历史增长）。
2. 安全测试：锚点不早于已验证位置但不在链上（哈希伪造）时拒绝；锚点早于已验证位置且不在链上时拒绝；Roll 之后回到完整重放并正常工作；重放失败后缓存失效，下一次走完整重放。
3. 反向验证：去掉对缓存的使用后，判据 1 失败。
4. 性能复测（同 §50 环境）：并发 1、2000 个请求，最后 200 个的 p50 不超过前 200 个的 1.5 倍；并记录并发 16 的吞吐（只记录，不设线）。
5. 回归：`internal/ledger`、`internal/usage` 全包通过，`internal/app` 中 selected 相关测试通过，受影响包跑 race。

### 51.1 结果（2026-10-13 23:38）

修复见 `915b417b`；探针加了按完成顺序的前后十分之一中位数（`20c66a4b`）。

1. 通过。`TestSelectedAnchorVerificationReplayDoesNotGrowWithHistory`：每 40 个请求重放 160 条记录（每请求 4 条），6 批完全相同。
2. 通过。`TestVerifyAnchorsStillRejectsForgedAnchorsAfterACachedVerification`：缓存位置上的伪造哈希被拒绝，失败后缓存清空；早于缓存位置的伪造锚点被拒绝；之后合法锚点从起点重放。`TestVerifiedAnchorCacheResumesOnlyFromItsOwnRootAndGeneration`：换起点、Roll 后（代号变化）、完整历史模式、早于缓存位置且未证明的锚点，都不使用缓存。
3. 通过。`resume` 改为总是返回起点后（已确认改动生效），判据 1 失败：每批重放 25760 → 281722 条；恢复后通过。
4. 通过。同 §50 环境，并发 1、2000 个请求：16.1 请求/秒（修复前 5.05），p50 60 ms，前十分之一中位数 59.6 ms、后十分之一 59.2 ms；checkpoint 最长暂停 0.47 秒（修复前 0.98 秒）。与原引擎（33.4/s）的差距从约 6.6 倍缩小到约 2 倍。
5. 通过。`internal/ledger`、`internal/usage` 全包，`internal/app` 中 selected 相关测试（1062 秒）通过；锚点相关测试 race 通过。

**设计调整（实现中发现，未放宽判据）。**
- 完整历史模式不使用缓存。`TestSelectedCompleteUsageSourceReauthenticatesOldPrefixAcrossCompressedRoll` 要求完整历史来源每次重新认证整个已保存前缀，用来发现审计之后的旧前缀损坏。这是有意的保证，保持不变。登记里写的取舍因此只适用于常规来源（从启动点验证）。
- 热路径的锚点对是（Usage 上次处理的位置，Ledger 投影的引用位置），新的 `from` 等于上一次调用证明过的 `to`，可能早于缓存的头部。所以缓存同时记住上一次证明过的两个锚点，等于它们的锚点视为已证明；其他锚点仍必须不早于缓存位置，并由本次重放实际看到。

**新发现：投影写满后整体不可用（未修，待裁定）。** 同一次复测中，并发 16 跑到第 1329 个请求时（约 22 请求/秒，p50 666 ms），Ledger 与 Usage 投影根都长到约 251 MB，即 256 MiB 上限。之后剩下的 18671 个请求全部以 `ledger projection is not available` 失败，约 50 秒内没有恢复。这不是 §26 设计的反压（`ErrCapacity`，整笔拒绝、等 checkpoint 回收后恢复），而是投影变为不可用。cgroup 峰值 1.09 GiB。另外，投影从 83 MB 长到 251 MB 只用了约 3300 个请求（每请求约 50 KB），期间 11 次 checkpoint 没有把空间收回来。原因未查。

## 52. 投影写满导致停服且无法重启（2026-10-13 23:45，排查结果）

本地确定性复现：`selectedInitFixture`（投影上限 64 MiB），并发 16，经业务请求门顺序提交账务请求（一次性探针，未提交）。三次运行结果一致：约第 510～810 个请求后，其余请求全部失败。

**缺陷 A：checkpoint 在 Usage 投影满时无法回收空间。** Usage 投影先满。checkpoint 需要先让 Usage 追上原生日志，追赶本身又要往这个已满的 pack 写入，于是每次都报 `typed Usage catch-up native anchor verification: authenticated store pack needs reclamation before the next update`，日志中 `memory state common checkpoint deferred` 反复出现。回收空间的操作依赖空间，形成循环。

**缺陷 B：已持久化的原生记录在投影上应用失败，投影随即不可用。** 随后 Ledger 投影也满了。某次 Settle/Finalize 的原生记录已经写入 WAL，同步应用到 Ledger 投影时报 `ErrCapacity`。这一步已不能拒绝，原生状态进入需要恢复，之后所有请求报 `ledger projection is not available` 或 `selected Ledger startup requires historical audit before writes or readiness`。§26 的准入只在请求进入时检查一次空间，并发 16 个请求同时通过后各自继续消耗，没有为已接纳请求的后续写入预留 pack 空间。

**缺陷 C：无法重启，调大上限也不行。** 关闭时报 `catch up drained Usage: ledger projection is not available`。重新打开时，把 seal 之后的增量重放进 Ledger 根失败（`open selected Ledger root: native record 1385: authenticated store pack needs reclamation`）。把两个投影上限都翻倍后再打开，Ledger 根通过，但 Usage 根同样失败（`open selected Usage root: ...`）。运维无法通过配置恢复，实例需要人工处理。

**与 §51.1 的关系。** Docker 复测中并发 16 的停服就是这一现象（256 MiB 上限、约 3300 个请求）。每个请求让投影增长约 50 KB，checkpoint 回收不了，所以任何上限都只是推迟发生的时间。

**评估。** A、B、C 都是协议层面的问题，修复要动 checkpoint 与回收的顺序、准入预留和启动重放路径，每一项都要单独登记设计，工作量以天计。

## 53. 用户裁定：换路线（2026-10-13 23:50）

依据 §49～§52，用户选择换路线：原引擎继续作为默认，取消 R-2（删除原引擎）；selected 不作为发布内容；内存随历史增长的原始问题改在原引擎上找更小的修法。方案草案见[换路线方案](memory-bounded-route-change.zh-CN.md)，其中第 4 节（分支是否合入 `main`）待用户裁定。§45.1 的第 2、3 条（单一引擎、重新初始化）随之作废；R-0（macOS 主机压力源）与原引擎无关，按分支处理方式决定去留。

## 54. 用户裁定：撤回 §53，继续当前分支，先修 §52（2026-10-14）

用户指出：§52 的停服且无法重启是缺陷，应当修复；如果修复中发现是方案本身的问题，再重新考虑方案，而不是放弃整个分支。§53 的换路线撤回，[换路线方案](memory-bounded-route-change.zh-CN.md)标为已撤回、不执行。§45.1 的裁定恢复有效，R-2 仍暂停，等 §52 修完并重跑容量探针后再定。

下一步：查清 §52 的 A（checkpoint 在 Usage 投影满时无法回收）、B（已持久化记录在投影上应用失败导致不可用）、C（重启时无法重放）三处的根因，以及每请求约 50 KB 增长为何不被回收；分别登记修复设计，再实现。

## 55. 更正 §52：停服与无法重启是测试绕过了保护层（2026-10-14 00:55）

**错误所在。** §49～§52 的探针只模拟了 checkpoint 请求门（`withMemoryCheckpointRequest`），没有经过紧随其后的投影满载准入（`withProjectionPackAdmission`，§26）。这一层在投影剩余空间不够已接纳请求继续写入时，让新请求返回 503，同时立即触发 checkpoint 回收。生产的 HTTP 请求都会经过它。§52 的缺陷 B（已持久化记录应用失败、投影不可用）和缺陷 C（无法重启）只在绕过这一层时出现，是测试方法造成的，撤回。缺陷 A 的循环依赖确实存在，但准入层正是为了不让 pack 写满，所以它不会被触发。

**按生产的两层门重做（64 MiB 投影，并发 16，一次性探针，日志在被忽略的 `gated-full2.log`）。**
- 前 8 分钟：完成 1869 个请求（约 3.9 请求/秒），0 错误。checkpoint 频繁暂停业务，回收确实生效：Ledger pack 在约 11～31 MB 之间反复回落。
- 第 8 分钟以后：完成数停在 1869，再也没有增长。checkpoint 仍在反复运行，但 Usage pack 停在约 39 MB（可用 48 MB），Ledger 约 25 MB，回收不到空间，新请求一直返回 503。原因是 repack 的规则“复制后省不到 1/8 就不替换”：pack 里几乎全是活数据，不是垃圾。
- 整个过程中“database not open”的错误日志出现 89 次（`pending admin audit delivery retry failed`，checkpoint 期间），另记待查。

**真正的结论。**
1. 保护层按设计工作：不会停服报错，也不会损坏，满了就持续 503。
2. 问题在容量：每个请求在 Usage 投影里留下约 21 KB 活数据，在 Ledger 投影里约 13 KB（原生 WAL 只有约 2 KB，原引擎内存约 2.7 KB）。投影上限填满的是历史本身，不是可回收的垃圾。按 2 GiB 预算下投影最多约 256～320 MiB 计，只能容纳约 1 万个请求的历史，之后服务持续 503。这远低于十万请求的目标。
3. 单请求活数据为什么这么大（每个请求写了哪些行和索引、节点填充率），还没查。它决定这条路线能否成立：如果能降一个数量级，容量问题随之解决；如果是认证 COW 结构本身的开销，就需要重新考虑方案。

**下一步（建议）。** 只做测量：拆出每个请求在两个投影里写入的行、索引与字节，以及 B 树节点的填充率。不改代码。

补充（同次运行结束时）：测试在自己的 15 分钟上限处结束，1869 个请求完成，其余 1131 个在 503 中等到上限（`context deadline exceeded`）。关闭成功、无错误，没有损坏。重新打开时上限已过、被立即取消，所以这次没有回答“满了之后能否重启”，留待测量时一并确认。

## 56. 测量：每个请求在两个投影里写了什么（2026-10-14 01:10，只测不改）

一次性探针（未提交，临时导出底层 store 后已删除）：selected 实例、投影上限 256 MiB（避免中途触发反压），先跑 100 个请求预热，再跑 200 个，对比前后全量扫描。

**每个请求的增量。**

| 投影 | 行数 | 键+值 | 写入 pack 的字节 |
| --- | --- | --- | --- |
| Usage | 28.7 | 9.1 KB | 约 622 KB（未回收前） |
| Ledger | 13 | 3.8 KB | 约 97 KB |

Usage 的 28.7 行：attempt 明细 1 行（约 800 B）、请求摘要 1 行（约 400 B）、attempt 二级索引 12 行和摘要二级索引 3 行（值为空，只有约 43 B 的键），其余为少量汇总行。Ledger 的 13 行分布在 7 个键前缀下，单行值约 50～1300 B。

**存储格式的结构（`authenticatedstate/format.go`）。** B 树扇出 `fanout = 4`，每个叶子只存一行；内部节点为每个子节点完整保存首键和尾键。

**结论。**
1. 活数据约等于数据量的 2 倍：每行一个叶子，再分摊扇出 4 的内部节点（约 400 B/行）。Usage 9.1 KB + 28.7 × 约 400 B ≈ 20.5 KB，与 §55 观察到的约 21 KB 一致。15 行空值索引仅结构开销就约 6 KB。
2. 写放大更严重：Usage 每请求写入约 622 KB，是数据量的 69 倍。每插入一行都要沿约 log₄N 层复制整条路径。已核实（`authenticatedstate/update.go` 的 `Transaction.mutate`）：每次 Put/Delete 都立即把新路径写入 pack，同一事务里的下一次修改再从 pack 读回并重新复制，上一份副本当即成为垃圾，事务内没有共享已复制的节点。
3. 这些都属于实现层面的选择（节点格式、叶子装载、事务内复制、索引行数），不是“认证 COW”这个思路固有的上限：较大的扇出、一个叶子装多行、事务内复用已复制节点，都有可能把活数据和写放大降一个数量级。但存储格式与认证校验紧密相关，改动需要单独登记设计并评审。

**仍待确认。** 投影写满（持续 503）之后关闭再重启是否正常（§55 未答）。
