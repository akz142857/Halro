# V6 · 对抗验证：B1-09「连接失败的请求按 `max_completion_tokens` 满额结算」

- 被验证发现：B1.md · B1-09（原判 P2 疑似BUG，需裁决，未经 A1/A5 复核）
- 验证 HEAD：`2cd24a76a569fe53f878c1ab1be31441f4c008e0`（与 B1 实跑同一 HEAD）
- 验证时间：2026-08-11 19:1x ~ 19:3x (+0800)
- 所用模型：**Fable 5（claude-fable-5）**
- 拒答 / 空响应：无
- 隔离方式：仓库工作树只读；二进制构建产物、两套实例（config/data/master.key）、探针程序
  全部在 scratchpad；上游为不可路由/立即断开的本地地址；未向任何真实 Provider 发请求。

```
$ git status --porcelain
?? docs/review/260811/releases.1.0.0/
```

（仅评审目录未跟踪——本报告即在其内。无任何仓库文件改动。）

---

## 1. 裁决：**PARTIAL**

**B1-09 的核心主张被证伪，但它的原始观测是真实的，且背后有一个更窄的真缺陷。**

- **证伪的部分（主张的机制与外推）**：「`error_class: connect` = 字节从未发出 = 按满额结算」
  与「上游一断，项目预算就会被满速烧掉」都不成立。**真正"上游根本没连上"的两类失败——
  dial 被拒绝（connection refused）与 DNS 解析失败——在本 HEAD 上结算为 0，预约全额释放，
  不进预算。** 本轮实测：对一个 connection-refused 的上游发请求得到 502，
  `halro_cost_usd_total` 保持 0.000000，预算无消耗。这不是巧合，是 260806 的提交
  `d116f77`（"fix: route around a provider that was never reached"）专门修的，
  且有三个具名回归测试守护（见 §4）。上游 HTTP 5xx（含挂掉的 LB 返回 502）同样结算为 0。
- **成立的部分（观测本身 + 一个更窄的缺陷）**：确实存在"零字节发出却按满额估算结算"的路径，
  B1 观测到的 130 micros 是真实的**已结算成本**（不是预约中间态，见 §3）。触发它的不是
  "上游宕机"，而是两类特定形态（§5）：其一是**有意设计**（契约明文），其二是
  **`provider.Unsent` 分类边界的真实缺陷**——safetransport 在 dial 前自行拒绝
  （被拒地址/空解析/允许清单不命中）产生的是裸 `fmt.Errorf` 错误，`Unsent` 认不出来，
  于是一个**由本方代码保证零字节发出**的失败被当作 ambiguous 满额结算，且不做 failover。
  本轮在一个 DNS 被劫持成 fake-IP（198.18.4.9，落在 safetransport 的 denied 前缀
  198.18.0.0/15 内）的环境下实测复现：两个请求各结算 130 micros，第三个请求被
  `budget_exceeded` 拒绝——B1 描述的账面现象逐字重现，但机制完全不同。

**严重度校正**：原判 P2 高估约一档、机制归因错误。普遍性主张（"每次上游宕机都烧预算"）
不成立；残余缺陷（§5.2）建议 **P3 / 问题 / 不阻塞 v1.0.0**——触发条件窄
（劫持型 DNS、rebinding 防护触发、解析异常）、失败方向保守（只会多记账、绝不少记账，
符合仓库 fail-closed 取向），但触发时的后果与 B1 担心的一致（按预约满速消耗预算 + 抑制
failover），值得修。另附带一个 P3 建议：`error_class: connect` 同时覆盖"从未连上"与
"连上后失败"两种事实，正是这个标签把 B1 引向了错误前提。

## 2. B1 主张的路径逐步走查

B1 的推理链是：`error_class: connect` ⇒ 字节从未发出 ⇒ 却结算了 130 micros ⇒
连接失败被满额结算 ⇒ 上游宕机烧光预算。逐环走查：

**环 1（error_class 的语义）——断裂。** openai 适配器把 `client.Do` 返回的**一切**
非 deadline 错误都标成 `ErrorConnect`（`internal/provider/openai/adapter.go:306-316`，
Chat 路径）：dial 被拒、DNS 失败、TLS 握手失败、连上后被 RST、safetransport 自身的
策略拒绝，全都叫 "connect"。所以 `error_class: connect` **不能**推出"字节从未发出"。

**环 2（结算逻辑）——存在明确的免费分支。** 结算入口
`internal/gateway/service.go:829` → `settlementForResult`（`service.go:2037-2080`）：

- `service.go:2046-2051`：providerErr 非空且 `!classified.Ambiguous` 时**提前返回**
  `{Outcome: "provider_error"}`——tokens 全 0、`CommittedMicrosUSD` 0。
- `service.go:2059-2064`：仅当 `Ambiguous == true` 才按估算 tokens 记
  `ProviderInputTokens/ProviderOutputTokens/PreparedOutputTokens` 并
  `setSettlementCost`（= B1 看到的 2 + 64×2 = 130）。

**环 3（Ambiguous 的判定）——就是分水岭。** 适配器设
`Ambiguous: !provider.Unsent(err)`（`adapter.go:313`）。
`provider.Unsent`（`internal/provider/provider.go:77-87`）只认两种形态：
`*net.DNSError`，或 `*net.OpError{Op:"dial"}`。注释（`provider.go:74-76`）明言测试
**故意收窄**：认不出的一律保守按 ambiguous。

**环 4（结算落账）——settle 的是 Settlement，不是预约额。**
`attempt.finish`（`service.go:496-503`）→ `settleAttempt`（`service.go:1722-1725`）→
`budget.Manager.Settle`（`internal/budget/manager.go:559-652`）：写入
`EventAttemptSettled`，`CommittedMicrosUSD` 取自 settlement（`manager.go:605-608`），
预约在同一持久事件中释放（`manager.go:557-558` 注释："Settle releases the reservation
and commits usage and cost in one durable event"）。**所以 Unsent 的失败 = 提交 0 +
预约释放；ambiguous 的失败 = 提交满额估算。** 不存在"预约额被原样落账"的第三条路。

结论：B1 主张的"connect ⇒ 满额结算"链条在环 1 与环 3 之间断开。走得通的只是它的一个
子集：**Ambiguous 为真的 connect 类失败**。哪些失败会落进这个子集，见 §5。

## 3. 130 micros 到底是什么

**是 Ledger 已结算成本，不是预约中间态。** Admin 的
`/admin/api/v1/usage/requests/{id}`（`internal/app/admin_usage.go:368-383`）读
`usage.RequestDetail`，其 `cost_micros_usd` 直接来自 **`EventAttemptSettled`** 事件的
`CommittedMicrosUSD`（`internal/usage/aggregate.go:278-284,313`——只在
`case ledger.EventAttemptSettled` 分支填充；`EventAttemptStarted` 只记时间戳，
`aggregate.go:276-277`）。B1 看到的 `output_tokens=64(prepared)` 对应 settlement 的
`PreparedOutputTokens`，同样只在估算分支被置位（`service.go:2061`）。

所以 B1 读到 130 时，那笔钱**确实已从预约转为已提交成本**、计入项目当日预算消耗——
在这一点上 B1 没有看错。本轮复现也证实了后果：预算 300 micros 的项目，两笔此类失败
（2×130=260）后第三笔请求被 `budget_exceeded`（HTTP 403）拒绝。**"占用后释放"与
"落账成真实花费"的分辨结果：对 ambiguous 类失败是后者；对 Unsent 类失败是前者。**

## 4. 实证一：真正"没连上"的失败结算为 0（证伪主项）

环境：干净实例（scratchpad），`allow_private_provider_endpoints: true`，
`halro bootstrap` 建链，upstream = `https://192.168.10.97:59911`（本机私网 IP、
无监听 → dial 立即 connection refused），定价 1.0/2.0 USD per M（与 B1 相同），
日预算 300 micros，请求带 `max_completion_tokens: 64`。

```
$ curl ... /v1/chat/completions -d '{"model":"chat","max_completion_tokens":64,...}'
{"error":{...,"code":"provider_error"}}   HTTP 502   （0.167s，即时拒绝）

$ curl -H "Authorization: Bearer <metrics-token>" http://127.0.0.1:58190/metrics | grep ...
halro_requests_total{status="error"} 1
halro_cost_usd_total 0.000000              ← 若按 B1 主张应为 0.000130
halro_policy_rejections_total{reason="budget"} 0
```

`halro_cost_usd_total` 是从 Ledger 重放的读模型总额
（`docs/contracts/metrics-reference.md:142`），0 即"结算成本为 0、预约已释放"。

守护测试（`-count=1` 全部通过）：

- `TestUnreachableProviderIsNotBilled`（`internal/gateway/unsent_attempt_test.go:40-54`）：
  refused dial 的 settlement tokens 为 0 且非估算。
- `TestUnreachableProviderFailsOverToTheNextTarget`（同文件 :30-34）：refused dial
  会 failover。
- `TestAttemptThatReachedTheProviderStillStopsAndSettles`（同文件 :59-77）：连上后
  被 reset 的失败仍按估算保守结算——**收窄是防复计费的安全边界**。
- `TestUnsentRecognisesARefusedConnectionFromTheStandardLibrary`
  （`internal/provider/unsent_test.go:49-70`）。

```
$ go test ./internal/gateway/ -run 'Unreachable|AttemptThatReached' -count=1   # PASS
$ go test ./internal/provider/ -run 'Unsent' -count=1                          # PASS
```

另：上游（或中间 LB）返回 HTTP 5xx 的失败也结算为 0——`classifyHTTPError`
（`adapter.go:541-562`）从不置 `Ambiguous`，走 `service.go:2049-2050` 的免费分支。
所以"上游宕机"的三种最常见表现（refused / NXDOMAIN / LB 502）**全部不烧预算**。

## 5. 实证二：两类"零应用字节发出却满额结算"的真实形态（B1 观测的来源）

### 5.1 TCP 被接受后立即断开（有意设计，契约明文）

形态：中间设备（docker-proxy / vpnkit / TLS 终结器 / LB）接受了 TCP 连接，随后在
TLS/HTTP 完成前断开。dial 成功 ⇒ 不是 `Op:"dial"` ⇒ `Unsent=false` ⇒ ambiguous ⇒
按估算满额结算，且不 failover。

复现（accept-then-close 监听器模拟该形态）：

```
req1: HTTP 502   req2: HTTP 502   req3: HTTP 403 {"code":"budget_exceeded"}
halro_cost_usd_total 0.000260               ← 2 × 130 micros，与 B1 数字逐位一致
halro_policy_rejections_total{reason="budget"} 1
```

这**是** `docs/contracts/gateway-correctness.md:12-17` 写明的规范行为："A failure is
ambiguous when the request may have reached the provider ... The attempt is
conservatively settled"。字节确实离开了本进程（TCP 握手 + TLS ClientHello），Halro
无法证明上游没收到请求，保守结算方向正确。**B1 要求"若是有意设计必须写进
gateway-correctness.md"——它已经写在那里了。**

B1 的实跑环境（macOS + Docker Desktop）恰好容易落进这一形态：vpnkit/docker-proxy
对已死后端仍接受 TCP 连接再断开。

### 5.2 safetransport 在 dial 前自行拒绝（真缺陷，`Unsent` 认不出自家的拒绝）

`pinnedDialContext`（`internal/safetransport/transport.go:131-161`）有三个**保证零字节
发出**的拒绝点，返回的都是裸错误，不含 `*net.OpError`/`*net.DNSError`：

- `:139` 允许清单不命中：`fmt.Errorf("outbound host %q is not in the allowlist", host)`
- `:151` 解析结果为空：`errors.New("outbound host resolved to no addresses")`
- `:155` 解析出的地址被策略拒绝（denied 前缀 / 私网 / metadata，
  `validateAddress`，`transport.go:190-216`）：`fmt.Errorf("outbound host %q: %w", ...)`
  包一个裸 `fmt.Errorf`

这三类是**本方代码拒绝拨号**——比 refused dial 更强的"未发出"证明——却因错误形态
不被 `Unsent`（`provider.go:77-87`）识别而按 ambiguous 满额结算，且不 failover。

复现（本机 DNS 为 fake-IP 劫持环境，`v6-halro-nonexistent.invalid` 被解析成
198.18.4.9，落在 deniedPrefixes 的 198.18.0.0/15，`transport.go:171`）：

```
$ nslookup v6-halro-nonexistent.invalid   →  Address: 198.18.4.9
dns-req1: HTTP 502   dns-req2: HTTP 502   dns-req3: HTTP 403 budget_exceeded
halro_cost_usd_total 0.000260               ← 又是 2 × 130，第三笔预算拒绝
```

独立探针（scratchpad 内 standalone Go，逐字复制 `Unsent` 逻辑与 `pinnedDialContext`
的错误包装）确认机制：

```
LookupNetIP: addrs=[198.18.4.9] err=<nil>
client.Do error: ... outbound host "...": reserved address 198.18.4.9 is not allowed
  Unsent(doErr) = false  -> Ambiguous = true   （满额结算）
refused dial error: ... connect: connection refused
  Unsent(refusedErr) = true -> Ambiguous = false （结算 0）
```

真实触发场景：fake-IP/劫持型 DNS 环境（本机与 B1 的实跑机就是）、DNS rebinding 防护
触发（Provider 域名开始解析出私网/保留地址）、解析返回空集。注意副作用是双重的：
除了烧预算，**Ambiguous 还抑制 failover**（`retryable`，`service.go:1773` 一带），
多 Deployment 路由同样绕不开——这正是 `d116f77` 想修而没修全的同族缺陷。

修复方向：让这三个拒绝点返回可识别的类型化错误（或包一个 `*net.OpError{Op:"dial"}`
语义的哨兵），把"传输层自证未发出"纳入 `Unsent`；并为 `error_class` 拆分或在 usage
行暴露 ambiguous/unsent 标志，消除 §2 环 1 的标签歧义。均需带负面测试
（fake-IP → denied 地址 → 结算必须为 0）。

## 6. 对 B1-09 各断言的逐条裁决

| B1-09 断言 | 裁决 |
|---|---|
| 失败请求结算了 130 micros，等于 2 + 64×2 的满额估算 | **属实**（已结算成本，非预约中间态，§3） |
| `error_class: connect` 说明字节从未发出、上游根本没连上 | **不成立**——该 class 覆盖一切非 deadline 的 `client.Do` 失败（§2 环 1） |
| 连接失败（未发出）的请求按满额结算 | **不成立**——refused dial / NXDOMAIN / HTTP 5xx 结算 0 并释放预约（§4）；满额结算仅发生在 ambiguous 子集（§5） |
| 上游一断，项目预算被按最大补全长度满速烧掉 | **大体不成立**——直连宕机不烧；仅"接受连接的死中间层"或 §5.2 缺陷形态下成立 |
| 若为有意设计须写进 gateway-correctness.md | **已写明**（`gateway-correctness.md:12-17`）；但 §5.2 子集超出了该设计意图，是缺陷 |
| 原判 P2 疑似BUG、需裁决 | **降为 P3**：残余缺陷（§5.2）触发窄、方向保守（只多记不少记）、不阻塞 v1.0.0；另加一条 P3 建议（error_class 语义拆分） |

## 7. 附录

### 7.1 读过的文件

- `docs/review/260811/releases.1.0.0/role-prompts.md`（§1、§8）
- `docs/review/260811/releases.1.0.0/findings/B1.md`（全文）
- `internal/gateway/service.go`（Chat 主循环 :760-876、startAttempt/finish/settleAttempt
  :360-530,1707-1725、settlementForResult/enrichSettlement :2037-2143、MessagesNative :1056-1120）
- `internal/gateway/unsent_attempt_test.go`（全文）
- `internal/provider/provider.go`（Error/Unsent :55-115）
- `internal/provider/unsent_test.go`（经 grep 定位关键断言）
- `internal/provider/openai/adapter.go`（ListInvocationTargets :139-180、probe :240-270、
  Chat :277-339、Embed :341-390、classifyHTTPError :541-574）
- `internal/provider/primitive.go`（legacyGenerationPrimitive.Generate :116-170）
- `internal/safetransport/transport.go`（NewClient/ValidateURL :41-130、
  pinnedDialContext/validateAddress :131-216）
- `internal/budget/manager.go`（Settle/settle :555-652、reserve 一带经 grep）
- `internal/usage/aggregate.go`（AttemptEvent 构建 :255-344）
- `internal/app/admin_usage.go`（adminUsage/adminUsageRequest :300-383）
- `internal/app/bootstrap.go`（全文）
- `internal/app/providers.go`（providerEndpointPolicy :25-45）
- `internal/app/metrics.go`（MetricsToken/authorizeMetrics :1-60）
- `cmd/halro/main.go`（bootstrap/stats/metrics/init/serve 命令段）
- `configs/config.example.yaml`（server/storage/security/metrics 键）
- `docs/contracts/gateway-correctness.md`（:1-40）
- `docs/contracts/metrics-reference.md`（经 grep：`halro_cost_usd_total`）

### 7.2 走过的调用链

失败请求结算主链：
`Service.Chat`（service.go:788 循环）→ `startAttempt`（:800，
`ReserveAttemptDetailed` :393 预约落账）→ `legacyGenerationPrimitive.Generate`
（primitive.go:127 → openai `Adapter.Chat` adapter.go:277）→ dial 失败分支
（adapter.go:304-316，`Ambiguous: !provider.Unsent(err)`）→ 错误原样上抛
（primitive.go:134）→ `settlementForResult`（service.go:829→2037，Ambiguous 分水岭
:2049）→ `attempt.finish`（:496）→ `Service.settleAttempt`（:1722）→
`budget.Manager.Settle`（manager.go:559→567，`EventAttemptSettled` 同事件释放预约、
提交成本）→ usage 读模型（aggregate.go:278 起，`cost_micros_usd` = 事件
`CommittedMicrosUSD`）→ Admin API（admin_usage.go:377）/ `halro_cost_usd_total`。

dial 内部策略链：`http.Transport.DialContext` = `pinnedDialContext`
（transport.go:76→131）：allowlist :139 → 解析 :145 → 空集 :151 →
`validateAddress` :155→190 → `dialer.DialContext` :159。

### 7.3 运行过的命令（关键）

```bash
go build -o <scratch>/v6/bin/halro ./cmd/halro
# 实例一：refused-dial 上游（https://192.168.10.97:59911，无监听）
halro init / bootstrap（openai_compatible, 1.0/2.0 USD per M, 日预算 300 micros）/ serve
curl /v1/chat/completions（max_completion_tokens: 64）        # 502，cost 0
halro metrics token && curl :58190/metrics                     # halro_cost_usd_total 0.000000
# 形态 5.1：accept-then-close 监听器（python，192.168.10.97:59911）
curl ×3                                                        # 502/502/403 budget_exceeded
curl :58190/metrics                                            # 0.000260，budget rejection 1
# 实例二：fake-IP 劫持 DNS 上游（https://v6-halro-nonexistent.invalid → 198.18.4.9）
curl ×3                                                        # 502/502/403 budget_exceeded
curl :58290/metrics                                            # 0.000260，budget rejection 1
nslookup v6-halro-nonexistent.invalid                          # 198.18.4.9（denied 前缀）
go run <scratch>/v6/probe                                      # Unsent 分类独立验证
go test ./internal/gateway/ -run 'Unreachable|AttemptThatReached' -count=1   # PASS
go test ./internal/provider/ -run 'Unsent' -count=1                          # PASS
git merge-base --is-ancestor d116f77 2cd24a7                   # 是（修复在评审 HEAD 内）
git status --porcelain                                         # 仅 ?? docs/review/260811/releases.1.0.0/
```

### 7.4 无法验证的事项

- B1 的 130 micros 究竟由 §5.1（docker-proxy/vpnkit 接受后断开）还是 §5.2
  （容器 DNS 回落到宿主的 fake-IP 解析 → denied 地址拒绝）触发，无法事后判定——
  两者都在本轮以逐位一致的数字复现，任一都足以产生 B1 的观测。B1 的实跑机与本机是
  同一台（macOS + fake-IP DNS 环境），两种形态都真实可及。
- 真实 NXDOMAIN 端到端路径（`transport.go:147` → `*net.DNSError` → 结算 0）在本机
  无法实测（DNS 被劫持，NXDOMAIN 不可得）；该路径由代码走查
  （`%w` 保链 + `Unsent` :81-84）与 `internal/provider/unsent_test.go` 单测支撑。
