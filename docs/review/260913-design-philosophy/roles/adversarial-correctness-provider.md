# 角色 F：正确性、数据与 Provider findings 独立反证

## 1. 范围与方法

- 目标源码固定为 `v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`。
- 被反证对象为角色 B 的 `PHIL-B01` 至 `PHIL-B04`。本角色不是这些 finding 的作者；逐条寻找不可达条件、既有防御、语义误读和严重度过高的可能。
- 重点复核写入失败边界、HTTP/SSE 交付语义、settlement / recovery 防御与真实影响。没有调用真实 Provider、KMS 或告警端，没有修改产品代码；故障注入仅位于 `git archive HEAD` 的临时副本，双版本演练仅使用 `/private/tmp` 下的假 Provider 地址和临时数据。
- 裁决词：`CONFIRMED` 表示可达链和影响成立；`PARTIAL` 表示核心风险的一部分成立但原表述或证据范围过宽；`REFUTED` 表示存在决定性不可达条件或误读；`UNVERIFIED` 表示本轮证据不足。

## 2. 裁决摘要

| Finding | 最终状态 | 建议最终严重度 | 结论 |
| --- | --- | ---: | --- |
| PHIL-B01 | **CONFIRMED** | **P2** | 两个 portable stream facade 的 complete / final-event 阶段确实晚于 `RequestFinalized(success)`；settlement 正确和 no-fallback 防御缩小了影响，但不能修复 caller outcome 与历史终态分叉。 |
| PHIL-B02 | **PARTIAL** | **P3** | 本轮已用 v0.7.1 与目标 SHA 二进制直接证明 schema 36→37 升级和旧读者拒绝；但没有完成含 Ledger/Usage 的生产形态数据、pre-upgrade backup restore 和进程 kill-point 演练，INV-09 仍只部分成立。 |
| PHIL-B03 | **CONFIRMED** | **P3** | 目标 SHA 的真实 Provider response / SSE 证据确实缺失；但官方 SDK 黑盒 contract、严格 decoder、ambiguous no-retry 和 owner 明示接受使它更适合作为按 profile 管理的 P3 证据缺口，而不是笼统 P2 产品缺陷。 |
| PHIL-B04 | **CONFIRMED（ACCEPTED_CONSTRAINT）** | **P3** | `AttemptStarted` durable 与真实 socket I/O 之间的模糊窗口不可消除；当前恢复选择有明确、可测的保守结算语义，不应作为 defect 修复。 |

## 3. 逐条反证

### PHIL-B01 — Portable stream 在最终事件交付前已把请求记为成功

**裁决：CONFIRMED，P2。**

完整可达链成立：`ResponsesStream` 在 `internal/gateway/service.go:1448-1464` 调用核心 `generateStream`；核心在 Provider 无错误时先执行 `run.finalize("success")` 并返回（`internal/gateway/service.go:2380-2384`）；随后 facade 才执行 `renderer.Complete()` 并发送 `response.completed` 等最终事件（`internal/gateway/service.go:1468-1476`）。portable `MessagesStream` 同样在 `ChatStream` 返回后才 complete / emit（`internal/gateway/service.go:1545-1576`）。`requestRun.finalize` 是一次性写边界（`internal/gateway/service.go:244-266`），后续错误不能改写已落盘 outcome。

HTTP 可达性也成立，不只是测试钩子：Responses 和 Messages handler 都为每个 SSE event 设置 write deadline，`encoder.Write` 可返回错误（`internal/gatewayapi/handler.go:148-164,640-653`）。服务已经开始响应后，handler 最多再尽力发送一个 `error` event（`internal/gatewayapi/handler.go:175-183,659-666`），这不能修改 Ledger。`RequestFinalized` 又是 Usage `request_errors` 和失败列表的事实来源，因此该分叉会漏记对外失败，而不只是日志措辞差异。

本角色在独立 `git archive HEAD` 副本 `/private/tmp/halro-adversarial-b01.QjCKhB` 添加 assessment-only test：让普通 payload 正常发送、仅在 `response.completed` 返回注入错误，并回放 Ledger。命令：

```text
env GOCACHE=/private/tmp/halro-adversarial-cache go test -count=1 ./internal/gateway \
  -run TestAdversarialFinalResponseEventFailureAfterSuccessFinalization -v
```

退出码 0；测试观察到 caller error，同时 `RequestFinalized.outcome == "success"`。这独立确认了时序，而不是复用角色 B 的临时测试。

最强反证不足以推翻 finding：Provider 已执行，`AttemptSettled` 的 cost 不应退款；首字节后禁止 retry/fallback 也避免了重复调用。这些防御说明缺陷不在金额或 exactly-once，而在请求级终态。普通 unary handler 在服务返回后也可能发生不可原子协调的 socket 失败，但此处还包含 facade 自身可确定的 `Complete()` 失败，且仓库现有 unary 回归明确要求 wire renderer 在 finalization 前运行（`internal/gateway/outbound_failure_outcome_test.go:35-116`）。因此保留 P2，但应把爆炸半径限定为 portable Responses / Messages 的后置 complete 与最终合成事件，不扩展到 Chat Completions、native Messages 或 settlement。

### PHIL-B02 — 缺目标 SHA 的双二进制升级 / 恢复演练

**裁决：PARTIAL，P3（从 P2 下调）。**

原报告对“完全没有目标 SHA 双二进制证据”的描述已被本轮最小直接演练部分反驳：从 tag `v0.7.1` 构建旧二进制，从目标 SHA 构建 v0.8.0 二进制；旧二进制在临时目录初始化并 bootstrap 一个指向 `.invalid` 的假 Provider。目标二进制打开该目录，虽因 sandbox 禁止 loopback bind 最终退出 1，但在 bind 前明确完成 Usage 重建；随后目标 `doctor` 退出 0 并报告 `bbolt schema v37`。同一目录交给旧二进制 `doctor` 时退出 1，明确报告：

```text
metadata schema version 37 does not match required version 36
usage manifest schema version 8 is not supported
```

因此 schema 36→37 与 old-reader refusal 已达到直接二进制 E3，而不是只有 fixture。目标源码的 `TestMigration37FencesOldReadersAndDropsTheUsageCheckpoint` 也以 `-count=1` 通过，覆盖 migration boundary 前后两处原子失败点。

仍不能把 finding 全部驳回：临时旧目录只有配置拓扑，没有真实请求产生的 Ledger/Usage 历史；本轮未制作并验证精确 pre-migration backup、未执行 restore 后旧二进制复核，也未做真实进程 kill-point。`docs/verification/assessments/v0.8.0.md:83-94` 描述了正确的升级/回滚策略，但不是本轮缺失步骤的原始运行记录。剩余风险由 bbolt transaction、schema/manifest 明确拒绝和 migration fault tests 显著降低，故建议将剩余目标-SHA 证据缺口定为 P3；INV-09 维持 PARTIAL，而非 UNVERIFIED。

### PHIL-B03 — Provider 兼容性仍局限于模拟上游

**裁决：CONFIRMED，P3（从 P2 下调）。**

不可达反证不存在：各 `real_smoke_test.go` 都由 `HALRO_REAL_PROVIDER_SMOKE=1`、profile、真实 endpoint / credential 显式启用；普通测试和 release workflow 不会运行它们。本轮又明确禁止真实 Provider，因此目标 SHA 的真实 response shape、header、usage 与 SSE termination 仍没有新 E3/E4 证据。

但原 finding 的严重度和范围偏宽。`.github/workflows/release.yml:134-180` 用 Python、Node、Go 官方 SDK 对本地兼容服务器执行黑盒 contract；adapter fixture 还覆盖 malformed 2xx、截断流和 provider-specific termination。严格 decoder 遇到上游漂移会 fail closed，ambiguous failure / 已发 payload 不会 fallback，因此主要残余影响是某个具体 profile 的可用性下降，而不是静默错误结算或重复 Provider 执行。`docs/verification/assessments/v0.8.0.md:16-21,129-134` 也明确记录 owner 对 BigModel、MiniMax、Kimi billable smoke 缺口的 v0.x 接受，这不能证明兼容，但避免把未执行误报成未知发布违规。

最终记录应按 `provider + profile + operation + model` 给 PASS / UNVERIFIED，优先补 non-billable catalog / identity capture；只有发生该 profile 的兼容性改动、外部漂移信号或发布策略把 live smoke 定为硬闸时，才把对应 profile 提升为 P2。不能把“所有真实 Provider 未在每个 SHA 调用”笼统记成一个 P2 defect。

### PHIL-B04 — `AttemptStarted` 与 socket I/O 之间存在保守收费窗口

**裁决：CONFIRMED（ACCEPTED_CONSTRAINT），P3。**

顺序无误：`startAttempt` 在返回前写 `MarkStarted`（`internal/gateway/service.go:700-709`），Provider generate 随后才进入 adapter（unary 为 `internal/gateway/service.go:1294-1303`，stream 为 `internal/gateway/service.go:2293-2377`）。崩溃恢复把未 started lease 释放为零成本，把 started 且未知结果的 lease 标记为 `recovered_started_unknown_result`、`Ambiguous=true`，并用 prepared token bounds 与冻结价格保守结算（`internal/budget/manager.go:1171-1242`）。

本角色运行：

```text
env GOCACHE=/private/tmp/halro-adversarial-cache go test -count=1 ./internal/budget \
  -run 'TestRecover(StartedLeaseUsesFrozenPriceAndPreparedBounds|PendingLeasePreservesFrozenProviderAttribution)' -v
```

退出码 0；started / not-started 两类和 Provider attribution 均通过。已有防御的意义是：把 started 写到 socket I/O 之后会产生“上游可能已执行、本地却没有 durable started”的更危险窗口；单机系统无法让本地 WAL 与远端副作用原子提交。因此 KEEP 是正确处置。

原报告“一个或少量”不是严格上界：一次进程崩溃可能覆盖所有刚跨过 started barrier 的并发 attempt。实际损失仍受每个 reservation、Project/Run budget、配置的 provider/deployment concurrency 等边界约束，但若部分并发限制配置为 unlimited，不能承诺固定条数。建议保留 P3，并在 runbook / metric 中按 crash 时的 started-pending 集合描述爆炸半径。

## 4. 证据等级与限制

- PHIL-B01：E2，目标源码静态链 + 目标 SHA 隔离副本故障注入。
- PHIL-B02：schema 升级和 old-reader refusal 为 E3；migration atomicity 为 E2；backup/restore、含 Ledger/Usage 的旧数据和进程 kill-point仍为 UNVERIFIED。
- PHIL-B03：本地 SDK / fake Provider 为 E2；真实 Provider、真实 SDK→Halro→真实 upstream 的目标 SHA 证据为 UNVERIFIED，没有 E4。
- PHIL-B04：恢复语义为 E2；真实进程 crash 对 started-before-socket 窗口的数量测量为 UNVERIFIED。
- 未调用真实外部端，未修改产品代码，未 commit / push。目标二进制 `serve` 的 bind 失败是 sandbox loopback 权限限制；migration 和之后的 offline doctor 结果不依赖监听器成功启动。

## 5. 给总评的建议

1. 保留 PHIL-B01 为本组唯一 P2 产品缺陷；修复必须同时断言：不 fallback、不退款已发生 cost、但 `RequestFinalized` 不得伪记 success。
2. PHIL-B02 改为 PARTIAL/P3：本轮已补最小双二进制 schema fence，剩余是 production-shaped backup/restore 与 kill-point 证据。
3. PHIL-B03 保留为 CONFIRMED/P3 证据缺口，按 profile 列矩阵；不要把它表述成所有 Provider 已知不兼容。
4. PHIL-B04 保留为 P3 accepted constraint；修文档和量化 blast radius，不引入分布式事务或推迟 durable started。
