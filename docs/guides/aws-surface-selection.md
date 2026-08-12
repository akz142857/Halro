# 选择 AWS 接入面：Bedrock Runtime 还是 Bedrock Mantle

Halro 对 AWS 有**两个并存的接入面**，不是一个在替代另一个。它们是同一个 Provider
类型（`bedrock`）下的两组 Profile，凭据方案、Endpoint 和能力上限都不同，一个 Provider
实例只能选其中一个。

本文回答「我该用哪个」。每个 Profile 逐项的能力、鉴权与翻译行为在
[Provider capability contract](../contracts/provider-capabilities.md) 的 Shipped
profiles 表，本文不重复。

## 先说结论

| 你要做的事 | 用哪个 |
| --- | --- |
| 对话、流式、工具调用、视觉输入 | **两个都行**，Mantle 的能力面更宽（见下） |
| Embedding、图像生成、Rerank、异步视频 | **只有 Runtime** |
| 跨区域推理（Inference Profile）、预置吞吐 | **只有 Runtime，且只在 Converse 文本 Profile 上** |
| 护栏（Guardrails）、批量推理、服务层 | **两个都还不支持**，见「都做不到的事」 |

## 两个接入面的硬边界

| | Bedrock Runtime | Bedrock Mantle |
| --- | --- | --- |
| Access Surface | `SurfaceBedrockRuntime` | `SurfaceBedrockMantle` |
| Endpoint | `bedrock-runtime.<region>.amazonaws.com`（含 FIPS、`.com.cn`、PrivateLink `.vpce.`）与 `bedrock-agent-runtime.<region>.*` | 仅 `bedrock-mantle.<region>.api.aws` |
| 凭据 | AWS SigV4（Access Key + Secret + 可选 Session Token + Region，加密为一条 audience-bound 凭据） | Bedrock API Key（静态 Header） |
| 协议 | AWS 原生（Converse / InvokeModel / Agent Runtime） | OpenAI 与 Anthropic 兼容线 |
| Profile 数 | 5 | 3 |

**Runtime 凭据不能挂到 Mantle 上，反之亦然。** 这由 Profile 的
`CredentialScheme` 强制（`AWSSigV4Explicit` vs `BedrockAPIKey`），装配期就会拒绝。
Runtime 适配器也**不读环境变量凭据、不访问 IMDS**，Region 必须与 Endpoint 主机名一致。

## 能力面的差别

Runtime 的 Converse Profile 被**刻意钉死在纯文本对话**：只声明 Chat、Streaming、
StreamUsage，工具、视觉、JSON 模式一律在 Provider I/O 之前拒绝，而不是静默降级。
Mantle 的三个 Profile 反而更宽：

| Profile | 能力 |
| --- | --- |
| `bedrock.runtime.converse.text.v1` | Chat、Streaming、StreamUsage |
| `bedrock.runtime.invoke.titan-embed-text-v2.v1` | Embeddings，上下文 8192 |
| `bedrock.runtime.invoke.titan-image-v2.v1` | Images |
| `bedrock.agent-runtime.rerank.cohere-v3-5.v1` | Rerank |
| `bedrock.runtime.async.nova-reel-v1.v1` | AsyncGenerate（需显式 S3 输出） |
| `bedrock.mantle.openai.chat.v1` | Chat、Streaming、Tools、Vision、JSON、DeveloperRole、**Reasoning**、StreamUsage |
| `bedrock.mantle.openai.responses.v1` | 同上，但**没有 Reasoning** |
| `bedrock.mantle.anthropic.messages.v1` | Chat、Streaming、Tools、Vision、Reasoning、StreamUsage |

Mantle Responses 少一项 Reasoning 是有原因的，不是遗漏：它只参与 Halro 的无状态层、
恒发 `store:false`，而当前的 canonical response 映射保不住 reasoning item，所以那项能力
不声明，而不是声明了再在运行时丢掉。

**这些上限是刻意钉死的。** 目录、上游元数据和管理员声明都不得放宽 Beta Profile 的上限；
放宽属于需要独立契约评审的决定，不能作为别的改动的副作用发生。

## 部署目标类型：跨区域推理与预置吞吐的真实边界

这一条容易想当然，实现比「Bedrock 支持 Inference Profile」要窄：

| 接入面 / Profile | 可用的部署目标类型 |
| --- | --- |
| Mantle（全部三个 Profile） | 仅 `model_id` |
| Runtime · Converse 文本 | `bedrock_foundation_model`、`bedrock_inference_profile`、`bedrock_provisioned_throughput` |
| Runtime · 其余四个 Profile | 仅 `bedrock_foundation_model` |

也就是说**跨区域推理和预置吞吐只对 Converse 文本对话可用**。Titan Embedding、
Titan Image、Cohere Rerank、Nova Reel 都只能指向基础模型，Mantle 则完全没有这两种目标。

## 都做不到的事

以下能力在**两个接入面上都未对接**，需要新开发，且涉及 Beta Profile 上限的部分要走契约评审：

- **护栏（Guardrails）** —— 不能配置或调用。唯一相关的实现是把上游返回的
  `guardrail_intervened` / `content_filtered` 停止原因映射为 `content_filter`，也就是
  Halro 能**识别护栏拦截发生了**，但不能主动使用护栏。
- **批量推理** —— Bedrock 侧没有 `batches` 能力。该能力目前只存在于
  `openai.media-resources.v1`。
- **服务层（Service tiers）** —— 没有对接。北向 Anthropic Messages 门面接受
  `service_tier` 请求字段，但那是协议兼容面的事，与 Bedrock 的服务层无关。

## 一条常见的选型误解

AWS 给 Mantle 的推荐理由里有「按项目隔离工作负载、跟踪应用级成本与用量」。
**在 Halro 场景下这不构成选型依据 —— 这件事网关自己就在做，且对两个接入面一视同仁。**
Project 隔离、RPM/TPM/并发上限、预算预留与结算、按 Project 与 Deployment 的成本和
Token 记账，都由 Halro 的 `budget`、`ledger`、`usage` 负责，与你选 Runtime 还是 Mantle
无关。选型应当只看上面几节的能力与目标类型差异。

## 同时需要两边的能力时

一个 Deployment 只承载**一个模型自己的能力**，通过**一个内部 Binding**，它不表达组合。
需要把跨 Profile 的能力（例如 Converse 对话 + Titan 图像生成）合成一个对外模型时，
**组合发生在 Route 层**：同一个 public model 下挂多条 Route，各自指向一个 Deployment，
路由按请求的核心操作选择候选。

因为 Runtime 与 Mantle 的凭据方案不同，跨这两个接入面的组合还意味着**两个 Provider
实例**，各自持有自己的凭据。

提交 `operation_bindings` 试图把多个 Binding 塞进一条 Deployment 会被具名拒绝
（`400 operation_bindings_unavailable`），拒绝信息会指向上面这条做法。

## Mantle 的当前边界（1.0.0）

三条限制，配置前先确认能接受：

**只支持 Bedrock API key。** AWS 控制台的接入向导默认选中 IAM 凭据（走默认凭据链：
环境变量 → `~/.aws/credentials` → 附加角色）。**Halro 走的是第二个选项**，照着控制台
默认值配会在保存凭据时失败。数据面不使用默认凭据链、IMDS 或容器 metadata，这是
安全裁决，不是待办。

IAM 用户仍然是权限的来源，只是以 API key 的形式投影出来。长期 key 就是该用户的
service-specific credential：

```bash
aws iam create-service-specific-credential \
  --user-name <your-bedrock-user> \
  --service-name bedrock.amazonaws.com \
  --credential-age-days 90
```

响应中的 `ServiceApiKeyValue` 即 Halro 凭据里要填的密钥；`ServiceSpecificCredentialId`
留作停用与轮换之用。`--credential-age-days` 定下的到期日可以填进凭据表单的**到期时间**
（可选）：控制台会在凭据库里显示剩余天数、过期后标红。这是提醒，不是强制限制——网关不会
因为这个日期拒绝请求，到期后仍照常调用，直到 AWS 自己返回 401。除了推理权限（例如 `AmazonBedrockMantleInferenceAccess`），还要确认
没有策略 Deny 掉 `bedrock-mantle:CallWithBearerToken` —— 那个 action 单独管“能否用
bearer token 走 Mantle 端点”。

**短期 key 目前不适用。** 它取 12 小时与 IAM 会话时长中更短者，且只在生成它的 region
有效；Halro 在 topology 构造时固定密钥、没有自动刷新，到期后全部请求 401，且按下面的
规则不重试、不 fallback。在自动刷新立项完成前，请使用长期 key 并人工轮换。

**Bedrock Project 是 Provider 级属性。** AWS 把 Workspaces（Anthropic 协议）与
Projects（OpenAI 协议）实现为同一种 Bedrock project 资源，分别由 `anthropic-workspace`
与 `OpenAI-Project` 请求头选择；省略请求头时归入账户的 default project。

Halro 的做法：Provider 表单上的 **Bedrock Project ID** 留空即 default project（不发头），
填写 `proj_` 开头的 ID 则该实例的全部请求都归入该 project。

- 一个 Provider 只指向一个 project。需要两个 project 就建两个 Provider——**可以复用同一
  条凭据**，各自保留自己的并发上限、熔断状态、能力证据与记账目标；
- 不接受 `wrkspc_` 开头的值：那是 Claude Platform on AWS 的 workspace ID，属于另一条
  产品线；
- 保存时不做在线校验（AWS 长期 key 的默认策略只允许 get/list projects）。project 不存在
  或已归档会在**请求时**以认证错误暴露，且按 401/403 的规则不重试、不 fallback。

注意这与上一节说的不是一回事：Halro 自己的 Project 隔离、预算与记账与此无关，这里管的是
**AWS 账单与 IAM 侧**的 project 维度。

**Deployment 的 Region 必须与 Provider endpoint 的 Region 一致。** Bedrock project
是 region 作用域资源（ARN 里带 region），模型可用性、配额与能力证据同样按 region
区分。显式填写的 Region 与 endpoint 派生的 Region 不一致会被拒绝；留空则从 endpoint
推导。

另外，三个 Mantle Profile 的能力集是**编译期固定**的：控制台不提供勾选，API 保存
超出上限的能力会被拒绝。这与 Runtime 的 Converse 不同，后者仍由运维声明。

**连接测试的费用不一样。** Anthropic Messages 没有免费的元数据接口，它的探测是一次
真实推理调用（输出限 1 个令牌）——会计费。Chat 与 Responses 两个实现读取模型元数据，
不计费。控制台在选中 Messages 实现时会给出提示。

顺带一条与记账有关的事实：能力识别、连接测试与健康探测都可能产生上游费用，但**不进
Ledger、不计入项目预算与用量**（见发布说明的已知限制）。也就是说 AWS 账单会略高于
Halro 的账本合计。

最后一条不是限制而是事实：**这三个 Profile 尚未对真实 Bedrock Mantle 账户发过任何
请求。** 端点形状、路径、认证头与能力声明都来自 AWS 文档，并由本仓的契约测试钉住
——契约测试证明 Halro 按文档发送，不证明服务端接受。见
`docs/verification/provider-real-matrix.md`。
