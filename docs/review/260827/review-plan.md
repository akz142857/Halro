# v0.4.0 发布评审方案（2026-08-27）

> 本文件是**方案**，不是报告。执行后的结论落在同目录 `260827.md`（格式参照
> [`260814/260814.md`](../260814/260814.md)），整改进度落在 `progress.md`。
> 角色定义与选择指南见 [`docs/review/README.md`](../README.md)；发布时刻的标准动作见
> [`docs/verification/release-assessment.md`](../../verification/release-assessment.md)。
> 本文件只做"这一轮该怎么跑"的具体化，不重复定义角色，也不重写发布流程——它把两者
> 按本次范围**接在一起**，并补上两者都没覆盖的部分。

## 1. 本轮的性质：一次会发出去的范围评审

发布目标是**下一个 v0.x（预定 v0.4.0）**，不是 v1.0.0。这决定了两件事：

1. **v1.0.0 的四道未清门禁不在本轮范围**（24 小时 soak、真账号 Provider 全矩阵、崩溃恢复矩阵
   重跑、发布治理重连）。它们记在 issue #110，该 issue 目前状态为 **CLOSED 而四项仍未打勾**
   ——这个不一致本身要在本轮结束时给一个处置（重开、或把门禁清单搬到文档里），否则 1.0.0 的
   前置条件就只存在于一个已关闭的页面里。见 §9。
2. **v0.x 线无 CI 强制门禁**，`release-assessment.md` 是"评估而非门禁"，最终是 owner 的
   GO/NO-GO。因此本轮的产出必须是**一份可签字的记录**（`docs/verification/assessments/v0.4.0.md`），
   而不只是一份问题清单。

但本次范围不是一次普通的 v0.x 补丁，理由在下一节：它同时触发了 `release-assessment.md` §0
表里**六行触发条件中的五行**，并且改动了会随发布固化的对外构造（能力词表、端点清单、
bbolt schema、探测契约）。所以在标准评估流程之外，另加多角色并行评审与对抗验证。

## 2. 范围与已量得的事实

```
基线 v0.3.0 = abfc05c (2026-08-24 01:25 +0800)
HEAD        = 8bb4847
git diff --stat v0.3.0..HEAD  →  182 files changed, 12913 insertions(+), 1520 deletions(-)
提交 3 个（squash 后）：#229 发布标签文案、#230 探测可答性 + 部署卡片、#231 Responses 档案四处缺陷
```

改动面按包（生产代码，不含测试与 `internal/webui/dist`）：

| 面 | 主要文件 | 性质 |
|---|---|---|
| 能力词表拆分 | `internal/domain/capability_dictionary.go`、`capability_modality.go` | `json_mode` 就地拆为 `json_object` + `structured_outputs`；词表 v2 |
| durable schema | `internal/store/bolt/store.go:24`（`schemaVersion = 32`）、`store.go:839` 迁移 `structured_output_capability_split` | 存量记录两半皆关、证据置 unsupported、探测结果清空 |
| 语义请求上移 | `internal/semantic/request.go`、`internal/gateway/service.go`（+268 行） | 一元生成热路径改为以 semantic request 进入；脱敏与 token 估算随之上移 |
| Responses 档案 | `internal/provider/openai/adapter.go`、`internal/provider/profile.go`、`internal/compatibility/openai/*` | 新增 `openai.responses.v1`；`web_search` 唯一准入的 hosted tool，默认关闭 |
| 脱敏遍历 | `internal/redaction/engine.go`（+196 行） | 新增 `ContentProviderToolCall` / `Citations` 处理；`default` 分支改为拒绝未知内容种类 |
| 能力探测 | `internal/provider/capability_detection.go`、`internal/app/admin_model_capability_detections.go` | 探测契约 v5（v0.3.0 为 v1；方案初稿误记 v4，叫停点 1 依 range-map 表 4 更正）；预算 8→9；新增 `json_schema` 探针 |
| 目录与前端 | `internal/modelcatalog/builtin.go`、`web/src/pages/*`（22 文件） | 目录基线扩充；部署改卡片式渲染 |
| 对外契约文档 | `docs/compatibility/endpoint-manifests.json`、`docs/contracts/{openai-compatibility,provider-capabilities,metrics-reference}.md` | 随上面同步 |

**未触及**（本轮据此收窄，不作全面复审）：`internal/ledger`、`auth`、`adminauth`、
`safetransport`、`budget`、`limiter`、`tokenguard`、`usage`、`audit`、`contentscan`、`sse`、
`circuit`、`idempotency` 在 `v0.3.0..HEAD` 内零改动（`git diff --name-only` 空输出）。
这条事实是本轮所有"不查"决定的唯一依据，执行时先自行复核一遍再采信。

### 2.1 触发的深度检查（`release-assessment.md` §0 表逐行判定）

| 触发行 | 本轮 | 依据 |
|---|---|---|
| `internal/ledger` / WAL 帧 / `internal/store` schema → 恢复通道 | **触发** | schema 31→32（v0.3.0 已是 31；方案初稿误记 30，叫停点 1 更正），含数据搬移的迁移 32 |
| `internal/provider/*` 线上行为 / `semantic` 映射 → 受影响 Provider 的真账号冒烟 | **触发** | OpenAI（新 Responses 档案）、Bedrock Mantle、Anthropic、Gemini、Bedrock 均有 adapter 改动 |
| `auth`/`adminauth`/`redaction`/`contentscan`/`safetransport` → 针对性安全复读 | **触发**（仅 `redaction`） | `engine.go` 重写遍历；其余四包未动 |
| 请求热路径 / `budget` / `limiter` / `tokenguard` → 基准对比强制 | **触发**（仅热路径） | `gateway/service.go`、`semantic/request.go`、facade |
| `web/` only | 不适用 | 后端同时大改 |
| 任何 durable 格式 → 原地升级检查强制 | **触发** | bbolt schema、探测契约 v5、能力词表 v2 |

五行触发。唯一未触发的是"`web/` only"那一行，它本就是收窄条款。

## 3. 判定基准

一条发现要成立，必须指认它违反下列某一条基准，并给出 `文件:行号`。没有基准支撑的写成"疑问"
而不是"问题"。前七条来自 `CLAUDE.md` 不变式与 `release-assessment.md` §3，后三条是本次范围
特有的。

| 编号 | 基准 | 来源 |
|---|---|---|
| B1 | 预算预留在 Provider 请求发出**之前**持久化；结算原子；语义不明的上游结果保守记账，绝不静默退款 | `CLAUDE.md`、ADR 0018 |
| B2 | fail-closed：损坏/不可用/语义不明/陈旧状态一律拒绝而非降级 | `CLAUDE.md` |
| B3 | 密钥、提示词、响应体、上游模型标识不进日志/错误/指标/审计 | `CLAUDE.md`、威胁模型 |
| B4 | 能力过滤发生在 Provider I/O 之前；不支持的字段**拒绝**而非静默丢弃 | `CLAUDE.md`、`docs/contracts/provider-capabilities.md` |
| B5 | 重试/回退有界，且响应字节对客户端可见之后不再切换 Provider | `CLAUDE.md` |
| B6 | 前 1.0.0 就地修复：错误构造不得与替代品并存；durable 格式改动必须 bump 版本使陈旧状态被拒而非误读 | `CLAUDE.md` |
| B7 | 单写者、单数据目录；WAL 是记账权威，bbolt/Parquet 是派生物 | `CLAUDE.md` |
| B8 | **拒绝要发生在能改变账目之前**。本次范围内已实证过一次反例：脱敏后的引用 span 校验失败发生在 `attempt.finish` 与 `run.finalize` 之后，一次已计费的成功调用以 502 抵达调用方（#231 提交信息）。同类顺序错误按此基准判 | 本次范围 |
| B9 | **一次能力开关必须只描述一件事**。`json_mode` 之所以要拆，是因为一个开关同时承诺"能出 JSON"与"能守 schema"，勾上其一即被路由到另一半、在预留之后被上游拒绝。范围内新增的任何布尔能力都按这条检查 | 本次范围、#231 |
| B10 | **探测必须问它将来会走的那张面**。探测走 Chat 而档案调用 `/v1/responses`，会把六项能力量在错误端点上并存成 verified 证据（#231 已修一次）。任何新档案/新表面按此检查 | 本次范围、#231 |

## 4. 阶段 0：范围事实底座（先做，无结论）

**目的**：在判断之前把范围的真实形状抄下来，避免用自己脑子里的模型去测自己脑子里的模型
（`CLAUDE.md`「Verify, never assume」）。

**交付物**：`range-map.md`，含四张表：

1. **能力词表迁移表**：`json_mode` 的每一个读点与写点（后端、前端、目录、探测、证据、
   端点清单、文档），迁移 32 前后各是什么值，以及"两半皆关"对存量部署的**可见后果**
   （哪些部署会因此从可路由变为不可路由）。
2. **热路径改道表**：一元生成从 facade 到 Provider 的调用链，改前 / 改后逐跳对照；标出脱敏
   与 token 估算的新位置，以及每一跳相对于预算预留、`attempt.finish`、`run.finalize` 的
   **顺序位置**（B8 的判定底座）。
3. **Responses 档案表**：`openai.responses.v1` 的 Access Surface、凭据方案、能力上限、
   `web_search` 的准入条件与默认值、拒绝路径；与 `openai.chat.v1` 的差异逐项列出。
4. **durable 版本表**：bbolt 32、探测契约 v5、能力词表 v2 三者各自的写者、读者、拒绝条件，
   以及"远端目录在旧词表下发布会被拒"这条是在哪一行代码上成立的。

阶段 0 只抄事实，不下判断。它同时是发布记录 §3 不变式逐条回答的原材料。

## 5. 阶段 1：多角色并行评审（后端）

沿用 `docs/review/README.md` 的角色框架，按本次范围选角色，**每个角色独立通读、互不知情**。
共七个角色，其中三个是发布专项档。

| # | 角色 | 本轮的具体靶子 |
|---|---|---|
| A1 | 核心逻辑 | 热路径改道后的顺序正确性（B1/B8）：脱敏、估算、能力过滤、预留、attempt 生命周期五者的相对次序；`gateway/service.go` +268 行逐段对照改前行为；新增的"脱敏改变了请求对目标的要求"守卫是否覆盖全部内容种类，以及它自身发生在预留之前 |
| A2 | 安全（脱敏专项） | `redaction/engine.go` 的新遍历：`ContentProviderToolCall`、`Citations`、`ContentReasoning` 三条新路径是否都受强制基线与项目策略约束；`default` 拒绝分支能否被绕过；`processCitations` 的 clone 是否覆盖全部共享点（重试会再次路由同一 message）；span 归零策略是否会把"报告了来源"与"来源可信"混同 |
| A3 | 安全（出网与新表面） | `web_search` 意味着上游代替我们发起网络请求，**这条出网不经过 SafeTransport**。查：默认关闭是否在每条写入路径上都成立（含导入/恢复/迁移）、能力上限是否阻止 Deployment 单方面打开、审计里是否留下"本次回答含上游自行发起的检索"的痕迹；`code_interpreter`/`file_search` 的拒绝是否在 I/O 之前 |
| A4 | 测试盲区 | 范围内新增 ~2000 行测试守住了什么、没守住什么。重点：迁移 32 的**存量数据**方向（不是空库方向）、探测在错误端点上作答的负面测试（#231 已声称做过反向验证，复核该反向验证真的失败过）、B8 类顺序缺陷有无回归测试 |
| A5 | BUG 排查 | 全范围找有代码证据的具体缺陷，高危区优先：探测预算 8→9 的分配与下取整、`LegacyAdapterBridge` 的 wrapper 解包、目录 clamp 与 binding 自带上限的取值优先级（#231 修过一次零值回归）、并发下的 Deployment 卡片数据源 |
| A6 | API 兼容性契约 | `docs/compatibility/endpoint-manifests.json`（+47 行）是否与实现一致；能力词表 v2 是**对外可见**的破坏性改动吗（对已有客户端的请求体、对 Admin API 的响应体）；`openai-compatibility.md` 的 28 行改动是否等价描述了新行为；官方 SDK 契约测试覆盖 Responses 新路径与否 |
| A7 | 数据迁移与升级 | 迁移 32 的平滑度与回滚：COW 是否成立、失败点注入、以及**"要不要重新初始化数据目录"这一句必须由实测回答**（#231 提交信息声称不需要，本轮实测复核，见阶段 3） |

**不派的角色与理由**（写下来，是为了让读者知道它被判过而不是被忘了）：交互/视觉设计并入阶段 2；
性能与容量降级为阶段 3 的基准对比（范围内无新增序列化点的初判需 A1 确认）；供应链与发布工程
降级为 §8 的发布前置清单（`release.yml` 本轮只改了标签文案 #229）；可观测性只查
`metrics-reference.md` 那 7 行改动是否与代码一致（并入 A6）。

**交付物**：`findings-backend.md`，每条带 `文件:行号`、违反的基准编号、【肯定 / 建议 / 问题 /
疑似BUG】分级，以及**"是否阻塞发布"**一栏——按 `release-assessment.md` §5 的阻塞类（记账错误、
fail-open、密钥泄漏、数据丢失或静默误读、无界缓冲/重试、静默丢能力、升级无 fail-closed 拒绝）。

## 6. 阶段 2：Admin 控制台（`web/`）

22 个页面文件改动，主体是部署卡片化与能力选择随词表拆分而变。不重审 UI 质量，只审三件事：

- **词表拆分的忠实表达**：控制台是否把 `json_object` 与 `structured_outputs` 呈现为两件事，
  而不是一个开关换了名字；迁移后"两半皆关"的存量部署，运维**看得出它被关了、也看得出为什么**吗
  （B2 的可行动性一面）。
- **`web_search` 的知情开启**：默认关闭要在 UI 上是显式选择，且旁边必须说清它意味着上游自行出网
  （A3 的前端对侧）。
- **卡片化后的信息不丢失**：改前表格里承载的字段，改后是否还能被找到；不可用/被扣留的部署在
  卡片上是否仍然醒目。

另按惯例复核两条常设项：409 revision 冲突是提示重载而非静默覆盖；一次性明文不落浏览器存储
（生产 bundle 的 secret canary 扫描）。

**交付物**：`findings-web.md`；与后端同一条约束两侧不一致的，单列为"契约漂移"。

## 7. 阶段 3：真实二进制验证

静态审查产出"代码看起来会这样"，这一阶段回答"真跑起来是不是这样"。**环境一律用一次性目录，
不碰仓库里的 `data/` 与 `master.key`**。

### 7.1 强制项（由 §2.1 的五行触发推出）

| 编号 | 动作 | 通过判据 |
|---|---|---|
| R1 | 全量门禁：`make check` + `make frontend` 后 `git diff --exit-code -- internal/webui/dist` | 全绿且无 bundle 漂移 |
| R2 | **原地升级**：用 v0.3.0 二进制建库并跑出真实数据（含至少一个勾了旧 `json_mode` 的 Deployment、若干条已探测证据），再用 HEAD 启动 | 干净加载（schema 31→32），或干净 fail-closed 拒绝。若拒绝，则发布说明必须写"需重新初始化数据目录" |
| R3 | 迁移 32 的**语义**复核：R2 之后查那个 Deployment 的两半能力、证据状态、探测记录 | 与 §4 表 1 抄下来的预期逐字段一致；差一个字段就是发现 |
| R4 | 恢复通道：populated 数据目录下 `halro doctor`、`halro ledger verify`、`backup create → verify → restore` 到新目录后启动 | 全绿；ledger 链认证通过 |
| R5 | 基准对比：HEAD vs v0.3.0 同主机同 tree 背靠背跑 `performance-baseline.md` 的基准 | 热路径无 >10% 退化；路由解析与 Token Guard admit 无新增分配。**注意**：`performance-baseline.md`（2026-07-31）在路由解析一项上已知过时（v0.3.0 评估已记录 405ns/1 alloc vs ~4.5µs/41 allocs），本轮以 v0.3.0 实测为基线，并顺手把该文档重测重注日期 |
| R6 | 真账号 Provider 冒烟：按 `provider-real-matrix.md`（本范围内 +77 行）跑**受影响的档案**——OpenAI Chat、OpenAI Responses、Bedrock Mantle 三个 Responses/Chat 表面、Anthropic、Gemini、Bedrock | 计费、opt-in，需 owner 明确下单。若不跑，记录里写明"未验证"，不得以静态推断顶替 |

### 7.2 场景剧本（每条 = 一个可复现序列 + 一个明确预期）

| 编号 | 操作 | 预期 |
|---|---|---|
| S1 | 建一个 `openai.responses.v1` 连接，跑通一次非流式请求 | 打到 `/v1/responses`；审计与用量含完整 owner 三元组，不含凭据与上游模型 ID |
| S2 | 在该连接上跑能力探测 | 九个探针**全部**打在 `/v1/responses`（B10）；证据落为该表面的 verified |
| S3 | 用只有 chat/completions 权限的 key 在 Responses 档案上探测 | 探测判为不可用；**不得**绿灯通过后在首次真实请求（预留之后）才失败 |
| S4 | 开启 `web_search` 并发一次会检索的请求 | 返回 `web_search_call` 与 `url_citation`；档案承载不了引用时**拒绝**而不是抹掉来源返回答案（B4） |
| S5 | 对 S4 的响应施加一条会命中 `action.query` 的 reject 规则 | 422 拒绝；**不得**出现"答案里 `[REDACTED]`、`action.query` 里原文"的不对称（A2 的实证面） |
| S6 | 构造一条脱敏会改变长度的引用文本 | span 归零，Message.Validate 通过，请求正常返回；**不出现**已计费成功调用以 502 抵达（B8） |
| S7 | 构造脱敏把 data URL 内容改成一个地址的请求 | 在预留**之前**被守卫拒绝 |
| S8 | 只勾 `json_object` 的部署收到 `json_schema` 请求；只勾 `structured_outputs` 的收到 schema-less 请求 | 两次都在 Provider I/O 之前拒绝，且错误说得出缺哪一半（B4/B9） |
| S9 | 一次重试路径上的 message 复用 | citations 不被前一次遍历就地改写（A2 的 clone 面） |
| S10 | 目录已覆盖的模型跑一次验证 | 上下文与输出上限保持目录 clamp 值，不归零（#231 修过，守回归） |

**交付物**：`runtime-evidence.md`，含实际命令、实际响应片段（脱敏）、以及与静态判断不符之处。

## 8. 阶段 4：对抗验证

对阶段 1–3 中**最严重的若干条**各派一个独立证伪视角：默认发现为错，要求在代码里复现完整可达
路径或找出拦截防御，裁决 CONFIRMED / REFUTED / PARTIAL。

这一步不是可选项：本仓三轮评审（260805、260807、260814）的最严重发现**无一条原样成立**——
被证伪的、被收窄的、以及比原报告更严重的各有其例。发布前尤其要防的是反向错误：**把一条其实
成立的 fail-open 当成误报放行**。因此裁决 REFUTED 的条目也要写出"拦截它的那道防御在哪一行"，
没写出行号的 REFUTED 不成立。

**交付物**：`adversarial-verdicts.md`。

## 9. 阶段 5：发布前置清单与放行记录

以下是**机械但会卡住发布**的项，执行时逐条打勾。前四条已核实处于未完成状态：

- [ ] **`CHANGELOG.md` 的 `[Unreleased]` 是空的**（`CHANGELOG.md:7`），而范围内有大量对外可见
      改动。`release.yml` 的 `prepare` job 在 `CHANGELOG.md` 没有该版本小节时**拒绝启动**，
      所以这一条既是纪律也是硬卡点。词表拆分与"存量部署两半皆关"必须写进去。
- [ ] **`web/package.json:4` 仍是 `0.3.0`**。
- [ ] **`README.md` 的四处镜像标签仍钉 `v0.3.0`**（`README.md:72,73,116,127`）。
- [ ] **依赖许可漂移哈希**：`scripts/check-dependency-license-review.sh` 校验
      `go.mod`/`go.sum`/`web/package.json`/`web/package-lock.json` 四个 blob 哈希与
      `dependency-license-review.md` 一致；版本号一改，`web/package.json` 的哈希就变，必须同步刷新。
- [ ] **`CLAUDE.md` 的发布状态段落已经过时**：它写着"Nothing has been published (no GitHub
      Release)"，而 v0.1.0 / v0.2.0 / v0.3.0 均已发布（最早 2026-08-15）。前 1.0.0 的
      "就地修复不积累兼容"规则仍然成立（1.0.0 未发），但"没有任何部署在野"这个论据已经不成立，
      句子要按事实改写。**这条不阻塞发布，但属于"发出去就收不回"的对外文案**。
- [ ] **issue #110 的状态与内容不一致**：已 CLOSED，四道 1.0.0 门禁仍未打勾。要么重开，要么把
      门禁清单迁进 `docs/verification/`，让 1.0.0 的前置条件不只存在于一个已关闭的页面里。
- [ ] `docs/verification/performance-baseline.md` 重测重注日期（见 R5）。
- [ ] `docs/milestones/implementation-status.md` 的 "Last updated: 2026-08-04" 与本次范围的差距
      ——更新，或在记录里说明它不再作为发布依据。

**放行记录**：`docs/verification/assessments/v0.4.0.md`，严格按 `release-assessment.md` §5 的模板
（Range / Deep passes / 1 Defects / 2 Performance / 3 Invariants 逐条 / 4 Design / Re-init
required / Verdict）。§3 的九条不变式**每条都要有一句回答**，"untouched" 也要写出来。

**发布动作**：Actions → release → Run workflow，先 `dry_run` 跑一次（走完全部门禁、构建、签名、
证明，但不发布），确认 `quality` / `sdk-compatibility` / `stress` / `provenance` 四个 job 全绿，
再实跑。标签在 `publish` job 内创建，失败不会留下可被误认的标签。

## 10. 进入评审时已存在的待验证假设

下面几条是拟方案时读代码顺手记下的**疑问**，不是发现。列出来是为了让阶段 1 有明确靶子，
同时避免它们被当成结论传播——每一条都必须在阶段 1 里独立证实或证伪。

- **H1 · 迁移 32 的"两半皆关"是一次静默的能力回退。** 提交信息给的理由是"旧的一个 bit 说不出
  部署有哪一半，关掉只是拒绝，开着会转发一个注定失败的请求"——这个取舍成立。但存量部署会**从
  可路由变为不可路由**，而运维只有在请求失败时才发现。要查：是否有启动期或 doctor 层面的可见
  信号，还是只有一条 migration 记录。若只有后者，这是发布说明里必须写明的一条。
- **H2 · `web_search` 是本仓第一条不经 SafeTransport 的出网。** 上游代替我们发起检索，
  `safetransport` 的主机白名单、DNS/IP 校验、禁重定向对它一概不适用。默认关闭是正确的缺省，
  但威胁模型文档里没有这条边界的描述（`docs/architecture/threat-model.md` 本范围内未改动）。
  要查：这是"已知并接受"还是"没人写下来"。
- **H3 · 探测预算 8→9 的分配。** 提交信息说"预算从 8 提到 9，所以没有能力因为新增探针而不再被
  验证"。v0.3.0 评估记录过"探测预算在可运行探针间均分，下限为 1"。要查：9 个探针在 attempt
  timeout 下的实际分配，边界上是否有探针拿到不足以完成一次请求的时间片——那会表现为"能力被
  静默判为不支持"，是 B4 的反面。
- **H4 · span 归零把两件事合并了。** 脱敏改变文本长度时引用 span 归零，与"入站解码放不下的 span"
  走同一条路。两者的语义不同：一个是"来源仍然准确但位置不可知"，另一个是"位置从来就没给对"。
  下游（前端、审计、调用方）能区分吗？若不能，要判断这是可接受的合并还是信息丢失。
- **H5 · `ContentReasoning` 按普通模型文本脱敏。** 提交信息说另一条路是"把一个已知泄漏写成故意的"。
  这个取舍成立，但推理内容与答案内容的策略是否**应该**相同，是产品决定不是实现细节——要确认它
  在 `provider-capabilities.md` 或策略文档里有落笔，而不只是活在一条提交信息里。
- **H6 · 热路径 +268 行而基准未变。** `gateway/service.go` 与 `semantic/request.go` 的改动把
  脱敏与估算搬上了一元路径。R5 若测出"无退化"，要能解释为什么——搬动而非新增遍历，还是有一次
  遍历被消掉了。测不出退化又说不清原因的，说明基准没覆盖到改动。

## 11. 执行顺序与叫停点

```
阶段 0 范围底座 ──▶ 阶段 1 七角色并行 ──▶ 阶段 3 真实二进制 ──▶ 阶段 4 对抗验证 ──▶ 阶段 5 记录与放行
                      ▲                        ▲                     ▲
                 阶段 2 前端（并行）        叫停点 2              叫停点 3
                      ▲
                  叫停点 1
```

- **叫停点 1**（阶段 0 之后）：若 §4 的四张表抄出来与本方案的描述不符，先对齐事实再往下走——
  后面所有判断都建在这四张表上。
- **叫停点 2**（阶段 1/2 之后）：若出现阻塞类发现，先修再进阶段 3；修完的每一条都带回归测试，
  并做反向验证（backing out 后测试必须真的失败；`go test -count=1`，缓存的 `ok` 不算证据）。
- **叫停点 3**（阶段 3 之后）：R6 真账号冒烟是计费项，需 owner 下单。不跑就在记录里写"未验证"，
  **不得**用静态推断顶替。R2 若判出"需重新初始化数据目录"，发布说明与 CHANGELOG 要先改再发。

阶段 0、1、3、5 是必做的地基；阶段 2 与阶段 1 并行；阶段 4 只对最严重的若干条做，不求覆盖。
