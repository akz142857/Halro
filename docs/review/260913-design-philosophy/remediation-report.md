# Halro 系统设计哲学评估：首轮顺序整改复评

评估基线：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`

整改载体：上述基线之上的本地工作树（尚未提交、推送或发布）

复评日期：2026-09-13

## 1. 复评结论

按首轮报告建议的顺序，本轮已完成 portable stream 终态、failure capture 入队字节上限、current
capability 与 Workbench 示例、账务告警闭环，以及最终门禁五个工作包。与这些工作包直接对应的六条
finding 已在代码或 current 文档中关闭。

本轮裁决是 **`ORDERED REMEDIATION COMPLETE / PRODUCTION UNVERIFIED`**：

- 本轮约定顺序内的实现和文档整改已完成；
- 全量 Go、前端、静态检查、竞态检查和 Prometheus 规则测试通过；
- `PHIL-A04`（Provider operation→primitive 独立机械证明）不在本轮约定顺序内，仍是活动 P2；
- failure capture 的 production-shaped RSS/GC/p99、真实 Alertmanager Contact Point、生产环境和 24h soak
  仍未执行，不能从局部门禁推断 E4 或生产准入；
- S3/S4 覆盖仍未达到方案要求的 80%，因此继续不输出官方 100 分总分。

## 2. 逐项复评

| 顺序 | Finding | 结果 | 当前证据 | 保留边界 |
| --- | --- | --- | --- | --- |
| 1 | `PHIL-B01` portable stream 最终事实分叉 | **CLOSED / E2** | Responses 与 portable Messages 的 facade completion 进入 Request 生命周期；最终事件失败时 Request 记 `provider_error`，Provider attempt 保留真实 settlement，且不 fallback | 未以真实断连代理做 E3 |
| 2 | `PHIL-CD-001` capture 入队前无 byte bound | **DEFECT CLOSED / E2** | `PrepareRecord` 在异步队列取得所有权前按三侧 `max_bytes` 截断；Store 边界再次幂等执行；blocking-store 大载荷测试证明队列内记录已受限 | production-shaped RSS/GC/p99 仍 `UNVERIFIED` |
| 3 | `PHIL-A01` current capability 真相分叉 | **CLOSED / E2** | User/Operator/Architecture/Milestone 文档统一标明 Bedrock Runtime/Agent Runtime 为 Withheld，Run Governance 为已实现；domain golden 防止 current 文档再次漂移 | 历史计划保留历史语义，不作为 current truth |
| 3 | `PHIL-A05` Workbench Go snippet 不可运行 | **CLOSED / E2** | 非流式与流式示例均处理构造、transport、status、body/stream error，关闭 body 并限制读取；页面回归与临时最小 Go 程序编译通过 | 尚未建立所有语言 × streaming 的独立编译矩阵 |
| 3 | `PHIL-E-004` release evidence 错述 | **CLOSED / E1** | current 文档改为 release 会重建并比较 committed web bundle，且明确 fuzz 不在该 workflow 中 | 没有改变 workflow 本身 |
| 4 | `PHIL-CD-003` accepted-work 告警缺口 | **LOCAL CLOSED / E3** | 新增 shutdown-truncated 和 stale pending lease rules、阈值 gauge、runbook；固定 Prometheus 镜像的 promtool firing/non-firing/resolved 测试通过 | 未接真实 Alertmanager/Contact Point，外部投递仍 `UNVERIFIED` |

## 3. 设计判断

本轮没有引入消息队列、外部缓存、通用 DI 或新的运行时服务。整改继续保留 Halro 的 single-binary、
single-process、single-writer 方向：

- stream 修复收敛了一个已有生命周期边界，没有复制第二套 accounting；
- capture 修复复用了 Store 的同一截断语义，没有引入不可观测的全局内存配额；
- stale lease 告警阈值直接来自 route total timeout 与 stream max duration 的较大值，而不是硬编码另一套
  “正常时长”；
- current capability golden 只锁定 externally visible truth，不把整个文档生成系统扩成新框架；
- Go 示例仍从环境变量读取 Key，错误正文读取有 1 MiB 上限。

因此本轮符合原方案中“先修不变量和真相，再增加证据”的方向，也没有借整改扩张 Halro 的产品边界。

## 4. 验证结果

完整机器可读记录见 [remediation-gates.json](evidence/remediation-gates.json)。关键结果：

- `go test -count=1 ./...`：PASS；
- `go test -race -count=1 ./internal/gateway ./internal/failurecapture`：PASS；
- `go vet ./...`：PASS；
- 前端 Node 22 typecheck、44/44 test files、605/605 tests、production build 与 artifact secret check：PASS；
- Prometheus rules 的 Go contract 与固定 `prom/prometheus:v3.5.0` promtool 测试：PASS；
- Workbench 两个 Go 示例的临时最小程序编译：PASS；
- `git diff --check`：PASS。

第一次 sandbox 内全量 Go 尝试因 `httptest` 不能绑定本地回环端口而失败；随后在获准的本机回环环境
重新运行并取得 exit code 0。该环境失败没有被计为产品失败，也没有被隐去。

## 5. 尚未关闭的评估项

### 活动 P2

- `PHIL-A04`：为 `(profile, operation)` 声明 primitive 与实际 adapter entry point 建立独立事实源；交换
  两个同类型 primitive 的 sabotage 必须使测试失败，并同时核对 resolved primitive、adapter 与 attempt
  attribution。

### E3/E4 与生产准入

- production-shaped capture RSS、GC、p99 和排队容量曲线；
- 真实 Alertmanager Contact Point 的 firing/resolved/heartbeat-loss；
- 完整 backup/restore/kill-point 与带真实 Ledger/Usage 历史的双二进制演练；
- 真实 Provider、KMS、PKI、clean-host package、完整浏览器旅程和 24h soak。

上述证据未取得前，首轮报告的 **`PRODUCTION UNVERIFIED`** 与 Production Admission No-Go 不变。

## 6. 下一步

下一项应执行 `PHIL-A04` 的独立 primitive contract 和 sabotage test。它关闭后，再进入 31–60 天 E3
阶段；不建议在此之前新增 Provider/Profile 或扩大产品 surface。
