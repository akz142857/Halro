# v0.8.0 全面评审 Findings

评审对象：`v0.7.1..222d08f84f61493fc9a273d351cc728528d6e30c`
当前判定：**NO-GO**

> 2026-09-11 整改更新：F-001～F-017 已在本地工作区修复，候选 C-002～C-007 已关闭；
> C-001 以 `operator_declared_unverified` 的显式残余风险处置。完整状态和回归证据见
> [`remediation.md`](remediation.md)。本文件保留原始候选 SHA 的发现与 NO-GO 裁决，不用未提交
> 的整改工作区改写历史结论。

## 1. 已确认问题

| ID | 级别 | 置信度 | 摘要 | 违反/影响 |
| --- | --- | --- | --- | --- |
| F-001 | P1 | 高 | v0.7.1 面对 schema-8 Usage manifest 仍启动并 ready；同为 v13 的旧 checkpoint reader 会忽略新归因/失败字段并重写 open tail | INV-11/12；回滚 fail-open、派生数据静默降级 |
| F-002 | P1 | 高 | `RecoverPendingLeases` 从 reservation 重建 Attempt 时遗漏 Offering/Profile/AccountRegion | INV-11；重启后的 settlement 永久丢产品/地域归因 |
| F-003 | P1 | 高 | 上游错误 prose 可把 Provider credential 回显到 failure capture，随后被全局 `read_only` Admin 解密读取 | INV-08；跨权限 credential disclosure |
| F-004 | P1 | 高 | 受限产品条款 revision 只在新建/换绑时校验；revision 更新后既有连接仍装载并接流量 | INV-06；旧确认自动代表新条款 |
| F-005 | P1 | 高 | BigModel 对 `glm-4.7` 等默认/强制推理模型接受 `reasoning_effort=none` 却省略 `thinking` | INV-05/07；调用者意图、额度和北向渲染语义漂移 |
| F-006 | P2 | 高 | 34 Profile、18 Surface、14 Offering 的永久 ID 缺少全量字面值兼容闸 | 历史持久化身份可被同步改常量绕过测试 |
| F-007 | P2 | 高 | failure capture `max_records_per_day` 是进程内计数，重启后恢复满额度且指标低报 | INV-14；磁盘上限与监控失真 |
| F-008 | P2 | 高 | release 文档/实际 gate/branch protection 不一致；主镜像缺镜像级 scan/SBOM，dead-man 镜像缺许可证文件 | 发布治理与供应链证明不闭合 |
| F-009 | P2 | 高 | dry-run evidence artifact 带 `-dry-run`，归档脚本只查无后缀名称 | 彩排证据无法按发布文档归档 |
| F-010 | P2 | 高 | tag 已创建而 GitHub Release 失败的半发布状态无法幂等重跑 | 恢复流程会被已有 tag 卡住 |
| F-011 | P2 | 高 | SDK compatibility 的 Node/Go/Python 依赖未纳入完整 license/drift/vulnerability gate；Python 无传递锁/hash | 兼容性 fixture 供应链不可完整重建 |
| F-012 | P2 | 高 | Usage 最终失败页在 query-only 导航或前进/后退时 URL 已变、过滤 state 仍旧 | INV-15；页面展示和实际查询不一致 |
| F-013 | P2 | 高 | BigModel fixed-region URL 冲突时保存被禁用，但唯一错误只在不可触发的 submit 中设置 | INV-15；操作员被无解释阻塞 |
| F-014 | P2 | 高 | Credential 名称/地址为空时保存仍可点，handler 静默 return，无错误/焦点/ARIA | INV-15；静默失败 |
| F-015 | P2 | 高 | GatewayRequest reveal 把 audit unavailable、session 过期、disabled 等都显示为“没有捕获” | 安全故障和可恢复错误被误诊 |
| F-016 | P3 | 高 | 旧 Usage 缺新归因字段时 UI 直接省略，而不是明确显示 unknown | INV-11 的 legacy 可解释性不足 |
| F-017 | P3 | 高 | by-endpoint 的 MiniMax/Kimi Provider 行不显示已可从 endpoint 推导的 Region | 跨地域连接可辨识性不足 |

### F-001：旧版回滚 fail-open 并可静默降级 checkpoint

当前把 Parquet schema 从 6 提升至 8，却保持 bbolt schema 36、Ledger epoch 5 和 Usage checkpoint 13。`Runtime.Open` 在 `internal/app/runtime.go:359` 只构造 exporter；`NewExporterWithOptions` 不读取 manifest，真正的版本检查延迟到 `Export`/`Verify`（`internal/usage/parquet.go:185-205,271-281`）。因此旧版能完成 registry/runtime 装配、bind 三个监听器并返回 ready，之后只周期性 WARN。

更隐蔽的边界是 checkpoint：新字段位于 JSON `AttemptEvent`，旧版 v13 reader 会忽略未知成员，下一次保存仍写 v13；当前版重开时把它视为可信，不会重放已被旧版 checkpoint watermark 覆盖的 Ledger tail。独立运行时演练确认 old `doctor`/`usage verify` 拒绝但 old `serve` ready=200；旧数据面完成第三条 accounting 请求，当前版再次接管后普通 `usage verify` 仍可为绿，说明该验证不能发现 checkpoint 字段语义降级。

退出条件：任何 current-only durable/derivative 语义必须建立可机器拒绝的版本边界；旧版 `Open` 必须在 bind 前 fail；四段 roundtrip 回归要逐字段比较 Ledger→checkpoint→Admin/Usage/Parquet，不只比较条数。

### F-002：pending lease 恢复丢失目标归因

正常 reservation 与 settlement 都携带 Offering/Profile/AccountRegion，但 `internal/budget/manager.go:1192-1203` 的恢复 Attempt 构造漏掉三项；`settle` 随后从空 Attempt 写空值。Ledger settlement 仅检查 project/period、run 和 price snapshot（`internal/ledger/event.go:643-664`），未检查 settlement 与 reservation 的目标归因一致。独立 overlay 复现 reservation 为 `(bigmodel.coding-plan, bigmodel.cn.coding.chat.v1, cn)`，恢复 settlement 为三个空值。金额保守结算仍正确。

退出条件：Started/未 Started 两条恢复路径保留完整 frozen target snapshot；Ledger durable boundary 拒绝归因改变；回归贯穿 Usage、checkpoint、Parquet、backup/restore。

### F-003：Provider credential 可进入 failure capture

OpenAI/Anthropic adapter 会把不受信任的上游错误 message 放入 `provider.Error.Message`；`internal/gateway/capture.go:94-117` 又把该 prose 作为 response body 写入 capture，尽管注释明确承认其可能回显 credential。Store 只截断、不净化。payload GET 受通用 `requireAdmin` 保护，而 `read_only` 是实例级全局 GET 身份。

独立 canary 测试确认：假 adapter 返回含 secret 的 401 错误后 capture 持有 canary；两个 Project 的密封 payload 均可由 `read_only` Admin 以 200 读取。默认关闭、AEAD、TTL、0700/0600、读取审计及 audit fail-closed 都成立，但不能阻止这一跨权限扩散。

退出条件：默认不持久化任意上游 prose，或用实际 authorizer secret 做精确擦除且无法证明净化时 fail closed；以回显真实 Authorization/x-api-key 的 fake upstream 覆盖文件、Admin payload、日志、审计和错误响应。

### F-004：条款 revision 更新不影响既有受限连接

当前 revision 只在内存 Surface 表与历史 audit metadata 中；Credential/Provider durable model 不保存“当前有效确认”。创建与换绑会精确校验，但启动 loader `internal/app/providers.go:470-557` 不比较 policy revision，route 仍注册。故 BigModel Coding Plan/MiniMax Subscription 在 R1→R2 后可继续接流量，除非管理员主动换绑。

退出条件：持久确认至少绑定 Credential + Offering/Surface + Region + revision；每次 topology 激活/启动比较当前 revision，不一致时 withholding 并给出稳定原因码及重新确认入口。

### F-005：BigModel `none` 被静默丢弃

`internal/compatibility/bigmodel.go` 把少数已知模型之外的目标默认为 no-reasoning，并对 `none` 省略 `thinking`。当前官方契约中 `thinking.type` 默认 enabled，`glm-4.7` 为强制推理模型；仓库测试还把 `glm-4.7 / none → no thinking member` 固化为通过。见 [Z.AI Chat Completion 官方说明](https://docs.z.ai/api-reference/llm/chat-completion)。

退出条件：按 `(profile, exact model)` 维护 forced/default-on-disableable/optional/unknown policy；可关闭模型把 `none` 映射为 `thinking.disabled`，不可关闭或未知模型在 Provider I/O/额度预留前拒绝或路由 away，并修正 `ReasonsUnasked`。

## 2. 候选问题与外部证据缺口

| ID | 候选级别 | 状态 | 所需裁决 |
| --- | --- | --- | --- |
| C-001 | P1 | MiniMax Global general 与 subscription 同 host/path/Bearer，Halro 无法从 wire 验证产品声明 | 一方契约/脱敏实测；或产品明确接受“管理员声明可信”的例外 |
| C-002 | P2 | price pin commit / `MarkStarted` 失败的 settlement phase 为空 | 两个故障点注入，比较 log/Ledger/Usage/capture |
| C-003 | P2 | Ledger durable boundary 未约束 outcome/class/phase/retryable/ambiguous 的交叉一致性 | 新 caller/recovery 负测与有限状态表 |
| C-004 | P2 | capture 同步且不可取消，慢盘+失败风暴可能线性占用 goroutine/socket | blocking writer 压测与资源曲线 |
| C-005 | P2 | pending 表单可关闭重开并用新 idempotency intent 双写 | 慢 Admin 端到端双提交实验 |
| C-006 | P3 | disabled capture 启动吞掉非 NotExist 的 `stat` 错误 | 权限/I/O 故障注入 |
| C-007 | P3 | Provider tabs 方向键不环绕 | Chrome/Safari + 屏幕阅读器裁决 |

真实 Global Coding Plan、MiniMax CN/Global Subscription 的 enumeration/chat/stream/usage/substitution/quota/error 样本均未取得；本轮没有授权计费调用。这些不能从一般 API、fixture 或绿色 SDK gate 推导为通过。

## 3. 负面证据

- Offering Kind 没有进入 adapter/auth/path/retry/pricing/budget 决策；四象限 host/path/scheme/group 隔离和注册闸大体成立。
- 上游枚举只证明模型存在，未知目标不会凭枚举或手输自动获得能力；metadata source 有调用点 gate。
- Gateway Key/header 没有正常 capture 入库路径；三侧正文独立截断、AEAD project binding、TTL 和读取前审计成立。
- 正常请求的金额、reservation/settlement、ambiguous 保守结算、canonical failure reason 主路径防线成立。
- 完整本机门禁和 exact-SHA CI 全绿；这与上述负向生命周期/语义复现并不矛盾。
