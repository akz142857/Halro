# v0.5.0 发布评审方案（2026-09-01）

> 本文件是**方案**，不是报告。执行后的结论落在同目录 `260901.md`，对抗验证落在
> `adversarial-verdicts.md`，整改进度落在 `progress.md`，可签字的发布记录落在
> [`docs/verification/assessments/v0.5.0.md`](../../verification/assessments/v0.5.0.md)。
> 角色定义与选择指南见 [`docs/review/README.md`](../README.md)；发布时刻的标准动作见
> [`docs/verification/release-assessment.md`](../../verification/release-assessment.md)；
> 上一轮同类方案见 [`260827/review-plan.md`](../260827/review-plan.md)。
> 本文件只做"这一轮该怎么跑"的具体化，不重复定义角色，也不重写发布流程。

## 1. 本轮的性质：v0.4.0 之后的第一次发布评审

**目标版本是 v0.5.0，不是 v0.4.0。** 这一条是本轮开工前先量过的事实，写在最前面是因为
它改变了整份方案的对象：

| 事实 | 证据 |
|---|---|
| v0.4.0 已发布 | tag `v0.4.0` → `2501668`（2026-08-28 11:32 +0800），已推到 origin；GitHub Release "Halro v0.4.0" 状态 Latest（2026-08-28T04:53:39Z） |
| CHANGELOG 已固化 v0.4.0 | `CHANGELOG.md` 有 `## [0.4.0] - 2026-08-28` |
| 之后又落了 10 个提交 | `git rev-list --count v0.4.0..main` = 10（#241…#250） |
| 这 10 个提交尚未进入 CHANGELOG | `## [Unreleased]` 段为空——**本轮必须补，否则 release workflow 直接拒绝**（`release.yml` §prepare：`CHANGELOG.md has no '## [0.5.0]' section`） |
| 版本号未 bump | `web/package.json` 仍是 `0.4.0` |

范围包含一个**新 Provider（MiniMax，三个 Profile 行）**、一个**新 durable 结构（bbolt
schema 33 + 日粒度 rollup）**、一个**新 Admin 端点族（`/admin/api/v1/usage/summary`）**。
按语义化版本，这是 minor 而不是 patch。

同时，它触发了 `release-assessment.md` §0 表里**六行中的四行**（判定见 §3），并且改动了
会随发布固化的对外构造（Provider Profile 表、端点兼容清单、bbolt 桶、Admin API 响应
形状）。所以在标准评估流程之外，另加多角色并行评审与对抗验证。

## 2. 范围与已量得的事实

```
基线 v0.4.0 = 2501668 (2026-08-28 11:32:19 +0800)
HEAD        = cd37927
git diff --stat v0.4.0..main  →  198 files changed, 15862 insertions(+), 2168 deletions(-)
生产 Go（不含 *_test.go、不含 internal/webui/dist）
                              →   47 files changed,  3767 insertions(+),  457 deletions(-)
```

十个提交（squash 后）：

| 提交 | 主题 |
|---|---|
| `c1af5c3` #241 | Provider 行布局；多目标别名保证什么（文档） |
| `00e28eb` #242 | 修 Mantle workspace 头与写路径上限 |
| `9dedb90` #243 | 让 Mantle 冒烟能证明 project 头确实发出 |
| `ba1d581` #244 | 撤回 D6.2 的诊断，刷新漂移的行号引用 |
| `f413dbd` #245 | failover S0：拒绝重复目标、报告 withheld 路由、路由列表分组 |
| `bd9a5a5` #246 | 用量归因到具体 Deployment |
| `fc169fa` #247 | 按日/月/年的用量汇总 |
| `5fe435a` #248 | i18n：补中文操作者真正会读到的缺口 |
| `4d624cb` #249 | 接入 MiniMax，并给它经过的注册点上闸 |
| `cd37927` #250 | 用真账号实测 MiniMax 适配，并修实测证伪掉的部分 |

改动面按包（生产代码）：

| 面 | 主要文件 | 性质 |
|---|---|---|
| 用量 rollup（新 durable 结构） | `internal/domain/usage_rollup.go`（新 348 行）、`internal/usage/rollup.go`（新 265 行）、`internal/store/bolt/store_usage.go`（+234）、`store.go` schema 32→33 | 新桶 `usage_daily_rollup`、新元数据键 `usage_rollup_state`、`RollupVersion = 1`；`PutUsageCheckpoint` 签名改为四参并在同一事务内落 rollup 增量；`DeleteUsageCheckpoint` 就地改名为 `ResetUsageDerivatives` |
| 用量汇总 API | `internal/app/admin_usage_summary.go`（新 642 行）、`internal/app/admin_usage.go`（+94）、`internal/usage/query.go`、`aggregate.go` | 新端点族 `/admin/api/v1/usage/summary`，支持 `granularity`/`group_by`/`sort`/`order`/`project_id`/`provider_id` |
| MiniMax 接入 | `internal/domain/provider_table.go`（+161）、`internal/provider/openai/minimax.go`（新）、`internal/compatibility/minimax.go`（新 214）、`internal/modelcatalog/builtin.go`（+75）、`internal/provider/model_catalog.go`（+73） | 三个 Profile 行（Anthropic Messages / Chat / Responses），同一 Surface 与凭据方案；`BaseURLTemplate = https://api.minimax.io`，另有 `api.minimaxi.com` 用于中国大陆账号 |
| 平台注册点合并 | `internal/provider/profile.go`（−150 净）、`profile_bindings.go`（新 191）、`internal/app/provider_adapters.go`（新 286）、`providers.go`（±157） | 注册点从多处收敛为绑定表——**新增 Provider 的门在这里，评审重点不是 MiniMax 本身而是这道门** |
| 兼容清单与端点损失 | `internal/compatibility/manifest.go`（+70）、`provider_fields.go`（新 70）、`anthropic/native.go`（+63）、`docs/compatibility/endpoint-manifests.json` | 端点级不支持字段从 14 个 profile 行里提出，声明一次；双向精确的覆盖校验 |
| Provider adapter | `internal/provider/anthropic/adapter.go`（+139）、`openai/adapter.go`（±115）、`bedrock_project.go`（+33） | Mantle workspace 头修正；Anthropic 面接入 MiniMax |
| 记账热路径 | `internal/gateway/service.go`（+1 行） | 仅 `isNativeAnthropicProfile` 增加 MiniMax 一项 |
| Ledger 观测 | `internal/ledger/log.go`（+34） | 新增 `batchNanos` 与 `AppendStats.BatchDuration`/`MaxBatch`；**未改帧布局** |
| 前端 | `web/src/pages/*`（20 文件）、`trend.ts`、`components.tsx`、`i18n/locales/*` | 新 `UsageSummaryPanel`、时区字段、路由分组、i18n 补全 |

**未触及**（本轮据此收窄，不作全面复审；依据是 `git diff --name-only v0.4.0..main -- internal/<pkg>` 为空）：
`auth`、`adminauth`、`safetransport`、`budget`、`limiter`、`tokenguard`、`audit`、
`contentscan`、`sse`、`circuit`、`idempotency`、`redaction`、`semantic` 全部零改动。
执行时先自行复核一遍再采信——这条事实是本轮所有"不查"决定的唯一依据。

**注意一个不对称**：`safetransport` 包零改动，但本轮**新增了两个上游主机**
（`api.minimax.io`、`api.minimaxi.com`）。"包没改"不等于"出网面没变"，这条要按 A3 角色查。

## 3. `release-assessment.md` §0 触发表逐行判定

| 触发行 | 本轮 | 依据 |
|---|---|---|
| `internal/ledger` / WAL 帧 / `internal/store` schema → 恢复通道（§1c） | **触发** | schema 32→33，迁移 33 `usage_daily_rollup`；`ledger/log.go` 有改动但只加统计字段，帧布局未变，故 10 GiB 回放界限不需重测 |
| `internal/provider/*` 线上行为 / `semantic` 映射 → 受影响 Provider 真账号冒烟 | **触发** | MiniMax（全新）、Anthropic（adapter +139）、Bedrock Mantle（workspace 头修正）、OpenAI（adapter ±115）。#250 已对 MiniMax 做过一次真账号实测，本轮**复核那次实测的证据，而不是重新计费重跑** |
| `auth`/`adminauth`/`redaction`/`contentscan`/`safetransport` → 针对性安全复读 | **不触发（包层面）**，但按 A3 单独查新出网主机与新凭据方案 | 五个包 `git diff` 均为空 |
| 请求热路径 / `budget` / `limiter` / `tokenguard` → 基准对比**强制** | **不触发** | `gateway/service.go` 仅 +1 行谓词；`budget`/`limiter`/`tokenguard` 零改动。基准对比降为**抽样**：只跑 Ledger append 与路由解析两组，理由是 rollup 增量与 checkpoint 同事务写入，落在写放大而非请求路径上 |
| `web/` only | 不适用 | 后端同时大改 |
| 任何 durable 格式 → 原地升级检查**强制**（§1d） | **触发** | bbolt schema 33、`usage_rollup_state` 结构版本、`PutUsageCheckpoint` 契约变更 |

四行触发。

## 4. 判定基准

一条发现要成立，必须指认它违反下列某一条基准，并给出 `文件:行号`。没有基准支撑的写成
"疑问"而不是"问题"。B1–B7 来自 `CLAUDE.md` 不变式与 `release-assessment.md` §3，
B8–B12 是本次范围特有的。

| 编号 | 基准 | 来源 |
|---|---|---|
| B1 | 预算预留在 Provider 请求发出**之前**持久化；结算原子；语义不明的上游结果保守记账，绝不静默退款 | `CLAUDE.md`、ADR 0018 |
| B2 | fail-closed：损坏/不可用/语义不明/陈旧状态一律拒绝而非降级 | `CLAUDE.md` |
| B3 | 密钥、提示词、响应体、上游模型标识不进日志/错误/指标/审计 | `CLAUDE.md`、威胁模型 |
| B4 | 能力过滤发生在 Provider I/O 之前；不支持的字段**拒绝**而非静默丢弃 | `CLAUDE.md`、`docs/contracts/provider-capabilities.md` |
| B5 | 重试/回退有界，且响应字节对客户端可见之后不再切换 Provider | `CLAUDE.md` |
| B6 | 前 1.0.0 就地修复：错误构造不得与替代品并存；durable 格式改动必须 bump 版本使陈旧状态被拒而非误读 | `CLAUDE.md` |
| B7 | 单写者、单数据目录；WAL 是记账权威，bbolt/Parquet 是派生物 | `CLAUDE.md` |
| B8 | **rollup 是派生物，不是第二个账本。** 汇总端点的任何数字都必须能从 WAL 重建出同一个值；增量路径与全量重建路径对同一段 WAL 必须给出**逐行相等**的结果；checkpoint 与 rollup 状态只能描述 WAL 的同一个前缀，二者不一致时清空重建而不是"补跑一段" | 本次范围、`store_usage.go` 自述 |
| B9 | **有界性不得以静默丢账为代价。** 每个维度的键数有上限（`MaxRollupKeysPerDimension`），超出折进 `RollupOtherKey`；折叠后该维度各行相加仍须等于当天总计，且折叠决策与增量批次划分无关 | 本次范围 |
| B10 | **上游能有的清单就问上游要。** 新 Provider 的模型清单，凡上游serve了枚举路由的，就走那条路由；内置目录只供能力证据，不代替枚举 | `AGENTS.md`「An adapter's silence is not the upstream's answer」 |
| B11 | **能力上限只能收窄，不能靠"没写"放宽。** 新 Profile 行的 `Ceiling` 必须由文档或实测证据支撑；`Defaults == Ceiling` 意味着 Deployment 单方面打不开任何东西，这一条要在写路径上真的成立 | `CLAUDE.md`、`provider_table.go` 自述 |
| B12 | **注册点合并后，新增 Provider 必须无法漏过任何一道闸。** #249 的主题就是"给它经过的注册点上闸"——本轮反过来查：是否存在一条注册路径能造出一个不带能力证据/不带兼容声明/不在端点清单里的 Provider | 本次范围 |

## 5. 阶段 0：范围事实底座（先做，无结论）

**目的**：在判断之前把范围的真实形状抄下来，避免用自己脑子里的模型去测自己脑子里的模型
（`CLAUDE.md`「Verify, never assume」）。

**交付物**：`range-map.md`，含五张表：

1. **用量派生物版本表**：schema 33、`RollupVersion = 1`、usage checkpoint payload 版本
   三者各自的写者、读者、拒绝条件；三者不一致时各自会发生什么；启动时"清空重建"的
   触发条件与实际代码行。
2. **rollup 键空间表**：`RollupKey` 的编码、分隔符、维度清单、每个维度的键来源、
   `MaxRollupKeysPerDimension` 的值与折叠规则；时区版本（`TimezoneVersion`）参与键的
   哪一段，改时区后旧行如何处置。
3. **汇总端点契约表**：`/admin/api/v1/usage/summary` 的全部查询参数、合法值域、非法值
   的响应码、分页/排序默认值；与既有 `/admin/api/v1/usage` 的关系（谁是谁的派生）。
4. **MiniMax Profile 表**：三行的 Access Surface、凭据方案、BaseURL 与备用主机、
   `Defaults`/`Ceiling` 逐能力对照、模型枚举走哪条路由、内置目录供的是什么；
   以及 #250 实测证伪掉了哪几条、现在的值由什么证据支撑。
5. **注册点表**：新增一个 Provider 需要落地的全部注册点（Profile 表、adapter 绑定、
   兼容声明、端点清单、目录、前端 golden fixture、i18n），以及每个注册点上"漏登记就
   失败"的那道闸具体是哪个测试/哪行代码。

阶段 0 只抄事实，不下判断。它同时是发布记录 §3 不变式逐条回答的原材料。

## 6. 阶段 1：多角色并行评审（后端）

沿用 `docs/review/README.md` 的角色框架，按本次范围选角色，**每个角色独立通读、互不知情**。
共七个角色，其中三个是发布专项档。

| # | 角色 | 本轮的具体靶子 |
|---|---|---|
| A1 | 核心逻辑（记账派生物） | B7/B8/B9：`usage/rollup.go` + `store_usage.go` + `domain/usage_rollup.go`。增量与全量重建的等价性、checkpoint 与 rollup 状态的同事务性、崩溃在两次写之间会留下什么、`Add` 的溢出与负值、跨账期（23:59 admit / 00:02 settle）归属、时区版本变更后的旧行 |
| A2 | 核心逻辑（汇总查询） | `admin_usage_summary.go` 642 行：day/month/year 三种粒度的边界（月末、闰年、DST）、`group_by` 与 `sort` 的组合是否都被穷举校验、非法组合是拒绝还是静默回落（B2/B4）、大范围查询的内存与遍历上界、只读角色可见性 |
| A3 | 安全（新出网面与新凭据） | MiniMax 两个主机是否都在允许清单内且都走 SafeTransport（`safetransport` 包零改动 ≠ 出网面未变）；`CredentialBearerStatic` 在错误路径/日志/审计里是否可能外泄（B3）；`api.minimaxi.com` 这条备用主机由谁配置、能否被 Deployment 单方面指向任意主机；汇总端点是否泄漏跨 Project 数据 |
| A4 | 数据迁移与升级 | 迁移 33 的平滑度与回滚：只建桶看似无风险，但**旧 checkpoint 存在而 rollup 状态不存在**才是真实的升级形态——查这条路径是否走到"清空重建"而不是"带着空 rollup 继续增量"；`ResetUsageDerivatives` 是否覆盖全部派生物；**"要不要重新初始化数据目录"必须由实测回答**（阶段 3） |
| A5 | BUG 排查 | 全范围找有代码证据的具体缺陷，高危区优先：`applyUsageRollupDelta` 的 cursor 前缀扫描与键内 `/`、`storedDimensionKeys` 每次增量的 O(键数) 重扫、折叠后 `FirstSequence` 的语义、`profile_bindings.go` 的绑定查找是否有未命中静默返回零值、`manifest.go` 双向覆盖校验的反向是否真会失败 |
| A6 | API 兼容性契约 | `docs/compatibility/endpoint-manifests.json` 与实现一致性；#250 声称"published contract's meaning is unchanged"——**复核这句**（比对 v0.4.0 与 HEAD 的覆盖集合）；新增 Admin 端点族的响应形状是否属于对外承诺；MiniMax 三行进入服务矩阵对既有客户端有无可见影响 |
| A7 | 架构（注册点合并） | B12：`profile.go` 净减 150 行 / `profile_bindings.go` 新增 191 行 / `provider_adapters.go` 新增 286 行是不是一次真正的收敛，还是把分散换成了两层间接；新增 Provider 的门是否**穷举式**（遍历 Profile 表断言每行都有绑定），还是逐个登记式（漏了就静默） |

**不派的角色与理由**（写下来，是为了让读者知道它被判过而不是被忘了）：

- 安全威胁建模（完整）——`auth`/`adminauth`/`safetransport`/`redaction`/`contentscan` 本轮
  零改动，完整威胁建模的性价比低于把力气集中在 A3 的两条新面上。
- 性能与容量——热路径未动（§3 第四行未触发），降为抽样基准，见阶段 3。
- 可用性/文档——并入阶段 2 与阶段 5（发布动作清单本身就是可用性检查）。

## 7. 阶段 2：前端评审

一个角色，覆盖 20 个页面文件与 i18n：

- **交互**：`UsageSummaryPanel` 的粒度切换、分组、排序是否与后端参数一一对应；大范围
  查询的等待与空态；时区字段（`components.timezonefield`）的回落行为；路由分组后
  危险操作（删除路由）的防误触是否仍成立。
- **视觉**：新面板是否复用 design-system 而非自造样式（`styles.css` 有改动，查是否引入
  一次性类）；`TrendChart` 时区改动后的坐标轴与空数据表现。
- **i18n**：#248 声称补齐了中文操作者会读到的缺口——复核新增的汇总/MiniMax/failover
  三块文案是否两语齐全（`i18n.test.tsx` 有门禁，查门禁是否覆盖新键）。
- **前端不变式**：CSRF 与密钥仍只在内存；产品包 secret canary 扫描通过。

## 8. 阶段 3：实机验证（不可省，替代不了）

按 `release-assessment.md` §1c/§1d/§2，全部在**副本**上做，绝不碰 live `data/`：

1. `make check`（fmt、test、race、vet、前端测试、observability），
   加 `make frontend` 后 `git diff --exit-code -- internal/webui/dist`。
2. **原地升级（本轮最重要的一条）**：取一份 **v0.4.0 写出的 data 目录**副本，用
   HEAD 二进制启动。两种可接受结果：干净加载，或干净地 fail-closed 拒绝。任何
   "带着不一致的派生物继续跑"都是 B2 + B8 的阻断级发现。随后验证：
   - `bin/halro doctor`
   - `bin/halro ledger verify`
   - 汇总端点在**升级后立刻**返回的数字，与全量重建后的数字**逐行比对**（B8 的实证）
3. 全新数据目录：启动 → 打一次真实请求 → `doctor` → `ledger verify` →
   `make backup` → 恢复到 scratch 目录并从中启动。
4. 抽样基准：Ledger append 与路由解析两组，`benchstat` 与
   `docs/verification/performance-baseline.md` 比；前端包体积与 v0.4.0 比，>10% 需理由。
5. MiniMax 真账号冒烟**不重跑**（billable，且 #250 刚做过）。改为复核 #250 留下的证据
   文件是否覆盖三个 Profile 行各自的实际调用，缺哪一行就在记录里写明"未实测"。

## 9. 阶段 4：对抗验证

阶段 1–3 产出后，**对最严重的 3–5 条各派一个独立"证伪型"角色**：默认 finding 为错，
要求在代码里复现完整路径或找出拦截防御，裁决 CONFIRMED / REFUTED / PARTIAL，
落 `adversarial-verdicts.md`。这一环在 260805 与 260807 两轮都改写了结论
（六条 P0 无一原样成立、五条最严重发现无一原样成立），不是可选项。

本轮预判最需要证伪的方向（不预设结论，只预留位置）：

- "升级后 rollup 与 checkpoint 不一致会被检出"——真的会吗，还是只在两者都存在时比较？
- "折叠不丢账"——折叠行的 `FirstSequence` 被后来的增量覆盖后，下一次增量的准入判断
  是否还成立？
- "MiniMax 能力上限由证据支撑"——`Defaults == Ceiling` 在**导入/恢复/迁移**三条写路径上
  是否都成立，还是只在 Admin 表单上成立？

## 10. 阶段 5：发布记录与发布动作

评审通过后才做，顺序不能反。

**发布记录**：`docs/verification/assessments/v0.5.0.md`，按 §5 模板填满，
含 `Re-init required: yes/no` 与 owner 的 GO/NO-GO 签字。

**发布机械动作**（`chore(release): v0.5.0` 一个提交）：

- [ ] `CHANGELOG.md`：把 `## [Unreleased]` 落成 `## [0.5.0] - <日期>`，逐条写用户可见
      变化；**升级需要注意的（若有）单独成段**
- [ ] `web/package.json` 版本 `0.4.0` → `0.5.0`（并同步 `package-lock.json`）
- [ ] README 镜像标签
- [ ] `docs/verification/dependency-license-review.md` 的漂移哈希（版本 bump 会移动它）
- [ ] 确认 `## [0.5.0]` 段存在——否则 release workflow 在 `prepare` 阶段就拒绝
- [ ] Actions → release → Run workflow，先 `dry_run: true` 跑一遍完整彩排，
      再正式发布（tag 由 publish job 创建，失败不留 tag）

## 11. 本轮明确不做

- 不重跑计费的真账号 Provider 矩阵（§8.5 已说明替代做法）。
- 不对 v1.0.0 的历史门禁做处置——那是 v1.0.0 的议题，本轮是 v0.x。
- 不做完整威胁建模、不做全量性能压测（理由见 §6 末与 §3）。
- 不改动被评审的代码：评审阶段只产出结论与证据，整改另起提交，进度落 `progress.md`。
