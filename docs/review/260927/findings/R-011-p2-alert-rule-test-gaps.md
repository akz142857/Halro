# R-011 — P2 CONFIRMED：8 条新增告警没有正向 firing 语义测试

以下告警存在于 `deploy/observability/prometheus/alert-rules.yml`，但没有出现在 `rule-tests.yml` 的 `alert_rule_test` 中：

- `HalroAdminAuditBacklogStuck`
- `HalroAdminAuditBacklogUnreadable`
- `HalroAdminAuditDeliveryFailing`
- `HalroCredentialUnusable`
- `HalroProviderQuotaExhausted`
- `HalroProjectDailyBudgetRefusingTraffic`
- `HalroRunBudgetRefusingTraffic`
- `HalroProviderSpendUnreported`

Prometheus 配置、规则语法和已有 rule tests 均通过，但不能证明这些表达式在预期输入下会 firing、保持 `for` 窗口并恢复。关闭条件是每条至少有正向 firing 和恢复用例，对安全/账务类告警补充必要的负向控制。
