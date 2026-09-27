# Review 整改与复验进度

> 本文件跟踪后续整改，不抹除 `review-report.md` 的初始 No-Go 证据。2026-09-27 多角色实施、主 Reviewer 交叉复核及仓库门禁完成后，R-001–R-014 均在 repository scope 关闭；详细证据见 [`remediation-verification.md`](remediation-verification.md)。

| ID | 严重度 | 仓库状态 | 关键关闭证据 |
| --- | --- | --- | --- |
| R-001 | P0 | FIXED / VERIFIED | `provider_resources.in_flight` 返回前强制 quorum confirmation；store 等待确认和 HA/race 回归通过 |
| R-002 | P1 | FIXED / VERIFIED | Files/Batches/Async/Deferred 统一要求 `inference`；各资源族 403/no Provider I/O 测试通过 |
| R-003 | P1 | FIXED / VERIFIED | planned stepdown 先撤 readiness、`http.Server.Shutdown` drain，再冻结 confirmed prefix；drain 期间 index 前进拒绝旧 proposal |
| R-004 | P1 | FIXED / VERIFIED | session delete、MFA challenge 消耗/删除、TOTP watermark 等一次性状态进入确认边界 |
| R-005 | P2 | FIXED / VERIFIED | Claude JSON expiry 进入 `Credential.ExpiresAt`；RFC3339/epoch、过期及冲突 fail-closed 测试 |
| R-006 | P2 | FIXED / VERIFIED | strict positive schema version、拒绝 symlink/换身份、保留 mode/UID/GID、备份与 rename fsync、失败清理测试 |
| R-007 | P2 | FIXED / VERIFIED | offline clear 以同事务写入 durable `AdminAuditIntent`；审计交付失败保留 pending intent |
| R-008 | P2 | FIXED / VERIFIED | suspension store 读取失败返回 503；Console 保留未知/错误态 |
| R-009 | P2 | FIXED / VERIFIED | Console clear 提供可清除性、确认、step-up、错误保留及成功后 cache invalidation |
| R-010 | P2 | FIXED / VERIFIED | CHANGELOG 补齐；release assessment 要求逐提交 changelog/no-changelog 决策且拒绝未完成项 |
| R-011 | P2 | FIXED / VERIFIED | 8 条缺口告警补 firing/for-window/recovery 规则测试及负对照 |
| R-012 | P2 | FIXED / VERIFIED | seed index 0 仅允许首个 ordered frame 前使用，代码、文档和测试一致 |
| R-013 | P2 | FIXED / VERIFIED | domain 强制 `discovery => inference`；历史 invalid key 保持不可用但允许 tombstone，不静默扩权 |
| R-014 | P2 | FIXED / VERIFIED | 固定 Key/Project ceiling 移到认证之后、scope/CIDR 拒绝之前；403→429 顺序回归通过 |

R-003 的仓库测试证明 shutdown seam、顺序和 stale-prefix fail-closed；真实 Pod/SSE/长连接 drain 仍归入下述目标环境验收，不把模拟器证据冒充集群证据。

目标环境验收仍独立保持 BLOCKED：kind、三节点 partition/fencing、真实 Pod/SSE drain、Linux fault/performance、相邻版本、G0–G7、72 小时 soak、RTO、artifact/provenance/生产同 SHA。未运行 billable Provider smoke。
