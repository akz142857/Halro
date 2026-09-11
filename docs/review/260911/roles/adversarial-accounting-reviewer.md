# S4 对抗裁决：安全、条款、BigModel 与 rollback readiness

## 范围与独立性

- 候选 SHA：`222d08f84f61493fc9a273d351cc728528d6e30c`。
- 身份：非四项原报告作者；先从源码入口和运行结果建立判断，再读取原报告定位和对照。
- 约束：没有修改产品代码；独立 probe 均通过仓库外临时 overlay 或临时导出的 `v0.7.1` 源码执行；没有访问真实 Provider、客户数据或凭据。
- 本报告不复核 R2/R6 的 pending lease finding。

## 裁决总表

| 项目 | 裁决 | 严重度 | 置信度 | 对抗结论 |
| --- | --- | --- | --- | --- |
| SEC-R3-01：上游错误 prose 可把 Provider credential 带入 capture 并被 read_only 读取 | **CONFIRMED** | P1 | 高 | canary 从 `provider.Error.Message` 原样进入 capture；payload GET 只要求 `requireAdmin`，read_only 合法通过 |
| R1-01：policy revision 变化后旧受限连接继续装载 | **CONFIRMED** | P1 | 高 | revision 只进入审计 metadata，没有进入 Credential/Provider 持久状态；loader 完全不读取 revision |
| R4-P1-001：`glm-4.7 reasoning_effort=none` 静默丢弃 | **CONFIRMED（措辞收窄）** | P1 | 高 | wire 同时没有 `thinking` 和 `reasoning_effort`，上游默认开启 thinking；但正确修复可以映射到 `thinking.disabled`，不一定必须拒绝 |
| `v0.7.1` 对 schema-8 usage 目录仍报告 serve ready | **CONFIRMED** | P1 | 高 | 旧 Runtime `Open` 和 `/health/ready` 都成功，显式 `Verify` 同时以 unsupported schema 失败 |

## ADV-01 — SEC-R3-01

### 裁决：CONFIRMED / P1 / High

### 入口与完整链

1. OpenAI-shaped HTTP error decoder 将上游 sentence 放入 `provider.Error.Message`：
   `internal/provider/openai/adapter.go:1004-1048`。Anthropic-shaped decoder同样复制
   `envelope.Error.Message`：`internal/provider/anthropic/adapter.go:782-847`。
2. `captureProviderFailure` 明确取 `providerFailureReason(classified)` 并写入
   `upstreamFailureBody.Message`；注释也承认正文可能回显刚拒绝的 credential：
   `internal/gateway/capture.go:94-127`。
3. terminal outcome 为 `provider_error` 时，`writeCapture` 将该对象 JSON 编码进
   `failurecapture.Record.Response`：`internal/gateway/capture.go:129-164`。
4. Store 只执行大小截断，不执行 secret scrub 或基于实际 authorizer secret 的拒绝：
   `internal/failurecapture/failurecapture.go:273-276,573-608`。
5. payload 路由使用 `requireAdmin` 而非 `requireAdminMutation` 或 administrator-only gate：
   `internal/app/runtime.go:1756-1760`。`requireAdmin` 只做 session/MFA，不拒绝
   `read_only`：`internal/app/admin_session.go:258-269`；项目只从 capture 定位并用于 AEAD 绑定，
   返回前没有调用者 Project ACL：`internal/app/failure_capture.go:87-149`。
6. 角色模型明确 read_only 是合法 Admin role，且设计为 GET 可读：
   `internal/domain/admin.go:9-21`。

### 独立证伪尝试

尝试寻找以下拦截均失败：adapter message 脱敏、capture 前以真实 credential canary 擦除、Store secret scan、payload endpoint administrator-only 权限。普通日志确实不记录 prose，但该防线与 capture 是不同 sink。

仓库外 overlay probe 给 fake adapter 返回：

```text
provider.Error{StatusCode: 401,
  Message: "upstream rejected credential provider-secret-canary-9f37"}
```

一次正常 Gateway 调用产生一条 capture，`Record.Response` 包含 canary：

```text
=== RUN   TestAdversarialProviderCredentialProseReachesCapture
--- PASS: TestAdversarialProviderCredentialProseReachesCapture
```

这不是假设真实厂商一定会回显 secret；它证明任意已配置上游或中间代理一旦回显，Halro 没有第二道边界阻止 secret 持久化。自定义 endpoint 使这个输入不依赖特定厂商实现。

### 已有防御及为何不能证伪

- capture 默认关闭：`internal/config/default.yaml:186-202`；开启后文件加密、有界、TTL、读取审计且审计不可用时 fail closed。
- Provider prose 不进入普通日志，identifier 会收窄。
- 这些控制降低暴露面并保护磁盘，但合法 read_only session 最终仍会得到解密后的完整 Record。当前“read_only 可读客户 prompt”可能是产品契约，却不能自动授权其读取 Provider credential。

### 退出条件

捕获层不保存任意 upstream prose，或在构造 Record 前用实际使用的 Provider secret 做精确 canary scrub/fail-closed；回归至少覆盖 OpenAI/Anthropic envelope、custom proxy 回显、短/长 secret、截断边界和 administrator/read_only。

## ADV-02 — R1-01

### 裁决：CONFIRMED / P1 / High

### 入口与完整链

1. 当前 revision 只存在于内存 `surfaceTable`：
   `internal/domain/provider_offering.go:171-201,235-281`。
2. 创建 Credential/Provider 时精确校验输入 revision，并仅把 acknowledgement 写进审计 metadata：
   `internal/app/admin_providers.go:75-105,225-252,1291-1331`。
3. `domain.Credential` 没有 acknowledged revision 字段：
   `internal/domain/models.go:197-217`；`domain.ProviderInstance` 也没有：
   `internal/domain/models.go:400-450`。
4. 全仓 `usage_policy_revision` 生产代码引用只有 Surface 定义、Admin 校验/审计和 catalog view；
   `internal/store` 中没有 entitlement/acknowledgement 状态。审计事实没有被提升为 loader 的授权输入。
5. enabled Provider 的启动/激活 loader 读取 credential、endpoint、audience、profile、surface、scheme、
   capability 后直接解密并创建 adapter：`internal/app/providers.go:470-557`；随后 route 正常注册：
   `internal/app/providers.go:697-734`。链上没有读取 `UsagePolicyRevision`。
6. 更新已存在 Provider 时，仅当 CredentialID 或 ProfileID 变化才重新要求 acknowledgement；单纯启停、
   endpoint 或其他同 Profile 修改不会检查当前 revision：`internal/app/admin_providers.go:326-357`。

### 独立证伪尝试

重点搜索了三种可能的反证：Credential/Provider 持久字段、单独的 acknowledgement store、loader 从 audit 重建当前确认。三者均不存在。现有定向测试：

```text
go test -count=1 ./internal/app \
  -run '^TestSubscriptionUsageWarningIsRequiredAndAuditedForCredentialAndConnection$' -v
PASS
```

它确认创建时 gate 和审计防线有效，但测试到创建完成即止；没有 revision 变化后的 reload 断言。也没有可注入 Surface revision 的现成 seam。由于 loader 的输入类型根本不携带 revision，静态完整链足以确认旧连接在 revision 变化后仍装载，运行注入只会重复该事实。

### 可达性与影响

条件是未来把受限 Surface 的 revision 从 R1 提升到 R2；这不是当前已有记录立即失效的声明，而是确定的生命周期缺口。升级后二进制只看 enabled 和现有 identity，一份只确认过 R1 的连接仍能接收流量，违反 INV-06。严重度维持 P1。

### 退出条件

把当前有效确认作为受认证持久状态，至少绑定 Credential/Provider、Surface、Offering、Region、精确 revision；每次启动和 topology activation 都比较当前 Surface revision，不匹配时只 withhold 受影响 binding/route，并提供重新确认入口。

## ADV-03 — R4-P1-001

### 裁决：CONFIRMED（原报告的“必须拒绝”结论需收窄）/ P1 / High

### 源码与独立 wire 证据

1. `bigModelReasoningFor` 只识别 GLM-5.3 为 always、GLM-5.2 为 optional，其他模型（含 GLM-4.7）
   一律归入 no-reasoning：`internal/compatibility/bigmodel.go:44-60`。
2. no-reasoning 分支接受的唯一显式 effort 是 `none`：
   `internal/compatibility/bigmodel.go:63-76`。
3. `applyBigModelReasoning` 没有 no-reasoning case，因此同时不写 `thinking` 和
   `reasoning_effort`：`internal/compatibility/bigmodel.go:162-186`。
4. `glm-4.7` 在内置 BigModel model list 中可达：
   `internal/modelcatalog/builtin.go:707,759-760`。

独立 overlay probe 构造最小 OpenAI Chat request，设置
`model=glm-4.7, reasoning_effort=none`，实际 JSON：

```text
{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}]}
```

测试结果：

```text
=== RUN   TestAdversarialGLM47NoneIsOmittedOnWire
--- PASS: TestAdversarialGLM47NoneIsOmittedOnWire
```

仓库现有 `internal/compatibility/bigmodel_test.go:53-88` 也把
`glm-4.7 / none / thinking absent / no error` 固定为预期，故常规测试绿不能证伪。

### 上游契约核对与措辞修正

智谱和 Z.AI 当前官方文档都声明 `thinking.type` 默认是 `enabled`，GLM-4.7 在 enabled 下会强制思考；关闭需显式发送 `thinking.type=disabled`：
[智谱深度思考](https://docs.bigmodel.cn/cn/guide/capabilities/thinking)、
[Z.AI Thinking Mode](https://docs.z.ai/guides/capabilities/thinking-mode)、
[Z.AI Chat Completion](https://docs.z.ai/api-reference/llm/chat-completion)。

因此省略绝非与 `none` 等价，silent intent loss 已确认。另一方面，官方也说明 GLM-4.7 支持 turn-level disabling，所以“强制推理模型收到 none 必须在 Provider I/O 前拒绝”不是唯一正确处理；Halro 可以把北向 `none` 精确映射成 `thinking:{type:"disabled"}`。P1 的理由应是 silent semantic inversion 及其对 token-limit/能力判断的连带影响，而不是声称上游绝对不能关闭 GLM-4.7 thinking。

### 退出条件

为 GLM-4.7 及同契约模型建立可审计的 per-model reasoning 表；`none` 精确发 `thinking.disabled`，或若产品选择不支持该映射则在出站前拒绝，绝不能省略；用 wire JSON 断言而非只看内部 mode。

## ADV-04 — v0.7.1 schema-8 rollback readiness

### 裁决：CONFIRMED / P1 / High

### 独立跨版本运行

从 tag `v0.7.1^{}` 导出源码到临时目录，在其正常 `testConfig` 数据目录执行 Initialize，然后仅写入：

```json
{"schema_version":8,"last_sequence":0,"files":[]}
```

再调用旧版 `app.Open` 和真实 Gateway `/health/ready` handler，并在同一 Runtime 对 exporter 做显式 Verify。结果：

```text
=== RUN   TestAdversarialSchema8ManifestDoesNotBlockV071Readiness
ready=200 while explicit Verify fails:
  usage manifest schema version 8 is not supported
--- PASS: TestAdversarialSchema8ManifestDoesNotBlockV071Readiness
```

测试源码和构建都位于 `/private/tmp/halro-v071-adversarial.gkAgKK`，没有修改工作树。

### 根因与证伪范围

1. v0.7.1 的 `parquetSchemaVersion=6`，`supportedManifestSchema` 上限是 6；其 Export/Verify 确实会拒绝 8：`v0.7.1:internal/usage/parquet.go:37-47,175-190,255-270`。
2. 但 Runtime startup 只调用 `NewExporterWithOptions`，不 Load/Verify manifest：
   `v0.7.1:internal/app/runtime.go:346-364`。
3. readiness 只检查 draining、Ledger accounting、pricing 和 activation stale；不检查 usage exporter：
   `v0.7.1:internal/app/runtime.go:1929-1964`。
4. 周期 export 遇到 unsupported schema 只写 WARN，不标记 Runtime unavailable：
   `v0.7.1:internal/app/runtime.go:1076-1118,1233-1242`。

空 files manifest 已足以裁决 startup gate，因为 Open 根本不读取 manifest；换成 populated schema-8 partitions 不会改变启动/ready 路径。该实验没有证明旧版本会误解 schema-8 row——它不会读到那一步；问题恰恰是数据面继续 ready，而 Usage export 已确定不可用。

### 影响与退出条件

Ledger/WAL 权威没有因此损坏，但 rollback 后实例会继续接流量并持续制造只能留在 Ledger/内存派生中的新 Usage，Parquet pipeline 只告警降级；这与 review-plan 要求的“旧 reader 明确拒绝，而非静默误读/继续服务”不符。严重度维持 P1。

旧 Runtime 无法回补，但 v0.8.0 应在升级时写入它会在启动阶段识别的 durable minimum-reader/feature gate，或发布程序明确禁止 in-place binary rollback 并要求先恢复 v0.7.1 backup。新版本还应有双版本进程级测试，断言旧 binary 在监听/ready 前拒绝新格式目录。

## 执行记录

```text
go test -count=1 -overlay=/private/tmp/halro_adversarial_overlay.json \
  ./internal/gateway ./internal/compatibility -run '^TestAdversarial' -v
PASS (gateway, compatibility)

go test -count=1 ./internal/app \
  -run '^TestSubscriptionUsageWarningIsRequiredAndAuditedForCredentialAndConnection$' -v
PASS

# workdir=/private/tmp/halro-v071-adversarial.gkAgKK, source=v0.7.1
go test -count=1 ./internal/app \
  -run '^TestAdversarialSchema8ManifestDoesNotBlockV071Readiness$' -v
PASS; ready=200 and Verify(schema8)=unsupported
```

## S4 建议

四项均未被证伪，应进入 findings/adversarial verdict 汇总。SEC-R3-01、R1-01、BigModel silent omission 和旧版 rollback readiness 都应作为 v0.8.0 的 P1 退出条件；其中 BigModel 修复标准应采用本报告的收窄表述，允许精确映射 `none → thinking.disabled`，不强制选择“拒绝”。
