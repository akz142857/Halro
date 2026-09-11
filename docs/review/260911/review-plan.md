# v0.8.0 发布评审方案（2026-09-11）

> 本文件是**评审与放行计划**，不是评审报告，也不构成发布授权。
> 执行结论应分别落在本目录的范围、发现、对抗裁决和进度文件中；最终可签字记录写入
> `docs/verification/assessments/v0.8.0.md`。
> 通用 v0.x 发布程序以
> [`docs/verification/release-assessment.md`](../../verification/release-assessment.md) 为准，
> 本方案只把它具体化到 `v0.7.1..main` 的 v0.8.0 候选范围。

## 1. 目标与结论边界

本轮目标是在**不发布、不打 tag**的前提下，对 `main` 上计划进入 v0.8.0 的完整范围做一次
跨 PR 的发布评审，修复阻断问题后，在同一个精确提交上完成发布彩排，并由发布责任人作出
GO / NO-GO 决定。

评审必须回答：

1. 新增的 Provider Offering、账号地域、连接组和订阅产品边界是否形成唯一事实源；
2. BigModel / Z.AI 通用 API 与 GLM Coding Plan 是否在凭据、路径、地域、模型和权益上隔离；
3. 失败请求捕获是否在提供诊断信息的同时守住密钥、正文、租户和留存边界；
4. Offering / Profile / 地域与标准化失败原因是否从路由一直正确归因到 Ledger、Usage、
   Parquet、failure capture、日志和控制台；
5. v0.7.1 的真实数据目录能否安全升级，旧版本回滚是否明确拒绝而不是静默误读；
6. 请求热路径、前端包体和依赖升级是否引入不可接受的性能、供应链或交付风险；
7. CHANGELOG、版本号、README、内嵌 bundle、许可证记录与发布工作流是否绑定同一候选 SHA。

以下行为不在本方案授权范围内：正式发布、创建 tag、触碰生产数据、使用真实客户凭据、
调用可能计费的真实 Provider、真实 KMS 或外部告警接收端。需要这些动作时必须另有明确授权。

## 2. 当前基线与候选范围

### 2.1 计划建立时的事实快照

| 项目 | 当前值 |
| --- | --- |
| 上一个发布 tag | `v0.7.1` |
| `v0.7.1` 指向的提交 | `84f2638e973f23935b9eda423143f65ff25a852c` |
| 当前 `main` | `222d08f84f61493fc9a273d351cc728528d6e30c` |
| 提交数 | `git rev-list --count v0.7.1..main` = **12** |
| 全范围规模 | **147 files changed, 12,862 insertions, 1,165 deletions** |
| 生产 Go（排除测试与内嵌 bundle） | **41 files, +3,110 / -277** |
| `web/src` | **19 files, +3,237 / -516** |
| 当前前端版本 | `0.7.1` |
| 当前 CHANGELOG | 尚无 `0.8.0` 段 |

执行 S0 时必须重新读取以上数值。若 `main` 已移动，以新的候选 SHA 重新生成范围表，不能让本文件
中的旧 SHA 冒充最终发布对象。

### 2.2 十二个提交按主题分组

| 提交 / PR | 主题 | 发布风险 |
| --- | --- | --- |
| `f31bc87` | 修复 Gateway Key scope 页面布局 | 前端回归、可访问性 |
| `8dc23dc` / #279 | 在失败诊断中捕获 Gateway 原始请求与规范化请求 | 敏感正文、留存、租户授权、内存与文件上界 |
| `459a572` / #280 | BigModel / Z.AI 地域化 Provider 与协议适配 | 新出网面、地域隔离、模型枚举、能力与错误映射 |
| `a1c0541` / #281 | Provider Offering、订阅访问与 GLM Coding Plan | 新领域模型、稳定 ID、使用条款确认、路由与账务归因 |
| `2bbd954`–`629b6b7` / #282–#287 | Python、Go、Node 官方 SDK 兼容性依赖升级 | SDK 行为、锁文件一致性 |
| `1c67e27` / #288 | Admin UI 运行与开发依赖升级，含 Vitest 5 | 测试工具重大版本、bundle 可复现性、许可证 |
| `222d08f` / #289 | AWS SDK、`x/crypto`、`x/sys` 升级 | KMS/凭据发现、加密与系统调用、供应链 |

### 2.3 主要改动面

| 面 | 重点入口 | 需要证明的事实 |
| --- | --- | --- |
| Offering 与地域 | `internal/domain/provider_offering.go`、`provider_table.go`、`provider_connection.go` | Offering 只描述产品，不偷偷决定路由；Surface、Region、Connection Group 的映射全量且唯一 |
| BigModel / Coding Plan | `internal/compatibility/bigmodel.go`、`internal/provider/openai/adapter.go`、`internal/modelcatalog/builtin.go` | 国内/海外、通用/Coding 四条产品边界不可串用，枚举与能力证据分离 |
| Provider 组装 | `internal/app/provider_adapters.go`、`admin_providers.go`、`admin_provider_profiles.go` | 新 Profile 经过 adapter、credential、manifest、catalog、Admin API 的全部注册闸 |
| 请求与失败语义 | `internal/gateway/`、`internal/requestmeta/`、`internal/provider/` | 原始请求仅在 opt-in 失败捕获中出现；错误分类不改变重试与保守结算语义 |
| 账务与归因 | `internal/budget/manager.go`、`internal/ledger/event.go`、`internal/usage/` | Offering / Profile / Region 在 reservation、attempt、settlement、query 中不丢失、不串户 |
| 持久格式 | `internal/usage/parquet.go`、failure capture JSON、Ledger event payload | Parquet schema **6 → 8** 可升级；bbolt schema 36 与 Ledger frame version 2 虽未变化，新增字段仍需旧数据验证 |
| 控制台 | `web/src/pages/ProvidersPage.tsx`、`UsagePage.tsx`、`UsageFailuresPanel.tsx` | 地域/产品选择、使用条款确认、失败详情与筛选和后端事实一致 |
| 依赖与交付 | `go.mod`、三个 SDK 兼容目录、`web/package-lock.json`、`internal/webui/dist` | 许可证、漏洞、锁文件、官方 SDK 与内嵌产物均可复现 |

本范围没有修改 `internal/store`、`auth`、`adminauth`、`redaction`、`contentscan`、
`safetransport`、`tokenguard`、`limiter`、`circuit`、`idempotency`、`backup` 或发布 workflow。
这只能用于收窄逐行审查，不能推出相关边界未受影响：新上游地址仍经过 SafeTransport，未脱敏的
Gateway 请求仍进入 failure capture，Provider 身份仍进入既有 Ledger / Usage / backup 数据链。

## 3. 发布深度检查触发判定

依据 `release-assessment.md` §0，对本轮逐行判定：

| 触发项 | 判定 | 本轮动作 |
| --- | --- | --- |
| Ledger / WAL / store schema | **触发恢复检查** | Ledger event payload 增加 Offering、Profile、Region 与失败语义字段；验证旧 WAL、重放与新旧 reader 行为。frame version 和 bbolt schema 未变也要由证据确认 |
| Provider wire / semantic mapping | **触发 Provider 专项** | BigModel 国内与海外通用 API、GLM Coding Plan、共享 OpenAI adapter、Anthropic 兼容面均纳入；真实账号 smoke 仍受计费授权限制 |
| 安全相关包 | **按数据流触发** | 包本身未改，但新增未脱敏 Gateway 请求捕获和区域 endpoint；执行正文、密钥、SSRF、授权、留存专项 |
| 请求热路径 / budget | **强制性能对比** | `gateway/service.go`、失败路径和 `budget/manager.go` 已改变；不能降为纯抽样 |
| `web/` | **触发完整前端门禁** | 页面逻辑、i18n、样式、Vitest 主版本和生成 bundle 均变化 |
| Durable format | **强制升级检查** | Parquet schema 6 → 8，failure capture 与 Ledger payload 扩展；用 populated v0.7.1 数据副本验证 |
| 依赖 / 构建 | **触发供应链检查** | Go、Web 与三套兼容性 SDK 锁文件均变化；执行许可证、漏洞、SBOM、签名与可复现构建检查 |

## 4. 判定基准与本轮不变量

每条“问题”必须给出入口、`文件:行号`、可达条件、违反的基准和最小复现。只看到可疑分支但
尚未证明可达的，标记为“候选 finding”，不得直接宣称漏洞或数据损坏。

| ID | 必须成立的命题 |
| --- | --- |
| INV-01 | 单写者 / 单数据目录边界不变；诊断、升级、备份和恢复不绕过目录锁 |
| INV-02 | Ledger 仍是账务权威；reservation 在出站前持久化，settlement 原子，未知上游结果保守结算且不自动退款 |
| INV-03 | Offering、Profile、Region 只是经验证的归因事实；不得因为 `Kind=subscription` 改变路由、鉴权、重试、定价或预算行为 |
| INV-04 | 每个 Profile 都解析到唯一 Surface、Offering 和合法 Region；一个连接组内不能混入不同产品、地域或凭据边界 |
| INV-05 | 通用 API 与 Coding Plan 的 host/path/key/model/权益不可交叉；错误 host 或错误产品必须在可解释的位置拒绝 |
| INV-06 | 使用条款确认绑定精确的 `OfferingID + Region + UsagePolicyRevision` 并进入完整审计；旧确认不能自动代表新 revision |
| INV-07 | 模型“存在性”来自上游枚举，“能力”来自声明或探测证据；手填 ID、空列表、刷新失败与未知模型不得获得虚构能力 |
| INV-08 | Gateway 原始请求只在显式启用的加密、审计、有界、按 Project 授权的 failure capture 中落盘；headers、Gateway Key 和 Provider credential 永不进入记录 |
| INV-09 | 原始请求、规范化请求和响应各自独立受大小上限与 TTL 约束；截断标志真实，取消/成功请求不留下不应存在的正文 |
| INV-10 | 标准化 `ProviderFailureReason` 不得把 definitive 变成 retryable、把 ambiguous 变成可回退，或改变已经选定的保守结算结果 |
| INV-11 | Offering / Profile / Region 与失败语义在 Ledger、Usage checkpoint、Parquet、failure capture、日志、Admin API 和 UI 间一致；旧记录缺字段时显示 unknown，而不是伪造默认值 |
| INV-12 | Parquet 6 → 8 的读取、升级、重建与旧 reader 拒绝行为明确；升级失败不污染原数据，backup/restore 保持认证与归因 |
| INV-13 | 新出网地址、重定向、代理和自定义 endpoint 仍受 SafeTransport 与既有 SSRF 边界约束，不因 RegionHost “识别表”被误当成安全 allowlist |
| INV-14 | 所有缓存、重试、并发、日志、capture 文件和筛选维度有界；新增低基数标签不能被用户或上游原文放大成高基数指标/日志 |
| INV-15 | Admin UI 的 Offering/Region 选择、确认门禁、编辑回显、失败筛选和错误提示与服务端一致；服务端是最终授权与确认权威 |

## 5. 评审组织

沿用 [`docs/review/README.md`](../README.md) 的“发现、证伪、裁决分离”方式。多人执行时角色间不
共享中间结论；只有一名执行者时按角色串行完成，并在报告中明确“未取得独立复核”，不得声称
多角色交叉证实。

| 角色 | 主要靶点 | 必须提交的证据 |
| --- | --- | --- |
| R1 架构与领域模型 | Offering、Surface、Region、Connection Group、稳定 ID、注册闸 | 全量映射表；一条新 Profile 从声明到可用的注册链；重复、缺失、跨组负向用例 |
| R2 核心逻辑与账务 | gateway → target → budget → Ledger → Usage/Parquet | 成功、发送前失败、已接受后失败、重试/回退、取消、重启的 attempt 与金额对照 |
| R3 安全与隐私 | failure capture、原始请求、Admin 读取权限、SSRF、日志 | 数据生命周期图；双 Project/角色矩阵；secret canary、TTL、截断、非法标识符和 endpoint 攻击用例 |
| R4 Provider 与 API 兼容性 | BigModel CN/Global、General/Coding、OpenAI/Anthropic facade、模型枚举 | Profile × endpoint × SDK × 地域矩阵；真实响应证据来源；decoder 与错误 envelope 对照 |
| R5 前端与可用性 | Provider 创建/编辑、usage warning、失败详情、筛选、i18n、窄屏 | 页面级测试与真实浏览器旅程；键盘/焦点/缩放证据；前后端 payload 对照 |
| R6 数据升级、可靠性与性能 | Parquet 6→8、旧 WAL/capture、备份恢复、请求热路径 | populated 升级档案；rollback refusal；同机 benchstat；资源与长稳采样 |
| R7 供应链与发布工程 | Go/npm/SDK 依赖、bundle、SBOM、签名、release dry run | 锁文件与许可证审查；漏洞结果；可复现产物；精确 SHA 彩排与归档定位 |
| R8 BUG 与测试盲区 | 跨上述边界找具体反例，并检查测试是否真的能看见改动 | 最小复现、现有防御、缺失断言、严重度与建议回归测试 |

建议投入 **5–8 人日**；3–4 名评审者可在约 2–3 个工作日完成 S0–S4，不含缺陷修复、真实
Provider 授权等待或正式发布。时间是估算，不是用来压缩证据的硬截止线。

## 6. 阶段、产物与退出条件

| 阶段 | 工作 | 产物 | 退出条件 |
| --- | --- | --- | --- |
| S0 范围冻结 | 记录 tag、候选 SHA、工作区、工具链、提交、文件、路由、Profile、格式版本和历史开放项 | `range-map.md` | 候选 SHA 唯一；每个改动面有负责人；工作区状态可解释 |
| S1 独立源码评审 | R1–R8 按范围审阅，不先共享结论 | `findings.md`、`roles/*.md` | 每个角色有结论、证据或明确阻塞；所有候选 finding 可复现或标记未验证 |
| S2 定向自动化 | 先跑能看见改动的包/页面/契约测试，再补必要 race、fuzz 与 SDK | `evidence/gate-results.json`、原始日志 | 高风险分支至少覆盖成功、拒绝、边界、取消/超时、并发、重启中适用的组合 |
| S3 实机与兼容性 | populated 升级/回滚/备份恢复、fake-provider 端到端、浏览器、性能与短稳 | `runtime-evidence.md`、`evidence/runtime/` | 每项实验有 SHA、命令、退出码、环境和限制；不以 fixture 冒充真实 Provider |
| S4 对抗裁决 | 对 P0/P1、账务、授权、持久化和契约争议由非原作者从入口证伪 | `adversarial-verdicts.md` | 每项裁决为 CONFIRMED / REFUTED / PARTIAL / UNVERIFIED，并重新定级 |
| S5 整改复验 | 修复已确认阻断项，增加回归，按影响面复验 | `progress.md` | P0/P1 清零；P2 有 owner、期限和风险处置；最终全量门禁一次通过 |
| S6 放行与彩排 | 形成 release commit、owner 条件式签字、精确 SHA dry run | `docs/verification/assessments/v0.8.0.md` | 候选 SHA 的 CI 与 dry run 全绿；`main` 未移动；owner 明确 GO |

本轮方案文件之外，建议最终目录结构：

```text
docs/review/260911/
  review-plan.md
  range-map.md
  findings.md
  adversarial-verdicts.md
  runtime-evidence.md
  progress.md
  completion-audit.md
  roles/
  evidence/
docs/verification/assessments/v0.8.0.md
docs/verification/performance/<date>-v0.8.0/
docs/verification/upgrades/<date>-v0.8.0/
```

## 7. 专项检查清单

### 7.1 Offering、地域与订阅产品

- 从 `AllProviderProfiles` 反向遍历，证明每行都有唯一的 Surface、Offering、Region scope、
  connection group、adapter builder、primitive binding、manifest 和前端 catalog 表示。
- 对 CN / Global、General / Coding 做四象限负向测试：错误 key、错误 host、错误 path、错误模型、
  错误确认 revision 均不得悄悄落到另一产品。
- 验证 `ProviderOfferingKind` 没有进入路由、鉴权、预算或定价分支；真正的行为仍由精确 Profile、
  Surface、credential scheme 和 primitive 决定。
- `RegionHost` 是识别表而不是 allowlist；企业代理和私有入口保持可用，但 Region 不能因此伪装成
  已知值。固定地域的 endpoint mismatch 必须在 `doctor` 与 Admin UI 可行动地呈现。
- 使用条款确认必须服务端校验，审计记录携带稳定 revision；前端复选框不能成为唯一防线。
- Kimi Code 与 MiniMax subscription/entitlement 虽非本轮新适配，也被统一 Offering 表重新分类；
  对它们做回归，避免“修 GLM、坏已有订阅产品”。

### 7.2 Provider、模型枚举与能力

- 先读取已保存的真实上游响应/官方 OpenAPI，再审 decoder；不得从自制 fixture 推导上游事实。
- `/models` 回答“谁存在”，catalog 回答“会什么”。分别验证成功空列表、鉴权失败、超时、畸形
  envelope、旧缓存保留和未知 ID；禁止以名称前缀猜能力。
- 核对 BigModel 国内/海外 Chat、Embeddings 与 Anthropic facade 的路径、Bearer/header、请求字段、
  SSE 终止、usage、thinking/reasoning、错误 envelope 与 provider request ID。
- 检查共享 OpenAI adapter 的 BigModel 分支没有把 BigModel-only 字段或错误规则泄漏给 OpenAI、
  Azure、DeepSeek、Kimi、MiniMax 等既有 Profile。
- 官方 Go / Node / Python SDK 的受控兼容服务全部通过；明确它只证明 Halro facade，不证明真实
  Provider 账号、模型、余额或区域可用。

### 7.3 失败捕获、安全与授权

- 建立 `GatewayRequest → semantic request → provider request/response → encrypted capture → Admin read →
  purge` 数据生命周期，标注未脱敏、已脱敏、加密、审计和授权边界。
- 用带 Authorization、API key-like 字符串、工具参数、图像 URL、超长 UTF-8、多 choice 与嵌套 JSON
  的输入验证：headers 不入库，各字段独立截断，输出仍为合法可解释 JSON 或明确标记截断。
- 验证 capture disabled、成功、caller abandoned、发送前拒绝、上游失败、已接受后畸形响应、TTL
  临界点、清扫失败和重启路径。
- 以两个 Project、administrator、read_only、无会话、过期会话测试正文读取和筛选；若 read_only
  读取正文仍是产品契约，必须在评估记录中明确接受，而不是把它隐含为“安全默认”。
- 检查日志、错误、指标、审计、Usage 和 Parquet 只记录有界标识符与标准化枚举，不写上游原文、
  prompt、response body、Provider key、Gateway key 或原始 IP。
- 针对自定义 endpoint、重定向、DNS rebinding、IPv4/IPv6、本机和云元数据地址复读 SafeTransport
  边界；测试使用本地可控目标，不探测真实内网。

### 7.4 账务、错误语义与持久化

- 对每种终态核对 `Retryable`、`Ambiguous`、`ProviderFailureReason`、route fallback、reservation 与
  settlement：标准化展示字段不得改变执行事实。
- 针对订阅额度耗尽、订阅失效、无效凭据、429、403、Kimi Code 402 和未知错误，验证 canonical
  reason、error class、HTTP 响应、日志、capture 与 Usage 行一致。
- 验证 Offering / Profile / AccountRegion 快照在请求准入时冻结；配置修改、重试换 target 或重启后，
  历史事件不得按当前配置重新解释。
- 从 Ledger 重建 Usage / Parquet，逐行比较新增字段与总金额；旧记录缺新字段时保持空/unknown，不能
  归到默认 Offering 或 Region。
- 测试非法 Offering/Profile 组合、Region 不属于 Surface、超长 provider identifier、损坏 capture
  和篡改 Parquet；所有路径 fail closed 且不污染已认证前缀。

### 7.5 前端与真实用户旅程

至少走完：登录 → 创建 BigModel Provider → 选择 Region 与 Offering → 阅读并确认条款 → 创建
Deployment → Project / Gateway Key → fake-provider 调用 → Usage / failure 筛选 → 打开 failure detail →
编辑 Provider → 禁用 Key 后再次调用。

同时检查：

- CN/Global 与 General/Coding 的选项文本、默认 endpoint、确认状态和编辑回显不互串；
- confirmation pending 时不能通过折叠、Escape、返回或重复提交绕过；服务端错误能恢复；
- query-only 导航、浏览器前进后退、分页、空态、慢请求、401/403 和过期会话保持正确；
- 中英文键完整，价格/额度/订阅措辞不把未知费用显示为零；
- 键盘、焦点、标签、对比度、200% 缩放和窄屏布局有真实浏览器证据；
- `styles.css` 与 design-system 测试通过，未引入仅服务单页的不可复用视觉规则。

### 7.6 依赖、构建与发布供应链

- 复核 #282–#289 的实际锁文件 diff、许可证、NOTICE、运行/开发作用域和新增/移除模块；
  `docs/verification/dependency-license-review.md` 的哈希必须与候选一致。
- 对 AWS SDK / KMS 重点检查 credential chain、IMDS/ECS、endpoint、重试、错误分类和 File mode
  “不初始化云 SDK”边界；对 `x/crypto` / `x/sys` 检查受影响调用点。
- Vitest 5 使用 CI 支持的 Node 版本运行；本地不受支持的 Node 结果不能替代 CI，也不能被误报为
  产品失败。
- 从干净 `npm ci` 重建前端并验证 `internal/webui/dist` 零漂移；不手工编辑或合并生成 bundle。
- release dry run 必须产出 archives、容器、SBOM、checksum、Sigstore bundle、provenance 和签名的
  run-evidence manifest；按 `release-run-evidence.md` 归档并验证。

## 8. 验证执行顺序

以下命令是执行计划，不表示本文件建立时已经运行。执行者应记录真实工具版本、退出码、耗时和
日志路径；缓存的 `ok`、旧 PR 的绿色检查和通过管道读取的退出码都不能冒充当前候选证据。

### 8.1 冻结与定向测试

```bash
git fetch --tags origin
git rev-parse 'v0.7.1^{}'
git rev-parse main
git status --short
git log --oneline v0.7.1..main
git diff --stat v0.7.1..main

go test -count=1 ./internal/domain/ ./internal/compatibility/ ./internal/provider/...
go test -count=1 ./internal/gateway/ ./internal/budget/ ./internal/failurecapture/
go test -count=1 ./internal/usage/ ./internal/ledger/ ./internal/app/
go test -race -count=1 ./internal/gateway/ ./internal/budget/ ./internal/failurecapture/ ./internal/usage/
```

先按失败归属进一步收窄到具体测试；只有并发、goroutine、共享状态或生命周期受影响的包才跑
`-race`。真实通过证据一律使用 `-count=1`。

### 8.2 最终本地门禁（候选 push 前只跑一次）

```bash
gofmt -l ./cmd ./internal ./tools
go test -count=1 ./...
go vet ./...
sh scripts/check-dependency-license-review.sh

cd web
npm ci
npm run typecheck
npm test
npm run build
cd ..
git diff --exit-code -- internal/webui/dist
git diff --check
```

还要运行当前 CI / release workflow 实际使用的 `govulncheck`、npm audit、fuzz、observability、
container、SBOM、secret scan、SDK compatibility 与 SSE stress 命令。优先引用 workflow 的精确命令，
不要在计划中另造一个与 CI 不同的“等价门禁”。修复后只重跑能受该修复影响的定向测试；最终
候选变更后再执行一次完整门禁，后续纯 Markdown 记录不重复浪费整套测试。

### 8.3 Upgrade / rollback / backup 实验

所有实验使用临时目录和合成数据，绝不直接打开 live `data/`：

1. 用 v0.7.1 二进制建立 populated 数据：Provider/Deployment/Project、成功与失败 attempt、
   非空 Ledger、Usage/Parquet、至少一条 failure capture 和 backup；记录文件哈希。
2. 复制数据目录，以 v0.8.0 候选先执行只读检查，再首次启动；记录 schema、Parquet manifest、
   Ledger sequence、Usage 行数、归因字段、doctor 与 verify 结果。
3. 验证 Parquet 6 → 8 后旧行保持可读且新字段为空；新行携带正确 Offering/Profile/Region 和失败
   语义；从 Ledger 重建的结果逐行相等。
4. 用 v0.7.1 二进制尝试读取已被候选写过的副本。允许兼容读取或明确 fail-closed；禁止启动成功
   后静默忽略 schema 7/8 字段。根据结果写清 rollback 是否必须恢复升级前 backup。
5. 对候选 backup 执行 verify、错误 key/截断/错误 master key 的拒绝测试，再恢复到全新 scratch
   目录并启动；核对 Ledger、Usage、capture、Provider 配置与旧 Admin session 行为。
6. 在关键写入后 `SIGKILL`，重启并验证已确认 attempt 不丢失、不重复，capture 临时文件不形成
   越权可读孤儿。物理断电、真实云盘和真实 KMS 若未执行必须列为边界。

最终评估必须明确 `Re-initialization required: yes/no` 和 rollback 步骤。

### 8.4 性能、容量与包体

- 在同一主机、同一 Go 工具链、同一 `GOMAXPROCS` 和 fixture 上同时测 v0.7.1 与候选；归档原始
  样本、源码 SHA、命令和 `benchstat`。
- 必测 route resolution、普通/失败请求、budget admission/settlement、Ledger append/replay、
  Usage/Parquet rebuild；新增 failure capture 分别测 disabled、enabled 小正文和截断正文。
- 时间回归超过 10%，或原本零分配路径新增分配，必须解释、修复或由 owner 书面接受；不能只看
  平均值，记录 p50/p95/p99、allocs、RSS、goroutine、FD 和 capture 文件增长。
- 前端 gzip 包体与 v0.7.1 在同一构建环境比较；无法解释的增长超过 10% 进入 finding。
- 先做 30 分钟受控 smoke；若资源持续增长或关键路径变化明显，再扩到 2–4 小时。短跑不能宣称
  24 小时稳定性或生产 SLO。

### 8.5 真实 Provider 证据

BigModel CN/Global 与 GLM Coding Plan 是新增用户可见能力，理想最低矩阵为每个已启用 Profile 的
模型枚举和一次最小请求，并验证地域错误、产品错误与额度错误。执行前必须有用户对真实账号、
模型、地域和费用上限的明确授权。

若未获授权：

- 复核已有无凭据探测、官方 OpenAPI、保存的真实响应与默认 skip smoke；
- 在 assessment 中明确写“未进行真实账号/真实模型端到端验收”；
- 由发布责任人书面接受该证据缺口后才可给带条件 GO；不得把本地 fixture 或 SDK harness 写成
  真实 Provider 验收。

## 9. Finding、严重度与叫停规则

| 级别 | 标准 | 发布处置 |
| --- | --- | --- |
| P0 | 可达的凭据/正文泄漏、严重越权、不可恢复账务或数据破坏、广泛不可用 | 立即 NO-GO，修复并由独立评审者复验 |
| P1 | 错产品/地域路由、预算或结算错误、静默持久化误读、确认门禁绕过、可显著放大的资源问题 | 默认 NO-GO；不能仅建 issue 后发布 |
| P2 | 受限条件功能缺陷、可恢复偏差、明显性能/运维/可访问性问题 | 指定 owner、期限、缓解；owner 决定是否接受 |
| P3 | 低影响体验、文档或维护性问题 | 记录排期，不混入阻断统计 |

任一条件满足即停止发布流程：

- 仍有 CONFIRMED / PARTIAL 的 P0 或 P1；
- 账务不守恒、未知结果被退款、响应已可见后仍切换 Provider；
- Gateway 原始请求、密钥或上游正文进入日志/指标/未授权响应；
- General / Coding 或 CN / Global 可被错误组合并实际出站；
- 使用条款确认可绕过、revision 不可追溯，或 UI 与服务端判定不同；
- Parquet / Ledger / capture 的旧数据被静默误读、升级污染原目录或 rollback 行为不明确；
- 模型刷新失败清空有效目录，或手填模型被虚构为拥有能力；
- 任一要求的 CI、bundle drift、许可证、漏洞、SDK、SBOM、签名或 dry run 门禁未绿；
- 候选 SHA 在 dry run 后发生变化。

真实 Provider、KMS、生产告警或长稳缺环境时，不把它们伪装成通过。能否带限制发布由 owner 在
assessment 中逐项接受；涉及 P0/P1 类风险的缺口不得仅靠“未验证”绕过。

## 10. Release commit 与精确 SHA 放行顺序

S0–S5 完成且无阻断项后，建立一个 `chore(release): v0.8.0` 提交，至少包含：

- [ ] `CHANGELOG.md` 新增 `## [0.8.0] - <日期>`，按 Added / Changed / Fixed / Operator impact
      写用户可见变化；明确 Provider、订阅使用边界、failure capture 隐私与升级/回滚影响；
- [ ] `web/package.json` 与 `web/package-lock.json` 的版本从 `0.7.1` 改为 `0.8.0`；
- [ ] README 和文档中的稳定镜像/下载示例更新为 `v0.8.0`；
- [ ] 重新构建并提交 `internal/webui/dist`，确认源码与 bundle 零漂移；
- [ ] 更新 `docs/verification/dependency-license-review.md` 的 web 输入哈希；
- [ ] 写入 `docs/verification/assessments/v0.8.0.md`，逐项回答本方案不变量、finding、升级、
      性能、真实 Provider 边界及 `Re-initialization required`；
- [ ] `git status` 干净，`main` CI 全绿，无未处理 release-blocking issue。

随后严格按以下顺序执行：

1. 推送 release commit，记录唯一候选 SHA，并冻结 `main`；
2. 从 `main` 手动运行 `release` workflow，输入 `v0.8.0`、`dry_run: true`；
3. 验证彩排在该 SHA 上完成 quality、SDK、stress、web、binaries、container、provenance 全图，
   下载并验证 run evidence；
4. 确认 `main` 仍指向同一 SHA。若移动，重新评审增量并重跑 dry run；
5. owner 在 assessment 已给出条件式 GO，且 dry run 条件已满足后，才运行同一 workflow，输入
   `v0.8.0`、`dry_run: false`；
6. 不手工预建 tag。workflow 在所有 gate 通过后创建 tag、GitHub Release、容器 manifest 并触发
   Homebrew / APT 下游；
7. 发布后验证 release/tag/容器/包管理渠道均绑定候选 SHA，归档 run ID、attempt、commit、tag、
   校验和和受限存储位置。为避免改变已发布 SHA，运行链接和归档位置可写入后续 post-release
   evidence 记录。

## 11. 完成定义

只有同时满足以下条件，评审才可标记完成：

- 范围覆盖 `v0.7.1..候选 SHA` 的整体 diff，而非只汇总各 PR 结果；
- R1–R8 均有证据化结论或明确外部阻塞；
- 所有 P0/P1 完成独立对抗裁决并关闭；P2/P3 有清晰处置；
- 定向测试、最终完整 Go/前端门禁、SDK、fuzz、stress、供应链与 bundle drift 全绿；
- populated v0.7.1 升级、rollback 行为、backup/restore 与 candidate restart 有归档证据；
- 性能与前端包体完成同机比较，异常已解释；
- 真实 Provider 验收已完成，或 owner 明确接受未执行的费用与真实性边界；
- `docs/verification/assessments/v0.8.0.md` 给出具名、带日期的 GO / NO-GO；
- GO 只对通过 exact-SHA dry run 的提交有效，任何后续代码变化都会使其失效。

本方案完成不等于 v0.8.0 已发布。正式发布只有在 S6 条件满足并由发布责任人执行工作流后才发生。
