# V-007 — P2：`main` 的 CHANGELOG 再次缺项（R-010 同类问题复现）

- 基线：冻结 `main` `2e749db1`
- 段/功能：D、E 段；R-010 复核
- 分类：【问题】

## 现象

R-010 之后，发布流程要求逐提交的 CHANGELOG 处置表，`release.yml` 拒绝缺表或含未决项的评估——这道门在发布时有效。但 `main` 上的 `[Unreleased]` 再次漏掉多项用户/运维可见的变化：Ledger 热状态快照启动（#421）、封存默认开启（#421）、治理归档 `archive.db`（#422）、每小时用量对账（#421）、控制台 alias 分组标题（#353）、Providers 页面的拒绝面板（#330）、Go 1.26.9 工具链（#404 中的 `f018cc8a`）。这些是在 #426 的逐提交表中才补上的。另外，`[Unreleased]` 有 4 个 `Added`、4 个 `Changed`、2 个 `Fixed` 小节，逐 PR 追加而未合并。

另外，`docs/guides/operator-guide.md:562-568` 仍写着"The accounting Ledger's own replayed state still grows with history until the Ledger snapshot lands (… phase P2); size the container against … baseline until then"——P2 已合并，这段容量指导已过期；同一节也没有给出 `history.db` 约 5.75 KB/请求、无保留期的磁盘成本（E01 实测）。

## 影响

在 `main` 上读 CHANGELOG 的人（包括本轮评审之外的任何人）得到的是不完整的变化清单；只有走完发布准备的人才会发现。

## 建议

在 PR 层面（而不只是发布时）检查：改动了 `internal/`、`cmd/`、`configs/`、`deploy/` 的 PR 必须改 CHANGELOG 或在 PR 描述中写明 `no changelog` 及理由。
