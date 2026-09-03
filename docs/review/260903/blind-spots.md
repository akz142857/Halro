# 260903 评审 · 测试盲区地图

基线 `v0.5.0` → `ff12842`。角色:测试盲区。方法是两遍——对每个新增源文件找它的测试,
再对每条新引入的不变量找守护它的断言。所有"无测试"结论都注明了搜过的文件名或符号。

验证过的事实(非推断):`go test ./internal/{failurecapture,usage,ledger,gateway,config,logging}/`
全绿;覆盖率取自 `go test -coverprofile` 的真实运行,不是估计。

---

## 一、总体判断

**这一轮的测试不是"写少了",是"写偏了"。** 30 个新测试文件、约 3000 行测试代码,
密度不低,命名讲究,不变量表述清楚。问题出在分布:

- **单元层厚,装配层薄。** 纯函数与 store 级行为守护得很好;把它们接到一起的那行代码几乎没有断言。
  三个例子:`RunDeferredResponses` 里的 `engine.recover(ctx)`(`internal/gateway/deferred_response.go:104`)、
  `DeferredExecutionTimeout: cfg.Gateway.RouteTotalTimeout.Value()`(`internal/app/runtime.go:473`)、
  `openFailureCapture` 传 `Retain` 的几行(`internal/app/failure_capture.go:31-35`)。
  **删掉其中任何一行,全套测试仍然全绿**,而三者分别对应:重启后在途请求永久挂起、
  单次延迟执行不受超时约束、留存窗口不按配置生效。
- **copilot-instructions 点名的四类,只有一类合格。** 负向路径覆盖得相当好(见第三节);
  崩溃恢复只在 ledger 一处做到位;回滚路径基本只断言"返回了 error",不断言"回到了一致状态";
  **并发路径几乎为零**——30 个新测试文件里 `go func` 总共出现 3 次,集中在 2 个文件。
  CI 跑 `go test -race ./...`(`.github/workflows/ci.yml:67`)不构成缓解:race detector 只报告
  测试真正跑过的交错,没有并发测试就没有交错可报。
- **一个盲区已经藏住了真缺陷**(见 P0-1)。这不是"可能有问题",是读代码就能确认的。

发布判断:**P0 两条应在 v0.6.0 前处置**,其中 P0-1 是修代码不是补测试。P1 若不处置,
应在发布评估里记名,理由是它们守护的都是记账权威与调用方数据留存。

---

## 二、盲区清单(按风险排序)

### 【问题】P0-1 · 失败诊断读侧重复了日志侧刚修掉的缺陷,且无测试

`internal/usage/failures.go:168` 的 `lastFailure` 倒着走 attempt 链、跳过 `status == "success"`,
返回最后一个失败的 attempt。它的注释断言这个形状不可能出现:

> `// 一个请求只能以失败或没有 attempt 结束`

**网关与这条注释矛盾,我逐行确认过。** 在 unary generate 路径上,attempt 先于渲染/脱敏裁决结算:
`settlementForResult` + `attempt.finish(providerErr=nil, ...)` 在 `internal/gateway/service.go:1194-1197`,
此时 outcome 是 `success`;渲染裁决与 `run.finalize(outcome)` 在 `:1218-1250`,把**请求**标成
`provider_error` / `policy_rejected`。所以"回退后成功、随后渲染失败"的请求,attempt 链是
`[失败, 成功]` 而请求非成功,`lastFailure` 会返回**第一次尝试**的
`provider_status` / `provider_code` / `provider_request_id`。

这正是本轮在日志侧找到并修掉的缺陷,带名字的回归测试就在旁边:
`internal/gateway/final_failure_log_test.go:421`
`TestARenderFailureAfterAFallbackDoesNotReportTheEarlierAttempt`,注释写着
"运维拿着那个标识符去问上游,问的是一次成功的调用"。**读侧是同一个倒走逻辑,没有对应测试。**
`internal/usage/failures_test.go` 与 `internal/app/admin_usage_failures_test.go` 里没有任何
fixture 是"失败 attempt 后面跟一个成功 attempt 而请求非成功"。

缺了会让什么存活:`/admin/api/v1/usage/failures` 与控制台 `FailureDetailDrawer` 把一次成功调用的
上游 request ID 展示给运维,作为一次失败的证据。与日志侧已修的是同一个用户可见后果。

该测的:`internal/usage/failures_test.go` 加一个 `req_render` fixture(att1 失败、att2 成功、
request outcome `provider_error`),断言 `FailureContext` 不来自 att1。修法参照日志侧的
`terminalDescriptor`。

---

### 【问题】P0-2 · 全新北向端点族 `POST/GET/DELETE /v1/responses`,端点层零测试

实测覆盖率:`internal/gatewayapi/deferred_response.go` 的
`GetDeferredResponse`、`CancelDeferredResponse`、`DeleteDeferredResponse`、`deferredAction`、
`writeDeferredError`、`writeJSON` **全部 0.0%**。
`grep -rn '"/v1/responses/' --include='*_test.go'` → **零命中**,没有任何测试对这三个路由发过 HTTP 请求。

三个互相放大的漏洞:

1. **没有 `var _ DeferredResponsesService = (*fakeService)(nil)` 编译期钉子。**
   同一个文件对 `MessagesService` 有(`internal/gatewayapi/handler_test.go:53`),就是为了防这个失效模式。
   `handler.go:381` 用 comma-ok 断言取接口,`fakeService` 不实现它 → `h.deferredResponses == nil` →
   `TestResponsesRejectsStateBeforeServiceInvocation`(`handler_test.go:377`)测到的是 501 分支。
   接口一漂移,四个端点静默全变 501,编译和测试都不报。
2. **调度器整体未执行。** `RunDeferredResponses` 与 `dispatch` 实测 0.0%。测试用 helper
   `runOnce`(`deferred_response_test.go:68-79`)直接在测试 goroutine 上调 `runDeferredResponse`,
   绕开了 worker slot 上限、`running` sync.Map 生命周期、backoff 阶梯、`blocked atomic.Bool`。
3. **`recover()` 的接线未测。** `recover` 本身有测试(`deferred_response_test.go:355/:381` 直接调用),
   但它唯一的生产调用点是 `deferred_response.go:104`,在 0% 覆盖的 `RunDeferredResponses` 里。
   删掉那一行,测试全绿,而重启后所有 `in_progress` 记录永远停在 `in_progress`,直到 24h TTL。

该测的:`internal/gatewayapi/` 加接口钉子(一行,收益最大);
`internal/app/` 加端点级测试打这四条路由(其余 admin 路由都有,这族没有);
`internal/gateway/` 加一个真正启动 `RunDeferredResponses` 的测试。

---

### 【问题】P1-1 · 延迟取回:花了钱的那几条路径全部无断言

`internal/gateway/deferred_response.go` 有 21 个测试,覆盖提交/执行/取回主干。
**唯一"对上游花了钱、然后出事"的几条分支一条都没执行到**:

| 不变量 | 位置 | 判定 |
|---|---|---|
| 取消 `in_progress`:查 `running`、调 CancelFunc | `:745-754` | **无测试** — `running` 只由 0% 的 `dispatch` 写入,grep `deferred_response_cancelled` 全树零命中 |
| ADR 0011 保守结算(释放预留、提交成本、不退款) | 取消/超时/中断/超大回答四条终态 | **无测试** — 四个终态测试没有一个读 `f.state.Balance` |
| 已终结再取消 → 409 `deferred_response_not_cancellable` | `:755-759` | **无测试**(grep 零命中) |
| 队列中记录过 TTL → `deferred_response_expired` | `:159-165` | **无测试**(grep 零命中;且在 0% 的 `dispatch` 内) |
| 跨 project 取回 → 404 | `deferredOwner` `:793-816` | **无测试** — fixture 全程单 project(`service_test.go:401` 硬编码 `project_1`) |
| D3 部署 pin 失效 → 409 `deferred_response_route_unavailable` | `:615-621` | **无测试** — pin 在提交时设置,但"路由变了要拒"这个 pin 存在的理由从未被断言 |
| `budget_exceeded` 必须失败而非重排队(否则循环到 TTL) | `:563-573` 的 code 列表 | **无测试** — 只有 `concurrency_limit_exceeded` 的重排队被测(`:398`) |

`TestADeferredAttemptIsBoundedLikeASynchronousOne`(`:626`)是条好测试(真的注入 20ms 超时并断言
`deferred_response_timeout`),但它注入的是 service 字段;**没有测试断言这个字段来自
`route_total_timeout`**(`runtime.go:473`,grep `DeferredExecutionTimeout` 无任何 `_test.go` 命中)。

已守护的(应记功):提交不写 ledger、不碰上游(`:94`);执行计费与同步一致(`:250`);
队列满 429 + Retry-After(`:307`);重启回收 queued(`:372`)与失败 in_progress(`:344`);
限流在出队时退回队列并保留输入对象(`:398`);冷却窗口(`:519`);撤销的密钥停工(`:550`)。

---

### 【问题】P1-2 · `internal/app/ledger_seal.go` 零测试,含唯一的 fail-closed 闩

实测:`sealLedgerGeneration`、`compactLedgerSegments`、`ledgerArchivedThrough`、
`SealLedger` **全部 0.0%**。247 行新增的记账权威编排代码,没有任何测试。

最要紧的一条:`ledger_seal.go:70` 在密封后的世代回读校验失败时调 `r.status.RequireRecovery()`
——这是本轮唯一一个会让网关整体停止服务的新闩,从未被任何测试执行过。
其次:`ledgerArchivedThrough` 的两个 not-found 吞掉分支(`:122`、`:133`)返回 `(0, false)`
会让压缩永久停止,无断言。

同类:`internal/durable/directory.go` **覆盖率 0.0%,包内无测试文件**,9 个调用方
(ledger seal/segment、backup、key_rotation、kms_master_key、usage/parquet、vault、
gateway/inference_resources_store)没有一个能注入失败——它是包级函数,没有 seam。
对目录 fd 做 `Sync()` 在若干文件系统上返回 `EINVAL`/`ENOTSUP`;真发生时 `Roll` 每个维护 tick
都会 `MarkUnavailable()`,一个 fail-closed 的砖。按 CLAUDE.md「fail-closed 检查最值得推敲,
因为错了是拒绝启动而不是降级」,这条至少该有一个 `t.TempDir()` 上的正例和一个不存在路径的负例。

---

### 【问题】P1-3 · 失败捕获:真实 AES-256-GCM 密封没有任何负向测试

`internal/failurecapture/failurecapture_test.go` 全部用 `fakeSealer`(`:24-30`),一个字符串前缀替身。
`grep EncryptFailurePayload/DecryptFailurePayload --include='*_test.go'` → 只命中那个替身。
`internal/vault/vault_test.go` 4 个测试全是 credential scope,**没有一个碰 failure-payload scope**。

AAD 组成本身是对的(`internal/vault/vault.go:186-190`:request ID + kind + project,
密钥派生与 AAD 双重绑定),但:

- **换 request ID 的文件替换攻击无测试** — 把 `<millis>-req_1.hfc` 改名成 `req_2.hfc`
  再 `Get("req_2", …)` 应当拒绝。机制成立、包注释也这么写(`failurecapture.go:263-265`),但无断言。
- **错主密钥 / 跨实例无测试**。
- 结果:`internal/app/failure_capture.go:109-116` 的解密失败分支(warn + 404)对测试套件而言是死代码。
- **主密钥轮换与已落盘捕获的关系无人测也无人写**:grep `internal/app/key_rotation.go`、
  `kms_master_key.go` 的 `failures` / `FailurePayload` → 零命中。轮换后旧捕获永久不可读,直到过期。

Halro 全库唯一一份"调用方写的内容"落盘,密封的负向路径为零。

---

### 【问题】P1-4 · 没有任何断言说捕获的 prompt 不进备份

`data/failures` 不在 `internal/app/backup.go:661-681` 的归档清单里——**这是正确的**
(捕获按时钟过期,进了备份就成了永久副本,且备份可能被复制出机器)。
但 `grep -rn 'failures\|failurecapture' internal/app/backup.go internal/backup/*.go` → **零命中**,
`internal/app/backup_test.go` 与 `internal/backup/*_test.go` 同样零命中。

排除是"靠遗漏成立"的,没有断言。往归档清单里加一行 `data/failures/**`——把一个 24 小时的
prompt 存储变成永久的、离机的——**不会有任何测试失败**。这条留存承诺是这个特性的全部立身之本,
值得一条断言。

---

### 【建议】P1-5 · 并发路径:新特性侧近乎全空

30 个新测试文件,`go func` 共 3 处(`ledger/seal_concurrency_test.go` 2 处、
`gateway/capture_test.go` 1 处)。`internal/usage/*_test.go` 与
`internal/gateway/deferred_response_test.go` 的 `go func|sync.WaitGroup|t.Parallel` 全部零命中。

具体未覆盖的交错,每条都对应真实结构:

- **延迟取回**:两个 worker 抢同一条记录;两个收集方同时轮询同一 id(都会写 `RetrievedAt`,
  一方 CAS 失败只记日志,`:715-717`);取消与 worker 完成竞争(`running.Load` vs `running.Delete`,`:180`);
  队列边界并发提交(源码 `:396-400` 自称"故意近似",但近似到什么程度无断言)。
- **ledger**:`Roll` 与 append 并发(`Roll` 全程持 `l.mu`,但无断言);`Roll` 与 `Compact` 并发
  ——`Compact` 在压缩期间放开 `l.mu`(`seal.go:207-228`)再重解析 `findSegment`(`:233`),
  三个重解析分支(`:234-246`)正是并发 roll 会打到的,全无测试;
  `Compact` 与 `StageSegments`/`VerifySealed` 并发(后两者在锁外做文件 I/O,
  前者 `os.Remove(source)`,`:264`)——备份或 verify 撞上维护 tick 会拿到 `ErrSegmentMissing`。
- **usage checkpoint**:`TakeCheckpoint` 故意在编码时放开锁(`checkpoint.go:195`)、
  `CommitCheckpoint` 再取回(`:320`),而 collector 的 `Apply` 在另一 goroutine 上。这个缺口无断言。
- **failurecapture 日额上限**:实现是对的(`reserve` 在 `s.mu` 内 check-and-increment,
  `failurecapture.go:236-248`,无 check-then-act 窗口),但没有并发测试,该包也从未在 `-race` 下跑过任何交错。

已守护的一条,值得记功:`TestCompactionDoesNotHoldTheWriterLock`(`seal_concurrency_test.go:52`)
真的把 `Compact` 和 `Append` 并发起来了,是本轮唯一一条 race 覆盖到的密封路径。

---

### 【问题】P1-6 · 崩溃恢复:密封世代的"损坏 vs 断尾"规则说了三处,一处未断言

源码在三个地方声明同一条规则——密封世代不可变,读短了是损坏而非可修复的断尾:
`seal.go:348-350`(`VerifySegments`)、`log.go` 的 `Replay`、`InspectReplay`。

`grep 'Truncate|truncat' internal/ledger/*_test.go` → 只命中 `log_test.go:381/:398/:425`,
**全部针对活动文件**,断言的是相反的策略(断尾被修复)。**没有任何测试截断过一个密封 segment。**
连带未守护:`checkSegmentsPresent` 的尺寸不符(`segment.go:483`)与 `StoredLength <= 0`
(`:479`)拒绝分支;gzip 不可读(`segment.go:237`)与"解压不还原明文"(`segment.go:384`)。

`TestAnInterruptedRollIsFinishedOrAbandonedByTheFilesAlone`(`seal_test.go:306`)只有两个子测试,
且都是**手工伪造** pending manifest(`:322`、`:356`)而非中断一次真实 `Roll`。
`Roll` 的六个崩溃窗口覆盖三个;`Roll` 的七个前置条件(含 `len(l.chainKey) != 32`——
无密钥的 log 会用空 `EndHash` 密封)**全部无测试**,grep 各自的错误字面量全零命中。

---

### 【建议】P2-1 · `runUsageMaintenance` 整体无测试,其顺序约束是承重的

`internal/app/runtime.go:943-991` 是驱动 export → 窗口裁剪 → 归档裁剪 → 捕获清扫 → 密封 → 压缩的
单一 goroutine。`grep runUsageMaintenance internal/app/` → 只有定义与启动两处,**没有任何测试驱动它**。
每个步骤函数都被单独测过,顺序没有。

源码注释明确说明顺序是承重的("裁剪受 export 走到哪里约束,先读那个水位就白白落后一个 tick"、
"归档最后,因为控制台窗口可能还在读那个分区")。重排会造成派生物先于 export 被裁掉。
`ctx.Done()` 分支只跑 checkpoint + export + purge,**不跑 seal/compact**,同样无断言。

---

### 【建议】P2-2 · 新增 Admin 端点仍不在冻结清单,且该测试是单向的

本轮新增四条:`GET /admin/api/v1/usage/failures`、`.../failures/{}/payload`、
`GET|PUT /admin/api/v1/settings/usage`(`runtime.go:1598-1599, 1609-1610`)。
加上 260901 那轮的 `usage/summary`,五条都不在 `internal/app/admin_contract_test.go:33-67` 的
`expected` 清单里。而 `:69-73` 只检查 `expected ⊆ registered`——**没有反向检查**,
所以新路由不入清单不会有任何提示。这是上一轮 P4 原样重现。

对照:网关路由**本轮刚拿到双向守护**(`internal/app/gateway_contract_test.go:35`
`TestEveryGatewayRouteIsADeclaredNorthboundMethod`,两个方向都查)。同样的形状,
admin 侧还差一半——把 `TestFrozenV1AdminRoutesAreRegistered` 补上反向断言即可。

连带:`TestReadOnlyRoleCannotReachAnyRegisteredMutationRoute`(`admin_users_test.go:285`)
是条很好的全路由扫描,但它显式跳过 GET(`:291`)。于是
**没有任何测试说明 `read_only` 管理员能不能读走一条捕获的 prompt**——按 `runtime.go:1599`
用的是 `requireAdmin` 而非 mutation 门,今天是能的。这是个策略决定,两个方向都没有断言。

---

### 【建议】P2-3 · #259 路由分区:检查点覆盖三处,另有三条通往上游的路径没有检查

| 路径 | 有检查 | 有测试 |
|---|---|---|
| 部署创建 | 是(`admin_deployments.go:1129`) | 纯函数级(`deployment_route_partition_test.go`) |
| 部署更新(PUT,也是唯一的启用开关) | 是(同一函数) | **无** — 没有测试调 `updateAdminDeployment` |
| 连接测试 | 是(`:465`) | 直接调 `deploymentRouteRefusal`,非 HTTP 层 |
| 主动健康探测 | 是(`health.go:77`) | **有,且是这组里最扎实的**(`health_probe_route_test.go:37`,断言探测适配器调用次数为 0) |
| capability-detection 解析分支 | **无** | 无 |
| variant / resolution-revision 解析分支 | **无** | 无 |
| capability preflight 拨号 | **无** | 无 |
| 运行时推理拨号 | **无** | 无 |

`deploymentFromInput` 在三个互斥解析器里选一个(`admin_deployments.go:573-578`),
**只有第三个带这道门**;带 `capability_detection_id` 或 `resolution_revision` 的写入完全绕开。

其余两条:
- HTTP 状态映射 `400` + `"code": "model_not_served_by_profile"`(`admin_deployments.go:125`)
  **无测试**——我实测 grep 全树(去掉 `webui/dist`)只有两处命中:那行源码和 `web/src/i18n/errors.ts:152`。
  八个分区测试断言的都是 Go 哨兵 `errModelNotServedByProfile`。
- **分区表本身没有完备性测试**:`grep RoutePartitioned --include='*_test.go'` → **0**。
  五行声明(`provider_table.go:287,295,306,314,322`)、一处读取,没有任何断言说
  "每个 Mantle profile 都声明了它"或"非 Mantle profile 都没声明"。第六个 Mantle profile
  加进来会静默失去这道拒绝。该扩的是 `internal/app/bedrock_mantle_route_test.go:53`——
  它已经在走这五行了。

---

### 【建议】P2-4 · 前端:三个最容易误导运维的分支没测

- **截断横幅从未渲染。** `FailureDetailDrawer.tsx:179` 的 `usage.failures.payloadTruncated`。
  `grep 'request_truncated\|response_truncated' web/src --include='*.test.tsx'` → **零命中**;
  payload mock(`UsageFailuresPanel.test.tsx:156-161`)不设这两个标志。源码注释自己写着:
  未标注的截断会让运维去查一个上游根本没有的 bug。
- **Go ↔ 控制台没有失败分类契约。** Go 侧 9 个 `provider.ErrorClass`
  (`internal/provider/provider.go:35-46`),前端 10 个(`web/src/failure.ts:18-29`)。
  `errorClasses` 这个导出的数组**除了自己的类型别名外无人引用**(我实测 grep 确认)。
  这个仓库已经有两条同形状的跨语言契约——`admin_provider_profiles_golden_test.go:32` 对
  `provider-profiles.golden.json`,`admin_error_codes_localized_test.go:29` 对 `i18n/errors.ts`
  ——失败分类没有。加一个类,控制台显示原始英文 token,不会有任何测试失败。
- **`UsageWindowForm` 的三个 read-only 守卫可以删掉而套件全绿**
  (`:96` 下拉、`:106` 保存、`:126` 缩短确认——最后一个是破坏性的)。
  `UsageWindowForm.test.tsx` 里 `session|role` 出现 **0 次**,`useIsReadOnly` 恒为 false。
  `readOnlyRole.test.tsx` 本轮只改了 2 行 fixture,没有新增任何新屏幕。

次一级:`UsageFailuresPanel` 的分页与错误态未测(11 个测试里 `next_cursor` 恒为 `""`);
SettingsPage 的 instance pane 没有任何测试渲染过,新增的 `usage-settings` 查询与
`UsageWindowForm` 挂载点均无断言;三条 `errors.consoleWindow*` 文案没有任何测试渲染
(唯一的拒绝测试抛裸 `Error` 而非 `ApiError`);`api.test.ts` 全文没有 401/403/409/429。

---

### 【建议】P2-5 · checkpoint 恢复:13 条拒绝分支覆盖 6 条

`TestRestoreRefusesACheckpointItCannotFullyRead`(`checkpoint_test.go:233`)覆盖 6 条。
未覆盖的 7 条(各自错误字面量全树 grep 零命中)里,最要紧的是
`checkpoint.go:453/456`「持有水位之后的 attempt/request 摘要」——**这正是"派生物不得越过权威"
那条不变量本身**;以及 `:410`「最后一个之前有未密封 segment」,其注释写着"那之后的记录会同时存在于两处"。

上一轮的 **P5 两段读竞态仍在**:`internal/app/admin_usage_summary.go:211` 先读存储的汇总行,
`:227` 再从活的内存聚合读 `Snapshot().Watermark.Sequence`——响应宣称覆盖到行里并不包含的 sequence。
`grep watermark_sequence` 在 Go 测试里 **零命中**(只有 `web/src` 的 fixture)。

**P10 也仍在,且被密封放大**:`openUsageOffline`(`internal/app/usage.go:160`)调
`ledger.InspectReplay` 不传 ChainKey,`RebuildUsageSummary` 随后据此**持久写两个派生物**。
现在 `InspectReplay` 还会走密封世代,同样无认证。`tamperWithSealedFrame` 这个 helper 存在
(`seal_authentication_test.go`),但从未喂给 `RebuildUsageSummary`。

---

### 【建议】P3 · 零散但便宜的几条

- `internal/config` 的 `validateFailureCapture`(`config.go:1360-1378`)三条区间校验
  **零测试**(grep `FailureCapture|max_bytes` 在 `config_test.go` 无命中)。
- `TestDisablingCaptureStillExpiresWhatWasAlreadyWritten`(`admin_usage_failures_test.go:279`)
  名字承诺"仍会过期",**函数体断言的是记录在窗口内仍然存在**——它从不推进时钟。
  清扫会删这件事在 store 级有测(`failurecapture_test.go:181`),缺的是 app 层"配置的 Retain
  真的传下去了";`Options.Now` 存在但 `openFailureCapture` 从不接线,该路径不可注入时钟。
- `openFailureCapture`(`failure_capture.go:26`)把**任何** `os.Stat` 错误(含 EACCES)当作"没有存储"
  → 不启清扫器 → prompt 永久保留。fail-open,无测试。
- `failurecapture.Purge` 用裸 `os.Remove` 且不做目录 fsync,旁边的 parquet 清扫用了
  `durable.SyncDirectory`。不一致、无注释、无测试。
- 审计记录**内容**未断言:`TestReadingACapturedPayloadIsAudited`(`:249-259`)只查 action 与 TargetID,
  不查 actor、不查 project 元数据、**不查审计记录里没有 prompt 内容**(网关侧有对应检查,
  `gateway/capture_test.go:214`,审计侧没有)。被拒绝的读取不写审计,两个方向都无断言。
- `errorfan.go` 的 `fanout.Handle`「一个 sink 坏了不能带走另一个」(`:44-56`)**无测试**;
  包内没有 `errorfan_test.go`。
- segment manifest 版本门(`segment.go:104`)形状是对的(区间而非等值),但**两个方向都无测试**——
  CLAUDE.md 点名这个仓库被等值版本检查咬过多次,这是本轮唯一一个新增的、无测试的版本门。
- `Watermark.After` **专为密封而加**,没有直接单元测试;它存在的理由——密封后跨世代比较
  ——那个组合(有密封 × 非零 checkpoint)从未被执行。
- `web/` 没有 ESLint 配置,也没有测试禁止 `console.*` 打印 payload;存储侧的强守护
  (`secret_canary_test.go:225`)在日志侧没有对应物。

---

## 三、守护充分的区域

这几块不需要在发布前动,列出来是为了让后续评审不重复挖。

**1. 负向参数与拒绝码(admin 层)** — `internal/app/admin_usage_failures_test.go:143-179`
逐个断言 `status=error`、`limit=0`、`limit=101`、坏 cursor、坏日期、超长 request_id 全被拒;
`:180` 断言需要 admin session;`:264` 断言非失败请求与不存在请求的 payload 读取。
`admin_usage_settings_test.go` 覆盖缩短需确认(`:59`)、超过归档留存被拒(`:72`)、低于下限被拒(`:80`)、
revision 冲突 412(`:94`)、ETag(`:89`)、审计(`:106`)。

**2. 失败分类学的四视图一致性** — `internal/gateway/failure_taxonomy_test.go:104`
把 WAL 的 `RequestFinalized.Outcome`、全量重放后的 `RequestsError`、WARN 计数、ERROR 计数
放在一起比。`:261`/`:278` 断言准入前拒绝不写 ledger 请求。
`internal/usage/failures_test.go:20-71` 的 fixture 是**真的多 attempt**
(`req_fallback` 一失败一成功、`req_failed` 两次都失败),`:77` 断言回退后成功的请求不出现在失败列表里
——按请求而非按 attempt 计数这条口径在 usage 层是扎实的(admin 层同名测试则没有多 attempt fixture)。

**3. 无密钥/无 prompt 泄漏(日志与 ledger 边界)** — `final_failure_log_test.go:261`
(未分类错误只记 `%T`,金丝雀不入日志)、`provider_failure_log_test.go:57`(上游拒绝不带响应体)、
`ledger/event_test.go:69`(超长 provider 标识符在持久边界被拒,含 `sk-live-` 与走私换行)、
`logging/error_file_test.go:133`(`gw_` 金丝雀过 authorization 属性,两个文件都 grep)。

**4. 失败捕获的存储级不变量** — `internal/failurecapture/failurecapture_test.go`:
路径穿越拒绝且"没有东西落在存储之外"(`:94-109`)、文件权限与"字节真的过了 sealer"(`:245`)、
日额上限与"只报一次"完整契约(`:146`)、`max_bytes` 双侧上界(`:316`,请求与响应都断言)、
留存按窗口逐条过期并清空日目录(`:181`)、缺 bounds 时 `Open` 拒绝(`:290`)。
网关侧 `capture_test.go` 九个测试覆盖"成功不捕获""回退成功不捕获""策略拒绝不捕获"
"默认关且什么都不存""存储写不进去不改变回答""慢捕获不占租约"。

**5. 派生物可重建(bbolt 侧)** — `internal/app/checkpoint_test.go:18`
删掉 usage checkpoint 后 `reflect.DeepEqual` 全量 `Snapshot()`;
`usage_rollup_test.go:265` 增量 rollup 与单遍重建逐行相等。
`internal/usage/aggregate_test.go:15` 在 126 个 kill point × 2 上做全状态 DeepEqual。
(Parquet 分区的重建没有对应测试,见 P2-5。)

**6. 备份/verify 跨密封世代** — `internal/app/ledger_seal_contract_test.go:102`
断言密封后取的备份恢复出每一个世代(含一压缩一未压缩,`SealedGenerations`/`SealedAuthenticated`/
`Authenticated` 三个数都查);`:187` 断言少一个世代时 verify 拒绝。
`ledger/seal_authentication_test.go:52`/`:89` 覆盖被篡改的密封世代与丢世代的 manifest。

**7. 网关路由的双向契约(本轮新增,值得记功)** — `internal/app/gateway_contract_test.go:35`
两个方向都查:服务了但没有 profile 声明 → 报错;声明了但没服务 → 报错。
`:64` 另外断言每条北向路由都在带守卫的分组里(用中间件计数,并以 `/health/live` 作为见证)。
这是 `docs/contracts/adding-a-northbound-endpoint.md` 点名"三步里最没有机械守护"的那一步,
本轮把它补上了。延迟取回的四个端点因此**在 manifest 与 profile 层面是有守护的**
(`compatibility/manifest.go:388/395/402`、`profile.go:46`)——缺的是端点行为,不是端点存在性。

**8. i18n 完整性** — `web/src/i18n/i18n.test.tsx:18` 断言两个 locale 的 key 集合完全相等
(单边加 key 会失败,不是静默);`:178` 断言没有无人引用的文案;另有全角标点、
一英文术语一中文词、禁 markdown 三条。`internal/app/admin_error_codes_localized_test.go:24`
正则扫描所有 `admin_*.go` 的带码拒绝,要求每个都出现在 `web/src/i18n/errors.ts`。

**9. 浏览器侧不落盘** — `internal/app/secret_canary_test.go:225` 在 `internal/webui/dist` 里
禁止 `localStorage`/`sessionStorage`/`indexedDB` 字面量与 source map。这比单元测试更强:
它让整个包(含 payload 抽屉)在结构上不可能持久化。
配套的 `FailureDetailDrawer` 前端行为也有守护:抽屉打开**不**拉取 payload、
必须点显式的揭示控件(`UsageFailuresPanel.test.tsx:155-178`),这条正是为审计记录而设。

**10. 迁移名冻结** — `internal/store/bolt/migration_names_test.go:40` 与
`store_test.go:230-231` 把本轮两条新迁移(34 `ledger_chain_checkpoint_generation`、
35 `usage_checkpoint_segments`)的版本号与名字钉住,符合 CLAUDE.md「标识符不得复用」那条。
迁移 35 会 drop 并重建 `usage_daily_rollup`,其注释论证是对的(两个派生物一起清,
全量重放才不会重复计数)——但**没有测试在一个已填充的 bolt 文件上跑过这条迁移再验证重建结果**。
