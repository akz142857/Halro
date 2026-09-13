# 角色 B：正确性、账务、数据耐久与 Provider 兼容性评估

## 1. 结论摘要

- 评估目标：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`。
- 范围：D3 正确性 / 账务 / 数据耐久，D6 API / Provider / 兼容性；重点复核 INV-01、02、03、06、07、08、09、12。
- 独立性：本角色独立阅读目标源码和运行测试，未读取本轮其他角色报告；历史 finding 只作为回归种子，没有直接继承其裁决。
- 总体判断：主账务链采用单一 Ledger 权威、先持久化 reservation / started 再调用 Provider、结算与失败恢复保守、Usage 可从 Ledger 追赶重建，设计边界清楚；模型存在性与能力证据也已明确分离。当前确认 1 个 P2：Responses / portable Messages 流式 facade 在核心请求已记为成功后才发送最终合成事件，最终写失败会让调用者终态和 Ledger 终态分叉。
- 证据上限：本轮只有目标源码静态证据和目标 SHA fixture / 故障注入自动化（E1/E2）。未执行真实 Provider、真实基础设施或新旧二进制升级恢复演练，因此 D3、D6 均不能高于 2 分。

| 维度 | 成熟度 | 最高证据 | 裁决 |
| --- | ---: | --- | --- |
| D3 正确性、账务、数据耐久 | **2 / 4** | E2 | 主链和负向防御完整度高，但存在流式终态分叉，且 INV-09 缺目标 SHA 的双二进制恢复演练。 |
| D6 API、Provider、兼容性 | **2 / 4** | E2 | malformed 2xx、首字节后禁止 fallback、模型证据分层有自动化支持；真实 Provider / SDK 兼容仍未验证，且两个 portable stream facade 存在已复现的最终事件时序缺陷。 |

## 2. 主链追踪

### 2.1 Gateway → route → budget → Provider → settlement → Usage

1. `internal/gateway/service.go:310-349` 先认证 Gateway Key、检查 inference scope、Project allowlist、来源地址与 policy snapshot，再区分 unavailable / unsupported / not found。
2. `internal/gateway/service.go:364-442`（`beginRequestRun`）完成 Run 归属、Token Guard、限流和 durable `RequestAccepted`；`internal/gateway/service.go:445-469` 对 `X-Halro-Run-ID` 做 Project、scope、active 状态检查。
3. `internal/gateway/service.go:558-709` 选定价格快照，执行 Project / Run 预算准入并写 reservation，提交 price pin，然后写 `AttemptStarted`；任一步失败都在 Provider 调用前 fail closed。
4. unary Provider 调用发生在 `internal/gateway/service.go:1278-1303`；stream Provider 调用发生在 `internal/gateway/service.go:2293-2377`。两者都晚于 `startAttempt` 返回。
5. `internal/gateway/service.go:768-801` 统一释放并发租约、记录 Provider 结果、补齐 settlement 语义并持久化结算；`internal/gateway/service.go:2550-2559` 使用独立于客户端取消的 bounded cleanup context。
6. `internal/budget/manager.go:1030-1168` 把 reservation 释放、provider token、冻结价格和 committed cost 写进同一个 `AttemptSettled` 事件；`internal/ledger/event.go:705-790` 在状态机中原子校验归属和 price snapshot，随后释放 reserved 并增加 committed。
7. `internal/usage/collector.go:23-25,42-94` 明确把 Usage 定义为衍生视图：通知队列溢出只置 lagging，随后从 Ledger watermark 追赶；`internal/usage/aggregate.go:235-295` 只消费 Ledger record 构造 Usage。

### 2.2 并发、故障和恢复

- `internal/budget/manager.go:648-727` 用 Project admission lock 把 committed、reserved 和尚未 apply 的 pending admission 一起计入，避免并发超卖；Run budget 同走该临界区。
- `internal/budget/manager.go:938-996` 在短临界区完成准入后才 append，append / apply 失败会释放 pending headroom。
- `internal/budget/manager.go:1282-1328` 按 Ledger sequence 串行 apply；任何 apply 失败把 accounting 标为 unavailable，后续 fail closed。
- `internal/ledger/log.go:385-457` 只自动截断不完整尾部；段缺失、损坏或认证失败要求恢复。`internal/ledger/log.go:822-897` 只有完整写入并 `fsync` 后才推进内存 watermark 并响应成功。
- `internal/ledger/log.go:972-1084` 区分未来 epoch、非法降级、非单调 sequence、超限 payload、CRC / chain / MAC 失败。
- `internal/budget/manager.go:1171-1242` 启动时释放未 started 的 lease，对已 started 且结果未知的 lease 按 prepared token 与冻结价格保守结算。

### 2.3 Provider 与能力证据

- OpenAI unary 接受 2xx 后的缺字段、JSON 解码失败、超限 body 都经 `acceptedResponseMalformed` 变成 `ErrorMalformed + Ambiguous=true`（`internal/provider/openai/adapter.go:455-465,539-588,630-656`）。
- Anthropic、Gemini、Bedrock Mantle 的相同边界分别位于 `internal/provider/anthropic/adapter.go:569-644,881-883`、`internal/provider/gemini/adapter.go:305-380,525-562,671-677`、`internal/provider/bedrockmantle/adapter.go:202-297,496-517`。
- `internal/gateway/service.go:2582-2593` 明确禁止 ambiguous failure 重试；stream 在任何安全字节发出后也不再 fallback（`internal/gateway/service.go:2280-2405`）。未知 unary / stream 结果分别走保守结算（`internal/gateway/service.go:2983-3035,3177-3221`）。
- OpenAI-shaped `/models` 只证明存在性，不产生能力 metadata（`internal/provider/model_catalog.go:10-22,51-72`）。未知模型为零能力，冲突能力关闭并受 profile ceiling 限制（`internal/modelcatalog/catalog.go:270-285,359-429`）。只有 upstream 实际枚举、标记 `MetadataSourceProvider` 的 target 才进入 provider metadata mapper（`internal/app/admin_invocation_targets.go:606-652`）；手填模型不会因 endpoint 或字段缺失获得能力。
- capability claim 绑定 provider / target / binding / profile / location，provider / probe 证据必须过期且证据等级不得超过 source（`internal/domain/invocation_target.go:71-75,144-214`）。

### 2.4 版本闸与 Governance

- metadata 当前 schema 为 37；未来 schema 明确拒绝，迁移链在单个 bbolt write transaction 内运行（`internal/store/bolt/store.go:24,1787-1845`）；只读诊断拒绝自动迁移（`internal/store/bolt/store.go:1456-1481`）。
- schema 迁移写入 Ledger reader / feature epoch gate，Runtime 在打开和 replay Ledger 前验证闸门（`internal/store/bolt/store.go:1027-1041`；`internal/app/runtime.go:272-312`）。
- Governance handler 只经授权 scope 和 idempotency intent 调用 accounting lifecycle 或独立 outcome journal（`internal/app/run_governance.go:73-115,131-177,180-215,218-240`）；WorkUnit / Run 的 create / close 是 Ledger 事件，Run budget 仍在统一 admission 临界区校验。未发现 Governance 主动调用 Provider 或改写历史 settlement 的路径。

## 3. 不变量裁决

| 不变量 | 裁决 | 证据 | 备注 |
| --- | --- | --- | --- |
| INV-01 | SUPPORTED | E2 | auth / route / Project 与 Run admission / Attempt / settlement 汇聚到 Gateway + budget manager + Ledger 状态机；并发窄测含 `-race` 通过。 |
| INV-02 | PARTIAL | E2 | reservation 和 started 均先于 Provider，settlement 原子，ambiguous 不重试且保守收费；但 portable stream 的 RequestFinalized success 可早于最终 facade event 交付，见 PHIL-B01。 |
| INV-03 | SUPPORTED | E2 | Ledger 决定余额；Usage queue 丢通知后按 watermark replay，checkpoint / aggregate 不能反写余额。 |
| INV-06 | SUPPORTED | E2 | existence、capability、availability、evidence source 分离；未知 / 手填 / 冲突 fail closed。 |
| INV-07 | SUPPORTED | E2 | attempt 数有界，ambiguous 不 retry；首个 payload 后不换 Provider。PHIL-B01 不触发 fallback，故不否定本条。 |
| INV-08 | PARTIAL | E2 | route / provider attribution、price snapshot、policy / capability revision 均被快照；但 PHIL-B01 会让最终 caller outcome 未进入历史终态。 |
| INV-09 | UNVERIFIED | E2 | schema / WAL / rollback fence 的代码和 fixture 测试存在；缺当前目标 SHA 的真实旧数据、新旧二进制和 restore 演练，见 PHIL-B02。 |
| INV-12 | SUPPORTED | E2 | Governance 生命周期经 Ledger；outcome journal 与账务分离；没有释放既发生成本、绕过 Project budget 或发起 Provider I/O 的可达路径。 |

## 4. 肯定项

1. **账务写入边界集中且保守。** reservation、started、settled、finalized 都是显式事件；不能证明上游未执行时宁可保守结算，也不自动触发第二个 Provider。
2. **派生视图可以丢通知，但不能丢事实。** Usage queue 有界，溢出可观测并能从 Ledger 补齐，符合“权威日志 + 可重建视图”的简单恢复哲学。
3. **Provider 2xx 不等于业务成功。** 各 adapter 对 malformed 2xx、截断 stream、错误 content type 和超限 body 均有负向分类；这直接保护重复执行与错误退款边界。
4. **存在性和能力证据分层清晰。** `/models`、builtin catalog、provider metadata、verified probe、operator declaration 各有不同证据上限、scope、expiry 和冲突规则。
5. **兼容性默认拒绝而非猜测。** 新 schema、未来 WAL epoch、认证 epoch 降级、缺段、破链和 replay 失败都阻止 Runtime 继续启动。
6. **Governance 没有成为第二套账本。** 它扩展 Ledger 的 WorkUnit / Run attribution，outcome 则存入单独 journal；读取时组合视图，而不是修改结算事实。

## 5. 候选 findings

### PHIL-B01 — Portable stream 在最终事件交付前已把请求记为成功

- 类型：DEFECT
- 维度与原则：D3、D6；“一个可观察终态只有一个事实”、boring correctness、协议终态与账务终态一致
- 严重度：P2
- 置信度：HIGH
- 状态：CONFIRMED
- 入口与可达条件：调用 `/v1/responses`（或 portable `/v1/messages`）stream；Provider 成功并已发送至少一个 payload；核心 stream 返回成功并写入 `RequestFinalized(success)`；随后 facade 生成的 `response.completed` / `message_stop` 等最终事件因客户端断开、write deadline 或 ResponseWriter 写错误而发送失败。
- 代码与文档证据：`internal/gateway/service.go:2380-2384` 在 `generateStream` 内 finalize success 并返回；Responses 在此后才 `renderer.Complete` 和 emit（`internal/gateway/service.go:1468-1476`），Messages 同样在 `internal/gateway/service.go:1568-1576`；真实 HTTP emit 可在 `internal/gatewayapi/handler.go:148-164,640-653` 返回写错误；`requestRun.finalize` 为一次性边界（`internal/gateway/service.go:244-258`）。代码自己的 unary 回归说明“record and answer agree”是承诺（`internal/gateway/service.go:1315-1325`；`internal/gateway/outbound_failure_outcome_test.go:35-65,89-110`）。
- 最小复现与原始证据：在 `git archive HEAD` 的隔离副本 `/private/tmp/halro-roleb-repro.PCmnx0` 增加 assessment-only test，令 `response.completed` 的 emitter 返回注入错误；运行 `env GOCACHE=/private/tmp/halro-phil-go-cache go test -v -count=1 ./internal/gateway -run TestReproResponsesFinalEmitFailureLeavesSuccessInLedger`，退出码 **0**，日志关键行：`caller error=injected response.completed delivery failure; ledger RequestFinalized outcome="success"`。该 test 断言当前缺陷行为以便稳定复现，并未加入产品工作树。
- 违反的不变量或用户承诺：INV-02 的“最终”请求结果一致性、INV-08 的历史事实完整性；直接违反现有源码注释中“record and answer agree”的承诺。
- 用户影响、爆炸半径和可恢复性：用户收到没有协议终止事件的 stream 或写错误，而 Usage / 请求 outcome 显示 success；失败率、客户争议与 failed-request 调查会漏报。Provider 已真实执行，当前 cost settlement 仍应保留，也不应 retry / fallback，因此不是错误退款或重复扣费。影响 portable Responses / Messages 的最终合成事件；已经在核心 emit 内发生的错误仍会进入失败路径。请求 finalization 一旦写入不能原地修正。
- 已有防御与反证：核心 `generateStream` 在首字节后禁止 fallback；Provider attempt 已按实际 usage / 保守估算结算；HTTP handler 会尝试补发安全 error event。上述防御阻止重复执行和错误退款，但无法修正 Ledger outcome。Chat Completions 没有 facade-complete 后置阶段，不受同一时序影响。
- 建议处置：SIMPLIFY
- 建议回归或验收：把 caller-specific stream renderer 的 `Complete` 与最终事件 emission 纳入 RequestFinalized 之前的统一完成边界；至少新增 Responses 和 Messages 两个测试，分别注入 final-event emitter error，并断言无 fallback、settlement 保留、RequestFinalized 不是 success。再加 handler 层 write-deadline / disconnect 测试验证对外 SSE 没有伪 success。
- 成本、owner、期限：S（约 0.5–1 天）；Gateway / Compatibility owner；建议 30 天内完成，发布前应至少固定回归。

### PHIL-B02 — INV-09 只有 fixture 证据，缺 v0.8.0 目标 SHA 的双二进制升级 / 恢复演练

- 类型：EVIDENCE_GAP
- 维度与原则：D3、D6；SQLite 式格式兼容、明确拒绝和可恢复演进
- 严重度：P2
- 置信度：HIGH
- 状态：UNVERIFIED
- 入口与可达条件：从上一个可支持版本的真实数据目录升级到本目标版本；在 migration / 首个 epoch-5 append 的关键点崩溃；使用旧二进制打开升级后的 live directory；从精确 pre-migration backup 恢复并复核 Ledger、Usage 和 readiness。
- 代码与文档证据：schema 37 与 transactional migration 位于 `internal/store/bolt/store.go:24,1787-1845`；Ledger gate 位于 `internal/store/bolt/store.go:1027-1041`、`internal/app/runtime.go:272-312`；ADR 要求 old-reader refusal、kill points、restore 与 exact rollback rehearsal（`docs/adr/0014-ledger-wal-backup-compatibility.md:137-148`）。仓库只有 `docs/verification/upgrades/2026-09-06-v0.7.0/` 的较早演练，不能证明当前 `v0.8.0@1d48ecde`。
- 最小复现与原始证据：`env GOCACHE=/private/tmp/halro-phil-go-cache go test -count=1 ./internal/app ./internal/store/bolt -run 'Test(.*InvocationTarget.*|.*HandEntered.*|.*Enumerated.*|.*Upgrade.*|.*Rollback.*|.*Backup.*|.*Restore.*|.*Migration.*|.*Schema.*|.*RunGovernance.*)'`，两 package 均 `ok`，退出码 **0**。这只达到 E2；本轮未找到绑定当前 SHA 的双二进制原始记录，故没有可声称的 E3 结果。
- 违反的不变量或用户承诺：没有证明 INV-09 被违反；INV-09 保持 UNVERIFIED，不能据此判 BUG 或给成熟度 3。
- 用户影响、爆炸半径和可恢复性：若 fixture 未覆盖真实旧数据组合，风险在升级或回滚时才暴露，可能导致启动拒绝、恢复窗口延长或派生视图不一致；当前 fail-closed 设计降低静默损坏风险。
- 已有防御与反证：bbolt transaction 回滚、未来 schema 拒绝、WAL future epoch / downgrade / CRC / MAC 检查、Runtime 启动前 gate 和历史 v0.7.0 演练均为强防御；它们不能替代本候选的目标 SHA 运行证据。
- 建议处置：PROVE
- 建议回归或验收：用上一个受支持 release binary 创建并使用数据；制作和验证 pre-migration backup；用本 SHA binary 启动、产生 epoch-5 事件、执行 doctor / ledger / usage；证明旧 binary 明确拒绝；再从 backup 恢复并用旧 binary 验证。保存命令、版本、hash、exit code 和关键输出到新的目标 SHA evidence 目录。
- 成本、owner、期限：S–M（约 1 天）；Core Data + Release owner；最终总评前完成，最迟 30 天。

### PHIL-B03 — Provider 兼容性在目标 SHA 上仍局限于模拟上游

- 类型：EVIDENCE_GAP
- 维度与原则：D6；真实边界验证、外部证据与兼容性回归
- 严重度：P2
- 置信度：HIGH
- 状态：UNVERIFIED
- 入口与可达条件：真实 OpenAI / Anthropic / Gemini / Bedrock Mantle / MiniMax 等 endpoint 的当前 response shape、header、SSE 终止、usage 和 SDK 消费行为与 fixture 不同。
- 代码与文档证据：各 adapter 均有严格 decoder 与 malformed / ambiguous 分类，且存在 `real_smoke_test.go` 一类显式真实上游测试；本轮约束禁止真实或计费 Provider，未取得目标 SHA 的当前 response capture 或 SDK matrix。
- 最小复现与原始证据：OpenAI、Anthropic、Gemini、Bedrock Mantle 的本地 fake-server / fixture tests 均通过（见第 6 节）；真实 Provider 未调用，故此项不能由 `ok` 推进到 E3/E4。
- 违反的不变量或用户承诺：未证明 INV-06 / 07 被违反；真实上游兼容主张保持 UNVERIFIED。
- 用户影响、爆炸半径和可恢复性：若上游漂移，严格 decoder 会优先报错并阻止重复 fallback，正确性优先；但可用性可能下降，特定 provider / profile / model 受到影响。
- 已有防御与反证：响应大小上界、严格字段 / semantic validator、ambiguous no-retry、capability evidence expiry 和 fixture negative tests；真实 Provider smoke 明确隔离，不会默认计费执行。
- 建议处置：PROVE
- 建议回归或验收：在授权、限额和可审计账号上运行 non-billable catalog / SDK contract；对 billable generation 由用户显式批准后仅做最小样本，保存脱敏 response shape、终止事件和 usage；按 profile 而不是品牌给出 PASS / UNVERIFIED。
- 成本、owner、期限：M；Provider owner + Release / QA；30 天内建立可重复证据，未授权前保持 UNVERIFIED。

### PHIL-B04 — `AttemptStarted` 与实际 socket I/O 之间存在保守收费窗口

- 类型：ACCEPTED_CONSTRAINT
- 维度与原则：D3；外部副作用无法与本地 WAL 原子提交、保守恢复
- 严重度：P3
- 置信度：HIGH
- 状态：ACCEPTED
- 入口与可达条件：`AttemptStarted` 已 fsync，但进程在真正进入 Provider adapter / socket 写入前崩溃；重启恢复只能看到 started、看不到上游是否实际接收。
- 代码与文档证据：`startAttempt` 在 `internal/gateway/service.go:700-709` 写 started，实际 unary 调用在 `internal/gateway/service.go:1294-1303`；`internal/budget/manager.go:1171-1228` 对 started unknown-result 按 prepared bounds 和冻结价格结算。
- 最小复现与原始证据：budget recovery fixture（第 6 节）通过；本轮未执行真实进程 kill-point，因此可达窗口以 E1/E2 为准。
- 违反的不变量或用户承诺：不违反 INV-02；它是“不错误退款可能已执行请求”的保守选择，但会产生小概率 false-positive charge。
- 用户影响、爆炸半径和可恢复性：单个 crash 可使尚未到达 Provider 的一个或少量 in-flight attempt 按 reservation 结算；Ledger outcome / recovery metric 可解释，但不能自动判定上游事实。
- 已有防御与反证：reservation 先于 started；未 started lease 零成本释放；started recovery 使用明确 outcome `recovered_started_unknown_result`、`Ambiguous=true` 和 deterministic recovery event ID；预算上界限制单次影响。
- 建议处置：KEEP
- 建议回归或验收：保留现设计；补一个真实进程 kill-point 量化窗口，并在操作文档中明确该恢复语义与查询方式。不要引入分布式事务或假装能从本地推断远端副作用。
- 成本、owner、期限：S（文档与 kill-point 证据）；Core Data owner；60 天内补证即可。

## 6. 运行证据

所有 Go 命令均在目标 SHA 工作树或其 `git archive HEAD` 精确副本运行，使用 `-count=1` 和 `/private/tmp/halro-phil-go-cache`；没有真实 Provider 调用。

| 命令 / 范围 | 退出码 | 结果 |
| --- | ---: | --- |
| `go test -count=1 ./internal/gateway -run 'Test(Accepted|.*Ambiguous|.*Fallback|.*Stream|.*Budget|.*RunAttribution|.*Capability)'` | 0 | `ok`；覆盖 malformed / ambiguous、fallback、stream、budget、Run attribution、capability 路径。 |
| `go test -count=1 ./internal/budget -run 'Test(Concurrent|.*Admission|.*Recover|.*Boundary|.*Overflow|.*Settlement|.*Poison|.*Run)'` | 0 | `ok`。 |
| `go test -race -count=1 ./internal/budget -run 'Test(Concurrent.*|.*RecoverPending.*|.*Boundary.*)'` | 0 | `ok`，race detector 未报告竞争。 |
| Ledger durability / corruption / downgrade / replay 相关窄测 | 0 | `ok github.com/akz142857/Halro/internal/ledger 59.912s`。 |
| Usage checkpoint / replay / rollup 相关窄测 | 0 | `ok github.com/akz142857/Halro/internal/usage 11.527s`。 |
| OpenAI adapter stream / retry / ambiguous / catalog 相关窄测 | 0 | `ok .../internal/provider/openai 6.774s`。 |
| Anthropic adapter stream / catalog / retry / ambiguous 相关窄测 | 0 | `ok .../internal/provider/anthropic 0.648s`。首次 sandbox 运行因 `httptest` 无权绑定 `[::1]:0` 退出 1；在允许本机 loopback、仍不访问外部网络的环境重跑通过，前一次不计产品失败。 |
| `go test -count=1 ./internal/provider/gemini` | 0 | `ok .../internal/provider/gemini 0.637s`。首次 sandbox 同样因 loopback 权限退出 1，获允许后本机 fake server 重跑通过。 |
| Bedrock Mantle adapter 窄测 | 0 | `ok .../internal/provider/bedrockmantle 6.163s`。 |
| `go test -count=1 ./internal/provider ./internal/modelcatalog ./internal/domain -run 'Test(.*Capability.*|.*Unknown.*|.*Evidence.*|.*Catalog.*|.*Provider.*)'` | 0 | 三个 package 均 `ok`。 |
| `go test -count=1 ./internal/app ./internal/store/bolt -run 'Test(.*InvocationTarget.*|.*HandEntered.*|.*Enumerated.*|.*Upgrade.*|.*Rollback.*|.*Backup.*|.*Restore.*|.*Migration.*|.*Schema.*|.*RunGovernance.*)'` | 0 | `internal/app` 与 `internal/store/bolt` 均 `ok`。 |
| assessment-only stream final-event repro | 0 | 明确打印 caller error 与 Ledger `success` 分叉；见 PHIL-B01。 |

## 7. 限制与未执行项

- 没有真实或计费 Provider 调用；因此 response shape、SSE、usage、速率限制和 provider-specific 语义最高为 E2。
- 没有外部 SDK matrix；未对 OpenAI / Anthropic 官方 SDK 的当前版本执行端到端 contract。
- 没有目标 SHA 的双二进制升级、kill-point、backup / restore 运行演练；INV-09 保持 UNVERIFIED。
- 没有长期故障率、恢复时间、真实 fsync / disk-full / power-loss 数据；不能给 D3 成熟度 3 或 4。
- 没有修改产品代码、没有 commit / push。工作树原有 `docs/review/README.md` 和其他评估目录改动均未触碰；本角色只新增本报告。
- 本轮未做非原作者对抗复核；PHIL-B01 虽有独立最小复现，最终严重度仍应由角色 F 检查 outcome 语义、入口可达性和 blast radius。

## 8. 给总评的输入

- 最高风险：PHIL-B01（P2，CONFIRMED）。它不破坏金额结算和 no-fallback 红线，但破坏调用者与历史终态的一致性，且正好落在现有 unary 已明确修复过的同类时序边界。
- 结构性证据缺口：PHIL-B02（升级 / 恢复）和 PHIL-B03（真实 Provider / SDK）。二者应影响成熟度和 release evidence，不应被改写成已确认产品 BUG。
- 不建议引入第二账本、分布式事务或 provider-specific 旁路；最小修复是把 facade 的 complete / final emit 纳入统一一次性 finalization 边界，并用精确负向测试固定时序。
