# V5 证伪 — A3-4「目录声明可把 PET 带进 Deployment」

**裁决：REFUTED**

拦截它的防御（即使目录声明 PET 也打不开）：

- `internal/app/admin_deployments.go:1064` — `modelcatalog.Clamp(entry.Capabilities, binding.Capabilities)`
- `internal/app/admin_invocation_targets.go:579` — 变体路径同样 `Clamp(..., binding.Capabilities)`
- `internal/domain/provider_table.go:151`、`:158` — `Defaults` 不含 PET，只有 `Ceiling` 含
- `internal/domain/provider_connection.go:100-157` — `AssignConnectionCapabilities` 只在 `requested`
  的开启位上分配，从不加宽；`internal/app/admin_providers.go:1364` 新建连接从 `Defaults` 起步

原发现的核心前提——「PET 的默认关闭靠的是目录恰好没人声明」——**不成立**。默认关闭由
profile 的 `Defaults` 列 + 上面那道 Clamp 共同保证，与目录声明与否无关。

---

## 1. `Ceiling()` 是不是"刻意"——是，但它的角色是"上限"不是"取值"

`internal/modelcatalog/catalog.go:214-225` 的注释确实点名 PET，原报告这一句复述准确。但
Ceiling 在本包里只出现在三处，全部是**判据**而非**取值**：

- `catalog.go:299` `Entry.Validate()`：`ProviderCapabilitiesSubset(e.Capabilities, e.Key.Ceiling())`，
  超出即拒（构建期 panic / 远端解析期报错）；
- `catalog.go:388` `Merge()`：`capabilities = Clamp(capabilities, key.Ceiling())`，把多来源合并
  结果收到上限内；
- `builtin.go:98` `pinnedProfileEntry()`：唯一一处把 Ceiling 当**取值**用，但只服务四个 Bedrock
  pinned profile（`builtin.go:103-108`），这四行的 profile Ceiling 里没有 PET
  （`provider_table.go:327-330` 的 `withProviderExecutedTools` 只用在 OpenAI Responses 与
  Anthropic Messages 两行）。

原报告设问的"再与 Defaults 求交"没有发生——但发生了一件等价的事：与
**binding.Capabilities** 求交。binding 的能力集在新建连接时来自 `Defaults`
（`admin_providers.go:1364`），PET 不在其中；要打开必须显式在请求体里送
`capabilities.provider_executed_tools = true`，而且 `AssignConnectionCapabilities`
（`provider_connection.go:112-126`）只在 `requested` 已开的位上做归属，**没有任何路径把
binding 提到 Ceiling**（全仓 `MaxProviderCapabilitiesForProfile` 的非测试调用只有
`providers.go:427`、`models.go:390` 两处校验，和 `catalog.go:225`）。

这道显式勾选还带强制告知：`domain.CapabilityOptInWarnings()`（`provider_table.go:462-476`）
把 `provider_executed_tools` 单列，经 `admin_provider_profiles.go:82-86,134` 下发给表单，
控制台 `ProvidersPage.tsx:636` 消费。

## 2. 三个采纳点逐条

| 采纳点 | 采纳的是什么 | 目录声明 PET 时能否打开 |
|---|---|---|
| `admin_deployments.go:947` `retained := selected.Capabilities` | 变体能力，其构造已在 `admin_invocation_targets.go:579` 被 `Clamp(..., binding.Capabilities)` | 仅当 binding 已开 PET |
| `admin_deployments.go:1064` `Clamp(entry.Capabilities, binding.Capabilities)` | 目录条目 ∩ binding | 仅当 binding 已开 PET |
| `admin_deployments.go:1077-1085` `retained == nil` 时原样返回 | 上一格的结果 | 同上 |

写入前的后续校验，逐条结论：

- `admin_deployments.go:555-556` `ProviderCapabilitiesSubset(capabilities, binding.Capabilities)`：
  Clamp 已保证成立，**这一道不拦 PET**（原报告若指望它，指望错了）。
- `providers.go:808-830` `deploymentCapabilities()` 的逐位与：`available` 来自 adapter，而
  adapter 的能力就是 binding 声明（`anthropic/adapter.go:77,83` 取 `options.Capabilities`），
  **也不拦**。
- 真正拦住的只有 `:1064` / `:579` 这一道 Clamp，以及它的左值 `binding.Capabilities` 的默认值。

## 3. 内置目录是否声明 PET——独立核对：不声明

写脚本遍历 `modelcatalog.Builtin().Entries()`：**143 条，0 条声明 PET**。
`builtin.go` 全文 `ProviderExecutedTools` 只有一次命中，是 `:226-229` 的注释；文件里
根本没有 PET 的构造 helper（`with(...)` 的可选项只有 vision / structuredOutputs /
reasoning / developerRole 等）。原报告这一条属实。

## 4. 「签名远端目录 1.1.0 才启用」——**这句不准确，但真实的门更硬**

- 配置面**今天就有**：`internal/config/config.go:291-301` 的 `ModelCatalog{Enabled ...}`，
  `internal/config/default.yaml:218-219` `enabled: false`；`runtime.go:367-375` 已接线。
  所以"1.1.0 才启用"只是 `docs/contracts/provider-capabilities.md:124-125` 的路线图措辞，
  **不是代码 gate**。原报告以它作为可达性依据是薄弱的。
- 真正的 gate 是信任根：`internal/modelcatalog/trust.go:20` 的 `ReleaseTrustRoots` 只能由
  ldflags 注入；`Makefile:24` 的 `GO_LDFLAGS` 不注入，`Dockerfile:19` 的 ARG 默认空，且
  `tools/modelcatalog/test_workflow_contract.py:44-47`
  （`test_release_keeps_dynamic_signed_catalog_inactive`）**断言 release workflow 里既不出现
  `MODEL_CATALOG_TRUST_ROOTS` 也不出现 `modelcatalog.ReleaseTrustRoots`**。
  `ProductionTrustRoots()`（`trust.go:22-28`）因此在任何构建里都返回空，
  `snapshot.go:263-289` `verifySignatures` 在 `rootByID` 为空时对每个签名 `continue`，
  最终返回 `catalog signature is not trusted or is invalid`。LKG 复读同样走这条
  （`manager.go:405`）。
- 结论：可达性判断不需要改成"更宽"，反而应改成"**更窄且有测试守着**"——即使运维把
  `model_catalog.enabled` 打开，也载不进任何签名条目。

## 5. 假想世界的可见性——运维看得见，不是静默开启

- 控制台把 `provider_executed_tools` 排在部署能力表单的 `protocol` 组里
  （`web/src/pages/DeploymentsPage.tsx:1209`），选定变体时用
  `setCapabilities({ ...variant.capabilities })` 预填（`:2103`），保存时**总是**显式带上
  `capabilities`——也就是说控制台流程根本走不到 `retained == nil` 分支；`retained == nil`
  是裸 API / 脚本路径。
- 即便走裸 API 建成，部署详情页仍逐条列出已开能力（`DeploymentsPage.tsx:278,545-547`）。
- 数据面还要客户端主动请求 provider tool 才会触发：`internal/gateway/service.go:2542` 是
  requirements→capabilities 的匹配，能力开着但没人请求就没有出网。

所以即使将来目录声明了 PET，其形状是"默认值来自目录、在运维已接受的信封内、且屏幕上看得见"，
而不是"静默开启"。

## 实测结果（脚本已移出仓库，存 scratchpad）

在 `internal/app` 构造一条声明 PET 的 Anthropic Messages 目录条目
（`Entry.Validate()` 通过 → 说明目录侧确实允许声明），再跑
`resolveDeploymentTargetWithCatalog(instance, deploymentInput{}, "claude-pet-1", "", nil, cat)`：

| binding 能力 | 结果 |
|---|---|
| `MaxProviderCapabilitiesForProfile`（PET 已开） | `capabilities.ProviderExecutedTools = true`，`declared=false`，`source=signed_catalog` |
| `DefaultProviderCapabilitiesForProfile`（PET 未开） | `capabilities.ProviderExecutedTools = false` |

第二行是决定性的：**目录声明了 PET，也打不开一个没在连接层勾选过的部署。**

另测：`ProductionTrustRoots()` 在本仓构建下返回 0 个根。
`Builtin().Entries()` 143 条全部无 PET。

## 与原报告的差异

1. 原报告的判据「默认关闭靠的是目录恰好没声明」错了：靠的是 `Defaults` 列 + `:1064`/`:579`
   的 Clamp，这是一道**结构性的**默认关闭，不是巧合。
2. 原报告承认了前提 (a)「连接侧 binding 已开 PET」，却把结论写成"运维没做过任何动作"。
   实际做过：一次带强制告知（`CapabilityOptInWarnings`）的显式勾选，且没有任何路径能替他勾。
3. 原报告的可达性依据（1.1.0 路线图）不是代码 gate；真实 gate 是空信任根 + 一条
   Python 契约测试，比原报告说的更硬。
4. 原报告未提可见性：控制台流程不经过 `retained == nil`，且能力逐条上屏。
5. 原报告建议的两个修法都有代价：在 `Key.Ceiling()` 上单独扣掉 PET，会让目录永远无法表达
   "这个模型支持 web_search"，正是 `catalog.go:216-222` 注释说明要保留的那半张表；在采纳处
   把 PET 清零，会让"运维已在连接层接受、又逐模型确认"的正常配置每次都要重勾。

## 残留观察（降级为"建议"，不建议按问题处理）

裸 API `POST /admin/api/v1/deployments` 省略 `capabilities` 时，能力默认值来自目录 ∩ binding。
若将来签名目录启用且某条目声明 PET，这条路径会让一个新部署带上 PET 而请求体里没提过它——
仍在运维已接受的信封内，但脚本作者可能没意识到。若要收紧，成本最低的形状是**只对
`CapabilityOptInWarnings()` 里的名字**要求显式出现在请求体中（缺失即视为关闭），
落点在 `admin_deployments.go:1077-1085` 与 `:947`，一处 3 行，既不动 `Key.Ceiling()`
的表达能力，也不影响控制台（它本就总是显式发送）。此项应写进 CHANGELOG 的已知边界即可。
