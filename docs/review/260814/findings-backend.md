# 阶段 1：后端链路静态一致性审查发现

按 [review-plan.md](review-plan.md) 八族（C1–C8）横穿全链。每条发现标注判定基准（B1–B8，定义见方案）
与分级【肯定 / 建议 / 问题 / 疑似BUG】。事实底座见 [chain-map.md](chain-map.md)。

分级口径：**肯定** = 查证后确认符合设计；**建议** = 不违反基准但值得改；**问题** = 违反基准、影响有限
或前提难达；**疑似BUG** = 违反基准且路径可达——本轮**没有**产出这一级。所有"问题"级发现均需阶段 4
对抗验证后才算成立。

---

## C1 · 引用完整性与删除语义

**F1【问题】墓碑编辑语义五跳不一致**（B4）
Deployment 更新拒绝墓碑（`admin_deployments.go:222` `err != nil || current.DeletedAt != nil` → 404），
GatewayKey 更新拒绝（`adminProjectKey` 助手过滤 `DeletedAt`），但 **Provider 更新**
（`admin_providers.go:245-250` 只查 err 与 revision）、**Route 更新**（`admin_providers.go:880-888`）、
**Project 更新**（`admin_projects.go:118-126`）都不拒绝：`DeletedAt` 前传保持墓碑
（`admin_providers.go:290`、`:890`、`admin_projects.go:132`），但字段可改、审计照记 `*.update`。
失败场景：对已删除 Provider 发 PUT → 200，产生一条"更新了不存在对象"的审计事件与一个字段被改过的
墓碑。运行时不受影响（registry 与 auth 快照都跳过墓碑），纯一致性/审计噪声缺陷。
修法：三个 update 处理器补 `current.DeletedAt != nil` → 404，与 Deployment/Key 对齐。

**F2【问题】空 BindingID 回退三种拼写不等价，差异在 `Enabled`**（B4/B8）
`admin_providers.go:1556` 注释声称"Resolved exactly the way the registry resolves it"，对该调用点成立，
但三处回退规则实际不同：
- `validateDeploymentProviderProfile`（`store_pricing.go:158`）：`DefaultProviderProfileBindingID`，
  **不看 `Enabled`**；
- `matchingBindingID`（`providers.go:724`）：要求**唯一且 `Enabled`**；
- `normalizeDeploymentBinding`（`store_pricing.go:117`）：profile+surface 匹配，**不看 `Enabled`**。

binding ID 恒为默认模式（`admin_providers.go:1249` 强制覆写）且 profile 不可重复
（`models.go:534`），所以 ID 解析在实践中收敛；**残余差异是 `Enabled`**：store 层校验允许把
deployment 写在 disabled binding 上（`ProfileBinding` 不滤 Enabled，`models.go:382`），registry 层
则 withhold。经查 Admin 创建/更新路径经 `resolveDeploymentTarget` 过滤 Enabled binding
（`admin_deployments.go:1007`），`validateBindingsCanDeactivate` 守反向，所以**经支持的路径大概率
不可达**——但"大概率"未穷尽（恢复、直接 store 调用方）。方向 fail-closed（registry withhold），
无安全后果。修法：`validateDeploymentProviderProfile` 对 `deployment.Enabled && !binding.Enabled`
拒绝，把规则收成一种拼写。

**F3【建议】`PutGatewayKey` 的 store 层项目存在性检查不排除墓碑**（B4）
`store_projects.go:68` 只查 `Get != nil`，软删 Project 的记录仍在 bucket 里，检查通过。Admin 层挡住了
（`admin_projects.go:249`），运行时也 fail-closed（快照过滤墓碑 Project，`snapshot.go:61`），但 store
作为"每个写入都要过的边界"（binding ceiling 注释 `models.go:345-350` 自己立的标准）少了一层。同类：
`PutRoute` 只查 Deployment 存在不查墓碑（`store_providers.go:325`），`PutProviderResource` 三向检查
同样只查存在（`store_providers.go:386-394`）。建议统一：store 层引用检查排除 `DeletedAt != nil`。

**F4【肯定】Credential 硬删与墓碑 Provider 的组合已被正确处理**
`ensureCredentialUnreferenced`（`store_providers.go:356`）只数未软删引用，所以墓碑 Provider 的
credential 可删；`loadProviderRegistry` 在 `GetCredential` **之前**跳过墓碑（`providers.go:366` 先于
`:369`），悬空引用不触发 refuse。链条闭合。

**F5【肯定】删除守卫矩阵双向闭合**
下行：Provider→Deployment（`admin_providers.go:1443`）、Binding→Deployment（`:1538`）、
Deployment→Route（`admin_deployments.go:1276`）、Policy→Project（`admin_redaction.go:275`/`:294`
delete 比 deactivate 更严，连 disabled Project 的引用也算，注释说明 re-enable fail-open 链）。
上行：Route→Project alias（`admin_providers.go:1483`）、Project→alias 存在（`admin_projects.go:438`）。
Credential→Provider（`store_providers.go:217`，事务内）。

---

## C2 · 能力收窄单调性

**F6【肯定】收窄链五级完整，写入边界选对了位置**
上限检查在 `binding.Validate`（`models.go:357`），注释明言这是"每个写入都要过的边界"，并解释了
为什么绑定数组曾是绕过口；`PutProvider` 同事务遍历存量 Deployment 防收窄悬空
（`store_providers.go:247-265`）；Deployment 子集+evidence 等级检查在 store 事务内
（`store_pricing.go:156-178`）；加载期对越限存量 withhold 而非 clamp（`providers.go:407-416`，注释
说明理由）；请求期按 operation 过滤（`provider.go:438`）。Admin 输入路径二次检查同一上限
（`admin_providers.go:1258`）。**未发现任何一层放宽。**

**F7【问题】加载路径不复验 Deployment 记录，空能力声明整套采纳 adapter 能力**（B3）
`loadProviderRegistry` 从 store 读 Deployment 后不调 `deployment.Validate()`；
`deploymentCapabilities` 对 `!declared.AnyOperation()` 返回 adapter 全集（`providers.go:767-769`）。
经支持的写入路径不可达（`Deployment.Validate` 强制至少一个 operation，`models.go:952`；所有 store
写入都过 Validate），但被篡改/手写的记录会拿到 adapter 全部能力——fail-open 方向，与
`Registry.Register` 对空交集拒绝（`provider.go:340-351`）方向相反，也与本仓对篡改状态的其他处理
（凭据不符 refuse）不一致。前提是 bbolt 被篡改，威胁模型内属边缘。修法：加载期对
`!deployment.Capabilities.AnyOperation()` withhold 而非采纳。

**F8【肯定】漂移复核 fail-closed 且新状态默认不可路由**
`capabilityReviewAdmitsTraffic` allowlist 写法（`capability_drift.go:235-243`）；`drifted` 在加载期
withhold（`providers.go:511-518`）；请求路径读钉住的快照、不读活目录（`models.go:719` 注释）。

---

## C3 · 秘密与上游标识不外泄

**F9【肯定】数据平面：Model 字段全部重写为请求别名，错误固定句式，上游正文不落日志**
响应重写：非流式 `service.go:969`、native `:1242`、native 流事件 `:1424`、流式 chunk `:1857`/`:1877`、
embeddings `:2024`。错误：`gateway.Error` 只有 `Code`+`Message` 到客户端（`gatewayapi/handler.go:109`
只写这两个，`Cause` 不序列化）；`mapProviderError` 全部固定句式。日志：`logProviderFailure`
（`service.go:562-605`）注释明言**不写上游自己的句子**（上游最可能回显的就是它刚拒绝的凭据），只记
分类、状态码、request ID；只有无上游状态码的 Halro 自产错误才记文本。

**F10【肯定】控制平面：凭据密文不出 Admin API，KeyHash 有测试守护**
`credentialViewFrom`（`admin_providers.go:1591`）只出 `SecretConfigured` 布尔与 `BoundBaseURL`；
解密明文用后即清（`providers.go:428`）；`gatewayKeyView` 由 `TestAdminKeyResponsesNeverExposeKeyHash`
守护（`models.go:229` 注释）；签发响应 `Cache-Control: no-store`（`admin_projects.go:276`）。
Admin 响应含 `credential_id`/`base_url`/`provider_model`——在 Admin 信任域内，符合设计。

**F11【问题】策略快照覆盖检查只护推理平面，资源平面缺第二道闸**（B3）
`assertPolicySnapshotsCoverProject` 唯一调用点在 `resolveRequest`（`service.go:231`）。资源平面入口
`resourcePrincipal`（`inference_resources_store.go:157`）不做等价检查，而 CreateFile/批量结果路径
都依赖 redaction（`:194-204`、`redactBatchResults :864`），引擎对 lookup miss 的语义是 fail-open
"no policy"（`service.go:187-189` 注释自述）。第一道闸 `refuseWhileSnapshotsStale` 中间件覆盖两个
平面（files/batches 在 guarded 组内，`runtime.go:1318-1324`），拦住**被追踪的**失稳；但注释列举的
另一种到达方式——"one of those guards regressed"（未被追踪的漂移）——只有推理平面有第二道闸。
非对称防御。修法：`resourcePrincipal` 加同一断言。

---

## C4 · 项目隔离与授权粒度

**F12【肯定】ProviderResource 隔离闭合（H6 判定）**
store 读/删按 projectID 收口（`store_providers.go:415-424`、`:470-486`，跨项 404 不泄露存在性）；
数据平面 owner 解析全部传 `principal.Project.ID`（`fileOwner :336`、`batchOwner :990`、
`batchInputFile :411`）；`ownedTarget` 按 (PublicModel, ProviderID, DeploymentID, ProfileID, Region)
**全元组**对活 registry 匹配（`:170-178`），route 被改指后旧资源 409 而非错误接管。
`ListProviderResources` 不收口但仅备份路径调用（`store_providers.go:426-430` 注释限定），无 HTTP 出口。

**F13【肯定】四个入口族全部过 `resolveRequest` 的 AllowedRoutes 检查**
chat/embeddings（`service.go:855`/`:1933`）、native messages（`:1445`）、流式（`:1738`）、资源创建
单独比对（`inference_resources_store.go:205`、`:1101`）。检查顺序先 403 后 404——不在授权集里的
别名一律 403，不泄露该别名是否存在。

**F14【建议】资源读取不复查 AllowedRoutes**
GET/下载/删除既有资源只验所有权不验别名仍被授权（`fileOwner`/`batchOwner` 无 AllowedRoutes 比对）。
运维把别名从 Project 撤走后，项目仍可轮询/下载既有 batch 结果（在记账信封内，有限速有痕迹，
`localFileObject` 注释 `:370-378` 说明信封的用意）。撤销语义是"不能新建，能收尾"——合理的设计选择，
但方案文档没写。建议在 `docs/contracts/` 明确这条撤销语义。

---

## C5 · revision、并发与写入原子性

**F15【肯定】revision 协议与锁矩阵一致**
`putVersioned`（`store.go:1557`）：create 要求 expected=0、update 精确匹配、冲突 `ErrRevisionConflict`；
Key 哈希索引与记录同事务维护（`store_projects.go:67-84`）。锁序全局一致：**topology 先于 project**
（`admin_providers.go:873-875` 注释、route update/delete `:876-879`/`:938-942`、project create/update
`admin_projects.go:69-76`/`:110-117`）；策略写持 `adminProjectMu`（`admin_redaction.go:98` 等）与
Project 写序列化；跨对象守卫读（ListRoutes/ListDeployments）都在对应锁内。跨对象强一致的检查
（Provider 收窄 vs 存量 Deployment、Credential 删除 vs 引用）都在单 bbolt 事务内。

**F16【肯定】激活失败语义正确（阶段 0 第八节的判定收尾）**
提交点=store 事务；激活失败不谎报请求失败，改为 markStale → 数据平面整体拒绝 → 恢复循环重试；
激活 context 独立于 Admin 请求。修复的历史缺陷有注释存档（`providers.go:63-70`）。

---

## C6 · 探测状态与新鲜度

**F17【肯定→设计确认】三份 `LastTest*` 副本不参与决策，健康门是独立内存态（H4 判定）**
路由只读 `Registry.health`；三个放行默认（未探测可路由 `provider.go:526`、非 Prober 不探测
`health.go:56-61`、换代保留搬运 `provider.go:696-700`）各有注释说明取舍（防启动黑洞、被动熔断兜底、
防换代闪断）。探测循环对停用/软删/adapter 缺失记不健康（`health.go:48`/`:53`）。**判定：这是刻意的
可用性取舍，被动熔断为兜底，非 B3 违反。** 残余暴露窗口 = 探测间隔内的不健康上游，由熔断收敛。

**F18【建议】`Registry.health` 墓碑条目不清理**
换代搬运保留所有旧条目（`provider.go:696-700`），已删 Deployment 的健康位永久留存。无正确性影响
（无 target 引用它），纯内存微涨。可在搬运时按 replacementTargets 的 DeploymentID 集合过滤。

---

## C7 · 命名与语义漂移

**F19【问题】`Project.AllowedRoutes` 名实不符（H1 判定）**（B8）
存的是 public model 别名，非 route ID（三处一致：`admin_projects.go:451`、`admin_providers.go:1512`、
`service.go:225`；JSON 契约字段名 `allowed_routes`，前端/文档同名）。后果有二：① 读者按名理解会以为
授权到路由粒度；② 实际粒度是别名——同别名多条 Route（优先级/轮询容量组）无法对 Project 拆分授权。
②经查是自洽的设计：候选组内 failover 本就该整组授权，`validateAliasKeepsServingProjects` 的
"容量变更"分支（`admin_providers.go:1499-1502`）证实别名粒度是有意选择。**所以缺陷是名字，不是语义。**
修法（B7 就地改）：字段与 JSON 名改为 `AllowedModels`/`allowed_models`，durable schema 变更，需数据
目录再初始化或迁移；连带 Admin API 契约、前端、`bootstrap.go:175`、onboarding。属阶段 4 后统一整改项。

**F20【建议】`Route.LastTest*` 语义是"所指 Deployment 的健康"，字段却挂在 Route 名下**
route 测试端点（`admin_providers.go:714`）实际测的是 deployment 链路。与 F17 同根：三份副本都是
运维旁证。若做 F19 的 schema 整改，可顺带评估三份副本是否收敛为一份（Deployment 上）+ 两处引用。

---

## C8 · 错误语义与可行动性

**F21【肯定】链路配置错误全部指名哪一跳**
样本：credential 不存在（`store_providers.go:237` 带 ID）、profile 不符指明 deployment
（`store_providers.go:260`）、binding 被引用指明 deployment 名（`admin_providers.go:1564-1567`）、
alias 最后一条路由指明 project 名（`:1513-1516`）、单模型 profile 拒绝时报"哪个过滤器拒的"而非泛化
（`admin_deployments.go:1024-1031` 注释）。错误码可编程分派（`binding_referenced_by_deployment`、
`route_referenced_by_project`、`gateway_key_idempotency_replay`）。

**F22【建议】`adminConfigurationError` 吞掉 err 参数**
`admin_providers.go:1602-1606` 收 `err` 但只写固定句"configuration could not be activated"。若为防
泄漏是对的方向，但连错误分类都没有，排障只能翻服务端日志。建议至少分类出码。

---

## 汇总

| 级别 | 条目 |
|---|---|
| 疑似BUG | 无 |
| 问题 | F1（墓碑编辑不一致）、F2（回退拼写差异，限 Enabled）、F7（加载不复验，需篡改前提）、F11（资源平面缺第二道闸）、F19（AllowedRoutes 名实不符） |
| 建议 | F3、F14、F18、F20、F22 |
| 肯定 | F4、F5、F6、F8、F9、F10、F12、F13、F15、F16、F17、F21 |

**修复优先级预排**（严重度 × 成本，待阶段 4 裁决后生效）：
1. F11 — 一行断言补齐非对称防御，成本最低、方向正确；
2. F1 — 三处 404 检查，机械修；
3. F2 — store 层补 `binding.Enabled` 检查，收敛为一种拼写；
4. F7 — 加载期 withhold 空能力记录；
5. F19 — durable schema 改名，动静最大，攒同类一次做（含 F20 评估）。

阶段 4 对抗验证靶单：F1、F2、F7、F11（F19 是结构事实无需对抗，直接进设计裁决）。
本轮明确说明无法静态判定、留给阶段 3 运行时的：F2 的"经支持路径不可达"是否穷尽（R4 变体：先建
disabled-binding deployment 再启用）、F11 的实际可达性（需人为制造快照回退）。
