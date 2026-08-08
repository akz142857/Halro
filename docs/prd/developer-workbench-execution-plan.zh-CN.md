# 开发者工作台执行闭环

## 目标

开发者工作台用于验证其他业务系统通过 Halro Gateway 发起请求时的真实行为。第一版执行闭环支持：

- Chat Completions、Responses、Embeddings；
- 普通 JSON 响应和 SSE 流式响应；
- HTTP 状态、响应头、响应体、Request ID 与端到端延迟；
- 取消正在进行的请求；
- 使用 Request ID 跳转并筛选“用量与调用”；
- Gateway Key 只存在当前页面内存和本次请求，不写入浏览器存储、URL、Admin 配置或 React Query 缓存。

## 安全边界

工作台页面中的 Gateway 地址用于生成外部系统代码示例，不作为 Admin 服务端的网络访问目标。真实调试请求只允许进入当前 Halro Runtime 已配置的 Gateway Handler，不接受任意 URL、Host、重定向或上游 Provider 地址，避免新增 SSRF 和内网探测能力。

Admin 执行入口必须同时满足 Admin Session、MFA policy、Same-Origin 和 CSRF 校验。Gateway Key 通过本次请求的 `Authorization: Bearer` 传入，执行入口不得记录、回显或持久化该值。执行请求随后使用独立构造的 Gateway Request，只复制允许的 Content-Type、Accept、正文和客户端地址，不复制 Admin Cookie、CSRF、转发链或其他 Admin Header。

## 请求与观测契约

执行入口只接受以下固定映射：

| 工作台协议 | Gateway Path |
|---|---|
| Chat Completions | `/v1/chat/completions` |
| Responses | `/v1/responses` |
| Embeddings | `/v1/embeddings` |

Gateway 为每个 OpenAI-compatible 请求生成 `X-Request-ID`，并通过请求 Context 交给 Accounting，因此响应头、Ledger 和 Usage 使用同一个 ID。普通响应在读取完成后显示总延迟；SSE 按到达顺序增量显示原始事件，完成或取消时冻结延迟。前端最多保留 1 MiB 响应文本，超过后取消读取并明确标记截断，防止管理页面无界增长。

“用量与调用”增加精确 `request_id` 过滤。只有响应返回非空 Request ID 时才允许跳转；鉴权前或请求解析前失败可能没有 Usage 记录，页面应保留原始响应而不伪造关联。

## 验收

1. 普通响应能显示真实状态、头、格式化 JSON、Request ID 和延迟。
2. SSE 事件逐块出现，完成、服务端错误和用户取消均有明确状态。
3. 无 Key、非法 JSON、非法协议和超限正文均 fail closed，且不触发任意网络访问。
4. 刷新页面后 Gateway Key 消失；代码示例、响应、错误和 URL 均不包含 Key。
5. Request ID 可精确筛选 Usage，且与 Ledger/Usage 中记录一致。
