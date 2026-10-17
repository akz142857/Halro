# 261017 Review 进度

> 最后更新：2026-10-17 21:03 +08

| 阶段 | 状态 | 产出 |
| --- | --- | --- |
| 0 范围冻结 | 完成（19:47） | 冻结 `main` = `2e749db132751c4e1bab947e0af1856be7161039`；`git fetch` 后 `origin/main` 未前进 |
| 1 回退完整性 + 260927 复核 | 完成（19:56） | `revert-integrity.md`、`revalidation-260927.md`：回退干净；R-xxx 10 项仍有效（7 项反向验证）、4 项随 HA 移除、0 项失效 |
| 2 D 段深读 | 完成（20:20） | `findings/V-001`–`V-009`（1 个 P1、6 个 P2、2 个 P3）；V-001 与 V-003 有一次性测试复现/测量 |
| 3 净 diff + 交叉点 | 完成（21:25） | `memory-bounds.md`、`range-map.md`（X10–X19：6 PASS、2 PARTIAL、3 FAIL 对应 V-001/V-002/V-005） |
| 4 运行时证据 | 进行中 | `runtime-evidence.md`：E01、E02、E07、E08 完成（PASS），E09 BLOCKED；E03 PASS（11 轮）；E04 运行中；E06 待跑；**E05（4 小时长跑）开始前须先向协调者报告，等待主机空闲确认** |
| 5 对抗复核与报告 | 未开始 | `adversarial-verdicts.md`、`review-report.md`、`range-map.md` |

## 恢复工作所需的现场

- 评审分支 `docs/review-261017-plan`，worktree `scratchpad/wt-review`；
- 冻结 SHA 的 scratch worktree `scratchpad/r261/frozen`，内含一次性测试（`internal/usage/zz_review_flush_growth_test.go`、`internal/ledger/zz_review_snapshot_amplification_test.go`、`cmd/zzflip/`），不进评审分支；
- 候选二进制 `scratchpad/r261/bin/halro-{darwin,linux-arm64}`；v0.8.5 二进制 `scratchpad/rel/v085/`（darwin）、`scratchpad/r261/v085l/`（linux）；
- E01 数据：`scratchpad/r261/e01/d085.pristine`（v0.8.5 写出，未改动）、`up`（升级后）、升级前备份 `pre-upgrade-v085.hmbk`；驱动脚本 `e01/drive.py`（容器用副本在 `r261/cwork/`）；
- E03：Docker volume `r261e03`，容器 `r261-e03`，日志 `r261/e03.log`；
- 主机上的 `langfuse`、`midigi-postgres` 容器属于 owner，不得触碰。
