# Halro 生产验证执行方案

> - 状态：`PROPOSED / TARGET-ENVIRONMENT EXECUTION REQUIRED`
> - 编制日期：2026-09-14
> - 编制基线：`e194ee92df011dde110eda707ad76e22ed9f5e12`
> - 执行候选：`CANDIDATE_SHA = TBD`，开始验证前必须冻结
> - 准入结论：所有强制门禁通过并完成四方签署前，维持 `PRODUCTION UNVERIFIED`

## 1. 目的

本方案把“代码、测试和发布流水线已经工作”推进到“在目标生产形态中得到可复核的运行证据”。
验证对象不是某个浮动分支，而是一个不可变的候选提交、构建产物及目标环境配置组合。

本轮需要回答以下问题：

1. 正式声明支持的 Provider primitive 是否在真实服务商端点上成立。
2. 身份、密钥、策略和网络边界失效时，系统是否默认拒绝并可恢复。
3. 告警、独立 dead-man、审计、备份和恢复是否形成闭环。
4. 生产形态负载下，容量、延迟、资源增长和 24 小时稳定性是否达标。
5. 正式发布及 Homebrew、APT 等下游制品是否能在干净主机上安装和验证。

本方案不以本地单元测试、普通 CI、模拟 Provider 或 release dry-run 代替真实环境证据；也不在未获
明确授权时执行计费 Provider 请求、修改生产账户或触发外部通知。

## 2. 适用范围与证据等级

| 等级 | 含义 | 可支持的结论 |
| --- | --- | --- |
| E1 | 文档、设计和静态检查 | 设计意图存在 |
| E2 | 单元、集成、契约和故障注入测试 | 仓库内实现满足契约 |
| E3 | 本机、容器或托管 CI 的端到端运行 | 可重复构建和集成 |
| E4 | 目标生产形态中的真实依赖、身份、网络和故障演练 | 生产准入证据 |

本方案的最终目标是 E4。E1–E3 是前置门禁，不得被报告为生产验证完成。

## 3. 不可变验证单元

开始执行前冻结以下六项；任一项变化都必须判断受影响门禁并重跑，不能沿用旧证据。

| 字段 | 要求 | 当前状态 |
| --- | --- | --- |
| `CANDIDATE_SHA` | 完整 40 位 Git SHA；不得使用浮动 `main` | 待冻结 |
| `RELEASE_VERSION` | 使用新的候选版本，例如 `v0.8.1-rc.1`；不得移动或复用 `v0.8.0` 标签 | 待确定 |
| `ARTIFACT_DIGESTS` | 二进制、镜像、包、SBOM、签名和 provenance 的摘要 | 待生成 |
| `TARGET_ID` | 一次性或隔离的生产形态环境标识 | 待准备 |
| `CONFIG_DIGEST` | 非敏感配置、部署清单、告警规则和策略的规范化摘要 | 待生成 |
| `TEST_PLAN_REVISION` | 本文执行时的提交 SHA | 待冻结 |

本文的“编制基线”只是方案起草时的代码基线。本文提交后，正式执行必须填写新的
`CANDIDATE_SHA`，并用该 SHA 重新完成精确提交 CI。

## 4. 启动条件与授权边界

### 4.1 必备条件

- 一个与生产拓扑、TLS、IAM、KMS、Secret Store、网络策略和存储类型一致的隔离目标环境。
- 每个正式支持 Provider 的专用测试账户、最小权限凭证、合成测试数据和硬性费用上限。
- 可验证 `firing` 与 `resolved` 的真实 Contact Point，以及处于独立故障域的 dead-man 监控。
- 不可变证据存储；仓库只记录证据 ID、摘要和无敏感信息的结论。
- Application、Security、SRE、Platform 四方负责人和至少一名非作者演练人员。
- 连续 24 小时的浸泡窗口，以及可控的故障注入、恢复和回滚窗口。
- GitHub App、GHCR、Homebrew、APT 等发布凭证及受保护环境已经过最小权限检查。

### 4.2 必须单独授权的操作

以下操作不能因“执行本方案”而自动获得授权：

- 产生真实费用的 Provider 调用；
- 修改生产或共享账户、KMS、PKI、IAM、DNS、仓库权限和通知目标；
- 向真实人员、Pager、Slack、邮件或其他外部 Contact Point 发送告警；
- 创建正式 Git 标签、GitHub Release、镜像或下游软件包发布；
- 在非隔离环境执行磁盘填满、网络阻断、进程杀死、证书吊销或数据恢复演练。

执行负责人必须在对应门禁开始前记录授权人、范围、费用上限、时间窗口和回滚方式。

## 5. 预先填写的服务目标

下表必须在首次测量前填写，禁止看到结果后调整阈值。若暂时没有业务 SLO，应由四方共同签署
临时准入阈值及有效期。

下表的「状态」列是 2026-09-18 执行记录产出的草案结论，完整取值、依据 `path:line` 与推导见
[生产验证执行记录 · 2026-09-18](production-validation-run-260918.zh-CN.md) 第 4 节。草案不等于
签署：正式执行前四方必须逐行确认或改写，并在此处填入最终值。

| 指标 | 目标值 | 状态（2026-09-18 草案） | 负责人 |
| --- | --- | --- | --- |
| 同步请求吞吐与 p50/p95/p99 | 待签署 | 有参考主机基准可依据；延迟必须落在 12 个直方图桶边界上 | Application / SRE |
| 流式首字节与完整响应 p95/p99 | 待签署 | 首字节**已可测量**（2026-09-24，260918-PV-F-06 已关闭）：`halro_stream_first_byte_seconds{operation,le}`，从请求到达到首个事件写出并 flush，按四个 northbound face 分列，落在与其它延迟序列相同的 15 个桶边界上 | Application / SRE |
| 允许错误率、超时率和限流率 | 待签署 | 错误率有依据（告警 5% / 浸泡 1%）；超时率与限流率无依据 | Application / SRE |
| CPU、RSS/heap、goroutine、FD 上限 | 待签署 | 增长容差有依据；绝对值需外部采集器，构成 G1 硬前置 | SRE |
| 队列、重试、failure capture 和 WAL 上限 | 待签署 | 全部为代码中生效的硬上限，可直接签署 | Application / SRE |
| Ledger、Audit、Parquet、TSDB 增长预算 | 待签署 | TSDB 有模板阈值（假设 5 GiB 卷）；应用侧每日字节数无依据，须在 G5 实测 | SRE / Platform |
| Provider 费用和 token 预算 | 待签署 | 无依据，且当前没有任何费用/token 告警规则 | Product / Application |
| RPO / RTO | 待签署 | RPO=0 仅在 `usage.durability: strict` 下成立；RTO 参考值来自 opt-in 测试 | SRE / Platform |
| 24 小时浸泡允许事件预算 | 待签署 | 门禁已在 `tests/soak` 中固化，可直接签署 | 四方签署 |

## 6. 分阶段执行门禁

阶段必须按顺序执行。某阶段出现 No-Go 时，停止扩大流量和破坏性实验，先完成修复、重新冻结候选
并重跑受影响门禁。

### G0：候选冻结与仓库门禁

执行内容：

1. 冻结 `CANDIDATE_SHA` 和候选版本，确认工作树、标签和构建输入。
2. 对精确 SHA 运行仓库完整门禁和普通 CI；保存 workflow/run ID 与日志摘要。
3. 从该 SHA 构建二进制、镜像和包，记录 digest、SBOM、签名与 provenance。
4. 验证运行时版本信息能回指同一 SHA，且所有后续阶段只部署这些 digest。

通过标准：完整门禁成功；源代码、CI、运行版本和所有制品可追溯到同一 SHA；无未解释的生成物漂移。

### G1：目标环境与观测基线

执行内容：

1. 在全新目标环境完成部署、初始化、健康检查和最小权限访问。
2. 记录基础设施、部署配置和告警规则摘要，确认时间同步及日志时间线可关联。
3. 验证指标抓取、日志、追踪、审计写入和 Alertmanager/Contact Point 基础连通性。
4. 验证敏感字段、凭证和测试 canary 不出现在日志、指标标签、追踪、错误响应与证据包中。

通过标准：部署可重复；全部观测链路可用；无敏感信息泄漏；环境身份和配置摘要固定。

### G2：真实 Provider primitive 矩阵

以 [真实服务商验证矩阵](provider-real-matrix.md) 为执行台账，对每个正式声明支持的 profile
分别验证其所声明的 primitive，包括适用的 chat、responses、messages、streaming、embeddings
和工具调用。Provider 返回的“有哪些模型”与 Halro 对“模型具有什么能力”的证据必须分开记录。

每个 profile 至少覆盖：

- 一个有效请求和一个有效流式请求；
- 无效或已撤销凭证、权限不足、未知模型、限流、超时和上游 5xx；
- 使用量、费用、终态、错误分类、Ledger 与审计的一致性；
- 首字节或外部副作用发生后不切换 Provider、不重复执行副作用；
- 未被 Provider 实际枚举或确认的目标不伪装成 `provider_metadata` 能力；
- 真实响应体先被安全捕获和脱敏，再据此验证 decoder，而不是只验证自造 fixture。

通过标准：正式声明的每项能力都有 E4 正反例；不存在静默降级、错误归因、双写/漏记或终态分叉；
费用不超过预授权上限。未执行真实验证的 profile 必须从本次生产声明中移除，不能以模拟结果代替。

### G3：身份、密钥、策略与网络边界

按照 [准入清单](../observability/admission-checklist.md) 执行：

1. 验证有效、缺失、错误、过期和被吊销的 mTLS 身份，以及证书轮换与回滚。
2. 验证凭证双钥重叠、热重载、旧钥返回 401、重启和恢复不会使已撤销身份复活。
3. 对管理面高风险动作验证 RBAC 允许/拒绝矩阵和完整审计。
4. 验证 KMS/Secret Store 正常、不可用、恢复、轮换和错误密钥场景。
5. 验证 SSRF、DNS/IP 复核、私网/metadata 地址、重定向和出站 allowlist。
6. 对审计篡改、删除、顺序异常和写入失败执行检测与告警。

通过标准：身份或策略不确定时默认拒绝；授权边界不可绕过；轮换和恢复不扩大权限；审计失效可见。

### G4：告警、故障注入、备份与恢复

执行内容：

1. 用真实 Contact Point 完成 `firing`、确认、`resolved` 全生命周期，并验证通知中无 secret canary。
2. 证明 dead-man 监控与 Halro、Prometheus、Alertmanager 不共享同一故障域。
3. 分别停止 Halro、Prometheus 和 Alertmanager，验证独立监控在预定窗口内发现心跳丢失。
4. 执行只读磁盘、磁盘满、TSDB 不可写、网络中断、`SIGKILL` 和关键持久化点故障。
5. 按 [崩溃恢复矩阵](crash-recovery-matrix.md) 验证 WAL、Ledger、Audit、状态和终态一致性。
6. 从受控备份恢复到全新环境，测量 RPO/RTO；随后验证升级和回滚。

通过标准：无静默丢失或错误成功；故障被正确分类和告警；恢复满足预填 RPO/RTO；已撤销凭证不会
因恢复复活；非作者能仅凭 runbook 完成演练。

### G5：容量、退化行为与 24 小时浸泡

负载模型至少包含同步请求、慢 Provider、长流式响应、大响应体/失败捕获、多 Project、公平性、
预算与 token guard、管理面大数据量、备份与压缩等后台任务。基线方法参考
[Standalone 容量基线](standalone-capacity-baseline.md)、[容量模型](../observability/capacity-model.md)
和 [浸泡测试](soak-testing.md)。

执行顺序：

1. 空载基线与低负载校准；
2. 分级升压至预定目标，定位首个瓶颈但不临时改变准入阈值；
3. 在目标持续负载下执行 24 小时浸泡；
4. 浸泡期间注入已批准的 Provider 慢响应、限流和短暂网络失败；
5. 停止负载后观察资源、队列、连接和 goroutine 是否回落。

必须记录 p50/p95/p99、吞吐、错误率、CPU、RSS/heap、GC、goroutine、FD、连接池、队列、重试、
failure capture、WAL/fsync、Ledger/Audit/Parquet/TSDB 增长及 Provider 侧配额。

通过标准：所有预填 SLO 达标；无崩溃、数据不一致、持续内存/FD/goroutine 泄漏、无界缓冲、重试风暴
或队列饥饿；降载后资源在约定窗口内回落；24 小时事件预算未超限。

### G6：正式发布与干净主机验证

发布流程遵循 [发布指南](../guides/releasing.md)，并把“核心制品发布”和“下游包可用”作为不同门禁：

1. 在同一候选 SHA 上完成 ordinary CI、release dry-run 和发布前权限检查。
2. 获得单独发布授权后创建新版本正式发布；不得移动既有标签。
3. 验证 GitHub Release、GHCR、二进制、deb、SBOM、签名、provenance 的版本和 digest 一致。
4. 在从未安装 Halro 的干净 macOS Apple Silicon、macOS Intel，以及受支持的 Debian/Ubuntu
   amd64/arm64 主机上验证安装、版本、启动、升级、回滚和卸载。
5. 验证 Homebrew 与 APT 实际解析到新版本；不得只根据 package job 成功推断客户端可用。
6. 只有全部下游门禁通过后，才更新官网、安装说明和正式生产推荐版本。

通过标准：所有正式渠道可从干净主机安装同一已签名版本；无旧版本漂移；失败时有经过验证的恢复路径。

### G7：非作者演练与四方签署

1. 由未参与实现的值班人员仅使用 runbook 完成一次 15 分钟受控故障演练。
2. 将本方案每项证据 ID 回填到 [准入清单](../observability/admission-checklist.md)。
3. Application、Security、SRE、Platform 分别审阅原始证据、例外和残余风险。
4. 生成最终生产验证报告，明确 `GO` 或 `NO-GO`，不得用模糊分数替代门禁结论。

通过标准：非作者演练成功；所有强制行均为 PASS；证据可访问且可复核；四方完成签署。

## 7. 立即停止与 No-Go 条件

发生以下任一情况必须停止扩大验证范围，并直接记录 No-Go：

- 凭证、密钥、用户内容或测试 canary 泄漏到非授权证据、日志、指标或通知；
- 认证、授权、策略、KMS 或审计异常时 fail-open；
- Ledger、费用、usage、流终态或持久化状态不一致；
- 已输出首字节或产生外部副作用后发生跨 Provider 重试；
- 无界 failure capture、队列、重试、内存、FD 或 goroutine 增长；
- 备份恢复使已撤销身份复活，或无法满足 RPO/RTO；
- 主监控失效时独立 dead-man 同时失明；
- 任一强制门禁无证据 ID、证据无法回指候选 SHA，或四方拒绝签署。

安全或数据一致性相关红线不得通过 waiver 放行。其他例外必须包含负责人、影响范围、补偿控制、截止日
和自动失效条件，并仍由四方签署。

## 8. 证据记录格式

每个实验使用一个不可变证据 ID。原始日志、截图、追踪和报告放在受控证据平台；Git 只保存无敏感
信息的索引和摘要。

```yaml
evidence_id: E4-<gate>-<sequence>
candidate_sha: <40-char SHA>
release_version: <version>
artifact_digest: <sha256>
target_id: <environment identity>
config_digest: <sha256>
gate_id: <G0..G7>
started_at: <RFC3339>
completed_at: <RFC3339>
operator: <identity>
observer: <independent identity>
authorization_id: <ticket or approval id>
command_or_run_id: <reproducible command/workflow/run id>
expected: <predeclared result>
observed: <actual result>
sensitive_data_check: PASS|FAIL
rollback_result: PASS|FAIL|NOT_APPLICABLE
verdict: PASS|FAIL|BLOCKED
artifacts:
  - <immutable evidence URI and digest>
```

每次重跑创建新证据 ID，不覆盖失败记录。最终报告同时列出失败、修复提交和重验证证据，保留完整时间线。

## 9. 建议日程与责任

| 时间 | 工作 | 主责 |
| --- | --- | --- |
| Day 0 | 授权、阈值、账户、环境和候选冻结 | 四方 |
| Day 1 | G0–G1：精确 SHA、制品、环境和观测基线 | Application / Platform |
| Day 2 | G2–G3：真实 Provider、安全与密钥边界 | Application / Security |
| Day 3 | G4：告警、故障、备份、恢复与回滚 | SRE / Platform |
| Day 4 | G5：负载校准和容量爬坡 | Application / SRE |
| Day 5–6 | G5：连续 24 小时浸泡及资源回落 | SRE |
| Day 7 | G6–G7：发布、干净主机、非作者演练和签署 | 四方 |

这是最短执行路径，通常还应预留修复和重验证时间。若账户、证书、KMS、通知或发布权限尚未就绪，
现实排期应按 1–2 周估算，而不是压缩验证内容。

## 10. 完成定义

只有同时满足以下条件，才可把结论从 `PRODUCTION UNVERIFIED` 改为 `PRODUCTION VERIFIED`：

- G0–G7 全部 PASS，且每项都有可访问的不可变证据 ID；
- 精确候选 SHA 的完整 CI、制品摘要、签名和运行时版本一致；
- 所有正式声明的 Provider/profile/primitive 均有真实 E4 证据；
- 身份、KMS/PKI、Secret Store、RBAC、SSRF、审计和告警红线全部通过；
- 备份恢复满足预填 RPO/RTO，独立 dead-man 和非作者演练通过；
- 生产形态容量指标及连续 24 小时浸泡达标；
- 正式发布与所有声明支持的下游包在干净主机通过验证；
- 没有未解决的 P0/P1；任何影响本轮生产声明的 P2 已修复并重验证；
- Application、Security、SRE、Platform 四方签署最终 GO 决策。

若任一条件缺失，最终报告必须保持 `NO-GO / PRODUCTION UNVERIFIED`，并明确下一次可执行动作，
不得以“架构方向正确”、普通 CI 成功或综合评分替代生产准入结论。

## 11. 当前启动清单

在实际执行前，负责人先完成以下空项。2026-09-18 的核实结果是 **0/9 就绪**，逐项理由见
[执行记录](production-validation-run-260918.zh-CN.md) 第 7 节：

- [ ] 冻结 `CANDIDATE_SHA`、候选版本和制品 digest
- [ ] 填写全部服务目标、容量阈值、费用上限和 RPO/RTO
- [ ] 确认真实 Provider 测试账户、声明矩阵和计费授权
- [ ] 准备隔离目标环境、KMS/PKI/Secret Store/RBAC 与故障窗口
- [ ] 配置真实 Contact Point 和独立故障域 dead-man
- [ ] 确认不可变证据存储及敏感数据处理规则
- [ ] 确认 GitHub App、Homebrew、APT、GHCR 权限和干净主机矩阵
- [ ] 指定 Application、Security、SRE、Platform 签署人及非作者演练人员
- [ ] 预约连续 24 小时浸泡与至少一个修复/重验证窗口

上述清单完成前可以继续做 E1–E3 准备，但不能开始计费调用、破坏性实验或正式发布，也不能宣布
生产验证已经完成。
