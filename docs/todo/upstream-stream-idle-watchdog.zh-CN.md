# 上游流空闲看门狗：开发文档

- 状态：已在分支 `feat/upstream-stream-idle-watchdog` 实现（2026-10-23），未合并；默认值 120s 为临时值，待 §5 实测后确定
- 范围：Provider 流式响应的读取侧。只加一个上限，不改重试、回退与记账的规则
- 参考：[Respan Gateway](https://github.com/respanai/respan-gateway) 的
  `respan_gateway_http::call::watchdog(send, head, idle)`：响应头等待上限，加上读取响应体时
  两帧之间的空闲上限

## 1. 一句话

上游流每来一点数据就重新计时；超过设定时间一点数据都没来，就判定它卡住了，主动断开，
按现有规则结算，把占着的预留与并发名额放回去。

## 2. 现状（已对照代码核实，`main` 011bc5cd；2026-10-24 按 `main` 1acd9782 更新默认值）

一次流式请求今天受这些上限约束：

| 上限 | 配置 | 默认 | 管什么 |
| --- | --- | --- | --- |
| 连接 | `gateway.attempt_connect_timeout` | 5s | 建连与 TLS |
| 响应头 | `gateway.attempt_response_header_timeout` | 5m（#451 前为 60s） | 发出请求到收到响应头（`safetransport` 的 `ResponseHeaderTimeout`） |
| 下游单次写 | `gateway.downstream_write_timeout` | 15s | 写给客户端的每个事件，`gatewayapi` 每个事件前重新设置 |
| 流总时长 | `gateway.stream_max_duration` | 10m | 整个流式请求（`gatewayapi/handler.go` 的 `context.WithTimeout`） |
| Project 流时长 | `max_stream_duration_seconds`（Admin） | 0 = 跟随实例 | #452 起生效，与 `stream_max_duration` 取较小者 |

**缺的是上游方向的空闲上限。** 上游发出响应头和几个事件之后既不发数据也不断开，Halro 会一直
阻塞在 `sse.Decoder.Next()` 里，直到 `stream_max_duration` 到期。这期间这次尝试一直占着：

- 预算预留（Ledger 里的 reservation）；
- Project 并发与 Provider 并发名额；
- 一个 goroutine 和一条上游连接。

这个缺口是已知的。2026-08-20 的 `21fcd5e7` 删除了 `gateway.stream_idle_timeout`，因为没有任何
代码读它；提交说明写明"An upstream that sends headers and then goes quiet is bounded only by those
totals"。这个键现在列在 `internal/config/retirement.go` 的退役表里。

**文档与代码不一致：** `docs/contracts/gateway-correctness.md` 的"Deadline hierarchy"仍列着
"stream idle deadline"，但它并不存在。本方案落地前，应先把这一行改成如实描述（见 §8 第 1 步）。

## 3. 上游流在哪里被读

每个适配器都把 `response.Body` 交给 `sse.NewDecoder`，然后循环 `decoder.Next()`：

| 文件 | 函数 |
| --- | --- |
| `internal/provider/openai/adapter.go` | `ChatStream` |
| `internal/provider/anthropic/adapter.go` | `MessagesNativeStream` |
| `internal/provider/gemini/adapter.go` | `ChatStream` |
| `internal/provider/bedrockmantle/adapter.go` | `(*ResponsesAdapter).ChatStream` |

`internal/provider/bedrock`（eventstream，不走 SSE）的五个 Profile 目前是 Withheld。实现时仍要
覆盖它，或在它的读取点留一个失败的测试，让它重新开放时不会漏掉。

实现前先用 `grep -rn "response.Body" internal/provider` 重新枚举一遍。上表是 2026-10-23 的
结果，不保证完整，例如 Anthropic 的 Chat 流是否都经过 `MessagesNativeStream` 要再确认。

## 4. 设计

### 4.1 计时规则

- **从收到响应头开始计时**；在那之前由 `attempt_response_header_timeout` 管。
- **每次从上游读到至少 1 个字节就重新计时**。计的是字节，不是解析出的事件：SSE 注释行
  （`: ping`）和 Anthropic 的 `event: ping` 都算活着。这样不需要理解任何一家的心跳格式。
- **只在上游方向计时。** 客户端读得慢由 `downstream_write_timeout` 管；两者不能互相替代。
  下游写阻塞期间上游读也不推进，所以要么把下游写阻塞时间排除在空闲计时之外，要么接受这时
  先触发 `downstream_write_timeout`（15s 远小于空闲上限，后者自然成立）。实现时写一个测试
  把这个顺序钉住。

### 4.2 实现位置（实现时调整）

原计划在四个适配器里各自包装 `response.Body`。实现时改成在 Transport 层接一次：所有适配器都通过
`internal/app/providers.go` 的 `newBindingClientWithEgress` 拿到 HTTP 客户端，在那里给它的
Transport 包一层 `provider.StreamIdleTransport`（`internal/provider/stream_idle.go`），一处就覆盖
全部适配器，包括 Withheld 的 Bedrock Runtime。告警 webhook、模型目录用的是别的客户端，不受影响。

- **按请求的 `Accept` 判断是不是流**（`text/event-stream` 或 `application/vnd.amazon.eventstream`），
  且只包 2xx 响应。`Accept` 是 Halro 自己的适配器设置的，比上游声称的 `Content-Type` 可靠。
- 包装后的 body：`time.AfterFunc` 计时，每次 `Read` 返回 n > 0 时 `Reset`；到期时关闭底层 body，
  让阻塞的 `Read` 返回，之后的 `Read` 一律返回 `provider.ErrStreamIdle`。`Close` 幂等，
  与计时器回调并发安全（`-race` 测过）。
- 包装必须转发 `CloseIdleConnections`：`http.Client.CloseIdleConnections()` 只在 Transport 自己
  有这个方法时才生效，不转发的话适配器 `Close` 时空闲连接不会释放。实现时由测试发现。
- 包装提供 `Unwrap()`，`safetransport.PolicyOf` 改为沿 `Unwrap` 向下找，仍能读到出站策略。
- 各适配器读流出错时先调 `provider.StreamIdleFailure(err)`：是空闲到期就返回
  `ErrorTimeout`、`Ambiguous: true`。这一步是必要的：Anthropic 适配器在第一个事件之前的读错误
  原本标为**不模糊**，不处理的话卡住的尝试会被回退到下一个 Deployment。
- 不放在 `sse.Decoder` 里：它是纯解析器，不该持有时钟。

### 4.3 触发后怎么算

不新增规则，套用 `gateway-correctness.md` 已有的定义：

- 请求已经发到上游、没拿到权威结果，属于**模糊尝试**（ambiguous）。不重试，不回退，
  **即使客户端还没收到任何字节也不回退**，因为上游可能已经在生成和计费。
- 按保守规则结算：现有的 `cappedOutputEstimate` 用已交付字节数给输出估算封顶，这一点不变。
- 计入熔断与 Provider 可用性统计，和其他模糊失败一样。
- 已经开始向客户端写之后，按"流异常终止"处理：发 `error` 事件，不发 `[DONE]`。这已是
  现有路径。
- 错误分类：`provider.Error{Class: provider.ErrorTimeout, Ambiguous: true}`，`Message` 只写
  固定文案，不带任何上游字节。

### 4.4 配置

- 新键 `gateway.upstream_stream_idle_timeout`，必须为正，且不超过 `stream_max_duration`。
  未写这个键时取默认值与 `stream_max_duration` 中较小的那个，否则把 `stream_max_duration` 调到
  2 分钟以下的部署升级后会因为一个自己没写过的键被拒绝启动。
- **不复用退役的 `gateway.stream_idle_timeout`。** 那个名字在退役表里的含义是"写了也没用"，
  复用会让按旧文档写过它的部署悄悄生效。新名字也把"上游方向"写清楚，免得和
  `downstream_write_timeout` 混淆。
- 默认值待实测决定（见 §5），暂定 120s。
- 改了 `internal/config/default.go`、`default.yaml`（配置参考）、`Normalize` 与 `Validate`。
  `doctor` 建议规则和控制台不涉及这个键，没有改。

### 4.5 可观测性

- 新计数器 `halro_upstream_stream_idle_aborts_total`，不带标签。在 `logProviderFailure` 里计数，
  与失败日志出自同一个分类结果。
- 已写进 `docs/contracts/metrics-reference.md`；告警是否需要，评审时决定。
- 失败日志沿用现有的 failure descriptor，`ProviderFailureReason` 不新增枚举值。

## 5. 默认值的风险：推理模型的沉默期

最大的风险是误杀。推理模型在出第一个 token 前可能很长时间不发内容；它们在这段时间里**是否
发心跳**，各家不同，而且没有核实过：

- Anthropic 的流有 `event: ping`，按字节计时可以覆盖，但间隔没实测过。
- OpenAI Chat Completions 与 Responses、Gemini、Bedrock Mantle 在长时间推理时是否发任何字节，
  **未知**。

所以默认值不能拍脑袋定。实施前用 `tests/provider-matrix`（计费、需显式开启）对每个
Profile 做一次测量：取各家能用的最长推理设置，记录"响应头 → 第一个字节"和"字节之间"的最大
间隔，默认值取实测最大值的若干倍并写进文档。owner 决定（2026-10-23）：默认打开，先用 120s，实测后再改这个数。

### 5.1 OpenAI 实测（2026-10-23，直连 api.openai.com，按字节到达时间记录）

| 接口 · 模型 · 推理强度 | 推理 token | 响应头到达 | 响应头之后最长空档 | 心跳 |
| --- | --- | --- | --- | --- |
| Chat · gpt-5.5 · high | 2,560 | 34.5s | 0.0s | — |
| Chat · o3 · high | 7,424 | 47.5s | 0.1s | — |
| Chat · gpt-6-sol · high | 1,455 | 18.2s | 0.0s | — |
| Responses · gpt-5.5 · high | 2,588 | 2.1s | 26.0s | 无（未到 30s） |
| Responses · gpt-6-sol · high | 1,247 | 1.8s | 6.2s | 无 |
| Responses · gpt-5.5 · xhigh（难题） | 57,496 | 1.6s | 30.1s（共 921s） | `event: keepalive`，每 30.0s |

结论：

- **Chat Completions 把响应头压到推理结束才发**，之后内容瞬间到齐。推理期间的沉默在响应头之前，
  由 `gateway.attempt_response_header_timeout` 管，看门狗看不到，也就不会误杀。
- **Responses 流**响应头 2 秒内就到，推理期间不发内容，但沉默超过 30 秒就每 30 秒发一次
  `event: keepalive`。按字节计时，120s 对 OpenAI 有 4 倍余量。
- Halro 的 OpenAI Responses Profile 目前向上游发的是**非流式**请求（`GenerateSemantic`
  设 `stream: false`），同样要等推理结束才有响应头，也由响应头超时管。
- 由此发现的已有问题：响应头超时默认 60 秒，o3 在一道中等难度题上已用 47.5 秒，生产上 Bedrock
  Mantle 也已 3 次在 60.02 秒被切断（#343）。另在分支 `fix/reasoning-response-header-timeout`
  处理，不在本方案范围内。

### 5.2 Anthropic 实测（2026-10-23，直连 api.anthropic.com Messages 流式，按字节到达时间记录）

同一道难题，`thinking: {type: "adaptive", display: …}`、`output_config.effort`、`max_tokens` 64000。
五组并行跑；除冒烟那组外都在 64000 处被 `max_tokens` 截断，所以总时长是下限，不是该模型的最长推理。

| 模型 · effort · display | 输出 token | 响应头到达 | 总时长 | 字节间最长空档 | 只算非 ping 字节的最长空档 | ping |
| --- | --- | --- | --- | --- | --- | --- |
| claude-haiku-5-5 · low · omitted | 7,208 | 1.8s | 35.9s | 30.0s | 32.7s | 2 |
| claude-haiku-5-5 · max · omitted | 64,000 | 1.3s | 275.3s | 30.1s | 273.4s | 10 |
| claude-sonnet-5-5 · max · omitted | 64,000 | 3.1s | 466.0s | 30.0s | 462.0s | 16 |
| claude-opus-5-5 · max · omitted | 64,000 | 2.1s | 580.2s | 30.1s | 577.3s | 20 |
| claude-fable-5-1 · max · omitted | 64,000 | 1.5s | 756.4s | 30.1s | 754.1s | 26 |
| claude-opus-5-5 · max · summarized | 64,000 | 1.9s | 550.4s | 4.6s | 4.6s | 1 |

结论：

- 响应头 1–3 秒就到，推理在响应头**之后**，所以这条路径归看门狗管，不归响应头超时。
- `display: omitted`（这些模型的默认值）时，推理期间除了 `event: ping` 什么都不发：Fable 5.1 有
  754 秒没有一个内容字节。记录到的长空档全部是 29.9–30.1s，每个都以 ping 结束，与 OpenAI Responses 的
  `event: keepalive` 相同。
- 看门狗按字节计时，ping 会重置计时，所以 120s 对 Anthropic 也有 4 倍余量。反过来说，**如果改成按
  事件或按内容计时，Anthropic 的默认设置会在 120s 被误杀**——§4.1 按字节计时的规则不能改。
- `display: summarized` 时推理摘要持续流出，最长空档 4.6s。

**仍未实测：** Gemini、Bedrock Mantle 的流式接口在长推理期间的心跳；Kimi、MiniMax 的 Anthropic 兼容
接口是否也发 ping。120s 对它们是否足够，要等有 key 时再测。

## 6. 不做的事

- 不做对冲请求（Respan 的 hedge）：输家在 Halro 里是模糊尝试，要保守计费，等于有意花双份钱，
  需要单独设计。
- 不按模型或 Deployment 分别配置空闲上限；先用一个全局值，有实测需要再加。
- 不改非流式请求：它们由 `route_total_timeout` 管。

## 7. 测试

1. `IdleBoundedBody` 单元测试：持续有数据时不触发；停顿超过上限时触发并返回 `ErrStreamIdle`；
   上游正常结束时计时器被停掉、没有泄漏的 goroutine；`Close` 与到期并发时无竞争（`-race`）。
2. 每个适配器一个测试：`httptest` 上游先发响应头和一个事件，然后挂起。断言在上限内返回
   `ErrorTimeout`、`Ambiguous=true`，错误里不含上游字节。
3. Gateway 层：客户端未收到字节时触发，断言**不回退**到第二个 Deployment，且尝试按保守规则
   结算；已收到字节时触发，断言流以 `error` 事件结束、没有 `[DONE]`；结算后 reservation 被
   释放、并发名额回到原值。
4. 反向验证：把包装从某个适配器移除，用 `go test -count=1` 确认第 2 条失败，并确认移除确实生效。
5. 真实二进制：本地起一个"发完响应头就挂起"的假上游（私有非回环地址，开
   `allow_private_provider_endpoints`），把上限设成几秒，确认请求按时结束、`halro_active_requests`
   回落、`ledger verify` 通过。

## 8. 实施步骤

1. 契约：`gateway-correctness.md` 的截止时间层级改为新键的描述。**已完成**
2. 实测各家推理沉默期（§5），定默认值。**待做，需要真实平台账号**
3. `StreamIdleTransport` 与单元测试。**已完成**
4. 适配器接入（Transport 层一处，加各适配器的 `StreamIdleFailure` 判断），加测试。**已完成**
5. 配置键、默认值、配置参考。**已完成**
6. 指标与文档（含 CHANGELOG）。**已完成**
7. Gateway 层测试、反向验证、真实二进制检查，然后 `make full-check`。**已完成**，见 §9

不涉及数据目录格式，**不需要重新初始化**。

## 9. 实现与验证记录（2026-10-23）

测试：

| 测试 | 内容 |
| --- | --- |
| `internal/provider/stream_idle_test.go` | 停顿即断、持续发送可超过上限、提前关闭只关一次、只包 2xx 流请求、转发 `CloseIdleConnections`、错误分类；`-race` |
| `TestEveryStreamingProfileEndsAStalledStreamAsAnAmbiguousTimeout` | 遍历 Profile 表里全部非 Withheld 的流式 Profile（24 个），每个对着"只发响应头就沉默"的上游，断言在上限内返回模糊超时。以后新加的 Profile 自动覆盖 |
| `TestConverseStreamEndsAStalledStreamAsAnAmbiguousTimeout` | Withheld 的 Bedrock Runtime 在自己包里测，帧中途卡住 |
| `TestBindingClientBoundsStreamsByTheConfiguredIdleLimit` | Provider 客户端真的套上了包装、读的是配置值、出站策略仍可读 |
| `TestStreamEndedByTheIdleLimitStopsAndSettlesConservatively` 等 | 不重试、保守结算含固定费；计数只算空闲到期 |
| `TestUpstreamStreamIdleTimeoutIsDefaultedWithinTheStreamAndBounded` | 缺省取值与上限校验 |

反向验证：去掉 Anthropic 适配器里的判断，6 个走 Anthropic 协议的 Profile 失败；去掉 Bedrock 的判断，
Bedrock 测试失败（报 `malformed_response`）。两次都先确认改动确实生效，用 `-count=1`。

真实二进制（linux/arm64，OrbStack 容器，假上游用测试 CA 签的 HTTPS 证书，
`upstream_stream_idle_timeout: 5s`、`stream_max_duration: 30s`）：

| 情况 | main 011bc5cd | 本分支 |
| --- | --- | --- |
| 发响应头和一个短事件后沉默 | 等满 30.0s，502 `provider_error` | 5.2s，504 `provider_timeout`，无 `[DONE]` |
| 只发响应头 | — | 5.1s，504 `provider_timeout` |
| 先交付约 20 KB 再沉默 | — | 200，内容送达后收到 `event: error`（`stream_error`），无 `[DONE]`，5.5s |

每次结束后 `halro_active_requests` 与 `halro_accounting_pending_leases` 都是 0，
`halro_upstream_stream_idle_aborts_total` 为 1，`ledger verify` 与 `doctor` 通过。

顺带确认的现有行为：流式脱敏按规则的最大匹配宽度留住一段尾部，内容短于这个宽度时，客户端在流
结束前一个字节都收不到。所以前两种情况里卡住时都还在交付边界之前，客户端拿到的是 504 而不是流内
错误事件。

**未完成：** §5 中 Anthropic、Gemini、Bedrock Mantle 的实测，需要对应平台账号。OpenAI 已测，见 §5.1。

