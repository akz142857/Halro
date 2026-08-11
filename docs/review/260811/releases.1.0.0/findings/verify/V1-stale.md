# V1 · 对抗验证：activationTracker 单标志 stale 机制（A1-01 / A4-01 / A4-02）

> 角色：[role-prompts.md](../../role-prompts.md) §1 + §8 对抗验证。任务是**证伪**，默认原发现为错。
> 验证日期：2026-08-11。仓库 HEAD：`2cd24a7`（工作树干净，见 §6）。

## 裁决：CONFIRMED

两条主张全部成立，包括 A4 自己标注为"推理结论、未实证"的那一半——本次已用独立探针实证补全。
未找到任何拦住该路径的既有控制点。原判 P1 / 阻塞发布**不高估**；后果一侧原判还略有**低估**
（见 §4：新建策略的场景不是"旧规则继续生效"，而是**整套策略规则完全旁路**，只剩内建 mandatory 规则）。

## 1. 主张一的逐步核对（任一域激活成功清除其他域的失败）

我按"它是错的"去找反例：找按域分维度的状态、找第二个 tracker、找 markCurrent 的条件分支、
找恢复时的按域重放。全部不存在。

逐步路径（每步实读代码）：

1. tracker 是进程级单例、单一标志：`internal/app/activation_state.go:31-36` —— 结构体只有一组
   `staleSince / reason / generation`，无任何按域字段。`Runtime` 上只有一个 `activation` 实例。
2. `markCurrent()` 无条件清空：`activation_state.go:52-58` —— 不看 reason、不看调用方属于哪个域。
3. 四个域共享这同一个 tracker，调用点核实为：
   - topology：`internal/app/providers.go:91`（markStale）/ `:94`（markCurrent），入口
     `activateTopology()`（`providers.go:87`）与 `activateTopologyAfterCommit()`（`:107`），
     后者被 14 个拓扑变更点调用（`admin_providers.go:131,203,284,332,654,717,769`、
     `admin_deployments.go:176,348,405`、`admin_prices.go:152,201,270,449`）。
   - auth：`internal/app/admin_projects.go:484` / `:487`（`activateAuthSnapshot()`，`:763-773`）。
   - redaction：`internal/app/admin_redaction.go:324` / `:327`（`activateRedactionPolicies()`，`:315`）。
   - token guard：`internal/app/admin_token_guard.go:342` / `:345`（`activateTokenGuardPolicies()`，`:333`）。
   全仓 `markStale/markCurrent` 调用点仅此四组 + 恢复循环 `activation_state.go:125` +
   测试 `commit_protocol_test.go:64,87`（grep 全量核对，见 §7）。
4. 数据面拒流与就绪只看这一个标志：`activation_state.go:84-99`
   （`refuseWhileSnapshotsStale`，挂载于 `runtime.go:1249,1272` 两个 route group）、
   `runtime.go:1493-1498`（`/health/ready`）。没有任何按域的第二道门。

因此路径完整：redaction 域 `markStale` → 数据面 503 / ready 503 → 任一无关拓扑或 auth 变更成功
→ `markCurrent()` → 标志清空 → 数据面放行、ready 200，而 redaction 变更仍未生效。

**独立探针 V1-P1 实证**（观察型，非断言型；挂载方式见 §7）：

```
V1-P1 data plane while redaction-stale: 503
V1-P1 after unrelated activateTopology: stale=false reason="" generation=1
V1-P1 data plane after clear: 401, readiness: 200
```

## 2. 主张二的逐步核对（恢复循环 ≤5.5s 治愈它并不修复的域）

1. 循环体：`activation_state.go:107-131`。每 5s（`:28`）在 stale 时只做两件事：
   `activateTopology()`（`:119`，持 `adminTopologyMu`）和 `reloadAdminAuth()`（`:124`）。
   对 `activateRedactionPolicies` / `activateTokenGuardPolicies` 的调用：**零**（grep 全仓核对）。
2. `activateTopology()` 成功即 `markCurrent()`（`providers.go:94`）——redaction/token guard 造成的
   stale 就此被清，那两个域没有被碰过。
3. 循环由 `Open()` 无条件启动：`internal/app/runtime.go:769`。
4. 触发它不需要任何管理员后续操作——比主张一更容易命中，A4 此点属实。

**独立探针 V1-P2 实证**（用 `Open()` 启动的**真实**恢复循环，不是模拟循环体）：
注入 redaction 域 stale 后 ~5.4s，`status()` 变为 `{Stale:false Reason:"" Generation:1}`，
`/health/ready` 200。见 §3 的完整输出。

## 3. 重点：A4 未实证的那一半，本次已实证——策略确实是旧的

A4 的推理依据是"`ReplacePolicies` 全仓各只有一个调用点"。我先独立核对这个前提，再用探针把
推理封死为事实。

**前提核对（grep 全仓，`*_test.go` 除外）**：
- `redactor.ReplacePolicies` 生产调用点仅 `admin_redaction.go:320`（在 `activateRedactionPolicies` 内）；
  引擎内部 `redaction/engine.go:101` 只是构造函数 `New` 的自调用。启动装载在
  `internal/app/redaction.go:15-22`（`loadRedaction`，仅 `Open` 时走一次，`runtime.go:396`）。
- `tokenGuard.ReplacePolicies` 生产调用点仅 `admin_token_guard.go:338`；`tokenguard/manager.go:175`
  是 `New` 的自调用。启动装载在 `internal/app/tokenguard.go:13-21`（`loadTokenGuard`）。
- 其余对 `ListRedactionPolicies` / `ListTokenGuardPolicies` 的调用（`admin_redaction.go:46`、
  `admin_resources.go:221`）只服务 Admin 只读端点，不触引擎。
- **不存在**任何周期任务、project 变更钩子或其他路径会重装这两个引擎。project 改绑
  `RedactionPolicyID` 走 `activateAuthSnapshot`（`admin_projects.go:481-487`），不触 redactor。

**结论**：一旦 `activateRedactionPolicies` / `activateTokenGuardPolicies` 失败，引擎快照回到 current
的路径只有三条——同类策略再保存一次成功、或进程重启、或永远不会。恢复循环不在其中。

**探针 V1-P2 实证**（状态构造的忠实性：`ReplacePolicies` 是整表原子换入——
`redaction/engine.go:157-196` 先建完 `next` 再 `e.policies = next`，失败即整体不换；所以
"commit 成功 + 激活失败"的后置状态 = store 有、引擎无 + `markStale`，与真实失败路径逐字节等价，
`markStale` 的 reason 字符串也按 `admin_redaction.go:324` 原样构造）：

```
V1-P2 data plane while stale: 503
V1-P2 recovery loop cleared=true status={Stale:false Reason:"" Generation:1}   ← ~5.4s，真实循环
V1-P2 store has policy "rp_v1probe" rev=1; engine HasPolicy=false              ← 快照确实是旧的
V1-P2 readiness after clear: 200                                               ← 自称 ready
V1-P2 ProcessText under the missing policy: out="here is SECRET-12345 in a prompt" err=<nil>
V1-P2 tokenGuard.Admit under a missing policy: allowed=true status=normal
```

最后两行是后果的直接测量，见下节。

## 4. 后果严重度：比原判还多一层

原判描述是"旧策略集继续处理流量"。核对数据面消费路径后，后果分两种情形，第二种更重：

1. **已有策略被修改，激活失败**：引擎持旧编译规则继续脱敏/准入。数据面消费点：
   `internal/gateway/inference_resources.go:82,91,106,149,177,199,210,217,233,238` 与
   `inference_resources_store.go`（共 ~20 处 `s.redactor.ProcessText/ProcessJSON`）、
   `internal/gateway/service.go:1837`（`s.tokenGuard.Acquire`）。管理员刚加的规则
   （比如新增一条要挡的 secret pattern）不生效，而运行时对外 current + ready。
2. **新建策略（或项目新绑定的策略）不在引擎里**：查不到即**整策略旁路**——
   - redaction：`redaction/engine.go:428-431`，`policy(policyID)` 未命中时 `return value, nil`，
     原文原样放行；只剩内建 mandatory 规则（`engine.go:417-427`）兜底。
   - token guard：`tokenguard/manager.go:308-312`，未知 policyID 直接
     `Decision{Allowed: true, Status: StatusNormal}`——探针里 2^40 估算 token 也放行。
   两处都是明确写死的 fail-open 方向（对"policyID 为空 = 未配置"是合理默认，但在
   "store 已有、引擎没装上"的状态下就成了整策略旁路）。启动期 `loadRedaction` /
   `loadTokenGuard` 会拒启这种引用悬空（`redaction.go:27-33`、`tokenguard.go:36-45`），
   运行期没有对应守卫。

**什么数据会走出去**：情形 2 下，绑定该策略的项目的全部 inbound prompt / outbound 响应
只过 mandatory 内建规则，操作员配置的 PII/secret 规则一条都不跑；token guard 侧该项目
无任何 TPM/成本/并发准入限制。这正是 `activation_state.go:14-22` 声明该机制要防住的
fail-open 方向，被机制自己的恢复循环在 ≤5.5s 内打开。

**触发前置条件核对**（未被夸大）：需要 redaction/token guard 激活失败恰好一次。两个失败支：
`ListRedactionPolicies(ctx)` 出错（30s `activationTimeout` 到期、关停压力下的 context 取消——
`providers.go:53-57,72-79`），或 `ReplacePolicies` 重编译失败（低概率：写入前
`admin_redaction.go:260` 已 `CompilePolicy` 预检）。概率低但非零——整个 stale 机制存在的
理由就是这类失败会发生；而一旦发生，恢复循环保证在 5s 内把 fail-closed 反转成 fail-open，
且 stale_since 被清后无任何指标/日志残留可归因（A4-12 属实：全仓无 activation 相关指标，
`metrics.go` 零命中）。

## 5. 证伪尝试清单（我找过、且不存在的控制点）

| 假想控制点 | 核对结果 |
|---|---|
| tracker 按域分维度 / 第二个 tracker | 无。`activation_state.go:31-36` 单例单标志 |
| `markCurrent` 看 reason 归属 | 无。`:52-58` 无条件清空 |
| 恢复循环重放全部四个域 | 无。`:107-131` 只有 topology + auth |
| project/其他变更路径顺带重装 redactor/tokenGuard | 无。`ReplacePolicies` 生产调用点各仅一处（§3） |
| 周期性策略 reload 任务 | 无。`runtime.go:743-770` 的后台任务清单里没有 |
| 引擎查不到策略时 fail-closed | 反向：`engine.go:428-431`、`manager.go:308-312` 均 fail-open |
| 运行期悬空引用守卫（类比启动期 loadRedaction 拒启） | 无运行期对应物 |
| readiness / Admin 状态按域报告 | 无。`runtime.go:1493-1498` 只看单标志 |

## 6. 结论与裁决细节

- **主张一（A1-01/A4-01）：CONFIRMED**。代码路径完整，独立探针复现（V1-P1）。
- **主张二（A4-02）：CONFIRMED**，含 A4 未实证的那一半：恢复循环清 stale 后，
  redaction/token guard 快照**确实是旧的**（V1-P2：store rev=1、engine HasPolicy=false、
  ready 200），且无任何其他重装路径。
- 严重度：原判 P1 / 阻塞发布成立。后果一侧原判略有低估——"新建/新绑定策略"情形不是旧规则
  继续跑，而是该策略整体旁路（仅 mandatory 内建规则兜底）+ token guard 无条件放行；
  不足以升 P0（需要一次真实激活失败作前置，且 mandatory 规则仍在），但修复时两个
  lookup-miss 的 fail-open 方向值得一并裁决。
- A4 的修复方向（stale 按域分维度，`markCurrent(domain)` 只清本域；恢复循环重放全部域）
  与代码结构相符，无更简单的既有机制可复用。

仓库工作树核验（全程未改动仓库文件，探针经 `-overlay` 从 scratchpad 挂载）：

```
$ git status --porcelain
?? docs/review/260811/releases.1.0.0/
```

唯一未跟踪项是本轮评审文档目录本身（A1~D2 各角色报告已在其中，先于本任务存在；
本文件按任务要求写入其 `findings/verify/` 子目录）。仓库受跟踪文件零改动。

## 7. 附录

### 7.1 读过的文件

- `internal/app/activation_state.go`（全文）
- `internal/app/providers.go`（:40-140）
- `internal/app/admin_redaction.go`（:46-115、:220、:260、:280-350）
- `internal/app/admin_token_guard.go`（:300-360）
- `internal/app/admin_projects.go`（:470-500、:763-773 activateAuthSnapshot）
- `internal/app/redaction.go`、`internal/app/tokenguard.go`（全文）
- `internal/app/runtime.go`（:396、:585、:743-775、:1240-1280、:1485-1505）
- `internal/redaction/engine.go`（:101、:124-235、:372-455、:505-545）
- `internal/tokenguard/manager.go`（:24-41、:175-210、:294-330）
- `internal/gateway/inference_resources.go`（redactor 消费点）、`internal/gateway/service.go`（:69、:120、:616-660、:1837）
- `internal/store/bolt/store_usage.go`（:101-130 PutRedactionPolicy）
- `internal/domain/redaction.go`（全文）
- `internal/app/commit_protocol_test.go`（全文）、`internal/app/activation_context_test.go`（:1-120）
- `docs/review/260811/releases.1.0.0/role-prompts.md`（§1、§8）、`findings/A4.md`（全文）

### 7.2 走过的调用链

- 主张一：`admin_redaction.go:324 markStale` → `activation_state.go:41-48` →
  `refuseWhileSnapshotsStale`（`:84-99`，挂载 `runtime.go:1249,1272`）/ `ready`（`runtime.go:1493`）
  → 无关变更 `admin_providers.go:131 activateTopologyAfterCommit` → `providers.go:107→87→94 markCurrent`
  → `activation_state.go:52-58` 清空 → 数据面放行。
- 主张二：`runtime.go:769 go runActivationRecovery` → `activation_state.go:114-128`
  （仅 `activateTopology` + `reloadAdminAuth`）→ `providers.go:94 markCurrent` → stale 清空；
  旧快照消费链：`gateway/service.go:1837 tokenGuard.Acquire` / `inference_resources.go:82…
  redactor.ProcessJSON` → `engine.go:428-431` / `manager.go:308-312` lookup-miss fail-open。
- 重装路径穷举：`redactor.ReplacePolicies` ← 仅 `admin_redaction.go:320`；
  `tokenGuard.ReplacePolicies` ← 仅 `admin_token_guard.go:338`；启动装载 ← `runtime.go:396
  loadRedaction` / `loadTokenGuard`。

### 7.3 运行过的命令

```bash
grep -rn "markStale\|markCurrent" internal/ --include='*.go'
grep -rn "ReplacePolicies" internal/ --include='*.go'
grep -rn "runActivationRecovery\|refuseWhileSnapshotsStale\|reloadAdminAuth" internal/ --include='*.go'
grep -rn "activateRedactionPolicies\|activateTokenGuardPolicies\|activateTopologyAfterCommit" internal/app/*.go
grep -rn "ListRedactionPolicies\|ListTokenGuardPolicies" internal/ --include='*.go'
grep -rn "redaction.New\|tokenguard.New" internal/ --include='*.go'
grep -n "activationTimeout" internal/app/*.go

# 探针（scratchpad 挂载，仓库零改动）：
go test -overlay=<scratchpad>/overlay.json ./internal/app/ -count=1 -v -run 'TestV1Probe'
#   TestV1ProbeCrossDomainClear        PASS 0.45s（输出见 §1）
#   TestV1ProbeRecoveryLeavesOldPolicySet PASS 5.43s（输出见 §3）
git status --porcelain   # 输出见 §6
```

### 7.4 执行信息

- 模型：Fable 5（`claude-fable-5`）。
- 全程无拒答、无内容策略拦截。
- 探针为观察型（t.Logf 记录实际行为），PASS 不代表行为正确；裁决依据是输出内容。
