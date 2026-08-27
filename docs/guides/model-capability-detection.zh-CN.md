# 模型能力识别操作指南

模型能力识别用于确定一个模型在这个账号上实际开放的能力：既用于创建未被
Halro 内置目录收录的模型 Deployment，也用于对目录已收录的模型做实测校验。
它不是连接测试，也不会代替保存后的 Deployment 测试。

## 创建流程

1. 选择服务商并填写或选择真实模型 ID。
2. 对内置目录已知模型，Halro 直接应用目录能力，不调用服务商；目录是评审
   当时的结论，需要核对账号实际情况时，点击“实测校验”显式发起识别。
3. 对未知模型，阅读调用上限与费用提示后，明确点击“确认模型并识别能力”。
4. 等待检测完成。Halro 只勾选通过固定协议验证的能力；可以关闭不需要的
   能力，普通流程不能开启未验证能力。
5. 保存停用的 Deployment，配置价格并执行该 Deployment 当前 revision 的
   测试。测试通过后再显式启用。

输入、搜索、选择、hover 与失焦不会触发检测。更换服务商、模型、区域、
目标或能力接口会立即丢弃表单中的旧检测引用，必须重新确认。

## 调用与费用边界

- 一次检测最多 9 次服务商调用，默认总超时 90 秒；全局最多 4 个检测，
  同一服务商最多 1 个，每位管理员默认每分钟最多启动 6 个新检测。目录、
  新鲜缓存和幂等复用不消耗这个创建额度。
- 请求只包含 Halro 编译进二进制的短固定输入，输出上限 16 tokens；不读取
  管理员 Prompt，也不相信模型对自身能力的自然语言描述。
- 普通检测不会创建文件、Batch、图像、音频或异步资源。Files、Batches、
  Images、Transcriptions、Speech、Async Generate 与无法强验证的 Reasoning
  保持未检测。
- JSON 相关能力占两个探针，不是一个。`json_object` 只发 schema-less 的
  `response_format`，`structured_outputs` 发一个 `strict` 的 json_schema 且
  校验返回值真的符合该 schema——只支持 schema-less 模式的模型会拒掉后者。
  一个探针分不开这两件事：被要求返回 JSON 的模型即使没有 schema 能力，
  也会给出能解析的 JSON。
- 调用可能计费。没有版本化价格时 Halro 只承诺请求与 token 上限，不承诺
  金额；这些调用不记入某个 Project 的 Gateway Usage 或预算。
- 每次可能计费的调用都先在 Detection 记录中持久化为 `reserved`，随后变为
  `running`。崩溃恢复把两者记为 `unknown`，绝不自动重放。

取消会停止尚未发出的调用并传播上下文；服务商已经收到的请求仍可能产生
费用。取消后到达的晚结果不会成为 `supported`。

## 结果与处置

| 显示结果 | 含义 | 建议 |
|---|---|---|
| 已支持 | 固定协议成功 | 可保留或关闭 |
| 明确不支持 | 上游拒绝可归因到被测字段 | 看下方上游状态与标识后接受关闭 |
| 无法确认 | 响应不足以证明能力 | 使用高级声明或稍后重试 |
| 暂时不可用 | 超时、连接、限流、配额或 5xx | 排除临时故障后重试 |
| 无权验证 | 凭据、权限或 opt-in 不足 | 修复凭据/权限后重试 |
| 未检测 | 风险政策、依赖或预算不允许调用 | 使用目录或高级声明 |

“明确不支持”只有两种来源，都不读服务商的自然语言错误文本：

- 上游用结构化标识把被拒字段本身标为不支持（`unsupported_parameter`、
  `unsupported_value`）。只有 OpenAI 系错误体（OpenAI、Azure、OpenAI 兼容
  端点、Bedrock Mantle）能给出这种标识。
- 上游拒绝了请求但没说拒的是哪一部分（Anthropic 的 `invalid_request_error`、
  Bedrock 的 `ValidationException`、Gemini 的 `INVALID_ARGUMENT`），而这次
  探针是依赖型探针——它等于已经验证通过的基线请求再加上被测的那一个字段，
  所以拒绝归因到该字段。基线探针本身（如 chat）拿到同样的拒绝时只记为
  “无法确认”：模型 ID 写错的报错和能力不支持的报错在这些服务商上完全一样。

后一种还要同时满足两个条件，否则同样只是“无法确认”：状态码确实是拒绝请求
体的那一类（400/422；Bedrock 看 `ValidationException`），并且上游自己的结构化
标识解析出了非空值。各家 adapter 的“坏请求”分类是状态码 switch 的兜底，404、
409、413 落进来的样子和 400 一模一样；而解析不出标识的响应体（前置代理、CDN、
HTML 错误页、只回 `{"detail": ...}` 的兼容服务）根本不是上游在谈这个请求。

## 实测校验目录已收录的模型

对目录已知模型发起识别时，Halro 记录当时的目录声明作为基线，直接在目录
指明的能力接口上探测，不额外花调用去识别接口。合并规则：

- 探针给出“已支持”或“明确不支持”的能力，以探针结果为准。
- 其余情况（未检测、无法确认、暂时不可用、无权验证、已取消）保留基线声明。
  沉默不等于否定：Images、Speech、Transcriptions、Files、Batches、Async
  Generate 按设计永远不会被探测，不能因此被判为不具备。
- 基线声明仍受探针结论约束：chat 被判明确不支持时，依赖 chat 的能力一并
  关闭。

保存后的 Deployment 快照按能力逐项记录证据等级：探针验证过的记
`verified`，从基线带过来的记 `declared`，不会把没测过的能力说成测过。

一次探针全部失败（凭据被拒、上游不可用）的校验记为**失败**，不是完成：沉默
保留基线意味着它的推荐结果看起来和真测过一模一样，而 `verified_probe` 是唯一
允许 `verified` 证据、且豁免目录漂移检测的来源。至少要有一项探针成功，这次
校验才算数。

“明确不支持”这一行会同时显示上游返回的状态码和标识（不含错误正文）。这不是
装饰：拒绝没点名参数时，Halro 把它归到本次探针新增的那个字段上——字段归得对，
结论未必对。上游也可能是在拒绝探针自己的载荷（2026-08-26 实测过一次：探针的
固定图片是一张损坏的 PNG，Bedrock Mantle 严格解码器拒收，于是一个能读图的模型
被记成不支持视觉）。模型文档说支持而这里判不支持时，先看这两个标识。

高级手动声明是私有模型、权限受限目标和当前不支持自动识别的能力接口的
逃生通道。它产生 `operator_declared`，不能伪装成 `verified_probe`。

## 缓存、重启与备份

同一 Provider revision、Credential revision/key version、Binding、Profile、
Access Surface、模型、目标类型、区域、Detector 版本与风险级别组成内部目标
指纹。默认 24 小时内复用同指纹成功结果，Provider 调用为零；强制刷新默认
有 5 分钟冷却。只有目录直接作答的那条记录被豁免（它按定义 `max_provider_calls`
为 0），所以免费的目录答案不会把随后的第一次实测校验挡在窗口外；排队中和运行中
的校验一样计入冷却。目标指纹和凭据版本不返回浏览器，也不进入指标标签。

检测任务、幂等索引、调用状态与 TTL 保存在 metadata bbolt 中，标准加密
Backup/Restore 会一并保存。恢复后 `queued/running` 统一转为 `interrupted`，
已可能发生的调用标为 `unknown`，管理员需要重新明确确认。终态记录默认保留
30 天；过期即时阻止 Deployment 引用，物理清理由后台完成。

## 配置、审计与指标

有效配置位于 `admin.model_capability_detection`，可在“设置 → 系统配置”查看
运行中进程的实际值。修改 YAML 后需要重启。

审计动作包括 `started`、`cache_reused`、`completed`、`failed`、
`cancel_requested` 与 `expired`。模型只以安全摘要进入检测审计；请求/响应、
生成文本、Provider 错误正文和凭据都不会记录。Deployment 继续使用既有
`deployment.capability_snapshot.created`，其中 `source=verified_probe`，不新增
职责重复的创建事件。

Prometheus 指标以 `provider_type`、能力、状态和证据来源等有界枚举聚合。
完整名称与 label 契约见 [Metrics reference](../contracts/metrics-reference.md)。

真实账号发布验证是 opt-in 且可能计费，按
[Real-account Provider matrix](../verification/provider-real-matrix.md) 在精确 RC
commit 上执行。
