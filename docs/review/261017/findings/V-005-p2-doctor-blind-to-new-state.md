# V-005 — P2：`halro doctor` 在 v0.8.5 目录首次启动前报不健康，且不检查任何新文件

- 基线：冻结 `main` `2e749db1`
- 段/功能：D 段 M01–M05 × 既有诊断；交叉点 X17
- 分类：【问题】，运行时证据（E01）+ 代码证据

## 现象一：升级前的 doctor 失败

> 对抗复核更正（2026-10-18）：这一行为**不是本范围引入的**——v0.8.5 已是同样的精确检查，历次版本都在 CHANGELOG 里说明它表示"还没升级"。本范围的缺口是 `main` 的 `[Unreleased]` 没写 38→41 的这句说明（V-007 一类）。下面的描述保留为改进建议。见 `../adversarial-verdicts.md`。

`doctor` 用 `boltstore.OpenReadOnly`（`internal/app/doctor.go:129`）打开元数据；`OpenReadOnly`（`internal/store/bolt/store.go:1545-1561`）要求 schema **精确等于** 41，否则报 `metadata schema version 38 does not match required version 41`，`doctor` 输出 `healthy: false`。只读诊断不能迁移是对的，但"比本二进制旧、`start` 会迁移"与"比本二进制新、无法读取"被报成同一个失败。运维按常规"升级前先用新二进制跑 doctor"，会看到一个看起来像数据损坏的失败。CLAUDE.md 专门提醒过精确相等的版本检查。

## 现象二：新文件不在任何离线检查中

`doctor` 的检查项与 v0.8.5 完全相同（25 项，逐项对比 `add("...")`）；`ledger verify`、`usage verify`、`audit verify` 也都不读 `data/history/history.db`、`data/history/archive.db` 或 `state.snap`。`doctor` 也不提示 `ledger.seal.enabled: false`、不报告热状态估算。

## 建议

- 只读打开时区分"旧 schema、待迁移"（warn，并说明 `start` 会迁移）与"新 schema"（fail）；
- 为三个新文件各加一项检查（能否以当前密钥打开、记录的位置/链头是否落在 Ledger 内），并在 `seal.enabled: false` 时给出 warn。
