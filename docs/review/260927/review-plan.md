# Halro v0.8.5 至当前 `main` 功能清单与专项 Review 方案

> 状态：已执行；原始裁决为 **No-Go**，随后完成 R-001–R-014 的仓库级整改与复验。原始结果见 [`review-report.md`](review-report.md)，整改结论见 [`remediation-verification.md`](remediation-verification.md)，命令证据见 [`runtime-evidence.md`](runtime-evidence.md)，严重 finding 的交叉验证见 [`adversarial-verdicts.md`](adversarial-verdicts.md)。目标环境 HA 验收仍保持 BLOCKED，仓库通过不等于生产启用授权。
>
> 制定日期：2026-09-27
>
> 对比范围：`v0.8.5`（`28ea173b8ccfa2a8043a767c4c4d2782018a4d1c`，2026-09-20）到当前 `main`（`0c4a270d7aae2b3d4562c35cd60ff14907278897`，2026-09-27）。两端之间共 52 个提交，当前制定工作区干净。
>
> 规模：508 个文件发生变化，约 55,090 行新增、2,621 行删除。Review 必须以风险和不变量为中心，不能退化成逐提交复述或仅跑一次全量测试。

## 1. Review 目标

本轮回答五个问题：

1. `v0.8.5` 之后新增或改变了哪些真实能力，哪些只是文档、证据或依赖维护；
2. 新能力是否保持 Halro 的账务权威、失败关闭、最小泄露、资源有界和协议兼容边界；
3. 路由准入、调用方幂等、配置迁移、metadata journal 与 HA 之间的组合是否产生单项测试看不到的缺口；
4. Admin、CLI、API、指标、告警、runbook、兼容性清单和实际运行行为是否描述同一个产品；
5. 当前代码是否只达到“仓库实现完成”，还是已有足够的目标环境证据支持发布或生产启用。

本轮是 **delta review**：重点审查 `v0.8.5..HEAD` 的新增面及其与既有不变量的交叉影响。若发现变化触及旧代码的权威边界，则沿调用链向基线以前追溯，不以 Git diff 边界截断事实。

## 2. 状态词的统一含义

| 状态 | 含义 |
| --- | --- |
| 已实现 | 运行时代码已进入当前 `main`，但尚未代表本轮 Review 通过 |
| 实验性 | 已有实现和局部契约，但尚未达到稳定兼容承诺 |
| 仓库完成 | 代码、确定性测试、仓库配置和文档已具备；目标环境验收仍独立存在 |
| 证据/门禁 | 不增加直接产品能力，用于证明、约束或诊断已有能力 |
| 设计稿 | 不得按已上线功能验收，也不得写入“已交付功能”结论 |

Review 报告必须分别给出：代码结论、仓库门禁结论、CI/集成结论、目标环境结论、生产验收结论。后四者不能互相替代。

## 3. `v0.8.5..HEAD` 功能清单

### F01 · 统一路由准入与基于拒绝原因的故障转移

- **状态**：已实现；替代原 `internal/circuit` 与独立的 probe-health 判定。
- **能力**：新增 `internal/routegate`，按 credential、credential+model、provider、deployment 等真实作用域记录拒绝；401、配额耗尽、限流和可用性失败使用不同恢复策略；非歧义拒绝可继续尝试下一个候选，可能已产生 Provider 副作用的歧义失败仍停止回退。
- **行为变化**：配置从 `circuit_breaker` 迁移到 `routing`；新安装默认 `max_total_attempts=4`、`max_attempts_per_target=1`，可到达第三、第四个 fallback target。
- **主要证据入口**：`internal/routegate/`、`internal/gateway/service.go`、`internal/gateway/refusal_codes.go`、`docs/todo/route-eligibility-design.zh-CN.md`、`docs/guides/alias-failover.zh-CN.md`。
- **Review 重点**：失败原因到作用域的映射、credential revision 自愈、`Retry-After`、流式首字节前/后回退边界、歧义调用不重复执行、候选优先级与尝试预算。

### F02 · 路由挂起持久化、人工清除与操作面可见性

- **状态**：已实现；metadata schema 39。
- **能力**：长时挂起跨重启保留；Admin API、CLI 和控制台可查看；可清除持久挂起并与审计意图同事务提交；新增挂起 gauge、transition/probe counter、告警和 runbook。
- **主要证据入口**：`internal/routegate/persistence.go`、`internal/store/bolt/store_route_suspensions.go`、`internal/app/admin_route_suspensions.go`、`web/src/pages/RouteSuspensionsPanel.tsx`、`deploy/observability/prometheus/alert-rules.yml`。
- **Review 重点**：clear 与并发失败写回的线性化、重启恢复、revision 失效、Admin RBAC/step-up/audit、低基数指标、控制台轮询与缓存失效。

### F03 · Gateway 模型别名发现 API 与 `discovery` scope

- **状态**：实验性。
- **能力**：新增 `GET /v1/models`、`GET /v1/models/{id}`，只列当前 Project 被允许且路由表存在的 Halro 公共 alias；不泄露上游模型、credential 或 deployment；新增 Gateway Key `discovery` scope，且必须同时具有 `inference`。
- **边界**：列出 alias 只保证它不会返回 `model_not_found`，不保证其当前健康、未挂起或支持调用方选择的 operation。
- **主要证据入口**：`internal/gateway/models.go`、`internal/gatewayapi/models.go`、`internal/openaiapi/models.go`、`docs/contracts/openai-compatibility.md`、`docs/compatibility/endpoint-manifests.json`。
- **Review 重点**：Project 隔离、404/403 防枚举语义、带 `/`、`%` 和双重编码的 alias、scope 默认值、SDK 兼容矩阵尚未覆盖时能否继续标记 experimental。

### F04 · 认证后本地读取的不可关闭速率上限

- **状态**：已实现。
- **能力**：对模型发现和资源平面中不进入 Provider/Project limiter 的读取，增加每 Gateway Key 600 RPM、每 Project 5,000 RPM 的固定上限；超限返回 `429 rate_limit_exceeded` 与 `Retry-After`。
- **主要证据入口**：`internal/keylimit/`、`internal/gateway/key_rate.go`、`internal/app/governance_rate.go`、`docs/contracts/gateway-correctness.md`。
- **Review 重点**：认证前 source limiter、认证后 key limiter、Provider 前 Project limiter 三层顺序；404/取消/轮询是否收费正确；内存基数、窗口回收、公平性和并发精确性。

### F05 · Chat/Embeddings 调用方幂等

- **状态**：已实现；metadata schema 40。
- **能力**：`POST /v1/chat/completions` 与 `POST /v1/embeddings` 支持 `Idempotency-Key`，提供上游 at-most-once，而不是响应重放；重复请求区分 conflict、in-progress/unknown 和 completed；未到 Provider 的本地拒绝释放 key，已触达 Provider 的失败消费 key。
- **数据边界**：只保存 key hash、请求指纹、路由和生命周期，不保存请求或响应正文；记录 24 小时后过期。
- **主要证据入口**：`internal/gateway/inference_idempotency.go`、`internal/gateway/inference_idempotency_test.go`、`internal/requestmeta/idempotency_key.go`、`docs/contracts/idempotency-contract.md`。
- **Review 重点**：发送前/发送后边界、流式拒绝时点、并发相同 key、进程崩溃后的 reservation 回收、Project 隔离、store 不可读时必须 503、HA 提升后记录仍一致。

### F06 · 配置 schema v2 与显式迁移命令

- **状态**：已实现；属于破坏性配置变化。
- **能力**：新增 `halro config migrate`；dry-run 默认、`--write` 才落盘，保存 `.before-migrate`；通过退休键表迁移名称保持语义的键，拒绝自动继承语义已变化的键；旧配置可向前迁移，未来版本配置 fail closed。
- **覆盖范围**：包含 `circuit_breaker` → `routing`、旧 `providers` 段删除，以及 v0.1.0–v0.8.5 发布配置快照的迁移加载门禁。
- **主要证据入口**：`cmd/halro/config_migrate.go`、`internal/config/retirement.go`、`internal/config/testdata/releases/`、`tools/release/prepare_release.py`、`docs/guides/configuration-findings.zh-CN.md`。
- **Review 重点**：原子写、备份覆盖策略、注释/权限/换行保留、部分迁移不得落盘、重复运行幂等、每个历史快照都能产生当前可加载配置、操作文档是否清楚说明人工决策项。

### F07 · Kimi Code 与 Claude 订阅产品接入

- **状态**：已实现，但真实 Provider 行为只能由留存的真实响应证据或经授权 smoke 证明。
- **能力**：新增 Kimi Code 订阅 profile/兼容契约；Claude 订阅作为 operator 显式启用的产品接入；拒绝把 Claude subscription token 当作普通 Anthropic API key。
- **主要证据入口**：`internal/compatibility/kimi_code.go`、`internal/app/claude_subscription_credential.go`、`internal/app/profile_offering_gate.go`、`docs/verification/kimi-code-subscription-evidence.md`、`docs/verification/anthropic-claude-subscription-evidence.md`。
- **Review 重点**：Offering/Profile/credential 类型绑定、条款与显式开关、secret 误用拒绝、模型枚举与能力证据分离、真实上游证据的新鲜度；未经用户明确授权不得运行计费 smoke。

### F08 · Admin 中 alias、拒绝状态和 Usage 可解释性改进

- **状态**：已实现。
- **能力**：Routes 页面把 alias 提升为一等分组对象；Providers 页面展示上游拒绝、修复动作、credential revision 和 accounting timezone；Usage 汇总逐行标明估算 token；空态、错误态、轮询和表格可读性同步调整。
- **主要证据入口**：`web/src/pages/RoutesPage.tsx`、`web/src/pages/ProvidersPage.tsx`、`web/src/pages/UsageSummaryPanel.tsx`、对应页面测试与中英文 locale。
- **Review 重点**：后端事实到 UI 文案是否一一对应、时间区、过期查询、键盘/窄屏、只读角色、危险动作确认、API 清除成功后的缓存一致性。

### F09 · 配置 Advisor 与可行动诊断

- **状态**：已实现。
- **能力**：纯函数 `internal/advisor` 对 attempt header timeout、route total budget、retry 数、fallback fan-out、当前挂起和未分类拒绝给出带原始比较值的 finding；同时进入 `halro doctor` 和 `GET /admin/api/v1/advisor-findings`，控制台在 Settings & Status 展示。
- **伴随变化**：延迟 histogram 新增 45s、60s、90s 桶，usage checkpoint 14→15、rollup 1→2 后重建。
- **主要证据入口**：`internal/advisor/`、`internal/app/doctor_advisor.go`、`internal/app/admin_advisor.go`、`web/src/pages/AdvisorFindingsPanel.tsx`。
- **Review 重点**：finding 与 health check 不混淆；每条规则既有 positive control 也有“已检查且正常”；输入不含用户正文；迁移/重建成本；CLI 和 Admin 输出一致。

### F10 · 可观测性、审计与崩溃诊断加固

- **状态**：证据/门禁与部分运行时能力。
- **能力**：新增流式首字节 histogram；Provider refusal 原因指标；Project budget exhaustion 告警；Audit pending/append 失败观测与告警；审计链缺帧、重排、sequence 和 re-sign 场景测试；真实 `SIGKILL` 后的请求可恢复性门禁；timeout 分类修正为 504 `provider_timeout`。
- **主要证据入口**：`internal/gatewayapi/first_byte_metrics.go`、`internal/gateway/failure_reason_metrics.go`、`internal/audit/`、`internal/app/crash_sigkill_test.go`、`deploy/observability/`。
- **Review 重点**：首字节起点与 flush 时点、失败样本不美化分位数、指标 label 基数、告警能否由规则测试触发、runbook 是否指向真实修复动作、SIGKILL 失败是否揭示真实 durable bug 而非直接标 flaky。

### F11 · Metadata journal：让 `halro.db` 成为可重建投影

- **状态**：已实现，是 HA 的 Phase 0a 基础，同时改变 Standalone 写路径。
- **能力**：对 bbolt 权威元数据建立 authenticated journal；按 bucket/key 区分权威、会话、派生、密钥信封和节点本地状态；事务提交与 journal 记录绑定；backup/restore 携带并原子发布 journal epoch。
- **主要证据入口**：`internal/metadatajournal/`、`internal/store/bolt/journal_*.go`、`docs/adr/0027-ha-replication-formats.md`、`docs/todo/halro-ha-architecture.zh-CN.md` §5–6。
- **Review 重点**：所有生产 `Update/Batch` 入口均被覆盖；跨类同事务只允许显式白名单；journal 写失败不产生无记录 mutation；Standalone 性能和恢复；备份、restore、trim、epoch 与数据库前缀一致。

### F12 · Primary/Replica HA、人工提升、备份与部署资产

- **状态**：**仓库完成，目标环境未验收**；自动故障切换明确不在 v1 范围内。
- **能力**：2–3 节点物理复制；ordering journal、frame/ACK/commit notice、confirmed/applied index；mTLS 1.3 + SPKI pin + Master Key proof；真实 listener/dialer；Primary/Replica 角色化 HTTP；Provider object 分块复制；seed/catch-up/re-seed；manual promote、planned stepdown、Replica backup/report、leave；Kubernetes StatefulSet、告警、HA 与 CA rotation runbook。
- **提交语义**：本机持久化 + 至少一个 Replica ACK；`ReservationCreated`、`AttemptStarted` 及撤销/降权类元数据在生效前必须 confirmed；Replica 只 apply confirmed 前缀；角色、term、schema、密钥或前缀不明确时 fail closed。
- **主要证据入口**：`internal/replication/`、`internal/app/replication_*.go`、四个权威存储的 `replica_*` / `replication_*` 文件、`deploy/kubernetes/halro-ha-statefulset.yaml`、`docs/runbooks/ha-operations.md`、`docs/verification/ha-repository-gates.md`。
- **Review 重点**：双 Primary 防护、ACK 后崩溃、commit notice 丢失、旧 Primary fencing、提升 term、对象元数据先于字节、seed 路径/权限/symlink、Replica 不发生权威本地写、备份一致前缀、schema-changing upgrade、会话失效和审计身份。
- **仍需外部证据**：kind 删除 Pod/网络与探针行为；相邻发布二进制升级；Linux ENOSPC/只读/慢盘；三节点性能；G0–G7、72 小时 soak 和真实 RTO。任何本地 `go test` 均不能把这些项标为 PASS。

### F13 · 配置、构建、供应链与发布门禁加固

- **状态**：证据/门禁。
- **能力**：所有配置键及 validation bound 进入双语引用并由反射门禁守护；`make full-check` 与较短 `make check` 的区别被机器检查；本地构建版本成为显式 build input，避免旧二进制携带陈旧 SHA；SBOM gate 区分 registry 不可达与无效 SPDX，release 仍要求 SBOM；发布准备工具和 release notes 生成链路加强。
- **主要证据入口**：`internal/app/config_reference_coverage_test.go`、`internal/app/config_constraint_documentation_test.go`、`tools/gates/`、`scripts/sbom-gate.sh`、`Makefile`、`.github/workflows/ci.yml`、`.github/workflows/release.yml`。
- **Review 重点**：门禁是否真的覆盖声明、release 与 ordinary CI 的 fail-open 差异、构建身份在两支二进制和所有 artifact 中一致、registry 故障不能掩盖损坏 SBOM、内嵌 bundle 不漂移。

### F14 · SDK/运行时依赖刷新与许可证证据

- **状态**：维护性变化，不作为新产品功能。
- **范围**：Go/Node/Python OpenAI 与 Anthropic compatibility SDK、AWS KMS/Smithy runtime、Admin React 依赖；新增 Go compatibility 图中的 ISC 许可证说明；重建 `internal/webui/dist`。
- **主要证据入口**：`tests/compatibility/*`、`go.mod` / `go.sum`、`web/package*.json`、`docs/verification/dependency-license-review.md`。
- **Review 重点**：锁文件精确性、许可证与漏洞状态、compatibility server 黑盒结果、AWS KMS 运行时影响、生成 bundle 与源码一致。

### F15 · 仅设计或跟踪的内容

- **状态**：不得当作已交付能力。
- `docs/todo/agent-self-description-plan.zh-CN.md` 是实例自描述/Agent channel 方案；只审查其是否被误写入当前产品承诺，不做运行时验收。
- HA 自动故障切换已明确关闭，当前只允许人工提升；不得因 deadman 或 promotion primitives 存在而宣称自动 failover。

## 4. 跨功能高风险交叉点

单个功能各自通过测试仍不足以关闭以下组合风险：

| ID | 交叉点 | 必须证明的命题 |
| --- | --- | --- |
| X01 | routegate × streaming × idempotency | 首字节前可安全 fallback；首字节后绝不换 Provider；已触达上游的 key 不被释放 |
| X02 | route suspension × metadata journal × HA | 节点本地挂起不被错误复制；clear 的审计意图仍复制；提升后不会把“本地观测”冒充集群事实 |
| X03 | idempotency × metadata journal × promotion | 已确认的 key 生命周期在提升后仍答相同状态；未确认后缀按恢复契约处理，不重复 Provider 副作用 |
| X04 | revocation × confirmed mutation × promotion | Gateway Key、credential、Project、Admin/MFA 的撤销或降权在调用方成功后，提升不得复活旧权限 |
| X05 | config migrate × HA member identity | 配置迁移不能重写 cluster identity/trust；已有 `cluster/` 时删除 replication 配置不能降级为 Standalone |
| X06 | Provider subscription × enumeration × capability | 上游“谁存在”和 catalog“能做什么”保持分离；手填 ID 不得获得伪造 `provider_metadata` 能力 |
| X07 | Advisor × metrics × operator action | finding、metric、alert 和 runbook 对同一条件使用同一语义，且不会把可接受配置描述成 unhealthy |
| X08 | backup/restore × journal epoch × HA incarnation | 备份内所有权威存储对应同一 applied prefix；restore 原子发布新 epoch；HA restore 强制新 incarnation |
| X09 | build identity × release evidence | source SHA、二进制版本、容器/包、SBOM、provenance 和运行实例可追溯到同一提交 |

## 5. 必须守住的不变量

| ID | 不变量 | 最低 Review 证据 |
| --- | --- | --- |
| INV-01 | 一个任期内至多一个进程确认权威写；更高 term 的 durable promise 会 fence 旧 Primary | promotion/stepdown/demotion 定向测试 + 双 Runtime 故障时序 |
| INV-02 | 至多一个合法角色发起新的 Provider 副作用；结果不明确时不自动重放 | 发送前、发送后、首字节前后、进程暂停/重启矩阵 |
| INV-03 | Ledger 仍是账务权威；reservation、attempt、settlement 与 conservative recovery 不重复、不遗漏 | Ledger 事件与调用结果逐场景对照 |
| INV-04 | 已确认 mutation 在合法切换后不丢；撤销/降权不得在提升后复活 | 双节点真实 mTLS Runtime + reopen projection 验证 |
| INV-05 | Replica 的 native bytes 始终是 Primary 曾持久化前缀；同一 index 不得对应不同字节 | corruption、reorder、partial tail、same-cursor-different-bytes 测试 |
| INV-06 | `halro.db` 的复制部分完全由 authenticated metadata journal 决定；节点派生状态不伪装成权威数据 | 全写入口清单、分类守护、跨类事务白名单 |
| INV-07 | Project/Gateway Key 隔离、scope、RBAC、MFA 和 step-up 不因新增读取/CLI/Replica Admin 路径而绕过 | 角色 × 资源 × 动作授权矩阵 |
| INV-08 | 错误响应、指标和日志不泄露 Provider、credential、其他 Project alias 或用户正文 | 黑盒响应检查 + label/log 字段审查 |
| INV-09 | 所有 limiter、队列、decoder、对象 staging、日志和指标基数有硬上限且可回收 | 并发边界、超限、取消、重启和长稳证据 |
| INV-10 | 旧配置、旧数据、未来版本、损坏状态和不兼容 schema 均有明确且 fail-closed 的处理 | 历史快照迁移、坏文件、旧/新二进制矩阵 |

## 6. Review 执行阶段

### S0 · 冻结基线与建立范围账本（0.5 人日）

1. 记录 SHA、tag、工作区、Go/Node/npm 版本和平台；
2. 导出 `v0.8.5..HEAD` 的提交、文件、路由、配置、schema、指标和 CLI 变化；
3. 为 F01–F15 建立“实现 → 测试 → 文档 → 外部证据”矩阵；
4. 把历史 issue/PR 的完成状态与当前代码交叉核对，但不把 issue checkbox 当作实现证据。

退出条件：每个变更文件归属一个功能或维护项；每个功能有 owner、风险等级和验证入口。

### S1 · 契约、架构与数据流 Review（1–1.5 人日）

- 画出 Gateway 请求从认证、scope、key limiter、Project limiter、routegate、Provider 到 Ledger 结算的时序；
- 画出 Primary mutation 从本地存储、ordering、Replica ACK、confirmed、apply 到调用方响应的时序；
- 核对 config v2、metadata schema 39/40、journal epoch、HA state v2 和 replication protocol 的版本边界；
- 对 F03、F07、F12、F15 分别确认“实验性、真实证据、仓库完成、设计稿”的文档状态没有串位；
- 对 X01–X09 给出代码路径和测试路径，不接受只引用设计文档。

退出条件：INV-01–INV-10 均有实际入口、权威状态、失败方向和 oracle。

### S2 · 专项源码与定向测试（4–6 人日，可并行）

| Track | 范围 | 主要产出 |
| --- | --- | --- |
| T1 路由/协议 | F01、F03、F04、F05 | 候选状态机、error/SSE/idempotency 矩阵、SDK 行为 |
| T2 配置/升级 | F06、F11 的 Standalone 部分 | 历史配置迁移矩阵、journal 覆盖清单、升级/回退裁决 |
| T3 Provider | F07 及拒绝分类 | Offering/Profile/credential 绑定、真实证据边界、未知项 |
| T4 Admin/UX | F02、F08、F09 | 浏览器关键旅程、RBAC、缓存、i18n、窄屏与可访问性 |
| T5 账务/审计/观测 | F05、F09、F10、F11 | crash 时序、审计完整性、指标和告警可执行性 |
| T6 HA | F12 与 X02–X08 | term/index/prefix/seed/promotion/backup 故障矩阵 |
| T7 交付/供应链 | F13、F14 | CI、依赖、许可证、SBOM、身份与 bundle 证据 |

每个 Track 必须提交：读过的文件、运行过的命令、确认项、候选 finding、未验证项和外部阻塞。没有证据的“未发现问题”无效。

### S3 · 对抗验证与组合故障实验（2–3 人日）

对全部 P0/P1 候选以及 X01–X09 进行独立证伪。验证者默认 finding 是错的，必须找出完整可达路径或现存拦截，裁决为 `CONFIRMED`、`REFUTED`、`PARTIAL` 或 `BLOCKED`。

最低故障组合：

- 相同 `Idempotency-Key` 并发、Provider 写前失败、写后断链、进程 SIGKILL、提升后重试；
- route suspension 写入/clear/credential replace 与 registry reload 并发；
- Primary 在 ACK 前、ACK 后 state publish 前、confirmed 后 commit notice 前、调用方响应前分别退出；
- ordering/native tail 部分写、完整但缺 ordering 的后缀、同 cursor 不同 bytes、Roll 中断；
- seed staging symlink、宽权限、路径逃逸、digest/manifest/index 篡改；
- Replica 上误触 maintenance、Admin mutation、离线命令和 schema migration；
- config migrate 途中写失败、重复迁移、未来版本、语义变化键；
- Admin 只读角色、过期 step-up、MFA replay、跨 Project model discovery。

### S4 · 集成门禁与交付物（1 人日 + 环境时间）

在代码未再变化后只运行一次完整仓库门禁，汇总本地、CI 和外部证据。文档追加本身不触发重跑已经成功的代码门禁。

交付物：

- `range-map.md`：提交/文件/功能/测试映射；
- `architecture-and-invariants.md`：时序、权威状态和不变量裁决；
- `findings/`：一项一文件，含复现与关闭条件；
- `adversarial-verdicts.md`：严重发现的独立证伪；
- `runtime-evidence.md`：命令、环境、退出码、日志和适用范围；
- `review-report.md`：最终结论与 Go/No-Go；
- `progress.md`：整改、复验与外部阻塞，不回写或篡改原始报告。

## 7. 建议验证矩阵

以下命令是 Review 计划，不表示本文制定时已运行。执行时遵循根目录 `AGENTS.md`：先定向、后包级，真实读取结果时使用 `-count=1`；并发/生命周期变化才运行受影响包的 `-race`；最后完整 gate 只跑一次。

### 7.1 Go 定向验证

```bash
go test -count=1 ./internal/routegate/ ./internal/gateway/ ./internal/gatewayapi/
go test -count=1 ./internal/keylimit/ ./internal/advisor/
go test -count=1 ./internal/config/ ./cmd/halro/
go test -count=1 ./internal/metadatajournal/ ./internal/store/bolt/
go test -count=1 ./internal/ledger/ ./internal/audit/ ./internal/governance/
go test -race -count=1 ./internal/replication/
go test -race -count=1 ./internal/app/ -run 'Replication|Cluster|SIGKILL|Audit|Idempotency'
```

若 sandbox 禁止 loopback，保留原始失败后在允许本地监听的环境中重跑完全相同命令；不得把 `bind: operation not permitted` 归类为代码回归，也不得把未重跑归类为通过。

### 7.2 Admin 前端

```bash
cd web
npx vitest run src/pages/RouteSuspensionsPanel.test.tsx
npx vitest run src/pages/AdvisorFindingsPanel.test.tsx
npx vitest run src/pages/RoutesPage.test.tsx src/pages/ProvidersPage.test.tsx
npx vitest run src/pages/UsageSummaryPanel.test.tsx src/pages/DeveloperPage.test.tsx
npm run typecheck
```

浏览器至少覆盖 1440px、1024px、390px：登录 → 查看挂起 → 修改 credential → 验证状态消失；通过 Admin API/CLI 单独验证持久挂起的 clear（当前控制台没有 clear 按钮）；查看 advisor finding；创建带/不带 `discovery` 的 key；核对 alias 与 Usage 估算提示。页面测试证明组件行为，真实浏览器证明完整旅程，二者不能互相替代。

### 7.3 契约、可观测性与兼容性

```bash
./deploy/observability/validate.sh
go test -count=1 ./deploy/kubernetes/
python3 tools/gates/test_documented_gate_contract.py
python3 tools/gates/test_build_identity_contract.py
go -C tests/compatibility/go test -count=1 ./...
npm test --prefix tests/compatibility/node
python tests/compatibility/python/test_sdk.py
```

SDK 命令执行前按 CI 建立干净依赖环境；漏洞扫描和依赖下载若需要网络，单独记录网络条件与结果。不能用已有本地 `node_modules` 或历史日志冒充本轮证据。

### 7.4 最终完整 gate

```bash
make full-check
git diff --exit-code -- internal/webui/dist
git diff --check
```

`make full-check` 包含当前仓库定义的 Go、race、vet、前端测试、typecheck、production build 和 observability 检查。不得在每个专项后重复运行；只在最终候选代码树上运行一次。若 Review 只产生 Markdown 文档，不需要因此重跑代码门禁。

## 8. 外部验收与禁止越界

以下项目不能由仓库测试代替：

- 真实 Provider 响应形状、订阅条款和计费调用；未获用户明确授权不得执行 billable smoke；
- kind 中 Pod 删除、PDB、endpoint 更新、SIGTERM/SSE drain、维护哨兵和 restore/re-seed；
- 相邻正式版本二进制的完整滚动升级与不兼容 schema 离线恢复；
- Linux 参考机的 ENOSPC、只读、慢盘和 Standalone/三节点性能对比；
- G0–G7 生产验证、72 小时 soak、真实事件响应时间和 RTO；
- GitHub Environment、secrets、发布签名或外部 artifact 的当前状态。

没有环境时状态写 `BLOCKED（所需条件）`，不能写 `PASS`，也不能因代码完整而删除该项。

## 9. Finding 严重度与关闭条件

| 等级 | 定义 | 处置 |
| --- | --- | --- |
| P0 | 可造成重复 Provider 副作用、不可恢复账务/权威数据损坏、双 Primary 确认、跨租户越权、凭据泄露，且无可靠绕过 | 阻断发布与 HA 启用；必须修复并独立复验 |
| P1 | 默认或常见条件下造成权限复活、持久不可用、错误迁移、主要协议破坏、告警静默或恢复流程不可执行 | 原则上阻断；若例外接受，必须有明确范围、运维绕过和责任人 |
| P2 | 非默认条件、可恢复错误、局部 UX/可观测性缺陷或测试门禁缺口 | 进入有验收条件的整改队列 |
| P3 | 文档、命名、低风险一致性或改进建议 | 可排期，不阻断 |

每条 finding 至少包含：基线 SHA、入口与前提、预期契约、实际结果、完整影响、代码位置、复现命令及退出码、现有防御、严重度理由、最小修复和关闭测试。推断必须标为推断。

关闭一条 finding 需要同时满足：根因修复、能在修复前失败的回归测试、受影响文档/API/指标同步、定向验证通过、独立复验通过。只改错误文案、只加重试或只让测试变绿不自动等于根因关闭。

## 10. Review 放行判据

### 10.1 仓库候选 Go

必须同时满足：

1. 经对抗验证的 P0 为 0；P1 为 0，或每项有书面接受、可靠绕过和明确关闭版本；
2. INV-01–INV-10 均有本轮有效证据，不以设计文本或历史通过替代；
3. X01–X09 均有明确裁决；
4. 最终 `make full-check`、bundle drift、whitespace 和 HA repository gates 通过；
5. 配置/metadata/protocol 版本变化均有 upgrade、拒绝和恢复证据；
6. Admin/API/CLI/metrics/alerts/runbook/compatibility manifest 与实际行为一致；
7. 所有 experimental、设计稿和外部阻塞均被如实标识。

### 10.2 HA 目标环境 Go

除仓库候选 Go 外，还必须完成 `docs/verification/ha-repository-gates.md` 的 target-environment gates 与 HA 设计 §1.3：kind、相邻版本、Linux 参考机、G0–G7、72 小时 soak 和 RTO。完成仓库 Review 不自动授权生产启用 HA。

### 10.3 No-Go 触发项

- 任一 CONFIRMED P0；
- 撤销/降权在提升后复活，或已确认 mutation 丢失；
- 上游结果不明确时发生自动重放；
- 配置迁移或 restore 可部分落盘并被当作成功；
- 模型/alias/credential/Project 信息跨权限边界泄露；
- 关键门禁缺失实际 oracle，或 CI 失败被直接当作基础设施噪声跳过；
- 把尚未执行的外部验收标为通过。

## 11. 建议投入与顺序

- **建议投入**：约 8–12 人日；3–4 个独立 Review track 并行时约 4–6 个工作日，不含 72 小时 soak、真实 Provider 授权等待与整改时间。
- **先后顺序**：S0 → S1 → T1/T2/T5/T6 高风险轨并行 → 其余 Track → S3 对抗验证 → S4 完整 gate 与报告。
- **优先级**：先验证 F11/F12 的权威与生命周期边界，再验证 F01/F05 的外部副作用边界，随后处理配置升级、安全授权、操作面和供应链。前端视觉或依赖维护不应抢占 P0/P1 的证伪资源。

最终报告不得只给一个分数。先给 Go/No-Go 与阻塞项，再给每项功能的 `PASS / PARTIAL / FAIL / BLOCKED / NOT APPLICABLE`、证据位置和下一步。
