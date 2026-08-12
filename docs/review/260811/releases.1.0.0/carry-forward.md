# v1.0.0 放行评审 · 第 0 步继承台账（carry-forward）

> 按 [review-plan.md](review-plan.md) §3 与 [role-prompts.md](role-prompts.md) §3 执行。
> 核对基准：`main@2cd24a7`（与方案 §2.1 记录的 HEAD 一致，已用 `git log` 核实区间提交）。
> 三态：**已关闭 / 仍开 / 无法验证**。每条附 `file:line` 或命令输出。
> 本文只重核历史结论，不产生新发现；新发现归各角色。

## 1. 结论（≤200 字）

35 条历史结论重核完毕：**已关闭 18 条，仍开 17 条，无法验证 0 条**。260811 API 链路的 7 条 finding 与 3 项子项全部既有修复提交、又有具名回归测试，且本次实跑全绿——"已修但无守护"表为**空**；但有 3 处守护覆盖不完整的边角，单列供 A4。260810 视觉 8 条全部关闭，12px 下限声明数从 116 归零并有零容忍测试守护。仍开的 17 条全部来自 260807 遗留（8 条原样未动）与 260809 方案 §5（9 项，因该轮评审未执行而无一有结论记录）——它们正是本轮各角色与 G7 裁决的待办，不是新债。

## 2. 本角色评分

**7.0 / 10**（锚点见 [scoring-rubric.md](scoring-rubric.md) §3）。

理由：不是 8，因为仍开的 17 条里有多条 P1 级实质问题（argon2 内存放大代码侧无界、探测计费不进 Ledger、`security-review-v1.md` 自称 blocked）**至今没有"接受 / 修复 / 写入已知限制"的书面裁决**，且发布说明缺已知限制清单——正是"机制建得对，但门没上锁"的 7 分形态。不是 6，因为已关闭的 18 条质量很高：最近两轮（260810/260811）的整改全部带自动化守护（CI 测试零容忍或具名回归测试），历史同类问题有防复发机制，仍开各条均在原始文档里有明确的取舍论证与定位。

证据基数：见 §4~§6 各表，每条独立 `file:line` 或命令输出，合计远超 3 条。

## 3. 肯定项（值得在后续改动中保护）

1. **12px 下限是零容忍测试而非 ratchet**：`web/src/design-system.test.ts:198-204` 断言业务 CSS 低于 12px 的绝对字号必须为空集，`:171` 注释明确"低于下限是缺陷，界为零"。字重字面值同样零容忍（`:212-220`）。这是 260807 轮"ratchet 基线写宽 4 格被悄悄吃光"教训的正确答案。
2. **每条 F-xx 修复都有以行为命名的回归测试**，测试名即不变量（如 `TestStaleActivationRefusesDataPlaneTraffic`、`TestDeletingTheLastRouteForAReferencedAliasIsRefused`），复发时报警信息可直接读懂。见 §6 表。
3. **`activateTopology()` 不接受 context 参数**（`internal/app/activation_context_test.go:93-95` 注释点明），把 F-01"调用方把请求生命周期传进激活"的整类错误变成编译期不可能，比测试守护更强。
4. **binding 级守卫与注册表宽恕语义配对落地**：`internal/app/admin_providers.go:1140-1155` 的注释同时记录了"为什么域校验没拦住"与"注册表不再致命"，`internal/app/doctor.go:313-327` 让排除可见——修复、降级语义、可观测三件套齐全。

## 4. 260809 方案 §5 的 9 项"不允许空白"清单

背景事实：`docs/review/260809/` 目录下只有 `review-plan.md`，**没有 260809.md 报告**（`ls docs/review/260809/` 实证）——该轮评审未执行，9 项因此无一有结论记录。逐项核对是否被后续提交"顺带"覆盖：

| # | 事项 | 三态 | 证据 |
|---|---|---|---|
| 1 | ADR 0018 额度不变量在异常路径下是否成立 | **仍开** | 无任何独立复核记录；`docs/adr/0018-project-admission-and-the-accounting-write-path.md` 仍是唯一论证来源（作者自述）。归本轮 A1 |
| 2 | 能力两个"安全阀"能否组合成越权放宽路径 | **仍开** | 两阀仍在：`operator_declared` 是合法 ClaimSource（`internal/domain/models.go:619`、`internal/domain/invocation_target.go:115`）。无结论记录。归本轮 A3 |
| 3 | Bedrock 控制面 host 派生是否仍受 safetransport 约束 | **仍开** | 派生逻辑在 `internal/provider/bedrock/adapter.go:38-43,126`（`newControlPlaneAuthorizer`）；无评审结论记录。归本轮 A2 |
| 4 | rc.1 publish 未运行的根因 | **仍开** | `git tag` 仅有 `v1.0.0-rc.1`，无 rc.2；`grep -rn "rc.1" docs/` 无根因记录。归本轮 B3（G4） |
| 5 | 能力选择 §15 剩余三条门禁的处置 | **仍开**（一条半已动） | 门禁表已随文档迁到 `docs/prd/provider-model-selection-and-capability-resolution.zh-CN.md`（`bd40e97` 归档）。浏览器验收：2026-08-10 已完成 **fixture 本地 RC**（该文件 `:562`，证据在 `docs/verification/provider-real-matrix.md`）；真实 Provider 证据：同一行明言"仍是精确 RC commit 的外部门禁，未以 fixture 结果冒充通过"；`provider_metadata`：〔2026-08-12 更正——原写"代码里只有枚举值与校验（`invocation_target.go:115,151,164`），无任何 Adapter 发射它"，**这是事实错误**。在评审 HEAD `2cd24a7` 上三个 Adapter 都在发射 `domain.ClaimSourceProviderMetadata`：`internal/provider/gemini/adapter.go:251`、`internal/provider/bedrock/models.go:153`、`internal/provider/anthropic/adapter.go:192`，各自的 `DescribeInvocationTargets` 路径有测试覆盖（`gemini/invocation_targets_test.go`、`bedrock/models_test.go`、`anthropic/invocation_targets_test.go`）。因此**这一条不是"已定义但无人发射的占位"，无需裁决**；报告 §9.3 #13 建议的"撤销该枚举值或补实现"建在这个错误前提上，不成立〕。剩余两条（浏览器验收、真实 Provider 证据）的三选一裁决未做，归 G7 |
| 6 | 260807 遗留 8 条逐条裁决 | **仍开** | 8 条本区间无一有提交动过（逐条证据见 §5），且"接受/修复/写入已知限制"书面裁决不存在。整体进 G7 裁决队列 |
| 7 | 能力探测计费调用不进 Ledger 是否阻塞 1.0.0 | **仍开** | 现状未变：`internal/app/admin_model_capability_detections.go:427-435,529,623` 仍只记 `ProviderCalls` 计数；该文件 `grep -n "edger"` 零命中，无 Ledger 引用。裁决归本轮 A1 |
| 8 | `security-review-v1.md` 自称 blocked 是否仍成立 | **仍开** | 原句原样仍在：`docs/verification/security-review-v1.md:62`——"Final v1 release remains blocked on the M10 recovery, soak, packaging, and release-signing gates." 归 B3 + A2 |
| 9 | 1.0.0 已知限制清单进发布说明 | **仍开** | `grep -rniE "limitation|单写者|single.writer|高可用" docs/milestones/release-notes-v1.0.0.md` 零命中——发布说明没有已知限制章节。归汇总阶段（G8） |

小计：仍开 9，已关闭 0，无法验证 0。**这 9 项没有一项被后续提交顺带关闭**，它们构成本轮各角色下限项的直接来源。

## 5. 260807 遗留 8 条半项

全部**仍开**，且本区间（33bc13b→2cd24a7）无提交触碰。每条按 G7 要求需要"接受 / 修复 / 写入已知限制"三选一书面裁决：

| 编号 | 未做的部分 | 三态 | 证据 |
|---|---|---|---|
| P1-6(b) | argon2 并发信号量 | **仍开** | `grep -rniE "semaphore|acquire" internal/adminauth/` 零命中。内存放大仍只靠 k8s 1Gi limit 兜底，裸机/Docker 无界 |
| P0-5 | `syncUsageAdmin` 的 `WithoutCancel` + 独立超时；`applyMu` 不感知 ctx | **仍开** | `internal/app/admin_usage.go:553-558` 仍用 `request.Context()`；全仓 `WithoutCancel` 仅在 `runtime.go:1068`（shutdown）与 `adminauth/session.go:109,114`，无 usage 路径；`applyMu` 现居 `internal/usage/collector.go:30,55,72`，仍是普通 `sync.Mutex` |
| P1-7 | `PUT /credentials/{id}`、`PUT /redaction-policies/{id}` 的 step-up | **仍开** | `requireDestructiveStepUp` 在这两个文件里只挂在 DELETE 上：`admin_providers.go:147`（deleteAdminCredential）、`:300`（deleteAdminProvider）、`:732`（deleteAdminRoute）、`admin_redaction.go:171`（deleteAdminRedactionPolicy）；`updateAdminCredential`（`admin_providers.go:91`）与 redaction 的 PUT 均无 |
| P1-10 | 锚点文件自身的 HMAC / 前向 hash 链 | **仍开** | `grep -rniE "hmac" internal/deadman/*.go`（非测试）零命中 |
| P1-12 | 溢出预算随 `maxTracked` 缩放 | **仍开** | `internal/sourcelimit/limiter.go:112` 仍是 `l.overflow > l.limit`——溢出预算 = 单源预算，未随表容量缩放 |
| P2-21 | `halro audit anchor rotate` 子命令、deadman 侧独立 token 文件 | **仍开** | `cmd/halro/main.go:696,738` 的 usage 仍只有 `audit verify|verify-anchor`；`grep -rn "anchor_bearer_token_file" internal/ cmd/` 零命中 |
| P2-28 | fuzz 失败语料自动回灌成回归种子 | **仍开** | `.github/workflows/ci.yml:118-131`：新语料仍只 `::notice` 提示"Commit the files above"，提交是手工步骤 |
| P2-29 | 尺寸 ratchet 扩展到 TSX 内联样式 | **仍开** | `web/src/design-system.test.ts:152-162` 的 spacing/radius ratchet 只读 `styles.css`；`:235-238` 对 ts/tsx 只查字面颜色与原语 token，不查内联尺寸 |

小计：仍开 8，已关闭 0。注：其中 6 条在 260807/progress.md 里有明确的取舍论证（"不是遗漏"），仍开 ≠ 无人考虑过；但书面三选一裁决是 G7 的要求，尚不存在。

## 6. 260810 视觉评审 P1×3、P2×4、P3×1

整改提交：`de53ee0`（主体）、`f058d2f`、`dbe9318`。8 条全部**已关闭**：

| 条目 | 三态 | 证据 |
|---|---|---|
| P1-1 低于 12px 的 116 处字号声明 | **已关闭（有守护）** | `grep -nE 'font-size:[^;]*(10px|11px|\.625rem|\.6875rem)' web/src/styles.css` = **0 命中**（原 116）。剩余 10px/11px 字面量全部落在 padding/gap/margin 等间距属性。守护：`design-system.test.ts:198-204` 零容忍断言，本次实跑通过 |
| P1-2 标题与正文倍率过大（H1 54px） | **已关闭** | `styles.css:31` H1 现为 `clamp(32px, 4vw, 42px)`；登录标题 `clamp(34px, 4.2vw, 46px)`（`:1540`）。字阶收敛到 token 12/13/15/18/22(/32)（`tokens.css:77-82`）；全文件 259 条 font-size 声明中 250 条走 `var(--font-size-*)`，字面值仅 9 条且集中在 6 行（`:31,1053,1306,1540,1541,1550`），均为评审建议保留的档位 |
| P1-3 高密度页两级次要信息压进小字+muted | **已关闭** | 审计时间线：`.timeline p` 现为 sm(13px) sans（`styles.css:1018`），序号保留 xs mono（`:1007`）；System Config：title sm、key xs mono、描述 sm、值 sm mono semibold（`:1134-1137`），与评审建议逐项一致；开发者工作台 tab/说明统一 xs/sm token（`:1568-1590`） |
| P2-1 Light 主操作色偏沉、三套绿不一致 | **已关闭** | `git diff 33bc13b..HEAD -- web/src/design-system/themes/light.css`：`--color-action-primary` 从 lime-800 提亮为 lime-700，`--color-focus-ring` 从 green-600 改为同源 lime-700（`light.css:33-38`）。AA 由 `design-system.test.ts:96-119` 门禁维持 |
| P2-2 中等宽度登录表单贴左 | **已关闭** | `.login-panel` 为 `display:grid; place-items:center`（`styles.css:1545`）；≤820px 改单列、故事区保留 42vh（`:1722-1726`），≤580px 故事隐藏时面板 `min-height:100vh` 居中（`:1910-1912`）——"故事被隐藏而表单仍贴左"的状态不再存在 |
| P2-3 移动端导航原生滚动条 | **已关闭** | `styles.css:1685-1688`：`scrollbar-width:none` + `::-webkit-scrollbar` 隐藏 + 尾缘渐隐 mask 作为可滚动提示，注释即评审原话的实现 |
| P2-4 弹窗说明/辅助文字过小 | **已关闭** | `.field small` 现为 xs=12px（`styles.css:1340`），弹窗标题 `--font-size-xl`=22px（`:1322`），断层消除；由 P1-1 的零容忍测试一并守护 |
| P3-1 ratchet 不约束字体尺度 | **已关闭** | 字号下限测试（`design-system.test.ts:198`）+ 字重字面值禁令（`:212`）已存在；spacing ratchet 基线同步从 748 降到 743（`:150`），未被顶高。备注：>12px 的字面字号仍允许（现存 9 处），属可接受残余，不构成原 P3 的复发 |

小计：已关闭 8，仍开 0。**下限项应答（260810 12px 整改）：当前仍低于下限的字号声明数 = 0（原基线 116 处）**；`grep -c 'font-weight: *650' web/src/styles.css` = 0，业务 CSS 字重全部 token 化（`grep -oE 'font-weight:[^;]+' | sort | uniq -c` 仅见 var(--font-weight-\*) 与一处 inherit）。

## 7. 260811 API 链路 F-01~F-07 及三项子项：回归守护核对

按方案 §2.3 要求，本节的重点不是"修没修"，而是**每条是否有具名回归守护**。核对方法：定位守护测试 → 实跑确认全绿（命令见 §9）。

| 条目 | 修复提交 | 三态 | 回归守护（具名测试 / 门禁） |
|---|---|---|---|
| F-05 禁用被引用的 binding 写坏数据目录 | `e1d94be` | **已关闭（有守护）** | 守卫：`TestProviderRefusesToSwitchOffAnInterfaceADeploymentRunsOn`（`internal/app/admin_providers_test.go:996`，断言 409 `binding_referenced_by_deployment` 且**未落盘**）+ `TestProviderAllowsSwitchingOffAnUnusedInterface`（`:1052`）；withhold 语义：`TestDanglingBindingWithholdsTheRouteAndStillLoadsTheRegistry`（`capability_withholding_test.go:285`）、`TestRuntimeOpensWithADanglingBinding`（`:310`，重启存活）、`TestHotReloadSucceedsWithADanglingBinding`（`:326`） |
| F-01 撤销已落盘但未激活，旧快照继续放行 | `354428c` | **已关闭（有守护）** | `TestRevokedKeyStopsAuthenticatingWhenTheAdminClientDisconnects`、`TestDisabledProjectStopsAuthorizingWhenTheAdminClientDisconnects`、`TestDeletedRouteStopsRoutingOnRuntimeOwnedActivation`（`internal/app/activation_context_test.go:47,73,97`）；另有编译期守护——`activateTopology()` 无 context 形参（`:93-95`） |
| F-02 删最后一条 Route 留悬空别名 + TOCTOU | `066a08a` | **已关闭（有守护）** | `route_reference_integrity_test.go:27,78,96,119,149` 五条：拒删、多 Route 允许、无引用允许、重命名同守卫、disabled 引用允许（Q-01 语义一并钉住），断言码 `route_referenced_by_project`（`:42`） |
| F-03 mutation/激活/审计三阶段无统一提交点 | `18f3939`+`a96cf4b`+`c051edc`，后续 R-03 | **已关闭（有守护）**〔2026-08-12 修订：本轮曾按 release-1.0.0-report §9.2 第 1 条改判为**已修但语义未闭合**——提交协议自称的“激活失败即拒流”在跨域场景不成立，恢复循环会主动清掉它没修的域；R-03 把 stale 按域建模、恢复循环重放全部四域，并补了跨域不互清与从调用点出发的负面测试，该项重新闭合〕 | HTTP 层：`TestAdminMutationReportsItsOperationAndActivationState`、`TestStaleActivationRefusesDataPlaneTraffic`（`commit_protocol_test.go:17,56`）；存储层：`TestAdminMutationAndAuditIntentCommitTogether`、`TestDeliveredAdminAuditIntentIsRetired`、`TestAdminAuditIntentRejectsDuplicateEventID`（`internal/store/bolt/admin_audit_intent_test.go:25,69,104`） |
| F-06 Provider 级装载失败致命 | `95b5d47` | **已关闭（有守护）** | `TestTighteningTheEndpointPolicyExcludesTheProviderRatherThanTheProcess`（`private_endpoint_policy_test.go:71`）——直接钉住"排除而非拒绝启动" |
| F-07 `allow_private_provider_endpoints` 形同虚设 | `95b5d47` | **已关闭（有守护）** | `TestPrivateProviderEndpointIsUsableWhenConfigurationAllowsIt`（`private_endpoint_policy_test.go:22`，开关生效）+ `TestPrivateProviderEndpointStaysRefusedByDefault`（`:51`，关掉仍拒） |
| F-04 Deployment priority/weight 死字段 | `3580a89` | **已关闭（有守护）** | `TestDeploymentRejectsTheRemovedSchedulingFields` + `TestRoutePriorityStillOrdersCandidates`（`deployment_scheduling_fields_test.go:19,49`）——既拒复活又钉住 Route 侧仍生效 |
| 子项 1：create 幂等语义 | `c051edc` | **已关闭（有守护）** | `TestRetriedCreateDoesNotProduceASecondRecord`（`commit_protocol_test.go:100`，断言 409 `route_idempotency_replay` 点名已有 ID）+ 缺键 400 `idempotency_key_required`（`:154`）+ Gateway Key 侧同断言（`admin_projects_test.go:82,113`） |
| 子项 2：告警/redaction/Token Guard 纳入提交协议 | `c051edc` | **已关闭（有守护，见 §8 残余）** | `TestPolicyMutationsFollowTheCommitProtocol`（`commit_protocol_test.go:164`，operation ID + intent 零积压 + 审计可见）+ `TestPolicyMutationsCarryTheirAuditIntent`（`admin_audit_intent_test.go:135`，被拒的写连记录一起回滚） |
| 子项 3：doctor 看见 endpoint 排除 | `c051edc` | **已关闭（有守护）** | `private_endpoint_policy_test.go:129-140`：收紧策略后 `DoctorWithOptions` 必须报不健康并点名被排除的 Provider（`doctor.go:313-327` 为被测实现） |

小计：已关闭 10，仍开 0。上述具名测试本次全部实跑通过（`go test ./internal/app/ -run '<17 个测试名>' -count=1` → `ok 8.409s`；`go test ./internal/store/bolt/ -run '<4 个测试名>' -count=1` → `ok 0.866s`）。

### "已修但无守护"表（供 A4）

**空。** 七条 finding 与三项子项每条都至少有一个以该缺陷行为命名的回归测试，且当前全绿。

### 守护覆盖不完整的边角（不构成"无守护"，单列供 A4 的补测试清单）

| # | 条目 | 缺口 | 证据 |
|---|---|---|---|
| 1 | F-05 | 只有"禁用 binding"变体有具名测试；"从 bindings 列表**整体移除**被引用的 binding"无独立测试。守卫实现按"启用集合缺席"判定，移除与禁用走同一分支（`admin_providers.go:1164-1169` 构建 enabled 集合，注释 `:1146` 明言 "switching off — or simply omitting"），但 260811 评审自己的验证矩阵把移除列为独立场景 |
| 2 | F-01 | 具名测试覆盖 Key / Project / Route 三类；Provider、Deployment 的 disable-后-断连变体无独立用例（机制共享 `activateTopology()`，风险低） |
| 3 | 子项 2 | "redaction / Token Guard 激活失败 → markStale"（`admin_redaction.go:324`、`admin_token_guard.go:342`）与"告警投递失败**不**标 stale"（`admin_alerts.go:358` 注释）这两个方向性决定没有从调用点出发的负面测试；现有 `TestStaleActivationRefusesDataPlaneTraffic` 是直接调 `markStale`（`commit_protocol_test.go:64`），不经由这些路径 |

## 8. 三态汇总

| 来源 | 已关闭 | 仍开 | 无法验证 | 合计 |
|---|---:|---:|---:|---:|
| 260809 方案 §5 | 0 | 9 | 0 | 9 |
| 260807 遗留 | 0 | 8 | 0 | 8 |
| 260810 视觉 | 8 | 0 | 0 | 8 |
| 260811 API 链路 | 10 | 0 | 0 | 10 |
| **合计** | **18** | **17** | **0** | **35** |

仍开 17 条的去向：260807 的 8 条 + 260809 #5/#6 进 G7 裁决队列；#1/#7 归 A1，#2 归 A3，#3 归 A2，#4 归 B3（G4），#8 归 B3+A2，#9 归 G8。无一条需要新开角色。

## 9. 附录：实际读过的文件与运行过的命令

### 读过的文件（完整阅读或定位段落）

- `docs/review/260811/releases.1.0.0/role-prompts.md`、`review-plan.md`、`scoring-rubric.md`（§1-§4）
- `docs/review/260809/review-plan.md`（全文）
- `docs/review/260807/progress.md`（全文）
- `docs/review/260810/260810.md`（全文）
- `docs/review/260811/provider-to-project-api-chain.zh-CN.md`（全文）
- `web/src/design-system.test.ts`（全文）
- `web/src/styles.css`（字号/字重/布局相关段落：31, 1006-1019, 1053, 1131-1137, 1306, 1317-1340, 1540-1550, 1674-1740, 1900-1960）
- `web/src/design-system/tokens.css:77-86`、`themes/light.css`（diff 33bc13b..HEAD）
- `internal/app/admin_usage.go:553-559`、`admin_stepup.go`（函数清单）、`admin_providers.go:1140-1190`（binding 守卫全文）、`doctor.go:313-327`、`activation_state.go:38-41` 及各 markStale 调用点
- `internal/app/commit_protocol_test.go`（:17-230）、`activation_context_test.go`（函数与注释）、`route_reference_integrity_test.go`、`private_endpoint_policy_test.go`、`deployment_scheduling_fields_test.go`、`capability_withholding_test.go`（函数清单）、`admin_providers_test.go:996-1051`
- `internal/store/bolt/admin_audit_intent_test.go`（:25-172）、`store_test.go:220-223`（迁移 24~27）
- `internal/sourcelimit/limiter.go`（:26-135）、`internal/usage/collector.go`（applyMu 段）、`internal/provider/bedrock/adapter.go:38-126`、`internal/domain/invocation_target.go`（ClaimSource 段）、`internal/app/admin_model_capability_detections.go`（ProviderCalls 段）
- `cmd/halro/main.go:696-738`、`.github/workflows/ci.yml:79-138`
- `docs/verification/security-review-v1.md:62`、`docs/milestones/release-notes-v1.0.0.md`（前 60 行 + 全文 grep）、`docs/prd/model-aware-capability-selection.zh-CN.md`（3 行指针）、`docs/prd/provider-model-selection-and-capability-resolution.zh-CN.md`（:58, :190-203, :560-580）
- `git show de53ee0 / f058d2f / bd40e97 / 2cd24a7`（提交信息与 stat）

### 运行过的命令（关键项）

```bash
git log --oneline 33bc13b..2cd24a7                      # 区间 46 提交核实
git tag | grep -i rc                                     # 仅 v1.0.0-rc.1
ls docs/review/260809/                                   # 无 260809.md 报告
grep -nE 'font-size:[^;]*(10px|11px|\.625rem|\.6875rem)' web/src/styles.css   # 0 命中
grep -c 'font-weight: *650' web/src/styles.css           # 0
grep -oE 'font-size:[^;]+' web/src/styles.css | sort | uniq -c                # 250 token / 9 字面
git diff 33bc13b..HEAD -- web/src/design-system/themes/light.css
grep -rniE "semaphore|acquire" internal/adminauth/       # 0 命中
grep -rn "WithoutCancel" internal/                       # 无 usage 路径
grep -rn "requireDestructiveStepUp|verifyAdminStepUp" internal/app/*.go       # 仅 DELETE 挂载
grep -rniE "hmac" internal/deadman/*.go                  # 0 命中（非测试）
grep -rn "anchor_bearer_token_file" internal/ cmd/       # 0 命中
grep -rniE "limitation|单写者|single.writer|高可用" docs/milestones/release-notes-v1.0.0.md  # 0 命中
grep -n "blocked" docs/verification/security-review-v1.md               # :62 原句仍在
go test ./internal/app/ -run '<F-01..F-07 及子项的 17 个具名测试>' -count=1   # ok 8.409s
go test ./internal/store/bolt/ -run '<4 个 audit intent 测试>' -count=1        # ok 0.866s
npx vitest run src/design-system.test.ts                 # 13 passed (13)
```

### 执行信息

- 模型：**Fable 5**（claude-fable-5）。
- 执行日期：2026-08-11。
- 全程无拒答、无内容策略拦截、无空响应。
