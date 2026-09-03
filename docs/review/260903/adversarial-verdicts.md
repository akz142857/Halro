# 260903 对抗验证裁决

五条最严重的 finding,各送一个独立角色去**证伪**:默认 finding 为错,要求在代码里
复现完整路径,或找出拦住它的防御。裁决 CONFIRMED / REFUTED / PARTIAL。

结果:**4 CONFIRMED、1 REFUTED,且没有一条原样照收** —— 一条降级、一条改写性质、
一条被实测坐实到比原描述更重、一条被判为既定设计。这与 260805、260901 两轮的经验
一致:交叉证实提高的是「代码可疑」的置信度,不等于「这条路径真能走到」。

| # | finding | 裁决 | 相对原描述的变化 |
|---|---|---|---|
| V1 | 失败列表展示成功那次尝试的 `provider_request_id` | **CONFIRMED** | 实测复现,并证明日志侧的修法在读侧结构上不可能生效 |
| V2 | 密封孤儿对象永不清扫 | **CONFIRMED** | 严重度 P1 → **P2**;并找到一个把该行为锁成契约的测试 |
| V3 | `read_only` 管理员可读客户 prompt | **CONFIRMED** | 措辞改写:不是「绕过」,是「设计含义漂移」 |
| V4 | 同 project 内跨 Gateway Key 可取回他人延迟结果 | **REFUTED** | 既定且已声明的资源模型边界,降为 informational |
| V5 | 全局 FIFO + 硬编码 4 worker 可跨项目饿死 | **CONFIRMED** | 由主持方直接核验,无变化 |

---

## V1 — 失败列表把成功那次尝试的上游标识符展示给运维 · CONFIRMED

**原 finding**(测试盲区角色):`internal/usage/failures.go:168` 的 `lastFailure` 倒走
attempt 链、跳过 success,其注释断言「请求只能以失败结束」;而网关在渲染裁决之前就把
attempt 结算成 success,之后才把请求标成 `provider_error`。

**证伪失败,且证据比原描述更强。** 证伪角色没有停在推导,而是在仓库副本里跑出了完整
复现:

```
outcome=provider_error attempts=3 fallbacks=1
last_failure = {AttemptNumber:2 ErrorClass:provider_5xx ProviderStatus:503
  DeploymentID:dep_target_1 ProviderCode:srv_overload
  ProviderRequestID:upstream-req-first FailurePhase:provider}
```

同一次运行,两个视图对同一个请求给出互相矛盾的说法:日志侧报 `dep_target_2` 且不带
上游标识符,失败列表报 `dep_target_1` + `upstream-req-first`。而 `failures.go:14-16`
的设计前提正是「列表与汇总卡不会不一致」。

时序主持方独立核实:`internal/gateway/service.go:1198` 的
`attempt.finish(providerErr, settlement)` 在 `:1239` 的 `render(semanticResponse)`
之前,`providerErr == nil` 时该 attempt 已结算为 success,渲染失败后 `:1240` 才把
outcome 置为 `provider_error`。触发条件是渲染失败,来自
`internal/compatibility/openai/mapping.go:317-340`(带 citations 的文本、Chat wire
装不下的 content kind),不是 `unsupported_feature`(那条走 `attempt.abort`,attempt
不会结算成 success)。reasoning 有专门分支,不触发。

**为什么日志侧的修复在读侧不生效** —— 这是本条最重要的部分。日志侧的修法落在
`requestRun` 的进程内字段上(`service.go:673-681` 在 `providerErr == nil` 时清空
`run.failure`),从不落盘;读侧从 ledger 事件重建,而 ledger 里没有任何标记能区分
「上游失败导致的 provider_error」与「上游成功、渲染失败导致的 provider_error」
(`internal/budget/manager.go:769-785` 的 `Finalize` 只写 `Outcome`)。所以那次修复
不可能覆盖读侧,两者不共享数据。

运维实际看到的:`internal/app/admin_usage.go:524` 原样透出 `page.Failures`,前端
`web/src/pages/UsageFailuresPanel.tsx:21-23` 的 `policyOutcomes` 集合不含
`provider_error`,所以 deployment 链接、HTTP 状态、`provider_request_id` 全部照渲染。
运维拿着那个标识符去问上游,问的是一次**成功**的调用 —— 与
`internal/gateway/final_failure_log_test.go:421` 注释描述的、本轮刚在日志侧修掉的,
是同一句话。

**测试缺口**:`internal/usage/failures_test.go:20-71` 的 fixture 有四种请求形态,
`req_fallback` 是「失败后成功且请求终态 success」,`req_failed` 是「全失败」。缺的正是
第五种:回退后成功、请求终态非 success。所以那条注释断言从未被测试挑战过。

**级别 P1。** 不损坏数据,但失败诊断这一整个特性的目的就是把「一个计数」变成「一个
解释」,而这条路径给出的解释指向错误的上游调用。

---

## V2 — 密封孤儿对象永不清扫 · CONFIRMED,严重度下调至 P2

**原 finding**(核心逻辑、安全、BUG 三个角色独立指向):`data/provider-objects/` 下
没有记录指向的**密封**文件没有任何清扫路径,而
`internal/gateway/deferred_response.go:351-353` 与 `:666-673` 的注释声称
「A sealed object nothing names is swept at startup」。

**证伪失败,但严重度被下调,并找到一个三个发现型角色都漏掉的证据。**

证伪角色穷尽了六个删除点,确认唯一从**目录**出发的
`removeUnsealedObjectFiles`(`internal/app/provider_resources.go:127`)在 `:124` 明确
`if err != nil || vault.SealedEnvelope(header) { continue }` 跳过密封文件;其余五个
全部从**记录**出发,而孤儿没有记录。CLI 无任何子命令触及该目录;小时 ticker 只调
纯记录驱动的 `reapProviderResources`,连那条阉割版目录扫描也只在启动时跑一次。

**决定性证据**:`internal/app/provider_resources_test.go:49-66` 写入一个没有任何记录
指向的密封文件,然后断言它**必须存活**:

```go
if _, err := os.Stat(filepath.Join(directory, "file-sealed.object")); err != nil {
    t.Fatalf("a sealed object was reclaimed too: %v", err)
}
```

主持方已核实这段代码。所以那个扫描是一次性的**明文迁移**清扫,不是孤儿清扫,而
「密封孤儿存活」是被测试锁死的当前契约。三者组合 —— 注释说有兜底、测试说不许清、
实现两边都不做 —— 会让下一个维护者相信问题已被处理。

**孤儿的产生路径有三条,其中一条不需要崩溃**:`finishDeferred` 在
`deferred_response.go:649` 写完答案对象、`:663` 的 `saveDeferred` 失败时直接 return,
**没有任何补偿删除**,而记录里 `ObjectPath` 仍是空。重启后 recover 以 `answer=nil`
把它置为 failed,TTL reaper 因 `ObjectPath == ""` 跳过对象、删掉记录 ——
`<id>.content`(模型答案全文)永久留下。只需要一次 bbolt 写失败。

**为什么是 P2 而不是 P1**:对象是 AES-256-GCM 密封、AAD 绑定 `(objectScope, projectID)`,
无 master key 打不开,所以对外部攻击者不构成保密性泄露(对持有 master key 的人可读,
`resourceID` 就在文件名里);三条路径都需要故障触发,不是稳态;单文件有
256 KiB / 1 MiB 上限,每个 resource ID 最多贡献 2 个文件,是慢漏而非无界喷发。

**为什么不是 P3**:它使 ADR 0024 的 24h 留存承诺在故障路径上无条件失效,而 doctor、
backup、日志、指标四条可观测渠道**全部**看不见 —— 运维无从发现、无从计数、无从清理。
一个附带事实同属该目录:`.resource-*` 临时文件在写入中途崩溃时留下,而
`provider_resources.go:119` 明确跳过 dot 前缀项。

---

## V3 — `read_only` 管理员可读客户 prompt · CONFIRMED,性质改写

**原 finding**(安全角色):`GET /admin/api/v1/usage/failures/{requestID}/payload` 用
`requireAdmin` 注册,而 `requireAdmin` 不判角色,于是 `read_only` 拿到含 prompt 与
上游响应体的完整记录。

**证伪失败,实测复现。** 证伪角色在隔离 worktree 里建了真的 `read_only` 账号,走真实
`adminRouter()`,拿到 200 与明文 body(prompt 与上游响应体原样),审计记录
`actor="viewer"`。中间件链逐层确认无角色判定:`requireAdministratorRole`
(`internal/app/admin_session.go:352`)全仓只有两个调用点,都在 mutation 侧。
`failurecapture.go:294` 在 `Get` 内部解封,handler `failure_capture.go:126` 直接
`writeJSON(record)`,无任何脱敏。

**措辞被纠正,这是本条的价值**:不是「read_only 绕过了角色门」,而是
**「角色模型的 GET-only 前提在 #254 被打破,而模型没有跟着改」**。
`internal/domain/admin.go:9-14` 明确记录了「两个角色、无 per-endpoint 例外」是被评估
后否决 per-endpoint 权限矩阵的结果,其隐含前提是所有 admin GET 只返回 Halro 自己的
元数据 —— 这个前提在三处代码与文档里被写死过。证伪角色确认**本轮之前无反例**:
`internal/app/failure_capture.go` 与 `internal/failurecapture/` 的全部历史只有一个
提交,就是本轮的 `b3a65a8`(#254);此前最接近的 GET 返回的是纯 ID / 计数 / 成本 /
时间戳。

**严重度:中**(默认 `gateway.failure_capture.enabled: false`,保留窗口 24h,每次读
写审计);**对任何打开了失败捕获的安装则为高** —— `read_only` 就是一个能读走客户
prompt 明文的账号,且它对实例内所有 project 一视同仁。

**附带发现,主持方已核实**:`internal/app/failure_capture.go:139-143` 审计写失败时只
`logger.Warn`,函数返回后 `:126` 继续 `writeJSON(writer, http.StatusOK, record)` ——
审计不可用时,这条唯一返回 prompt 的读是 **fail-open** 的。与 CLAUDE.md
「Fail-closed, not fail-open」直接相左。这是一条独立的新发现,不在原 finding 内。

审计记录含用户名、请求 ID、project,但**不含角色**,所以从审计日志无法直接看出
「一个 read_only 读了 prompt」,需回查 `admin-users`,而角色可被改。

---

## V4 — 同 project 内跨 Gateway Key 可取回他人延迟结果 · REFUTED

**原 finding**(安全角色,列为该角色头号发现):同 project 的任意 key 可取回、取消、
删除他人提交的延迟响应;记录里存了 `KeyID` 却从不用于授权。

**代码事实成立 —— 证伪角色实测跑通了三个动作 —— 但对意图的判断错了。**

project 级授权是**整个资源模型既定且已声明的边界**,不是延迟取回的疏漏:

- 同源资源用完全相同的检查:`fileOwner`
  (`internal/gateway/inference_resources_store.go:475-478`)、`batchOwner`(`:1063-1069`)、
  `GetAsyncInvoke`(`:1246-1252`)、`CancelAsyncInvoke`(`:1280-1286`),全部
  `ProviderResource(ctx, principal.Project.ID, id)`,且**连 `KeyID` 字段都不填**。
- `KeyID` 只写在 deferred 记录上,用途在 `internal/domain/models.go:68-72` 写明:出队时
  问「这把 key 现在还认不认」,即**吊销传播**,不是取回授权。
- 所有强制维度都按 project 键控:模型白名单、预算、RPM/TPM、CIDR、redaction policy、
  `MaxDeferredQueue` 全在 `Project` 上(`models.go:251-269`);`domain.GatewayKey`
  (`models.go:365-376`)只有 id/project/name/hash/enabled/expiry,**没有任何 per-key
  策略**。同 project 的多把 key 在所有强制维度上不可区分。
- 已声明:`docs/compatibility/endpoint-manifests.json` 的 get/cancel/delete 三条
  `state_semantics` 逐条写 `"project-owned deferred record"`(机器可读契约,不是散文);
  `docs/guides/deferred-responses.zh-CN.md:66` 写「不属于本项目 → 404」;ADR 0009
  把 owner 枚举为 project/provider/deployment/profile/region/upstream id/expiry,无 key;
  ADR 0024 全文不出现 Gateway Key;ADR 0004 写明「Project is the primary consistency
  and sharding boundary」。

**降为 informational。** 三点残留值得留档,都不改变裁决:

1. **无测试**。跨 project 取回返回 404 这条边界靠 `store_providers.go:459` 一行守着,
   而 fixture 全程单 project(`blind-spots.md:104` 已记),跨 key 连这行都没有。
   回归会静默通过。
2. **取回无任何痕迹**。`internal/gateway/deferred_response.go` 全文无 audit 调用,按
   ADR 0024:163 取回也不写 ledger。files/batches 的读走 `accountedInferenceResources`
   且请求记录带 `KeyID`;deferred 的读什么都不留 —— 即便同 project 内 key 等价成立,
   一次取回也无法事后归因到哪把 key。这是可观测性缺口,不是授权缺口。
3. **文档口径张力**。`docs/guides/app-integration.zh-CN.md:18,40` 称 key 为「你的应用
   专属密钥」并要求「一个应用一把 Key」,运维可能据此把两个应用放进同一 project 并
   期待隔离 —— 而该期待在预算、限流、白名单、CIDR、脱敏任何维度上都不成立。
   「需要隔离就拆 project」这句话没有任何文档明说。

---

## V5 — 全局 FIFO + 硬编码 4 worker 可跨项目饿死 · CONFIRMED

由主持方直接核验,未派角色:这是纯代码事实,不需要独立视角。

`grep -rn 'DeferredResponseWorkers' --include='*.go' internal | grep -v _test` 全仓只有
三处:字段声明两行(`internal/gateway/service.go:146,149`)与一次读取
(`:955`)。**没有任何赋值点**,所以 `options.DeferredResponseWorkers` 恒为 0,
`newDeferredEngine` 在 `internal/gateway/deferred_response.go:76` 落到
`deferredDefaultWorkers = 4`,运维无从调整。

而队列上限是 **Project 级**字段(`internal/domain/models.go:269` `MaxDeferredQueue`,
天花板 `MaxDeferredQueueCeiling = 10_000`,`:297`),消费能力却是全局固定的 4。
`PendingDeferredResponses` 按 `SubmittedAt` **全局**排序
(`internal/store/bolt/store_providers.go:508-510`),没有任何按项目的轮转。

后果由核心逻辑角色给出算术:单项目灌满 10000 条、每条最坏 `route_total_timeout`
(默认 2 分钟),排空需 10000/4×2min ≈ **83 小时**,远超 24h TTL。此后其他项目的每一条
提交都排在这些之后,在 24h 后以 `deferred_response_expired` 失败 —— 而这条失败在
ledger 里没有事件(提交不写 ledger、过期路径也不写)、在失败列表里没有行、记录一小时
后被 reaper 删除。运维唯一的线索是调用方投诉。默认 100/项目 × 十个项目同样成立:
1000/4×2min ≈ 8.3 小时的队首延迟。

ADR 0024 论证了「有界队列在压力面前当面拒绝」,但界是每项目的,消费能力是全局的,
两者不匹配。

**级别 P1。** 默认配置下即可触发,且失败对运维不可见。
