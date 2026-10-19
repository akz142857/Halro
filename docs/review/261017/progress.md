# 261017 Review 进度

> 最后更新：2026-10-19 14:15 +08

| 阶段 | 状态 | 产出 |
| --- | --- | --- |
| 0 范围冻结 | 完成（19:47） | 冻结 `main` = `2e749db132751c4e1bab947e0af1856be7161039`；`git fetch` 后 `origin/main` 未前进 |
| 1 回退完整性 + 260927 复核 | 完成（19:56） | `revert-integrity.md`、`revalidation-260927.md`：回退干净；R-xxx 10 项仍有效（7 项反向验证）、4 项随 HA 移除、0 项失效 |
| 2 D 段深读 | 完成（20:20） | `findings/V-001`–`V-009`（7 个 P2、2 个 P3）；V-001 初稿为 P1，E04c 与 Collector 层测试推翻"无界增长"后降为 P2 |
| 3 净 diff + 交叉点 | 完成（21:19） | `memory-bounds.md`、`range-map.md`（X10–X19：5 PASS、2 PARTIAL、3 FAIL（X12→V-002、X13→V-001、X17→V-005）） |
| 4 运行时证据 | 完成（19:26） | `runtime-evidence.md`：E01–E08 PASS（E04 只读/慢盘 BLOCKED；E05 第 60 分钟起负载形态因 V-010 改变），E09 BLOCKED；E04c 复现 V-001；E05 发现 V-010 |
| 5 对抗复核与报告 | 完成（19:26） | `adversarial-verdicts.md`、`review-report.md`（NO-GO：无 P0、1 个 P1，阻断来自出口准则 1、5、6）、`range-map.md` |

## 恢复工作所需的现场

- 评审分支 `docs/review-261017-plan`，worktree `scratchpad/wt-review`；
- 冻结 SHA 的 scratch worktree `scratchpad/r261/frozen`，内含一次性测试（`internal/usage/zz_review_flush_growth_test.go`、`internal/ledger/zz_review_snapshot_amplification_test.go`、`cmd/zzflip/`），不进评审分支；
- 候选二进制 `scratchpad/r261/bin/halro-{darwin,linux-arm64}`；v0.8.5 二进制 `scratchpad/rel/v085/`（darwin）、`scratchpad/r261/v085l/`（linux）；
- E01 数据：`scratchpad/r261/e01/d085.pristine`（v0.8.5 写出，未改动）、`up`（升级后）、升级前备份 `pre-upgrade-v085.hmbk`；驱动脚本 `e01/drive.py`（容器用副本在 `r261/cwork/`）；
- E03：Docker volume `r261e03`，容器 `r261-e03`，日志 `r261/e03.log`；
- 主机上的 `langfuse`、`midigi-postgres` 容器属于 owner，不得触碰。

## 更正（2026-10-18）

- V-001 恢复为 P1：#427 评审指出对抗复核只覆盖了高吞吐；按节奏到达的复现证实待写批次无界增长（约 1.24 KB 堆/请求）。
- 新增 V-011（P2，v0.8.5 已存在）：延迟请求放回队列时重复计 Token Guard 用量；由 #428 评审指出，确定性复现确认。
- 阶段 6 的发布前整改由 5 项变为 7 项（加 V-001 增长部分、V-011）。

## 阶段 6：整改

| Finding | 处理 | PR | 状态 |
| --- | --- | --- | --- |
| V-002 | 延迟层把 `accounting_capacity_exhausted` 放回队列 | #428 | 已合并 |
| V-011 | 放回队列时撤回 Token Guard 样本与这次准入造成的 violation（按 violation epoch，只减自己的），并退回 Project TPM | #428 | 已合并；#428 的两轮自动评审各指出一处（重复计 TPM、重叠撤回恢复旧快照），均经测试确认后修复 |
| V-006 | 封存关闭且活动 WAL 超过 256 MiB 时启动告警；CHANGELOG 写明需手动设置 | #429 | 已合并 |
| V-001（增长部分） | 写入失败即置 lagging，CatchUp 先写回持有的批次、仍失败则不再应用 | #430 | 已合并 |
| V-007、V-005（说明）、V-003（文档） | CHANGELOG 采用 #426 的整理与升级说明；operator-guide 与配置说明写明快照约 6× 与 `history.db` 磁盘成本 | 本 PR | 待合并 |

每项代码整改都有"修复前失败、修复后通过"的回归测试（反向验证时确认改动已落地），并在推送前跑过 `make full-check`。随后版本处理：V-001 的停更与可见性部分、V-004、V-005 的新文件检查、V-003 的流式编码、V-010、V-008、V-009。

## E05b / V-012–V-014（2026-10-19）

| Finding | 处理 | PR | 状态 |
| --- | --- | --- | --- |
| V-010 | 半开失败对占用的每个 scope 再观察一次 | #432 | 已合并 |
| V-012 | `Run` 在 `applyMu` 下排空队列，记录不在锁外被持有 | #433 | 已合并 |
| V-013 | 按墙上时钟相对单调时钟的漂移判断跳变；ADR 0012 追加修订 | #434 | 待合并；E05d 确认 |
| V-014 | 需要设计解除动作 | — | 待 owner 决定 |

E05b（8 小时）FAIL 于 V-013；E05d（修复后续跑 2 小时）PASS。原定的 8 小时全新重跑由 owner 改为 E05d：触发条件来自累积的磁盘压力，在已有压力的终态目录上续跑比从零重跑更直接。

