# 默认投影构造与两级账务恢复的共享 RAM 生命周期

默认 Standalone 和 Primary 的 fresh Ledger 投影启动恢复通过 `Runtime.recoverProjectedAccounting` 执行。默认三角色的账务候选构造也使用同一 workspace 配置：Standalone/Primary 覆盖完整 native 位置认证及有限批归约，Replica 覆盖共同 Ledger/Usage 候选构造。Standalone/Primary 的 Usage 完整回放、Governance 外部锚校验与投影构造，以及 Usage 启动归档/窗口核对也使用这份临时 workspace。Replica 不写恢复终结事实。这些接线不代表可信增量启动或完整状态的资源接受。

## 准入与退账

启用 `resource_budget` 时，`accounting_recovery_ram_bytes` 必须显式提供，结构下限为 128 MiB，且对应同一个 Runtime 的共享 Manager。该估算必须覆盖 Ledger/Usage/Governance 构造、Usage 归档窗口核对和两级恢复各阶段的最大峰值。这些阶段在默认启动中串行，分别取得同额费用，无需新增并行 claim。未配置共享预算时保持原有路径；配置存在而 Manager 缺失、费用不足或配置与 Manager 不一致时拒绝调用，不能默默跳过计费。

默认候选构造在删除旧派生文件、取得 native 维护 lease 和解码源之前申请费用。预算拒绝不会先删除旧文件。费用保持到同步构造/验证返回或失败候选实际清理完毕；取消或 panic 不提前退账。Standalone/Primary 的失败路径先清理候选、再释放 native 独占权；Replica 共同 generation 在 Ledger 构建完成之后的异常也会关闭两份候选 DB。成功返回后退还的是临时构造 workspace，继续存活的 DB/mmap/cache 不因此获得已计费声明。Usage 借用的业务 Ledger 和维护 lease 在失败及 panic 时继续属于调用者，不能随派生候选关闭；独立 Primary Usage helper 自己构造的 Ledger 和 lease 则先清理候选再释放独占权。Governance 费用在外部锚完整回放前取得，原始签名、位置和角色复核仍执行；Standalone 的 Governance 错误保持原有 unavailable 路径，Primary 仍拒绝启动，不能因已取得 workspace 授予治理或 HA 权限。Usage 启动窗口在任何归档比较或 prune 之前申请，预算拒绝返回零报告并保持旧窗口/归档不变；费用直到全部比较、有限 prune 步及最终全 native 认证返回后退还。默认启动之外的离线/维护构造 API 仍需调用者自己的预算准入。

费用使用独立 Recovery 类和储备，允许物理压力为 unknown/pressured 的启动恢复，不借用普通工作准入。开始构造维护 writer 或枚举输入前申请；整个两级算法、关联校验、有限页、结算编码、native 写入与确认等待均处于这份费用的生命周期内。算法返回并完成 writer.Close 的实际 join 后才退账。调用者取消或暂时失去确认不能提前退账。该费用不提前释放 native 维护独占权。

执行槽属于账务 owner，由 fresh owner 构造一次；启动/维护 Runtime 视图复制同一槽的指针，不能通过换一个视图绕过独占。同一 owner 的第二个并发恢复调用立即返回 busy，不排队，也不再申请费用；缺少 owner 槽或传入另一份投影会拒绝调用。维护 writer 保持私有，不能由算法以外的调用者提前 Close。构造 writer 失败会退还已取得费用；正常返回、失败和 panic 都沿 defer 顺序先关闭 writer，再退账，最后释放执行槽。默认 Primary 的最终排序前缀确认和就绪屏障继续由启动流程负责。

## 预算组成

Standalone 的校验至少容纳 Governance 固定 owner、价格固定 owner、价格启动 IO workspace、账务恢复 workspace 四份 Recovery claim 及控制开销。HA 的校验同时容纳三份重建/候选、三份 session 队列与入站 envelope、一份 NativeSource 读者、Governance owner、价格 owner、价格启动 workspace、账务恢复 workspace，共十一份 claim。聚合校验覆盖类别上限、储备及整数溢出；字段不能只独立通过而总额不足。

128 MiB 只是拒绝明显不足配置的逻辑下限；必须在目标环境校准页对象、宽记录、JSON/日志编码和投影归约同时存活时的峰值。它不约束 Go allocator、RSS 或 mmap，不预分配磁盘，也不证明终结或 HA 确认一定成功。

原生日志 Open、默认 Standalone/Primary 的完整 Usage/Governance/Metadata 启动恢复、长期 DB/cache、日志维护、共享实际磁盘和未确认 owner、所有业务接受的终结信用、pin/恢复截止期限、生产曲线仍待完成。本契约不授予已保存投影重开、跨组件 Seal 或跳过原生日志认证的权限。

两级恢复验证见 [本地证据](../verification/performance/2026-10-04-accounting-recovery-resources/README.md)；后续默认构造与异常清理见 [构造 workspace 证据](../verification/performance/2026-10-04-accounting-startup-workspace/README.md)。

默认 Usage/Governance 构造及窗口接线见 [投影启动 workspace 证据](../verification/performance/2026-10-04-projection-startup-workspace/README.md)。这些阶段的费用不涵盖 exporter 构造、原生 Open、长期 DB/cache/mmap、Metadata 初始化或物理磁盘容量；128MiB结构下限不是这些阶段的实测峰值。

[认证恢复整job owner](authenticated-recovery-resource-owner.md)增加显式的同Runtime池校验、共享执行槽和真实IO/join后退账。后续[限定生命周期的Recovery授权](../verification/performance/2026-10-04-recovery-scope/README.md)已使显式job内部临时IO/Background在Unknown/Pressured使用同池Recovery逐项计费并join；实际配置的Disk=0仍阻止构造持久认证owner，该API不能替代上述默认启动恢复或启用M3。
