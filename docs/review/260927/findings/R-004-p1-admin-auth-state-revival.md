# R-004 — P1 CONFIRMED：Admin session/MFA 一次性状态可在提升后复活

- 基线：`0c4a270d7aae2b3d4562c35cd60ff14907278897`
- 影响：F11、F12、X04、INV-04
- 裁决：CONFIRMED；No-Go 条件

`metadataOpRequiresConfirmation` 的 delete 集合遗漏 `admin_sessions` 与 `admin_mfa_challenges`，而 authenticator put 只有 `status=revoked` 才同步确认（`internal/store/bolt/journal_tx.go:248-312`）。因此以下成功结果只保证本地 durable，不保证已进入可提升的 confirmed 前缀：

- logout/session revoke：`internal/store/bolt/store_admin.go:406-434`、`internal/app/admin_session.go:323-340`；
- MFA challenge claim、完成删除和取消：`internal/store/bolt/store_admin.go:669-715`、`internal/app/admin_mfa.go:258-344`；
- TOTP `LastAcceptedTimeStep` 消费：`internal/store/bolt/store_admin.go:521-549`、`internal/app/admin_mfa.go:366-378`。

在调用方已经收到 logout/cancel/login 成功后立即发生 crash promotion，Replica 可能仍持有旧 session、challenge 或旧 TOTP watermark，从而复活已撤销访问或允许一次性因子重放。状态“被 journal”不能替代“成功响应前 confirmed”。

## 关闭条件

为所有撤权与一次性消费状态建立同步 confirmation 策略，或在 promotion 时以已证明的方式整体失效。双 Runtime 回归测试至少覆盖 logout、challenge cancel/complete 和 TOTP step consumption，成功响应后切主必须拒绝旧凭证、token 和同一步 TOTP。
