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

### P1（发布前应修）

P1-6 ~ P1-15，见 [260807.md 第九章](260807.md#九修复清单)。全部未开始。

**注意 P1-6 的修法已被对抗验证重写**——不要直接改走 `verifyAdminStepUp`，那会造成 4 处语义回归（`disableAdminMFA` 的恢复码分支会直接破功能）。正确做法是抽包装器 + 加并发信号量两件独立的事。

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
