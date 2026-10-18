# V-009 — P3：每次启动全量扫描两遍治理日志，启动时间随 Outcome 数增长

- 基线：冻结 `main` `2e749db1`
- 段/功能：D 段 M04（#422）
- 分类：【建议】，代码证据；非回退（v0.8.5 生产中实际同样全量重放）

`governance.Open`（`internal/governance/log.go:163-175`）扫描整份日志验证，随后 `internal/app/runtime.go:1091-1120` 的 `replay` 再 `Replay` 并 `Apply` 每条记录。#422 删除了治理 checkpoint 读路径（理由：自 #272 起生产从未写过 checkpoint），Outcome 归档只把内存降下来，没有让启动跳过已归档前缀。内存有界重设计让 Ledger 启动与历史长度脱钩（快照），治理日志没有对应机制，也没有测量。建议在 E01 规模之上测一次启动时间随 Outcome 数的变化，决定是否需要治理侧的快照。
