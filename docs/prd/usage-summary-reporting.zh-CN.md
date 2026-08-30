# 用量汇总报表（按日 / 按月 / 按年）：设计与实施计划

- 状态：In progress — D6/D7/D9 取 a、D8 取 d，均已实现（S0/S1 完成，S2 部分完成，未提交）
- 目标版本：metadata schema v33 / usage checkpoint v8 / Admin API v1
- 日期：2026-08-30
- 文档语言：中文
- 适用范围：Usage 汇总存储与读路径、Admin API、控制台「用量与调用」页、CLI `halro usage`
- 评审：已过一轮多视角评审（3 blocker / 8 major / 13 minor / 6 nit），本版是评审后的修订稿

## 一、这份计划要解决的问题

控制台今天只能回答「此刻」和「这一次」，回答不了「这个月花了多少」。

| | 运行总览 | 用量与调用 |
|---|---|---|
| 回答的问题 | 现在健康吗 | 这一次调用发生了什么 |
| 数据源 | 内存 `usage.Aggregate`，15 秒轮询（`web/src/pages/DashboardPage.tsx:31`） | 同一份内存明细，游标翻页（`internal/usage/query.go:70`） |
| 时间范围 | 当天账期 + 7 天逐小时，窗口硬编码（`internal/usage/query.go:149`） | 任意 start/end 过滤，但**不做任何聚合** |
| 上限 | — | 单页 1–100 条（`internal/usage/query.go:71`） |

结论：**后端没有任何「按区间聚合」的读路径**。按月/按年不是给现有接口加一个参数就能出来的
东西，需要新建一层持久汇总。

读码另有两个必须在设计里正面处理的既有事实：

1. **Parquet 分区按 UTC 日切**（`internal/usage/parquet.go:201`，`CompletedAt.UTC().Format("2006-01-02")`），
   而运行总览的「今天」是**账期日**（`internal/budget/period.go:134`，本地时区的 `[Start, End)`）。
   两者在非 UTC 实例上本来就不是同一批调用。汇总报表如果按 UTC 截断，就会出现「总览说今天
   130 次、月报里这一天 118 次」这种无法解释的差异。
2. **Ledger 事件自带账期身份**：`Period.Stamp` 把 `PeriodID`、`PeriodTimezone`、
   `PeriodTimezoneVersion`、`PeriodStart/EndMicros` 写进每一条事件（`internal/budget/period.go:42`）。
   汇总的日键因此**不需要重新推导**，时区切换、夏令时、日界线这几类边界在写入时已被裁决过一次。
   > 但这只解决 rollup **自身**的边界，**不能**让它自动与今天的运行总览对上：账期在**受理时**
   > 裁定并被下游继承（`internal/budget/manager.go:369` `PeriodAt(now)` + `Stamp`；
   > `period.go:36-41` 明说不在结算时重新推导），而 `Dashboard.Today` 按**完成时刻**落窗
   > （`internal/usage/query.go:189`、`:215` 的 `period.Contains(...CompletedAt)`）。23:59 受理、
   > 00:02 结算的请求，rollup 记 D 日、总览记 D+1 日。**D6 已裁决为 a**：S0 内把总览也改成按
   > `PeriodID` 归集，此后一个实例只有一种「日」。这是一处会改变运维每天看到的数字的改动。

## 二、目标与非目标

目标：

1. 运维/财务能在控制台按**日、月、年**看到调用量、成功率、令牌、成本，并按项目 / 公开模型 /
   服务商 / 部署下钻；
2. 汇总数字与运行总览的「今天」**在同一口径上相等**（D6-a：总览一并改为按 `PeriodID` 归集）；
3. 汇总是**可从 Ledger 完整重建的派生物**，永远不成为余额或成本的第二真相源；
4. 历史深度不被 Parquet 的 `retention_days`（默认 90，`internal/config/config.go:675`）截断。

非目标：

- 不改预算账期语义（账期仍是一天，月/年只是报表的分组，不是新的预算窗口）；
- **本期不按 Gateway Key / Route 分摊**。数据全程可得（`manager.go:655` → `aggregate.go:44`），
  这是一个范围决定而非遗漏；日后要加就是新增维度并跑一次 `rebuild-summary`，WAL 从不裁剪，
  补得回来。
- 不做多租户计费出账、发票、税；
- 不引入外部数据库或 OLAP 引擎（违反单进程 / 单数据目录不变量）。

## 三、必须先裁决的决策

推荐项是我的判断，不是既定结论。D1–D5 是第一版就有的，D6–D9 是评审暴露出来、**必须在 S0 之前
定**的四项。

### D1 · 放在哪个入口

- **D1-a（推荐）**：留在「用量与调用」，页面内分两个标签页——「汇总」（默认）与「调用明细」
  （现有表格）。理由：两者是同一个问题的两层（花了多少 → 谁花的 → 具体哪一次），并且从汇总
  行**跨路由**进入明细的能力今天就有（`UsagePage` 在挂载时从 URL 读 `model/provider_id/
  project_id/request_id/provider_model`，`web/src/pages/UsagePage.tsx:36-41`）。
  > 注意：这只在**整页进入**时成立。同页两个标签页之间切换不会重新读 URL（那六个筛选只在
  > `useState` 初始化器里读一次，`navigation.tsx` 的 `usePathname` 只跟 pathname，`App.tsx:143`
  > 的 `key={path}` 也只用 pathname），实现要求见 §六。
- **D1-b**：新增顶级菜单「用量报表」。侧栏已有 11 项（`web/src/Layout.tsx:11-21`），再加一个和
  「用量与调用」在运维心里同义的入口，只会让人犹豫点哪个。等对账成为独立角色的日常工作、
  它有了自己的账期、导出与权限时，再提为顶级菜单。
- **D1-c**：在运行总览上加时间粒度切换。**不推荐**：总览是实时脉搏（15 秒轮询、需要关注队列、
  P95 告警信号），切到「年」之后同一组卡片变成对账数字，一个页面回答两个问题、两种时效契约。

**推荐 D1-a**，并在运行总览的 7 天图右上角放一个「查看完整用量 →」链接，把想看长周期的人
引过去，而不是把长周期塞进来。

### D2 · 汇总数据从哪里来

- **D2-a（推荐）**：新建**按账期日的持久 rollup**（bbolt），随 Ledger 增量维护，可全量重建。
- **D2-b**：扩大内存 `usage.Aggregate` 的窗口。**不可行**：明细只增不裁——唯一的追加点是
  `internal/usage/aggregate.go:328`，另一处整体恢复在 `:211`，全文件**没有任何截断**；checkpoint
  把全部明细序列化进 JSON（`aggregate.go:155`）。年尺度会让它和它的 checkpoint 一起线性膨胀。
- **D2-c**：查询时现读 Parquet 分区。**不可行**：分区在 `retention_days` 之后被裁掉
  （`internal/usage/parquet.go:333` `PruneBefore`，裁剪判断在 `:346`），年视图会**静默**缺数据；
  而且读路径今天不存在，每次查询扫全年分区的延迟也不适合交互式页面。

**推荐 D2-a**。Ledger WAL 从不裁剪（`internal/ledger/log.go` 全文只有 `:328` 修复残帧的
`Truncate`），所以任何时候都能重放出完整历史，这是 rollup 敢当纯派生物的前提。

### D3 · 月 / 年的边界怎么定

- **D3-a（推荐）**：月 = 若干个**账期日**的并集，日键取事件上的 `PeriodID`。
- **D3-b**：按 UTC 月截断。会与总览口径分叉（见 §一.1），且与 Parquet 的 UTC 分区一致也没有
  价值——Parquet 本身就与总览不一致。

**推荐 D3-a**。附带规则：`PeriodID` 只是本地日期标签，跨时区版本不可比，所以 rollup 的键必须
是 `(PeriodID, TimezoneVersion)`；月视图在**包含时区切换**的月份上要显式标注「本月含一次账期
时区变更（vN → vN+1）」，而不是把两代日期悄悄相加（`internal/domain/accounting_settings.go:23`
的 `TimezoneVersion` 就是为此存在的）。

### D4 · rollup 的保留期

- **D4-a（推荐）**：rollup **不设保留期**（或独立配置、默认保留全部）。它与 Parquet 明细的
  `retention_days` 是两件事：明细为了合规与体积被裁，汇总为了长周期趋势被留。
- **D4-b**：与 `retention_days` 对齐。等于让年视图默认只有 90 天，功能自废。

**推荐 D4-a**。两点必须写进文档与 UI，否则会承诺一个不存在的约束：

- **`retention_days` 今天不影响明细可查性**。明细读路径根本不碰 Parquet：
  `internal/app/admin_usage.go:434` → `internal/usage/query.go:70-95` 直接倒序遍历内存
  `a.attempts`；`Usage.RetentionDays` 的唯一非测试消费者是运维手动跑的离线
  `halro usage prune`（`cmd/halro/main.go:681` → `parquet.go:333`），只删 manifest 分区。
  所以本方案**不做**「超出保留期禁用下钻」——那会在实际仍可查的行上做出一个假的禁用态。
  真要给下钻设边界，边界是「aggregate 当前持有的最早 attempt」，与 `retention_days` 无关，
  且只有 R1 真的引入裁剪后才会移动；那是 S3 的下游功能。
- **rollup 计入 metadata.db，因而计入每一份离线备份归档**（`internal/app/backup.go:642`）。
  规模阈值见 §九 R2 与 §十。

### D5 · 成本口径

沿用运行总览已有的口径，不新造：**已知成本**、**估算成本**、**未知计费尝试数**分别累加、
分别展示（`web/src/pages/DashboardPage.tsx:40-47`）。成本在结算时已固化，报表只做求和，
不做重新定价。

口径细节（`internal/usage/query.go:219-236`）：`CostMicrosUSD` 是**结算成本合计，已经包含估算
部分**；`EstimatedCostMicrosUSD` 是其中来自估算的**子集**，不是并列的第二列；令牌侧同构。
前端是减法展示（`DashboardPage.tsx:41-46`、`trend.ts:63`）。未知计费的判据是
`CommittedMicrosUSD == nil`（由 `LeaseModeUnknownAllowed` 决定，`manager.go:637-641`），
**不得**改用 `domain.CostValueStatus` —— 那是另一个谓词，会与总览分叉。

### D6 · `Dashboard.Today` 是否改为按 `PeriodID` 归集 — **裁决：D6-a**

见 §一.2。今天两条路径的日归属不同：rollup 按受理时裁定的 `PeriodID`，总览按 `CompletedAt`
落窗。

- **D6-a**：就地把 `Dashboard.Today` 也改成按 `PeriodID` 归集——`query.go:189/215` 的
  `period.Contains(...)` 换成 `PeriodID == period.ID && TimezoneVersion == period.TimezoneVersion`
  （`RequestSummary` 同样要带账期字段，见 §4.3）。此后「rollup 当天 == Dashboard.Today」才是
  真命题。代价：**它会改变运维每天看的那个数字**，且必须列进 S0 范围。
- **D6-b**：两种日归属长期并存。S0 验收标准改写成差额可归因：
  `rollup(D) = Dashboard.Today(D) − 今日结算但属昨日账期 + 昨日结算但属今日账期`，
  测试显式构造这批跨界请求并断言其条数。

**裁决 D6-a**：一个实例只该有一种「日」。归集口径的修改列入 S0，发布说明里必须写明「运行总览
『今天』的数字口径已改为按受理账期归集，跨日请求的归属会与旧版不同」。

### D7 · 一维边际聚合，还是二维交叉行 — **裁决：D7-a**

§4.1 的键每行只承载**一个维度的一个取值**，是边际聚合、不含交叉项。因此
`group_by=requested_model&project_id=X`（某项目下的模型分布）**没有任何一行能回答**。而明细页
的六个筛选是可同时生效的（`UsagePage.tsx:36-41` → `query.go:82-89` 逐项 AND）。

- **D7-a（推荐）**：只做一维。API 把「分组」与「维度筛选」定义为**互斥**两种形态，汇总页的
  筛选条明确弱于明细页（且没有 `status`——`status` 是明细页现成的筛选项 `query.go:87`，
  rollup 维度里不存在它）。
- **D7-b**：显式增加二维行（`dimension=project+requested_model`，键用 `\x00` 连接）。回答得了
  交叉问题，代价是基数相乘，必须同时调整 R2 的上限。

**裁决 D7-a**：只做一维。「某项目下的模型分布」这类交叉问题走下钻到明细回答，不在汇总层解决。
日后要加二维行，键形状已经预留，跑一次 `rebuild-summary` 即可。

### D8 · 长尾折叠规则 — **裁决：D8-d（a 在实施中被推翻）**

§九 R2 要在写入侧把长尾聚成 `__other__`。但增量落盘由 checkpoint tick 驱动（默认 1 分钟，
`internal/config/config.go:668`），一天上千次；而 `rebuild-summary` 是一次性全量重放。若按 tick
逐段折叠，某个键一旦被折进 `__other__` 身份就不可逆丢失，与 S0 的「重建等价」直接冲突。

- **D8-a（推荐）**：**日关闭后一次折叠**。当天保持全量键，`PeriodID` 所属账期结束后（或首次
  被查询到该日已关闭时）执行一次确定性折叠：按 `(指标降序, key 升序)` 取前 N，其余合并。
  增量与重建都对同一个已关闭日执行同一函数，等价性成立。
- **D8-b**：确定性阈值折叠（低于某绝对阈值即入 `__other__`），无需等日关闭，但阈值选择会随
  流量分布漂移，且当天与次日的行为不同。
- **D8-c**：不折叠，仅在读路径截断。基数风险全留在存储侧（见 R2 的体量测算）。

**D8-a 不成立（实施中发现）**：它假设「日关闭后不再有该日的增量」。但 D6-a 恰恰保留了相反的
事实——账期在**受理时**裁定，23:59 受理、00:02 结算的请求会在 D 日**关闭之后**才落进 D 日。
于是「关闭后折一次」会被后续到达的迟到行破坏：再折一次得到的分区与 `rebuild-summary` 单遍
折叠的分区不一定相同（一个曾被折进 `__other__` 的键，可能因迟到增量重新进入 Top-N）。
这不是实现细节，是规则本身的洞。

**替代选项**（S0 未实现任何一种，当前存储不设上限）：

- **D8-d（新，推荐）· 账本顺序前 N**：每个 `(日, 维度)` 保留**账本顺序**最先出现的 N 个键，
  其余一律进 `__other__`。增量与重建都按账本顺序处理事件，结果逐字节相同，**不需要判断日是否
  关闭**。代价：留下的是最早出现的键而非最大的键（大流量键通常也早出现，但不保证）。
  实现要点：行上需带 `FirstSequence`（该键首次出现的账本序号），落盘时按它排序准入，
  否则不同的 tick 批次会产生不同的准入顺序。
- **D8-e · 只在读路径截断**：存储不折叠，API 按指标取 Top-N 并把其余合成 `__other__`（S0 的
  `/usage/summary` 已经这样做了）。重建等价天然成立，但基数风险全留在存储侧（见 R2）。
- **D8-b/c** 同前。

**裁决 D8-d**：每个 `(账期日, 维度)` 保留**账本顺序**最先出现的 200 个键
（`domain.MaxRollupKeysPerDimension`），其余合并进 `__other__`。行上带 `FirstSequence`（该键首次
出现的账本序号），落盘时按它排序准入——**不按键名，也不按落盘批次**：一天可能分成上千个增量写
入，而重建是一遍写完，只有按账本序号准入才能让两者得出同一批键。已验证：把排序换成键名后，
分批与单遍的结果立刻分叉（`TestUsageRollupCapIsIndependentOfIncrementBatching`）。

代价说清楚：留下的是**最早出现**的键，不是最大的键。大流量键通常也出现得早，但没有保证；真正
关心「谁最贵」的问题由读路径回答——`/usage/summary` 按成本降序取 Top-N，并把其余（含存储层已折
叠的那一行）合成一行 `__other__`，所以页面上的行始终加得回总量。

### D9 · 请求级指标的维度范围 — **裁决：D9-a**

`Requests / RequestErrors` 只来自 `EventRequestFinalized`，该事件只带 RequestID / ProjectID /
KeyID / RequestedModel（`internal/budget/manager.go:767-773`）。`provider` / `deployment` /
`provider_model` 三个维度的**请求级**计数没有来源（一个请求可跨 provider 重试/降级）。

- **D9-a（推荐）**：`Requests/RequestErrors` 只存在于 `dimension ∈ {total, project,
  requested_model}` 的行；其余维度只有尝试级指标，UI 在这些维度下把成功率标注为**尝试成功率**。
- **D9-b**：全维度统一只报尝试级成功率，请求数只在总量卡片上出现。口径最简单，但与运行总览
  的「完成请求 / 请求成功率」措辞不一致。

**裁决 D9-a**：请求级指标只在 `{total, project, requested_model}` 上有值，其余维度只有尝试级
指标，UI 在这些维度下把成功率标注为「尝试成功率」。API 在无请求级数据的维度上**不返回**
`requests/request_errors` 字段，而不是返回 0——0 会被读成「没有请求」。

### 裁决一览（2026-08-30）

| # | 决策 | 结论 | 对 S0 的影响 |
|---|---|---|---|
| D6 | Today 是否改按 `PeriodID` | **a · 改** | S0 含 `Dashboard.Today` 归集口径修改；总览当天数字会变，需写进发布说明 |
| D7 | 一维 / 二维 | **a · 一维** | API 的分组与维度筛选互斥；交叉问题走下钻 |
| D8 | 长尾折叠规则 | **d · 账本顺序前 200** | 存储层按 `FirstSequence` 准入；读路径另按成本 Top-N |
| D9 | 请求级指标的维度范围 | **a · 限三个维度** | 其余维度不返回 `requests` 字段，UI 标「尝试成功率」 |

## 四、数据模型

### 4.1 存储

bbolt 新 bucket `usage_daily_rollup`。落地要点：

- 新增 migration 33（接在 `internal/store/bolt/store.go:839` 的 `{version: 32,
  name: "structured_output_capability_split"}` 之后），用 `tx.CreateBucketIfNotExists` 并配
  `before_/after_` 两个 step（形状对齐迁移 5，`store.go:145-155`）；
- 把 `bucketUsageDailyRollup` 登记进 `requiredBuckets()`（`store.go:1735`）——迁移后的存在性断言
  只遍历这张表（`store.go:1670-1674`），不登记就不受覆盖；
- `schemaVersion` 32 → 33（`store.go:24`）。`migration_names_test.go:17-33` 会校验最高版本号与
  迁移条数同步（该测试只冻结 version 6 的名字，「不复用旧名称」是仓库约定，不由它强制）。

键（分隔符用 `\x00`，**不能用 `/`**：`provider_model` 合法地含 `/`——Gemini 的 `models/...`、
Bedrock inference-profile ARN，域模型只校验非空去空格，`internal/domain/models.go:926/1033`）：

```
<period_id> \x00 <timezone_version> \x00 <dimension> \x00 <dimension_key>
例：2026-08-30 \x00 2 \x00 project \x00 prj_01H...
    2026-08-30 \x00 2 \x00 total   \x00 -
```

`dimension ∈ {total, project, requested_model, provider, deployment, provider_model}`。
`total` 单独存一行，让「只要总量」的查询不必扫全部维度行再求和（也避免求和口径与 `total` 漂移）。
前缀 Seek 不受分隔符影响；反解一律 `SplitN(key, "\x00", 4)`，`dimension_key` 取最后一段。

长尾按 D8-a 折叠：当天不折叠；账期日关闭后由 `foldClosedDay(periodID, tzVersion)` 一次性按
`(排序指标降序, key 升序)` 取前 N、其余合并为 `__other__`。已关闭日另存一个 `folded` 标记，
重复调用是幂等的——这是增量与 `rebuild-summary` 能落到同一结果的前提。

值（JSON）：

```go
type DailyRollup struct {
    Version         int    // rollup 结构版本，变更即拒读重建
    PeriodID        string
    TimezoneVersion uint64
    Timezone        string
    PeriodStartMicros, PeriodEndMicros int64  // 自描述，与 budget.Period 一致

    // 与 usage.Bucket（aggregate.go:102-119）同名同义的字段
    Requests, RequestErrors int64  // 仅 total/project/requested_model 三个维度有值，见 D9
    Attempts, Errors        int64
    InputTokens, OutputTokens int64
    EstimatedInputTokens, EstimatedOutputTokens int64  // 上一行的子集，不另加
    CostMicrosUSD           int64  // 结算成本合计（已含估算部分）
    EstimatedCostMicrosUSD  int64  // 上一行中来自估算的部分，子集不另加
    UnknownAttempts         int64
    LatencyMillis           int64  // 尝试延迟之和，对齐 Bucket

    // 来自 AttemptEvent 的 Provider* 令牌，是 Input/OutputTokens 的子集而非加项
    // （照抄 aggregate.go:52 的措辞）
    ProviderCachedInputTokens, ProviderCacheWriteInputTokens, ProviderReasoningTokens int64

    // 延迟直方图：与 usage.Metrics（aggregate.go:136-142）同构的两组
    RequestLatencyBuckets  [12]uint64
    RequestLatencyOverflow uint64  // >120s；recordLatency 今天直接丢弃（aggregate.go:493-500）
    RequestLatencySamples  int64
    RequestLatencyMillis   int64
    AttemptLatencyBuckets  [12]uint64
    AttemptLatencyOverflow uint64
    AttemptLatencySamples  int64
}
```

三处与第一版不同、且都是**写入侧的实际工作量**，不能想当然：

1. `EstimatedInputTokens / EstimatedOutputTokens / EstimatedCostMicrosUSD` 在 `Apply` 里**从不
   入 Bucket**（`aggregate.go:350-400` 只累加 Attempts/Input/Output/Cost/Unknown/Latency/Errors），
   它们是 Dashboard 查询时按 `TokensEstimated`/`CostEstimated` 现算的（`query.go:209-236`）。
   rollup 必须在写入侧自己累加。
2. 缓存/推理令牌在 `Bucket` 上**不存在**，只在 `AttemptEvent` 上且叫 `Provider*`
   （`aggregate.go:53-55`）。
3. 延迟用**直方图**而非精确分位：跨日/跨月的 P95 无法由每日 P95 相加得到。桶边界复用
   `usage.LatencyBucketsMillis`（`aggregate.go:33`），但必须自带溢出计数——`recordLatency` 对
   超过最后一个上界（120000 ms）的样本什么都不做直接返回，现有 Prometheus 输出正因如此不从桶
   求 `+Inf` 而是单独传 count（`internal/app/metrics.go:533-535`）。UI 上标为「近似 P95」，
   落入溢出计数时只报「> 120s」。

### 4.2 写入与重建

**增量累加必须发生在 `Aggregate.Apply` 内部，不能放在 `Collector`。** Collector 不是事件的唯一
入口，也看不到 Apply 的判定结果：

- 去重在 Apply 内部：重复 EventID 直接 `return nil`（`aggregate.go:262-264`），返回值与成功路径
  完全相同，Collector 无法区分。崩溃恢复会用确定性 EventID 重发（`internal/budget/manager.go:737`），
  去重窗口（`aggregate.go:16-22`）就是为此存在；
- lagging 时 Collector 整段跳过 Apply（`collector.go:56`、`:101` 的 `if !c.lagging.Load()`），
  队列溢出后出队记录被整条丢掉 → 会漏计；
- **启动重放**（`runtime.go:306` `ledgerLog.Replay(usageWatermark, usageAggregate.Apply)`）与
  `CatchUp`（`collector.go:77-84`）都直接调 Apply、完全绕过 Collector——而这两条正是 rollup 最
  需要追平的路径；
- 取样点也不一致：watermark 只在 `aggregate.go:458` 于 `a.mu` 内推进，Collector 侧的增量结构与它
  不在同一临界区。

所以：

- **增量**：脏日增量是 `usage.Aggregate` 的字段，在 `Apply` 内、事件去重之后、与 watermark 推进
  同一把锁之下累加；`Collector` 只负责调度，不参与累加。
- **落盘**：新增一个在同一次 RLock 内返回 `(watermark, checkpointPayload, rollupDelta)` 的方法；
  `PutUsageCheckpoint`（`internal/store/bolt/store_usage.go:27`，今天自己独占一个 `db.Update`）
  **就地改成**同时写 checkpoint 与 rollup 增量的单一方法，在一个 `db.Update` 内完成（pre-1.0，
  不并存两个方法）。只有持久化成功后才清空内存增量。
- **幂等**：累加只能来自事件本身，不得读取墙上时钟；重放同一段 WAL 必须得到同一结果。

**rollup 与 usage checkpoint 是同生共死的一个整体**（第一版只写了单向契约，会翻倍）：

- rollup 除数据行外另存一行 `rollup_watermark{Generation, Offset, Sequence} + Version`；
- 启动时与 usage checkpoint 的 watermark 做**相等**判定（不是 ≥）。任一被丢弃、两者不相等、
  `Version` 不匹配 → 在同一个 bbolt 事务里清空 `usage_daily_rollup` 并从零重建（或反过来调
  `DeleteUsageCheckpoint` 强制全量重放）。落实处是 `restoreUsageAggregate`
  （`internal/app/runtime.go:813-836`）那**五条**返回 `NewAggregate(), Watermark{}` 的路径：
  ErrNotFound（:815）、store 读错（:818）、`RestoreCheckpoint` 失败（:822）、信封与载荷不符
  （:827）、领先 head（:831）。其中四条今天只打一条 Warn，之后 `runtime.go:306` 从零重放整条
  WAL——盘上完整的 rollup 若不清掉就是全表二次累加，且无任何告警。
- 反向同样成立：rollup 版本不匹配而 checkpoint 仍有效时，若只从 checkpoint 的 watermark 重放
  WAL **后缀**，rollup 会被从一段残缺日志里「重建」出来，同样不报错。
- `rebuild-summary` 结束时必须把 rollup watermark 与 usage checkpoint 写成同一个值，或直接删除
  checkpoint——否则它之后的第一次启动就是上面那个翻倍场景。

**重建**：新增 `halro usage rebuild-summary`（离线）。它与 `compact/verify/prune` **不是**同一
形态，这一点第一版写错了：

- `openUsageOffline`（`internal/app/usage.go:65-98`）只返回 `(*usage.Aggregate, *usage.Exporter,
  closer, error)`，全程不打开 bbolt。写 rollup 需要另以 `boltstore.Open(cfg.MetadataPath())`
  （`store.go:1285`，会执行待跑迁移）在数据锁内取可写句柄；
- 数据锁是排他的（`usage.go:72` `lock.Acquire` → `lock_unix.go:33` `LOCK_EX|LOCK_NB`），运行中的
  实例在就直接失败——这是预期行为，运维需要先停机；
- **重建不能是流式的**（第二版修正）：rollup 与 checkpoint 必须写在同一事务、描述同一个 WAL
  位置，而 checkpoint 载荷只有完整的 `usage.Aggregate` 才产得出来。所以重建走
  `openUsageOffline` 的全量重放，与 `usage compact` 付同样的内存代价（R1）。
  只写 rollup 不写 checkpoint 是行不通的：下次启动会因为「没有 checkpoint」而清空两者重建，
  等于这次重建白做。

**地位**：与 usage checkpoint 同级的可重建加速器。删掉它不丢账（`store_usage.go:64` 的注释就是
这个契约），Ledger 仍是唯一权威。

### 4.3 `AttemptEvent` 与 `RequestSummary` 各补两个账期字段

`usage.AttemptEvent` 目前没有 `PeriodID`（投影处 `aggregate.go:306` 丢弃了它）；`RequestSummary`
（`aggregate.go:86-100`）同样没有，而 `Dashboard.Today.Requests` 正是从 `a.summaries` 累加的
（`query.go:174-197`）。两者都要补 `PeriodID` + `PeriodTimezoneVersion`——数据现成，
`manager.go:774` 已经 Stamp 过 finalized 事件，投影处（`aggregate.go:422` 附近）直接可读。

一个请求的多次 attempt 与它的 finalize 共享同一个 `PeriodID`（都继承受理时的裁定），这正是
requests 与 attempts 能落在同一行 rollup 上的前提。

`checkpointVersion` 7 → 8（`aggregate.go:29`）。

> **是否需要重新初始化数据目录：不需要。** checkpoint 版本不匹配会被拒并从 Ledger 重放重建
> （已有行为），代价是一次较慢的启动；bbolt 走正常迁移。注意这次升级本身**不会**触发 §4.2 的
> 翻倍场景——迁移 33 刚建出空 bucket，从零重放不会重复计数；翻倍的暴露面是**此后**任何一次
> checkpoint 丢弃/损坏。Parquet 既有分区不重写（ADR 0017 的冻结约定），报表也不读它。

## 五、Admin API

已实现于 `internal/app/admin_usage_summary.go`：

```
GET /admin/api/v1/usage/summary
  ?granularity=day|month|year               // 默认 day
  &start=2026-01-01&end=2026-08-31          // 账期日期标签，闭区间（含 8/31）
                                            // 省略时：day 取最近 30 天，month 取最近 12 个月，
                                            // year 取最近 3 年，均以当前账期日为末端

  // 以下两组互斥（D7-a），且各自最多一个：
  &group_by=project|requested_model|provider|deployment|provider_model
  &project_id= | &provider_id= | &deployment_id= | &model= | &provider_model=

  &limit=50                                  // groups 条数，默认 50，上限 100
```

响应：

```json
{
  "granularity": "month",
  "start": "2025-09-01", "end": "2026-08-31",
  "totals":  { "requests": 12, "attempts": 15, "cost_micros_usd": 1234, "...": "见下" },
  "buckets": [{ "period": "2026-08", "...": "同 totals" }],
  "group_by": "project",
  "groups":  [{ "key": "prj_..." }, { "key": "__other__" }],
  "groups_truncated": true, "groups_other_count": 7,
  "filter": { "dimension": "provider", "value": "prov_..." },
  "timezone_changes": [{ "period_id": "2026-05-04", "from_version": 1, "to_version": 2 }],
  "resource_labels": { "prj_...": "结算服务" },
  "watermark_sequence": 8123,
  "time_context": { "...": "既有信封" }
}
```

指标字段与 `usage.Bucket` 同名；`requests` / `request_errors` / `request_latency_*` 在非请求级
维度上**不出现**（D9-a）。延迟一律是 `*_p95_millis` 加 `latency_approximate: true`；样本落在
最后一个桶之上时另给 `*_latency_over_max` 计数，前端据此显示「> 120s」而不是一个数。

要点：

- **互斥语义**：选了 `group_by` 就不能再带维度筛选，反之亦然（rollup 是一维边际聚合，不存交叉
  项）。汇总页的筛选条因此明确弱于明细页，且**没有 `status`**（D7-a）。
- **新鲜度**：与 `/dashboard`、`/usage` 一致，响应前先 `syncUsageAdmin`（`admin_usage.go:637-642`
  → `CatchUp`）。rollup 只随 checkpoint tick 落盘（最多滞后 1 分钟），所以返回值是「已落盘
  rollup + 尚未落盘的内存脏日增量」的合并，且与落盘用**同一个累加函数**，不引入第二套逻辑。
  响应带 `watermark_sequence`；不另给「汇总到哪」——rollup 与 checkpoint 共用同一个 watermark，
  第二个位置字段只会制造它们可以不一致的错觉。
- **区间语义**：汇总接口是**日期标签、右闭**；明细接口是 RFC3339 瞬时、右开
  （`admin_usage.go:422-429` + `query.go:89`，前端 `UsagePage.tsx:52-53` `zonedInputToISO`）。
  前端下钻必须用 `buckets[].start/end` 的绝对边界构造明细参数，**不得**拿日期标签硬拼。
- **时区**：响应带既有的 `time_context` 信封（`internal/app/time_context.go:56` `writeTimeContext`），
  不要自造裸 `timezone` 字段——`time_context_test.go:44-47` 遍历 `/dashboard`、`/usage`、
  `/system/status` 断言这套字段，前端全局采纳（`App.tsx:88` → `timezone.ts:69`）。
  `timezone_changes` 另外给（`time_context` 只描述此刻）。
- **标签**：删除的项目/部署仍有历史，用既有的 `resource_labels: {id: name}` 映射
  （`admin_usage.go:61`，`types.ts:200`，消费点 `DashboardPage.tsx:56`），缺席即已删除。
  不要新造 `label_available` 布尔——那会成为第三种标签约定。
- **groups 的截断语义**：服务端按成本降序取前 `limit`，**剩余项在服务端聚成一行
  `__other__`**，保证 `Σgroups == totals`；返回 `groups_truncated` / `groups_other_count`。
  不要留给前端截断
  （本仓所有列表读接口都有上限：`admin_usage.go:399` 默认 50、`query.go:71` 上限 100）。
- **请求级字段**：`requests` / `request_errors` 只在 `group_by ∈ {project, requested_model}` 与
  总量上返回；其余维度的 group 对象**不含**这两个键（D9-a），不要返回 0。
- **延迟**：桶合并与近似分位在**服务端**完成，响应按 Bucket 现有字段名给出
  `request_latency_samples` / `request_latency_p95_millis` 并附近似标记与溢出计数，不要把 12 个
  原始桶发到浏览器。
- **权限**：注册在 `internal/app/runtime.go:1414` 旁，`requireAdmin`；纯 GET 无副作用，只读管理员
  可读；错误走 `admin_errors.go` 既有约定。
- **区间上限**：年视图最多 24 个月；超出返回 400 而不是静默截断。

**前置改动（S0 必须一起做，否则 `deployment` 维度没有下钻落点）**：明细端点是参数白名单，未知键
直接 400（`admin_usage.go:380-388`），`usage.AttemptQuery`（`query.go:11-21`）无 `DeploymentID`，
过滤链（`query.go:81-89`）也没有。要补：`AttemptQuery` 加字段 → `QueryAttempts` 过滤链加一条 →
白名单加 `deployment_id` → `UsagePage` 从 URL 读并回填。group_by 值到明细参数名的映射：
`requested_model` → `model=`、`provider_model` → `provider_model=`、`deployment` → `deployment_id=`。

## 六、控制台

「用量与调用」页面结构：

```
用量与调用
├─ [汇总]  ← 默认
│   粒度：日 / 月 / 年 / 自定义      对比：上一期（可关）
│   KPI：调用数 · 成功率 · 令牌（报告/估算） · 已知成本（估算部分与未知计费另行标注）
│   趋势图：月视图按天、年视图按月，指标切换
│   维度表：项目 / 公开模型 / 服务商 / 部署，可排序，行内「查看明细 →」
│   导出 CSV（当前筛选与区间）
└─ [调用明细]  ← 现有表格与筛选条，原样保留
```

实现要点：

- **趋势图不是「加个参数」**。`web/src/trend.ts` 的窗口永远以 `Date.now()` 结尾（`:23-24`，画不了
  过去的某个月）；`:35` 用固定秒步长生成 x 轴（月不是固定秒数，账期日在夏令时也不是）；`:29` 按
  UTC 纪元对齐桶（换成 86400 就是 UTC 午夜，正好把 §一.1 的分叉搬回图表），而图轴按账期时区渲染
  （`TrendChart.tsx` 的 `uPlot.tzDate`）；`:27/:105` 硬读 `bucket.hour`。现有单测钉死这套形状
  （`trend.test.ts:9-17`）。改法：`buildTrendSeries` 改为接收服务端返回的 `buckets[]`（每桶自带
  `start/end`），x 轴取桶边界本身、不再步长生成，窗口取 `[buckets[0].start, 末桶.end]`；桶的时间
  字段从 `hour` **就地改**成通用名（不并存两个字段）；`summarizeTrend` 去掉 now 锚定。
- **默认落在哪个标签页**（已实现）：URL 显式带 `tab=` 就听它的；否则只要 URL 带了任何一个明细
  筛选（`request_id` / `project_id` / `model` / `provider_id` / `provider_model` / `deployment_id` /
  `status` / `start` / `end`）就落在「调用明细」，其余情况落在「汇总」。这样一条下钻链接不会
  把筛选丢在一个它没有应用的页面上，而空手进来的人先看到的是「花了多少」。
- **标签页与下钻**。`UsagePage` 的六个筛选只在 `useState` 初始化器里读一次 URL，且
  `navigation.tsx` 的 `usePathname` 只跟 pathname、`App.tsx:143` 的 `key={path}` 也是——同路径不同
  query 不会重渲染，`navigate()` 对同页 tab 切换无效。两条可选路径：把 tab 与筛选全部提升为
  `UsagePage` 内的普通 state 直接联动；或按 `PoliciesPage.tsx:76-87` 的既有模式（本地 state +
  pushState + popstate 同步）。无论哪条，浏览器前进/后退都要能在两个标签页间回退。
- **指标切换的 tab 组件**。`DashboardTabs` 未导出（`DashboardPage.tsx:206`），样式 `.dashboard-tabs`
  在全局表 `styles.css:196-203`，且 DashboardPage 是懒加载的（`App.tsx:23`）——直接 import 会把
  总览页 chunk 拖进用量页。要用就提取到 `design-system` 或 `components.tsx`，不要跨页 import。
- **CSV 在前端生成**：`web/src/api.ts:136-144` 对所有响应体一律 `JSON.parse`，服务端返回
  `text/csv` 会被当成错误抛掉。复用 `MFASettings.tsx:96` 的 Blob + `createObjectURL` +
  `<a download>` 模式（当前 CSP `runtime.go:1518-1521` 下已在用），不新增端点、不改 `api.ts` 的
  解析约定。导出范围即当前响应范围；若 groups 被截断，CSV 必须带上 `__other__` 行。
- 文案按运维语言，不用实现术语：「按月看用量」而不是「rollup 聚合」；「近似 P95」标注清楚；
  非请求级维度（provider / deployment / provider_model）的成功率标注为「尝试成功率」（D9-a）。
- 空态保留表格骨架与行标签，解释一句为什么是空的，不整块替换成插图。
- i18n：zh-CN 与 en 同步补 `usage.summary.*` 键。

## 七、分期

| 阶段 | 内容 | 完成标准 |
|---|---|---|
| **S0（已完成，未提交）** | rollup 存储 + 迁移 33（含 `requiredBuckets` 登记）+ `AttemptEvent`/`RequestSummary` 补账期字段 + `Dashboard.Today` 改按 `PeriodID` 归集 + Apply 内增量 + checkpoint/rollup 单事务落盘 + 双向拒读重建 + `rebuild-summary` + 明细端点 `deployment_id` + `GET /usage/summary`（日/月/年）+ 每维度键上限（D8-d） | 见 §八 前四条，均已有测试并做过逆向验证 |
| **S1（已完成，未提交）** | 「汇总」标签页：KPI、趋势图（`buildTrendSeries` 已按桶边界重构）、维度表、下钻联动 | 已实现并有测试：下钻链接带 `tab=attempts` + 维度筛选 + **绝对时间边界**，明细页从 URL 还原为账期时区的本地输入 |
| **S2（部分完成）** | 年视图 ✓、CSV 导出 ✓、时区变更提示 ✓、24 个月上限 ✓；**未做**：环比上一期 | CSV 由前端从当前响应生成（含 `__other__` 行），不新增端点 |
| **S3（另议）** | 内存 aggregate 的裁剪策略（见 R1），以及在其之上才谈得上的「明细可查下界」 | 单独提案，不与本功能同期改 |

## 八、验证

按 `AGENTS.md` 的范围规则；下面是本功能**必须**有的证据。

- **交叉校验**：同一份 Ledger，`Dashboard.Today` 与该日 rollup 行在**明确列出的可比字段集合**上
  相等（`Requests/RequestErrors/Attempts/Errors/Input/OutputTokens/Estimated*/CostMicrosUSD/
  UnknownAttempts/LatencyMillis`；延迟分位不在其中，它在 rollup 里是直方图）。断言前先
  `CatchUp` 再强制落一次 checkpoint，否则测试与 tick 时序相关。D6-a 之后这是**严格相等**，
  并另有一条专门的跨界用例：23:59 受理、次日 00:02 结算的请求，rollup 与总览必须都记在受理日。
- **重建等价性**：增量写出的 rollup 与 `rebuild-summary` 全量重放结果完全一致，覆盖：中途
  checkpoint、进程重启、collector 队列溢出后 `CatchUp` 补齐（并断言 `CatchUp` 之后 rollup 与
  `aggregate.totals` 相等）、以及 `foldClosedDay` 折叠后的已关闭日（D8-a：对同一个已关闭日，
  增量路径与全量重建必须落到同一批 `__other__` 成员，含并列值的 tie-break）。
- **双向拒读**（§4.2 的两个反向）：① 删除或写坏 usage checkpoint 后重启，rollup **不得翻倍**；
  ② 人为把 rollup `Version` 改旧后启动，结果必须等于**全量**重放而非后缀重放；
  ③ `rebuild-summary` 之后的第一次启动，数字不变。
- **账期边界**：跨日请求按其 `PeriodID` 归集（受理侧，非完成侧）；时区切换后同一天的两代版本不
  合并；Samoa 式跳过的日期不产生空洞行（`internal/budget/period.go:151` 已处理，rollup 不得重新
  推导）。
- **成本口径**：`Estimated*` ≤ 对应总量，且求和后与 `Dashboard.Today` 同名字段相等；未知计费判据
  是 `CommittedMicrosUSD == nil`；含 `unknown_attempts` 的月份在 UI 上有标注。
- **延迟**：P95 落在某桶内时偏差不超过该桶宽；样本落入溢出计数时只报「> 120s」。对照基准是
  `query.go:328-336` 的请求级精确 P95。
- **前端**：`trend.ts` 重构后的单测（天桶、月桶、跨月边界、**夏令时切换所在的天桶**、**查询一个
  已过去的月份**）+ 汇总页组件测试 + tab/下钻的 popstate 行为。
- **真机（按顺序）**：运行中记下 dashboard 当天数字与 watermark → 停机 → `halro usage
  rebuild-summary` → 重启 → 比对 → `halro doctor`。确认迁移 33 不阻止既有实例启动。
- **门禁**：`make check`（`Makefile:170` = fmt-check test race vet frontend-test
  observability-check）**不含** typecheck 与 frontend build（`frontend-test` 只有 `npm test`，
  `Makefile:145-146`），所以推送前必须另跑 `cd web && npm run typecheck && npm run build`、提交
  `internal/webui/dist`、再 `git diff --exit-code -- internal/webui/dist`。
  迭代期范围：S0 纯 Go 跑 `go test -count=1 ./internal/usage/ ./internal/store/bolt/
  ./internal/app/` 加 `go test -race ./internal/usage/`（新增了与 Collector 共享的状态）；
  S1/S2 先跑直接相关的 vitest 文件。
- **登记项**：新端点要同步登记 `time_context_test.go:44`、`content_canary_test.go:219-222`、
  `secret_canary_test.go:151`。

## 九、风险

- **R1 · 内存 aggregate 无界增长**：明细只增不裁（`aggregate.go:328` 唯一追加点，无截断），
  checkpoint 是全量 JSON。本功能不加重它，但会让长期运行的实例第一次被人认真审视，且
  `rebuild-summary` 若走 `aggregate.Apply` 全量重放会直接撞上它（所以 §4.2 要求流式重建）。
  **不在本期修**，在 S0 的 PR 描述里点名。
- **R2 · 维度基数与体量**：`provider_model × 天` 基数最高。两个量级要分别算清：
  *典型安装*（每天每维度几十个键，5 个非 total 维度 ≈ 百余行/天 ≈ 数万行/年，单行数百字节 →
  年级别十几 MB）与 *上限口径*（每天每维度 500 键 ≈ 2500 行/天 ≈ 91 万行/年 → 数百 MB）。
  第一版写的「年尺度只有几万行 / 每天几十 KB」只对典型安装成立，与 500 的上限不自洽——上限应
  下调到与之自洽的量级（**已定为每天每维度 200**，`domain.MaxRollupKeysPerDimension`），
  配 D8-d 的账本顺序准入。按此上限，最坏情况约 1000 行/天、36 万行/年。
  另外 rollup 计入 metadata.db，因而**计入每一份离线备份归档**（`backup.go:642`）。
- **R3 · 口径解释**：Parquet 按 UTC 日、报表按账期日，二者本来就不同。`docs/guides/` 要有一句话
  说明，避免有人拿 Parquet 文件数量质疑月报。
- **R4 · 双真相源的诱惑**：一旦报表好用，就会有人想直接从 rollup 出账。代码注释与 API 响应里要
  保持「派生物」定位，账务结论仍以 Ledger 为准。
- **R5 · 落盘路径的并发**：增量移进 `Apply` 意味着 rollup 状态与 aggregate 共享 `a.mu`，
  且 `PutUsageCheckpoint` 变成写两类数据的单一事务。这条路径同时被 Collector、`CatchUp`、
  启动重放和维护 tick 触碰——`go test -race ./internal/usage/ ./internal/app/` 是必须项，不是可选。

## 十、附录 · 为什么不引入独立报表数据库

单进程、单数据目录、独占锁是 v1 的一致性边界（ADR 0001）。引入第二个存储引擎会立刻带来第二套
备份、第二套一致性窗口与第二个失败域，而本功能的数据量（天 × 维度）在典型安装下用 bbolt 一个
bucket 就装得下。规模真的越界时（R2 的上限量级、或备份归档因它明显变大），正确的下一步是把
rollup 落到 Parquet 侧的汇总分区并复用既有 manifest 校验，而不是引入外部 DB。
