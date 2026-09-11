# v0.8.0 评审整改记录

日期：2026-09-11
原始评审候选：`main@222d08f84f61493fc9a273d351cc728528d6e30c`
整改对象：上述候选之上、包含本文件的整改候选提交
当前状态：**整改代码与本机全量门禁已完成；精确 SHA 的 CI 与发布彩排待完成**

本文件记录 [`findings.md`](findings.md) 中确认问题与候选问题的处置，不替代发布责任人的
GO / NO-GO 决定。包含本文件的整改提交仍须取得精确 SHA 的 CI、发布彩排或签名证据，因此即使
本机回归通过，也不能直接发布。

## 1. 确认问题整改矩阵

| ID | 状态 | 处置 | 主要回归证据 |
| --- | --- | --- | --- |
| F-001 | FIXED LOCALLY | bbolt schema `36 → 37` 建立旧二进制回滚闸；Usage checkpoint `13 → 14`；迁移删除旧派生 checkpoint 并从认证 Ledger 重放；Runtime 构造期即验证现有 Parquet manifest | `TestMigration37FencesOldReadersAndDropsTheUsageCheckpoint`、`TestExporterRefusesANewerManifestDuringConstruction`、checkpoint/Runtime 回归 |
| F-002 | FIXED LOCALLY | pending lease 恢复复制冻结的 Offering/Profile/Region；Ledger 拒绝 settlement 改写 Provider 归因；恢复结果贯穿 Usage、checkpoint、Parquet | `TestRecoverPendingLeasePreservesFrozenProviderAttribution`、`TestRecoveredLeaseAttributionSurvivesUsageCheckpointAndParquet` |
| F-003 | FIXED LOCALLY | failure capture 不再持久化任何上游 prose；render 失败只保存安全 shape；Gateway 错误链不再 unwrap 上游错误 | 真实 `StaticHeaderAuthorizer` + 本地回显上游 canary 回归覆盖 capture/Admin/HTTP/log/audit/disk |
| F-004 | FIXED LOCALLY | Credential 与 Provider 分别持久化精确 Offering/Surface/Region/revision/assurance 确认；loader 在构造 adapter 前对缺失/过期确认 fail closed，并返回稳定原因码；旧记录不迁移伪造同意 | 创建、换绑、旧 revision、legacy nil、显式重确认、loader withholding 回归 |
| F-005 | FIXED LOCALLY | BigModel 推理策略按 Profile + 精确模型维护；可关闭模型的 `none` 映射 `thinking.disabled`，强制推理/未知的不兼容 effort 在 Provider I/O 前拒绝；`ReasonsUnasked` 复用同一策略 | `TestBigModelChatReasoningIsExactPerModel`、`TestBigModelReasoningPolicyIsProfileAndExactModelScoped` 及 adapter 回归 |
| F-006 | FIXED LOCALLY | 增加独立字面量兼容表，冻结 34 Profile、18 Surface、14 Offering 及关键绑定，不会因同步修改生产常量逃逸 | `TestProviderProfileIdentifiersAndBindingsAreStable`、`TestAccessSurfaceIdentifiersAndProductBindingsAreStable`、`TestProviderOfferingIdentifiersAreStable` |
| F-007 | FIXED LOCALLY | failure capture 启动时扫描当天合法记录恢复额度；用 pending/commit 计数避免失败写虚增；指标跨午夜主动 rollover | 重启额度、失败写、午夜指标、并发/race 回归 |
| F-008 | FIXED LOCALLY | release 强制精确 SHA 的普通 main CI 成功证明；文档与实际门禁对齐；主/探针镜像均生成镜像 SBOM 并扫描；dead-man 镜像包含 LICENSE/NOTICE/THIRD_PARTY_NOTICES | workflow contract、YAML/action 语义检查、容器文件契约 |
| F-009 | FIXED LOCALLY | 归档脚本预检并唯一选择 formal 或 `-dry-run` evidence；所有下载与验证在同文件系统 staging 完成后原子发布，失败不留半成品目录 | `tools/release/test_archive_release_run.py` |
| F-010 | FIXED LOCALLY | formal 发布允许同一 SHA、尚无 GitHub Release 的既有 tag 幂等续跑；不同 SHA 或已发布版本仍硬拒绝 | workflow contract 中 tag-resume 负向/正向断言 |
| F-011 | FIXED LOCALLY | Python 增加完整传递 hash lock；三语言 SDK 依赖加入 license hash 漂移与漏洞门禁；发布和普通 CI 使用同一约束 | `check-dependency-license-review.sh`、Python `--require-hashes`、pip-audit/npm audit/govulncheck |
| F-012 | FIXED LOCALLY | Usage failures 订阅应用内 location，query-only 导航与前进/后退会重新同步筛选 state 与查询 key | 页面级 history/query 回归 |
| F-013 | FIXED LOCALLY | fixed-region endpoint 冲突直接显示在地址字段，保存路径也保留同一校验与焦点反馈 | Provider 页面 fixed-region 回归 |
| F-014 | FIXED LOCALLY | Credential 名称、地址、URL 形状和新建 secret 均产生行内错误、`aria-invalid` 与焦点定位，不再静默 return | Credential 表单键盘/校验回归 |
| F-015 | FIXED LOCALLY | payload reveal 按 disabled、audit unavailable、session expired、forbidden、一般读取失败分别呈现，并为会话失效提供重新登录动作 | Failure detail 状态码/错误码页面回归 |
| F-016 | FIXED LOCALLY | 只有 legacy 三项归因同时缺失时显式显示 unknown；正常 regionless 产品不被误报为未知 | Usage attempt/failure detail legacy 回归 |
| F-017 | FIXED LOCALLY | Provider 行对 by-endpoint 产品从 base URL 推导已知地域并显示；未知代理仍保持 unknown | MiniMax/Kimi endpoint-derived region 页面回归 |

## 2. 候选问题处置

| ID | 状态 | 处置 / 剩余边界 |
| --- | --- | --- |
| C-001 | MITIGATED / EXPLICIT RESIDUAL | MiniMax Global general 与 subscription 同 host/path/auth，Halro 不能机械鉴别购买产品。两者现在分别绑定确认，并标记 `operator_declared_unverified`；未知代理不能绕过 Global 责任。没有真实 Provider 授权，因此不宣称上游已验证。 |
| C-002 | FIXED LOCALLY | price-pin commit 与 `MarkStarted` 失败 settlement 均写 `failure_phase=accounting`，有故障注入回归。 |
| C-003 | FIXED LOCALLY | 新 settlement 在 append boundary 通过有限状态约束 outcome/phase/class/status/retryable/ambiguous/provider evidence；历史认证记录仍可读。 |
| C-004 | FIXED LOCALLY | capture 改为单 worker、容量 64 的 best-effort 队列；`PutContext` 与 Runtime 超时关停可取消。内核已经进入的普通文件 syscall 无法强杀，但只携带已密封 bytes 且并发上限为 1。 |
| C-005 | FIXED LOCALLY | Provider/Credential mutation pending 时锁定 modal 关闭/取消并用同步提交闩防重复 intent。 |
| C-006 | FIXED LOCALLY | disabled capture 启动只忽略 `NotExist`；权限和其他 I/O 错误 fail closed。 |
| C-007 | FIXED LOCALLY | Provider tabs 的左右方向键按 WAI-ARIA 模式首尾环绕，Home/End 保持确定跳转。 |

## 3. 外部证据与发布边界

以下是验收证据缺口，不是可以用本地代码“修掉”的 finding：

- 未调用任何可能计费的真实 BigModel、MiniMax、Kimi 或其他 Provider，也未使用真实客户凭据；
- 未执行真实 KMS、真实外部告警接收端、24 小时 soak 或完整辅助技术矩阵；
- 包含本文件的整改提交尚未取得普通 CI、分支保护或 release dry-run 证据；
- 不会用 fixture、一般 API 或本地绿色门禁冒充上述外部事实。

这些项目应在获得明确授权后按 `review-plan.md` 的 S3/S6 执行，或由发布责任人在最终 assessment
中逐项接受残余风险。在此之前不得把本地整改状态写成正式 `GO`。

## 4. 最终本机复验

| 门禁 | 结果 | 说明 |
| --- | --- | --- |
| `go test -count=1 ./...` | PASS | 在允许本机临时监听器的环境执行；所有包通过，`internal/app` 约 206 秒 |
| `go vet ./...` | PASS | 使用隔离 Go build cache |
| 聚焦 race | PASS | failurecapture/gateway、应用层 secret canary 与 budget 并发影响面均由修复批次执行 |
| `npm run typecheck` | PASS | TypeScript build mode |
| `npm test` | PASS | 44 个测试文件，604 项测试 |
| `npm run build` | PASS | Vite 生产构建与浏览器产物 secret scan 通过 |
| bundle 可复现性 | PASS | 保存第一次构建后再次构建，目录逐文件无差异；当前 git diff 是应随源码提交的新 bundle |
| 发布/归档契约 | PASS | workflow、archive 与 run-evidence 共 20 项 Python 测试 |
| license drift / shell / diff | PASS | dependency review hash、归档脚本语法、`git diff --check`、Go 格式均通过 |
| SDK 依赖漏洞检查 | PASS | Python pip-audit、Node npm audit、兼容性 Go govulncheck 均未发现已知漏洞 |

本地门禁通过只关闭代码整改阶段。该提交仍需在同一 SHA 上通过普通 CI，并在获得相应授权后完成
真实 Provider/KMS/外部接收端验收或具名风险接受；在此之前不构成 release-ready 或正式 `GO`。
