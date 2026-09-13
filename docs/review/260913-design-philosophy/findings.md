# 经独立反证后的 Findings

目标版本：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`

本文件只保留非作者反证后的净化结论。发现过程、完整代码行号和原始命令见 `roles/`；裁决映射见
`adversarial-verdicts.md`。`PARTIAL` 表示核心事实成立，但原范围或严重度已被反证收窄。

## 1. 总览

| ID | 严重度 | 状态 | 类型 | 最终结论 |
| --- | --- | --- | --- | --- |
| PHIL-CD-001 | P2 | PARTIAL | DEFECT | failure capture 在 enqueue 前保留未按 capture limit 截断的完整 JSON；原 P1 因默认关闭、多条件入口和无 RSS/OOM 实测而降级 |
| PHIL-B01 | P2 | CONFIRMED | DEFECT | portable Responses/Messages 在 facade 最终事件交付前已写 `RequestFinalized(success)`，写失败时 caller 与 Ledger 终态分叉 |
| PHIL-A01 | P2 | CONFIRMED | DEFECT | current README/指南/架构/里程碑对 withheld Bedrock 与已实现 Run Governance 给出相反答案 |
| PHIL-A04 | P2 | CONFIRMED | EVIDENCE_GAP | operation→primitive 从同一事实源自证；目标 SHA sabotage 对调后 provider tests 仍全绿 |
| PHIL-A05 | P2 | CONFIRMED | DEFECT | Workbench 非流式 Go snippet 复制后不能编译，也未处理 response/error |
| PHIL-CD-003 | P2 | PARTIAL | DESIGN_DEBT | 缺口收窄为 shutdown-truncated 与 aged pending lease 的 actionable rule/runbook；pricing 已由 readiness 覆盖 |
| PHIL-A02 | P3 | PARTIAL | EVIDENCE_GAP | Run Governance 自身已有目标/指标/退出门；只保留跨能力 adoption/support-cost/sunset 台账缺口 |
| PHIL-A03 | P3 | PARTIAL | DESIGN_DEBT | Runtime 集中度存在，但字段/锁预算可向下 ratchet 且已有子 runtime；没有 P2 级事故证据 |
| PHIL-A06 | P3 | CONFIRMED | DESIGN_DEBT | 发布二进制 `halro --help` / `halro help` 均退出 1；无参数与子命令 help 是缓解 |
| PHIL-B02 | P3 | PARTIAL | EVIDENCE_GAP | 已取得 schema 36→37 与 old-reader refusal 的双二进制 E3；含 Ledger/Usage、backup/restore 和 kill-point 仍缺 |
| PHIL-B03 | P3 | CONFIRMED | EVIDENCE_GAP | 真实 Provider 证据缺失；SDK/fake contracts 与严格 fail-closed 防御使其应按 profile 管理而非笼统 P2 |
| PHIL-E-001 | P3 | PARTIAL | DESIGN_DEBT | `make check` 漏 web build/drift，但文档未把它单独冒充完整发布门禁，CI/release 有覆盖 |
| PHIL-E-002 | P3 | PARTIAL | DESIGN_DEBT | `make test -shuffle` 不命中 result cache；race、CI/release 仍与 `-count=1` 证据政策不一致 |
| PHIL-E-003 | P3 | PARTIAL | DESIGN_DEBT | 下游自动 preflight 缺失，但渠道本来就是可恢复 saga，且有人工 checklist/宣传门禁 |
| PHIL-E-004 | P3 | CONFIRMED | DEFECT（文档） | 标为 current 的 release 证据文档错误声称 workflow 不检查 web bundle drift |

## 2. P2 Findings

### PHIL-CD-001 — failure capture 队列在字节截断前持有完整请求副本

- 类型：DEFECT；维度 D5/D8；严重度 P2；置信度 HIGH；状态 PARTIAL。
- 入口与可达：operator 显式启用 capture；有效 Gateway Key 的大合法请求通过准入后得到可捕获失败；sink 足够慢使队列积压。
- 证据与复现：`internal/gateway/capture.go:22-38,70-103,274-324` 在 enqueue 前 marshal；
   `internal/failurecapture/failurecapture.go:311-326` 到 worker/store 才按 `max_bytes` 截断。64 槽 × 两侧
   × 10 MiB 是约 1.25 GiB 的源码上界模型，不是 RSS 实测。阻塞 sink/队列测试与 race 通过。
- 影响与防御：可能造成单实例内存/GC 压力；默认关闭、需认证和可捕获终态、队列满后 drop，故没有
   足够证据维持 P1。
- 建议处置与验收：SIMPLIFY；enqueue 前截断或 queued-byte semaphore。用真实 handler、blocking
  store、大合法请求测 queued bytes、RSS、GC、p99；若达到进程不可用再重审 P1。
- Owner/期限：Gateway + Security；S–M；任何生产启用 capture 前。

### PHIL-B01 — portable stream 最终交付失败后 Ledger 仍记 success

- 类型：DEFECT；维度 D3/D6；严重度 P2；置信度 HIGH；状态 CONFIRMED。
- 入口与可达：`/v1/responses` 或 portable `/v1/messages` streaming；Provider/core stream 已成功且至少
  发出 payload；随后 facade `Complete()` 或最终合成事件因客户端断开/write deadline/encoder 错误失败。
- 证据与复现：`internal/gateway/service.go:2380-2384` 在 core stream 返回前 finalize success；Responses
  `:1468-1476`、Messages `:1568-1576` 之后才 complete/emit。两个非作者各自在目标 SHA 临时副本注入
  final-event error，均观察 caller error 与 `RequestFinalized.outcome == "success"`。
- 影响与防御：Usage/request failure 漏记、客户争议与诊断终态分叉；Provider cost settlement 正确，
  首字节后不 retry/fallback，因此不造成错误退款或重复 Provider 执行，范围限于两个 portable facade。
- 建议处置与验收：SIMPLIFY；final renderer/emit 进入一次性 RequestFinalized 前边界。两 facade 均注入
  final-event error，断言无 fallback、保留 settlement、RequestFinalized 非 success。
- Owner/期限：Gateway + Compatibility；S；30 天。

### PHIL-A01 — current capability 真相分叉

- 类型：DEFECT；维度 D1/D9；严重度 P2；置信度 HIGH；状态 CONFIRMED。
- 入口与可达：用户/操作者从 current Operator/User Guide 配置 Bedrock，或维护者从 Governance/current
  implementation status 判断范围。
- 证据与复现：`internal/domain/provider_table.go:277-315` 与根 README withheld Runtime/Agent Runtime，
  current guides 仍列为 Beta；`internal/app/runtime.go:1706-1709,1794-1798` 和前端已有 Governance，
  `docs/architecture/run-governance-data-flow.zh-CN.md:1-6` 仍写生产实现未开始。
- 影响与防御：造成无效准备、错误发布/升级范围和信任损伤；代码 served/write path fail closed，故不是
  实际启用绕过。
- 建议处置与验收：SIMPLIFY/DELETE；用 manifest/golden 生成 current capability 摘要，current guides
  不得列 Withheld，已注册公开 surface 必须在 status 有状态。
- Owner/期限：Product + Docs；S；下一个候选版本前。

### PHIL-A04 — Provider operation→primitive 缺独立机械证明

- 类型：EVIDENCE_GAP；维度 D2/D6/D7；严重度 P2；置信度 HIGH；状态 CONFIRMED。
- 入口与可达：新增/修改 Profile 或 operation，两个同类型 primitive 被错误交换。
- 证据与复现：目标 SHA 隔离副本交换 OpenAI/DeepSeek chat+stream primitive，完整
  `go test -count=1 ./internal/provider` 仍通过；manifest/validation 从同一 table 推 expected。
- 影响与防御：可能产生有损映射、错误 capability filter 和 attempt attribution；不证明当前 v0.8.0
  已有错绑。profile table 收敛身份且高风险 profile 有局部硬编码测试。
- 建议处置与验收：PROVE；从 adapter entry point 获取独立 expected，交换 sabotage 必须失败，并同时
  断言 resolved primitive、实际 adapter 和 attempt record。
- Owner/期限：Provider Architecture；S–M；下一个 Provider/Profile 合入前。

### PHIL-A05 — Workbench 非流式 Go snippet 不可运行

- 类型：DEFECT；维度 D9；严重度 P2；置信度 HIGH；状态 CONFIRMED。
- 入口与可达：Developer Workbench 默认非流式请求，选择 Go 并复制代码。
- 证据与复现：`web/src/pages/DeveloperPage.tsx:820-833` 以 `resp, err := ...Do(req)` 结束；最小
  `package main` 编译退出 1，`resp` 和 `err` 未使用；现有 19 个页面测试不覆盖该组合。
- 影响与防御：Go 新用户得到立即不可编译且无响应处理的示例；Gateway 和 Workbench 实际 Send 不受影响，
  streaming Go/curl/Python/Java 分支提供局部防御。
- 建议处置与验收：SIMPLIFY；补 error/status/close/有界读取，为语言 × streaming 建 golden，Go snippet
  在 CI 包入最小程序编译。
- Owner/期限：Frontend/DX；S；下一个 patch release。

### PHIL-CD-003 — accepted-work 退化信号缺最小 actionable 告警闭环

- 类型：DESIGN_DEBT；维度 D4；严重度 P2；置信度 HIGH；状态 PARTIAL。
- 入口与可达：shutdown 强制截断 accepted attempts；pending lease 年龄超过配置允许的 route/stream/deferred 上界。
- 证据与复现：metrics 已导出 `shutdown_truncated_attempts`、pending lease count/oldest age，但 shipped
  Prometheus rules/runbook 无对应项。pricing quarantine 已经使 readiness 503 并可由 dead-man 探测，
  因此从原 finding 删除；capture/deferred 更适合 warning/ticket。
- 影响与防御：账务或 accepted-work 风险可能只在人工 doctor 时发现；通用 error/readiness、doctor 和
  Operator Guide 提供部分替代，但不能精准告知重启截断。
- 建议处置与验收：PROVE；为 truncated 建低噪声 rule，为 pending lease 定义超过合法上界的年龄公式；
  promtool 验证 firing/non-firing/resolved，非作者 15 分钟找到安全动作。
- Owner/期限：SRE + Accounting；S–M；30 天。

## 3. P3 Findings

### PHIL-A02 — 缺跨能力价值、支持成本和 sunset 台账

- 类型：EVIDENCE_GAP；D1；P3；HIGH；PARTIAL。
- 反证：Run Governance 与 Workbench 已有目标用户、job、指标和 rollout/停止门，原普遍断言过宽。
- 剩余影响：33 条 Gateway routes / 12 个 Admin area 未形成统一 adoption/support-cost/sunset 视图。
- 处置：PROVE；用访谈、opt-in reference deployment、支持请求，而非强制遥测；Product，60 天。

### PHIL-A03 — Runtime 组合根仍集中

- 类型：DESIGN_DEBT；D2；P3；HIGH；PARTIAL。
- 证据：`Runtime.Open` 约 747 行、10 个后台责任；但 74 fields/10 mutexes 预算在实际减少时也会失败并
  要求下调，已有多个子 runtime，未找到由集中度直接造成的当前事故。
- 处置：SIMPLIFY；只下沉一个高内聚生命周期并验证字段/receiver/Open LOC 下降；Architecture，60 天。

### PHIL-A06 — 顶层 CLI help 不符合通用发现习惯

- 类型：DESIGN_DEBT；D9；P3；HIGH；CONFIRMED。
- 复现：目标 binary `halro --help` 与 `halro help` 退出 1；无参数会列命令，subcommand `--help` 可用。
- 处置：SIMPLIFY；小 command descriptor 生成 dispatch/help，不引入 Cobra；CLI owner，1.0 前。

### PHIL-B02 — 完整升级/恢复证据尚未闭环

- 类型：EVIDENCE_GAP；D3/D6；P3；HIGH；PARTIAL。
- 新证据：v0.7.1 binary 的临时 schema 36 数据由 v0.8.0 迁移到 37，目标 doctor 通过；旧 doctor
  明确拒绝 schema 37 与 Usage manifest 8，直接二进制证据成立。
- 剩余缺口：临时数据没有真实 Ledger/Usage 历史；未做 pre-upgrade backup、restore 后旧读者复核与
  migration kill-point。INV-09 只能标 PARTIAL。
- 处置：PROVE；用生产形态合成数据完成 backup→upgrade→old-reader refusal→restore→旧 binary 验证；
  Core Data + Release，60 天。

### PHIL-B03 — 真实 Provider 兼容仍是按 profile 的证据缺口

- 类型：EVIDENCE_GAP；D6；P3；HIGH；CONFIRMED。
- 证据：本轮没有调用真实 Provider，因此当前 response shape、header、SSE termination、usage 仍非 E3/E4。
- 防御：三语言 SDK 黑盒 contract、严格 decoder、malformed/ambiguous/no-retry 测试和 owner 明示残余边界；
  上游漂移优先表现为具体 profile 可用性下降，而不是静默重复执行。
- 处置：PROVE；按 provider+profile+operation+model 记录 PASS/UNVERIFIED，先 non-billable catalog/identity，
  billable generation 需明确授权；Provider + Release/QA，60 天。

### PHIL-E-001 — 聚合本地检查入口不覆盖完整 web gate

- 类型：DESIGN_DEBT；D7/D9；P3；HIGH；PARTIAL。
- 证据：`make check` 只依赖 `frontend-test`，无 build/drift；但 `make help`、AGENTS、release assessment
  均没有说它单独等于完整发布门禁，CI/release 会捕获。
- 处置：SIMPLIFY；新增命名明确的 full gate 或统一发布入口；Build，30 天。

### PHIL-E-002 — 部分 evidence gate 仍允许 Go result cache

- 类型：DESIGN_DEBT；D7；P3；HIGH；PARTIAL。
- 反证：`make test` 的 `-shuffle=on` 会禁用结果缓存，连续执行确实重跑；`go test -race` 第二次可显示
  `(cached)`，CI/release 普通 Go test 也与 AGENTS 的 `-count=1` 字面政策不一致。
- 处置：SIMPLIFY；证据入口加 `-count=1`，快速开发入口可保留缓存；Build，30 天。

### PHIL-E-003 — 下游凭据缺少发布前自动 preflight

- 类型：DESIGN_DEBT；D10/D4；P3；HIGH；PARTIAL。
- 证据：workflow 在 publish/container 后才创建 App token，正式 run 因空 client ID 失败；但人工
  checklist 已要求检查，渠道被明确建模为可恢复 saga，网站须 acceptance 后才宣传。
- 处置：PROVE；publish 前自动验证变量、secret 存在性、App 安装和 repo 权限；Release，30 天。

### PHIL-E-004 — release evidence 文档错误描述 bundle drift

- 类型：DEFECT（文档）；D9/D10；P3；HIGH；CONFIRMED。
- 证据：`docs/verification/release-run-evidence.md:41-45` 声称 release 不比较 bundle drift；
  `.github/workflows/release.yml:194-214` 明确在 build 后执行该比较。
- 处置：DELETE/SIMPLIFY；修正为“release 不跑 fuzz，但会在 build 后比较 committed bundle”；Docs/Release，30 天。

## 4. 被撤销的缺陷与明确约束

- `PHIL-CD-002`：撤销 P2。`read_only` 是实例级全 GET，包含启用后的 failure payload；这是代码、测试、
  UI 和两角色模型共同表达的契约。capture 默认关闭，读取按次审计且 audit 不可用时 withholding。
- `PHIL-CD-004`：撤销 P2。缺生产 SLO/24h/真实告警是明确、强制、可观测的 v0.x constraint；
  Production Admission 在无 evidence ID 时必须 No-Go。任何“生产已验证”主张仍为 UNVERIFIED。
- `PHIL-B04`：保留 P3 ACCEPTED_CONSTRAINT。`AttemptStarted` 必须在远端副作用前 durable；本地 WAL 与
  socket I/O 无法原子提交。crash 后对全部 started-pending attempts 按 frozen price/bounds 保守结算，
  爆炸半径受 reservation/budget/concurrency 限制但不是固定“一两个”。
