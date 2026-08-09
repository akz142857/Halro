# 模型能力选择 1.1.0：能力组合归属路由层

- 状态：Proposed / 目标版本 1.1.0。**§3 的 1.0.0 收尾三项已完成**；**§4 的 1.1.0 工作项尚未开始，也不应在 1.0.0 内开始**
- 日期：2026-08-09
- 基线文档：[`model-aware-capability-selection.zh-CN.md`](model-aware-capability-selection.zh-CN.md)
- 影响：取消基线文档的 Phase 3（多 Operation Binding），改由路由层承担能力组合

## 0. 结论

**一个 Deployment 表达的是这个模型自己的能力，不表达组合。需要把多种能力组合成一个对外模型时，组合发生在 Route 层。**

由此，基线文档 §5.3 与 §9 Phase 3 提出的 `operation_bindings`（在一条 Deployment 记录内维护 operation → binding 映射）**取消，不是推迟**。

```text
Deployment  = 一个模型 + 它自己具备的能力 + 一个内部 Profile
Route       = 一个对外模型名 → 一个 Deployment
对外模型能力 = 其名下所有已启用 Route 的能力并集
```

## 1. 为什么取消而不是推迟

### 1.1 运行时本来就是这样工作的

`internal/provider/provider.go` 的 `resolveCandidatesLocked` 已经按请求的核心操作过滤候选，依次检查 Adapter 是否声明该 Operation、Deployment 是否启用对应能力、证据是否达到最低等级。同一个 public model 下挂多条 Route 指向不同 Deployment 是被支持的配置：`validateAdminRoute` 只要求同一 public model 的已启用 Route 使用相同 strategy，不要求它们的能力一致；Deployment 也没有 (provider, model) 唯一性约束，因此同一个 `gpt-4o` 建两个部署是允许的。

也就是说，本原则描述的是**既有架构**，不需要新造机制。`operation_bindings` 才是新增的那一套。

### 1.2 它会同时削弱四个不变量

下面四样今天都以 Deployment 为键：

| 事项 | 现在的单位 | 若引入 `operation_bindings` |
| --- | --- | --- |
| 健康测试与 `LastTestRevision` | Deployment | 需逐 Operation，且整体 Ready 变成合取 |
| 版本化价格 | Deployment | 需按操作种类选维度，token 价与固定请求价不能混用 |
| 能力证据 | Deployment | 需逐 Operation 各自持有 |
| 文件/批处理/异步资源 owner（ADR 0009） | Deployment | 需逐 Operation 钉住，且不得跨 Binding 重试 |

基线文档 §5.3 列出的六道运行时门禁，正是这四项被拆开之后必须补回来的东西。**这些改动全部落在请求路径上**，而它们换来的只是把两条记录并成一条。

### 1.3 它会拆掉一个类型级事实

现在「Deployment 的能力不得超过其 Provider Profile 上限」是可校验的：一条记录对应一个 Profile，有唯一的比较对象。一条记录挂多个 Profile 之后，这个检查不再有单一上限可比。

### 1.4 语义上，那本来就是两个上游身份

同一个模型经两个协议提供两种核心操作，在上游是两次不同的调用：不同的失败模式、不同的价格维度、不同的探测方式。合进一条记录是把这些差异藏起来，而 Deployment 的全部价值就在于它是**一个受治理的上游身份**。

## 2. 代价，以及为什么可以接受

**运维会看到「一个模型两行」。** 基线文档想避免的正是这一点（§5.3：「不是让用户重复创建逻辑上相同的模型」）。本方案把这个负担挪到 Route 层，因此**必须由 Route 层把组合显示出来**，否则组合虽然成立却不可见。这是 1.1.0 的主要工作，见 §4。

**「这个对外模型准备好了吗」目前没有答案。** 就绪状态按 Deployment 计算；在本模型下它是一组 Route 的并集问题，控制台今天问不出来。

**一个真实边界。** 若单**一次**上游调用需要同时使用两个 Profile，本方案表达不了。目前找不到这样的场景 —— 一个请求只有一个核心操作。若将来出现，那是新的设计题，不是把 `operation_bindings` 拿回来。

## 3. 与 1.0.0 的边界

以下属于 **1.0.0 收尾**，在基线文档中完成，不等 1.1.0。**三项已于 2026-08-09 全部完成**：

- **B1.** ✅ 基线文档 §5.3、§9 Phase 3、§16、§17.4 按本结论改写：取消 `operation_bindings`，把「两个 Deployment」确立为**正式设计**而非过渡方案；§15 中与 Phase 3 绑定的两条门禁随之解除阻塞。（PR #135、#136）
- **B2.** ✅ 保留并强化对 `operation_bindings` 的拒绝。已不再依赖 `decodeAdminJSON` 的未知字段规则：该字段被显式解码为 `json.RawMessage` 只为能具名拒绝，返回 `400 operation_bindings_unavailable`，拒绝信息指向「建立第二个 Deployment 并挂到同一个 public model 上」。见 `internal/app/admin_deployments.go` 的 `refuseOperationBindings`，由 `operation_bindings_unavailable_test.go` 断言 POST 与 PUT 两条路径且断言未落库。
- **B3.** ✅ `builtinEntry` 的静默 Clamp 已移除。现在条目按写下的原样声明，由 `Entry.Validate` 拒绝超出 Profile 上限的条目，使其成为**构建期失败**而不是控制台上少几个勾。`internal/modelcatalog/builtin.go` 在该函数上方写明了失败时的正确反应：不是删掉那项能力，而是**把它建到承载得了它的 Profile 上** —— 装不进一个 Profile 说明 Halro 的 Profile 划分与该模型不符。

以下属于 **1.1.0**：

- 见 §4。**未开始，且刻意不在 1.0.0 内开始** —— 1.0.0 先发布并在生产验证，与 [`halro-ha-architecture.zh-CN.md`](halro-ha-architecture.zh-CN.md) §0 的排序理由相同。§4 是本文件剩余的全部内容。

## 4. 1.1.0 工作项：让路由层显示组合

### 4.1 对外模型视图

路由页目前是一张平铺列表（public model、部署、服务商、上游模型各占一列），没有任何地方回答「`my-gpt` 现在能做什么」。需要按 public model 归组，并给出：

- **组合后的能力覆盖** —— 该 public model 名下所有已启用 Route 的能力并集，以及每项能力由哪个 Deployment 提供；
- **未覆盖的能力** —— 没有任何 Route 提供，因此该对外模型收到这类请求会被拒绝；
- **重复覆盖** —— 多个 Deployment 提供同一能力。这是**正常的**，它就是故障转移与权重分配的形态，不应报成冲突，但要能看出来；
- **就绪状态的并集语义** —— 覆盖某项能力的 Deployment 若健康测试未通过或处于 `drifted`，该能力在这个对外模型上实际不可用，需要显示出来。

### 4.2 创建流程的衔接

当运维在部署页为某个模型选定能力后，若该模型还有其他能力落在别的 Profile 上（如对话与图像生成），控制台应当明确指出：**这属于同一个对外模型的另一条路由**，并提供直达入口，而不是让运维自己推断要再建一个部署。

这条是本方案对「一个模型两行」的正面回答：不隐藏它，而是让它读起来是**一个对外模型的两个组成部分**。

### 4.3 不做的事

- 不引入任何形式的「组合部署」记录。组合只存在于 Route 层的读模型中，不落库为新的实体。
- 不改变路由候选的解析规则。§1.1 已确认它符合本原则。
- 不因为显示需要而放宽 Deployment 侧的任何校验。

## 5. 门禁

- 一个 public model 由多个 Deployment 组成时，控制台能说出它覆盖了哪些能力、缺哪些、各由谁提供；
- 覆盖同一能力的多个 Deployment 显示为冗余而非冲突；
- 某能力的全部提供方不健康时，该能力在对外模型视图中显示为不可用，且与「从未配置」可区分；
- 提交 `operation_bindings` 的请求仍被拒绝，且拒绝理由指向 Route 层的替代做法（1.0.0 的 B2 已建立，1.1.0 不得回退）；
- 不新增落库实体，不改变请求路径。
