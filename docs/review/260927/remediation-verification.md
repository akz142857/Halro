# Review 整改复验报告

> 日期：2026-09-27
>
> 基线：`0c4a270d7aae2b3d4562c35cd60ff14907278897` 上的未提交整改工作区
>
> 结论：**Go（repository scope）**；**BLOCKED（目标环境 HA / production scope）**

## 1. 多角色执行结果

本轮按原 Review 的风险边界并行实施，并由主 Reviewer 做交叉复核：

- Gateway / scope：关闭 R-002、R-013、R-014；
- HA / lifecycle：关闭 R-001、R-003、R-004、R-012；
- Product / operations：关闭 R-005–R-011；
- 主 Reviewer：复核跨角色契约、补齐 compatibility manifest 的生成源与 golden 防回归门禁，并执行最终完整组成门禁。

没有以删除原 finding 或改写初始报告的方式“关闭”问题。原始 No-Go 仍保留在 `review-report.md`，当前状态单独记录在 `progress.md`。

## 2. P0/P1 关闭裁决

### R-001 · Provider at-most-once confirmation gap

`provider_resources.creation_status=in_flight` 现在属于需要 confirmation 的 metadata mutation。资源调用只有在该状态到达 confirmed prefix 后才能继续 Provider I/O；无 quorum 时调用阻塞/失败关闭，不再让 promotion 把同一 idempotency key 恢复为可再次 dispatch 的 `reserved`。

回归覆盖 reserved 不等待、in-flight 等待确认、journal classification、HA runtime 与全量 race。目标环境 promotion/partition 仍由 HA 验收门禁负责。

### R-002 · Resource-plane scope bypass

Files、Batches、Async invocations 与 Deferred Responses 共用的 resource principal 现在统一要求 `inference` scope，并在 record lookup 和 Provider I/O 前返回 403。兼容性 manifest 的权威 Go 定义、生成 JSON 与契约文档同步更新，并增加防止只改 golden 的测试。

### R-003 · Planned stepdown active-request gap

Primary 接受 planned stepdown 前先撤销 readiness，再使用 `http.Server.Shutdown` 等待已接纳 handler 结束，随后才冻结 confirmed prefix 并签发 promise。drain 失败不冻结；drain 使 index 前进时，旧 proposal 因 prefix 过期而失败，coordinator 保持可重试状态。

仓库测试覆盖 drain-before-freeze、shutdown seam 和 stale-prefix fail-closed。真实 Pod、SSE 和长连接 drain 是目标环境证据，仍为 BLOCKED。

### R-004 · Admin session/MFA state revival

Admin session 删除、MFA challenge 的 claim/attempt/delete、已有 authenticator 更新及 TOTP watermark 推进全部进入 confirmed mutation 边界；challenge cancel 不再吞掉存储错误。promotion 后不再从未确认后缀复活 logout、challenge 或已消费 TOTP 状态。

## 3. P2 关闭摘要

- Claude subscription expiry 从 secret document 进入权威 Credential expiry，冲突或非法值失败关闭；
- config migration 严格解析正整数版本，拒绝 symlink/身份竞态，保留 portable 文件身份并对 backup/staging/rename 做持久化和失败清理；
- route suspension 的 offline clear 与审计意图同事务，Admin degraded read 返回 503，Console clear 补全确认/step-up/cache/error 语义；
- CHANGELOG 与 release assessment 完整性成为发布门禁；
- 缺失的告警 firing、持续窗口、recovery 与负对照已补齐；
- seed index 0、`discovery => inference`、认证后 fixed ceiling 顺序均以代码、测试和契约固定。

## 4. 最终验证证据

代码稳定后执行完整组成门禁：

1. `make full-check` 在 Node 24 shell 中通过普通 Go 全量、全量 `-race`、`go vet`、前端全量测试和 observability 校验；最后的 production bundle target 按设计拒绝 Node 24。
2. 切换本机已安装的 Node `v22.18.0` 后，执行 `make frontend-production-check` 通过。两条命令之间没有代码变化。

通过项：

- `go test -count=1 -shuffle=on ./...`；
- `go test -race -count=1 -timeout=20m ./...`，其中 `internal/app` 约 611 秒；
- `go vet ./...`；
- Vitest：46 files、677 tests；
- Prometheus/Alertmanager：49 alerts、13 recording rules，规则与 provisioning 校验通过；
- Node `v22.18.0` production build、29 个浏览器 artifact secret scan、embedded bundle drift check 通过；
- `git diff --check` 通过。

第一次完整门禁曾发现 compatibility JSON 被直接修改而 Go 生成源未同步。该失败没有被忽略：整改为修改 `internal/compatibility/manifest.go`、增加 source-level test、重新生成 golden 后，相关定向测试及上述完整组成门禁通过。

## 5. 仍未通过的边界

以下结论没有被本地门禁替代：kind 故障、三节点非对称 partition/physical fencing、真实 Pod/SSE drain、Linux ENOSPC/只读/慢盘、三节点性能、相邻发布升级、G0–G7、72 小时 soak、真实 RTO、签名 artifact/provenance 与生产实例同 SHA。

因此：当前工作区可以进入代码审查/提交阶段，但不能据此宣称 HA 目标环境已验收，也不能直接授权生产启用。未运行任何 billable real-provider smoke。
