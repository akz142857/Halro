# 模型能力自动识别：保留的安全与任务契约

状态：**普通创建交互已由[跨服务商模型选择与能力自动解析方案](provider-model-selection-and-capability-resolution.zh-CN.md)取代；本文件仅保留能力检测的安全、预算和任务生命周期契约。**

更新日期：2026-08-10

本文不再定义“主要用途”、Binding 消歧或普通流程能力矩阵。Invocation Target、Capability Claim、Deployment Variant、0/1/N Variant 和普通 Admin UX 以替代方案为准。

## 1. 证据与 fail-closed 边界

- `/models` 或其他目标目录只证明目标可见，不证明任何能力。
- 不向模型询问“你支持什么”，不把模型自然语言自述作为证据。模型输出是不可信输入，只能由受限解析器用于验证 wire 结构。
- 只有固定协议探测明确成功的能力可记为 `supported`；一次 Chat 成功不能推断 Embeddings、SSE、工具、视觉、JSON 或媒体能力。
- 超时、连接失败、5xx、限流、认证失败、权限不足、配额不足、区域不可用、取消、预算耗尽和响应不明确都不能写成 `unsupported`。
- `unsupported` 只来自稳定、经过评审且明确表示“不支持”的协议结果。
- 检测不在 Gateway 请求热路径执行，不改变在线 Deployment，也不能替代对已保存精确 Deployment revision 的测试。
- 检测不得预填 `LastTestRevision`；新 Deployment 仍保存为停用，测试成功后显式启用。

## 2. 逐能力终态与用户出口

| 状态 | 含义 | 可自动保存为能力 | Admin 下一步 |
| --- | --- | --- | --- |
| `supported` | 固定协议判据明确成立 | 是 | 可继续，并且只能收窄 |
| `unsupported` | 稳定明确的“不支持” | 否 | 接受结果或换目标 |
| `inconclusive` | 已完成但没有可信结论 | 否 | 结束本次检测；可进入高级接入 |
| `unavailable` | 超时、限流、5xx、配额或暂时不可用 | 否 | 仅引导检查 Provider 配置/状态；冷却期内不重复检测 |
| `unauthorized` | 凭据、权限或 Provider opt-in 不足 | 否 | 仅引导修复 Provider；不得用高级声明绕过本次结果 |
| `not_probed` | 风险级别、预算或接口决定不发请求 | 否 | 保持未知 |
| `canceled` | 管理员取消或任务被终止 | 否 | 保持未知 |

`inconclusive` 是本次检测终态，普通界面不显示立即重试 CTA。失败、取消或重启中断也不得由前端自动生成新幂等键重试；继续操作必须由管理员重新明确发起。

## 3. 调用预算与副作用

- 一次检测最多执行配置允许的有界 Provider 调用数；当前普通确认界面显示上限 8 次。
- 每项探测必须限制输入、输出、响应体和超时；费用未知时不得显示虚假的美元估算。
- 默认安全检测不得创建文件、批处理、异步任务或其他持久 Provider 资源。具有持久副作用的能力不进入普通自动检测。
- 取消只停止尚未发出的请求。已经到达 Provider 的请求可能产生费用，UI 必须明确说明。
- Provider 返回 usage 时可记录有界用量和调用计数，但不得记录 Prompt、模型输出或 Provider 错误正文。

## 4. 幂等、single-flight 与 UNKNOWN

- 同一管理员、同一请求语义和同一幂等键返回原任务；同 key 不同语义返回冲突。
- 同一目标指纹的并发请求 single-flight，不能因多标签页重复执行可能计费的 Provider 调用。
- 调用结果若可能已到达 Provider 但本地无法确认，状态为 `UNKNOWN`：不自动重试、不换 key 重试、不把结果写成支持或不支持。
- 同一个幂等键不得重新执行已经可能发生的调用；只能查询或重放同一任务结果。
- 所有终态不可重新进入运行态。重试必须由管理员重新确认，并使用新的明确任务和幂等键。

## 5. 选择版本、取消与晚到结果

- 前端每次切换 Provider、目标、region、canonical model 或调用接口都生成新的 `selection_revision`，并清除旧 Binding、能力、解析结果和确认状态。
- 检测响应只有在 `selection_revision` 与当前表单选择精确一致时才能进入 UI。
- 服务端同时绑定 Provider/Credential revision、目标、Binding、Profile、region、检测器版本和目标指纹。
- 取消后或选择变化后到达的成功结果可以留作任务审计，但不得覆盖新表单、不得写入新 Deployment snapshot。
- 重启把仍在执行的任务收口为 `interrupted`；不得自动重放可能已经计费的调用。

## 6. TTL、快照与持久化边界

- 检测结果有 `observed_at`、`expires_at` 和 revision。过期结果只供审计查看，不可用于新的 Deployment snapshot。
- Claim/detection cache 是可重建派生数据；已保存 Deployment 的不可变 Model Capability Snapshot 才是运行时权威。
- TTL 到期、cache 删除、目录不可用或后续发现新能力都不得静默改写、收窄或扩大已保存 Deployment。
- 新能力进入复核流程；冲突使新解析 fail closed。现有在线流量只按其已保存 snapshot 和既有生命周期门禁运行。

## 7. 数据最小化与审计

任务记录和审计 metadata 只保留完成复核所需的有界字段：Provider/Binding/Profile 标识、目标安全摘要、状态计数、调用次数、风险级别、revision 和错误分类。

禁止持久化或写入审计：

- Provider 凭据、认证头和密钥版本原文；
- 原始 Provider 请求/响应、Prompt、模型输出和 Provider 错误正文；
- 可被误用为重放材料的任意私有协议载荷。

指标 label 只能使用有界枚举，不使用模型 ID、目标 ID、检测 ID 或其他高基数标识。

## 8. 发布门禁

仓库测试必须证明：

- 同幂等键与同指纹并发不会重复调用；
- 暂时故障、认证、权限、配额、限流和 UNKNOWN 不会被误记为 `unsupported`；
- 达到调用预算后不再发起新请求；
- 取消和 selection revision 会丢弃晚到结论；
- 重启不会自动重放可能计费的请求；
- 原始响应、输出、凭据和错误正文不会进入存储、审计或日志；
- 过期 Claim/cache 删除不改变已保存 snapshot。

精确 RC commit 的真实 Provider opt-in 检测仍需计费授权和真实凭据，属于外部发布门禁；fixture、fake-server 和本地浏览器 RC 不能替代它。
