# Halro 支撑 Opsique 智能体并发的开发方案

- 方案起草于 2026-11-18，验收状态更新于 2026-11-19。批次 A 已通过本机联调，批次 B 已完成本机容量和单条真实备用请求验收。生产配置核对与模型质量评估待完成，批次 C 待实施。Halro 实现已分别合并。[批次 A #520](https://github.com/akz142857/Halro/pull/520) · [批次 B #521](https://github.com/akz142857/Halro/pull/521)
- 需求来源为 Opsique《Halro 需求：支撑 Opsique 智能体并发》v0.2，覆盖 H1–H7，并与 Platform P3、P8 联调。
- Halro 代码基线为 `v0.15.0`（`15fb372d`）。Platform 行为依据本地 `edd601e` 的模型客户端和 Temporal Activity，联调时以实际部署版本为准。

## 1. 交付目标与顺序

支持 10～15 名用户同时使用、业务峰值约 30 个在途模型调用。对话使用 OpenAI 标准档，遇明确的上游限流可在同一请求内切到 AWS。候选均因临时限流不可用时，Platform 在 Run 的剩余时间内按等待提示用原键重试。后台使用独立别名，优先 OpenAI Flex，其次 AWS。Halro 单进程容量验收以 60 个在途非流式 Chat 请求为准。

| 批次 | Halro 交付 | Platform / 运维交付 | 完成条件 |
| --- | --- | --- | --- |
| A · 限流恢复（H1–H3） | 解析和汇总等待提示，区分全部限流与其他不可用，支持已证明未执行的同键重试 | 模型客户端读取头，Temporal 使用实际等待提示 | 双目标 429、等待、原键成功恢复的端到端用例通过 |
| B · AWS 配置与容量（H4–H6） | 提供可复用的模拟配置、故障切换验证和容量测量 | 选定并验证 AWS 模型、项目限额、真实流量形状 | 模拟切换及 60 并发达标，AWS 真实评估通过后才启用对话备用路由 |
| C · 后台 Flex（H7） | 增加 OpenAI Chat 档位配置、计费证据、限流隔离、部署级等待响应头的配置 | P8 按运行来源选择后台别名，核对真实账号和代理超时 | Flex 价格正确、Flex 429 切 AWS、对话继续走标准档 |

B 的模拟和容量环境可以与 A 同时准备。C 复用 A 的限流恢复与 B 的 AWS 目标，待二者模拟验收通过后做联合验收。首期业务路径是同步、非流式的 `/v1/chat/completions`。Batch、后台 Responses、流式 Flex 和其他资源接口留在现有契约内。

## 2. 批次 A · 限流与同键恢复

### A1 · 统一解析上游等待提示（H1）

- 将 [Bedrock 的解析逻辑](../../internal/provider/bedrock/adapter.go) 提取为 `internal/provider` 的共享工具，通过注入或传入 `now` 测试 HTTP 日期。在 [OpenAI 适配器](../../internal/provider/openai/adapter.go) 的 `postJSON` 接入真实响应头，集中处理 429 和 503。检查同一适配器其它处理 HTTP 错误的入口，使用同一个响应头解析入口，同时保持各 profile 现有的错误分类。
- 支持十进制秒数和 HTTP 日期。有效延迟至少 1 秒、最多 24 小时，数字溢出时按上限处理。缺失、负值、无效日期和过期日期视为无提示。保留 [网关 HTTP 层](../../internal/gatewayapi/handler.go)向上取整的行为。首版只使用 `Retry-After`，等上游抓包证明能识别具体受限维度后再评估 `x-ratelimit-reset-*`。
- `Retry-After` 属于发出该响应的目标。仅当重试同一个目标时使用它，且要尊重该目标的准入状态与请求总时限。切换到另一个可用目标时继续走既有短退避策略。收到 503 时保留有效提示和原有网关错误分类，不把故障归成限流。
- 同步 Chat 执行路径与其他协议共用 `executeGenerate`。改动要检查共享调用方的行为，维护各自的错误外层及现有的流式状态边界。

### A2 · 汇总候选结果并区分错误码（H1、H2、H3）

在 [网关候选循环](../../internal/gateway/service.go)按 Deployment 记录最后一次有意义的结果。一次请求的归类基于同一份路由快照中，能满足操作、工具、输出格式和 token 上限的候选。建议从 `provider.Registry` 同次解析拿到可执行候选和用于解释的候选全集，继续复用能力过滤。前置准入过滤、`startAttempt` 时的竞态挂起、实际响应三处都要保留原因，避免用最后一个错误覆盖此前证据。读取准入状态不消耗半开探测名额。

| 候选状态 | 对外响应 |
| --- | --- |
| 有候选成功 | 返回成功，响应模型保持公开别名，记录实际成功的 Route 与 Deployment |
| 本轮各候选均明确临时限流，或仅受临时限流准入门阻挡 | HTTP 429，`type=rate_limit_error`，`code=provider_rate_limit`。从可用的等待提示中选最早恢复值 |
| 本轮各候选均返回临时 429 且无提示 | HTTP 429，不写 `Retry-After`。后续请求若直接被准入门拒绝且已知恢复时间，返回门内剩余等待时间 |
| 兼有故障、凭据失效、长期额度耗尽，或还有未尝试且状态未知的候选 | 保留现有故障区分与优先级。耗尽尝试预算时只归因于已知结果，避免把尾部 429 当成全候选临时限流的证据 |
| Halro 项目 RPM、TPM、并发或记账容量拒绝 | 保留现有独立错误码和状态。记账容量使用 `503 accounting_capacity_exhausted` |

在 [准入门](../../internal/routegate)为单个目标返回结构化阻挡原因和剩余时间。同一目标若被多个有效限流作用域挡住，以最后解除的时间为准，跨目标取最早时间。对带头的上游 429，准入门的本地指数退避与上游截止时间共同决定最早探测时刻。当前普通限流的 `MaxWindow` 是 1 分钟，继续用它封顶本地指数退避，另保留上游等待截止时间，最长 24 小时，避免准入比明确的等待提示更早解除。半开槽已占满而结束时间未知时不编造等待时长。解析与等待提示应随已有门状态持久化并在重启后正确恢复。

### A3 · 在明确拒绝后释放同一个键

修改 [同步推理幂等结算](../../internal/gateway/inference_idempotency.go)。新增内部处理结果，明确区分未发出、所有已发出的调用均有明确拒绝证据、已有成功、结果未知。由执行路径收集证据，只有解析到上游的明确限流响应且适配器能确认未执行时，才纳入首版放行条件。Platform 或 Halro 自身超时、连接提前断开、无法确认结果的 5xx，以及收到成功后解析、渲染或计账失败，都保留现有的结果未知保护。最终状态码、`Retryable` 或 `Ambiguous == false` 单独出现时都不能作为释放证据。

1. 继续在首次发送前持久化 `in_flight`，从首次发送到整条路由链结算期间维持同一份 claim。并发提交同键时，至多一个请求能推进到上游。
2. 所有已发出的调用都明确因临时限流未执行，且本请求已完成账本结算时，在返回 429 前按当前资源行的 `project_id`、ID、revision 和状态原子释放 claim。建议用 CAS 将现有记录转成可重试状态（如 `retryable_refused`），保持原请求指纹和 24 小时生命周期。相同指纹以 CAS 接管，异指纹仍返回 409。新状态接入 [资源幂等判定](../../internal/gateway/inference_resources_store.go) 和存储、复制路径，限定于同步推理资源。老状态、老记录继续按原语义读取。
3. 尝试失败零费用结算，最终请求失败仍按现有账本形态记录。写入可重试状态失败时沿用保守的 `in_flight` / `unknown`，返回可诊断的存储故障。进程在发出请求后、保存明确拒绝结果前退出时同样保留 in-flight 标记。成功的键仍是 `idempotency_completed`，网关继续只保存指纹和状态。
4. 检查存量二进制或 HA 节点解码新增状态时的行为、资源行复制和晋升。确认下一次接管能看到同一 revision，并覆盖写入响应丢失、过期重用、同键竞争。存量 `unknown` 无法补出拒绝证据，保持原语义。

### A4 · Platform 读取并执行等待提示

Platform 修改 `services/platform/internal/agentcore/model/chat.go`，在错误对象中携带解析后的 `RetryAfter`、状态、错误码和请求 ID。`runtime/activities/botrun.go` 把可用延迟带进 Temporal SDK 的 `ApplicationErrorOptions.NextRetryDelay`，保留可重试与永久失败的分类。只在上游建议值之后加少量抖动。缺失提示沿用现有指数退避。以 `Command.DeadlineAt` 和工作流的收尾预留计算剩余窗口。等待后已无法开始下一次 Activity 时，返回可诊断的限流终态。

修改 `runtime/activities/chatmodel.go` 中按尝试次数换用 `/retry-1` 的逻辑。业务步骤从原 `CallKey` 开始。明确临时 429、结果未知的 5xx 和 `idempotency_in_progress` 都保持当前键，直到成功或达到 Run 时限。只有网关明确给出 `idempotency_completed`、且 Platform 确认当前业务步骤尚未持久化时，才走现有的已完成答复恢复分支。该分支继续使用同一个固定后缀，重试不会派生更多键。联调覆盖重试超过现有换键阈值的场景，以实际 Temporal Activity 和真实网关 HTTP 入口验证等待与键值。

## 3. 批次 B · AWS 备用与容量

### B1 · 配置和模型评估（H4）

1. 记录 Platform 实际的 OpenAI 模型 ID、当前别名、典型和高位上下文长度、AWS 区域、可用模型与配额。用已接通的 Bedrock Mantle 账号核对候选列表，优先评估同系列 GPT。依 [AWS 接入指南](../guides/aws-surface-selection.md)为 `/openai/v1` 模型选 `bedrock.mantle.openai.chat.v1`。截至该基线，Halro 的 Runtime Converse profile 仅声明文本对话能力。
2. 通过管理员配置创建 AWS Deployment 并补齐版本化价格。批准价格前核对输入、缓存输入、缓存写入和输出的计费口径。验证每个目标的 token 上限和能力证据都能覆盖 Platform 请求。模拟一次 OpenAI 429，核查 AWS 候选被选中并正确结算，且 OpenAI 拒绝尝试的费用为零。
3. 使用 Platform `services/platform/internal/app/eval` 和代表性业务数据对比候选与主模型。至少覆盖有工具调用、多个工具结果回填后的下一轮、长上下文、最终 JSON 和出错时的修复轮。按组记录用例通过率、工具参数及调用 ID 合法性、延迟与成本，由 Platform 确认阈值与结果。当前客户端发 `model`、`messages`、`tools`。如果验收要求上游严格 JSON Schema，临时测试还要显式发送 `response_format` 并验证模型、Halro profile 和工具 Schema 的组合。
4. 对话别名用 `ordered`，给主 OpenAI 较小的 `priority`，已评估的 AWS 排在后面。逐一检查现网尝试预算。第二目标可达的基本条件为 `max_total_attempts > max_attempts_per_target`，`4/1` 和 `3/2` 都满足，`4/1` 会更早切换。能力过滤、预算、部署并发和准入挂起仍会影响候选是否能执行。检查 Token Guard 按所有可用候选的最高预估成本准入，以及价格窗口跨越时的临场重校验。别名用于 Chat，图片、音频、批处理和文件继续使用各自适用的路由。

### B2 · 项目配置（H5）

在 Platform Project 的当前管理配置中核对 `rpm`、`tpm`、`max_concurrency`。三个字段各自设 `0` 表示该维度不限制。保留上限时，并发至少 60，RPM 至少 300，TPM 依据实际 OpenAI 项目和模型的限制及 Halro 请求预估调整。压测前单独记录入口来源 RPM、Provider / Deployment 并发上限、预算和 Token Guard。普通同步 Chat 经过 Project 限额，模型列表和资源平面的内置 Key / Project RPM 上限适用各自路径。

### B3 · 容量测量（H6）

在接近生产的单进程环境使用真实 Halro HTTP、鉴权、限额、准入、幂等和持久化记账。将两个上游换成能记录每个请求起止时间、注入延迟和配置响应码的 HTTP fixture。负载使用不同的幂等键，覆盖代表性请求体与长上下文。不以独立的网关代码基准代替这次集成测试。

1. 先运行 20 个会话同时发消息的业务场景，记录峰值模型并发、成功率和按错误码分组的 429。再用负载工具持续维持 60 个在途请求，记录系统实际的并发。用快速成功、分钟级等待、以及少数请求持续到配置超时边缘的三组可复跑负载。
2. 按请求 ID 对齐客户端总耗时及各次上游 HTTP 调用耗时。逐请求先扣除上游实际耗时，再在每个负载组计算 Halro 增量 p50、p95、p99。将模拟上游部署在同机或独立记录网络开销。健康组 p95 必须低于 200 ms，切换组另外报告退避、目标切换和第二次调用。保留负载脚本、样本数、机器、拓扑、数据目录规模和 commit；当前的平均值指标不足以证明 p95。
3. 检查 Halro 自身的拒绝数、WAL 写入和结算等待、连接复用、预算租约及进程资源。通过条件包括健康组无项目限额 429，所有已接收请求完成记账，压测收尾后的活跃与待结算租约回落。若超阈值，先分解客户端到 Halro、网关准入、等上游连接、账本预留与结算的耗时，再根据实测改最慢环节。

## 4. 批次 C · 后台 Flex

OpenAI 文档确认同步 Chat Completions 支持 Flex，资源不足返回 429 且该请求不收费。具体型号以价格页和账号可用性核实。Chat API 的响应档位可能与请求档位不同，联调应取得去敏的响应档位、用量及 429 状态和错误码。小量真实调用须由操作者显式授权。 [Flex 文档](https://developers.openai.com/api/docs/guides/flex-processing) · [OpenAI 价格页](https://developers.openai.com/api/docs/pricing) · [Chat Completions 契约](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)

### C1 · Deployment 配置和上游请求

- 在 [Deployment](../../internal/domain/models.go)和 [管理 API](../../internal/app/admin_deployments.go)增加可选 `service_tier`，取值为 `default` 或 `flex`。存量缺省值与创建时省略均保持现有行为。更新时省略保留已有配置，显式清空回到缺省行为。服务端校验部署绑定官方 OpenAI 的 `openai.chat-embeddings.v1` profile，配置档位的 Deployment 只承接首期已验收的非流式 Chat。过滤候选时检查北向入口，覆盖使用同一 primitive 的 Responses、Messages、流式和 deferred 请求。`default` 用于显式请求标准档。用独立 Deployment 承载不同档位，Route 仍引用 Deployment。
- 从 [拓扑装配](../../internal/app/providers.go)把配置放入目标快照，经 `provider.GenerateCall`、`provider.ChatCall` 传入 [OpenAI Chat 适配器](../../internal/provider/openai/adapter.go)，在那里写入上游 JSON。适配器单独读取响应档位，再经 `semantic.GenerateResult` 或等效的内部调用结果带回计费。北向 Chat 输入保持严格解码，调用者无需也不能改选路由的档位。切换 AWS 时用 AWS 目标自己的请求参数。
- 配置校验、测试连接或部署验证、保存、拓扑热装配、配置导入导出和管理员界面都使用同一配置语义。用带明确模型、档位和证据来源的校验覆盖 Flex 配置，模型可用列表仍从上游枚举。目录证据只用于能力核对。测试和探针请求必须使用部署所声明的档位，或明确记录自己只检查了基础连接。

### C2 · 按实际档位记账

使用独立 Flex Deployment 的版本化定价。建议把标准费率和可选 Flex 费率放在**同一价格版本**中，分别存输入、缓存读、两档缓存写、输出和固定费，并随版本保存价格来源。标准 Deployment 和历史版本继续使用现有字段。每个 Flex 目标在调度前须有两组费率，结算可处理上游明确返回的 `default`。首期 Flex 使用固定费率，价格表单与配置导入校验同一个价格版本不能同时给 Flex 声明分时费率。

| 证据 | 结算与记录 |
| --- | --- |
| 上游确认 `flex` 且用量有效 | 按已钉住的 Flex 费率结算，记录请求档位和实际档位 `flex` |
| 上游确认 `default` 且用量有效 | 按同一版本已钉住的标准费率结算，记录实际 `default` 和档位不符，返回已有回答 |
| 上游省略响应档位或返回首期无法识别的档位 | 实际档位记为 `unknown`，在已知两组费率中对该笔用量取较高估值，标 `cost_estimated`，保留可对账证据。若已知档位但缺有效用量，按已选档位和现有用量估算规则处理。账号验收要求实际返回可确认的 Flex 档位 |
| 明确 429 并切换到 AWS | OpenAI 拒绝尝试按零费用结算，AWS 按独立 Deployment 价格结算，AWS 用量不挂 Flex 标签 |
| 结果未知或进程在上游执行期间退出 | 用同一 lease 中的已钉住费率按保守的恢复策略处理，保留估算标记和幂等保护 |

- 在 [价格快照](../../internal/domain/pricing_snapshot.go)加可选 Flex 费率组并保持老快照编码、digest 和历史已结算金额。预留预算和 Token Guard 对每个目标按两组已知费率的较高预估值核算，随后仍在全候选之间取最大。选择档位时只读准备阶段的快照，价格修改和窗口跨越不会改变已发出尝试。
- [Accounting](../../internal/budget/manager.go)在结算时验证由真实档位或估算规则选出来的费率及算式。[Ledger](../../internal/ledger/event.go)写入有界枚举，保证结算与预留仍指向同一组不可变证据。为 Usage 明细、汇总、Parquet / NDJSON 与控制台增加可核对的档位信息，历史行显示未知或留空。使用实际 Route、Deployment 和价格版本关联主备费用。同一用途的值在指标中使用低基数枚举，敏感上游数据继续走现有记录边界。
- 兼容性验证覆盖 metadata schema 43 的备份升级、准备未结清租约的恢复、Ledger 重放、历史导出和 HA 复制。可选字段能安全解码时沿用现有格式。若新快照或状态的语义要求同版本节点，就先确定支持的升级顺序和旧版本拒读机制，再允许写入。

### C3 · 隔离 Flex 容量拒绝

当官方 OpenAI Flex 明确以 429 拒绝容量请求时，沿用可换目标判定，再使用 Deployment 作用域，或在现有 `credential_model` 之外加档位作用域。建议首期使用 Deployment 作用域，验证同 Deployment 在重启后仍保持挂起。分类应使用实际去敏响应或对应 Chat API 的上游证据。缺乏区分证据的 429 保守沿用相应的共享限流分类，明确的账号配额、凭据无效和共享 RPM / TPM 仍用它们真实的作用域。正常标准请求的成功不能清掉 Flex 的容量挂起。

上线前核对两个档位的账号、项目、模型限额和是否共享 OpenAI 配额。独立别名只决定 Halro 的选路。若真实响应无法区分 Flex 容量拒绝与共享限流，需要使用已验证的独立限额范围才能验收隔离。若账号配额共享且压测显示互相影响，先调整上游配额安排或后台并发上限，再提高后台流量。

### C4 · 时间预算与后台启用

当前全局 `attempt_response_header_timeout` 为 5 分钟，`route_total_timeout` 为 10 分钟，而 Platform 单模型调用上限为 8 分钟，并受 Run 剩余时间约束。为 Flex Deployment 增加可选的 `attempt_response_header_timeout` 字段，并使用该目标专用的传输配置。联调设为至少 8 分钟（预发布建议 9 分钟），校验它不超过 `route_total_timeout`。全局总时限、入口代理、负载均衡和 HTTP 客户端须覆盖 Platform 的 8 分钟。连接超时和标准档等待响应头沿用各自设置。在拓扑装配时创建或复用 transport，刷新替换的连接关闭空闲连接。

后台别名按 `ordered` 配 `OpenAI Flex → AWS`，对话别名按 `OpenAI default → 已批准的 AWS`。P8 将例程、摘要和应用事件路由到后台别名，用户对话继续使用对话别名。要求在提前收到明确 Flex 429 且还有时间时切到 AWS。Platform 取消或 Flex 超时后结果可能未知，原键继续受结果未知保护。若产品还要求长时间等待后保证有足够时间完成 AWS 调用，应额外约定端到端 Run 预算和受支持的明确拒绝机制。仅缩短 Flex 超时无法证明上游未执行。

## 5. 验收矩阵

| 用例 | 怎样测 | 通过条件 |
| --- | --- | --- |
| A-01 · 上游提示 | OpenAI HTTP fixture 分别返回 429 + `7`、HTTP 日期、无头、坏值、超上限数字，另测 503 含头 | 秒数和日期正确，缺失与无效不生成提示，超上限封顶，保留 503 的上游错误分类 |
| A-02 · 多目标限流 | 双目标分别给 `7 → 3`、`3 → 7`、一个有头及两个无头 | 依次返回 429 和 `3`、`3`、唯一有效值、无头；`type` 与 `code` 一致 |
| A-03 · 准入与混合故障 | 预挂起双目标，覆盖重启、操作能力过滤、半开并发与首个目标 429 后另一目标被门拒绝；另混入故障、凭据失效或预算耗尽 | 全临时限流为 429，等待时长以同一目标实际恢复时间为准。其他原因保持准确分类，必要时无头 |
| A-04 · Platform 原键 | 双上游先明确 429，等待到一个恢复后发同键，并重跑竞争、崩溃、存储写入失败和不确定的 5xx | 无早于提示的 Activity 重试。同一请求用原键成功，只有合法接管能发往上游。不确定结果仍被保护，费用不重复 |
| B-01 · 对话备用 | OpenAI 429 带长提示，AWS fixture 返回工具调用和第二轮成功 | 请求在对话预算内到达 AWS，HTTP 200，按 AWS Route 与价格结算，OpenAI 429 零费用 |
| B-02 · 容量 | 20 会话业务场景与 60 个在途的控制负载，均带不同键 | 无项目限额 429，健康组的 Halro 增量 p95 小于 200 ms，最终用量完整、租约回落 |
| C-01 · Flex 档位与价格 | 截获上游 JSON 和响应，分别返回 `flex`、`default`、缺失、未知档位；中途更新价格后重放 Ledger | 线上请求端写 `flex`，账本和用量按已准备快照正确分类和计价，旧数据重放金额一致 |
| C-02 · Flex 与标准隔离 | 同凭据同模型的两个 Deployment，Flex 返回容量 429，标准返回 200；另测真实共享限流形态 | 后台同请求落到 AWS，标准 Deployment 仍可用。共享限流按真实作用域处理 |
| C-03 · 端到端长等待 | Fake clock 单测 8 分钟边界，预发布部署用延迟返回的本地上游贯穿 Platform、代理、Halro | 等待超过 5 分钟且在调用时限内的 Flex 请求可完成。取消能传递，模糊超时后无重复执行 |
| C-04 · 管理和数据兼容 | 新旧 Deployment、配置导入导出、价格提案、重启、恢复和发布支持的 HA 升级组合 | 默认请求形状正确，Flex 字段完整往返，价格不可误覆盖，恢复后账本与报表一致 |

实施时按变更运行 `go test -count=1` 的定向测试。重点包包括 `internal/provider/openai`、`internal/gateway`、`internal/gatewayapi`、`internal/routegate`，以及 Flex 涉及的 `internal/domain`、`internal/budget`、`internal/ledger`、`internal/usage`、`internal/app` 和实际改动的存储或复制包。幂等竞争、准入隔离和传输生命周期的改动对受影响包跑 `-race`。最终推送前跑一次仓库要求的完整门禁。控制台若改动，更新并提交 `internal/webui/dist`，按仓库规则验证生成结果。

## 6. 发布、观察与回滚

1. A 先部署 Halro，再开启 Platform 按提示的同键恢复。小流量观察每个错误码、同键 409、切换成功率、账本结算错误及 AWS 用量。模拟验收和真实账号证据标记为两个阶段。
2. B 的 AWS 路由在模型评估通过后配置，保留单目标配置快照作为回滚依据。C 先创建隔离的后台别名试跑一类低频任务。按同版本的对话基线比较标准档成功率、延迟和 429，核对 Flex 实际档位、估算差异和 AWS 切换量。
3. AWS 出现不兼容时停用对应第二目标。Flex 出现持续的档位偏差、计账差异或影响对话时，暂停 Flex Route，后台别名保留已批准的 AWS。若 AWS 也不可用，由 Platform 暂缓这批后台任务。保留已写的价格、用量和审计证据。回退二进制之前用升级验证结果判断现有数据是否能由旧版读取，必要时先回滚路由配置再用兼容版本修复。
4. 随实现更新 [幂等契约](../contracts/idempotency-contract.md)、[网关契约](../contracts/gateway-correctness.md)、兼容性清单及中英文受影响的使用和运维文档。按 [网站发版规程](../verification/website-docs-release.md)基于确切的已合并 Halro release commit 准备网站双语文档和 API 合同，给演练和正式发布传同一个 `website_docs_commit`。发布后合并、部署网站并通过 `website-acceptance`，再记录完整发布验收。

实施前由 Halro 与 Platform 共同确认的外部输入为实际 OpenAI 模型和配额、Bedrock 账号和区域、主备模型评估阈值、Flex 价格和真实档位响应，以及所有入口的超时配置。上述外部输入不影响 A 的模拟开发和 B 的容量基线准备。

## 7. 批次 A 本机验收记录

在 Halro `codex/opsique-rate-limit-recovery` 与 Platform `codex/halro-rate-limit-recovery`（基线 `db758945`）上，运行本机 Temporal Server 1.32.0、实际 Platform Worker 和 Halro HTTP 网关。两个回环上游依次返回 `Retry-After: 7` 和 `Retry-After: 3`。最终复跑中，Platform 收到 429 后等了 3.035 秒，用同一 `Idempotency-Key` 重试并收到 200，BotRun 成功交付。Halro 的 bbolt 保留一条完成的幂等记录，Ledger 有两次零费用拒绝和一次由第二目标结算的成功。Temporal 历史检查通过，凭据与答复正文未写入历史。

- Halro 入口为 `HALRO_RATE_LIMIT_FIXTURE_READY_FILE=<临时文件> go test -count=1 -v ./internal/app -run '^TestServePlatformRateLimitRecoveryFixture$'`。测试网关将回环地址、别名和一次性本地测试 Key 写入权限为 `0600` 的 JSON。 [Halro 验收夹具](../../internal/app/rate_limit_recovery_test.go)
- Platform 在 `services/platform` 运行 `go test -count=1 -v ./internal/runtime/workflows -run '^TestBotRunOnGatewayRateLimitRecovery$'`。按该仓库 Platform README 注入本机 Temporal 地址和 namespace，并从 JSON 读取 `HARNESS_RATE_LIMIT_TEST_BASE_URL`、`HARNESS_RATE_LIMIT_TEST_NAME`、`HARNESS_RATE_LIMIT_TEST_API_KEY`。真实 Temporal 需支持 `NextRetryDelay`，服务端最低版本为 1.24.2。
- Halro 通过 `go test -count=1 ./...` 和网关、准入门、Provider 包的竞态检查。Platform 通过 `go vet ./...`、`go test -count=1 -race ./...`、既有 `TestBotRunOnTemporal` 和 38 条工作流历史重放检查。
- 批次 A 的验收使用测试 Host、步骤存储和模拟上游。批次 B 的容量和单条真实调用结果见下节。

## 8. 批次 B 本机验收记录

2026-11-19，使用合入批次 B 的 `b430a6f3` 构建本地 Docker 实例。该实例的 `harness` 项目设为 300 RPM、2,000,000 TPM、并发 60。容量和单条真实调用均通过本地配置副本的 Halro HTTP 入口。

| 验收项 | 结果 |
| --- | --- |
| 60 个在途请求 | 模拟上游，持续 65 秒保持 60 个在途请求。300 个请求全部成功并结算，项目限额 429 和待结算租约均为 0。按请求扣除上游耗时后，各负载组的 Halro 增量 p95 最高为 140.4 ms。 |
| 单条真实备用请求 | 主路由 `gpt-6.1-sol` 模拟返回 429，Halro 在同一请求内切到 Bedrock Mantle `us-east-2` 的 `mistral.mistral-large-3-675b-instruct`，返回 HTTP 200 和带合法参数的客户端工具调用。主目标结算 USD 0，备用按上游报告的 106 输入、16 输出 token 结算 USD 0.000077。 |

本地对话别名 `gpt-sol` 采用有序路由，主目标优先级 10，Mistral Large 3 优先级 20。此前候选 `openai.gpt-5.6-sol` 的真实调用因地区访问要求被 AWS 拒绝。Mistral Large 3 的 32K 输出上限覆盖本地项目的 16,384 token 上限。[AWS 模型文档](https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-mistral-ai-mistral-large-3.html)

待验收项包括生产 AWS 模型与配额、生产 Platform 项目的 RPM、TPM 和并发上限，以及代表性业务数据上的多轮工具结果回填、长上下文和最终 JSON 质量。
