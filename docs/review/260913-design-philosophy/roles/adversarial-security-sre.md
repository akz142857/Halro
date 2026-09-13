# 角色 F：安全、SRE 与容量 findings 独立反证

> 被反证对象：`roles/security-sre-performance.md` 中 `PHIL-CD-001..004`
>
> 目标源码：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`
>
> 角色关系：本角色不是四条 finding 的发现者或作者
> 边界：只读代码/文档与窄测；未修改产品代码，未调用真实 Provider、KMS、Contact Point 或生产环境

## 1. 裁决总览

| Finding | 对抗裁决 | 最终严重度建议 | 核心理由 |
| --- | --- | --- | --- |
| PHIL-CD-001 failure capture 队列字节放大 | **PARTIAL** | **P2**；有大载荷 RSS/GC 实证后可重审 P1 | enqueue 前确实保留两份未按 capture limit 截断的 JSON，64 槽按条数不按字节；但 1.25 GiB 是可达上界模型而非实测，功能默认关闭，攻击链还要求有效 Gateway Key、可捕获终态和阻塞 sink。缺陷成立，原 P1 证据不足。 |
| PHIL-CD-002 `read_only` 可读 pre-redaction payload | **REFUTED**（作为 P2 设计债） | **撤销 P2；按当前产品模型记为 ACCEPTED_CONSTRAINT** | 读取事实成立，但不是默示继承或门禁遗漏：两角色契约明确是 read-only 可读所有 GET/用量视图，代码特意按该读者清除 Provider prose，当前安全回归还明确断言 read-only 取得 200。 |
| PHIL-CD-003 关键信号没有告警/处置链 | **PARTIAL** | **P2，范围收窄** | 五类信号确实没有直接 Prometheus rule；但 pricing quarantine 已由 readiness→dead-man 覆盖，pricing/pending lease 有 doctor/指南，通用错误/容量告警也覆盖部分症状。应只保留 shutdown-truncated 与 aged pending lease 的强闭环缺口，capture/deferred 更适合作为 warning/ticket。 |
| PHIL-CD-004 没有运行中的 SLO/error budget | **REFUTED**（作为当前 P2） | **撤销 P2；v0.x 为 ACCEPTED_CONSTRAINT，生产主张仍 UNVERIFIED/No-Go** | 缺口事实存在，但仓库与 v0.8.0 发布裁决已经明确接受，且 Production Admission 在缺 evidence ID 时强制 No-Go；当前没有把本地 fixture/微基准宣传为生产 SLO。 |

独立反证没有推翻 PHIL-CD-001 的实现缺口，也没有证明 1.25 GiB 估算错误；它推翻的是“现有证据已足够把它定为 P1”。对 PHIL-CD-002 和 PHIL-CD-004，事实部分存在，但原 finding 试图继续赋予缺陷严重度，与代码/产品已经明确选择并强制执行的约束不符。

## 2. 本次复现记录

```text
git rev-parse HEAD
1d48ecde216ef40e653738f8bdc9657f88a717cc

git describe --tags --exact-match HEAD
v0.8.0
```

### 2.1 failure capture 队列与关闭

```text
GOCACHE=/private/tmp/halro-adversarial-go-cache \
go test -count=1 ./internal/gateway \
  -run '^(TestASlowCaptureDoesNotHoldTheRequestsLeases|TestFailureCaptureShutdownCancelsAnActiveWrite)$'

ok github.com/akz142857/Halro/internal/gateway 2.844s
```

证明范围：阻塞 sink 不延迟调用方/不继续占请求 lease；一个 active write 可阻塞，队列固定容纳 64 条，超出 drop；shutdown cancellation 能释放 worker。它没有装入大正文，也没有测 RSS/GC。

### 2.2 `read_only` payload 的当前运行契约

沙箱内第一次执行因 `httptest` 不能 bind `[::1]:0` 失败；在允许本机 loopback、仍无外部网络的隔离执行中重跑：

```text
GOCACHE=/private/tmp/halro-adversarial-go-cache \
go test -count=1 ./internal/app \
  -run '^TestEchoedAuthorizerSecretNeverLeavesTheProviderBoundary$'

ok github.com/akz142857/Halro/internal/app 1.562s
```

该测试通过真实 Runtime/router 创建 `read_only` 用户，再对捕获 payload GET 断言 HTTP 200，同时验证 Provider credential canary 不进入 payload、响应、日志、Audit 或磁盘（`internal/app/failure_capture_security_test.go:26-190`）。这是目标 SHA 的 E2 证据；沙箱首败仅是 loopback 权限，不是产品失败。

### 2.3 告警规则与 dead-man 逻辑

```text
GOCACHE=/private/tmp/halro-adversarial-go-cache \
go test -count=1 ./deploy/observability
ok github.com/akz142857/Halro/deploy/observability 1.242s

GOCACHE=/private/tmp/halro-adversarial-go-cache \
go test -count=1 ./internal/deadman \
  -run '^(TestTransitionUsesFailureAndRecoveryHysteresis|TestInitialHealthyObservationDoesNotEmitFalseRecovery)$'
ok github.com/akz142857/Halro/internal/deadman 0.596s
```

规则测试只证明已交付规则自洽；它不证明真实 Contact Point。dead-man 窄测证明失败/恢复迟滞状态机，结合交付配置确实把 Halro `/health/ready` 作为独立探测目标（`deploy/observability/external-probe/config.example.yaml:29-38`）。

## 3. 逐条裁决

### PHIL-CD-001 — failure capture 在字节截断前把完整请求副本放入 64 槽队列

- 原始类型与严重度：DEFECT，P1，MEDIUM
- 对抗裁决：PARTIAL
- 最终严重度建议：P2，HIGH；大载荷 RSS/GC/请求延迟复现若达到进程级不可用，再升级 P1
- 反证目标：确认队列里到底是什么对象、单条能否接近两倍请求上限、哪些入口能到达、默认是否暴露，以及现有防御能否在 enqueue 前生效。
- 代码与文档证据：`internal/gateway/capture.go:14-38,40-103,146-186,188-210,274-324`；`internal/failurecapture/failurecapture.go:255-334`；`internal/gateway/service.go:433-441,472-500`；`internal/gatewayapi/handler.go:64-103`；`internal/config/default.yaml:20-33,180-202`；`internal/config/config.go:1139-1155,1395-1423`；`internal/gateway/capture_test.go:260-359`；`docs/guides/operator-guide.md:607-690`。

#### 成立部分

1. `writeCapture` 在请求 goroutine 中分别对 `capturedGatewayRequest`、normalized `capturedRequest` 和安全 response shape 调用 `json.Marshal`，再构造 `failurecapture.Record`（`internal/gateway/capture.go:274-324`）。
2. channel 元素是 `failurecapture.Record`。channel 复制 struct/slice header，但每条 `json.RawMessage` 的 backing bytes 继续存活；因此这不是“队列里只有几个指针”的反例。一个已进入 `PutContext` 的 active record 会在 `:312-314` 被截断，但尚未出队的 64 条不会。
3. `failure_capture.max_bytes` 的 64 KiB 默认和 1 MiB 配置上限只在 store `PutContext` 内应用（`internal/failurecapture/failurecapture.go:311-326`），晚于 enqueue。`max_records_per_day` 与 retention 也只限制落盘，不限制 queue retained bytes。
4. 默认 `server.max_request_bytes` 为 10 MiB，只校验正数、没有上限；capture queue 是固定 64 条。对能在 Gateway 形状和 normalized 形状中保留同一大内容的请求，`64 × 2 × 10 MiB ≈ 1.25 GiB` 是合理的默认配置数量级，而不是被 capture 64 KiB 上限限制的 8 MiB。
5. 请求到达 store 前不会等待 capture worker，队列满时 drop，保持了主请求 latency/lease 隔离；这防止阻塞扩散，但不回收已经排队的字节。

#### 削弱原结论的条件

1. 功能默认 `enabled: false`，且配置和 Operator Guide 明确说明它会保存 caller-written material；普通安装不存在这条队列（`internal/gateway/capture.go:14-16`；`internal/config/default.yaml:186-202`）。这排除了“默认不安全”。
2. 不是任意匿名大包都能入队。HTTP handler 在读 body 前要求语法有效的 bearer，真实 Key 校验发生在后续 service admission；只有通过有效 Gateway Key、Project/capability/budget 准入并得到 `provider_error` 或内部 `unsupported_feature` 的请求才会进入 capture。success、policy/token/budget/accounting rejection、caller cancellation 都不捕获（`internal/gatewayapi/handler.go:64-103`；`internal/gateway/capture.go:146-186,279-288`）。
3. 1.25 GiB 是保守上界模型，不是当前目标 SHA 的 RSS 实测。并非每种 10 MiB request 都会让两个 marshaled 形状各保留 10 MiB；例如会在 token/body/capability 检查前拒绝的形状不会捕获。可达样本更可能是大 image/base64 或 schema 经准入后遭遇 Provider 拒绝/认证故障，但本轮未运行大内存实验。
4. 要同时保留 64 条还需要 sink 足够慢/阻塞和失败流量持续到达。正常 64 KiB 截断与本地写盘会持续出队；已有 race/关闭测试不能证明正常磁盘上会积累到上界。

#### 裁决

资源上界放在了错误层级，**DEFECT 成立**；固定条数不能代替字节预算。但按方案，P1 必须是核心不变量可达失效或默认不安全。默认关闭、多条件入口、缺少大正文 RSS/GC 实证使当前 P1 不足，降为 P2。修复仍应在任何生产启用 capture 前完成：在 enqueue 前按每侧 capture limit 编码/截断，或维护 queued-bytes semaphore；同时保留 64 槽和 drop-on-full。

#### 验收要求

- blocking store + 真实 Gateway handler + 有效 Key + 接近 10 MiB 且能到达 Provider 的 payload；确认实际 `len(GatewayRequest)+len(Request)`。
- 连续填满队列，记录 peak RSS、heap allocation、GC pause 和调用方 p99；验收 queued bytes 有硬上限且与 `server.max_request_bytes` 解耦。
- 验证 capture disabled 时无额外 marshal/queue；capture enabled 时 success/rejection/cancel 仍不入队。

### PHIL-CD-002 — `read_only` Admin 的“所有 GET”包含 pre-redaction caller payload

- 原始类型与严重度：DESIGN_DEBT，P2，HIGH
- 对抗裁决：REFUTED（只反驳 P2 设计债定性；read-only HTTP 200 的事实已确认）
- 最终严重度建议：撤销 P2，不进入缺陷清单；按当前两角色产品模型记录为 ACCEPTED_CONSTRAINT
- 反证目标：判断 payload GET 是否偶然漏掉 administrator gate，或是代码、测试和用户界面共同表达的明确契约。
- 代码与文档证据：`internal/domain/admin.go:9-21`；`internal/app/admin_session.go:258-327,340-357`；`internal/app/runtime.go:1758-1764`；`internal/app/failure_capture.go:76-164`；`internal/gateway/capture.go:247-266`；`internal/app/failure_capture_security_test.go:26-190`；`web/src/i18n/locales/en-US.ts:174-190,642-656,875-887`；`web/src/i18n/locales/zh-CN.ts:184-200,650-664,875-888`；`web/src/pages/FailureDetailDrawer.tsx:150-219`；`docs/guides/operator-guide.md:607-690,912-951`；`docs/guides/user-guide.zh-CN.md:390`。

#### 反证

1. `AdminRoleReadOnly` 的定义明确是“GET only, no exceptions per endpoint”，并记录曾考虑、后拒绝 per-endpoint permission matrix（`internal/domain/admin.go:9-14`）。这不是 middleware 忘了查角色。
2. payload route 刻意使用 `requireAdmin`；handler 又把它定义成唯一返回 caller material、唯一在读之前写 Audit 的 GET，并在 Audit 不可用时 withholding（`internal/app/failure_capture.go:76-82,136-150`）。
3. capture 层明确以 `read-only administrator` 为威胁边界：上游 prose 可能回显 credential，所以只保存结构化 status/class（`internal/gateway/capture.go:247-266`）。如果产品本意是 read-only 不可读 payload，这段净化理由和相应回归不会成立。
4. 当前目标 SHA 的 `TestEchoedAuthorizerSecretNeverLeavesTheProviderBoundary` 创建 `read_only` 账号、通过真实 Admin router 读取 payload 并断言 200（`internal/app/failure_capture_security_test.go:139-156`）；本角色独立运行通过。
5. 控制台在常驻 banner 和创建账号的 role hint 中明示 read-only “every configuration and usage view is available”；payload 位于 Usage & Requests，并在点击前提示内容来自 Gateway/normalized/upstream、每次查看都审计。它不是隐藏 API。
6. default Admin listener 是 loopback，failure capture 默认关闭；远程 Admin 的 Operator Guide 要求 TLS/identity boundary，并建议当 read-only 可见的数据需要二次因子时使用 `mfa_policy=required`（`docs/guides/operator-guide.md:67-75,912-951`）。

#### 剩余约束

“read-only”不等于 Project-scoped support viewer。当前只有 administrator/read_only 两档，后者是实例级全读；启用 capture 的 operator 必须把它当作能读失败 prompt 的敏感身份。Operator Guide 对 payload 的内容和审计很清楚，但没有在同一句中点名 read_only，这可以做文案强化，不足以构成 P2。

若产品未来需要“看健康/配置但不能看正文”的第三种主体，应先用真实角色需求证明；不应仅为这一个 endpoint 引入全局 permission matrix。可选的最小强化是：failure capture 启用时，在创建/查看 read_only 账号处明确显示“包括捕获的失败请求正文”，并要求 operator 对远程此类账号启用 MFA。这是 KEEP/文档改进，不是修复越权。

### PHIL-CD-003 — 已导出信号缺少随仓库交付的告警与处置链

- 原始类型与严重度：DESIGN_DEBT，P2，HIGH
- 对抗裁决：PARTIAL
- 最终严重度建议：P2，但必须收窄为 `shutdown_truncated_attempts` 与“超过正常请求时长的 pending lease”；capture/deferred 建议 warning/ticket，pricing quarantine 从本 finding 删除
- 反证目标：检查缺少精确 metric 名称的 Prometheus rule，是否等于没有任何可发现/可处置路径。
- 代码与文档证据：`internal/app/metrics.go:145-181,281-320`；`internal/app/runtime.go:1406-1458,1935-1960`；`internal/app/doctor.go:165-188,265-278`；`deploy/observability/prometheus/alert-rules.yml:1-160`；`deploy/observability/external-probe/config.example.yaml:29-63`；`docs/guides/operator-guide.md:562-605,733-740,1170-1194`；`docs/contracts/metrics-reference.md:52-95,154-178`；`docs/observability/operations-runbook.md:1-160`。

#### 成立部分

对 `alert-rules.yml`、`recording-rules.yml`、`rule-tests.yml` 和 `operations-runbook.md` 搜索以下 metric 名称均无匹配：

```text
halro_failure_capture_saturated
halro_deferred_responses_expired_total
halro_deferred_responses_interrupted_total
halro_shutdown_truncated_attempts_total
halro_accounting_pending_leases
halro_accounting_oldest_pending_lease_age_seconds
halro_pricing_quarantined_deployments
```

因此“没有一对一的 Prometheus alert + alertname runbook”成立。特别是 shutdown truncated counter 被持久化，正是为了让重启后的 Prometheus 能看见上一进程无法暴露的终态（`docs/contracts/metrics-reference.md:165-178`）；如果没有规则读取它，这个设计目的只完成了一半。aged pending lease 同样关联已接受工作是否完成 settlement，不应仅等待人工 doctor。

#### 已有替代覆盖

1. **Pricing quarantine 不是静默的。** readiness 明确返回 503 `pricing: quarantined`（`internal/app/runtime.go:1947-1951`）；独立 dead-man 示例直接探测 `/health/ready`，所以符合部署契约时会产生 Halro down transition。offline doctor 也把 pricing readiness 作为 fail，Operator Guide 明确要求同时观察 quarantine metric、Accounting recovery、readiness 和 doctor。因此它没有专属 Prometheus rule，但已有更强的 symptom path，应从本 finding 删除。
2. **Pending lease 有诊断但没有自动 page。** doctor 报 count、oldest age、recovered released/settled 并在 pending>0 时 warn（`internal/app/doctor.go:165-188`）。数量本身不能 alert，因为正常 active Provider attempt 就可能持有 lease；只有 age 超过 route/stream/deferred 合法边界才是异常，原 finding 没给这个阈值。
3. **Deferred loss 有明确操作文档。** Operator Guide 解释 interrupted 可能已计费、expired 表示 arrival 超过 worker drain，并直接说应监控/告警对应 counter（`docs/guides/operator-guide.md:588-597`）。缺的是 shipped rule，不是处置知识完全不存在。
4. **通用症状有覆盖。** `HalroHighErrorRate`、deployment health、fallback saturation、provider capacity、WAL error、usage lag 和 target/dead-man 能发现部分共同症状；它们不能区分 capture evidence loss 或一次 shutdown truncation，但足以反驳“所有这些路径都无告警”。
5. **Failure capture saturation 不影响请求正确性。** capture 是 best effort，饱和只丢诊断证据；metric、一次本地 warning 和 UI 404 仍可见。它更适合 warning/ticket，而不是与账务不确定性同一 page 等级。

#### 裁决

聚合 finding 的方向正确、范围过宽。保留一个收窄的 P2：

- `increase(halro_shutdown_truncated_attempts_total[...]) > 0` 是明确、低噪声、与用户请求被强制截断相关的事件，应有 warning/page 和恢复核对 runbook；
- pending lease 应使用 `oldest_pending_lease_age_seconds` 相对配置的 route/stream/deferred 上界，而不是 `pending_leases > 0`；阈值确定前保持 UNVERIFIED，不能随意 page。

对 capture saturation、deferred expired/interrupted 建 warning/ticket rules，并由 owner 用实际 arrival/worker/重启策略定阈值。不要为 pricing quarantine 再造重复 page；验证 readiness→dead-man firing/resolved 即可。

### PHIL-CD-004 — 生产可靠性与容量没有运行中的 SLO/error-budget 证据

- 原始类型与严重度：EVIDENCE_GAP，P2，HIGH，原状态 UNVERIFIED
- 对抗裁决：REFUTED（作为当前 P2 finding）
- 最终严重度建议：撤销 P2；重分类为 ACCEPTED_CONSTRAINT。任何 production-SLO/production-ready 主张仍必须是 UNVERIFIED/No-Go
- 反证目标：确认仓库是否在缺 SLO/24h/真实告警/KMS 证据时仍宣称生产可靠，或者已经把缺口明确设为不允许越过的 admission constraint。
- 代码与文档证据：`docs/observability/admission-checklist.md:1-34`；`docs/observability/implementation-evidence.md:1-24,40-46`；`docs/verification/assessments/v0.8.0.md:1-21,127-134`；`docs/verification/standalone-capacity-baseline.md:1-24,38-97`；`docs/observability/capacity-model.md:34-66`。

#### 反证

1. Production admission 的权威文档明确规定：真实 Contact Point、独立 dead-man、TSDB failure、backup/restore、approved RPO/RTO、24h production-sized soak、upgrade/rollback 和四方签署全部 `BLOCKED`；任何 required row blocked/无 evidence ID/无签署时 Core 都是 No-Go（`docs/observability/admission-checklist.md:15-34`）。
2. Repository evidence 明确自限：本地 rules/smoke/dead-man artifact 不把 Phase D gate 标成通过，并列出不可由仓库验证的内容（`docs/observability/implementation-evidence.md:1-8,20-24,40-46`）。
3. v0.8.0 的精确发布裁决把真实 Provider/KMS、生产告警 receiver、24h soak 和完整辅助技术矩阵写成“owner 明确接受的 v0.x residual evidence boundary”，并明确说这不是已知 P0/P1 defect（`docs/verification/assessments/v0.8.0.md:8-21,127-134`）。
4. Capacity baseline 要求 exact SHA、硬件、durability、p50/p95/p99、RSS/goroutine/FD、WAL/fsync/recovery，并明说短 CI 只证明 harness、host-specific throughput 不是门禁（`docs/verification/standalone-capacity-baseline.md:1-24`）。当前没有用 microbench 偷换 production capacity。
5. 本角色未找到对外声称“v0.8.0 已达到某个生产 SLO/error budget”或“24h soak 已通过”的文本。缺 SLO 会降低 D4/D8 成熟度，但在 pre-1.0 且 admission fail-closed 的状态下，不自动成为 P2。

#### 裁决

事实“没有运行中的用户视角 SLO/error budget”成立；“因此当前存在 P2 finding”不成立。它满足 accepted constraint 的四个条件：

- **明确**：缺项逐行列出；
- **强制**：blocked/无 evidence ID 即 No-Go；
- **可观测**：admission 表和 exact-release assessment 可查；
- **不误导**：baseline 和发布裁决禁止把 fixture/微基准外推。

因此从当前 P2 清单撤销，保留为路线图和评分上限。若未来出现 production-ready、SLO 或容量承诺，而表中仍 blocked，则该具体主张应立即裁为 UNVERIFIED，且不能因本裁决获得豁免。

## 4. 建议进入主报告的净化版本

1. **P2 CONFIRMED/PARTIAL：failure capture enqueue 前无字节预算。** 不引用“已实测 1.25 GiB/OOM”；写成“默认配置可达数量级上界，需 RSS/GC 实证决定是否升 P1”。
2. **删除 PHIL-CD-002 的缺陷项。** 在 accepted constraints 中写清：read_only 是实例级全读，包括启用后捕获的失败正文；默认 capture off、读取按次审计且 audit fail-closed、Provider prose 已净化。可补一行更直白的账号创建文案。
3. **P2 PARTIAL：告警缺口收窄。** 优先 shutdown-truncated 与 aged pending lease；capture/deferred 为 warning/ticket；pricing quarantine 复用 readiness→dead-man，不重复告警。
4. **删除 PHIL-CD-004 的当前 P2。** 保留“D4/D8 最高证据不足、production claims UNVERIFIED、Production Admission No-Go”作为总评限制和后续 PROVE 项。

## 5. 覆盖限制

- 没有执行接近 10 MiB 的 capture storm 或人为 OOM；PHIL-CD-001 的 1.25 GiB 是源码约束推导，不是观测峰值。
- 没有真实慢盘/只读盘、生产 RSS/GC、真实 Provider 失败或多 Project 流量；capture 严重度仍可能被目标环境证据上调或下调。
- read_only 复现使用本机 `httptest` 与真实 Runtime/router/store，属于 E2，不代表生产身份代理、MFA 或浏览器残留 E3。
- 告警规则测试不证明 Contact Point 到达；dead-man 本轮只跑状态机窄测，readiness→真实独立 receiver 仍是 admission checklist 的 BLOCKED 项。
- 没有为 pending lease 建立合法最大年龄公式，也没有测 deferred arrival/worker distribution；相应阈值仍需 owner 批准。
- 本报告不改变 C/D 的 D4/D5/D8 成熟度评分，只净化四条 finding 的状态和严重度。
