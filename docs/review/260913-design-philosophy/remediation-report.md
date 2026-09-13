# Halro 系统设计哲学评估：顺序整改与补证复评

评估基线：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`

整改候选：已提交的 `b7edd4d24152582e5978cf58d1917fedd803e542` 加当前未提交工作树；尚未形成新的
候选提交 SHA，也未发布。首轮 `v0.8.0` 冻结事实与整改候选证据不得互相替代。

复评日期：2026-09-13

## 1. 复评结论

首轮六个工作包之后，本轮继续按顺序完成 Provider primitive 独立契约、完整本地门禁、fresh Go
证据、发布 preflight、CLI help、带真实 Ledger/Usage 的双二进制恢复、Admin 浏览器旅程和交替容量
复测。所有已确认 P2 均已有代码或 current 文档处置；外部 E4 不因本地证据而关闭。

本轮裁决是 **`ORDERED REMEDIATION COMPLETE / EXTERNAL EVIDENCE BLOCKED / PRODUCTION UNVERIFIED`**：

- 本轮约定顺序内的实现和文档整改已完成；
- 全量 Go、前端、静态检查、竞态检查和 Prometheus 规则测试通过；
- `PHIL-A04`、CLI/help、完整门禁、cache 语义和发布前下游 preflight 已关闭；
- S3/S4 保守加权覆盖达到约 84%，因此按原方案首次形成整改工作树的 67/100 可追溯总分；候选提交
  冻结后仍须重跑受影响门禁，才能把该分数绑定到 release SHA；
- v0.8.0 Homebrew 仍停在 v0.7.0，APT `InRelease` 返回 503；当前没有有效 GitHub App、真实 Provider、
  KMS/PKI、Contact Point 或 24h target workload 条件，故这些项目是具名 BLOCKED/UNVERIFIED；
- 67 分落在“高风险”档，且 D3/D5 关键外部主张没有 E4，生产准入仍为 No-Go。

本文状态词分层使用：`CLOSED` 只表示整改候选上的 finding 已由对应本地证据关闭；`BLOCKED` 表示
本轮因渠道状态、凭据或 target 条件无法执行；`UNVERIFIED` 表示对应外部主张尚未被证明。一个实验可
同时处于“执行 BLOCKED、主张 UNVERIFIED”，两者都不等于失败或通过。

## 2. 逐项复评

| 顺序 | Finding | 结果 | 当前证据 | 保留边界 |
| --- | --- | --- | --- | --- |
| 1 | `PHIL-B01` portable stream 最终事实分叉 | **CLOSED / E2** | Responses 与 portable Messages 的 facade completion 进入 Request 生命周期；最终事件失败时 Request 记 `provider_error`，Provider attempt 保留真实 settlement，且不 fallback | 未以真实断连代理做 E3 |
| 2 | `PHIL-CD-001` capture 入队前无 byte bound | **DEFECT CLOSED / E2** | `PrepareRecord` 在异步队列取得所有权前按三侧 `max_bytes` 截断；Store 边界再次幂等执行；blocking-store 大载荷测试证明队列内记录已受限 | production-shaped RSS/GC/p99 仍 `UNVERIFIED` |
| 3a | `PHIL-A01` current capability 真相分叉 | **CLOSED / E2** | User/Operator/Architecture/Milestone 文档统一标明 Bedrock Runtime/Agent Runtime 为 Withheld，Run Governance 为已实现；domain golden 防止 current 文档再次漂移 | 历史计划保留历史语义，不作为 current truth |
| 3b | `PHIL-A05` Workbench Go snippet 不可运行 | **CLOSED / E2** | 非流式与流式示例均处理构造、transport、status、body/stream error，关闭 body 并限制读取；页面回归与临时最小 Go 程序编译通过 | 尚未建立所有语言 × streaming 的独立编译矩阵 |
| 3c | `PHIL-E-004` release evidence 错述 | **CLOSED / E1** | current 文档改为 release 会重建并比较 committed web bundle，且明确 fuzz 不在该 workflow 中 | 没有改变 workflow 本身 |
| 4 | `PHIL-CD-003` accepted-work 告警缺口 | **LOCAL CLOSED / E2** | 新增 shutdown-truncated 和 stale pending lease rules、阈值 gauge、runbook；固定 Prometheus 镜像的 promtool firing/non-firing/resolved 测试通过 | promtool 是合成规则测试，不是 E3/E4 投递；未接真实 Alertmanager/Contact Point，外部投递仍 `UNVERIFIED` |
| 5 | `PHIL-A04` operation→primitive 自证 | **CLOSED / E2** | 独立 literal oracle 覆盖全部 Profile/operation；具体 adapter 家族 guard；attempt 的 reservation/start/settlement/recovery 持久化 primitive，Ledger 拒绝错绑 | 真实 Provider entry point 仍按 profile 为 `UNVERIFIED` |
| 6 | `PHIL-A06` 顶层 CLI help | **CLOSED / E2** | `--help`、`-h`、`help` 与 `help <command>` 均从同一 descriptor 清单生成并回归 18 个命令 | 未引入 Cobra/大型框架 |
| 7 | `PHIL-E-001` / `PHIL-E-002` 本地 gate/cache | **CLOSED / E2** | `make full-check` 覆盖 web production build/drift；test/race、CI、release 与 SDK Go contract 明确 `-count=1` | fuzz 仍是独立 CI job，不伪装成本地 full-check |
| 8 | `PHIL-E-003` 下游发布 preflight | **IMPLEMENTED / E2** | publish/tag 前检查变量、secret、App 安装、两仓库可见性与 contents:write；workflow contract 防倒退 | 未在有效 App 凭据上跑正式发布，外部运行证据仍缺 |
| 9 | `PHIL-B02` 完整恢复演练 | **LOCAL CLOSED / E3** | v0.7.1 真实 8-frame/2-row 数据→schema 37→16-frame/4-row；old-reader refusal；候选 backup/restore；旧 binary 恢复 pre-upgrade backup | OS `SIGKILL` 命中 schema transaction 未确定性执行；target KMS/磁盘故障未测 |
| 10 | Admin/容量/Key lifecycle | **LOCAL E3** | 八页真实 server data；320/768/1440 与 200% 等效视口；禁用 Key 后 401；8 轮交替 benchstat；1000 SSE cleanup | 辅助技术实机、production host 与 24h soak 未测 |

## 3. 设计判断

本轮没有引入消息队列、外部缓存、通用 DI 或新的运行时服务。整改继续保留 Halro 的 single-binary、
single-process、single-writer 方向：

- stream 修复收敛了一个已有生命周期边界，没有复制第二套 accounting；
- capture 修复复用了 Store 的同一截断语义，没有引入不可观测的全局内存配额；
- stale lease 告警阈值直接来自 route total timeout 与 stream max duration 的较大值，而不是硬编码另一套
  “正常时长”；
- current capability golden 只锁定 externally visible truth，不把整个文档生成系统扩成新框架；
- Go 示例仍从环境变量读取 Key，错误正文读取有 1 MiB 上限。
- operation→primitive 的 expected 不再从生产 registry 反向生成，且执行 primitive 成为 durable attempt
  attribution；
- 发布渠道仍是可恢复 saga，但可预检的 App 权限现在位于不可逆 tag/Release 之前；
- Admin 宽屏修复只补 sidebar 存在时的中间断点，没有引入另一套页面结构。

因此本轮符合原方案中“先修不变量和真相，再增加证据”的方向，也没有借整改扩张 Halro 的产品边界。

## 4. 验证结果

首轮机器可读记录见 [remediation-gates.json](evidence/remediation-gates.json)；本轮 E3 原始证据见
[e3-completion](evidence/e3-completion/README.md)。关键新增结果：

- `go test -count=1 ./...`：PASS；
- `go test -race -count=1 ./internal/gateway ./internal/failurecapture`：PASS；
- `go vet ./...`：PASS；
- 前端 Node 22 typecheck、44/44 test files、605/605 tests、production build 与 artifact secret check：PASS；
- Prometheus rules 的 Go contract 与固定 `prom/prometheus:v3.5.0` promtool 测试：PASS；
- Workbench 两个 Go 示例的临时最小程序编译：PASS；
- `git diff --check`：PASS。
- Provider/App/Budget/Ledger/Gateway focused `-count=1`：PASS；
- release workflow contract 与 CLI help regression：PASS；
- v0.7.1→候选→旧读者拒绝→候选 restore→v0.7.1 rollback restore：PASS；
- 8 轮交替 benchstat：四项均无显著 latency 变化；1000 SSE cleanup：PASS；
- Admin 真实页面与响应式矩阵：PASS（首次发现的 1440px 横向溢出已修复并复测）。

第一次 sandbox 内全量 Go 尝试因 `httptest` 不能绑定本地回环端口而失败；随后在获准的本机回环环境
重新运行并取得 exit code 0。该环境失败没有被计为产品失败，也没有被隐去。

## 5. 尚未关闭的评估项

### 活动 P0/P1/P2

- 未确认活动 P0/P1/P2。这里不包含证据等级不足的生产主张；它们保持 No-Go，而不是被降格为代码 bug。

### E3/E4 与生产准入

- production-shaped capture RSS、GC、p99 和排队容量曲线；
- 真实 Alertmanager Contact Point 的 firing/resolved/heartbeat-loss；
- OS kill-point、target 磁盘故障与 KMS restore；
- 真实 Provider、KMS、PKI、clean-host v0.8.0 package、辅助技术实机和 24h soak。

上述证据未取得前，首轮报告的 **`PRODUCTION UNVERIFIED`** 与 Production Admission No-Go 不变。

## 6. 下一步

本次授权整改批次中的仓库项已经执行完；路线图仍保留 Runtime 渐进收缩与实验能力价值/删除台账等
未执行 P3，不能把“批次完成”外推为整个 31–60 天路线图完成。下一步的生产准入工作是准备 disposable target、有效
GitHub App、Provider/KMS/PKI/Contact Point 凭据和 production-shaped 24h workload，再按
`docs/observability/admission-checklist.md` 逐行取得不可变 evidence ID 与四方签署。Homebrew/APT 必须先
修复渠道状态并对精确版本做 clean-host 验收；在此之前网站继续保持逐渠道诚实状态。
