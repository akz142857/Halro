# 模型能力字典演进与四层展示

状态：已归档（2026-08-11）。Phase 1 的一部分已落地，另一部分已被后来的检测改版取代；Phase 2–3 从未开始，能力字典仍是 v1。

更新日期：2026-08-11

> 归档说明：本文写于 2026-08-10，其后 `7d1a48b`、`4af0228` 重做了能力检测的呈现方式，本文 Phase 1 的两条勾选因此不再描述现存 UI。下面的清单已按当时代码逐条订正 —— 一份勾选说谎的归档比没有归档更坏。Phase 2–3 的内容仍然成立，将来要做能力字典 v2 时可以从 §3、§4 重新起草。

## 1. 背景与边界

当前 Deployment 使用 17 个布尔能力描述 Halro 已经能够路由、验证和保存的模型能力：

- `chat`、`embeddings`、`moderations`、`images`、`transcriptions`、`speech`、`rerank`、`async_generate`；
- `vision`；
- `streaming`、`tools`、`json_mode`、`developer_role`、`reasoning`、`stream_usage`；
- `files`、`batches`。

这是一份**固定、版本化的能力字典**，不是“所有主流模型能力全集”。识别结果可以来自内置目录精确条目或当前模型的协议级探测，但结果字段必须来自该字典。`max_context_tokens` 和 `max_output_tokens` 是数值规格，不属于 17 个布尔能力。

本方案不把 Provider 产品功能、模型输入输出模态、协议特性继续混在一个平铺列表中，也不因为某个 Provider 发布了新功能就立即加入路由契约。只有 Halro 已定义请求语义、Provider 映射、证据判据和路由行为的能力，才能进入可保存字典。

## 2. 四层能力模型

| 层级 | 责任 | 当前字段 |
| --- | --- | --- |
| 模型操作 | Deployment 可以执行的顶层操作，也是路由候选筛选的主要依据 | `chat`、`embeddings`、`moderations`、`images`、`transcriptions`、`speech`、`rerank`、`async_generate` |
| 输入输出模态 | 模型在某个操作中能够直接理解或生成的内容类型 | `vision`（当前仅表示对话中的图像输入） |
| 协议特性 | 同一模型操作在 wire contract 上可选的行为 | `streaming`、`tools`、`json_mode`、`developer_role`、`reasoning`、`stream_usage` |
| 服务商托管能力 | 依赖账户级资源、持久任务或服务商托管生命周期的功能 | `files`、`batches` |

第一阶段只改变 Admin UI 的组织方式，不改变 API、存储结构、路由语义和现有能力名称。

## 3. 明显缺失或粒度不足

| 候选能力 | 当前问题 | 建议归属 | 进入字典前必须解决 |
| --- | --- | --- | --- |
| 严格结构化输出 / JSON Schema | `json_mode` 只证明可返回 JSON，不能证明 schema 约束 | 协议特性 | 统一 schema 请求、严格校验和 Provider 降级语义 |
| 原生音频输入 | `transcriptions` 是独立转写操作，不能表示多模态模型直接理解音频 | 输入输出模态 | 定义音频 content part、大小限制和计费单位 |
| 原生音频输出 | `speech` 是独立 TTS 操作，不能表示对话模型直接生成音频 | 输入输出模态 | 定义输出事件、编码格式和流式行为 |
| Realtime 会话 | `streaming` 只表示单次 HTTP/SSE 流，不能表示 WebRTC/WebSocket、VAD、打断和有状态会话 | 协议特性或独立操作 | 完成 Realtime 架构、会话所有权、审计和用量契约 |
| 视频输入 / 视频理解 | `vision` 仅覆盖静态图像输入 | 输入输出模态 | 定义上传/引用方式、时长限制和输入计量 |
| 视频生成 / 编辑 | `async_generate` 过于笼统，不能表达视频生成的输入输出和任务生命周期 | 模型操作 | 定义异步任务状态机、取消、清理和 UNKNOWN 处理 |
| 文档/PDF理解 | `files` 只表示资源接口，`vision` 也不能表达文档解析和引用行为 | 输入输出模态 | 区分直接 content、Files API 引用、OCR/视觉解析 |
| 引用 / Citations | 当前没有可路由、可验证的引用输出契约 | 协议特性 | 定义引用结构、来源绑定和完整性校验 |
| Grounding / Web Search | `tools` 只证明普通函数调用，不能表示 Provider 托管搜索 | Provider 托管能力 | 区分托管工具与客户端工具，并定义费用和数据边界 |
| Code Execution / Computer Use | `tools` 粒度不足，且两者具有显著执行副作用 | Provider 托管能力 | 独立安全策略、沙箱、授权、审计和副作用状态机 |
| File Search / URL Context / MCP | 不能由 `files` 或 `tools` 准确代表 | Provider 托管能力 | 定义资源所有权、外部访问与 SSRF/凭据边界 |
| 并行工具调用 | `tools` 只表示至少一个合法 tool call | 协议特性 | 定义并行调用语义、上限和顺序保证 |
| 流式工具参数 | `streaming + tools` 不能证明参数增量事件可用 | 协议特性 | 定义增量组装、终止和错误恢复规则 |
| Prompt Caching | 当前既没有请求能力也没有缓存用量证据 | 协议特性 | 定义显式/隐式缓存、TTL、读写计费和 usage 字段 |
| 多候选输出 | 当前没有 `n`/candidate 路由契约 | 协议特性 | 定义响应集合、流式索引和费用计算 |

不应加入能力字典的项目：Fine-tuning、模型部署容量、区域可用性、价格、上下文窗口、输出上限、数据驻留和配额。它们属于模型规格、Deployment 配置或治理元数据。

## 4. 字典与证据契约

- 字典必须有显式版本；新增、重命名或拆分能力都属于契约变更。
- 内置目录、Provider Profile、Detection Adapter、Deployment Snapshot、路由语义、前端类型和 i18n 必须覆盖同一版本。
- `/models` 中出现模型 ID 仍不构成能力证据。
- Provider 托管工具不能因为通用 `tools` 探测成功而自动启用。
- 有费用、持久资源或外部副作用的能力不得进入默认低成本自动探测。
- `not_probed`、`inconclusive`、`unauthorized` 和 `unavailable` 都不能转换成 `unsupported`。

## 5. 实施任务

### Phase 1：四层 UI（不改后端契约）

- [x] “适用能力”按四层分组展示。**成立**：`deploymentCapabilityGroups`（`web/src/pages/DeploymentsPage.tsx`）的 `operations` / `modalities` / `protocol` / `managed` 与 §2 四层一一对应，能力编辑器与只读视图共用同一份定义。
- [~] “查看逐项识别结果”使用同一分组和稳定顺序。**已被取代**：`7d1a48b`、`4af0228` 把逐项结果换成了按候选接口出卡片——每个接口写明被问了什么、回了什么、它本来能确定什么。今天没有 17 项逐项列表，检测结果只用于算出“未能确定”的那几项。
- [~] 每层显示简短责任说明和该层已启用/已支持数量。**只做了一半**：数量在（`capabilityGroupSelected` 渲染 `{selected}/{total}`）；责任说明没渲染——`deployments.capabilityGroups.*.description` 中英文都写了，代码只取 `.title`，是死字符串。
- [~] 保留完整 17 项结果和原有状态，不隐藏 `not_probed` 或失败分类。**已被有意推翻**：风险策略按设计不探测安全自动集之外的能力，把这类 `not_probed` 报成未决会让每次检测都显得没做完，因此现在刻意不显示。失败分类本身仍然分开保留。
- [ ] 使用真实 Provider 检测结果完成视觉验收；不得为验收额外触发付费检测。**未做**。

### Phase 2：冻结 Capability Dictionary v2（未开始，`CapabilityDictionaryVersion` 仍为 1）

- [ ] 为能力条目定义稳定 ID、层级、依赖、风险等级、证据方式和生命周期类型。
- [ ] 决定首批新增项；建议优先 `structured_outputs`、`audio_input`、`audio_output`、`document_input`、`citations`。
- [ ] 明确 `json_mode → structured_outputs`、`vision → image_input`、`async_generate` 的兼容或迁移策略。
- [ ] 决定 Realtime 是否属于独立 Operation，禁止只增加一个布尔字段后复用普通 Chat 生命周期。

### Phase 3：端到端契约（未开始）

- [ ] 更新 domain、API schema、存储迁移和备份恢复版本。
- [ ] 更新模型目录及目录摘要，缺失来源保持 unknown。
- [ ] 为每个 Provider/Profile 添加明确映射；无 Adapter 时 fail closed。
- [ ] 为可安全验证的能力定义协议级探测，禁止解析模型自然语言自述。
- [ ] 更新语义路由需求和 compatibility manifest。
- [ ] 增加跨 Provider 契约测试、真实账号 RC 门禁和升级回滚测试。

## 6. 验收标准

- UI 分层和底层保存字段一一对应，不因分组改变 payload。
- 同一能力在手动声明和逐项识别结果中只出现一次且属于同一层。
- 新字典版本不会把旧 Deployment 静默扩宽能力。
- 任何自动勾选结果都有内置目录或 `verified_probe` 证据。
- 未实现完整请求、响应、路由和审计契约的候选能力，只保留在本 TODO，不进入生产字典。
