# 主持补充：核心状态机与运行接缝

基线 `381743f6613607dc256828f4776b52af8bdd232c`，2026-09-05。原核心账务、网关两个专项执行被工具中断，主持接手；本文件不是第三份独立核心评审。严重发现的独立裁决另见对应报告。没有修改业务实现。

## 实际阅读与判断

| 链路 | 源码、现存防御与结论 | 证据限制 |
| --- | --- | --- |
| Runtime Open/Close | `internal/app/runtime.go`：先取得数据目录锁，再初始化 vault/store/Ledger、重放与恢复、组装网关，最后启动后台任务；Close 停止接受工作、取消并等待任务，刷新派生/审计检查点后关闭资源 | 共享 Go 门禁及真实二进制演练；不是每个 Open 失败点的系统调用注入 |
| Admin 写入→运行快照 | Provider/Project 管理事务之后激活；Provider 激活用 runtime 所有的上下文，不依赖已经离开的 Admin 请求；激活失败记录 stale 并拒绝网关流量，恢复循环重试 | 不是跨 bbolt 与内存的单一事务；可用性代价是明确拒绝而非继续旧授权 |
| Request→attempt→settlement | `gateway/service.go`：鉴权、别名/CIDR、能力与治理准入；request accounting 与 attempt accounting 分开；每次尝试持有熔断/并发/价格 pin/资金预留，出站前 MarkStarted；结算清理与调用方取消脱钩 | PROV-01 证明上游错误分类仍能错误绕过保守结算；低层 settlement 正确不足以证明整体正确 |
| 预算并发 | `budget/manager.go`：pending admission 在金额判断中计入未完成预留，WAL 追加不长期持有准入锁；settlement 验证固定价格快照与实际 usage；允许实际成本超过估计预留并记账 | 本轮 gateway/budget/ledger race 通过；不能把资金估计当绝对费用硬上限 |
| Ledger Apply | `ledger/event.go`：事件身份/digest、顺序、项目/期间、预留与结算关系及金额检查；重复已知事件幂等但不能悄悄接受不一致内容 | 篡改/恢复测试已有；真实物理断电、设备谎报 fsync 未模拟 |
| Usage 派生 | `usage/collector.go` 饱和标记 lagging，由 CatchUp 补齐；checkpoint 增量与 bbolt rollup 事务配合，失败返回增量 | SUM-01：TakeCheckpoint 已取走但尚未持久化时 HTTP summary 暂时漏算，独立复现；无永久丢账证据 |
| 离线认证 | `app/usage.go`：只读锁、当前 schema、解钥、认证活动及封存历史/checkpoint，之后才打开 exporter/可写派生 | 当前自动化覆盖篡改、缺材料、KMS fake；真实33→35演练说明诊断拒绝迁移 |
| Deferred | `gateway/deferred_response.go`：提交只做请求准入；密封输入后登记；worker 钉住目标、重新验证提交 Key，先写 in_progress 再出站；queued 可重启继续，in_progress 重启终结为可能收费的失败；project round-robin 防止一项目独占全局排序 | 不把 queued 当已花费；deployment/provider 并发拒绝仍终结为可重提的暂时失败，见历史 C9；队列扫描仍遍历资源桶，见 C7 |
| 留存文件 | `app/provider_resources.go`、`gateway/inference_resources_store.go` 与 deferred：Project 归属、作用域加密、登记失败清理、到期与孤儿清扫；deferred 读取直接检查 TTL | SEC-01 换钥未迁移对象；SEC-04 是另一条 capture Get 的 TTL 缺口，不能用 deferred 正向测试覆盖 |
| 小型支撑模块 | 完整读取 `idempotency/store.go`、`circuit/manager.go`、`limiter/manager.go`、`sourcelimit/limiter.go`、`requestmeta/{request,source}.go`，并检查 `id`/`durable`/`timezone` 在调用链中的使用 | 以下限定判断；没有以模块规模小为由跳过 |

## 支撑模块与真实边界

- `idempotency` 当前只验证 1–128 个可见 ASCII 字符，不再有第二套持久化生命周期；真正幂等状态是 ProviderResource。`store_providers.go:397` 的同项目/同 kind/hash 检查与写入在一个 bbolt Update 内。不能因包名推定存在独立去重库，也不能将 Admin Key 的 256 字符契约与资源请求的128字符限制混淆。
- `circuit` 的 Lease 用 `sync.Once` 确保只报告一次；未到上游的准入拒绝调用 Abandon，不应成功关闭 half-open；配置要求正阈值/时长，并限制 half-open 数量。历史 target churn 下的 map 生命周期仍是容量证据缺口。
- `limiter` 用每项目锁原子决策 RPM/TPM/并发；拒绝不先扣 RPM；deferred 请求槽与执行槽分离，避免重复扣 RPM；Release/Reconcile 分别 once。实际 token 超估计形成债务，回补不超过桶上限。无长期项目增删后的容量清扫证明。
- `sourcelimit` 固定一分钟窗口、IPv6 /64 聚合、IPv4-mapped 统一、最多16384来源、超量共用 overflow 预算；无效来源也计数。窗口切换重新分配 map。多来源公平性存在明确降级，不能将 overflow 当每个新 IP 的独立预算。
- `requestmeta` 使用私有类型 context key；来源地址 Unmap/IsValid，空 request ID 不被当有效；这些是上下文类型/表示防御，不替代入口对代理来源的信任策略。

## 测试映射

本轮 `go test -count=1 ./...` 已执行一次；以下是已核实存在的测试入口，不重复运行整套来制造证据数量。具体门禁退出码/耗时见 [runtime-evidence](../runtime-evidence.md)。

| 场景 | 当前测试 |
| --- | --- |
| 资金并发与实际费用 | `TestThousandConcurrentReservationsNeverOversell`、`TestBudgetCheckIncludesConcurrentReservations`、`TestSettlementCommitsTrueCostBeyondTheReservation`、`TestRecoverStartedLeaseUsesFrozenPriceAndPreparedBounds` |
| Deferred 提交、执行与恢复 | `TestSubmissionReachesNoProviderAndNoLedger`、`TestExecutionChargesTheSameAsASynchronousRequest`、`TestInterruptedRequestIsFailedRatherThanResumed`、`TestQueuedRequestIsReclaimedAfterARestart` |
| Deferred 身份与清理 | `TestWorkStopsWhenTheSubmittingKeyIsRevoked`、`TestCancelIsDeterminateWhileQueued`、`TestDeleteRemovesTheRecordAndItsObjects`、`TestDeleteRefusesWhileTheRequestIsStillOwed`、`TestAnUncollectedAnswerStopsBeingReadableAtItsTTLNotAtTheNextSweep` |
| 限流与熔断 | `TestAtomicPolicyAdmissionAndRelease`、`TestAcquireDoesNotConsumeRPMWhenTPMRefuses`、`TestAbandonedHalfOpenProbeNeitherClosesNorFaultsTheCircuit`、`TestLeaseOutcomeIsRecordedOnlyOnce` |
| 来源容量 | `TestTrackingCeilingSharesOneBudgetInsteadOfGrowing`、`TestConcurrentSourcesAdmitExactlyTheBudget`、`TestIPv6RotationCannotFillTheTrackingTable` |

## 未成立的候选与剩余缺口

曾怀疑 worker 被取消后把已取消的 ctx 传给 finishDeferred，必然使终态保存失败。实际 `PutProviderResource` 直接执行 bbolt Update，并不检查 ctx.Err；因此这个因果链不成立。该反证不等于磁盘写入失败一定会被自动重试，也不替代终态写入失败的故障注入。

仍保留高基数配置 churn、慢客户端/上游长期占用、所有存储写入/rename/fsync 失败排列、真实云/KMS/Provider、生产负载和延长 soak 为证据缺口。本轮结论是核心链路已审查且存在明确反例与防御，不是全部不变量成立。备份与进程 kill 的实测范围以 runtime-evidence 的最终记录为准。
