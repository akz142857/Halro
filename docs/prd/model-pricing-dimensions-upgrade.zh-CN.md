# 模型计价维度升级方案

- 状态：P1 已实施；P2、P3 待评审
- 日期：2026-11-13
- 承接：[ADR 0022](../adr/0022-cache-read-input-pricing.md)（缓存读取计价，文中把缓存写入列为未计价缺口）、[ADR 0023](../adr/0023-time-of-day-pricing.md)（分时价位）
- 实施决策：[ADR 0028 缓存写入计价](../adr/0028-cache-write-input-pricing.md)

## 1. 背景

模型部署页面的价格表单只有三个 token 单价：输入、缓存输入（命中）、输出，外加每次请求固定费。
OpenAI 官方价格表已经是 Input / Cached input / Cache writes / Output 四列。对照当前对接的全部计费平台后，
缺口不止缓存写入一项。

### 1.1 现状（代码已核实）

- 缓存写入 token 数**已经采集**：`semantic.Usage.CacheWriteInputTokens` 统一了 Anthropic
  `cache_creation_input_tokens`、OpenAI `cache_write_tokens`、Bedrock `cacheWriteInputTokens`，
  落到 Ledger 事件 `ProviderCacheWriteInputTokens` 和 Parquet。
- 但**没有计价**：`domain.PriceTier.Calculate`（`internal/domain/pricing_cost.go`）和
  `budget.EstimateCostMicros`（`internal/budget/manager.go`）只吃 input / cached / output，
  写入部分按普通输入价计费。上游对写入收溢价，所以这是**少收**；`recordUsageTiers`
  （`internal/gateway/service.go`）对这类行打 `CostEstimated` 标记。
- 写入只解析了**总数**，没有按缓存时长拆分；而两家上游都按时长给不同价格（见 1.2）。
- 预留（`estimateReservation`、`accountingTermsFromSnapshot`）把整段 prompt 按普通输入价计，
  前提是"普通输入价是最贵的输入类价格"。缓存写入比输入贵，这个前提会被打破。

### 1.2 各平台计价维度调研（2026-11-13）

✅ = 读过官方页面原文；⚠️ = 只有第三方资料，未核实官方。

| 平台 | 输入 | 缓存读 | 缓存写 | 输出 | 输入长度分档 | 其他会影响单次请求金额的项 |
|---|---|---|---|---|---|---|
| OpenAI ✅ | ✓ | ✓ | 两档：30m（1.25×）、12h（2.5×）；仅 GPT-5.6 及以后、Responses API 收 | ✓ | >272K 整单换价（含缓存读写、输出） | 联网搜索 $10/千次；File search $2.50/千次；Batch/Flex/Fast/Ultrafast 独立价表；数据驻留 +10% |
| Anthropic ✅ | ✓ | ✓（按模型 0.025×–0.1×） | 两档：5m（1.25×）、1h（2×） | ✓ | Haiku 5.5 >100K 整单换价（缓存读写计入长度） | 联网搜索 $10/千次；代码执行按容器小时；Batch 五折；`inference_geo:"us"` 1.1×；`speed:"fast"` 2× |
| Bedrock Mantle ⚠️ | ✓ | ✓ | 跟随上游（Claude 5m/1h；GPT-5.6+ 1.25×） | ✓ | 跟随上游 | 本区域可能 +10%，官方页面说法不一 |
| Azure OpenAI | 跟随 OpenAI | | | | | 区域/数据区差价按部署体现 |
| Gemini ✅ | ✓ | ✓ | 显式缓存另收存储费（每百万 token 每小时） | ✓ | >200K 整单换价 | |
| DeepSeek ✅ | ✓（未命中） | ✓（命中） | 无 | ✓ | 无 | 峰谷价：已由 ADR 0023 分时价位覆盖 |
| 火山方舟 ⚠️ | ✓ | ✓ | 上下文缓存另收存储费（按小时） | ✓ | 0–32K / 32K–128K / 128K–256K | |
| 智谱 BigModel ⚠️ | ✓ | ✓ | 隐式缓存，无写入费 | ✓ | 阿里云上架版本有 32K 分档；官方人民币分档未核实 | |
| Kimi ⚠️ | ✓ | ✓ | 无 | ✓ | 据称不分档 | |
| MiniMax ⚠️ | ✓ | ✓ | 未查到 | ✓ | 未查到 | 官方价格页未找到 |

上游 usage 中缓存写入按时长拆分的字段：

- Anthropic（含 Bedrock Mantle 的 Anthropic 面）：`usage.cache_creation.ephemeral_5m_input_tokens`、
  `usage.cache_creation.ephemeral_1h_input_tokens`，二者之和 = `cache_creation_input_tokens`。
- OpenAI Responses：`usage.input_tokens_details.cache_write_tokens_details.ttl_30m_tokens`、
  `ttl_12h_tokens`，二者之和 = `cache_write_tokens`，两字段即使为 0 也会返回。
- OpenAI Chat Completions 的 `prompt_tokens_details` 是否带同样的拆分：**未核实**，见 T2。

### 1.3 当前不会产生费用、本方案不处理的项

- `speed:"fast"`、`inference_geo`：`anthropicapi.MessageRequest` 解码用了 `DisallowUnknownFields`
  （`internal/anthropicapi/types.go`），请求进不来；OpenAI 的 `service_tier` 在 Responses 被拒。
  **约束：将来放开这些参数，必须在同一变更里加上对应计价。**
- Gemini 显式缓存、火山方舟上下文缓存的存储费：Halro 不提供创建缓存的接口。
- 数据驻留 +10%：部署级差价，直接在该部署填更高的价格即可。
- 订阅类连接（Coding Plan / Token Plan 等）：包月，不按 token 计价。

## 2. 范围与分期

| 分期 | 内容 | 本文详细程度 |
|---|---|---|
| **P1（本期）** | 缓存写入计价，分短时、长时两档 | 任务级，可直接开发 |
| P2 | 按输入长度分档（整单换价） | 设计要点，需单独评审 |
| P3 | 服务端工具按次计价（联网搜索等） | 设计要点，需单独评审 |
| 待查 | Batch 当前按什么价格计费；MiniMax/Kimi/智谱/火山官方价格页 | 列为开放问题 |

## 3. 已定的决定

1. **历史数据补 0，不重新初始化数据目录。** Ledger 重放不会按价格快照重算金额（重算只在结算写入时发生，
   `budget/manager.go` 的 `settle`），所以旧事件里缺少的新字段解码为 0 是安全的。Parquet 已有按版本补 0
   的阶梯（`internal/usage/parquet.go` 的 `version < 4` 分支），沿用同一做法。
2. **生效中的价格版本不补 0，补输入价。** bbolt 里的 `DeploymentPriceVersion`、`PriceWindow`、
   `DeploymentPriceProposal` 如果补 0，升级后缓存写入变成免费，比现在还少收。迁移用该版本（或该时段）
   的输入价填两档写入价，升级后相同上游用量的结算金额不变，由管理员按需调高。理由与 ADR 0022 的 schema 30 迁移相同。
3. **两档而不是三档或按供应商命名。** Anthropic 是 5m/1h，OpenAI 是 30m/12h，结构相同：一档短时、一档长时，
   长时更贵。字段命名用 short / long，具体时长写在前端提示与文档里。
4. **写入总数保留，长时档作为它的子集。** 与"缓存读是输入的子集"的约定一致：
   `CacheWriteInputTokens` 仍是写入总数，新增 `CacheWriteLongInputTokens` 为其子集，
   短时 = 总数 − 长时。这样 Ledger/Parquet 只加一列，不改已有列的含义。
   上游只报总数、不报拆分时，全部记为短时。

## 4. 数据模型

### 4.1 Token 拆分（`semantic.Usage`）

```
InputTokens                     全部 prompt token
├─ CachedInputTokens            缓存读取
├─ CacheWriteInputTokens        缓存写入总数
│   └─ CacheWriteLongInputTokens   其中长时档（新增）
└─ 余下部分                      普通输入
```

校验（`Usage.Validate`）新增：`0 ≤ CacheWriteLongInputTokens ≤ CacheWriteInputTokens`。

### 4.2 价格字段

`DeploymentPriceVersion`、`PriceWindow`、`PriceTier`、`PriceSnapshot`、`DeploymentPriceProposal`、
`provider.Target` 各加两项：

| Go 字段 | JSON | 含义 |
|---|---|---|
| `CacheWriteMicrosPerMillion` | `cache_write_micros_per_million` | 短时写入单价（Anthropic 5m / OpenAI 30m） |
| `CacheWriteLongMicrosPerMillion` | `cache_write_long_micros_per_million` | 长时写入单价（Anthropic 1h / OpenAI 12h） |

Admin API 入参对应 `cache_write_usd_per_million`、`cache_write_long_usd_per_million`（十进制字符串，
与现有 `cached_input_usd_per_million` 同样走 `domain.ParseUSDMicros`）。

规则：

- 价格版本、提案、Admin 创建请求里**必填**，缺省即拒绝。这和 ADR 0022 对缓存读价格的处理一致：
  缺失不能被读成"免费"。
- Ledger 事件内嵌的 `PriceSnapshot` 中，新字段**允许缺失并按 0 解码**（§3 决定 1）。这是对历史事件的唯一宽容，
  新写入的快照一律带上这两个字段。
- `free` 计费模式下两项必须为 0；`metered` 模式"至少一个正数分量"的判断把两项纳入。

### 4.3 计价公式

```
  (input − cached − write) × input_rate
+ cached                    × cached_input_rate
+ (write − write_long)      × cache_write_rate
+ write_long                × cache_write_long_rate
+ output                    × output_rate
+ fixed_request
```

每个计价分量独立向上取整到 1 微美元。两档写入价均等于普通输入价时，三段合并后按普通输入价取整，保留旧价格的结算金额。`PriceCostBreakdown.InputCostMicrosUSD`
继续表示整段 prompt 的费用，不新增列；拆分可由快照和 token 数还原（ADR 0022 的同一理由）。

### 4.4 预留与保守计价

凡是还不知道 token 拆分的地方，整段 prompt 按
`max(input_rate, cached_input_rate, cache_write_rate, cache_write_long_rate)` 计价。预留及 Token Guard 上限还覆盖各档独立取整：

- `estimateReservation`（`internal/gateway/service.go`）
- `accountingTermsFromSnapshot`（`internal/gateway/service.go`）
- 崩溃恢复后按准备上限重算的租约（`budget/manager.go` 中 `event.PriceSnapshot.Calculate(event.PreparedInputTokens, 0, …)`）
- Token Guard 对请求的上限估算（如有引用价格的地方，开发时 grep 确认）

建议把这个"最高输入类单价"做成 `PriceTier` / `PriceSnapshot` 上的一个方法，各调用点共用，避免四处各写一份 `max`。

## 5. 计算逻辑只留一份

现在有两份相同的计价算术：`domain.PriceTier.Calculate` 和 `budget.EstimateCostMicros`，结算时二者结果
必须一分不差，否则结算被拒（`settlement cost does not match immutable price snapshot`）。
本期要给两边同时加两个分量，建议顺手让 `EstimateCostMicros` 委托给 `domain.PriceTier.Calculate`，
删掉第二份算术。如果评审认为不应在本期动，至少要加一个对两者做交叉比对的表驱动测试。

## 6. 任务拆分

每个任务列出要改的文件和验收标准。依赖顺序：T0 → T1 → T2/T3 可并行 → T4 → T5 → T6 → T7 → T8。

### T0 ADR 0028：缓存写入计价

- 新建 `docs/adr/0028-cache-write-input-pricing.md`，记录 §3 的四个决定、§4.3 公式、§4.4 预留规则，
  以及 P2/P3 作为已知少收缺口。注明它补全 ADR 0022 的"Cache writes are still unpriced"。
- 更新 `docs/README.md` 的 ADR 与 PRD 索引。

### T1 domain：价格类型与公式

文件：`internal/domain/pricing.go`、`pricing_schedule.go`、`pricing_cost.go`、`pricing_snapshot.go`、
`pricing_proposal.go`、`internal/semantic/result.go`

- `semantic.Usage` 加 `CacheWriteLongInputTokens`，补 `Validate` 和 `UncachedInputTokens`（不变，仍减写入总数）。
- 四个价格类型加两个字段，补非负、`free` 全零、`metered` 至少一个正数分量的校验。
- `PriceTier.Calculate`、`CalculateUSDTokensV1`、`PriceSnapshot.Calculate` 的签名改为接收一个 token 拆分结构体，
  不再继续追加位置参数。建议新类型 `domain.TokenUsage{Input, CachedInput, CacheWrite, CacheWriteLong, Output}`。
- `PriceSnapshot.Validate`：新字段缺失时按 0 处理（§4.2）；其余已知字段的完整性要求不变。
- 新增"最高输入类单价"方法（§4.4）。
- 提案摘要（digest）覆盖新字段。

验收：`go test ./internal/domain/ ./internal/semantic/` 通过；新增用例覆盖两档写入的取整、写入 = 0、
长时 = 总数、长时 > 总数被拒、`free` 模式非零被拒、缺字段快照按 0 解码。

### T2 adapter：解析写入时长拆分

文件：`internal/anthropicapi/types.go`、`internal/provider/anthropic/adapter.go`、
`internal/compatibility/anthropic/mapping.go`、`internal/gateway/service.go`（`nativeAnthropicUsage`）、
`internal/openaiapi/types.go`、`internal/openaiapi/responses.go`、`internal/compatibility/openai/mapping.go`、
`internal/compatibility/openai/provider_responses.go`、`internal/provider/bedrockmantle/adapter.go`、
`internal/provider/bedrock/adapter.go`

- Anthropic：解析 `usage.cache_creation.ephemeral_5m_input_tokens` / `ephemeral_1h_input_tokens`；
  长时 = 1h。流式路径里 `message_delta` 对 usage 的覆盖逻辑（`provider/anthropic/adapter.go` 中
  `CacheCreationInputTokens != 0` 那段）要同步处理 `cache_creation` 对象。
- OpenAI Responses：解析 `input_tokens_details.cache_write_tokens_details.ttl_30m_tokens` / `ttl_12h_tokens`；长时 = 12h。
- OpenAI Chat Completions：**先核实** `prompt_tokens_details` 是否带同样的拆分（读官方文档或用真实账号跑一次）。
  不带就全部记为短时，并在 ADR 里写明。
- Bedrock Mantle：Anthropic 面复用 Anthropic 解析；OpenAI 面复用 Responses 解析；Mantle 自有 Chat 面先核实。
- Bedrock Converse（withheld）：[AWS TokenUsage](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_TokenUsage.html) 已定义 `cacheDetails`。Halro 的 withheld profile 本期维持原有总数映射。
- 拆分之和与总数不一致时：以总数为准，长时截断到不超过总数，并记录一次 warning（不含任何请求内容）。
- 对外回写（Halro 作为 OpenAI / Anthropic 兼容面返回 usage 给客户端）时，同样输出拆分字段，保持与上游形状一致。

验收：每个 adapter 的 fixture 测试覆盖"只有 5m""只有 1h""两者都有""只报总数"四种情况。

### T3 Ledger / usage / 汇总：记录长时档

文件：`internal/ledger/event.go`、`internal/budget/manager.go`（`Settlement`）、`internal/usage/aggregate.go`、
`internal/usage/rollup.go`、`internal/usage/parquet.go`、`internal/app/admin_usage_summary.go`、`web/src/types.ts`

- Ledger 事件加 `ProviderCacheWriteLongInputTokens`（`json:",omitempty"`，旧事件缺省为 0）；校验长时 ≤ 写入总数。
- Parquet 行加 `provider_cache_write_long_input_tokens` 列，**提升 Parquet schema 版本**，并在版本阶梯里加
  "旧版本该列补 0"的分支（仿照 `version < 4`）。核对 manifest 版本门是"接受范围"而不是精确相等
  （CLAUDE.md：这个仓库在精确相等的版本判断上出过事）。
- 用量汇总与 Admin 用量接口暴露新字段。

验收：`go test ./internal/ledger/ ./internal/usage/ ./internal/budget/`；用旧版本 Parquet 分区做读取测试，新列为 0。

### T4 计价与结算接入

文件：`internal/gateway/service.go`、`internal/budget/manager.go`、`internal/provider/provider.go`、`internal/app/providers.go`

- `provider.Target` 加两项单价，`app/providers.go` 组装路由目标时从生效价格版本带过来；分时价位场景下从命中的时段取值
  （`service.go` 里 `priced.CachedInputMicrosPerMillion = *snapshot.…` 那段一并补齐）。
- `setSettlementCost`、`budget.TokenCost`、`EstimateCostMicros`：带上两档写入（或按 §5 改为委托 domain）。
- `budget.settle` 的快照复算传入写入总数与长时档。
- 预留四处改用"最高输入类单价"（§4.4）。
- `recordUsageTiers`：写入有价之后不再因写入设置 `CostEstimated`；改写函数注释，删掉"未来再重算"的说法。

验收：结算金额与快照复算逐微美元一致的测试覆盖两档写入；预留金额 ≥ 任一实际拆分下的结算金额（性质测试或表驱动穷举）；
`go test -race ./internal/gateway/ ./internal/budget/ -count=1`（结算路径涉及并发）。

### T5 元数据迁移（schema 42 → 43）

文件：`internal/store/bolt/store.go`、新增 `internal/store/bolt/migration43_test.go`

- `schemaVersion` 提升到 43，迁移名 `deployment_price_cache_write_rates`。
- 遍历价格版本、每个 `PriceWindow`、价格提案：两档写入价填为同一层级的输入价（§3 决定 2）。
- 提案重新计算摘要（与 schema 30 迁移一致）。
- 迁移步骤写审计记录，与已有迁移格式一致。

验收：迁移测试覆盖无时段价格、有时段价格、`free` 模式（写入价保持 0）、提案摘要更新；
**用真实 `data/` 的副本跑一次真实二进制**：`halro start` 能启动，`halro doctor`、`halro ledger verify`、
`halro usage verify` 通过（CLAUDE.md「Verify, never assume」）。

### T6 Admin API、配置导入导出、bootstrap

文件：`internal/app/admin_prices.go`、`internal/app/admin_configuration_bundle.go`、
`internal/app/admin_configuration_import.go`、`internal/app/bootstrap.go`、`cmd/halro/main.go`、
`internal/app/admin_ai_configuration.go`（AI 配置方案中如有价格项）

- 价格创建、时段价格、提案接口加 `cache_write_usd_per_million`、`cache_write_long_usd_per_million`，必填。
- 价格试算接口（preview）入参加 `cache_write_input_tokens`、`cache_write_long_input_tokens`，按时段分别输出。
- 审计 `ChangeSummary` 带上新字段。
- 配置包：导出带新字段；导入**旧版配置包**的处理见开放问题 Q1。
- bootstrap CLI：加 `-cache-write-micros-per-million`、`-cache-write-long-micros-per-million`，用 `*int64`，
  未指定时等于输入价（与 `-cached-input-micros-per-million` 的约定一致）。
- 更新 `docs/compatibility` 或 Admin API 契约快照（如有），以及 `internal/app/admin_contract_test.go`。

验收：`go test ./internal/app/ -run 'Price|Configuration|Bootstrap|Contract'`。

### T7 前端：模型部署页

文件：`web/src/pages/DeploymentsPage.tsx`、`web/src/pages/DeploymentsPage.test.tsx`、`web/src/types.ts`、
`web/src/api.ts`、`web/src/i18n/locales/zh-CN.ts`、`web/src/i18n/locales/en-US.ts`

- 价格表单加两个输入框：
  - 「缓存写入（短时）USD / 百万词元」，提示：「Anthropic 5 分钟缓存、OpenAI 30 分钟缓存的写入单价。未修改时跟随输入价。」
  - 「缓存写入（长时）USD / 百万词元」，提示：「Anthropic 1 小时缓存、OpenAI 12 小时缓存的写入单价。未修改时跟随输入价。」
  - 跟随规则与现有缓存输入字段一致；`free` 模式下提交 0。
- 时段价格编辑器、部署详情（`DetailFact`）、价格审阅文案（`clockPriceRates`）同步加两项。
- 用量页如展示缓存写入，增加长时档。
- 跑 `npm run build`，提交 `internal/webui/dist`。

验收：`npx vitest run src/pages/DeploymentsPage.test.tsx`；用例覆盖跟随输入价、手动修改后不再跟随、提交载荷含两个新字段。

### T8 文档与发布说明

- `docs/guides/operator-guide.md` 价格章节：说明两档写入、各家时长对应关系、升级后写入价默认等于输入价、需要手动调高。
- `CHANGELOG.md`：写明 schema 43、无需重新初始化、升级后计费不变。
- 本文状态改为"已实施"，并在 `docs/README.md` 索引中更新。
- 推送前跑一次 `make full-check`。

## 7. P2：按输入长度分档（设计要点，待评审）

- 维度：价格版本加可选的"长度档"列表，每档 `{min_input_tokens, 全套单价}`，阈值按 prompt 全长（含缓存读写）判定，
  整单换价（输入、缓存读写、输出全部换）。
- 与分时价位的关系：两个独立维度，组合方式需定（"时段 × 长度"二维表，还是每个时段内再嵌长度档）。
  先确认有没有同时需要这两个维度的平台；DeepSeek 只有时段，OpenAI/Gemini 只有长度，可能可以二选一。
- 预留：请求实际执行可能越过阈值，预留按可能命中的最高档计；快照记录"档位在结算时确定"，
  与分时价位"预留时定档"的规则不同，需要单独写清楚。
- 涉及平台：OpenAI（>272K）、Gemini（>200K）、Anthropic Haiku 5.5（>100K）、火山方舟（32K/128K/256K）、智谱（待核实）。

## 8. P3：服务端工具按次计价（设计要点，待评审）

- 只在连接声明了 `provider_executed_tools` 时才会产生。
- 需要新的"按次计价项"（如联网搜索每千次单价），以及从 usage 读取次数：Anthropic `usage.server_tool_use.web_search_requests`；
  OpenAI 需从输出中的 `web_search_call` 条目计数。
- 预留：工具调用次数事先不可知，需要一个每请求上限（例如连接或项目上配置最大调用次数），否则无法保守预留。
- 代码执行容器按时长计费，形状不同，建议继续在连接层拒绝，不纳入 P3。

## 9. 开放问题

- **Q1 旧版配置包导入（已定）。** 导入要求 v2。v1 需升级源实例后重新导出。
- **Q2 OpenAI Chat Completions 写入用量（已核实）。** 官方契约只列总数，Halro 按短时计费。
- **Q3 Batch 计价。** OpenAI、Anthropic 的 Batch 都是五折，Halro 目前 Batch 结果按什么价格结算未核实。
  如果按标准价，属于多收，需另开任务。
- **Q4 国内平台官方价格。** MiniMax、Kimi、智谱、火山方舟的官方价格页本次未读到原文，P2 设计前需要从控制台确认
  是否有长度分档、缓存写入或存储费。
- **Q5 两份计价算术（已定）。** 网关与结算统一使用 domain 计价公式（§5）。

## 10. 参考

- [OpenAI API Pricing](https://developers.openai.com/api/docs/pricing)
- [OpenAI Prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching)
- [Claude Pricing](https://platform.claude.com/docs/en/about-claude/pricing)
- [Claude Prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)
- [Gemini Developer API pricing](https://ai.google.dev/gemini-api/docs/pricing)
- [DeepSeek Models & Pricing](https://api-docs.deepseek.com/quick_start/pricing/)
- [Amazon Bedrock prompt caching](https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html)
- [火山方舟 模型价格](https://www.volcengine.com/docs/82379/1956278)
- [智谱 上下文缓存](https://docs.bigmodel.cn/cn/guide/capabilities/cache)
