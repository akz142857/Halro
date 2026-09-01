# v0.5.0 评审 · 阶段 0：范围事实底座

> 本文件只抄事实，不下判断。每一行都注明 `文件:行号` 或命令输出。
> 方案见 [`review-plan.md`](review-plan.md)。凡本文件与方案的数字不一致，以本文件为准
> ——本文件的数字是当场量的。

基线 `v0.4.0` = `2501668`，HEAD = `cd37927`。

## 0. 与方案 §2 的差异（已复量）

| 项 | 方案写的 | 实测 | 命令 |
|---|---|---|---|
| 全量 diff | 198 files / 15862 / 2168 | 同 | `git diff --stat v0.4.0..main` |
| 生产 Go | 47 files / 3767 / 457 | **48 files / 3828 / 460** | `git diff --stat v0.4.0..main -- '*.go' ':!*_test.go' ':!internal/webui/dist'` |
| 十三个"未触及"包 | 全零 | 全零（逐包复核通过） | `git diff --name-only v0.4.0..main -- internal/<pkg>` |

差异不影响任何范围决定，但方案 §2 的生产 Go 数字不要再引用。"未触及"这条事实成立，
方案所有"不查"决定的依据是可靠的。

---

## 表 1：用量派生物版本

三个版本量，各自的写者、读者、拒绝条件：

| 版本量 | 值（HEAD） | 值（v0.4.0） | 写者 | 读者 | 不匹配时 |
|---|---|---|---|---|---|
| bbolt `schemaVersion` | **33** | 32 | `internal/store/bolt/store.go:24`；迁移 33 在 `:901`，只做 `CreateBucketIfNotExists(usage_daily_rollup)` | `store.go:1313`、`:1620` | 高于当前版本：拒绝打开（`:1620`）。低于：顺序跑迁移到 33 |
| usage `checkpointVersion` | **8** | **7** | `internal/usage/aggregate.go:31`、`:265` | `RestoreCheckpoint` `:210`、`:218` | `"usage checkpoint version %d is not supported"` → 上层视为拒绝理由 |
| `domain.RollupVersion` | **1** | 不存在 | `internal/domain/usage_rollup.go:14`；随 `UsageRollupState` 落盘 `store_usage.go:59` | `internal/app/runtime.go:870` | `"usage rollup version %d is not %d"` → 重建 |

**关键事实：`checkpointVersion` 在本范围内从 7 变成了 8。** 所以 v0.4.0 写出的 data 目录
用 HEAD 启动时，走的是 `RestoreCheckpoint` 拒绝那条路，而不是"checkpoint 能读、只缺
rollup 状态"那条。两条路的终点相同（清空重建），但阶段 3 §2 复核的应该是前者实际触发的
那一条日志。

**清空重建的触发条件**（`internal/app/runtime.go:843-878`，`loadUsageCheckpoint` 逐条命名理由）：

| # | 条件 | 行 | reason 字符串 |
|---|---|---|---|
| 1 | 无 checkpoint | 845 | `no usage checkpoint` |
| 2 | checkpoint 读不出来 | 848 | `usage checkpoint unreadable: %v` |
| 3 | payload 版本不符（含 7→8） | 852 | `usage checkpoint rejected: %v` |
| 4 | 信封 watermark ≠ payload watermark | 855 | `usage checkpoint envelope does not match its payload` |
| 5 | checkpoint 超前于 WAL head | 858 | `usage checkpoint is ahead of the ledger head` |
| 6 | **rollup 状态缺失** | 862 | `usage rollup state is missing` |
| 7 | rollup 状态读不出来 | 865 | `usage rollup state unreadable: %v` |
| 8 | `RollupVersion != 1` | 870 | `usage rollup version %d is not %d` |
| 9 | **rollup watermark ≠ checkpoint watermark（相等判定，不是顺序判定）** | 876 | `usage rollup and checkpoint describe different ledger positions` |

任一命中 → `store.ResetUsageDerivatives()`（`runtime.go:836`），失败则**拒绝启动**
（`:837` 返回错误，注释明写"serving doubled totals is worse than not serving"）。

`ResetUsageDerivatives`（`store_usage.go:229-247`）在**一个事务**内做三件事：删
`keyUsageCheckpoint`、删 `keyUsageRollupState`、`DeleteBucket(usage_daily_rollup)` 后
重建空桶。

**同事务性**：`PutUsageCheckpoint`（`store_usage.go:40-79`）在一个 `db.Update` 里依次做
`applyUsageRollupDelta` → `Put(keyUsageRollupState)` → `Put(keyUsageCheckpoint)`。
写失败时调用方把增量还回内存（`runtime.go:939-947` + `ReturnCheckpoint`
`usage/rollup.go:187`）。

---

## 表 2：rollup 键空间

| 项 | 事实 | 位置 |
|---|---|---|
| 编码 | `PeriodID \x00 TimezoneVersion \x00 Dimension \x00 DimensionKey` | `usage_rollup.go:106-110` |
| 分隔符 | **NUL (`\x00`)**，选它是因为维度键合法地含 `/`（Gemini `models/...`、Bedrock ARN） | `:60-63` |
| 解码 | `SplitN(..., 4)`，第 4 段是余下全部，故维度键可含除 NUL 外任意字节 | `:128-143` |
| 维度 | `total`, `project`, `requested_model`, `provider`, `deployment`, `provider_model` | `:26-33` |
| total 行的键 | 常量 `"-"`；total **存储**而非读时求和 | `:37`、`:158-163` |
| 键来源 | `event.ProjectID / RequestedModel / ProviderID / DeploymentID / ProviderModel`；空值**跳过**不落空键 | `usage/rollup.go:46-70` |
| 请求级维度 | 仅 `total` / `project` / `requested_model`（请求身份只存在于 `EventRequestFinalized`） | `usage_rollup.go:70-75` |
| 每维度键上限 | `MaxRollupKeysPerDimension = 200` | `usage_rollup.go:57` |
| 溢出键 | `RollupOtherKey = "__other__"` | `:41` |
| 折叠规则 | 按 **ledger 顺序**（`FirstSequence`）准入前 200 个，其后并入 `__other__`；不按大小 | `usage_rollup.go:44-56`、`store_usage.go:82-137` |
| 上限计数的范围 | 恰好一个 `RollupDimensionPrefix`（= 一天 + 一个时区版本 + 一个维度） | `usage_rollup.go:113-117`、`store_usage.go:139-152` |
| `__other__` 是否计入 200 | **不计**，`storedDimensionKeys` 显式跳过 | `store_usage.go:146-149` |
| 时区版本在键的哪一段 | 第 2 段；写者 `budget/resolver.go:93`、`budget/period.go:45`，从 admission 时刻的 period 继承 | `usage_rollup.go:107` |
| 改时区后旧行 | 旧行不动；同一日期标签的两个版本是两把不同的键，`Add` 跨版本合并直接报错 | `usage_rollup.go:245-249` |
| 账期归属 | 用事件自带的 `PeriodID`（admission 时刻盖章），**不**由完成时间重新推导 | `usage/rollup.go:29-38` |
| 溢出处理 | `Add` 的每一列走 `addRollupInt64/Uint64`，溢出/下溢返回 error 而非回绕 | `usage_rollup.go:326-341` |

---

## 表 3：`/admin/api/v1/usage/summary` 契约

路由：`internal/app/runtime.go:1473`，`router.With(r.requireAdmin)` —— 与
`/admin/api/v1/usage`（`:1472`）、`/usage/requests/{requestID}`（`:1474`）同一道 admin 门。

**参数**（未列出的参数一律 400 `unsupported summary filter`，`admin_usage_summary.go:118-127`）：

| 参数 | 合法值 | 默认 | 非法时 |
|---|---|---|---|
| `granularity` | `day` / `month` / `year` | `day` | 400 `granularity must be day, month, or year` |
| `start` / `end` | `YYYY-MM-DD`（**闭区间两端**） | end=今天账期；start 见下 | 400 `start/end must be an accounting date` |
| `group_by` | 任一 rollup 维度，**除 `total`** | 空（不分组） | 400 `unknown group_by dimension` |
| `limit` | 1..100 | 50 | 400 `invalid group limit` |
| `sort` | `cost`/`calls`/`tokens`/`errors`/`success_rate` | `cost` | 400 `unknown sort measure` |
| `order` | `asc`/`desc` | `desc` | 400 `order must be asc or desc` |
| `project_id`/`model`/`provider_id`/`deployment_id`/`provider_model` | 任意字符串 | 空 | 多个同时给：400；与 `group_by` 同时给：400 |

**互斥规则**：`group_by` 与任一维度过滤器不能并用（`:154-158`），理由写在代码里——rollup
是边际聚合，没有交叉项。多个维度过滤器也不能并用（`:145-151`）。

**默认窗口与上限**（`:26-28`、`:597-624`）：

| 粒度 | 默认起点 | 桶数上限 |
|---|---|---|
| day | end − 29 天 | 400 |
| month | end − 11 月 | 36 |
| year | end − 2 年 | 10 |

上限按**桶数**而不是按月数，注释明写按月数写会让 year 视图的默认窗口自己违法。

**与 `/admin/api/v1/usage` 的关系**：两者都读同一个 `usage.Aggregate`，但 summary 读的是
**持久化的日 rollup + 未落盘增量**（`collectSummaryRows` `:243-292`：先
`store.UsageRollupRange`，再 merge `r.usage.PendingRollup()`），`/usage` 读的是 aggregate
的实时明细。二者都不是权威——WAL 才是。

**响应形状**（`:206-238`）：`granularity`、`start`、`end`、`buckets[]`、`totals`、
`timezone_changes[]`、`watermark_sequence`、`time_context`；分组时另加 `group_by`、
`groups[]`、`groups_truncated`、`groups_other_count`、`sort`、`order`；过滤时加 `filter`；
`resource_labels`（项目名 + 部署名，**失败则整段省略**，`:234-236`）。

**总计的来源**：`totals` 和 chart 走 `reportDimension`（无过滤时是 `total` 行），
breakdown 走 `groupBy` 行 —— 两组行，不是同一组（`:196-205`，注释说从分组行读总计曾让
"completed requests" 在选中无请求身份的维度时塌成 0）。

**请求级列的缺席语义**：`summaryMetrics` 里请求级字段是 `*int64` + `omitempty`，无请求身份
的维度**不输出该字段**而非输出 0（`:50-57`、`renderMetrics` `:553-580`）。

**P95**：从直方图读，返回**桶上界**，`latency_approximate` 恒为 true；越过最后一个界时
另给 `*_over_max`（`usage/rollup.go:240-263`）。

---

## 表 4：MiniMax Profile

三行共用一个 Surface（`minimax-api`，`provider_profile.go:31`）与一个凭据方案
（`CredentialBearerStatic`），故构成一个连接组，一把 key 绑三行（`provider_table.go:309-338`）。

| 项 | Anthropic Messages | Chat | Responses |
|---|---|---|---|
| Profile ID | `minimax.anthropic.messages.v1` | `minimax.chat.v1` | `minimax.responses.v1` |
| BaseURL 预填 | `https://api.minimax.io` | 同 | 同 |
| 备用主机 | `https://api.minimaxi.com`（大陆账号），**由 operator 编辑 base URL 达成，不是第二个 profile 行**；key 两边不通用 | 同 | 同 |
| 绑定 | `anthropicWire`（Chat/ChatStream/Messages/MessagesStream） | `chatPair` | **只有 `OperationChat`，不绑 stream** |
| Primitive | `minimax.anthropic.messages(.stream)` | `minimax.chat-completions(.stream)` | `minimax.responses` |
| 适配器 | `anthropicprovider.New`，`MessagesPath: anthropic/v1/messages` | `openaiprovider`（`Responses:false`） | `openaiprovider`（`Responses:true`） |
| 凭据头 | `Authorization: Bearer `，冲突头 `x-api-key` 被清 | 同 | 同 |
| `validateEndpoint` | **无** | 无 | 无 |

**Defaults / Ceiling 逐能力**（`provider_table.go:405-446`；三行都是 `Defaults == Ceiling`）：

| 能力 | Anthropic | Chat | Responses |
|---|---|---|---|
| chat | ✓ | ✓ | ✓ |
| streaming | ✓ | ✓ | ✗（Halro 侧范围决定：Responses 分支不绑 stream primitive） |
| stream_usage | ✓ | ✓ | ✗ |
| tools | ✓ | ✓ | ✓ |
| vision / fetched_image | ✓ | ✓ | ✓ |
| reasoning | ✓ | ✓ | ✗（canonical response mapper 丢 reasoning item） |
| json_object | ✗ | **✓（实测得来）** | ✗ |
| structured_outputs | ✗ | ✗ | ✗ |
| developer_role | ✗ | ✗ | ✗ |
| embeddings | ✗（MiniMax 的 `/v1/embeddings` 是自有形状：`texts`/`type` 入、`vectors` 出） | ✗ | ✗ |
| provider_executed_tools | ✗（服务端 web search 会绕过 SafeTransport，属契约评审） | ✗ | ✗ |

**模型枚举**：Anthropic 行设 `CatalogShape: CatalogOpenAI`
（`provider_adapters.go:143-150`）——走同主机的 OpenAI 路由 `GET /v1/models`，
2026-09-01 实测：`object=list`、8 条 `{id,object,created,owned_by}`。这是 B10 落地点。
内置目录（`modelcatalog/builtin.go:380-455`）供的是**能力证据**，8 个精确标识符，
两处保守收窄：只有 M3 有 vision；M2.x 不写 output 上界；reasoning 只给 M3（M2.x 关不掉思考）。

**#250 实测证伪掉的**（`docs/verification/provider-real-matrix.md:84-150`）：

| 原假设 | 实测结果 | 现在的值由什么支撑 |
|---|---|---|
| 三站文档都没写 `response_format` → 不支持 | **支持**，200 + 合法 JSON | Chat 行加 `JSONObject`；schema 模式没发过，仍不声明 |
| 文档说无 prompt caching | **两条路由都报** `cached_tokens: 128` | 运维后果：要录 cache-read 价，否则按 input 价结算 |
| M2.7 不接受关闭思考 | **接受** | —— |
| 流以 `[DONE]` 结束 | **不发哨兵**，`finish_reason` 块 + usage 块后直接关闭 | 缺陷已修：豁免限本 provider 且仍要求先有 `finish_reason` |

**没实测的**：Responses 行（不同端点，未测）；大陆主机 `api.minimaxi.com`
（明确记为 not measured，不是 passing）。评审记录里这两条要照写"未实测"。

---

## 表 5：新增 Provider 的注册点与各自的闸

来源：`docs/contracts/adding-a-platform.md`（`TestTheChecklistNamesGuardsThatExist`
把下表的测试名钉到真实存在的守卫上）+ 实际代码。

| # | 注册点 | 文件 | 闸（测试） |
|---|---|---|---|
| 1 | Profile 标识符 | `domain/provider_profile.go:64-66` | 随后续各条 |
| 2 | Profile 表行（Surface/Scheme/BaseURL/Defaults/Ceiling） | `domain/provider_table.go:321-338` | `TestOnlyNamedProfilesHaveAWiderCeiling`、`TestCeilingWithinProfileManifestOperations` |
| 3 | 操作→primitive 绑定 | `provider/profile_bindings.go:126-135` | `TestEveryPrimitiveConstantIsBoundBySomeProfile`（只抓"孤儿常量"，抓不到两 profile 互换 primitive） |
| 4 | 字段级声明 | `compatibility/minimax.go` | `TestEveryProfileRegistersItsOwnFieldRules` |
| 5 | 端点覆盖 | `compatibility/manifest.go` + `docs/compatibility/endpoint-manifests.json` | `TestEveryChatProfileAppearsInAnEndpointManifest`、`TestTheManifestDeclaresEverythingTheRulesRefuse`（#250 起**双向精确**） |
| 6 | 适配器构造 | `app/provider_adapters.go:134-166` | `TestEveryReachableProfileBuildsAnAdapter`（遍历 domain 表，**穷举式**）、`TestEveryReachableProfileReachesTheNetworkWhenCalled` |
| 7 | Provider 类型准入 | 无需手工——`IsRegisteredProviderType` 读表（`provider_table.go:557-560`） | `TestEveryOfferedProviderTypeIsAcceptedOnSave`、`TestEveryRegisteredProviderTypePassesInstanceValidation` |
| 8 | Anthropic 线族专项 | `gateway/service.go:1714`、`provider/anthropic/*` | `TestNativeAnthropicListsAgree`、`TestNoNativeProfileIsWithheld`、`TestEveryAnthropicWireProfileIsExcludedFromTheReasoningProbe` |
| 9 | 平台自有布线 | —— | `TestMiniMaxWiringAddressesOneRoutePerProfile` |
| 10 | 控制台 golden fixture | `web/` fixture | `TestProviderProfilesGoldenMatchesConsoleFixture` |
| 11 | 模型目录 | `modelcatalog/builtin.go:409` | **无闸——文档明写"Nothing enforces that a platform's models are seeded"** |
| 12 | i18n | `web/src/i18n/locales/*` | `i18n.test.tsx`（本轮阶段 2 复核是否覆盖新键） |

**文档自陈的三处无机械守卫**（`adding-a-platform.md:185-213`，原文照抄，不加判断）：

1. `semanticGenerationPrimitives` 漏登记：什么都不失败，代价是更有损的翻译。
2. 模型目录跳过：合法，代价是运维手工声明能力（= 一次放宽）。
3. `profileOperationTable` 里写错 primitive 但每个常量仍有绑定（如两 profile 互换）：
   `ProfileManifest.Validate` 对内置 manifest 是同义反复，抓不到。

**出网面**：`safetransport` 包零改动，且**没有全局主机清单**——每个连接的允许主机由它自己的
endpoint 主机名推导（`app/admin_providers.go:1359`
`AllowedHosts: []string{strings.ToLower(endpoint.Hostname())}`，
`app/providers.go:454` 取自 provider 记录）。所以两个 MiniMax 主机不需要登记；
反过来说，MiniMax 三行没有 `validateEndpoint`，base URL 指向哪台主机由 operator 的连接
记录决定，与 OpenAI-compatible 同形，与 Bedrock Mantle（有 `ValidateEndpoint`）不同形。
这条是 A3 的靶心，本文件只陈述，不判定。

---

## 阶段 0 期间顺手记下、留给阶段 1 判定的两处观察

不下结论，只标位置，免得后面重新找：

1. `provider_table.go:378-381` 的注释仍写 "none of it has been confirmed against a real
   account"，而同一块 `:409-418` 与 `:424-444` 已经在引用 2026-08-31 的实测。两句话
   在同一个 `var` 块里。→ A6 / A5。
2. `store_usage.go:139-152` `storedDimensionKeys` 每次增量对每个 (天,维度) 前缀做一次
   全前缀 cursor 扫描，构造一个最多 200 项的 map。→ A5（方案 §6 已点名）。
