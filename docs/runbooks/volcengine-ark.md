# 火山方舟按量付费 API

本接入使用火山引擎 **火山方舟标准 API Key**，不是 Agent Plan / Coding Plan 订阅，也不是火山引擎 AK/SK。
当前兼容性状态仍为 experimental。2026-11-02 已完成下文列出的真实账号与原生运行时验收；其他模型和生产部署仍须按实际环境验证。

## 配置

1. 在方舟控制台开通需要使用的模型，并创建有权调用它的 API Key。
2. 在 Halro「服务商」创建凭据，选择「火山引擎 / 火山方舟」。产品为「火山方舟按量付费 API」。
3. Base URL 默认 `https://ark.cn-beijing.volces.com/api/v3`，保存标准 API Key，再创建使用此凭据的连接。
4. 添加部署。上游模型标识填写控制台的 **Model ID**，或者自己的 **Endpoint ID**（`ep-...`）；不要填写模型展示名称。
5. 刷新模型列表，读取上游能力元数据；缺少证据的能力可另行声明或检测。检测会发送真实推理请求并按上游规则计费。

2026-11-02 追加实测可用的固定版本 ID（须分别开通）：

- `doubao-seed-2-1-lite-260915`（Doubao-Seed-2.1-lite）：原生五次验收与 `-race` 通过，成功批次 1,268 tokens。
- `doubao-seed-2-1-turbo-260628`（Doubao-Seed-2.1-turbo）：原生五次验收与 `-race` 通过，成功批次 1,055 tokens。

在已开通对应模型的账号中，可用该 ID 创建部署。这两次加密思考工具往返均使用 `tool_choice=auto`；Turbo 强制命名工具的真实响应没有思考状态，应将工具选择与思考场景分别验证。

Halro 对上游发送 `Authorization: Bearer <API Key>`，调用
`POST /api/v3/chat/completions`。下游继续使用 Halro 的公开模型名称和项目 Key。
Base URL 可以配置自己的代理地址，已有路径保持原样，不会再插入 `/v1`。

## 能力边界

- 支持普通对话、SSE 流式、function 工具调用、图片输入、JSON Object / JSON Schema 输出，以及 token 和缓存命中用量。具体模型是否支持，仍由该部署的能力证据决定；连接可承载某项能力，不会自动把未知模型标为支持。
- 深度思考通过 Chat 请求的 `reasoning_effort` 控制：Seed 2.x 的 `low` / `medium` / `high` 启用思考，`none` 或未指定时关闭。按官方精确 Model ID 核对档位；上游会降档或改变开关含义的值会在调用前拒绝。GLM / DeepSeek 等使用各自的已核对档位，未知模型不会凭名称继承参数契约。
- GLM-5.3-Flash 和 Kimi-K3.1-Pro 的已核对版本始终思考，拒绝 `none`；Kimi 不发送不适用的 `thinking` 开关。无法承载思考输出的便携 Responses / Messages 接口会提前排除这些目标。
- `reasoning_content`、`encrypted_content` 和 reasoning tokens 在普通响应与 SSE 中保留。工具结果回传时，须原样附回整个 assistant 消息，包括加密块；加密块不参与文本脱敏改写，独立 SSE 加密事件也不会丢失。明文思考仍执行脱敏。加密状态限于 assistant 思考内容（单字段不超过 64 KiB，流式事件另受 64 KiB 总上限约束），其他上游 profile 拒绝接收或返回该状态。
- Endpoint ID 原样发往上游；启用显式思考档位需要部署快照中有已审核的 canonical model 映射，单独的 `ep-...` 不足以证明档位语义。
- DeepSeek V4.1 的 `max_tokens` 包含思考，与便携协议的回答上限不同；开启思考时该组合会提前拒绝。需要限定总输出时使用 `max_completion_tokens`，不会自动改写调用者的预算含义。
- 不提供 embeddings、原生 Responses 或 Anthropic Messages 上游 profile。Halro 的便携 Responses / Messages 请求可转成非思考 Chat 请求；其字段限制发布在兼容性清单中。
- `n=1` 使用上游默认单候选；多候选、`user`、developer 消息以及同时设置两种输出上限被拒绝。
- 图片、工具、JSON Schema 和采样参数的支持范围依赖实际模型；本地模拟测试不代替真实账号验收。

## 模型列表与连接测试

模型刷新尝试同一 Base URL 下的 `GET /models`，复用现有 OpenAI 列表读取器。
已用标准 API Key 验证该路径返回 HTTP 200。列表包含多种模态的模型，以及已下线和即将下线的条目，是平台目录，不是账号已开通服务的清单。

保留上游标识，不在失败时退回编译进二进制的模型清单。`Shutdown` 显示为不可用且已退役；`Retiring` 显示为退役状态。没有 status 的条目保持 unknown 生命周期和 unverified 可用性，不推断为 Active 或已开通。
只有上游实际描述的条目才能形成 `provider_metadata` 能力证据：`TextGeneration` 加文本输出建立 Chat；图片输入模态、正数 reasoning 上限以及显式 function calling / JSON flags 分别建立对应能力。保留 context/output 上限和输入输出模态；明确 false 是不支持，缺失字段是 unknown。没有字段证明的 streaming 和远程图片抓取不会由列表推断，视频/图片/embedding 模型也不会被标为 Chat。
未知模型和自定义接入点可以手动添加，能力保持 unknown，直到声明或检测。

本机默认 TUN 出口的 TLS 握手仍会中断。原生验收使用临时库中持久化的托管 CONNECT 出口，经测试进程绑定已有 `en0`，使用进程内公共 DNS 解析后成功；请求经过正式 SafeTransport、Go net/http 和正常 TLS 证书/SNI 校验。没有修改系统 DNS、路由、VPN 或正式配置。正式使用仍需配置可用出口。

官方另有 `ListFoundationModels` / `ListEndpoints` 管控 API，使用
`ark.cn-beijing.volcengineapi.com`，要求 AK/SK 签名。此版本没有接入这套管理身份；标准推理 API Key 不能冒充 AK/SK。
连接测试读取兼容列表并检查目标是否在其中，同时拒绝上游已标记 Shutdown 的目标。它不调用模型，也不证明账号已开通该模型或自定义接入点可推理。

## 依据与验证（2026-11-02）

- [兼容 OpenAI SDK](https://docs.volcengine.com/docs/ark/compatible-with-openai-sdk?lang=zh)：Base URL、API Key、Model ID。
- [Chat API](https://docs.volcengine.com/docs/ark/chat-api?lang=zh)：请求字段、流式、工具和用量。
- [深度思考](https://docs.volcengine.com/docs/ark/deep-thinking?lang=zh)：默认思考行为和加密上下文回传。
- [结构化输出](https://docs.volcengine.com/docs/ark/structured-output-beta?lang=zh)：JSON Object / JSON Schema 能力。
- [基础模型列表](https://docs.volcengine.com/docs/ark/list-foundation-models-api?lang=zh)、[接入点列表](https://docs.volcengine.com/docs/ark/list-endpoints-api?lang=zh)：仅支持 Access Key 鉴权。

本地验证覆盖凭据/连接保存、供应商注册、请求路径与鉴权、Endpoint ID 转发、流式工具与用量映射、字段拒绝及列表失败。
初始映射测试使用合成协议样例，后续补入真实列表中的四个公开模型作为生命周期及能力元数据回归样本。

[真实账号验收记录](../verification/volcengine-ark-2026-11-02.md)覆盖 Mini 的基础功能，以及 `doubao-seed-2-0-lite-260428` 的原生网关思考开关、加密工具往返、实时 SSE、JSON Schema 和账本核对。原生成功批次 5 次调用共 1,062 tokens，并通过 `-race`。未观察到缓存命中。

复用 smoke：设置 `HALRO_REAL_PROVIDER_SMOKE=1`、`HALRO_SMOKE_PROFILE=volcengine`、`HALRO_SMOKE_MODEL` 和 `HALRO_SMOKE_API_KEY` 后运行 `go test -count=1 -v ./internal/provider/openai -run '^TestRealVolcengineSmoke$'`。测试不会自行读取 `.env`，由父进程只解析所需密钥并放入子进程环境；不要把密钥写进命令参数。

仅在本机网络诊断时，可设置 `HALRO_SMOKE_NETWORK_INTERFACE=en0` 和 `HALRO_SMOKE_RESOLVE_IP=<实时公共 DNS 地址>`。这启用测试专用 curl transport，凭据通过 stdin 传递；它会缓冲响应，因此验证的是 SSE 内容、结束与用量映射，不是首包时延或持续交付时序。生产适配器没有这些出口覆写。

Darwin 上的完整原生验收：设置 `HALRO_REAL_PROVIDER_SMOKE=1`、`HALRO_SMOKE_PROFILE=volcengine_runtime`、`HALRO_SMOKE_API_KEY`、支持加密思考的 `HALRO_SMOKE_MODEL`、`HALRO_SMOKE_NETWORK_INTERFACE`，可选 `HALRO_SMOKE_DNS_SERVER`，运行 `go test -race -count=1 -v ./internal/app -run '^TestRealVolcengineRuntime$' -timeout 5m`。它创建临时运行时与透传 TCP 代理，发送 5 次收费推理请求，不使用 curl 或模拟 HTTP 响应。普通测试跳过所有真实请求。
