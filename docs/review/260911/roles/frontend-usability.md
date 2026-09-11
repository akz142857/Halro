# R5 前端与可用性独立评审

评审对象：`v0.7.1..222d08f84f61493fc9a273d351cc728528d6e30c`
评审日期：2026-09-11
角色结论：**PARTIAL**。未发现前端能够绕过服务端产品/地域/条款授权的 P0/P1；确认 4 项 P2、2 项 P3，并保留 2 个需要真实浏览器或慢服务端实验才能裁决的候选。P2 在修复前需要 owner 明确处置，不能把 222 个 jsdom 测试通过解释为真实用户旅程已通过。

本角色未读取其他 `roles/` 草稿，未修改产品代码。为复现负向行为曾临时加入两个 Vitest 文件，运行后已经删除；最终工作区没有 `web/` 改动。

## 1. 覆盖范围

逐行审阅了以下本轮改动及其直接边界：

- `web/src/pages/ProvidersPage.tsx`、`useProviderProfiles.ts`：Credential 与 Provider 的创建、编辑、详情、Offering/Surface/Region、条款确认和提交；
- `DeploymentsPage.tsx`：Provider 产品/地域标签与订阅成本提示；
- `UsagePage.tsx`、`UsageFailuresPanel.tsx`、`FailureDetailDrawer.tsx`：Offering 筛选、失败语义、Gateway 原始请求展示与浏览器缓存边界；
- `ProjectsPage.tsx`、`styles.css`、`components.tsx`：Gateway Key scope、Modal、键盘、焦点、窄屏与对比度相关实现；
- 中英文词条、类型声明、页面测试、i18n、accessibility 与 design-system 测试。

## 2. 已确认问题

### R5-F01 — P2 — 最终失败页不会跟随同 tab 的 query-only 导航

**入口与可达条件**

1. 打开 `/admin/usage?tab=failures&request_id=req_old`；
2. 页面不卸载，通过应用内 `navigate` 或浏览器前进/后退进入 `/admin/usage?tab=failures&request_id=req_new`；
3. 地址栏已经是 `req_new`，输入框和发往 `/usage/failures` 的筛选仍是 `req_old`。

**证据**

- `UsagePage.tsx:118-131` 订阅 `useNavigationLocation`，会在位置变化时更新父组件状态；
- `UsageFailuresPanel.tsx:31-41` 直接从 `window.location.search` 初始化六个 `useState`，之后没有 `popstate`/`halro:navigate` 同步；
- 父组件仍渲染同一个 `<UsageFailuresPanel />`，React 保留子组件状态；
- 临时 Vitest 负向复现：URL 从 `req_old` 变为 `req_new` 后，`Request ID` 输入仍为 `req_old`，1/1 通过（测试确认现有错误行为，随后删除）。

**违反基准**：review-plan §7.5 “query-only 导航、浏览器前进后退保持正确”，INV-15。
**建议回归**：在 `UsagePage.test.tsx` 从一个 failures URL 导航到另一个 failures URL，断言输入值和最后一次 `api.usageFailures` 参数均采用新值；覆盖 request/project/deployment/offering/start/end。

### R5-F02 — P2 — Provider 跨固定地域时禁用保存却不显示原因

**入口与可达条件**

1. 创建 BigModel Global Provider，选中 `bigmodel.global.chat.v1`；
2. 把基础地址改成 CN 官方 host `https://open.bigmodel.cn`；
3. 保存按钮被禁用，但地址字段没有错误文案，页面也没有说明下一步。

**证据**

- `ProvidersPage.tsx:938-942` 正确计算 `fixedRegionMismatch`；
- 唯一把解释写入 `errors.baseURL` 的代码位于表单 `onSubmit`，见 `ProvidersPage.tsx:1053-1067`；
- 地址字段仅显示 `errors.baseURL`，见 `ProvidersPage.tsx:1131-1139`；
- 按钮却在 `fixedRegionMismatch` 时直接禁用，见 `ProvidersPage.tsx:1233-1236`，因此 `onSubmit` 永远不会执行；
- 临时 Vitest 负向复现确认“按钮 disabled 且跨地域说明不存在”，1/1 通过（随后删除）。

Credential 表单在相同条件下会直接把错误传给 `Field`（`ProvidersPage.tsx:807-810`），两条路径行为不一致。服务端仍会拒绝错误地域，未形成越权；问题在于操作员无法解释地被卡住。

**违反基准**：INV-05、INV-15；review-plan §7.1 要求 endpoint mismatch 在 Admin UI 可行动呈现。
**建议回归**：Global/CN 四象限分别断言 match、cross-region、custom unknown；cross-region 时错误文本可见且与 control 关联，不依赖点击 disabled button。

### R5-F03 — P2 — Credential 必填名称/地址的本地拒绝是静默的

**入口与可达条件**

1. 打开新建 Credential；
2. 填入 secret，但把“凭据名称”或地址留空；
3. “加密保存”仍可点击；点击后不发送请求，也没有错误、`aria-invalid` 或焦点变化。

**证据**

- `ProvidersPage.tsx:702-708` 在 `name.trim()` 或 `baseURL.trim()` 为空时只是不调用 `mutation.mutate()`；
- 名称和地址 input 没有 `required`，也没有 `Field error`，见 `ProvidersPage.tsx:734-736`、`807-809`；
- footer 仅因 pending、policy refresh、region mismatch、缺 secret 或 step-up 缺密码而禁用，见 `ProvidersPage.tsx:836-840`；
- 临时 Vitest 负向复现确认：空名称、已填 secret 时保存按钮 enabled；点击后 API 未调用，页面无 alert，名称无 `aria-invalid`，1/1 通过（随后删除）。

**违反基准**：INV-15 与 review-plan §7.5 的错误提示/焦点要求。
**建议回归**：名称空、地址空、非法 URL、secret 空分别验证可见且关联到 control 的错误和首个错误焦点；不要仅断言请求未发送。

### R5-F04 — P2 — GatewayRequest reveal 把所有读取失败都误报为“没有捕获”

**入口与可达条件**

在失败详情中点击“查看”，服务端返回下列任一响应：

- `404 failure_capture_disabled`；
- `503 audit_unavailable`（审计不可写，服务端按设计拒绝正文）；
- `401` 过期会话或其他网络/解析错误。

UI 都显示同一句“没有保存该请求的原始内容”。

**证据**

- `FailureDetailDrawer.tsx:124-151` 使用 `retry:false`，并把任意 `payload.isError` 映射到 `usage.failures.noPayload`；
- 服务端特意区分“功能未启用”（`internal/app/failure_capture.go:91-96`）和“审计不可用，正文被扣留”（`:134-145`），前端丢掉了这个可行动信息；
- 当前测试 `UsageFailuresPanel.test.tsx:222-231` 只用普通 `Error("not found")` 固化统一文案，没有覆盖 `ApiError.code/status`。

该问题不会泄漏正文，反而保持 fail-closed；但它会把安全控制故障误诊成“当时没抓到”，过期会话也没有可恢复提示。

**违反基准**：review-plan §7.5 的 401/403/过期会话与错误态要求；INV-08 的审计边界需要在 UI 中可解释。
**建议回归**：至少区分 disabled/not-found、audit unavailable、session expired、generic transport；仍可让真正的跨 Project miss 保持不可枚举。

### R5-F05 — P3 — 旧 Usage 记录缺少新增归因字段时 UI 直接省略，而非显示 unknown

`UsagePage.tsx:381-397` 与 `UsageFailuresPanel.tsx:274-290` 在 `offering_id/profile_id/account_region_id` 为空时给 `Fact` 传 `undefined`；`FailureDetailDrawer.tsx:80-90` 随即不渲染这一事实。新记录、旧记录和“产品没有地域轴”因此缺少清楚区别。没有伪造默认值，但未满足 INV-11 明确要求的“旧记录缺字段时显示 unknown”。

建议让 API 暴露记录/schema 世代或明确的 `*_recorded` 信号；前端不要仅凭空字符串猜测“旧记录”。

### R5-F06 — P3 — Provider 列表对 endpoint 派生地域不显示账号地域

`ProvidersPage.tsx:127-139` 的 `productLabel` 只读取 profile 的固定 `region_id`；`ProviderRow` 在 `:382-387` 使用它。Kimi 与 MiniMax API Platform 的 Profile 按设计 `region_id=""`、地域由 endpoint 派生，因此已知 CN/Global host 在 Provider 行仍不显示账号地域。相邻的 Deployment 选择器已经采用正确做法：`DeploymentsPage.tsx:1257-1272` 用 `regionForEndpoint(offering, provider.base_url)` 补全地域。

这不会改变路由，endpoint 仍可见；但 Provider 详情未完整呈现本轮新增归因事实，容易让同名跨地域连接难以快速区分。

## 3. 候选 finding / 需要实机裁决

### R5-C01 — P2 候选 — pending 表单可关闭并以新 idempotency 意图重开

Credential 和 Provider 表单均没有把 `mutation.isPending` 传给 `Modal.closeDisabled`（`ProvidersPage.tsx:731`、`:1046`），Cancel 还直接调用 `onClose`（`:837`、`:1233`）。请求没有 AbortSignal；因此慢请求提交后可以关闭表单，再打开新表单提交第二次。Provider 的 idempotency key 是“每个 form 一个”（`:947-950`），重开会生成新 key；Credential create 根本不带 idempotency key（`web/src/api.ts:351-353`，服务端在 `internal/app/admin_providers.go:81` 生成随机 ID）。若第一次请求最终成功，第二次也可创建重复资源。

静态路径完整，但本角色未用受控慢 Admin server 证明两个写都落盘，暂列候选。应以 deferred response 做端到端测试：pending 时 Escape/Cancel/×/backdrop 的行为、重开重复提交、原请求成功/失败后的通知与列表刷新。

### R5-C02 — P3 候选 — Provider tabs 的方向键不环绕

`ProvidersPage.tsx:269-276`（当前文件相应 `handleTabKey`）把 ArrowLeft 永远指向 Providers、ArrowRight 永远指向 Credentials；从首项向左、末项向右不会环绕。Usage tabs 在 `UsagePage.tsx:138-148` 已实现环绕。现有 Provider 测试没有方向键用例。需要在 Chrome/Safari + 屏幕阅读器下确认目标交互规范后裁决。

## 4. 负面证据（已检查且未发现问题）

- Provider/Credential 表单只在 `/provider-profiles` 成功后挂载；catalog 失败有 retry，没有本地猜测或 fallback matrix（`ProvidersPage.tsx:238-243`、`:354-368`）。
- BigModel Credential 四个产品/地域 identity 都来自服务端 catalog；选择时同时切换 endpoint，并提交精确 `access_surface + scheme`。Provider 在存在多个 connection choice 时提交精确 `profile_id + access_surface + credential_scheme`。
- 条款确认是原生 required checkbox；折叠状态下提交会展开并聚焦。Provider/Credential 都在服务端返回 `usage_policy_revision_mismatch` 后清空确认、刷新 catalog、要求确认新 revision；刷新失败时禁用继续提交。目标测试覆盖 CN/Global revision。
- 条款链接使用 `target="_blank" rel="noreferrer"`；BigModel CN/Global 指向各自文档。中英文词条完整性测试通过。
- Deployment 选择器把名称、Offering、endpoint 派生/固定 Region 一起显示；subscription/entitlement 展示“订阅成本治理”，没有把未知账单表现为零。
- Failure payload 在明确点击前不读取；读取 key 包含 request ID，`gcTime:0`、`staleTime:0`、`retry:false`，drawer 卸载后不保留查询缓存。正文通过 React 文本节点中的 `JSON.stringify` 输出，不使用 `innerHTML`；三个截断标志独立显示。
- policy rejection 不显示 Provider/正文读取入口；失败详情能显示 Offering/Profile/Region、Provider reason、retryable 与 ambiguous，并用 `failure_semantics_recorded` 区分旧记录与明确的 `false`。
- Gateway Key scopes 使用 `fieldset/legend`，两列 grid 在 580px 以下折为一列；scope 文本允许换行。design-system 断言覆盖布局结构。
- Modal 有 `role=dialog`、`aria-modal`、标题关联、Escape、topmost-dialog 判断、Tab trap、关闭后焦点恢复；terms 使用原生 `details/summary`。这些是静态/jsdom 证据，不替代真实浏览器与辅助技术。
- 对 catalog 做了当前数据检查：同一 `ProviderType + access_surface` 没有多种 credential scheme，故表单当前按 surface 过滤 Credential 不会在 v0.8.0 catalog 中混入错误 scheme；这是当前数据事实，不是未来结构保证。

## 5. 测试与命令证据

在候选 SHA 上执行：

```text
$ npm test -- src/pages/ProvidersPage.test.tsx src/pages/DeploymentsPage.test.tsx \
    src/pages/UsageFailuresPanel.test.tsx src/pages/UsagePage.test.tsx \
    src/accessibility.test.tsx src/i18n/i18n.test.tsx src/design-system.test.ts
Test Files  7 passed (7)
Tests       222 passed (222)
Duration    4.96s
exit        0

$ npm test -- src/pages/UsageFailuresPanel.review.test.tsx
Test Files  1 passed (1)   # 通过代表复现了 stale filter
exit        0

$ npm test -- src/pages/ProvidersPage.review.test.tsx
Test Files  1 passed (1)
Tests       2 passed (2)   # 通过代表复现了无解释 region block 与静默必填拒绝
exit        0

$ git diff --check -- web/src
exit        0
```

临时 `*.review.test.tsx` 已通过 `apply_patch` 删除；`git status --short` 仅有 `docs/review/260911/`。

执行环境是 Node `v25.9.0` / npm `11.12.1`。Vitest 5 / jsdom 30 的依赖引擎不把 Node 25 列为支持版本，因此本地通过只能作为定向逻辑证据；受支持 Node 版本上的 CI/最终 gate 仍是必须条件。

## 6. 未验证边界

本角色没有启动真实浏览器和完整 Admin 后端，因此以下不能宣称通过：

- 登录 → BigModel Credential/Provider → Deployment → Project/Gateway Key → fake-provider → Usage failure → disable key 的完整旅程；
- Chrome/Safari 的原生 `<details required checkbox>`、焦点恢复、Escape/Tab、200% zoom、forced-colors、窄屏与水平表格滚动；
- 屏幕阅读器对 drawer、terms 状态和动态错误的实际播报；
- 慢请求时关闭/重开表单的双写结果（R5-C01）；
- Admin 会话在 payload reveal 瞬间过期的真实恢复路径。

在这些实机项完成前，不能把 R5 写成 PASS。建议先修 R5-F01～F04，再用真实浏览器把上述旅程录为 `evidence/runtime/`，并由非本角色评审者复核 P2 的严重度与处置。
