# Heimdall 时区治理与周期身份版本化 PRD

- 状态：Implemented — §4 目标 1–7 已实现并通过自动化验收
- 范围变更：项目级会计时区与管理员级显示时区均已移出范围，见 §5 第 5、9 条
- 目标版本：metadata schema v16 / Ledger WAL v3 / Admin API v1
- 日期：2026-08-06
- 文档语言：中文
- 适用范围：日预算周期、Ledger 事件、Usage 聚合、Dashboard、Parquet 保留期、Admin Console 时间显示、构建产物

## 1. 文档定位

本 PRD 定义 Heimdall 从"单一静态配置时区"升级为"会计时区受治理、周期身份自描述且版本化"的产品与工程要求。

Heimdall 是面向国际化部署的 AI Gateway。它的日预算（`Daily Budget`）按自然日执行，Usage 与 Dashboard 按自然日汇总。"自然日"是一个必须被精确定义、可审计、且不会因配置变更而被追溯重解释的概念。当前实现把这个概念寄托在一个启动期读取的 YAML 字段上，既缺少版本化，也缺少与展示层的契约。

本升级不改变成本计量原则（Provider 返回的 Token Usage 优先），不改变价格时间线的 UTC 语义，也不引入用户级时区。它解决的是以下问题：

1. 会计口径（周期边界）与显示口径（UI 渲染）当前由同一个字段承担，两者的变更风险、审计要求和生效语义完全不同；
2. `PeriodID` 是一个裸日期字符串，不携带解析它所需的时区信息，因此历史周期在时区变更后含义会静默漂移；
3. 时区解析依赖运行环境提供的 zoneinfo，跨节点 tzdata 版本差异可能导致同一时刻落入不同周期；
4. Admin Console 完全不知道服务端会计时区，图表刻度与"今天"指标使用两套日边界；
5. 保留期裁剪使用 UTC 日切，与会计日切不一致，且未在文档中形成契约。

相关现有边界：

- `usage.timezone` 定义于 `internal/config/config.go:84`，默认值 `UTC`（`internal/config/default.yaml:71`），示例文件写 `Asia/Shanghai`（`configs/config.example.yaml:41`）；
- Runtime 在启动时调用 `time.LoadLocation` 并在失败时拒绝启动（`internal/app/runtime.go:250`），该 `*time.Location` 注入 `budget.Manager` 与 Usage 查询；
- 日预算周期由 `PeriodID: now.Format("2006-01-02")` 派生（`internal/budget/manager.go:443`），余额键为 `(projectID, periodID)`（`internal/ledger/event.go:526`）；
- Dashboard 的"今天"由 `DashboardForBasis` 按 `now.In(location)` 的 `Year + YearDay` 归集（`internal/usage/query.go:182`）；
- Parquet 分区日期与保留期裁剪固定使用 UTC（`internal/usage/parquet.go:223`、`internal/usage/parquet.go:500`、`cmd/heimdall/main.go:613`）；
- 价格时间线明确不使用 `usage.timezone`（见 `docs/prd/prd-versioned-model-pricing.zh-CN.md` §6.4），TOTP 校验明确只用 UTC（见 `docs/prd/prd-authenticator-totp.zh-CN.md` §9.3）；
- Admin Console 的实例级设置已有 `InstanceUISettings` 领域对象与 `/admin/api/v1/settings/ui` 接口，管理员级偏好已有 `Locale` 与 `Appearance`（`internal/domain/admin.go:11`）。

## 2. 问题陈述

### 2.1 一个字段承担两个语义

`usage.timezone` 当前同时决定：

- 日预算周期边界与结算归属（会计语义，写入 Ledger，不可追溯变更）；
- Dashboard"今天"指标的聚合窗口（展示语义，可随时切换视角）。

这两者的变更风险相差数个量级。修改会计时区会改变预算何时重置、一次调用归属哪一天；修改显示口径只是换一个观察角度。把它们绑定在同一个需要重启才生效的静态配置上，导致：会计侧拿不到它必须具备的版本化、审计与生效时点控制，显示侧拿不到它应有的灵活性。

### 2.2 `PeriodID` 不自描述

当前周期标识是裸日期字符串 `"2026-08-06"`。同一个字符串在 `UTC` 与 `Asia/Shanghai` 下代表两个不同的 UTC 区间。Ledger 中的历史事件不记录当时使用的时区，因此：

- 时区变更后，历史周期的含义被静默重解释，审计人员无法从事件本身还原当时的日边界；
- 日中变更时区时，`(projectID, periodID)` 键可能命中一个用不同边界累积出来的余额，产生重复消费或预算被意外抹平；
- 无法回答"这笔消费属于哪个 UTC 区间"这一对账基本问题。

### 2.3 时区解析结果依赖运行环境

`cmd/heimdall` 未导入 `time/tzdata`，`time.LoadLocation` 依赖运行环境的 `/usr/share/zoneinfo`。Dockerfile 使用 `gcr.io/distroless/static-debian12`，该镜像携带 tzdata，但那是镜像属性而非构建产物属性。同一版本 Heimdall 在不同宿主、不同基础镜像或非容器安装下，可能解析出不同 tzdata 版本。

IANA 时区数据库每年多次发布，历史与未来的 UTC 偏移规则会被修订。两个节点若使用不同 tzdata 版本，在某个受影响的 DST 切换日会算出不同的本地日边界，进而对同一时刻生成不同 `PeriodID`。这是账本层面的分叉，且没有任何现有机制能检测到它。

### 2.4 Admin Console 与服务端使用两套日边界

`/admin/dashboard` 响应（`internal/app/admin_usage.go:36`）不包含任何时区信息。前端因此各自为政：

- `web/src/format.ts` 的 `Intl.DateTimeFormat` 未传 `timeZone`，按浏览器本地时区渲染；
- `web/src/TrendChart.tsx` 的 uPlot 配置使用 `scales.x.time = true` 且未提供 `tzDate`，X 轴刻度按浏览器本地时区绘制；
- "今天"指标卡片的数值由服务端按会计时区算出。

结果是同一个页面上，卡片总和与图表当日柱状区间不对应。管理员时区与服务端会计时区相差 8 小时时，偏差是整整 8 小时的数据。对国际化部署而言这是必然触发的缺陷，而非边缘情况。

### 2.5 保留期与会计期使用不同日切且未成契约

`retention_days` 的裁剪 cutoff 为 `time.Now().UTC().AddDate(0, 0, -RetentionDays)`，分区日期取 `CompletedAt.UTC()`。会计日切则按配置时区。两套边界共存本身可以接受（分区是存储布局而非会计口径），但当前既未在文档中声明为不变式，也未处理边界日的差一天问题：在 UTC+8 部署上，本地"90 天前"的数据可能已被按 UTC 裁剪。

## 3. 目标用户与核心场景

### 3.1 目标用户

- 在非 UTC 时区运营 Heimdall、依赖日预算控制成本的管理员；
- 需要与内部财务系统按自然日对账的平台或 FinOps 团队；
- 需要从 Ledger 独立还原周期边界的审计人员；
- 负责跨区域多节点部署、升级与账本验证的 SRE；
- 使用与服务端不同时区的分布式管理团队成员。

### 3.2 核心用户故事

1. 作为管理员，我可以在 Admin Console 中查看当前实例的会计时区，以及"今天"具体对应的 UTC 区间。
2. 作为管理员，我可以变更会计时区，并确知该变更在下一个周期开始时生效，不会影响进行中的周期或任何历史账。
3. 作为审计人员，我可以从单条 Ledger 事件独立还原该周期的时区、时区版本和 UTC 起止时刻，无需依赖当前配置。
4. 作为身处 UTC+8 的管理员，我在查看 UTC 部署的 Dashboard 时，图表刻度与"今天"卡片使用同一个日边界，且界面明确标注该边界属于哪个时区。
5. 作为 SRE，我可以确认所有实例使用同一份 tzdata 规则，并在不一致时收到告警。
6. 作为 SRE，我可以确认周期边界只由存储中的设置决定，不受运行节点所在主机环境影响。

## 4. 目标

1. 将会计时区从静态配置升级为存储中的受治理实例设置，具备版本号、审计与生效时点语义。
2. 使 Ledger 中的周期身份自描述：携带时区名、时区版本与 UTC 起止区间。
3. 使余额键包含时区版本，让时区变更铸造新周期而非污染既有周期。
4. 将 IANA 时区数据库嵌入构建产物，使时区解析成为二进制的确定性属性。
5. 在 Admin API 中引入统一的 `time_context`，使客户端永不自行推导日边界。
6. 统一 Admin Console 全部时间渲染路径，图表与指标共享同一日边界。
7. 将"分区与保留期使用 UTC"升格为书面契约，并消除保留期的差一天问题。

## 5. 非目标

1. 不引入 Gateway 调用方（终端用户）级别的时区。时区是实例级治理属性。
2. 不改变价格时间线的 UTC 生效语义，不改变 TOTP 的 UTC 校验语义。
3. 不改变成本计量原则，不改变 Token 权威来源。
4. 不改变 Parquet 分区的 UTC 布局。
5. **不支持项目级会计时区。** 全实例共用一个会计时区。跨区多租户不是本系统的目标，为它预留字段会让 Usage 聚合、报表口径和"今天"的含义都变成按项目分叉的，成本与收益不成比例。
6. 不支持固定偏移量时区（如 `UTC+08:00`）作为会计时区。只接受 IANA 名，因为固定偏移无法表达 DST，会在跨 DST 的部署中产生错误的日长度。
7. 不提供会计时区的追溯变更或历史周期重算能力。
8. 不引入 `SIGHUP` 式的 `config.yaml` 热重载机制。
9. **不提供管理员级显示时区偏好。** Console 全站时间戳一律按会计时区渲染。让每个管理员各看各的时区，会使两个人对着同一个页面读出不同的"今天"，讨论同一笔消费时说的不是同一天——这比迁就本地习惯更容易出错。
10. **不支持多个节点共享一个数据目录。** 数据目录持有独占锁（`internal/store/lock`），一个 store 只有一个写入者。跨节点一致性的唯一现实议题是各实例的 tzdata 规则是否相同，由 §12.2 的指纹比对承担。

## 6. 核心产品决策

### 6.1 会计口径与显示口径是两个独立职责

**会计时区（Accounting Timezone）**：定义日预算周期边界与结算归属。属于治理配置，存储于 metadata store，具备版本号，变更需审计，生效时点受控，历史不可重解释。

**显示口径**：Admin Console 渲染时间戳所用的时区。它不是一个独立设置，而是恒等于会计时区——见 §5 第 9 条。分离的是**职责**（一个决定账怎么算，一个决定字怎么显示），不是**取值**：显示层必须显式向服务端索取时区，绝不回退到浏览器本地时区，这样即使将来引入个人偏好，也只需换掉取值来源。

### 6.2 会计时区的权威来源是存储，不是配置文件

`config.yaml` 中的 `usage.timezone` 降级为**首次启动播种值**：仅在实例会计设置尚未存在时用于初始化。此后存储为唯一权威。

不保留双活来源。`heimdall doctor` 在配置文件值与存储值不一致时输出 `warn`，提示管理员配置文件已不生效，避免运维修改 YAML 后误以为已生效。

理由：会计时区需要版本号、审计链和生效时点，这三者都无法由无状态的配置文件承载。同时保留配置文件作为播种入口，使无人值守首次部署仍可通过配置完成初始化。

### 6.3 周期身份必须自描述

`PeriodID` 保留 `YYYY-MM-DD` 形式以维持可读性与既有兼容性，但周期的完整身份由四元组定义：

```
(PeriodID, PeriodTimezone, PeriodTimezoneVersion, [PeriodStartMicros, PeriodEndMicros))
```

区间为 UTC 半开区间。任何一条 Ledger 事件都携带完整四元组，审计人员无需访问配置或存储即可还原边界。

### 6.4 周期区间必须由日历运算得出

周期结束时刻必须通过在目标 location 上执行 `time.Date(y, m, d+1, 0, 0, 0, 0, loc)` 得出，**禁止使用 `start.Add(24 * time.Hour)`**。

理由：DST 切换日的本地自然日长度为 23 或 25 小时。日预算的产品语义是"一个自然日"，不是"24 小时"。使用固定 24 小时偏移会在 DST 日产生周期重叠或空隙，导致预算被少算或多算一小时。

对于本地午夜被跳变抹除的日期（智利、古巴在本地 00:00 起跳夏令时），`time.Date` 会**向前一日**归一化，因此**不得**直接取其结果作为周期起点——那会与前一周期重叠，同一时刻落入两个周期。周期起点定义为"该本地日期真实存在的第一个瞬间"，实现上自 `00:00` 起向后逐分钟搜索至多 240 分钟。

若该本地日期整体不存在（跨日界线移动，如萨摩亚没有 2011-12-30），跳过被抹除的标签取下一个存在的日期，至多跳过 2 个。该日本身长度正常，只是标签消失。

### 6.5 时区变更在下一个周期开始时生效

会计时区变更不影响进行中的周期。变更被记录为带生效时刻的待生效设置，在当前周期的 `PeriodEnd` 时刻切换。

理由与实现参考：系统已有"未来生效"的成熟模式（价格版本的 effective-at 与 `RecoverDeploymentPricePins`）。复用该模式，不引入第二套生效机制。

变更时 `TimezoneVersion` 自增。因为余额键包含时区版本（见 §6.6），新旧周期在账本上完全隔离。

生效判定是关于瞬间的纯函数：给定设置与 `instant` 即可确定时区与版本，不依赖任何定时任务。宕机期间错过生效时刻的实例，恢复后与一直在线的实例得出相同边界；把存储记录从"待生效"改为"已生效"的后台步骤只负责补齐审计与展示，不承担正确性。

### 6.6 余额键包含时区版本

`State.Balance` 的键从 `(projectID, periodID)` 扩展为 `(projectID, periodID, timezoneVersion)`。

理由：即使遵循 §6.5，仍存在崩溃恢复、时钟回拨、手工干预等路径可能使新旧边界的事件落在同一 `PeriodID` 上。把时区版本纳入键，使这类情况退化为"两个独立周期"而非"一个被污染的周期"。这是一个防御性不变式，不依赖流程正确性。

### 6.7 分区与保留期恒用 UTC

Parquet 分区日期、保留期裁剪 cutoff 与备份时间戳恒用 UTC，且升格为书面契约。

理由：分区是存储布局，不是会计口径。若按项目会计时区分区，同一条 Attempt 在项目时区变更后会归属两个不同分区，破坏分区不可变性与 manifest 校验。

同时修正保留期语义为"至少保留 N 天"：cutoff 取 `today_UTC - N - 1`。多保留一天使任何时区的项目都不会少看一天，代价是最多多占用一天的存储。

### 6.8 会计时区只接受 IANA 名

写入时校验：非空、可被 `time.LoadLocation` 解析、且不是固定偏移形式。显式拒绝 `Local`——它的含义取决于宿主环境，与 §6.2 的确定性目标冲突。

`UTC` 是合法值且为出厂默认值。

### 6.9 时区数据库嵌入构建产物

`cmd/heimdall` 导入 `_ "time/tzdata"`。构建产物自带 IANA 数据库副本，`time.LoadLocation` 不再依赖运行环境。

Go 的解析顺序为：`ZONEINFO` 环境变量、系统路径、嵌入数据库。为保证确定性，Runtime 启动时显式记录实际使用的来源与版本，并在 `doctor` 中输出。生产部署应确保未设置 `ZONEINFO`。

代价约 450 KB 二进制体积，换取消除一整类跨实例账本分叉。

除来源与版本外，Runtime 另计算一份**转换表指纹**：对既定时间窗内一组固定时区加上当前会计时区的全部偏移转换点做哈希。版本号是打包者的声明，指纹是实际规则的证据；跨实例一致性比对以指纹为准。

## 7. 数据模型

### 7.1 `InstanceAccountingSettings`

新增领域对象，存储于 metadata store，遵循既有 `InstanceUISettings`（`internal/domain/ui_settings.go:26`）的形态与 `Revision` 乐观锁约定。

```go
type InstanceAccountingSettings struct {
    Timezone            string     `json:"timezone"`
    TimezoneVersion     uint64     `json:"timezone_version"`
    PendingTimezone     string     `json:"pending_timezone,omitempty"`
    PendingEffectiveAt  *time.Time `json:"pending_effective_at,omitempty"`
    UpdatedAt           time.Time  `json:"updated_at"`
    Revision            uint64     `json:"revision"`
}
```

字段语义：

| 字段 | 语义 |
| --- | --- |
| `Timezone` | 当前生效的 IANA 时区名 |
| `TimezoneVersion` | 从 1 开始，每次实际切换自增 |
| `PendingTimezone` | 已提交但尚未生效的目标时区，空表示无待生效变更 |
| `PendingEffectiveAt` | 待生效变更的 UTC 生效时刻，等于提交时当前周期的 `PeriodEnd` |
| `Revision` | 乐观锁，写入需 If-Match |

约束：

- `Timezone` 非空且通过 §6.8 校验；
- `PendingTimezone` 与 `PendingEffectiveAt` 必须同时存在或同时为空；
- `PendingEffectiveAt` 必须严格晚于提交时刻；
- 同时只允许存在一个待生效变更，新的提交覆盖旧的（覆盖动作同样记入审计）；
- 提交与当前时区相同且无待生效变更时不写入、不推进 `TimezoneVersion`：版本是余额键，为空操作自增会把同一天的余额劈成两半；
- 拒绝比当前更小的 `TimezoneVersion`，防止把两个被刻意隔离的周期重新合并。

### 7.2 周期解析器

新增 `internal/budget` 内的周期解析组件，作为周期计算的唯一入口：

```go
type Period struct {
    ID              string
    Timezone        string
    TimezoneVersion uint64
    Start           time.Time // UTC，闭端
    End             time.Time // UTC，开端
}

// PeriodAt 返回 instant 在给定会计设置下所属的周期。
func (r *PeriodResolver) PeriodAt(instant time.Time) (Period, error)
```

要求：

- `Start`/`End` 按 §6.4 的日历运算得出；
- 保证 `Start <= instant < End`；
- 保证**同一 `TimezoneVersion` 内**相邻周期首尾相接、无重叠无空隙；跨版本边界不保证该性质（见 §16.2），隔离由 §6.6 的版本化余额键承担；
- 处理 §6.5 的待生效切换：若 `instant >= PendingEffectiveAt`，使用新时区并返回自增后的版本；
- 全系统禁止在此组件之外出现 `Format("2006-01-02")` 形式的周期派生。这是可通过 lint 或测试断言的硬约束。

## 8. Ledger 周期身份扩展

### 8.1 事件字段扩展

`ledger.Event`（`internal/ledger/event.go:64`）新增四个字段：

```go
PeriodTimezone        string `json:"period_timezone,omitempty"`
PeriodTimezoneVersion uint64 `json:"period_timezone_version,omitempty"`
PeriodStartMicros     int64  `json:"period_start_micros,omitempty"`
PeriodEndMicros       int64  `json:"period_end_micros,omitempty"`
```

写入时机：在 `EventRequestAccepted` 由 `PeriodResolver` 生成一次，随后沿 lease、settle、adjustment 链路原样传递。**下游事件禁止重新计算周期**，只能继承。理由：一次跨越周期边界的长请求必须在单一周期内完成预留与结算，与价格快照绑定同一个版本的设计一致（见 `docs/prd/prd-versioned-model-pricing.zh-CN.md` §6.3）。

`EventCostAdjusted` 的 `ServicePeriodID` 与 `PostedPeriodID` 双时间轴保持不变，两者各自携带对应的时区与版本。

### 8.2 WAL Frame 版本

当前 frame 版本为 legacy=1 与 current=2，`eventFrameVersion`（`internal/ledger/log.go:417`）依据事件是否携带新字段决定版本。

引入 `frameVersionPeriod = 3`。携带周期时区字段的事件写入版本 3，其余保持既有判定。读取端必须接受 1、2、3。

回滚约束：写入过版本 3 frame 的 WAL 无法被旧版本读取。回滚路径见 §13.3。

### 8.3 余额键扩展

`State.Balance`（`internal/ledger/event.go:526`）签名变更：

```go
func (s *State) Balance(projectID, periodID string, timezoneVersion uint64) Balance
```

回放缺少 `PeriodTimezoneVersion` 的事件时一律使用版本 `0`，与设置生效后写入的版本 `1` 及以上天然隔离。

### 8.4 缺字段记录的处理

本系统处于开发阶段，上线前会清理并重置数据，因此**不提供历史记录的语义还原**：缺少周期字段的事件不做时区反解，只按版本 `0` 归集，其 `PeriodStart` / `PeriodEnd` 为空。

保留这一分支的唯一目的，是让尚未重置数据的开发库不会在回放时崩溃。它不是一条兼容性承诺，实现上也不应为它增加任何额外机制。

## 9. Usage 与 Dashboard

### 9.1 `time_context`

`/admin/api/v1/dashboard` 及全部 Usage 查询接口的响应新增顶层对象：

```json
{
  "time_context": {
    "accounting_timezone": "Asia/Shanghai",
    "timezone_version": 3,
    "period_id": "2026-08-06",
    "period_start": "2026-08-05T16:00:00Z",
    "period_end": "2026-08-06T16:00:00Z",
    "pending_timezone": "Europe/Berlin",
    "pending_effective_at": "2026-08-06T16:00:00Z",
    "generated_at": "2026-08-06T09:12:33Z"
  }
}
```

契约：

- 所有"今天"口径的数值均由服务端在 `[period_start, period_end)` 内算出；
- 客户端**不得**自行推导日边界，只能使用 `time_context` 渲染标签与轴范围；
- `pending_*` 字段用于在界面上提示即将发生的边界变更。

### 9.2 聚合实现

`DashboardForBasis`（`internal/usage/query.go:182`）的日归属判定从"比较 `Year + YearDay`"改为"比较是否落在 `[period_start, period_end)`"。

理由：`Year + YearDay` 比较隐含"本地日"假设，无法表达 DST 日的实际长度。改为区间判定后，同一份 hourly bucket（本就是 UTC）可以被任意周期定义切分。

hourly bucket 保持 UTC 存储与 UTC 序列化，不做时区转换。转换只发生在渲染层。

### 9.3 保留期

`cmd/heimdall/main.go:613` 的 cutoff 改为：

```go
cutoff := time.Now().UTC().AddDate(0, 0, -(cfg.Usage.RetentionDays + 1))
```

`retention_days` 的文档语义改为"至少保留 N 天"。`PruneBefore`（`internal/usage/parquet.go:499`）与分区日期生成逻辑不变。

## 10. Admin API 草案

### 10.1 读取会计设置

```
GET /admin/api/v1/settings/accounting
```

响应：

```json
{
  "timezone": "Asia/Shanghai",
  "timezone_version": 3,
  "pending_timezone": null,
  "pending_effective_at": null,
  "current_period": {
    "period_id": "2026-08-06",
    "period_start": "2026-08-05T16:00:00Z",
    "period_end": "2026-08-06T16:00:00Z"
  },
  "config_file_timezone": "UTC",
  "config_file_in_effect": false,
  "tzdata": { "source": "embedded", "version": "2026a", "fingerprint": "sha256:…" },
  "updated_at": "2026-07-01T02:00:00Z",
  "revision": 4
}
```

附加查询参数 `?preview_timezone=<IANA>` 返回 `switch_preview`，描述该目标时区若被采纳会产生的首个周期、下一次预算重置时刻，以及它距生效时刻的小时数（§16.2）。不带该参数时不返回此对象；目标与当前时区相同时同样不返回。

`config_file_in_effect: false` 与 `config_file_timezone` 的差异用于在界面上明确提示配置文件已不再生效（§6.2）。

### 10.2 变更会计时区

```
PUT /admin/api/v1/settings/accounting
If-Match: "4"

{ "timezone": "Europe/Berlin" }
```

行为：

1. 校验目标时区（§6.8）；
2. 计算当前周期的 `PeriodEnd` 作为生效时刻；
3. 写入 `PendingTimezone` 与 `PendingEffectiveAt`，`Revision` 自增；
4. 写入审计事件；
5. 返回 200 与更新后的设置。

若目标时区与当前时区相同：

- 且无待生效变更：返回 200，不写入、不推进 `TimezoneVersion`、不产生审计（幂等）。版本是余额键，为空操作自增会把同一天的余额劈成两半；
- 且存在待生效变更：视为撤销该变更，清空 `PendingTimezone` 与 `PendingEffectiveAt`，写入 `change_cancelled` 审计，返回 200。此路径与 §10.3 等价，保留它是为了让"改回原时区"这一自然操作不需要客户端先判断状态。

存在待生效变更时提交**另一个**时区，直接覆盖（§7.1），记入 `change_scheduled` 审计，不返回错误。

### 10.3 撤销待生效变更

```
DELETE /admin/api/v1/settings/accounting/pending
If-Match: "5"
```

仅在 `PendingEffectiveAt` 尚未到达时可用。撤销同样记入审计。

### 10.4 稳定错误码

| 错误码 | 触发条件 |
| --- | --- |
| `timezone_invalid` | 无法被 `time.LoadLocation` 解析、为固定偏移形式、或值为 `Local`。三种原因由响应 `message` 区分，不各占一个错误码——调用方对它们的处理方式相同 |
| `timezone_pending_absent` | 撤销时不存在待生效变更 |
| `timezone_pending_elapsed` | 撤销时生效时刻已过 |

不存在"已有待生效变更"这一错误码：新的提交覆盖旧的（§7.1），这是有用的行为而非错误。

`If-Match` 不匹配沿用既有管理写路径的 412 响应，不引入本节专属的错误码。

## 11. Admin Console 需求

### 11.1 时间渲染收口

全部时间渲染收敛到 `web/src/format.ts` 中的单一入口，该入口强制接受时区参数：

```ts
export function formatInstant(value: string, timeZone: string, style?: InstantStyle): string
```

`Intl.DateTimeFormat` 必须显式传 `timeZone`。禁止出现未指定时区的日期格式化调用，通过 ESLint 规则或单元测试断言。

`web/src/TrendChart.tsx` 的 uPlot 配置新增 `tzDate`，使 X 轴刻度使用与 `time_context` 相同的时区。

趋势图是一个 7 天滚动窗口，不是"今天"，因此其 X 轴范围仍由窗口长度决定，不取自 `time_context`——两者本就不是同一个区间。必须一致的是**时区**：刻度所在的日边界与"今天"卡片相同，这样图上的一天与卡片说的一天是同一天。

### 11.2 渲染归类规则

界面上每一个时间戳都按会计时区渲染，无例外。这是 §5 第 9 条的直接结果：只有一个显示口径，就不存在"这个时间戳属于哪一类"的判断。

必须带可见时区标注的，是承载治理含义的时间：周期边界、日预算重置、"今天"指标、价格生效时刻。运维性时间戳（审计条目、密钥过期、备份时刻、会话过期）与它们共用同一时区，无需逐个标注。

价格生效时刻虽然按 UTC 选择（见定价 PRD §6.4），但按会计时区展示，并同时给出 UTC 值，避免管理员误判。

### 11.3 运行总览页

- "今天"指标区域标题旁展示会计时区名；
- 悬停展示当前周期的完整 UTC 区间；
- 存在待生效时区变更时，总览页顶部展示提示条，说明新时区与生效时刻。设置页同样展示并提供撤销入口（§11.4）；提示条放在总览页是因为这里正是"今天"的数字即将改变含义的地方。

### 11.4 设置页

- 会计时区：展示当前值、版本号、当前周期区间、tzdata 来源与版本；提供变更入口；
- 变更流程必须展示确认步骤，明确列出："生效时刻（UTC 与本地两种表示）"、"进行中周期不受影响"、"历史数据不重算"三条；
- 配置文件值与存储值不一致时，展示警示说明配置文件已不生效。

### 11.5 国际化

全部新增文案提供 `zh-CN` 与 `en-US` 两份。时区名本身不翻译，使用 IANA 原名，可附加 `Intl.DateTimeFormat` 的 `timeZoneName: "longGeneric"` 作为辅助说明。

## 12. Audit、Metrics 与告警

### 12.1 Audit 事件

沿用既有 `<domain>.<action>` 命名约定：

| Action | TargetType | 触发 |
| --- | --- | --- |
| `accounting.timezone.change_scheduled` | `instance_accounting` | 提交时区变更 |
| `accounting.timezone.change_cancelled` | `instance_accounting` | 撤销待生效变更 |
| `accounting.timezone.change_applied` | `instance_accounting` | 生效时刻到达并完成切换 |

事件负载须包含：原时区、目标时区、原版本、新版本、生效时刻、发起者；若本次提交覆盖了一个既有的待生效变更，还须记录被覆盖的目标时区。`change_applied` 由系统触发，`ActorType` 为 `system`。

### 12.2 Metrics

沿用 `heimdall_` 前缀：

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `heimdall_accounting_timezone_version` | gauge | 当前时区版本，用于跨节点比对 |
| `heimdall_accounting_period_end_seconds` | gauge | 当前周期结束的 Unix 时间，用于外部系统对齐 |
| `heimdall_tzdata_info{source,version,fingerprint}` | gauge，值恒为 1 | tzdata 来源、版本与转换表指纹。版本号是打包者的声明，指纹是实际规则的证据——两个实例可能报告同一版本号却携带不同的转换表，跨实例一致性以指纹为准 |

### 12.3 告警与 doctor

`heimdall doctor`（`internal/app/doctor.go:171`）的 `clock` 检查扩展为输出：系统 UTC 时刻、会计时区、当前周期 UTC 区间、tzdata 来源与版本、配置文件值是否与存储一致。

跨节点部署应基于 `heimdall_tzdata_info` 与 `heimdall_accounting_timezone_version` 配置一致性告警。tzdata 版本不一致时应视为高优先级，因为它可能导致账本分叉且不会自行暴露。

## 13. 升级与数据迁移

### 13.1 Schema 迁移

metadata schema 从 v15 升至 v16（当前值见 `internal/store/bolt/store.go:26`），迁移步骤：

迁移步骤本身是一个空操作标记：会计设置记录由首次启动时的 `SeedInstanceAccountingSettings` 写入（`Timezone` 取自 `config.yaml` 的 `usage.timezone`，`TimezoneVersion = 1`），而不是由迁移写入——存储层拿不到配置，且 §6.2 要求配置只作播种。

迁移不触碰 Ledger WAL，不重写任何历史事件。

### 13.2 历史数据

**本系统处于开发阶段，上线前会清理并重置数据。因此不提供历史账本的兼容性承诺。**

由此明确取消的工作：历史周期的语义还原、升级前后余额逐条不变的演练、跨升级周期的余额结转事件。缺字段记录按 §8.4 处理——只保证不崩溃，不保证可解释。

上线前的数据重置是这些承诺得以取消的前提；若该前提改变，§8.4 与本节都必须重写。

### 13.3 回滚

- Phase A 无 schema 与 WAL 变更，可直接回滚二进制。
- Phase B 写入 v3 frame 后，旧版本无法读取 WAL。开发阶段的回滚方式是清理数据目录重新初始化，不做备份恢复演练。

### 13.4 备份与恢复

备份 manifest 记录会计时区与时区版本。恢复到 tzdata 版本不同的实例时输出警告但不阻断：周期字段自描述，边界不依赖恢复端的规则，这正是 §6.3 的价值所在。

## 14. 建议实施顺序

### Phase A：显示层收口与确定性前提

无 schema 变更，无 WAL 变更，可独立发布与回滚。

1. `cmd/heimdall` 导入 `_ "time/tzdata"`；新增 `internal/timezone` 报告来源、版本与**转换表指纹**；`heimdall version`、`doctor` 的 `tzdata` 检查与 `heimdall_tzdata_info` 指标输出三者；
2. Admin API 增加 `time_context`（`internal/app/time_context.go`），覆盖 `/dashboard`、`/usage`、`/system/status`，此阶段由 `cfg.Usage.Timezone` 派生，`timezone_version` 恒为 `0`；
3. `web/src/format.ts` 收口为必须显式传入时区的 `formatInstant`，`web/src/timezone.ts` 持有全站渲染时区，`web/src/TrendChart.tsx` 通过 `tzDate` 对齐；
4. 运行总览页展示会计时区与周期 UTC 区间；
5. 文档补齐 UTC 与会计时区的分工说明（operator guide 新增「Time zones」小节，两份 user guide 同步）；
6. 保留期 cutoff 改为 `N + 1` 天。

交付后，图表与指标日边界不一致的缺陷即被消除。

实施期间发现 `time.Date` 对被抹除的本地日期是**向回**归一化的，与本文档初稿的假设相反；规范已据此改写（§6.4），周期起点定义为"该本地日期真实存在的第一个瞬间"。

### Phase B：会计时区治理化与周期版本化

1. schema v16 迁移与 `internal/domain/accounting_settings.go` 的 `InstanceAccountingSettings`；存储访问在 `internal/store/bolt/store.go`，含 `SeedInstanceAccountingSettings`（仅在记录不存在时写入）；
2. `internal/budget/resolver.go` 的 `PeriodResolver`，`budget.Manager` 与 Admin API 的周期派生全部收口，`Manager` 不再持有 `*time.Location`；
3. Ledger 事件新增四个周期字段，WAL frame v3（`internal/ledger/log.go`），`BalanceKey` 加入 `TimezoneVersion`；`Request`/`Attempt` 携带完整 `Period`，下游事件继承而不重算；
4. 待生效变更：`PendingTimezone` + `PendingEffectiveAt`，生效时刻取当前周期 `PeriodEnd`；到期由 `runUsageMaintenance` 促成记录落定；
5. Admin API `GET/PUT /admin/api/v1/settings/accounting` 与 `DELETE .../pending`，Console 新增记账时区面板（`AccountingTimezoneForm`）；
6. Audit 三个事件（scheduled / cancelled / applied）与 `heimdall_accounting_timezone_version`、`heimdall_accounting_period_end_seconds` 指标；
7. `config.yaml` 降级为播种值，`doctor` 新增 `accounting_timezone` 检查，配置文件与存储不一致时 `warn`。

实施期间补入 §7.1 的两条写入约束（空操作不推进版本、版本不可回退），以及 §16.2 的切换窗口告警——后者是评审发现的：切换会铸造一份全新余额，使日额度在一个自然日内接近翻倍，而初稿把这一后果写反了。

## 15. 安全与正确性要求

1. 会计时区变更走既有管理写路径鉴权（CSRF + `If-Match` + 审计），与 `/settings/ui` 等治理写入同级，不额外要求 MFA 提升。它不操作凭据，与其他设置写入承担相同的风险等级。
2. 时区名写入前必须校验，禁止把未经校验的字符串传入 `time.LoadLocation` 后仅依赖错误处理。
3. 时区名不得进入任何文件路径构造。分区路径恒由 UTC 日期生成，与时区名无关。
4. 周期解析入口——`PeriodResolver.PeriodAt` 与其依赖的包级 `budget.PeriodAt(instant, location)`——必须对 `instant` 为零值、`location` 为 nil、设置未加载的输入返回错误，而非静默使用 UTC。
5. 周期区间必须保证 `Start < End` 且 `End - Start` 在 22 至 26 小时之间，超出范围视为解析错误并拒绝服务，防止 tzdata 损坏导致无边界周期。该守卫针对损坏的规则，不针对不寻常的辖区：两种真实的日长度变化（夏令时、日期标签被抹除）都在 §6.4 中处理且都落在带内。
6. 生效判定必须是关于任意瞬间的纯函数：给定设置与 `instant` 即可确定时区与版本，不得依赖后台任务是否按时运行。补齐存储状态的后台任务只承担审计与展示，不承担正确性。

## 16. 兼容性与边界行为

### 16.1 DST 边界

- 秋季回拨日本地日长 25 小时，计为**一个**周期，日预算额度不变；
- 春季前跳日本地日长 23 小时，同样计为一个周期；
- 本地午夜在跳变中不存在时，周期起点取该本地日期真实存在的第一个瞬间，而非 `time.Date` 的归一化结果（§6.4）。

### 16.2 时区变更跨越周期边界

- 变更在当前周期结束时刻生效，切换瞬间新周期以新时区的日历日开始；
- 切换后的首个周期仍是新时区的一个**完整**日历日（22–26 小时），长度不因切换而改变；
- 但该周期的起点通常**早于**切换时刻——新时区的这一天在切换前就已开始。由于余额键含时区版本（§6.6），它是一份全新的空余额，因此"从生效时刻到下一次预算重置"的墙钟窗口为 `新周期End − 生效时刻`，落在 `(0, 24h]`。实测 `Asia/Shanghai → UTC` 该窗口为 8 小时，反向为 16 小时。
- 后果：一个自然日内该项目的可用额度接近翻倍。额度不按比例折算是刻意的（周期是完整的一天），但这个后果不直观，**必须在确认界面中以具体数值显示**：下一次重置的时刻，以及它距生效时刻还有多久。

### 16.3 长请求跨越周期边界

请求在 `EventRequestAccepted` 时确定周期，跨越边界完成的请求仍结算在起始周期，与既有价格快照绑定语义一致。

## 17. 测试计划

### 17.1 周期解析测试

1. 表驱动覆盖 `UTC`、`Asia/Shanghai`、`Europe/Berlin`、`America/New_York`、`America/Santiago`（本地 00:00 起跳 DST）、`America/Havana`（同上）、`Australia/Lord_Howe`（半小时 DST）、`Pacific/Apia`（2011-12-30 标签被抹除）；
2. 秋季回拨日断言周期长度为 25 小时且计为一个周期；
3. 春季前跳日断言周期长度为 23 小时且计为一个周期；
4. 本地午夜被抹除的时区断言周期起点为该本地日期第一个真实瞬间，且与前一周期恰好首尾相接（不重叠）；
5. 连续 400 天遍历断言相邻周期首尾相接、无重叠无空隙；
6. 周期长度守卫在边界值上直接断言（22h/26h 接受，21h59m/26h01m/0/负值拒绝）。没有任何真实 IANA 时区能触发它——两种改变日长度的真实情形都已处理且都落在带内——因此另有一项测试遍历 12 个时区共 400 天，断言守卫从不误伤。

### 17.2 时区变更测试

1. 变更在下一个周期生效，进行中周期边界不变；
2. 变更后 `TimezoneVersion` 自增，新旧周期余额互不影响；
3. 撤销待生效变更；
4. 生效时刻已过后撤销返回 `timezone_pending_elapsed`（断言 `code` 字段，不只是状态码）；
5. 目标时区与当前相同且无待生效变更时，`TimezoneVersion` 与 `Revision` 均不变；
6. 陈旧 `If-Match` 被拒绝；
7. 切换预览返回下一次重置时刻，且该时刻晚于生效时刻、间隔落在 `(0, 24h]`，首个周期起点早于生效时刻。

### 17.3 Ledger 测试

1. 携带周期字段的事件写入 v3 frame，未携带的保持既有版本判定；
2. v3 事件经真实 WAL 写入并从磁盘回放，四个周期字段逐一比对不变，且同一文件内的 legacy 事件仍以 v1 回放；v3 frame 载荷缺少时区标识时判为损坏；
3. 缺字段事件归入时区版本 `0`，与版本 `1` 及以上的余额互不影响；
4. 长请求跨周期边界结算在起始周期：起始周期承担全部已提交金额且预留归零，后一周期分文未沾；
5. 一次请求的全部五个事件（accepted / reservation / started / settled / finalized）携带完全相同的周期四元组；
6. 崩溃恢复后 pending lease 的周期字段保持不变。

### 17.4 Usage 与 API 测试

1. `time_context` 在全部按日聚合的接口（`/dashboard`、`/usage`、`/system/status`）中存在且自洽（`period_start < period_end`，`generated_at` 落在区间内）。单条记录详情接口不按日聚合，不携带该对象；
2. 周期边界为半开区间：起点计入、终点不计入，边界两侧各一条记录验证不重不漏；
3. DST 日的"今天"聚合覆盖完整的 23 或 25 小时，且区间外相邻记录被排除；
4. `PruneBefore` 的分区裁剪行为（`internal/usage/parquet_test.go`）；另有独立测试断言 cutoff 恰好比精确年龄多保留一天，且 UTC−12 至 UTC+14 各时区下运维被承诺的最旧一天都不会被裁掉；
5. §10.4 的三个错误码均可复现，断言 `code` 字段而非仅状态码。

### 17.5 前端测试

1. 测试进程时区固定为 `America/New_York`（`web/vite.config.ts`），使"忽略服务端时区"的回归必然失败，而不是在开发机时区恰好相同的情况下蒙混过关；
2. 图表刻度使用服务端会计时区绘制，会计时区变化时图表重建；
3. 不存在未指定 `timeZone` 的 `Intl.DateTimeFormat` 调用，且 `format.ts` 内每处构造都携带 `timeZone`；
4. 设置页：待生效变更提示与撤销、配置文件失效警示、切换后重置窗口告警、当前时区规则指纹可见；
5. 总览页：会计时区名与周期 UTC 区间可见，异常时间戳按会计时区渲染（以非本机时区的夹具验证），待生效变更提示条在有变更时出现、无变更时不出现；
6. `zh-CN` 与 `en-US` 文案键完整对齐。

### 17.6 完成定义

1. §17.1–§17.5 全部通过；
2. `heimdall doctor` 输出会计时区、周期区间与 tzdata 指纹，并在配置文件与存储不一致时 `warn`，均有断言测试；
3. 已有开发库可无 panic 打开并完成 v16 迁移；
4. `docs/guides/operator-guide.md` 与两份 user-guide 更新完毕。

## 18. 待实现阶段确认的问题

1. 是否为会计时区提供 CLI 变更入口。倾向：提供只读查询（`heimdall doctor` 已输出），变更只走 Admin API，保证审计链完整。

