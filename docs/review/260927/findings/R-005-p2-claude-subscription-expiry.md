# R-005 — P2 CONFIRMED：Claude subscription JSON 的 expiry 被丢弃

`parseClaudeSubscriptionCredential` 解析并校验 JSON 内的 `expires_at`，其注释明确称该值会映射到 Credential expiry（`internal/app/claude_subscription_credential.go:14-32,45-107`）。但保存路径的 `validateCredentialMaterial` 丢弃解析结果（`internal/app/admin_providers.go:1567-1580`），最终 `Credential.ExpiresAt` 只来自独立的顶层 `input.ExpiresAt`（同文件 `1383-1395`）。

操作员粘贴带过期时间的 OAuth JSON、但没有另填 UI/API 顶层 expiry 时，会得到没有 expiry 的 durable credential；Halro 将继续送往上游，直到上游拒绝。关闭条件是明确一个权威来源、保存解析出的时间并覆盖 RFC3339/毫秒 epoch/已过期/冲突双来源测试。
