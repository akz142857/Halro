# Range Map 与功能裁决

基线范围：`v0.8.5`（`28ea173b8ccfa2a8043a767c4c4d2782018a4d1c`）..冻结 `main`（`2e749db132751c4e1bab947e0af1856be7161039`）。

## 分段裁决

| 段 | 合并记录 | 裁决 | 依据 |
| --- | --- | --- | --- |
| A | v0.8.5..`0c4a270d`（#316–#384，52 提交） | **复核通过** | `revalidation-260927.md`：R-xxx 10 项仍有效、4 项随 HA 移除、0 项失效 |
| B | `c8b97b1b`、#385、#386、#387 | **复核通过** | `c8b97b1b` 的非 HA 部分 86 个文件补丁完整存在；#385、#386 被 #404 整体回退 |
| C | #404（`dc704030`） | **PASS** | `revert-integrity.md`：无残留、无误删、无行为倒退 |
| D | #405 #406 #407 #408 #421 #422 #423 #424 | **FAIL（1 个 P1）/ 有条件** | V-001（P1）；V-002–V-006、V-009 |
| E | #410 #411 #415 #417 #396 #425、`f018cc8a` | **PASS** | Actions pin 均与上游 tag SHA 一致；依赖许可证复核见 #425；chi 5.4.0 行为变化已读 |

## D 段逐合并

| 合并 | 内容 | 裁决 | 说明 |
| --- | --- | --- | --- |
| #405 | 重设计方案与 v1 归档 | PASS（文档） | V-008：归档稿 46 个死链 |
| #406 | P0 基线探针与 pprof | PASS | pprof 只接受字面回环地址、默认关闭（`internal/diagnostics/server.go`） |
| #407/#408 | P1 用量历史 `history.db` | **FAIL** | V-001（写入失败时批次无界、重启不恢复）；V-004（无告警）；磁盘无保留期（`memory-bounds.md`） |
| #421 | P2 Ledger 热状态快照、封存默认开、对账 | PARTIAL | 快照恢复与链绑定 E01/E03 实测正确；V-003（快照 6× 堆）、V-006（封存默认值到不了已有配置）、V-004（对账/快照失败无告警） |
| #422 | P3 治理归档 `archive.db` | PASS（仓库）/ 有观察 | 归档在锁内搬移、先写后删、位置不回退；E01/E03/E07 Work Unit 列表在快照/重建/恢复间一致；V-009 |
| #423/#424 | 热状态上限 | PARTIAL | 准入与释放路径正确（7/7 入口 `defer run.close()`）；V-002（延迟层不放回队列）；V-003；E06 实测见 `runtime-evidence.md` |

## 功能项最终裁决

| 编号 | 功能 | 裁决 | Finding / 证据 |
| --- | --- | --- | --- |
| F01 | RouteGate | PASS | 热状态 503 在 `beginRequestRun` 内、任何尝试之前，不计入挂起（X19） |
| F02 | 路由挂起 | PASS | R-007/008/009 仍有效 |
| F03 | `/v1/models` 与 `discovery` | PASS | R-013；升级后已有 Key 无 `discovery`，E01 实测 403 |
| F04 | 固定速率上限 | PASS | R-014；表按 Key/Project 封顶 |
| F05 | 调用方幂等 | PASS | 单机 `in_flight` 先落盘再调上游；热状态拒绝归还 key（X16） |
| F06 | 配置 v2 与迁移 | PASS / V-006 | 14 个历史快照迁移后可加载；E01 实测 |
| F07 | Kimi Code / Claude 订阅 | PARTIAL | R-005 有效；真实上游 BLOCKED（E09） |
| F08 | 控制台可解释性 | PASS（仓库） | R-009；无浏览器证据 |
| F09 | Advisor | PASS | 不依赖 P1 删除的 rollup |
| F10 | 可观测性 | PARTIAL | 引用的指标全部存在；V-004 |
| F11/F12 | journal / HA | 已移除 | `revert-integrity.md` |
| F13 | 发布门禁 | PARTIAL | 门禁有效；V-007 |
| F14 | 依赖 | PASS | #425 |
| F15 | 设计稿 | PASS | — |
| M01 | 用量历史 | **FAIL** | V-001、V-004 |
| M02 | 热状态快照启动 | PASS / V-003 | E01、E03 |
| M03 | 封存默认开 | PARTIAL | V-006 |
| M04 | 治理归档 | PASS / V-009 | E01、E03、E07 |
| M05 | 热状态上限 | PARTIAL | V-002、V-003、E06 |
| M06 | 每小时对账 | PARTIAL | 只计数与记日志，无告警（V-004） |
| M07 | pprof | PASS | 回环、默认关闭 |
| M08 | 工具链与依赖 | PASS | — |

## 交叉点 X10–X19

| ID | 裁决 | 依据 |
| --- | --- | --- |
| X10 快照 × 封存 × 压缩 × 备份 | **PASS** | 已封存代内快照恢复有测试（`TestLedgerStateResumesFromASnapshotInASealedGeneration`）；压缩代由一次性测试补证；E03 跨 5 次封存的 11 次 SIGKILL 全部恢复；E07 备份不带 `state.snap`，恢复后全量重建且结果一致 |
| X11 归档 × 治理日志 × 幂等 | **PASS** | 治理幂等查找按 热状态 → 队列 → 归档，归档读失败为错误；`Apply` 内的重复由热 map 去重；E01/E03 的 Work Unit 视图在重建前后一致 |
| X12 热状态上限 × 延迟层 | **FAIL** | V-002 |
| X13 用量历史 × 离线命令 × 对账 | PARTIAL | `usage verify` 只核对已导出前缀（既有语义）；`history.db` 无保留期；对账只告知日志（V-004） |
| X14 回退 × 挂起/幂等 | **PASS** | R-007 反向验证；`in_flight` 单机持久 |
| X15 配置迁移 × 新键默认值 | PARTIAL | V-006 |
| X16 幂等 × 结算后剪裁 | **PASS** | 幂等记录在 bbolt `provider_resources`，与 Ledger 状态剪裁无关；二次 `Settle` 仍被拒（与 v0.8.5 同） |
| X17 离线诊断 × 新文件 | **FAIL** | V-005 |
| X18 全局内存有界 | **FAIL** | `memory-bounds.md`：V-001 无界；V-003 有界但 6× |
| X19 热状态拒绝 × RouteGate | **PASS** | 拒绝发生在 `BeginRequestAttributed`，早于任何尝试与挂起记录 |
