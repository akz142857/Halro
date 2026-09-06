# Run Governance Admin UI 专业重构计划

日期：2026-09-06
状态：Implemented and validated（2026-09-06）
范围：Halro Admin `/admin/run-governance` 的信息架构、交互、视觉层级、文案、响应式与前端数据状态；不改变 Run Governance 的账本、预算和 Outcome 权威语义。

## 1. 结论

当前页面的核心问题不是单纯的样式粗糙，而是把三种频率、三种风险级别完全不同的工作放在一个连续长页面中：

1. **日常运行监控**：判断哪些 Work Unit / Run 正在运行、失败、超预算或缺少结果；
2. **业务结果分析**：选择 Definition 与 cohort，理解覆盖率、成功率、费用完整性；
3. **治理配置**：创建、版本化、启停 Outcome Definition。

页面因此没有稳定的主任务。操作者必须跨多个大面板来回建立 `Project → Work Unit → Run → Attempt → Outcome` 的关系，结果定义表单又插在执行链与分析链之间，导致信息层级、操作层级和视觉层级同时混乱。

建议把页面重构为一个以 **Work Unit 为主对象** 的治理工作台，并分成三个清晰视图：

- **概览**：回答“现在是否健康、哪里需要处理”；
- **工作单元**：回答“这个业务工作发生了什么、花了多少、结果是什么”；
- **结果与口径**：回答“业务结果如何统计、Definition 如何管理”。

第一阶段不需要更改后端权威模型。现有 Work Unit、Run、Usage、Outcome、Definition 与 Summary API 足以完成主体重构；只把聚合接口优化列为后续可选项。

## 2. 当前证据与已确认问题

### 2.1 来自真实页面和验收链路的事实

- 页面当前按固定顺序纵向展示：筛选、Work Units、结果定义与创建表单、业务结果摘要、Outcome 历史、Runs、选中 Run 的 Attempts。
- 单项目环境仍要求手动选择 Project；选择前整个主体只有大面积空状态。
- 外部 Gateway 调用创建或关闭 Work Unit / Run、上报 Outcome 后，页面不会明确提示数据已过期，也没有显式刷新入口；截图中的 active Run、Outcome 计数与稍后的权威状态可能不同步。
- 表格以完整 `wku_`、`run_`、`odef_`、`key_` 标识为第一视觉信息，业务状态、生命周期和异常原因反而弱化。
- 状态主要显示为绿色圆点。颜色不能独立表达 open、active、closed、expired、available、depleted 等不同语义。
- Definition 创建表单永久占据主页面，并与只读监控内容拥有相同视觉权重。
- cohort 摘要一次展示 7 个同权指标、完整性警告和两个底层 watermark；运营结论与审计细节没有分层。
- 宽屏仍出现大量无效空白；较窄屏会面对 7–9 列表格和长 ID 的横向压力。

### 2.2 已确认的功能缺陷

`RunGovernancePage` 在查看 Run Attempts 时请求：

```text
/admin/api/v1/usage?limit=200&run_id=...
```

服务端分页上限为 100，真实请求返回 HTTP 400。当前单元测试只断言携带 `run_id`，没有验证 `limit` 与服务端契约一致。

该问题应在任何视觉重构之前修复，否则新的下钻交互仍会落入不可用路径。

### 2.3 根因

1. 组件按“后端资源列表”排列，而不是按“操作者的决策流程”排列；
2. 页面状态只有零散 React state，没有完整的可分享/可恢复 URL 状态；
3. 所有资源和配置均使用同一种 panel + table 表达，没有主次层级；
4. 缺少 progressive disclosure：Run 和 Attempt 应是 Work Unit 的下钻内容，而不是页面底部的新板块；
5. 缺少 refresh/staleness 模型，导致 Gateway 写入后的 Admin 读取体验不可信；
6. 测试覆盖了数据能渲染，但没有覆盖真实服务端分页边界、信息层级和响应式任务完成度。

## 3. 产品目标

重构后的页面应让管理员在 10 秒内回答四个问题：

1. 当前 Project 有多少 open Work Units、active Runs 和需要关注的异常？
2. 某个 Work Unit 的 Runs、模型调用、已结算费用和 Outcome 是什么？
3. 某段 cohort 的结果覆盖率、成功率和单位成功模型费用是否完整可信？
4. 当前使用的 Outcome Definition 是哪个不可变版本，后续 Work Unit 会采用什么版本？

同时满足：

- 不把零费用误解为没有调用；明确区分 `free`、known zero、estimated 和 unknown；
- 不把 provisional Outcome 或未 matured Work Unit 纳入完成结果；
- 不把旧快照显示成实时状态；
- 不把 raw ID、watermark 和底层审计信息放在主要决策之前；
- 中英文在信息结构、状态语义和交互动作上完全对齐。

## 4. 非目标

- 不把 Halro 变成 Agent workflow、编排、记忆或执行回放产品；
- 不在 Admin 中创建或发起业务 Work Unit / Run；这些仍由应用通过 Gateway API 创建；
- 不改变 Accounting Ledger、Governance Journal、Definition 不可变版本或 cohort 口径；
- 不把模型费用扩写成 ROI、全 TCO 或业务利润；
- 不为了新布局引入新的前端框架或并行设计系统。

## 5. 目标信息架构

### 5.1 页面骨架

```text
运行治理
┌ Project switcher ─ Governance 状态 ─ 数据时间 ─ 刷新 ─ 项目设置 ┐
├ 概览 ─ 工作单元 ─ 结果与口径 ────────────────────────────────┤
└ 当前视图内容 ───────────────────────────────────────────────┘
```

项目选择是全页上下文，不再与 Work Unit/Run 状态过滤器混在一起。选择写入 URL，并按以下顺序恢复：

1. URL 中有效的 `project_id`；
2. 上次使用且仍有权限的 Project；
3. 只有一个启用 Run Governance 的 Project 时自动选中；
4. 多项目且无历史选择时才显示 Project 选择空状态。

推荐 URL 状态：

```text
/admin/run-governance?project_id=prj_...&view=work-units&work_unit_id=wku_...&run_id=run_...
```

### 5.2 视图一：概览

目标是异常优先，而不是把所有明细再复制一遍。

首屏只保留四个决策指标：

- Open Work Units；
- Active Runs；
- 预算受限 Runs（fully reserved / depleted）；
- 缺少最终 Outcome 的 matured Work Units。

下面采用两列：

- 左：最近活动的 Work Units；
- 右：需要关注的异常队列，例如即将过期、预算耗尽、unknown cost、缺少 Outcome。

每个指标都可进入带过滤条件的“工作单元”视图。没有异常时显示明确的“当前无需处理”，避免仅显示绿色数字。

审计 watermarks、generation、sequence、offset 收进“数据与审计详情”可展开区域；首屏只显示“更新于 14:46 · 数据完整/部分”。

### 5.3 视图二：工作单元

Work Unit 是主对象，Run 和 Attempt 不再是独立的页面底部板块。

推荐 master-detail 结构：

```text
Work Unit 列表                         Work Unit 详情抽屉/侧栏
┌ 状态/时间/搜索筛选 ┐                ┌ 生命周期与结果 ┐
│ 业务标识  状态  费用 │  选择一行 →  │ Outcome        │
│ ...                 │                │ Runs           │
└─────────────────────┘                │ Attempt 明细    │
                                       └───────────────┘
```

列表首列不再直接展示完整 ID：

- 主行：`Work Unit · d07ff…ca9p8`；
- 次行：创建时间、创建 Key 名称或短 ID；
- 提供复制完整 ID 的明确按钮和 accessible label；
- 支持按完整 ID 搜索，但不让 ID 主导版面。

建议列：

1. Work Unit；
2. 生命周期状态（带文字 badge）；
3. Runs（总数 / active 数）；
4. Outcome（accepted / rejected / missing / provisional）；
5. 费用证据（金额 + known/estimated/unknown）；
6. 最近活动时间；
7. 查看详情。

详情区按业务因果顺序排列：

1. Work Unit 状态、创建/关闭时间和冻结 Definition；
2. 当前 Outcome 与修订时间线；
3. Runs 列表；
4. 选中 Run 的预算条和 Attempts；
5. 技术审计信息。

Run 预算使用可视化为一条简单进度条：`committed + reserved / cap`。同时保留精确金额和状态文字，不能只靠颜色。

Attempt 行重点显示：请求状态、public alias → provider model、tokens、费用证据、延迟和完成时间。`free` 应显示“免费价格版本”，known zero 显示“已知 US$0.00”，不能与 unknown 混为一谈。

### 5.4 视图三：结果与口径

该视图包含两个二级区域：

- **结果分析**：默认区域；
- **Definition 管理**：低频配置区域。

#### 结果分析

筛选条固定为 Definition version + cohort date range，并在选择后显示：

1. 一条完整性结论：完整、部分、无 eligible units、缺少 Outcomes 或存在 unknown cost；
2. 三个主指标：覆盖率、成功率、单位成功模型费用；
3. 三个证据指标：known cost、estimated subset、unknown Attempts；
4. cohort 漏斗：eligible → matured → evaluated → successful；
5. Outcome 修订列表，只显示当前筛选 Definition/cohort 的相关数据。

`in_progress_cost` 不与最终结果指标并列，应放在完整性说明或 cohort 漏斗旁，明确“尚未进入最终分母”。

#### Definition 管理

- 默认只展示每个 Definition 的最新版本；
- 历史版本通过展开行查看，不把每一版平铺成主表行；
- “创建 Definition”和“新建版本”均使用现有 Modal/Drawer，不常驻占用页面；
- 禁用动作需要标准确认 Modal；创建新版本时先显示影响说明：“只影响之后新建的 Work Unit”；
- 成功值使用 token/chip 展示，避免逗号文本在窄输入框中被截断。

## 6. 视觉与组件规范

### 6.1 层级

- 一个页面只允许一个主标题；二级视图使用清晰 tab，不重复堆叠大标题和 count；
- 主内容区使用 1 个主要 panel，内部以 section/divider 分组，减少“每块都带完整边框”的容器套娃；
- 荧光绿色只用于 primary action、选中态和需要注意的关键数值，不用于所有计数；
- 只读技术信息使用 muted mono；业务结论使用正常 UI 字体；
- 空状态高度由内容决定，不再使用大面积固定留白。

### 6.2 状态表达

统一文字 badge：

- Work Unit：进行中 / 已关闭；
- Run：活跃 / 已关闭 / 已过期；
- Budget：可用 / 已全部预留 / 已耗尽；
- Outcome：已接受 / 已拒绝 / 待上报 / 暂定；
- Cost evidence：已知 / 含估算 / 未知 / 免费价格版本。

每个 badge 同时包含图形、文字和可访问名称，颜色仅作为辅助。

### 6.3 刷新与时效

- 页头提供“刷新”按钮、最后成功更新时间和加载状态；
- Project 选中后对 Work Units、Runs、Outcomes、Summary 做一致刷新；
- active Runs 存在时可采用 10–15 秒温和轮询；全部终态后停止轮询；
- 浏览器重新获得焦点时刷新；
- 外部数据更新造成旧详情时，显示“有新数据，刷新查看”，不能静默保留旧状态；
- Definition mutation 成功后同时刷新 Definitions 与依赖它的 Summary selector。

## 7. API 与数据边界

### 7.1 MVP 不新增后端接口

继续复用：

- `/admin/api/v1/run-governance/work-units`；
- `/admin/api/v1/run-governance/runs`；
- `/admin/api/v1/usage?run_id=...`；
- `/admin/api/v1/projects/{id}/outcome-definitions`；
- `/admin/api/v1/governance/outcomes`；
- `/admin/api/v1/governance/summary`。

Attempts 查询必须改为服务端允许的 `limit=100`，并遵循 `next_cursor` 读取完整列表；不能只把 200 改成 100 后丢弃后续页。

### 7.2 可选后端优化

只有在前端并行请求和大数据量测量证明有必要后，才考虑新增 project-scoped overview endpoint，返回：

- open/active/at-risk/missing-outcome counts；
- recent Work Units；
- accounting/governance watermarks；
- generated_at。

该接口只能汇总现有权威读模型，不得创建新的费用或 Outcome 权威来源。

## 8. 交付阶段

### Phase 0：修复阻塞缺陷并冻结基线

- 修复 Attempts `limit=200` 与服务端上限 100 的不一致；
- 为分页和 Run filter 增加真实契约测试；
- 补一个包含 open/closed、free/known/unknown、provisional/final 的页面 fixture；
- 保存当前桌面截图作为 before evidence。

完成标准：真实 Run 的“查看调用”可以看到完整 Attempt 明细。

### Phase 1：页面 shell 与状态模型

- 建立全页 Project context 和三个视图 tab；
- 单项目自动选择；
- 将 view/project/work-unit/run 写入 URL；
- 增加 refresh、更新时间和 staleness 状态；
- 保留现有 API，不改变业务语义。

完成标准：刷新、返回、复制 URL 后能恢复相同上下文；外部 Gateway 写入后不会长期显示旧状态。

### Phase 2：Work Unit 主路径

- 重做 Work Unit 列表；
- 增加详情 Drawer/侧栏；
- 把 Runs、Attempts、Outcome 移入 Work Unit 下钻；
- 增加预算条、费用证据 badge、短 ID 与复制动作；
- 删除页面底部独立 Runs/Attempts 面板。

完成标准：从 Work Unit 到真实 Attempt 不超过两次操作，并始终可见当前 Work Unit/Run 上下文。

### Phase 3：结果分析与 Definition 管理

- 重组 summary 为完整性结论、3 个主指标、证据指标和 cohort 漏斗；
- Outcome 历史跟随 Definition/cohort 筛选；
- Definition 创建/版本化移入 Modal/Drawer；
- 默认折叠历史 Definition versions；
- 完成中英文术语与说明重写。

完成标准：管理员能明确区分 provisional、matured、missing outcome、unknown cost 和 free price。

### Phase 4：响应式、无障碍与视觉收口

- 验证 1440、1024、768、390 px；
- 1024 以下详情区改为全宽 Drawer；
- 768 以下表格转为关键字段卡片或可控列，不依赖无提示横向滚动；
- 完整键盘路径、焦点返回、screen-reader label 和非颜色状态；
- 复核 dark/light theme、中文长文本和 en-US 文案。

完成标准：所有核心任务在桌面、平板和手机宽度均可完成；无状态仅靠颜色表达。

## 9. 建议的代码边界

避免继续扩大单个 242 行页面组件，建议拆分为：

```text
web/src/pages/run-governance/
  RunGovernancePage.tsx
  GovernanceProjectBar.tsx
  GovernanceOverview.tsx
  WorkUnitExplorer.tsx
  WorkUnitDetail.tsx
  RunBudgetBar.tsx
  AttemptTable.tsx
  OutcomeAnalytics.tsx
  DefinitionManager.tsx
  governance-state.ts
```

公共设计系统只抽取真正跨页面复用的组件，例如 `StatusBadge`、`CopyableID` 和 `DataFreshness`；领域组件留在 run-governance 目录，避免把业务语义塞进通用组件。

预计涉及：

- `web/src/pages/RunGovernancePage.tsx` 或新的领域目录；
- `web/src/pages/RunGovernancePage.test.tsx`；
- `web/src/api.ts` 与 `web/src/api.test.ts`；
- `web/src/types.ts`（仅在现有响应字段未完整表达时）；
- `web/src/styles.css` / design-system layer；
- `web/src/i18n/locales/zh-CN.ts`；
- `web/src/i18n/locales/en-US.ts`；
- 重建 `internal/webui/dist`。

## 10. 测试与验收矩阵

### 10.1 行为测试

- 单 Project 自动选择；多 Project 不擅自选择；无治理 Project 给出配置入口；
- URL context 的写入、刷新恢复、浏览器前进/后退；
- Work Unit → Run → Attempt 下钻及返回；
- Attempt 分页上限、cursor 前进和不完整列表 fail closed；
- active polling、终态停止、focus refresh、手动 refresh；
- Definition 创建、新版本、禁用及 mutation 后的相关查询刷新；
- Summary complete/partial/unavailable/zero-denominator；
- free、known zero、estimated、unknown cost 的不同显示；
- provisional Outcome 在 Work Unit 关闭和 Run settled 后转为 final；
- API 503 不得显示为 0 项成功或完整数据。

### 10.2 可访问性与响应式

- 所有 status badge 有可见文字和 accessible name；
- Drawer/Modal 打开后焦点进入，关闭后返回触发点；
- 表格/卡片在 390 px 不丢失状态、金额和主操作；
- 中文、en-US、长 Definition 名称、长 provider model 不溢出；
- Copy ID 动作可键盘操作并有成功反馈。

### 10.3 真实验收

使用独立验证数据完成：

1. 创建 Definition；
2. 创建 Work Unit 和 Run；
3. 带 `X-Halro-Run-ID` 发起真实模型调用；
4. 验证 Attempt 的 Project/Work Unit/Run/Key 归因；
5. 上报 provisional Outcome；
6. 关闭 Run/Work Unit，验证 Outcome final；
7. 验证 summary 的 eligible/matured/evaluated/successful 与手算一致；
8. 验证 Admin 刷新和 URL 下钻；
9. 验证无价格、免费价格、已知价格三种费用证据。

### 10.4 仓库验证

迭代时只运行直接相关测试：

```sh
cd web
npx vitest run src/pages/RunGovernancePage.test.tsx src/api.test.ts
npx vitest run src/design-system.test.ts
npm run typecheck
```

交付前只执行一次完整 frontend gate：

```sh
cd web
npm run typecheck
npm test
npm run build
cd ..
git diff --exit-code -- internal/webui/dist
```

CSS-only 迭代使用视觉检查和 `design-system.test.ts`，不在每次间距调整后重复完整测试。

## 11. PR 拆分建议

1. **PR 1 — contract fix + page state**：Attempts 分页、Project 自动选择、URL 状态、refresh；
2. **PR 2 — Work Unit explorer**：主列表、详情 Drawer、Runs/Attempts/Outcome 下钻；
3. **PR 3 — analytics + definitions**：cohort 漏斗、指标层级、Definition Modal；
4. **PR 4 — responsive + i18n + visual QA**：多断点、无障碍、中英文、bundle 与最终截图。

每个 PR 保持行为、测试和生成 bundle 同步；最后一个 PR 不承担补救前面遗漏契约的职责。

## 12. 完成定义

只有同时满足以下条件，才能称为“运行治理 UI 重构完成”：

- 页面以 Work Unit 为主路径，Runs/Attempts/Outcomes 的关系无需上下滚动寻找；
- 监控、分析、配置不再同屏争夺主层级；
- 真实 Attempt 下钻不再因分页参数失败；
- 数据新鲜度、完整性和费用证据均可见且语义准确；
- Definition 版本化不被简化成普通编辑；
- 中英文、键盘、screen reader、390–1440 px 均通过验收；
- 真实 Gateway 调用的归因链和 summary 手算一致；
- 前端完整 gate 与 embedded bundle drift check 通过；
- 实现状态、浏览器验证状态和真实 Provider 验证状态在交付报告中分别说明。

## 13. 实施与验收记录

### 13.1 已实现

- 修复 Usage Attempts 的服务端分页契约：固定每页 `limit=100`、保留 `run_id`、跟随 `next_cursor`，并对重复 cursor 与页数上限 fail closed；
- 建立 Project 全页上下文、概览 / Work Units / 结果与口径三级信息架构，以及可分享、可前进后退的 URL 状态；
- 将 Work Unit → Outcome → Run → Attempt 收进 master-detail Drawer，增加预算条、短 ID、复制反馈、文字状态和逐 Attempt 费用证据；
- 按每个 Work Unit 冻结的 Definition + version 计算 Outcome 完整性，不再把不同 Definition 的 revision 混排或用任意一个 Outcome 代表全部口径；
- 结果分析使用后端一致的 `period_id` cohort，展示统一完整性结论、主指标、费用证据、漏斗和审计水位；
- Definition 管理默认只展示最新版本，历史版本折叠，创建 / 新版本 / 启停使用 Modal，允许值与成功值使用可换行 token editor；
- active Run 每 15 秒温和刷新，终态停止；窗口重新获得焦点和手动刷新会触发查询刷新；超过 60 秒或 refetch 失败时明确标记过期 / 缓存快照；
- 完成中英文对齐、英文单复数、非颜色状态、Modal 焦点进入 / 返回、移动端字段标签，以及 390 / 768 / 1024 / 1440 px 响应式布局；
- 收口页头操作与审计凭据的视觉规范：刷新 / 项目设置使用有边界的 44 px 操作控件，审计信息采用标准 disclosure、结构化证据卡和固定区块间距；
- 升级概览活动区：最近 Work Unit 使用身份、状态、运行数、费用证据和最近活动的结构化记录卡；异常队列使用一致的标题计数和正向空状态，并修复中文 Run 数量文案；
- 已重建 `internal/webui/dist`，源码与 embedded bundle 同步交付。

### 13.2 安全约束下的状态恢复

生产浏览器 artifact gate 明确禁止 `localStorage`、`sessionStorage` 与 IndexedDB。为避免在浏览器持久化控制平面上下文，本实现采用：

1. URL 中的有效 `project_id` 作为跨刷新、跨标签和分享的持久上下文；
2. 当前 SPA 生命周期内记忆最近 Project，用于控制台内部导航后返回；
3. 单个启用治理的 Project 自动选择；
4. 多项目且 URL / 当前会话均无选择时显示选择状态。

因此不新增不符合仓库安全门禁的浏览器持久化例外。

### 13.3 多 Agent review

实现完成后开启了数据 / 状态语义、UX / 无障碍、验证 / 生成物三个独立只读审查。首轮发现并修复：

- 多 Definition Outcome 被错误压缩为单 Outcome；
- Summary 的 Outcome 完整但费用不完整时仍显示绿色完整；
- cohort 使用 UTC `created_at` 而非 Accounting `period_id`；
- 浏览器历史跨 Project 时 Definition 与 Modal 状态串项目；
- 自动选中已禁用 Definition 的历史版本；
- 零费用、无调用和 estimated 证据混淆；
- Definition 新版本 Modal 初始焦点落在 disabled 字段；
- 390–1024 px 卡片缺字段标签及 1024 px 横向溢出；
- embedded bundle 晚于源码修改而过期。

二次 Agent 复核受到 Agent 额度限制；改由定向回归、类型检查、四断点真实浏览器检查、完整前端测试和生产构建完成闭环。

最终门禁：TypeScript typecheck 通过；Vitest `44 files / 556 tests` 通过；生产构建、bundle size 与 `29 files` artifact secret scan 通过。连续两次生产构建的 embedded bundle 内容哈希一致（`7994c343021a1beb86bbf3c11fc32da8b127e9ffafafd8673ae8317a6febe0f9`）。本次仅修改前端、文案和生成 bundle，没有 Go 源码变化，依据仓库“run what the change can affect”规则未重复运行 Go / race 门禁。

### 13.4 验证边界

- **实现验证**：分页、URL、项目选择、多 Definition、provisional → final 刷新、Definition mutation、费用四态、staleness 与 en-US 文案均有自动化覆盖；
- **浏览器验证**：本地 Admin + Vite 代理读取真实 Demo Project、Work Unit、Run、Outcome 与真实模型 Attempt；验证了 overview、Work Unit Drawer、结果分析和 390 / 768 / 1024 / 1440 px 布局；
- **真实 Provider 验证**：现有账本中的免费价格版本真实 Attempt 已完成归因与下钻；known zero、estimated 和 unknown 的展示由契约 fixture 覆盖，本轮没有为了制造状态而改写 Provider 价格或伪造账本事件。
