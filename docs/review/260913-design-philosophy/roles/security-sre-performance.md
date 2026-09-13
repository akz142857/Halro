# 角色 C/D：安全、SRE 与性能容量评估

- 评估日期：2026-09-13
- 目标源码：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`
- 负责维度：D4 可靠性与可运维性、D5 安全与隐私、D8 性能与容量诚实度
- 重点不变量：INV-04、INV-05、INV-10、INV-13
- 阶段：S2 独立角色评估；findings 尚待非作者 S5 证伪
- 变更边界：只新增本报告；未修改产品代码，未 commit/push，未读取或写入现有 Halro 数据目录，未调用真实 Provider、KMS、Contact Point 或告警接收端

## 1. 阶段结论

Halro 在目标 SHA 上已经有一组值得肯定、且相互咬合的基础控制：单写者不是文档约定而是文件锁；readiness 不把进程存活冒充账务可用；SafeTransport 同时拒绝环境代理、redirect、未批准 host、危险 IP 与 DNS rebinding；备份恢复有离线锁、认证加密、链校验、原子发布和失败回滚；failure capture 默认关闭、加密、限时、限条数、按字段截断，并且读取前先写审计，审计失败则不返回正文。

但三个维度尚未形成 Google SRE 意义上的闭环：仓库没有已经批准并运行的用户视角 SLO/error budget；若干代码作者明确希望告警的退化指标没有进入随仓库交付的告警与 runbook；性能只有局部微基准，没有尾延迟、RSS/FD/goroutine 曲线或生产形状 soak。更重要的是，failure capture 的字节限制在存储 worker 中才执行，而异步队列在此前已经持有两份完整 JSON，因此默认 10 MiB 请求上限下，一个阻塞的 capture sink 可令 64 槽队列仅正文就接近 1.25 GiB。这是本角色发现的最高风险候选。

当前结论是阶段性诊断，不是系统总分或 release `GO/NO-GO`。D5 没有 E3 目标环境证据，生产 PKI/KMS、真实告警链、灾备 RPO/RTO 与 24h soak 均仍为 `UNVERIFIED`。

## 2. 评估方法与覆盖

执行顺序为：先冻结 SHA 与边界；追踪入口、身份、出网、敏感正文、持久层、readiness、metrics、alerts、dead-man、backup/restore 与单写者链；再运行能够看到这些主张的窄测试；最后做同机、同工具链、同参数的前一版本微基准对照。

| 子域 | 本轮做了什么 | 最高证据 | 未覆盖 |
| --- | --- | --- | --- |
| D4 | readiness、启动/关闭、Accounting 状态、指标、告警规则、dead-man、备份恢复代码和窄测 | E2 | `kill -9`、磁盘满/只读盘、证书替换、真实 Contact Point、非作者 15 分钟操作演练、批准 RPO/RTO |
| D5 | Admin/Gateway/Metrics/Provider/KMS/backup 信任边界；SafeTransport；secret/capture 生命周期；轮换保护；权限链 | E2 | 真实 KMS/IAM、目标 PKI、heap/core dump、浏览器残留、完整 secret canary 扫描、外部安全复核 |
| D8 | 明确上界静态审计；capture 队列；同机 `v0.7.1`/`v0.8.0` 四项微基准 | E3（局部） | p50/p95/p99、RSS/FD/goroutine、慢读/慢上游、大表、多 Project、backup、审计、TSDB cardinality、24h soak |

角色内检查项覆盖率估算：D4 约 62%，D5 约 70%，D8 约 35%。这不是全局评估覆盖率；按方案纪律，不据此输出 Halro 的 100 分总分。

## 3. 信任边界与敏感数据生命周期

### 3.1 主体、入口与权力

| 边界 | 可做什么 | 已见控制 | 本轮剩余问题 |
| --- | --- | --- | --- |
| Gateway 客户端 | 持 Gateway Key 发起有成本的请求并提交正文 | body 解析前先提取凭据；source limiter、Project policy/budget/capability 在 Provider I/O 前；请求体上限 | 启用 failure capture 时，可用合法大正文放大 capture 队列内存 |
| `read_only` Admin | 所有 Admin GET | session、可配置 MFA、no-store、审计链 | 所有 GET 包括 pre-redaction failure payload；默认 MFA 为 optional，无 Project scope |
| administrator | GET 与变更 | mutation 自动带 CSRF、role gate；危险动作复用 step-up | 真实身份代理/RBAC 与目标环境 MFA 未验证 |
| Metrics scraper | 读 `/metrics` | 独立 listener/TLS/credential 配置 | 生产 mTLS/credential 轮换与高基数预算未验证 |
| Provider / webhook | 接收经过策略后的请求或告警 | SafeTransport；无环境代理/redirect；DNS 后 pin IP；host/IP policy | Provider 自身执行工具产生的出网不经过 Halro；产品对此有显式 capability 提示，属剩余风险 |
| KMS | 包装/解包 Master Key | key slot、fingerprint、startup lifecycle；无真实调用 | IAM、区域、节流、撤权、灾难恢复未在本轮验证 |
| backup operator | 离线读取一致状态并发布 restore | data-dir lock、专用 backup key、认证 archive、确认 ID、atomic rename/rollback | 外部保管、immutable store、RPO/RTO 与撤销身份复核依赖部署环境 |
| dead-man | 独立读取 Halro/Prometheus/Alertmanager 并发通知 | HTTPS、CA、bearer/mTLS、凭据文件权限、持久有界 outbox | “独立”只能由真实故障域证明；仓库明确标成 BLOCKED |

### 3.2 完整正文与 secret 链

1. Gateway API 先从 `Authorization` 提取 bearer；无凭据时在读取 body 前返回 401，之后才用 `http.MaxBytesReader` 解码正文（`internal/gatewayapi/handler.go:64-92`，router 顺序见 `internal/app/runtime.go:1652-1683`）。
2. 正常调用保留解码后的 northbound 请求和 normalized 请求用于当前 request run（`internal/gateway/service.go:433-441`）。日志、metrics、Ledger/Usage 的主路径使用结构化 identifier、状态、token/cost，而不是正文；本轮未发现 Gateway credential 或 Provider secret 被显式写入这些表面。
3. `gateway.failure_capture.enabled=false` 是默认值；启用后仅在 `provider_error`/`unsupported_feature` 等选择性失败上存原始解码正文、normalized 请求和结构化上游错误，不存 headers/Gateway credential（`internal/config/default.yaml:186-202`，`internal/gateway/capture.go:274-309`）。
4. 存储前逐字段截断，record 按 `(request_id, project_id)` envelope 加密；目录/文件权限、日限额、retention 和 read-time TTL 共同限制持久暴露（`internal/failurecapture/failurecapture.go:266-334`）。关闭新捕获后仍打开旧目录执行 purge，避免“关闭功能即永久保留”（`internal/app/failure_capture.go:17-42`）。
5. payload 只能经 Admin GET 返回；handler 先定位并认证解密，再在输出 body 前写 `usage.failure_payload.read` audit；audit 不可写时返回 503 并 withholding（`internal/app/failure_capture.go:76-164`）。
6. file/KMS Master Key rotation 都在发现 `failures/` 或 `provider-objects/` retained ciphertext 时 fail closed，避免轮换后遗留密文不可读（`internal/app/retained_ciphertext_rotation.go:13-53`、`internal/app/key_rotation.go:180-191`、`internal/app/kms_key_lifecycle.go:844-851`）。
7. 加密 backup 明确不携带 Master Key 或 backup key；它也不是 failure capture 的长期归档。恢复会认证 archive、Ledger/Governance/Usage 与 key slot，并使已恢复 Admin session 失效、隔离 scheduled price，再原子发布数据目录；具体外部密钥保管和撤销复核仍属于 operator 责任。

### 3.3 SafeTransport 反证

`safetransport.NewClient` 显式禁用环境代理和压缩，禁止 redirect，设置连接/响应头/连接池边界；`ValidateURL` 拒绝非 HTTP(S)、userinfo、未批准 host 和 IPv6 zone；dial 时重新解析并验证全部地址，只连接本次已验证的确切 IP，因此 URL 校验后的 DNS rebinding 不能换入私网地址（`internal/safetransport/transport.go:41-83,118-149,177-225`）。默认远程 model catalog 还固定 HTTPS host、单连接，并限制压缩体、解压体、compression ratio 和条目数（`internal/modelcatalog/manager.go:111-178,334-378`）。

反证边界：允许 private endpoint 是 operator 的显式配置；Provider 自己执行 code/MCP/web-search 的二次出网不受本进程 socket policy 控制。后者不应被描述成 SafeTransport 漏洞，但应继续以 capability 与 UI 警告表现为信任转移。

## 4. 可靠性、恢复与告警闭环

### 4.1 已成立的结构

- `RunWithReady` 在 ready callback 前加载 TLS 并成功 bind 所有 listener；任一 bind 失败会关闭已绑定 listener（`internal/app/runtime.go:1307-1359`）。
- `/health/live` 与 `/health/ready` 位于 source limiter 之外，避免 orchestrator 自己把实例探测成不健康；ready 在 draining、Accounting 不健康、pricing quarantine 或 activation stale 时返回 503（`internal/app/runtime.go:1652-1669,1929-1965`）。它没有声称 Provider 可达，语义诚实。
- data directory writer 使用 sibling publication lock 与 `.halro.lock` 非阻塞独占 `flock`；初始化、普通运行和离线 read-only 工具共享互斥边界（`internal/store/lock/lock_unix.go:20-101`）。Kubernetes 参考清单同时固定 `replicas: 1` 与 `Recreate`（`deploy/kubernetes/halro-aws-kms.yaml:8-28`），INV-13 是硬约束。
- backup 是 offline 一致性单元；archive 路径、重复项、symlink/hardlink、schema、hash、认证 tag 与 key fingerprint 被验证，restore 在同 filesystem staging 后原子 rename，失败可回滚。代码与 `docs/guides/backup-restore.md` 对适用边界的表述一致。
- dead-man 对 notification 采用持久化后发送；outbox 有 16..65536 显式范围，达到上限拒绝继续排队；heartbeat 会合并旧值避免 outage 后旧心跳延长 receiver TTL（`internal/deadman/config.go:107-194`、`internal/deadman/engine.go:257-305`）。
- metrics 已暴露 deferred expiry/interruption、failure capture saturation、Usage window、WAL append/sync、project-lock wait、queue lag、provider/deployment health、KMS、alert delivery 等诊断信号（例：`internal/app/metrics.go:145-190,320-365,403-465`）。

### 4.2 尚未闭环

随仓库交付的 Prometheus rules/runbook 未引用 `halro_failure_capture_saturated`、`halro_deferred_responses_expired_total`、`halro_deferred_responses_interrupted_total`、`halro_shutdown_truncated_attempts_total`、pending leases 或 pricing quarantine。尤其 metrics 源码自己说明 capture saturation 的一次日志“不适合 operator 需要注意的事”，但目前止于 metric。

仓库对更大的边界是诚实的：`docs/observability/admission-checklist.md:15-34` 将 target PKI、credential lifecycle、真实 Contact Point、dead-man 独立性、TSDB failure、SSRF、immutable audit、backup/restore、24h capacity/soak 全部保留为 `BLOCKED`；`docs/observability/implementation-evidence.md:40-46` 明确它们不可由仓库验证。这种“不把 fixture 说成生产”的证据纪律应保留。

## 5. SLI/SLO 与 error-budget 草案

以下是待 owner 与部署方批准的测量契约，不是当前已达到的数字。先按请求类型拆分 `unary`、`streaming`、`deferred/resource`，并同时展示 `all` 和 Halro-attributable 子集；Provider 5xx/429/网络慢不应从总用户体验中删除，但不能记为 Halro 自身回归。

| 用户旅程 / SLI | 建议定义 | 初始 SLO 草案 | 责任边界与所需新增证据 |
| --- | --- | --- | --- |
| 合法请求准入可用率 | 已认证、policy/budget/capability 允许的请求中，未因 Halro 内部 accounting/config/storage 错误在 Provider I/O 前失败的比例 | 30d `>=99.95%` | policy/budget 拒绝不进分母；需从 request + attempt terminal 事件形成一致计数 |
| Halro-attributable 完成率 | 已准入请求中，未因 Halro panic、deadline、write/settlement、adapter malformed 或 shutdown truncation 失败的比例 | unary 30d `>=99.9%`；stream 单独定 | 同屏保留 Provider-attributable 全量成功率；歧义分类必须可审计 |
| Gateway 附加延迟 | northbound 总延迟减去首次 Provider attempt 的 connect/TTFB/完成区间 | unary p95 `<25 ms`、p99 `<100 ms`，先用目标硬件校准 | 当前 metrics 不足以可靠相减；不得用 mock Provider 总延迟冒充附加延迟 |
| streaming 首字节与完成 | 首个可交付 northbound SSE byte；成功 terminal event 到 close 的完整率 | TTFB p95 目标按 Provider/profile 分桶；已首字节后的 Halro 中断 `<0.1%` | 慢读者与断流需容量曲线；禁止首字节后 fallback 是不变量而非 error budget |
| Accounting 完整率 | 每个 accepted request 恰有一个 authenticated finalization；每个 attempt reservation 恰有一个 terminal settlement/recovery | `100%` 安全目标，不允许用 error budget 消费 | 这是账务 invariant；任何缺失触发变更冻结，而不是允许月度误差 |
| 配置激活时间 | durable Admin mutation commit 到所有相关 snapshot 生效且 readiness 恢复的时长 | p99 `<5 s`，超时必须明确 not-ready | 需 revision 关联 metric，不能仅用最终 readiness 采样推断 |
| 恢复能力 | 从声明故障开始，到 restored instance 通过 doctor、chain 校验、ready 和受控 smoke；RPO 以最后 authenticated committed record 衡量 | 参考数据集 RTO `<=120 s`；durable committed Ledger RPO `0` | 要固定 archive 大小、磁盘、KMS mode、operator 起点；现有单机历史数字不能替代目标环境 |
| 检测时间 | 用户影响开始到 actionable page 到达独立 receiver | 关键不可用 MTTD `<5 min` | 必须真实停止 Halro/Prometheus/Alertmanager 并验证 firing/resolved 与 heartbeat-loss |

建议 error-budget 政策：Accounting/越权/secret 泄漏不设可消费预算，命中即冻结风险发布；其他 Halro-attributable SLO 在 30 天预算消耗超过 50% 时只允许可靠性修复，超过 100% 时停止功能发布。没有生产流量时，用经过审计的 synthetic + target staging 分母启动测量，不制造“100%”历史。

## 6. Findings（角色本地候选编号）

### PHIL-CD-001 — failure capture 在字节截断前把完整请求副本放入 64 槽队列

- 类型：DEFECT
- 维度与原则：D5、D8；安全诊断不应扩大主数据面的拒绝服务面，资源上界必须按字节而不只按条数
- 严重度：P1
- 置信度：MEDIUM
- 状态：CANDIDATE
- 入口与可达条件：operator 显式开启 `gateway.failure_capture`；持有效 Gateway Key 的客户端发送接近 `server.max_request_bytes` 的合法 JSON；请求到达 Provider 后以可捕获结果失败；capture store 因慢盘、锁、加密或测试 sink 阻塞，使异步 worker 不能及时出队。
- 代码与文档证据：`internal/config/default.yaml:31-33,186-202`；`internal/config/config.go:1150-1155,1409-1420`；`internal/gatewayapi/handler.go:64-92`；`internal/gateway/service.go:433-441,472-500`；`internal/gateway/capture.go:22-38,70-103,274-324`；`internal/failurecapture/failurecapture.go:266-334`；`internal/gateway/capture_test.go:326-359`。
- 最小复现与原始证据：现有 `TestFailureCaptureShutdownCancelsAnActiveWrite` 证明阻塞 sink 能占住一个 active write 并填满其余 64 槽；本轮 `go test -race -count=1 ./internal/gateway -run 'TestFailureCapture|TestShutdownFailureCapture|TestCaptureQueue'` 通过。静态链证明 `encodeCaptured` 在 enqueue 前对 northbound 与 normalized 两侧执行无上限 `json.Marshal`，而 `truncateJSON(..., s.maxBytes)` 到 `PutContext` 才执行。默认 `max_request_bytes=10 MiB` 下，队列正文理论量约为 `64 × 2 × 10 MiB = 1.25 GiB`，尚未计 decoded object、active write 和 marshal 瞬时副本；本轮为避免人为 OOM 未执行大内存复现。
- 违反的不变量或用户承诺：INV-10；诊断能力不能让已认证单 Project 流量把网关推入内存压力/OOM。
- 用户影响、爆炸半径和可恢复性：进程 RSS 激增、GC pause、OOM/重启，影响该单实例上的全部 Project。重启可释放内存但会丢失未落盘诊断；账务持久状态应仍由其既有恢复路径处理。若 operator 增大 `server.max_request_bytes`，当前校验只要求正数，进程级上界随配置任意放大。
- 已有防御与反证：capture 默认关闭；入口需认证；source rate 默认 600/min；队列条数固定 64，满后 drop；持久层每字段默认截断 64 KiB、最大 1 MiB，并有日限额与 retention。反证不足在于这些字节/retention 控制均未限制 enqueue 前的两次 marshal 和 queue retained bytes。
- 建议处置：SIMPLIFY。把 capture byte limit 带到 gateway，在 enqueue 前按每侧上限编码/截断，或用总 queued-bytes semaphore；继续保留固定条数与 drop-on-full，不引入新服务。
- 建议回归或验收：blocking fake store + 接近 10 MiB 的合法请求，断言 queued payload 总字节不超过 `capacity × per_record_limit`、truncation flag 正确、请求响应不等待 capture、race 通过；再采集 RSS/GC 曲线而不是只断言 `len(queue)==64`。
- 成本、owner、期限：Gateway + Security；小到中等；下一个启用 failure capture 的生产版本前。

### PHIL-CD-002 — `read_only` Admin 的“所有 GET”权力包含 pre-redaction 客户正文

- 类型：DESIGN_DEBT
- 维度与原则：D5；最小权限与敏感数据访问应按数据等级裁决，同时避免全局 permission matrix
- 严重度：P2
- 置信度：HIGH
- 状态：CANDIDATE
- 入口与可达条件：failure capture 已由 operator 开启且存在记录；攻击者拥有一个有效 `read_only` Admin session。默认 `admin.mfa_policy=optional` 时该账号不强制 MFA，也不要求 payload-read step-up。
- 代码与文档证据：`internal/domain/admin.go:9-18` 明确 `read_only` 是所有 GET、无 endpoint exception；`internal/config/default.yaml:58-83` 与 `internal/config/config.go:352-364`；payload route 使用 `requireAdmin`（`internal/app/runtime.go:1760-1764`），该 middleware 只做 session 与实例 MFA policy（`internal/app/admin_session.go:258-297`）；handler 返回正文前会审计但没有 role/project scope（`internal/app/failure_capture.go:76-164`）。
- 最小复现与原始证据：静态完整链；`internal/app/admin_usage_failures_test.go:222-270` 证明普通 Admin session 可取回捕获正文且产生 audit。现有测试未用 `read_only` 账号直接钉住该敏感 GET，因此留待 S5 独立复现。
- 违反的不变量或用户承诺：不构成当前 ACL 实现绕过；它挑战 D5 最小权限和 failure capture 的“显式风险接受”质量。若团队明确把 `read_only` 定义成可读全部客户内容，则应升级为清楚、可审计的 accepted constraint，而不是默示继承 GET 规则。
- 用户影响、爆炸半径和可恢复性：一个只需观察配置/运行状态的账号也能读取 retention window 内所有 Project 的失败 prompt/response；读取已审计但不能撤回已泄露内容。
- 已有防御与反证：功能默认关闭、记录加密/截断/过期、Admin session 必需、可配置全员 MFA、每次读取先持久审计且 audit failure 时 fail closed。简单的“两角色 + mutation gate”显著降低新增写接口漏授权风险，不能轻率替换成大 permission matrix。
- 建议处置：SIMPLIFY。只对这一条敏感读取复用既有 administrator + fresh re-auth/step-up primitive；`read_only` 继续读取 failure summary。若决定继续允许，应在角色创建、UI 与文档明确“可读捕获的客户正文”，并强制所有此类账号 MFA。
- 建议回归或验收：创建 read_only 与 administrator fixture；前者 payload GET 得 403、仍可读 summary，后者无 fresh step-up 得 403、完成 step-up 后 200；audit unavailable 时两者均不得收到正文。
- 成本、owner、期限：Security + Admin API；小；30 天内裁决。

### PHIL-CD-003 — 已导出的关键退化信号没有随仓库交付的告警与处置链

- 类型：DESIGN_DEBT
- 维度与原则：D4；Google SRE 的 symptom/actionable alert 与 15 分钟诊断闭环
- 严重度：P2
- 置信度：HIGH
- 状态：CANDIDATE
- 入口与可达条件：failure capture 达日上限或 queue/store 退化；deferred response 过期/被重启中断；shutdown 截断 accepted attempt；accounting 有 pending lease；restored price 被 quarantine。对应代码路径可在合法流量、重启或恢复中到达。
- 代码与文档证据：metrics 定义见 `internal/app/metrics.go:145-190` 及相关 accounting/shutdown/pricing 段；源码在 `:169-171` 明说一次日志不是 operator 需要的形状。对 `deploy/observability/prometheus` 与 `docs/observability` 执行 `rg -n 'failure_capture|capture.*satur|deferred.*(expire|interrupt)|shutdown_truncated|pending_leases|pricing.*quarant'` 无结果。
- 最小复现与原始证据：`go test -count=1 ./deploy/observability` 通过，只证明现有规则自洽；它没有上述 series 的规则。failure capture saturation 可由 fake store/daily ceiling 确定性触发，未连接真实 receiver。
- 违反的不变量或用户承诺：不直接违反 INV-04/05/10/13；违反“关键退化必须可发现、可定位、可安全动作”的 D4 目标。
- 用户影响、爆炸半径和可恢复性：系统可能继续 serving，但 operator 丢失用于重现真实失败的 capture、丢失 deferred work，或在 shutdown/recovery 后保留会计风险，而没有 page/runbook 告知影响与动作。问题通常可恢复，但证据窗口可能不可逆丢失。
- 已有防御与反证：signals 已存在；日志至少给一次本地线索；现有 rules 已覆盖 target down、activation stale、WAL error、Usage lag、provider/fallback/KMS/TSDB 等大量核心故障。并非每个 metric 都应 page，低频诊断可以 ticket/recording rule。
- 建议处置：PROVE。按用户症状挑最小集合：accepted-work/accounting 风险 page；capture/deferred 证据损失 ticket；给 firing/resolved、抑制、阈值依据和明确 runbook，避免“所有 metric 都告警”。
- 建议回归或验收：`promtool test rules` 覆盖触发、不触发、resolved 与 inhibition；fake Alertmanager 验证 payload 无正文/secret；非作者从 page 在 15 分钟内找到影响、停止扩大和恢复步骤。
- 成本、owner、期限：SRE + Accounting/Gateway owner；中等；30 天内定义，60 天内 target drill。

### PHIL-CD-004 — 生产可靠性与容量仍只有 admission 清单，没有运行中的 SLO/error-budget 证据

- 类型：EVIDENCE_GAP
- 维度与原则：D4、D8；SLO 驱动风险管理，测量必须匹配生产形状
- 严重度：P2
- 置信度：HIGH
- 状态：UNVERIFIED
- 入口与可达条件：任何“已具备生产可靠性/容量”或“性能无回归”的对外主张。
- 代码与文档证据：`docs/observability/admission-checklist.md:15-34` 和 `docs/observability/implementation-evidence.md:40-46` 明确生产 PKI、真实通知、独立 dead-man、灾备与 24h soak 均未取得 evidence ID；仓库没有已批准的 Gateway 用户视角 SLO/error-budget 记录。
- 最小复现与原始证据：本轮只有窄单测、规则测试与四个同机微基准；未运行 real Provider/KMS/alert、目标容器/磁盘、完整 binary smoke 或 soak。
- 违反的不变量或用户承诺：不等同产品 defect；禁止把 E2 fixture/E3 微基准外推为 E4 生产可靠性。
- 用户影响、爆炸半径和可恢复性：无法量化真实错误预算、最先饱和资源、检测/恢复时间或升级风险，因此发布速度只能依赖主观判断。
- 已有防御与反证：仓库没有伪造绿色状态，而是正确保持 `BLOCKED`；metrics、rules、dead-man、backup/restore 与 capacity 脚手架已具备，补证不要求重写系统。
- 建议处置：PROVE。批准本报告第 5 节的 SLI 定义，先在 disposable target 环境执行 staged failure/capacity matrix，再决定数字；不要为了得到绿色结果降低阈值。
- 建议回归或验收：绑定 immutable evidence ID，记录 SHA、配置、fixture、硬件、原始时序、资源曲线、firing/resolved 与 restore watermark；至少一次 24h production-sized soak 和非作者 recovery drill。
- 成本、owner、期限：SRE + Platform + Security + Application；中到大；60–90 天，独立于本地代码发布判断。

## 7. 性能与容量证据

### 7.1 同机 paired 微基准

环境：Apple M4、Darwin arm64、Go 1.26.6、`GOMAXPROCS=4`。目标工作树为固定 SHA；前一版使用 `git archive v0.7.1` 解到 `/private/tmp`，无共享数据目录、无网络调用。命令：

```text
go test -run '^$' \
  -bench 'BenchmarkRegistryResolveCandidates|BenchmarkStandardRedaction|BenchmarkRollingRedaction|BenchmarkReplayLargeWAL' \
  -benchmem -count=3 -benchtime=500ms \
  ./internal/provider ./internal/redaction ./internal/ledger
```

| Benchmark | v0.7.1 中位数 | v0.8.0 中位数 | 方向性变化 | 分配变化 |
| --- | ---: | ---: | ---: | ---: |
| RegistryResolveCandidates | 14,005 ns/op | 20,342 ns/op | `+45.2%` slower | 15,888 → 16,703 B/op (`+5.1%`)，41 allocs 不变 |
| StandardRedaction | 146,829 ns/op | 181,280 ns/op | `+23.5%` slower | 13,786 → 13,788 B/op，32 allocs 不变 |
| RollingRedaction | 53,157 ns/op | 55,355 ns/op | `+4.1%` slower | 17,976 → 17,878 B/op，79 allocs 不变 |
| ReplayLargeWAL | 1.319 s/op | 1.176 s/op | `-10.8%` faster | 282.2 → 307.9 MB/op (`+9.1%`)，约 3.0M allocs 不变 |

原始 `ns/op` 样本（均按命令输出顺序，`v0.7.1 → v0.8.0`）：Registry `[14333, 14005, 13326] → [23122, 20342, 18505]`；StandardRedaction `[143500, 146829, 166161] → [193964, 181280, 169919]`；RollingRedaction `[33883, 53157, 55630] → [48928, 58114, 55355]`；ReplayLargeWAL `[1317679917, 1318750125, 1470413625] → [1467583292, 1176193750, 1164828709]`。

原始 3 样本范围很宽：目标 Registry 为 18.5–23.1 µs，StandardRedaction 为 169.9–194.0 µs；每次 WAL benchmark 只有一次 iteration。执行顺序也未交替。因此前两项超过 15% 只能标成复测信号，不能称统计显著 regression；WAL 时间改善也不能冲销累计 allocation 增长。建议用仓库既有 paired/alternating harness 各 8–10 样本和 `benchstat` 复核，再做 flamegraph/pprof 定位，不先猜原因。

### 7.2 上界审计摘要

已看到显式上界：HTTP body/header/timeouts、retry 次数/backoff、Provider/deployment concurrency、source tracking、deferred workers/TTL、failure capture 单字段/日/retention、catalog 下载/解压/比例/条数、dead-man outbox、Usage window、Admin pagination、多种 Provider response limit。

需要补证或加固：

- capture 队列按条数而非总字节限制，见 PHIL-CD-001；
- `server.max_request_bytes` 只校验正数，没有进程安全上限；即使允许 operator 调大，也应在 capacity 文档给出 RSS 放大模型；
- provider/deployment IDs 被作为 Prometheus labels 输出（`internal/app/metrics.go:403-462`），而相邻代码因 Project 数不受限而刻意不加 project label（`:335-338`）。本轮未找到 provider/deployment 总数上限，也未用大规模 topology 测 TSDB，因此记录为 `UNVERIFIED` cardinality 风险，不直接升级为 defect；
- 没有对 streaming 慢读、Provider trickle、backup archive size、audit/ledger 日增长、Admin 大表或 24h goroutine/FD/RSS 做当前 SHA 曲线。

## 8. 运行证据与限制

### 8.1 当前执行记录

| 命令 | 结果 | 能证明什么 |
| --- | --- | --- |
| `go test -count=1 ./internal/safetransport ./internal/failurecapture ./internal/deadman ./internal/backup` | 首次 mixed：SafeTransport/failurecapture 通过；backup 因只读 module-cache lock、deadman 因 sandbox 禁 loopback 失败 | 首次失败属于执行环境，不能判产品失败 |
| 在允许 loopback/依赖缓存的隔离环境重跑 `go test -count=1 ./internal/deadman ./internal/backup ./internal/adminauth ./internal/app -run 'Test(RetainedCiphertext|Ready|Readiness|FailureCapture|FailurePayload|Metrics|Single|Lock|Backup|Restore|AdminUsageFailure|KMSSecretCanary|PrivateEndpoint|GatewayContract|Anchor)'` | PASS；deadman 2.262s，backup 0.663s，app 18.307s；adminauth 无匹配测试但编译通过 | 目标 SHA 上相关 fixture/负向路径 E2；不证明真实 PKI/KMS/告警 |
| `go test -race -count=1 ./internal/gateway -run 'TestFailureCapture|TestShutdownFailureCapture|TestCaptureQueue'` | PASS，1.854s | capture 并发/关闭已有测试在 race 下通过；不覆盖大字节队列 |
| `go test -count=1 ./deploy/observability` | PASS，2.714s | 随仓库规则/配置测试通过；不证明通知送达 |
| 同机 `v0.7.1` 与目标 SHA 四项 benchmark，各 3 样本 | 全部 PASS | 局部 E3 方向性数据；不是容量曲线或统计显著结论 |

未运行全量 `go test ./...`：本角色没有产品代码变化，且全量门由评审负责人统一执行更符合仓库验证政策。未运行 Docker smoke、真实网络、真实凭据、billable smoke、KMS、Alertmanager Contact Point、生产数据或现有数据目录。

### 8.2 证据等级纪律

- D4 的最高有效证据为 E2：代码、fixture 与规则测试；target-environment alert/recovery 仍为 E0/E1 声明。
- D5 的最高有效证据为 E2：SafeTransport、capture、rotation、backup/auth 的 local negative tests；没有 E3 安全环境或 E4 外部复核。
- D8 仅四个 microbench 达 E3；容量、尾延迟、资源泄漏和生产形状仍是 E0/E1/EVIDENCE_GAP。
- 本报告作者也是 findings 发现者，不能把自我复读称作独立证伪；P1 与结构性 P2 必须交给角色 F。

## 9. 临时评分

| 维度 | 成熟度 | 最高证据 | 加权贡献 | 理由 |
| --- | ---: | --- | ---: | --- |
| D4 可靠性与可运维性（14） | 2.0 / 4 | E2 | 7.0 / 14 | readiness、单写、恢复、metrics/dead-man 设计扎实；没有 SLO/error budget、真实通知和 target drill，若干退化信号未闭环 |
| D5 安全与隐私（16） | 2.0 / 4 | E2 | 8.0 / 16 | 默认关闭 capture、SafeTransport、加密/审计/rotation fail-closed 强；raw-body RBAC 边界待裁决，且无 E3 KMS/PKI/heap/外部安全证据 |
| D8 性能与容量诚实度（5） | 1.5 / 4 | 局部 E3 | 1.875 / 5 | 多数内部结构有上界且文档不冒充生产；但 capture 有字节放大，paired benchmark 只给方向性信号，容量与尾延迟大部未测 |
| **角色小计** | — | — | **16.875 / 35** | 仅供总评负责人汇总；不能外推为 48.2/100 或系统总 verdict |

## 10. 建议优先级

1. 在任何 production enablement 前修正或证伪 capture enqueue 前的字节放大，并用 RSS/GC 证据验收。
2. 对 failure payload GET 作一次明确的权限决定：administrator + step-up，或把 read_only 可读客户正文写进风险接受、角色 UI 和 MFA 强制策略。
3. 从已存在 metrics 中补最小 actionable alert/runbook 集合，并做 firing/resolved + 15 分钟非作者 drill。
4. 批准用户视角 SLI，再采 target staging/production-sized evidence；不要从现有 metric 倒推出漂亮 SLO。
5. 用交替 8–10 样本复核 Registry/StandardRedaction 信号；完成慢流、RSS/FD/goroutine、WAL/Journal/TSDB 增长和 24h soak。
