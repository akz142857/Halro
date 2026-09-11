# R2 + R6 账务、错误语义与持久化独立评审

## 结论摘要

- 评审范围：`v0.7.1^{}` (`84f2638e973f23935b9eda423143f65ff25a852c`) 到
  `main` (`222d08f84f61493fc9a273d351cc728528d6e30c`)。
- 角色范围：R2 核心逻辑与账务、R6 数据升级/可靠性中与 Ledger、Usage、Parquet、备份恢复有关的源码链。
- 独立性：未读取其他角色报告；未修改产品代码；未访问真实数据或真实 Provider。
- 总体判断：INV-02 和 INV-10 的主路径防线成立，但发现 **1 个 P1/high 已确认问题**：重启恢复 pending lease 时，新版本新增的 Offering/Profile/Region 快照被丢弃，且 Ledger 状态机允许该 settlement 落盘。它直接违反 INV-11，应在 v0.8.0 放行前修复并补回归。
- 另有 3 个候选问题，需要跨版本运行或故障注入后裁决；其中 checkpoint 版本未随新增字段变化会使旧二进制可能静默接受并重写新 checkpoint，是本轮 rollback refusal 必须完成的实验。

## 评审方法与入口

按以下数据链逐段反向核对字段和执行语义：

`Registry.Register(target)` → `gateway.startAttempt` → `budget.ReserveLeaseDetailed` →
`Ledger reservation/started/settled` → `usage.Aggregate.Apply` → checkpoint → Parquet schema 6→8 →
failed-request query / failure capture / backup-restore。

主要入口及已有防线：

| 环节 | 入口和证据 | 已有防线 | 结论 |
| --- | --- | --- | --- |
| Target 注册 | `internal/provider/provider.go:546-648` | adapter 必须有 Profile；Profile/Surface/Offering 精确匹配；Region 必须属于 Surface；非法组合在进 Registry 前拒绝 | 正常 Gateway target 不可携带非法组合 |
| 预留 | `internal/gateway/service.go:625-643`；`internal/budget/manager.go:917-996` | target 快照复制到 Attempt 和 reservation；WAL append/apply 成功后才返回；上游调用发生在此后 | INV-02 主路径成立 |
| WAL append | `internal/ledger/log.go:507-548` | append 前 `validateForAppend`；请求一旦进入 writer，即使 caller 取消也等待持久化结果 | 取消不会让已接受 append 被误报为未提交 |
| Started/settlement | `internal/budget/manager.go:1006-1027,1122-1168` | started 和 normal settlement 复制 Attempt 的完整 target 快照；price snapshot/lease mode 冻结 | 正常路径字段连续 |
| Ledger 原子结算 | `internal/ledger/event.go:643-725` | settlement 必须对应未结算 reservation；核对 project/period、run、price snapshot、lease mode；同一状态转换释放 reservation 并提交金额 | 金额与 run 归因原子；但 target 字段未与 reservation 比对，见 F-01 |
| 错误标准化 | `internal/gateway/failure.go:76-184`；`internal/gateway/service.go:2570-2581,2971-3023,3092-3131` | retry/fallback 和 ambiguous 结算读取原始 `provider.Error`；canonical reason 只修正展示 class，明确不改 `Retryable/Ambiguous` | INV-10 成立 |
| Usage 派生 | `internal/usage/aggregate.go:293-322`；`internal/usage/failures.go:249-270` | settlement 字段逐项复制；最后一次成功不会错误借用之前失败的 provider context | 正常链一致；上游 Ledger 缺失会被忠实放大 |
| checkpoint | `internal/usage/checkpoint.go:359-471`；`internal/app/runtime.go:1016-1069` | 缺失、版本不符、watermark/rollup 不符时丢弃派生并从 Ledger 重建；旧 JSON 缺字段自然为空 | 向前升级可把旧缺字段解释为 unknown；回滚风险见 C-01 |
| Parquet | `internal/usage/parquet.go:45-55,191-258,272-353,621-695,712-751` | 支持 schema 3..8；旧行按行版本收窄后与 Ledger 比对；partition 和 manifest 均临时文件+fsync+rename；checksum、序列、汇总、内容可校验 | 设计可覆盖 6→8；现有物理旧 schema 测试只覆盖 7→8，需 populated 6→8 实验 |
| Failure capture | `internal/failurecapture/failurecapture.go:231-299` | 标识符有界；failure reason 枚举；Offering/Profile/Region 再次校验；加密后原子写 | 非法新记录 fail closed |
| Backup/restore | `internal/app/backup.go:430-468,656-700` | 备份前从 Ledger 重建快照并与 Parquet reconcile；恢复 staged Ledger 后再次 reconcile；认证链头不一致拒绝 | 可阻止错误派生进入可恢复备份；F-01 已经成为合法 Ledger 事实，无法由此修复 |

## 终态与金额/归因对照

| 终态 | 可达入口 | 金额处理 | Retry/Fallback | 归因与失败语义 |
| --- | --- | --- | --- | --- |
| 成功 | `activeAttempt.finish(nil, ...)`，`internal/gateway/service.go:756-789` | provider usage 合法时按实际；缺失/非法时按 prepared bounds 估算；使用冻结 price target | 本 attempt 终止 | target 快照保持；HTTP 200；旧失败上下文先清空 |
| 创建 attempt 后、发送前失败 | `activeAttempt.abort`，`internal/gateway/service.go:846-865` | 0，释放 reservation | 不评价 breaker | `FailurePhase=pre_provider`，字段保持 |
| price pin commit / MarkStarted 失败 | `internal/gateway/service.go:675-696` | 0，后台 cleanup context settle | 不评价 breaker | target 字段保持，但 phase 为空，见 C-02 |
| 上游 definitive 失败 | `settlementForResult`，`internal/gateway/service.go:2982-3010` | 0 | 仅依据原始 `provider.Error` 决定；authentication 等不 fallback | canonical reason 和 class 在 settle 前统一补齐 |
| 上游可能已接受后的 ambiguous 失败 | 同上 `:2991-3009` | 有有效 usage 用其结算，否则按 prepared bounds / reservation 保守估算 | `retryable` 首先拒绝 Ambiguous，故不重试/回退 | `Ambiguous=true` 原样持久化；符合 INV-02/10 |
| 重试/回退后成功 | 每个 target 各自经过 `startAttempt` | 之前 definitive attempt 为 0；最终成功 attempt 单独收费 | 次数写入各 attempt | `AttemptNumber/RetryCount/FallbackCount` 与该次 target 一起冻结；成功清除之前失败上下文 |
| caller 取消 | `describeProviderFailure`，`internal/gateway/failure.go:109-118`；breaker 处理 `internal/gateway/service.go:868-875` | 裸 context cancel 为 definitive 0；若 adapter 能证明已接受并返回 Ambiguous，则走保守结算 | 裸 cancel 不 fallback，且不惩罚 breaker | phase=`client`；adapter 的 accepted/ambiguous 事实优先 |
| 重启，未 Started | `RecoverPendingLeases`，`internal/budget/manager.go:1171-1239` | `recovered_not_started`，0，释放 reservation | 不重试 | **丢 Offering/Profile/Region，F-01** |
| 重启，已 Started | 同上 | `recovered_started_unknown_result`，prepared bounds + frozen price 保守收费 | 不重试 | **丢 Offering/Profile/Region，F-01** |

## 已确认 finding

### F-01 — P1 / High — 重启恢复 settlement 丢失 Offering、Profile、AccountRegion

**违反不变量：** INV-11；同时削弱 INV-02 的可审计性，但未发现余额计算错误。

**入口与可达条件：** 任一携带新 Provider 归因的 attempt 已持久化 reservation，而进程在 settlement 前退出。启动时 Runtime 在 listener 就绪前注册 Usage observer 并调用恢复：
`internal/app/runtime.go:397-415`。该条件同时覆盖 reservation 后/Started 前退出以及 Started 后结果未知退出，是正常 crash-recovery 面，不要求伪造输入。

**根因：**

1. 正常预留已把三字段写进 Attempt 和 reservation：
   `internal/budget/manager.go:917-925,963-988`。
2. `RecoverPendingLeases` 从 reservation 重建 Attempt 时只复制 Route/Deployment/Provider/Model，遗漏
   `OfferingID`、`ProfileID`、`AccountRegionID`：`internal/budget/manager.go:1192-1203`。
3. `m.settle` 随后从这个残缺 Attempt 构造新 settlement：`internal/budget/manager.go:1122-1168,1223-1229`。
4. Ledger 状态机只比较 project/period、run、price snapshot 和 lease mode，不比较 reservation 与 settlement 的 Route/Deployment/Provider/Offering/Profile/Region/Model/attempt counters：
   `internal/ledger/event.go:643-664`。
5. append 校验为兼容旧记录，允许 Offering/Profile/Region 同时为空：
   `internal/ledger/event.go:216-230,309-325`。因此新版本产生的空归因不会被拒绝。
6. Usage 忠实复制 settlement：`internal/usage/aggregate.go:293-322`；checkpoint、query 和 schema-8 Parquet 之后都会保存空值。Offering/Region breakdown 会把它当 unknown/缺失，而不是原 reservation 所记录的产品。

**最小复现：** 用仓库外临时 Go overlay 增加一个只读 probe：创建带
`bigmodel.coding-plan / bigmodel.cn.coding.chat.v1 / cn` 的 metered lease，MarkStarted，调用
`RecoverPendingLeases`，由 observer 读取新 `AttemptSettled`。命令：

```text
go test -count=1 -overlay=/private/tmp/halro_recovery_attribution_overlay.json \
  ./internal/budget -run TestReviewRecoveryPreservesProviderAttribution -v
```

结果（预期断言失败，证明问题可达）：

```text
recovery lost provider attribution:
reservation=("bigmodel.coding-plan","bigmodel.cn.coding.chat.v1","cn")
settlement=("","","")
FAIL github.com/akz142857/Halro/internal/budget
```

**影响：** crash/restart 窗口内所有 pending attempt 的金额仍按现有保守规则正确释放/提交，但历史产品、Surface 和账号地域不可恢复；Usage 筛选、失败归因、breakdown、checkpoint、Parquet 和备份会永久继承空值。多产品共用 Provider 时，按 Offering/Region 的成本与事故分析不完整。

**已有防御为何没有挡住：** Registry 的组合校验只保护入口；reservation 本身正确；Ledger 的“空字段=旧记录”兼容规则没有办法区分真正旧 reservation 和当前 recovery 产生的缺失 settlement；backup 只能证明 Parquet 与已落盘 Ledger 一致。

**建议修复与回归：**

1. recovery 重建 Attempt 时复制三字段（以及最好系统性复制所有 immutable attempt attribution）。
2. Ledger settlement 对 reservation 的 Route/Deployment/Provider/Offering/Profile/Region、RequestedModel/ProviderModel、attempt counters 做一致性比较；旧 reservation 若确实缺字段，只允许 settlement 同样为空，不能由当前配置补默认。
3. 增加 Started 与未 Started 两个恢复用例，断言 reservation → settlement → Usage → checkpoint restore → Parquet reconcile 全链字段相等，同时金额分别为 conservative 与 0。

## 候选 findings（尚未完成运行裁决）

### C-01 — 候选 P1 / Medium-High — checkpoint 仍为 v13，旧二进制可静默接受新归因字段

**证据：** v0.7.1 与 main 的 `checkpointVersion` 都是 13；main 将新字段直接加入
`AttemptEvent` JSON，但 `RestoreCheckpoint` 只检查 version 和结构顺序，不校验每条 attempt 的 attribution/语义：
`internal/usage/checkpoint.go:59,130-135,359-471`。Go JSON 对未知字段默认忽略。v0.7.1 的 Parquet reader 上限是 schema 6，而 main 是 8；但 Runtime 创建 exporter 时不加载/校验 manifest：
`internal/app/runtime.go:346-364`，实际 Verify 在后台导出、doctor、backup 或离线命令才执行。

**可达性推演：** main 写 v13 checkpoint → 用 v0.7.1 启动同一副本 → old reader 接受 head/segment 并丢弃不认识的新字段 → 若旧进程有新事件，open tail 会从
`internal/usage/checkpoint.go:184-217` 对内存对象重编码 → 再切回 main 时仍是 v13，不触发 Ledger 重建。与此同时 schema-8 manifest 可能只让周期 export 报错而不拒绝进程启动。

**风险：** rollback 没有“明确拒绝”，而可能以降级 Usage 继续运行；重写 open checkpoint segment 后，新字段可能从派生层消失，且备份前 Parquet reconcile 可能开始失败。Ledger WAL 仍是权威，所以可通过强制丢弃 checkpoint 重建恢复，尚未证明权威账务损坏。

**需要裁决：** 用真实构建出的 v0.7.1/main 二进制和 populated 数据副本做
`v0.7.1 → main → v0.7.1(产生至少一条新记录并 checkpoint) → main`，逐步记录启动是否拒绝、checkpoint tail 是否重写、Usage/Parquet/backup 是否一致。未完成该实验前不把推演冒充运行事实。

### C-02 — 候选 P2 / High — 两个当前版本的发送前失败 settlement 留空 FailurePhase

**证据：** 通用 `abort` 明确写 `pre_provider`，并说明空 phase 会被 UI 当作字段出现前的旧记录：
`internal/gateway/service.go:846-865`。但 price pin commit 与 `MarkStarted` 失败直接调用
`settleAttempt(... Outcome: "pin_commit_failed"/"start_failed")`，未经过 `enrichSettlement`，也未设置 phase：
`internal/gateway/service.go:675-696`。

**可达条件：** reservation 已成功，随后 bbolt price-pin commit 或 Ledger started append 返回错误，而 cleanup settlement 仍成功。该路径在存储 I/O/注入故障下可达，但本轮未对 bbolt/WAL 组合注入精确单次错误。

**影响：** 金额为 0 且 reservation 被释放，故不是账务错误；但新记录的失败阶段与旧记录不可区分，违反 INV-11 的诊断语义。建议统一通过一个 pre-provider settlement helper，并做一次“pin store 失败、Ledger 仍可 settle”的故障注入。

### C-03 — 候选 P2 / Medium — Ledger durable boundary 未约束失败语义的交叉一致性

**证据：** `Event.Validate` 校验 provider 标识符、canonical reason 枚举和数值非负，却不校验
`Outcome`、`ErrorClass`、`FailurePhase` 枚举，也不约束
`FailureSemanticsRecorded/Retryable/Ambiguous/reason/status/outcome` 的组合：
`internal/ledger/event.go:135-230,300-306`。现有 fixture
`internal/usage/provider_identifiers_test.go:29-40` 本身构造并接受了
`bad_request + HTTP 400 + rate_limited + retryable + ambiguous` 的矛盾组合，且
`FailureSemanticsRecorded=false`。

**现有防御/可达性：** 正常 Gateway 由 `describeProviderFailure` 单点分类，并从原始 provider error 保持 retry/ambiguous，故当前外部请求路径没有发现可制造该矛盾的方法。风险来自 recovery、新内部 caller 或未来 adapter 直接构造 settlement；因此保留为 hardening 候选，而非当前漏洞。建议在 budget/ledger 边界定义有限 outcome/phase/class 和 recorded 语义组合，并保留明确的 legacy 读取规则。

## Parquet 6 → 8 与旧记录结论

1. main 可读范围是 3..8，manifest 6 会先给既有 entry 补 `SchemaVersion=6`，再只升级 manifest；旧 partition 不重写：`internal/usage/parquet.go:191-215`。
2. reconcile 按每行 schema 版本把当前 Ledger 派生行收窄。schema <7 清 Offering/Profile；schema <8 再清 Region/canonical failure semantics：`internal/usage/parquet.go:712-751`。这使 v0.7.1 旧记录保持空/unknown，不会被默认归入新 Offering/Region。
3. partition 与 manifest 的发布均为 temp → chmod → encode/write → fsync → rename → directory fsync：`internal/usage/parquet.go:621-695`。manifest commit 失败可能留下未引用 orphan partition，但不会把它纳入已认证 manifest；重试按 snapshot/sequence 再发布。
4. Verify 检查安全路径、checksum、row schema、事件唯一性、序列/汇总，并在提供 Ledger snapshot 时逐行内容 reconcile：`internal/usage/parquet.go:272-353,754-790`。doctor 的 `Verify(nil)` 只做结构/校验和，不等同于 Ledger reconcile；backup 和 restore 使用 snapshot reconcile：`internal/app/doctor.go:191-201`、`internal/app/backup.go:457-468,688-700`。
5. 自动测试的 manifest-upgrade 循环覆盖 3..7，但从空 manifest 写当前行：
   `internal/usage/parquet_test.go:190-217`。实际“旧物理列不存在”的 fixture 只构造 schema 7：
   `internal/usage/parquet_test.go:325-397`。因此 **物理 schema 6 partition 与新 schema 8 partition 共存**仍需 populated v0.7.1 实验，不应仅凭当前单测宣称完成 6→8 认证。
6. v0.7.1 的源码上限为 schema 6，会在其 Export/Verify 遇到 manifest 8 时拒绝；但如 C-01 所述，尚未证明旧 Runtime 启动本身拒绝。

## INV-14 有界性观察

- Ledger payload 有 `maxPayloadSize`；provider code/request ID 在 Gateway、Ledger、capture 三处收窄；failure reason、Region 使用枚举，未发现把上游正文写入指标/归因维度的路径。
- failure query 的默认页只为候选页建立 attempt index；带 attempt 过滤的高级查询仍会在 RLock 下扫描保留窗口，代码明确承认该权衡：`internal/usage/failures.go:105-132`。Retention window 和返回 limit=100 提供边界，本轮没有足够证据把它定为性能 finding；应在 S3 的 populated 大窗口数据上测 P95/alloc/collector stall。
- Recovery 遍历 durable pending lease 数量；该集合受已接受未结算 attempt 数约束，但 crash 后一次性恢复成本需要在长稳/大 pending fixture 中记录。

## 已执行验证

### 通过的现有窄包测试

```text
go test -count=1 ./internal/budget ./internal/ledger ./internal/usage ./internal/gateway ./internal/failurecapture

ok  github.com/akz142857/Halro/internal/budget         3.714s
ok  github.com/akz142857/Halro/internal/ledger        54.990s
ok  github.com/akz142857/Halro/internal/usage          9.560s
ok  github.com/akz142857/Halro/internal/gateway       26.535s
ok  github.com/akz142857/Halro/internal/failurecapture 2.943s
```

这些包已有成功、definitive/ambiguous、retry/fallback、caller cancel、accepted malformed response、发送前 abort、恢复金额、checkpoint/Parquet 与 failure capture 覆盖；但它们没有断言 recovery 的新增 target attribution，所以整体包绿不能反驳 F-01。

### 独立 probe

仓库外 overlay probe 如 F-01 所列，稳定失败并给出 reservation/settlement 三字段对照。它没有进入产品工作树，也没有调用网络或 Provider。

## 仍需的运行实验与回归优先级

1. **阻断修复回归（P1）：** F-01 的 Started/未 Started 两路径，贯穿 Ledger → Usage → checkpoint restore → schema-8 Parquet → backup/restore。
2. **跨版本 rollback（P1 候选裁决）：** populated v0.7.1/schema-6 数据副本四段切换实验，特别检查旧 binary 是否在启动时拒绝 schema8，以及 open checkpoint tail 是否丢字段。
3. **物理 schema 6→8：** 至少一个真正由 v0.7.1 写出的 schema-6 partition，加一个 main 写出的 schema-8 partition；compact、verify/reconcile、backup、restore 后逐行与 Ledger 比较。
4. **kill-point 矩阵：** reservation append 前/后、Started append 前/后、provider 已接受但 response 前、settlement append 前/后、checkpoint/partition/manifest fsync/rename 前后；每个点记录余额、pending、归因、重复恢复和 orphan 文件。
5. **C-02 故障注入：** price pin commit 和 MarkStarted 单次失败，证明金额、phase、terminal log/Usage/capture 是否一致。
6. **大窗口性能：** Offering/Region/failure reason 组合过滤的 P50/P95、alloc 和 Usage collector stall；大量 pending lease 的启动恢复时间和内存峰值。

## 放行建议

当前 R2/R6 结论为 **NO-GO（局部）**：F-01 是可复现的当前版本 crash-recovery 归因丢失，应先修复并完成第 1 项回归。完成 C-01 的真实跨版本实验前，不应在发布评估中宣称“旧版本明确拒绝 schema 8 / rollback 安全”；其余候选可在故障注入后重新分级。
