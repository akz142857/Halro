# 火山方舟真实账号验收 — 2026-11-02

用户明确授权使用 `.env` 中的 `VOLCENGINE_API_KEY` 测试按量付费 API。
密钥仅由父进程读取并通过请求头/子进程环境传递，没有写入日志、命令行参数或提交文件。

## 实测结果

端点：`https://ark.cn-beijing.volces.com/api/v3`。测试模型：`doubao-seed-2-0-mini-260428`，先确认它在真实上游目录中出现，再发送限长请求。

| 检查 | 结果 | 输入 tokens | 输出 tokens | 总 tokens |
| --- | --- | ---: | ---: | ---: |
| 普通对话 | 精确返回 OK | 37 | 1 | 38 |
| SSE 流式 | 内容、结束标记及 usage 正确映射 | 37 | 1 | 38 |
| 工具调用 | 指定 echo，参数正确 | 488 | 26 | 514 |
| 工具结果回传 | tool_call_id 关联成功，返回 pong | 93 | 2 | 95 |
| JSON Object | 合法 JSON，内容符合预期 | 176 | 5 | 181 |
| 严格 JSON Schema | 符合声明结构，strict=true 被接受 | 188 | 5 | 193 |
| 图片输入 | 32×32 合成红色 PNG，被识别为 red | 1,339 | 1 | 1,340 |
| 合计 | 7 个收费短请求 | 2,358 | 41 | 2,399 |

上述基础批次由实际 Volcengine adapter 构造并发送 `thinking.type=disabled`；响应中的 reasoning tokens 全部为 0。
所有请求均未命中缓存，因此只验收基本用量拆分，没有取得缓存命中的正向证据。本文不估算金额。

## 深度思考与原生运行时验收

使用已开通的 `doubao-seed-2-0-lite-260428` 先捕获真实响应，再实现并验证解码。最初选取的 `doubao-seed-2-1-lite-260915` 返回 `ModelNotOpen`，没有将目录中出现当作账号开通证据，也没有依赖它完成验收。

较早一次用户回复已开通后的复测：使用同一 `.env` Key 和 `doubao-seed-2-1-lite-260915`，原生运行时成功刷新 138 条目录并解析目标能力，但首次推理被拒绝。另以相同模型、关闭思考及 1,024 token 总输出上限直接核对，当时官方端点仍返回 HTTP 404、`ModelNotOpen`。这次失败没有获得成功推理或 usage，不能把 Seed 2.0 Lite 的通过结果视为 Seed 2.1 Lite 通过；其后实际开通成功的结果另列如下。

此次也修复了验收代理的 DNS 轮转问题：初次解析与 SafeTransport 解析可能返回不同 A 记录，测试代理只会从同一官方主机的新 DNS 答案补充允许地址，且保留正式 SafeTransport 与 TLS 校验；没有放宽正式运行时策略。

随后直接核对已登录控制台，选择已开通的 **Doubao-Seed-2.1-turbo**，与真实目录对照得到精确 ID `doubao-seed-2-1-turbo-260628`。同一 Key 的普通推理及自动工具选择请求均返回 HTTP 200。真实捕获也确认一个模式差异：强制命名工具调用时，Turbo 返回工具调用但没有思考摘要、加密块，reasoning tokens 为 0；改为 `tool_choice=auto` 后，返回摘要、加密块、工具调用和正数 reasoning tokens。因此原生加密思考验收使用自动工具选择，强制工具选择继续由基础 smoke 单独覆盖。

**Turbo 追加原生验收通过**：`TestRealVolcengineRuntime` 使用上述精确 ID，以 `-race -count=1` 完成全部 5 次真实推理。覆盖关闭思考、自动工具调用中的加密思考、加密状态原样回传、实时 SSE 和严格 JSON Schema；账本 5 次成功 settlement 的输入 881、输出 174、总计 **1,055 tokens** 与客户端 usage 完全一致。工具往返保留 280 字节加密块，reasoning tokens 合计 63；SSE 包含独立加密事件和摘要事件，首个事件约 3,940 ms，结束约 3,957 ms。1,055 tokens 只统计该成功批次，另有前述排障调用；没有据此估算费用。

**Seed 2.1 Lite 开通后追加原生验收通过**：用户再次确认开通后，同一 Key 直接调用 `doubao-seed-2-1-lite-260915` 已返回 HTTP 200。随后 `TestRealVolcengineRuntime` 以 `-race -count=1` 完成全部 5 次推理：关闭思考、自动工具调用、加密状态回传、实时 SSE 和严格 JSON Schema 全部通过。账本输入 **964**、输出 **304**、总计 **1,268 tokens** 与客户端 usage 完全一致；加密工具往返保留 248 字节状态，reasoning tokens 合计 71；SSE 摘要及独立加密事件完整，首事件约 4,556 ms、结束约 4,576 ms。

Lite 复测过程中曾出现原生 400，以及一个与适配无关的自由文本格式断言失败：模型在正确的最终答案 391 之前增加了解释。协议验收改为检查最后一个独立词为 391，仍要求摘要、加密事件、正数 reasoning usage、完整终止和账本一致；严格 JSON Schema 验证保持不变。1,268 tokens 只统计最后成功批次，前述失败排障和直接调用另计。测试失败时现在只输出账本中已清洗的上游错误码，不输出错误原文、账号、密钥或请求 ID。

原始响应确认：工具调用返回明文思考摘要及独立加密块；原样回传加密块后完成工具结果回复；SSE 在摘要之后返回独立 `encrypted_content` 事件，随后才是答案。使用占位字符串测试字节保真，不把真实密文写入仓库。

`TestRealVolcengineRuntime` 在临时加密存储中 bootstrap 完整运行时，通过 Admin API 创建托管 CONNECT 出口，持久化初始出口关联后重新打开运行时。启用部署禁止热改出口的生产保护保持生效。随后经过正式 provider registry、SafeTransport、Go net/http/TLS、canonical mapping、脱敏、北向 HTTP/SSE 和 ledger；没有 curl transport 或模拟上游响应。

下表保留 `doubao-seed-2-0-lite-260428` 的首个成功批次；Turbo 和 Seed 2.1 Lite 的追加结果见上文。

| 原生验收项 | 结果 |
| --- | --- |
| Admin API 刷新模型目录 | 138 条，所选模型的 reasoning/tools/structured outputs 元数据成功解析 |
| 普通对话 / `none` | 成功，reasoning tokens 为 0 |
| `low` 思考 + 指定 echo 工具 | 返回工具调用、思考摘要、276 字节加密状态和 reasoning usage |
| 加密状态原样回传 + tool 结果 | 成功返回 pong；工具往返的 reasoning tokens 合计 71 |
| 原生 SSE | 答案 391；1 个加密事件、1 个思考摘要事件、usage 和 `[DONE]` 均到达客户端 |
| 严格 JSON Schema | 成功返回 `{"ok":true}` |
| 用量账本 | 5 次成功 settlement；输入 882、输出 180、总计 1,062 tokens，与客户端 usage 完全一致 |
| 并发检查 | 同一原生验收以 `-race -count=1` 通过 |

该成功批次 SSE 首个可交付事件约 3,089 ms、结束约 3,107 ms，共 4 个数据事件；这是本次短请求的观测值，不是性能承诺。文本脱敏仍会保留必要的尾部缓冲。验收发现独立加密事件被脱敏层当作空事件丢弃，已修复并补充不依赖真实服务的回归测试。

思考开关与精确模型档位依据[官方深度思考文档](https://docs.volcengine.com/docs/ark/deep-thinking?lang=zh)。档位被上游折算的请求提前拒绝；强制思考模型不接受关闭，Kimi-K3.1-Pro 不发送不适用的 `thinking` 字段。Endpoint ID 的 canonical model 语义映射另以模拟传输完成集成回归，保留原始 Endpoint ID；没有用户接入点，未声称真实 Endpoint ID 调用已验收。

加密状态只允许 assistant 的思考内容，限制大小，并禁止跨非 Ark profile 回传或由非 Ark primitive 返回；文本摘要继续脱敏。请求/普通响应/独立 SSE 事件、失效角色和大小限制均有回归。修改通用消息模型后，其他 provider 的边界也纳入测试。

最终回归还补正了便携 Messages 投影中的端点身份保留：普通与流式请求在投影后仍携带 Messages source，Ark 的显式思考和强制思考目标会在上游调用前被拒绝，避免付费后才发现无法表达加密思考结果。DeepSeek V4.1 的 `max_tokens` 实为思考加回答总量，开启思考时不会把它当作便携回答上限使用；对应路由、渲染和兼容性清单均有回归。

1,062 tokens 仅是上述最终成功批次；原始响应捕获和失败排障中另有真实调用，不能将它视为本次全部消耗。

## 模型列表

携带同一标准 API Key 的 `GET /api/v3/models` 返回 HTTP 200：

- 外层字段为 `object`、`data`，`object=list`，共 138 条。
- 83 条 `status=Shutdown`，17 条 `status=Retiring`，38 条没有 status。
- 条目还包含 `domain`、`features`、`modalities`、`task_type`、`token_limits` 等可选字段。
- 包含 Embedding、图片、视频及 3D 模型；**出现于列表不证明支持 Chat，也不证明账号已开通调用**。

据此补正列表生命周期：Shutdown 为不可用/退役，Retiring 为退役状态，没有 status 的保持未知。连接测试不能让 Shutdown 目标通过。
本次只从上游实际字段建立 `provider_metadata` 证据：TextGeneration 加文本输出建立 Chat；图片输入模态、正数 reasoning 上限及显式 function calling / JSON flags 分别映射能力。保留 context/output 上限和模态。false 为不支持，缺失为 unknown，不凭名称或条目出现建立能力。没有 streaming/远程图片抓取字段，不生成相应元数据声明；Shutdown 不产生可用能力声明，视频模型也不会被标为 Chat。

回归样本：[四个公开模型的实际响应子集](../../internal/provider/openai/testdata/volcengine-models.json)。仅从实际捕获中选取公开模型，不含用户自建接入点、账号信息或凭据。

## 网络条件与验收边界

默认到方舟真实地址的路由经过 `utun1500`。直连、系统代理、TLS 1.2 以及使用公共 DNS 真实地址的普通路由都在 TLS 握手阶段中断，尚未进入 HTTP 鉴权。
对照请求单独绑定 `en0` 后，官方端点先返回无密钥 401，随后带密钥返回 200，推理 smoke 成功。

成功测试保留官方主机名/SNI 和正常证书校验，没有关闭证书校验，没有修改 DNS、系统路由或 VPN。
基础批次的 adapter 经测试专用 curl transport 发送真实请求并解析真实响应，会缓冲响应；它只证明协议映射。后续原生验收通过进程内公共 DNS 解析、绑定 en0 的透传 TCP relay 和正式托管出口，完成 Go/TLS/SSE 实时交付。只调整测试进程，不变更系统设置。默认 TUN 出口的问题仍需在正式使用环境解决。

尚未验收：其他模型的真实推理、自定义 Endpoint ID 的真实调用、缓存命中、默认 TUN 出口及生产部署。模型目录不是已开通服务的清单。兼容性保持 experimental。

## 复现

`TestRealVolcengineSmoke` 需 `HALRO_REAL_PROVIDER_SMOKE=1` 与 `HALRO_SMOKE_PROFILE=volcengine` 双重启用，并显式指定模型和密钥。普通测试自动跳过，不产生费用。
完整运行包含上述 7 个短推理请求；以 `-run '^TestRealVolcengineSmoke$/^catalog$'` 选择 catalog 子测试时只读取目录，不产生推理请求。

可选的 `HALRO_SMOKE_NETWORK_INTERFACE` 和 `HALRO_SMOKE_RESOLVE_IP` 只作用于 smoke transport。IP 是诊断时从公共 DNS 获取的临时值，不作为生产配置或固定地址保存。

原生验收见[接入手册](../runbooks/volcengine-ark.md)的 `volcengine_runtime` 说明；该 Darwin 测试只在双重显式开关、模型、密钥和接口均配置时执行。普通本地及 CI 测试不调用真实服务。

## 最终本地回归

- 执行一次 `go test -count=1 ./...`：66 个包通过（含 app、gateway）；另有兼容性清单断言和 deadman 计时测试失败。
- 兼容性问题修正后，`./internal/compatibility/...`、`./internal/provider/...`、`./internal/gateway` 全包复测通过；应用层的 Messages、思考路由、火山接入和 profile 快照集成复测通过。
- deadman 当时测得 253.7 ms，阈值为 250 ms；整个 `./internal/deadman` 独立以 `-count=1` 通过，没有修改该包或放宽阈值。未为此重跑无关的完整 Go 套件。
- 前端类型检查、713 个测试和生产构建通过，内嵌 bundle 已重新生成；更新 profile 测试快照后再次执行前端测试。
- `git diff --check` 通过；改动文件未发现实际 API Key，生成资源的密钥扫描通过。以上均为本地验收，不代表生产部署。
- 提交前最终门禁：再次执行修复后完整的 `go test -count=1 ./...`，68 个测试包全部通过；`go vet ./...`、`make fmt-check` 通过。使用 Node 22.18.0（当时仅对齐 CI 主版本），类型检查、713 个前端测试、`scripts/check-web-bundle.sh` 和 `git diff --exit-code -- internal/webui/dist` 全部通过；暂存内容的实际密钥扫描通过。本轮未重跑收费测试。

## PR #480 CI 复核更正

PR #480 已合并为 `83e6a6e0`，但 PR CI 和合并后主干 CI 的 web 任务均在资源漂移检查失败；PR CI 的 Go 全量测试、全量 race、静态检查、漏洞扫描、SDK、fuzz 和其他任务通过。

此前本地前端门禁仅证明旧 `node_modules` 下可以重复构建，没有验证依赖目录与提交声明的一致性：本机 Vite 为 8.3.4、Rolldown 为 1.2.13，而锁文件要求 8.3.5 / 1.2.15，另有数个直接依赖也已过期。Node 主版本相同不能弥补这个差异。CI 实际使用 Node 22.23.4，并执行干净的 `npm ci --ignore-scripts`。

后续修复按锁文件干净安装并重新生成内嵌资源，生成哈希与失败日志中 CI 的输出一致；使用 Node 22.23.4 完成类型检查、713 个测试和生产构建。资源门禁增加 `npm ls --depth=0` 检查，在旧的直接依赖到达构建前拒绝，并补充“过期依赖不能启动构建 / 正确依赖继续校验资源”的脚本回归。本次修复不改变火山适配的 Go 业务逻辑，也不重新调用收费模型。

修复分支的推送前检查：68 个 Go 测试包、713 个前端测试、7 个门禁契约测试、shell 语法、资源漂移及暂存内容检查全部通过；依赖声明和锁文件保持原样。
