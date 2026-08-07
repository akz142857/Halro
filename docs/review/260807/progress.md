# 评审整改进度（260807 轮）

> [260807.md](260807.md) 是一份有日期的发现记录，不改动。本文件是它的**活的对照表**：哪些做了、哪些没做、以及做的过程中改变了对原结论的判断。
>
> 编号沿用 260807.md 第九章的修复清单。最后更新：2026-08-07，P1 后端批次完成 + 清单外的回归修复与 1.0.0 债务清理。

## 一句话状态

清单共 32 项，**全部结项**（30 项完成 + 2 项经论证后不做）（逐条到代码里核实过，非仅凭本文件记录）：

| 档位 | 完成 / 总数 | 提交 |
|---|---|---|
| P0 | **5 / 5** | `897894e` |
| P1 | **10 / 10** | `8e373fb`、`e41446a`、`9c038b5`、`4bb3fc2` |
| P2 | **17 / 17** | `093ff1a`、`70d107d`、`c9528ab`、`18ff5d5`、`863adc2`、`97befaf`、`25f3939`、`9b31e62`、`cd1f32e` |

P0、P1、P2 三档全部完成。

P0 五项高度同源——全部是"完整性门禁与记账权威的 fail-open"——所以作为一个批次一起做、一起写测试、一起提交。

**清单之外另做了三批工作**（见文末"清单外的改动"）：整改过程中引入的一处回归及其同类、合并进来的提交带的同类回归，以及按 1.0.0 债务边界做的两次收敛。这些不在评审清单里，因为它们是整改本身产生的，或是评审之后才确立的策略。

## 验证流程（沿用上一轮，不是可选的）

每项按同一套流程验证：写测试 → **把代码改回缺陷状态确认测试真的失败** → 恢复 → 目标包 race 检测 → 全仓无缓存套件。反向验证是必需步骤——它是唯一能证明"测试守护的是真问题"的手段。前端项另加 `make frontend` 重建 + `npm run typecheck`（即 `tsc -b`，**不是** `npx tsc --noEmit`，后者不检查测试文件）+ 全量 vitest。

## 待办清单

### P0（发布前必须修）— 全部完成，提交 `897894e`

| 编号 | 内容 | 状态 |
|---|---|---|
| P0-1 | chain checkpoint 门禁无条件加载 checkpoint；`VerifyLedger` 同样处理且 CLI 返回非零退出码 | **完成** |
| P0-2 | `scan` 内维护 epoch 单调性，封死降级与追加伪造 | **完成** |
| P0-4 | 请求 run 关闭时兜底 finalize（**修法与清单不同，见下**） | **完成** |
| P0-5 | `Replay` 区分错误类型 | **完成**（`WithoutCancel` 部分见下） |
| P0-6 | `restoreUsageAggregate` 拒绝 watermark 超过 WAL 头的 checkpoint | **完成** |

新增测试，每条都做了反向验证（把修复改回缺陷状态、确认测试以描述的症状失败、再恢复）：

| 测试 | 缺陷态下的失败症状 |
|---|---|
| `ledger/downgrade_test.go` · `TestEpochDowngradeIsRejected` | `err=<nil>`（降级后的文件被当成合法旧账本接受） |
| `ledger/downgrade_test.go` · `TestForgedLegacyFrameAppendedAfterChainIsRejected` | `err=<nil>`（追加的伪造帧连 `chainVerified=false` 都不触发） |
| `app/ledger_chain_checkpoint_test.go` · `TestDeletedLedgerIsRejectedByChainCheckpoint` | 删光 WAL 后启动成功 |
| `app/ledger_chain_checkpoint_test.go` · `TestUsageCheckpointAheadOfLedgerHeadIsDiscarded` | `watermark={Offset:998 Sequence:2}`（checkpoint 指向已消失的字节） |
| `ledger/replay_cancel_test.go` · `TestCanceledVisitDoesNotCondemnTheLedger` | `state=3`（`AccountingRecoveryRequired`） |
| `ledger/replay_cancel_test.go` · `TestCorruptScanStillCondemnsTheLedger` | 两态下都通过——它钉的是"不许放宽"，不是新行为 |
| `gateway/budget_exhausted_leak_test.go` · Chat / Embeddings 两条 | `8 of 8`、`5 of 5` 全部滞留在途 |

验证：`go build ./...`、`go vet ./...`、`go test ./...` 全绿；`go test -race` 覆盖 ledger/gateway/app/budget/usage 五包全绿。

### P0-4 的修法与清单不同（重要）

清单原文是"让 `startAttempt` 返回软错误前自己 finalize"。**这个修法是错的**，实现时才发现：循环路径（Chat/ChatStream/Embeddings）会对多个候选 target 反复调 `startAttempt`，若它自己 finalize，第一个 target 软失败就把整个请求结账掉，后续 target 无请求可用。

实际做法是把不变式挪到请求 run 的生命周期上：`requestRun` 记一个 `finalized` 标志，新增 `run.finalize(outcome)` 做一次性收口，所有属于该 run 的 finalize 调用点（14 处）统一改走它，`run.close()` 在没人 finalize 过时兜底。这样"run 结束了"才是关闭记账的那件事，而不是每个 return 语句各自记得——循环路径与单发路径一并修好。

`s.cleanup`（`service.go:1722`）在排查中确认为**全仓零调用点的死代码**，未动，留给 P2 清理。

### P0-5 的范围

`Replay` 的错误分类已完成。清单里同项的第二半——`syncUsageAdmin` 改用 `context.WithoutCancel` + 独立超时——**未做**：`Replay` 不再毒化状态后，请求取消已经不产生持久后果，`WithoutCancel` 从"必须"降为纵深防御。连同对抗验证指出的 `applyMu.Lock()` 不感知 ctx（并发 admin 请求时后到者会阻塞整个 leader 的 replay 时长），一起移到 P1 处理。

## 整改中发现的、报告未覆盖的问题

**1. `internal/ledger/event.go` 在 `main` 上就未通过 `gofmt`**（已修，`dca8bee`）。退役提交 `8868e85` 删掉 `EventCostAdjusted` 后，`Event` 结构体最宽的字段没了，剩下的 tag 全部过对齐到一个不再存在的列。

**2.（未修，建议纳入 P2）CI 和 `make check` 都没有 gofmt 门禁。** 上一条能在 `main` 上存活正是因为这个——CLAUDE.md 写了"run gofmt on changed files"，但没有任何东西强制。`make check` 现在跑 test/race/vet/frontend-test/observability-check，缺一条 `gofmt -l` 非空即失败。成本一行。

### P1（发布前应修）— 五项完成，五项待办

| 编号 | 内容 | 状态 |
|---|---|---|
| P1-6 | 五处直接口令校验接入失败预算 + 失败审计（包装器方案） | **完成** `e41446a`（并发信号量部分未做，见下） |
| P1-8 | MFA 登录失败审计 | **完成** `e41446a`（**原结论一半被证伪，见下**） |
| P1-11 | chain checkpoint 周期推进 + 关闭时推进 | **完成** `8e373fb` |
| P1-12 | `sourcelimit` IPv6 按 /64 聚合 | **完成** `8e373fb`（溢出预算缩放未做，见下） |
| P1-13 | 信封缺失且已轮换时 fail-closed | **完成** `8e373fb` |
| P1-7 | step-up 判据改为"发凭据/削弱安全控制/不可逆" | **完成** `4bb3fc2`（覆盖铸密钥与解除封锁；改凭据/改脱敏策略未做，见下） |
| P1-9 | 锚点三个指标 + deadman 侧记录并公告拉取结果 | **完成** `9c038b5` |
| P1-10 | `VerifyAuditAnchors` 序号连续性 + 未见证尾巴 | **完成** `9c038b5`（锚点文件加链未做，见下） |
| P1-14 | 路由删除错误渲染 + step-up 对话框成功后才关 | **完成** `4bb3fc2` |
| P1-15 | Usage 页筛选改用记账时区 | **完成** `4bb3fc2` |

### P1-7 的覆盖范围（做了两处，留了两处）

判据已从"是不是 DELETE"改成"是不是发凭据 / 是不是关掉一个正在生效的保护"，并落到两个端点：

- `POST /projects/{id}/keys`（含工作台的调试 Key——它就是一把真的网关密钥）。凭据挂在请求体上而不是走共享中间件，因为该 handler 自己解 body，而 body 只能读一次。
- `POST /projects/{id}/unblock`。

**`PUT /credentials/{id}` 和 `PUT /redaction-policies/{id}` 未做。** 它们是表单保存流，不是确认对话框，前端要在表单里加凭据字段并改提交路径；后端单独加会直接让控制台的保存开始返回 401，所以没有只改一半。留作后续，判据本身已经确立。

### P1-14 的实现

`ConfirmButton` 原来先关对话框再执行动作，所以 step-up 被拒时对话框已经没了、凭据也清了；而 `RoutesPage` 是**唯一**不渲染删除错误的资源页，两者叠加就是彻底静默。现在对话框等待动作结果，失败时保持打开并就地显示原因，**保留密码但清空验证码**——TOTP 一步只能用一次，留着已用的码会让第二次拒绝看起来像密码错。13 处调用点从 `mutate` 改为 `mutateAsync`。`RoutesPage` 补上了错误渲染。

新增测试（同样全部反向验证）：

| 测试 | 缺陷态下的失败症状 |
|---|---|
| `admin_credential_guard_test.go` · 口令改动限流 | 第 6 次仍 401（预算用完不生效） |
| `admin_credential_guard_test.go` · 与 step-up 共用预算 | 删除返回 204（两套独立计数器） |
| `admin_mfa_guard_test.go` · MFA 失败审计 | `failures=0`（第二因子猜测零留痕） |
| `sourcelimit/aggregation_test.go` · 三条 | 同 /64 拿到新预算；`overflows=24`（一个 /64 灌满表）；mapped 形式另算一份 |
| `app/ledger_chain_advance_test.go` · 两条 | 启动后写入的帧未被 checkpoint 覆盖；轮换后仍重新派生 |

### P1-8 的原结论一半被证伪（整改过程中发现）

安全角色断言"MFA 无账号级限速，因为 challenge 可无限申请，每 challenge 5 次不构成上限"。**这一半不成立。** `internal/store/bolt/store_admin.go:432-435` 的 `PutAdminMFAChallenge` 在为同一 user+session generation 重发 challenge 时会**把 `AttemptsRemaining` 结转过去**：

```go
if value.CreatedAt.Before(existing.ExpiresAt) && existing.AttemptsRemaining < value.AttemptsRemaining {
    value.AttemptsRemaining = existing.AttemptsRemaining
}
```

所以重新申请 challenge 买不到新的尝试次数，事实上已经是账号级预算。攻击者只能等旧 challenge 过期才拿到新的 5 次，是低速率上限而非无上限。这条是写测试时发现的——测试怎么也测不出 429，追下去才看到第 6 次挂在 `ClaimAdminMFAChallenge: record not found`，走不到新加的守卫。

**真正的缺口只有审计**：口令失败从一开始就留痕，而"口令已泄露后唯一还挡着的那个因子"猜测零留痕。已修。结转行为原本没有任何测试守护，一并钉住。

### P1-6 未做的那半：argon2 并发信号量

对抗验证建议的第二件事——在 `internal/adminauth` 给 argon2 加并发信号量（照抄 `runtime.go:548` 的 `metricsScrapes` 模式）——**未做**。它和限流是两个问题：限流挡的是猜测速率，信号量挡的是内存放大（实测单次 64 MiB 无复用，`deploy/kubernetes` 的 512Mi limit 意味着 8 并发即 OOMKill）。信号量需要定容量和排队超时两个数，且要覆盖未认证就能触达的 `loginAdmin`/`DummyVerify`，改动面比包装器大。移到 P2-32 一起处理。

### P1-9/P1-10 的实现取舍

**"纳入 deadman down 判定"改成了"记录 + 公告"。** 直接把锚点失败并进探测的 success 判定会混淆两件事——探测答的是"实例还活着吗"，锚点答的是"见证还在吗"，一个实例完全健康而见证挂掉是真实且需要区分的状态。改为每 tick 写一条 `deadman.anchor` 审计记录，并在状态翻转时入 outbox 通知接收端。告警接在 `heimdall_audit_anchor_last_emit_timestamp_seconds` 的陈旧度上，而不是失败计数——**发射彻底停掉时失败计数一次都不涨**。

新增的三个指标只在 `Audit.Anchor.Enabled` 时渲染，避免给没开这个功能的实例增加恒零的时间序列。

**锚点文件本身加 HMAC/hash 链未做。** 安全 B 指出证人文件是明文 JSON-lines、无完整性，能编辑它的人可以删掉不利的行。序号连续性检查已经把"删行"变成可检测的（缺号会报 `missing`），这是低成本的那一半。给文件本身加链需要在 deadman 侧引入一把密钥并决定它的托管方式——而 deadman 的整个立论是"它在 Heimdall 的失效域之外"，密钥放哪是个设计决策不是实现细节。留给 ADR 0015 的后续修订。

### P1-12 未做的那半：溢出预算

"溢出预算随 `maxTracked` 缩放"未做。/64 聚合已经把"零成本灌满跟踪表"这条堵死了；剩下的公平性问题（表被合法流量占满后，新来的合法客户端共享一份预算）需要一个没人论证过的倍数，凭空定一个会把明确的上限换成任意的上限。留给产品决策。

### P2（可排期）— 全部完成

| 编号 | 内容 | 状态 |
|---|---|---|
| P2-3 | 保留退役 event kind 占位 | **不做**（见下） |
| P2-16 | `doctor` 做链校验并对账 checkpoint | **完成** `093ff1a` |
| P2-17 | `openUsageOffline` 改只读 | **完成** `093ff1a` |
| P2-19 | ADR 0013 状态改 Superseded | **完成** `c9528ab` |
| P2-22 | `allowAdminRate` 换有界策略 | **完成** `18ff5d5` |
| P2-23 | 源限流默认开启 | **完成** `70d107d` |
| P2-24 | checkpoint 携带去重窗口 | **完成** `093ff1a` |
| P2-25 | ADR 0016 的信任边界摘进威胁模型 | **完成** `c9528ab` |
| P2-26 | Workbench 不再转发调用方 XFF | **完成** `093ff1a` |
| P2-31 | 恢复复位路径 | **完成** `c9528ab`（结论与清单不同，见下） |
| P2-32 | k8s 内存与 argon2 的关系 | **完成** `c9528ab` |
| P2-18 | `internal/app` 规模闸门 | **完成** `18ff5d5` |
| P2-21 | 锚点凭据独立 + 强制 TLS | **完成** `18ff5d5`（`heimdall audit anchor rotate` 与 deadman 独立 token 文件未做，见下） |
| P2-27 | 补测试（五处） | **完成** `97befaf`（另三处在 P1 期间已补） |
| P2-28 | 测试基础设施 | **完成** `863adc2`（fuzz 语料入库未做，见下） |
| P2-20 | `metricsauth` 改名为 `bearercred` | **完成** `25f3939` |
| P2-29 | 视觉六项 | **完成** `9b31e62` |
| P2-30 | 交互四项 | **完成** `cd1f32e` |

另完成一项清单外的：`make check` 加 gofmt 门禁（`c9528ab`），补上前面记录的第 2 条发现。

**P2-3 不做。** 它要求保留 `EventCostAdjusted = 6` 作为"已退役但可读"的占位——正是 `CLAUDE.md` 现在明令禁止的"错的构件与替代品并存"。清单里同项的另外两半（钉住 kind 编号不可复用的契约、删掉退役后变成死代码的子句）仍成立，前者已写进 ADR 0013 的退役说明。

**P2-31 的结论与清单不同。** 清单要求"给 `MarkHealthyAfterVerifiedRecovery` 补一条经审计的运维复位路径"。查下来它零调用者的原因是**重启本身就是复位路径**：新进程建新 `Status` 并重新扫描 WAL、重校每帧 CRC、对着 checkpoint 重新认证链——状态回到健康是因为日志被重新检查过。补一个"清标志位"的端点反而是真正的 fail-open（清了标志却没重新建立任何保证）。所以删掉死函数，把这个事实写进 `RequireRecovery` 的注释。

**P2-28 期间发现 CI 有个 fuzz 目标一周没跑过。** `go test -fuzz` 在模式匹配不到任何目标时打印 `no fuzz tests to fuzz` 并**退出 0**，所以 `956c06b` 把 phase2 解码器改名后，CI 照旧列着旧名字、照旧显示绿色、而那个目标再没被 fuzz 过。已改名修正，补上从未在列表里的 `FuzzRuleCompilerNeverPanics`，并加了一道"列表里的名字必须存在对应函数"的自检——下次改名会让构建红而不是悄悄降低覆盖。两个此前没跑过的目标现在都干净（120 万 / 640 万次执行无崩溃）。

`-shuffle=on` 已进 `make test`（三个种子都干净），新增 `make cover` 报总语句覆盖率，当前 **61.1%**——这是仓库第一次有这个数字。**fuzz 失败语料自动回灌未做**：CI 现在会把新语料内联打印到日志（避免 artifact 过期后崩溃输入丢失），但把它提交成回归种子仍是手工一步。

**P2-21 做了配置校验两条，工具链两条未做。** 拒绝与 metrics 共用 `credential_file`、以及要求 `metrics.tls.enabled`，都已落到 `Validate`。清单里另外两条——补 `heimdall audit anchor rotate` 子命令、deadman 侧独立的 `anchor_bearer_token_file`——未做：现在配置**强制**两份凭据文件，但生成/轮换第二份仍得手工照着 metrics 的做，deadman 也仍共用一个 token 文件。也就是说约束已经立起来，配套的便利还没有；运维当下能做但不顺手。

**整改中发现并修掉的一处测试脆弱性。** step-up 的失败预算读的是硬编码 `time.Now()`，而旁边的登录限流用的是可注入的 `r.clockNow()`——`runtime.go` 的注释说明登录限流当初正是因为这个问题才改成可注入的，step-up 漏了。后果是预算类测试单独跑必过、整包跑约每次一挂（取决于 5 次尝试是否跨过整分钟）。这轮全量门禁挂了一次才暴露出来。已改用可注入时钟并在测试里固定，连跑三遍全包确认。

**P2-16 只做了 doctor 一半。** 清单还提到"不可用时报 unverified 而非 pass"——已做。三态（authenticated / unverified / failed）已用真实数据副本验证：现有账本报 `unverified`（15 帧全 checksum-only，原来报 pass），翻转一个 payload 字节后报 `fail` 且 `healthy: False`。

## 对抗验证对原结论的修正（开工前必读）

本轮五条最严重发现全部经过对抗验证，**无一条原样成立**。动手前请先读 [260807.md 第十章](260807.md#十对抗验证)，尤其这四处：

1. **V-1 比原报告更严重**：全量 epoch 降级**不依赖 checkpoint 陈旧**（用刚推进过的新鲜 checkpoint 实测仍放行）。另外两条原报告没提到——追加伪造帧连 `chainVerified=false` 都不触发；降级后重启两次会把 checkpoint 推过伪造区，**证据不可逆销毁**。
2. **V-2 紧急程度被高估约两档**：仓库从未发布过任何 GitHub Release，功能存在窗口 69 小时，现实受害者集合几乎确定为空；且**这不是数据丢失**（scan 报错先于 `Truncate`，回退旧二进制即可启动）。已从 P0 降为 P2-3，修的理由是钉住契约不是救实例。
3. **V-3 归因错了**：BUG #2 的来源是 `a58423f` 不是 `8868e85`。finding #3 也削弱——循环路径只有 `ErrExceeded` 那条泄漏，其余三条会 break 到底部 finalize。
4. **V-4 的修法建议被推翻**：见上方 P1-6 注意事项。另外"read_only 引入了这个暴露面"的归因不成立（同一路径对 administrator 一样敞开且早于角色特性存在），`read_only` 的产品定位是"全可见审计员"不是低信任沙箱。

## 已复核的一项（2026-08-07 复核完毕）

**视觉 ratchet 基线**（260807.md 第六章问题 1）：用 `design-system.test.ts:150-154` 自身的正则、一字未改跑三个版本，数字全部证实：

| 版本 | 实际计数 |
|---|---|
| `126fb32`（上轮基线） | 748 |
| `56ef5a0`（棘轮引入提交） | **754** |
| HEAD | 758 |
| 测试写死的基线 | 758 |

**但归因要改，视觉角色的因果推断被时间序推翻。** `f7aa889`（08-06 07:59）加 `FirstRunChecklist` 使裸值上涨在先，`56ef5a0`（08-06 09:15）引入棘轮在后——回归发生在棘轮出生前 1 小时 15 分，棘轮作者继承的是 754，不存在"为了让回归通过而调高基线"。真实问题是**棘轮出生时基线就写宽了 4 格**（实际 754 / 写成 758），而这 4 格已被后续提交吃光：HEAD 恰好 758，余量为零。

净效果与原结论一致（基线偏高、`FirstRunChecklist` 的裸值被就地合法化），修法不变：基线降到 748 + 把 `FirstRunChecklist` 那段 CSS 的裸值改用 token。已并入 P2-29，无需单列。


## 清单外的改动

评审清单只覆盖报告里的发现。下面这些是整改过程中产生或暴露的，同样已完成。

### 1. 两处"拒绝了自己本该保护的历史"的回归（`8b2f8e7`）

**P0-2 的第一版实现是错的，我自己引入的回归。** 它假设"写入方只会把 frame epoch 往前推"，从没打开过真实账本验证。事实是 ADR 0016 之前 epoch 是**按事件形状**选的：带 lease mode 的记账事件写 epoch 2，旁边的普通事件写 epoch 1，真实旧账本通篇交错。这条检查让真实实例**启动即失败**（`frame epoch 1 follows epoch 2`），会打死每一个已有安装。测试之所以通过，是因为混合 epoch 的 fixture 是按升序手搭的——测试验证的是我的心智模型，不是现实。

收窄成设计上真正成立的不变式：**一旦出现过认证帧（epoch 4），之后不许出现未认证帧**。epoch 1-3 之间随便交错，它们本来就没被认证过。三种攻击仍全部拦住（后缀降级、全量降级走 checkpoint 门禁、追加伪造帧）。新增 `TestInterleavedLegacyEpochsStillReplay` 钉住真实旧账本的形状。

**合并进来的 `1b0ed08` 带了同类回归。** Parquet schema 从 3 提到 4 后，行级检查要求每行**精确等于** 4，而分区发布后永不重写。用"构建合并前二进制、对同一个真实数据目录跑"隔离确认：合并前 parquet pass、合并后 fail。这不只是 doctor 显示问题——`backup.go:417`/`:603` 都带 snapshot 调 `Verify`，**真实安装的备份和恢复会直接失败**（已实测 `backup create` 修复前后）。行级检查改为接受可读区间；比对时把重新生成的行归一化到磁盘行的 schema（`narrowToSchema`），因为分区是冻结的历史，拿后加的列去比必然不等。

### 2. 1.0.0 债务收敛（`9d148eb`、`d472d3b`）

按维护者确立的边界——**不许错的构件和对的构件并存**——清掉 cost adjustments 退役后的残留：

- `AttemptEvent` 的 `CostMicrosUSD`/`OriginalCostMicrosUSD`/`FinalCostMicrosUSD` 三个字段装同一个值 → 只留一个；`Bucket`/`RequestSummary`/`totals` 各去掉一个恒等字段，连带三处重复的 `addInt64`；`SettledAttempt` 的 `Base`/`Net` 两个外部零读取的字段 → `CostMicrosUSD`；`originalAttemptCost` 与 `KnownCostMicrosUSD` 已逐字同义 → 删一个。checkpoint 版本 5→6，旧 checkpoint 拒绝并从账本重建（实测启动日志确认）。
- 删掉 `parquetAttemptV2` 整条双读路径（结构体 + 解码分支 + `verifyRowsV2`），`parquetSchemaMinReadable` 提到 3。**不需要重新初始化**——实测现有分区是 schema 3，落在保留的区间内。

`parquet` 的可读区间机制、frame epoch 阶梯予以保留：它们是为 1.0.0 之后演进而设计的能力，不是债，拆它们是另一个设计决策。

### 3. 把两条规则写进仓库（`d8e6157`）

"不许假设、必须验证真实产物"和"1.0.0 前不积累兼容"原本只在机器本地的记忆里，换台机器或换个协作者就没了。写进了 `CLAUDE.md`（两个新小节）、`CONTRIBUTING.md`、`.github/copilot-instructions.md`。

顺带查出**三处自相矛盾**：`CLAUDE.md`、`CONTRIBUTING.md`、`.github/copilot-instructions.md` 原本都要求"keep durable event schemas backward compatible"，与新策略正相反，已一并修正。`docs/review/` 下的历史存档未动。


## 收尾（2026-08-07）

三档全部结项。32 项里 30 项完成，2 项经论证后不做（P2-3 的占位、P2-19 拆包已在上一轮关闭）。

整改期间另做了三批清单外的工作，见上方"清单外的改动"；另修掉四处清单没有、但在整改过程中被实际触发或实测发现的问题：

1. **epoch 单调性回归**（我自己引入，打死了真实实例）——见"清单外的改动"第 1 节。
2. **合并提交带的 parquet schema 回归**——真实安装备份与恢复会失败，同上。
3. **CI 有个 fuzz 目标一周没跑过**——`go test -fuzz` 匹配不到目标时退出 0，改名后 CI 静默失效。已加自检。
4. **step-up 预算读硬编码时钟**——预算类测试单独跑必过、整包跑约每次一挂。改用可注入时钟。

最后一项尤其值得记：它不是被评审发现的，是被"全量门禁挂了一次"发现的，而正确的反应是追根因而不是重跑。

### 视觉部分的最终数字

裸尺寸值回到 **748**，与上一轮基线 `126fb32` 持平，ratchet 基线同步降到 748（此前写宽了 4 格，且已被后续提交吃光）。现在任何新裸值都会让测试红。
