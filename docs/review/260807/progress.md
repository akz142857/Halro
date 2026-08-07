# 评审整改进度（260807 轮）

> [260807.md](260807.md) 是一份有日期的发现记录，不改动。本文件是它的**活的对照表**：哪些做了、哪些没做、以及做的过程中改变了对原结论的判断。
>
> 编号沿用 260807.md 第九章的修复清单。最后更新：2026-08-07，P0 批次完成。

## 一句话状态

清单共 32 项：P0 五项、P1 十项、P2 十七项。**P0 五项已全部完成**（`897894e`），P1/P2 未开始。

P0 五项高度同源——全部是"完整性门禁与记账权威的 fail-open"——所以作为一个批次一起做、一起写测试、一起提交。整改过程中另发现一项报告未覆盖的问题（见下）。

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
| P1-7 | step-up 判据改为"发凭据/削弱安全控制/不可逆" | 未开始（**需前端改动**） |
| P1-9 | 锚点三个指标 + 纳入 deadman down 判定 | 未开始 |
| P1-10 | `VerifyAuditAnchors` 序号连续性 + 锚点文件加链 | 未开始 |
| P1-14 | 路由删除错误渲染 + step-up 对话框成功后才关 | 未开始（前端） |
| P1-15 | Usage 页筛选改用记账时区 | 未开始（前端） |

前端三项（P1-7/14/15）攒成一批做，只重建一次 `internal/webui/dist`。

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

### P1-12 未做的那半：溢出预算

"溢出预算随 `maxTracked` 缩放"未做。/64 聚合已经把"零成本灌满跟踪表"这条堵死了；剩下的公平性问题（表被合法流量占满后，新来的合法客户端共享一份预算）需要一个没人论证过的倍数，凭空定一个会把明确的上限换成任意的上限。留给产品决策。

### P2（可排期）

P2-3、P2-16 ~ P2-32，见 [260807.md 第九章](260807.md#九修复清单)。全部未开始。

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
