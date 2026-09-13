# 设计处置路线图：KEEP / SIMPLIFY / DELETE / PROVE / DEFER

目标版本：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`

这是评估建议，不是已授权的实施计划。owner、日期和风险接受必须由项目负责人确认；真实 Provider、
KMS、外部告警、生产与付费运行仍需单独授权。

## 1. 立即保留的设计

- KEEP：single-binary、single-process、single-writer；内部模块化不能借机演变成微服务、共享数据库或消息队列。
- KEEP：bbolt metadata、Accounting Ledger、Governance Journal、Audit 与派生 Usage/Parquet 的权威拆分。
- KEEP：Provider I/O 前持久 reservation/started；ambiguous 或首字节后不 fallback；未知结果保守结算。
- KEEP：管理 mutation 的 durable record + audit intent 单提交点，以及 activation stale 时数据面 fail closed。
- KEEP：模型存在性、能力、可用性、证据来源和过期时间分离；未知不推断能力。
- KEEP：failure capture 默认关闭、加密、限时、审计读取；真实 Provider/KMS 测试保持 opt-in。
- KEEP：Run Governance 不运行 evaluator、不抓 evidence、不保存自由文本正文、不反向影响实时路由/预算。
- KEEP：嵌入式静态 Admin，不增加生产 Node runtime；发布制品使用 SBOM、签名和 provenance。

## 2. 0–30 天：关闭已确认 P2 与低成本真相缺口

| 优先级 | 处置 | 类型 | Owner 建议 | 成本 | 验收 |
| --- | --- | --- | --- | --- | --- |
| 1 | 将 Responses / portable Messages 的 complete/final event emission 纳入 `RequestFinalized` 前的统一边界 | SIMPLIFY | Gateway + Compatibility | S | final-event write error：无 fallback、保留 cost settlement、RequestFinalized 不得 success；两 facade 回归 |
| 2 | failure capture 在 enqueue 前按每侧 limit 截断，或引入 queued-byte semaphore | SIMPLIFY | Gateway + Security | S–M | blocking store + 大合法请求；queued bytes 硬上限；RSS/GC/p99 记录；disabled 无额外 marshal |
| 3 | 建立 current-capability 单一摘要，清理 withheld Bedrock 与已实现 Governance 的相反描述 | DELETE/SIMPLIFY | Product + Docs | S | README、指南、docs index、implementation status 与 manifest golden 一致 |
| 4 | 为 `shutdown_truncated_attempts` 与 aged pending lease 设计最小 actionable rule/runbook | PROVE | SRE + Accounting | S–M | promtool firing/non-firing/resolved；阈值关联请求合法上界；非作者 15 分钟定位 |
| 5 | 修复 Developer Workbench 非流式 Go snippet，并编译检查语言/streaming 组合 | SIMPLIFY | Frontend/DX | S | snippet 可编译、处理 error/status、关闭并有界读取 body；Key 仍来自环境变量 |
| 6 | 修正 release evidence 文档的 bundle-drift 错述 | DELETE | Docs/Release | XS | current 文档与解析后的 workflow step 一致 |
| 7 | 统一“快速检查”和“完整证据门禁”的名称/入口；证据型 race/CI/release test 使用 `-count=1` | SIMPLIFY | Build | XS–S | 快速入口保持快；完整入口覆盖 web build/drift；证据日志无 cached result |
| 8 | 正式发布前增加下游 App 安装/权限/变量的只读自动 preflight | PROVE | Release | S | 缺变量、缺 secret、未安装、权限不足均在 publish 前失败；transient downstream 可安全重放 |

发布建议：前两项在 production enable failure capture 或把 portable stream 作为稳定承诺前完成；其余不
要求撤回已发布的核心 v0.8.0 制品，但需要逐渠道诚实报告当前状态。

## 3. 31–60 天：降低演进放大并取得 E3

| 处置 | 类型 | Owner 建议 | 成本 | 验收 |
| --- | --- | --- | --- | --- |
| 用 subsystem dependency table 拆出一个高内聚 Runtime 生命周期/路由切片 | SIMPLIFY | Architecture + App | M | 不引入 DI 框架；Runtime fields/receivers/Open LOC 下降；startup/stale/shutdown/race 仍通过 |
| 让 `(profile, operation)` 的声明 primitive 与实际 adapter entry point 独立对照 | PROVE | Provider architecture | S–M | 交换两个同类型 primitive 的 sabotage 必须让测试失败；attempt record 也断言一致 |
| 当前 SHA 双二进制升级/回滚/restore 演练 | PROVE | Core Data + Release | M | v0.7.1 数据→v0.8.0；旧 binary 明确拒绝不兼容状态；backup restore 后 Ledger/Usage/readiness/doctor 一致 |
| 完整 Admin 浏览器旅程与可访问性矩阵 | PROVE | Frontend + Product QA | M | setup→Provider→Route→Project→Key→call→Usage→disable/revoke；键盘、焦点、320/768/1440、200%、中英文 |
| 对 capture、registry、redaction 和 WAL 信号做 paired alternating benchmark | PROVE | Performance owner | S–M | 8–10 样本、benchstat/置信区间；只对统计显著且用户可见变化立性能 finding |
| clean-host Homebrew/APT 安装与精确 SHA 渠道验收 | PROVE | Release/Packaging | M | 两渠道分别记录版本、digest、架构、安装/升级/卸载；网站只宣传已验收渠道 |
| 为 Experimental/Preview 建立轻量价值与删除账本 | PROVE | Product | S | 目标用户、job、成功代理、支持成本、晋级门槛、withhold/delete 日期；不要求默认遥测 |

## 4. 61–90 天：生产准入证据，而非继续扩功能

- 在 disposable target 环境验证真实 PKI/IAM/KMS、credential rotation 与撤权；每条证据绑定 SHA、配置和身份。
- 接真实 Contact Point，并在独立故障域验证 Halro/Prometheus/Alertmanager down、firing、resolved 与 heartbeat-loss。
- 运行 production-shaped 慢流、慢 Provider、大正文、多 Project、Admin 大表、backup、Audit、Ledger/WAL/TSDB 增长测试。
- 完成至少一次 24 小时 soak，记录 p50/p95/p99、吞吐、RSS、GC、goroutine、FD、queue、WAL/fsync 与恢复水位。
- 由非作者从 page 或故障症状执行 15 分钟诊断和 backup/restore drill，批准 RPO/RTO、SLI/SLO 与 error-budget 政策。
- 补最小持续 mutation/sabotage：revision gate、error classification、fsync/capability gate、stream finalization 被破坏时必须红。
- 上述证据 ID 未齐全前，Production Admission 继续 No-Go；不得以普通 CI、短 smoke 或微基准替代。

## 5. 应删除或改写的内容

- DELETE：current 文档中“Run Governance 尚未开始”的失效状态；如需保留，移入带日期的历史计划。
- DELETE：current setup guide 中被代码主动 withheld 的 Bedrock Runtime/Agent Runtime 表项。
- DELETE：docs index 的旧 ADR 范围和仍作为 current 的 v1.0 RC critical path；保留历史价值但加冻结横幅。
- DELETE：release evidence 中“release 不检查 bundle drift”的相反陈述。
- DELETE：不能完成一次可运行请求的 Go 示例尾部；以编译和真实错误处理替换。

## 6. 应明确延后的扩张

- DEFER：Realtime、HA/Cluster、多写、动态插件 ABI；直到有明确用户、容量、故障与 owner 证据。
- DEFER：为 read-only payload 单独引入通用 permission matrix。当前两角色模型是明确契约；如需第三种
  support-viewer 身份，先证明真实角色需求。可先把“包括启用后的失败正文”写得更直白并要求远程 MFA。
- DEFER：仅因代码集中而引入微服务/通用 DI/Cobra 等大框架；优先做小型内部组件和 command descriptor。
- DEFER：用更多 Provider 数量或 API 数量衡量产品进步。先完成当前兼容、升级、容量和渠道证据。

## 7. 复评门槛

30 天复评：两个核心 P2 有回归，文档/门禁/发布 preflight 关闭；目标提交重新冻结。

60 天复评：D3 有双二进制 E3，D1/D9 有浏览器旅程 E3，D2 有独立 primitive contract，包渠道分别验收。

90 天复评：D4/D5/D8 获 target-environment E3，关键外部项有获授权 E4 或保持具名 No-Go。覆盖率达到
80% 后才输出 100 分总分与分档 verdict。
