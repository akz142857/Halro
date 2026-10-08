# Governance Manager 退休与 owner 生命周期

2026-10-03，I-04/I-10/Runtime generation 的前置边界。State 与 projection Manager 使用同一 operation gate；不提供默认磁盘选择或持久化信任。

## 准入与 join

所有公开 owner 读、Ready/State、Report、Outcome fence/page/batch/detail 和导出在访问依赖前取得私有 ticket。退休永久拒绝新调用，错误同时匹配 `ErrRetired` 与 `ErrUnavailable`；取消/失败不变成空结果或成功。既有 availability/cancellation 分类保持。

`Retire(ctx)` 的 context 只限制等待，不取消已接受调用。进入前已经取消的 context 不撤销入口；进入后超时保持永久退休，重试等待同一 drain channel。每个调用独立 ticket；嵌套操作只接受仍活跃且属于同一 Manager 的 parent ticket。callback 结束后保留的 context 不能重新准入，另一 Manager 的 ticket 不能授权旧 owner。

Report ticket 包括等待 Manager mutex、账务/定义检查、native append、已落盘后的 Apply、checkpoint 和成熟度读取。durable Apply 使用 WithoutCancel，不因调用取消跳过事实；调用取消仍可能使最终成熟度/响应失败，不能据失败重复制造已接受事实。

导出票据持有整个 callback：有限来源 Complete/seal/join 之后仍等待实际 manifest 发布及外层回调返回。`WriteExportWithDefinitionOwner` 在取得 accounting dependency 之前入场，嵌套 `WithOutcomeExport` 继承 ticket。来源 reader 清理先于最终 ticket 释放。

最后一个接受操作退出时自动清理 Log/State/projection、accounting/definitions/checkpoint/clock、页签名 key，并清 unavailable 外部错误对象；外部错误本身也可能捕获历史 State。无需调用方重试 Retire 才清引用。Manager 不关闭这些共享资源，独立持有的 State alias 和 context 其它外部 values 仍由其 owner 管理。`State()` 是旧兼容别名而非 lifetime loan，退休后返回 nil。

## Runtime 顺序

业务 drain：永久撤销新工作与导出→HTTP shutdown→导出 join→后台/alert join→Governance Retire→Usage CatchUp→Budget Retire→允许 native maintenance。任何失败都不授权新 generation 或 promotion；依赖仍开放以便接受的清理/发布完成，后续调用可重试 join。正常 drain 保持复制确认能力。

终态 Close：join Runtime 导出与后台→Governance Retire→Budget 非阻塞 WithdrawAdmission→failure capture 等终态清理→coordinator Stop→Ledger Close→Budget Retire→清 Runtime.state→其余 native/Metadata/audit/vault 关闭。native Close 唤醒缺失 Apply 前序；Stop 唤醒已 Apply 但等待未确认 ACK 的 Background 调用。不能在这两个唤醒之前无界等待 Budget Retire。终态拒绝未确认成功，不撤销已经 durable/Apply 的事实。

仅 terminal accounting close helper 执行 Stop，planned drain 不提前终结 ACK。Replica 没有默认业务 Manager；共享 native/projection 资源仍必须按实际 owner 生命周期关闭。Gov Report/export 的外部 callback 不得调用自身 Retire，会等待自己；不可中断 IO 仍可能延迟 join。

## 证明范围

回归包含 State/disk blocked checkpoint/canceled Report、完整实际 manifest 发布、Complete 后仍保持 callback、捕获 parent 过期拒绝、已入场等待 mutex 的读取、每个公开 owner 路径拒绝、自定义外部 error 捕获 State；Runtime 包含 Governance 发布在账务退休前、直接/失败 HTTP drain 后旧 Manager 读拒绝、真实 native Apply 前序缺口和最小真实 Ledger/PrimaryCoordinator ACK 停滞。检查与复核见 [retirement evidence](../verification/performance/2026-10-03-governance-manager-retirement/README.md)。

ticket 数量与等待 mutex 的 goroutine 尚无共享费用上限；本边界只证明永久撤销、join 和 Manager 自己的引用释放，不证明实际 GC/RSS、IO 时长、D-03 内容/key-set/absence、默认 Runtime/Replica 磁盘 generation 发布或生产接受。
