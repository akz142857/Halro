# R-012 — P2 CONFIRMED：seed 的 non-zero index 声明与实现不符

HA repository gate 与 runbook 声称 seed 需要 non-zero approved index（`docs/verification/ha-repository-gates.md:19`、`docs/runbooks/ha-operations.md:57-61`）。`CreateSeedManifest` 只检查 `durable == confirmed == applied`，没有 `>0` 要求；manifest validator、seed state 和 journal open 也接受 index 0（`internal/app/cluster_seed.go:49-88,317-390`、`internal/replication/journal.go:78-87`）。当前测试只覆盖 index 1。

index 0 可能是首次 seed 为打破 bootstrap 循环所需的合法例外，因此本项是契约/门禁矛盾，而不是已证明的数据损坏。关闭条件是明确例外并测试 index 0，或实现拒绝并证明首次集群建立无循环依赖。
