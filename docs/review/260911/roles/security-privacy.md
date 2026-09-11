# R3 安全与隐私评审

## 结论

- 评审基线：`v0.7.1..222d08f84f61`（`main`，`v0.7.1-12-g222d08f`）
- 评审时间：2026-09-11 CST
- 工具链：`go1.26.6 darwin/arm64`
- R3 建议：**NO-GO**。确认 1 个 P1、1 个 P2；P1 未修复或未从 capture 中彻底排除 Provider credential 前，不建议进入 v0.8.0 发布候选。
- 本轮只使用临时目录、假 Provider、假 sealer 和仓库内测试设施；未读取 live `data/`，未访问真实 Provider、内网、KMS，也未修改产品代码。

不变量结论：

| 不变量 | 结论 | 摘要 |
| --- | --- | --- |
| INV-08 | **失败** | headers/Gateway Key 没有进入记录的调用路径，但上游错误 prose 可把 Provider credential 带入 capture，并可由 `read_only` Admin 读取 |
| INV-09 | 通过 | 三份正文独立截断；精确读时 TTL；成功、caller abandoned、策略拒绝不落正文 |
| INV-10 | 通过 | 标准化 reason 不改写 adapter 给出的 `Retryable` / `Ambiguous`，结算和 fallback 使用的仍是结构化语义 |
| INV-13 | 通过 | BigModel 新 endpoint 仍经 URL 校验、按实例 host allowlist、DNS 后地址校验、禁代理和禁重定向 |
| INV-14 | **失败** | `max_records_per_day` 是进程内计数；同一天重启会重新获得完整额度，实际文件数和公开指标均可超过/低报日上限 |

## 外部入口到落盘、读取与出网的完整链路

1. Gateway HTTP handler 先从 `Authorization` 单独取 Gateway Key，再以 `http.MaxBytesReader` 限制正文并只把**解码后的 body**放进 request context：`internal/gatewayapi/handler.go:22`、`internal/gatewayapi/handler.go:74-92`、`internal/requestmeta/inbound_request.go:7-25`。这里没有把 headers 放入 capture context。
2. 请求通过鉴权、策略和 accounting admission 后，`beginRequestRun` 保存规范化请求以及 context 中的原始 Gateway body：`internal/gateway/service.go:388-429`。capture disabled 时三个保存函数立即返回：`internal/gateway/capture.go:60-92`。
3. OpenAI-shaped adapter 把上游拒绝句子放入 `provider.Error.Message`：`internal/provider/openai/adapter.go:1004-1048`；Anthropic-shaped adapter 读取最多 1 MiB 错误正文并把 `error.message` 原样放入同一字段：`internal/provider/anthropic/adapter.go:782-847`。
4. attempt 结束时，`captureProviderFailure` 把该 Message 组装为 capture `response.body`：`internal/gateway/service.go:756-762`、`internal/gateway/capture.go:94-127`。终态只有 `provider_error` 和 `unsupported_feature` 可写；策略拒绝、一般拒绝和 accounting error 被排除：`internal/gateway/capture.go:19-58`。
5. request close 释放 accounting/策略/Token Guard lease 后同步调用 `writeCapture`：`internal/gateway/service.go:460-487`。记录包含真实 Project、三份正文和有界失败标识符：`internal/gateway/capture.go:129-177`。
6. store 分别截断三份正文，以 `(request ID, project ID)` 派生 AEAD scope，并以 `0700/0600` 权限原子写文件：`internal/failurecapture/failurecapture.go:48-53`、`internal/failurecapture/failurecapture.go:231-299`、`internal/vault/vault.go:100-114`、`internal/vault/vault.go:144-200`。
7. Admin 读取入口是 `GET /admin/api/v1/usage/failures/{requestID}/payload`，挂在通用 `requireAdmin` 而非 mutation/administrator gate：`internal/app/runtime.go:1756-1760`。middleware 验证 session、按配置要求 MFA，并在每次请求从 store 刷新角色：`internal/app/admin_session.go:258-297`。
8. handler 从文件名取得真实 Project，再用 `(request ID, project ID)` 解密；在返回 body 前写入审计，审计不可用时 fail closed：`internal/app/failure_capture.go:74-149`。`Get` 在精确 TTL 边界拒绝读取：`internal/failurecapture/failurecapture.go:429-477`。
9. Provider 管理入口先校验 URL、产品 surface/region、credential audience 和 profile，再把 endpoint hostname 固化为实例 allowlist：`internal/app/admin_providers.go:1097-1165`、`internal/app/admin_providers.go:1193-1229`、`internal/app/admin_providers.go:1422-1519`、`internal/app/admin_providers.go:1547-1558`。注册表加载时再次校验，最终 client 只能由 SafeTransport 构造：`internal/app/providers.go:470-499`、`internal/app/providers.go:745-770`。

## 角色与双 Project 矩阵

| 主体 | Project A capture | Project B capture | 说明 |
| --- | --- | --- | --- |
| 无 session / 过期 session | 401 | 401 | `Authenticate` 失败即拒绝；不会进入查找或审计 |
| `administrator` | 可读 | 可读 | Admin 是实例级全局身份，不带 Project scope |
| `read_only` | 可读 | 可读 | 现有角色模型明确是“所有 GET、无 mutation 特例”：`internal/domain/admin.go:9-21` |
| 错误 Project 解密参数 | 不可读 | 不可读 | AEAD scope 绑定 request + project；已有负向测试覆盖 |
| audit store 不可用 | 503，不返回正文 | 503，不返回正文 | body 写出前审计，fail closed |

“`read_only` 能读取客户 prompt/response”是当前明确的全局 Admin 产品契约，评审计划也要求若保留则显式接受；本报告不把它单独定性为漏洞。但是，该契约不能解释或接受 Provider credential 进入正文，见 SEC-R3-01。

## 确认问题

### SEC-R3-01 — P1：上游错误正文可把 Provider credential 持久化并暴露给 `read_only` Admin

- 严重度：**P1**
- 置信度：**高**（调用链、源码语义和合成端到端复现一致）
- 违反：INV-08

**可达条件**

1. operator 显式启用 `gateway.failure_capture.enabled`；默认是关闭的：`internal/config/default.yaml:186-202`。
2. Gateway 请求已经通过 admission 并到达 Provider，Provider 以 HTTP 错误返回，错误 message 中包含它刚收到的凭据或等价 secret。
3. 请求终态为 `provider_error`，且 caller 未 abandoned。
4. 攻击者或低权限运维者持有有效 `read_only` Admin session；若该角色配置要求 MFA，还必须满足 MFA；audit 写入必须可用。

**根因与证据**

- `provider.Error.Message` 是不受信的上游 prose：`internal/provider/provider.go:127-148`。
- 两条实际 adapter 路径都会把上游 message 放入该字段：`internal/provider/openai/adapter.go:1013-1048`、`internal/provider/anthropic/adapter.go:782-840`。
- capture 代码明确承认正文“may quote back the credential”，但仍把它写进 `response.body`：`internal/gateway/capture.go:94-117`。
- store 对正文只做大小截断，不做 secret 移除或拒绝：`internal/failurecapture/failurecapture.go:273-276`、`internal/failurecapture/failurecapture.go:573-608`。
- payload GET 对 `read_only` 开放，并返回完整 record：`internal/app/runtime.go:1756-1760`、`internal/app/failure_capture.go:87-149`。

**已有防御**

- 功能默认关闭；文件 AEAD 加密、request/project 绑定、权限 `0700/0600`、独立截断、TTL、审计读取且审计故障时 fail closed。
- Provider HTTP refusal prose 没有进入普通日志；日志 descriptor 只保留标准化枚举和有界 identifier：`internal/gateway/failure.go:76-123`、`internal/gateway/failure.go:186-231`。运行时 logger 还有格式型 secret redaction：`internal/safelog/safelog.go:11-29`、`internal/safelog/safelog.go:43-109`。
- 这些防御保护“磁盘外泄”和“未认证读取”，但不阻止一个合法 `read_only` 主体解密并获得 Provider credential。

**最小复现**

- `/private/tmp/halro-security-review-260911/gateway_security_review_test.go`：假 adapter 返回 401 `provider.Error`，Message 包含 credential canary；一次失败后断言 capture `Response` 包含 canary。
- `/private/tmp/halro-security-review-260911/app_security_review_test.go`：通过正常 Admin API 创建 `read_only` 用户，在真实 Runtime/capture store 中放入 Project A/B 两条含不同 canary 的密封记录；该用户对两个 request ID 的 payload GET 均得到 200 和对应 canary。
- 两个测试均通过；“通过”表示危险行为被稳定复现，不表示安全断言通过。

**影响**

Provider 凭据从只应由 credential vault/authorizer 使用的边界，扩散到 failure capture 和所有 `read_only` Admin。获得它后可能直接访问 Provider 账户、消费额度或读取该凭据有权访问的 Provider 资源。触发依赖 opt-in capture 和会回显 secret 的上游/代理，降低发生概率，但一旦发生是跨权限 credential disclosure。

**修复与退出条件**

- 最安全的缺省是 capture 永不持久化任意上游 prose，只存 status、标准化 reason、有界 code/request ID。
- 若产品必须保留 prose，应在进入 `Record` 前以**实际 authorizer secret**为 canary 做精确擦除，并对无法证明已净化的内容 fail closed；仅依赖 `sk-`、Bearer 等正则不能覆盖未知/self-hosted 凭据格式。
- 增加 OpenAI-shaped 与 Anthropic-shaped 假 HTTP upstream：服务端回显收到的实际 Authorization/x-api-key，最终断言 capture 文件、Admin payload、log、audit 和 error response 均不含 canary。需同时覆盖 administrator/read_only 和截断分支。

### SEC-R3-02 — P2：每日 capture 文件上限在每次进程重启后重置

- 严重度：**P2**
- 置信度：**高**（源码明确说明，临时目录复现稳定）
- 违反：INV-14；同时使 capture 监控值失真

**可达条件**

1. failure capture 已启用并在当天持续产生可捕获的 Provider failures。
2. Halro 在同一自然日内重启、崩溃重启或被编排器反复替换。
3. 每一代进程都可再次写满 `max_records_per_day`。

**根因与证据**

- `Store` 的 `countedDay/dayCount` 只存在内存，`Open` 不扫描当天已有文件：`internal/failurecapture/failurecapture.go:175-225`。
- 注释明确说明 restart resets count；`reserve` 遇到新/空 day 直接从 0 计数：`internal/failurecapture/failurecapture.go:181-187`、`internal/failurecapture/failurecapture.go:371-383`。
- 因此配置宣称的“每日日上限”实际上是“每进程代、每日上限”。默认每 side 64 KiB、1000 条/日；允许配置到每 side 1 MiB、1,000,000 条/日：`internal/config/default.yaml:198-202`、`internal/config/config.go:1409-1421`。
- `halro_failure_captures_today` 和 saturation 读取同一进程内计数，因此重启后会低报磁盘中的实际当日 capture：`internal/app/metrics.go:169-181`。

**已有防御**

- 每份记录的三侧正文仍分别有大小上限，记录有 TTL，单进程内计数受 mutex 保护，到上限后只告警一次。
- 精确读时 TTL 不依赖后台 purge，所以过期正文不会因本问题重新可读。
- 这些措施不能约束同一天跨重启的文件总量，也不能保证 capture 不与 ledger 争用磁盘。

**最小复现**

`/private/tmp/halro-security-review-260911/failurecapture_security_review_test.go` 以 `max_records_per_day=2` 打开同一临时 root，写 2 条后重新 `Open`，再写 2 条；同一天目录得到 4 个 `.hfc` 文件。测试通过。

**修复与退出条件**

- `Open` 时重建当天已识别 capture 的计数，或引入与文件写入一致的持久计数；已有数量达到上限时必须立即进入 saturated 状态。
- 新增 reopen、崩溃后遗留文件、非 capture 文件、跨日、并发 Put 和已有数量超过新配置上限的测试；指标必须报告实际当天数量/饱和状态。

## 候选问题与待验证证据

### C-R3-01 — P2 候选：同步且不可取消的 capture 写入可放大故障期间的 goroutine/响应阻塞

`requestRun.close` 释放资源 lease 后同步执行 `Store.Put`，注释称 caller 已拿到答案：`internal/gateway/service.go:460-487`。但 unary handler 要等 `service.Chat/Responses` 返回后才写 HTTP error：`internal/gatewayapi/handler.go:103-121`、`internal/gatewayapi/handler.go:792-806`；现有 blocking capture 测试也证明 service 调用在 `Put` 解除前不返回：`internal/gateway/capture_test.go:250-287`。这不会占住 accounting/concurrency lease，但慢盘/卡住的文件系统叠加 Provider failure storm 时，入站 goroutine 和 socket 数没有 capture 专属上界。

候选原因：未做真实慢盘/故障注入，尚无 RSS、FD、goroutine 增长曲线。建议 R6 用可控 blocking writer 压测；若增长随失败请求线性增加，应改为有界队列/worker，队列满时丢弃并发出低基数告警。

### C-R3-02 — P3 候选：disabled 启动路径吞掉 failure capture 目录的所有 `stat` 错误

capture disabled 时，`openFailureCapture` 对 `os.Stat(root)` 的任何错误都当作“目录不存在”并返回 nil：`internal/app/failure_capture.go:24-29`。如果之前已有 capture，而目录因权限或 I/O 错误不可访问，运行时不会打开 store、不会执行 purge，也没有启动告警；过期正文可能继续物理驻留。精确读取会被关闭，且不可访问目录本就妨碍删除，所以这是留存可观测性/恢复问题，而非已确认的越权读取。应只吞 `os.IsNotExist`，其他错误 fail/warn，并补权限故障测试。

### 契约接受项（非漏洞）

当前 Admin 身份没有 Project scope，`read_only` 被设计为所有 GET；所以其读取两个 Project 的客户正文是既有产品契约，而不是绕过 Project ACL。v0.8.0 release owner 仍需按 review plan 显式记录是否接受该隐私模型；若期望 Project-scoped operator，则应作为权限模型变更单独设计，不能把现有行为含混地描述为“按 Project 授权”。

## 已确认防御与未发现问题

- **原始 Gateway body / headers**：公开 JSON 入口先执行 4 MiB body ceiling，只把 decoded body 放入 context；headers 没有进入 `GatewayRequest`。Gateway Key 从 Authorization 单独解析。未发现 header、Gateway Key 的正常数据流进入 capture。
- **加密与双 Project**：AES-GCM 使用随机 nonce，HKDF scope/AAD 绑定 kind、request、project；错误 Project 无法打开同一 envelope。文件名中的 Project 只用于定位，真正解密仍需 scope 匹配。
- **TTL / 截断**：Gateway、normalized request、response 在 `Put` 中各自调用 `truncateJSON`；超限值变成有界合法 JSON string 并设置独立 flag。`Get` 在 `capturedAt + retain` 的精确边界即拒绝，后台 purge 延迟不会延长读取权限：`internal/failurecapture/failurecapture.go:449-477`、`internal/failurecapture/failurecapture.go:482-529`、`internal/failurecapture/failurecapture.go:573-608`。
- **成功/取消/策略拒绝**：capture outcome allowlist 不包含 success、policy rejected、一般 rejected；caller abandoned 再次被拒绝。窄测试确认 caller hang-up 不写 terminal failure capture。
- **失败语义**：descriptor 只从 `provider.Error` 的结构化字段取得 status、retryable、ambiguous；canonical reason 只改显示 class，不改这两个执行/结算位：`internal/gateway/failure.go:80-184`。结构化 subscription mapping 窄测试通过。
- **identifier / 日志 / 指标**：provider code/request ID 在写日志和 durable record 前均经 `SafeProviderIdentifier`；failure reason 是 closed enum。普通 Provider HTTP error 不记录 upstream prose；capture 指标没有用户/上游字符串 label。未发现 prompt、response body、原始 IP 或上游 prose 新增到日志/指标的路径。
- **BigModel / SSRF**：CN/Global × General/Coding 的路径由精确 Profile 决定：`internal/app/provider_adapters.go:386-409`。RegionHost/Surface 表只参与产品识别；实际安全 allowlist 来自保存的 endpoint hostname。SafeTransport 禁用环境代理和重定向，在 DNS 后校验全部候选地址，再只拨号已校验 IP；metadata、loopback、link-local、reserved/tunnel 和默认 private 地址均拒绝：`internal/safetransport/transport.go:41-83`、`internal/safetransport/transport.go:118-149`、`internal/safetransport/transport.go:177-264`。未发现新 BigModel endpoint 绕过 INV-13。

## 执行证据

临时 overlay 复现：

```text
go test -count=1 -overlay=/private/tmp/halro-security-review-260911/gateway-overlay.json ./internal/gateway -run '^TestSecurityReviewProviderCredentialEchoEntersCapture$' -v
PASS

go test -count=1 -overlay=/private/tmp/halro-security-review-260911/app-overlay.json ./internal/app -run '^TestSecurityReviewReadOnlyCanReadProviderCredentialEcho$' -v
PASS  (Project A 与 Project B 均返回 canary)

go test -count=1 -overlay=/private/tmp/halro-security-review-260911/failurecapture-overlay.json ./internal/failurecapture -run '^TestSecurityReviewRestartExceedsDailyRecordCeiling$' -v
PASS  (4 files > configured limit 2)
```

仓库窄测试：

```text
go test -count=1 -v ./internal/failurecapture ./internal/safetransport ./internal/gateway \
  -run '^(TestExpiredCaptureIsUnreadableBeforePhysicalPurge|TestTheCeilingBoundsWhatIsStoredNotWhatWasCut|TestACaptureCannotBeOpenedUnderAnotherProject|TestCaptureStopsAtTheDailyCeilingAndSaysSoOnce|TestMixedPublicPrivateDNSAnswerIsRejectedBeforeDial|TestDialUsesValidatedIP|TestClientIgnoresEnvironmentProxyAndRefusesRedirects|TestReservedAndTunnelAddressesAreRefused|TestAnUpstreamRefusalIsLoggedWithoutItsResponseBody)$'
PASS

go test -count=1 -v ./internal/gateway ./internal/provider ./internal/usage ./internal/app ./internal/domain ./internal/provider/openai \
  -run '^(TestACallerHangingUpWritesNoTerminalFailure|TestSubscriptionFailureMappingUsesOnlyStructuredEvidence|TestProviderIdentifiersSurviveTheDerivation|TestBigModelWiringKeepsEachProductOnItsOwnHostSurfaceAndPath|TestCredentialCreationRejectsAHostPublishedByAnotherProductSurface|TestCredentialCreationRejectsAKnownHostFromAnotherFixedRegion|TestPrivateProviderEndpointStaysRefusedByDefault|TestBigModelUsesTheRegionalGeneralAPIPathAndDialect|TestRegionForEndpointReadsTheHost|TestSurfaceForEndpointSeparatesBigModelRegions)$'
PASS
```

## 剩余证据缺口

- 未按约束调用真实 Provider，因此没有实测哪个供应商/代理会回显 credential；但代码注释、adapter 数据流和合成 canary 已证明一旦回显就会落盘并可读。
- 未做真实慢盘、只读文件系统、磁盘满、I/O hang 的 fault injection；C-R3-01/C-R3-02 保持候选。
- 未使用真实 KMS 或生产 master key；本轮只验证了实现和本地/假 sealer 路径。
- 未找到 `SafeProviderIdentifier` 对 `code:param` **整体**长度的直接测试。两个分量各限制 128，返回值最大可达 257 字节，仍是常数有界但与 `MaxProviderIdentifierLength=128` 的命名可能不一致；建议补整体边界契约测试，当前不定性为漏洞。
- BigModel Global Coding wrong-path 是否会误扣另一权益未做 billable smoke；本轮只确认 host/path/Profile 与 SafeTransport wiring，不对真实账户行为作推断。
