# 内存预算可行性：P1 裁定与共享验证

日期：2026-10-08。最新P1-A2会话15:16开始，16:46为90分钟裁定上限；实际工作约19.2分钟。首次预算会话 11:18:03–13:18:03（新加坡）；共享前置验证会话从13:37:11开始，裁定上限15:37:11。后一会话不把两次交互间隔记为开发时间。范围是预算/生命周期与在线维护前置接线，不执行大规模迁移或开放公共物理入口。

## 1. 裁定与实际交付

**原 2 GiB 配置不成立，改成 1 GiB 也不成立。** 此结论由必要下界即可得出，不必先运行完整容量实验。三后台池共享已实际接线，当前仍有128.386 MiB必要缺口；完整阶段峰值和容量接受仍未通过。Recovery reserve保持原值。

本轮发现并修正必要预检遗漏：Usage/Governance 的两笔常驻费用和五个原始认证 Store 的驻留费用，共增加 **69,757,952 B（66.526 MiB）、7 个 normal claim**。费用从实际构造器使用的常量导出，实际金额与行为不变。旧报告只统计工作池；现在的必要普通 RAM 是 **1,096,865,792 B（1046.053 MiB）、70 个 normal claim**，其中 Completion **842,481,664 B（803.453 MiB）** 已包含，不再额外相加。

原 Total=2048 MiB、Recovery reserve=1387 MiB、normal=661 MiB。因此普通预算必要缺口升为 **403,757,056 B（385.053 MiB）**，仍不包含 collector、所有 pins、物理 cache 工作、更多请求及 OS 开销。旧 318.526 MiB 是旧必要下界，历史结果保留。

交付：实际费用预检修正；申请/持有/释放与阶段审计；2 GiB/1 GiB 数值对照；排除两个单独不足的优化；首选工程候选和进入实现的条件。**完整阶段峰值与安全共享证明尚未完成，P1 不标完整通过，原 19 项不关闭。**

证据集中在[本轮输入与哈希](../verification/performance/2026-10-08-budget-feasibility/inputs.json)、[可复算费用](../verification/performance/2026-10-08-budget-feasibility/bounds.json)、[重算脚本](../verification/performance/2026-10-08-budget-feasibility/recompute.py)、[实际 Manager 对账](../verification/performance/2026-10-08-budget-feasibility/config-vector-residents-fixed.log)。本轮未增加整份源码压缩归档。

本次 P1-A1 已交付真实接线：在线 checkpoint 不再以一笔 Directory RAM loan 包住后续所有等待。Metadata facade 先 join；Primary 只在短 loan 内读取 Ledger 位置，退出后由原 coordinator 验证并捕获目标、等待 peer 应用；collector 先 join，借 loan 完成最后重放后退出 loan，保持 reducer 写排他；随后进入原 reserve/receiver/applier 屏障，再借 Directory loan 完成 maintenance/publication。目标捕获会验证位置与实际 durable prefix，一旦不匹配仍拒绝。

新增 collector 重放借用最多调用一次；过期调用拒绝；实际已进入重放须 join；提供者即使错误地异步返回或吞掉重放错误，也不能使 reducer 提前解除排他或宣告成功。caller cancel 仍传到实际重放，取消不释放仍在执行的 IO/费用。此项是共享前置顺序修复，**P1-A1费用节约0 B**，不是共享池实现或P1完整通过。

## 2. 真实 owner 与生命周期

Class 是 Manager 的类别；正常工作区在借用/激活后仍收费，不能因从 Completion 转到 IO/Background 而再扣一次。常驻 Disk ceiling 不等于宿主物理分配。

| owner / 申请点 | RAM / Disk / claim | 持有区间与重叠 | 释放或 join 点 |
| --- | --- | --- | --- |
| Directory 原池：`ledger.acquirePhysicalCatchupWork` | normal 总 270424064 B，26 claims；completion 270393344 B；该工作槽 Disk=0 | 启用后保留到 Directory 退出；checkpoint 在外层借用它，源读与冻结/发布重叠 | `AuthenticatedDirectory.Close` 先停止借用、关 Store，再在 mutex 外 Close 工作池；失败留债 |
| Usage 原池：`typedPhysicalUsageOwner.acquireWork` | normal 总 168129536 B，9 claims；completion 134565888 B | 物理接收/追赶整个生命周期；source、page reader、COW、pins 各自有槽 | 原投影 Close / source job join；HTTP 返回不释放这笔池 |
| Governance 原池：`haPhysicalGovernanceProjection.acquireWork` | normal 总 134584320 B，10 claims；completion 134569984 B | 原投影/原生日志窗口；`withPhysicalProjectionWork` 屏蔽外层 RAM scope 后借自己的池 | `GovernanceProjection.Close` 原 native/reader/cow join 后 Close pool/Active；未知退出保留 |
| Metadata 一笔原窗口：`Baseline.acquirePhysicalCompletion` | normal 总 201952256 B，7 claims；active 为 Delta64+Tx32 MiB+4096 B；completion 101277696 B | Primary 价格 pin 窗口或 Replica receive escrow；接收 jobs 和终结 callback 活跃时持有 | `TerminalPhysicalWindow.closeLocked` 与原接收 jobs join；恢复债不退费 |
| 一笔 Ledger 请求：Gateway 的 pre-admitted completion profile | normal 总 218462208 B，10 claims；16 MiB Active；completion 201674752 B | 准入到所有终结事实完成；取消后实际 continuation 仍持有 | 原 handler/terminal continuation 完成，不能靠 caller cancel 退费 |
| 独立 Pricing IO | 33554432 B + 1024 B control，1 claim | 与请求并发；与其它 normal jobs 共享总容量 | 原 Pricing IO 生命周期结束 |
| Usage 常驻投影：`openTypedUsageProjection` | Background 33554432 B + control，1 claim | 与 Usage catch-up Active32 是两笔独立 claim；跟随原投影 | `DiskProjection.Close` 原实际任务/Store join 后 `liveRAM.Release` |
| Governance 常驻投影：`openSelectedTypedGovernanceProjection` | IO 33554432 B + control，1 claim | 与 Governance 原池的 source32 是两笔独立 claim | `GovernanceProjection.Close` 原实际来源/Store join 后释放 |
| 五个认证 Store：`authenticatedstate.openStore` | 每个 Background 528384 B + control，5 claims | Ledger/Usage/Governance/Metadata/Directory 同时驻留；不与 query/update workspace 混算 | Store 原 pins/FD 退出后释放 resident；失败留债 |
| 三个 peer session：`PeerQueueBudget.acquire` | 每个 Recovery q+r=167815840 B + control | 包括两个 peer 与替换/控制 session；退休 session 占用 slot 直到 socket、queue、reader/handler、writer 全退出 | `peerQueueLease.release` 最后退 fee 和 slot |
| candidate / reconstruction：`PrimaryWindowLimits.acquireCandidate` | 每笔 Recovery n=100689312 B + control；校验按三笔预留 | 两个 retained peer candidate 不能由 coordinator mutex 推断成一笔；重构与发送队列复制可能重叠 | 原 candidate 丢弃/队列同步复制完毕、回调退出才释放 |
| NativeSource+Metadata baseline reader | Recovery d+baseline=53870592 B + control | d 为三组 native readers，baseline 为三组 17 MiB；是原源 lifetime 聚合 owner | source 的 borrowed reader 与 child join 后 Close；不是每个 session 再重复加整笔 |
| Audit/Governance native retry cache | Recovery 各配置 AuditLogRAMBytes / GovernanceLogRAMBytes；当前诊断各 2 MiB | 源日志 lifetime；不能把它们与 capsule 文件/临时 decoder 合并 | 两个 native log Close、cache/reader 退出 |
| Primary unconfirmed release table | Recovery 固定 4 MiB；Unconfirmed bytes 另有 total/reserve | 表按协议 Max65536 校准，不能因为配置 entries=64 就缩成 64 行成本；复制期间存在 | coordinator Stop/真实 suffix retirement 与 shutdown release |
| Primary 两级恢复父工作区：`recoverHASelectedAccountingMode` | Recovery amount=128 MiB，1 claim | transport 已存在时进行；`WithRecoveryScope` 的 descendants 是另收费，不是从父 128 MiB 中借 | 原双流完整枚举、writer.Close、lease scope 和 descendants join 后父释放 |
| 两级恢复 descendants | view64+source32+Ledger point64+page32+COW64 MiB+2×4096 B；父/子合计校验 384.016 MiB、8 claims | code 显式 `WithoutRAMWorkspace`，不能把 Directory 外池当成这笔恢复 fee | `WithRecoveryScope` join children；父许可不会自动覆盖它们 |
| 原生/投影/Metadata/ordering/cache/seal Disk | `openHASelectedRuntimeFoundation`、`prepareHAPhysicalNativeSources`、Store 原 diskReservations 按配置收 ceiling 与 control | 在 RAM 申请阶段仍有实际留盘 owner；两个 cache slot 与 pending/prepared target 的文件身份要分别核对 | foundation Close 注释明确：文件仍存在，固定 ceiling 留在原 Manager，不能以 Close 当作 Disk 变零 |
| collector queue | Background 4096+8×AnalyticsQueueCapacity B + control | service 生命周期，和正在处理的事件费用另算 | `haSelectedBusinessServices.close` 在 collector.Retire 后退 fee |
| capsule/cache、共同发布临时工作 | 实际 persist/read 的有限 IO profile；部分原 Directory profile 已覆盖；物理 target RAM/额外 pin 仍需逐调用去重 | captured FD、decoded capsule、freeze 和 common publish 可重叠；禁止把全部存储 ceiling 相加到 RAM | captured FD/publisher/target 实际 Close、未知退休保留；相关累计峰值尚未知 |
| promotion carrier / audit | 独立 carrier 4×MaxHAPromotionCarrierBytes RAM 与 Disk；audit.PhysicalBatchRAMBytes IO | 停机命令的 peer promise、ordering/source 与 audit 生命周期；物理 promotion 入口仍拒绝 | 原 carrier.Close / audit scope 结束；原先保留的 coordinator 须独立 release |
| Gateway deferred/capture/admin、TLS、Go runtime/page cache | 原配置单独额度和实测 OS 工作集 | 更多请求/后台/握手与 native IO 是否同时最大尚未核定 | 各 service drain/join 与 foundation 最终 Close；完整数值未知 |

来源均为当前源码；上述工作池费用保留旧真实 profile 数值，常驻追加采用新实际导出。参考源码：[预算](../../internal/config/ha_physical_budget.go)、[Store](../../internal/authenticatedstate/store.go)、[Usage](../../internal/usage/typed_projection.go)、[Governance](../../internal/governance/selected_typed_projection.go)、[Metadata](../../internal/metadatastate/terminal_physical.go)、[session](../../internal/replication/peer_queue_budget.go)、[candidate](../../internal/replication/primary_window.go)、[recovery](../../internal/app/ha_selected_recovery.go)、[foundation](../../internal/app/ha_memory_state_foundation.go)。

## 3. 各阶段的并发关系与缺证据位置

| 阶段 | 已确认的同时持有 / 排他边界 | 数值裁定与仍缺的内容 |
| --- | --- | --- |
| 开库/历史审计 | stop-directory lock、源初始 lease；五根的常驻 fee 随构造积累，Audit/Gov 原 caches 与 native reader 可同存 | 必要 owner 已定位；完整审计/初始 open 的 transient 最大重叠未合计 |
| 物理向量准备 | `prepareSelectedServicesWithLease` 在 DAO/service/transport producers 暴露前准备；四组原池逐个安装，但已安装的不退 fee | 构造串行不能当 RAM 串行；原配置必要下界已拒绝 |
| Primary 两级恢复 | transport 已启动，startupProducers 未开放；原 services pause；父128及 recovery descendants 和 session 可重叠 | selected session profile 819.586 MiB 不能独自当 Recovery reserve；另有 384.016 MiB 恢复 profile 和 native caches/table |
| Replica 接收/重放 | 原 escrow 在 receiver 前安装；普通 applier 用 applied cut；receiveJobs 是真实 join owner | Ledger receive、Metadata 原窗口、native/source/caches 的独立并发峰值尚未知；不能用 Primary profile 完整替代 |
| 正常请求 | 请求和已安装组件原池+五个 Store+两个常驻投影仍在；controller/source 权限和 HTTP completion 分开 | 固定一请求必要 normal=1046.053 MiB；更多请求、管理/后台峰值未知 |
| 共同 checkpoint | `f.mu.TryLock`、服务/Metadata join；Primary 短 loan 读位置后捕获/等待 peer；collector join→短 loan 最后重放→loan退出；admission/receiver/applier屏障后借 maintenance loan | 本次已移除 loan 跨这些 producer 等待的外层包裹；Usage/Gov/Metadata 的嵌套仍须原池、Member/source 等待仍须审计，不能直接取最大值 |
| 角色切换/晋升/轮换 | 现有停机路径有 original source/carrier/promise 生命周期；opening/promotion/rotation physical public guards 仍拒绝 | 完整物理模式无数值接受，保持未知；不以部分停机路径通过代替角色切换峰值 |
| 退出/错误清理 | foundation 撤销 producer，停 completion/coordinator/manager，join callbacks/peer work，关业务/roots/native/caches/publisher；sampler 最后 join | 退出需要原工作池；未知 FD 退休留原 claim；Disk ceiling 不随 Close 变零，完整退出瞬时峰值未知 |

这张表覆盖了要求的阶段，但不是已经完成的完整峰值表。任何未知项均阻止“整条路线可行”的声明；已知必要下界足以拒绝当前配置。

## 4. 为什么不能直接共享或降低恢复预留

1. `StatePublisher.withReadAuthority` 确实有一条 `authorityLane`，但 IO fee 和 bounded waiter slot 在进入 lane **之前**申请；等待者已经带着自己的 RAM 与源工作区。`authorityJobs[16]`、Close 的 WaitGroup 也说明等待者是被接受且须 join 的工作。
2. `CompletableWork.WithWorkspace` 只允许同一真实 pool/generation 嵌套，外国 pool 拒绝；`withRAMWorkspaceLoan` 在 inUse/children 未退时拒绝下一次借用。这是组件内排他，不是五个 pool 的共同排他。
3. 原 `checkpointHAPhysicalOnline` 在 join 前取得外层 Directory loan。本次已分开前述等待和借用，但 maintenance 内仍会调用其他组件，Usage/Gov 路径仍显式屏蔽 caller RAM scope并保持两笔原 fee。源 FD、returned rows、pins 和 update 的生命周期不能按函数返回次序猜测成互斥。
4. 只合并五笔 COW64 MiB，即使能够证明互斥，最多省 256 MiB，**仍缺129.053 MiB**。只合并 Directory/Usage/Gov 整池的乐观 scalar 模型仍缺128.385 MiB；两个单独方案均不进入目标规模运行。
5. 现有 `validateSelectedHASessionBudget` 只算实际 selected transport，公式为三笔(n+q+r)+d+Metadata baseline+12 controls=819.586 MiB。它不是整个启动/恢复预留。`recoverHASelectedAccountingMode` 在已经有 transport 时还要求父/子 384.016 MiB；使用二者相加只是保守重叠情形，仍须证明各 envelope 能否同时达到，并加入 caches/table。不能直接把 1387 改成 820。

源码：[Member lane](../../internal/replication/read_authority.go)、[工作区](../../internal/resourcebudget/completable_work.go)、[恢复收费](../../internal/resourcebudget/recovery_scope.go)、[在线 checkpoint](../../internal/app/ha_terminal_physical_online.go)、[selected session](../../internal/app/ha_selected_session_budget.go)。

## 5. 2 GiB / 1 GiB 对照及首选候选

下面固定原 Recovery reserve=1387 MiB、单请求16 MiB、Pricing IO32 MiB，以及本轮识别的常驻项；只比较 RAM，未替代 Disk/claim/OS 接受。

| 模型 | 已识别 normal MiB | 2 GiB 剩余 MiB（负数为缺口） | 裁定 |
| --- | ---: | ---: | --- |
| 原实际申请 | 1046.053 | -385.053 | 不成立；已修正预检 |
| 仅共享五笔 COW（尚未证明） | 790.053 | -129.053 | 即使能实现也不足 |
| Directory/Usage/Gov 共享池（已接线并核对费用） | 789.386 | -128.386 | 真实消费者与基础生命周期验证通过；仍不足，非完整峰值 |
| 五池按 class/精确 slot 大小取最大重数：27 slots（尚未证明） | 500.732 | +160.268 | 只有工程候选价值；嵌套/等待/源 pin 不受约束时这个估计无效 |

**1 GiB：在原 reserve 下单独 reserve 就超过总额；即使以上最乐观整池候选，也无法支持 1 GiB。** 将 reserve 乐观换成仅 transport 的819.586 MiB，最后一行仍需1320.319 MiB，且还漏恢复/caches/其它 owner。这个结论只拒绝当前参数和这些候选，不能宣称所有可能的 1 GiB 架构都不可能。

首选工程候选：**把持久终结保证与跨组件实际执行工作区分开，建立一个 foundation 持有的有限共享执行 lane。** 目标不是合并数据、权限或物理窗口；每个 source/role/generation/physical promise 仍由其原 owner 负责。仅 RAM 执行槽可以由原生生命周期协议借用。乐观节约为571809792 B（545.320 MiB），首次审计时已证明的节约为0；本次三后台接线已证明节约256.667 MiB，详见§7。

先做唯一一个候选 A，不运行原 100k/完整 HA 接受：

- 固定原五类 profile 和真实嵌套调用图。lane 的上限由最大的**同时活跃槽集合**导出，不以整池最大值代替嵌套集合；超过32槽/预算直接判定此候选不足。
- 所有 queue/waiter 在 source/native acceptance 前取得有限票据，公平/取消/撤销和已接受终结有明确 deadline；caller cancel 后仍保留原 source/physical promise。
- 先 pause/join accepted producers，再取得共同维护执行 lane；不能持 lane 等待一个也要借 lane 的 collector/peer callback 退出。
- 把跨组件嵌套改为原 owner 经真实生命周期发出的 RAM 子借用；保留旧/外国 scope、逃逸引用、未知 FD 退休拒绝。不能直接把 `p.work` 指向另一组件的 CompletableWork，或删除 WithoutRAMWorkspace 绕过检查。
- foundation 是共享 lane 的最终释放 owner；组件退出只退休自己的借用/reference。整体 Close 先撤销准入、join 全部实际借用，再释放总池；关闭一个组件不能撤销其他已接受终结。
- 校验对象：bounded concurrent waiter、跨组件嵌套、accepted cancel、Close 顺序、unknown child debt、maintenance drain，无能力升级。先协议与定向/race 证据，再计算真的节约与全阶段峰值。

候选 A 若无法给出安全同时活跃集合或最终预算仍装不下2 GiB，保留拒绝结果并裁定下一条路线，不能直接用乐观参数开公共 guard。当前不能给出1 GiB的交付承诺。

## 6. 本轮验证与后续检查点

预算问题直接由当前 `HAPhysicalWorkMinimum` 与真实 Manager 同时申请核对：normal、Completion、claim 精确匹配；一字节/一claim不足、溢出、无 Active、原配置拒绝及 opt-out 边界保留。新增常驻 permits 在最终实际释放后，RAM/claim/owner 归零。

第一次扩展对账的金额已匹配，但测试清理代码把新增普通 permits 释放了两遍，导致定向/包测试失败；已修正测试，在完整成功日志中保留正确释放检查。原失败日志保持，不归因为 Runtime 新故障。

本轮最终检查结果见[检查裁定](../verification/performance/2026-10-08-budget-feasibility/checks.json)。生产代码只有费用描述导出与必要预检补漏，没有修改并发协议，故本轮不新增 race/全仓库 gate 或容量运行；未来候选 A 触及并发与 lifecycle 必须执行受影响 race 和实际运行接受。

P1-A1 的最终验证见[本会话检查与源码身份](../verification/performance/2026-10-08-budget-feasibility/sharing-order/checks.json)：

| 检查 | 证据边界 |
| --- | --- |
| 新增6项 collector 生命周期回归及既有 collector/实际 native Close 定向 race | 覆盖已有 reducer join、借用在维护前结束、取消前/后、有限/旧回调、失败不进入维护、真实进入任务和退休；最后一 child 退出前费用不退 |
| 最终 Linux 真实 TLS/ext4 功能检查 | peer 信用及同一 owner 在线 checkpoint 两用例；两角色、三轮 Metadata 写入、两次共同 checkpoint/bank 续期、同一 service/DAO/transport 和最终 Close |
| 相同最终生产代码的 Linux race | 相同两个实际分支通过，无 race/OOM；宿主盘、mount、原 loop、容器均经实际退休核对 |
| Usage 全包 | 初版接线530.381秒通过；之后补入重放回调的有限生命周期，由最终定向/race覆盖。该旧变体全包不能标为最终源码完整 gate，未重复不可见新分支的漫长检查 |
| 隔离构建 | 第一次快照遗漏 app 编译所需 docs/runbooks，构建拒绝；补齐嵌入输入后构建通过。原失败保留，未改生产依赖/嵌入内容 |

功能/race运行采用实际2GiB cgroup、独占全分配3GiB ext4、2 CPU、GOMEMLIMIT=768MiB。在线组件的逻辑 reserve 仍为896MiB、手工Healthy；它验证本次顺序和原真实权限，**不能接受原1387MiB reserve的固定Runtime配置**。普通峰值221556736 B、race峰值554135552 B仅是这些小规模用例的观测，不是容量/吞吐门槛。

P1-A1当时的下一目标（本次三后台范围结果见§7）为P1-A2：跨组件 RAM 子借用与 foundation 最终释放协议，必须实际接线并检验有限等待票据、外国/旧 generation、取消后终结和 unknown child debt；不交付未被消费者使用的共享库，也不按测试串行减费。P1-A3 完整阶段费用随后核对，尤其 Member authority 等待者、native source/pins、恢复父/子与多session重叠。A2/A3 未完成前不进入P2。原24项仍5完成/19开放，此次只关闭一个明确的维护前置子项。

## 7. P1-A2 三个后台池的实际共享交付

本次范围为 Directory/Usage/Governance，保留 Metadata 物理窗口和 Ledger 请求的独立工作池。选定 Runtime foundation 在独立 selected-cut/原生前缀资格复核之后、真实消费者构造之前安装共享 owner；实际 Primary/Replica 各三个成员已由在线 TLS 测试确认。普通请求借用仍受压力限制；嵌套子借用保留原 source/cancel 身份和真实进入阶段，普通阶段不能升级为终结阶段。共享 RAM 不提供 native、确认、角色或物理存储权限。

共享池为原三种 profile 的 class/精确大小最大重数合集：25个槽，270393344 B；另付16384 B控制 RAM及一个claim。每个消费者仍预付并保留原 Active。最多8个成员，一个成员最多一个根调用进入/排队，固定FIFO，无等待goroutine或历史身份列表；取消从真实队列撤出。嵌套只由同owner、仍有效的父借用进入，单父最多一个活跃子、深度最多8；已经进入的子任务必须join，父返回后其旧context拒绝新申请，活跃子仍可完成。固定槽按实际占用核对，叠加超过合集直接拒绝，不额外支付或降级绕过。首个子任务错误会传回父任务，避免无限累计历史错误。

组件Close仅退休自己的票据，不能关闭别人的活跃池；foundation在业务、roots/native/source等实际join后释放最终共享owner。未知物理child污染原池并保留原费用，直到最后真实child退休；关闭绑定保留为拒绝，不能悄悄回落到新私有池。既有维护join在借用之前的顺序保留。

| 实际费用 | 共享之前 | 本次接线之后 |
| --- | ---: | ---: |
| 必要normal（含Completion/control/此前resident） | 1096865792 B / 1046.053 MiB | 827729920 B / 789.386 MiB |
| 其中Completion | 842481664 B | 573345792 B |
| normal claims | 70 | 54 |
| 原normal=661 MiB下的必要缺口 | 403757056 B | 134621184 B / 128.386 MiB |

实际减少269135872 B（256.667 MiB），并不是整个进程或cgroup峰值减少同样数值。配置预检采用实际安装后的向量，真实Manager同时申请全部必要owner核对一致；原2 GiB/1 GiB仍拒绝。首轮预算审计及旧对照保留其独立池范围，不再用旧数值描述当前共享实现。

最终证据：[本会话检查、源码与退休裁定](../verification/performance/2026-10-08-budget-feasibility/shared-background/checks.json)、[实际费用向量](../verification/performance/2026-10-08-budget-feasibility/shared-background/actual-vector.json)。生产改动涵盖resourcebudget共享借用/生命周期、config必要预检及foundation接线；测试确认真实两角色三个消费者和最终owner关闭。六组回归覆盖合集/成员上限与无费用泄漏、真实槽占用及过期、FIFO/取消/idle组件Close、异步已进入子任务join/错误、unknown child debt、普通/终结及外国owner边界。resourcebudget/config全包最终race通过。相同最终生产源码的Linux实际TLS/ext4功能/race都通过，无OOM，所有owned宿主盘/mount/loop/容器经实际退休核对。

最终普通/race观测峰值分别为229392384 B / 548610048 B；它们是小规模夹具观测，不是容量改善对照。夹具仍为逻辑2 GiB、reserve896 MiB、手工Healthy、实际2 GiB cgroup及3 GiB自有ext4；不是原reserve1387 MiB的Runtime接受。未执行全仓库gate或目标规模迁移；先前全gate失败及公开HA资格边界保留。证据目录当前被忽略，正式交付仍须提供可校验归档。

本次只完成三后台共享接线子项。原任务仍5完成/19开放，P1完整峰值和P2尚未通过。下一唯一目标P1-A3：核对剩余128.386 MiB缺口及全阶段owner/同时活跃峰值，特别是Member等待者、native pins、恢复父/子和session；在2小时工作上限内给出可复算裁定与唯一后续路线。当前向量仍不足，不直接进入容量接受，也不根据本次两项小规模成功取消Ledger/Metadata的独立终结保证。
