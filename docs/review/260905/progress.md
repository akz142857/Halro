# 整改清单与验收状态

本文件记录 2026-09-05 评审之后的 S5 整改。评审结论、原始复现和严重度仍以
[review-report.md](review-report.md)、[findings.md](findings.md) 与
[adversarial-verdicts.md](adversarial-verdicts.md) 为准；那些文件描述的是整改前基线，
不因本次修复而改写。

当前 18 项确认问题均已实施防御性修复并增加回归覆盖，Go 集成测试、受影响包 race、
vet、前端 typecheck/test/build 以及嵌入 bundle 漂移检查均已通过。这里的“已修复”表示源码和本地
自动化验收完成，不代表真实 Provider、真实云 KMS、生产发布、完整浏览器权限旅程或
24 小时 release soak 已完成。发布前仍须按各 release gate 和下方“仍需外部验收”执行。

## 已实施整改

| ID | 状态 | 修复边界 | 回归证据 |
| --- | --- | --- | --- |
| SEC-01 | 已修复 / 本地验证通过 | File/KMS 更换 DEK 前扫描 failure capture 与 provider object；存在留存密文时在生成或持久化新 DEK 前拒绝，KEK-only rewrap 与既有 operation 恢复不受影响；runbook 同步限制 | `TestFileMasterKeyRotationRefusesRetainedCaptureBeforeMutation`、`TestKMSMasterKeyRotationRefusesRetainedProviderObjectBeforeMutation` |
| SEC-02 | 已修复 / race 通过 | Session refresh 改为 bbolt 条件事务；已注销、代际变化、过期或被并行更新的记录不会由旧 refresh 重建 | `TestSessionRefreshCannotRecreateRevokedSession` 及 session/app race |
| PROV-01 | 已修复 / 本地验证通过 | HTTP 已接受后发生 body 读取、解码或 envelope 校验失败统一标记为 ambiguous；Gateway 不再向第二 target 重复发送，并保守结算 | `TestAcceptedUnaryResponseFailuresAreAmbiguous`、`TestRejectedUnaryResponseIsDefinitive`、`TestAcceptedMalformedResponseIsNotRetriedAndSettlesConservatively` |
| SEC-03 | 已修复 / 本地验证通过 | MFA 管理端点的密码与第二因子失败统一进入共享失败预算及审计；required-policy 特例仍保持原业务结果 | `TestAdminMFAManagementFactorFailuresShareTheCredentialBudget`、既有登录与 MFA guard 覆盖 |
| SEC-04 | 已修复 / 本地验证通过 | failure capture 在读取时强制 TTL；即使物理清扫尚未发生，过期正文也不可读 | `TestExpiredCaptureIsUnreadableBeforePhysicalPurge` |
| SEC-05 | 已修复 / race 通过 | Audit 查询先生成有界磁盘 snapshot，再在锁外回放，避免把全历史留在内存并阻塞 append | `TestReplaySnapshotDoesNotBlockAppend`、Audit round-trip/tamper 覆盖 |
| PROV-02 | 已修复 / 本地验证通过 | SSE decoder 将 bare CR 识别为行终止符，同时正确处理跨 read 的 CRLF，不再等待 EOF 才交付完整事件 | `TestBareCarriageReturnEventIsDeliveredBeforeEOF`、`TestCRLFMaySpanReadsWithoutCreatingAnEmptyEvent` |
| FE-01 | 已修复 / 前端测试通过 | 同 pathname 的 query-only 导航主动广播位置变化；Usage tab/filter/API 参数随 URL、前进后退和刷新同步 | navigation 与 `UsagePage` query-only 回归测试 |
| FE-02 | 已修复 / 前端测试通过 | Admin API client 完整遍历分页结果；Project 编辑器不再只看第一页资源 | API pagination 与第 51 项以后资源选择回归测试 |
| FE-03 | 已修复 / 前端测试通过 | Developer 工作台把幂等 key 保持到逻辑提交完成；响应丢失后的重试复用相同身份 | `DeveloperPage` 响应丢失/重试回归测试 |
| FE-04 | 已修复 / 前端测试通过 | 确认动作 pending 时统一锁定 Escape、关闭按钮、遮罩和取消路径 | `keeps every close path locked while the confirmed action is pending` |
| FE-05 | 已修复 / 前端测试通过 | Project body limit 以 bytes 为单位无损往返，UI 不再把 bytes 反复按 KiB 换算扩大配额 | `ProjectsPage` 500/1023/1025 bytes 保存回归测试 |
| REL-01 | 已修复 / race 通过 | dead-man 先持久化 sequence/outbox 再允许投递；保存失败不发送，慢 probe 与 heartbeat delivery 相互隔离 | `TestDurableEnqueueCommitsBeforeDeliveryAndRollsBackOnSaveFailure`、慢 probe/receiver 测试 |
| REL-02 | 已修复 / 本地验证通过 | `halro stats` 对 queue depth/capacity 保留当前 gauge 值，仅对 counter 求差并处理 reset | `TestStatsWindowKeepsCurrentGaugeLevels` |
| REL-03 | 已修复 / 文档核对通过 | 发布证据文档改为描述当前 v0.x workflow，移除把历史 v1 Environment/审批计划写成现行 gate 的表述 | `docs/verification/release-run-evidence.md` 与当前 workflow 人工核对 |
| REL-04 | 已修复 / 本地验证通过 | 不允许 provenance bounds 的分支改为任一单侧 bound 即拒绝；需要窗口的分支仍要求成对出现 | `TestPriceScheduleTierValidationRejectsIncoherentProvenance` 的单侧组合 |
| SUM-01 | 已修复 / race 通过 | Usage summary 在读取 aggregate 前等待进行中的 checkpoint 结果，避免 drain/commit 窗口短暂漏算 | `TestUsageSummaryWaitsForInFlightCheckpointOutcome` 及 Usage rollup race |
| CFG-01 | 已修复 / 配置测试通过 | 默认配置模板不再把 Normalize 才会填充的值伪装成 YAML merge default；敏感 boolean 保持显式语义 | config package tests 与 `config check` 路径 |

## 本地完成门禁

- changed Go files 已格式化，定向回归测试通过；并发相关包已执行 race。
- `go test -count=1 ./...`、`go vet ./...` 通过。
- `web` 的 typecheck、全部测试和 production build 通过。
- `internal/webui/dist` 已由当前 `web` 源码重建，漂移检查通过。
- 没有执行真实 Provider 付费 smoke，也没有修改生产环境。

## 仍需外部验收

- SEC-01：真实云 KMS/IAM、留存周期结束后的 rotate，以及备份恢复演练。
- PROV-01：在有明确费用预算的真实 Provider 测试账户上确认“已接受但响应损坏”的运营处置；
  本地测试不声称真实上游发生了重复扣费。
- SEC/FE：完整浏览器 MFA、降权、异步资源、读屏、窄屏和 200% 缩放旅程。
- REL：目标发布工作流的真实 run、告警接收端、生产相近负载与 24 小时 release soak。
- 发布：在精确 release candidate 上重新执行发布门禁并保留签名、制品、备份与回滚证据。

上述外部项不是重新打开已修复源码缺陷；它们是本地实现不能替代的发布和环境验收边界。
