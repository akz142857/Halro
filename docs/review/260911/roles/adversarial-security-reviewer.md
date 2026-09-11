# S4 对抗裁决：安全评审人

基线：`main@222d08f84f61493fc9a273d351cc728528d6e30c`；回滚实验基线：`v0.7.1@84f2638e973f23935b9eda423143f65ff25a852c`。

方法：从外部/生命周期入口重新追到持久化或 Provider wire renderer，并用仅位于 `/private/tmp/halro-s4-adversarial` 的 Go overlay 测试和隔离运行时复现尝试证伪。未修改产品代码，未访问真实 Provider/KMS/内网，也未复核本人原先的 SEC-R3-01。

| 项目 | 裁决 | 严重度 | 置信度 |
| --- | --- | --- | --- |
| R2 pending lease 恢复丢 Offering/Profile/Region | **CONFIRMED** | P1 | 高 |
| R1 条款 revision 变化后旧连接继续装载 | **CONFIRMED** | P1 | 高 |
| R4 `glm-4.7` + `reasoning_effort=none` 静默丢弃 | **CONFIRMED** | P1 | 高 |
| `v0.7.1` 面对 usage schema 8 仍报告 ready | **CONFIRMED** | P1 | 高 |

## 1. R2 pending lease 恢复丢产品归属

**证伪尝试。** 分别覆盖 lease 的 `not_started` 与 `started` 恢复分支，检查结算是否能从原 reservation 或 ledger state 恢复 `OfferingID`、`ProfileID`、`AccountRegionID`。

**证据。** 正常请求在 reservation 写入三项归属（`internal/budget/manager.go:917-925,963-988`），正常结算也逐项复制（`internal/budget/manager.go:1122-1168`）。但 `RecoverPendingLeases` 在 `internal/budget/manager.go:1171-1239` 重建 `Attempt`，其中 `1192-1203` 没有三项字段；两个恢复分支最终均由 `1223-1229` 用该空值 Attempt 结算。ledger 对遗留空归属明确放行（`internal/ledger/event.go:216-230,309-325`），settlement 匹配也不比较这三项（`internal/ledger/state.go:643-664`），所以不会 fail closed。

独立 overlay 测试：

```text
go test -count=1 -overlay=/private/tmp/halro-s4-adversarial/budget-overlay.json \
  ./internal/budget -run '^TestS4RecoveryRebuildDropsProviderProductAttribution$' -v
PASS
not_started: reservation=(bigmodel.coding-plan,bigmodel.cn.coding.chat.v1,cn), settlement=(,,)
started:     reservation=(bigmodel.coding-plan,bigmodel.cn.coding.chat.v1,cn), settlement=(,,)
```

**裁决。** 两条可达恢复路径都永久丢失不可变产品归属；金额结算本身仍可完成，但后续按 Offering/Profile/Region 的权威归因被静默降级。已有 legacy 兼容校验是促成条件而非防御。维持 **CONFIRMED / P1**。

## 2. R1 usage-policy revision 变化后旧连接继续装载

**证伪尝试。** 检查 revision 是否被持久化到 Credential/Provider、是否由加载路径重新校验，或是否存在能把旧记录判为未确认的旁路状态。

**证据。** revision 只存在于内存产品表（`internal/domain/provider_offering.go:190-194,244-281`）。Credential 与 ProviderInstance 的持久模型均无 revision 字段（`internal/domain/models.go:197-217,400-450`）。创建/当前更新入口会调用确认门（`internal/app/admin_providers.go:75-105,225-263,1285-1331`），但更新仅在 `productBindingChanged` 时触发（`334-346`），确认结果只进 audit metadata。实际启动加载链 `internal/app/providers.go:402-557` 读取 enabled Provider/Credential/profile/capability 并构建 adapter，全程不比较 policy revision。

独立 overlay 测试直接持久化一个有效 restricted BigModel 连接，确认 Provider/Credential JSON 均不含 `usage_policy_revision`，然后调用真实 `loadProviderRegistry`：

```text
go test -count=1 -overlay=/private/tmp/halro-s4-adversarial/app-overlay.json \
  ./internal/app -run '^TestS4RestrictedProviderLoadsWithoutPersistedPolicyRevision$' -v
PASS: provider_s4:bigmodel.cn.coding.chat.v1 loaded with no persisted policy revision
```

**裁决。** 在未来 revision 增加后，二进制无法区分“按旧 revision 确认”与“按当前 revision 确认”，而启动 loader 会继续装载。当前历史中没有一次可直接重放的既有产品 revision 迁移，故实验使用等价旧记录模拟；这限制历史事件断言，不影响机制和下一次迁移的确定性。维持 **CONFIRMED / P1**。

## 3. R4 `glm-4.7` 的 `reasoning_effort=none` 被静默丢弃

**证伪尝试。** 检查兼容层是否拒绝该组合、是否把 `none` 转成 Provider 的显式关闭字段，或下游 adapter 是否补齐。

**证据。** BigModel 分类仅将 `glm-5.3` 设为 always、`glm-5.2` 设为 optional，`glm-4.7` 落入 no-reasoning 默认分支（`internal/compatibility/bigmodel.go:52-60`）；默认分支接受 `none`（`63-76`），随后返回 thinking off（`79-84`），renderer 在 no-reasoning 分支不写任何 thinking/reasoning 字段（`101-186`）。OpenAI adapter 直接使用该 renderer（`internal/provider/openai/adapter.go:188-201`）；gateway 的预留前兼容判断也依赖同一分类（`internal/gateway/service.go:2879-2907`）。现有参数测试甚至把 `glm-4.7/none` 固化为合法 no-reasoning（`internal/compatibility/bigmodel_test.go:53-88`）。

独立 wire 级 overlay 测试：

```text
go test -count=1 -overlay=/private/tmp/halro-s4-adversarial/compat-overlay.json \
  ./internal/compatibility -run '^TestS4GLM47NoneIsAcceptedButOmittedOnWire$' -v
PASS
{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"request_id":"request-s4"}
```

请求成功生成，但 wire body 同时缺少 `thinking` 与 `reasoning_effort`。官方 [Thinking Mode](https://docs.z.ai/guides/capabilities/thinking-mode) 和 [Chat Completion](https://docs.z.ai/api-reference/llm/chat-completion) 契约说明 GLM-4.7 默认启用 thinking，关闭需显式发送 `{"thinking":{"type":"disabled"}}`。因此省略字段会把调用方的 `none` 改成默认启用，并可能改变成本、延迟及 token 行为。

**裁决。** “成功但语义静默漂移”已确认，维持 **CONFIRMED / P1**。但若原建议声称 GLM-4.7 完全不能关闭、因此只能拒绝 `none`，该修复前提被官方契约证伪；至少应显式翻译为 `thinking.type=disabled`，而不是继续省略。

## 4. `v0.7.1` 面对已填充 schema 8 仍报告 ready

**证伪尝试。** 不只伪造 manifest，而是用当前 exporter 生成包含一条真实记录、且通过当前 `Verify` 的 schema 8 Parquet 分区，再启动实际 `v0.7.1` 二进制观察 readiness、healthcheck 与 exporter 行为。

**独立实验。** 当前 exporter 生成 `schema_version=8 / files=1 / records=1`：

```text
HALRO_S4_USAGE_ROOT=/private/tmp/halro-s4-adversarial/data/usage \
go test -count=1 -overlay=/private/tmp/halro-s4-adversarial/usage-overlay.json \
  ./internal/usage -run '^TestS4WritePopulatedSchema8Fixture$' -v
PASS: schema=8 files=1 records=1; Verify passed
```

随后运行从 tag 源码独立构建的 `v0.7.1`（SHA-256 `aaeefb134e1ee4bddfbeceb3b1ce749fee3205be1213535b63d0ec92cb3a85de`）：

```text
GET http://127.0.0.1:38180/health/ready
HTTP/1.1 200 OK
{"accounting":"healthy","status":"ready"}

halro-v0.7.1 healthcheck --url http://127.0.0.1:38180
exit 0
```

同一旧二进制离线执行 `usage verify` 会退出 1，明确报 `usage manifest schema version 8 is not supported`；进程关闭触发 export 时也只记录 `usage parquet export failed` 警告。源码原因是 `v0.7.1` 的 `parquetSchemaVersion=6`，而启动仅构造 exporter（当前对应 `internal/app/runtime.go:359-365`），没有 LoadManifest/Verify；周期导出错误只记 warning（`1233-1242`），readiness 只检查 draining、ledger、pricing 与 activation（`1929-1961`），不包含 usage exporter 健康状态。

**裁决。** 一个确实无法理解已有 schema 8 分区的旧二进制仍监听流量并报告 ready；离线 verify 和延迟 warning 不构成启动/流量门。实验没有继续写旧请求来证明永久数据损失，故裁决限于“回滚 fail-open 与不可服务 schema 未进入 readiness”，这已经满足发布计划的 silent persistence misread 阻断标准。维持 **CONFIRMED / P1**。

## 总结

四项均经独立入口追踪或隔离实验确认，仍应作为 v0.8.0 发布阻断项。唯一需要校正的是 R4 的修复表述：漏洞是 `none` 被省略并回落到默认 thinking；官方契约支持对 GLM-4.7 显式 disabled，因此不能仅凭“forced thinking”假设断言必须拒绝。
