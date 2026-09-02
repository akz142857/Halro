# 异步提交与延迟取回（Deferred Response）：设计与实施方案

- 状态：提案，未实施
- 日期：2026-09-02
- 文档语言：中文
- 适用范围：`POST /v1/responses` 北向面、`internal/gateway` 请求执行、`domain.ProviderResource`、
  `internal/limiter`、`internal/vault`、`data/provider-objects`
- 前置决策：本方案修订 ADR 0005「资源归属」一节中对 `background` 的拒绝，**必须先有一份新 ADR
  才能开工**；ADR 0005 自己写明了这一点（"A future stored tier requires a new ADR defining
  provider/deployment/profile/region binding, lifecycle, encryption, deletion, and failover
  behavior"）
- 数据迁移：需要一次 bbolt 迁移（新增记录字段与状态）与一次对象目录格式变更（明文改为密封）。
  实测本仓库 `data/provider-objects/` 为空目录，因此格式变更今天的迁移代价为零；见第七节

## 〇、决策摘要

业务方的诉求是「调用完立即结束，有结果再取」。**建议实现「异步提交 + 延迟取回」，不建议在第一阶段
实现结果回调（webhook push）。**

- 提交：`POST /v1/responses` 携带 `background: true`，立刻返回一个 `status: "queued"` 的
  Response 对象，连接随即断开，业务方不再等待。
- 取回：`GET /v1/responses/{id}` 轮询，得到 `queued` / `in_progress` / `completed` /
  `failed` / `cancelled`。终态携带完整输出与用量。
- 取消：`POST /v1/responses/{id}/cancel`；删除：`DELETE /v1/responses/{id}`。

三条不能松动的边界：

1. **轮询是权威。** 任何未来的通知机制（webhook、长轮询）都只是省掉轮询，不能成为结果的唯一投递
   路径。否则「推失败 = 结果消失」，直接违反 fail-closed。
2. **延迟取回不是会话状态。** `store: true`、`previous_response_id`、`conversation` 等 ADR 0005
   拒绝的其余字段全部保持拒绝。background 只是「同一次无状态生成的延迟取回」。
3. **这是一次数据分级变更，不是一个功能开关。** 见第一节。

## 一、为什么这不是一个普通功能

`internal/vault/vault.go:99-106` 对失败捕获的加密函数写着一句话，它是本方案最重要的约束：

> This is the only material Halro stores that a caller wrote — a prompt, tool arguments, an
> upstream error body

也就是说，今天整个实例里，**调用方自己写的内容只在一个地方落盘**：失败捕获。它默认关闭
（`internal/config/config.go:460-475`），有大小上限、每日条数上限、保留期上限，用主密钥密封并绑定
到 request 与 project，只能通过一次被审计的管理动作读取。而且只捕获失败——"A successful call is
never stored, which is what keeps this a small tail of traffic rather than a copy of it."

延迟取回会让**成功的请求的完整输出**也落盘，而且是常态而非尾部。这是 Halro 数据分级的一次实质变更。
因此本方案的存储部分必须逐条对齐失败捕获已经建立的那套纪律，而不是复用现有的
`provider-objects` 明文目录（见第七节 D7）。

## 二、当前实现证据（已核验）

### 2.1 已经存在的异步面

Halro 已经有一套「提交后轮询」的资源机制，但只覆盖上游原生就有异步能力的操作：

| 端点 | 归属 | 状态语义（endpoint-manifests.json） |
| --- | --- | --- |
| `POST /v1/batches`、`GET`、`/cancel` | 项目 | project-owned resource with 7 day TTL |
| `POST /v1/async/invocations`、`GET`、`/cancel` | 项目 | project-owned resource with 7 day TTL |
| `POST /v1/files`、`GET`、`/content`、`DELETE` | 项目 | project-owned resource with 30 day TTL |

路由注册在 `internal/app/runtime.go:1523-1532`。领域模型是
`domain.ProviderResource`（`internal/domain/models.go:26-32`，Kind 为 `file` / `batch` /
`async_invoke`），带幂等键哈希、请求指纹、`CreationStatus`、`ReservedBy`。到期回收由
`internal/app/provider_resources.go` 的 ticker 驱动，间隔 1 小时，扫
`Store.ExpiredProviderResources`（`internal/store/bolt/store_providers.go:526`）。

ADR 0021 已经允许一个资源**没有上游孪生体**（Anthropic 的 Message Batches 用 inline requests，
输入文件只存在于 Halro 本地）。延迟取回的记录正属于这一类：上游只收到一次普通的同步生成调用，
没有任何可供查询的句柄。**这条路已经被 ADR 0021 铺好了，不需要新的所有权模型。**

### 2.2 普通生成没有异步面

`POST /v1/chat/completions`、`POST /v1/responses`、`POST /v1/messages` 全部是同步的：要么等完整
响应，要么用 SSE 把连接占住。ADR 0005 明确拒绝 `background`：

> `previous_response_id`, `conversation`, `background`, prompt resources, metadata persistence,
> retrieval, deletion, cancellation, input-item listing, compaction/context management, and
> webhooks are unavailable.

实现上是干净的：`openaiapi.ResponseRequest` **没有** `background` 字段，
`DecodeResponseRequest` 拒绝未知字段（`internal/gatewayapi/handler.go:81`），所以
`background: true` 今天返回 400 而不是被静默忽略。只有输出侧的 `Response` 结构有
`Background bool`（`internal/openaiapi/responses.go:178`），恒为 `false`
（`internal/compatibility/openai/responses.go:273`）。

### 2.3 现有轮询的开销：每次 GET 都写 ledger

这是设计新端点时必须避开的坑。`GetBatch`（`internal/gateway/inference_resources_store.go:1012`）
把每一次轮询都当成一次完整请求走账：

```go
target.FixedRequestMicrosUSD = 0
err = s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
    result, callErr = adapter.GetBatch(ctx, requestID, resource.UpstreamID)
    ...
})
```

`accountedInferenceResources`（`internal/gateway/inference_resources.go:28`）会
`beginRequestRun` + `startAttempt` + `finish` + `finalize`，即按 ADR 0018 写满一个请求生命周期的
ledger 事件。钱接近零（固定请求成本被显式置 0），但**每轮询一次就写一组 WAL 帧并占一个 RPM 名额**。

对 batch 这是必要的：它必须打上游才知道状态。对延迟取回**不是**：结果已经在本地。

### 2.4 对象存储今天是明文

`writeResourceObject`（`internal/gateway/inference_resources_store.go:118-146`）把字节直接写盘：
临时文件 → `Chmod(0600)` → `Sync` → `Rename`。目录 `data/provider-objects` 权限 0700
（`internal/gateway/service.go:859-862`，路径见 `internal/app/runtime.go:471`）。

**没有任何加密。** 实测真实数据目录：

```
drwx------  2 ziy  staff   64  8月 17 16:14 provider-objects
```

存在、权限正确、当前为空。batch 的输入 JSONL 与结果 JSONL 都会以明文进入这里。这与 2.1 节里
`vault.EncryptFailurePayload` 对同类材料的处理标准不一致。

### 2.5 限流是一次原子准入

`limiter.Manager.Acquire`（`internal/limiter/manager.go:56`）在一次调用里同时判定 RPM、TPM、
并发，返回一个 `Lease`。同步路径下这个租约由 HTTP 请求持有，结束时 `Release`，中途用
`Reconcile` 用实际 token 数修正 TPM（`internal/gateway/service.go:368`）。

延迟取回会把「持有并发名额的实体」从 HTTP 连接换成后台 worker，这需要拆开这次原子准入。见 D5。

## 三、目标与非目标

### 目标

- 业务方提交一次生成请求后可以立即断开连接，稍后用同一个 id 取回结果。
- 不为此新增第二套账务权威、第二套路由、第二套鉴权。
- 结果在 Halro 落盘期间的保护标准，不低于失败捕获今天的标准。
- 崩溃、重启、取消、过期这四种情况下，业务方拿到的答案都是确定的，而不是「查不到」。

### 非目标

- **不做 webhook 推结果体。** 第二阶段最多做「状态变了，来拉」的瘦通知，见第十四节。
- **不做会话状态。** `store: true`、`previous_response_id`、`conversation` 保持拒绝。
- **不做流式的延迟取回。** `background: true` 与 `stream: true` 互斥；取回时也不重放 SSE 事件序列
  （那要求持久化事件流而非最终结果，是另一个量级的存储承诺）。
- **不做跨实例排队。** 单进程单写者边界不变（ADR 0001）；队列在进程内存 + bbolt 记录里，不引入
  外部队列。
- **不改 `POST /v1/chat/completions`。** OpenAI 在 Chat Completions 上没有 background 语义，
  自造一个会得到一个没有 SDK 覆盖的方言端点。

## 四、这份方案必须回答 ADR 0005 的六个问题

ADR 0005 为「有状态层」预设了六项必答内容。逐条对应本文档：

| ADR 0005 要求 | 本方案的回答 | 章节 |
| --- | --- | --- |
| provider / deployment / profile / region binding | 提交时解析一次路由并钉死在记录上，出队时不重新解析 | D3 |
| lifecycle | queued → in_progress → completed/failed/cancelled，终态后进入冷却期再回收 | 五、D6 |
| encryption | vault scoped AEAD，新 scope `deferred-response`，绑定 (resource_id, project_id) | D7 |
| deletion | 显式 `DELETE`、取回后冷却回收、TTL 到期由现有 reaper 回收 | D8 |
| failover | 单进程，无 failover；重启后 `in_progress` 一律失败并保守结算 | D9 |
| 与 provider 状态的关系 | 无上游孪生体（ADR 0021），上游只见到一次普通同步调用 | D2 |

## 五、接口设计

### 5.1 提交

```http
POST /v1/responses
Authorization: Bearer gw_...
Idempotency-Key: <可选>

{"model": "my-alias", "input": "...", "background": true}
```

立即返回一个标准 Response 对象：

```json
{
  "id": "resp_...",
  "object": "response",
  "created_at": 1756800000,
  "status": "queued",
  "background": true,
  "model": "my-alias",
  "output": [],
  "usage": null
}
```

`background` 字段在输出结构上**已经存在**（`internal/openaiapi/responses.go:178`），这次让它开始
说真话。

### 5.2 取回

```http
GET /v1/responses/resp_...
```

| status | 含义 | HTTP | 附带 |
| --- | --- | --- | --- |
| `queued` | 已受理，尚未取得并发名额 | 200 | `Retry-After` |
| `in_progress` | 正在调上游 | 200 | `Retry-After` |
| `completed` | 成功，`output` 与 `usage` 完整 | 200 | — |
| `failed` | 失败，`error` 说明原因 | 200 | — |
| `cancelled` | 被取消 | 200 | — |
| 记录不存在或已回收 | — | 404 | — |

轮询节奏由服务端给：响应头 `Retry-After`（秒）。不在 Response 对象里塞非标准字段——那会污染一个
有 SDK 契约的结构。

### 5.3 取消与删除

```http
POST   /v1/responses/{id}/cancel
DELETE /v1/responses/{id}
```

`cancel` 对 `queued` 是确定的（还没发上游，直接置 `cancelled` 并释放预留）；对 `in_progress` 是
尽力而为的（取消 worker 的 context，按 ADR 0011 保守结算，终态为 `cancelled` 且标注可能已在上游
产生费用）。`DELETE` 只删记录与对象，不影响已完成的账务。

## 六、设计决策

### D1：复用 OpenAI 的 background 语义，不自造端点

**决定。** 走 `POST /v1/responses` + `background: true`，配 `GET` / `cancel` / `DELETE`。

**理由。** 官方 SDK 已经有这套调用形状，`docs/contracts/adding-a-northbound-endpoint.md` 把
SDK 黑盒证据列为一类兼容性证据（`EvidenceSDKBlackBox`），自造端点拿不到这类证据。

**被否方案。** 新增 `POST /v1/deferred`。省事，但等于宣布一个只有 curl 会用的方言面。

**待核验项。** OpenAI 对 `background: true` 的成功提交返回的确切 HTTP 状态码与字段集，必须用钉住
版本的官方 SDK 对拍确认，而不是照本文档写死。这条进验证计划（第十二节）。

### D2：`background: true` 隐含 Halro 侧存储，`store` 仍然拒绝

**决定。** `store: true` 保持返回 4xx。`background: true` 单独即可成立。

**理由。** OpenAI 语义里 `store` 是「provider 替你存」。Halro 存的是 Halro 自己的对象，上游对此
一无所知（ADR 0021 的无孪生体资源）。让 `store` 通过，等于对调用方声称了一件不成立的事。这条要作为
一条 documented deviation 写进 manifest。

### D3：路由在提交时解析一次并钉死

**决定。** 提交阶段完成鉴权、源策略、Token Guard、模型别名解析、能力过滤、部署选择，把
provider / deployment / profile / region 写进记录（`ProviderResource` 上这几个字段已经存在，
`internal/domain/models.go:38-42`）。出队时不重新解析。

**理由。** 提交与执行之间可能隔着几分钟，期间部署可能被改或被停用。重新解析意味着业务方拿到的结果
来自一个它提交时并不存在的路由；钉死意味着一个已被删除的部署会让请求失败——**后者是可以解释的，
前者不是**。失败时错误码明确说明「提交时选定的部署已不可用」。

### D4：账务严格保持 ADR 0011 的事件顺序

**决定。**

- 提交阶段：只做**准入**——鉴权、CIDR、Token Guard、路由可解析、队列未满。**不写 ledger。**
- 出队阶段（打上游之前）：`ReservationCreated` → fsync → `AttemptStarted` → fsync → Provider I/O
  → `AttemptSettled` → `RequestFinalized`。与同步路径逐字一致。
- 取回阶段（`GET`）：**不建 requestRun、不建 attempt、不写 ledger。** 只做鉴权 + 归属校验 +
  读本地对象。

**理由。** ADR 0011 要求预留在 Provider I/O 之前落盘。如果把预留提前到提交时，一个排队 10 分钟的
请求就占住 10 分钟的日预算额度，且崩溃恢复要区分「排队中」与「已发出」两种未结算租约——凭空多一个
状态。把预留放在出队时，恢复状态机一个字都不用改。

代价是业务方在提交时不会因为超预算被拒，而是稍后拿到 `failed`。这个代价是可接受的：提交阶段仍然
可以做一次廉价的余额快照检查，把「明显已经超了」的请求当场拒掉，只是这次检查不是权威，也不写
ledger。

**必须避开的反例。** 不要照抄 `GetBatch` 对轮询走全套账务的做法（2.3 节）。业务方每 2 秒轮一次，
一小时就是 1800 组 WAL 帧，全是噪音，而且直接加速 `docs/todo/data-retention-plan.zh-CN.md` 描述
的那两条增长曲线。

### D5：RPM 在提交时扣，TPM 与并发在出队时扣

**决定。** 拆开 `limiter.Acquire` 的原子准入：

- 提交：扣 1 个 RPM 名额。**不**扣 TPM、**不**占并发。RPM 拒绝 → 429。
- 出队：扣 TPM + 并发名额。拿不到就退避重试，记录保持 `queued`。
- 完成：`Release`，并用实际 token 数 `Reconcile`。

**理由。** RPM 的语义是「每分钟请求数」。如果 background 提交不占 RPM，业务方就能用 background
绕开 RPM 上限——这是一个必须堵死的口子。反过来，并发名额的语义是「同时在跑的上游调用数」，排队中的
请求并没有在跑，占住并发会让队列深度被 `MaxConcurrency` 隐式限死（`MaxConcurrency=5` 时第 6 个
提交就 429），排队也就失去了意义。

**实现影响。** `internal/limiter` 需要一对分离的入口，而不是只有一个 `Acquire`。这是 limiter 的一处
真实改动，进实施切片 S2。**不要**在 gateway 侧用两次 `Acquire` 拼出来——那会重复扣 RPM。

### D6：队列有上限，队满 429

**决定。** 新增 per-project 的 `MaxDeferredQueue`（默认值保守，建议 100），队满时提交返回 429 并带
`Retry-After`。

**理由。** 无界队列是 fail-open：上游变慢时队列无声增长，直到内存和磁盘一起出问题，而每一个入队的
请求都对业务方承诺过「稍后有结果」。有界队列在压力下明确拒绝，业务方立刻知道。

### D7：结果密封落盘，且顺手修掉 `provider-objects` 的明文问题

**决定。**

1. 新增 vault scope `deferred-response`，绑定 `(resource_id, project_id)`，与
   `EncryptFailurePayload`（`internal/vault/vault.go:108`）同形。
2. **`writeResourceObject` 整体改为密封写入**，而不是只给延迟取回开一条加密支路。batch 的输入与
   结果对象一并密封。
3. 输入（prompt）只在 `queued` 与 `in_progress` 期间保留；`AttemptStarted` 之后一旦拿到上游答复，
   立即抹掉输入对象，只保留输出。

**理由。** 第 2 点是 CLAUDE.md 「pre-1.0：错的构造不得与替代品并存」的直接要求——如果明文落盘是错
的，就不能留着明文路径再加一条加密路径。实测 `data/provider-objects/` 为空目录，所以今天做这次
格式变更的迁移代价是零；晚做代价会随真实 batch 使用而上升。

第 3 点是因为提示词只在「还没发出去」的窗口里有存在的必要。上游答复之后再留着它，就是白白多存一份
调用方写的内容。

**迁移。** bbolt 迁移取 main 上的下一个未占用编号（当前最新为 33，
`internal/store/bolt/store.go:903`；注意 `feat/usage-retention` 分支已占用 34）。对象目录：识别不
出密封信封魔数的旧对象连同其记录一并回收，并在启动日志里报一次数量。

### D8：短 TTL、取回后冷却回收、显式删除

**决定。**

- 默认 TTL 24 小时（可配），显著短于 batch 的 7 天与 file 的 30 天。
- 首次成功取回终态后进入 15 分钟冷却期，冷却期内允许重复取回（覆盖业务方拉取失败后重试），之后回收。
- `DELETE /v1/responses/{id}` 立即回收。
- 回收由现有的 1 小时 reaper 完成（`internal/app/provider_resources.go`），不新增清理循环。

**理由。** 对齐 `DefaultFailureCaptureRetain = 24 * time.Hour`
（`internal/config/config.go:483`）。存的是同一类材料，保留期就不应该更长。冷却期而非「取回即删」，
是因为「HTTP 200 已发出」不等于「业务方已收到」。

### D9：`in_progress` 不跨重启存活

**决定。** 启动重放时：

- `queued` 且 `ReservedBy` 是已消失的实例 → 重新入队（还没打过上游，幂等安全）。
- `in_progress`（已写 `AttemptStarted`）→ **一律置为 `failed`**，按 ADR 0011 第 2 条保守结算，
  错误码明确说明「本次调用可能已在上游产生费用，结果无法取回」。

**理由。** 这是延迟取回与 batch 的本质差别，必须写进契约而不是留给用户猜。batch 在上游有句柄，
重启后可以继续查；一次普通的同步生成调用在进程死掉之后**没有任何句柄可查**——无法确认它是否完成、
是否计费、答案是什么。唯一诚实的行为是失败并保守计费。

`ProviderResource.ReservedBy` 就是为这件事存在的，`internal/domain/models.go:47-51` 的注释已经写明：
「a reservation still held by a process that is gone can never complete on its own」。

**必须在面向用户的文档里显著说明这一点。** 业务方需要知道：background 请求在 Halro 重启时会失败，
需要它自己重试。

### D10：第一切片不做长轮询

**决定。** 只出 `Retry-After`。`GET /v1/responses/{id}?wait=30` 形式的长轮询列为第二切片的可选项。

**理由。** 长轮询能把请求数从「每 2 秒一次」降到「每 30 秒一次」且降低延迟，收益是真的；但它要挂住
一个连接和一个 goroutine，需要一套与 `MaxConcurrency` 语义对齐的独立上限，否则就是绕过并发限制的
第二条路。先把状态机做对，再谈省请求数。

## 七、数据设计

### 7.1 领域模型

`domain.ProviderResourceKind` 新增 `deferred_response`。`ProviderResource` 复用既有字段：
`ProjectID`、`ProviderID`、`DeploymentID`、`PublicModel`、`ProfileID`、`Region`（D3 钉死的路由）、
`IdempotencyKeyHash`、`RequestFingerprint`、`CreationStatus`、`ReservedBy`、`Status`、
`ObjectPath`、`ExpiresAt`。

新增字段：

| 字段 | 用途 |
| --- | --- |
| `InputObjectPath` | 密封的输入对象，`AttemptStarted` 得到答复后清空 |
| `SubmittedAt` / `StartedAt` / `CompletedAt` | 状态机时间点，也是 `Retry-After` 的计算依据 |
| `RetrievedAt` | 首次成功取回时间，冷却期起点 |
| `AttemptID` | 关联 ledger，供审计与用量页反查 |
| `ErrorCode` / `ErrorMessage` | 终态原因，内容不含任何调用方写的材料 |

`UpstreamID` 保持为空——ADR 0021 已经允许无孪生体资源。

### 7.2 兼容性清单

`internal/compatibility/manifest.go:138` 会校验 `Method + " " + Path` 是否登记在北向 profile 的
`Methods` 里，所以四条新路由必须同时登记进 `builtinNorthboundProfiles`
（`internal/compatibility/profile.go:41`），并把 `ProfileOpenAIResponses` 的 `Revision` 从 1 升到 2。

profile ID 本身叫 `openai.responses.stateless.v1`（`internal/compatibility/profile.go:13`），加了
background 之后这个名字就不再准确。按 pre-1.0「fix in place」的规矩，**改名而不是并列一个新的**：
建议改为 `openai.responses.v1`，`StateSemantics` 由描述承载差异。这会改动
`docs/compatibility/endpoint-manifests.json`（20 条清单里的 1 条改名 + 3 条新增）。

## 八、安全边界

- 结果对象密封落盘，绑定 project 与 resource id：跨实例搬运或改名都打不开（复用
  `vault.encryptScoped` 的成熟形状）。
- 输出在写入前必须走与同步路径同一条出站脱敏权威；不得因为「稍后才返回」而绕过。
- 输入在拿到上游答复后立即抹除（D7）。
- 大小上限：单条记录的输入与输出分别设上限，参照
  `DefaultFailureCaptureMaxBytes = 64 << 10` 的思路分别限制，超限的请求在提交时就拒绝，而不是执行
  完再说存不下。
- 错误信息不得携带调用方写的材料（延续「no secrets in logs/errors」不变量）。
- **建议默认关闭，per-project 显式开启。** 理由同第一节：这改变的是「这个实例的数据目录里有什么」。

## 九、实施切片

| 切片 | 内容 | 交付判据 |
| --- | --- | --- |
| S0 | 写 ADR：修订 ADR 0005 的资源归属条款，回答第四节六问 | ADR 合入 |
| S1 | `writeResourceObject` 改为密封写入 + 对象格式迁移 + 旧明文对象回收 | batch 路径回归通过，启动能识别并回收旧对象 |
| S2 | `internal/limiter` 拆分 RPM 与 TPM/并发两个准入入口 | 现有同步路径行为不变（原子性由组合入口保证） |
| S3 | 领域模型 + bbolt 迁移 + `deferred_response` 记录读写 | 迁移正反向测试通过 |
| S4 | 提交路径：`background: true` 解码、准入、路由钉死、入队、返回 queued | 提交后立即返回，记录落盘 |
| S5 | worker：出队、扣 TPM/并发、走完整 ADR 0011 事件序列、脱敏、密封落盘 | 端到端拿到 completed |
| S6 | 取回 / 取消 / 删除三个端点，`Retry-After`，冷却期回收 | 状态机全路径覆盖 |
| S7 | 崩溃恢复：`queued` 重新入队，`in_progress` 失败并保守结算 | 见第十节 |
| S8 | 兼容性清单、profile 改名与 revision、`endpoint-manifests.json`、SDK 对拍 | 清单不变量测试通过 |
| S9 | 控制台展示与文档：用量页可见、`docs/guides` 说明重启行为 | — |

S1 与 S2 是独立的前置改动，可以先合入而不依赖本方案其余部分——这也是它们排在前面的原因。

## 十、验证计划

按 `AGENTS.md` 的分层原则，迭代期只跑受影响的包；合入前跑一次完整门禁。

- **单元。** 状态机全迁移路径；提交时超预算的快照拒绝；队满 429；`background` 与 `stream` 互斥；
  `store: true` 仍被拒。
- **账务。** 断言取回路径**不产生任何 ledger 事件**——这条要有专门的测试盯着，因为它正是
  `GetBatch` 走错的地方。断言出队路径的事件顺序与同步路径逐字一致。
- **崩溃恢复。** 在 `AttemptStarted` 之后、结算之前杀掉进程，重启后断言：记录为 `failed`、
  ledger 有保守结算、错误码说明可能已计费。按 CLAUDE.md 的反向验证要求，**先确认注入的故障确实生效**
  再断言测试能抓住它，并用 `-count=1` 跑。
- **加密。** 把一个密封对象改名到另一个 resource id 下，断言打不开。
- **回收。** TTL 到期后 reaper 清理记录与对象；冷却期内重复取回成功，冷却期后 404。
- **并发。** `go test -race ./internal/gateway/ -count=1`——worker 池、队列、limiter 拆分都触及
  共享状态，属于 CLAUDE.md 列出的「该跑 race」的改动。
- **SDK 对拍。** 用钉住版本的官方 OpenAI SDK 验证提交与取回的实际状态码与字段（D1 的待核验项）。
- **不做。** 不启用真实 Provider 冒烟（计费且默认关闭，见
  `docs/verification/provider-real-matrix.md`）。

## 十一、验收标准

1. 业务方提交后可在 1 秒内断开连接，随后凭 id 取回结果。
2. 每次轮询产生 0 条 ledger 事件、0 次上游调用。
3. 提交占 1 个 RPM 名额；排队中的请求不占并发名额；执行中的请求占 1 个。
4. 一次 background 请求的 ledger 事件序列与同一请求走同步路径时逐字相同。
5. 进程在请求执行中被杀，重启后该请求为 `failed`，且账务已保守结算，无悬挂租约。
6. `data/provider-objects/` 下不存在任何明文对象。
7. 输入对象在拿到上游答复后不再存在于磁盘。
8. TTL 到期、显式删除、取回后冷却，三条路径都能把记录与对象清干净。

## 十二、第二阶段：完成通知（仅在第一阶段稳定后评估）

如果轮询确实成了业务方的负担，再考虑推送，且必须满足：

- **只推信号，不推结果。** 载荷限于 `{id, status, project_id, timestamp}`。不含输出、不含用量明细。
  这样就绕开了脱敏重复执行、载荷体积、投递丢失即结果丢失三个问题。
- **轮询仍是权威。** 推送失败不影响结果可取回，不需要「保证送达」。
- **落盘队列。** 现有的 `internal/alert/dispatcher.go` 用的是内存 `chan Event`，进程重启即丢。
  告警可以丢，这个也可以丢（因为有轮询兜底）——但如果哪天要求不丢，就必须换成落盘队列，不能沿用。
  签名与重试退避的形状可以复用。
- **出口是新的攻击面。** 回调 URL 由业务方提供，是典型的 SSRF 放大器。必须走 per-project
  allowlist，默认拒绝私网地址（`internal/safetransport/transport.go:139` 的 `AllowedHosts` 与
  `:258` 的私网判定就是为此存在的）。放开私网需要单独的威胁模型复审。

**推完整结果体不在任何已规划的阶段内**，需要独立 ADR。

## 十三、不做什么

- 不做跨实例队列、不做多副本消费（ADR 0001）。
- 不把延迟取回的记录当成对话历史库——TTL 短、取回即进入冷却回收，是刻意的。
- 不为 background 单独建一套路由/鉴权/账务，一律复用同步路径的权威。
- 不在轮询路径上调用上游。
- 不保留 `openai.responses.stateless.v1` 这个名字的同时新增一个 background 变体（pre-1.0 就地
  修正，不并列两个构造）。

## 十四、开放问题

1. **默认开还是默认关。** 本文建议默认关闭、per-project 开启（第八节），但这会让「开箱即用」体验
   多一步。需要产品决定。
2. **worker 池的大小从哪来。** 当前建议由各项目的 `MaxConcurrency` 自然限制，进程侧再加一个全局
   上限。全局上限的默认值需要按 `docs/verification/standalone-capacity-baseline.md` 的实测数据定，
   不要拍脑袋。
3. **`Retry-After` 的取值策略。** 建议按已等待时长递增（首次 1s，上限 10s），但对推理模型的实际
   分布需要用真实数据校准。
4. **用量页与审计如何呈现。** 一次 background 请求在 ledger 里与同步请求无异，但它的墙钟时长包含
   排队时间。用量页要不要区分排队与执行，需要与
   `docs/todo/request-failure-diagnostics-plan.zh-CN.md` 的口径统一。
5. **是否顺带修正 `GetBatch` 的轮询账务。** 2.3 节指出的问题在 batch 上是既有行为。它是否算 bug、
   要不要在本方案内一并修，需要单独判断——改动会影响既有的用量读数。
