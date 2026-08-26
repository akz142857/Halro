# 控制台无法创建 `openai.responses.v1` 连接：修复方案

状态：**待实施**
更新日期：2026-08-27
范围：`web/src/pages/ProvidersPage.tsx`、`web/src/hooks/useProviderProfiles.ts`、`web/src/i18n/locales/{zh-CN,en-US}.ts`、`web/src/pages/ProvidersPage.test.tsx`
前置：PR #231（`fix(domain): stop offering a connection tick whose save is always refused`）

---

## 0. 决策

`b8ddd1b` 新增的 `openai.responses.v1` profile 和它独有的 `provider_executed_tools`（web search）能力，**在 Admin 控制台里没有入口**。操作者无法创建以该 profile 为 anchor 的连接，因此也无法启用 web search；只有直接调 Admin API 显式传 `profile_id` 才能用。

评估后的决定：

1. **把「能力实现」选择器从 Bedrock 专属推广到所有服务商类型**（§3）。选择器本身、它背后的 catalog 数据、以及服务端对显式 `profile_id` 的处理**全部已经存在且已就绪**——缺的只是 `ProvidersPage.tsx` 里三处把它锁死在 `type === "bedrock"` 的条件（§1.2）。
2. **服务端不改**（§4）。已核验 `providerFromInput` 对 `input.ProfileID` 没有任何按类型的门禁，catalog 也已经把 `openai.responses.v1` 连同 `combines_with` 一起供给了前端。任何"服务端要先支持"的判断都是错的。
3. **不把 `provider_executed_tools` 放回 chat profile 的连接天花板**（§2）。那正是 PR #231 修掉的缺陷：它会让表单重新 offer 一个必定 400 的勾。
4. **不改 anchor 的默认值**。OpenAI 的 `default_profile_id` 仍是 `openai.chat-embeddings.v1`；Responses 是操作者显式选择的结果，不是默认。

本方案**不需要**重新初始化数据目录，**不改任何持久化结构**，也**不改 Admin API 的线协议**。它是一次纯前端改动，外加 i18n 文案。

---

## 1. 现状

### 1.1 缺口的形状

`provider_executed_tools` 在 OpenAI 上只存在于 `openai.responses.v1` 的 profile 天花板上（`internal/domain/provider_table.go:151`）。而连接的能力分配遵循「一个能力 → 唯一归属 profile」：

- anchor = `openai.chat-embeddings.v1` 时，`chat` 归属 anchor 自己，`provider_executed_tools` 归属 peer。peer 只拿到一个 modifier、没有任何 operation 能力，写边界拒绝该 binding。**PR #231 已让天花板不再 offer 这个勾。**
- anchor = `openai.responses.v1` 时，`chat` 和 `provider_executed_tools` **同时归属 anchor 自己**，binding 合法，保存成功。

所以功能的唯一正确入口就是「把连接 anchor 在 Responses profile 上」。金文件可以直接验证这一点：

```
openai.chat-embeddings.v1 -> provider_executed_tools = False | chat = True
openai.responses.v1       -> provider_executed_tools = True  | chat = True
openai.media-resources.v1 -> provider_executed_tools = False | chat = False
```

### 1.2 控制台把 anchor 锁死在默认 profile

三处，全在 `web/src/pages/ProvidersPage.tsx`：

| 位置 | 现状 | 后果 |
| --- | --- | --- |
| `:620-624` | `initialProfile` 无论什么类型都回落到 `defaultProfileID(catalog, "bedrock")` | 非 Bedrock 连接的 `profileID` 状态从一开始就是一个**跨类型的无关值** |
| `:639` | `anchorProfile = type === "bedrock" ? profileID : defaultProfileID(catalog, type)` | 非 Bedrock 一律用默认 profile 算天花板 |
| `:690` | `profile_id` 只在 `...(type === "bedrock" ? { profile_id: profileID, ... } : {})` 里发送 | 非 Bedrock 的创建请求根本不带 `profile_id`，服务端回落到默认 profile |

外加两处配套：`:105-107` 的 `isBedrockProfile` 是类型专用的；`:796` 的选择器渲染条件是 `type === "bedrock"`。

### 1.3 catalog 已经供出了需要的一切

`/admin/api/v1/provider-profiles` 的响应（金文件 `web/src/test/provider-profiles.golden.json`）里，OpenAI 三个 profile 齐全，且 `openai.responses.v1` 的 `immutable` 为 `false`（意味着它的能力集可由操作者勾选，不是构建期固定）：

```
openai | default_profile_id = openai.chat-embeddings.v1
    openai.chat-embeddings.v1 | immutable=False | combines_with=['openai.responses.v1', 'openai.media-resources.v1']
    openai.responses.v1       | immutable=False | combines_with=['openai.chat-embeddings.v1', 'openai.media-resources.v1']
    openai.media-resources.v1 | immutable=True  | combines_with=['openai.chat-embeddings.v1', 'openai.responses.v1']
```

`profilesForType`、`connectionCeiling`、`connectionDefaults`、`combinableProfiles`、`unservableCapabilities`（`web/src/hooks/useProviderProfiles.ts`）全部接受任意 `type` + `profileID`，**没有一个是 Bedrock 专用的**。

---

## 2. 为什么不能走「把能力放回 chat 天花板」这条捷径

看起来更省事的做法是让 `provider_executed_tools` 重新出现在 `openai.chat-embeddings.v1` 的连接天花板上，操作者就不用换 anchor 了。这条路是错的，理由有三层：

1. **它就是 PR #231 修掉的那个缺陷。** `ConnectionCeiling` 的契约写在函数注释里：*"a form built from this cannot offer a tick whose save is refused"*。放回去等于恢复一个每次保存都 400、且错误信息既不点名能力也不点名 profile 的勾。
2. **绕过写边界同样不行。** 让 assignment 把 `chat` 也复制给 peer，会产生同一连接下两个都声明 `chat` 的 binding。这改变了「一个能力 → 唯一归属」的模型语义，影响面远超本缺口。
3. **anchor 不只是一个标签。** 它决定 `base_url`、`access_surface`、`credential_scheme`，以及**这个连接的请求打到哪个端点**。把 Responses 能力挂到 chat anchor 上，等于让一个寻址 `/v1/chat/completions` 的连接声明只有 `/v1/responses` 才服务的能力——这正是 `openai.responses.v1` 被拆成独立 profile 的原因（见 `provider_table.go:110-121` 的注释）。

---

## 3. 方案：推广「能力实现」选择器

### 3.1 状态初始化（`:620-624`）

```tsx
// 现在
const bedrockDefaultProfile = defaultProfileID(catalog, "bedrock");
const initialProfile = current?.profile_id && isBedrockProfile(catalog, current.profile_id)
  ? current.profile_id
  : bedrockDefaultProfile;

// 改为
const initialProfile = current?.profile_id && isProfileOfType(catalog, initialType, current.profile_id)
  ? current.profile_id
  : defaultProfileID(catalog, initialType);
```

`isBedrockProfile`（`:105-107`）相应改成按类型参数化：

```tsx
function isProfileOfType(catalog: ProviderProfilesCatalog, type: ProviderType, value: string) {
  return profilesForType(catalog, type).some((profile) => profile.id === value);
}
```

### 3.2 anchor（`:639`）

```tsx
const anchorProfile = profileID;   // 去掉 type === "bedrock" 三元
```

注释需要同步更新——现有注释解释的是「天花板与默认值是两个不同问题」，那部分仍然成立，但「Bedrock 才有 anchor 选择」的隐含前提要去掉。

### 3.3 提交载荷（`:690`）

`profile_id` 对所有类型发送。`access_surface` 与 `credential_scheme` **保持只在 Bedrock 发送**——服务端在 `admin_providers.go:1321-1323` 会校验这两个字段（若发送）必须与 profile 一致，多发一处就多一处漂移点，而不发时服务端从 profile 自己解析：

```tsx
const value = {
  name, type, base_url: baseURL,
  profile_id: profileID,
  ...(type === "bedrock" ? {
    access_surface: findProfile(catalog, type, profileID)?.access_surface,
    credential_scheme: findProfile(catalog, type, profileID)?.credential_scheme,
    ...(selectedSurface === "bedrock-mantle" ? { bedrock_project_id: ... } : {}),
  } : {}),
  ...
};
```

### 3.4 类型切换时的重置（`:785-795`）

```tsx
// 现在：硬编码 "bedrock"
setProfileID(defaultProfileID(catalog, "bedrock"));
setCapabilities(connectionDefaults(catalog, next, defaultProfileID(catalog, next)));

// 改为
const nextProfile = defaultProfileID(catalog, next);
setProfileID(nextProfile);
setCapabilities(connectionDefaults(catalog, next, nextProfile));
```

### 3.5 选择器的渲染条件与副作用（`:796-811`）

渲染条件从 `type === "bedrock"` 改为 `profilesForType(catalog, type).length > 1`——单 profile 的类型（如 DeepSeek）不该出现一个只有一个选项的下拉。

`onChange` 里的三个副作用要按类型泛化：

- `setBaseURL(profile?.default_base_url ?? "")` —— 对 OpenAI 两个 profile 都是 `https://api.openai.com`，行为不变，但代码路径要通用
- `setCredentialID(...)` 的凭据筛选按 `profile?.access_surface` 匹配。OpenAI 的 chat 与 responses **共享 `openai` 访问面与 `bearer_static` 方案**，所以既有凭据可以直接复用，不需要新建凭据——这是本方案对操作者最友好的一点，值得在 hint 文案里说
- `setCapabilities(connectionDefaults(catalog, type, next))`

### 3.6 i18n

`providers.bedrockProfiles`（`zh-CN.ts:1027`）是一张 `profileID → 显示名` 的表，键本身与 Bedrock 无关，只是命名如此。改为 `providers.profileNames`，补上三个 OpenAI 条目：

```
"openai.chat-embeddings.v1": "Chat Completions + Embeddings（默认）",
"openai.responses.v1":       "Responses（无状态，支持供应商自执行工具）",
"openai.media-resources.v1": "媒体与资源（图像 / 语音 / 文件 / 批处理）",
```

`providers.bedrockProfileHint`（`:1003`）是 Bedrock 专属措辞（"每个服务商实例只选择一种线协议；需要多个 Mantle 协议时，请分别创建实例。"）。hint 要回答操作者此刻的问题，所以按类型分文案：Bedrock 保留原句；OpenAI 用一句说明"选 Responses 才能启用 web search，且与 Chat 连接共用同一把 Key"。

en-US 同步。

---

## 4. 服务端不需要改（已核验）

- `internal/app/admin_providers.go:1312-1319`：`requestedProfile := input.ProfileID`，随后 `domain.ResolveProviderProfile(input.Type, requestedProfile)`。**没有任何按 provider 类型的门禁。**
- `:1450-1452`：若请求显式给了 `profile_id`，服务端会检查它确实出现在分配结果里，否则拒绝——"would answer 201 with a profile_id the caller did not ask for"。这条保护对新入口同样有效。
- `AssignConnectionCapabilities(ProviderOpenAI, ProfileOpenAIResponses, openAIResponsesSet)`：`chat`、`tools`、`vision`、`json_object`、`structured_outputs`、`developer_role` 全部归属 anchor 自己，`provider_executed_tools` 也归属 anchor（在其 Ceiling 内）。binding 合法。

---

## 5. 编辑既有连接：重新 anchor 的语义

`admin_providers.go:274-284` 已有的规则，本方案不改，但要在 UI 上说清楚：

- **有部署引用时不允许改 profile**：`"provider type and profile cannot change while deployments reference it"`（400）。表单应在这种情况下禁用选择器并给出原因，而不是让操作者提交后才知道。
- **改 profile 会清空能力证据**：`currentEvidence = nil`。这是正确的——证据是对某个接口面测出来的，换面即失效。UI 需要一句确认提示，否则操作者会以为检测结果还在。

---

## 6. 验证清单

前端（`web/src/pages/ProvidersPage.test.tsx`）：

1. OpenAI 类型下选择器出现，且列出三个 profile
2. 选中 `openai.responses.v1` 后，`provider_executed_tools` 出现在可勾选项里（`configurableCapabilities` 来自 `connectionCeiling`）
3. 提交载荷带上 `profile_id: "openai.responses.v1"`
4. 单 profile 的类型（如 `deepseek`）不渲染选择器
5. 切换类型后 `profileID` 落到新类型的默认 profile，而不是残留上一个类型的值

`web/src/hooks/useProviderProfiles.test.ts`：`connectionCeiling(catalog, "openai", "openai.responses.v1").provider_executed_tools === true`，且 `"openai.chat-embeddings.v1"` 为 `false`。

后端：**无需新增测试**。`TestConnectionCeilingIsAssignableAndBounded`（PR #231 已加强，现在要求天花板产生的每个 binding 都声明 operation）已经覆盖了这条路径的正确性。

改完 `web/src` 后必须 `npm run build` 并提交 `internal/webui/dist`，否则 CI 会因 bundle 陈旧而失败。

对既有测试的影响（已核验，非推测）：`ProvidersPage.test.tsx` 共 33 个用例，其中 8 处通过可及名 `/^能力实现/` 定位该选择器，**全部在 Bedrock 流程里**；没有任何一处断言"非 Bedrock 不存在该选择器"（`queryByRole` 的 7 次使用均与此无关）。因此真正的风险不是断言失败，而是 OpenAI 流程的表单里**新出现一个控件**，可能影响那些按位置或按快照定位元素的用例。

---

## 7. 不做什么

- **不改 `default_profile_id`。** OpenAI 新连接仍默认 chat profile。
- **不做「一个连接同时服务 chat 与 responses」。** 需要两者的操作者建两个连接，共用同一把 Key。这不是权宜之计：两个 profile 寻址不同端点，能力集也不同（Responses 不声明 streaming 与 reasoning，见 `provider_table.go:117-121`），把它们合成一个连接会让路由无法回答"这个请求该打哪个端点"。
- **不动 profile 表的行序。** `provider_table.go:143-147` 明确写着：chat profile 必须排在 responses 之前，因为两者共享 `(type, surface, scheme)`，而存储的凭据解析到第一个匹配行；调换会静默地把每个既有 OpenAI 连接重新指向另一个端点。

---

## 8. 工作量

纯前端，约 6 处改动 + 1 张 i18n 表 + 5 条测试。没有迁移、没有 API 变更、没有数据目录影响。风险集中在 OpenAI 流程的表单里新增一个控件对既有用例的连带影响（§6 末）。
