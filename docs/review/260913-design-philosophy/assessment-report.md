# Halro 系统设计哲学评估报告（第一轮多角色评审）

评估对象：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`

评估日期：2026-09-13

执行状态：S0–S2、S5 已完成；S3/S4 部分完成；S6/S7 形成阶段性结论。

> 后续状态（2026-09-13）：首轮建议顺序内的 portable stream、capture byte bound、current truth、
> Workbench Go 示例、release evidence 文档与 accounting alerts/runbook 已完成本地整改和复评；
> 详见 [首轮顺序整改复评](remediation-report.md)。本文件继续保存冻结基线的历史结论。

## 1. 一页结论

Halro 的**核心设计方向应保留**。它把自己限定为自托管、单进程、单写者的 LLM 访问安全与治理
边界，使用本地 Provider credential、Project、预算、路由、脱敏、审计和 Accounting Ledger 形成
窄腰；它没有滑向 Agent 编排、通用工作流、训练或“什么都做”的模型平台。单进程不是这里的落后
设计，而是换取更强状态所有权、较小运维面和可恢复性的主动选择。

当前阶段裁决是 **`REMEDIATION REQUIRED / PRODUCTION UNVERIFIED`**，原因不是核心架构失配，而是
实现增长、能力真相和运行证据还没有形成同样成熟的反馈闭环：

- 经独立反证，**没有确认 P0/P1**；最初唯一 P1 候选被降为 P2。
- 已确认的实现级 P2 包括：failure capture enqueue 前缺字节预算；portable Responses/Messages 的
  最终事件写失败后 Ledger 仍可能记 success。
- 结构性 P2 包括：current capability 文档分叉、少量关键 accounting/shutdown 信号缺 actionable
  alert/runbook；Provider operation→primitive 仍缺独立机械证明；Workbench 非流式 Go snippet 不可编译。
- E3/E4 缺口包括：已有最小 schema 双二进制 E3，但完整 backup/restore/kill-point 仍缺；真实
  Provider/KMS/PKI/Contact Point、独立 dead-man、完整浏览器旅程、production-shaped 容量曲线与
  24h soak 也未完成。
- 方案要求检查覆盖率至少 80% 才能给 100 分总分；本轮保守估计约 70%，因此**不输出官方总分**。

这个结论与 release readiness 分离。普通 CI 和 release dry-run 对精确 SHA 成功；正式 v0.8.0 的
quality、SDK、stress、制品、provenance、GitHub Release 和 GHCR 发布成功，但下游 package job 因
GitHub App client ID 缺失失败。快照时 Homebrew 仍为 v0.7.0，APT 状态不可验证。不能把这组事实
压成一个“发布成功”或“发布失败”。

## 2. Halro 应该是什么 / 不应该是什么

### 应该继续成为

- 一个小而可靠、可嵌入、自托管、协议中立的 LLM access-security boundary；
- 一个在 Provider I/O 前统一完成身份、Project、能力、预算、限流和敏感数据控制的窄腰；
- 一个用 Accounting Ledger 保存不可替代事实、用可重建投影服务查询的单写者系统；
- 一个对 unknown/ambiguous 明确 fail closed、不用自动 fallback 假装 exactly-once 的兼容层；
- 一个把 GA、Experimental、Withheld、Deferred 和 UNVERIFIED 明确区分的产品。

### 不应该扩张为

- Agent runtime、工作流编排、memory/skill 执行或业务 evaluator；
- 为尚未发生的 HA 需求提前引入的多写、共享数据库、消息队列或分布式事务；
- 以 Provider 数量、endpoint 数量、Admin 页面数作为价值代理的“大而全”平台；
- 把短 CI、fixture、微 benchmark 或 GitHub Release 成功描述成生产 SLO、真实 Provider 或全部包渠道已验证。

## 3. 五项最高杠杆决定

1. **统一 stream 的最终事实。** 调整 portable facade final-event 与 RequestFinalized 时序；保留真实
   Provider cost settlement，禁止 fallback，但调用方未收到协议终态时不能记请求 success。
2. **在诊断数据进入队列前限定字节。** failure capture 的 64 槽限制不能替代 byte limit；在
   enqueue 前截断或用 queued-byte semaphore，并用大载荷 RSS/GC 数据决定是否曾接近 P1。
3. **恢复 current-capability 单一真相。** 从 current 指南删除 withheld Bedrock 表项，更新已实现的
   Run Governance、ADR 范围与版本状态；让 manifest/golden 驱动摘要，减少手工列表。
4. **保留单进程，收缩组合根。** 不做微服务重构；先用 subsystem dependency table 下沉一个高内聚
   生命周期/路由组，并让 Provider operation→primitive 从独立来源得到可执行证明。
5. **暂停证据不足的扩张，先取得 E3/E4。** 完成升级/恢复、真实告警、target PKI/KMS、浏览器旅程、
   package clean-host 与 24h capacity/soak，再重算总分和生产准入。

## 4. 复杂度投入与用户价值

| 设计投入 | 用户价值 | 当前成本/风险 | 裁决 |
| --- | --- | --- | --- |
| 单进程、单 writer、一个 data dir | 清楚所有权、低运维、强本地控制 | 无水平多写；升级需停机 | KEEP，边界真实且被锁/部署约束 |
| Ledger + Governance Journal + Audit + rebuildable Usage | 账务、业务结果、审计各有权威 | 多 durable format 与恢复证明成本 | KEEP；补双二进制/kill-point E3，不合并成通用 event bus |
| Provider/Profile/operation/capability 分层 | 避免“存在即有能力”和错误 fallback | 新 Provider 跨多个 registry/测试/文档 | KEEP；补独立 primitive binding sabotage |
| 嵌入式 React Admin | 安全地降低配置/诊断门槛，无生产 Node 服务 | 12 页面、137 Admin routes，文档/示例易漂移 | KEEP；从服务端事实生成 current 状态，编译代码示例 |
| Run Governance | 将成本连接到外部业务结果，且不侵入 evaluator | 新 journal/routes/UI/文档长期成本 | KEEP boundary；用价值/删除账本证明继续投入 |
| failure capture | 真实失败可复现，默认关闭且审计读取 | pre-redaction 敏感正文与当前队列内存放大 | KEEP opt-in；先修 byte bound；read_only 全读是明示约束 |
| `internal/app.Runtime` 集中组合 | 启动/关闭/激活顺序集中可见 | 74 fields、10 locks、342 receiver methods，变更放大 | SIMPLIFY 渐进下沉；禁止架构表演式重写 |
| SBOM/签名/provenance/多渠道发布 | 可验证供应链和可安装性 | 渠道是 saga，凭据缺失可造成部分状态 | KEEP；publish 前自动 preflight、逐渠道状态报告 |

## 5. 十维成熟度

详细表见 [scorecard.md](scorecard.md)。当前维度分数为：D1 2、D2 2、D3 2、D4 2、D5 2、D6 2、
D7 3、D8 1.5、D9 2、D10 3。由于 S3/S4 覆盖不足，禁止相加形成对外 100 分总分。

整体形状很清楚：**设计与自动化基础强于目标环境和长期证据**。D7/D10 已接近可持续反馈环；D3/D5
的安全关键机制多，但受一个当前缺陷和 E3 缺口封顶；D8 是最弱维度，不是已证明性能差，而是没有
足够数据证明容量边界。

## 6. Findings 与对抗裁决

完整净化清单见 [findings.md](findings.md)，逐条非作者复核见
[adversarial-verdicts.md](adversarial-verdicts.md)。本报告只保留反证后的结论：

- P2：failure capture enqueue 前无 byte budget；portable stream caller/Ledger 终态分叉；current
  capability 文档分叉；Provider primitive binding 证据缺口；Workbench 非流式 Go snippet；
  shutdown-truncated/aged pending lease 告警闭环。
- P3：Runtime 组合根收缩、实验能力价值/删除证据、CLI help、完整本地 gate 入口、部分 Go evidence
  cache 语义、下游凭据自动 preflight、release evidence 文档漂移。
- ACCEPTED/UNVERIFIED：read_only 是实例级全读并包含启用后的 failure payload；AttemptStarted 与
  socket I/O 之间的不可原子窗口；无生产 SLO 时 Production Admission 保持 No-Go；真实 Provider
  不得从 fixture 推断，升级证据也只到 schema fence，不能冒充完整恢复演练。

## 7. 运行证据与限制

本地、托管 CI 和发布详情见 [runtime-evidence.md](runtime-evidence.md)：

- 前端隔离全量 44/44 files、604/604 tests 通过；typecheck 通过。
- Node 24 构建和 artifact secret scan 通过，但与 Node 22 提交 bundle 的 hash 不同；仓库支持工具链是
  Node 22，因此该结果不作为 drift 证据，生成变化已恢复。目标 SHA 的 GitHub Node 22 gate 通过。
- 全量 Go 在允许回环监听后除一个 dead-man 250ms timing test 外均通过；该 test 隔离 `-count=10`
  全过。保留为资源敏感的本地证据不稳定，不谎报全量绿色，也不在未复现下称产品阻塞。
- 各角色的 gateway/budget/ledger/provider/SafeTransport/capture/backup/observability 窄测与 race 证据
  通过；portable stream final-event 分叉在隔离目标副本中被确定性复现。

未执行的真实 Provider、KMS、PKI、Contact Point、生产数据、24h soak、多架构 clean-host 和辅助技术
矩阵都是明确限制，不是隐含 pass。

## 8. 决策与下一步

具体 30/60/90 天处置见 [roadmap.md](roadmap.md)。建议先关闭两个实现级 P2 和 current truth 漂移，
再用 31–60 天取得升级、浏览器、package 与 target alert 的 E3；61–90 天完成 production-shaped
capacity/soak、真实 KMS/PKI/Contact Point 与非作者恢复演练。

在达到 80% 检查覆盖前，本报告不得被改写为“Halro 获得某科技公司设计认证”或“生产已验证”。
Google、Redis/antirez、OpenAI、SQLite、Linux 和 Go 在本方案中只是提取可证伪原则的方法来源；最终
判断只来自 Halro 当前代码、可运行证据、外部状态和独立反证。
