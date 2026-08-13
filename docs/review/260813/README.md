# 260813 三平台 API/模型适配摸底

对 OpenAI、AWS Bedrock（Runtime + Mantle）、Anthropic 三个平台，摸清 Halro 当前**实际适配到哪一步**：北向端点、南向 Provider Profile、模态覆盖（文本/图片/语音/嵌入/重排/批处理/异步）、以及每处适配声明与验证证据之间的落差。

本次摸底的触发背景：三个平台刚完成初步的人工联调验证（OpenAI、Bedrock Mantle、Anthropic 各接入一份真实凭据）。摸底要回答的是"联调通了之后，还剩什么没适配、什么适配了但没证据"。

## 文档

| 文件 | 内容 |
|---|---|
| [`api-model-adaptation.zh-CN.md`](api-model-adaptation.zh-CN.md) | 跨平台矩阵：北向端点 × Provider Profile × 模态，以及五条跨平台发现 |
| [`openai.zh-CN.md`](openai.zh-CN.md) | OpenAI 平台逐项摸底 |
| [`bedrock.zh-CN.md`](bedrock.zh-CN.md) | AWS Bedrock（Runtime/Agent Runtime + Mantle）逐项摸底 |
| [`anthropic.zh-CN.md`](anthropic.zh-CN.md) | Anthropic 平台逐项摸底 |

## 方法与边界

**做了什么**：通读 `internal/provider/*`、`internal/compatibility/*`、`docs/compatibility/endpoint-manifests.json`、`internal/app/runtime.go` 的北向路由表、`tests/provider-matrix`，逐条核对"声明的适配面"与"代码里真实存在的路径"。每条结论都带 `文件:行号`。

**没做什么**：没有对三个平台发起真实计费请求。本文所有关于"上游行为"的判断，来源是各平台的公开文档与仓库内既有证据，不是本次实测。凡是无法从代码或文档确证的，本文标注为"未验证"，不写成结论。

**判定标记**：【肯定】既有设计正确且有证据；【问题】有代码证据的缺陷或空洞；【建议】方向性意见；【未验证】需要真实账号才能定论。

## 结论摘要

三个平台的**文本对话**主链路都已打通，且都经过了初步人工验证。差距集中在两处：一是**非文本模态严重不对称**，二是**验证证据的覆盖面远小于声明的适配面**。

北向 20 个端点里，`compatible` 状态的只有 5 个（chat/embeddings/responses/messages/count_tokens），其余 15 个是 `experimental`，且每一个的 manifest 里都写着同一句话——"official SDK black-box matrix is not yet validated"（`docs/compatibility/endpoint-manifests.json`）。

### 风险排序

| # | 严重度 | 发现 | 位置 |
|---|---|---|---|
| 1 | 高 | ~~Anthropic 既不在 GA 真实账号发布门禁里，也没有适配器级真实冒烟测试~~ **已修复**：新增 `internal/provider/anthropic/real_smoke_test.go`（原生非流式/流式、portable 非流式/流式、count_tokens、模型目录、空模型探测），并加入 `gaProfiles` | `internal/provider/anthropic/real_smoke_test.go`；`tests/provider-matrix/main.go:54-57` |
| 2 | 中 | Bedrock Converse Profile 拒绝 `tools`/`tool_choice`/`response_format`/`reasoning_effort`，实际只支持纯文本对话。**降级**：定位早已写清（见下方更正），缺的只是功能本身 → [TODO §1](../../todo/provider-adaptation-gaps.zh-CN.md) | `internal/compatibility/provider_fields.go:44-53`；`internal/provider/bedrock/adapter.go:296` |
| 3 | — | ~~七类能力没有自动探针~~ **误判，已更正**：图片/语音/转写/异步是高成本，文件/批处理会创建持久对象，自动探测在这两类上不能做。保留的部分（"这些能力永远只能是 declared"这一后果从未被写下）已补进代码注释 | `internal/provider/capability_detection.go:44-59` |
| 4 | 中 | 非文本模态的南向实现是单点：图片=OpenAI + Titan Image V2，语音/转写/文件/批处理=仅 OpenAI，重排/异步=仅 Bedrock 固定模型 → [TODO §2](../../todo/provider-adaptation-gaps.zh-CN.md) | `docs/compatibility/endpoint-manifests.json` |
| 5 | 中 | Anthropic 平台已有的 Message Batches、Files API 完全未适配；Halro 的 `/v1/batches`、`/v1/files` 只能落到 OpenAI → [TODO §3](../../todo/provider-adaptation-gaps.zh-CN.md) | 见 [anthropic.zh-CN.md](anthropic.zh-CN.md) |
| 6 | 低 | Bedrock 四个固定模型 Profile 每个只允许一个写死的模型 ID，新模型需要改代码。**倾向保持现状** → [TODO §4](../../todo/provider-adaptation-gaps.zh-CN.md) | `internal/provider/bedrock/invoke_titan_embedding.go:65-70` |

## 两处自我更正

摸底初稿有两条判断是错的，处理如下。两条都源于同一个毛病：只读了代码，没有先去找已有文档说了什么。

**其一，#3 的探针缺失是有意排除。** 图片/语音/转写/异步的一次探测就是一次按生成价计费的真实生成，文件/批处理会在操作者账户上创建此处不会删除的对象——自动探测在这两类上不能做。保留下来的只有"这个限制的后果从未被写下"，已补进 `internal/provider/capability_detection.go:44-59`。

**其二，#2 的"定位应写进文档"早已完成。** [`docs/guides/aws-surface-selection.md`](../../guides/aws-surface-selection.md) 的「能力面的差别」一节明确写着 Converse 被"刻意钉死在纯文本对话"，附八个 Profile 的能力对照表，并写明「这些上限是刻意钉死的……放宽属于需要独立契约评审的决定」。同一份文档的「都做不到的事」一节也已经记录了 Bedrock 侧没有批量推理与护栏。**摸底漏读了这份文档**，因此把一个已经解释清楚的边界报成了文档缺口。

这也说明摸底本身该改进：下次先枚举 `docs/guides` 与 `docs/adr`，再去读代码。

## 处理结果

| # | 结果 |
|---|---|
| 1 | ✅ **已修复并已用真实账号验证**：`internal/provider/anthropic/real_smoke_test.go` + 加入 `gaProfiles` + 更新 `docs/verification/provider-real-matrix.md`。2026-08-13 对 `claude-opus-5` 实跑，七项在同一次运行内全部通过（17.165s；`count_tokens` 首跑暴露了冒烟自身的 payload 错误，修正后整跑全绿） |
| 2 | 📋 记入 [TODO §1](../../todo/provider-adaptation-gaps.zh-CN.md)；定位文档已存在，无需补 |
| 3 | ✅ **误判已更正**，代码注释已补 |
| 4 | 📋 记入 [TODO §2](../../todo/provider-adaptation-gaps.zh-CN.md)，倾向先做 Azure 语音/审核 |
| 5 | 📋 记入 [TODO §3](../../todo/provider-adaptation-gaps.zh-CN.md)，需先定北向端点形状 |
| 6 | 📋 记入 [TODO §4](../../todo/provider-adaptation-gaps.zh-CN.md)，倾向保持现状 |

#2、#4、#6 受 `CLAUDE.md` 的约束——放宽 Gemini/Bedrock Beta 的能力上限需要独立契约评审。#2 与 #6 还有一个现实阻塞：手头没有 Bedrock Runtime 的 SigV4 凭据（现有两份 AWS 凭据都是 Mantle），改了也无法用真实账号验证。

## 真实账号验证过程中新发现的两条

摸底之后按建议去跑真实冒烟，跑的过程本身又掀出两个缺陷。两条都记在 [openai.zh-CN.md](openai.zh-CN.md) 的「验证证据」一节：

1. **GA 门禁的 OpenAI 冒烟对现代模型是坏的**（已修复并实跑验证）。它对所有 profile 发 `max_tokens`，而当前 OpenAI 模型只收 `max_completion_tokens`，答以 `bad_request http=400 code=unsupported_parameter:max_tokens`。旁边的能力探测路径早就用对了参数，只有这个文件停在旧的——说明它很久没被真实账号跑过。这正是本次摸底 #1 那条结论的又一个实例，而且落在 GA 档。
2. **OpenAI 系适配器把上游错误码整个丢掉了**（已修复）。`limitedErrorMessage` 只取 `message`，`code`/`type`/`param` 全丢，`classifyHTTPError` 从不设 `ProviderCode`。后果是 OpenAI/Azure/DeepSeek/兼容端点的连接测试与网关失败日志里 `provider_code` 永远为空。上面第 1 条的诊断就是被它挡住的——第一次跑只看到 `bad_request`，补上提取后立刻看到 `unsupported_parameter:max_tokens`。

第 2 条值得单独说：它是**摸底读代码没看出来、只有真实上游返回结构化错误时才显形**的那一类。这也是本次摸底方法论上的边界——纯代码审查发现不了"字段在这里被丢弃"，除非正好去比对两个适配器的同名函数。

## 两个平台的真实验证结果

| 平台 | 结果 | 时间 |
|---|---|---|
| Anthropic | 七项一次全绿（原生双路、portable 双路、count_tokens、模型目录、空模型探测），17.165s，`claude-opus-5` | 2026-08-13 |
| OpenAI | 通过（非流式对话、语义 SSE、嵌入），7.17s，`gpt-5.4` + `text-embedding-3-small` | 2026-08-13 |

两者都在修好各自的冒烟缺陷之后才通过——Anthropic 那次是冒烟 payload 带了 count_tokens 不接受的 `max_tokens`，OpenAI 那次是冒烟发了模型不接受的 `max_tokens`。同一个词，两个不同的陈旧假设。

## 后续处理（摸底之后）

### ✅ 给 `openai.media-resources.v1` 补真实冒烟

摸底指出这个 Profile 的六个操作零真实证据。新增 `internal/provider/openai/media_smoke_test.go`，与对话冒烟分开——两者的成本与副作用不同，合在一起会让便宜的证据必须连同昂贵的一起买。

每个操作由"是否给出模型名"单独开启，操作者可以精确买自己要的证据。两处设计值得记：语音合成的输出直接喂给转写，避免在仓库里放一份没人能重新生成的音频 fixture；文件与批处理是唯一留下痕迹的部分，因此单独开关，且上传的文件在结尾无条件删除（批处理记录本身留在终态——OpenAI 没有删除批处理的操作，这也正是自动能力探测排除持久化 primitive 的原因）。

### ✅ 把 "experimental" 分级落成数据

原先 15 个端点共用同一句免责声明，它说的是**缺什么**，从不说**有什么**——于是"有契约测试和传输 fixture"的端点与"什么都没有"的端点长得一模一样。

`EndpointCompatibilityManifest` 新增 `evidence` 字段（`internal/compatibility/manifest.go`），取值为枚举：`gateway_contract`、`provider_transport_fixture`、`sdk_blackbox`。status 是判决，evidence 是判决的依据，不同意判决的人现在能看到依据。

校验强制两件事：每个端点必须声明证据；`sdk_blackbox` 与 `sdk_matrix` 必须同真同假——两种说法说的是同一件事，漂移时高估的那一侧读者是察觉不到的。那句重复了 15 次的散文随之删除，因为它现在是数据。

**刻意只放端点级的证据种类**：真实账号冒烟验证的是适配器，不是北向端点，把它算进来等于把南向证据记到一个它从未接触的界面上。那一维在 `docs/verification/provider-real-matrix.md`，按 Provider Profile 组织。

### 📋 仍未处理

`docs/todo/provider-adaptation-gaps.zh-CN.md` 的四条。其中两条的阻塞不是优先级而是条件：Converse 工具与 Bedrock 固定模型 pin 都在 Runtime（SigV4）侧，手头凭据是 Mantle，改了无法验证；第二供应商的最小方案需要 Azure 凭据。唯一凭据齐备的是 Anthropic Batches/Files，但它要先定北向端点形状——OpenAI 的 `/v1/batches` 吃已上传文件的 ID，Anthropic 吃内联请求数组，这个选择决定工作量级别。
