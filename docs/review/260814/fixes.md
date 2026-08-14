# 整改记录（对应 adversarial-verdicts.md 修复清单 1–5）

按"严重度 × 成本"顺序执行。第 5 项（F19/W4/W8）随后在同一轮完成，见文末。

## 已修

**1. F11 — 资源平面补策略快照断言**
`internal/gateway/inference_resources_store.go`：`resourcePrincipal` 在源授权之后加
`assertPolicySnapshotsCoverProject`，与 `resolveRequest` 同一道闸。资源平面（files/batches）从此
与推理平面防御对称：项目引用的 redaction/Token Guard 策略未加载时 503 拒绝，而非引擎 lookup miss
的静默 fail-open。

**2. F1 — 墓碑编辑/重复删除六处补 404**
`internal/app/admin_providers.go`（provider update、provider delete、route update、route delete）、
`internal/app/admin_projects.go`（project update、project delete）：`current.DeletedAt != nil` →
`adminNotFound`。与 Deployment（既有）、Gateway Key（既有）对齐。消除"对已删对象记成功 update/二次
delete 审计事件"。

**3. F3 — store 层引用检查排除墓碑（活体写入限定）**
- `PutRoute`（`store_providers.go`）：活 route 不得引用墓碑 deployment；route 自身的墓碑写豁免——
  否则 deployment 先删的场景下 route 永远删不掉。
- `PutGatewayKey`（`store_projects.go`）：活 key 不得挂在墓碑 project 上；key 自身墓碑写豁免。
- `PutProviderResource`（`store_providers.go`）：仅创建时（`expectedRevision == 0`）要求三个 owner
  活体；更新豁免——batch 在 deployment 墓碑化后仍要被轮询与结算，拒绝状态写会把记录卡死。

**4. F7 — 加载期空能力 withhold**
`internal/app/providers.go`：route 循环中对 `!deployment.Capabilities.AnyOperation()` 的记录
withhold（新常量 `withheldCapabilitiesInvalid`），不再落入 `deploymentCapabilities` 把空集读作
"未指定"、整套采纳 adapter 能力的 fail-open 分支。

## 尝试后撤回

**F2 — store 层 binding.Enabled 检查（已撤回，改为注释固化）**
在 `validateDeploymentProviderProfile` 加 `deployment.Enabled && !binding.Enabled` 拒绝后，三个测试
失败：`TestDanglingBindingWithholdsTheRouteAndStillLoadsTheRegistry`、
`TestRuntimeOpensWithADanglingBinding`、`TestHotReloadSucceedsWithADanglingBinding`。它们**故意**经
store 直写构造该状态，编码的是一条修复史决定：dangling binding 是二进制必须能加载、重载并被运维
编辑修复的状态——store 拒绝它会连修复它的那次 provider 写一起拒绝。结论：store 层容忍是刻意且
load-bearing 的，enforcement 点在 Admin 解析层（对抗验证已确认三层拦截）。撤回检查，改为在该函数
留注释说明为何不查，防止后人再"补"上。

## 回归测试（随修复新增）

- `internal/app/admin_tombstone_test.go` — 墓碑 Route/Project/Provider 的 update 与二次 delete 均
  404，且墓碑内容不被改动（Provider 用例顺带覆盖整条链自底向上的拆除顺序）。
- `internal/store/bolt/tombstone_reference_test.go` — 活 route 不得指墓碑 deployment、活 key 不得挂
  墓碑 project、resource 创建要求三 owner 活体；三者的墓碑写/更新豁免各有对应用例（deployment 先删
  route 仍可删、project 先删 key 仍可删、deployment 删后 batch 状态仍可写）。
- `internal/gateway/inference_resources_policy_gate_test.go` — 项目引用未加载策略时资源平面 503
  `configuration_stale`，清除引用后放行。

## 验证

- `gofmt` 干净；`go build ./...`、`go vet`（三个改动包）通过。
- `internal/app`、`internal/store/bolt`、`internal/gateway` 全包 `-count=1` 通过。
- `internal/app` 首轮仅 F2 检查引入的三个失败，撤回后复跑通过。
- **反向验证**：临时退掉 route update 的墓碑 404 后 `TestTombstonedRouteRefusesUpdateAndSecondDelete`
  确实 FAIL，恢复后通过——测试抓得住缺陷本体。
- 完整 gate（`make check` 级）留待推送前一次性执行，per AGENTS.md。

## 第 5 项：F19/W4/W8 设计裁决落地

**F19 — `allowed_routes` → `allowed_models`（就地改名，B7）**
- `domain.Project.AllowedRoutes` → `AllowedModels`，JSON 与 Admin API wire 名
  `allowed_routes` → `allowed_models`，全仓（Go + 测试 + `web/src` + 架构文档）同步。
- **迁移 29 `project_allowed_models`**（`store.go`，schemaVersion 28→29）：存量 project 记录的键
  就地搬移，`migration29_test.go` 验证值跨迁移保留且旧键从存储字节中消失——**无需再初始化数据目录**。
- 前端 wire 字段同步（i18n 文案本就叫 "Allowed model aliases"，即 W8 的归档）；bundle 重建，
  330 前端测试 + secret 扫描通过。
- 运行时确认：真实二进制上 Admin API 只出 `allowed_models`；真实数据目录迁移到 v29 后 doctor 全绿。

**W4 — 前端更严约束判定为有意，就地文档化**
控制台强制 ≥1 别名、隐藏从未绑定的全禁用别名——判定为链路引导的产品决策，API 保持权威
（脚本可建零别名 Project）。已在 `ProjectsPage.tsx` schema 处注释声明，不放开。

## 阶段 3 之后的遗留项（下一批）

- **F23**（运行时新发现，见 runtime-evidence.md）：健康过滤空候选误报 400 `unsupported_feature`，
  应与操作过滤区分、映射 5xx。
- **F24**（真实上游冒烟新发现）：openai 适配器 `operationURL` 硬编码 `/v1`，非 `/v1` 版本段的
  兼容端点（如 Z.AI `/api/paas/v4`）无法配置，上游 404 实证。
- F20（三份 `LastTest*` 副本收敛）仍为建议级，未动。
